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
	Port                string
	DatabaseURL         string
	SolanaRPCURL        string
	ProgramID           string
	PrivyAppID          string
	LogLevel            string
	AdminEmails         map[string]struct{}
	AdminWallets        map[string]struct{}
	NATSURL             string
	NATSJSDomain        string
	NATSStreamCMD       string
	NATSStreamEVT       string
	RedisURL            string
	MatcherTickInterval time.Duration
	OrderbookDepth      int

	VUSDCMint                  string
	VUSDCDecimals              int
	FaucetPayerKeypair         string
	FaucetMintAuthorityKeypair string
	FaucetAmount               uint64
	FaucetDisableRateLimit     bool

	// Helius Webhook Configuration
	HeliusAPIKey        string
	HeliusWebhookSecret string
	HeliusWebhookEnabled bool

	// Alchemy Webhook Configuration
	AlchemySigningKey string
}

func Load() Config {
	dbURL := buildDatabaseURLFromParts()
	if dbURL == "" {
		// Back-compat; prefer split env vars going forward.
		dbURL = os.Getenv("DATABASE_URL")
	}
	vusdcMint := strings.TrimSpace(os.Getenv("VUSDC_MINT"))
	return Config{
		Port:                getEnv("PORT", "8080"),
		DatabaseURL:         dbURL,
		SolanaRPCURL:        getEnv("SOLANA_RPC_URL", "https://api.devnet.solana.com"),
		ProgramID:           getEnv("PROGRAM_ID", "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE"),
		PrivyAppID:          os.Getenv("PRIVY_APP_ID"),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		AdminEmails:         csvSet(os.Getenv("ADMIN_EMAILS")),
		AdminWallets:        csvSet(os.Getenv("ADMIN_WALLETS")),
		NATSURL:             os.Getenv("NATS_URL"),
		NATSJSDomain:        os.Getenv("NATS_JS_DOMAIN"),
		NATSStreamCMD:       getEnv("NATS_STREAM_CMD", "AP_CMD"),
		NATSStreamEVT:       getEnv("NATS_STREAM_EVT", "AP_EVT"),
		RedisURL:            strings.TrimSpace(os.Getenv("REDIS_URL")),
		MatcherTickInterval: getEnvDuration("MATCHER_TICK_INTERVAL", time.Second),
		OrderbookDepth:      getEnvInt("ORDERBOOK_DEPTH_LEVELS", 20),

		VUSDCMint:                  vusdcMint,
		VUSDCDecimals:              getEnvInt("VUSDC_DECIMALS", 6),
		FaucetPayerKeypair:         os.Getenv("FAUCET_PAYER_KEYPAIR"),
		FaucetMintAuthorityKeypair: getEnv("FAUCET_MINT_AUTHORITY_KEYPAIR", os.Getenv("FAUCET_PAYER_KEYPAIR")),
		FaucetAmount:               uint64(getEnvInt("FAUCET_AMOUNT", 500)),
		FaucetDisableRateLimit:     getEnvBool("FAUCET_DISABLE_RATE_LIMIT", false),

		// Helius Webhook Configuration
		HeliusAPIKey:        getEnv("HELIUS_API_KEY", ""),
		HeliusWebhookSecret: getEnv("HELIUS_WEBHOOK_SECRET", ""),
		HeliusWebhookEnabled: getEnvBool("HELIUS_WEBHOOK_ENABLED", false),

		// Alchemy Webhook Configuration
		AlchemySigningKey: getEnv("ALCHEMY_SIGNING_KEY", ""),
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
