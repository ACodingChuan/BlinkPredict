package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/orders"
	"blinkpredict/banckend/internal/settlement"

	gsolana "github.com/gagliardetto/solana-go"
)

func TestValidateBorshOrderIntent(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey error: %v", err)
	}

	programID := gsolana.NewWallet().PublicKey()
	market := gsolana.NewWallet().PublicKey()
	wallet := gsolana.PublicKeyFromBytes(pub)

	req := orders.PlaceOrderRequest{
		Version:             1,
		ProgramID:           programID.String(),
		Market:              market.String(),
		User:                wallet.String(),
		WalletAddress:       wallet.String(),
		MarketID:            42,
		OriginalAction:      "buy",
		OriginalOutcome:     "yes",
		OriginalPriceTick:   55,
		Side:                "buy",
		OrderType:           "limit",
		PriceTick:           55,
		QtyLots:             100,
		SpendAmount:         0,
		ExpireTime:          1700000000,
		Nonce:               7,
		LimitPrice:          55,
		TotalAmount:         100,
		ExpiryTs:            1700000000,
		NormalizedSide:      "buy",
		NormalizedPriceTick: 55,
	}

	intentBytes, err := buildRawOrderIntentBytes(req, programID, market, wallet)
	if err != nil {
		t.Fatalf("buildRawOrderIntentBytes error: %v", err)
	}
	intent, err := settlement.ParseOrderIntentV1(intentBytes)
	if err != nil {
		t.Fatalf("ParseOrderIntentV1 error: %v", err)
	}

	req.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, intent.SignableMessage()))

	if _, _, err := validateBorshOrderIntent(req, wallet.String()); err != nil {
		t.Fatalf("validateBorshOrderIntent valid signature error: %v", err)
	}

	req.LimitPrice = 54
	if _, _, err := validateBorshOrderIntent(req, wallet.String()); err != errOrderSignatureInvalid {
		t.Fatalf("validateBorshOrderIntent tampered payload error = %v, want %v", err, errOrderSignatureInvalid)
	}
}

func TestDecodePlaceOrderRequestMarketIDVariants(t *testing.T) {
	t.Run("string market_id", func(t *testing.T) {
		req, err := decodePlaceOrderRequest(strings.NewReader(`{
			"market_id":"5256853293881055289",
			"program_id":"prog",
			"market":"market",
			"user":"user",
			"side":"buy",
			"outcome":"yes",
			"order_type":"limit",
			"limit_price":50,
			"total_amount":10,
			"nonce":"1",
			"expiry_ts":1700000000,
			"signature":"sig"
		}`))
		if err != nil {
			t.Fatalf("decodePlaceOrderRequest(string) error: %v", err)
		}
		if req.MarketID != 5256853293881055289 {
			t.Fatalf("market id mismatch: got=%d", req.MarketID)
		}
	})

	t.Run("numeric market_id", func(t *testing.T) {
		req, err := decodePlaceOrderRequest(strings.NewReader(`{
			"market_id":42,
			"program_id":"prog",
			"market":"market",
			"user":"user",
			"side":"buy",
			"outcome":"yes",
			"order_type":"limit",
			"limit_price":50,
			"total_amount":10,
			"nonce":"1",
			"expiry_ts":1700000000,
			"signature":"sig"
		}`))
		if err != nil {
			t.Fatalf("decodePlaceOrderRequest(number) error: %v", err)
		}
		if req.MarketID != 42 {
			t.Fatalf("market id mismatch: got=%d", req.MarketID)
		}
	})
}

func TestNormalizePlaceOrderRequestCanonicalizesServerFields(t *testing.T) {
	serverProgram := gsolana.NewWallet().PublicKey().String()
	marketPDA := gsolana.NewWallet().PublicKey().String()
	wallet := gsolana.NewWallet().PublicKey().String()

	s := &Server{
		cfg: config.Config{
			ProgramID: serverProgram,
		},
	}
	market := markets.Market{
		MarketID:  123,
		MarketPDA: marketPDA,
	}
	req := orders.PlaceOrderRequest{
		ProgramID:   "",
		Market:      "",
		User:        wallet,
		Signature:   "placeholder",
		OrderType:   "limit",
		Side:        "buy",
		Outcome:     "yes",
		LimitPrice:  55,
		TotalAmount: 100,
		ExpiryTs:    1_700_000_000,
		Nonce:       7,
	}

	normalized, _, err := s.normalizePlaceOrderRequest(req, market, wallet)
	if err != nil {
		t.Fatalf("normalizePlaceOrderRequest error: %v", err)
	}
	if normalized.ProgramID != serverProgram {
		t.Fatalf("program id mismatch: got=%s want=%s", normalized.ProgramID, serverProgram)
	}
	if normalized.Market != marketPDA {
		t.Fatalf("market mismatch: got=%s want=%s", normalized.Market, marketPDA)
	}
	if normalized.MarketID != market.MarketID {
		t.Fatalf("market id mismatch: got=%d want=%d", normalized.MarketID, market.MarketID)
	}
	if normalized.WalletAddress != wallet {
		t.Fatalf("wallet mismatch: got=%s want=%s", normalized.WalletAddress, wallet)
	}
}

func TestNormalizePlaceOrderRequestRejectsProgramMismatch(t *testing.T) {
	s := &Server{
		cfg: config.Config{
			ProgramID: gsolana.NewWallet().PublicKey().String(),
		},
	}
	market := markets.Market{
		MarketID:  1,
		MarketPDA: gsolana.NewWallet().PublicKey().String(),
	}
	wallet := gsolana.NewWallet().PublicKey().String()
	req := orders.PlaceOrderRequest{
		ProgramID:   gsolana.NewWallet().PublicKey().String(),
		Market:      market.MarketPDA,
		User:        wallet,
		Signature:   "placeholder",
		OrderType:   "limit",
		Side:        "buy",
		Outcome:     "yes",
		LimitPrice:  40,
		TotalAmount: 10,
		ExpiryTs:    1_700_000_000,
		Nonce:       1,
	}

	if _, _, err := s.normalizePlaceOrderRequest(req, market, wallet); err == nil {
		t.Fatal("expected program mismatch error")
	}
}
