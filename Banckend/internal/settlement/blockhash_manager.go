package settlement

import (
	"context"
	"sync"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type blockhashRPC interface {
	GetLatestBlockhash(ctx context.Context, commitment rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error)
}

type blockhashSnapshot struct {
	Blockhash            solana.Hash
	LastValidBlockHeight uint64
	FetchedAt            time.Time
}

type BlockhashManager struct {
	rpc              blockhashRPC
	commitment       rpc.CommitmentType
	pollInterval     time.Duration
	maxCacheAge      time.Duration
	forceRefreshLock sync.Mutex
	startOnce        sync.Once
	activeMu         sync.Mutex
	activeUntil      time.Time
	wakeCh           chan struct{}

	mu       sync.RWMutex
	snapshot blockhashSnapshot
}

func NewBlockhashManager(client blockhashRPC, commitment rpc.CommitmentType, pollInterval time.Duration, maxCacheAge time.Duration) *BlockhashManager {
	if pollInterval <= 0 {
		pollInterval = 15 * time.Second
	}
	if maxCacheAge <= 0 {
		maxCacheAge = 45 * time.Second
	}
	return &BlockhashManager{
		rpc:          client,
		commitment:   commitment,
		pollInterval: pollInterval,
		maxCacheAge:  maxCacheAge,
		wakeCh:       make(chan struct{}, 1),
	}
}

// Start launches an active-only refresh loop.
//
// The manager stays fully idle until Snapshot/ForceRefresh marks the cache as
// active. While new demand keeps arriving, it refreshes in the background on
// pollInterval so submit paths usually hit warm cache.
func (m *BlockhashManager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.startOnce.Do(func() {
		go m.loop(ctx)
	})
}

func (m *BlockhashManager) Snapshot(ctx context.Context) (blockhashSnapshot, error) {
	if m == nil || m.rpc == nil {
		return blockhashSnapshot{}, nil
	}
	m.markActive()
	snapshot := m.snapshotUnsafe()
	if snapshot.Blockhash != (solana.Hash{}) && time.Since(snapshot.FetchedAt) <= m.maxCacheAge {
		return snapshot, nil
	}
	if err := m.refresh(ctx); err != nil {
		return blockhashSnapshot{}, err
	}
	return m.snapshotUnsafe(), nil
}

func (m *BlockhashManager) ForceRefresh(ctx context.Context) (blockhashSnapshot, error) {
	if m == nil || m.rpc == nil {
		return blockhashSnapshot{}, nil
	}
	m.markActive()
	if err := m.refresh(ctx); err != nil {
		return blockhashSnapshot{}, err
	}
	return m.snapshotUnsafe(), nil
}

func (m *BlockhashManager) snapshotUnsafe() blockhashSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot
}

func (m *BlockhashManager) loop(ctx context.Context) {
	for {
		wait := m.nextRefreshDelay()
		if wait < 0 {
			select {
			case <-ctx.Done():
				return
			case <-m.wakeCh:
				continue
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wakeCh:
			timer.Stop()
			continue
		case <-timer.C:
		}
		if !m.isActive() {
			continue
		}
		refreshCtx, cancel := context.WithTimeout(ctx, m.refreshTimeout())
		_ = m.refresh(refreshCtx)
		cancel()
	}
}

func (m *BlockhashManager) markActive() {
	if m == nil {
		return
	}
	window := m.pollInterval * 10
	if window <= 0 {
		window = 150 * time.Second
	}
	m.activeMu.Lock()
	m.activeUntil = time.Now().UTC().Add(window)
	m.activeMu.Unlock()
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

func (m *BlockhashManager) isActive() bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	return time.Now().UTC().Before(m.activeUntil)
}

func (m *BlockhashManager) nextRefreshDelay() time.Duration {
	if !m.isActive() {
		return -1
	}
	snapshot := m.snapshotUnsafe()
	if snapshot.Blockhash == (solana.Hash{}) || snapshot.FetchedAt.IsZero() {
		return 0
	}
	nextAt := snapshot.FetchedAt.Add(m.pollInterval)
	wait := time.Until(nextAt)
	if wait < 0 {
		return 0
	}
	return wait
}

func (m *BlockhashManager) refreshTimeout() time.Duration {
	timeout := m.pollInterval / 2
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	return timeout
}

func (m *BlockhashManager) refresh(ctx context.Context) error {
	m.forceRefreshLock.Lock()
	defer m.forceRefreshLock.Unlock()
	if m == nil || m.rpc == nil {
		return nil
	}
	res, err := m.rpc.GetLatestBlockhash(ctx, m.commitment)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.snapshot = blockhashSnapshot{
		Blockhash:            res.Value.Blockhash,
		LastValidBlockHeight: res.Value.LastValidBlockHeight,
		FetchedAt:            time.Now().UTC(),
	}
	m.mu.Unlock()
	return nil
}
