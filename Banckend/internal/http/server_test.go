package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/faucet"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/orders"
	"blinkpredict/banckend/internal/protocol"
	"blinkpredict/banckend/internal/txreqs"

	"github.com/alicebob/miniredis/v2"
	gsolana "github.com/gagliardetto/solana-go"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/sha3"
)

func TestMatchingEndpointsReturnDisabledContract(t *testing.T) {
	server := newTestServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/orderbook/1", nil)
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		MatchingEnabled bool `json:"matching_enabled"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.MatchingEnabled {
		t.Fatal("expected matching to be disabled")
	}
}

func TestCreateMarketValidatesResolutionMode(t *testing.T) {
	server := newTestServer()
	body := bytes.NewBufferString(`{"title":"test","close_time":"2026-03-13T00:00:00Z","resolution":{"mode":"creator"}}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/markets", body)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestCreateMarketSucceedsForAdmin(t *testing.T) {
	server := newTestServer()
	payload := markets.CreateMarketRequest{
		Title:       "Will SOL close above 250?",
		Description: "Skeleton market",
		Category:    "crypto",
		MetadataURL: "https://example.com/market.json",
		CloseTime:   time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),
		Resolution: markets.ResolutionConfig{
			Mode:      markets.ResolutionModeCreator,
			Authority: "Resolver111111111111111111111111111111111",
		},
	}
	body, _ := json.Marshal(payload)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/markets", bytes.NewReader(body))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", recorder.Code)
	}
}

func TestPlaceOrderRequiresCommandBusConfig(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "1000", "collateral_free_units", "1000")

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 1)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, nil, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)
	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          1,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 50,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         50,
		QtyLots:           100,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", recorder.Code)
	}
}

func TestPlaceOrderPublishesCommand(t *testing.T) {
	publisher := &capturePublisher{}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "1000", "collateral_free_units", "1000")

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 1)
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID:   "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		AdminEmails: map[string]struct{}{"admin@example.com": {}},
		VUSDCMint:   "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, nil, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, rdb, nil, nil, nil, nil)
	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 63,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         63,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if publisher.placePayload.MarketID != 42 {
		t.Fatalf("expected market_id=42, got %d", publisher.placePayload.MarketID)
	}
	if publisher.placePayload.Payload.OrderType != protocol.OrderTypeLimit {
		t.Fatalf("expected limit order type, got %s", publisher.placePayload.Payload.OrderType)
	}
	if publisher.placePayload.Payload.PriceTick != 63 {
		t.Fatalf("expected price tick 63, got %d", publisher.placePayload.Payload.PriceTick)
	}
	if publisher.placePayload.Payload.ExpireTime != 1_893_456_000 {
		t.Fatalf("expected expire_time to be set")
	}
	if publisher.placePayload.Payload.Signature == "" {
		t.Fatalf("expected signature to be populated")
	}
	if publisher.placePayload.Payload.IntentBytesHex == "" {
		t.Fatalf("expected intent hex to be populated")
	}
	if publisher.placePayload.Payload.OrderID == 0 {
		t.Fatalf("expected order_id to be generated")
	}
}

