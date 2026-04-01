package settlement

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UserPositionKey identifies one on-chain UserPosition PDA by market and wallet.
type UserPositionKey struct {
	MarketID uint64
	Wallet   string
}

// UserPositionRegistry is the in-memory hot-path index used by settlement.
type UserPositionRegistry struct {
	mu     sync.RWMutex
	exists map[UserPositionKey]struct{}
}

func NewUserPositionRegistry() *UserPositionRegistry {
	return &UserPositionRegistry{exists: make(map[UserPositionKey]struct{})}
}

func (r *UserPositionRegistry) Load(keys []UserPositionKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		r.exists[key] = struct{}{}
	}
}

func (r *UserPositionRegistry) Has(marketID uint64, wallet string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.exists[UserPositionKey{MarketID: marketID, Wallet: wallet}]
	return ok
}

func (r *UserPositionRegistry) MarkExists(marketID uint64, wallet string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exists[UserPositionKey{MarketID: marketID, Wallet: wallet}] = struct{}{}
}

func (r *UserPositionRegistry) FilterUnknown(marketID uint64, wallets []string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	unknown := make([]string, 0, len(wallets))
	seen := make(map[string]struct{}, len(wallets))
	for _, wallet := range wallets {
		if wallet == "" {
			continue
		}
		if _, ok := seen[wallet]; ok {
			continue
		}
		seen[wallet] = struct{}{}
		if _, ok := r.exists[UserPositionKey{MarketID: marketID, Wallet: wallet}]; !ok {
			unknown = append(unknown, wallet)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func (r *UserPositionRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.exists)
}

// UserPositionAccountRepo loads the durable existence index on startup.
type UserPositionAccountRepo struct {
	pool *pgxpool.Pool
}

func NewUserPositionAccountRepo(pool *pgxpool.Pool) *UserPositionAccountRepo {
	return &UserPositionAccountRepo{pool: pool}
}

func (r *UserPositionAccountRepo) LoadAll(ctx context.Context) ([]UserPositionKey, error) {
	if r.pool == nil {
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
		var (
			marketID uint64
			wallet   string
		)
		if err := rows.Scan(&marketID, &wallet); err != nil {
			return nil, fmt.Errorf("scan user_position_accounts: %w", err)
		}
		keys = append(keys, UserPositionKey{MarketID: marketID, Wallet: wallet})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user_position_accounts: %w", err)
	}
	return keys, nil
}

func LoadRegistryFromRepo(ctx context.Context, repo *UserPositionAccountRepo, registry *UserPositionRegistry) error {
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
