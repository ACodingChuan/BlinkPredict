package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"blinkpredict/banckend/internal/config"
)

type contextKey string

const userContextKey contextKey = "auth_user"

type User struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	SolanaAddress string `json:"solana_address"`
	IsAdmin       bool   `json:"is_admin"`
}

type linkedAccount struct {
	Type      string `json:"type"`
	ChainType string `json:"chain_type"`
	Address   string `json:"address"`
}

type claims struct {
	Subject        string          `json:"sub"`
	Email          string          `json:"email"`
	Name           string          `json:"name"`
	LinkedAccounts []linkedAccount `json:"linked_accounts"`
}

func Middleware(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := strings.TrimSpace(r.Header.Get("privy-id-token"))
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}

			user, err := ParseToken(token, cfg)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := FromContext(r.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if !user.IsAdmin {
			http.Error(w, "admin access required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ParseToken(raw string, cfg config.Config) (User, error) {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return User{}, errors.New("invalid privy token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return User{}, errors.New("invalid privy token payload")
	}
	var parsed claims
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return User{}, errors.New("invalid privy claims")
	}
	user := User{
		Subject:       parsed.Subject,
		Email:         parsed.Email,
		Name:          parsed.Name,
		SolanaAddress: findSolanaAddress(parsed.LinkedAccounts),
	}
	_, emailAdmin := cfg.AdminEmails[strings.ToLower(user.Email)]
	_, walletAdmin := cfg.AdminWallets[strings.ToLower(user.SolanaAddress)]
	user.IsAdmin = emailAdmin || walletAdmin
	return user, nil
}

func findSolanaAddress(accounts []linkedAccount) string {
	for _, account := range accounts {
		if strings.EqualFold(account.ChainType, "solana") && account.Address != "" {
			return account.Address
		}
	}
	return ""
}

func FromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey).(User)
	return user, ok
}