func TestPlaceOrderRejectsMissingSignature(t *testing.T) {
	publisher := &capturePublisher{}
	server := newTestServerWithPublisher(publisher)
	body := bytes.NewBufferString(`{"market_id":"42","wallet_address":"` + testWalletAddress() + `","original_action":"buy","original_outcome":"yes","original_price_tick":63,"side":"buy","order_type":"limit","price_tick":63,"qty_lots":200,"spend_amount":0,"expire_time":1893456000,"nonce":"1"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", body)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recorder.Code)
	}
}

func TestPlaceOrderRejectsInvalidSignature(t *testing.T) {
	publisher := &capturePublisher{}
	server := newTestServerWithPublisher(publisher)
	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 63,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         63,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	reqBody.QtyLots = 201
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestPlaceOrderRejectsWalletMismatch(t *testing.T) {
	publisher := &capturePublisher{}
	server := newTestServerWithPublisher(publisher)
	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 63,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         63,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken("So11111111111111111111111111111111111111112"))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestMarketBuyRejectsNoLiquidity(t *testing.T) {
	publisher := &capturePublisher{}
	server := newTestServerWithPublisher(publisher)
	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 99,
		Side:              "buy",
		OrderType:         "market",
		PriceTick:         99,
		QtyLots:           0,
		SpendAmount:       5000,
		ExpireTime:        0,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recorder.Code)
	}
}

func TestSellYesRejectsInsufficientProjectedPosition(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("position:42:"+testWalletAddress(), "yes_free_lots", "50")

	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, rdb, nil, nil, nil, nil)

	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "sell",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 63,
		Side:              "sell",
		OrderType:         "limit",
		PriceTick:         63,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recorder.Code)
	}
}

func TestSellNoRejectsInsufficientProjectedPosition(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("position:42:"+testWalletAddress(), "no_free_lots", "50")

	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, rdb, nil, nil, nil, nil)

	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "sell",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         60,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recorder.Code)
	}
}

func TestBuyYesRejectsInsufficientProjectedCollateral(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "50", "collateral_free_units", "50")

	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, rdb, nil, nil, nil, nil)

	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 60,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         60,
		QtyLots:           100,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recorder.Code)
	}
}

func TestBuyNoRejectsInsufficientProjectedCollateral(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "50", "collateral_free_units", "50")

	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, rdb, nil, nil, nil, nil)

	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		Side:              "sell",
		OrderType:         "limit",
		PriceTick:         60,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", recorder.Code)
	}
}

func TestSplitUpdatesProjectedPosition(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "1000", "collateral_free_units", "1000")
	mr.HSet("position:42:"+testWalletAddress(), "yes_free_lots", "0", "no_free_lots", "0")

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	market, _ := repo.Get(context.Background(), 42)
	market.Status = markets.MarketStatusResolved
	market.Outcome = markets.MarketOutcomeYes
	_ = repo.Update(context.Background(), market)
	server := New(config.Config{}, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)

	body := bytes.NewBufferString(`{"market_id":"42","collateral_mint":"mint","amount":300}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders/split", body)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	values, err := rdb.HGetAll(context.Background(), "position:42:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read split position: %v", err)
	}
	if values["yes_free_lots"] != "300" || values["no_free_lots"] != "300" {
		t.Fatalf("unexpected split position values: %+v", values)
	}
	account, err := rdb.HGetAll(context.Background(), "wallet-account:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read split wallet account: %v", err)
	}
	if account["collateral_free_units"] != "700" {
		t.Fatalf("unexpected split wallet account values: %+v", account)
	}
}

