// @title BlinkPredict Banckend API
// @version v1
// @description BlinkPredict backend API documentation. Import /api/openapi.json into Postman or FoxAPI.
// @BasePath /
// @schemes http https
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
package main

import (
	"context"
	"net/http"
	"time"

	"github.com/joho/godotenv"

	"blinkpredict/banckend/internal/auth"
	"blinkpredict/banckend/internal/bootstrap"
	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/chainconfirm"
	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/db"
	"blinkpredict/banckend/internal/depositconfirm"
	"blinkpredict/banckend/internal/faucet"
	"blinkpredict/banckend/internal/funds"
	httpapi "blinkpredict/banckend/internal/http"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/marketconfirm"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"
	"blinkpredict/banckend/internal/pusher"
	"blinkpredict/banckend/internal/query"
	"blinkpredict/banckend/internal/settlement"
	"blinkpredict/banckend/internal/txreqs"
	"blinkpredict/banckend/internal/webhooks"
	"blinkpredict/banckend/internal/writer"

	"github.com/gagliardetto/solana-go"
	"github.com/redis/go-redis/v9"
)

var logger = logging.New("main")
var zerologLogger = logging.Component("main")

func main() {
	// 加载 .env 文件到环境变量（优先使用工作目录下的 .env）
	if err := godotenv.Load(); err != nil {
		logger.Warnf("Failed to load .env file: %v (continuing with system env vars)", err)
	}

	cfg := config.Load()
	logging.Configure(cfg.LogLevel)
	if _, err := auth.NewSessionManager(cfg); err != nil {
		logger.Fatalf("auth session manager: %v", err)
	}

	var commandPublisher protocol.CommandPublisher = protocol.DisabledCommandPublisher{}
	var natsClient *natsjs.Client
	var redisClient *redis.Client
	var fundsService *funds.Service
	var fundsManager *funds.Manager
	var pusherHub *pusher.Hub
	var pusherService *pusher.Service
	var pgWriter *writer.Writer
	var settlementService *settlement.Service
	var depositConfirmService *depositconfirm.Service
	var marketConfirmService *marketconfirm.Service
	var marketProjector *marketconfirm.Projector
	var marketManager *matching.MarketManager
	var wsRouter chainconfirm.WSRouter
	var err error
	if cfg.NATSURL != "" {
		natsClient, err = natsjs.New(natsjs.Config{
			URL:       cfg.NATSURL,
			Domain:    cfg.NATSJSDomain,
			CmdStream: cfg.NATSStreamCMD,
			EvtStream: cfg.NATSStreamEVT,
			WhkStream: cfg.NATSStreamWHK,
		})
		if err != nil {
			logger.Fatalf("nats: %v", err)
		}
		defer natsClient.Close()
		if err := natsClient.EnsureStreams(context.Background()); err != nil {
			logger.Fatalf("nats streams: %v", err)
		}
		commandPublisher = natsjs.NewCommandPublisher(natsClient)

		// 初始化Snowflake ID生成器（使用machineID=1）
		if err := matching.InitGlobalSnowflake(1); err != nil {
			logger.Fatalf("failed to initialize snowflake: %v", err)
		}

		// 创建市场管理器
		marketManager = matching.NewMarketManager(natsClient, nil, matching.ManagerConfig{
			Batch: matching.BatchConfig{
				MaxFillsHot:  cfg.MatcherBatchMaxFillsHot,
				MaxFillsCold: cfg.MatcherBatchMaxFillsCold,
				MaxOrders:    cfg.MatcherBatchMaxOrders,
				MaxBytes:     cfg.MatcherBatchMaxBytes,
				MaxAge:       cfg.MatcherBatchMaxAge,
				IdleFlush:    cfg.MatcherBatchIdleFlush,
				FlushTick:    cfg.MatcherBatchFlushTick,
			},
		})
	} else {
		logger.Warnf("NATS disabled (set NATS_URL to enable command bus)")
	}

	if cfg.DatabaseURL == "" {
		logger.Fatalf("database is required for markets metadata (set DB_* or DATABASE_URL)")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("database: %v", err)
	}
	defer pool.Close()
	marketRepo := markets.Repository(markets.NewPostgresRepository(pool))
	logger.Infof("Markets repository: postgres (metadata reads/writes)")
	if marketManager != nil {
		marketManager = matching.NewMarketManager(natsClient, pool, matching.ManagerConfig{
			Batch: matching.BatchConfig{
				MaxFillsHot:  cfg.MatcherBatchMaxFillsHot,
				MaxFillsCold: cfg.MatcherBatchMaxFillsCold,
				MaxOrders:    cfg.MatcherBatchMaxOrders,
				MaxBytes:     cfg.MatcherBatchMaxBytes,
				MaxAge:       cfg.MatcherBatchMaxAge,
				IdleFlush:    cfg.MatcherBatchIdleFlush,
				FlushTick:    cfg.MatcherBatchFlushTick,
			},
		})
	}

	fundsManager = funds.NewManager()
	if natsClient != nil {
		fundsService = funds.NewService(natsClient, pool, redisClient, fundsManager)
	}

	var matchingEngine query.Engine = query.NewDisabledEngine()
	var marketCache *cache.MarketCache
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			logger.Fatalf("redis parse url: %v", err)
		}
		redisClient = redis.NewClient(opts)
		redisClient.AddHook(logging.NewRedisHook("redis"))
		if err := redisClient.Ping(ctx).Err(); err != nil {
			logger.Fatalf("redis: %v", err)
		}
		defer redisClient.Close()
		logger.Infof("Redis read model enabled")
		matchingEngine = query.NewRedisEngine(redisClient, pool)
		if fundsService != nil {
			fundsService = funds.NewService(natsClient, pool, redisClient, fundsManager)
		}

		// 初始化市场缓存
		marketCache = cache.NewMarketCache(redisClient, zerologLogger)
		logger.Infof("Market cache initialized")
	}

	var webhookHandler *webhooks.HeliusHandler
	var alchemyHandler *webhooks.AlchemyHandler
	logger.Infof("Webhook ingress disabled; deposit and market flows use confirm workers only")
	boot := bootstrap.NewCoordinator(nil, nil, nil, nil, nil, nil, nil, nil, cfg.MatcherTickInterval)
	if natsClient != nil {
		if cfg.SolanaRPCURL != "" {
			wsRouter = chainconfirm.NewRouter(cfg.SolanaWSURL, cfg.SolanaRPCURL, 2)
			if wsRouter == nil {
				logger.Warnf("shared solana websocket router disabled; using HTTP fallback only")
			} else {
				defer wsRouter.Close()
			}
		}
		pusherHub = pusher.NewHub(cfg, nil, matchingEngine)
		pusherService = pusher.NewService(natsClient, pusherHub)
		pgWriter = writer.New(pool, natsClient, redisClient, "")
		marketProjector = marketconfirm.NewProjector(natsClient, pool, marketRepo, marketCache)
		if cfg.SettlementRelayerKeypair != "" && cfg.ProgramID != "" {
			programID := solana.MustPublicKeyFromBase58(cfg.ProgramID)
			relayer, err := faucet.LoadKeypair(cfg.SettlementRelayerKeypair)
			if err != nil {
				logger.Fatalf("settlement relayer keypair: %v", err)
			}
			settlementService = settlement.NewService(natsClient, pool, cfg.SolanaRPCURL, programID, relayer, wsRouter, "", cfg.SettlementReconcileInterval)
		}
		if cfg.SolanaRPCURL != "" {
			depositConfirmService = depositconfirm.NewService(natsClient, pool, cfg, wsRouter)
		}
		if cfg.SolanaRPCURL != "" {
			marketConfirmService = marketconfirm.NewService(natsClient, pool, cfg, wsRouter)
		}
		boot = bootstrap.NewCoordinator(
			pgWriter,
			fundsService,
			marketManager,
			pusherService,
			depositConfirmService,
			marketConfirmService,
			marketProjector,
			settlementService,
			cfg.MatcherTickInterval,
		)
		if err := boot.Start(ctx); err != nil {
			logger.Fatalf("bootstrap: %v", err)
		}
	}

	var faucetSvc faucet.Service = faucet.DisabledService{}
	if cfg.VUSDCMint != "" && cfg.FaucetPayerKeypair != "" {
		mint := solana.MustPublicKeyFromBase58(cfg.VUSDCMint)
		payer, err := faucet.LoadKeypair(cfg.FaucetPayerKeypair)
		if err != nil {
			logger.Fatalf("faucet payer keypair: %v", err)
		}
		mintAuthority, err := faucet.LoadKeypair(cfg.FaucetMintAuthorityKeypair)
		if err != nil {
			logger.Fatalf("faucet mint authority keypair: %v", err)
		}
		repo := faucet.NewPostgresClaimsRepository(pool)
		service, err := faucet.NewSolanaService(faucet.SolanaServiceConfig{
			RPCURL:           cfg.SolanaRPCURL,
			Mint:             mint,
			Decimals:         cfg.VUSDCDecimals,
			Payer:            payer,
			MintAuthority:    mintAuthority,
			AmountTokens:     cfg.FaucetAmount,
			Cooldown:         24 * time.Hour,
			DisableRateLimit: cfg.FaucetDisableRateLimit,
		}, repo)
		if err != nil {
			logger.Fatalf("faucet service: %v", err)
		}
		faucetSvc = service
	} else {
		logger.Warnf("Faucet disabled (set DB config, VUSDC_MINT, FAUCET_PAYER_KEYPAIR to enable)")
	}

	server := httpapi.New(
		cfg,
		marketRepo,
		matchingEngine,
		noopIndexer{},
		boot,
		txreqs.NewStore(),
		faucetSvc,
		commandPublisher,
		pusherHub,
		marketCache,
		redisClient,
		pool,
		webhookHandler,
		alchemyHandler,
		zerologLogger,
	)

	logger.Infof("BlinkPredict Banckend listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, server.Router()); err != nil {
		logger.Fatalf("listen http: %v", err)
	}
}

type noopIndexer struct{}

func (noopIndexer) Start(_ context.Context) error { return nil }
func (noopIndexer) Stop(_ context.Context) error  { return nil }

var _ indexer.Listener = noopIndexer{}
