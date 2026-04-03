package funds

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
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
	defaultSubmitConsumerName     = "funds-submit"
	defaultMatchConsumerName      = "funds-match"
	defaultSettlementConsumerName = "funds-settlement-confirmed"
	defaultDepositConsumerName    = "funds-deposit-confirmed"
)

type Service struct {
	client                  *natsjs.Client
	pool                    *pgxpool.Pool
	rdb                     *redis.Client
	manager                 *Manager
	submitConsumerName      string
	matchConsumerName       string
	settlementConsumerName  string
	depositConsumerName     string
	submitSub               *nats.Subscription
	matchSub                *nats.Subscription
	settlementConfirmedSub  *nats.Subscription
	depositConfirmedSub     *nats.Subscription
	mu                      sync.Mutex
	seenSubmit              map[string]struct{}
	seenMatchBatch          map[string]struct{}
	seenSettlementConfirmed map[string]struct{}
	seenDepositConfirmed    map[string]struct{}
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, rdb *redis.Client, manager *Manager) *Service {
	if manager == nil {
		manager = NewManager()
	}
	return &Service{
		client:                  client,
		pool:                    pool,
		rdb:                     rdb,
		manager:                 manager,
		submitConsumerName:      defaultSubmitConsumerName,
		matchConsumerName:       defaultMatchConsumerName,
		settlementConsumerName:  defaultSettlementConsumerName,
		depositConsumerName:     defaultDepositConsumerName,
		seenSubmit:              make(map[string]struct{}),
		seenMatchBatch:          make(map[string]struct{}),
		seenSettlementConfirmed: make(map[string]struct{}),
		seenDepositConfirmed:    make(map[string]struct{}),
	}
}

func (s *Service) Manager() *Manager {
	return s.manager
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.manager == nil {
		return nil
	}
	if err := s.RecoverFromStore(ctx); err != nil {
		return err
	}
	if err := s.ensureSubmitSubscription(); err != nil {
		return err
	}
	if err := s.ensureMatchSubscription(); err != nil {
		return err
	}
	if err := s.ensureSettlementSubscription(); err != nil {
		return err
	}
	if err := s.ensureDepositSubscription(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		if s.submitSub != nil {
			_ = s.submitSub.Unsubscribe()
		}
		if s.matchSub != nil {
			_ = s.matchSub.Unsubscribe()
		}
		if s.settlementConfirmedSub != nil {
			_ = s.settlementConfirmedSub.Unsubscribe()
		}
		if s.depositConfirmedSub != nil {
			_ = s.depositConfirmedSub.Unsubscribe()
		}
	}()
	return nil
}

func (s *Service) RecoverFromStore(ctx context.Context) error {
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
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("funds iterate wallet_accounts: %w", err)
	}

	positionRows, err := s.pool.Query(ctx, `
		SELECT
			p.wallet_address,
			COALESCE(m.market_pda, ''),
			p.yes_free_lots,
			p.yes_locked_lots,
			p.yes_pending_lots,
			p.no_free_lots,
			p.no_locked_lots,
			p.no_pending_lots
		FROM positions p
		JOIN markets m ON m.market_id = p.market_id
	`)
	if err != nil {
		return fmt.Errorf("funds load positions: %w", err)
	}
	defer positionRows.Close()

	for positionRows.Next() {
		var (
			walletAddress  string
			marketPDA      string
			yesFreeLots    int64
			yesLockedLots  int64
			yesPendingLots int64
			noFreeLots     int64
			noLockedLots   int64
			noPendingLots  int64
		)
		if err := positionRows.Scan(
			&walletAddress,
			&marketPDA,
			&yesFreeLots,
			&yesLockedLots,
			&yesPendingLots,
			&noFreeLots,
			&noLockedLots,
			&noPendingLots,
		); err != nil {
			return fmt.Errorf("funds scan positions: %w", err)
		}
		wallets[walletAddress] = struct{}{}
		s.manager.SeedPosition(walletAddress, marketPDA, MarketPosition{
			AvailableYesShares: clampNonNegative(yesFreeLots),
			LockedYesShares:    clampNonNegative(yesLockedLots),
			PendingYesShares:   clampSigned(yesPendingLots),
			AvailableNoShares:  clampNonNegative(noFreeLots),
			LockedNoShares:     clampNonNegative(noLockedLots),
			PendingNoShares:    clampSigned(noPendingLots),
		})
	}
	if err := positionRows.Err(); err != nil {
		return fmt.Errorf("funds iterate positions: %w", err)
	}
	for walletAddress := range wallets {
		if err := s.syncWalletProjection(ctx, walletAddress); err != nil {
			return fmt.Errorf("funds resync wallet redis snapshot: %w", err)
		}
	}
	return nil
}

