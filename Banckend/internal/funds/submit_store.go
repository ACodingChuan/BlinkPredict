package funds

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"blinkpredict/banckend/internal/protocol"
)

type SubmitStore struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

func NewSubmitStore() *SubmitStore {
	return &SubmitStore{keys: make(map[string]struct{})}
}

func (s *SubmitStore) Has(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	s.mu.RLock()
	_, ok := s.keys[key]
	s.mu.RUnlock()
	return ok
}

func (s *SubmitStore) Mark(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	s.mu.Lock()
	s.keys[key] = struct{}{}
	s.mu.Unlock()
}

func (s *SubmitStore) Snapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.keys))
	for key := range s.keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (s *SubmitStore) Restore(keys []string) {
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

func submitKeyFromCommand(cmd protocol.PlaceOrderCommand) string {
	if key := strings.TrimSpace(cmd.CommandID); key != "" {
		return "cmd:" + key
	}
	if key := strings.TrimSpace(cmd.IdempotencyKey); key != "" {
		return "idem:" + key
	}
	if cmd.Execution.OrderID != 0 {
		return "order:" + strconv.FormatUint(cmd.Execution.OrderID, 10)
	}
	if key := strings.TrimSpace(cmd.TraceID); key != "" {
		return "trace:" + key
	}
	return ""
}

func submitKeyFromReserveRejected(event protocol.OrderReserveRejectedEvent) string {
	if key := strings.TrimSpace(event.CommandID); key != "" {
		return "cmd:" + key
	}
	if key := strings.TrimSpace(event.IdempotencyKey); key != "" {
		return "idem:" + key
	}
	if event.OrderID != 0 {
		return "order:" + strconv.FormatUint(event.OrderID, 10)
	}
	if key := strings.TrimSpace(event.TraceID); key != "" {
		return "trace:" + key
	}
	return ""
}
