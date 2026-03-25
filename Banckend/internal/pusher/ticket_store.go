package pusher

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const defaultTicketTTL = 45 * time.Second

type TicketStore struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewTicketStore(rdb *redis.Client, ttl time.Duration) *TicketStore {
	if ttl <= 0 {
		ttl = defaultTicketTTL
	}
	return &TicketStore{rdb: rdb, ttl: ttl}
}

func (s *TicketStore) Enabled() bool {
	return s != nil && s.rdb != nil
}

func (s *TicketStore) Issue(ctx context.Context, walletAddress string) (string, time.Time, error) {
	if !s.Enabled() {
		return "", time.Time{}, errors.New("websocket ticket store is not configured")
	}
	walletAddress = strings.TrimSpace(walletAddress)
	if walletAddress == "" {
		return "", time.Time{}, errors.New("wallet address is required")
	}

	ticket := uuid.NewString()
	expiresAt := time.Now().Add(s.ttl).UTC()
	if err := s.rdb.Set(ctx, ticketKey(ticket), walletAddress, s.ttl).Err(); err != nil {
		return "", time.Time{}, err
	}
	return ticket, expiresAt, nil
}

func (s *TicketStore) Consume(ctx context.Context, ticket string) (string, error) {
	if !s.Enabled() {
		return "", errors.New("websocket ticket store is not configured")
	}
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", errors.New("missing websocket ticket")
	}

	wallet, err := s.rdb.GetDel(ctx, ticketKey(ticket)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", errors.New("invalid or expired websocket ticket")
		}
		return "", err
	}
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return "", errors.New("invalid websocket ticket payload")
	}
	return wallet, nil
}

func ticketKey(ticket string) string {
	return "ws:ticket:" + ticket
}