func (s *Service) ensureSubmitSubscription() error {
	if s.submitSub != nil {
		return nil
	}
	sub, err := s.client.JetStream().QueueSubscribe(
		protocol.SubjectOrderSubmit,
		"funds_submit_group",
		s.handleSubmitMessage,
		nats.Durable(s.submitConsumerName),
		nats.ManualAck(),
		nats.DeliverAll(),
	)
	if err != nil {
		return fmt.Errorf("funds subscribe submit: %w", err)
	}
	s.submitSub = sub
	return nil
}

func (s *Service) ensureMatchSubscription() error {
	if s.matchSub != nil {
		return nil
	}
	sub, err := s.client.JetStream().QueueSubscribe(
		protocol.SubjectMatchBatchV2+".*",
		"funds_match_group",
		s.handleMatchBatchMessage,
		nats.Durable(s.matchConsumerName),
		nats.ManualAck(),
		nats.DeliverNew(),
	)
	if err != nil {
		return fmt.Errorf("funds subscribe match batch: %w", err)
	}
	s.matchSub = sub
	return nil
}

func (s *Service) ensureSettlementSubscription() error {
	if s.settlementConfirmedSub != nil {
		return nil
	}
	sub, err := s.client.JetStream().QueueSubscribe(
		protocol.SubjectSettlementConfirm+".*",
		"funds_settlement_group",
		s.handleSettlementConfirmedMessage,
		nats.Durable(s.settlementConsumerName),
		nats.ManualAck(),
		nats.DeliverNew(),
	)
	if err != nil {
		return fmt.Errorf("funds subscribe settlement confirmed: %w", err)
	}
	s.settlementConfirmedSub = sub
	return nil
}

func (s *Service) ensureDepositSubscription() error {
	if s.depositConfirmedSub != nil {
		return nil
	}
	sub, err := s.client.JetStream().QueueSubscribe(
		protocol.SubjectDepositConfirmed,
		"funds_deposit_group",
		s.handleDepositConfirmedMessage,
		nats.Durable(s.depositConsumerName),
		nats.ManualAck(),
		nats.DeliverNew(),
	)
	if err != nil {
		return fmt.Errorf("funds subscribe deposit confirmed: %w", err)
	}
	s.depositConfirmedSub = sub
	return nil
}

