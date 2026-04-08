package funds

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"blinkpredict/banckend/internal/matching"
)

type UserWallet struct {
	AvailableUSDC     uint64 `json:"available_usdc"`
	LockedUSDC        uint64 `json:"locked_usdc"`
	PendingUSDC       int64  `json:"pending_usdc"`
	CancelAllBeforeTs int64  `json:"cancel_all_before_ts"`
	Dirty             bool   `json:"-"` // signals projector to sync this wallet
}

type MarketPosition struct {
	MarketID uint64 `json:"market_id"`

	AvailableYesShares uint64 `json:"available_yes_shares"`
	LockedYesShares    uint64 `json:"locked_yes_shares"`
	PendingYesShares   int64  `json:"pending_yes_shares"`

	AvailableNoShares     uint64 `json:"available_no_shares"`
	LockedNoShares        uint64 `json:"locked_no_shares"`
	PendingNoShares       int64  `json:"pending_no_shares"`
	CollateralLockedUnits uint64 `json:"collateral_locked_units"`
	Dirty                 bool   `json:"-"` // signals projector to sync this position
}

type WalletSnapshot struct {
	WalletAddress string         `json:"wallet_address"`
	MarketPDA     string         `json:"market_pda"`
	Ledger        UserWallet     `json:"ledger"`
	Position      MarketPosition `json:"position"`
}

type ReserveOrderInput struct {
	WalletAddress     string
	MarketID          uint64
	MarketPDA         string
	OriginalAction    uint8
	OriginalOutcome   uint8
	OriginalPriceTick uint8
	OrderType         uint8
	QtyLots           uint64
	SpendAmount       uint64
}

func pendingReserveFromInput(cmd ReserveOrderInput) PendingReserve {
	pending := PendingReserve{
		WalletAddress: cmd.WalletAddress,
		MarketPDA:     cmd.MarketPDA,
	}
	switch cmd.OriginalAction {
	case SideBuy:
		pending.LockedUSDC = requiredReserveUnits(cmd)
	case SideSell:
		switch cmd.OriginalOutcome {
		case OutcomeYes:
			pending.LockedYesShares = cmd.QtyLots
		case OutcomeNo:
			pending.LockedNoShares = cmd.QtyLots
		}
	}
	return pending
}

type ActiveOrder struct {
	WalletAddress     string
	MarketID          uint64
	MarketPDA         string
	OriginalAction    uint8
	OriginalOutcome   uint8
	OriginalPriceTick uint8
	OrderType         uint8
	RemainingQty      uint64
	RemainingSpend    uint64
}

type walletShard struct {
	mu        sync.RWMutex
	ledgers   map[string]UserWallet
	positions map[string]MarketPosition
}

type dirtyIndex struct {
	mu sync.Mutex

	walletQueue []string
	walletSeen  map[string]struct{}

	positionQueue []string
	positionSeen  map[string]struct{}
}

type Manager struct {
	shards []walletShard
	dirty  dirtyIndex
}

func NewManager() *Manager {
	shards := make([]walletShard, 64)
	for i := range shards {
		shards[i] = walletShard{
			ledgers:   make(map[string]UserWallet),
			positions: make(map[string]MarketPosition),
		}
	}
	return &Manager{
		shards: shards,
		dirty: dirtyIndex{
			walletSeen:   make(map[string]struct{}),
			positionSeen: make(map[string]struct{}),
		},
	}
}

func (m *Manager) Snapshot(walletAddress, marketPDA string) WalletSnapshot {
	shard := m.shard(walletAddress)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	return WalletSnapshot{
		WalletAddress: walletAddress,
		MarketPDA:     marketPDA,
		Ledger:        shard.ledgers[walletAddress],
		Position:      shard.positions[positionKey(walletAddress, marketPDA)],
	}
}

func (m *Manager) Ledger(walletAddress string) UserWallet {
	shard := m.shard(walletAddress)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	return shard.ledgers[walletAddress]
}

func (m *Manager) Position(walletAddress, marketPDA string) MarketPosition {
	shard := m.shard(walletAddress)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	return shard.positions[positionKey(walletAddress, marketPDA)]
}

func (m *Manager) SeedLedger(walletAddress string, ledger UserWallet) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.ledgers[walletAddress] = ledger
}

func (m *Manager) SeedPosition(walletAddress, marketPDA string, position MarketPosition) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.positions[positionKey(walletAddress, marketPDA)] = position
}

