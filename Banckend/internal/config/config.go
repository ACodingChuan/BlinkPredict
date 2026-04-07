package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                      string
	DatabaseURL               string
	SolanaRPCURL              string
	SolanaWSURL               string
	ProgramID                 string
	AuthTokenSecret           string
	LogLevel                  string
	AdminEmails               map[string]struct{}
	AdminWallets              map[string]struct{}
	NATSURL                   string
	NATSJSDomain              string
	NATSStreamCMD             string
	NATSStreamEVT             string
	NATSStreamWHK             string
	RedisURL                  string
	MatcherTickInterval       time.Duration
	MatcherBatchMaxFillsHot   int
	MatcherBatchMaxFillsCold  int
	MatcherBatchMaxOrders     int
	MatcherBatchMaxBytes      int
	MatcherBatchMaxAge        time.Duration
	MatcherBatchIdleFlush     time.Duration
	MatcherBatchFlushTick     time.Duration
	MatcherCheckpointInterval time.Duration

	VUSDCMint                  string
	GlobalVault                string
	VUSDCDecimals              int
	FaucetPayerKeypair         string
	FaucetMintAuthorityKeypair string
	FaucetAmount               uint64
	FaucetDisableRateLimit     bool

	// Helius Webhook Configuration
	HeliusAPIKey         string
	HeliusWebhookSecret  string
	HeliusWebhookEnabled bool

	// Alchemy Webhook Configuration
	AlchemySigningKey string

	SettlementRelayerKeypair    string
	SettlementReconcileInterval time.Duration
	SettlementPrepareWorkerTick time.Duration
	SettlementSchedulerScan     time.Duration
	SettlementSubmittedPoll     time.Duration
	SettlementRebroadcast       time.Duration
	SettlementSubmittedHeight   time.Duration
	SettlementTerminalPoll      time.Duration
	SettlementTerminalBatchSize int
	SettlementBlockhashPoll     time.Duration
	SettlementBlockhashMaxAge   time.Duration
	SettlementSendSkipPreflight bool
	SettlementTxMaxBytes        int
	SettlementStaticALTIDs      []string
}

