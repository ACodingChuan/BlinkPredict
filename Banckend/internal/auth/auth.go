package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/config"

	gsolana "github.com/gagliardetto/solana-go"
	"github.com/sony/sonyflake"
)

type contextKey string

const userContextKey contextKey = "auth_user"

const (
	defaultTokenTTL     = 7 * 24 * time.Hour
	defaultChallengeTTL = 5 * time.Minute
	loginMessageVersion = "blinkpredict.login.v1"
)

type User struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	SolanaAddress string `json:"solana_address"`
	IsAdmin       bool   `json:"is_admin"`
}

type sessionClaims struct {
	Subject       string `json:"sub"`
	SolanaAddress string `json:"solana_address"`
	IssuedAt      int64  `json:"iat"`
	ExpiresAt     int64  `json:"exp"`
}

type Challenge struct {
	ID        string    `json:"challenge_id"`
	Message   string    `json:"message"`
	ExpiresAt time.Time `json:"expires_at"`
}

type storedChallenge struct {
	Challenge
	WalletAddress string
}

type SessionManager struct {
	cfg        config.Config
	secret     []byte
	sonyflake  *sonyflake.Sonyflake
	challenges map[string]storedChallenge
	mu         sync.Mutex
}

func NewSessionManager(cfg config.Config) (*SessionManager, error) {
	secret := strings.TrimSpace(cfg.AuthTokenSecret)
	if secret == "" {
		secret = "blinkpredict-dev-auth-secret"
	}
	sf := sonyflake.NewSonyflake(sonyflake.Settings{})
	if sf == nil {
		return nil, errors.New("failed to initialize sonyflake")
	}
	return &SessionManager{
		cfg:        cfg,
		secret:     []byte(secret),
		sonyflake:  sf,
		challenges: make(map[string]storedChallenge),
	}, nil
}

