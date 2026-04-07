package settlement

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type OrderStateAccountRecord struct {
	MarketID         uint64
	WalletAddress    string
	Nonce            uint64
	OrderStatePDA    string
	CreatedByRelayer string
	CreatedTxSig     string
}

type OrderStateAccountRepo struct {
	pool *pgxpool.Pool
}

func NewOrderStateAccountRepo(pool *pgxpool.Pool) *OrderStateAccountRepo {
	return &OrderStateAccountRepo{pool: pool}
}

func (r *OrderStateAccountRepo) LoadAll(ctx context.Context) ([]OrderStateKey, error) {
	if r == nil || r.pool == nil {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT market_id, wallet_address, nonce
		FROM order_state_accounts
	`)
	if err != nil {
		return nil, fmt.Errorf("query order_state_accounts: %w", err)
	}
	defer rows.Close()

	keys := make([]OrderStateKey, 0, 2048)
	for rows.Next() {
		var key OrderStateKey
		if err := rows.Scan(&key.MarketID, &key.Wallet, &key.Nonce); err != nil {
			return nil, fmt.Errorf("scan order_state_accounts: %w", err)
		}
		key.Wallet = strings.TrimSpace(key.Wallet)
		if key.MarketID == 0 || key.Wallet == "" || key.Nonce == 0 {
			continue
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate order_state_accounts: %w", err)
	}
	return keys, nil
}

func (r *OrderStateAccountRepo) UpsertObserved(ctx context.Context, records []OrderStateAccountRecord) error {
	if r == nil || r.pool == nil || len(records) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	queued := 0
	for _, record := range records {
		record.WalletAddress = strings.TrimSpace(record.WalletAddress)
		record.OrderStatePDA = strings.TrimSpace(record.OrderStatePDA)
		record.CreatedByRelayer = strings.TrimSpace(record.CreatedByRelayer)
		record.CreatedTxSig = strings.TrimSpace(record.CreatedTxSig)
		if record.MarketID == 0 || record.WalletAddress == "" || record.Nonce == 0 || record.OrderStatePDA == "" {
			continue
		}
		batch.Queue(`
			INSERT INTO order_state_accounts (
				market_id,
				wallet_address,
				nonce,
				order_state_pda,
				created_by_relayer,
				created_tx_sig,
				first_observed_at,
				last_observed_at
			) VALUES (
				$1::NUMERIC(20,0),
				$2,
				$3,
				$4,
				NULLIF($5, ''),
				NULLIF($6, ''),
				NOW(),
				NOW()
			)
			ON CONFLICT (market_id, wallet_address, nonce) DO UPDATE SET
				order_state_pda = EXCLUDED.order_state_pda,
				created_by_relayer = COALESCE(order_state_accounts.created_by_relayer, EXCLUDED.created_by_relayer),
				created_tx_sig = COALESCE(order_state_accounts.created_tx_sig, EXCLUDED.created_tx_sig),
				last_observed_at = NOW()
		`, record.MarketID, record.WalletAddress, int64(record.Nonce), record.OrderStatePDA, record.CreatedByRelayer, record.CreatedTxSig)
		queued++
	}
	if queued == 0 {
		return nil
	}
	results := r.pool.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	for i := 0; i < queued; i++ {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("upsert order_state_accounts: %w", err)
		}
	}
	return nil
}

func LoadOrderStateRegistryFromRepo(ctx context.Context, repo *OrderStateAccountRepo, registry *OrderStateRegistry) error {
	if repo == nil || registry == nil {
		return nil
	}
	keys, err := repo.LoadAll(ctx)
	if err != nil {
		return err
	}
	registry.Load(keys)
	return nil
}