func TestMergeUpdatesProjectedPosition(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "100", "collateral_free_units", "100")
	mr.HSet("position:42:"+testWalletAddress(), "yes_free_lots", "500", "no_free_lots", "500")

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	market, _ := repo.Get(context.Background(), 42)
	market.Status = markets.MarketStatusResolved
	market.Outcome = markets.MarketOutcomeYes
	_ = repo.Update(context.Background(), market)
	server := New(config.Config{}, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)

	body := bytes.NewBufferString(`{"market_id":"42","collateral_mint":"mint","amount":200}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders/merge", body)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	values, err := rdb.HGetAll(context.Background(), "position:42:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read merge position: %v", err)
	}
	if values["yes_free_lots"] != "300" || values["no_free_lots"] != "300" {
		t.Fatalf("unexpected merge position values: %+v", values)
	}
	account, err := rdb.HGetAll(context.Background(), "wallet-account:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read merge wallet account: %v", err)
	}
	if account["collateral_free_units"] != "300" {
		t.Fatalf("unexpected merge wallet account values: %+v", account)
	}
}

func TestClaimUpdatesProjectedPosition(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(), "collateral_total_units", "50", "collateral_free_units", "50")
	mr.HSet("position:42:"+testWalletAddress(), "yes_free_lots", "400", "no_free_lots", "100")

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	market, _ := repo.Get(context.Background(), 42)
	market.Status = markets.MarketStatusResolved
	market.Outcome = markets.MarketOutcomeYes
	_ = repo.Update(context.Background(), market)
	server := New(config.Config{}, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)

	body := bytes.NewBufferString(`{"market_id":"42","collateral_mint":"mint","amount":150,"outcome":"yes"}`)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/claims", body)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	values, err := rdb.HGetAll(context.Background(), "position:42:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read claim position: %v", err)
	}
	if values["yes_free_lots"] != "250" {
		t.Fatalf("unexpected claim position values: %+v", values)
	}
	account, err := rdb.HGetAll(context.Background(), "wallet-account:"+testWalletAddress()).Result()
	if err != nil {
		t.Fatalf("read claim wallet account: %v", err)
	}
	if account["collateral_free_units"] != "200" {
		t.Fatalf("unexpected claim wallet account values: %+v", account)
	}
}

func TestGetPositionReturnsProjectedSnapshot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("position:42:"+testWalletAddress(),
		"yes_free_lots", "250",
		"yes_locked_lots", "10",
		"no_free_lots", "100",
		"no_locked_lots", "20",
		"collateral_free_units", "700",
		"collateral_locked_units", "30",
	)

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	server := New(config.Config{}, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/positions/42", nil)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var payload positionResponse
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.YesFreeLots != "250" || payload.NoLockedLots != "20" || payload.CollateralFreeUnits != "700" {
		t.Fatalf("unexpected position payload: %+v", payload)
	}
}

func TestGetWalletAccountReturnsProjectedSnapshot(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.HSet("wallet-account:"+testWalletAddress(),
		"collateral_total_units", "900",
		"collateral_free_units", "700",
	)

	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	server := New(config.Config{}, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, rdb, nil, nil, nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/wallet-account", nil)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var payload walletAccountResponse
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.CollateralTotalUnits != "900" || payload.CollateralFreeUnits != "700" {
		t.Fatalf("unexpected wallet account payload: %+v", payload)
	}
}

func TestRequiredCollateralUnitsUsesLotScaling(t *testing.T) {
	req := orders.PlaceOrderRequest{
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 60,
		OrderType:         "limit",
		QtyLots:           100,
	}
	required, err := requiredCollateralUnits(req)
	if err != nil {
		t.Fatalf("required collateral failed: %v", err)
	}
	if required != 60 {
		t.Fatalf("expected 60 amount units for 1 share @ 60c, got %d", required)
	}
}

func newTestServer() *Server {
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 1)
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID:   "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		AdminEmails: map[string]struct{}{"admin@example.com": {}},
		VUSDCMint:   "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	return New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, nil, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, nil, nil, nil, nil, nil)
}

func newTestServerWithPublisher(publisher protocol.CommandPublisher) *Server {
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 1)
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID:   "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		AdminEmails: map[string]struct{}{"admin@example.com": {}},
		VUSDCMint:   "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	return New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, nil, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, nil, nil, nil, nil, nil)
}

type noopTestListener struct{}

func (noopTestListener) Start(_ context.Context) error { return nil }
func (noopTestListener) Stop(_ context.Context) error  { return nil }

type staticWriteGate struct {
	ready bool
}

func (g staticWriteGate) OrdersReady() bool { return g.ready }
func (g staticWriteGate) Status() map[string]any {
	return map[string]any{
		"writer":              "ready",
		"matcher":             "ready",
		"pusher":              "ready",
		"settlement":          "init",
		"gateway_write_ready": g.ready,
	}
}

func adminToken(walletAddress string) string {
	payload := `{"sub":"did:privy:test","email":"admin@example.com","name":"Admin","linked_accounts":[{"chain_type":"solana","address":"` + walletAddress + `"}]}`
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func testSigningKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for idx := range seed {
		seed[idx] = byte(idx + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func testWalletAddress() string {
	pub := testSigningKey().Public().(ed25519.PublicKey)
	return gsolana.PublicKeyFromBytes(pub).String()
}

func TestReadyEndpointReturnsGateStatus(t *testing.T) {
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 1)
	cfg := config.Config{}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: true}, txreqs.NewStore(), faucet.DisabledService{}, protocol.DisabledCommandPublisher{}, nil, nil, nil, nil, nil, nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	var payload struct {
		Writer            string `json:"writer"`
		Matcher           string `json:"matcher"`
		Pusher            string `json:"pusher"`
		Settlement        string `json:"settlement"`
		GatewayWriteReady bool   `json:"gateway_write_ready"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.GatewayWriteReady || payload.Writer != "ready" {
		t.Fatalf("unexpected ready payload: %+v", payload)
	}
}

