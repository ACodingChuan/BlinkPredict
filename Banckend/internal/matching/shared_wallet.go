package matching

import (
	"fmt"
	"hash/fnv"
	"strconv"
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

type walletShard struct {
	mu        sync.RWMutex
	ledgers   map[string]UserWallet
	positions map[string]MarketPosition
}

type SharedWalletManager struct {
	shards []walletShard
}

func NewSharedWalletManager() *SharedWalletManager {
	shards := make([]walletShard, 64)
	for i := range shards {
		shards[i] = walletShard{
			ledgers:   make(map[string]UserWallet),
			positions: make(map[string]MarketPosition),
		}
	}
	return &SharedWalletManager{shards: shards}
}

func (m *SharedWalletManager) Snapshot(walletAddress, marketPDA string) WalletSnapshot {
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

func (m *SharedWalletManager) SeedLedger(walletAddress string, ledger UserWallet) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.ledgers[walletAddress] = ledger
}

func (m *SharedWalletManager) SeedPosition(walletAddress, marketPDA string, position MarketPosition) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.positions[positionKey(walletAddress, marketPDA)] = position
}

func (m *SharedWalletManager) ApplyChainLedger(walletAddress string, ledger UserWallet) {
	m.SeedLedger(walletAddress, ledger)
}

func (m *SharedWalletManager) ApplyDeposit(walletAddress string, amount uint64) {
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

func (m *SharedWalletManager) ApplyChainPosition(walletAddress, marketPDA string, position MarketPosition) {
	m.SeedPosition(walletAddress, marketPDA, position)
}

func (m *SharedWalletManager) DeletePosition(walletAddress, marketPDA string) {
	shard := m.shard(walletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.positions, positionKey(walletAddress, marketPDA))
}

func (m *SharedWalletManager) ReserveOrder(cmd *PlaceOrderCommand) error {
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
		case 0:
			if position.AvailableYesShares < cmd.QtyLots {
				return fmt.Errorf("insufficient available yes shares: available=%d required=%d", position.AvailableYesShares, cmd.QtyLots)
			}
			position.AvailableYesShares -= cmd.QtyLots
			position.LockedYesShares += cmd.QtyLots
		case 1:
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

func (m *SharedWalletManager) RecoverOrderLock(order *MemoryOrder, marketPDA string) {
	shard := m.shard(order.WalletAddress)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	ledger := shard.ledgers[order.WalletAddress]
	posKey := positionKey(order.WalletAddress, marketPDA)
	position := shard.positions[posKey]

	if order.OriginalAction == SideBuy {
		lockAmount := requiredReserveUnitsFromOrder(order)
		lockAmount = minUint64(lockAmount, ledger.AvailableUSDC)
		ledger.AvailableUSDC -= lockAmount
		ledger.LockedUSDC += lockAmount
	} else if order.OriginalAction == SideSell {
		if order.OriginalOutcome == 0 {
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

func (m *SharedWalletManager) ReleaseOrder(order *MemoryOrder, refundAmount uint64) {
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
		if order.OriginalOutcome == 0 {
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

func (m *SharedWalletManager) ApplyLocalFill(maker, taker *MemoryOrder, qty uint64, normalizedPrice uint8, matchType string) {
	m.applyFillForOrder(maker, qty, normalizedPrice, matchType)
	m.applyFillForOrder(taker, qty, normalizedPrice, matchType)
}

func (m *SharedWalletManager) applyFillForOrder(order *MemoryOrder, qty uint64, normalizedPrice uint8, _ string) {
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
			if order.RemainingSpend > 0 && order.RemainingSpend < actualUnits {
				actualUnits = consume
			}
		}
		ledger.PendingUSDC -= int64(actualUnits)
		if order.OriginalOutcome == 0 {
			position.AvailableYesShares += qty
			position.PendingYesShares += int64(qty)
		} else {
			position.AvailableNoShares += qty
			position.PendingNoShares += int64(qty)
		}
	} else {
		if order.OriginalOutcome == 0 {
			consume := minUint64(qty, position.LockedYesShares)
			position.LockedYesShares -= consume
			position.PendingYesShares -= int64(consume)
		} else {
			consume := minUint64(qty, position.LockedNoShares)
			position.LockedNoShares -= consume
			position.PendingNoShares -= int64(consume)
		}
		ledger.AvailableUSDC += actualUnits
		ledger.PendingUSDC += int64(actualUnits)
	}

	shard.ledgers[order.WalletAddress] = ledger
	shard.positions[posKey] = position
}

func (m *SharedWalletManager) shard(walletAddress string) *walletShard {
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

func requiredReserveUnits(cmd *PlaceOrderCommand) uint64 {
	if cmd.OrderType == OrderTypeMarket {
		return cmd.SpendAmount
	}
	return reserveUnitsForLots(cmd.QtyLots, cmd.OriginalPriceTick)
}

func requiredReserveUnitsFromOrder(order *MemoryOrder) uint64 {
	if order.OrderType == OrderTypeMarket {
		return order.RemainingSpend
	}
	return reserveUnitsForLots(order.RemainingQty, order.OriginalPriceTick)
}

func reserveUnitsForLots(qtyLots uint64, priceTick uint8) uint64 {
	return ceilMulDiv(qtyLots, uint64(priceTick), 100)
}

func actualUnitsForOrder(order *MemoryOrder, qty uint64, normalizedPrice uint8) uint64 {
	if order.OriginalOutcome == 1 {
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

func marketPDAForOrder(order *MemoryOrder, fallbackMarketID uint64) string {
	if order.MarketPDA != "" {
		return order.MarketPDA
	}
	return strconv.FormatUint(fallbackMarketID, 10)
}
