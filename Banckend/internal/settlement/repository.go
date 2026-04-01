package settlement

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// UserPositionAccountRecord is the durable representation used by webhook persistence.
type UserPositionAccountRecord struct {
	MarketID         uint64
	WalletAddress    string
	UserPositionPDA  string
	CreatedByRelayer string
	CreatedTxSig     string
}

func (r *UserPositionAccountRepo) UpsertObserved(ctx context.Context, records []UserPositionAccountRecord) error {
	if r == nil || r.pool == nil || len(records) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(`
			INSERT INTO user_position_accounts (
				market_id, wallet_address, user_position_pda,
				created_by_relayer, created_tx_sig,
				first_confirmed_at, last_observed_at
			) VALUES ($1::NUMERIC(20,0), $2, $3, $4, $5, NOW(), NOW())
			ON CONFLICT (market_id, wallet_address) DO UPDATE SET
				user_position_pda = EXCLUDED.user_position_pda,
				created_by_relayer = COALESCE(EXCLUDED.created_by_relayer, user_position_accounts.created_by_relayer),
				created_tx_sig = COALESCE(EXCLUDED.created_tx_sig, user_position_accounts.created_tx_sig),
				last_observed_at = NOW()
		`, strconv.FormatUint(record.MarketID, 10), record.WalletAddress, record.UserPositionPDA, nullableString(record.CreatedByRelayer), nullableString(record.CreatedTxSig))
	}
	results := r.pool.SendBatch(ctx, batch)
	defer results.Close()
	for range records {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert user_position_accounts: %w", err)
		}
	}
	return nil
}

func (r *UserPositionAccountRepo) Delete(ctx context.Context, marketID uint64, wallet string) error {
	if r == nil || r.pool == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		DELETE FROM user_position_accounts
		WHERE market_id = $1::NUMERIC(20,0) AND wallet_address = $2
	`, strconv.FormatUint(marketID, 10), wallet)
	if err != nil {
		return fmt.Errorf("delete user_position_account: %w", err)
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
