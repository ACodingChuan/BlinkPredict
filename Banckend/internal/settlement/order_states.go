package settlement

import (
	"strings"
	"sync"
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
	if r == nil || marketID == 0 || wallet == "" || nonce == 0 {
		return false
	}
	r.mu.RLock()
	_, ok := r.exists[OrderStateKey{MarketID: marketID, Wallet: wallet, Nonce: nonce}]
	r.mu.RUnlock()
	return ok
}

func (r *OrderStateRegistry) MarkExists(marketID uint64, wallet string, nonce uint64) {
	if r == nil || marketID == 0 || wallet == "" || nonce == 0 {
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
