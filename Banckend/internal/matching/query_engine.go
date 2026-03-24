package matching

import (
	"context"
	"fmt"
	"strconv"
)

// QueryEngine 查询引擎（实现Engine接口）
type QueryEngine struct {
	manager *MarketManager
}

// NewQueryEngine 创建查询引擎
func NewQueryEngine(manager *MarketManager) *QueryEngine {
	return &QueryEngine{manager: manager}
}

// GetOrderbook 获取订单簿快照
func (q *QueryEngine) GetOrderbook(ctx context.Context, marketID uint64) OrderbookSnapshot {
	actor, exists := q.manager.actors[marketID]
	if !exists {
		return OrderbookSnapshot{
			Bids:            []OrderLevel{},
			Asks:            []OrderLevel{},
			MatchingEnabled: false,
		}
	}

	book := actor.Book
	snapshot := OrderbookSnapshot{
		Bids:            make([]OrderLevel, 0),
		Asks:            make([]OrderLevel, 0),
		MatchingEnabled: book.IsActive,
	}

	// 收集买单
	for p := book.BestBidPrice; p >= 1; p-- {
		if book.Bids[p] != nil && book.Bids[p].TotalVolume > 0 {
			snapshot.Bids = append(snapshot.Bids, OrderLevel{
				Price:       strconv.FormatUint(uint64(p), 10),
				TotalVolume: strconv.FormatUint(book.Bids[p].TotalVolume, 10),
			})
		}
	}

	// 收集卖单
	for p := book.BestAskPrice; p <= 99; p++ {
		if book.Asks[p] != nil && book.Asks[p].TotalVolume > 0 {
			snapshot.Asks = append(snapshot.Asks, OrderLevel{
				Price:       strconv.FormatUint(uint64(p), 10),
				TotalVolume: strconv.FormatUint(book.Asks[p].TotalVolume, 10),
			})
		}
	}

	if book.BestBidPrice > 0 {
		snapshot.BestBidPrice = strconv.FormatUint(uint64(book.BestBidPrice), 10)
	}
	if book.BestAskPrice > 0 {
		snapshot.BestAskPrice = strconv.FormatUint(uint64(book.BestAskPrice), 10)
	}

	return snapshot
}

// GetTrades 获取交易记录（TODO: 从持久化存储读取）
func (q *QueryEngine) GetTrades(ctx context.Context, marketID uint64) []Trade {
	// TODO: 从PostgreSQL读取交易历史
	return []Trade{}
}

// GetOpenOrders 获取用户开仓订单
func (q *QueryEngine) GetOpenOrders(ctx context.Context, walletAddress string, marketID uint64) []OpenOrder {
	actor, exists := q.manager.actors[marketID]
	if !exists {
		return []OpenOrder{}
	}

	orders := make([]OpenOrder, 0)
	for _, order := range actor.Book.Orders {
		if order.WalletAddress == walletAddress {
			orders = append(orders, OpenOrder{
				ID: fmt.Sprintf("%d", order.OrderID),
			})
		}
	}

	return orders
}

func (q *QueryEngine) GetPriceHistory(ctx context.Context, marketID uint64, historyRange PriceHistoryRange) PriceHistory {
	return PriceHistory{
		Range:  historyRange,
		Points: []PricePoint{},
	}
}
