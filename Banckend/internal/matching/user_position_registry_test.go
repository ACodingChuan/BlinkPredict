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
