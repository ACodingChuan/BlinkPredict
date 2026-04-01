package webhooks

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	blinksolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
)

type matchBranch uint8

const (
	matchBranchMatchAndMint matchBranch = iota
	matchBranchTransfer
	matchBranchMergeAndBurn
)

type UserPositionInitializedEvent struct {
	Owner  string
	Market string
	Payer  string
}

type MatchSettledEvent struct {
	Market     string
	Branch     matchBranch
	Maker      string
	Taker      string
	FillAmount uint64
	FillPrice  uint64
}

type SettlementBatchAcceptedEvent struct {
	Market     string
	OrderCount uint16
	FillCount  uint16
}

type OrderStateClosedEvent struct {
	Owner  string
	Market string
	Nonce  uint64
}

type UserPositionClosedEvent struct {
	Owner  string
	Market string
}

func classifyAlchemyProgramEvent(programData string, programID string) (*alchemyClassifiedEvent, error) {
	raw, err := decodeProgramData(programData)
	if err != nil {
		return nil, err
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("event payload too short")
	}

	switch {
	case hasAnchorEventDiscriminator(raw, "MarketCreated"):
		event, err := decodeMarketCreatedRaw(raw)
		if err != nil {
			return nil, err
		}
		metadataURL := event.MetadataURI
		if metadataURL == "" && strings.TrimSpace(event.MetadataCID) != "" {
			metadataURL = "ipfs://" + strings.TrimSpace(event.MetadataCID)
		}
		return &alchemyClassifiedEvent{
			Subject:   SubjectAlchemyMarketCreated,
			EventType: EventTypeMarketCreated,
			Payload: MarketCreatedPayload{
				MarketID:            event.MarketID,
				MarketPDA:           event.MarketPDA,
				Creator:             event.Authority,
				MetadataCID:         event.MetadataCID,
				MetadataURL:         metadataURL,
				ResolutionMode:      normalizeResolutionMode(event.ResolutionMode),
				ResolutionAuthority: event.Authority,
				OracleFeed:          event.OracleFeedID,
				OracleCondition:     normalizeOracleCondition(event.OracleCondition),
				OracleTargetPrice:   event.OracleTargetPriceInt,
				OracleTargetExpo:    event.OracleTargetExpo,
				CloseTS:             event.CloseTime,
				ResolveAfterTS:      event.ResolveAfterTS,
				ClaimDeadlineTS:     event.ClaimDeadlineTS,
			},
		}, nil
	case hasAnchorEventDiscriminator(raw, "UserPositionInitialized"):
		event, err := decodeUserPositionInitializedRaw(raw)
		if err != nil {
			return nil, err
		}
		positionPDA, err := deriveUserPositionPDA(programID, event.Owner, event.Market)
		if err != nil {
			return nil, err
		}
		return &alchemyClassifiedEvent{
			Subject:   SubjectAlchemyUserPositionInitialized,
			EventType: EventTypeUserPositionInitialized,
			Payload: UserPositionInitializedPayload{
				WalletAddress:   event.Owner,
				MarketPDA:       event.Market,
				UserPositionPDA: positionPDA,
				Payer:           event.Payer,
			},
		}, nil
	case hasAnchorEventDiscriminator(raw, "MatchSettled"):
		event, err := decodeMatchSettledRaw(raw)
		if err != nil {
			return nil, err
		}
		return &alchemyClassifiedEvent{
			Subject:   SubjectAlchemyMatchSettled,
			EventType: EventTypeMatchSettled,
			Payload: MatchSettledPayload{
				MarketPDA:   event.Market,
				Branch:      normalizeMatchBranch(event.Branch),
				MakerWallet: event.Maker,
				TakerWallet: event.Taker,
				FillAmount:  event.FillAmount,
				FillPrice:   event.FillPrice,
			},
		}, nil
	case hasAnchorEventDiscriminator(raw, "OrderStateClosed"):
		event, err := decodeOrderStateClosedRaw(raw)
		if err != nil {
			return nil, err
		}
		return &alchemyClassifiedEvent{
			Subject:   SubjectAlchemyOrderStateClosed,
			EventType: EventTypeOrderStateClosed,
			Payload: OrderStateClosedPayload{
				WalletAddress: event.Owner,
				MarketPDA:     event.Market,
				Nonce:         event.Nonce,
			},
		}, nil
	case hasAnchorEventDiscriminator(raw, "UserPositionClosed"):
		event, err := decodeUserPositionClosedRaw(raw)
		if err != nil {
			return nil, err
		}
		return &alchemyClassifiedEvent{
			Subject:   SubjectAlchemyUserPositionClosed,
			EventType: EventTypeUserPositionClosed,
			Payload: UserPositionClosedPayload{
				WalletAddress: event.Owner,
				MarketPDA:     event.Market,
			},
		}, nil
	case hasAnchorEventDiscriminator(raw, "SettlementBatchAccepted"):
		_, err := decodeSettlementBatchAcceptedRaw(raw)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("settlement batch accepted is intentionally not handled yet")
	default:
		return nil, fmt.Errorf("unsupported anchor event")
	}
}

