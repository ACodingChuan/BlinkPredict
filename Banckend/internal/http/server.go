package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blinkpredict/banckend/internal/auth"
	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/faucet"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/orders"
	"blinkpredict/banckend/internal/solana"
	"blinkpredict/banckend/internal/txreqs"

	gsolana "github.com/gagliardetto/solana-go"
	"github.com/go-chi/cors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type Server struct {
	cfg        config.Config
	markets    markets.Repository
	matching   matching.Engine
	listener   indexer.Listener
	txRequests *txreqs.Store
	faucet     faucet.Service
}

func New(cfg config.Config, repo markets.Repository, engine matching.Engine, listener indexer.Listener, txStore *txreqs.Store, faucetSvc faucet.Service) *Server {
	if faucetSvc == nil {
		faucetSvc = faucet.DisabledService{}
	}
	return &Server{cfg: cfg, markets: repo, matching: engine, listener: listener, txRequests: txStore, faucet: faucetSvc}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	// CORS for the Next.js dev server (http://localhost:3000). We must allow preflight
	// OPTIONS for custom headers like "privy-id-token", otherwise the browser blocks the request.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000", "http://127.0.0.1:3000"},
		AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "privy-id-token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Use(auth.Middleware(s.cfg))

	r.Get("/api/health", s.handleHealth)
	r.Get("/api/markets", s.handleListMarkets)
	r.Get("/api/markets/{marketId}", s.handleGetMarket)
	r.Get("/api/orderbook/{marketId}", s.handleOrderbook)
	r.Get("/api/orders/open/{marketId}", s.handleOpenOrders)
	r.Get("/api/trades/{marketId}", s.handleTrades)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireUser)
		// Market creation is open to any authenticated user in the new product model.
		r.Post("/api/markets", s.handleCreateMarket)
		r.Post("/api/faucet/claim", s.handleFaucetClaim)
		r.Post("/api/markets/delegate", s.handleDelegate)
		r.Post("/api/orders/split", s.handleSplit)
		r.Post("/api/orders/merge", s.handleMerge)
		r.Post("/api/claims", s.handleClaim)
		r.Post("/api/orders", s.handlePlaceOrder)
		r.Delete("/api/orders/{orderId}", s.handleCancelOrder)
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin)
		r.Post("/api/admin/markets/{marketId}/resolve", s.handleResolveCreator)
		r.Post("/api/admin/markets/{marketId}/trigger-oracle-resolve", s.handleResolvePyth)
	})

	return r
}

func (s *Server) handleFaucetClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WalletAddress string `json:"wallet_address"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	user, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	walletAddress := user.SolanaAddress
	if walletAddress == "" && req.WalletAddress != "" {
		walletAddress = strings.TrimSpace(req.WalletAddress)
	}
	if walletAddress == "" {
		rawToken := authTokenFromRequest(r)
		if rawToken != "" {
			resolved, err := s.resolveSolanaAddressFromPrivy(r.Context(), rawToken, r.Header.Get("Origin"))
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unable to resolve wallet from privy token: "+err.Error())
				return
			}
			walletAddress = resolved
		}
	}
	if walletAddress == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if _, err := gsolana.PublicKeyFromBase58(walletAddress); err != nil {
		writeError(w, http.StatusBadRequest, "invalid wallet_address")
		return
	}

	result, err := s.faucet.Claim(r.Context(), walletAddress, faucet.ClientIP(r))
	if err != nil {
		var rate faucet.RateLimitError
		if errors.As(err, &rate) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"message":         rate.Error(),
				"next_allowed_at": rate.NextAllowedAt.Format(time.RFC3339),
			})
			return
		}
		if errors.Is(err, faucet.ErrFaucetNotConfigured) {
			writeError(w, http.StatusNotImplemented, "faucet not configured")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":   "vUSDC faucet claim submitted",
		"signature": result.Signature,
		"mint":      result.Mint,
		"ata":       result.ATA,
		"amount":    result.Amount,
		"claimed_at": result.ClaimedAt.Format(time.RFC3339),
	})
}

func authTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("privy-id-token")); token != "" {
		return token
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return strings.TrimSpace(authHeader[len("Bearer "):])
	}
	return ""
}

func (s *Server) resolveSolanaAddressFromPrivy(ctx context.Context, token string, origin string) (string, error) {
	appID := strings.TrimSpace(s.cfg.PrivyAppID)
	if appID == "" {
		appID = parsePrivyAppIDFromToken(token)
	}
	if appID == "" {
		return "", errors.New("missing privy app id")
	}

	if strings.TrimSpace(origin) == "" {
		origin = "http://localhost:3000"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://auth.privy.io/api/v1/users/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("privy-app-id", appID)
	req.Header.Set("Origin", origin)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("privy /users/me status=%d", resp.StatusCode)
	}

	var payload struct {
		User struct {
			LinkedAccounts []struct {
				Type      string `json:"type"`
				Address   string `json:"address"`
				ChainType string `json:"chain_type"`
			} `json:"linked_accounts"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	for _, account := range payload.User.LinkedAccounts {
		if strings.EqualFold(account.Type, "wallet") && strings.EqualFold(account.ChainType, "solana") && account.Address != "" {
			return account.Address, nil
		}
	}
	return "", errors.New("no solana wallet in privy profile")
}

