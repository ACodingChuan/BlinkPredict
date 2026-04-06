package matching

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

type BatchConfig struct {
	MaxFillsHot  int
	MaxFillsCold int
	MaxOrders    int
	MaxBytes     int
	MaxAge       time.Duration
	IdleFlush    time.Duration
	FlushTick    time.Duration
}

func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		MaxFillsHot:  64,
		MaxFillsCold: 16,
		MaxOrders:    96,
		MaxBytes:     262144,
		MaxAge:       40 * time.Millisecond,
		IdleFlush:    15 * time.Millisecond,
		FlushTick:    10 * time.Millisecond,
	}
}

type pendingBatch struct {
	event      MatchBatchEvent
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
		event: MatchBatchEvent{
			SchemaVersion: 1,
			MarketID:      marketID,
			MarketPDA:     marketPDA,
			ProducedAt:    now.Unix(),
			Orders:        make([]MatchedOrder, 0, 16),
			Fills:         make([]MatchFill, 0, 16),
			OrderUpdates:  make([]OrderUpdate, 0, 16),
			DepthUpdates:  make([]DepthUpdate, 0, 16),
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
	b.event.Orders = append(b.event.Orders, MatchedOrder{
		OrderIndex: idx,
		OrderID:    order.OrderID,
		Execution:  executionSnapshotFromOrder(order),
		Settlement: SettlementPayload{
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
	b.event.Orders = append(b.event.Orders, MatchedOrder{
		OrderIndex: idx,
		OrderID:    cmd.OrderID,
		Execution:  executionSnapshotFromCommand(cmd),
		Settlement: SettlementPayload{
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
	b.event.Fills = append(b.event.Fills, MatchFill{
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
	b.event.OrderUpdates = append(b.event.OrderUpdates, OrderUpdate{
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
	b.event.OrderUpdates = append(b.event.OrderUpdates, OrderUpdate{
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
	b.event.DepthUpdates = append(b.event.DepthUpdates, DepthUpdate{
		Side:        sideLabelForDepth(side),
		PriceTick:   price,
		TotalVolume: volume,
	})
	b.sizeBytes += 32
}

func (b *pendingBatch) hasPayload() bool {
	return len(b.event.Fills) > 0 || len(b.event.OrderUpdates) > 0 || len(b.event.DepthUpdates) > 0
}

func (b *pendingBatch) requiresColdLimit(registry *UserPositionRegistry) bool {
	if b == nil || registry == nil || len(b.event.Fills) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(b.event.Orders))
	for _, order := range b.event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		if wallet == "" {
			continue
		}
		if _, ok := seen[wallet]; ok {
			continue
		}
		seen[wallet] = struct{}{}
		if !registry.Has(b.event.MarketID, wallet) {
			return true
		}
	}
	return false
}

func (b *pendingBatch) shouldFlush(now time.Time, cfg BatchConfig, maxFills int, forceIdle bool) bool {
	if b == nil || !b.hasPayload() {
		return false
	}
	if maxFills <= 0 {
		maxFills = cfg.MaxFillsHot
	}
	if maxFills > 0 && len(b.event.Fills) >= maxFills {
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

func (b *pendingBatch) freeze(now time.Time) MatchBatchEvent {
	event := b.event
	event.EventID = generateBatchEventID(event, b.seqs, now)
	event.ProducedAt = now.Unix()
	return event
}

func executionSnapshotFromCommand(cmd *PlaceOrderCommand) ExecutionSnapshot {
	return ExecutionSnapshot{
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

func executionSnapshotFromOrder(order *MemoryOrder) ExecutionSnapshot {
	return ExecutionSnapshot{
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

func generateBatchEventID(event MatchBatchEvent, seqs map[uint64]struct{}, now time.Time) string {
	marketID := event.MarketID
	seqMin := event.SourceCmdSeqMin
	seqMax := event.SourceCmdSeqMax
	if len(seqs) == 0 || seqMax == 0 {
		digest := fnv.New64a()
		for _, order := range event.Orders {
			_, _ = digest.Write([]byte(strconv.FormatUint(order.OrderID, 10)))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(order.Execution.NormalizedSide))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(uint64(order.Execution.NormalizedPriceTick), 10)))
			_, _ = digest.Write([]byte{';'})
		}
		for _, update := range event.OrderUpdates {
			_, _ = digest.Write([]byte(strconv.FormatUint(uint64(update.OrderIndex), 10)))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(update.Status))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(update.RemainingQtyLots, 10)))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(update.RemainingSpendAmount, 10)))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(update.RefundAmount, 10)))
			_, _ = digest.Write([]byte{';'})
		}
		for _, depth := range event.DepthUpdates {
			_, _ = digest.Write([]byte(depth.Side))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(uint64(depth.PriceTick), 10)))
			_, _ = digest.Write([]byte{':'})
			_, _ = digest.Write([]byte(strconv.FormatUint(depth.TotalVolume, 10)))
			_, _ = digest.Write([]byte{';'})
		}
		if len(event.Orders) == 0 && len(event.OrderUpdates) == 0 && len(event.DepthUpdates) == 0 {
			return "match-batch-" + strconv.FormatUint(marketID, 10) + "-tick-" + strconv.FormatInt(now.UnixMilli(), 10)
		}
		return "match-batch-" + strconv.FormatUint(marketID, 10) + "-tick-" + strconv.FormatUint(digest.Sum64(), 16)
	}
	digest := fnv.New64a()
	ordered := make([]uint64, 0, len(seqs))
	for seq := range seqs {
		ordered = append(ordered, seq)
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j] < ordered[i] {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, seq := range ordered {
		_, _ = digest.Write([]byte(strconv.FormatUint(seq, 10)))
		_, _ = digest.Write([]byte{':'})
	}
	return "match-batch-" +
		strconv.FormatUint(marketID, 10) +
		"-" + strconv.FormatUint(seqMin, 10) +
		"-" + strconv.FormatUint(seqMax, 10) +
		"-" + strconv.FormatUint(digest.Sum64(), 16)
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
