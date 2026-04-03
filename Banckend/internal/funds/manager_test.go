package funds

import "testing"

func TestBuyFillStaysPendingUntilSettlementConfirmed(t *testing.T) {
	mgr := NewManager()
	mgr.SeedLedger("alice", UserWallet{AvailableUSDC: 1000})

	cmd := ReserveOrderInput{
		WalletAddress:     "alice",
		MarketPDA:         "market-1",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 60,
		OrderType:         OrderTypeLimit,
		QtyLots:           100,
	}
	if err := mgr.ReserveOrder(cmd); err != nil {
		t.Fatalf("ReserveOrder error: %v", err)
	}

	snap := mgr.Snapshot("alice", "market-1")
	if snap.Ledger.AvailableUSDC != 940 || snap.Ledger.LockedUSDC != 60 {
		t.Fatalf("unexpected ledger after reserve: %#v", snap.Ledger)
	}

	maker := ActiveOrder{
		WalletAddress:     "maker",
		MarketPDA:         "market-1",
		OriginalAction:    SideSell,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 55,
		OrderType:         OrderTypeLimit,
		RemainingQty:      100,
	}
	order := ActiveOrder{
		WalletAddress:     "alice",
		MarketPDA:         "market-1",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 60,
		OrderType:         OrderTypeLimit,
		RemainingQty:      100,
	}
	mgr.ApplyMatchPending(maker, order, 100, 55)

	snap = mgr.Snapshot("alice", "market-1")
	if snap.Position.AvailableYesShares != 0 {
		t.Fatalf("shares must not be available before settlement: %#v", snap.Position)
	}
	if snap.Position.PendingYesShares != 100 {
		t.Fatalf("expected pending yes shares to increase, got %#v", snap.Position)
	}
	if snap.Ledger.LockedUSDC != 0 || snap.Ledger.AvailableUSDC != 945 || snap.Ledger.PendingUSDC != -55 {
		t.Fatalf("unexpected ledger after fill: %#v", snap.Ledger)
	}

	mgr.ApplySettlementConfirmed("alice", "market-1")
	snap = mgr.Snapshot("alice", "market-1")
	if snap.Position.AvailableYesShares != 100 || snap.Position.PendingYesShares != 0 {
		t.Fatalf("settlement confirmation did not release pending shares: %#v", snap.Position)
	}
	if snap.Ledger.PendingUSDC != 0 {
		t.Fatalf("pending usdc should be cleared after settlement: %#v", snap.Ledger)
	}
}

func TestSellFillCreatesPendingUSDC(t *testing.T) {
	mgr := NewManager()
	mgr.SeedPosition("bob", "market-1", MarketPosition{AvailableYesShares: 300})

	cmd := ReserveOrderInput{
		WalletAddress:   "bob",
		MarketPDA:       "market-1",
		OriginalAction:  SideSell,
		OriginalOutcome: OutcomeYes,
		OrderType:       OrderTypeLimit,
		QtyLots:         120,
	}
	if err := mgr.ReserveOrder(cmd); err != nil {
		t.Fatalf("ReserveOrder error: %v", err)
	}

	maker := ActiveOrder{
		WalletAddress:     "maker",
		MarketPDA:         "market-1",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 47,
		OrderType:         OrderTypeLimit,
		RemainingQty:      100,
	}
	order := ActiveOrder{
		WalletAddress:   "bob",
		MarketPDA:       "market-1",
		OriginalAction:  SideSell,
		OriginalOutcome: OutcomeYes,
		OrderType:       OrderTypeLimit,
		RemainingQty:    120,
	}
	mgr.ApplyMatchPending(maker, order, 100, 47)

	snap := mgr.Snapshot("bob", "market-1")
	if snap.Position.AvailableYesShares != 180 {
		t.Fatalf("locked shares should not return to available before release/settlement: %#v", snap.Position)
	}
	if snap.Position.LockedYesShares != 20 {
		t.Fatalf("expected remaining locked shares, got %#v", snap.Position)
	}
	if snap.Ledger.AvailableUSDC != 0 || snap.Ledger.PendingUSDC != 47 {
		t.Fatalf("sell proceeds must stay pending before settlement: %#v", snap.Ledger)
	}

	mgr.ApplySettlementConfirmed("bob", "market-1")
	snap = mgr.Snapshot("bob", "market-1")
	if snap.Ledger.AvailableUSDC != 47 || snap.Ledger.PendingUSDC != 0 {
		t.Fatalf("settlement confirmation did not release pending proceeds: %#v", snap.Ledger)
	}
}

func TestReleaseOrderReturnsLockedBalance(t *testing.T) {
	mgr := NewManager()
	mgr.SeedLedger("carol", UserWallet{AvailableUSDC: 500})

	cmd := ReserveOrderInput{
		WalletAddress:     "carol",
		MarketPDA:         "market-2",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 50,
		OrderType:         OrderTypeLimit,
		QtyLots:           200,
	}
	if err := mgr.ReserveOrder(cmd); err != nil {
		t.Fatalf("ReserveOrder error: %v", err)
	}

	order := ActiveOrder{
		WalletAddress:     "carol",
		MarketPDA:         "market-2",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 50,
		OrderType:         OrderTypeLimit,
		RemainingQty:      200,
	}
	mgr.ReleaseOrder(order, 0)

	snap := mgr.Snapshot("carol", "market-2")
	if snap.Ledger.AvailableUSDC != 500 || snap.Ledger.LockedUSDC != 0 {
		t.Fatalf("release did not restore locked funds: %#v", snap.Ledger)
	}
}
