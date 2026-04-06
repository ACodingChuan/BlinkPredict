package funds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

var serviceLogger = logging.New("funds")

const (
	// defaultDispatchConsumerName is the durable consumer for evt.match.execution.* (AP_EVT stream)
	defaultDispatchConsumerName = "funds-execution"
	// defaultCmdConsumerName is the durable consumer for cmd.order.submit (AP_CMD stream)
	defaultCmdConsumerName = "funds-submit"
	// defaultSettlementConsumerName is the durable consumer for evt.settlement.> (AP_EVT stream)
	defaultSettlementConsumerName = "funds-settlement"
	// defaultDepositConsumerName is the durable consumer for evt.deposit.confirmed (AP_EVT stream)
	defaultDepositConsumerName = "funds-deposit"
	// defaultReleasedConsumerName is the durable consumer for evt.order.released (AP_EVT stream)
	defaultReleasedConsumerName = "funds-released"
	// defaultReservedConsumerName is the durable consumer for evt.order.reserved.* (AP_EVT stream)
	defaultReservedConsumerName = "funds-reserved"
	// defaultRejectedConsumerName is the durable consumer for evt.order.reserve_rejected.* (AP_EVT stream)
	defaultRejectedConsumerName = "funds-reserve-rejected"
	dispatchFetchBatch          = 64
	dispatchMaxWait             = 1500 * time.Millisecond
)

// Service 是 funds 模块的唯一入口，统一 dispatcher 驱动所有权威事件。
//
// 架构说明：
//   - 多个独立 Pull Consumer，分别订阅 CMD stream 和 EVT stream 上的不同 subject
//   - cmdSub: cmd.order.submit (AP_CMD stream)
//   - evtSub: evt.match.execution.* (AP_EVT stream)
//   - settlementSub: evt.settlement.> (AP_EVT stream)
//   - depositSub: evt.deposit.confirmed (AP_EVT stream)
//   - releasedSub: evt.order.released.* (AP_EVT stream)
//   - 幂等通过 InflightStore/SubmitStore/DepositStore 实现，重启可从 PG recovery state 恢复
//   - 资金状态热路径不等待 DB，立即 Ack，由 Projector 异步刷盘
type Service struct {
	client        *natsjs.Client
	pool          *pgxpool.Pool
	rdb           *redis.Client
	manager       *Manager
	inflight      *InflightStore
	submits       *SubmitStore
	deposits      *DepositStore
	pending       *PendingReserveStore
	projector     *Projector
	consumerName  string
	stateMu       sync.RWMutex
	evtSeq        atomic.Uint64
	sub           *nats.Subscription // EVT stream: match execution
	cmdSub        *nats.Subscription // CMD stream: order submit
	settlementSub *nats.Subscription // EVT stream: settlement lifecycle
	depositSub    *nats.Subscription // EVT stream: deposit confirmed
	releasedSub   *nats.Subscription // EVT stream: order released
	reservedSub   *nats.Subscription // EVT stream: order reserved
	rejectedSub   *nats.Subscription // EVT stream: order reserve rejected
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, rdb *redis.Client, manager *Manager) *Service {
	if manager == nil {
		manager = NewManager()
	}
	inflight := NewInflightStore()
	submits := NewSubmitStore()
	deposits := NewDepositStore()
	pending := NewPendingReserveStore()
	svc := &Service{
		client:       client,
		pool:         pool,
		rdb:          rdb,
		manager:      manager,
		inflight:     inflight,
		submits:      submits,
		deposits:     deposits,
		pending:      pending,
		consumerName: defaultDispatchConsumerName,
	}
	svc.projector = NewProjector(manager, inflight, submits, deposits, pending, pool, rdb, &svc.stateMu, &svc.evtSeq)
	return svc
}

func (s *Service) Manager() *Manager {
	return s.manager
}

func (s *Service) Inflight() *InflightStore {
	return s.inflight
}

// Start 启动 funds 服务：
//  1. 从 Postgres projection + recovery checkpoint 恢复内存。
//  2. 从 AP_EVT 回放 checkpoint 之后的缺口。
//  3. 成功后写入一次 recovery checkpoint，再进入 live consumption。
func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.manager == nil {
		return nil
	}
	if err := s.recover(ctx); err != nil {
		return err
	}
	if s.projector != nil {
		if err := s.projector.FlushNow(ctx); err != nil {
			return err
		}
	}
	// 建立 pull consumer 订阅
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	go s.dispatchLoop(ctx)           // EVT: match execution
	go s.reservedDispatchLoop(ctx)   // EVT: order reserved
	go s.rejectedDispatchLoop(ctx)   // EVT: order reserve rejected
	go s.cmdDispatchLoop(ctx)        // CMD: order submit
	go s.settlementDispatchLoop(ctx) // EVT: settlement lifecycle
	go s.depositDispatchLoop(ctx)    // EVT: deposit confirmed
	go s.releasedDispatchLoop(ctx)   // EVT: order released
	// 启动异步投影器
	go s.projector.Run(ctx)
	return nil
}

