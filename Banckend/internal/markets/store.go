package markets

import (
	"context"
	"errors"
	"sort"
	"sync"
)

var ErrMarketNotFound = errors.New("market not found")

type Repository interface {
	Save(context.Context, Market) error
	List(context.Context) ([]Market, error)
	Get(context.Context, uint64) (Market, error)
	Update(context.Context, Market) error
}

type MemoryRepository struct {
	mu      sync.RWMutex
	markets map[uint64]Market
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{markets: make(map[uint64]Market)}
}

func (r *MemoryRepository) Save(_ context.Context, market Market) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markets[market.MarketID] = market
	return nil
}

func (r *MemoryRepository) List(_ context.Context) ([]Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Market, 0, len(r.markets))
	for _, market := range r.markets {
		result = append(result, market)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result, nil
}

func (r *MemoryRepository) Get(_ context.Context, marketID uint64) (Market, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	market, ok := r.markets[marketID]
	if !ok {
		return Market{}, ErrMarketNotFound
	}
	return market, nil
}

func (r *MemoryRepository) Update(_ context.Context, market Market) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.markets[market.MarketID]; !ok {
		return ErrMarketNotFound
	}
	r.markets[market.MarketID] = market
	return nil
}
