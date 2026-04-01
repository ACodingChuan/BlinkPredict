package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	docs "blinkpredict/banckend/docs"
	"blinkpredict/banckend/internal/auth"
	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/faucet"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/orders"
	"blinkpredict/banckend/internal/protocol"
	"blinkpredict/banckend/internal/pusher"
	"blinkpredict/banckend/internal/settlement"
	"blinkpredict/banckend/internal/solana"
	"blinkpredict/banckend/internal/txreqs"
	"blinkpredict/banckend/internal/webhooks"

	gsolana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/sony/sonyflake"
	httpSwagger "github.com/swaggo/http-swagger/v2"
)

const orderIntentVersion = "blinkpredict.order.v1"

var (
	errOrderSignatureInvalid = errors.New("invalid order signature")
	errOrderWalletMismatch   = errors.New("wallet signature does not match authenticated wallet")
	errNoMarketLiquidity     = errors.New("insufficient market liquidity")
	errInsufficientFunds     = errors.New("insufficient vUSDC balance")
)

type Server struct {
	cfg       config.Config
	markets   markets.Repository
	matching  matching.Engine
	listener  indexer.Listener
	writeGate interface {
		OrdersReady() bool
		Status() map[string]any
	}
	txRequests            *txreqs.Store
	faucet                faucet.Service
	commands              protocol.CommandPublisher
	pusherHub             *pusher.Hub
	sonyflake             *sonyflake.Sonyflake
	marketCache           *cache.MarketCache
	redisClient           *redis.Client
	dbPool                *pgxpool.Pool
	rpcClient             *rpc.Client
	webhookHandler        *webhooks.HeliusHandler
	alchemyWebhookHandler *webhooks.AlchemyHandler
	logger                *zerolog.Logger
	sessions              *auth.SessionManager
}

func New(cfg config.Config, repo markets.Repository, engine matching.Engine, listener indexer.Listener, writeGate interface {
	OrdersReady() bool
	Status() map[string]any
}, txStore *txreqs.Store, faucetSvc faucet.Service, cmdPublisher protocol.CommandPublisher, pusherHub *pusher.Hub, marketCache *cache.MarketCache, redisClient *redis.Client, dbPool *pgxpool.Pool, webhookHandler *webhooks.HeliusHandler, alchemyHandler *webhooks.AlchemyHandler, logger *zerolog.Logger) *Server {
	if faucetSvc == nil {
		faucetSvc = faucet.DisabledService{}
	}
	if cmdPublisher == nil {
		cmdPublisher = protocol.DisabledCommandPublisher{}
	}
	sessions, _ := auth.NewSessionManager(cfg)
	// 初始化 Sonyflake 雪花ID生成器
	sf := sonyflake.NewSonyflake(sonyflake.Settings{})
	return &Server{
		cfg: cfg, markets: repo, matching: engine, listener: listener,
		writeGate: writeGate, txRequests: txStore, faucet: faucetSvc, commands: cmdPublisher,
		pusherHub: pusherHub, sonyflake: sf, marketCache: marketCache, redisClient: redisClient, dbPool: dbPool,
		rpcClient:      rpc.New(cfg.SolanaRPCURL),
		webhookHandler: webhookHandler, alchemyWebhookHandler: alchemyHandler, logger: logger, sessions: sessions,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000", "http://127.0.0.1:3000"},
		AllowedMethods:   []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "Idempotency-Key", "X-Trace-Id"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Use(auth.Middleware(s.cfg, s.sessions))

	r.Get("/api/health", s.handleHealth)
	r.Get("/api/ready", s.handleReady)
	r.Post("/api/auth/challenge", s.handleAuthChallenge)
	r.Post("/api/auth/login", s.handleAuthLogin)
	r.Get("/api/openapi.json", s.handleOpenAPISpec)
	r.Get("/api/docs", s.handleOpenAPIDocs)
	r.Get("/swagger/*", httpSwagger.Handler(
		httpSwagger.URL("/swagger/doc.json"),
	))
	r.Get("/api/markets", s.handleListMarkets)
	r.Get("/api/markets/{marketId}", s.handleGetMarket)
	r.Get("/api/orderbook/{marketId}", s.handleOrderbook)
	r.Get("/api/positions/{marketId}", s.handlePosition)
	r.Get("/api/wallet-account", s.handleWalletAccount)
	r.Get("/api/orders/open/{marketId}", s.handleOpenOrders)
	r.Get("/api/trades/{marketId}", s.handleTrades)
	r.Get("/api/price-history/{marketId}", s.handlePriceHistory)
	r.Get("/ws/markets/{marketId}", s.handleMarketOrderbookWS)
	r.Get("/ws/orders", s.handleUserOrdersWS)
	r.Get("/ws/users/me", s.handleUserOrdersWS)

	// Helius Webhook (不需要认证)
	r.Post("/api/webhooks/helius", s.handleHeliusWebhook)
	// Alchemy Webhook (不需要认证)
	r.Post("/api/webhooks/alchemy", s.handleAlchemyWebhook)

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
		r.Post("/api/ws-ticket", s.handleCreateWSTicket)
		r.Delete("/api/orders/{orderId}", s.handleCancelOrder)
	})

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAdmin)
		r.Post("/api/admin/markets/{marketId}/resolve", s.handleResolveCreator)
		r.Post("/api/admin/markets/{marketId}/trigger-oracle-resolve", s.handleResolvePyth)
	})

	return r
}

func init() {
	docs.SwaggerInfo.BasePath = "/"
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
	account, accountErr := s.loadWalletAccountSnapshotFromDB(r.Context(), walletAddress)
	if accountErr == nil {
		_ = s.upsertWalletAccountSnapshot(r.Context(), walletAddress, account)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":    "vUSDC faucet claim submitted",
		"signature":  result.Signature,
		"mint":       result.Mint,
		"ata":        result.ATA,
		"amount":     result.Amount,
		"claimed_at": result.ClaimedAt.Format(time.RFC3339),
		"trading_account": map[string]any{
			"collateral_total_units": account.CollateralTotalUnits,
			"collateral_free_units":  account.CollateralFreeUnits,
		},
	})
}