// recover 冷启动恢复：
//   - wallet_accounts / positions 提供权威基线。
//   - funds_recovery_state 提供 inflight / submits / deposits / evt checkpoint。
//   - 若 recovery checkpoint 尚不存在，则把当前 DB 视图作为基线并从当前 AP_EVT 尾部开始。
func (s *Service) recover(ctx context.Context) error {
	if err := s.recoverFromPostgres(ctx); err != nil {
		return err
	}
	state, err := loadRecoveryState(ctx, s.pool)
	if err != nil {
		return err
	}
	if state != nil {
		if err := s.restoreRecoveryState(state); err != nil {
			return err
		}
		return s.replayEventLog(ctx, state.LastFlushedEventSeq+1)
	}
	return s.seedCurrentEventSeq()
}

// recoverFromPostgres 从数据库读取 wallet/position 初始化内存基线。
func (s *Service) recoverFromPostgres(ctx context.Context) error {
	if s.pool == nil || s.manager == nil {
		return nil
	}
	wallets := make(map[string]struct{})
	rows, err := s.pool.Query(ctx, `
		SELECT wallet_address,
		       collateral_total_units,
		       collateral_free_units,
		       collateral_locked_units,
		       collateral_pending_units
		FROM wallet_accounts
	`)
	if err != nil {
		return fmt.Errorf("funds load wallet_accounts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			walletAddress string
			totalUnits    int64
			freeUnits     int64
			lockedUnits   int64
			pendingUnits  int64
		)
		if err := rows.Scan(&walletAddress, &totalUnits, &freeUnits, &lockedUnits, &pendingUnits); err != nil {
			return fmt.Errorf("funds scan wallet_accounts: %w", err)
		}
		if freeUnits < 0 {
			freeUnits = 0
		}
		if lockedUnits < 0 {
			lockedUnits = 0
		}
		wallets[walletAddress] = struct{}{}
		s.manager.SeedLedger(walletAddress, UserWallet{
			AvailableUSDC: uint64(freeUnits),
			LockedUSDC:    uint64(lockedUnits),
			PendingUSDC:   pendingUnits,
		})
		s.manager.MarkLedgerDirty(walletAddress)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("funds iterate wallet_accounts: %w", err)
	}

	positionRows, err := s.pool.Query(ctx, `
		SELECT
			p.wallet_address,
			p.market_id,
			COALESCE(p.market_pda, ''),
			p.yes_free_lots,
			p.yes_locked_lots,
			p.yes_pending_lots,
			p.no_free_lots,
			p.no_locked_lots,
			p.no_pending_lots,
			p.collateral_locked_units
		FROM positions p
	`)
	if err != nil {
		return fmt.Errorf("funds load positions: %w", err)
	}
	defer positionRows.Close()

	for positionRows.Next() {
		var (
			walletAddress         string
			marketIDText          string
			marketPDA             string
			yesFreeLots           int64
			yesLockedLots         int64
			yesPendingLots        int64
			noFreeLots            int64
			noLockedLots          int64
			noPendingLots         int64
			collateralLockedUnits int64
		)
		if err := positionRows.Scan(
			&walletAddress,
			&marketIDText,
			&marketPDA,
			&yesFreeLots,
			&yesLockedLots,
			&yesPendingLots,
			&noFreeLots,
			&noLockedLots,
			&noPendingLots,
			&collateralLockedUnits,
		); err != nil {
			return fmt.Errorf("funds scan positions: %w", err)
		}
		marketID := uint64(0)
		if parsed, parseErr := strconv.ParseUint(strings.TrimSpace(marketIDText), 10, 64); parseErr == nil {
			marketID = parsed
		}
		wallets[walletAddress] = struct{}{}
		s.manager.SeedPosition(walletAddress, marketPDA, MarketPosition{
			MarketID:              marketID,
			AvailableYesShares:    clampNonNegative(yesFreeLots),
			LockedYesShares:       clampNonNegative(yesLockedLots),
			PendingYesShares:      clampSigned(yesPendingLots),
			AvailableNoShares:     clampNonNegative(noFreeLots),
			LockedNoShares:        clampNonNegative(noLockedLots),
			PendingNoShares:       clampSigned(noPendingLots),
			CollateralLockedUnits: clampNonNegative(collateralLockedUnits),
		})
		s.manager.MarkPositionDirty(walletAddress, marketPDA)
	}
	if err := positionRows.Err(); err != nil {
		return fmt.Errorf("funds iterate positions: %w", err)
	}
	serviceLogger.Infof("funds: recovered from postgres wallets=%d", len(wallets))
	return nil
}