func (s *Service) handleSubmitMessage(msg *nats.Msg) {
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		serviceLogger.Warnf("funds decode submit failed: %v", err)
		_ = msg.Term()
		return
	}
	cmd := env.Payload
	if s.markSeen(s.seenSubmit, cmd.CommandID) {
		_ = msg.Ack()
		return
	}
	err := s.manager.ReserveOrder(ReserveOrderInput{
		WalletAddress:     cmd.Execution.WalletAddress,
		MarketPDA:         cmd.MarketPDA,
		OriginalAction:    mapAction(cmd.Execution.OriginalAction),
		OriginalOutcome:   mapOutcome(cmd.Execution.OriginalOutcome),
		OriginalPriceTick: cmd.Execution.OriginalPriceTick,
		OrderType:         mapOrderType(cmd.Execution.OrderType),
		QtyLots:           cmd.Execution.QtyLots,
		SpendAmount:       cmd.Execution.SpendAmount,
	})
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
			s.unmarkSeen(s.seenSubmit, cmd.CommandID)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
		return
	}
	if err := s.syncWalletProjection(context.Background(), cmd.Execution.WalletAddress); err != nil {
		serviceLogger.Warnf("funds sync wallet projection after reserve failed wallet=%s err=%v", cmd.Execution.WalletAddress, err)
		s.manager.ReleaseOrder(activeOrderFromCommand(cmd), 0)
		s.unmarkSeen(s.seenSubmit, cmd.CommandID)
		_ = msg.Nak()
		return
	}
	if err := s.client.PublishJSON(context.Background(), protocol.SubjectOrderReservedMarket(cmd.MarketID), env.IdempotencyKey, env); err != nil {
		serviceLogger.Warnf("funds publish reserved event failed market=%d err=%v", cmd.MarketID, err)
		s.manager.ReleaseOrder(activeOrderFromCommand(cmd), 0)
		if syncErr := s.syncWalletProjection(context.Background(), cmd.Execution.WalletAddress); syncErr != nil {
			serviceLogger.Warnf("funds sync wallet projection after reserve rollback failed wallet=%s err=%v", cmd.Execution.WalletAddress, syncErr)
		}
		s.unmarkSeen(s.seenSubmit, cmd.CommandID)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

func (s *Service) handleMatchBatchMessage(msg *nats.Msg) {
	var event matching.MatchBatchEventV2
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode match batch failed: %v", err)
		_ = msg.Term()
		return
	}
	if s.markSeen(s.seenMatchBatch, event.EventID) {
		_ = msg.Ack()
		return
	}
	orderByIndex := make(map[uint16]matching.MatchedOrderV2, len(event.Orders))
	for _, order := range event.Orders {
		orderByIndex[order.OrderIndex] = order
	}
	for _, fill := range event.Fills {
		maker, makerOK := orderByIndex[fill.MakerOrderIndex]
		taker, takerOK := orderByIndex[fill.TakerOrderIndex]
		if !makerOK || !takerOK {
			serviceLogger.Warnf("funds match batch references missing order index market=%d fill=%d", event.MarketID, fill.FillIndex)
			s.unmarkSeen(s.seenMatchBatch, event.EventID)
			_ = msg.Term()
			return
		}
		s.manager.ApplyMatchPending(activeOrderFromExecution(maker.Execution, event.MarketPDA, fill.FillAmount), activeOrderFromExecution(taker.Execution, event.MarketPDA, fill.FillAmount), fill.FillAmount, uint8(fill.FillPrice))
	}
	for _, update := range event.OrderUpdates {
		if update.Status != "canceled" && update.Status != "expired" && update.Status != "rejected" {
			continue
		}
		order, ok := orderByIndex[update.OrderIndex]
		if !ok {
			serviceLogger.Warnf("funds order update references missing order index market=%d idx=%d", event.MarketID, update.OrderIndex)
			s.unmarkSeen(s.seenMatchBatch, event.EventID)
			_ = msg.Term()
			return
		}
		s.manager.ReleaseOrder(activeOrderFromExecution(order.Execution, event.MarketPDA, update.RemainingQtyLots), update.RefundAmount)
	}
	for _, wallet := range collectMatchBatchWallets(event) {
		if err := s.syncWalletProjection(context.Background(), wallet); err != nil {
			serviceLogger.Warnf("funds sync wallet projection after match failed wallet=%s market=%d err=%v", wallet, event.MarketID, err)
		}
	}
	_ = msg.Ack()
}

func (s *Service) handleSettlementConfirmedMessage(msg *nats.Msg) {
	var event protocol.SettlementConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode settlement confirmation failed: %v", err)
		_ = msg.Term()
		return
	}
	if s.markSeen(s.seenSettlementConfirmed, event.EventID) {
		_ = msg.Ack()
		return
	}
	for _, wallet := range event.Wallets {
		wallet = strings.TrimSpace(wallet)
		if wallet == "" {
			continue
		}
		s.manager.ApplySettlementConfirmed(wallet, event.MarketPDA)
		if err := s.syncWalletProjection(context.Background(), wallet); err != nil {
			serviceLogger.Warnf("funds sync wallet projection after settlement failed wallet=%s market=%s err=%v", wallet, event.MarketPDA, err)
		}
	}
	_ = msg.Ack()
}

