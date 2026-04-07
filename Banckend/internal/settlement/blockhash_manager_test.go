package settlement

import (
	"context"
	"sync"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type blockhashRPCTestDouble struct {
	mu    sync.Mutex
	calls int
}

func (d *blockhashRPCTestDouble) GetLatestBlockhash(ctx context.Context, commitment rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++

	var hash solana.Hash
	hash[0] = byte(d.calls)
	return &rpc.GetLatestBlockhashResult{
		RPCContext: rpc.RPCContext{Context: struct{ Slot uint64 `json:"slot"` }{Slot: uint64(d.calls)}},
		Value: &rpc.LatestBlockhashResult{
			Blockhash:            hash,
			LastValidBlockHeight: uint64(100 + d.calls),
		},
	}, nil
}

func (d *blockhashRPCTestDouble) CallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestBlockhashManagerStaysIdleUntilSnapshotRequested(t *testing.T) {
	rpcDouble := &blockhashRPCTestDouble{}
	manager := NewBlockhashManager(rpcDouble, rpc.CommitmentProcessed, 10*time.Millisecond, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()

	if got := rpcDouble.CallCount(); got != 0 {
		t.Fatalf("expected idle blockhash manager to avoid background RPCs, got %d calls", got)
	}

	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Blockhash == (solana.Hash{}) {
		t.Fatalf("expected snapshot blockhash to be populated")
	}
	if got := rpcDouble.CallCount(); got != 1 {
		t.Fatalf("expected one on-demand RPC call after first snapshot, got %d", got)
	}

	snapshot2, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if snapshot2.Blockhash != snapshot.Blockhash {
		t.Fatalf("expected cached blockhash reuse between snapshots")
	}
	if got := rpcDouble.CallCount(); got != 1 {
		t.Fatalf("expected cached snapshot to avoid extra RPCs, got %d calls", got)
	}
}

func TestBlockhashManagerRefreshesInBackgroundWhileActive(t *testing.T) {
	rpcDouble := &blockhashRPCTestDouble{}
	manager := NewBlockhashManager(rpcDouble, rpc.CommitmentProcessed, 20*time.Millisecond, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)

	if _, err := manager.Snapshot(context.Background()); err != nil {
		t.Fatalf("initial snapshot: %v", err)
	}
	time.Sleep(35 * time.Millisecond)

	if got := rpcDouble.CallCount(); got < 2 {
		t.Fatalf("expected active background refresh to run, got %d calls", got)
	}
}