func (s *Service) restoreRecoveryState(state *RecoveryState) error {
	if state == nil {
		return nil
	}
	s.inflight.RestoreFromSnapshot(state.Inflight)
	s.inflight.RestorePendingTerminals(state.PendingTerminals)
	if s.submits != nil {
		s.submits.Restore(state.ProcessedSubmits)
	}
	if s.deposits != nil {
		s.deposits.Restore(state.ProcessedDeposits)
	}
	if s.pending != nil {
		s.pending.Restore(state.PendingReserves)
	}
	s.evtSeq.Store(state.LastFlushedEventSeq)
	serviceLogger.Infof("funds: restored pg recovery state seq=%d inflight=%d pending_terminals=%d pending_reserves=%d submits=%d deposits=%d",
		state.LastFlushedEventSeq, len(state.Inflight), len(state.PendingTerminals), len(state.PendingReserves), len(state.ProcessedSubmits), len(state.ProcessedDeposits))
	return nil
}

func (s *Service) replayEventLog(ctx context.Context, startSeq uint64) error {
	if s.client == nil {
		return nil
	}
	if startSeq == 0 {
		startSeq = 1
	}
	endSeq, err := s.client.StreamLastSeq("evt.>")
	if err != nil {
		return fmt.Errorf("funds get evt stream tail: %w", err)
	}
	if endSeq == 0 || startSeq > endSeq {
		serviceLogger.Infof("funds: replay skipped start_seq=%d end_seq=%d", startSeq, endSeq)
		if endSeq > s.evtSeq.Load() {
			s.evtSeq.Store(endSeq)
		}
		return nil
	}
	sub, err := s.client.ReplaySubscribe("evt.>", startSeq)
	if err != nil {
		return fmt.Errorf("funds replay subscribe: %w", err)
	}
	defer func() {
		_ = sub.Unsubscribe()
	}()

	replayed := 0
	for {
		msgs, fetchErr := sub.Fetch(dispatchFetchBatch, nats.MaxWait(dispatchMaxWait))
		if fetchErr != nil {
			if errors.Is(fetchErr, nats.ErrTimeout) || errors.Is(fetchErr, context.DeadlineExceeded) {
				if s.evtSeq.Load() >= endSeq {
					break
				}
				continue
			}
			return fmt.Errorf("funds replay fetch: %w", fetchErr)
		}
		for _, msg := range msgs {
			s.dispatchMessage(ctx, msg)
			replayed++
			if seq, ok := eventSeqFromMsg(msg); ok && seq >= endSeq {
				serviceLogger.Infof("funds: replay completed start_seq=%d end_seq=%d count=%d", startSeq, endSeq, replayed)
				return nil
			}
		}
	}
	serviceLogger.Infof("funds: replay completed start_seq=%d end_seq=%d count=%d", startSeq, endSeq, replayed)
	return nil
}

func (s *Service) seedCurrentEventSeq() error {
	if s.client == nil {
		return nil
	}
	lastSeq, err := s.client.StreamLastSeq("evt.>")
	if err != nil {
		return fmt.Errorf("funds get evt tail seq: %w", err)
	}
	s.evtSeq.Store(lastSeq)
	serviceLogger.Infof("funds: seeded current evt seq=%d", lastSeq)
	return nil
}

// ensureSubscription 建立独立 JetStream Pull Consumer 订阅：
//  1. EVT stream: match execution
//  2. CMD stream: order submit command
//  3. EVT stream: settlement lifecycle
//  4. EVT stream: deposit confirmed
//  5. EVT stream: order released
//
// 各 consumer 独立 goroutine 驱动，互不阻塞，共享同一个内存状态机（Manager）。
// 状态机内部通过 ShardedWorker 做 wallet 级串行化，无全局锁。
func (s *Service) ensureSubscription() error {
	// EVT consumer: execution batches
	if s.sub == nil {
		subjectFilter := protocol.SubjectMatchExecution + ".*"
		sub, err := s.client.PullSubscribe(subjectFilter, s.consumerName)
		if err != nil {
			return fmt.Errorf("funds pull subscribe execution stream: %w", err)
		}
		s.sub = sub
	}
	// CMD consumer: cmd.order.submit
	if s.cmdSub == nil {
		cmdSub, err := s.client.PullSubscribe(protocol.SubjectOrderSubmit, defaultCmdConsumerName)
		if err != nil {
			// cmd stream may use a different stream name; log warning but don't hard-fail
			serviceLogger.Warnf("funds: cmd pull subscribe failed (cmd stream may not exist yet): %v", err)
		} else {
			s.cmdSub = cmdSub
		}
	}
	// Settlement consumer: evt.settlement.>
	if s.settlementSub == nil {
		settlementSub, err := s.client.PullSubscribe("evt.settlement.>", defaultSettlementConsumerName)
		if err != nil {
			serviceLogger.Warnf("funds: settlement pull subscribe failed: %v", err)
		} else {
			s.settlementSub = settlementSub
		}
	}
	// Deposit consumer: evt.deposit.confirmed
	if s.depositSub == nil {
		depositSub, err := s.client.PullSubscribe(protocol.SubjectDepositConfirmed, defaultDepositConsumerName)
		if err != nil {
			serviceLogger.Warnf("funds: deposit pull subscribe failed: %v", err)
		} else {
			s.depositSub = depositSub
		}
	}
	// Released consumer: evt.order.released.*
	if s.releasedSub == nil {
		releasedSub, err := s.client.PullSubscribe(protocol.SubjectOrderReleased+".*", defaultReleasedConsumerName)
		if err != nil {
			serviceLogger.Warnf("funds: released pull subscribe failed: %v", err)
		} else {
			s.releasedSub = releasedSub
		}
	}
	// Reserved consumer: evt.order.reserved.*
	if s.reservedSub == nil {
		reservedSub, err := s.client.PullSubscribe(protocol.SubjectOrderReserved+".*", defaultReservedConsumerName)
		if err != nil {
			serviceLogger.Warnf("funds: reserved pull subscribe failed: %v", err)
		} else {
			s.reservedSub = reservedSub
		}
	}
	// Rejected consumer: evt.order.reserve_rejected.*
	if s.rejectedSub == nil {
		rejectedSub, err := s.client.PullSubscribe(protocol.SubjectOrderReserveRejected+".*", defaultRejectedConsumerName)
		if err != nil {
			serviceLogger.Warnf("funds: reserve rejected pull subscribe failed: %v", err)
		} else {
			s.rejectedSub = rejectedSub
		}
	}
	return nil
}

