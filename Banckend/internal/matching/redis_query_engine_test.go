package matching

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisQueryEngineReadsHotModels(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mr.HSet("l2:depth:1001", "bid:60", "700")
	mr.HSet("l2:depth:1001", "ask:61", "900")
	mr.ZAdd("user:orders:walletA", 7, "11")
	mr.HSet("order:info:11",
		"market_id", "1001",
		"wallet_address", "walletA",
		"side", "0",
		"order_type", "0",
		"price_tick", "60",
		"remaining_qty", "700",
		"created_cmd_seq", "7",
	)
	tradePayload, _ := json.Marshal(Trade{
		ID:         "t_1",
		Price:      "60",
		Quantity:   "300",
		ExecutedAt: "2026-03-22T10:00:00Z",
	})
	if err := rdb.LPush(context.Background(), "trades:latest:1001", string(tradePayload)).Err(); err != nil {
		t.Fatalf("seed trades list: %v", err)
	}
	pricePayload, _ := json.Marshal(PricePoint{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Price:     "60",
		Quantity:  "300",
	})
	mr.ZAdd("price:history:1001", float64(time.Now().UTC().UnixMilli()), string(pricePayload))

	engine := NewRedisQueryEngine(rdb, nil, NewDisabledEngine())

	book := engine.GetOrderbook(context.Background(), 1001)
	if !book.MatchingEnabled || len(book.Bids) != 1 || len(book.Asks) != 1 {
		t.Fatalf("unexpected orderbook: %+v", book)
	}
	if book.BestBidPrice != "60" || book.BestAskPrice != "61" {
		t.Fatalf("unexpected best prices: %+v", book)
	}

	orders := engine.GetOpenOrders(context.Background(), "walletA", 1001)
	if len(orders) != 1 {
		t.Fatalf("expected one open order, got %d", len(orders))
	}
	if orders[0].ID != "11" || orders[0].Side != "buy" || orders[0].Price != "60" || orders[0].Status != "open" {
		t.Fatalf("unexpected open order: %+v", orders[0])
	}

	trades := engine.GetTrades(context.Background(), 1001)
	if len(trades) != 1 || trades[0].ID != "t_1" || trades[0].Price != "60" {
		t.Fatalf("unexpected trades: %+v", trades)
	}

	history := engine.GetPriceHistory(context.Background(), 1001, PriceHistoryRange1D)
	if history.Range != PriceHistoryRange1D || len(history.Points) != 1 {
		t.Fatalf("unexpected price history: %+v", history)
	}
	if history.Points[0].Price != "60" {
		t.Fatalf("unexpected price point: %+v", history.Points[0])
	}
}

func TestRedisQueryEngineFallsBackWhenRedisMissing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	engine := NewRedisQueryEngine(rdb, nil, NewDisabledEngine())

	book := engine.GetOrderbook(context.Background(), 1)
	if book.MatchingEnabled {
		t.Fatalf("expected disabled fallback orderbook, got %+v", book)
	}
	if len(engine.GetOpenOrders(context.Background(), "wallet", 1)) != 0 {
		t.Fatalf("expected no open orders from fallback")
	}
	if len(engine.GetTrades(context.Background(), 1)) != 0 {
		t.Fatalf("expected no trades from fallback")
	}
	history := engine.GetPriceHistory(context.Background(), 1, PriceHistoryRange1D)
	if len(history.Points) != 0 {
		t.Fatalf("expected empty price history from fallback")
	}
}
