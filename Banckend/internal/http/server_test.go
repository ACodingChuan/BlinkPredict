package httpapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"

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
