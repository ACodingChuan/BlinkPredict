package settlement

import (
	"bytes"
	"encoding/hex"
	"testing"

	solana "github.com/gagliardetto/solana-go"
)

func TestOrderIntentSerializeRoundTrip(t *testing.T) {
	intent := OrderIntentV1{
		Version:     1,
		ProgramID:   solana.NewWallet().PublicKey(),
		Market:      solana.NewWallet().PublicKey(),
		User:        solana.NewWallet().PublicKey(),
		Side:        SideBuy,
		Outcome:     OutcomeNo,
		OrderType:   OrderTypeLimit,
		LimitPrice:  60,
		TotalAmount: 1234,
		Nonce:       5678,
		ExpiryTs:    999,
	}
	encoded := intent.Serialize()
	if len(encoded) != OrderIntentSize {
		t.Fatalf("unexpected encoded size: %d", len(encoded))
	}
	decoded, err := ParseOrderIntentV1(encoded)
	if err != nil {
		t.Fatalf("ParseOrderIntentV1 error: %v", err)
	}
	if decoded != intent {
		t.Fatalf("decoded mismatch: %#v != %#v", decoded, intent)
	}
}

func TestOrderIntentSerializeMatchesContractFieldLayout(t *testing.T) {
	intent := OrderIntentV1{
		Version:     1,
		ProgramID:   solana.MustPublicKeyFromBase58("11111111111111111111111111111112"),
		Market:      solana.MustPublicKeyFromBase58("11111111111111111111111111111113"),
		User:        solana.MustPublicKeyFromBase58("11111111111111111111111111111114"),
		Nonce:       0x0807060504030201,
		Side:        SideSell,
		Outcome:     OutcomeNo,
		OrderType:   OrderTypeMarket,
		LimitPrice:  0x1817161514131211,
		TotalAmount: 0x2827262524232221,
		ExpiryTs:    0x3837363534333231,
	}

	encoded := intent.Serialize()
	if len(encoded) != OrderIntentSize {
		t.Fatalf("unexpected encoded size: %d", len(encoded))
	}

	if encoded[0] != intent.Version {
		t.Fatalf("version offset mismatch: got %d want %d", encoded[0], intent.Version)
	}
	if !bytes.Equal(encoded[1:33], intent.ProgramID.Bytes()) {
		t.Fatalf("program id offset mismatch")
	}
	if !bytes.Equal(encoded[33:65], intent.Market.Bytes()) {
		t.Fatalf("market offset mismatch")
	}
	if !bytes.Equal(encoded[65:97], intent.User.Bytes()) {
		t.Fatalf("user offset mismatch")
	}
	if got := leu64(encoded[97:105]); got != intent.Nonce {
		t.Fatalf("nonce offset mismatch: got %d want %d", got, intent.Nonce)
	}
	if got := Side(encoded[105]); got != intent.Side {
		t.Fatalf("side offset mismatch: got %d want %d", got, intent.Side)
	}
	if got := Outcome(encoded[106]); got != intent.Outcome {
		t.Fatalf("outcome offset mismatch: got %d want %d", got, intent.Outcome)
	}
	if got := OrderType(encoded[107]); got != intent.OrderType {
		t.Fatalf("order type offset mismatch: got %d want %d", got, intent.OrderType)
	}
	if got := leu64(encoded[108:116]); got != intent.LimitPrice {
		t.Fatalf("limit price offset mismatch: got %d want %d", got, intent.LimitPrice)
	}
	if got := leu64(encoded[116:124]); got != intent.TotalAmount {
		t.Fatalf("total amount offset mismatch: got %d want %d", got, intent.TotalAmount)
	}
	if got := int64(leu64(encoded[124:132])); got != intent.ExpiryTs {
		t.Fatalf("expiry offset mismatch: got %d want %d", got, intent.ExpiryTs)
	}

	decoded, err := ParseOrderIntentV1(encoded)
	if err != nil {
		t.Fatalf("ParseOrderIntentV1 error: %v", err)
	}
	if decoded != intent {
		t.Fatalf("decoded mismatch after explicit layout check: %#v != %#v", decoded, intent)
	}
}

func TestOrderIntentMessageIsLowerHexKeccak(t *testing.T) {
	intent := OrderIntentV1{
		Version:     1,
		ProgramID:   solana.PublicKey{},
		Market:      solana.PublicKey{},
		User:        solana.PublicKey{},
		Nonce:       1,
		Side:        SideBuy,
		Outcome:     OutcomeYes,
		OrderType:   OrderTypeLimit,
		LimitPrice:  99,
		TotalAmount: 100,
		ExpiryTs:    2,
	}
	msg := intent.SignableMessage()
	if _, err := hex.DecodeString(string(msg)); err != nil {
		t.Fatalf("signable message is not lower hex: %v", err)
	}
	if len(msg) != 64 {
		t.Fatalf("unexpected signable message length: %d", len(msg))
	}
}
