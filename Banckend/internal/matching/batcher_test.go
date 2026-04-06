package matching

import (
	"testing"
	"time"
)

func TestTickBatchEventIDIsDeterministic(t *testing.T) {
	build := func(now time.Time) string {
		order := AcquireOrder()
		order.OrderID = 9
		order.MarketPDA = "market"
		order.WalletAddress = "maker"
		order.OriginalAction = SideBuy
		order.OriginalOutcome = 0
		order.OriginalPriceTick = 57
		order.Side = SideBuy
		order.OrderType = OrderTypeLimit
		order.PriceTick = 57
		order.RemainingQty = 600

		batch := newPendingBatch(2001, "market", now)
		batch.addOrderUpdateForOrder(order, "expired", order.RemainingQty, order.RemainingSpend, releaseRefundForOrder(order), "expired")
		batch.addDepthUpdate(order.Side, order.PriceTick, 0)
		return batch.freeze(now).EventID
	}

	first := build(time.Unix(1_700_000_000, 0).UTC())
	second := build(time.Unix(1_800_000_000, 0).UTC())

	if first != second {
		t.Fatalf("expected deterministic tick event id, got %q vs %q", first, second)
	}
}
