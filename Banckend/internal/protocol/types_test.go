package protocol

import "testing"

func TestValidatePlaceOrderCommandLimit(t *testing.T) {
	cmd := PlaceOrderCommand{
		CommandID:      "1001",
		TraceID:        "1002",
		IdempotencyKey: "1003",
		Timestamp:      1_700_000_000,
		MarketID:       42,
		MarketPDA:      "marketPda",
		Execution: PlaceOrderExecution{
			OrderID:             1001,
			WalletAddress:       "wallet",
			OriginalAction:      "buy",
			OriginalOutcome:     "yes",
			OriginalPriceTick:   50,
			OrderType:           "limit",
			NormalizedSide:      "buy",
			NormalizedPriceTick: 50,
			QtyLots:             100,
			SpendAmount:         0,
			ExpireTime:          1_800_000_000,
			Nonce:               123,
		},
		Settlement: SettlementPayload{
			IntentBytesHex: "deadbeef",
			Signature:      "sig",
		},
	}
	if err := ValidatePlaceOrderCommand(cmd); err != nil {
		t.Fatalf("expected valid command, got %v", err)
	}
}

func TestValidatePlaceOrderCommandRejectsMarketBuyQtyLots(t *testing.T) {
	cmd := PlaceOrderCommand{
		CommandID:      "1001",
		TraceID:        "1002",
		IdempotencyKey: "1003",
		Timestamp:      1_700_000_000,
		MarketID:       42,
		MarketPDA:      "marketPda",
		Execution: PlaceOrderExecution{
			OrderID:             1001,
			WalletAddress:       "wallet",
			OriginalAction:      "buy",
			OriginalOutcome:     "yes",
			OriginalPriceTick:   50,
			OrderType:           "market",
			NormalizedSide:      "buy",
			NormalizedPriceTick: 50,
			QtyLots:             100,
			SpendAmount:         100,
			Nonce:               123,
		},
		Settlement: SettlementPayload{
			IntentBytesHex: "deadbeef",
			Signature:      "sig",
		},
	}
	if err := ValidatePlaceOrderCommand(cmd); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidatePlaceOrderCommandAllowsMarketBuyNoWithNormalizedSell(t *testing.T) {
	cmd := PlaceOrderCommand{
		CommandID:      "1001",
		TraceID:        "1002",
		IdempotencyKey: "1003",
		Timestamp:      1_700_000_000,
		MarketID:       42,
		MarketPDA:      "marketPda",
		Execution: PlaceOrderExecution{
			OrderID:             1001,
			WalletAddress:       "wallet",
			OriginalAction:      "buy",
			OriginalOutcome:     "no",
			OriginalPriceTick:   36,
			OrderType:           "market",
			NormalizedSide:      "sell",
			NormalizedPriceTick: 64,
			QtyLots:             0,
			SpendAmount:         1000,
			Nonce:               123,
		},
		Settlement: SettlementPayload{
			IntentBytesHex: "deadbeef",
			Signature:      "sig",
		},
	}
	if err := ValidatePlaceOrderCommand(cmd); err != nil {
		t.Fatalf("expected valid market buy no command, got %v", err)
	}
}
