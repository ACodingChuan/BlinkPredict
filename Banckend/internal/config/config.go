package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port              string
	DatabaseURL       string
	SolanaRPCURL      string
	ProgramID         string
	PrivyAppID        string
	AdminEmails       map[string]struct{}
	AdminWallets      map[string]struct{}
	DefaultCollateral string

	VUSDCMint                 string
	VUSDCDecimals             int
	FaucetPayerKeypair        string
	FaucetMintAuthorityKeypair string
	FaucetAmount              uint64
}

func Load() Config {
	dbURL := buildDatabaseURLFromParts()
	if dbURL == "" {
		// Back-compat; prefer split env vars going forward.
		dbURL = os.Getenv("DATABASE_URL")
	}
	return Config{
		Port:              getEnv("PORT", "8080"),
		DatabaseURL:       dbURL,
		SolanaRPCURL:      getEnv("SOLANA_RPC_URL", "https://api.devnet.solana.com"),
		ProgramID:         getEnv("PROGRAM_ID", "2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE"),
		PrivyAppID:        os.Getenv("PRIVY_APP_ID"),
		AdminEmails:       csvSet(os.Getenv("ADMIN_EMAILS")),
		AdminWallets:      csvSet(os.Getenv("ADMIN_WALLETS")),
		DefaultCollateral: getEnv("DEFAULT_COLLATERAL_MINT", "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"),

		VUSDCMint:                 os.Getenv("VUSDC_MINT"),
		VUSDCDecimals:             getEnvInt("VUSDC_DECIMALS", 6),
		FaucetPayerKeypair:        os.Getenv("FAUCET_PAYER_KEYPAIR"),
		FaucetMintAuthorityKeypair: getEnv("FAUCET_MINT_AUTHORITY_KEYPAIR", os.Getenv("FAUCET_PAYER_KEYPAIR")),
		FaucetAmount:              uint64(getEnvInt("FAUCET_AMOUNT", 500)),
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
