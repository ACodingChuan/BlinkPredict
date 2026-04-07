package pusher

import (
	"testing"
	"time"

	"blinkpredict/banckend/internal/matching"
)

func TestCompressDepthUpdatesNormalizesLegacySellSide(t *testing.T) {
	levels := compressDepthUpdates([]matching.DepthUpdate{
		{Side: "sell", PriceTick: 42, TotalVolume: 10},
		{Side: "ask", PriceTick: 42, TotalVolume: 25},
	})
	if len(levels) != 1 {
		t.Fatalf("expected 1 level after normalization, got %d", len(levels))
	}
	if levels[0].Side != "ask" || levels[0].PriceTick != 42 || levels[0].TotalVolume != 25 {
		t.Fatalf("unexpected normalized level: %+v", levels[0])
	}
}

func TestMarketStateApplyMatchEventTreatsSellAsAsk(t *testing.T) {
	state := &marketState{}
	event := matching.MatchBatchEvent{
		MarketID:     99,
		EventID:      "evt-1",
		ProducedAt:   time.Now().UTC().Unix(),
		DepthUpdates: []matching.DepthUpdate{{Side: "sell", PriceTick: 61, TotalVolume: 900}},
	}
	_ = state.applyMatchEvent(event)

	_, snapshot := state.snapshotPayload()
	if len(snapshot.Orderbook.Asks) != 1 {
		t.Fatalf("expected 1 ask level, got %d", len(snapshot.Orderbook.Asks))
	}
	if snapshot.Orderbook.Asks[0].Price != "61" || snapshot.Orderbook.Asks[0].TotalVolume != "900" {
		t.Fatalf("unexpected ask level: %+v", snapshot.Orderbook.Asks[0])
	}
}
