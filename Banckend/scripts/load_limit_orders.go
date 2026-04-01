package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gsolana "github.com/gagliardetto/solana-go"
)

type placeOrderRequest struct {
	Version     uint8  `json:"version"`
	ChainID     uint16 `json:"chain_id"`
	ProgramID   string `json:"program_id"`
	Market      string `json:"market"`
	User        string `json:"user"`
	Side        string `json:"side"`
	Outcome     string `json:"outcome"`
	OrderType   string `json:"order_type"`
	LimitPrice  uint64 `json:"limit_price"`
	TotalAmount uint64 `json:"total_amount"`
	Nonce       string `json:"nonce"`
	ExpiryTs    int64  `json:"expiry_ts"`
	Signature   string `json:"signature"`
}

type result struct {
	status int
	body   string
	err    error
}

func main() {
	var (
		apiBase     = flag.String("api", "http://localhost:8080/api", "API base URL")
		marketPDA   = flag.String("market", "", "market PDA")
		programID   = flag.String("program", "", "program id")
		chainID     = flag.Uint("chain-id", 101, "chain id")
		total       = flag.Int("total", 10000, "total order count")
		concurrency = flag.Int("concurrency", 200, "parallel workers")
		timeoutSec  = flag.Int("timeout", 20, "per-request timeout seconds")
		expireHours = flag.Int("expire-hours", 2, "limit order expire hours from now")
		amountUnits = flag.Uint64("total-amount", 100, "raw total amount units (100 = 1.00)")
	)
	flag.Parse()

	if strings.TrimSpace(*marketPDA) == "" || strings.TrimSpace(*programID) == "" {
		fmt.Fprintln(os.Stderr, "missing -market or -program")
		os.Exit(1)
	}
	if *total <= 0 || *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "total and concurrency must be > 0")
		os.Exit(1)
	}

	privateKey := buildStablePrivateKey()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	walletAddress := gsolana.PublicKeyFromBytes(publicKey).String()
	authToken := buildFakeAuthToken(walletAddress)

	client := &http.Client{Timeout: time.Duration(*timeoutSec) * time.Second}
	jobs := make(chan int, *concurrency)
	results := make(chan result, *total)
	var wg sync.WaitGroup

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for idx := range jobs {
				reqBody := buildRequest(idx, *marketPDA, *programID, uint16(*chainID), walletAddress, *amountUnits, *expireHours, rng)
				idem := snowflakeLikeID(rng)
				trace := snowflakeLikeID(rng)
				out, err := submitOrder(context.Background(), client, strings.TrimRight(*apiBase, "/")+"/orders", authToken, reqBody, idem, trace)
				if err != nil {
					results <- result{err: err}
					continue
				}
				results <- out
			}
		}(w)
	}

	start := time.Now()
	go func() {
		for i := 0; i < *total; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var accepted int64
	var failed int64
	for out := range results {
		if out.err != nil || out.status != http.StatusAccepted {
			atomic.AddInt64(&failed, 1)
			continue
		}
		atomic.AddInt64(&accepted, 1)
	}

	elapsed := time.Since(start)
	fmt.Printf("accepted=%d failed=%d elapsed=%s rps=%.2f\n", accepted, failed, elapsed.Round(time.Millisecond), float64(*total)/elapsed.Seconds())
}

func buildStablePrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 17)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func buildFakeAuthToken(walletAddress string) string {
	header := map[string]any{"alg": "none", "typ": "JWT"}
	payload := map[string]any{
		"sub":            "did:wallet:loadtest",
		"solana_address": walletAddress,
	}
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(header) + "." + encode(payload) + ".loadtest"
}

func buildRequest(idx int, marketPDA, programID string, chainID uint16, walletAddress string, totalAmount uint64, expireHours int, rng *rand.Rand) placeOrderRequest {
	side := "buy"
	outcome := "yes"
	if idx%2 == 1 {
		side = "sell"
	}
	if idx%3 == 1 {
		outcome = "no"
	}
	priceUnits := uint64(3510 + rng.Intn(50))
	nonce := snowflakeLikeID(rng)
	expiry := time.Now().UTC().Add(time.Duration(expireHours) * time.Hour).Unix()
	return placeOrderRequest{
		Version:     1,
		ChainID:     chainID,
		ProgramID:   programID,
		Market:      marketPDA,
		User:        walletAddress,
		Side:        side,
		Outcome:     outcome,
		OrderType:   "limit",
		LimitPrice:  priceUnits,
		TotalAmount: totalAmount,
		Nonce:       nonce,
		ExpiryTs:    expiry,
		Signature:   base64.StdEncoding.EncodeToString(make([]byte, 64)),
	}
}

func snowflakeLikeID(rng *rand.Rand) string {
	now := uint64(time.Now().UnixMilli())
	randPart := uint64(rng.Intn(1 << 22))
	return fmt.Sprintf("%d", (now<<22)|randPart)
}

func submitOrder(ctx context.Context, client *http.Client, url string, authToken string, reqBody placeOrderRequest, idempotencyKey string, traceID string) (result, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("X-Trace-Id", traceID)

	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return result{status: resp.StatusCode, body: string(raw)}, nil
}