func parsePrivyAppIDFromToken(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var decoded struct {
		AppID string `json:"aid"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	return strings.TrimSpace(decoded.AppID)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "blinkpredict-banckend-v1a"})
}

func (s *Server) handleListMarkets(w http.ResponseWriter, r *http.Request) {
	items, err := s.markets.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"markets": items})
}

func (s *Server) handleGetMarket(w http.ResponseWriter, r *http.Request) {
	marketID, err := parseMarketID(chi.URLParam(r, "marketId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, err := s.markets.Get(r.Context(), marketID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, markets.ErrMarketNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"market": item})
}

func (s *Server) handleCreateMarket(w http.ResponseWriter, r *http.Request) {
	var req markets.CreateMarketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateCreateMarket(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	programID := gsolana.MustPublicKeyFromBase58(s.cfg.ProgramID)
	marketID := solana.StableMarketID(req.Title + req.MetadataURL + time.Now().UTC().String())
	derived, err := solana.DeriveAddresses(programID, marketID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	market := markets.Market{
		ID:              uuid.NewString(),
		MarketID:        marketID,
		MarketPDA:       derived.MarketPDA.String(),
		MetadataURL:     req.MetadataURL,
		CollateralMint:  coalesce(req.CollateralMint, s.cfg.DefaultCollateral),
		CollateralVault: derived.CollateralVault.String(),
		YesMint:         derived.YesMint.String(),
		NoMint:          derived.NoMint.String(),
		Title:           req.Title,
		Description:     req.Description,
		Category:        req.Category,
		ImageURL:        req.ImageURL,
		Status:          markets.MarketStatusOpen,
		Outcome:         markets.MarketOutcomeUndecided,
		Resolution:      req.Resolution,
		CloseTime:       req.CloseTime,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.markets.Save(r.Context(), market); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tx := s.txRequests.Create("create_market", marketID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"market":     market,
		"tx_message": "",
		"message":    "Market saved in v1a skeleton. Contract tx builder slots in here next.",
		"tx_request": tx,
	})
}

func (s *Server) handleResolveCreator(w http.ResponseWriter, r *http.Request) {
	marketID, err := parseMarketID(chi.URLParam(r, "marketId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req markets.ResolveMarketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	market, err := s.markets.Get(r.Context(), marketID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	market.Outcome = req.Outcome
	market.Status = markets.MarketStatusResolved
	now := time.Now().UTC()
	market.ResolvedAt = &now
	market.UpdatedAt = now
	if err := s.markets.Update(r.Context(), market); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tx := s.txRequests.Create("resolve_by_creator", marketID)
	writeJSON(w, http.StatusOK, map[string]any{"market": market, "tx_message": "", "message": "Creator resolution recorded in v1a skeleton.", "tx_request": tx})
}

func (s *Server) handleResolvePyth(w http.ResponseWriter, r *http.Request) {
	marketID, err := parseMarketID(chi.URLParam(r, "marketId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	market, err := s.markets.Get(r.Context(), marketID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	tx := s.txRequests.Create("resolve_by_pyth", marketID)
	writeJSON(w, http.StatusOK, map[string]any{"market": market, "tx_message": "", "message": "Pyth trigger endpoint is scaffolded; on-chain oracle resolution plugs in here next.", "tx_request": tx})
}

func (s *Server) handleDelegate(w http.ResponseWriter, r *http.Request) {
	var req orders.DelegateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_ = req
	tx := s.txRequests.Create("delegate", req.MarketID)
	writeJSON(w, http.StatusOK, orders.TransactionEnvelope{TxMessage: "", Message: fmt.Sprintf("Delegate transaction scaffolded for market %d.", req.MarketID), Disabled: true, Code: "transaction_builder_pending"})
	_ = tx
}

func (s *Server) handleSplit(w http.ResponseWriter, r *http.Request) {
	s.handleTokenAction(w, r, "split")
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	s.handleTokenAction(w, r, "merge")
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	s.handleTokenAction(w, r, "claim")
}

func (s *Server) handleTokenAction(w http.ResponseWriter, r *http.Request, kind string) {
	var req orders.TokenActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	s.txRequests.Create(kind, req.MarketID)
	writeJSON(w, http.StatusOK, orders.TransactionEnvelope{TxMessage: "", Message: fmt.Sprintf("%s transaction scaffolded for v1a.", kind), Disabled: true, Code: "transaction_builder_pending"})
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req orders.PlaceOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Quantity <= 0 {
		writeError(w, http.StatusBadRequest, "qty must be greater than 0")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{"code": matching.ErrMatchingDisabled, "message": "Matching module is intentionally disabled in v1a."})
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"code": matching.ErrMatchingDisabled, "message": "Matching module is intentionally disabled in v1a."})
}

func (s *Server) handleOrderbook(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	writeJSON(w, http.StatusOK, s.matching.GetOrderbook(r.Context(), marketID))
}

func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	user, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"orders": s.matching.GetOpenOrders(r.Context(), user.SolanaAddress, marketID), "matching_enabled": false})
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	writeJSON(w, http.StatusOK, map[string]any{"trades": s.matching.GetTrades(r.Context(), marketID), "matching_enabled": false})
}

func validateCreateMarket(req markets.CreateMarketRequest) error {
	if req.Title == "" {
		return errors.New("title is required")
	}
	if req.CloseTime.IsZero() {
		return errors.New("close_time is required")
	}
	switch req.Resolution.Mode {
	case markets.ResolutionModeCreator:
		if req.Resolution.Authority == "" {
			return errors.New("resolution.authority is required for creator markets")
		}
	case markets.ResolutionModePyth:
		if req.Resolution.OracleFeed == "" {
			return errors.New("resolution.oracle_feed is required for pyth markets")
		}
		if req.Resolution.ObservationTime.IsZero() {
			return errors.New("resolution.oracle_observation_time is required for pyth markets")
		}
	default:
		return errors.New("resolution.mode must be creator or pyth")
	}
	return nil
}

func parseMarketID(value string) (uint64, error) {
	marketID, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid market id: %w", err)
	}
	return marketID, nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func coalesce(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
