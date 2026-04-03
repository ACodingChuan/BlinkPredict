package webhooks

// Deprecated: market creation no longer depends on Alchemy webhook ingestion.
// This handler remains only for legacy/non-market event paths during transition.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// AlchemyHandler Alchemy webhook 处理器
type AlchemyHandler struct {
	client     *natsjs.Client
	log        *zerolog.Logger
	signingKey string
	programID  string
}

// NewAlchemyHandler 创建 Alchemy webhook 处理器
func NewAlchemyHandler(
	client *natsjs.Client,
	logger *zerolog.Logger,
	signingKey string,
	programID string,
) *AlchemyHandler {
	return &AlchemyHandler{
		client:     client,
		log:        logger,
		signingKey: signingKey,
		programID:  programID,
	}
}

// alchemyWebhookEvent Alchemy webhook payload 结构
type alchemyWebhookEvent struct {
	WebhookID string                 `json:"webhookId"`
	ID        string                 `json:"id"`
	CreatedAt time.Time              `json:"createdAt"`
	Type      string                 `json:"type"`
	Event     map[string]interface{} `json:"event"`
}

// HandleWebhook 处理 Alchemy webhook 请求
func (h *AlchemyHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.New().String()

	h.log.Info().
		Str("request_id", requestID).
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("received Alchemy webhook request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error().Err(err).Str("request_id", requestID).Msg("failed to read body")
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}

	signature := r.Header.Get("x-alchemy-signature")
	if !h.isValidSignature(body, signature) {
		h.log.Error().
			Str("request_id", requestID).
			Str("signature", signature).
			Msg("invalid Alchemy signature")
		http.Error(w, "Unauthorized", http.StatusForbidden)
		return
	}

	var event alchemyWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		h.log.Error().Err(err).Str("request_id", requestID).Msg("failed to parse payload")
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	go h.processAsync(event, requestID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "accepted",
		"request_id": requestID,
	})
}

func (h *AlchemyHandler) isValidSignature(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(h.signingKey))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return expected == signature
}

func (h *AlchemyHandler) processAsync(event alchemyWebhookEvent, requestID string) {
	if h.client == nil {
		h.log.Error().Str("request_id", requestID).Msg("nats client is not configured")
		return
	}

	txList, ok := extractTxList(event.Event)
	if !ok || len(txList) == 0 {
		h.log.Warn().Str("request_id", requestID).Msg("alchemy payload contained no transactions")
		return
	}

	for _, tx := range txList {
		sig := extractString(tx, "signature")
		if strings.TrimSpace(sig) == "" {
			continue
		}
		logMessages := extractLogMessages(tx)
		programDataEntries := findProgramDataEntries(logMessages, h.programID)
		if len(programDataEntries) == 0 {
			continue
		}
		slot := extractUint64(tx, "slot")
		blockTime := extractInt64(tx, "blockTime")
		if blockTime == 0 {
			blockTime = extractInt64(tx, "block_time")
		}

		for idx, programData := range programDataEntries {
			classified, err := classifyAlchemyProgramEvent(programData, h.programID)
			if err != nil {
				h.log.Debug().
					Err(err).
					Str("request_id", requestID).
					Str("signature", sig).
					Int("event_index", idx).
					Msg("skip unsupported Alchemy program event")
				continue
			}

			if err := h.publishClassifiedEvent(event, sig, slot, blockTime, idx, classified); err != nil {
				h.log.Error().
					Err(err).
					Str("request_id", requestID).
					Str("signature", sig).
					Str("event_type", classified.EventType).
					Msg("failed to publish Alchemy classified event")
			}
		}
	}
}

