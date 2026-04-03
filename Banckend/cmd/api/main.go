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
	var wsTicketStore *pusher.TicketStore
	var pusherHub *pusher.Hub
	var pusherService *pusher.Service
	var settlementService *settlement.Service
	var depositConfirmService *depositconfirm.Service
	var marketConfirmService *marketconfirm.Service
	var marketProjector *marketconfirm.Projector
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
		wsTicketStore = pusher.NewTicketStore(redisClient, 45*time.Second)
		matchingEngine = query.NewRedisEngine(redisClient, pool)
		if fundsService != nil {
			fundsService = funds.NewService(natsClient, pool, redisClient, fundsManager)
		}

		// 初始化市场缓存
		marketCache = cache.NewMarketCache(redisClient, zerologLogger)
		logger.Infof("Market cache initialized")
	}

	// 初始化 Helius Webhook Handler
	var webhookHandler *webhooks.HeliusHandler
	if cfg.HeliusWebhookEnabled && cfg.HeliusWebhookSecret != "" {
		if cfg.VUSDCMint == "" || cfg.GlobalVault == "" {
			logger.Warnf("Helius webhook enabled but VUSDC_MINT/GLOBAL_VAULT is missing")
		} else {
			depositProjector := webhooks.NewDepositProjector(pool, redisClient, fundsManager, zerologLogger)
			webhookHandler = webhooks.NewHeliusHandler(
				depositProjector,
				zerologLogger,
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
			zerologLogger,
			cfg.AlchemySigningKey,
			cfg.ProgramID,
		)
		logger.Infof("Alchemy webhook enabled")
	} else {
		logger.Infof("Alchemy webhook disabled (set ALCHEMY_SIGNING_KEY to enable)")
	}
	boot := bootstrap.NewGate()
	if natsClient != nil {
		pusherHub = pusher.NewHub(cfg, wsTicketStore)
		pusherService = pusher.NewService(natsClient, pusherHub)
		pgWriter := writer.New(pool, natsClient, redisClient, "")
		marketProjector = marketconfirm.NewProjector(natsClient, pool, marketRepo, marketCache)
		if cfg.SettlementRelayerKeypair != "" && cfg.ProgramID != "" {
			programID := solana.MustPublicKeyFromBase58(cfg.ProgramID)
			relayer, err := faucet.LoadKeypair(cfg.SettlementRelayerKeypair)
			if err != nil {
				logger.Fatalf("settlement relayer keypair: %v", err)
			}
			settlementService = settlement.NewService(natsClient, pool, cfg.SolanaRPCURL, programID, relayer, "")
		}
		if pgWriter != nil {
			boot.Set("writer", bootstrap.StateCatchingUp)
			logger.Infof("starting writer catch-up")
			if err := pgWriter.Start(ctx); err != nil {
				boot.Set("writer", bootstrap.StateFailed)
				logger.Fatalf("writer: %v", err)
			}
			boot.Set("writer", bootstrap.StateReady)
		}
		if fundsService != nil {
			boot.Set("funds", bootstrap.StateRecovering)
			logger.Infof("recovering funds snapshots")
			if err := fundsService.Start(ctx); err != nil {
				boot.Set("funds", bootstrap.StateFailed)
				logger.Fatalf("funds: %v", err)
			}
			boot.Set("funds", bootstrap.StateReady)
		}
		if marketManager != nil {
			boot.Set("matcher", bootstrap.StateRecovering)
			logger.Infof("recovering matcher from orders")
			if err := marketManager.RecoverFromStore(ctx); err != nil {
				boot.Set("matcher", bootstrap.StateFailed)
				logger.Fatalf("matcher recover: %v", err)
			}
			logger.Infof("running bootstrap tick")
			if err := marketManager.RunBootstrapTick(ctx); err != nil {
				boot.Set("matcher", bootstrap.StateFailed)
				logger.Fatalf("matcher bootstrap tick: %v", err)
			}
			go func() {
				if err := marketManager.StartConsumer(ctx); err != nil {
					logger.Warnf("matcher consumer stopped: %v", err)
					boot.Set("matcher", bootstrap.StateFailed)
				}
			}()
			marketManager.StartTickLoop(ctx, cfg.MatcherTickInterval)
			boot.Set("matcher", bootstrap.StateReady)
		}
		if pusherService != nil {
			boot.Set("pusher", bootstrap.StateStarting)
			logger.Infof("starting pusher service")
			if err := pusherService.Start(ctx); err != nil {
				boot.Set("pusher", bootstrap.StateFailed)
				logger.Fatalf("pusher: %v", err)
			}
			boot.Set("pusher", bootstrap.StateReady)
		}
		if cfg.SolanaRPCURL != "" {
			depositConfirmService = depositconfirm.NewService(natsClient, pool, cfg)
		}
		if depositConfirmService != nil {
			boot.Set("deposit-confirm", bootstrap.StateStarting)
			logger.Infof("starting deposit confirm service")
			if err := depositConfirmService.Start(ctx); err != nil {
				boot.Set("deposit-confirm", bootstrap.StateFailed)
				logger.Fatalf("deposit confirm: %v", err)
			}
			boot.Set("deposit-confirm", bootstrap.StateReady)
		}
		if cfg.SolanaRPCURL != "" {
			marketConfirmService = marketconfirm.NewService(natsClient, pool, cfg)
		}
		if marketConfirmService != nil {
			boot.Set("market-confirm", bootstrap.StateStarting)
			logger.Infof("starting market confirm service")
			if err := marketConfirmService.Start(ctx); err != nil {
				boot.Set("market-confirm", bootstrap.StateFailed)
				logger.Fatalf("market confirm: %v", err)
			}
			boot.Set("market-confirm", bootstrap.StateReady)
		}
		if marketProjector != nil {
			boot.Set("market-projector", bootstrap.StateStarting)
			logger.Infof("starting market projector")
			if err := marketProjector.Start(ctx); err != nil {
				boot.Set("market-projector", bootstrap.StateFailed)
				logger.Fatalf("market projector: %v", err)
			}
			boot.Set("market-projector", bootstrap.StateReady)
		}
		if settlementService != nil {
			boot.Set("settlement", bootstrap.StateStarting)
			logger.Infof("starting settlement service")
			if err := settlementService.Start(ctx); err != nil {
				boot.Set("settlement", bootstrap.StateFailed)
				logger.Fatalf("settlement: %v", err)
			}
			boot.Set("settlement", bootstrap.StateReady)
		}
		boot.MarkReady()
		logger.Infof("bootstrap ready; write traffic enabled")
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
