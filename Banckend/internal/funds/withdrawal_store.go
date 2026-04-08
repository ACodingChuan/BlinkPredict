package funds

import (
	"sort"
	"strings"
	"sync"
)

type WithdrawalStore struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

func NewWithdrawalStore() *WithdrawalStore {
	return &WithdrawalStore{keys: make(map[string]struct{})}
}

func (s *WithdrawalStore) Has(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.keys[key]
	s.mu.RUnlock()
	return ok
}

func (s *WithdrawalStore) Mark(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	s.mu.Lock()
	s.keys[key] = struct{}{}
	s.mu.Unlock()
}

func (s *WithdrawalStore) Snapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.keys))
	for key := range s.keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (s *WithdrawalStore) Restore(keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		s.keys[key] = struct{}{}
	}
}
