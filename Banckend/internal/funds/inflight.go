package funds

import (
	"encoding/json"
	"sync"
)

// ==========================================
// In-flight 状态常量
// ==========================================

const (
	PhasePendingApplied = "pending_applied"
	PhaseSubmitted      = "submitted"
	PhaseConfirmed      = "confirmed"
	PhaseFailed         = "failed"
)

// ==========================================
// InflightMatch — 单个 match batch 的生命周期窗口
// ==========================================

// InflightMatch 记录一个撮合批次从 pending 到终态的完整状态。
// RawMatchEvent 仅在未终态时保留，用于 confirmed/failed 时精确重算。
type InflightMatch struct {
	MatchEventID         string   `json:"match_event_id"`
	MarketID             uint64   `json:"market_id"`
	MarketPDA            string   `json:"market_pda"`
	Wallets              []string `json:"wallets"`
	Phase                string   `json:"phase"` // pending_applied | submitted | confirmed | failed
	LastEventSeq         uint64   `json:"last_event_seq"`
	SubmittedTxSignature string   `json:"submitted_tx_signature,omitempty"`
	// RawMatchEvent 保存原始 evt.match.execution payload，用于 confirmed/failed 精确重算。
	// 终态 + snapshot 落盘后可安全淘汰，置为 nil。
	RawMatchEvent json.RawMessage `json:"raw_match_event,omitempty"`
}

func (im *InflightMatch) IsTerminal() bool {
	return im.Phase == PhaseConfirmed || im.Phase == PhaseFailed
}

// ==========================================
// PendingTerminal — 乱序时先到的终态事件缓冲
// ==========================================

// PendingTerminal 在 submitted/confirmed/failed 先于对应 match batch 到达时，
// 暂存用于后续补执行（乱序防护）。
type PendingTerminal struct {
	MatchEventID string
	Phase        string          // submitted | confirmed | failed
	RawEvent     json.RawMessage // 原始事件 payload
}

// ==========================================
// InflightStore — 线程安全的 in-flight 状态存储
// ==========================================

// InflightStore 管理所有当前在飞的 match batch 状态。
// 它不是账本，只是 batch 生命周期窗口。
type InflightStore struct {
	mu      sync.RWMutex
	entries map[string]*InflightMatch // key: match_event_id

	termMu          sync.Mutex
	pendingTerminal map[string]*PendingTerminal // key: match_event_id
}

func NewInflightStore() *InflightStore {
	return &InflightStore{
		entries:         make(map[string]*InflightMatch),
		pendingTerminal: make(map[string]*PendingTerminal),
	}
}

// Get 线程安全地获取 inflight 条目（返回拷贝）。
func (s *InflightStore) Get(matchEventID string) (*InflightMatch, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[matchEventID]
	if !ok {
		return nil, false
	}
	// 返回浅拷贝，防止外部修改
	cp := *entry
	return &cp, true
}

// Register 登记一个新的 match batch 为 pending_applied。
// 如果已存在则幂等忽略，返回 false。
func (s *InflightStore) Register(entry *InflightMatch) (registered bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[entry.MatchEventID]; exists {
		return false
	}
	cp := *entry
	s.entries[entry.MatchEventID] = &cp
	return true
}

// AdvanceToSubmitted 把 phase 从 pending_applied 推进到 submitted。
// 如果当前 phase 不是 pending_applied 则幂等（不报错）；
// 如果已是 confirmed/failed 则告警并返回 false。
func (s *InflightStore) AdvanceToSubmitted(matchEventID, txSignature string) (ok bool, conflict bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[matchEventID]
	if !exists {
		return false, false
	}
	if entry.Phase == PhaseConfirmed || entry.Phase == PhaseFailed {
		return false, true // 已终态，冲突
	}
	if entry.Phase == PhaseSubmitted {
		return true, false // 已是 submitted，幂等
	}
	entry.Phase = PhaseSubmitted
	entry.SubmittedTxSignature = txSignature
	return true, false
}