func TestPlaceOrderReturns503WhenWriteGateClosed(t *testing.T) {
	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{
		ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE",
		VUSDCMint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
	}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: false}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, nil, nil, nil, nil, nil)

	reqBody := mustSignPlaceOrderRequest(t, orders.PlaceOrderRequest{
		MarketID:          42,
		WalletAddress:     testWalletAddress(),
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 63,
		Side:              "buy",
		OrderType:         "limit",
		PriceTick:         63,
		QtyLots:           200,
		ExpireTime:        1_893_456_000,
	})
	rawBody, _ := json.Marshal(reqBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/orders", bytes.NewReader(rawBody))
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", recorder.Code)
	}
}

func TestCancelOrderReturns503WhenWriteGateClosed(t *testing.T) {
	publisher := &capturePublisher{}
	repo := markets.NewMemoryRepository()
	seedTestMarket(repo, 42)
	cfg := config.Config{}
	server := New(cfg, repo, matching.NewDisabledEngine(), noopTestListener{}, staticWriteGate{ready: false}, txreqs.NewStore(), faucet.DisabledService{}, publisher, nil, nil, nil, nil, nil, nil, nil)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/orders/123?market_id=42", nil)
	request.Header.Set("privy-id-token", adminToken(testWalletAddress()))
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", recorder.Code)
	}
}