// dispatchLoop 消费 evt.match.execution.*
func (s *Service) dispatchLoop(ctx context.Context) {
	defer func() {
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	}()
	s.runFetchLoop(ctx, s.sub, "Match")
}

// cmdDispatchLoop 消费 CMD stream（cmd.order.submit）
func (s *Service) cmdDispatchLoop(ctx context.Context) {
	if s.cmdSub == nil {
		return
	}
	defer func() {
		_ = s.cmdSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.cmdSub, "CMD")
}

// reservedDispatchLoop 消费 evt.order.reserved.*
func (s *Service) reservedDispatchLoop(ctx context.Context) {
	if s.reservedSub == nil {
		return
	}
	defer func() {
		_ = s.reservedSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.reservedSub, "Reserved")
}

// rejectedDispatchLoop 消费 evt.order.reserve_rejected.*
func (s *Service) rejectedDispatchLoop(ctx context.Context) {
	if s.rejectedSub == nil {
		return
	}
	defer func() {
		_ = s.rejectedSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.rejectedSub, "Rejected")
}

// settlementDispatchLoop 消费 evt.settlement.>
func (s *Service) settlementDispatchLoop(ctx context.Context) {
	if s.settlementSub == nil {
		return
	}
	defer func() {
		_ = s.settlementSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.settlementSub, "Settlement")
}

// depositDispatchLoop 消费 evt.deposit.confirmed
func (s *Service) depositDispatchLoop(ctx context.Context) {
	if s.depositSub == nil {
		return
	}
	defer func() {
		_ = s.depositSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.depositSub, "Deposit")
}

// releasedDispatchLoop 消费 evt.order.released.*
func (s *Service) releasedDispatchLoop(ctx context.Context) {
	if s.releasedSub == nil {
		return
	}
	defer func() {
		_ = s.releasedSub.Unsubscribe()
	}()
	s.runFetchLoop(ctx, s.releasedSub, "Released")
}

// runFetchLoop 通用 pull consumer fetch 循环，按 subject 路由到 dispatchMessage。
func (s *Service) runFetchLoop(ctx context.Context, sub *nats.Subscription, label string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgs, err := sub.Fetch(dispatchFetchBatch, nats.MaxWait(dispatchMaxWait))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			serviceLogger.Warnf("funds %s dispatch fetch failed: %v", label, err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			s.dispatchMessage(ctx, msg)
		}
	}
}

// dispatchMessage 按 subject 路由单条消息到对应处理函数。
func (s *Service) dispatchMessage(ctx context.Context, msg *nats.Msg) {
	subj := msg.Subject

	switch {
	case strings.HasPrefix(subj, protocol.SubjectOrderSubmit):
		s.handleSubmitMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectOrderReserved):
		s.handleReservedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectOrderReserveRejected):
		s.handleReserveRejectedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectMatchExecution):
		s.handleMatchBatchMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectOrderReleased):
		s.handleOrderReleasedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectSettlementSubmitted):
		s.handleSettlementSubmittedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectSettlementConfirmed):
		s.handleSettlementConfirmedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectSettlementFailed):
		s.handleSettlementFailedMessage(ctx, msg)
	case strings.HasPrefix(subj, protocol.SubjectDepositConfirmed):
		s.handleDepositConfirmedMessage(ctx, msg)
	default:
		serviceLogger.Warnf("funds dispatch: unknown subject %s, skipping", subj)
		s.ack(msg)
	}
}

// ==========================================
// 事件处理函数
// ==========================================

