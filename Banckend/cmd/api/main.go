package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/db"
	"blinkpredict/banckend/internal/faucet"
	httpapi "blinkpredict/banckend/internal/http"
	"blinkpredict/banckend/internal/indexer"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/txreqs"

	"github.com/gagliardetto/solana-go"
)

func main() {
	cfg := config.Load()

	var faucetSvc faucet.Service = faucet.DisabledService{}
	if cfg.VUSDCMint != "" && cfg.DatabaseURL != "" && cfg.FaucetPayerKeypair != "" {
		ctx := context.Background()
		pool, err := db.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("database: %v", err)
		}
		mint := solana.MustPublicKeyFromBase58(cfg.VUSDCMint)
		payer, err := faucet.LoadKeypair(cfg.FaucetPayerKeypair)
		if err != nil {
			log.Fatalf("faucet payer keypair: %v", err)
		}
		mintAuthority, err := faucet.LoadKeypair(cfg.FaucetMintAuthorityKeypair)
		if err != nil {
			log.Fatalf("faucet mint authority keypair: %v", err)
		}
		repo := faucet.NewPostgresClaimsRepository(pool)
		service, err := faucet.NewSolanaService(faucet.SolanaServiceConfig{
			RPCURL:        cfg.SolanaRPCURL,
			Mint:          mint,
			Decimals:      cfg.VUSDCDecimals,
			Payer:         payer,
			MintAuthority: mintAuthority,
			AmountTokens:  cfg.FaucetAmount,
			Cooldown:      24 * time.Hour,
			DisableRateLimit: cfg.FaucetDisableRateLimit,
		}, repo)
		if err != nil {
			log.Fatalf("faucet service: %v", err)
		}
		faucetSvc = service
	} else {
		log.Printf("Faucet disabled (set DATABASE_URL, VUSDC_MINT, FAUCET_PAYER_KEYPAIR to enable).")
	}

	server := httpapi.New(
		cfg,
		markets.NewMemoryRepository(),
		matching.NewDisabledEngine(),
		noopIndexer{},
		txreqs.NewStore(),
		faucetSvc,
	)

	log.Printf("BlinkPredict Banckend listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, server.Router()); err != nil {
		log.Fatal(err)
	}
}

type noopIndexer struct{}

func (noopIndexer) Start(_ context.Context) error { return nil }
func (noopIndexer) Stop(_ context.Context) error  { return nil }

var _ indexer.Listener = noopIndexer{}
