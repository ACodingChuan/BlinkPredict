package webhooks

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

type HeliusHandler struct {
	projector          *DepositProjector
	log                *zerolog.Logger
	webhookSecret      string
	collateralMint     string
	globalVault        string
	collateralDecimals int
}

func NewHeliusHandler(
	projector *DepositProjector,
	logger *zerolog.Logger,
	webhookSecret string,
	collateralMint string,
	globalVault string,
	collateralDecimals int,
) *HeliusHandler {
	return &HeliusHandler{
		projector:          projector,
		log:                logger,
		webhookSecret:      webhookSecret,
		collateralMint:     strings.TrimSpace(collateralMint),
		globalVault:        strings.TrimSpace(globalVault),
		collateralDecimals: collateralDecimals,
	}
}

func (h *HeliusHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.isAuthorized(r.Header.Get("Authorization")) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if h.projector == nil {
		http.Error(w, "Webhook projector is not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	payloads, err := parseHeliusPayloads(body)
	if err != nil {
		if h.log != nil {
			h.log.Error().Err(err).Str("request_id", requestID).Msg("parse helius payload failed")
		}
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	processed := 0
	for _, payload := range payloads {
		events, err := h.extractDepositEvents(payload)
		if err != nil {
			if h.log != nil {
				h.log.Error().Err(err).Str("request_id", requestID).Msg("extract helius deposit events failed")
			}
			http.Error(w, "Failed to project webhook", http.StatusInternalServerError)
			return
		}
		for _, event := range events {
			if err := h.projector.ApplyDeposit(r.Context(), event); err != nil {
				if h.log != nil {
					h.log.Error().
						Err(err).
						Str("request_id", requestID).
						Str("wallet", event.WalletAddress).
						Str("signature", event.Signature).
						Msg("apply helius deposit failed")
				}
				http.Error(w, "Failed to project webhook", http.StatusInternalServerError)
				return
			}
			processed++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     "ok",
		"request_id": requestID,
		"processed":  processed,
	})
}

func (h *HeliusHandler) isAuthorized(authHeader string) bool {
	receivedToken := strings.TrimSpace(authHeader)
	if strings.HasPrefix(receivedToken, "Bearer ") {
		receivedToken = strings.TrimSpace(strings.TrimPrefix(receivedToken, "Bearer "))
	}
	return receivedToken != "" && receivedToken == h.webhookSecret
}

func parseHeliusPayloads(body []byte) ([]HeliusWebhookPayload, error) {
	var payloads []HeliusWebhookPayload
	if err := json.Unmarshal(body, &payloads); err == nil {
		return payloads, nil
	}

	var payload HeliusWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return []HeliusWebhookPayload{payload}, nil
}

func (h *HeliusHandler) extractDepositEvents(payload HeliusWebhookPayload) ([]DepositSettledPayload, error) {
	if payload.Transaction == nil {
		return nil, nil
	}
	tx := payload.Transaction
	if !heliusTransactionSucceeded(tx) {
		return nil, nil
	}

	events := make([]DepositSettledPayload, 0, len(tx.TokenTransfers))
	for _, transfer := range tx.TokenTransfers {
		if !strings.EqualFold(strings.TrimSpace(transfer.Mint), h.collateralMint) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(transfer.ToTokenAccount), h.globalVault) {
			continue
		}
		if transfer.Amount <= 0 {
			continue
		}
		walletAddress := strings.TrimSpace(transfer.FromUserAccount)
		if walletAddress == "" {
			continue
		}
		amountUnits, err := rawAmountToLedgerUnits(transfer.Amount, h.collateralDecimals)
		if err != nil {
			return nil, err
		}
		events = append(events, DepositSettledPayload{
			WalletAddress:    walletAddress,
			Mint:             strings.TrimSpace(transfer.Mint),
			AmountUnits:      amountUnits,
			Signature:        strings.TrimSpace(tx.Signature),
			Slot:             firstNonZeroUint64(payload.Slot, tx.Slot),
			BlockTime:        firstNonZero(payload.Timestamp, tx.Timestamp),
			FromTokenAccount: strings.TrimSpace(transfer.FromTokenAccount),
			ToTokenAccount:   strings.TrimSpace(transfer.ToTokenAccount),
		})
	}
	return events, nil
}

func heliusTransactionSucceeded(tx *HeliusTransaction) bool {
	if tx == nil {
		return false
	}
	if tx.Err != nil {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(tx.Status))
	return status == "" || status == "success" || status == "confirmed"
}

func rawAmountToLedgerUnits(rawAmount int64, decimals int) (uint64, error) {
	if rawAmount <= 0 {
		return 0, fmt.Errorf("raw amount must be positive")
	}
	if decimals < 2 {
		return 0, fmt.Errorf("collateral decimals must be at least 2")
	}
	divisor := uint64(1)
	for i := 0; i < decimals-2; i++ {
		divisor *= 10
	}
	raw := uint64(rawAmount)
	if divisor > 1 && raw%divisor != 0 {
		return 0, fmt.Errorf("raw amount %d is not aligned to ledger units", rawAmount)
	}
	return raw / divisor, nil
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func firstNonZeroUint64(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