func (m *Manager) MarkLedgerDirty(walletAddress string) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	ledger, ok := shard.ledgers[walletAddress]
	if !ok {
		return
	}
	m.markLedgerDirtyLocked(walletAddress, &ledger)
	shard.ledgers[walletAddress] = ledger
}

func (m *Manager) MarkPositionDirty(walletAddress, marketPDA string) {
	key := positionKey(walletAddress, marketPDA)
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	position, ok := shard.positions[key]
	if !ok {
		return
	}
	m.markPositionDirtyLocked(walletAddress, marketPDA, &position)
	shard.positions[key] = position
}

func (m *Manager) ApplyChainLedger(walletAddress string, ledger UserWallet) {
	m.SeedLedger(walletAddress, ledger)
}

func (m *Manager) ApplyChainPosition(walletAddress, marketPDA string, position MarketPosition) {
	m.SeedPosition(walletAddress, marketPDA, position)
}

func (m *Manager) DeletePosition(walletAddress, marketPDA string) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.positions, positionKey(walletAddress, marketPDA))
}

func (m *Manager) ApplyDepositConfirmed(walletAddress string, amount uint64) {
	if amount == 0 {
		return
	}
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	ledger := shard.ledgers[walletAddress]
	ledger.AvailableUSDC += amount
	m.markLedgerDirtyLocked(walletAddress, &ledger)
	shard.ledgers[walletAddress] = ledger
}

func (m *Manager) ApplyWithdrawConfirmed(walletAddress string, amount uint64) {
	if amount == 0 {
		return
	}
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	ledger := shard.ledgers[walletAddress]
	if ledger.AvailableUSDC < amount {
		ledger.AvailableUSDC = 0
	} else {
		ledger.AvailableUSDC -= amount
	}
	m.markLedgerDirtyLocked(walletAddress, &ledger)
	shard.ledgers[walletAddress] = ledger
}

