package txreqs

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type Request struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	MarketID  uint64    `json:"market_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`
}

type Store struct {
	mu       sync.Mutex
	requests []Request
}

func NewStore() *Store {
	return &Store{}
}

func (s *Store) Create(kind string, marketID uint64) Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := Request{
		ID:        uuid.NewString(),
		Kind:      kind,
		MarketID:  marketID,
		CreatedAt: time.Now().UTC(),
		Status:    "created",
	}
	s.requests = append(s.requests, request)
	return request
}
