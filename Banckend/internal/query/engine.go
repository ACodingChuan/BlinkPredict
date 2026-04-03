package query

import (
	"context"

	"blinkpredict/banckend/internal/matching"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Engine interface {
	GetOrderbook(context.Context, uint64) OrderbookSnapshot
	GetTrades(context.Context, uint64) []Trade
	GetOpenOrders(context.Context, string, uint64) []OpenOrder
	GetPriceHistory(context.Context, uint64, PriceHistoryRange) PriceHistory
}

type (
	OrderbookSnapshot = matching.OrderbookSnapshot
	OrderLevel        = matching.OrderLevel
	Trade             = matching.Trade
	OpenOrder         = matching.OpenOrder
	PriceHistoryRange = matching.PriceHistoryRange
	PricePoint        = matching.PricePoint
	PriceHistory      = matching.PriceHistory
)

const (
	PriceHistoryRange1H  = matching.PriceHistoryRange1H
	PriceHistoryRange6H  = matching.PriceHistoryRange6H
	PriceHistoryRange1D  = matching.PriceHistoryRange1D
	PriceHistoryRange1W  = matching.PriceHistoryRange1W
	PriceHistoryRange1M  = matching.PriceHistoryRange1M
	PriceHistoryRangeAll = matching.PriceHistoryRangeAll
)

func NewDisabledEngine() Engine {
	return matching.NewDisabledEngine()
}

func NewRedisEngine(rdb *redis.Client, pool *pgxpool.Pool) Engine {
	return matching.NewRedisQueryEngine(rdb, pool, matching.NewDisabledEngine())
}
