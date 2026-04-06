package matching

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type UserPositionKey struct {
	MarketID uint64
	Wallet   string
}

type UserPositionRegistry struct {
	mu     sync.RWMutex
	exists map[UserPositionKey]struct{}
}

func NewUserPositionRegistry() *UserPositionRegistry {
	return &UserPositionRegistry{exists: make(map[UserPositionKey]struct{})}
}

func (r *UserPositionRegistry) Load(keys []UserPositionKey) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		key.Wallet = strings.TrimSpace(key.Wallet)
		if key.MarketID == 0 || key.Wallet == "" {
			continue
		}
		r.exists[key] = struct{}{}
	}
}

func (r *UserPositionRegistry) MarkExists(marketID uint64, wallet string) {
	if r == nil {
		return
	}
	wallet = strings.TrimSpace(wallet)
	if marketID == 0 || wallet == "" {
		return
	}
	r.mu.Lock()
	r.exists[UserPositionKey{MarketID: marketID, Wallet: wallet}] = struct{}{}
	r.mu.Unlock()
}

func (r *UserPositionRegistry) Has(marketID uint64, wallet string) bool {
	if r == nil {
		return false
	}
	wallet = strings.TrimSpace(wallet)
	if marketID == 0 || wallet == "" {
		return false
	}
	r.mu.RLock()
	_, ok := r.exists[UserPositionKey{MarketID: marketID, Wallet: wallet}]
	r.mu.RUnlock()
	return ok
}

func (r *UserPositionRegistry) Size() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.exists)
}

type UserPositionAccountRepo struct {
	pool *pgxpool.Pool
}

func NewUserPositionAccountRepo(pool *pgxpool.Pool) *UserPositionAccountRepo {
	return &UserPositionAccountRepo{pool: pool}
}

func (r *UserPositionAccountRepo) LoadAll(ctx context.Context) ([]UserPositionKey, error) {
	if r == nil || r.pool == nil {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT market_id, wallet_address
		FROM user_position_accounts
	`)
	if err != nil {
		return nil, fmt.Errorf("query user_position_accounts: %w", err)
	}
	defer rows.Close()

	keys := make([]UserPositionKey, 0, 1024)
	for rows.Next() {
		var key UserPositionKey
		if err := rows.Scan(&key.MarketID, &key.Wallet); err != nil {
			return nil, fmt.Errorf("scan user_position_accounts: %w", err)
		}
		if key.MarketID == 0 || strings.TrimSpace(key.Wallet) == "" {
			continue
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user_position_accounts: %w", err)
	}
	return keys, nil
}