func anchorEventDiscriminator(name string) []byte {
	h := sha256.New()
	h.Write([]byte("event:" + name))
	return h.Sum(nil)[:8]
}

func decodeProgramData(programData string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(programData)
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed: %w", err)
	}
	if len(raw) < 8 {
		return nil, fmt.Errorf("data too short: %d bytes", len(raw))
	}
	return raw, nil
}

func hasAnchorEventDiscriminator(raw []byte, name string) bool {
	expected := anchorEventDiscriminator(name)
	for i := 0; i < 8; i++ {
		if raw[i] != expected[i] {
			return false
		}
	}
	return true
}

func decodeMarketCreatedRaw(raw []byte) (*MarketCreatedEvent, error) {
	data := raw[8:]
	event := &MarketCreatedEvent{}
	offset := 0

	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for market_id")
	}
	event.MarketID = readU64LE(data[offset:])
	offset += 8

	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for market_pda")
	}
	event.MarketPDA = base58Encode(data[offset : offset+32])
	offset += 32

	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for authority")
	}
	event.Authority = base58Encode(data[offset : offset+32])
	offset += 32

	if offset+96 > len(data) {
		return nil, fmt.Errorf("data too short for metadata_cid")
	}
	rawCID := strings.ReplaceAll(string(data[offset:offset+96]), "\x00", "")
	event.MetadataCID = strings.TrimRight(rawCID, " ")
	offset += 96

	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for close_ts")
	}
	event.CloseTime = int64(readU64LE(data[offset:]))
	offset += 8

	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for resolve_after_ts")
	}
	event.ResolveAfterTS = int64(readU64LE(data[offset:]))
	offset += 8

	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for claim_deadline_ts")
	}
	event.ClaimDeadlineTS = int64(readU64LE(data[offset:]))
	offset += 8

	if offset+1 > len(data) {
		return nil, fmt.Errorf("data too short for resolution_mode")
	}
	if data[offset] == 0 {
		event.ResolutionMode = "Creator"
	} else {
		event.ResolutionMode = "Pyth"
	}
	offset++

	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_feed_id")
	}
	event.OracleFeedID = hex.EncodeToString(data[offset : offset+32])
	offset += 32

	if offset+1 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_condition")
	}
	conditions := []string{"GreaterThan", "GreaterThanOrEqual", "LessThan", "LessThanOrEqual"}
	if int(data[offset]) < len(conditions) {
		event.OracleCondition = conditions[data[offset]]
	}
	offset++

	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_target_price_int")
	}
	event.OracleTargetPriceInt = readU64LE(data[offset:])
	offset += 8

	if offset+4 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_target_expo")
	}
	event.OracleTargetExpo = int32(readU32LE(data[offset:]))

	return event, nil
}

func decodeMarketCreatedEvent(programData string) (*MarketCreatedEvent, error) {
	raw, err := decodeProgramData(programData)
	if err != nil {
		return nil, err
	}
	if !hasAnchorEventDiscriminator(raw, "MarketCreated") {
		return nil, fmt.Errorf("discriminator mismatch")
	}
	return decodeMarketCreatedRaw(raw)
}