func (s *Service) handleSubmitMessage(_ context.Context, msg *nats.Msg) {
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		serviceLogger.Warnf("funds decode submit failed: %v", err)
		_ = msg.Term()
		return
	}
	cmd := env.Payload
	if strings.TrimSpace(cmd.CommandID) == "" {
		_ = msg.Term()
		return
	}
	submitKey := submitKeyFromCommand(cmd)
	if s.submits != nil && submitKey != "" && s.submits.Has(submitKey) {
		s.ack(msg)
		return
	}
	if s.pending != nil && submitKey != "" && s.pending.Has(submitKey) {
		s.ack(msg)
		return
	}
	reserveInput := ReserveOrderInput{
		WalletAddress:     cmd.Execution.WalletAddress,
		MarketID:          cmd.MarketID,
		MarketPDA:         cmd.MarketPDA,
		OriginalAction:    mapAction(cmd.Execution.OriginalAction),
		OriginalOutcome:   mapOutcome(cmd.Execution.OriginalOutcome),
		OriginalPriceTick: cmd.Execution.OriginalPriceTick,
		OrderType:         mapOrderType(cmd.Execution.OrderType),
		QtyLots:           cmd.Execution.QtyLots,
		SpendAmount:       cmd.Execution.SpendAmount,
	}

	var (
		ledger   UserWallet
		position MarketPosition
	)
	s.stateMu.RLock()
	snap := s.manager.Snapshot(cmd.Execution.WalletAddress, cmd.MarketPDA)
	ledger = snap.Ledger
	position = snap.Position
	if s.pending != nil {
		ledger, position = s.pending.Apply(cmd.Execution.WalletAddress, cmd.MarketPDA, ledger, position)
	}
	err := validateReserveAgainstSnapshot(ledger, position, reserveInput)
	s.stateMu.RUnlock()

	if err != nil {
		rejected := protocol.OrderReserveRejectedEvent{
			CommandID:      cmd.CommandID,
			TraceID:        cmd.TraceID,
			IdempotencyKey: cmd.IdempotencyKey,
			MarketID:       cmd.MarketID,
			MarketPDA:      cmd.MarketPDA,
			OrderID:        cmd.Execution.OrderID,
			WalletAddress:  cmd.Execution.WalletAddress,
			ReasonCode:     "insufficient_balance",
			ReasonMessage:  err.Error(),
			CreatedAt:      env.CreatedAt.UTC().Unix(),
		}
		if pubErr := s.client.PublishJSON(context.Background(), protocol.SubjectOrderReserveRejectedMarket(cmd.MarketID), env.IdempotencyKey, rejected); pubErr != nil {
			serviceLogger.Warnf("funds publish reserve rejection failed market=%d err=%v", cmd.MarketID, pubErr)
			_ = msg.NakWithDelay(time.Second)
			return
		}
		s.ack(msg)
		return
	}

	if err := s.client.PublishJSON(context.Background(), protocol.SubjectOrderReservedMarket(cmd.MarketID), env.IdempotencyKey, env); err != nil {
		serviceLogger.Warnf("funds publish reserved event failed market=%d err=%v", cmd.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if s.pending != nil && submitKey != "" {
		pending := pendingReserveFromInput(reserveInput)
		pending.SubmitKey = submitKey
		s.pending.Mark(pending)
		if s.projector != nil {
			s.projector.MarkRecoveryDirty()
		}
	}
	s.ack(msg)
}

func (s *Service) handleReservedMessage(_ context.Context, msg *nats.Msg) {
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		serviceLogger.Warnf("funds decode reserved failed: %v", err)
		_ = msg.Term()
		return
	}
	cmd := env.Payload
	submitKey := submitKeyFromCommand(cmd)
	if s.submits != nil && submitKey != "" && s.submits.Has(submitKey) {
		if s.pending != nil && submitKey != "" {
			s.pending.Delete(submitKey)
		}
		s.ack(msg)
		return
	}
	s.stateMu.RLock()
	if err := s.manager.ReserveOrder(ReserveOrderInput{
		WalletAddress:     cmd.Execution.WalletAddress,
		MarketID:          cmd.MarketID,
		MarketPDA:         cmd.MarketPDA,
		OriginalAction:    mapAction(cmd.Execution.OriginalAction),
		OriginalOutcome:   mapOutcome(cmd.Execution.OriginalOutcome),
		OriginalPriceTick: cmd.Execution.OriginalPriceTick,
		OrderType:         mapOrderType(cmd.Execution.OrderType),
		QtyLots:           cmd.Execution.QtyLots,
		SpendAmount:       cmd.Execution.SpendAmount,
	}); err != nil {
		serviceLogger.Warnf("funds replay reserved apply failed order=%d command=%s err=%v",
			cmd.Execution.OrderID, cmd.CommandID, err)
		if s.pending != nil && submitKey != "" {
			s.pending.Delete(submitKey)
		}
		if s.submits != nil && submitKey != "" {
			s.submits.Mark(submitKey)
		}
		s.ack(msg)
		s.stateMu.RUnlock()
		return
	}
	if s.submits != nil {
		s.submits.Mark(submitKey)
	}
	if s.pending != nil && submitKey != "" {
		s.pending.Delete(submitKey)
	}
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleReserveRejectedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.OrderReserveRejectedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode reserve rejected failed: %v", err)
		_ = msg.Term()
		return
	}
	submitKey := submitKeyFromReserveRejected(event)
	s.stateMu.RLock()
	if s.pending != nil && submitKey != "" {
		s.pending.Delete(submitKey)
	}
	if s.submits != nil {
		s.submits.Mark(submitKey)
	}
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleMatchBatchMessage(_ context.Context, msg *nats.Msg) {
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode match batch failed: %v", err)
		_ = msg.Term()
		return
	}
	matchEventID := strings.TrimSpace(event.EventID)
	if matchEventID == "" {
		serviceLogger.Warnf("funds match batch missing event_id market=%d", event.MarketID)
		_ = msg.Term()
		return
	}
	// 幂等检查
	if _, exists := s.inflight.Get(matchEventID); exists {
		s.ack(msg)
		return
	}
	s.stateMu.RLock()
	rawBatch := make(json.RawMessage, len(msg.Data))
	copy(rawBatch, msg.Data)
	lastEventSeq := uint64(0)
	if meta, metaErr := msg.Metadata(); metaErr == nil {
		lastEventSeq = meta.Sequence.Stream
	}
	if registered := s.inflight.Register(&InflightMatch{
		MatchEventID:  matchEventID,
		MarketID:      event.MarketID,
		MarketPDA:     event.MarketPDA,
		Wallets:       collectMatchBatchWallets(event),
		Phase:         PhasePendingApplied,
		LastEventSeq:  lastEventSeq,
		RawMatchEvent: rawBatch,
	}); !registered {
		s.ack(msg)
		s.stateMu.RUnlock()
		return
	}
	orderByIndex := make(map[uint16]matching.MatchedOrder, len(event.Orders))
	for _, order := range event.Orders {
		orderByIndex[order.OrderIndex] = order
	}
	for _, fill := range event.Fills {
		maker, makerOK := orderByIndex[fill.MakerOrderIndex]
		taker, takerOK := orderByIndex[fill.TakerOrderIndex]
		if !makerOK || !takerOK {
			serviceLogger.Warnf("funds match batch references missing order index market=%d fill=%d", event.MarketID, fill.FillIndex)
			continue
		}
		s.manager.ApplyMatchPending(
			activeOrderFromExecution(maker.Execution, event.MarketID, event.MarketPDA, fill.FillAmount),
			activeOrderFromExecution(taker.Execution, event.MarketID, event.MarketPDA, fill.FillAmount),
			fill.FillAmount,
			uint8(fill.FillPrice),
		)
	}
	for _, update := range event.OrderUpdates {
		if update.Status != "canceled" && update.Status != "expired" && update.Status != "rejected" {
			continue
		}
		order, ok := orderByIndex[update.OrderIndex]
		if !ok {
			serviceLogger.Warnf("funds order update references missing order index market=%d idx=%d", event.MarketID, update.OrderIndex)
			continue
		}
		s.manager.ReleaseOrder(activeOrderFromExecution(order.Execution, event.MarketID, event.MarketPDA, update.RemainingQtyLots), update.RefundAmount)
	}
	if pt, ok := s.inflight.TakePendingTerminal(matchEventID); ok {
		serviceLogger.Infof("funds: applying pending lifecycle for match_event_id=%s phase=%s", matchEventID, pt.Phase)
		s.applyPendingLifecycle(matchEventID, pt)
	}
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleSettlementSubmittedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.SettlementSubmittedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode settlement submitted failed: %v", err)
		_ = msg.Term()
		return
	}
	matchEventID := strings.TrimSpace(event.MatchEventID)
	if matchEventID == "" {
		_ = msg.Term()
		return
	}
	s.stateMu.RLock()
	if _, exists := s.inflight.Get(matchEventID); !exists {
		serviceLogger.Warnf("funds: settlement submitted but no inflight for match_event_id=%s, storing as pending terminal", matchEventID)
		rawEvent := make(json.RawMessage, len(msg.Data))
		copy(rawEvent, msg.Data)
		s.inflight.AddPendingTerminal(&PendingTerminal{
			MatchEventID: matchEventID,
			Phase:        PhaseSubmitted,
			RawEvent:     rawEvent,
		})
		s.ack(msg)
		s.stateMu.RUnlock()
		return
	}
	ok, conflict := s.inflight.AdvanceToSubmitted(matchEventID, event.TxSignature)
	if conflict {
		serviceLogger.Warnf("funds: settlement submitted conflict match_event_id=%s (already terminal)", matchEventID)
	} else if !ok {
		serviceLogger.Warnf("funds: settlement submitted not found or already submitted match_event_id=%s", matchEventID)
	}
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleSettlementConfirmedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.SettlementConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode settlement confirmed failed: %v", err)
		_ = msg.Term()
		return
	}
	matchEventID := strings.TrimSpace(event.MatchEventID)
	if matchEventID == "" {
		_ = msg.Term()
		return
	}
	rawEvent := make(json.RawMessage, len(msg.Data))
	copy(rawEvent, msg.Data)
	s.stateMu.RLock()
	s.applyTerminal(matchEventID, PhaseConfirmed, rawEvent)
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleSettlementFailedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.SettlementFailedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode settlement failed event failed: %v", err)
		_ = msg.Term()
		return
	}
	matchEventID := strings.TrimSpace(event.MatchEventID)
	if matchEventID == "" {
		_ = msg.Term()
		return
	}
	rawEvent := make(json.RawMessage, len(msg.Data))
	copy(rawEvent, msg.Data)
	s.stateMu.RLock()
	s.applyTerminal(matchEventID, PhaseFailed, rawEvent)
	s.ack(msg)
	s.stateMu.RUnlock()
}

