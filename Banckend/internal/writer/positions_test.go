package writer

import (
	"testing"

	"blinkpredict/banckend/internal/matching"
)

func TestInitialLockDeltaForBuyNoLocksCollateralByOriginalPrice(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletA",
		OriginalAction:    "buy",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := initialLockDelta(1001, meta, 1000, 0) // 10 shares @ 40c => 400 units
	if delta.CollateralLockedDelta != 400 {
		t.Fatalf("unexpected collateral lock delta: %+v", delta)
	}
}

func TestSettlementDeltaForBuyNoCreditsNoAndConsumesCorrectCollateral(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletA",
		OriginalAction:    "buy",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := settlementDelta(1001, meta, 1000, 60) // normalized yes price 60 => no price 40
	if delta.NoPendingLotsDelta != 1000 {
		t.Fatalf("expected 1000 no lots credited, got %+v", delta)
	}
	if delta.CollateralLockedDelta != -400 {
		t.Fatalf("expected locked collateral decrease 400, got %+v", delta)
	}
}

func TestSettlementDeltaForBuyYesMovesSharesToPending(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletA",
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 57,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := settlementDelta(1001, meta, 600, 55)
	if delta.YesPendingLotsDelta != 600 {
		t.Fatalf("expected 600 pending yes lots, got %+v", delta)
	}
	if delta.YesFreeLotsDelta != 0 {
		t.Fatalf("buy fill must not release shares to free before settlement: %+v", delta)
	}
	if delta.CollateralLockedDelta != -342 {
		t.Fatalf("expected locked collateral decrease 342, got %+v", delta)
	}
}

func TestSettlementDeltaForSellNoCreditsCollateralAtNoPrice(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletB",
		OriginalAction:    "sell",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := settlementDelta(1001, meta, 1000, 60)
	if delta.NoLockedLotsDelta != -1000 {
		t.Fatalf("expected no locked lots decrease, got %+v", delta)
	}
}

func TestUnlockDeltaForBuyNoReleasesOriginalCollateral(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletA",
		OriginalAction:    "buy",
		OriginalOutcome:   "no",
		OriginalPriceTick: 40,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := unlockDelta(1001, meta, 500, 0) // 5 remaining shares @ 40c => 200 units unlock
	if delta.CollateralLockedDelta != -200 {
		t.Fatalf("unexpected unlock delta: %+v", delta)
	}
}

func TestInitialLockDeltaRoundsCollateralUpForCentiShares(t *testing.T) {
	meta := orderMeta{
		WalletAddress:     "walletC",
		OriginalAction:    "buy",
		OriginalOutcome:   "yes",
		OriginalPriceTick: 60,
		OrderType:         matching.OrderTypeLimit,
	}

	delta := initialLockDelta(1001, meta, 101, 0) // 1.01 shares @ 60c => ceil(60.6c) = 61
	if delta.CollateralLockedDelta != 61 {
		t.Fatalf("unexpected rounded lock delta: %+v", delta)
	}
}
