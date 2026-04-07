package settlement

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	solana "github.com/gagliardetto/solana-go"
)

const preparedPayloadSchemaVersion = 1

type preparedSubmissionPayload struct {
	SchemaVersion int                    `json:"schema_version"`
	MarketID      uint64                 `json:"market_id"`
	MarketPDA     string                 `json:"market_pda"`
	Orders        []preparedOrderPayload `json:"orders"`
	Fills         []FillIndexPair        `json:"fills"`
	UniqueUsers   []string               `json:"unique_users"`
}

type preparedOrderPayload struct {
	IntentHex  string `json:"intent_hex"`
	Signature  string `json:"signature"`
	OrderIndex uint16 `json:"order_index"`
	Warm       bool   `json:"warm"`
}

func encodePreparedPayload(batch SubmissionBatch) ([]byte, error) {
	payload := preparedSubmissionPayload{
		SchemaVersion: preparedPayloadSchemaVersion,
		MarketID:      batch.MarketID,
		MarketPDA:     batch.MarketPDA.String(),
		Orders:        make([]preparedOrderPayload, 0, len(batch.Orders)),
		Fills:         make([]FillIndexPair, len(batch.Fills)),
		UniqueUsers:   make([]string, 0, len(batch.UniqueUsers)),
	}
	copy(payload.Fills, batch.Fills)
	for _, user := range batch.UniqueUsers {
		payload.UniqueUsers = append(payload.UniqueUsers, user.String())
	}
	for _, order := range batch.Orders {
		raw := order.RawIntent
		if len(raw) == 0 {
			raw = order.Intent.Serialize()
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("empty intent payload for order_index=%d", order.OrderIndex)
		}
		payload.Orders = append(payload.Orders, preparedOrderPayload{
			IntentHex:  hex.EncodeToString(raw),
			Signature:  base64.StdEncoding.EncodeToString(order.Signature),
			OrderIndex: order.OrderIndex,
			Warm:       order.Warm,
		})
	}
	return json.Marshal(payload)
}

