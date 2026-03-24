package matching

import (
	"blinkpredict/banckend/internal/logging"
)

var bookLogger = logging.New("matcher-book")

// ==========================================
// 价格档位与订单簿结构
// ==========================================

// PriceLevel 价格档位（双向链表）
type PriceLevel struct {
	TotalVolume uint64
	Head        *MemoryOrder
	Tail        *MemoryOrder
}

// FixedArrayOrderBook 固定数组订单簿（零锁单线程状态机）
type FixedArrayOrderBook struct {
	MarketID         uint64
	CurrentTime      int64
	CloseTime        int64  // TODO: 从Redis/DB获取市场关闭时间，暂不使用
	IsActive         bool   // 熔断开关
	LastProcessedSeq uint64 // 幂等防线：防NATS重放

	Bids   [100]*PriceLevel        // 买单：索引1-99，0不使用
	Asks   [100]*PriceLevel        // 卖单：索引1-99，0不使用
	Orders map[uint64]*MemoryOrder // 快速查找索引

	BestBidPrice uint8
	BestAskPrice uint8
}

// NewFixedArrayOrderBook 创建订单簿
func NewFixedArrayOrderBook(marketID uint64) *FixedArrayOrderBook {
	return &FixedArrayOrderBook{
		MarketID:  marketID,
		CloseTime: 0, // TODO: 从Redis/DB获取，后期实现
		IsActive:  true,
		Orders:    make(map[uint64]*MemoryOrder),
	}
}

// RestoreOrder 将已持久化的活跃订单恢复回订单簿，不产生事件。
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

// ==========================================
// 主路由分支
// ==========================================

// ProcessCommand 处理命令的主入口
func (ob *FixedArrayOrderBook) ProcessCommand(cmd Command, batch *BatchEventPayload) {
	ob.CurrentTime = cmd.GetTimestamp()

	switch c := cmd.(type) {
	case *PlaceOrderCommand:
		ob.handlePlaceOrder(c, batch)
	case *TickCommand:
		ob.handleTick(c, batch)
	case *HaltMarketCommand:
		ob.handleHalt(c, batch)
	}
}

// ==========================================
// 订单撮合总控
// ==========================================

// handlePlaceOrder 处理下单命令
func (ob *FixedArrayOrderBook) handlePlaceOrder(cmd *PlaceOrderCommand, batch *BatchEventPayload) {
	// 【防线 1：静态时间墙】TODO: 后期启用CloseTime检查
	// if ob.CloseTime > 0 && ob.CurrentTime >= ob.CloseTime {
	// 	batch.AddStateEvent(cmd.OrderID, cmd.WalletAddress, StatusRejected, cmd.QtyLots, cmd.SpendAmount)
	// 	return
	// }

	// 【防线 2：动态熔断开关】
	if !ob.IsActive {
		batch.AddStateEvent(cmd.OrderID, cmd.WalletAddress, StatusRejected, cmd.QtyLots, cmd.SpendAmount)
		return
	}

	// 创建Taker订单
	taker := AcquireOrder()
	taker.InitFromCmd(cmd)

	// 根据方向执行撮合
	if taker.Side == SideBuy {
		ob.matchAsks(taker, batch)
	} else {
		ob.matchBids(taker, batch)
	}

	// 残骸处理
	if taker.IsFilled() {
		batch.AddStateEvent(taker.OrderID, taker.WalletAddress, StatusFilled, 0, 0)
		ReleaseOrder(taker)
	} else {
		if taker.OrderType == OrderTypeMarket {
			// 市价单剩余部分取消
			refund := taker.RemainingSpend
			batch.AddStateEvent(taker.OrderID, taker.WalletAddress, StatusCanceled, taker.RemainingQty, refund)
			ReleaseOrder(taker)
		} else {
			// 限价单加入订单簿
			ob.addOrderToBook(taker, batch)
		}
	}
}

// ==========================================
// 激进吃单循环 (Buy撞击Asks)
// ==========================================

