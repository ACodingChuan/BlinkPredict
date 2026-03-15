package httpapi

import (
	"bytes"
	"context"
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
	"blinkpredict/banckend/internal/txreqs"
)

func TestMatchingEndpointsReturnDisabledContract(t *testing.T) {
	server := newTestServer()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/orderbook/1", nil)
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
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
	request.Header.Set("privy-id-token", adminToken())
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
	request.Header.Set("privy-id-token", adminToken())
	server.Router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", recorder.Code)
	}
}

func newTestServer() *Server {
	cfg := config.Config{ProgramID: "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE", AdminEmails: map[string]struct{}{"admin@example.com": {}}}
	return New(cfg, markets.NewMemoryRepository(), matching.NewDisabledEngine(), noopTestListener{}, txreqs.NewStore(), faucet.DisabledService{})
}

type noopTestListener struct{}

func (noopTestListener) Start(_ context.Context) error { return nil }
func (noopTestListener) Stop(_ context.Context) error  { return nil }

func adminToken() string {
	payload := `{"sub":"did:privy:test","email":"admin@example.com","name":"Admin","linked_accounts":[{"chain_type":"solana","address":"So11111111111111111111111111111111111111112"}]}`
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

var _ indexer.Listener = noopTestListener{}
