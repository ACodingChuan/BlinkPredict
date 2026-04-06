package settlement

import (
	"encoding/hex"
	"fmt"

	solana "github.com/gagliardetto/solana-go"
	"golang.org/x/crypto/sha3"
)

const (
	OrderIntentVersion = 1
	OrderIntentSize    = 132
)

type Side uint8

type Outcome uint8

type OrderType uint8

const (
	SideBuy Side = iota
	SideSell
)

const (
	OutcomeYes Outcome = iota
	OutcomeNo
)

const (
	OrderTypeLimit OrderType = iota
	OrderTypeMarket
)

type OrderIntentV1 struct {
	Version     uint8
	ProgramID   solana.PublicKey
	Market      solana.PublicKey
	User        solana.PublicKey
	Nonce       uint64
	Side        Side
	Outcome     Outcome
	OrderType   OrderType
	LimitPrice  uint64
	TotalAmount uint64
	ExpiryTs    int64
}

func (o OrderIntentV1) Serialize() []byte {
	buf := make([]byte, 0, OrderIntentSize)
	buf = append(buf, o.Version)
	buf = append(buf, o.ProgramID.Bytes()...)
	buf = append(buf, o.Market.Bytes()...)
	buf = append(buf, o.User.Bytes()...)
	buf = append(buf, u64le(o.Nonce)...)
	buf = append(buf, byte(o.Side))
	buf = append(buf, byte(o.Outcome))
	buf = append(buf, byte(o.OrderType))
	buf = append(buf, u64le(o.LimitPrice)...)
	buf = append(buf, u64le(o.TotalAmount)...)
	buf = append(buf, u64le(uint64(o.ExpiryTs))...)
	return buf
}

func ParseOrderIntentV1(data []byte) (OrderIntentV1, error) {
	if len(data) != OrderIntentSize {
		return OrderIntentV1{}, fmt.Errorf("invalid order intent size: got %d want %d", len(data), OrderIntentSize)
	}
	var intent OrderIntentV1
	offset := 0
	intent.Version = data[offset]
	offset++
	intent.ProgramID = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.Market = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.User = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.Nonce = leu64(data[offset : offset+8])
	offset += 8
	intent.Side = Side(data[offset])
	offset++
	intent.Outcome = Outcome(data[offset])
	offset++
	intent.OrderType = OrderType(data[offset])
	offset++
	intent.LimitPrice = leu64(data[offset : offset+8])
	offset += 8
	intent.TotalAmount = leu64(data[offset : offset+8])
	offset += 8
	intent.ExpiryTs = int64(leu64(data[offset : offset+8]))
	return intent, nil
}

func (o OrderIntentV1) Hash() []byte {
	h := sha3.NewLegacyKeccak256()
	_, _ = h.Write(o.Serialize())
	return h.Sum(nil)
}

func (o OrderIntentV1) SignableMessage() []byte {
	return []byte(hex.EncodeToString(o.Hash()))
}

func OrderIntentFromHex(raw string) (OrderIntentV1, error) {
	bytes, err := decodeHex(raw)
	if err != nil {
		return OrderIntentV1{}, fmt.Errorf("decode intent hex: %w", err)
	}
	return ParseOrderIntentV1(bytes)
}

func u64le(v uint64) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24), byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56)}
}

func leu64(b []byte) uint64 {
	return uint64(b[0]) |
		uint64(b[1])<<8 |
		uint64(b[2])<<16 |
		uint64(b[3])<<24 |
		uint64(b[4])<<32 |
		uint64(b[5])<<40 |
		uint64(b[6])<<48 |
		uint64(b[7])<<56
}