func (s *Service) handleDepositConfirmedMessage(msg *nats.Msg) {
	var event protocol.DepositConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("funds decode deposit confirmation failed: %v", err)
		_ = msg.Term()
		return
	}
	if s.markSeen(s.seenDepositConfirmed, event.Signature) {
		_ = msg.Ack()
		return
	}
	s.manager.ApplyDepositConfirmed(strings.TrimSpace(event.WalletAddress), event.AmountUnits)
	if err := s.syncWalletProjection(context.Background(), strings.TrimSpace(event.WalletAddress)); err != nil {
		serviceLogger.Warnf("funds sync wallet projection after deposit failed wallet=%s err=%v", strings.TrimSpace(event.WalletAddress), err)
	}
	_ = msg.Ack()
}

func (s *Service) syncWalletProjection(ctx context.Context, walletAddress string) error {
	walletAddress = strings.TrimSpace(walletAddress)
	if walletAddress == "" || s.manager == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	ledger := s.manager.Ledger(walletAddress)
	totalUnits := ledger.AvailableUSDC + ledger.LockedUSDC
	if ledger.PendingUSDC > 0 {
		totalUnits += uint64(ledger.PendingUSDC)
	}

	if s.pool != nil {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO wallet_accounts (
				wallet_address,
				collateral_total_units,
				collateral_free_units,
				collateral_locked_units,
				collateral_pending_units,
				updated_at
			)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (wallet_address) DO UPDATE SET
				collateral_total_units = EXCLUDED.collateral_total_units,
				collateral_free_units = EXCLUDED.collateral_free_units,
				collateral_locked_units = EXCLUDED.collateral_locked_units,
				collateral_pending_units = EXCLUDED.collateral_pending_units,
				updated_at = NOW()
		`, walletAddress, int64(totalUnits), int64(ledger.AvailableUSDC), int64(ledger.LockedUSDC), ledger.PendingUSDC); err != nil {
			return fmt.Errorf("upsert wallet_accounts: %w", err)
		}
	}

	if s.rdb != nil {
		key := fmt.Sprintf("wallet-account:%s", walletAddress)
		if err := s.rdb.HSet(ctx, key, map[string]any{
			"collateral_total_units":   totalUnits,
			"collateral_free_units":    ledger.AvailableUSDC,
			"collateral_locked_units":  ledger.LockedUSDC,
			"collateral_pending_units": ledger.PendingUSDC,
			"updated_at":               time.Now().UTC().Unix(),
		}).Err(); err != nil {
			return fmt.Errorf("sync wallet redis: %w", err)
		}
	}
	return nil
}

func clampSigned(value int64) int64 {
	return value
}

func collectMatchBatchWallets(event matching.MatchBatchEventV2) []string {
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

func activeOrderFromExecution(exec matching.ExecutionSnapshotV2, marketPDA string, remainingQty uint64) ActiveOrder {
	remainingSpend := exec.SpendAmount
	if exec.OrderType == "market" && exec.OriginalAction == "buy" {
		remainingSpend = exec.SpendAmount
	}
	return ActiveOrder{
		WalletAddress:     exec.WalletAddress,
		MarketPDA:         marketPDA,
		OriginalAction:    mapAction(exec.OriginalAction),
		OriginalOutcome:   mapOutcome(exec.OriginalOutcome),
		OriginalPriceTick: exec.OriginalPriceTick,
		OrderType:         mapOrderType(exec.OrderType),
		RemainingQty:      remainingQty,
		RemainingSpend:    remainingSpend,
	}
}

func activeOrderFromCommand(cmd protocol.PlaceOrderCommand) ActiveOrder {
	return ActiveOrder{
		WalletAddress:     cmd.Execution.WalletAddress,
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

func (s *Service) markSeen(store map[string]struct{}, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := store[key]; ok {
		return true
	}
	store[key] = struct{}{}
	return false
}

func (s *Service) unmarkSeen(store map[string]struct{}, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(store, key)
}