// applyTerminal 是 confirmed/failed 的公共处理路径。
// 支持状态跳跃（pending_applied 直接到 confirmed/failed）。
func (s *Service) applyTerminal(matchEventID, phase string, rawEvent json.RawMessage) {
	rawBatch, transitioned, conflict := s.inflight.AdvanceToTerminal(matchEventID, phase)
	if conflict {
		serviceLogger.Warnf("funds: terminal conflict match_event_id=%s phase=%s (already in conflicting terminal)", matchEventID, phase)
		return
	}
	if !transitioned {
		if _, exists := s.inflight.Get(matchEventID); !exists {
			serviceLogger.Warnf("funds: terminal for unknown match_event_id=%s phase=%s, queueing as pending", matchEventID, phase)
			s.inflight.AddPendingTerminal(&PendingTerminal{
				MatchEventID: matchEventID,
				Phase:        phase,
				RawEvent:     rawEvent,
			})
		}
		return
	}
	if len(rawBatch) == 0 {
		serviceLogger.Warnf("funds: terminal but raw batch already evicted match_event_id=%s phase=%s", matchEventID, phase)
		return
	}
	// 从原始 batch 重算
	var batchEvent matching.MatchBatchEvent
	if err := json.Unmarshal(rawBatch, &batchEvent); err != nil {
		serviceLogger.Warnf("funds: decode raw batch for terminal failed match_event_id=%s err=%v", matchEventID, err)
		return
	}
	var err error
	switch phase {
	case PhaseConfirmed:
		err = s.manager.ApplySettlementConfirmedByBatch(&batchEvent)
	case PhaseFailed:
		err = s.manager.ApplySettlementFailedByBatch(&batchEvent)
	}
	if err != nil {
		serviceLogger.Warnf("funds: apply terminal failed match_event_id=%s phase=%s err=%v", matchEventID, phase, err)
	} else {
		serviceLogger.Infof("funds: applied terminal match_event_id=%s phase=%s market_id=%d", matchEventID, phase, batchEvent.MarketID)
	}
}