func (s *Server) handleAuthChallenge(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusNotImplemented, "auth session manager is not configured")
		return
	}
	var req struct {
		WalletAddress string `json:"wallet_address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	challenge, err := s.sessions.CreateChallenge(req.WalletAddress)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": challenge.ID,
		"message":      challenge.Message,
		"expires_at":   challenge.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusNotImplemented, "auth session manager is not configured")
		return
	}
	var req struct {
		WalletAddress string `json:"wallet_address"`
		ChallengeID   string `json:"challenge_id"`
		Signature     string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	user, token, expiresAt, err := s.sessions.VerifyChallenge(req.ChallengeID, req.WalletAddress, req.Signature)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_token": token,
		"expires_at": expiresAt.Format(time.RFC3339),
		"user": map[string]any{
			"walletAddress": user.SolanaAddress,
			"isAdmin":       user.IsAdmin,
		},
	})
}

// handleHealth godoc
// @Summary Health check
// @Tags System
// @Produce json
// @Success 200 {object} healthResponse
// @Router /api/health [get]
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "blinkpredict-banckend-v1a"})
}

// handleReady godoc
// @Summary Readiness status
// @Tags System
// @Produce json
// @Success 200 {object} readyResponse
// @Router /api/ready [get]
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.writeGate == nil {
		writeJSON(w, http.StatusOK, readyResponse{
			Writer:            "init",
			Matcher:           "init",
			Pusher:            "init",
			Settlement:        "init",
			GatewayWriteReady: false,
		})
		return
	}
	writeJSON(w, http.StatusOK, s.writeGate.Status())
}

// handleListMarkets godoc
// @Summary List markets
// @Tags Markets
// @Produce json
// @Success 200 {object} marketsResponse
// @Failure 500 {object} errorResponse
// @Router /api/markets [get]
func (s *Server) handleListMarkets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Redis-first: try to serve from cache
	if s.marketCache != nil {
		ids, err := s.marketCache.GetMarketsByStatus(ctx, "all", 1, 1000)
		if err == nil && len(ids) > 0 {
			cached := make([]markets.Market, 0, len(ids))
			allHit := true
			for _, id := range ids {
				md, err := s.marketCache.GetMarket(ctx, id)
				if err != nil {
					allHit = false
					break
				}
				cached = append(cached, marketDataToMarket(md))
			}
			if allHit {
				writeJSON(w, http.StatusOK, map[string]any{"markets": cached})
				return
			}
		}
	}

	// DB fallback
	items, err := s.markets.List(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Write-back to Redis
	if s.marketCache != nil {
		for _, m := range items {
			_ = s.marketCache.SetMarket(ctx, marketToMarketData(m))
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"markets": items})
}

func marketToMarketData(m markets.Market) cache.MarketData {
	return cache.MarketData{
		ID:                   m.ID,
		MarketID:             m.MarketID,
		MarketPDA:            m.MarketPDA,
		MetadataCID:          m.MetadataCID,
		MetadataURL:          m.MetadataURL,
		CollateralMint:       m.CollateralMint,
		Title:                m.Title,
		Description:          m.Description,
		Category:             m.Category,
		ImageURL:             m.ImageURL,
		Status:               string(m.Status),
		Outcome:              string(m.Outcome),
		ResolutionMode:       string(m.Resolution.Mode),
		ResolutionAuthority:  m.Resolution.Authority,
		OracleFeed:           m.Resolution.OracleFeed,
		OracleCondition:      string(m.Resolution.OracleCondition),
		OracleTargetPrice:    int64(m.Resolution.OracleTarget),
		OracleTargetExpo:     m.Resolution.OracleTargetExpo,
		CloseTime:            m.CloseTime.Unix(),
		ResolveAfterTime:     m.ResolveAfterTime.Unix(),
		ClaimDeadlineTime:    m.ClaimDeadlineTime.Unix(),
		CreatorUnclaimedFee:  m.CreatorUnclaimedFee,
		PlatformUnclaimedFee: m.PlatformUnclaimedFee,
		CreatedAt:            m.CreatedAt.Unix(),
		UpdatedAt:            m.UpdatedAt.Unix(),
	}
}

func marketDataToMarket(md *cache.MarketData) markets.Market {
	return markets.Market{
		ID:             md.ID,
		MarketID:       md.MarketID,
		MarketPDA:      md.MarketPDA,
		MetadataCID:    md.MetadataCID,
		MetadataURL:    md.MetadataURL,
		CollateralMint: md.CollateralMint,
		Title:          md.Title,
		Description:    md.Description,
		Category:       md.Category,
		ImageURL:       md.ImageURL,
		Status:         markets.MarketStatus(md.Status),
		Outcome:        markets.MarketOutcome(md.Outcome),
		Resolution: markets.ResolutionConfig{
			Mode:             markets.ResolutionMode(md.ResolutionMode),
			Authority:        md.ResolutionAuthority,
			OracleFeed:       md.OracleFeed,
			OracleCondition:  markets.OracleCondition(md.OracleCondition),
			OracleTarget:     uint64(md.OracleTargetPrice),
			OracleTargetExpo: md.OracleTargetExpo,
		},
		CloseTime:            time.Unix(md.CloseTime, 0),
		ResolveAfterTime:     time.Unix(md.ResolveAfterTime, 0),
		ClaimDeadlineTime:    time.Unix(md.ClaimDeadlineTime, 0),
		CreatorUnclaimedFee:  md.CreatorUnclaimedFee,
		PlatformUnclaimedFee: md.PlatformUnclaimedFee,
		CreatedAt:            time.Unix(md.CreatedAt, 0),
		UpdatedAt:            time.Unix(md.UpdatedAt, 0),
	}
}

// handleGetMarket godoc
// @Summary Get market
// @Tags Markets
// @Produce json
// @Param marketId path string true "Market ID"
// @Success 200 {object} marketResponse
// @Failure 400 {object} errorResponse
// @Failure 404 {object} errorResponse
// @Router /api/markets/{marketId} [get]
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

// handleCreateMarket godoc
// @Summary Create market
// @Tags Markets
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body markets.CreateMarketRequest true "Create market payload"
// @Success 201 {object} marketResponse
// @Failure 400 {object} errorResponse
// @Failure 500 {object} errorResponse
// @Router /api/markets [post]
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
	marketID := solana.StableMarketID(req.Title + req.MetadataCID + req.CloseTime.UTC().String())
	marketPDA, err := solana.DeriveMarketPDA(programID, marketID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	collateralMint := strings.TrimSpace(s.cfg.VUSDCMint)
	if collateralMint == "" {
		writeError(w, http.StatusInternalServerError, "VUSDC_MINT is required for market creation")
		return
	}
	now := time.Now().UTC()
	entityID, err := s.nextSnowflakeID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate market entity ID")
		return
	}
	market := markets.Market{
		ID:                entityID,
		MarketID:          marketID,
		MarketPDA:         marketPDA.String(),
		MetadataCID:       req.MetadataCID,
		MetadataURL:       req.MetadataURL,
		CollateralMint:    collateralMint,
		Title:             req.Title,
		Description:       req.Description,
		Category:          req.Category,
		ImageURL:          req.ImageURL,
		Status:            markets.MarketStatusOpen,
		Outcome:           markets.MarketOutcomeUndecided,
		Resolution:        req.Resolution,
		CloseTime:         req.CloseTime,
		ResolveAfterTime:  req.ResolveAfterTime,
		ClaimDeadlineTime: req.ClaimDeadlineTime,
		CreatedAt:         now,
		UpdatedAt:         now,
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
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	market, err := s.markets.Get(r.Context(), req.MarketID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	position, err := s.getOrInitPositionSnapshot(r.Context(), market, walletAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	account, err := s.getOrInitWalletAccountSnapshot(r.Context(), walletAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if kind == "claim" {
		if market.Status != markets.MarketStatusResolved {
			writeError(w, http.StatusConflict, "market must be resolved before claim")
			return
		}
		if req.Outcome == "" {
			req.Outcome = string(market.Outcome)
		}
	}
	nextPosition, nextAccount, err := applyTokenAction(kind, position, account, req)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.upsertPositionSnapshot(r.Context(), req.MarketID, walletAddress, nextPosition); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.upsertWalletAccountSnapshot(r.Context(), walletAddress, nextAccount); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.txRequests.Create(kind, req.MarketID)
	writeJSON(w, http.StatusOK, orders.TransactionEnvelope{TxMessage: "", Message: fmt.Sprintf("%s applied to projected positions.", kind), Disabled: true, Code: "projected_position_updated"})
}

func applyTokenAction(kind string, current positionSnapshot, account walletAccountSnapshot, req orders.TokenActionRequest) (positionSnapshot, walletAccountSnapshot, error) {
	nextPosition := current
	nextAccount := account
	switch kind {
	case "split":
		if account.CollateralFreeUnits < req.Amount {
			return positionSnapshot{}, walletAccountSnapshot{}, errors.New("insufficient collateral to split")
		}
		nextAccount.CollateralFreeUnits -= req.Amount
		nextPosition.YesFreeLots += req.Amount
		nextPosition.NoFreeLots += req.Amount
	case "merge":
		if current.YesFreeLots < req.Amount || current.NoFreeLots < req.Amount {
			return positionSnapshot{}, walletAccountSnapshot{}, errors.New("insufficient yes/no lots to merge")
		}
		nextPosition.YesFreeLots -= req.Amount
		nextPosition.NoFreeLots -= req.Amount
		nextAccount.CollateralFreeUnits += req.Amount
	case "claim":
		outcome := strings.ToLower(strings.TrimSpace(req.Outcome))
		switch outcome {
		case "yes":
			if current.YesFreeLots < req.Amount {
				return positionSnapshot{}, walletAccountSnapshot{}, errors.New("insufficient yes lots to claim")
			}
			nextPosition.YesFreeLots -= req.Amount
			nextAccount.CollateralFreeUnits += req.Amount
		case "no":
			if current.NoFreeLots < req.Amount {
				return positionSnapshot{}, walletAccountSnapshot{}, errors.New("insufficient no lots to claim")
			}
			nextPosition.NoFreeLots -= req.Amount
			nextAccount.CollateralFreeUnits += req.Amount
		default:
			return positionSnapshot{}, walletAccountSnapshot{}, errors.New("claim outcome must be yes or no")
		}
	default:
		return positionSnapshot{}, walletAccountSnapshot{}, errors.New("unsupported token action")
	}
	return nextPosition, nextAccount, nil
}

// handlePlaceOrder godoc
// @Summary Place order
// @Tags Orders
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body orders.PlaceOrderRequest true "Place order payload"
// @Success 202 {object} placeOrderAcceptedResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Failure 404 {object} errorResponse
// @Failure 501 {object} map[string]any
// @Router /api/orders [post]
func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	if s.writeGate != nil && !s.writeGate.OrdersReady() {
		writeError(w, http.StatusServiceUnavailable, "system bootstrap in progress")
		return
	}
	var req orders.PlaceOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	walletHint := req.WalletAddress
	if walletHint == "" {
		walletHint = req.User
	}
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, walletHint)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	market, err := s.resolveMarketForOrder(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, markets.ErrMarketNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	var intentBytes []byte
	req, intentBytes, err = s.normalizePlaceOrderRequest(req, market, walletAddress)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOrderWalletMismatch) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error())
		return
	}
	if err := s.precheckPlaceOrder(r.Context(), req, market, walletAddress); err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, errNoMarketLiquidity):
			status = http.StatusConflict
		case errors.Is(err, errInsufficientFunds):
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}

	// 生成雪花 ID
	orderID, err := s.sonyflake.NextID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate order ID")
		return
	}

	// 构建新的 PlaceOrderCommand
	cmd := buildPlaceOrderCommandV1(req, intentBytes, orderID)

	// 验证命令
	if err := protocol.ValidatePlaceOrderCommand(cmd); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 幂等 key 与 trace id 均要求前端显式传入，且使用雪花算法生成。
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}
	traceID := strings.TrimSpace(r.Header.Get("X-Trace-Id"))
	if traceID == "" {
		writeError(w, http.StatusBadRequest, "X-Trace-Id header is required")
		return
	}

	commandID, err := s.nextSnowflakeID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate command ID")
		return
	}
	cmd.CommandID = commandID
	cmd.TraceID = traceID
	cmd.IdempotencyKey = idempotencyKey

	envelope := protocol.CommandEnvelope[protocol.PlaceOrderCommand]{
		ID:             commandID,
		Type:           protocol.CommandTypePlaceOrder,
		SchemaVersion:  1,
		MarketID:       req.MarketID,
		Producer:       "gateway",
		TraceID:        traceID,
		IdempotencyKey: idempotencyKey,
		CreatedAt:      time.Now().UTC(),
		Payload:        cmd,
	}

	if err := s.commands.PublishPlaceOrder(r.Context(), envelope); err != nil {
		status := http.StatusInternalServerError
		message := "failed to publish place-order command"
		code := "command_publish_failed"
		if errors.Is(err, protocol.ErrCommandBusDisabled) {
			status = http.StatusNotImplemented
			message = "command bus is not configured"
			code = "command_bus_not_configured"
		}

		writeJSON(w, status, map[string]any{"code": code, "message": message})
		return
	}

	tx := s.txRequests.Create("place_order", req.MarketID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message":         "order command accepted",
		"command_id":      envelope.ID,
		"market_id":       strconv.FormatUint(envelope.MarketID, 10),
		"order_id":        strconv.FormatUint(cmd.Execution.OrderID, 10),
		"idempotency_key": envelope.IdempotencyKey,
		"tx_request":      tx,
	})
}

func (s *Server) resolveMarketForOrder(ctx context.Context, req orders.PlaceOrderRequest) (markets.Market, error) {
	if req.MarketID != 0 {
		return s.markets.Get(ctx, req.MarketID)
	}
	raw := strings.TrimSpace(req.Market)
	if raw == "" {
		return markets.Market{}, errors.New("market or market_id is required")
	}
	list, err := s.markets.List(ctx)
	if err != nil {
		return markets.Market{}, err
	}
	for _, market := range list {
		if strings.EqualFold(strings.TrimSpace(market.MarketPDA), raw) {
			return market, nil
		}
	}
	return markets.Market{}, markets.ErrMarketNotFound
}

func (s *Server) normalizePlaceOrderRequest(req orders.PlaceOrderRequest, market markets.Market, authenticatedWallet string) (orders.PlaceOrderRequest, []byte, error) {
	if strings.TrimSpace(req.User) != strings.TrimSpace(authenticatedWallet) {
		return req, nil, errOrderWalletMismatch
	}
	if req.Signature == "" {
		return req, nil, errors.New("signature is required")
	}
	if req.Version == 0 {
		req.Version = 1
	}
	if req.OrderType != "limit" && req.OrderType != "market" {
		return req, nil, errors.New("order_type must be limit or market")
	}
	if req.Side != "buy" && req.Side != "sell" {
		return req, nil, errors.New("side must be buy or sell")
	}
	if req.Outcome != "yes" && req.Outcome != "no" {
		return req, nil, errors.New("outcome must be yes or no")
	}
	req.MarketID = market.MarketID
	req.WalletAddress = req.User
	req.OriginalAction = req.Side
	req.OriginalOutcome = req.Outcome
	normalizedSide, normalizedPriceTick, err := normalizeToYesBook(req.Side, req.Outcome, req.LimitPrice)
	if err != nil {
		return req, nil, err
	}
	req.OriginalPriceTick = uint8(req.LimitPrice)
	req.NormalizedSide = normalizedSide
	req.NormalizedPriceTick = normalizedPriceTick
	req.Side = normalizedSide
	req.PriceTick = uint8(normalizedPriceTick)
	if req.OrderType == "market" && req.OriginalAction == "buy" {
		req.QtyLots = 0
		req.SpendAmount = req.TotalAmount
	} else {
		req.QtyLots = req.TotalAmount
		req.SpendAmount = 0
	}
	req.ExpireTime = req.ExpiryTs

	programID, err := gsolana.PublicKeyFromBase58(strings.TrimSpace(req.ProgramID))
	if err != nil {
		return req, nil, errors.New("program_id is not a valid solana address")
	}
	marketPubkey, err := gsolana.PublicKeyFromBase58(strings.TrimSpace(req.Market))
	if err != nil {
		return req, nil, errors.New("market is not a valid solana address")
	}
	userPubkey, err := gsolana.PublicKeyFromBase58(strings.TrimSpace(req.User))
	if err != nil {
		return req, nil, errors.New("user is not a valid solana address")
	}
	intentBytes, err := buildRawOrderIntentBytes(req, programID, marketPubkey, userPubkey)
	if err != nil {
		return req, nil, err
	}
	return req, intentBytes, nil
}

func normalizeToYesBook(side string, outcome string, limitPrice uint64) (string, uint64, error) {
	side = strings.ToLower(strings.TrimSpace(side))
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	if side != "buy" && side != "sell" {
		return "", 0, errors.New("side must be buy or sell")
	}
	if outcome != "yes" && outcome != "no" {
		return "", 0, errors.New("outcome must be yes or no")
	}
	if limitPrice < 1 || limitPrice > 99 {
		return "", 0, errors.New("limit_price must be between 1 and 99")
	}
	if outcome == "yes" {
		return side, limitPrice, nil
	}
	if side == "buy" {
		return "sell", 100 - limitPrice, nil
	}
	return "buy", 100 - limitPrice, nil
}

func (s *Server) nextSnowflakeID() (string, error) {
	id, err := s.sonyflake.NextID()
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(id, 10), nil
}

func (s *Server) precheckPlaceOrder(ctx context.Context, req orders.PlaceOrderRequest, market markets.Market, walletAddress string) error {
	if market.Status != markets.MarketStatusOpen {
		return errors.New("market is not open")
	}
	if !market.CloseTime.IsZero() && time.Now().UTC().After(market.CloseTime) {
		return errors.New("market already closed")
	}

	originalAction := strings.ToLower(strings.TrimSpace(req.OriginalAction))
	position, err := s.getOrInitPositionSnapshot(ctx, market, walletAddress)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn().Err(err).Str("wallet", walletAddress).Msg("gateway position precheck skipped")
		}
		position = positionSnapshot{}
	}
	account, accountErr := s.getOrInitWalletAccountSnapshot(ctx, walletAddress)
	if accountErr != nil {
		if s.logger != nil {
			s.logger.Warn().Err(accountErr).Str("wallet", walletAddress).Msg("gateway wallet account precheck skipped")
		}
		account = walletAccountSnapshot{}
	}

	if strings.EqualFold(req.OrderType, "market") {
		book := s.matching.GetOrderbook(ctx, req.MarketID)
		if strings.EqualFold(req.Side, "buy") && len(book.Asks) == 0 {
			return errNoMarketLiquidity
		}
		if strings.EqualFold(req.Side, "sell") && len(book.Bids) == 0 {
			return errNoMarketLiquidity
		}
	}

	if originalAction == "sell" {
		if err != nil {
			return nil
		}
		availableLots := position.YesFreeLots
		if strings.EqualFold(req.OriginalOutcome, "no") {
			availableLots = position.NoFreeLots
		}
		if availableLots < req.QtyLots {
			return errInsufficientFunds
		}
		return nil
	}

	requiredUnits, err := requiredCollateralUnits(req)
	if err != nil {
		return err
	}
	if requiredUnits == 0 {
		return nil
	}

	if err == nil {
		if account.CollateralFreeUnits < requiredUnits {
			return errInsufficientFunds
		}
		return nil
	}

	availableUnits, balanceErr := s.getCachedVUSDCUnits(ctx, walletAddress)
	if balanceErr != nil {
		if s.logger != nil {
			s.logger.Warn().Err(balanceErr).Str("wallet", walletAddress).Msg("gateway collateral fallback precheck skipped")
		}
		return nil
	}
	if availableUnits < requiredUnits {
		return errInsufficientFunds
	}
	return nil
}

func requiredCollateralUnits(req orders.PlaceOrderRequest) (uint64, error) {
	if strings.EqualFold(req.OrderType, "market") {
		return req.SpendAmount, nil
	}
	if req.OriginalPriceTick == 0 || req.QtyLots == 0 {
		return 0, nil
	}
	max := ^uint64(0)
	if req.QtyLots > max/uint64(req.OriginalPriceTick) {
		return 0, errors.New("order amount too large")
	}
	return (req.QtyLots*uint64(req.OriginalPriceTick) + 99) / 100, nil
}

func (s *Server) getCachedVUSDCUnits(ctx context.Context, walletAddress string) (uint64, error) {
	if strings.TrimSpace(walletAddress) == "" {
		return 0, errors.New("missing wallet address")
	}
	if strings.TrimSpace(s.cfg.VUSDCMint) == "" {
		return 0, errors.New("missing VUSDC mint config")
	}
	cacheKey := fmt.Sprintf("gateway:balance:vusdc:%s", walletAddress)
	if s.redisClient != nil {
		if value, err := s.redisClient.Get(ctx, cacheKey).Result(); err == nil && value != "" {
			return strconv.ParseUint(value, 10, 64)
		}
	}

	units, err := s.fetchVUSDCUnitsFromRPC(ctx, walletAddress)
	if err != nil {
		return 0, err
	}
	if s.redisClient != nil {
		_ = s.redisClient.Set(ctx, cacheKey, strconv.FormatUint(units, 10), 15*time.Second).Err()
	}
	return units, nil
}

func (s *Server) fetchOutcomeLotsFromRPC(ctx context.Context, market markets.Market, walletAddress string, outcome string) (uint64, error) {
	_ = ctx
	_ = market
	_ = walletAddress
	_ = outcome
	return 0, errors.New("token-mint based position lookup is no longer supported")
}

type positionSnapshot struct {
	YesFreeLots           uint64
	YesLockedLots         uint64
	NoFreeLots            uint64
	NoLockedLots          uint64
	CollateralFreeUnits   uint64
	CollateralLockedUnits uint64
}

type walletAccountSnapshot struct {
	CollateralTotalUnits uint64
	CollateralFreeUnits  uint64
}

func (s *Server) getPositionSnapshot(ctx context.Context, marketID uint64, walletAddress string) (positionSnapshot, error) {
	if strings.TrimSpace(walletAddress) == "" {
		return positionSnapshot{}, errors.New("missing wallet address")
	}
	cacheKey := fmt.Sprintf("position:%d:%s", marketID, walletAddress)
	if s.redisClient != nil {
		if values, err := s.redisClient.HGetAll(ctx, cacheKey).Result(); err == nil && len(values) > 0 {
			return parsePositionSnapshot(values)
		}
	}
	return positionSnapshot{}, errors.New("position not found")
}

func (s *Server) getWalletAccountSnapshot(ctx context.Context, walletAddress string) (walletAccountSnapshot, error) {
	if strings.TrimSpace(walletAddress) == "" {
		return walletAccountSnapshot{}, errors.New("missing wallet address")
	}
	cacheKey := fmt.Sprintf("wallet-account:%s", walletAddress)
	if s.redisClient != nil {
		if values, err := s.redisClient.HGetAll(ctx, cacheKey).Result(); err == nil && len(values) > 0 {
			return parseWalletAccountSnapshot(values)
		}
	}
	return walletAccountSnapshot{}, errors.New("wallet account not found")
}

func parseWalletAccountSnapshot(values map[string]string) (walletAccountSnapshot, error) {
	parse := func(key string) (uint64, error) {
		raw := strings.TrimSpace(values[key])
		if raw == "" {
			return 0, nil
		}
		return strconv.ParseUint(raw, 10, 64)
	}
	total, err := parse("collateral_total_units")
	if err != nil {
		return walletAccountSnapshot{}, err
	}
	free, err := parse("collateral_free_units")
	if err != nil {
		return walletAccountSnapshot{}, err
	}
	return walletAccountSnapshot{CollateralTotalUnits: total, CollateralFreeUnits: free}, nil
}

func (s *Server) upsertWalletAccountSnapshot(ctx context.Context, walletAddress string, next walletAccountSnapshot) error {
	if err := s.persistWalletAccountSnapshot(ctx, walletAddress, next); err != nil {
		return err
	}
	if s.redisClient == nil {
		return nil
	}
	key := fmt.Sprintf("wallet-account:%s", walletAddress)
	return s.redisClient.HSet(ctx, key, map[string]any{
		"collateral_total_units": next.CollateralTotalUnits,
		"collateral_free_units":  next.CollateralFreeUnits,
		"updated_at":             time.Now().UTC().Unix(),
	}).Err()
}

func (s *Server) persistWalletAccountSnapshot(ctx context.Context, walletAddress string, next walletAccountSnapshot) error {
	if s.dbPool == nil {
		return nil
	}
	_, err := s.dbPool.Exec(ctx, `
		INSERT INTO wallet_accounts (wallet_address, collateral_total_units, collateral_free_units, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (wallet_address) DO UPDATE SET
			collateral_total_units = EXCLUDED.collateral_total_units,
			collateral_free_units = EXCLUDED.collateral_free_units,
			updated_at = NOW()
	`, walletAddress, next.CollateralTotalUnits, next.CollateralFreeUnits)
	return err
}

func (s *Server) getOrInitWalletAccountSnapshot(ctx context.Context, walletAddress string) (walletAccountSnapshot, error) {
	if snapshot, err := s.getWalletAccountSnapshot(ctx, walletAddress); err == nil {
		return snapshot, nil
	}
	if snapshot, err := s.loadWalletAccountSnapshotFromDB(ctx, walletAddress); err == nil {
		_ = s.upsertWalletAccountSnapshot(ctx, walletAddress, snapshot)
		return snapshot, nil
	}
	return walletAccountSnapshot{}, nil
}

func (s *Server) loadWalletAccountSnapshotFromDB(ctx context.Context, walletAddress string) (walletAccountSnapshot, error) {
	if s.dbPool == nil {
		return walletAccountSnapshot{}, errors.New("db pool not configured")
	}
	row := s.dbPool.QueryRow(ctx, `
		SELECT collateral_total_units, collateral_free_units
		FROM wallet_accounts
		WHERE wallet_address = $1
	`, walletAddress)
	var snapshot walletAccountSnapshot
	if err := row.Scan(&snapshot.CollateralTotalUnits, &snapshot.CollateralFreeUnits); err != nil {
		return walletAccountSnapshot{}, err
	}
	return snapshot, nil
}

func (s *Server) loadPositionSnapshotFromDB(ctx context.Context, marketID uint64, walletAddress string) (positionSnapshot, error) {
	if s.dbPool == nil {
		return positionSnapshot{}, errors.New("db pool not configured")
	}
	marketIDStr := strconv.FormatUint(marketID, 10)
	row := s.dbPool.QueryRow(ctx, `
		SELECT yes_free_lots, yes_locked_lots,
		       no_free_lots, no_locked_lots,
		       collateral_free_units, collateral_locked_units
		FROM positions
		WHERE market_id = $1::NUMERIC(20,0) AND wallet_address = $2
	`, marketIDStr, walletAddress)
	var snapshot positionSnapshot
	if err := row.Scan(
		&snapshot.YesFreeLots,
		&snapshot.YesLockedLots,
		&snapshot.NoFreeLots,
		&snapshot.NoLockedLots,
		&snapshot.CollateralFreeUnits,
		&snapshot.CollateralLockedUnits,
	); err != nil {
		return positionSnapshot{}, err
	}
	return snapshot, nil
}

func parsePositionSnapshot(values map[string]string) (positionSnapshot, error) {
	parse := func(key string) (uint64, error) {
		raw := strings.TrimSpace(values[key])
		if raw == "" {
			return 0, nil
		}
		return strconv.ParseUint(raw, 10, 64)
	}
	yesFree, err := parse("yes_free_lots")
	if err != nil {
		return positionSnapshot{}, err
	}
	yesLocked, err := parse("yes_locked_lots")
	if err != nil {
		return positionSnapshot{}, err
	}
	noFree, err := parse("no_free_lots")
	if err != nil {
		return positionSnapshot{}, err
	}
	noLocked, err := parse("no_locked_lots")
	if err != nil {
		return positionSnapshot{}, err
	}
	collateralFree, err := parse("collateral_free_units")
	if err != nil {
		return positionSnapshot{}, err
	}
	collateralLocked, err := parse("collateral_locked_units")
	if err != nil {
		return positionSnapshot{}, err
	}
	return positionSnapshot{
		YesFreeLots:           yesFree,
		YesLockedLots:         yesLocked,
		NoFreeLots:            noFree,
		NoLockedLots:          noLocked,
		CollateralFreeUnits:   collateralFree,
		CollateralLockedUnits: collateralLocked,
	}, nil
}

func (s *Server) upsertPositionSnapshot(ctx context.Context, marketID uint64, walletAddress string, next positionSnapshot) error {
	if err := s.persistPositionSnapshot(ctx, marketID, walletAddress, next); err != nil {
		return err
	}
	if s.redisClient == nil {
		return nil
	}
	cacheKey := fmt.Sprintf("position:%d:%s", marketID, walletAddress)
	return s.redisClient.HSet(ctx, cacheKey, map[string]any{
		"yes_free_lots":           next.YesFreeLots,
		"yes_locked_lots":         next.YesLockedLots,
		"no_free_lots":            next.NoFreeLots,
		"no_locked_lots":          next.NoLockedLots,
		"collateral_free_units":   next.CollateralFreeUnits,
		"collateral_locked_units": next.CollateralLockedUnits,
		"updated_at":              time.Now().UTC().Unix(),
	}).Err()
}

func (s *Server) persistPositionSnapshot(ctx context.Context, marketID uint64, walletAddress string, next positionSnapshot) error {
	if s.dbPool == nil {
		return nil
	}
	marketIDStr := strconv.FormatUint(marketID, 10)
	_, err := s.dbPool.Exec(ctx, `
		INSERT INTO positions (
			market_id, wallet_address,
			yes_free_lots, yes_locked_lots,
			no_free_lots, no_locked_lots,
			collateral_free_units, collateral_locked_units,
			updated_at
		) VALUES ($1::NUMERIC(20,0), $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (market_id, wallet_address) DO UPDATE SET
			yes_free_lots = EXCLUDED.yes_free_lots,
			yes_locked_lots = EXCLUDED.yes_locked_lots,
			no_free_lots = EXCLUDED.no_free_lots,
			no_locked_lots = EXCLUDED.no_locked_lots,
			collateral_free_units = EXCLUDED.collateral_free_units,
			collateral_locked_units = EXCLUDED.collateral_locked_units,
			updated_at = NOW()
		`, marketIDStr, walletAddress,
		next.YesFreeLots, next.YesLockedLots,
		next.NoFreeLots, next.NoLockedLots,
		next.CollateralFreeUnits, next.CollateralLockedUnits,
	)
	return err
}

func (s *Server) getOrInitPositionSnapshot(ctx context.Context, market markets.Market, walletAddress string) (positionSnapshot, error) {
	if snapshot, err := s.getPositionSnapshot(ctx, market.MarketID, walletAddress); err == nil {
		return snapshot, nil
	}
	if snapshot, err := s.loadPositionSnapshotFromDB(ctx, market.MarketID, walletAddress); err == nil {
		_ = s.upsertPositionSnapshot(ctx, market.MarketID, walletAddress, snapshot)
		return snapshot, nil
	}
	return positionSnapshot{}, nil
}

func (s *Server) fetchPositionSnapshotFromRPC(ctx context.Context, market markets.Market, walletAddress string) (positionSnapshot, error) {
	collateral, err := s.fetchVUSDCUnitsFromRPC(ctx, walletAddress)
	if err != nil {
		return positionSnapshot{}, err
	}
	yesLots, err := s.fetchOutcomeLotsFromRPC(ctx, market, walletAddress, "yes")
	if err != nil {
		return positionSnapshot{}, err
	}
	noLots, err := s.fetchOutcomeLotsFromRPC(ctx, market, walletAddress, "no")
	if err != nil {
		return positionSnapshot{}, err
	}
	return positionSnapshot{
		YesFreeLots:         yesLots,
		NoFreeLots:          noLots,
		CollateralFreeUnits: collateral,
	}, nil
}

func (s *Server) fetchVUSDCUnitsFromRPC(ctx context.Context, walletAddress string) (uint64, error) {
	if s.rpcClient == nil {
		return 0, errors.New("rpc client not configured")
	}
	wallet, err := gsolana.PublicKeyFromBase58(walletAddress)
	if err != nil {
		return 0, err
	}
	mint := gsolana.MustPublicKeyFromBase58(s.cfg.VUSDCMint)
	tokenProgramID, err := detectMintTokenProgram(ctx, s.rpcClient, mint)
	if err != nil {
		return 0, err
	}
	ata, err := findAssociatedTokenAddress(wallet, mint, tokenProgramID)
	if err != nil {
		return 0, err
	}
	balance, err := s.rpcClient.GetTokenAccountBalance(ctx, ata, rpc.CommitmentProcessed)
	if err != nil {
		if isMissingTokenAccountError(err) {
			return 0, nil
		}
		return 0, err
	}
	rawAmount, err := strconv.ParseUint(balance.Value.Amount, 10, 64)
	if err != nil {
		return 0, err
	}
	divisor := uint64(1)
	if s.cfg.VUSDCDecimals > 2 {
		for i := 0; i < s.cfg.VUSDCDecimals-2; i++ {
			divisor *= 10
		}
	}
	if divisor == 0 {
		divisor = 1
	}
	return rawAmount / divisor, nil
}

func detectMintTokenProgram(ctx context.Context, rpcClient *rpc.Client, mint gsolana.PublicKey) (gsolana.PublicKey, error) {
	info, err := rpcClient.GetAccountInfo(ctx, mint)
	if err != nil {
		return gsolana.PublicKey{}, err
	}
	if info == nil || info.Value == nil {
		return gsolana.PublicKey{}, errors.New("mint account not found")
	}
	owner := info.Value.Owner
	if owner.Equals(gsolana.TokenProgramID) || owner.Equals(gsolana.Token2022ProgramID) {
		return owner, nil
	}
	return gsolana.PublicKey{}, fmt.Errorf("unsupported token program for mint: %s", owner.String())
}

func findAssociatedTokenAddress(wallet, mint, tokenProgramID gsolana.PublicKey) (gsolana.PublicKey, error) {
	ata, _, err := gsolana.FindProgramAddress(
		[][]byte{
			wallet[:],
			tokenProgramID[:],
			mint[:],
		},
		gsolana.SPLAssociatedTokenAccountProgramID,
	)
	if err != nil {
		return gsolana.PublicKey{}, err
	}
	return ata, nil
}

func isMissingTokenAccountError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "could not find account")
}

// handleCancelOrder godoc
// @Summary Cancel order
// @Tags Orders
// @Produce json
// @Security BearerAuth
// @Param orderId path string true "Order ID"
// @Param market_id query string true "Market ID"
// @Success 202 {object} cancelOrderAcceptedResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Failure 501 {object} map[string]any
// @Router /api/orders/{orderId} [delete]
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	if s.writeGate != nil && !s.writeGate.OrdersReady() {
		writeError(w, http.StatusServiceUnavailable, "system bootstrap in progress")
		return
	}
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	marketID, err := parseMarketID(strings.TrimSpace(r.URL.Query().Get("market_id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "market_id query parameter is required for cancel command")
		return
	}
	orderID := strings.TrimSpace(chi.URLParam(r, "orderId"))
	if orderID == "" {
		writeError(w, http.StatusBadRequest, "orderId path parameter is required")
		return
	}
	commandID, err := s.nextSnowflakeID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate cancel command ID")
		return
	}
	envelope := protocol.CommandEnvelope[protocol.CancelOrderCommand]{
		ID:             commandID,
		Type:           protocol.CommandTypeCancelOrder,
		SchemaVersion:  1,
		MarketID:       marketID,
		Producer:       "gateway",
		TraceID:        commandID,
		IdempotencyKey: commandID,
		CreatedAt:      time.Now().UTC(),
		Payload: protocol.CancelOrderCommand{
			OrderID:       orderID,
			WalletAddress: walletAddress,
			Reason:        "user_request",
		},
	}
	if err := s.commands.PublishCancelOrder(r.Context(), envelope); err != nil {
		status := http.StatusInternalServerError
		message := "failed to publish cancel-order command"
		code := "command_publish_failed"
		if errors.Is(err, protocol.ErrCommandBusDisabled) {
			status = http.StatusNotImplemented
			message = "command bus is not configured"
			code = "command_bus_not_configured"
		}
		writeJSON(w, status, map[string]any{"code": code, "message": message})
		return
	}
	tx := s.txRequests.Create("cancel_order", marketID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message":    "cancel command accepted",
		"command_id": commandID,
		"market_id":  strconv.FormatUint(marketID, 10),
		"order_id":   orderID,
		"tx_request": tx,
	})
}

// handleOrderbook godoc
// @Summary Get orderbook
// @Tags Market Data
// @Produce json
// @Param marketId path string true "Market ID"
// @Success 200 {object} matching.OrderbookSnapshot
// @Router /api/orderbook/{marketId} [get]
func (s *Server) handleOrderbook(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	writeJSON(w, http.StatusOK, s.matching.GetOrderbook(r.Context(), marketID))
}

// handlePosition godoc
// @Summary Get current user's projected position
// @Tags Positions
// @Produce json
// @Security BearerAuth
// @Param marketId path string true "Market ID"
// @Success 200 {object} positionResponse
// @Failure 400 {object} errorResponse
// @Failure 401 {object} errorResponse
// @Router /api/positions/{marketId} [get]
func (s *Server) handlePosition(w http.ResponseWriter, r *http.Request) {
	marketID, err := parseMarketID(chi.URLParam(r, "marketId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	market, err := s.markets.Get(r.Context(), marketID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	position, err := s.getOrInitPositionSnapshot(r.Context(), market, walletAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, positionResponse{
		MarketID:              strconv.FormatUint(marketID, 10),
		WalletAddress:         walletAddress,
		YesFreeLots:           strconv.FormatUint(position.YesFreeLots, 10),
		YesLockedLots:         strconv.FormatUint(position.YesLockedLots, 10),
		NoFreeLots:            strconv.FormatUint(position.NoFreeLots, 10),
		NoLockedLots:          strconv.FormatUint(position.NoLockedLots, 10),
		CollateralFreeUnits:   strconv.FormatUint(position.CollateralFreeUnits, 10),
		CollateralLockedUnits: strconv.FormatUint(position.CollateralLockedUnits, 10),
	})
}

// handleWalletAccount godoc
// @Summary Get current user's trading account balance
// @Tags Positions
// @Produce json
// @Security BearerAuth
// @Success 200 {object} walletAccountResponse
// @Failure 401 {object} errorResponse
// @Router /api/wallet-account [get]
func (s *Server) handleWalletAccount(w http.ResponseWriter, r *http.Request) {
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	account, err := s.getOrInitWalletAccountSnapshot(r.Context(), walletAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, walletAccountResponse{
		WalletAddress:        walletAddress,
		CollateralTotalUnits: strconv.FormatUint(account.CollateralTotalUnits, 10),
		CollateralFreeUnits:  strconv.FormatUint(account.CollateralFreeUnits, 10),
	})
}

// handleOpenOrders godoc
// @Summary Get current user's open orders
// @Tags Orders
// @Produce json
// @Security BearerAuth
// @Param marketId path string true "Market ID"
// @Success 200 {object} openOrdersResponse
// @Failure 401 {object} errorResponse
// @Router /api/orders/open/{marketId} [get]
func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	user, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"orders": s.matching.GetOpenOrders(r.Context(), user.SolanaAddress, marketID), "matching_enabled": s.cfg.NATSURL != ""})
}

// handleTrades godoc
// @Summary Get recent trades
// @Tags Market Data
// @Produce json
// @Param marketId path string true "Market ID"
// @Success 200 {object} tradesResponse
// @Router /api/trades/{marketId} [get]
func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	marketID, _ := parseMarketID(chi.URLParam(r, "marketId"))
	writeJSON(w, http.StatusOK, map[string]any{"trades": s.matching.GetTrades(r.Context(), marketID), "matching_enabled": s.cfg.NATSURL != ""})
}

// handlePriceHistory godoc
// @Summary Get price history
// @Tags Market Data
// @Produce json
// @Param marketId path string true "Market ID"
// @Param range query string false "1H|6H|1D|1W|1M|ALL"
// @Success 200 {object} matching.PriceHistory
// @Failure 400 {object} errorResponse
// @Router /api/price-history/{marketId} [get]
func (s *Server) handlePriceHistory(w http.ResponseWriter, r *http.Request) {
	marketID, err := parseMarketID(chi.URLParam(r, "marketId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rangeValue := matching.PriceHistoryRange(strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("range"))))
	switch rangeValue {
	case matching.PriceHistoryRange1H, matching.PriceHistoryRange6H, matching.PriceHistoryRange1D,
		matching.PriceHistoryRange1W, matching.PriceHistoryRange1M, matching.PriceHistoryRangeAll:
	default:
		rangeValue = matching.PriceHistoryRange1D
	}
	writeJSON(w, http.StatusOK, s.matching.GetPriceHistory(r.Context(), marketID, rangeValue))
}

func (s *Server) resolveAuthenticatedWalletAddress(r *http.Request, hintedWallet string) (string, error) {
	user, ok := auth.FromContext(r.Context())
	if !ok {
		return "", errors.New("authentication required")
	}
	if wallet := strings.TrimSpace(user.SolanaAddress); wallet != "" {
		return wallet, nil
	}
	return "", errors.New("authenticated solana wallet is required")
}

func (s *Server) handleMarketOrderbookWS(w http.ResponseWriter, r *http.Request) {
	if s.pusherHub == nil {
		writeError(w, http.StatusNotImplemented, "websocket pusher is not configured")
		return
	}
	if err := s.pusherHub.ServeMarketWS(w, r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
}

func (s *Server) handleUserOrdersWS(w http.ResponseWriter, r *http.Request) {
	if s.pusherHub == nil {
		writeError(w, http.StatusNotImplemented, "websocket pusher is not configured")
		return
	}
	if err := s.pusherHub.ServeUserWS(w, r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
}

func (s *Server) handleCreateWSTicket(w http.ResponseWriter, r *http.Request) {
	if s.redisClient == nil {
		writeError(w, http.StatusNotImplemented, "websocket tickets require redis")
		return
	}
	walletAddress, err := s.resolveAuthenticatedWalletAddress(r, "")
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	store := pusher.NewTicketStore(s.redisClient, 45*time.Second)
	ticket, expiresAt, err := store.Issue(r.Context(), walletAddress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to issue websocket ticket")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket":     ticket,
		"expires_at": expiresAt.Format(time.RFC3339),
	})
}

// buildPlaceOrderCommand 旧版本的订单命令构建函数
// Deprecated: 使用 buildPlaceOrderCommandV1 替代 (Borsh 签名 + 雪花ID)
// 此函数已废弃，直接返回错误
func buildPlaceOrderCommand(_ orders.PlaceOrderRequest, _ string) (protocol.PlaceOrderCommand, error) {
	return protocol.PlaceOrderCommand{}, errors.New("buildPlaceOrderCommand is deprecated, use buildPlaceOrderCommandV1 instead")
}

// validateSignedOrderIntent 旧版本的签名验证函数
// Deprecated: 使用 validateBorshOrderIntent 替代 (Borsh 序列化验签)
// 此函数已废弃，直接返回错误
func validateSignedOrderIntent(_ orders.PlaceOrderRequest, _ string) error {
	return errors.New("validateSignedOrderIntent is deprecated, use validateBorshOrderIntent instead")
}

func decodeSignature(raw string) ([]byte, error) {
	signatureBytes, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		signatureBytes, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return nil, errors.New("signature must be base64 encoded")
		}
	}
	if len(signatureBytes) != ed25519.SignatureSize {
		return nil, errors.New("signature must be 64 bytes")
	}
	return signatureBytes, nil
}

// buildOrderIntentMessage 旧版本的签名消息构建函数
// Deprecated: 使用 Borsh 序列化替代文本签名消息
// 此函数已废弃，直接返回空字符串
func buildOrderIntentMessage(_ orders.PlaceOrderRequest) string {
	return ""
}

// toQtyLots 旧版本的精度转换函数
// Deprecated: 前端直接传 qty_lots，后端不再需要转换
// 保留此函数仅用于向后兼容和测试，将在未来版本中删除
func toQtyLots(quantity float64) (int64, error) {
	if quantity <= 0 {
		return 0, errors.New("qty must be greater than 0")
	}
	raw := quantity * 100
	rounded := math.Round(raw)
	if math.Abs(raw-rounded) > 1e-9 {
		return 0, errors.New("qty must be in increments of 0.01")
	}
	return int64(rounded), nil
}

func parseExpireRFC3339(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("expire_time is required for limit order (RFC3339)")
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, errors.New("expire_time must be valid RFC3339")
	}
	utc := parsed.UTC()
	return &utc, nil
}

func parseSignedAtRFC3339(raw string) (*time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("signed_at is required (RFC3339)")
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		return nil, errors.New("signed_at must be valid RFC3339")
	}
	utc := parsed.UTC()
	return &utc, nil
}

// normalizeYesOrder 旧版本的 No 订单归一化函数
// Deprecated: 前端已处理归一化，后端不再需要
// 保留此函数仅用于向后兼容和测试，将在未来版本中删除
func normalizeYesOrder(side protocol.Side, outcome protocol.Outcome, priceTick int, orderType protocol.OrderType) (protocol.Side, int, error) {
	switch side {
	case protocol.SideBuy, protocol.SideSell:
	default:
		return "", 0, fmt.Errorf("invalid side: %s", side)
	}
	switch outcome {
	case protocol.OutcomeYes, protocol.OutcomeNo:
	default:
		return "", 0, fmt.Errorf("invalid share: %s", outcome)
	}

	if orderType == protocol.OrderTypeMarket {
		if priceTick < 1 || priceTick > 99 {
			return "", 0, errors.New("price_tick must be between 1 and 99")
		}
		if outcome == protocol.OutcomeYes {
			return side, priceTick, nil
		}
		if side == protocol.SideBuy {
			return protocol.SideSell, 100 - priceTick, nil
		}
		return protocol.SideBuy, 100 - priceTick, nil
	}

	if priceTick < 1 || priceTick > 99 {
		return "", 0, errors.New("price_tick must be between 1 and 99")
	}
	if outcome == protocol.OutcomeYes {
		return side, priceTick, nil
	}
	if side == protocol.SideBuy {
		return protocol.SideSell, 100 - priceTick, nil
	}
	return protocol.SideBuy, 100 - priceTick, nil
}

// OrderIntentBorsh Borsh 序列化的订单意图结构 (107 字节定长)
// 根据订单系统全局规范 V1 (orderDesign.md)
type OrderIntentBorsh struct {
	ProgramID         [32]byte
	WalletAddress     [32]byte
	MarketID          uint64
	OriginalAction    uint8
	OriginalOutcome   uint8
	OriginalPriceTick uint8
	Side              uint8
	OrderType         uint8
	PriceTick         uint8
	QtyLots           uint64
	SpendAmount       uint64
	ExpireTime        int64
	Nonce             uint64
}

type RawOrderIntentV1 struct {
	Version     uint8
	ChainID     uint16
	ProgramID   [32]byte
	Market      [32]byte
	User        [32]byte
	Nonce       uint64
	Side        uint8
	Outcome     uint8
	OrderType   uint8
	LimitPrice  uint64
	TotalAmount uint64
	ExpiryTs    int64
}

// serializeOrderIntentBorsh 序列化订单意图为 110 字节数组
func serializeOrderIntentBorsh(intent *OrderIntentBorsh) ([]byte, error) {
	if intent == nil {
		return nil, errors.New("intent is nil")
	}

	buf := make([]byte, 0, 110)

	// ProgramID [32]byte
	buf = append(buf, intent.ProgramID[:]...)

	// WalletAddress [32]byte
	buf = append(buf, intent.WalletAddress[:]...)

	// MarketID u64 (小端序)
	buf = append(buf, uint64ToLEBytes(intent.MarketID)...)

	buf = append(buf, intent.OriginalAction)
	buf = append(buf, intent.OriginalOutcome)
	buf = append(buf, intent.OriginalPriceTick)

	// Side u8
	buf = append(buf, intent.Side)

	// OrderType u8
	buf = append(buf, intent.OrderType)

	// PriceTick u8
	buf = append(buf, intent.PriceTick)

	// QtyLots u64 (小端序)
	buf = append(buf, uint64ToLEBytes(intent.QtyLots)...)

	// SpendAmount u64 (小端序)
	buf = append(buf, uint64ToLEBytes(intent.SpendAmount)...)

	// ExpireTime i64 (小端序)
	buf = append(buf, uint64ToLEBytes(uint64(intent.ExpireTime))...)

	// Nonce u64 (小端序)
	buf = append(buf, uint64ToLEBytes(intent.Nonce)...)

	if len(buf) != 110 {
		return nil, fmt.Errorf("serialized size mismatch: expected 110, got %d", len(buf))
	}

	return buf, nil
}

// uint64ToLEBytes 将 uint64 转换为小端序字节数组
func uint64ToLEBytes(v uint64) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56)}
}

func uint16ToLEBytes(v uint16) []byte {
	return []byte{byte(v), byte(v >> 8)}
}

func serializeRawOrderIntentV1(intent *RawOrderIntentV1) ([]byte, error) {
	if intent == nil {
		return nil, errors.New("intent is nil")
	}
	buf := make([]byte, 0, 134)
	buf = append(buf, intent.Version)
	buf = append(buf, uint16ToLEBytes(intent.ChainID)...)
	buf = append(buf, intent.ProgramID[:]...)
	buf = append(buf, intent.Market[:]...)
	buf = append(buf, intent.User[:]...)
	buf = append(buf, uint64ToLEBytes(intent.Nonce)...)
	buf = append(buf, intent.Side)
	buf = append(buf, intent.Outcome)
	buf = append(buf, intent.OrderType)
	buf = append(buf, uint64ToLEBytes(intent.LimitPrice)...)
	buf = append(buf, uint64ToLEBytes(intent.TotalAmount)...)
	buf = append(buf, uint64ToLEBytes(uint64(intent.ExpiryTs))...)
	if len(buf) != 134 {
		return nil, fmt.Errorf("serialized size mismatch: expected 134, got %d", len(buf))
	}
	return buf, nil
}

func buildOrderSignatureMessage(messageHash []byte) []byte {
	hexHash := hex.EncodeToString(messageHash)
	return []byte(hexHash)
}

func buildRawOrderIntentBytes(
	req orders.PlaceOrderRequest,
	programID gsolana.PublicKey,
	marketPubkey gsolana.PublicKey,
	userPubkey gsolana.PublicKey,
) ([]byte, error) {
	intent := settlement.OrderIntentV1{
		Version:     req.Version,
		ChainID:     req.ChainID,
		ProgramID:   programID,
		Market:      marketPubkey,
		User:        userPubkey,
		Side:        settlement.Side(mapSideToUint8(req.OriginalAction)),
		Outcome:     settlement.Outcome(mapOutcomeToUint8(req.OriginalOutcome)),
		OrderType:   settlement.OrderType(mapOrderTypeToUint8(req.OrderType)),
		LimitPrice:  req.LimitPrice,
		TotalAmount: req.TotalAmount,
		Nonce:       req.Nonce,
		ExpiryTs:    req.ExpiryTs,
	}
	return intent.Serialize(), nil
}

// validateBorshOrderIntent 验证 Borsh 订单意图签名
// 根据订单系统全局规范 V1 (orderDesign.md)
func validateBorshOrderIntent(req orders.PlaceOrderRequest, authenticatedWallet string) (*OrderIntentBorsh, []byte, error) {
	// 1. JWT 身份对齐
	walletAddress := strings.TrimSpace(req.WalletAddress)
	if walletAddress == "" {
		return nil, nil, errors.New("wallet_address is required")
	}
	if walletAddress != strings.TrimSpace(authenticatedWallet) {
		return nil, nil, errOrderWalletMismatch
	}

	// 2. 业务防呆断言
	if req.OriginalAction != "buy" && req.OriginalAction != "sell" {
		return nil, nil, errors.New("original_action must be buy or sell")
	}
	if req.OriginalOutcome != "yes" && req.OriginalOutcome != "no" {
		return nil, nil, errors.New("original_outcome must be yes or no")
	}
	if req.OriginalPriceTick < 1 || req.OriginalPriceTick > 99 {
		return nil, nil, errors.New("original_price_tick must be between 1 and 99")
	}
	if req.PriceTick < 1 || req.PriceTick > 99 {
		return nil, nil, errors.New("price_tick must be between 1 and 99")
	}
	expectedSide, expectedPriceTick, err := normalizeYesOrder(
		protocol.Side(req.OriginalAction),
		protocol.Outcome(req.OriginalOutcome),
		int(req.OriginalPriceTick),
		protocol.OrderType(req.OrderType),
	)
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(req.Side, string(expectedSide)) {
		return nil, nil, errors.New("normalized side does not match original semantics")
	}
	if req.PriceTick != uint8(expectedPriceTick) {
		return nil, nil, errors.New("normalized price_tick does not match original semantics")
	}

	// 根据订单类型验证字段组合
	if req.OrderType == "limit" || (req.OrderType == "market" && req.OriginalAction == "sell") {
		// 限价单或市价卖出：QtyLots 必须 > 0，SpendAmount 必须 = 0
		if req.QtyLots == 0 {
			return nil, nil, errors.New("qty_lots must be greater than 0 for limit/market sell order")
		}
		if req.SpendAmount != 0 {
			return nil, nil, errors.New("spend_amount must be 0 for limit/market sell order")
		}
	}
	if req.OrderType == "market" && req.OriginalAction == "buy" {
		// 市价买入：SpendAmount 必须 > 0，QtyLots 必须 = 0
		if req.SpendAmount == 0 {
			return nil, nil, errors.New("spend_amount must be greater than 0 for market buy order")
		}
		if req.QtyLots != 0 {
			return nil, nil, errors.New("qty_lots must be 0 for market buy order")
		}
	}

	// 3. 签名验证
	signature := strings.TrimSpace(req.Signature)
	if signature == "" {
		return nil, nil, errors.New("signature is required")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return nil, nil, fmt.Errorf("signature must be base64 encoded: %w", err)
	}
	if len(signatureBytes) != ed25519.SignatureSize {
		return nil, nil, errors.New("signature must be 64 bytes")
	}

	// 4. 重建 Borsh 意图并验签
	pubkey, err := gsolana.PublicKeyFromBase58(walletAddress)
	if err != nil {
		return nil, nil, errors.New("wallet_address is not a valid solana address")
	}

	programID, err := gsolana.PublicKeyFromBase58(strings.TrimSpace(req.ProgramID))
	if err != nil {
		return nil, nil, errors.New("program_id is not a valid solana address")
	}
	marketPubkey, err := gsolana.PublicKeyFromBase58(strings.TrimSpace(req.Market))
	if err != nil {
		return nil, nil, errors.New("market is not a valid solana address")
	}
	intentBytes, err := buildRawOrderIntentBytes(req, programID, marketPubkey, pubkey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to serialize intent: %w", err)
	}
	parsedIntent, err := settlement.ParseOrderIntentV1(intentBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse serialized intent: %w", err)
	}
	signMessage := parsedIntent.SignableMessage()
	if !ed25519.Verify(ed25519.PublicKey(pubkey.Bytes()), signMessage, signatureBytes) {
		return nil, nil, errOrderSignatureInvalid
	}

	intent := &OrderIntentBorsh{
		ProgramID:         [32]byte(programID.Bytes()),
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
	return intent, intentBytes, nil
}

// mapSideToUint8 将 Side 映射为 uint8
func mapSideToUint8(side string) uint8 {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "buy":
		return 0
	case "sell":
		return 1
	default:
		return 0 // 默认 buy
	}
}

func mapOutcomeToUint8(outcome string) uint8 {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "no":
		return 1
	default:
		return 0
	}
}

// mapOrderTypeToUint8 将 OrderType 映射为 uint8
func mapOrderTypeToUint8(orderType string) uint8 {
	switch strings.ToLower(strings.TrimSpace(orderType)) {
	case "limit":
		return 0
	case "market":
		return 1
	default:
		return 0 // 默认 limit
	}
}

// buildPlaceOrderCommandV1 构建新的 PlaceOrderCommand (根据 orderDesign.md)
func buildPlaceOrderCommandV1(req orders.PlaceOrderRequest, intentBytes []byte, orderID uint64) protocol.PlaceOrderCommand {
	return protocol.PlaceOrderCommand{
		CommandID:      "",
		TraceID:        "",
		IdempotencyKey: "",
		Timestamp:      time.Now().Unix(),
		MarketID:       req.MarketID,
		MarketPDA:      req.Market,
		Execution: protocol.PlaceOrderExecution{
			OrderID:             orderID,
			WalletAddress:       req.WalletAddress,
			OriginalAction:      strings.ToLower(strings.TrimSpace(req.OriginalAction)),
			OriginalOutcome:     strings.ToLower(strings.TrimSpace(req.OriginalOutcome)),
			OriginalPriceTick:   req.OriginalPriceTick,
			OrderType:           strings.ToLower(strings.TrimSpace(req.OrderType)),
			NormalizedSide:      strings.ToLower(strings.TrimSpace(req.NormalizedSide)),
			NormalizedPriceTick: uint8(req.NormalizedPriceTick),
			QtyLots:             req.QtyLots,
			SpendAmount:         req.SpendAmount,
			ExpireTime:          req.ExpireTime,
			Nonce:               req.Nonce,
		},
		Settlement: protocol.SettlementPayload{
			IntentBytesHex: hex.EncodeToString(intentBytes),
			Signature:      req.Signature,
		},
	}
}

func validateCreateMarket(req markets.CreateMarketRequest) error {
	if req.Title == "" {
		return errors.New("title is required")
	}
	if strings.TrimSpace(req.MetadataCID) == "" {
		return errors.New("metadata_cid is required")
	}
	if req.CloseTime.IsZero() {
		return errors.New("close_time is required")
	}
	if req.ClaimDeadlineTime.IsZero() {
		return errors.New("claim_deadline_time is required")
	}
	if !req.ClaimDeadlineTime.After(req.CloseTime) {
		return errors.New("claim_deadline_time must be later than close_time")
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
		if req.ResolveAfterTime.IsZero() {
			return errors.New("resolve_after_time is required for pyth markets")
		}
		if req.Resolution.OracleTarget == 0 {
			return errors.New("resolution.oracle_target_price is required for pyth markets")
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

// handleHeliusWebgodot 处理 Helius webhook 请求
func (s *Server) handleHeliusWebhook(w http.ResponseWriter, r *http.Request) {
	s.logger.Info().Str("remote_addr", r.RemoteAddr).Str("path", r.URL.Path).Msg("Helius webhook request received")
	s.webhookHandler.HandleWebhook(w, r)
}

func (s *Server) handleAlchemyWebhook(w http.ResponseWriter, r *http.Request) {
	if s.alchemyWebhookHandler == nil {
		writeError(w, http.StatusNotImplemented, "alchemy webhook not configured")
		return
	}
	s.alchemyWebhookHandler.HandleWebhook(w, r)
}

// lockBalanceAtomic 使用 Redis Lua 脚本原子性锁定余额
// 返回: (成功标志, 剩余可用余额, 错误)
func (s *Server) lockBalanceAtomic(ctx context.Context, walletAddress string, lockAmount int64) (bool, int64, error) {
	if s.redisClient == nil {
		return false, 0, errors.New("redis client not configured")
	}

	// Redis Lua 脚本：原子性检查并锁定余额
	luaScript := `
		local key = KEYS[1]
		local lockAmount = tonumber(ARGV[1])
		local updatedAt = ARGV[2]

		-- 获取当前余额（如果不存在则初始化为0）
		local totalUnits = tonumber(redis.call("HGET", key, "collateral_total_units")) or 0
		local freeUnits = tonumber(redis.call("HGET", key, "collateral_free_units")) or 0
		local lockedUnits = tonumber(redis.call("HGET", key, "collateral_locked_units")) or 0

		-- 检查余额是否足够
		if freeUnits < lockAmount then
			return {0, freeUnits}
		end

		-- 原子性扣减
		redis.call("HINCRBY", key, "collateral_free_units", -lockAmount)
		redis.call("HINCRBY", key, "collateral_locked_units", lockAmount)
		redis.call("HSET", key, "updated_at", updatedAt)

		return {1, freeUnits - lockAmount}
	`

	cacheKey := fmt.Sprintf("wallet-account:%s", walletAddress)
	updatedAt := strconv.FormatInt(time.Now().UTC().Unix(), 10)

	result, err := s.redisClient.Eval(ctx, luaScript, []string{cacheKey}, lockAmount, updatedAt).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redis eval failed: %w", err)
	}

	// 解析结果
	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 2 {
		return false, 0, errors.New("invalid redis result format")
	}

	success, ok := resultSlice[0].(int64)
	if !ok {
		return false, 0, errors.New("invalid success flag")
	}

	remaining, ok := resultSlice[1].(int64)
	if !ok {
		return false, 0, errors.New("invalid remaining amount")
	}

	return success == 1, remaining, nil
}

// releaseBalanceAtomic 释放锁定的余额
// 返回: (成功标志, 释放后的自由余额, 错误)
func (s *Server) releaseBalanceAtomic(ctx context.Context, walletAddress string, releaseAmount int64) (bool, int64, error) {
	if s.redisClient == nil {
		return false, 0, errors.New("redis client not configured")
	}

	cacheKey := fmt.Sprintf("wallet-account:%s", walletAddress)
	updatedAt := strconv.FormatInt(time.Now().UTC().Unix(), 10)

	// 使用管道减少往返次数
	pipe := s.redisClient.Pipeline()
	pipe.HIncrBy(ctx, cacheKey, "collateral_locked_units", -releaseAmount)
	pipe.HIncrBy(ctx, cacheKey, "collateral_free_units", releaseAmount)
	pipe.HSet(ctx, cacheKey, "updated_at", updatedAt)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("redis pipeline exec failed: %w", err)
	}

	// 获取更新后的自由余额
	freeUnits, err := s.redisClient.HGet(ctx, cacheKey, "collateral_free_units").Int64()
	if err != nil {
		return true, 0, nil // 释放成功，但无法获取余额
	}

	return true, freeUnits, nil
}

// persistOrderLock 持久化订单锁定记录到数据库
func (s *Server) persistOrderLock(ctx context.Context, orderID uint64, walletAddress string, marketID uint64, lockedAmount int64) error {
	if s.dbPool == nil {
		return nil
	}

	_, err := s.dbPool.Exec(ctx, `
		INSERT INTO order_locks (order_id, wallet_address, market_id, locked_amount, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'pending', NOW(), NOW())
		ON CONFLICT (order_id) DO NOTHING
	`, int64(orderID), walletAddress, int64(marketID), lockedAmount)

	return err
}

// deleteOrderLock 删除订单锁定记录
func (s *Server) deleteOrderLock(ctx context.Context, orderID uint64) error {
	if s.dbPool == nil {
		return nil
	}

	_, err := s.dbPool.Exec(ctx, `
		DELETE FROM order_locks WHERE order_id = $1
	`, int64(orderID))

	return err
}

// getOrderLockFromRedis 从 Redis 获取订单锁定信息
func (s *Server) getOrderLockFromRedis(ctx context.Context, orderID uint64) (map[string]string, error) {
	if s.redisClient == nil {
		return nil, errors.New("redis client not configured")
	}

	cacheKey := fmt.Sprintf("locked:order:%d", orderID)
	lockData, err := s.redisClient.HGetAll(ctx, cacheKey).Result()
	if err != nil {
		return nil, err
	}

	if len(lockData) == 0 {
		return nil, errors.New("order lock not found")
	}

	return lockData, nil
}
