package matching

import (
	"blinkpredict/banckend/internal/logging"
)

var bookLogger = logging.New("matcher-book")

type PriceLevel struct {
	TotalVolume uint64
	Head        *MemoryOrder
	Tail        *MemoryOrder
}

type FixedArrayOrderBook struct {
	MarketID         uint64
	CurrentTime      int64
	CloseTime        int64
	IsActive         bool
	LastProcessedSeq uint64

	Bids   [100]*PriceLevel
	Asks   [100]*PriceLevel
	Orders map[uint64]*MemoryOrder

	BestBidPrice uint8
	BestAskPrice uint8
}

func NewFixedArrayOrderBook(marketID uint64) *FixedArrayOrderBook {
	return &FixedArrayOrderBook{
		MarketID: marketID,
		IsActive: true,
		Orders:   make(map[uint64]*MemoryOrder),
	}
}

func (ob *FixedArrayOrderBook) RestoreOrder(order *MemoryOrder) {
	var sideArr *[100]*PriceLevel
	if order.Side == SideBuy {
		sideArr = &ob.Bids
	} else {
		sideArr = &ob.Asks
	}
	level := sideArr[order.PriceTick]
	if level == nil {
		level = &PriceLevel{}
		sideArr[order.PriceTick] = level
	}
	if level.Tail == nil {
		level.Head = order
		level.Tail = order
	} else {
		level.Tail.Next = order
		order.Prev = level.Tail
		level.Tail = order
	}
	level.TotalVolume += order.RemainingQty
	ob.Orders[order.OrderID] = order
	if order.Side == SideBuy && order.PriceTick > ob.BestBidPrice {
		ob.BestBidPrice = order.PriceTick
	}
	if order.Side == SideSell && (ob.BestAskPrice == 0 || order.PriceTick < ob.BestAskPrice) {
		ob.BestAskPrice = order.PriceTick
	}
}

func (ob *FixedArrayOrderBook) ProcessCommand(cmd Command, wallet *SharedWalletManager, batch *pendingBatch) {
	ob.CurrentTime = cmd.GetTimestamp()
	switch c := cmd.(type) {
	case *PlaceOrderCommand:
		ob.handlePlaceOrder(c, wallet, batch)
	case *TickCommand:
		ob.handleTick(c, wallet, batch)
	case *HaltMarketCommand:
		ob.handleHalt(c, batch)
	}
}

func (ob *FixedArrayOrderBook) handlePlaceOrder(cmd *PlaceOrderCommand, wallet *SharedWalletManager, batch *pendingBatch) {
	if !ob.IsActive {
		batch.addOrderUpdateForCommand(cmd, "rejected", cmd.QtyLots, cmd.SpendAmount, 0, "market_halted")
		return
	}
	if err := wallet.ReserveOrder(cmd); err != nil {
		batch.addOrderUpdateForCommand(cmd, "rejected", cmd.QtyLots, cmd.SpendAmount, 0, "insufficient_balance")
		return
	}

	taker := AcquireOrder()
	taker.InitFromCmd(cmd)

	if taker.Side == SideBuy {
		ob.matchAsks(taker, wallet, batch)
	} else {
		ob.matchBids(taker, wallet, batch)
	}

	if taker.IsFilled() {
		batch.addOrderUpdateForOrder(taker, "filled", 0, 0, 0, "")
		ReleaseOrder(taker)
		return
	}

	if taker.OrderType == OrderTypeMarket {
		refund := taker.RemainingSpend
		wallet.ReleaseOrder(taker, refund)
		batch.addOrderUpdateForOrder(taker, "canceled", taker.RemainingQty, taker.RemainingSpend, refund, "market_unfilled")
		ReleaseOrder(taker)
		return
	}

	ob.addOrderToBook(taker, batch)
}