func (h *AlchemyHandler) publishClassifiedEvent(
	providerEvent alchemyWebhookEvent,
	signature string,
	slot uint64,
	blockTime int64,
	eventIndex int,
	classified *alchemyClassifiedEvent,
) error {
	payload, err := json.Marshal(classified.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	env := WebhookEventEnvelope{
		EventID:         buildAlchemyEventID(signature, classified.EventType, eventIndex),
		Provider:        ProviderAlchemy,
		ProviderEventID: firstNonEmpty(providerEvent.ID, providerEvent.WebhookID),
		Signature:       signature,
		Slot:            slot,
		BlockTime:       blockTime,
		EventType:       classified.EventType,
		SchemaVersion:   1,
		ProducedAt:      time.Now().UTC(),
		Payload:         payload,
	}
	if err := h.client.PublishJSON(context.Background(), classified.Subject, env.EventID, env); err != nil {
		return fmt.Errorf("publish %s: %w", classified.Subject, err)
	}
	return nil
}

func buildAlchemyEventID(signature string, eventType string, eventIndex int) string {
	return ProviderAlchemy + ":" + strings.TrimSpace(signature) + ":" + strings.TrimSpace(eventType) + ":" + strconv.Itoa(eventIndex)
}

func readU64LE(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func readU32LE(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func readU16LE(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}

func base58Encode(b []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	result := make([]byte, 0, 44)
	num := make([]byte, len(b))
	copy(num, b)

	leadingZeros := 0
	for _, v := range num {
		if v != 0 {
			break
		}
		leadingZeros++
	}

	for len(num) > 0 {
		remainder := 0
		newNum := make([]byte, 0, len(num))
		for _, v := range num {
			digit := remainder*256 + int(v)
			if len(newNum) > 0 || digit/58 > 0 {
				newNum = append(newNum, byte(digit/58))
			}
			remainder = digit % 58
		}
		result = append(result, alphabet[remainder])
		num = newNum
	}

	for i := 0; i < leadingZeros; i++ {
		result = append(result, alphabet[0])
	}

	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

func extractTxList(event map[string]interface{}) ([]map[string]interface{}, bool) {
	txRaw, ok := event["transaction"]
	if !ok {
		return nil, false
	}
	txArr, ok := txRaw.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]map[string]interface{}, 0, len(txArr))
	for _, item := range txArr {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result, true
}

func extractLogMessages(tx map[string]interface{}) []string {
	metaRaw, ok := tx["meta"]
	if !ok {
		return nil
	}
	metaArr, ok := metaRaw.([]interface{})
	if !ok || len(metaArr) == 0 {
		return nil
	}
	meta, ok := metaArr[0].(map[string]interface{})
	if !ok {
		return nil
	}
	logsRaw, ok := meta["log_messages"]
	if !ok {
		return nil
	}
	logsArr, ok := logsRaw.([]interface{})
	if !ok {
		return nil
	}
	logs := make([]string, 0, len(logsArr))
	for _, l := range logsArr {
		if s, ok := l.(string); ok {
			logs = append(logs, s)
		}
	}
	return logs
}

func findProgramDataEntries(logs []string, programID string) []string {
	entries := make([]string, 0, 4)
	inProgram := false
	for _, line := range logs {
		if strings.Contains(line, programID+" invoke") {
			inProgram = true
			continue
		}
		if inProgram && strings.HasPrefix(line, "Program data: ") {
			entries = append(entries, strings.TrimPrefix(line, "Program data: "))
			continue
		}
		if inProgram && strings.Contains(line, programID+" success") {
			inProgram = false
		}
	}
	return entries
}

func extractString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		switch typed := v.(type) {
		case string:
			return typed
		}
	}
	return ""
}

func extractUint64(m map[string]interface{}, key string) uint64 {
	if v, ok := m[key]; ok {
		switch typed := v.(type) {
		case float64:
			if typed < 0 {
				return 0
			}
			return uint64(typed)
		case int64:
			if typed < 0 {
				return 0
			}
			return uint64(typed)
		case json.Number:
			parsed, _ := typed.Int64()
			if parsed < 0 {
				return 0
			}
			return uint64(parsed)
		}
	}
	return 0
}

func extractInt64(m map[string]interface{}, key string) int64 {
	if v, ok := m[key]; ok {
		switch typed := v.(type) {
		case float64:
			return int64(typed)
		case int64:
			return typed
		case json.Number:
			parsed, _ := typed.Int64()
			return parsed
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