// AdvanceToTerminal 把 phase 推进到 confirmed 或 failed。
// 支持状态跳跃（pending_applied 直接到 confirmed/failed）。
// 返回：(rawBatch, transitioned, conflict)
//   - rawBatch: 原始 match batch，用于重算
//   - transitioned: 是否成功推进
//   - conflict: 是否存在冲突（如 confirmed->failed）
func (s *InflightStore) AdvanceToTerminal(matchEventID, phase string) (rawBatch json.RawMessage, transitioned bool, conflict bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[matchEventID]
	if !exists {
		return nil, false, false
	}
	// 幂等：已是同一终态
	if entry.Phase == phase {
		return entry.RawMatchEvent, false, false
	}
	// 冲突：从一个终态到另一个终态
	if entry.IsTerminal() {
		return nil, false, true
	}
	// 正常推进（含从 pending_applied 直接跳到终态）
	rawBatch = entry.RawMatchEvent
	entry.Phase = phase
	// 终态后 RawMatchEvent 等待 snapshot 落盘再淘汰
	return rawBatch, true, false
}

// EvictTerminal 淘汰已终态且已完成 snapshot 的 batch 原始数据。
// 只清空 RawMatchEvent（以释放内存），不删除条目本身（保留幂等状态）。
func (s *InflightStore) EvictTerminal(snapshotSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.entries {
		if entry.IsTerminal() && entry.LastEventSeq <= snapshotSeq {
			entry.RawMatchEvent = nil
		}
	}
}

// PurgeTerminal 彻底删除已终态且已完成 snapshot 的条目（用于长期运行防内存泄漏）。
func (s *InflightStore) PurgeTerminal(snapshotSeq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, entry := range s.entries {
		if entry.IsTerminal() && entry.LastEventSeq <= snapshotSeq {
			delete(s.entries, id)
		}
	}
}

// AddPendingTerminal 暂存乱序到达的 lifecycle 事件（match batch 尚未 apply 时）。
func (s *InflightStore) AddPendingTerminal(pt *PendingTerminal) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if existing, ok := s.pendingTerminal[pt.MatchEventID]; ok {
		if pendingPhasePriority(existing.Phase) >= pendingPhasePriority(pt.Phase) {
			return
		}
	}
	s.pendingTerminal[pt.MatchEventID] = pt
}

// TakePendingTerminal 取走某个 match_event_id 的暂存终态（取走后删除）。
func (s *InflightStore) TakePendingTerminal(matchEventID string) (*PendingTerminal, bool) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	pt, ok := s.pendingTerminal[matchEventID]
	if !ok {
		return nil, false
	}
	delete(s.pendingTerminal, matchEventID)
	return pt, true
}

// Snapshot 返回当前 inflight 内容的深拷贝，用于持久化快照。
func (s *InflightStore) Snapshot() []*InflightMatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InflightMatch, 0, len(s.entries))
	for _, entry := range s.entries {
		cp := *entry
		if len(entry.RawMatchEvent) > 0 {
			raw := make(json.RawMessage, len(entry.RawMatchEvent))
			copy(raw, entry.RawMatchEvent)
			cp.RawMatchEvent = raw
		}
		out = append(out, &cp)
	}
	return out
}

func (s *InflightStore) PendingSnapshot() []*PendingTerminal {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	out := make([]*PendingTerminal, 0, len(s.pendingTerminal))
	for _, entry := range s.pendingTerminal {
		cp := *entry
		if len(entry.RawEvent) > 0 {
			raw := make(json.RawMessage, len(entry.RawEvent))
			copy(raw, entry.RawEvent)
			cp.RawEvent = raw
		}
		out = append(out, &cp)
	}
	return out
}

// RestoreFromSnapshot 从快照数据恢复 inflight 状态（冷启动用）。
func (s *InflightStore) RestoreFromSnapshot(entries []*InflightMatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*InflightMatch, len(entries))
	for _, entry := range entries {
		cp := *entry
		s.entries[entry.MatchEventID] = &cp
	}
}

func (s *InflightStore) RestorePendingTerminals(entries []*PendingTerminal) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	s.pendingTerminal = make(map[string]*PendingTerminal, len(entries))
	for _, entry := range entries {
		cp := *entry
		s.pendingTerminal[entry.MatchEventID] = &cp
	}
}

func pendingPhasePriority(phase string) int {
	switch phase {
	case PhaseConfirmed:
		return 3
	case PhaseFailed:
		return 2
	case PhaseSubmitted:
		return 1
	default:
		return 0
	}
}