func decodeUserPositionInitializedRaw(raw []byte) (*UserPositionInitializedEvent, error) {
	data := raw[8:]
	if len(data) < 96 {
		return nil, fmt.Errorf("data too short for user position initialized")
	}
	return &UserPositionInitializedEvent{
		Owner:  base58Encode(data[0:32]),
		Market: base58Encode(data[32:64]),
		Payer:  base58Encode(data[64:96]),
	}, nil
}

func decodeMatchSettledRaw(raw []byte) (*MatchSettledEvent, error) {
	data := raw[8:]
	if len(data) < 105 {
		return nil, fmt.Errorf("data too short for match settled")
	}
	offset := 0
	event := &MatchSettledEvent{}
	event.Market = base58Encode(data[offset : offset+32])
	offset += 32
	event.Branch = matchBranch(data[offset])
	offset++
	event.Maker = base58Encode(data[offset : offset+32])
	offset += 32
	event.Taker = base58Encode(data[offset : offset+32])
	offset += 32
	event.FillAmount = readU64LE(data[offset:])
	offset += 8
	event.FillPrice = readU64LE(data[offset:])
	return event, nil
}

func decodeSettlementBatchAcceptedRaw(raw []byte) (*SettlementBatchAcceptedEvent, error) {
	data := raw[8:]
	if len(data) < 36 {
		return nil, fmt.Errorf("data too short for settlement batch accepted")
	}
	return &SettlementBatchAcceptedEvent{
		Market:     base58Encode(data[0:32]),
		OrderCount: readU16LE(data[32:]),
		FillCount:  readU16LE(data[34:]),
	}, nil
}

func decodeOrderStateClosedRaw(raw []byte) (*OrderStateClosedEvent, error) {
	data := raw[8:]
	if len(data) < 72 {
		return nil, fmt.Errorf("data too short for order state closed")
	}
	return &OrderStateClosedEvent{
		Owner:  base58Encode(data[0:32]),
		Market: base58Encode(data[32:64]),
		Nonce:  readU64LE(data[64:]),
	}, nil
}

func decodeUserPositionClosedRaw(raw []byte) (*UserPositionClosedEvent, error) {
	data := raw[8:]
	if len(data) < 64 {
		return nil, fmt.Errorf("data too short for user position closed")
	}
	return &UserPositionClosedEvent{
		Owner:  base58Encode(data[0:32]),
		Market: base58Encode(data[32:64]),
	}, nil
}

func normalizeResolutionMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "creator":
		return "creator"
	case "pyth":
		return "pyth"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func normalizeOracleCondition(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "greaterthan":
		return "gt"
	case "greaterthanorequal":
		return "gte"
	case "lessthan":
		return "lt"
	case "lessthanorequal":
		return "lte"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func normalizeMatchBranch(branch matchBranch) string {
	switch branch {
	case matchBranchMatchAndMint:
		return "match_mint"
	case matchBranchTransfer:
		return "transfer"
	case matchBranchMergeAndBurn:
		return "merge_burn"
	default:
		return fmt.Sprintf("unknown_%d", branch)
	}
}

func deriveUserPositionPDA(programID string, owner string, market string) (string, error) {
	programKey, err := solana.PublicKeyFromBase58(strings.TrimSpace(programID))
	if err != nil {
		return "", fmt.Errorf("parse program id: %w", err)
	}
	ownerKey, err := solana.PublicKeyFromBase58(strings.TrimSpace(owner))
	if err != nil {
		return "", fmt.Errorf("parse owner: %w", err)
	}
	marketKey, err := solana.PublicKeyFromBase58(strings.TrimSpace(market))
	if err != nil {
		return "", fmt.Errorf("parse market: %w", err)
	}
	position, err := blinksolana.DeriveUserPositionPDA(programKey, ownerKey, marketKey)
	if err != nil {
		return "", err
	}
	return position.String(), nil
}
