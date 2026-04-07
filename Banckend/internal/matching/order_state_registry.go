package matching

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type OrderStateKey struct {
	MarketID uint64
	Wallet   string
	Nonce    uint64
}

type OrderStateRegistry struct {
	mu     sync.RWMutex
	exists map[OrderStateKey]struct{}
}

func NewOrderStateRegistry() *OrderStateRegistry {
	return &OrderStateRegistry{exists: make(map[OrderStateKey]struct{})}
}

func (r *OrderStateRegistry) Load(keys []OrderStateKey) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		key.Wallet = strings.TrimSpace(key.Wallet)
		if key.MarketID == 0 || key.Wallet == "" || key.Nonce == 0 {
			continue
		}
		r.exists[key] = struct{}{}
	}
}

func (r *OrderStateRegistry) Has(marketID uint64, wallet string, nonce uint64) bool {
	if r == nil {
		return false
	}
	wallet = strings.TrimSpace(wallet)
	if marketID == 0 || wallet == "" || nonce == 0 {
		return false
	}
	r.mu.RLock()
	_, ok := r.exists[OrderStateKey{MarketID: marketID, Wallet: wallet, Nonce: nonce}]
	r.mu.RUnlock()
	return ok
}

func (r *OrderStateRegistry) MarkExists(marketID uint64, wallet string, nonce uint64) {
	if r == nil {
		return
	}
	wallet = strings.TrimSpace(wallet)
	if marketID == 0 || wallet == "" || nonce == 0 {
		return
	}
	r.mu.Lock()
	r.exists[OrderStateKey{MarketID: marketID, Wallet: wallet, Nonce: nonce}] = struct{}{}
	r.mu.Unlock()
}

func (r *OrderStateRegistry) Size() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.exists)
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
