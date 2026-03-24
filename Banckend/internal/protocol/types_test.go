package protocol

import "testing"

func TestValidatePlaceOrderCommandLimit(t *testing.T) {
	cmd := PlaceOrderCommand{
		OrderID:           1001,
		MarketID:          42,
		WalletAddress:     "wallet",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 50,
		Signature:         "sig",
		Nonce:             123,
		IntentBytesHex:    "deadbeef",
		Timestamp:         1_700_000_000,
		Side:              SideBuy,
		OrderType:         OrderTypeLimit,
		PriceTick:         50,
		QtyLots:           100,
		SpendAmount:       0,
		ExpireTime:        1_800_000_000,
	}
	if err := ValidatePlaceOrderCommand(cmd); err != nil {
		t.Fatalf("expected valid command, got %v", err)
	}
}

func TestValidatePlaceOrderCommandRejectsMarketBuyQtyLots(t *testing.T) {
	cmd := PlaceOrderCommand{
		OrderID:           1001,
		MarketID:          42,
		WalletAddress:     "wallet",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 50,
		Signature:         "sig",
		Nonce:             123,
		IntentBytesHex:    "deadbeef",
		Timestamp:         1_700_000_000,
		Side:              SideBuy,
		OrderType:         OrderTypeMarket,
		PriceTick:         50,
		QtyLots:           100,
		SpendAmount:       100,
	}
	if err := ValidatePlaceOrderCommand(cmd); err == nil {
		t.Fatal("expected validation error")
	}
}
