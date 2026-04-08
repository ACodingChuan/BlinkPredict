package funds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blinkpredict/banckend/internal/logging"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var projectorLogger = logging.New("funds-projector")

const (
	projectorInterval   = 500 * time.Millisecond
	projectorBatchLimit = 500 // 单次最多处理多少条
)

type dbExec interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Projector 负责把内存中脏的 wallet/position 状态异步投影到 Postgres/Redis。
// 它不是热路径正确性的基础，只是查询层的最终一致副本。
type Projector struct {
	manager       *Manager
	inflight      *InflightStore
	submits       *SubmitStore
	deposits      *DepositStore
	withdrawals   *WithdrawalStore
	pending       *PendingReserveStore
	pool          *pgxpool.Pool
	rdb           *redis.Client
	stateMu       *sync.RWMutex
	lastEventSeq  *atomic.Uint64
	recoveryDirty atomic.Bool
}

func NewProjector(manager *Manager, inflight *InflightStore, submits *SubmitStore, deposits *DepositStore, withdrawals *WithdrawalStore, pending *PendingReserveStore, pool *pgxpool.Pool, rdb *redis.Client, stateMu *sync.RWMutex, lastEventSeq *atomic.Uint64) *Projector {
	return &Projector{
		manager:      manager,
		inflight:     inflight,
		submits:      submits,
		deposits:     deposits,
		withdrawals:  withdrawals,
		pending:      pending,
		pool:         pool,
		rdb:          rdb,
		stateMu:      stateMu,
		lastEventSeq: lastEventSeq,
	}
}

func (p *Projector) MarkRecoveryDirty() {
	if p == nil {
		return
	}
	p.recoveryDirty.Store(true)
}

// Run 启动 projector 投影循环，直到 ctx 取消。
func (p *Projector) Run(ctx context.Context) {
	ticker := time.NewTicker(projectorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 退出前做最后一次刷盘
			_ = p.FlushNow(context.Background())
			return
		case <-ticker.C:
			_ = p.flush(ctx)
		}
	}
}

func (p *Projector) FlushNow(ctx context.Context) error {
	p.MarkRecoveryDirty()
	return p.flush(ctx)
}

type recoverySnapshot struct {
	inflight        []*InflightMatch
	pending         []*PendingTerminal
	pendingReserves []PendingReserve
	submits         []string
	deposits        []string
	withdrawals     []string
	lastEvtSeq      uint64
}

// flush 采集脏数据并写入 Postgres/Redis。
func (p *Projector) flush(ctx context.Context) error {
	wallets, positions, recovery, err := p.captureFlushState()
	if err != nil {
		return err
	}
	if len(wallets) == 0 && len(positions) == 0 && recovery == nil {
		return nil
	}

	pgFailed := false
	if p.pool != nil {
		if err := p.flushPostgres(ctx, wallets, positions, recovery); err != nil {
			projectorLogger.Warnf("projector postgres flush failed: %v", err)
			pgFailed = true
		}
	}
	if pgFailed {
		p.manager.RestoreDirty(wallets, positions)
		p.MarkRecoveryDirty()
		return fmt.Errorf("projector postgres flush failed")
	}
	if recovery != nil && p.inflight != nil {
		p.inflight.EvictTerminal(recovery.lastEvtSeq)
	}

	restoreRedisDirty := false
	if p.rdb != nil {
		if err := p.refreshRedisWallets(ctx, wallets); err != nil {
			projectorLogger.Warnf("projector refresh redis wallets failed: %v", err)
			restoreRedisDirty = true
		}
		if err := p.refreshRedisPositions(ctx, positions); err != nil {
			projectorLogger.Warnf("projector refresh redis positions failed: %v", err)
			restoreRedisDirty = true
		}
	}
	if restoreRedisDirty {
		p.manager.RestoreDirty(wallets, positions)
	}
	return nil
}

func (p *Projector) captureFlushState() ([]DirtyWalletSnapshot, []DirtyPositionSnapshot, *recoverySnapshot, error) {
	if p.manager == nil {
		return nil, nil, nil, nil
	}
	var (
		wallets   []DirtyWalletSnapshot
		positions []DirtyPositionSnapshot
		recovery  *recoverySnapshot
	)
	if p.stateMu != nil {
		p.stateMu.Lock()
		defer p.stateMu.Unlock()
	}
	wallets, positions = p.manager.CollectAndClearDirty()
	needRecovery := len(wallets) > 0 || len(positions) > 0 || p.recoveryDirty.Swap(false)
	if needRecovery {
		recovery = &recoverySnapshot{
			lastEvtSeq: p.currentEventSeq(),
		}
		if p.inflight != nil {
			recovery.inflight = p.inflight.Snapshot()
			recovery.pending = p.inflight.PendingSnapshot()
		}
		if p.submits != nil {
			recovery.submits = p.submits.Snapshot()
		}
		if p.deposits != nil {
			recovery.deposits = p.deposits.Snapshot()
		}
		if p.withdrawals != nil {
			recovery.withdrawals = p.withdrawals.Snapshot()
		}
		if p.pending != nil {
			recovery.pendingReserves = p.pending.Snapshot()
		}
	}
	return wallets, positions, recovery, nil
}

