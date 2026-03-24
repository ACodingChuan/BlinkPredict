package matching

import "testing"

func TestMarketBuySpendUsesLotScaling(t *testing.T) {
	book := NewFixedArrayOrderBook(1001)
	maker := AcquireOrder()
	maker.OrderID = 1
	maker.WalletAddress = "maker"
	maker.Side = SideSell
	maker.OrderType = OrderTypeLimit
	maker.PriceTick = 50
	maker.RemainingQty = 1000 // 10.00 shares
	book.RestoreOrder(maker)

	taker := &PlaceOrderCommand{
		OrderID:       2,
		MarketID:      1001,
		WalletAddress: "taker",
		Side:          SideBuy,
		OrderType:     OrderTypeMarket,
		PriceTick:     99,
		QtyLots:       0,
		SpendAmount:   1000, // $10.00
		Timestamp:     1_700_000_000,
	}
	batch := &BatchEventPayload{
		MarketID:     1001,
		SourceCmdSeq: 1,
		Timestamp:    taker.Timestamp,
	}

	book.ProcessCommand(taker, batch)

	if len(batch.TradeEvents) != 1 {
		t.Fatalf("expected one trade event, got %d", len(batch.TradeEvents))
	}
	if batch.TradeEvents[0].MatchQty != 1000 {
		t.Fatalf("expected 1000 lots filled, got %d", batch.TradeEvents[0].MatchQty)
	}
}