func (m *Manager) ReserveOrder(cmd ReserveOrderInput) error {
	shard := m.shard(cmd.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[cmd.WalletAddress]
	posKey := positionKey(cmd.WalletAddress, cmd.MarketPDA)
	position := shard.positions[posKey]
	if position.MarketID == 0 {
		position.MarketID = cmd.MarketID
	}

	switch cmd.OriginalAction {
	case SideBuy:
		reserve := requiredReserveUnits(cmd)
		if ledger.AvailableUSDC < reserve {
			return fmt.Errorf("insufficient available usdc: available=%d required=%d", ledger.AvailableUSDC, reserve)
		}
		ledger.AvailableUSDC -= reserve
		ledger.LockedUSDC += reserve
		position.CollateralLockedUnits += reserve
	case SideSell:
		switch cmd.OriginalOutcome {
		case OutcomeYes:
			if position.AvailableYesShares < cmd.QtyLots {
				return fmt.Errorf("insufficient available yes shares: available=%d required=%d", position.AvailableYesShares, cmd.QtyLots)
			}
			position.AvailableYesShares -= cmd.QtyLots
			position.LockedYesShares += cmd.QtyLots
		case OutcomeNo:
			if position.AvailableNoShares < cmd.QtyLots {
				return fmt.Errorf("insufficient available no shares: available=%d required=%d", position.AvailableNoShares, cmd.QtyLots)
			}
			position.AvailableNoShares -= cmd.QtyLots
			position.LockedNoShares += cmd.QtyLots
		default:
			return fmt.Errorf("unsupported original outcome: %d", cmd.OriginalOutcome)
		}
	default:
		return fmt.Errorf("unsupported original action: %d", cmd.OriginalAction)
	}

	m.markLedgerDirtyLocked(cmd.WalletAddress, &ledger)
	m.markPositionDirtyLocked(cmd.WalletAddress, cmd.MarketPDA, &position)
	shard.ledgers[cmd.WalletAddress] = ledger
	shard.positions[posKey] = position
	return nil
}

func (m *Manager) RecoverOrderLock(order ActiveOrder) {
	shard := m.shard(order.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[order.WalletAddress]
	posKey := positionKey(order.WalletAddress, order.MarketPDA)
	position := shard.positions[posKey]
	if position.MarketID == 0 {
		position.MarketID = order.MarketID
	}

	if order.OriginalAction == SideBuy {
		lockAmount := requiredReserveUnitsFromOrder(order)
		lockAmount = minUint64(lockAmount, ledger.AvailableUSDC)
		ledger.AvailableUSDC -= lockAmount
		ledger.LockedUSDC += lockAmount
		position.CollateralLockedUnits += lockAmount
	} else if order.OriginalAction == SideSell {
		if order.OriginalOutcome == OutcomeYes {
			lockAmount := minUint64(order.RemainingQty, position.AvailableYesShares)
			position.AvailableYesShares -= lockAmount
			position.LockedYesShares += lockAmount
		} else {
			lockAmount := minUint64(order.RemainingQty, position.AvailableNoShares)
			position.AvailableNoShares -= lockAmount
			position.LockedNoShares += lockAmount
		}
	}

	shard.ledgers[order.WalletAddress] = ledger
	shard.positions[posKey] = position
}

func (m *Manager) ReleaseOrder(order ActiveOrder, refundAmount uint64) {
	shard := m.shard(order.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[order.WalletAddress]
	posKey := positionKey(order.WalletAddress, order.MarketPDA)
	position := shard.positions[posKey]
	if position.MarketID == 0 {
		position.MarketID = order.MarketID
	}

	if order.OriginalAction == SideBuy {
		unlock := refundAmount
		if unlock == 0 {
			if order.OrderType == OrderTypeMarket {
				unlock = order.RemainingSpend
			} else {
				unlock = reserveUnitsForLots(order.RemainingQty, order.OriginalPriceTick)
			}
		}
		unlock = minUint64(unlock, ledger.LockedUSDC)
		ledger.LockedUSDC -= unlock
		ledger.AvailableUSDC += unlock
		collateralUnlock := minUint64(unlock, position.CollateralLockedUnits)
		position.CollateralLockedUnits -= collateralUnlock
	} else {
		if order.OriginalOutcome == OutcomeYes {
			release := minUint64(order.RemainingQty, position.LockedYesShares)
			position.LockedYesShares -= release
			position.AvailableYesShares += release
		} else {
			release := minUint64(order.RemainingQty, position.LockedNoShares)
			position.LockedNoShares -= release
			position.AvailableNoShares += release
		}
	}

	m.markLedgerDirtyLocked(order.WalletAddress, &ledger)
	m.markPositionDirtyLocked(order.WalletAddress, order.MarketPDA, &position)
	shard.ledgers[order.WalletAddress] = ledger
	shard.positions[posKey] = position
}

func (m *Manager) ApplyMatchPending(maker, taker ActiveOrder, qty uint64, normalizedPrice uint8) {
	m.applyFillForOrder(maker, qty, normalizedPrice)
	m.applyFillForOrder(taker, qty, normalizedPrice)
}

func (m *Manager) ApplySettlementConfirmed(walletAddress, marketPDA string) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[walletAddress]
	posKey := positionKey(walletAddress, marketPDA)
	position := shard.positions[posKey]

	if ledger.PendingUSDC > 0 {
		ledger.AvailableUSDC += uint64(ledger.PendingUSDC)
	}
	ledger.PendingUSDC = 0

	if position.PendingYesShares > 0 {
		position.AvailableYesShares += uint64(position.PendingYesShares)
	}
	position.PendingYesShares = 0

	if position.PendingNoShares > 0 {
		position.AvailableNoShares += uint64(position.PendingNoShares)
	}
	position.PendingNoShares = 0
	m.markLedgerDirtyLocked(walletAddress, &ledger)
	m.markPositionDirtyLocked(walletAddress, marketPDA, &position)

	shard.ledgers[walletAddress] = ledger
	shard.positions[posKey] = position
}

func (m *Manager) applyFillForOrder(order ActiveOrder, qty uint64, normalizedPrice uint8) {
	shard := m.shard(order.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[order.WalletAddress]
	posKey := positionKey(order.WalletAddress, order.MarketPDA)
	position := shard.positions[posKey]
	if position.MarketID == 0 {
		position.MarketID = order.MarketID
	}

	actualUnits := actualUnitsForOrder(order, qty, normalizedPrice)
	if order.OriginalAction == SideBuy {
		if order.OrderType == OrderTypeLimit {
			reserved := reserveUnitsForLots(qty, order.OriginalPriceTick)
			reserved = minUint64(reserved, ledger.LockedUSDC)
			ledger.LockedUSDC -= reserved
			collateralConsume := minUint64(reserved, position.CollateralLockedUnits)
			position.CollateralLockedUnits -= collateralConsume
			if reserved > actualUnits {
				ledger.AvailableUSDC += reserved - actualUnits
			}
		} else {
			consume := minUint64(actualUnits, ledger.LockedUSDC)
			ledger.LockedUSDC -= consume
			collateralConsume := minUint64(consume, position.CollateralLockedUnits)
			position.CollateralLockedUnits -= collateralConsume
			actualUnits = consume
		}
		if order.OriginalOutcome == OutcomeYes {
			position.PendingYesShares += int64(qty)
		} else {
			position.PendingNoShares += int64(qty)
		}
	} else {
		if order.OriginalOutcome == OutcomeYes {
			consume := minUint64(qty, position.LockedYesShares)
			position.LockedYesShares -= consume
		} else {
			consume := minUint64(qty, position.LockedNoShares)
			position.LockedNoShares -= consume
		}
		ledger.PendingUSDC += int64(actualUnits)
	}

	m.markLedgerDirtyLocked(order.WalletAddress, &ledger)
	m.markPositionDirtyLocked(order.WalletAddress, order.MarketPDA, &position)
	shard.ledgers[order.WalletAddress] = ledger
	shard.positions[posKey] = position
}

func (m *Manager) shard(walletAddress string) *walletShard {
	idx := shardIndex(walletAddress) % uint32(len(m.shards))
	return &m.shards[idx]
}

func shardIndex(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32()
}

func positionKey(walletAddress, marketPDA string) string {
	return walletAddress + "|" + marketPDA
}

func requiredReserveUnits(cmd ReserveOrderInput) uint64 {
	if cmd.OrderType == OrderTypeMarket {
		return cmd.SpendAmount
	}
	return reserveUnitsForLots(cmd.QtyLots, cmd.OriginalPriceTick)
}

func validateReserveAgainstSnapshot(ledger UserWallet, position MarketPosition, cmd ReserveOrderInput) error {
	switch cmd.OriginalAction {
	case SideBuy:
		reserve := requiredReserveUnits(cmd)
		if ledger.AvailableUSDC < reserve {
			return fmt.Errorf("insufficient available usdc: available=%d required=%d", ledger.AvailableUSDC, reserve)
		}
	case SideSell:
		switch cmd.OriginalOutcome {
		case OutcomeYes:
			if position.AvailableYesShares < cmd.QtyLots {
				return fmt.Errorf("insufficient available yes shares: available=%d required=%d", position.AvailableYesShares, cmd.QtyLots)
			}
		case OutcomeNo:
			if position.AvailableNoShares < cmd.QtyLots {
				return fmt.Errorf("insufficient available no shares: available=%d required=%d", position.AvailableNoShares, cmd.QtyLots)
			}
		default:
			return fmt.Errorf("unsupported original outcome: %d", cmd.OriginalOutcome)
		}
	default:
		return fmt.Errorf("unsupported original action: %d", cmd.OriginalAction)
	}
	return nil
}

func requiredReserveUnitsFromOrder(order ActiveOrder) uint64 {
	if order.OrderType == OrderTypeMarket {
		return order.RemainingSpend
	}
	return reserveUnitsForLots(order.RemainingQty, order.OriginalPriceTick)
}

func reserveUnitsForLots(qtyLots uint64, priceTick uint8) uint64 {
	return ceilMulDiv(qtyLots, uint64(priceTick), 100)
}

func actualUnitsForOrder(order ActiveOrder, qty uint64, normalizedPrice uint8) uint64 {
	if order.OriginalOutcome == OutcomeNo {
		return ceilMulDiv(qty, uint64(100-normalizedPrice), 100)
	}
	return ceilMulDiv(qty, uint64(normalizedPrice), 100)
}

func ceilMulDiv(a, b, div uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	product := a * b
	return (product + div - 1) / div
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

const (
	SideBuy  uint8 = 0
	SideSell uint8 = 1

	OutcomeYes uint8 = 0
	OutcomeNo  uint8 = 1

	OrderTypeLimit  uint8 = 0
	OrderTypeMarket uint8 = 1
)

// ==========================================
// 按原始 MatchBatch 精确重算的新方法
// ==========================================

// walletBatchDelta 描述一个 wallet 在某个 match batch 中的资金影响。
// 由 ComputeBatchDeltas 从原始 batch 推导，然后分别用于 apply/revert。
type walletBatchDelta struct {
	WalletAddress string
	MarketPDA     string
	// USDC 侧：locked 消耗量（买单），pending 变化量
	LockedUSDCConsumed  uint64 // 从 locked_usdc 扣除的总量
	AvailableUSDCRefund uint64 // 限价差额立刻退回 available_usdc 的量
	PendingUSDCDelta    int64  // 正=卖出所得进 pending, 负=买入花费减 pending
	// Position 侧
	PendingYesDelta   int64  // 正=买入 yes 股，负=卖出 yes 股
	PendingNoDelta    int64  // 正=买入 no 股，负=卖出 no 股
	LockedYesConsumed uint64 // 卖 yes 时从 locked 扣除的量
	LockedNoConsumed  uint64 // 卖 no 时从 locked 扣除的量
}

// computeBatchDeltasForWallet 从原始 match batch 推导指定 wallet 的资金影响。
// 这是所有精确重算（apply/revert）的单一真相来源。
func computeBatchDeltasForWallet(event *matching.MatchBatchEvent, marketPDA string) map[string]*walletBatchDelta {
	orderByIndex := make(map[uint16]matching.MatchedOrder, len(event.Orders))
	for _, o := range event.Orders {
		orderByIndex[o.OrderIndex] = o
	}

	deltas := make(map[string]*walletBatchDelta)
	getDelta := func(addr, pda string) *walletBatchDelta {
		key := addr + "|" + pda
		if d, ok := deltas[key]; ok {
			return d
		}
		d := &walletBatchDelta{WalletAddress: strings.TrimSpace(addr), MarketPDA: pda}
		deltas[key] = d
		return d
	}

	for _, fill := range event.Fills {
		maker, makerOK := orderByIndex[fill.MakerOrderIndex]
		taker, takerOK := orderByIndex[fill.TakerOrderIndex]
		if !makerOK || !takerOK {
			continue
		}
		for _, matched := range []matching.MatchedOrder{maker, taker} {
			exec := matched.Execution
			addr := strings.TrimSpace(exec.WalletAddress)
			d := getDelta(addr, marketPDA)
			qty := fill.FillAmount
			normPrice := uint8(fill.FillPrice)

			if exec.OriginalAction == "buy" {
				// 买单：从 locked_usdc 扣，限价差额退 available，股份进 pending
				var actualUnits uint64
				if exec.OriginalOutcome == "no" {
					actualUnits = ceilMulDiv(qty, uint64(100-normPrice), 100)
				} else {
					actualUnits = ceilMulDiv(qty, uint64(normPrice), 100)
				}
				if exec.OrderType == "limit" {
					reserved := reserveUnitsForLots(qty, exec.OriginalPriceTick)
					d.LockedUSDCConsumed += reserved
					if reserved > actualUnits {
						d.AvailableUSDCRefund += reserved - actualUnits
					}
					d.PendingUSDCDelta -= int64(actualUnits)
				} else {
					// market buy: 消耗 actualUnits from locked
					d.LockedUSDCConsumed += actualUnits
					d.PendingUSDCDelta -= int64(actualUnits)
				}
				if exec.OriginalOutcome == "yes" {
					d.PendingYesDelta += int64(qty)
				} else {
					d.PendingNoDelta += int64(qty)
				}
			} else {
				// 卖单：从 locked_yes/no 扣，USDC 进 pending
				var actualUnits uint64
				if exec.OriginalOutcome == "no" {
					actualUnits = ceilMulDiv(qty, uint64(100-normPrice), 100)
				} else {
					actualUnits = ceilMulDiv(qty, uint64(normPrice), 100)
				}
				if exec.OriginalOutcome == "yes" {
					d.LockedYesConsumed += qty
					d.PendingYesDelta -= int64(qty)
				} else {
					d.LockedNoConsumed += qty
					d.PendingNoDelta -= int64(qty)
				}
				d.PendingUSDCDelta += int64(actualUnits)
			}
		}
	}
	return deltas
}

// ApplySettlementConfirmedByBatch 按原始 batch 精确转正 pending -> available。
// 不清零整钱包 pending，只处理本批次对应的影响。
func (m *Manager) ApplySettlementConfirmedByBatch(event *matching.MatchBatchEvent) error {
	if event == nil {
		return fmt.Errorf("nil match batch event")
	}
	deltas := computeBatchDeltasForWallet(event, event.MarketPDA)
	for _, d := range deltas {
		shard := m.shard(d.WalletAddress)
		shard.mu.Lock()

		ledger := shard.ledgers[d.WalletAddress]
		posKey := positionKey(d.WalletAddress, d.MarketPDA)
		position := shard.positions[posKey]

		// 转正 pending USDC（卖单所得）
		if d.PendingUSDCDelta > 0 {
			// 卖出所得转正：pending_usdc 减，available_usdc 加
			canTransfer := minInt64(d.PendingUSDCDelta, ledger.PendingUSDC)
			if canTransfer > 0 {
				ledger.PendingUSDC -= canTransfer
				ledger.AvailableUSDC += uint64(canTransfer)
			}
		}
		// 转正 pending Yes 股（买 yes 所得）
		if d.PendingYesDelta > 0 {
			canTransfer := minInt64(d.PendingYesDelta, position.PendingYesShares)
			if canTransfer > 0 {
				position.PendingYesShares -= canTransfer
				position.AvailableYesShares += uint64(canTransfer)
			}
		}
		// 转正 pending No 股（买 no 所得）
		if d.PendingNoDelta > 0 {
			canTransfer := minInt64(d.PendingNoDelta, position.PendingNoShares)
			if canTransfer > 0 {
				position.PendingNoShares -= canTransfer
				position.AvailableNoShares += uint64(canTransfer)
			}
		}
		m.markLedgerDirtyLocked(d.WalletAddress, &ledger)
		m.markPositionDirtyLocked(d.WalletAddress, d.MarketPDA, &position)
		shard.ledgers[d.WalletAddress] = ledger
		shard.positions[posKey] = position
		shard.mu.Unlock()
	}
	return nil
}

// ApplySettlementFailedByBatch 按原始 batch 精确回滚 pending 状态。
// 买单失败：pending_yes/no 还原，实际花费的 USDC 退回 available。
// 卖单失败：pending_usdc 还原，股份退回 available。
func (m *Manager) ApplySettlementFailedByBatch(event *matching.MatchBatchEvent) error {
	if event == nil {
		return fmt.Errorf("nil match batch event")
	}
	deltas := computeBatchDeltasForWallet(event, event.MarketPDA)
	for _, d := range deltas {
		shard := m.shard(d.WalletAddress)
		shard.mu.Lock()

		ledger := shard.ledgers[d.WalletAddress]
		posKey := positionKey(d.WalletAddress, d.MarketPDA)
		position := shard.positions[posKey]

		// 买单失败回滚：pending_yes/no 清掉，实际花费的 USDC (pending 减少部分) 退回 available
		if d.PendingYesDelta > 0 {
			// 撤掉买入产生的 pending yes 股
			canRevert := minInt64(d.PendingYesDelta, position.PendingYesShares)
			if canRevert > 0 {
				position.PendingYesShares -= canRevert
			}
		}
		if d.PendingNoDelta > 0 {
			canRevert := minInt64(d.PendingNoDelta, position.PendingNoShares)
			if canRevert > 0 {
				position.PendingNoShares -= canRevert
			}
		}
		// 买单失败时，只需要把本批真实成交花费退回 available。
		// buy-side 不再把花费记成负 pending_usdc，因此这里不再恢复 pending。
		if d.PendingUSDCDelta < 0 {
			refund := uint64(-d.PendingUSDCDelta)
			ledger.AvailableUSDC += refund
		}

		// 卖单失败回滚：pending_usdc 清掉，股份退回 available
		if d.PendingUSDCDelta > 0 {
			// 撤掉卖出产生的 pending usdc
			canRevert := minInt64(d.PendingUSDCDelta, ledger.PendingUSDC)
			if canRevert > 0 {
				ledger.PendingUSDC -= canRevert
			}
		}
		if d.LockedYesConsumed > 0 {
			// 卖 yes 的股份从 locked 扣了，失败了要还回 available
			position.AvailableYesShares += d.LockedYesConsumed
		}
		if d.LockedNoConsumed > 0 {
			position.AvailableNoShares += d.LockedNoConsumed
		}
		m.markLedgerDirtyLocked(d.WalletAddress, &ledger)
		m.markPositionDirtyLocked(d.WalletAddress, d.MarketPDA, &position)
		shard.ledgers[d.WalletAddress] = ledger
		shard.positions[posKey] = position
		shard.mu.Unlock()
	}
	return nil
}

// DirtyWalletSnapshot 用于 projector 收集的快照数据。
type DirtyWalletSnapshot struct {
	WalletAddress string
	Ledger        UserWallet
}

// DirtyPositionSnapshot 用于 projector 收集的仓位快照数据。
type DirtyPositionSnapshot struct {
	WalletAddress string
	MarketID      uint64
	MarketPDA     string
	Position      MarketPosition
}

// CollectAndClearDirty 只收集本轮实际变脏的 key，并清除 dirty 标记。
// 供 projectorLoop 调用，避免每次全量扫描所有 shard。
func (m *Manager) CollectAndClearDirty() (wallets []DirtyWalletSnapshot, positions []DirtyPositionSnapshot) {
	walletKeys, positionKeys := m.dirty.drain()
	for _, walletAddress := range walletKeys {
		shard := m.shard(walletAddress)
		shard.mu.Lock()
		ledger, ok := shard.ledgers[walletAddress]
		if ok && ledger.Dirty {
			ledger.Dirty = false
			shard.ledgers[walletAddress] = ledger
			wallets = append(wallets, DirtyWalletSnapshot{WalletAddress: walletAddress, Ledger: ledger})
		}
		shard.mu.Unlock()
	}
	for _, key := range positionKeys {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) != 2 {
			continue
		}
		shard := m.shard(parts[0])
		shard.mu.Lock()
		pos, ok := shard.positions[key]
		if ok && pos.Dirty {
			pos.Dirty = false
			shard.positions[key] = pos
			positions = append(positions, DirtyPositionSnapshot{
				WalletAddress: parts[0],
				MarketID:      pos.MarketID,
				MarketPDA:     parts[1],
				Position:      pos,
			})
		}
		shard.mu.Unlock()
	}
	return wallets, positions
}

func (m *Manager) RestoreDirty(wallets []DirtyWalletSnapshot, positions []DirtyPositionSnapshot) {
	for _, wallet := range wallets {
		shard := m.shard(wallet.WalletAddress)
		shard.mu.Lock()
		ledger := shard.ledgers[wallet.WalletAddress]
		m.markLedgerDirtyLocked(wallet.WalletAddress, &ledger)
		shard.ledgers[wallet.WalletAddress] = ledger
		shard.mu.Unlock()
	}
	for _, pos := range positions {
		shard := m.shard(pos.WalletAddress)
		shard.mu.Lock()
		key := positionKey(pos.WalletAddress, pos.MarketPDA)
		current := shard.positions[key]
		if current.MarketID == 0 {
			current.MarketID = pos.MarketID
		}
		m.markPositionDirtyLocked(pos.WalletAddress, pos.MarketPDA, &current)
		shard.positions[key] = current
		shard.mu.Unlock()
	}
}

func (m *Manager) markLedgerDirtyLocked(walletAddress string, ledger *UserWallet) {
	if ledger == nil {
		return
	}
	ledger.Dirty = true
	m.dirty.markWallet(walletAddress)
}

func (m *Manager) markPositionDirtyLocked(walletAddress, marketPDA string, position *MarketPosition) {
	if position == nil {
		return
	}
	position.Dirty = true
	m.dirty.markPosition(positionKey(walletAddress, marketPDA))
}

func (d *dirtyIndex) markWallet(walletAddress string) {
	walletAddress = strings.TrimSpace(walletAddress)
	if walletAddress == "" {
		return
	}
	d.mu.Lock()
	if _, ok := d.walletSeen[walletAddress]; !ok {
		d.walletSeen[walletAddress] = struct{}{}
		d.walletQueue = append(d.walletQueue, walletAddress)
	}
	d.mu.Unlock()
}

func (d *dirtyIndex) markPosition(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	d.mu.Lock()
	if _, ok := d.positionSeen[key]; !ok {
		d.positionSeen[key] = struct{}{}
		d.positionQueue = append(d.positionQueue, key)
	}
	d.mu.Unlock()
}

func (d *dirtyIndex) drain() (wallets []string, positions []string) {
	d.mu.Lock()
	wallets = append(wallets, d.walletQueue...)
	positions = append(positions, d.positionQueue...)
	d.walletQueue = nil
	d.positionQueue = nil
	d.walletSeen = make(map[string]struct{})
	d.positionSeen = make(map[string]struct{})
	d.mu.Unlock()
	return wallets, positions
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