func (p *Projector) currentEventSeq() uint64 {
	if p == nil || p.lastEventSeq == nil {
		return 0
	}
	return p.lastEventSeq.Load()
}

func (p *Projector) flushPostgres(ctx context.Context, wallets []DirtyWalletSnapshot, positions []DirtyPositionSnapshot, recovery *recoverySnapshot) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := p.upsertWalletsTx(ctx, tx, wallets); err != nil {
		return err
	}
	if err := p.upsertPositionsTx(ctx, tx, positions); err != nil {
		return err
	}
	if recovery != nil {
		if err := p.upsertRecoveryStateTx(ctx, tx, recovery); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// upsertWallets 微批 UPSERT wallet_accounts。
func (p *Projector) upsertWallets(ctx context.Context, wallets []DirtyWalletSnapshot) error {
	if len(wallets) == 0 {
		return nil
	}
	// 分批处理，防止单次参数过多
	for start := 0; start < len(wallets); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(wallets) {
			end = len(wallets)
		}
		batch := wallets[start:end]
		if err := p.upsertWalletBatch(ctx, p.pool, batch); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) upsertWalletsTx(ctx context.Context, tx pgx.Tx, wallets []DirtyWalletSnapshot) error {
	if len(wallets) == 0 {
		return nil
	}
	for start := 0; start < len(wallets); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(wallets) {
			end = len(wallets)
		}
		if err := p.upsertWalletBatch(ctx, tx, wallets[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) upsertWalletBatch(ctx context.Context, execer dbExec, wallets []DirtyWalletSnapshot) error {
	// 动态拼装 VALUES 子句
	var sb strings.Builder
	args := make([]any, 0, len(wallets)*5)
	sb.WriteString(`
		INSERT INTO wallet_accounts (
			wallet_address,
			collateral_total_units,
			collateral_free_units,
			collateral_locked_units,
			collateral_pending_units,
			updated_at
		) VALUES `)

	paramIdx := 1
	for i, w := range wallets {
		if i > 0 {
			sb.WriteString(", ")
		}
		totalUnits := int64(w.Ledger.AvailableUSDC) + int64(w.Ledger.LockedUSDC)
		pendingUnits := nonNegativePendingUnits(w.Ledger.PendingUSDC)
		totalUnits += pendingUnits
		sb.WriteString(fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, NOW())", paramIdx, paramIdx+1, paramIdx+2, paramIdx+3, paramIdx+4))
		args = append(args,
			w.WalletAddress,
			totalUnits,
			int64(w.Ledger.AvailableUSDC),
			int64(w.Ledger.LockedUSDC),
			pendingUnits,
		)
		paramIdx += 5
	}
	sb.WriteString(`
		ON CONFLICT (wallet_address) DO UPDATE SET
			collateral_total_units   = EXCLUDED.collateral_total_units,
			collateral_free_units    = EXCLUDED.collateral_free_units,
			collateral_locked_units  = EXCLUDED.collateral_locked_units,
			collateral_pending_units = EXCLUDED.collateral_pending_units,
			updated_at               = NOW()
	`)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := execer.Exec(timeoutCtx, sb.String(), args...)
	return err
}

// upsertPositions 微批 UPSERT positions 表。
func (p *Projector) upsertPositions(ctx context.Context, positions []DirtyPositionSnapshot) error {
	if len(positions) == 0 {
		return nil
	}
	for start := 0; start < len(positions); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(positions) {
			end = len(positions)
		}
		if err := p.upsertPositionBatch(ctx, p.pool, positions[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) upsertPositionsTx(ctx context.Context, tx pgx.Tx, positions []DirtyPositionSnapshot) error {
	if len(positions) == 0 {
		return nil
	}
	for start := 0; start < len(positions); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(positions) {
			end = len(positions)
		}
		if err := p.upsertPositionBatch(ctx, tx, positions[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) upsertPositionBatch(ctx context.Context, execer dbExec, positions []DirtyPositionSnapshot) error {
	var sb strings.Builder
	args := make([]any, 0, len(positions)*10)
	sb.WriteString(`
		INSERT INTO positions (
			market_id,
			wallet_address,
			market_pda,
			yes_free_lots,
			yes_locked_lots,
			yes_pending_lots,
			no_free_lots,
			no_locked_lots,
			no_pending_lots,
			collateral_locked_units,
			updated_at
		) VALUES `)

	paramIdx := 1
	written := 0
	for _, pos := range positions {
		if pos.MarketID == 0 {
			continue
		}
		if written > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(fmt.Sprintf("($%d::NUMERIC(20,0), $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, NOW())",
			paramIdx, paramIdx+1, paramIdx+2, paramIdx+3, paramIdx+4, paramIdx+5, paramIdx+6, paramIdx+7, paramIdx+8, paramIdx+9))
		args = append(args,
			fmt.Sprintf("%d", pos.MarketID),
			pos.WalletAddress,
			pos.MarketPDA,
			int64(pos.Position.AvailableYesShares),
			int64(pos.Position.LockedYesShares),
			pos.Position.PendingYesShares,
			int64(pos.Position.AvailableNoShares),
			int64(pos.Position.LockedNoShares),
			pos.Position.PendingNoShares,
			int64(pos.Position.CollateralLockedUnits),
		)
		paramIdx += 10
		written++
	}
	if written == 0 {
		return nil
	}
	sb.WriteString(`
		ON CONFLICT (market_id, wallet_address) DO UPDATE SET
			market_pda              = EXCLUDED.market_pda,
			yes_free_lots           = EXCLUDED.yes_free_lots,
			yes_locked_lots         = EXCLUDED.yes_locked_lots,
			yes_pending_lots        = EXCLUDED.yes_pending_lots,
			no_free_lots            = EXCLUDED.no_free_lots,
			no_locked_lots          = EXCLUDED.no_locked_lots,
			no_pending_lots         = EXCLUDED.no_pending_lots,
			collateral_locked_units = EXCLUDED.collateral_locked_units,
			updated_at              = NOW()
	`)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := execer.Exec(timeoutCtx, sb.String(), args...)
	return err
}

func (p *Projector) upsertRecoveryStateTx(ctx context.Context, tx pgx.Tx, recovery *recoverySnapshot) error {
	if recovery == nil {
		return nil
	}
	inflightJSON, err := json.Marshal(recovery.inflight)
	if err != nil {
		return fmt.Errorf("marshal recovery inflight: %w", err)
	}
	pendingJSON, err := json.Marshal(recovery.pending)
	if err != nil {
		return fmt.Errorf("marshal recovery pending terminals: %w", err)
	}
	pendingReservesJSON, err := json.Marshal(recovery.pendingReserves)
	if err != nil {
		return fmt.Errorf("marshal recovery pending reserves: %w", err)
	}
	submitsJSON, err := json.Marshal(recovery.submits)
	if err != nil {
		return fmt.Errorf("marshal recovery submits: %w", err)
	}
	depositsJSON, err := json.Marshal(recovery.deposits)
	if err != nil {
		return fmt.Errorf("marshal recovery deposits: %w", err)
	}
	withdrawalsJSON, err := json.Marshal(recovery.withdrawals)
	if err != nil {
		return fmt.Errorf("marshal recovery withdrawals: %w", err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = tx.Exec(timeoutCtx, `
		INSERT INTO funds_recovery_state (
			recovery_id,
			last_flushed_evt_seq,
			inflight_json,
			pending_terminals_json,
			pending_reserves_json,
			processed_submits_json,
			processed_deposits_json,
			processed_withdrawals_json,
			updated_at
		) VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb, $8::jsonb, NOW())
		ON CONFLICT (recovery_id) DO UPDATE SET
			last_flushed_evt_seq    = EXCLUDED.last_flushed_evt_seq,
			inflight_json           = EXCLUDED.inflight_json,
			pending_terminals_json  = EXCLUDED.pending_terminals_json,
			pending_reserves_json   = EXCLUDED.pending_reserves_json,
			processed_submits_json  = EXCLUDED.processed_submits_json,
			processed_deposits_json = EXCLUDED.processed_deposits_json,
			processed_withdrawals_json = EXCLUDED.processed_withdrawals_json,
			updated_at              = NOW()
	`, recoveryStateRowID, int64(recovery.lastEvtSeq), inflightJSON, pendingJSON, pendingReservesJSON, submitsJSON, depositsJSON, withdrawalsJSON)
	if err == nil || !isUndefinedColumnError(err, "processed_withdrawals_json") {
		return err
	}
	_, err = tx.Exec(timeoutCtx, `
		INSERT INTO funds_recovery_state (
			recovery_id,
			last_flushed_evt_seq,
			inflight_json,
			pending_terminals_json,
			pending_reserves_json,
			processed_submits_json,
			processed_deposits_json,
			updated_at
		) VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb, NOW())
		ON CONFLICT (recovery_id) DO UPDATE SET
			last_flushed_evt_seq    = EXCLUDED.last_flushed_evt_seq,
			inflight_json           = EXCLUDED.inflight_json,
			pending_terminals_json  = EXCLUDED.pending_terminals_json,
			pending_reserves_json   = EXCLUDED.pending_reserves_json,
			processed_submits_json  = EXCLUDED.processed_submits_json,
			processed_deposits_json = EXCLUDED.processed_deposits_json,
			updated_at              = NOW()
	`, recoveryStateRowID, int64(recovery.lastEvtSeq), inflightJSON, pendingJSON, pendingReservesJSON, submitsJSON, depositsJSON)
	return err
}

func isUndefinedColumnError(err error, column string) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42703" && (pgErr.ColumnName == column || strings.Contains(pgErr.Message, column))
}

func nonNegativePendingUnits(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// refreshRedisWallets 把单个 wallet 的最新状态刷入 Redis。
func (p *Projector) refreshRedisWallets(ctx context.Context, wallets []DirtyWalletSnapshot) error {
	if len(wallets) == 0 {
		return nil
	}
	for start := 0; start < len(wallets); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(wallets) {
			end = len(wallets)
		}
		if err := p.refreshRedisWalletBatch(ctx, wallets[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// refreshRedisPositions 把单个仓位的最新状态刷入 Redis。
func (p *Projector) refreshRedisPositions(ctx context.Context, positions []DirtyPositionSnapshot) error {
	if len(positions) == 0 {
		return nil
	}
	for start := 0; start < len(positions); start += projectorBatchLimit {
		end := start + projectorBatchLimit
		if end > len(positions) {
			end = len(positions)
		}
		if err := p.refreshRedisPositionBatch(ctx, positions[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) refreshRedisWalletBatch(ctx context.Context, wallets []DirtyWalletSnapshot) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	updatedAt := time.Now().UTC().Unix()
	pipe := p.rdb.Pipeline()
	for _, w := range wallets {
		key := fmt.Sprintf("wallet-account:%s", w.WalletAddress)
		totalUnits := int64(w.Ledger.AvailableUSDC) + int64(w.Ledger.LockedUSDC)
		pendingUnits := nonNegativePendingUnits(w.Ledger.PendingUSDC)
		totalUnits += pendingUnits
		pipe.HSet(timeoutCtx, key, map[string]any{
			"collateral_total_units":   totalUnits,
			"collateral_free_units":    int64(w.Ledger.AvailableUSDC),
			"collateral_locked_units":  int64(w.Ledger.LockedUSDC),
			"collateral_pending_units": pendingUnits,
			"updated_at":               updatedAt,
		})
	}
	if _, err := pipe.Exec(timeoutCtx); err != nil {
		return fmt.Errorf("redis wallet batch: %w", err)
	}
	return nil
}

func (p *Projector) refreshRedisPositionBatch(ctx context.Context, positions []DirtyPositionSnapshot) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	updatedAt := time.Now().UTC().Unix()
	pipe := p.rdb.Pipeline()
	for _, pos := range positions {
		if pos.MarketID == 0 {
			continue
		}
		key := fmt.Sprintf("position:%d:%s", pos.MarketID, pos.WalletAddress)
		pipe.HSet(timeoutCtx, key, map[string]any{
			"yes_free_lots":           int64(pos.Position.AvailableYesShares),
			"yes_locked_lots":         int64(pos.Position.LockedYesShares),
			"yes_pending_lots":        pos.Position.PendingYesShares,
			"no_free_lots":            int64(pos.Position.AvailableNoShares),
			"no_locked_lots":          int64(pos.Position.LockedNoShares),
			"no_pending_lots":         pos.Position.PendingNoShares,
			"collateral_locked_units": int64(pos.Position.CollateralLockedUnits),
			"updated_at":              updatedAt,
		})
	}
	if _, err := pipe.Exec(timeoutCtx); err != nil {
		return fmt.Errorf("redis position batch: %w", err)
	}
	return nil
}

// SyncWalletNow 立即同步单个 wallet（兼容旧代码调用路径，供 service 非热路径使用）。
func (p *Projector) SyncWalletNow(ctx context.Context, walletAddress string) error {
	walletAddress = strings.TrimSpace(walletAddress)
	if walletAddress == "" {
		return nil
	}
	ledger := p.manager.Ledger(walletAddress)
	snap := []DirtyWalletSnapshot{{WalletAddress: walletAddress, Ledger: ledger}}
	if p.pool != nil {
		if err := p.upsertWalletBatch(ctx, p.pool, snap); err != nil {
			return fmt.Errorf("sync wallet now: %w", err)
		}
	}
	if p.rdb != nil {
		if err := p.refreshRedisWallets(ctx, snap); err != nil {
			return fmt.Errorf("sync wallet redis: %w", err)
		}
	}
	return nil
}
