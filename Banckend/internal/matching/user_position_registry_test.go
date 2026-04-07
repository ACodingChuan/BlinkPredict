package matching

import "testing"

func TestPendingBatchRequiresColdLimit(t *testing.T) {
	registry := NewUserPositionRegistry()
	registry.MarkExists(7, "known-wallet")

	batch := newPendingBatch(7, "market-pda", testNow())
	batch.event.Fills = append(batch.event.Fills, MatchFill{FillIndex: 1})
	batch.event.Orders = append(batch.event.Orders,
		MatchedOrder{
			OrderIndex: 0,
			OrderID:    1,
			Execution:  ExecutionSnapshot{WalletAddress: "known-wallet"},
		},
		MatchedOrder{
			OrderIndex: 1,
			OrderID:    2,
			Execution:  ExecutionSnapshot{WalletAddress: "unknown-wallet"},
		},
	)

	if !batch.requiresColdLimit(registry) {
		t.Fatalf("expected cold limit when batch contains unknown user position wallet")
	}

	registry.MarkExists(7, "unknown-wallet")
	if batch.requiresColdLimit(registry) {
		t.Fatalf("expected hot limit once all wallets are known")
	}
}

func TestMarketManagerMaxFillsUsesColdCeilingWhenUserPositionUnknown(t *testing.T) {
	manager := &MarketManager{
		batchConfig: BatchConfig{
			MaxFillsHot:  9,
			MaxFillsCold: 3,
		},
		positionRegistry: NewUserPositionRegistry(),
		orderRegistry:    NewOrderStateRegistry(),
	}
	manager.positionRegistry.MarkExists(7, "known-wallet")
	manager.orderRegistry.MarkExists(7, "known-wallet", 11)
	manager.orderRegistry.MarkExists(7, "unknown-wallet", 22)

	batch := newPendingBatch(7, "market-pda", testNow())
	batch.event.Fills = append(batch.event.Fills, MatchFill{FillIndex: 1})
	batch.event.Orders = append(batch.event.Orders,
		MatchedOrder{
			OrderIndex: 0,
			OrderID:    1,
			Execution:  ExecutionSnapshot{WalletAddress: "known-wallet", Nonce: 11},
		},
		MatchedOrder{
			OrderIndex: 1,
			OrderID:    2,
			Execution:  ExecutionSnapshot{WalletAddress: "unknown-wallet", Nonce: 22},
		},
	)

	if got := manager.maxFillsForBatch(batch); got != 3 {
		t.Fatalf("expected cold ceiling 3 when user position is unknown, got %d", got)
	}

	manager.positionRegistry.MarkExists(7, "unknown-wallet")
	if got := manager.maxFillsForBatch(batch); got != 9 {
		t.Fatalf("expected hot ceiling 9 once all user positions are known, got %d", got)
	}
}
