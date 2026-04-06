package funds

import (
	"encoding/json"
	"testing"

	"blinkpredict/banckend/internal/matching"
)

// ==========================================
// InflightStore 测试
// ==========================================

func TestInflightStore_RegisterAndGet(t *testing.T) {
	store := NewInflightStore()
	entry := &InflightMatch{
		MatchEventID: "evt-001",
		MarketID:     1,
		Phase:        PhasePendingApplied,
	}
	registered := store.Register(entry)
	if !registered {
		t.Fatal("expected register to succeed")
	}
	// 重复注册应幂等
	registered = store.Register(entry)
	if registered {
		t.Fatal("expected duplicate register to return false")
	}
	got, ok := store.Get("evt-001")
	if !ok || got.Phase != PhasePendingApplied {
		t.Fatalf("expected pending_applied, got %v", got)
	}
}

func TestInflightStore_AdvanceToSubmitted(t *testing.T) {
	store := NewInflightStore()
	store.Register(&InflightMatch{MatchEventID: "evt-001", Phase: PhasePendingApplied})

	ok, conflict := store.AdvanceToSubmitted("evt-001", "sig-abc")
	if !ok || conflict {
		t.Fatal("expected advance to submitted succeed")
	}
	got, _ := store.Get("evt-001")
	if got.Phase != PhaseSubmitted {
		t.Fatalf("expected submitted, got %s", got.Phase)
	}
	// 幂等
	ok, conflict = store.AdvanceToSubmitted("evt-001", "sig-abc")
	if !ok || conflict {
		t.Fatal("expected idempotent advance to succeed")
	}
}

func TestInflightStore_AdvanceToTerminal_NormalPath(t *testing.T) {
	raw := json.RawMessage(`{"event_id":"evt-001"}`)
	store := NewInflightStore()
	store.Register(&InflightMatch{
		MatchEventID:  "evt-001",
		Phase:         PhasePendingApplied,
		RawMatchEvent: raw,
	})
	store.AdvanceToSubmitted("evt-001", "sig-abc")

	rawBatch, transitioned, conflict := store.AdvanceToTerminal("evt-001", PhaseConfirmed)
	if !transitioned || conflict {
		t.Fatal("expected terminal advance to succeed")
	}
	if string(rawBatch) != string(raw) {
		t.Fatal("expected raw batch returned")
	}
	got, _ := store.Get("evt-001")
	if got.Phase != PhaseConfirmed {
		t.Fatalf("expected confirmed, got %s", got.Phase)
	}
}

func TestInflightStore_AdvanceToTerminal_SkipSubmitted(t *testing.T) {
	// 测试 pending_applied 直接到 confirmed（submitted 事件丢失场景）
	raw := json.RawMessage(`{"event_id":"evt-002"}`)
	store := NewInflightStore()
	store.Register(&InflightMatch{
		MatchEventID:  "evt-002",
		Phase:         PhasePendingApplied,
		RawMatchEvent: raw,
	})

	rawBatch, transitioned, conflict := store.AdvanceToTerminal("evt-002", PhaseConfirmed)
	if !transitioned || conflict {
		t.Fatal("expected skip-submitted advance to succeed")
	}
	if len(rawBatch) == 0 {
		t.Fatal("expected raw batch")
	}
}

func TestInflightStore_TerminalConflict(t *testing.T) {
	store := NewInflightStore()
	store.Register(&InflightMatch{MatchEventID: "evt-003", Phase: PhaseConfirmed})

	_, _, conflict := store.AdvanceToTerminal("evt-003", PhaseFailed)
	if !conflict {
		t.Fatal("expected conflict when confirmed->failed")
	}
}

func TestInflightStore_PendingTerminal(t *testing.T) {
	store := NewInflightStore()
	pt := &PendingTerminal{
		MatchEventID: "evt-late",
		Phase:        PhaseConfirmed,
		RawEvent:     json.RawMessage(`{}`),
	}
	store.AddPendingTerminal(pt)

	got, ok := store.TakePendingTerminal("evt-late")
	if !ok || got.Phase != PhaseConfirmed {
		t.Fatal("expected pending terminal to be found")
	}
	// 取走后应为空
	_, ok = store.TakePendingTerminal("evt-late")
	if ok {
		t.Fatal("expected pending terminal to be consumed")
	}
}

