package matching

import (
	"sync"
	"time"
)

// MarketActor 是单个市场的并发壳（Actor模型）
type MarketActor struct {
	MarketID   uint64
	MarketPDA  string
	CmdChan    chan *CommandWrapper
	Book       *FixedArrayOrderBook
	Pending    *pendingBatch
	checkpoint matcherCheckpointState
	started    bool
}

// NewMarketActor 创建市场Actor
func NewMarketActor(marketID uint64, marketPDA string) *MarketActor {
	return &MarketActor{
		MarketID:  marketID,
		MarketPDA: marketPDA,
		CmdChan:   make(chan *CommandWrapper, 2048), // 缓冲2048个命令
		Book:      NewFixedArrayOrderBook(marketID),
		Pending:   newPendingBatch(marketID, marketPDA, time.Now().UTC()),
	}
}

type matcherCheckpointState struct {
	mu               sync.Mutex
	pendingEvtSeq    uint64
	pendingSourceSeq uint64
	flushedEvtSeq    uint64
	flushedSourceSeq uint64
	pendingAcks      []*CommandWrapper
}

type matcherCheckpointSnapshot struct {
	evtSeq       uint64
	sourceCmdSeq uint64
}

func (a *MarketActor) stageCheckpoint(evtSeq, sourceCmdSeq uint64, wrappers []*CommandWrapper) {
	if a == nil || sourceCmdSeq == 0 {
		return
	}
	a.checkpoint.mu.Lock()
	defer a.checkpoint.mu.Unlock()
	if evtSeq > a.checkpoint.pendingEvtSeq {
		a.checkpoint.pendingEvtSeq = evtSeq
	}
	if sourceCmdSeq > a.checkpoint.pendingSourceSeq {
		a.checkpoint.pendingSourceSeq = sourceCmdSeq
	}
	a.checkpoint.pendingAcks = append(a.checkpoint.pendingAcks, wrappers...)
}

func (a *MarketActor) checkpointSnapshot() (matcherCheckpointSnapshot, bool) {
	if a == nil {
		return matcherCheckpointSnapshot{}, false
	}
	a.checkpoint.mu.Lock()
	defer a.checkpoint.mu.Unlock()
	if a.checkpoint.pendingSourceSeq == 0 || a.checkpoint.pendingSourceSeq <= a.checkpoint.flushedSourceSeq {
		return matcherCheckpointSnapshot{}, false
	}
	return matcherCheckpointSnapshot{
		evtSeq:       a.checkpoint.pendingEvtSeq,
		sourceCmdSeq: a.checkpoint.pendingSourceSeq,
	}, true
}

func (a *MarketActor) completeCheckpoint(snapshot matcherCheckpointSnapshot) []*CommandWrapper {
	if a == nil || snapshot.sourceCmdSeq == 0 {
		return nil
	}
	a.checkpoint.mu.Lock()
	defer a.checkpoint.mu.Unlock()
	if snapshot.evtSeq > a.checkpoint.flushedEvtSeq {
		a.checkpoint.flushedEvtSeq = snapshot.evtSeq
	}
	if snapshot.sourceCmdSeq > a.checkpoint.flushedSourceSeq {
		a.checkpoint.flushedSourceSeq = snapshot.sourceCmdSeq
	}
	acked := make([]*CommandWrapper, 0, len(a.checkpoint.pendingAcks))
	remaining := make([]*CommandWrapper, 0, len(a.checkpoint.pendingAcks))
	for _, wrapper := range a.checkpoint.pendingAcks {
		if wrapper == nil || wrapper.SourceCmdSeq == 0 || wrapper.SourceCmdSeq <= a.checkpoint.flushedSourceSeq {
			acked = append(acked, wrapper)
			continue
		}
		remaining = append(remaining, wrapper)
	}
	a.checkpoint.pendingAcks = remaining
	return acked
}
