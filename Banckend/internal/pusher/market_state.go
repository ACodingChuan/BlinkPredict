package pusher

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"
)

const (
	publicRecentTradesLimit = 100
	publicPriceHistoryLimit = 4096
	maxPriceTick            = 100
)

type MarketDataSource interface {
	GetOrderbook(context.Context, uint64) matching.OrderbookSnapshot
	GetTrades(context.Context, uint64) []matching.Trade
	GetPriceHistory(context.Context, uint64, matching.PriceHistoryRange) matching.PriceHistory
}

type marketState struct {
	loadMu sync.Mutex
	mu     sync.RWMutex

	loaded       bool
	seq          uint64
	bids         [maxPriceTick + 1]uint64
	asks         [maxPriceTick + 1]uint64
	trades       []protocol.WSMarketTrade
	priceHistory []protocol.WSPricePoint
}

func (s *marketState) ensureLoaded(ctx context.Context, marketID uint64, source MarketDataSource) {
	if source == nil {
		s.mu.Lock()
		if !s.loaded {
			s.loaded = true
		}
		s.mu.Unlock()
		return
	}

	s.mu.RLock()
	if s.loaded {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	s.loadMu.Lock()
	defer s.loadMu.Unlock()

	s.mu.RLock()
	if s.loaded {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	orderbook := source.GetOrderbook(ctx, marketID)
	trades := source.GetTrades(ctx, marketID)
	priceHistory := source.GetPriceHistory(ctx, marketID, matching.PriceHistoryRangeAll)

	var bids [maxPriceTick + 1]uint64
	var asks [maxPriceTick + 1]uint64
	for _, level := range orderbook.Bids {
		price, err := strconv.ParseUint(level.Price, 10, 64)
		if err != nil || price > maxPriceTick {
			continue
		}
		vol, err := strconv.ParseUint(level.TotalVolume, 10, 64)
		if err != nil {
			continue
		}
		bids[price] = vol
	}
	for _, level := range orderbook.Asks {
		price, err := strconv.ParseUint(level.Price, 10, 64)
		if err != nil || price > maxPriceTick {
			continue
		}
		vol, err := strconv.ParseUint(level.TotalVolume, 10, 64)
		if err != nil {
			continue
		}
		asks[price] = vol
	}

	tradeItems := make([]protocol.WSMarketTrade, 0, minInt(len(trades), publicRecentTradesLimit))
	for _, trade := range trades {
		tradeItems = append(tradeItems, protocol.WSMarketTrade{
			TradeID:    trade.ID,
			PriceTick:  trade.Price,
			FillAmount: trade.Quantity,
			ExecutedAt: trade.ExecutedAt,
		})
		if len(tradeItems) >= publicRecentTradesLimit {
			break
		}
	}

	pricePoints := priceHistory.Points
	if len(pricePoints) > publicPriceHistoryLimit {
		pricePoints = pricePoints[len(pricePoints)-publicPriceHistoryLimit:]
	}
	historyItems := make([]protocol.WSPricePoint, 0, len(pricePoints))
	for _, point := range pricePoints {
		historyItems = append(historyItems, protocol.WSPricePoint{
			Timestamp: point.Timestamp,
			Price:     point.Price,
			Quantity:  point.Quantity,
		})
	}

	s.mu.Lock()
	s.bids = bids
	s.asks = asks
	s.trades = tradeItems
	s.priceHistory = historyItems
	s.loaded = true
	s.mu.Unlock()
}

func (s *marketState) snapshotPayload() (uint64, protocol.WSMarketSnapshotPayload) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	payload := protocol.WSMarketSnapshotPayload{
		MatchingEnabled: true,
		Orderbook: protocol.WSOrderbookPayload{
			Bids: make([]protocol.WSOrderbookLevel, 0),
			Asks: make([]protocol.WSOrderbookLevel, 0),
		},
		Trades:       append([]protocol.WSMarketTrade(nil), s.trades...),
		PriceHistory: append([]protocol.WSPricePoint(nil), s.priceHistory...),
	}

	for price := maxPriceTick; price >= 0; price-- {
		vol := s.bids[price]
		if vol == 0 {
			continue
		}
		payload.Orderbook.Bids = append(payload.Orderbook.Bids, protocol.WSOrderbookLevel{
			Price:       strconv.Itoa(price),
			TotalVolume: strconv.FormatUint(vol, 10),
		})
		if payload.Orderbook.BestBidPrice == "" {
			payload.Orderbook.BestBidPrice = strconv.Itoa(price)
		}
	}
	for price := 0; price <= maxPriceTick; price++ {
		vol := s.asks[price]
		if vol == 0 {
			continue
		}
		payload.Orderbook.Asks = append(payload.Orderbook.Asks, protocol.WSOrderbookLevel{
			Price:       strconv.Itoa(price),
			TotalVolume: strconv.FormatUint(vol, 10),
		})
		if payload.Orderbook.BestAskPrice == "" {
			payload.Orderbook.BestAskPrice = strconv.Itoa(price)
		}
	}

	return s.seq, payload
}

func (s *marketState) applyMatchEvent(event matching.MatchBatchEvent) protocol.WSMarketDelta {
	ts := time.Unix(event.ProducedAt, 0).UTC().Format(time.RFC3339)
	depthLevels := compressDepthUpdates(event.DepthUpdates)
	trades := make([]protocol.WSMarketTrade, 0, len(event.Fills))
	pricePoints := make([]protocol.WSPricePoint, 0, len(event.Fills))

	s.mu.Lock()
	defer s.mu.Unlock()

	s.loaded = true
	s.seq++
	for _, level := range depthLevels {
		if level.PriceTick > maxPriceTick {
			continue
		}
		if level.Side == "ask" {
			s.asks[level.PriceTick] = level.TotalVolume
			continue
		}
		s.bids[level.PriceTick] = level.TotalVolume
	}

	for _, fill := range event.Fills {
		trade := protocol.WSMarketTrade{
			TradeID:    tradeIDForFill(event.EventID, fill),
			PriceTick:  strconv.FormatUint(fill.FillPrice, 10),
			FillAmount: strconv.FormatUint(fill.FillAmount, 10),
			ExecutedAt: ts,
		}
		trades = append(trades, trade)
		pricePoints = append(pricePoints, protocol.WSPricePoint{
			Timestamp: ts,
			Price:     trade.PriceTick,
			Quantity:  trade.FillAmount,
		})
	}

	if len(trades) > 0 {
		nextTrades := make([]protocol.WSMarketTrade, 0, minInt(len(trades)+len(s.trades), publicRecentTradesLimit))
		nextTrades = append(nextTrades, trades...)
		for _, existing := range s.trades {
			if len(nextTrades) >= publicRecentTradesLimit {
				break
			}
			if containsTrade(nextTrades, existing.TradeID) {
				continue
			}
			nextTrades = append(nextTrades, existing)
		}
		s.trades = nextTrades

		s.priceHistory = append(s.priceHistory, pricePoints...)
		if len(s.priceHistory) > publicPriceHistoryLimit {
			s.priceHistory = append([]protocol.WSPricePoint(nil), s.priceHistory[len(s.priceHistory)-publicPriceHistoryLimit:]...)
		}
	}

	return protocol.WSMarketDelta{
		Type:     protocol.WSTypeMarketDelta,
		MarketID: strconv.FormatUint(event.MarketID, 10),
		Seq:      strconv.FormatUint(s.seq, 10),
		Ts:       ts,
		Payload: protocol.WSMarketDeltaPayload{
			DepthLevels: depthLevels,
			Trades:      trades,
			PricePoints: pricePoints,
		},
	}
}

func buildSnapshotMessage(marketID uint64, seq uint64, payload protocol.WSMarketSnapshotPayload) ([]byte, error) {
	body, err := json.Marshal(protocol.WSMarketSnapshot{
		Type:     protocol.WSTypeMarketSnapshot,
		MarketID: strconv.FormatUint(marketID, 10),
		Seq:      strconv.FormatUint(seq, 10),
		Ts:       time.Now().UTC().Format(time.RFC3339),
		Payload:  payload,
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

func containsTrade(trades []protocol.WSMarketTrade, tradeID string) bool {
	for _, trade := range trades {
		if trade.TradeID == tradeID {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