// handleOrderReleasedMessage 处理 evt.order.released 事件。
// 当 matcher 的订单因撤销/过期/拒绝而终结时，由 matcher 单独发布此事件，
// funds 消费后释放对应账户的 locked 资产回到 available。
// 注意：若同一订单已在 handleMatchBatchMessage 的 OrderUpdates 中处理过，
// manager.ReleaseOrder 内部通过余额边界保护（≥0 检查）确保幂等安全。
func (s *Service) handleOrderReleasedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.OrderReleasedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode OrderReleased failed: %v", err)
		_ = msg.Term()
		return
	}
	if event.OrderID == 0 || strings.TrimSpace(event.WalletAddress) == "" {
		s.ack(msg)
		return
	}
	ao := ActiveOrder{
		WalletAddress:     event.WalletAddress,
		MarketID:          event.MarketID,
		MarketPDA:         event.MarketPDA,
		OriginalAction:    mapAction(event.OriginalAction),
		OriginalOutcome:   mapOutcome(event.OriginalOutcome),
		OriginalPriceTick: event.OriginalPriceTick,
		OrderType:         mapOrderType(event.OrderType),
		RemainingQty:      event.RemainingQtyLots,
		RemainingSpend:    event.RefundAmount,
	}
	s.stateMu.RLock()
	s.manager.ReleaseOrder(ao, event.RefundAmount)
	serviceLogger.Infof("funds: released order=%d wallet=%s status=%s refund=%d",
		event.OrderID, event.WalletAddress, event.Status, event.RefundAmount)
	s.ack(msg)
	s.stateMu.RUnlock()
}

