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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gsolana "github.com/gagliardetto/solana-go"
)

type placeOrderRequest struct {
	MarketID       string `json:"market_id"`
	WalletAddress  string `json:"wallet_address"`
	Side           string `json:"side"`
	Share          string `json:"share"`
	OrderType      string `json:"order_type"`
	PriceTick      int    `json:"price_tick"`
	QtyLots        int64  `json:"qty_lots"`
	ExpireTime     string `json:"expire_time"`
	ClientOrderID  string `json:"client_order_id"`
	IdempotencyKey string `json:"idempotency_key"`
	SignatureNonce string `json:"signature_nonce"`
	SignedAt       string `json:"signed_at"`
	Signature      string `json:"signature"`
}

type result struct {
	status int
	body   string
	err    error
}

func main() {
	var (
		apiBase     = flag.String("api", "http://localhost:8080/api", "API base URL")
		marketID    = flag.String("market-id", "", "market id, e.g. 2222363171854875225")
		total       = flag.Int("total", 10000, "total order count")
		concurrency = flag.Int("concurrency", 200, "parallel workers")
		timeoutSec  = flag.Int("timeout", 20, "per-request timeout seconds")
		expireHours = flag.Int("expire-hours", 2, "limit order expire hours from now")
		qtyLots     = flag.Int64("qty-lots", 100, "order qty in lots (100 lots = 1.00 share)")
	)
	flag.Parse()

	if strings.TrimSpace(*marketID) == "" {
		fmt.Fprintln(os.Stderr, "missing -market-id")
		os.Exit(1)
	}
	if *total <= 0 || *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "total and concurrency must be > 0")
		os.Exit(1)
	}

	privateKey := buildStablePrivateKey()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	walletAddress := gsolana.PublicKeyFromBytes(publicKey).String()
	privyToken := buildFakePrivyToken(walletAddress)

	client := &http.Client{
		Timeout: time.Duration(*timeoutSec) * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
		},
	}

	jobs := make(chan int, *concurrency)
	results := make(chan result, *total)
	var wg sync.WaitGroup

	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			for idx := range jobs {
				reqBody := buildRequest(idx, *marketID, walletAddress, *qtyLots, *expireHours, privateKey, rng)
				out, err := submitOrder(context.Background(), client, strings.TrimRight(*apiBase, "/")+"/orders", privyToken, reqBody)
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
	statusCount := map[int]int64{}
	errSamples := make([]string, 0, 10)
	for out := range results {
		if out.err != nil {
			atomic.AddInt64(&failed, 1)
			if len(errSamples) < 10 {
				errSamples = append(errSamples, out.err.Error())
			}
			continue
		}
		statusCount[out.status]++
		if out.status == http.StatusAccepted {
			atomic.AddInt64(&accepted, 1)
			continue
		}
		atomic.AddInt64(&failed, 1)
		if len(errSamples) < 10 {
			body := strings.TrimSpace(out.body)
			if len(body) > 200 {
				body = body[:200] + "..."
			}
			errSamples = append(errSamples, fmt.Sprintf("status=%d body=%s", out.status, body))
		}
	}

	elapsed := time.Since(start)
	rps := float64(*total) / elapsed.Seconds()

	fmt.Printf("\nLoad test done\n")
	fmt.Printf("- total: %d\n", *total)
	fmt.Printf("- accepted(202): %d\n", accepted)
	fmt.Printf("- failed: %d\n", failed)
	fmt.Printf("- elapsed: %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("- request rate: %.2f req/s\n", rps)
	fmt.Printf("- status counts: %v\n", statusCount)
	if len(errSamples) > 0 {
		fmt.Printf("- sample errors:\n")
		for _, sample := range errSamples {
			fmt.Printf("  * %s\n", sample)
		}
	}
}

func buildStablePrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 17)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func buildFakePrivyToken(walletAddress string) string {
	header := map[string]any{"alg": "none", "typ": "JWT"}
	payload := map[string]any{
		"sub":   "did:privy:loadtest",
		"email": "loadtest@example.com",
		"linked_accounts": []map[string]string{
			{
				"type":       "wallet",
				"chain_type": "solana",
				"address":    walletAddress,
			},
		},
	}
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	// Backend currently decodes claims without verifying JWT signature in local mode.
	return encode(header) + "." + encode(payload) + ".loadtest"
}

func buildRequest(idx int, marketID string, walletAddress string, qtyLots int64, expireHours int, privateKey ed25519.PrivateKey, rng *rand.Rand) placeOrderRequest {
	side := "buy"
	if idx%2 == 1 {
		side = "sell"
	}

	// Keep both sides around mid price to trigger matching under load.
	priceTick := 58 + rng.Intn(5) // 58-62
	expire := time.Now().UTC().Add(time.Duration(expireHours) * time.Hour).Format(time.RFC3339)
	signedAt := time.Now().UTC().Format(time.RFC3339)
	clientOrderID := fmt.Sprintf("load-%d-%d", idx, time.Now().UnixNano())
	idem := fmt.Sprintf("idem-%d-%d", idx, time.Now().UnixNano())
	nonce := fmt.Sprintf("nonce-%d-%d", idx, time.Now().UnixNano())

	req := placeOrderRequest{
		MarketID:       marketID,
		WalletAddress:  walletAddress,
		Side:           side,
		Share:          "yes",
		OrderType:      "limit",
		PriceTick:      priceTick,
		QtyLots:        qtyLots,
		ExpireTime:     expire,
		ClientOrderID:  clientOrderID,
		IdempotencyKey: idem,
		SignatureNonce: nonce,
		SignedAt:       signedAt,
	}
	signPayload := strings.Join([]string{
		"blinkpredict.order.v1",
		"wallet_address=" + req.WalletAddress,
		"market_id=" + req.MarketID,
		"side=" + req.Side,
		"share=" + req.Share,
		"order_type=" + req.OrderType,
		"price_tick=" + strconv.Itoa(req.PriceTick),
		"qty_lots=" + strconv.FormatInt(req.QtyLots, 10),
		"expire_time=" + req.ExpireTime,
		"client_order_id=" + req.ClientOrderID,
		"signature_nonce=" + req.SignatureNonce,
		"signed_at=" + req.SignedAt,
	}, "\n")
	signature := ed25519.Sign(privateKey, []byte(signPayload))
	req.Signature = base64.StdEncoding.EncodeToString(signature)
	return req
}

func submitOrder(ctx context.Context, client *http.Client, url string, privyToken string, reqBody placeOrderRequest) (result, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("privy-id-token", privyToken)
	req.Header.Set("Idempotency-Key", reqBody.IdempotencyKey)

	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return result{status: resp.StatusCode, body: string(raw)}, nil
}