func Load() Config {
	dbURL := buildDatabaseURLFromParts()
	if dbURL == "" {
		// Back-compat; prefer split env vars going forward.
		dbURL = os.Getenv("DATABASE_URL")
	}
	vusdcMint := strings.TrimSpace(os.Getenv("VUSDC_MINT"))
	matcherHotFills := getEnvIntFallback([]string{"MATCHER_MARKET_MAX_FILLS_HOT", "MATCHER_MARKET_MAX_FILLS", "MATCHER_BATCH_MAX_FILLS"}, 64)
	matcherColdFills := getEnvInt("MATCHER_MARKET_MAX_FILLS_COLD", matcherHotFills/4)
	if matcherColdFills <= 0 {
		matcherColdFills = 1
	}
	return Config{
		Port:                      getEnv("PORT", "8080"),
		DatabaseURL:               dbURL,
		SolanaRPCURL:              getEnv("SOLANA_RPC_URL", "https://api.devnet.solana.com"),
		SolanaWSURL:               strings.TrimSpace(os.Getenv("SOLANA_WS_URL")),
		ProgramID:                 getEnv("PROGRAM_ID", "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE"),
		AuthTokenSecret:           getEnv("AUTH_TOKEN_SECRET", "blinkpredict-dev-auth-secret"),
		LogLevel:                  getEnv("LOG_LEVEL", "info"),
		AdminEmails:               csvSet(os.Getenv("ADMIN_EMAILS")),
		AdminWallets:              csvSet(os.Getenv("ADMIN_WALLETS")),
		NATSURL:                   os.Getenv("NATS_URL"),
		NATSJSDomain:              os.Getenv("NATS_JS_DOMAIN"),
		NATSStreamCMD:             getEnv("NATS_STREAM_CMD", "AP_CMD"),
		NATSStreamEVT:             getEnv("NATS_STREAM_EVT", "AP_EVT"),
		NATSStreamWHK:             getEnv("NATS_STREAM_WHK", "AP_WHK"),
		RedisURL:                  strings.TrimSpace(os.Getenv("REDIS_URL")),
		MatcherTickInterval:       getEnvDuration("MATCHER_TICK_INTERVAL", time.Second),
		MatcherBatchMaxFillsHot:   matcherHotFills,
		MatcherBatchMaxFillsCold:  matcherColdFills,
		MatcherBatchMaxOrders:     getEnvInt("MATCHER_BATCH_MAX_ORDERS", 96),
		MatcherBatchMaxBytes:      getEnvInt("MATCHER_BATCH_MAX_BYTES", 262144),
		MatcherBatchMaxAge:        time.Duration(getEnvInt("MATCHER_BATCH_MAX_AGE_MS", 40)) * time.Millisecond,
		MatcherBatchIdleFlush:     time.Duration(getEnvInt("MATCHER_BATCH_IDLE_FLUSH_MS", 15)) * time.Millisecond,
		MatcherBatchFlushTick:     time.Duration(getEnvInt("MATCHER_BATCH_FLUSH_TICK_MS", 10)) * time.Millisecond,
		MatcherCheckpointInterval: getEnvDuration("MATCHER_CHECKPOINT_INTERVAL", 100*time.Millisecond),

		VUSDCMint:                  vusdcMint,
		GlobalVault:                strings.TrimSpace(os.Getenv("GLOBAL_VAULT")),
		VUSDCDecimals:              getEnvInt("VUSDC_DECIMALS", 6),
		FaucetPayerKeypair:         os.Getenv("FAUCET_PAYER_KEYPAIR"),
		FaucetMintAuthorityKeypair: getEnv("FAUCET_MINT_AUTHORITY_KEYPAIR", os.Getenv("FAUCET_PAYER_KEYPAIR")),
		FaucetAmount:               uint64(getEnvInt("FAUCET_AMOUNT", 500)),
		FaucetDisableRateLimit:     getEnvBool("FAUCET_DISABLE_RATE_LIMIT", false),

		// Helius Webhook Configuration
		HeliusAPIKey:         getEnv("HELIUS_API_KEY", ""),
		HeliusWebhookSecret:  getEnv("HELIUS_WEBHOOK_SECRET", ""),
		HeliusWebhookEnabled: getEnvBool("HELIUS_WEBHOOK_ENABLED", false),

		// Alchemy Webhook Configuration
		AlchemySigningKey: getEnv("ALCHEMY_SIGNING_KEY", ""),

		SettlementRelayerKeypair:    strings.TrimSpace(os.Getenv("SETTLEMENT_RELAYER_KEYPAIR")),
		SettlementReconcileInterval: getEnvDuration("SETTLEMENT_RECONCILE_INTERVAL", 10*time.Second),
		SettlementPrepareWorkerTick: getEnvDuration("SETTLEMENT_PREPARE_WORKER_TICK", 300*time.Millisecond),
		SettlementSchedulerScan:     getEnvDuration("SETTLEMENT_SCHEDULER_FALLBACK_SCAN", 300*time.Millisecond),
		SettlementSubmittedPoll:     getEnvDuration("SETTLEMENT_SUBMITTED_STATUS_POLL", 15*time.Second),
		SettlementRebroadcast:       getEnvDuration("SETTLEMENT_SUBMITTED_REBROADCAST", 8*time.Second),
		SettlementSubmittedHeight:   getEnvDuration("SETTLEMENT_SUBMITTED_BLOCK_POLL", 15*time.Second),
		SettlementTerminalPoll:      getEnvDuration("SETTLEMENT_TERMINAL_POLL_INTERVAL", 12*time.Second),
		SettlementTerminalBatchSize: getEnvInt("SETTLEMENT_TERMINAL_POLL_BATCH_SIZE", 128),
		SettlementBlockhashPoll:     getEnvDuration("SETTLEMENT_BLOCKHASH_POLL_INTERVAL", 15*time.Second),
		SettlementBlockhashMaxAge:   getEnvDuration("SETTLEMENT_BLOCKHASH_MAX_CACHE_AGE", 45*time.Second),
		SettlementSendSkipPreflight: getEnvBool("SETTLEMENT_SEND_SKIP_PREFLIGHT", true),
		SettlementTxMaxBytes:        getEnvInt("SETTLEMENT_TX_MAX_BYTES", 1232),
		SettlementStaticALTIDs:      csvList(os.Getenv("SETTLEMENT_STATIC_ALT_IDS")),
	}
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnvIntFallback(keys []string, fallback int) int {
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		return parsed
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func csvSet(value string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		set[strings.ToLower(item)] = struct{}{}
	}
	return set
}

func csvList(value string) []string {
	out := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func buildDatabaseURLFromParts() string {
	// New-style DB env vars (preferred):
	// - DB_HOST
	// - DB_PORT
	// - APP_DB
	// - APP_DB_USER
	// - APP_DB_PASSWORD
	// Optional:
	// - DB_SSLMODE (defaults to disable)
	host := strings.TrimSpace(os.Getenv("DB_HOST"))
	port := strings.TrimSpace(os.Getenv("DB_PORT"))
	dbName := strings.TrimSpace(os.Getenv("APP_DB"))
	user := strings.TrimSpace(os.Getenv("APP_DB_USER"))
	pass := os.Getenv("APP_DB_PASSWORD")
	sslmode := getEnv("DB_SSLMODE", "disable")

	if host == "" || dbName == "" || user == "" {
		return ""
	}
	if port == "" {
		port = "5432"
	}
	if _, err := strconv.Atoi(port); err != nil {
		port = "5432"
	}

	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + dbName,
	}
	if pass != "" {
		u.User = url.UserPassword(user, pass)
	} else {
		u.User = url.User(user)
	}
	q := u.Query()
	if sslmode != "" {
		q.Set("sslmode", sslmode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
