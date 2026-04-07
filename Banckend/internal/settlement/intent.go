package settlement

import (
	"encoding/base64"
	"fmt"

	solana "github.com/gagliardetto/solana-go"
	"golang.org/x/crypto/sha3"
)

const (
	OrderIntentDomain = "predix-order"
	OrderIntentSize   = 118
	orderFlagsMask    = (1 << 0) | (1 << 1) | (1 << 2)
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
	ProgramID   solana.PublicKey
	Market      solana.PublicKey
	User        solana.PublicKey
	Nonce       uint64
	Side        Side
	Outcome     Outcome
	OrderType   OrderType
	LimitPrice  uint8
	TotalAmount uint64
	ExpiryTs    uint32
}

func (o OrderIntentV1) Serialize() []byte {
	buf := make([]byte, 0, OrderIntentSize)
	buf = append(buf, o.ProgramID.Bytes()...)
	buf = append(buf, o.Market.Bytes()...)
	buf = append(buf, o.User.Bytes()...)
	buf = append(buf, u64le(o.Nonce)...)
	buf = append(buf, o.Flags())
	buf = append(buf, o.LimitPrice)
	buf = append(buf, u64le(o.TotalAmount)...)
	buf = append(buf, u32le(o.ExpiryTs)...)
	return buf
}

func ParseOrderIntentV1(data []byte) (OrderIntentV1, error) {
	if len(data) != OrderIntentSize {
		return OrderIntentV1{}, fmt.Errorf("invalid order intent size: got %d want %d", len(data), OrderIntentSize)
	}
	var intent OrderIntentV1
	offset := 0
	intent.ProgramID = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.Market = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.User = solana.PublicKeyFromBytes(data[offset : offset+32])
	offset += 32
	intent.Nonce = leu64(data[offset : offset+8])
	offset += 8
	flags := data[offset]
	if flags&^orderFlagsMask != 0 {
		return OrderIntentV1{}, fmt.Errorf("invalid intent flags: %d", flags)
	}
	offset++
	intent.Side = sideFromFlags(flags)
	intent.Outcome = outcomeFromFlags(flags)
	intent.OrderType = orderTypeFromFlags(flags)
	intent.LimitPrice = data[offset]
	offset++
	intent.TotalAmount = leu64(data[offset : offset+8])
	offset += 8
	intent.ExpiryTs = leu32(data[offset : offset+4])
	return intent, nil
}

func (o OrderIntentV1) Hash() []byte {
	h := sha3.NewLegacyKeccak256()
	_, _ = h.Write([]byte(OrderIntentDomain))
	_, _ = h.Write(o.Serialize())
	return h.Sum(nil)
}

func (o OrderIntentV1) SignableMessage() []byte {
	return []byte("bp1:" + base64.RawURLEncoding.EncodeToString(o.Hash()))
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

func u32le(v uint32) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
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

func leu32(b []byte) uint32 {
	return uint32(b[0]) |
		uint32(b[1])<<8 |
		uint32(b[2])<<16 |
		uint32(b[3])<<24
}

func (o OrderIntentV1) Flags() uint8 {
	var flags uint8
	if o.Side == SideSell {
		flags |= 1 << 0
	}
	if o.Outcome == OutcomeNo {
		flags |= 1 << 1
	}
	if o.OrderType == OrderTypeMarket {
		flags |= 1 << 2
	}
	return flags
}

func sideFromFlags(flags uint8) Side {
	if flags&(1<<0) != 0 {
		return SideSell
	}
	return SideBuy
}

func outcomeFromFlags(flags uint8) Outcome {
	if flags&(1<<1) != 0 {
		return OutcomeNo
	}
	return OutcomeYes
}

func orderTypeFromFlags(flags uint8) OrderType {
	if flags&(1<<2) != 0 {
		return OrderTypeMarket
	}
	return OrderTypeLimit
}