func (ob *FixedArrayOrderBook) matchAsks(taker *MemoryOrder, wallet *SharedWalletManager, batch *pendingBatch) {
	targetPrice := ob.BestAskPrice
	if targetPrice == 0 {
		for p := uint8(1); p <= 99; p++ {
			if ob.Asks[p] != nil && ob.Asks[p].TotalVolume > 0 {
				targetPrice = p
				break
			}
		}
	}

	for !taker.IsFilled() && targetPrice != 0 && targetPrice <= taker.PriceTick && targetPrice <= 99 {
		level := ob.Asks[targetPrice]
		if level == nil || level.Head == nil {
			targetPrice++
			continue
		}

		maker := level.Head
		for maker != nil && !taker.IsFilled() {
			nextMaker := maker.Next
			if maker.ExpireTime > 0 && ob.CurrentTime >= maker.ExpireTime {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, wallet, batch, "expired", "expired")
				maker = nextMaker
				continue
			}
			if maker.WalletAddress == taker.WalletAddress {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, wallet, batch, "canceled", "self_trade_prevention")
				maker = nextMaker
				continue
			}
			var matchQty uint64
			if taker.OrderType == OrderTypeMarket && taker.OriginalAction == SideBuy {
				costToClear := spendForOrderLots(taker, maker.RemainingQty, targetPrice)
				if taker.RemainingSpend >= costToClear {
					matchQty = maker.RemainingQty
					taker.RemainingSpend -= costToClear
				} else {
					matchQty = lotsForOrderSpend(taker, taker.RemainingSpend, targetPrice)
					if matchQty == 0 {
						taker.RemainingSpend = 0
						break
					}
					taker.RemainingSpend -= spendForOrderLots(taker, matchQty, targetPrice)
				}
			} else {
				if taker.RemainingQty < maker.RemainingQty {
					matchQty = taker.RemainingQty
				} else {
					matchQty = maker.RemainingQty
				}
				taker.RemainingQty -= matchQty
			}

			maker.RemainingQty -= matchQty
			level.TotalVolume -= matchQty
			wallet.ApplyLocalFill(maker, taker, matchQty, targetPrice, classifyMatchType(maker, taker))
			batch.addFill(maker, taker, targetPrice, matchQty)

			if maker.RemainingQty == 0 {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, wallet, batch, "filled", "")
			} else {
				batch.addOrderUpdateForOrder(maker, "partially_filled", maker.RemainingQty, maker.RemainingSpend, 0, "")
			}
			maker = nextMaker
		}
		batch.addDepthUpdate(SideSell, targetPrice, level.TotalVolume)
		if level.Head == nil {
			targetPrice++
		}
	}
	ob.updateBestAsk(targetPrice)
}

func costForLots(qtyLots uint64, priceTick uint8) uint64 {
	return ceilMulDiv(qtyLots, uint64(priceTick), 100)
}

func lotsForSpend(spendAmount uint64, priceTick uint8) uint64 {
	if priceTick == 0 {
		return 0
	}
	return (spendAmount * 100) / uint64(priceTick)
}

func (ob *FixedArrayOrderBook) matchBids(taker *MemoryOrder, wallet *SharedWalletManager, batch *pendingBatch) {
	targetPrice := ob.BestBidPrice
	if targetPrice == 0 {
		for p := uint8(99); p >= 1; p-- {
			if ob.Bids[p] != nil && ob.Bids[p].TotalVolume > 0 {
				targetPrice = p
				break
			}
			if p == 1 {
				break
			}
		}
	}

	for !taker.IsFilled() && targetPrice != 0 && targetPrice >= taker.PriceTick && targetPrice >= 1 {
		level := ob.Bids[targetPrice]
		if level == nil || level.Head == nil {
			if targetPrice == 1 {
				break
			}
			targetPrice--
			continue
		}
		maker := level.Head
		for maker != nil && !taker.IsFilled() {
			nextMaker := maker.Next
			if maker.ExpireTime > 0 && ob.CurrentTime >= maker.ExpireTime {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, wallet, batch, "expired", "expired")
				maker = nextMaker
				continue
			}
			if maker.WalletAddress == taker.WalletAddress {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, wallet, batch, "canceled", "self_trade_prevention")
				maker = nextMaker
				continue
			}
			var matchQty uint64
			if taker.OrderType == OrderTypeMarket && taker.OriginalAction == SideBuy {
				costToClear := spendForOrderLots(taker, maker.RemainingQty, targetPrice)
				if taker.RemainingSpend >= costToClear {
					matchQty = maker.RemainingQty
					taker.RemainingSpend -= costToClear
				} else {
					matchQty = lotsForOrderSpend(taker, taker.RemainingSpend, targetPrice)
					if matchQty == 0 {
						taker.RemainingSpend = 0
						break
					}
					taker.RemainingSpend -= spendForOrderLots(taker, matchQty, targetPrice)
				}
			} else {
				if taker.RemainingQty < maker.RemainingQty {
					matchQty = taker.RemainingQty
				} else {
					matchQty = maker.RemainingQty
				}
				taker.RemainingQty -= matchQty
			}
			maker.RemainingQty -= matchQty
			level.TotalVolume -= matchQty
			wallet.ApplyLocalFill(maker, taker, matchQty, targetPrice, classifyMatchType(maker, taker))
			batch.addFill(maker, taker, targetPrice, matchQty)

			if maker.RemainingQty == 0 {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, wallet, batch, "filled", "")
			} else {
				batch.addOrderUpdateForOrder(maker, "partially_filled", maker.RemainingQty, maker.RemainingSpend, 0, "")
			}
			maker = nextMaker
		}
		batch.addDepthUpdate(SideBuy, targetPrice, level.TotalVolume)
		if level.Head == nil {
			if targetPrice == 1 {
				break
			}
			targetPrice--
		}
	}
	ob.updateBestBid(targetPrice)
}

