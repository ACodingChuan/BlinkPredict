package funds

import (
	"fmt"
	"hash/fnv"
	"sync"
)

type UserWallet struct {
	AvailableUSDC     uint64 `json:"available_usdc"`
	LockedUSDC        uint64 `json:"locked_usdc"`
	PendingUSDC       int64  `json:"pending_usdc"`
	CancelAllBeforeTs int64  `json:"cancel_all_before_ts"`
}

type MarketPosition struct {
	AvailableYesShares uint64 `json:"available_yes_shares"`
	LockedYesShares    uint64 `json:"locked_yes_shares"`
	PendingYesShares   int64  `json:"pending_yes_shares"`

	AvailableNoShares uint64 `json:"available_no_shares"`
	LockedNoShares    uint64 `json:"locked_no_shares"`
	PendingNoShares   int64  `json:"pending_no_shares"`
}

type WalletSnapshot struct {
	WalletAddress string         `json:"wallet_address"`
	MarketPDA     string         `json:"market_pda"`
	Ledger        UserWallet     `json:"ledger"`
	Position      MarketPosition `json:"position"`
}

type ReserveOrderInput struct {
	WalletAddress     string
	MarketPDA         string
	OriginalAction    uint8
	OriginalOutcome   uint8
	OriginalPriceTick uint8
	OrderType         uint8
	QtyLots           uint64
	SpendAmount       uint64
}

type ActiveOrder struct {
	WalletAddress     string
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

type Manager struct {
	shards []walletShard
}

func NewManager() *Manager {
	shards := make([]walletShard, 64)
	for i := range shards {
		shards[i] = walletShard{
			ledgers:   make(map[string]UserWallet),
			positions: make(map[string]MarketPosition),
		}
	}
	return &Manager{shards: shards}
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
	shard.ledgers[walletAddress] = ledger
}

func (m *Manager) ReserveOrder(cmd ReserveOrderInput) error {
	shard := m.shard(cmd.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[cmd.WalletAddress]
	posKey := positionKey(cmd.WalletAddress, cmd.MarketPDA)
	position := shard.positions[posKey]

	switch cmd.OriginalAction {
	case SideBuy:
		reserve := requiredReserveUnits(cmd)
		if ledger.AvailableUSDC < reserve {
			return fmt.Errorf("insufficient available usdc: available=%d required=%d", ledger.AvailableUSDC, reserve)
		}
		ledger.AvailableUSDC -= reserve
		ledger.LockedUSDC += reserve
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

	if order.OriginalAction == SideBuy {
		lockAmount := requiredReserveUnitsFromOrder(order)
		lockAmount = minUint64(lockAmount, ledger.AvailableUSDC)
		ledger.AvailableUSDC -= lockAmount
		ledger.LockedUSDC += lockAmount
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

	actualUnits := actualUnitsForOrder(order, qty, normalizedPrice)
	if order.OriginalAction == SideBuy {
		if order.OrderType == OrderTypeLimit {
			reserved := reserveUnitsForLots(qty, order.OriginalPriceTick)
			reserved = minUint64(reserved, ledger.LockedUSDC)
			ledger.LockedUSDC -= reserved
			if reserved > actualUnits {
				ledger.AvailableUSDC += reserved - actualUnits
			}
		} else {
			consume := minUint64(actualUnits, ledger.LockedUSDC)
			ledger.LockedUSDC -= consume
			actualUnits = consume
		}
		ledger.PendingUSDC -= int64(actualUnits)
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
