package writer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"blinkpredict/banckend/internal/matching"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestUpdateRedisReadModelsWritesOpenDepthTradeAndHistory(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	w := &Writer{rdb: rdb}
	now := time.Now().UTC().Unix()
	batch := matching.BatchEventPayload{
		MarketID:     1001,
		SourceCmdSeq: 7,
		Timestamp:    now,
		SourceOrder: &matching.FullOrderData{
			OrderID:            11,
			WalletAddress:      "walletA",
			Side:               matching.SideBuy,
			OrderType:          matching.OrderTypeLimit,
			PriceTick:          60,
			InitialQty:         1000,
			InitialSpendAmount: 0,
			ExpireTime:         1_800_000_000,
			Signature:          "sig",
			IntentBytesHex:     "beef",
			Nonce:              99,
			CreatedCmdSeq:      7,
		},
		StateEvents: []matching.OrderStateEvent{
			{OrderID: 11, Status: matching.StatusPartiallyFilled, RemainingQty: 700},
		},
		DepthEvents: []matching.L2DepthEvent{
			{Side: matching.SideBuy, PriceTick: 60, TotalVolume: 700},
		},
		TradeEvents: []matching.TradeEvent{
			{TradeID: "t_1", MatchPrice: 60, MatchQty: 300, MakerOrderID: 1, TakerOrderID: 11},
		},
	}

	w.updateRedisReadModels(context.Background(), &batch)

	if got := mr.HGet("l2:depth:1001", "bid:60"); got != "700" {
		t.Fatalf("expected bid depth 700, got %q", got)
	}
	if members, err := rdb.ZRevRange(context.Background(), "user:orders:walletA", 0, -1).Result(); err != nil || len(members) != 1 || members[0] != "11" {
		t.Fatalf("expected open order in user zset")
	}
	if got := mr.HGet("order:info:11", "remaining_qty"); got != "700" {
		t.Fatalf("expected remaining_qty=700, got %q", got)
	}
	if got := mr.HGet("order:info:11", "status"); got != "2" {
		t.Fatalf("expected status=2, got %q", got)
	}
	gotTrades, err := rdb.LRange(context.Background(), "trades:latest:1001", 0, -1).Result()
	if err != nil || len(gotTrades) != 1 {
		t.Fatalf("expected 1 recent trade, got %d", len(gotTrades))
	}
	gotPoints, err := rdb.ZCard(context.Background(), "price:history:1001").Result()
	if err != nil || gotPoints != 1 {
		t.Fatalf("expected 1 price point, got %d", gotPoints)
	}
}

func TestUpdateRedisReadModelsRemovesClosedOrderAndSetsTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	mr.HSet("order:info:12", "wallet_address", "walletB")
	mr.HSet("order:info:12", "created_cmd_seq", "8")
	mr.ZAdd("user:orders:walletB", 8, "12")

	w := &Writer{rdb: rdb}
	now := time.Now().UTC().Unix()
	batch := matching.BatchEventPayload{
		MarketID:  1002,
		Timestamp: now,
		StateEvents: []matching.OrderStateEvent{
			{OrderID: 12, Status: matching.StatusFilled, RemainingQty: 0},
		},
	}

	w.updateRedisReadModels(context.Background(), &batch)

	if members, err := rdb.ZRevRange(context.Background(), "user:orders:walletB", 0, -1).Result(); err != nil || len(members) != 0 {
		t.Fatalf("expected filled order removed from user zset")
	}
	if ttl := mr.TTL("order:info:12"); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("expected ttl set for closed order, got %v", ttl)
	}
}

func TestUpdateRedisReadModelsAppendsTradePayloadShape(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	w := &Writer{rdb: rdb}
	now := time.Now().UTC().Unix()
	batch := matching.BatchEventPayload{
		MarketID:  999,
		Timestamp: now,
		TradeEvents: []matching.TradeEvent{
			{TradeID: "t_2", MatchPrice: 41, MatchQty: 123, MakerOrderID: 5, TakerOrderID: 6},
		},
	}

	w.updateRedisReadModels(context.Background(), &batch)

	items, err := rdb.LRange(context.Background(), "trades:latest:999", 0, -1).Result()
	if err != nil {
		t.Fatalf("read trades list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one trade payload, got %d", len(items))
	}
	var trade matching.Trade
	if err := json.Unmarshal([]byte(items[0]), &trade); err != nil {
		t.Fatalf("unmarshal trade payload: %v", err)
	}
	if trade.ID != "t_2" || trade.Price != "41" || trade.Quantity != "123" {
		t.Fatalf("unexpected trade payload: %+v", trade)
	}
}
