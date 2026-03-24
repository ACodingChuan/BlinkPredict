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
	Type           string `json:"type"`
	ChainType      string `json:"chain_type"`
	ChainTypeCamel string `json:"chainType"`
	Address        string `json:"address"`
}

type claims struct {
	Subject        string          `json:"sub"`
	Email          string          `json:"email"`
	Name           string          `json:"name"`
	LinkedAccounts []linkedAccount `json:"linked_accounts"`
	Wallets        []linkedAccount `json:"wallets"`
	Wallet         linkedAccount   `json:"wallet"`
	SolanaAddress  string          `json:"solana_address"`
	WalletAddress  string          `json:"wallet_address"`
}

func Middleware(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := tokenFromRequest(r)
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

func tokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("privy-id-token")); token != "" {
		return token
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authHeader) > len("Bearer ") && strings.EqualFold(authHeader[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(authHeader[len("Bearer "):])
	}
	return ""
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
		SolanaAddress: findSolanaAddress(parsed),
	}
	_, emailAdmin := cfg.AdminEmails[strings.ToLower(user.Email)]
	_, walletAdmin := cfg.AdminWallets[strings.ToLower(user.SolanaAddress)]
	user.IsAdmin = emailAdmin || walletAdmin
	return user, nil
}

func (a linkedAccount) normalizedChainType() string {
	if a.ChainType != "" {
		return a.ChainType
	}
	return a.ChainTypeCamel
}

func findSolanaAddress(payload claims) string {
	findFromAccounts := func(accounts []linkedAccount) string {
		for _, account := range accounts {
			if account.Address == "" {
				continue
			}
			chain := account.normalizedChainType()
			// Some Privy token variants omit chain_type on single-wallet payloads.
			if chain == "" || strings.EqualFold(chain, "solana") {
				return account.Address
			}
		}
		return ""
	}

	if address := findFromAccounts(payload.LinkedAccounts); address != "" {
		return address
	}
	if address := findFromAccounts(payload.Wallets); address != "" {
		return address
	}
	if payload.Wallet.Address != "" {
		chain := payload.Wallet.normalizedChainType()
		if chain == "" || strings.EqualFold(chain, "solana") {
			return payload.Wallet.Address
		}
	}
	if payload.SolanaAddress != "" {
		return payload.SolanaAddress
	}
	if payload.WalletAddress != "" {
		return payload.WalletAddress
	}
	return ""
}

func FromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey).(User)
	return user, ok
}