func spendForOrderLots(order *MemoryOrder, qtyLots uint64, normalizedPrice uint8) uint64 {
	return actualUnitsForOrder(order, qtyLots, normalizedPrice)
}

func lotsForOrderSpend(order *MemoryOrder, spendAmount uint64, normalizedPrice uint8) uint64 {
	effectivePrice := normalizedPrice
	if order.OriginalOutcome == 1 {
		effectivePrice = 100 - normalizedPrice
	}
	return lotsForSpend(spendAmount, effectivePrice)
}

func (ob *FixedArrayOrderBook) removeAndRecycleMaker(maker *MemoryOrder, level *PriceLevel, price uint8, side uint8, wallet *SharedWalletManager, batch *pendingBatch, status, reason string) {
	if maker.Prev != nil {
		maker.Prev.Next = maker.Next
	} else {
		level.Head = maker.Next
	}
	if maker.Next != nil {
		maker.Next.Prev = maker.Prev
	} else {
		level.Tail = maker.Prev
	}
	level.TotalVolume -= maker.RemainingQty
	delete(ob.Orders, maker.OrderID)
	if status == "canceled" || status == "expired" || status == "rejected" {
		wallet.ReleaseOrder(maker, 0)
	}
	batch.addOrderUpdateForOrder(maker, status, maker.RemainingQty, maker.RemainingSpend, 0, reason)
	ReleaseOrder(maker)
}

func (ob *FixedArrayOrderBook) addOrderToBook(order *MemoryOrder, batch *pendingBatch) {
	var sideArr *[100]*PriceLevel
	if order.Side == SideBuy {
		sideArr = &ob.Bids
	} else {
		sideArr = &ob.Asks
	}
	level := sideArr[order.PriceTick]
	if level == nil {
		level = &PriceLevel{}
		sideArr[order.PriceTick] = level
	}
	if level.Tail == nil {
		level.Head = order
		level.Tail = order
	} else {
		level.Tail.Next = order
		order.Prev = level.Tail
		level.Tail = order
	}
	level.TotalVolume += order.RemainingQty
	ob.Orders[order.OrderID] = order
	batch.addOrderUpdateForOrder(order, "new", order.RemainingQty, order.RemainingSpend, 0, "")
	batch.addDepthUpdate(order.Side, order.PriceTick, level.TotalVolume)
	if order.Side == SideBuy && order.PriceTick > ob.BestBidPrice {
		ob.BestBidPrice = order.PriceTick
	}
	if order.Side == SideSell && (ob.BestAskPrice == 0 || order.PriceTick < ob.BestAskPrice) {
		ob.BestAskPrice = order.PriceTick
	}
}

func (ob *FixedArrayOrderBook) updateBestAsk(startPrice uint8) {
	ob.BestAskPrice = 0
	for p := startPrice; p <= 99; p++ {
		if ob.Asks[p] != nil && ob.Asks[p].TotalVolume > 0 {
			ob.BestAskPrice = p
			break
		}
		if p == 99 {
			break
		}
	}
}

func (ob *FixedArrayOrderBook) updateBestBid(startPrice uint8) {
	ob.BestBidPrice = 0
	for p := startPrice; p >= 1; p-- {
		if ob.Bids[p] != nil && ob.Bids[p].TotalVolume > 0 {
			ob.BestBidPrice = p
			break
		}
		if p == 1 {
			break
		}
	}
}

func (ob *FixedArrayOrderBook) handleTick(cmd *TickCommand, wallet *SharedWalletManager, batch *pendingBatch) {
	ob.CurrentTime = cmd.Timestamp
	changed := false
	for p := uint8(1); p <= 99; p++ {
		if ob.sweepLevelO1(ob.Bids[p], p, SideBuy, wallet, batch) {
			changed = true
		}
		if ob.sweepLevelO1(ob.Asks[p], p, SideSell, wallet, batch) {
			changed = true
		}
	}
	if changed {
		ob.updateBestBid(99)
		ob.updateBestAsk(1)
	}
}

func (ob *FixedArrayOrderBook) sweepLevelO1(level *PriceLevel, price uint8, side uint8, wallet *SharedWalletManager, batch *pendingBatch) bool {
	if level == nil || level.Head == nil {
		return false
	}
	removed := false
	curr := level.Head
	for curr != nil {
		next := curr.Next
		if curr.ExpireTime > 0 && ob.CurrentTime >= curr.ExpireTime {
			ob.removeAndRecycleMaker(curr, level, price, side, wallet, batch, "expired", "expired")
			removed = true
			curr = next
		} else {
			break
		}
	}
	if removed {
		batch.addDepthUpdate(side, price, level.TotalVolume)
	}
	return removed
}

func (ob *FixedArrayOrderBook) handleHalt(cmd *HaltMarketCommand, _ *pendingBatch) {
	if ob.IsActive {
		ob.IsActive = false
		bookLogger.Warnf("market %d halted at %d", ob.MarketID, cmd.Timestamp)
	}
}
