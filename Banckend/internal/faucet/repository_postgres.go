package faucet

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ClaimsRepository interface {
	LastClaimedAtByWallet(ctx context.Context, solanaAddress string) (time.Time, bool, error)
	LastClaimedAtByIP(ctx context.Context, ip string) (time.Time, bool, error)
	InsertClaim(ctx context.Context, row ClaimRow) error
	CreditWalletAccount(ctx context.Context, walletAddress string, amountUnits uint64) error
	InsertDepositRequest(ctx context.Context, row DepositRequestRow) error
}

type ClaimRow struct {
	SolanaAddress string
	IP            string
	Signature     string
	Amount        uint64
	Mint          string
	ATA           string
	ClaimedAt     time.Time
}

type DepositRequestRow struct {
	ID                  string
	WalletAddress       string
	AmountUnits         uint64
	Mint                string
	TreasuryDestination string
	ChainSignature      string
	Status              string
	Source              string
	CreatedAt           time.Time
	ConfirmedAt         *time.Time
}

type PostgresClaimsRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresClaimsRepository(pool *pgxpool.Pool) *PostgresClaimsRepository {
	return &PostgresClaimsRepository{pool: pool}
}

func (r *PostgresClaimsRepository) LastClaimedAtByWallet(ctx context.Context, solanaAddress string) (time.Time, bool, error) {
	var at time.Time
	err := r.pool.QueryRow(
		ctx,
		`select claimed_at from faucet_claims
		 where solana_address = $1
		 order by claimed_at desc
		 limit 1`,
		solanaAddress,
	).Scan(&at)
	if err != nil {
		if isNoRows(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("query faucet_claims by wallet: %w", err)
	}
	return at, true, nil
}

func (r *PostgresClaimsRepository) LastClaimedAtByIP(ctx context.Context, ip string) (time.Time, bool, error) {
	var at time.Time
	err := r.pool.QueryRow(
		ctx,
		`select claimed_at from faucet_claims
		 where ip = $1
		 order by claimed_at desc
		 limit 1`,
		ip,
	).Scan(&at)
	if err != nil {
		if isNoRows(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("query faucet_claims by ip: %w", err)
	}
	return at, true, nil
}

func (r *PostgresClaimsRepository) InsertClaim(ctx context.Context, row ClaimRow) error {
	_, err := r.pool.Exec(
		ctx,
		`insert into faucet_claims (solana_address, ip, signature, amount, mint, ata, claimed_at)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		row.SolanaAddress,
		row.IP,
		row.Signature,
		row.Amount,
		row.Mint,
		row.ATA,
		row.ClaimedAt,
	)
	if err != nil {
		return fmt.Errorf("insert faucet_claim: %w", err)
	}
	return nil
}

func (r *PostgresClaimsRepository) CreditWalletAccount(ctx context.Context, walletAddress string, amountUnits uint64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO wallet_accounts (wallet_address, collateral_total_units, collateral_free_units, updated_at)
		VALUES ($1, $2, $2, NOW())
		ON CONFLICT (wallet_address) DO UPDATE SET
			collateral_total_units = wallet_accounts.collateral_total_units + EXCLUDED.collateral_total_units,
			collateral_free_units = wallet_accounts.collateral_free_units + EXCLUDED.collateral_free_units,
			updated_at = NOW()
	`, walletAddress, amountUnits)
	if err != nil {
		return fmt.Errorf("credit wallet_accounts: %w", err)
	}
	return nil
}

func (r *PostgresClaimsRepository) InsertDepositRequest(ctx context.Context, row DepositRequestRow) error {
	if row.ID == "" {
		row.ID = uuid.NewString()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO deposit_requests (
			id, wallet_address, amount_units, mint, treasury_destination,
			chain_signature, status, source, created_at, confirmed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, row.ID, row.WalletAddress, row.AmountUnits, row.Mint, row.TreasuryDestination, row.ChainSignature, row.Status, row.Source, row.CreatedAt, row.ConfirmedAt)
	if err != nil {
		return fmt.Errorf("insert deposit_request: %w", err)
	}
	return nil
}
