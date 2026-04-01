package matching

import (
	"strconv"
	"time"
)

type BatchConfig struct {
	MaxFills  int
	MaxOrders int
	MaxBytes  int
	MaxAge    time.Duration
	IdleFlush time.Duration
	FlushTick time.Duration
}

func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		MaxFills:  64,
		MaxOrders: 96,
		MaxBytes:  262144,
		MaxAge:    40 * time.Millisecond,
		IdleFlush: 15 * time.Millisecond,
		FlushTick: 10 * time.Millisecond,
	}
}

type pendingBatch struct {
	event      MatchBatchEventV2
	orderIndex map[uint64]uint16
	fillIndex  uint32
	sizeBytes  int
	startedAt  time.Time
	lastAddAt  time.Time
	wrappers   []*CommandWrapper
	seqs       map[uint64]struct{}
}

func newPendingBatch(marketID uint64, marketPDA string, now time.Time) *pendingBatch {
	return &pendingBatch{
		event: MatchBatchEventV2{
			SchemaVersion: 1,
			MarketID:      marketID,
			MarketPDA:     marketPDA,
			ProducedAt:    now.Unix(),
			Orders:        make([]MatchedOrderV2, 0, 16),
			Fills:         make([]MatchFillV2, 0, 16),
			OrderUpdates:  make([]OrderUpdateV2, 0, 16),
			DepthUpdates:  make([]DepthUpdateV2, 0, 16),
		},
		orderIndex: make(map[uint64]uint16),
		startedAt:  now,
		lastAddAt:  now,
		seqs:       make(map[uint64]struct{}),
	}
}

func (b *pendingBatch) hasSeq(seq uint64) bool {
	if seq == 0 {
		return false
	}
	_, ok := b.seqs[seq]
	return ok
}

func (b *pendingBatch) includeWrapper(wrapper *CommandWrapper, now time.Time) {
	if wrapper == nil {
		return
	}
	b.lastAddAt = now
	if wrapper.SourceCmdSeq > 0 {
		if b.event.SourceCmdSeqMin == 0 || wrapper.SourceCmdSeq < b.event.SourceCmdSeqMin {
			b.event.SourceCmdSeqMin = wrapper.SourceCmdSeq
		}
		if wrapper.SourceCmdSeq > b.event.SourceCmdSeqMax {
			b.event.SourceCmdSeqMax = wrapper.SourceCmdSeq
		}
		b.seqs[wrapper.SourceCmdSeq] = struct{}{}
	}
	b.wrappers = append(b.wrappers, wrapper)
	if cmd, ok := wrapper.Cmd.(*PlaceOrderCommand); ok {
		if cmd.CommandID != "" {
			b.event.SourceCommandIDs = appendUniqueString(b.event.SourceCommandIDs, cmd.CommandID)
		}
		if cmd.TraceID != "" {
			b.event.TraceIDs = appendUniqueString(b.event.TraceIDs, cmd.TraceID)
		}
		if b.event.MarketPDA == "" {
			b.event.MarketPDA = cmd.MarketPDA
		}
	}
}

func (b *pendingBatch) ensureOrder(order *MemoryOrder, createdAt int64) uint16 {
	if createdAt == 0 {
		createdAt = order.Timestamp
	}
	if idx, ok := b.orderIndex[order.OrderID]; ok {
		return idx
	}
	idx := uint16(len(b.event.Orders))
	b.orderIndex[order.OrderID] = idx
	b.event.Orders = append(b.event.Orders, MatchedOrderV2{
		OrderIndex: idx,
		OrderID:    order.OrderID,
		Execution:  executionSnapshotFromOrder(order),
		Settlement: SettlementPayloadV2{
			IntentBytesHex: order.IntentBytesHex,
			Signature:      order.Signature,
		},
		CreatedAt: createdAt,
	})
	b.sizeBytes += 256
	return idx
}

func (b *pendingBatch) ensureOrderFromCommand(cmd *PlaceOrderCommand) uint16 {
	if idx, ok := b.orderIndex[cmd.OrderID]; ok {
		return idx
	}
	idx := uint16(len(b.event.Orders))
	b.orderIndex[cmd.OrderID] = idx
	b.event.Orders = append(b.event.Orders, MatchedOrderV2{
		OrderIndex: idx,
		OrderID:    cmd.OrderID,
		Execution:  executionSnapshotFromCommand(cmd),
		Settlement: SettlementPayloadV2{
			IntentBytesHex: cmd.IntentBytesHex,
			Signature:      cmd.Signature,
		},
		CreatedAt: cmd.Timestamp,
	})
	b.sizeBytes += 256
	return idx
}

func (b *pendingBatch) addFill(maker, taker *MemoryOrder, price uint8, qty uint64) {
	makerIdx := b.ensureOrder(maker, 0)
	takerIdx := b.ensureOrder(taker, 0)
	b.event.Fills = append(b.event.Fills, MatchFillV2{
		FillIndex:        b.fillIndex,
		MakerOrderIndex:  makerIdx,
		TakerOrderIndex:  takerIdx,
		FillAmount:       qty,
		FillPrice:        uint64(price),
		MatchType:        classifyMatchType(maker, taker),
		NotionalUnits:    actualUnitsForOrder(taker, qty, price),
		TakerFeeUnits:    0,
		CreatorFeeUnits:  0,
		PlatformFeeUnits: 0,
	})
	b.fillIndex++
	b.sizeBytes += 128
}

