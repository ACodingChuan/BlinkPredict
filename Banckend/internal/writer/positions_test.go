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
	if delta.CollateralFreeDelta != -400 || delta.CollateralLockedDelta != 400 {
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
	if delta.NoFreeLotsDelta != 1000 {
		t.Fatalf("expected 1000 no lots credited, got %+v", delta)
	}
	if delta.CollateralLockedDelta != -400 {
		t.Fatalf("expected locked collateral decrease 400, got %+v", delta)
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
	if delta.CollateralFreeDelta != 400 {
		t.Fatalf("expected collateral credit 400, got %+v", delta)
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
	if delta.CollateralLockedDelta != -200 || delta.CollateralFreeDelta != 200 {
		t.Fatalf("unexpected unlock delta: %+v", delta)
	}
}