func TestInflightStore_PendingTerminalPriority(t *testing.T) {
	store := NewInflightStore()
	store.AddPendingTerminal(&PendingTerminal{
		MatchEventID: "evt-priority",
		Phase:        PhaseFailed,
		RawEvent:     json.RawMessage(`{"phase":"failed"}`),
	})
	store.AddPendingTerminal(&PendingTerminal{
		MatchEventID: "evt-priority",
		Phase:        PhaseSubmitted,
		RawEvent:     json.RawMessage(`{"phase":"submitted"}`),
	})

	got, ok := store.TakePendingTerminal("evt-priority")
	if !ok {
		t.Fatal("expected pending terminal to exist")
	}
	if got.Phase != PhaseFailed {
		t.Fatalf("expected failed to keep higher priority than submitted, got %s", got.Phase)
	}

	store.AddPendingTerminal(&PendingTerminal{
		MatchEventID: "evt-priority-2",
		Phase:        PhaseSubmitted,
		RawEvent:     json.RawMessage(`{"phase":"submitted"}`),
	})
	store.AddPendingTerminal(&PendingTerminal{
		MatchEventID: "evt-priority-2",
		Phase:        PhaseConfirmed,
		RawEvent:     json.RawMessage(`{"phase":"confirmed"}`),
	})

	got, ok = store.TakePendingTerminal("evt-priority-2")
	if !ok {
		t.Fatal("expected pending terminal to exist")
	}
	if got.Phase != PhaseConfirmed {
		t.Fatalf("expected confirmed to override submitted, got %s", got.Phase)
	}
}

// ==========================================
// Manager 精确重算测试
// ==========================================

func makeTestBatch(marketPDA string, action, outcome, orderType string, qty, fillPrice uint64) *matching.MatchBatchEvent {
	return &matching.MatchBatchEvent{
		EventID:   "batch-01",
		MarketID:  1,
		MarketPDA: marketPDA,
		Orders: []matching.MatchedOrder{
			{
				OrderIndex: 0,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     "maker-wallet",
					OriginalAction:    action,
					OriginalOutcome:   outcome,
					OriginalPriceTick: uint8(fillPrice),
					OrderType:         orderType,
				},
			},
			{
				OrderIndex: 1,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     "taker-wallet",
					OriginalAction:    action,
					OriginalOutcome:   outcome,
					OriginalPriceTick: uint8(fillPrice),
					OrderType:         orderType,
				},
			},
		},
		Fills: []matching.MatchFill{
			{
				FillIndex:       0,
				MakerOrderIndex: 0,
				TakerOrderIndex: 1,
				FillAmount:      qty,
				FillPrice:       fillPrice,
			},
		},
	}
}