func Middleware(cfg config.Config, sessions *SessionManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := tokenFromRequest(r)
			if token == "" || sessions == nil {
				next.ServeHTTP(w, r)
				return
			}

			user, err := sessions.ParseToken(token)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}

			_, emailAdmin := cfg.AdminEmails[strings.ToLower(user.Email)]
			_, walletAdmin := cfg.AdminWallets[strings.ToLower(user.SolanaAddress)]
			user.IsAdmin = emailAdmin || walletAdmin

			ctx := context.WithValue(r.Context(), userContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func tokenFromRequest(r *http.Request) string {
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

func (s *SessionManager) CreateChallenge(walletAddress string) (Challenge, error) {
	walletAddress = strings.TrimSpace(walletAddress)
	if walletAddress == "" {
		return Challenge{}, errors.New("wallet address is required")
	}
	if _, err := gsolana.PublicKeyFromBase58(walletAddress); err != nil {
		return Challenge{}, errors.New("invalid wallet address")
	}
	id, err := s.nextIDString()
	if err != nil {
		return Challenge{}, err
	}
	issuedAt := time.Now().UTC()
	expiresAt := issuedAt.Add(defaultChallengeTTL)
	message := strings.Join([]string{
		loginMessageVersion,
		"wallet=" + walletAddress,
		"challenge_id=" + id,
		"issued_at=" + issuedAt.Format(time.RFC3339),
		"expires_at=" + expiresAt.Format(time.RFC3339),
	}, "\n")
	challenge := Challenge{ID: id, Message: message, ExpiresAt: expiresAt}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredChallengesLocked(time.Now().UTC())
	s.challenges[id] = storedChallenge{
		Challenge:     challenge,
		WalletAddress: walletAddress,
	}
	return challenge, nil
}

func (s *SessionManager) VerifyChallenge(challengeID, walletAddress, signature string) (User, string, time.Time, error) {
	challengeID = strings.TrimSpace(challengeID)
	walletAddress = strings.TrimSpace(walletAddress)
	signature = strings.TrimSpace(signature)
	if challengeID == "" || walletAddress == "" || signature == "" {
		return User{}, "", time.Time{}, errors.New("challenge_id, wallet_address and signature are required")
	}

	s.mu.Lock()
	s.cleanupExpiredChallengesLocked(time.Now().UTC())
	stored, ok := s.challenges[challengeID]
	if ok {
		delete(s.challenges, challengeID)
	}
	s.mu.Unlock()

	if !ok {
		return User{}, "", time.Time{}, errors.New("challenge not found or expired")
	}
	if stored.WalletAddress != walletAddress {
		return User{}, "", time.Time{}, errors.New("wallet address does not match challenge")
	}
	if time.Now().UTC().After(stored.ExpiresAt) {
		return User{}, "", time.Time{}, errors.New("challenge expired")
	}

	pubkey, err := gsolana.PublicKeyFromBase58(walletAddress)
	if err != nil {
		return User{}, "", time.Time{}, errors.New("invalid wallet address")
	}
	signatureBytes, err := decodeBase64Signature(signature)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	if !ed25519.Verify(ed25519.PublicKey(pubkey.Bytes()), []byte(stored.Message), signatureBytes) {
		return User{}, "", time.Time{}, errors.New("invalid login signature")
	}

	user := User{
		Subject:       walletAddress,
		SolanaAddress: walletAddress,
	}
	_, walletAdmin := s.cfg.AdminWallets[strings.ToLower(walletAddress)]
	user.IsAdmin = walletAdmin

	token, expiresAt, err := s.IssueToken(user)
	if err != nil {
		return User{}, "", time.Time{}, err
	}
	return user, token, expiresAt, nil
}

func (s *SessionManager) IssueToken(user User) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(defaultTokenTTL)
	claims := sessionClaims{
		Subject:       user.SolanaAddress,
		SolanaAddress: user.SolanaAddress,
		IssuedAt:      now.Unix(),
		ExpiresAt:     expiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payloadEncoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadEncoded + "." + signature, expiresAt, nil
}

func (s *SessionManager) ParseToken(raw string) (User, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 2 {
		return User{}, errors.New("invalid auth token")
	}
	payloadEncoded := parts[0]
	signatureEncoded := parts[1]
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payloadEncoded))
	expectedSig := mac.Sum(nil)
	gotSig, err := base64.RawURLEncoding.DecodeString(signatureEncoded)
	if err != nil {
		return User{}, errors.New("invalid auth token signature")
	}
	if !hmac.Equal(gotSig, expectedSig) {
		return User{}, errors.New("invalid auth token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadEncoded)
	if err != nil {
		return User{}, errors.New("invalid auth token payload")
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return User{}, errors.New("invalid auth token claims")
	}
	if claims.ExpiresAt <= time.Now().UTC().Unix() {
		return User{}, errors.New("auth token expired")
	}
	return User{
		Subject:       claims.Subject,
		SolanaAddress: claims.SolanaAddress,
	}, nil
}

func decodeBase64Signature(raw string) ([]byte, error) {
	signatureBytes, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		signatureBytes, err = base64.RawStdEncoding.DecodeString(raw)
		if err != nil {
			return nil, errors.New("signature must be base64 encoded")
		}
	}
	if len(signatureBytes) != ed25519.SignatureSize {
		return nil, errors.New("signature must be 64 bytes")
	}
	return signatureBytes, nil
}

func (s *SessionManager) nextIDString() (string, error) {
	id, err := s.sonyflake.NextID()
	if err != nil {
		return "", fmt.Errorf("generate snowflake id: %w", err)
	}
	return strconvFormatUint(id), nil
}

func strconvFormatUint(v uint64) string {
	return fmt.Sprintf("%d", v)
}

func (s *SessionManager) cleanupExpiredChallengesLocked(now time.Time) {
	for id, challenge := range s.challenges {
		if now.After(challenge.ExpiresAt) {
			delete(s.challenges, id)
		}
	}
}

func FromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(userContextKey).(User)
	return user, ok
}