// matchAsks 买单吃卖单
func (ob *FixedArrayOrderBook) matchAsks(taker *MemoryOrder, batch *BatchEventPayload) {
	targetPrice := ob.BestAskPrice

	// 如果没有初始最优价，从最低价开始搜索
	if targetPrice == 0 {
		// 找到第一个有订单的卖单档位
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

			// 【排雷：惰性删除过期单】
			if maker.ExpireTime > 0 && ob.CurrentTime >= maker.ExpireTime {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, batch, StatusExpired)
				maker = nextMaker
				continue
			}

			// 【STP：自成交防范拦截】
			if maker.WalletAddress == taker.WalletAddress {
				// 取消盘口老单，退回 Maker 资产
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, batch, StatusCanceled)
				maker = nextMaker
				continue
			}

			// 核心份额与粉尘计算
			var matchQty uint64
			if taker.OrderType == OrderTypeMarket && taker.Side == SideBuy {
				// 市价买入：qty_lots 的最小精度是 0.01 share，spend_amount 的最小精度是 $0.01，
				// 因此成本公式必须是 qty_lots * price_tick / 100。
				costToClear := costForLots(maker.RemainingQty, targetPrice)
				if taker.RemainingSpend >= costToClear {
					matchQty = maker.RemainingQty
					taker.RemainingSpend -= costToClear
				} else {
					matchQty = lotsForSpend(taker.RemainingSpend, targetPrice)
					if matchQty == 0 {
						// 致命粉尘，强行打断
						taker.RemainingSpend = 0
						break
					}
					taker.RemainingSpend -= costForLots(matchQty, targetPrice)
				}
			} else {
				// 限价单/市价卖出：基于QtyLots计算
				if taker.RemainingQty < maker.RemainingQty {
					matchQty = taker.RemainingQty
				} else {
					matchQty = maker.RemainingQty
				}
				taker.RemainingQty -= matchQty
			}

			// 扣减与发票开具
			maker.RemainingQty -= matchQty
			level.TotalVolume -= matchQty
			batch.AddTradeEvent(maker, taker, targetPrice, matchQty)

			if maker.RemainingQty == 0 {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideSell, batch, StatusFilled)
			} else {
				batch.AddStateEvent(maker.OrderID, maker.WalletAddress, StatusPartiallyFilled, maker.RemainingQty, 0)
			}

			maker = nextMaker
		}

		// 发送深度事件
		batch.AddDepthEvent(SideSell, targetPrice, level.TotalVolume)
		if level.Head == nil {
			targetPrice++
		}
	}

	ob.updateBestAsk(targetPrice)
}

func costForLots(qtyLots uint64, priceTick uint8) uint64 {
	return (qtyLots * uint64(priceTick)) / 100
}

func lotsForSpend(spendAmount uint64, priceTick uint8) uint64 {
	if priceTick == 0 {
		return 0
	}
	return (spendAmount * 100) / uint64(priceTick)
}

// ==========================================
// 对称吃单循环 (Sell撞击Bids)
// ==========================================

// matchBids 卖单吃买单（完整对称逻辑）
func (ob *FixedArrayOrderBook) matchBids(taker *MemoryOrder, batch *BatchEventPayload) {
	targetPrice := ob.BestBidPrice

	// 如果没有初始最优价，从最高价开始搜索
	if targetPrice == 0 {
		// 找到第一个有订单的买单档位
		for p := uint8(99); p >= 1; p-- {
			if ob.Bids[p] != nil && ob.Bids[p].TotalVolume > 0 {
				targetPrice = p
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

			// 【排雷：惰性删除过期单】
			if maker.ExpireTime > 0 && ob.CurrentTime >= maker.ExpireTime {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, batch, StatusExpired)
				maker = nextMaker
				continue
			}

			// 【STP：自成交防范拦截】
			if maker.WalletAddress == taker.WalletAddress {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, batch, StatusCanceled)
				maker = nextMaker
				continue
			}

			// 核心份额计算
			var matchQty uint64
			if taker.RemainingQty < maker.RemainingQty {
				matchQty = taker.RemainingQty
			} else {
				matchQty = maker.RemainingQty
			}
			taker.RemainingQty -= matchQty

			// 扣减与发票开具
			maker.RemainingQty -= matchQty
			level.TotalVolume -= matchQty
			batch.AddTradeEvent(maker, taker, targetPrice, matchQty)

			if maker.RemainingQty == 0 {
				ob.removeAndRecycleMaker(maker, level, targetPrice, SideBuy, batch, StatusFilled)
			} else {
				batch.AddStateEvent(maker.OrderID, maker.WalletAddress, StatusPartiallyFilled, maker.RemainingQty, 0)
			}

			maker = nextMaker
		}

		// 发送深度事件
		batch.AddDepthEvent(SideBuy, targetPrice, level.TotalVolume)
		if level.Head == nil {
			if targetPrice == 1 {
				break
			}
			targetPrice--
		}
	}

	ob.updateBestBid(targetPrice)
}

// ==========================================
// O(1) 链表操作
// ==========================================

