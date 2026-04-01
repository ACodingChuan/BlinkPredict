package matching

import (
	"testing"
	"time"
)

func TestMarketBuySpendUsesLotScaling(t *testing.T) {
	book := NewFixedArrayOrderBook(1001)
	wallets := NewSharedWalletManager()
	wallets.SeedLedger("taker", UserWallet{AvailableUSDC: 1000})
	maker := AcquireOrder()
	maker.OrderID = 1
	maker.MarketPDA = "market"
	maker.WalletAddress = "maker"
	maker.OriginalAction = SideSell
	maker.OriginalOutcome = 0
	maker.Side = SideSell
	maker.OrderType = OrderTypeLimit
	maker.PriceTick = 50
	maker.RemainingQty = 1000 // 10.00 shares
	book.RestoreOrder(maker)
	wallets.RecoverOrderLock(maker, "market")

	taker := &PlaceOrderCommand{
		OrderID:           2,
		MarketID:          1001,
		MarketPDA:         "market",
		WalletAddress:     "taker",
		OriginalAction:    SideBuy,
		OriginalOutcome:   0,
		OriginalPriceTick: 99,
		Side:              SideBuy,
		OrderType:         OrderTypeMarket,
		PriceTick:         99,
		QtyLots:           0,
		SpendAmount:       1000,
		Timestamp:         1_700_000_000,
	}
	batch := newPendingBatch(1001, "market", testNow())
	batch.includeWrapper(&CommandWrapper{Cmd: taker, SourceCmdSeq: 1}, testNow())

	book.ProcessCommand(taker, wallets, batch)

	if len(batch.event.Fills) != 1 {
		t.Fatalf("expected one fill, got %d", len(batch.event.Fills))
	}
	if batch.event.Fills[0].FillAmount != 1000 {
		t.Fatalf("expected 1000 lots filled, got %d", batch.event.Fills[0].FillAmount)
	}
}

func TestMarketBuyNoUsesSpendAgainstBidBook(t *testing.T) {
	book := NewFixedArrayOrderBook(1002)
	wallets := NewSharedWalletManager()
	wallets.SeedLedger("taker", UserWallet{AvailableUSDC: 1000})

	maker := AcquireOrder()
	maker.OrderID = 1
	maker.MarketPDA = "market"
	maker.WalletAddress = "maker"
	maker.OriginalAction = SideBuy
	maker.OriginalOutcome = 0
	maker.OriginalPriceTick = 40
	maker.Side = SideBuy
	maker.OrderType = OrderTypeLimit
	maker.PriceTick = 40
	maker.RemainingQty = 1000
	book.RestoreOrder(maker)
	wallets.RecoverOrderLock(maker, "market")

	taker := &PlaceOrderCommand{
		OrderID:           2,
		MarketID:          1002,
		MarketPDA:         "market",
		WalletAddress:     "taker",
		OriginalAction:    SideBuy,
		OriginalOutcome:   1,
		OriginalPriceTick: 60,
		Side:              SideSell,
		OrderType:         OrderTypeMarket,
		PriceTick:         40,
		QtyLots:           0,
		SpendAmount:       1000,
		Timestamp:         1_700_000_000,
	}
	batch := newPendingBatch(1002, "market", testNow())
	batch.includeWrapper(&CommandWrapper{Cmd: taker, SourceCmdSeq: 1}, testNow())

	book.ProcessCommand(taker, wallets, batch)

	if len(batch.event.Fills) != 1 {
		t.Fatalf("expected one fill, got %d", len(batch.event.Fills))
	}
	if batch.event.Fills[0].FillAmount != 1000 {
		t.Fatalf("expected 1000 lots filled, got %d", batch.event.Fills[0].FillAmount)
	}
	last := batch.event.OrderUpdates[len(batch.event.OrderUpdates)-1]
	if last.Status != "canceled" || last.RemainingSpendAmount != 400 || last.RefundAmount != 400 {
		t.Fatalf("expected residual spend refund on cancel, got %#v", batch.event.OrderUpdates)
	}
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}