func TestApplySettlementConfirmedByBatch_BuyYes(t *testing.T) {
	m := NewManager()
	const pda = "market-pda-1"
	const wallet = "maker-wallet"

	// 初始化：buy yes，先设置一些 pending 状态
	m.SeedLedger(wallet, UserWallet{
		AvailableUSDC: 0,
		LockedUSDC:    0,
		PendingUSDC:   -60, // 成交时花了 60 units
	})
	m.SeedPosition(wallet, pda, MarketPosition{
		PendingYesShares: 100, // 成交了 100 股等待确认
	})

	// 模拟 buy yes 100 lots @ price=60
	batch := &matching.MatchBatchEvent{
		EventID:   "batch-01",
		MarketID:  1,
		MarketPDA: pda,
		Orders: []matching.MatchedOrder{
			{
				OrderIndex: 0,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     wallet,
					OriginalAction:    "buy",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
			// taker 是 sell 方，另一个 wallet
			{
				OrderIndex: 1,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     "another-wallet",
					OriginalAction:    "sell",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
		},
		Fills: []matching.MatchFill{
			{FillIndex: 0, MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 100, FillPrice: 60},
		},
	}

	if err := m.ApplySettlementConfirmedByBatch(batch); err != nil {
		t.Fatalf("ApplySettlementConfirmedByBatch failed: %v", err)
	}

	pos := m.Position(wallet, pda)
	// pending_yes_shares 应该从 100 减少到 0，available_yes_shares 应该增加 100
	if pos.PendingYesShares != 0 {
		t.Errorf("expected PendingYesShares=0, got %d", pos.PendingYesShares)
	}
	if pos.AvailableYesShares != 100 {
		t.Errorf("expected AvailableYesShares=100, got %d", pos.AvailableYesShares)
	}
}

func TestApplySettlementFailedByBatch_BuyYes(t *testing.T) {
	m := NewManager()
	const pda = "market-pda-1"
	const wallet = "maker-wallet"

	// 初始化：buy yes 成交后的资金状态
	m.SeedLedger(wallet, UserWallet{
		AvailableUSDC: 40, // 限价差额已还回
		LockedUSDC:    0,
		PendingUSDC:   -60, // 实际花了 60（负数表示买入花费）
	})
	m.SeedPosition(wallet, pda, MarketPosition{
		PendingYesShares: 100,
	})

	batch := &matching.MatchBatchEvent{
		EventID:   "batch-fail-01",
		MarketID:  1,
		MarketPDA: pda,
		Orders: []matching.MatchedOrder{
			{
				OrderIndex: 0,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     wallet,
					OriginalAction:    "buy",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
			{
				OrderIndex: 1,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     "another-wallet",
					OriginalAction:    "sell",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
		},
		Fills: []matching.MatchFill{
			{FillIndex: 0, MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 100, FillPrice: 60},
		},
	}

	if err := m.ApplySettlementFailedByBatch(batch); err != nil {
		t.Fatalf("ApplySettlementFailedByBatch failed: %v", err)
	}

	ledger := m.Ledger(wallet)
	pos := m.Position(wallet, pda)

	// pending_yes_shares 应该回到 0
	if pos.PendingYesShares != 0 {
		t.Errorf("expected PendingYesShares=0, got %d", pos.PendingYesShares)
	}
	// 实际花的 60 USDC 应该退回 available
	if ledger.AvailableUSDC != 40+60 {
		t.Errorf("expected AvailableUSDC=100, got %d (should have refunded 60)", ledger.AvailableUSDC)
	}
}

func TestApplySettlementFailedByBatch_SellYes(t *testing.T) {
	m := NewManager()
	const pda = "market-pda-sell"
	const wallet = "seller-wallet"

	// 初始化：sell yes 成交后的资金状态
	m.SeedLedger(wallet, UserWallet{
		AvailableUSDC: 0,
		PendingUSDC:   60, // 卖出产生的 pending USDC
	})
	m.SeedPosition(wallet, pda, MarketPosition{
		LockedYesShares:    0,    // 已从 locked 扣除
		AvailableYesShares: 10,   // 其他仓位
		PendingYesShares:   -100, // 卖出 100 shares（负数）
	})

	batch := &matching.MatchBatchEvent{
		EventID:   "batch-sell-fail-01",
		MarketID:  1,
		MarketPDA: pda,
		Orders: []matching.MatchedOrder{
			{
				OrderIndex: 1,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     wallet,
					OriginalAction:    "sell",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
			{
				OrderIndex: 0,
				Execution: matching.ExecutionSnapshot{
					WalletAddress:     "buyer-wallet",
					OriginalAction:    "buy",
					OriginalOutcome:   "yes",
					OriginalPriceTick: 60,
					OrderType:         "limit",
				},
			},
		},
		Fills: []matching.MatchFill{
			{FillIndex: 0, MakerOrderIndex: 1, TakerOrderIndex: 0, FillAmount: 100, FillPrice: 60},
		},
	}

	if err := m.ApplySettlementFailedByBatch(batch); err != nil {
		t.Fatalf("ApplySettlementFailedByBatch sell failed: %v", err)
	}

	ledger := m.Ledger(wallet)
	pos := m.Position(wallet, pda)

	// pending_usdc 应该回到 0（撤回 60）
	if ledger.PendingUSDC != 0 {
		t.Errorf("expected PendingUSDC=0, got %d", ledger.PendingUSDC)
	}
	// yes 股份应该退回 available
	if pos.AvailableYesShares != 10+100 {
		t.Errorf("expected AvailableYesShares=110, got %d", pos.AvailableYesShares)
	}
}

// ==========================================
// CollectAndClearDirty 测试
// ==========================================

func TestCollectAndClearDirty(t *testing.T) {
	m := NewManager()
	m.SeedLedger("w1", UserWallet{AvailableUSDC: 100})
	m.ApplyDepositConfirmed("w1", 50) // 这个应该设置 Dirty

	// ApplyDepositConfirmed 没有 Dirty 标记（旧代码），手动 SeedLedger + Dirty
	// 改用 ApplyMatchPending 触发 Dirty
	m.SeedLedger("w2", UserWallet{AvailableUSDC: 200, LockedUSDC: 50})
	order := ActiveOrder{
		WalletAddress:     "w2",
		MarketPDA:         "pda-1",
		OriginalAction:    SideBuy,
		OriginalOutcome:   OutcomeYes,
		OriginalPriceTick: 50,
		OrderType:         OrderTypeLimit,
	}
	m.ApplyMatchPending(order, order, 10, 50)

	wallets, _ := m.CollectAndClearDirty()
	if len(wallets) == 0 {
		t.Error("expected dirty wallets")
	}
	// 清除后再次收集应为空
	wallets2, _ := m.CollectAndClearDirty()
	if len(wallets2) != 0 {
		t.Errorf("expected no dirty wallets after clear, got %d", len(wallets2))
	}
}