func (s *Service) handleDepositConfirmedMessage(_ context.Context, msg *nats.Msg) {
	var event protocol.DepositConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode deposit confirmation failed: %v", err)
		_ = msg.Term()
		return
	}
	signature := strings.TrimSpace(event.Signature)
	if signature == "" {
		_ = msg.Term()
		return
	}
	if s.deposits != nil && s.deposits.Has(signature) {
		s.ack(msg)
		return
	}
	s.stateMu.RLock()
	s.manager.ApplyDepositConfirmed(strings.TrimSpace(event.WalletAddress), event.AmountUnits)
	if s.deposits != nil {
		s.deposits.Mark(signature)
	}
	s.ack(msg)
	s.stateMu.RUnlock()
}

// ==========================================
// 辅助函数
// ==========================================

func (s *Service) applyPendingLifecycle(matchEventID string, pt *PendingTerminal) {
	if pt == nil {
		return
	}
	switch pt.Phase {
	case PhaseSubmitted:
		var event protocol.SettlementSubmittedEvent
		if err := json.Unmarshal(pt.RawEvent, &event); err != nil {
			serviceLogger.Warnf("funds: decode pending submitted failed match_event_id=%s err=%v", matchEventID, err)
			return
		}
		ok, conflict := s.inflight.AdvanceToSubmitted(matchEventID, event.TxSignature)
		if conflict {
			serviceLogger.Warnf("funds: pending submitted conflict match_event_id=%s", matchEventID)
		} else if !ok {
			serviceLogger.Warnf("funds: pending submitted not found match_event_id=%s", matchEventID)
		}
	case PhaseConfirmed, PhaseFailed:
		s.applyTerminal(matchEventID, pt.Phase, pt.RawEvent)
	default:
		serviceLogger.Warnf("funds: unknown pending lifecycle phase=%s match_event_id=%s", pt.Phase, matchEventID)
	}
}

func (s *Service) ack(msg *nats.Msg) {
	if msg == nil {
		return
	}
	seq, hasSeq := eventSeqFromMsg(msg)
	if err := msg.Ack(); err != nil {
		serviceLogger.Warnf("funds ack failed subject=%s err=%v", msg.Subject, err)
		return
	}
	if hasSeq && strings.HasPrefix(msg.Subject, "evt.") {
		for {
			current := s.evtSeq.Load()
			if seq <= current {
				break
			}
			if s.evtSeq.CompareAndSwap(current, seq) {
				break
			}
		}
		if s.projector != nil {
			s.projector.MarkRecoveryDirty()
		}
	}
}

func eventSeqFromMsg(msg *nats.Msg) (uint64, bool) {
	if msg == nil {
		return 0, false
	}
	meta, err := msg.Metadata()
	if err != nil || meta == nil {
		return 0, false
	}
	return meta.Sequence.Stream, true
}

func collectMatchBatchWallets(event matching.MatchBatchEvent) []string {
	set := make(map[string]struct{})
	for _, order := range event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		if wallet == "" {
			continue
		}
		set[wallet] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for wallet := range set {
		result = append(result, wallet)
	}
	sort.Strings(result)
	return result
}

func activeOrderFromExecution(exec matching.ExecutionSnapshot, marketID uint64, marketPDA string, remainingQty uint64) ActiveOrder {
	return ActiveOrder{
		WalletAddress:     exec.WalletAddress,
		MarketID:          marketID,
		MarketPDA:         marketPDA,
		OriginalAction:    mapAction(exec.OriginalAction),
		OriginalOutcome:   mapOutcome(exec.OriginalOutcome),
		OriginalPriceTick: exec.OriginalPriceTick,
		OrderType:         mapOrderType(exec.OrderType),
		RemainingQty:      remainingQty,
		RemainingSpend:    exec.SpendAmount,
	}
}

func activeOrderFromCommand(cmd protocol.PlaceOrderCommand) ActiveOrder {
	return ActiveOrder{
		WalletAddress:     cmd.Execution.WalletAddress,
		MarketID:          cmd.MarketID,
		MarketPDA:         cmd.MarketPDA,
		OriginalAction:    mapAction(cmd.Execution.OriginalAction),
		OriginalOutcome:   mapOutcome(cmd.Execution.OriginalOutcome),
		OriginalPriceTick: cmd.Execution.OriginalPriceTick,
		OrderType:         mapOrderType(cmd.Execution.OrderType),
		RemainingQty:      cmd.Execution.QtyLots,
		RemainingSpend:    cmd.Execution.SpendAmount,
	}
}

func mapAction(value string) uint8 {
	if strings.EqualFold(value, "sell") {
		return SideSell
	}
	return SideBuy
}

func mapOutcome(value string) uint8 {
	if strings.EqualFold(value, "no") {
		return OutcomeNo
	}
	return OutcomeYes
}

func mapOrderType(value string) uint8 {
	if strings.EqualFold(value, "market") {
		return OrderTypeMarket
	}
	return OrderTypeLimit
}

func clampNonNegative(v int64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

func clampSigned(value int64) int64 {
	return value
}

func SortedWallets(values []string) []string {
	uniq := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := uniq[value]; ok {
			continue
		}
		uniq[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