func mustSignPlaceOrderRequest(t *testing.T, req orders.PlaceOrderRequest) orders.PlaceOrderRequest {
	t.Helper()
	if req.WalletAddress == "" {
		req.WalletAddress = testWalletAddress()
	}
	if req.OriginalAction == "" {
		req.OriginalAction = "buy"
	}
	if req.OriginalOutcome == "" {
		req.OriginalOutcome = "yes"
	}
	if req.OriginalPriceTick == 0 {
		req.OriginalPriceTick = req.PriceTick
		if req.OriginalPriceTick == 0 {
			req.OriginalPriceTick = 99
		}
	}
	if req.Nonce == 0 {
		req.Nonce = 1
	}

	pubkey, err := gsolana.PublicKeyFromBase58(req.WalletAddress)
	if err != nil {
		t.Fatalf("invalid test wallet: %v", err)
	}

	intent := &OrderIntentBorsh{
		ProgramID:         [32]byte{},
		WalletAddress:     [32]byte(pubkey.Bytes()),
		MarketID:          req.MarketID,
		OriginalAction:    mapSideToUint8(req.OriginalAction),
		OriginalOutcome:   mapOutcomeToUint8(req.OriginalOutcome),
		OriginalPriceTick: req.OriginalPriceTick,
		Side:              mapSideToUint8(req.Side),
		OrderType:         mapOrderTypeToUint8(req.OrderType),
		PriceTick:         req.PriceTick,
		QtyLots:           req.QtyLots,
		SpendAmount:       req.SpendAmount,
		ExpireTime:        req.ExpireTime,
		Nonce:             req.Nonce,
	}
	intentBytes, err := serializeOrderIntentBorsh(intent)
	if err != nil {
		t.Fatalf("serialize intent: %v", err)
	}
	hash := hashOrderIntent(intentBytes)
	signMessage := buildOrderSignatureMessage(hash)
	signature := ed25519.Sign(testSigningKey(), signMessage)
	if !ed25519.Verify(ed25519.PublicKey(pubkey.Bytes()), signMessage, signature) {
		t.Fatalf("ed25519 verify failed for test helper")
	}
	rebuild := &OrderIntentBorsh{
		ProgramID:         [32]byte{},
		WalletAddress:     [32]byte(pubkey.Bytes()),
		MarketID:          req.MarketID,
		OriginalAction:    mapSideToUint8(req.OriginalAction),
		OriginalOutcome:   mapOutcomeToUint8(req.OriginalOutcome),
		OriginalPriceTick: req.OriginalPriceTick,
		Side:              mapSideToUint8(req.Side),
		OrderType:         mapOrderTypeToUint8(req.OrderType),
		PriceTick:         req.PriceTick,
		QtyLots:           req.QtyLots,
		SpendAmount:       req.SpendAmount,
		ExpireTime:        req.ExpireTime,
		Nonce:             req.Nonce,
	}
	rebuildBytes, err := serializeOrderIntentBorsh(rebuild)
	if err != nil {
		t.Fatalf("serialize rebuild intent: %v", err)
	}
	if string(intentBytes) != string(rebuildBytes) {
		t.Fatalf("intent bytes mismatch between helper and rebuild")
	}
	req.Signature = base64.StdEncoding.EncodeToString(signature)
	decoded, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	rebuildHash := hashOrderIntent(rebuildBytes)
	if !ed25519.Verify(ed25519.PublicKey(pubkey.Bytes()), buildOrderSignatureMessage(rebuildHash), decoded) {
		t.Fatalf("ed25519 verify failed after base64 roundtrip")
	}
	if _, _, err := validateBorshOrderIntent(req, req.WalletAddress); err != nil {
		t.Fatalf("test helper produced invalid signed request: %v", err)
	}
	return req
}

func hashOrderIntent(intentBytes []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(intentBytes)
	return h.Sum(nil)
}

var _ indexer.Listener = noopTestListener{}

type capturePublisher struct {
	placePayload  protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	cancelPayload protocol.CommandEnvelope[protocol.CancelOrderCommand]
}

func (p *capturePublisher) PublishPlaceOrder(_ context.Context, env protocol.CommandEnvelope[protocol.PlaceOrderCommand]) error {
	p.placePayload = env
	return nil
}

func (p *capturePublisher) PublishCancelOrder(_ context.Context, env protocol.CommandEnvelope[protocol.CancelOrderCommand]) error {
	p.cancelPayload = env
	return nil
}

func seedTestMarket(repo *markets.MemoryRepository, marketID uint64) {
	now := time.Now().UTC()
	_ = repo.Save(context.Background(), markets.Market{
		ID:              "seed-market",
		MarketID:        marketID,
		MarketPDA:       "seed-pda",
		MetadataURL:     "https://example.com/seed.json",
		CollateralMint:  "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
		CollateralVault: "seed-vault",
		YesMint:         "seed-yes",
		NoMint:          "seed-no",
		Title:           "Seed Market",
		Description:     "seed",
		Category:        "seed",
		ImageURL:        "",
		Status:          markets.MarketStatusOpen,
		Outcome:         markets.MarketOutcomeUndecided,
		Resolution: markets.ResolutionConfig{
			Mode:      markets.ResolutionModeCreator,
			Authority: "Resolver111111111111111111111111111111111",
		},
		CloseTime: now.Add(24 * time.Hour),
		CreatedAt: now,
		UpdatedAt: now,
	})
}
