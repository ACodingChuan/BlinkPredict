package settlement

import (
	"bytes"
	"encoding/base64"
	"testing"

	solana "github.com/gagliardetto/solana-go"
)

func TestOrderIntentRoundTrip(t *testing.T) {
	intent := OrderIntentV1{
		ProgramID:   solana.NewWallet().PublicKey(),
		Market:      solana.NewWallet().PublicKey(),
		User:        solana.NewWallet().PublicKey(),
		Nonce:       5678,
		Side:        SideBuy,
		Outcome:     OutcomeNo,
		OrderType:   OrderTypeLimit,
		LimitPrice:  60,
		TotalAmount: 1234,
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

func TestOrderIntentBinaryLayout(t *testing.T) {
	intent := OrderIntentV1{
		ProgramID:   solana.MustPublicKeyFromBase58("11111111111111111111111111111112"),
		Market:      solana.MustPublicKeyFromBase58("11111111111111111111111111111113"),
		User:        solana.MustPublicKeyFromBase58("11111111111111111111111111111114"),
		Nonce:       0x0807060504030201,
		Side:        SideSell,
		Outcome:     OutcomeNo,
		OrderType:   OrderTypeMarket,
		LimitPrice:  0x42,
		TotalAmount: 0x2827262524232221,
		ExpiryTs:    0x38373635,
	}

	encoded := intent.Serialize()
	if !bytes.Equal(encoded[0:32], intent.ProgramID.Bytes()) {
		t.Fatal("program id offset mismatch")
	}
	if !bytes.Equal(encoded[32:64], intent.Market.Bytes()) {
		t.Fatal("market offset mismatch")
	}
	if !bytes.Equal(encoded[64:96], intent.User.Bytes()) {
		t.Fatal("user offset mismatch")
	}
	if got := leu64(encoded[96:104]); got != intent.Nonce {
		t.Fatalf("nonce offset mismatch: got %d want %d", got, intent.Nonce)
	}
	if got := encoded[104]; got != intent.Flags() {
		t.Fatalf("flags offset mismatch: got %d want %d", got, intent.Flags())
	}
	if got := encoded[105]; got != intent.LimitPrice {
		t.Fatalf("limit_price offset mismatch: got %d want %d", got, intent.LimitPrice)
	}
	if got := leu64(encoded[106:114]); got != intent.TotalAmount {
		t.Fatalf("total_amount offset mismatch: got %d want %d", got, intent.TotalAmount)
	}
	if got := leu32(encoded[114:118]); got != intent.ExpiryTs {
		t.Fatalf("expiry_ts offset mismatch: got %d want %d", got, intent.ExpiryTs)
	}
}

func TestOrderIntentSignableMessageUsesBP1Base64URLHash(t *testing.T) {
	intent := OrderIntentV1{
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
	if !bytes.HasPrefix(msg, []byte("bp1:")) {
		t.Fatalf("unexpected signable message prefix: %q", msg)
	}
	if len(msg) != 47 {
		t.Fatalf("unexpected signable message length: %d", len(msg))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(string(msg[4:]))
	if err != nil {
		t.Fatalf("signable message is not raw base64url: %v", err)
	}
	if !bytes.Equal(decoded, intent.Hash()) {
		t.Fatalf("signable message hash mismatch")
	}
}

func TestParseOrderIntentRejectsUnknownFlagBits(t *testing.T) {
	intent := OrderIntentV1{
		ProgramID:   solana.NewWallet().PublicKey(),
		Market:      solana.NewWallet().PublicKey(),
		User:        solana.NewWallet().PublicKey(),
		Nonce:       1,
		Side:        SideBuy,
		Outcome:     OutcomeYes,
		OrderType:   OrderTypeLimit,
		LimitPrice:  50,
		TotalAmount: 10,
		ExpiryTs:    1000,
	}
	raw := intent.Serialize()
	raw[104] = 1 << 7

	if _, err := ParseOrderIntentV1(raw); err == nil {
		t.Fatalf("expected ParseOrderIntentV1 to reject unknown flags")
	}
}
