package walletshard

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	AssetCollateral = "collateral"
	AssetYes        = "yes"
	AssetNo         = "no"
)

type ReservationSnapshot struct {
	OrderID                uint64
	WalletAddress          string
	AssetKind              string
	MarketID               uint64
	OriginalReservedUnits  uint64
	OpenReservedUnits      uint64
	PendingSettlementUnits uint64
	ReleasedUnits          uint64
	FinalizedUnits         uint64
	RolledBackUnits        uint64
}

type walletState struct {
	walletAddress             string
	loaded                    bool
	frozen                    bool
	lastChainSlot             uint64
	confirmedCollateral       uint64
	reservedOpenCollateral    uint64
	reservedPendingCollateral uint64
	yes                       map[uint64]*assetPosition
	no                        map[uint64]*assetPosition
}

type assetPosition struct {
	confirmed uint64
	open      uint64
	pending   uint64
}

type shard struct {
	mu      sync.Mutex
	wallets map[string]*walletState
	orders  map[uint64]*ReservationSnapshot
}

type Manager struct {
	pool   *pgxpool.Pool
	shards []shard
}

func New(pool *pgxpool.Pool, shardCount int) *Manager {
	if shardCount <= 0 {
		shardCount = 128
	}
	m := &Manager{pool: pool, shards: make([]shard, shardCount)}
	for i := range m.shards {
		m.shards[i] = shard{wallets: map[string]*walletState{}, orders: map[uint64]*ReservationSnapshot{}}
	}
	return m
}

func (m *Manager) TryReserveOpenOrder(ctx context.Context, walletAddress string, orderID, marketID uint64, assetKind string, requiredUnits uint64) (*ReservationSnapshot, error) {
	s := m.shard(walletAddress)
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := m.ensureWalletLoaded(ctx, s, walletAddress)
	if err != nil {
		return nil, err
	}
	if state.frozen {
		return nil, fmt.Errorf("wallet is frozen")
	}
	if _, exists := s.orders[orderID]; exists {
		snap := *s.orders[orderID]
		return &snap, nil
	}
	if err := ensureSufficient(state, assetKind, marketID, requiredUnits); err != nil {
		return nil, err
	}
	applyOpenReserve(state, assetKind, marketID, requiredUnits)
	snap := &ReservationSnapshot{
		OrderID:               orderID,
		WalletAddress:         walletAddress,
		AssetKind:             assetKind,
		MarketID:              marketID,
		OriginalReservedUnits: requiredUnits,
		OpenReservedUnits:     requiredUnits,
	}
	s.orders[orderID] = snap
	copy := *snap
	return &copy, nil
}

func (m *Manager) ReleaseOpenReserve(ctx context.Context, walletAddress string, orderID uint64) (*ReservationSnapshot, error) {
	s := m.shard(walletAddress)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := m.ensureWalletLoaded(ctx, s, walletAddress); err != nil {
		return nil, err
	}
	snap, ok := s.orders[orderID]
	if !ok {
		return nil, nil
	}
	state := s.wallets[walletAddress]
	if snap.OpenReservedUnits > 0 {
		releaseOpenReserve(state, snap.AssetKind, snap.MarketID, snap.OpenReservedUnits)
		snap.ReleasedUnits += snap.OpenReservedUnits
		snap.OpenReservedUnits = 0
	}
	copy := *snap
	return &copy, nil
}

func (m *Manager) ApplyTrade(ctx context.Context, walletAddress string, orderID, marketID uint64, executedUnits uint64) (*ReservationSnapshot, error) {
	s := m.shard(walletAddress)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := m.ensureWalletLoaded(ctx, s, walletAddress); err != nil {
		return nil, err
	}
	snap, ok := s.orders[orderID]
	if !ok {
		return nil, nil
	}
	state := s.wallets[walletAddress]
	move := executedUnits
	if move > snap.OpenReservedUnits {
		move = snap.OpenReservedUnits
	}
	if move > 0 {
		moveToPending(state, snap.AssetKind, snap.MarketID, move)
		snap.OpenReservedUnits -= move
		snap.PendingSettlementUnits += move
	}
	copy := *snap
	return &copy, nil
}

func (m *Manager) ReserveRecoveredOrder(ctx context.Context, walletAddress string, orderID, marketID uint64, assetKind string, openReservedUnits uint64) error {
	s := m.shard(walletAddress)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := m.ensureWalletLoaded(ctx, s, walletAddress)
	if err != nil {
		return err
	}
	if _, exists := s.orders[orderID]; exists {
		return nil
	}
	applyOpenReserve(state, assetKind, marketID, openReservedUnits)
	s.orders[orderID] = &ReservationSnapshot{
		OrderID:               orderID,
		WalletAddress:         walletAddress,
		AssetKind:             assetKind,
		MarketID:              marketID,
		OriginalReservedUnits: openReservedUnits,
		OpenReservedUnits:     openReservedUnits,
	}
	return nil
}

