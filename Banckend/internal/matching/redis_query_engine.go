package matching

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const recentTradesLimit = 100

type RedisQueryEngine struct {
	rdb      *redis.Client
	pool     *pgxpool.Pool
	fallback Engine
}

func NewRedisQueryEngine(rdb *redis.Client, pool *pgxpool.Pool, fallback Engine) *RedisQueryEngine {
	if fallback == nil {
		fallback = NewDisabledEngine()
	}
	return &RedisQueryEngine{rdb: rdb, pool: pool, fallback: fallback}
}

func (r *RedisQueryEngine) GetOrderbook(ctx context.Context, marketID uint64) OrderbookSnapshot {
	if r.rdb == nil {
		return r.fallback.GetOrderbook(ctx, marketID)
	}
	key := fmt.Sprintf("l2:depth:%d", marketID)
	values, err := r.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(values) == 0 {
		return r.fallback.GetOrderbook(ctx, marketID)
	}

	type row struct {
		price uint64
		vol   string
	}
	bids := make([]row, 0)
	asks := make([]row, 0)
	for field, vol := range values {
		parts := strings.Split(field, ":")
		if len(parts) != 2 {
			continue
		}
		price, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		switch parts[0] {
		case "bid", "buy":
			bids = append(bids, row{price: price, vol: vol})
		case "ask", "sell":
			asks = append(asks, row{price: price, vol: vol})
		}
	}
	sort.Slice(bids, func(i, j int) bool { return bids[i].price > bids[j].price })
	sort.Slice(asks, func(i, j int) bool { return asks[i].price < asks[j].price })

	snapshot := OrderbookSnapshot{
		Bids:            make([]OrderLevel, 0, len(bids)),
		Asks:            make([]OrderLevel, 0, len(asks)),
		MatchingEnabled: true,
	}
	for _, bid := range bids {
		snapshot.Bids = append(snapshot.Bids, OrderLevel{
			Price:       strconv.FormatUint(bid.price, 10),
			TotalVolume: bid.vol,
		})
	}
	for _, ask := range asks {
		snapshot.Asks = append(snapshot.Asks, OrderLevel{
			Price:       strconv.FormatUint(ask.price, 10),
			TotalVolume: ask.vol,
		})
	}
	if len(bids) > 0 {
		snapshot.BestBidPrice = strconv.FormatUint(bids[0].price, 10)
	}
	if len(asks) > 0 {
		snapshot.BestAskPrice = strconv.FormatUint(asks[0].price, 10)
	}
	return snapshot
}

func (r *RedisQueryEngine) GetTrades(ctx context.Context, marketID uint64) []Trade {
	if r.rdb != nil {
		key := fmt.Sprintf("trades:latest:%d", marketID)
		payloads, err := r.rdb.LRange(ctx, key, 0, recentTradesLimit-1).Result()
		if err == nil && len(payloads) > 0 {
			trades := make([]Trade, 0, len(payloads))
			for _, payload := range payloads {
				var item Trade
				if err := json.Unmarshal([]byte(payload), &item); err != nil {
					continue
				}
				trades = append(trades, item)
			}
			if len(trades) > 0 {
				return trades
			}
		}
	}
	if r.pool != nil {
		rows, err := r.pool.Query(ctx, `
			SELECT trade_id, match_price, match_qty, executed_at
			FROM trades
			WHERE market_id = $1
			ORDER BY executed_at DESC
			LIMIT $2
		`, int64(marketID), recentTradesLimit)
		if err == nil {
			defer rows.Close()
			trades := make([]Trade, 0)
			for rows.Next() {
				var (
					id         string
					price      int16
					qty        int64
					executedAt time.Time
				)
				if err := rows.Scan(&id, &price, &qty, &executedAt); err != nil {
					return r.fallback.GetTrades(ctx, marketID)
				}
				trades = append(trades, Trade{
					ID:         id,
					Price:      strconv.FormatInt(int64(price), 10),
					Quantity:   strconv.FormatInt(qty, 10),
					ExecutedAt: executedAt.UTC().Format(time.RFC3339),
				})
			}
			if len(trades) > 0 {
				return trades
			}
		}
	}
	return r.fallback.GetTrades(ctx, marketID)
}

