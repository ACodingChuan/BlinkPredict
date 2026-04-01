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
	"os"
	"time"

	"github.com/joho/godotenv"

	"blinkpredict/banckend/internal/auth"
	"blinkpredict/banckend/internal/bootstrap"
	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/db"
	"blinkpredict/banckend/internal/faucet"
	httpapi "blinkpredict/banckend/internal/http"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/marketindexer"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"
	"blinkpredict/banckend/internal/pusher"
	"blinkpredict/banckend/internal/settlement"
	"blinkpredict/banckend/internal/txreqs"
	"blinkpredict/banckend/internal/webhooks"
	"blinkpredict/banckend/internal/writer"

	"github.com/gagliardetto/solana-go"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

var logger = logging.New("main")
var zerologLogger = zerolog.New(os.Stdout).With().Timestamp().Logger()

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
	var wsTicketStore *pusher.TicketStore
	var pusherHub *pusher.Hub
	var pusherService *pusher.Service
	var settlementService *settlement.Service
	var marketManager *matching.MarketManager
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
				MaxFills:  cfg.MatcherBatchMaxFills,
				MaxOrders: cfg.MatcherBatchMaxOrders,
				MaxBytes:  cfg.MatcherBatchMaxBytes,
				MaxAge:    cfg.MatcherBatchMaxAge,
				IdleFlush: cfg.MatcherBatchIdleFlush,
				FlushTick: cfg.MatcherBatchFlushTick,
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
				MaxFills:  cfg.MatcherBatchMaxFills,
				MaxOrders: cfg.MatcherBatchMaxOrders,
				MaxBytes:  cfg.MatcherBatchMaxBytes,
				MaxAge:    cfg.MatcherBatchMaxAge,
				IdleFlush: cfg.MatcherBatchIdleFlush,
				FlushTick: cfg.MatcherBatchFlushTick,
			},
		})
	}

	var matchingEngine matching.Engine = matching.NewDisabledEngine()
	if marketManager != nil {
		matchingEngine = matching.NewQueryEngine(marketManager)
	}
	var marketCache *cache.MarketCache
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			logger.Fatalf("redis parse url: %v", err)
		}
		redisClient = redis.NewClient(opts)
		if err := redisClient.Ping(ctx).Err(); err != nil {
			logger.Fatalf("redis: %v", err)
		}
		defer redisClient.Close()
		logger.Infof("Redis read model enabled")
		wsTicketStore = pusher.NewTicketStore(redisClient, 45*time.Second)
		matchingEngine = matching.NewRedisQueryEngine(redisClient, pool, matchingEngine)

		// 初始化市场缓存
		marketCache = cache.NewMarketCache(redisClient, &zerologLogger)
		logger.Infof("Market cache initialized")
	}

	// 初始化 Helius Webhook Handler
	var webhookHandler *webhooks.HeliusHandler
	if cfg.HeliusWebhookEnabled && cfg.HeliusWebhookSecret != "" {
		if cfg.VUSDCMint == "" || cfg.GlobalVault == "" {
			logger.Warnf("Helius webhook enabled but VUSDC_MINT/GLOBAL_VAULT is missing")
		} else {
			var sharedWallet *matching.SharedWalletManager
			if marketManager != nil {
				sharedWallet = marketManager.SharedWallet()
			}
			depositProjector := webhooks.NewDepositProjector(pool, redisClient, sharedWallet, &zerologLogger)
			webhookHandler = webhooks.NewHeliusHandler(
				depositProjector,
				&zerologLogger,
				cfg.HeliusWebhookSecret,
				cfg.VUSDCMint,
				cfg.GlobalVault,
				cfg.VUSDCDecimals,
			)
			logger.Infof("Helius webhook enabled")
		}
	} else {
		logger.Infof("Helius webhook disabled (set HELIUS_WEBHOOK_ENABLED=true to enable)")
	}

	// 初始化 Alchemy Webhook Handler
	var alchemyHandler *webhooks.AlchemyHandler
	if cfg.AlchemySigningKey != "" {
		alchemyHandler = webhooks.NewAlchemyHandler(
			natsClient,
			&zerologLogger,
			cfg.AlchemySigningKey,
			cfg.ProgramID,
		)
		logger.Infof("Alchemy webhook enabled")
	} else {
		logger.Infof("Alchemy webhook disabled (set ALCHEMY_SIGNING_KEY to enable)")
	}
	if natsClient != nil {
		indexConsumer := marketindexer.NewConsumer(natsClient.Conn(), pool, marketRepo, marketCache, &zerologLogger)
		if err := indexConsumer.Start(ctx); err != nil {
			logger.Fatalf("market index consumer: %v", err)
		}
		logger.Infof("Market index consumer started")
	}
	var boot *bootstrap.Coordinator
	if natsClient != nil {
		pusherHub = pusher.NewHub(cfg, wsTicketStore)
		pusherService = pusher.NewService(natsClient, pusherHub)
		pgWriter := writer.New(pool, natsClient, redisClient, "")
		if cfg.SettlementRelayerKeypair != "" && cfg.ProgramID != "" {
			programID := solana.MustPublicKeyFromBase58(cfg.ProgramID)
			relayer, err := faucet.LoadKeypair(cfg.SettlementRelayerKeypair)
			if err != nil {
				logger.Fatalf("settlement relayer keypair: %v", err)
			}
			settlementService = settlement.NewService(natsClient, pool, cfg.SolanaRPCURL, programID, relayer, "")
		}
		boot = bootstrap.NewCoordinator(pgWriter, marketManager, pusherService, settlementService, cfg.MatcherTickInterval)
		if err := boot.Start(ctx); err != nil {
			logger.Fatalf("bootstrap: %v", err)
		}
		logger.Infof("Bootstrap completed")
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
		&zerologLogger,
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