func (m *Manager) ensureWalletLoaded(ctx context.Context, s *shard, walletAddress string) (*walletState, error) {
	if state, ok := s.wallets[walletAddress]; ok && state.loaded {
		return state, nil
	}
	state := &walletState{walletAddress: walletAddress, loaded: true, yes: map[uint64]*assetPosition{}, no: map[uint64]*assetPosition{}}
	if m.pool != nil {
		rows, err := m.pool.Query(ctx, `
			SELECT asset_type, market_id, confirmed_units, last_observed_slot
			FROM wallet_asset_balances
			WHERE wallet_address = $1
		`, walletAddress)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var assetType string
			var marketIDStr *string
			var confirmedUnits uint64
			var slot uint64
			if err := rows.Scan(&assetType, &marketIDStr, &confirmedUnits, &slot); err != nil {
				return nil, err
			}
			if slot > state.lastChainSlot {
				state.lastChainSlot = slot
			}
			switch assetType {
			case AssetCollateral:
				state.confirmedCollateral = confirmedUnits
			case AssetYes, AssetNo:
				var marketID uint64
				if marketIDStr != nil && *marketIDStr != "" {
					marketID, _ = strconv.ParseUint(*marketIDStr, 10, 64)
				}
				pos := &assetPosition{confirmed: confirmedUnits}
				if assetType == AssetYes {
					state.yes[marketID] = pos
				} else {
					state.no[marketID] = pos
				}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	s.wallets[walletAddress] = state
	return state, nil
}

func ensureSufficient(state *walletState, assetKind string, marketID, requiredUnits uint64) error {
	switch assetKind {
	case AssetCollateral:
		available := state.confirmedCollateral - state.reservedOpenCollateral - state.reservedPendingCollateral
		if available < requiredUnits {
			return fmt.Errorf("insufficient collateral: available=%d required=%d", available, requiredUnits)
		}
	case AssetYes:
		pos := ensurePosition(state.yes, marketID)
		available := pos.confirmed - pos.open - pos.pending
		if available < requiredUnits {
			return fmt.Errorf("insufficient yes inventory: available=%d required=%d", available, requiredUnits)
		}
	case AssetNo:
		pos := ensurePosition(state.no, marketID)
		available := pos.confirmed - pos.open - pos.pending
		if available < requiredUnits {
			return fmt.Errorf("insufficient no inventory: available=%d required=%d", available, requiredUnits)
		}
	default:
		return fmt.Errorf("unsupported asset kind: %s", assetKind)
	}
	return nil
}

func applyOpenReserve(state *walletState, assetKind string, marketID, units uint64) {
	switch assetKind {
	case AssetCollateral:
		state.reservedOpenCollateral += units
	case AssetYes:
		ensurePosition(state.yes, marketID).open += units
	case AssetNo:
		ensurePosition(state.no, marketID).open += units
	}
}

func releaseOpenReserve(state *walletState, assetKind string, marketID, units uint64) {
	switch assetKind {
	case AssetCollateral:
		if units > state.reservedOpenCollateral {
			state.reservedOpenCollateral = 0
		} else {
			state.reservedOpenCollateral -= units
		}
	case AssetYes:
		pos := ensurePosition(state.yes, marketID)
		if units > pos.open {
			pos.open = 0
		} else {
			pos.open -= units
		}
	case AssetNo:
		pos := ensurePosition(state.no, marketID)
		if units > pos.open {
			pos.open = 0
		} else {
			pos.open -= units
		}
	}
}

func moveToPending(state *walletState, assetKind string, marketID, units uint64) {
	releaseOpenReserve(state, assetKind, marketID, units)
	switch assetKind {
	case AssetCollateral:
		state.reservedPendingCollateral += units
	case AssetYes:
		ensurePosition(state.yes, marketID).pending += units
	case AssetNo:
		ensurePosition(state.no, marketID).pending += units
	}
}

func ensurePosition(m map[uint64]*assetPosition, marketID uint64) *assetPosition {
	if pos, ok := m[marketID]; ok {
		return pos
	}
	pos := &assetPosition{}
	m[marketID] = pos
	return pos
}

func (m *Manager) shard(walletAddress string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(walletAddress))
	return &m.shards[uint(h.Sum32())%uint(len(m.shards))]
}
