package funds

import (
	"sort"
	"sync"
)

type PendingReserve struct {
	SubmitKey       string
	WalletAddress   string
	MarketPDA       string
	LockedUSDC      uint64
	LockedYesShares uint64
	LockedNoShares  uint64
}

type PendingReserveStore struct {
	mu          sync.RWMutex
	bySubmit    map[string]PendingReserve
	byWalletPos map[string]PendingReserve
}

func NewPendingReserveStore() *PendingReserveStore {
	return &PendingReserveStore{
		bySubmit:    make(map[string]PendingReserve),
		byWalletPos: make(map[string]PendingReserve),
	}
}

func (s *PendingReserveStore) Has(submitKey string) bool {
	s.mu.RLock()
	_, ok := s.bySubmit[submitKey]
	s.mu.RUnlock()
	return ok
}

func (s *PendingReserveStore) Mark(p PendingReserve) {
	if p.SubmitKey == "" || p.WalletAddress == "" {
		return
	}
	key := positionKey(p.WalletAddress, p.MarketPDA)
	s.mu.Lock()
	s.bySubmit[p.SubmitKey] = p
	agg := s.byWalletPos[key]
	agg.WalletAddress = p.WalletAddress
	agg.MarketPDA = p.MarketPDA
	agg.LockedUSDC += p.LockedUSDC
	agg.LockedYesShares += p.LockedYesShares
	agg.LockedNoShares += p.LockedNoShares
	s.byWalletPos[key] = agg
	s.mu.Unlock()
}

func (s *PendingReserveStore) Delete(submitKey string) {
	s.mu.Lock()
	p, ok := s.bySubmit[submitKey]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.bySubmit, submitKey)
	key := positionKey(p.WalletAddress, p.MarketPDA)
	agg := s.byWalletPos[key]
	if agg.LockedUSDC <= p.LockedUSDC {
		agg.LockedUSDC = 0
	} else {
		agg.LockedUSDC -= p.LockedUSDC
	}
	if agg.LockedYesShares <= p.LockedYesShares {
		agg.LockedYesShares = 0
	} else {
		agg.LockedYesShares -= p.LockedYesShares
	}
	if agg.LockedNoShares <= p.LockedNoShares {
		agg.LockedNoShares = 0
	} else {
		agg.LockedNoShares -= p.LockedNoShares
	}
	if agg.LockedUSDC == 0 && agg.LockedYesShares == 0 && agg.LockedNoShares == 0 {
		delete(s.byWalletPos, key)
	} else {
		s.byWalletPos[key] = agg
	}
	s.mu.Unlock()
}

func (s *PendingReserveStore) Apply(walletAddress, marketPDA string, ledger UserWallet, position MarketPosition) (UserWallet, MarketPosition) {
	key := positionKey(walletAddress, marketPDA)
	s.mu.RLock()
	agg, ok := s.byWalletPos[key]
	s.mu.RUnlock()
	if !ok {
		return ledger, position
	}
	if ledger.AvailableUSDC > agg.LockedUSDC {
		ledger.AvailableUSDC -= agg.LockedUSDC
	} else {
		ledger.AvailableUSDC = 0
	}
	if position.AvailableYesShares > agg.LockedYesShares {
		position.AvailableYesShares -= agg.LockedYesShares
	} else {
		position.AvailableYesShares = 0
	}
	if position.AvailableNoShares > agg.LockedNoShares {
		position.AvailableNoShares -= agg.LockedNoShares
	} else {
		position.AvailableNoShares = 0
	}
	return ledger, position
}

func (s *PendingReserveStore) Snapshot() []PendingReserve {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]PendingReserve, 0, len(s.bySubmit))
	for _, pending := range s.bySubmit {
		out = append(out, pending)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SubmitKey < out[j].SubmitKey
	})
	return out
}

func (s *PendingReserveStore) Restore(items []PendingReserve) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.bySubmit = make(map[string]PendingReserve, len(items))
	s.byWalletPos = make(map[string]PendingReserve, len(items))
	s.mu.Unlock()

	for _, item := range items {
		s.Mark(item)
	}
}
