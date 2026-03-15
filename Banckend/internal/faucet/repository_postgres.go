package faucet

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ClaimsRepository interface {
	LastClaimedAtByWallet(ctx context.Context, solanaAddress string) (time.Time, bool, error)
	LastClaimedAtByIP(ctx context.Context, ip string) (time.Time, bool, error)
	InsertClaim(ctx context.Context, row ClaimRow) error
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