func decodePreparedPayload(raw []byte, cfg BuildConfig) (SubmissionBatch, error) {
	var payload preparedSubmissionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SubmissionBatch{}, fmt.Errorf("decode prepared payload: %w", err)
	}
	if payload.SchemaVersion != preparedPayloadSchemaVersion {
		return SubmissionBatch{}, fmt.Errorf("unsupported prepared payload schema version %d", payload.SchemaVersion)
	}
	marketPDA, err := solana.PublicKeyFromBase58(strings.TrimSpace(payload.MarketPDA))
	if err != nil {
		return SubmissionBatch{}, fmt.Errorf("parse prepared market_pda: %w", err)
	}
	if len(payload.Orders) == 0 {
		return SubmissionBatch{}, fmt.Errorf("prepared payload has no orders")
	}
	if len(payload.Orders) > 255 {
		return SubmissionBatch{}, fmt.Errorf("prepared payload has too many orders: %d", len(payload.Orders))
	}
	if len(payload.UniqueUsers) == 0 {
		return SubmissionBatch{}, fmt.Errorf("prepared payload has no unique users")
	}
	if len(payload.UniqueUsers) > 255 {
		return SubmissionBatch{}, fmt.Errorf("prepared payload has too many unique users: %d", len(payload.UniqueUsers))
	}
	orders := make([]SubmissionOrder, 0, len(payload.Orders))
	referencedUsers := make(map[string]struct{}, len(payload.Orders))
	for idx, prepared := range payload.Orders {
		if prepared.OrderIndex != uint16(idx) {
			return SubmissionBatch{}, fmt.Errorf("prepared order indexes must be contiguous: got %d at position %d", prepared.OrderIndex, idx)
		}
		intentBytes, err := decodeHex(prepared.IntentHex)
		if err != nil {
			return SubmissionBatch{}, fmt.Errorf("decode prepared intent hex order_index=%d: %w", prepared.OrderIndex, err)
		}
		intent, err := ParseOrderIntentV1(intentBytes)
		if err != nil {
			return SubmissionBatch{}, fmt.Errorf("parse prepared intent order_index=%d: %w", prepared.OrderIndex, err)
		}
		if !intent.ProgramID.Equals(cfg.ProgramID) {
			return SubmissionBatch{}, fmt.Errorf("prepared intent program mismatch order_index=%d", prepared.OrderIndex)
		}
		if !intent.Market.Equals(marketPDA) {
			return SubmissionBatch{}, fmt.Errorf("prepared intent market mismatch order_index=%d", prepared.OrderIndex)
		}
		signature, err := decodeSignature(prepared.Signature)
		if err != nil {
			return SubmissionBatch{}, fmt.Errorf("decode prepared signature order_index=%d: %w", prepared.OrderIndex, err)
		}
		orders = append(orders, SubmissionOrder{
			Intent:     intent,
			Signature:  signature,
			RawIntent:  intentBytes,
			OrderIndex: prepared.OrderIndex,
			Warm:       prepared.Warm,
		})
		referencedUsers[intent.User.String()] = struct{}{}
	}
	uniqueUsers := make([]solana.PublicKey, 0, len(payload.UniqueUsers))
	uniqueUserSet := make(map[string]struct{}, len(payload.UniqueUsers))
	for _, user := range payload.UniqueUsers {
		parsed, err := solana.PublicKeyFromBase58(strings.TrimSpace(user))
		if err != nil {
			return SubmissionBatch{}, fmt.Errorf("parse prepared unique user: %w", err)
		}
		key := parsed.String()
		if _, exists := uniqueUserSet[key]; exists {
			return SubmissionBatch{}, fmt.Errorf("prepared payload has duplicate unique user %s", key)
		}
		uniqueUserSet[key] = struct{}{}
		uniqueUsers = append(uniqueUsers, parsed)
	}
	for _, order := range orders {
		if _, ok := uniqueUserSet[order.Intent.User.String()]; !ok {
			return SubmissionBatch{}, fmt.Errorf("prepared payload missing unique user for order_index=%d", order.OrderIndex)
		}
	}
	if len(uniqueUserSet) != len(referencedUsers) {
		return SubmissionBatch{}, fmt.Errorf("prepared payload has unused unique users")
	}
	if len(payload.Fills) == 0 {
		return SubmissionBatch{}, ErrNoSettlementWork
	}
	fills := make([]FillIndexPair, 0, len(payload.Fills))
	for _, fill := range payload.Fills {
		if fill.FillAmount == 0 {
			return SubmissionBatch{}, fmt.Errorf("prepared fill has zero amount")
		}
		if fill.FillPrice == 0 || fill.FillPrice >= 100 {
			return SubmissionBatch{}, fmt.Errorf("prepared fill has invalid price %d", fill.FillPrice)
		}
		if int(fill.MakerIdx) >= len(orders) || int(fill.TakerIdx) >= len(orders) {
			return SubmissionBatch{}, fmt.Errorf("prepared fill references missing order index")
		}
		if fill.MakerIdx == fill.TakerIdx {
			return SubmissionBatch{}, fmt.Errorf("prepared fill cannot reference the same maker and taker order")
		}
		fills = append(fills, fill)
	}
	return SubmissionBatch{
		MarketID:    payload.MarketID,
		MarketPDA:   marketPDA,
		Orders:      orders,
		Fills:       fills,
		UniqueUsers: uniqueUsers,
	}, nil
}

func decodeSignature(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty signature")
	}
	sig, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		if len(sig) != 64 {
			return nil, fmt.Errorf("invalid signature length: got %d want 64", len(sig))
		}
		return sig, nil
	}
	sig, rawErr := base64.RawStdEncoding.DecodeString(value)
	if rawErr != nil {
		return nil, err
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("invalid signature length: got %d want 64", len(sig))
	}
	return sig, nil
}