func (b *pendingBatch) addOrderUpdateForCommand(cmd *PlaceOrderCommand, status string, remainingQty, remainingSpend, refund uint64, reason string) {
	idx := b.ensureOrderFromCommand(cmd)
	b.event.OrderUpdates = append(b.event.OrderUpdates, OrderUpdateV2{
		OrderIndex:           idx,
		Status:               status,
		RemainingQtyLots:     remainingQty,
		RemainingSpendAmount: remainingSpend,
		RefundAmount:         refund,
		ReasonCode:           reason,
	})
	b.sizeBytes += 64
}

func (b *pendingBatch) addOrderUpdateForOrder(order *MemoryOrder, status string, remainingQty, remainingSpend, refund uint64, reason string) {
	idx := b.ensureOrder(order, 0)
	b.event.OrderUpdates = append(b.event.OrderUpdates, OrderUpdateV2{
		OrderIndex:           idx,
		Status:               status,
		RemainingQtyLots:     remainingQty,
		RemainingSpendAmount: remainingSpend,
		RefundAmount:         refund,
		ReasonCode:           reason,
	})
	b.sizeBytes += 64
}

func (b *pendingBatch) addDepthUpdate(side uint8, price uint8, volume uint64) {
	b.event.DepthUpdates = append(b.event.DepthUpdates, DepthUpdateV2{
		Side:        sideLabelForDepth(side),
		PriceTick:   price,
		TotalVolume: volume,
	})
	b.sizeBytes += 32
}

func (b *pendingBatch) hasPayload() bool {
	return len(b.event.Fills) > 0 || len(b.event.OrderUpdates) > 0 || len(b.event.DepthUpdates) > 0
}

func (b *pendingBatch) shouldFlush(now time.Time, cfg BatchConfig, forceIdle bool) bool {
	if b == nil || !b.hasPayload() {
		return false
	}
	if cfg.MaxFills > 0 && len(b.event.Fills) >= cfg.MaxFills {
		return true
	}
	if cfg.MaxOrders > 0 && len(b.event.Orders) >= cfg.MaxOrders {
		return true
	}
	if cfg.MaxBytes > 0 && b.sizeBytes >= cfg.MaxBytes {
		return true
	}
	if cfg.MaxAge > 0 && now.Sub(b.startedAt) >= cfg.MaxAge {
		return true
	}
	if forceIdle && cfg.IdleFlush > 0 && now.Sub(b.lastAddAt) >= cfg.IdleFlush {
		return true
	}
	return false
}

func (b *pendingBatch) freeze(now time.Time) MatchBatchEventV2 {
	event := b.event
	event.EventID = generateBatchEventID(event.MarketID, event.SourceCmdSeqMax, now)
	event.ProducedAt = now.Unix()
	return event
}

func executionSnapshotFromCommand(cmd *PlaceOrderCommand) ExecutionSnapshotV2 {
	return ExecutionSnapshotV2{
		OrderID:             cmd.OrderID,
		WalletAddress:       cmd.WalletAddress,
		OriginalAction:      sideString(cmd.OriginalAction),
		OriginalOutcome:     outcomeString(cmd.OriginalOutcome),
		OriginalPriceTick:   cmd.OriginalPriceTick,
		OrderType:           orderTypeString(cmd.OrderType),
		NormalizedSide:      sideString(cmd.Side),
		NormalizedPriceTick: cmd.PriceTick,
		QtyLots:             cmd.QtyLots,
		SpendAmount:         cmd.SpendAmount,
		ExpireTime:          cmd.ExpireTime,
		Nonce:               cmd.Nonce,
	}
}

func executionSnapshotFromOrder(order *MemoryOrder) ExecutionSnapshotV2 {
	return ExecutionSnapshotV2{
		OrderID:             order.OrderID,
		WalletAddress:       order.WalletAddress,
		OriginalAction:      sideString(order.OriginalAction),
		OriginalOutcome:     outcomeString(order.OriginalOutcome),
		OriginalPriceTick:   order.OriginalPriceTick,
		OrderType:           orderTypeString(order.OrderType),
		NormalizedSide:      sideString(order.Side),
		NormalizedPriceTick: order.PriceTick,
		QtyLots:             order.RemainingQty,
		SpendAmount:         order.RemainingSpend,
		ExpireTime:          order.ExpireTime,
		Nonce:               order.Nonce,
	}
}

func generateBatchEventID(marketID uint64, seq uint64, now time.Time) string {
	return "match-batch-" + strconv.FormatUint(marketID, 10) + "-" + strconv.FormatUint(seq, 10) + "-" + strconv.FormatInt(now.UnixMilli(), 10)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func sideLabelForDepth(side uint8) string {
	if side == SideSell {
		return "ask"
	}
	return "bid"
}

func sideString(side uint8) string {
	if side == SideSell {
		return "sell"
	}
	return "buy"
}

func outcomeString(outcome uint8) string {
	if outcome == 1 {
		return "no"
	}
	return "yes"
}

func orderTypeString(orderType uint8) string {
	if orderType == OrderTypeMarket {
		return "market"
	}
	return "limit"
}

func classifyMatchType(a, b *MemoryOrder) string {
	if a.OriginalAction == SideBuy && b.OriginalAction == SideBuy {
		return MatchTypeMatchMint
	}
	if a.OriginalAction == SideSell && b.OriginalAction == SideSell {
		return MatchTypeMergeBurn
	}
	return MatchTypeTransfer
}
