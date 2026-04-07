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

func TestPendingBatchPreservesOrderLevelCreatedMetadata(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	batch := newPendingBatch(2001, "market", now)

	cmd := &PlaceOrderCommand{
		OrderID:         11,
		WalletAddress:   "taker",
		OriginalAction:  SideBuy,
		OriginalOutcome: 0,
		OrderType:       OrderTypeLimit,
		PriceTick:       57,
		QtyLots:         500,
		Timestamp:       now.Unix(),
		SourceCmdSeq:    123,
	}
	batch.addOrderUpdateForCommand(cmd, "new", cmd.QtyLots, 0, 0, "")

	order := AcquireOrder()
	order.OrderID = 12
	order.WalletAddress = "maker"
	order.OriginalAction = SideSell
	order.OriginalOutcome = 0
	order.OrderType = OrderTypeLimit
	order.PriceTick = 57
	order.Timestamp = now.Add(-time.Second).Unix()
	order.CreatedCmdSeq = 77
	batch.addOrderUpdateForOrder(order, "partially_filled", 200, 0, 0, "")

	event := batch.freeze(now)
	if len(event.Orders) != 2 {
		t.Fatalf("expected 2 orders, got %d", len(event.Orders))
	}
	if event.Orders[0].CreatedCmdSeq != 123 {
		t.Fatalf("expected cmd seq 123 for taker, got %d", event.Orders[0].CreatedCmdSeq)
	}
	if event.Orders[1].CreatedCmdSeq != 77 {
		t.Fatalf("expected cmd seq 77 for maker, got %d", event.Orders[1].CreatedCmdSeq)
	}
	if event.Orders[0].CreatedAt != now.Unix() {
		t.Fatalf("expected taker created_at %d, got %d", now.Unix(), event.Orders[0].CreatedAt)
	}
	if event.Orders[1].CreatedAt != now.Add(-time.Second).Unix() {
		t.Fatalf("expected maker created_at preserved, got %d", event.Orders[1].CreatedAt)
	}
}