func (r *RedisQueryEngine) GetOpenOrders(ctx context.Context, walletAddress string, marketID uint64) []OpenOrder {
	if r.rdb == nil || strings.TrimSpace(walletAddress) == "" {
		return r.fallback.GetOpenOrders(ctx, walletAddress, marketID)
	}
	key := fmt.Sprintf("user:orders:%s", walletAddress)
	ids, err := r.rdb.ZRevRange(ctx, key, 0, -1).Result()
	if err != nil || len(ids) == 0 {
		return r.fallback.GetOpenOrders(ctx, walletAddress, marketID)
	}

	orders := make([]OpenOrder, 0, len(ids))
	for _, id := range ids {
		infoKey := fmt.Sprintf("order:info:%s", id)
		info, err := r.rdb.HGetAll(ctx, infoKey).Result()
		if err != nil || len(info) == 0 {
			continue
		}
		if info["market_id"] != strconv.FormatUint(marketID, 10) {
			continue
		}
		side := "buy"
		if info["original_action"] != "" {
			side = info["original_action"]
		} else if info["side"] == "1" {
			side = "sell"
		}
		price := info["original_price_tick"]
		if price == "" {
			price = info["price_tick"]
		}
		quantity := info["remaining_qty"]
		status := info["status_text"]
		if status == "" {
			status = "open"
		}
		outcome := info["original_outcome"]
		if outcome == "" {
			outcome = "yes"
		}
		orders = append(orders, OpenOrder{
			ID:       id,
			Side:     side,
			Outcome:  outcome,
			Price:    price,
			Quantity: quantity,
			Status:   status,
		})
	}
	if len(orders) == 0 {
		return r.fallback.GetOpenOrders(ctx, walletAddress, marketID)
	}
	return orders
}

func (r *RedisQueryEngine) GetPriceHistory(ctx context.Context, marketID uint64, historyRange PriceHistoryRange) PriceHistory {
	if historyRange == PriceHistoryRangeAll && r.pool != nil {
		points := r.loadPriceHistoryFromPostgres(ctx, marketID, historyRange)
		return PriceHistory{Range: historyRange, Points: points}
	}
	if r.rdb != nil {
		key := fmt.Sprintf("price:history:%d", marketID)
		minScore := rangeMinScore(historyRange)
		values, err := r.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
			Min: minScore,
			Max: "+inf",
		}).Result()
		if err == nil && len(values) > 0 {
			points := make([]PricePoint, 0, len(values))
			for _, value := range values {
				var point PricePoint
				if err := json.Unmarshal([]byte(value), &point); err != nil {
					continue
				}
				points = append(points, point)
			}
			if len(points) > 0 {
				return PriceHistory{Range: historyRange, Points: points}
			}
		}
	}
	if r.pool != nil {
		points := r.loadPriceHistoryFromPostgres(ctx, marketID, historyRange)
		return PriceHistory{Range: historyRange, Points: points}
	}
	return r.fallback.GetPriceHistory(ctx, marketID, historyRange)
}

func (r *RedisQueryEngine) loadPriceHistoryFromPostgres(ctx context.Context, marketID uint64, historyRange PriceHistoryRange) []PricePoint {
	if r.pool == nil {
		return nil
	}
	query := `
		SELECT match_price, match_qty, executed_at
		FROM trades
		WHERE market_id = $1
	`
	args := []any{int64(marketID)}
	if historyRange != PriceHistoryRangeAll {
		query += ` AND executed_at >= $2`
		args = append(args, rangeStartTime(historyRange))
	}
	query += ` ORDER BY executed_at ASC`
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	points := make([]PricePoint, 0)
	for rows.Next() {
		var (
			price      int16
			qty        int64
			executedAt time.Time
		)
		if err := rows.Scan(&price, &qty, &executedAt); err != nil {
			return nil
		}
		points = append(points, PricePoint{
			Timestamp: executedAt.UTC().Format(time.RFC3339),
			Price:     strconv.FormatInt(int64(price), 10),
			Quantity:  strconv.FormatInt(qty, 10),
		})
	}
	return points
}

func rangeStartTime(historyRange PriceHistoryRange) time.Time {
	now := time.Now().UTC()
	switch historyRange {
	case PriceHistoryRange1H:
		return now.Add(-1 * time.Hour)
	case PriceHistoryRange6H:
		return now.Add(-6 * time.Hour)
	case PriceHistoryRange1D:
		return now.Add(-24 * time.Hour)
	case PriceHistoryRange1W:
		return now.Add(-7 * 24 * time.Hour)
	case PriceHistoryRange1M:
		return now.Add(-30 * 24 * time.Hour)
	default:
		return time.Unix(0, 0).UTC()
	}
}

func rangeMinScore(historyRange PriceHistoryRange) string {
	if historyRange == PriceHistoryRangeAll {
		return "-inf"
	}
	return strconv.FormatInt(rangeStartTime(historyRange).UnixMilli(), 10)
}

var _ Engine = (*RedisQueryEngine)(nil)