// removeAndRecycleMaker O(1)从链表移除并回收订单
func (ob *FixedArrayOrderBook) removeAndRecycleMaker(maker *MemoryOrder, level *PriceLevel, price uint8, side uint8, batch *BatchEventPayload, status uint8) {
	// 从双向链表移除
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

	// 更新档位总量
	level.TotalVolume -= maker.RemainingQty
	delete(ob.Orders, maker.OrderID)

	// 生成状态事件
	batch.AddStateEvent(maker.OrderID, maker.WalletAddress, status, maker.RemainingQty, 0)

	// 回收内存
	ReleaseOrder(maker)
}

// addOrderToBook 添加订单到订单簿
func (ob *FixedArrayOrderBook) addOrderToBook(order *MemoryOrder, batch *BatchEventPayload) {
	var sideArr *[100]*PriceLevel
	if order.Side == SideBuy {
		sideArr = &ob.Bids
	} else {
		sideArr = &ob.Asks
	}

	// 获取或创建档位
	level := sideArr[order.PriceTick]
	if level == nil {
		level = &PriceLevel{}
		sideArr[order.PriceTick] = level
	}

	// 添加到链表尾部（FIFO）
	if level.Tail == nil {
		level.Head = order
		level.Tail = order
	} else {
		level.Tail.Next = order
		order.Prev = level.Tail
		level.Tail = order
	}

	// 更新总量
	level.TotalVolume += order.RemainingQty
	ob.Orders[order.OrderID] = order

	// 生成事件
	batch.AddStateEvent(order.OrderID, order.WalletAddress, StatusNew, order.RemainingQty, 0)
	batch.AddDepthEvent(order.Side, order.PriceTick, level.TotalVolume)

	// 更新最优价格游标
	if order.Side == SideBuy && order.PriceTick > ob.BestBidPrice {
		ob.BestBidPrice = order.PriceTick
	}
	if order.Side == SideSell && (ob.BestAskPrice == 0 || order.PriceTick < ob.BestAskPrice) {
		ob.BestAskPrice = order.PriceTick
	}
}

// ==========================================
// 最优价格游标更新
// ==========================================

// updateBestAsk 更新最优卖价
func (ob *FixedArrayOrderBook) updateBestAsk(startPrice uint8) {
	ob.BestAskPrice = 0
	for p := startPrice; p <= 99; p++ {
		if ob.Asks[p] != nil && ob.Asks[p].TotalVolume > 0 {
			ob.BestAskPrice = p
			break
		}
	}
}

// updateBestBid 更新最优买价
func (ob *FixedArrayOrderBook) updateBestBid(startPrice uint8) {
	ob.BestBidPrice = 0
	for p := startPrice; p >= 1; p-- {
		if ob.Bids[p] != nil && ob.Bids[p].TotalVolume > 0 {
			ob.BestBidPrice = p
			break
		}
	}
}

// ==========================================
// 心跳扫雷与熔断开关
// ==========================================

// handleTick 处理心跳命令（全盘扫描过期订单）
func (ob *FixedArrayOrderBook) handleTick(cmd *TickCommand, batch *BatchEventPayload) {
	ob.CurrentTime = cmd.Timestamp

	// 全盘扫描100个价格档位
	for p := uint8(1); p <= 99; p++ {
		ob.sweepLevelO1(ob.Bids[p], p, SideBuy, batch)
		ob.sweepLevelO1(ob.Asks[p], p, SideSell, batch)
	}

	// 更新最优价格游标
	ob.updateBestBid(99)
	ob.updateBestAsk(1)
}

// sweepLevelO1 扫描单个档位的过期订单（遇到第一个未过期的就截断）
func (ob *FixedArrayOrderBook) sweepLevelO1(level *PriceLevel, price uint8, side uint8, batch *BatchEventPayload) {
	if level == nil || level.Head == nil {
		return
	}

	curr := level.Head
	for curr != nil {
		next := curr.Next
		if curr.ExpireTime > 0 && ob.CurrentTime >= curr.ExpireTime {
			ob.removeAndRecycleMaker(curr, level, price, side, batch, StatusExpired)
			curr = next
		} else {
			// 截断：遇到第一个没过期的，直接闪人
			break
		}
	}

	if level.TotalVolume > 0 {
		batch.AddDepthEvent(side, price, level.TotalVolume)
	}
}

// handleHalt 处理熔断命令
func (ob *FixedArrayOrderBook) handleHalt(cmd *HaltMarketCommand, batch *BatchEventPayload) {
	if ob.IsActive {
		ob.IsActive = false
		bookLogger.Warnf("market %d halted", ob.MarketID)
		// TODO: 可选：遍历挂单全部撤销
	}
}
