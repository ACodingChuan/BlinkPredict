package faucet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

var ErrFaucetNotConfigured = errors.New("faucet not configured")

type RateLimitError struct {
	NextAllowedAt time.Time
	Message       string
}

func (e RateLimitError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "rate limited"
}

type Result struct {
	Signature string    `json:"signature"`
	Mint      string    `json:"mint"`
	ATA       string    `json:"ata"`
	Amount    uint64    `json:"amount"`
	ClaimedAt time.Time `json:"claimed_at"`
}

type Service interface {
	Claim(ctx context.Context, solanaAddress string, ip string) (Result, error)
}

func ClientIP(r *http.Request) string {
	// Prefer proxy headers if present.
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	// Fallback: RemoteAddr host:port
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func WrapRateLimit(err error, next time.Time) error {
	return RateLimitError{NextAllowedAt: next, Message: fmt.Sprintf("faucet available after %s", next.Format(time.RFC3339))}
}

