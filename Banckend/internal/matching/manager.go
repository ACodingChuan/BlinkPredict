package matching

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/protocol"
	internalsolana "blinkpredict/banckend/internal/solana"

	"github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var logger = logging.New("matcher")

const (
	matcherReservedConsumer            = "matcher-reserved"
	matcherSettlementConfirmedConsumer = "matcher-settlement-confirmed"
	defaultMatcherCheckpointInterval   = 100 * time.Millisecond
)

type ManagerConfig struct {
	Batch                BatchConfig
	CheckpointInterval   time.Duration
	ProgramID            solana.PublicKey
	SettlementTxMaxBytes int
	AddressTables        map[solana.PublicKey]solana.PublicKeySlice
}

// MarketManager 负责监听路由和管理所有市场Actor
type MarketManager struct {
	client               *natsjs.Client
	pool                 *pgxpool.Pool
	batchConfig          BatchConfig
	checkpointInterval   time.Duration
	positionRegistry     *UserPositionRegistry
	positionRepo         *UserPositionAccountRepo
	orderRegistry        *OrderStateRegistry
	orderRepo            *OrderStateAccountRepo
	txEstimator          *internalsolana.SettlementTxEstimator
	maxSettlementTxBytes int
	mu                   sync.RWMutex
	actors               map[uint64]*MarketActor
	checkpoints          map[uint64]uint64
}

// NewMarketManager 创建市场管理器
func NewMarketManager(client *natsjs.Client, pool *pgxpool.Pool, cfg ManagerConfig) *MarketManager {
	batchCfg := cfg.Batch
	if batchCfg == (BatchConfig{}) {
		batchCfg = DefaultBatchConfig()
	}
	checkpointInterval := cfg.CheckpointInterval
	if checkpointInterval <= 0 {
		checkpointInterval = defaultMatcherCheckpointInterval
	}
	maxSettlementTxBytes := cfg.SettlementTxMaxBytes
	if maxSettlementTxBytes <= 0 {
		maxSettlementTxBytes = internalsolana.DefaultSettlementMaxTxBytes
	}
	return &MarketManager{
		client:               client,
		pool:                 pool,
		batchConfig:          batchCfg,
		checkpointInterval:   checkpointInterval,
		positionRegistry:     NewUserPositionRegistry(),
		positionRepo:         NewUserPositionAccountRepo(pool),
		orderRegistry:        NewOrderStateRegistry(),
		orderRepo:            NewOrderStateAccountRepo(pool),
		txEstimator:          internalsolana.NewSettlementTxEstimator(cfg.ProgramID, cfg.AddressTables),
		maxSettlementTxBytes: maxSettlementTxBytes,
		actors:               make(map[uint64]*MarketActor),
		checkpoints:          make(map[uint64]uint64),
	}
}

func (m *MarketManager) Start(ctx context.Context, tickInterval time.Duration) error {
	if m == nil {
		return nil
	}
	if err := m.RecoverFromStore(ctx); err != nil {
		return err
	}
	if err := m.RunBootstrapTick(ctx); err != nil {
		return err
	}
	if err := m.StartConsumer(ctx); err != nil {
		return err
	}
	m.StartCheckpointLoop(ctx)
	m.StartTickLoop(ctx, tickInterval)
	return nil
}

// GetOrCreateMarket 获取或创建市场Actor（动态寻址）
func (m *MarketManager) GetOrCreateMarket(marketID uint64, marketPDA string) *MarketActor {
	m.mu.Lock()
	defer m.mu.Unlock()
	actor, exists := m.actors[marketID]
	if !exists {
		actor = NewMarketActor(marketID, marketPDA)
		if checkpoint := m.checkpoints[marketID]; checkpoint > actor.Book.LastProcessedSeq {
			actor.Book.LastProcessedSeq = checkpoint
		}
		m.actors[marketID] = actor
		m.ensureActorStartedLocked(actor)
	} else if actor.MarketPDA == "" && marketPDA != "" {
		actor.MarketPDA = marketPDA
		if actor.Pending != nil && actor.Pending.event.MarketPDA == "" {
			actor.Pending.event.MarketPDA = marketPDA
		}
	}
	if checkpoint := m.checkpoints[marketID]; checkpoint > actor.Book.LastProcessedSeq {
		actor.Book.LastProcessedSeq = checkpoint
	}
	return actor
}

func (m *MarketManager) ensureActorStartedLocked(actor *MarketActor) {
	if actor.started {
		return
	}
	actor.started = true
	go m.runMatcherEngine(actor)
}

func (m *MarketManager) listActors() []*MarketActor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	actors := make([]*MarketActor, 0, len(m.actors))
	for _, actor := range m.actors {
		actors = append(actors, actor)
	}
	return actors
}

// StartConsumer starts a durable Pull Consumer for evt.order.reserved.*.
// Pull + Durable enables backpressure control, explicit ack, and restart recovery.
func (m *MarketManager) StartConsumer(ctx context.Context) error {
	if m == nil || m.client == nil {
		return nil
	}
	sub, err := m.client.PullSubscribe(protocol.SubjectOrderReserved+".*", matcherReservedConsumer)
	if err != nil {
		return fmt.Errorf("matcher pull subscribe failed: %w", err)
	}
	settlementSub, err := m.client.PullSubscribe(protocol.SubjectSettlementConfirmed+".*", matcherSettlementConfirmedConsumer)
	if err != nil {
		return fmt.Errorf("matcher settlement confirmed subscribe failed: %w", err)
	}

	logger.Infof("matcher pull consumer started subject=%s.*", protocol.SubjectOrderReserved)
	logger.Infof("matcher settlement consumer started subject=%s.*", protocol.SubjectSettlementConfirmed)

	go func() {
		defer func() {
			_ = sub.Unsubscribe()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			msgs, err := sub.Fetch(32, nats.MaxWait(1500*time.Millisecond))
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				logger.Warnf("matcher fetch failed: %v", err)
				time.Sleep(time.Second)
				continue
			}
			for _, msg := range msgs {
				wrapper, err := m.parseCommandFromJSON(msg.Data)
				if err != nil {
					logger.Warnf("failed to parse command: %v", err)
					_ = msg.Term()
					continue
				}
				wrapper.Msg = msg
				if meta, metaErr := msg.Metadata(); metaErr == nil {
					wrapper.SourceCmdSeq = meta.Sequence.Stream
				}
				cmd, _ := wrapper.Cmd.(*PlaceOrderCommand)
				if cmd != nil {
					cmd.SourceCmdSeq = wrapper.SourceCmdSeq
				}
				actor := m.GetOrCreateMarket(wrapper.Cmd.GetMarketID(), cmd.MarketPDA)
				select {
				case actor.CmdChan <- wrapper:
				default:
					logger.Warnf("backpressure market=%d channel full, naking", actor.MarketID)
					_ = msg.NakWithDelay(500 * time.Millisecond)
				}
			}
		}
	}()
	go func() {
		defer func() {
			_ = settlementSub.Unsubscribe()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			msgs, err := settlementSub.Fetch(32, nats.MaxWait(1500*time.Millisecond))
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
					continue
				}
				logger.Warnf("matcher settlement fetch failed: %v", err)
				time.Sleep(time.Second)
				continue
			}
			for _, msg := range msgs {
				m.handleSettlementConfirmed(msg)
			}
		}
	}()
	return nil
}

func (m *MarketManager) runMatcherEngine(actor *MarketActor) {
	logger.Infof("Market %d matcher engine started", actor.MarketID)
	ticker := time.NewTicker(m.batchConfig.FlushTick)
	defer ticker.Stop()

	for {
		select {
		case wrapper, ok := <-actor.CmdChan:
			if !ok {
				_ = m.flushActorBatch(actor, time.Now().UTC())
				return
			}
			currentSeq := wrapper.SourceCmdSeq
			if currentSeq > 0 && currentSeq <= actor.Book.LastProcessedSeq {
				if wrapper.Msg != nil {
					_ = wrapper.Msg.Ack()
				}
				continue
			}
			if actor.Pending != nil && actor.Pending.hasSeq(currentSeq) {
				if wrapper.Msg != nil {
					_ = wrapper.Msg.Ack()
				}
				continue
			}
			if actor.Pending == nil {
				actor.Pending = newPendingBatch(actor.MarketID, actor.MarketPDA, time.Now().UTC())
			}
			actor.Pending.includeWrapper(wrapper, time.Now().UTC())
			actor.Book.ProcessCommand(wrapper.Cmd, actor.Pending)
			if actor.Pending.shouldFlush(time.Now().UTC(), m.batchConfig, m.maxFillsForBatch(actor.Pending), false) {
				_ = m.flushActorBatch(actor, time.Now().UTC())
			}
		case <-ticker.C:
			_ = m.flushActorBatch(actor, time.Now().UTC())
		}
	}
}

func (m *MarketManager) flushActorBatch(actor *MarketActor, now time.Time) error {
	if actor.Pending == nil || !actor.Pending.shouldFlush(now, m.batchConfig, m.maxFillsForBatch(actor.Pending), true) {
		return nil
	}
	if estimate, err := m.estimatePendingTxBytes(actor.Pending); err == nil && m.maxSettlementTxBytes > 0 && estimate.TransactionBytes > m.maxSettlementTxBytes {
		logger.Warnf("matcher batch estimated settlement tx bytes over limit market=%d bytes=%d limit=%d orders=%d cold_orders=%d fills=%d versioned=%t lookups=%d",
			actor.MarketID, estimate.TransactionBytes, m.maxSettlementTxBytes, len(actor.Pending.event.Orders), m.countColdOrders(actor.Pending), len(actor.Pending.event.Fills), estimate.Versioned, estimate.LookupAccounts)
	}
	event := actor.Pending.freeze(now)
	evtSeq, err := m.publishMatcherOutputs(event)
	if err != nil {
		logger.Warnf("failed to publish matcher outputs market=%d: %v", actor.MarketID, err)
		return err
	}
	if event.SourceCmdSeqMax > actor.Book.LastProcessedSeq {
		actor.Book.LastProcessedSeq = event.SourceCmdSeqMax
	}
	if event.SourceCmdSeqMax > 0 {
		if m.pool == nil {
			m.setCheckpoint(actor.MarketID, event.SourceCmdSeqMax)
			ackCommandWrappers(actor.Pending.wrappers)
		} else {
			actor.stageCheckpoint(evtSeq, event.SourceCmdSeqMax, actor.Pending.wrappers)
		}
	} else {
		ackCommandWrappers(actor.Pending.wrappers)
	}
	actor.Pending = newPendingBatch(actor.MarketID, actor.MarketPDA, now)
	return nil
}

func (m *MarketManager) settlementTxShape(batch *pendingBatch) (internalsolana.SettlementTxShape, bool) {
	uniqueWallets := make(map[string]struct{}, len(batch.event.Orders))
	coldOrders := 0
	for _, order := range batch.event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		if wallet != "" {
			uniqueWallets[wallet] = struct{}{}
		}
		if wallet == "" || order.Execution.Nonce == 0 || m.orderRegistry == nil || !m.orderRegistry.Has(batch.event.MarketID, wallet, order.Execution.Nonce) {
			coldOrders++
		}
	}
	return internalsolana.SettlementTxShape{
		UniqueUsers: len(uniqueWallets),
		Orders:      len(batch.event.Orders),
		ColdOrders:  coldOrders,
		Fills:       0,
	}, coldOrders > 0
}

func (m *MarketManager) countColdOrders(batch *pendingBatch) int {
	shape, _ := m.settlementTxShape(batch)
	return shape.ColdOrders
}

func (m *MarketManager) estimatePendingTxBytes(batch *pendingBatch) (internalsolana.SettlementTxEstimate, error) {
	if m == nil || m.txEstimator == nil || batch == nil {
		return internalsolana.SettlementTxEstimate{}, nil
	}
	shape, _ := m.settlementTxShape(batch)
	shape.Fills = len(batch.event.Fills)
	return m.txEstimator.Estimate(shape)
}

func (m *MarketManager) maxFillsForBatch(batch *pendingBatch) int {
	if batch == nil {
		return m.batchConfig.MaxFillsHot
	}
	shape, hasColdOrder := m.settlementTxShape(batch)
	hasColdPosition := batch.requiresColdLimit(m.positionRegistry)
	ceiling := m.batchConfig.MaxFillsHot
	if (hasColdOrder || hasColdPosition) && m.batchConfig.MaxFillsCold > 0 {
		ceiling = m.batchConfig.MaxFillsCold
	}
	if ceiling <= 0 {
		ceiling = m.batchConfig.MaxFillsHot
	}
	if m.txEstimator == nil || m.maxSettlementTxBytes <= 0 {
		return ceiling
	}
	allowed, _, err := m.txEstimator.MaxFillsWithinLimit(shape, m.maxSettlementTxBytes, ceiling)
	if err != nil || allowed <= 0 {
		return ceiling
	}
	return allowed
}

func (m *MarketManager) publishMatcherOutputs(event MatchBatchEvent) (uint64, error) {
	if len(event.Fills) == 0 && len(event.OrderUpdates) == 0 && len(event.DepthUpdates) == 0 {
		return 0, nil
	}
	deltaSeq, err := m.publishMarketDelta(event)
	if err != nil {
		return 0, err
	}
	execSeq := uint64(0)
	if len(event.Fills) > 0 {
		execSeq, err = m.publishMatchExecution(event)
		if err != nil {
			return 0, err
		}
	}
	m.publishOrderReleasedEvents(event)
	if execSeq > deltaSeq {
		return execSeq, nil
	}
	return deltaSeq, nil
}

func (m *MarketManager) publishMarketDelta(event MatchBatchEvent) (uint64, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal market delta: %w", err)
	}
	msg := nats.NewMsg(protocol.SubjectMarketDeltaMarket(event.MarketID))
	msg.Data = data
	if event.EventID != "" {
		msg.Header.Set(nats.MsgIdHdr, event.EventID+"-delta")
	}
	ack, err := m.client.JetStream().PublishMsg(msg)
	if err != nil {
		return 0, fmt.Errorf("failed to publish market delta: %w", err)
	}
	if err := m.client.PublishCoreJSON(protocol.SubjectMarketDeltaHotMarket(event.MarketID), event); err != nil {
		logger.Warnf("failed to publish hot market delta market=%d event=%s err=%v", event.MarketID, event.EventID, err)
	}
	if ack == nil {
		return 0, nil
	}
	return ack.Sequence, nil
}

func (m *MarketManager) publishMatchExecution(event MatchBatchEvent) (uint64, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal match execution: %w", err)
	}
	msg := nats.NewMsg(protocol.SubjectMatchExecutionMarket(event.MarketID))
	msg.Data = data
	if event.EventID != "" {
		msg.Header.Set(nats.MsgIdHdr, event.EventID+"-execution")
	}
	ack, err := m.client.JetStream().PublishMsg(msg)
	if err != nil {
		return 0, fmt.Errorf("failed to publish match execution: %w", err)
	}
	if ack == nil {
		return 0, nil
	}
	return ack.Sequence, nil
}

func (m *MarketManager) parseCommandFromJSON(data []byte) (*CommandWrapper, error) {
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse command envelope: %w", err)
	}
	cmd := ConvertProtocolToPlaceOrderCommand(env.Payload)
	wrapper := &CommandWrapper{Cmd: cmd}
	return wrapper, nil
}

func (m *MarketManager) RecoverFromStore(ctx context.Context) error {
	if m.pool == nil {
		return nil
	}
	if err := m.loadUserPositionRegistry(ctx); err != nil {
		return err
	}
	if err := m.loadOrderStateRegistry(ctx); err != nil {
		return err
	}
	if err := m.loadCheckpoints(ctx); err != nil {
		return err
	}
	rows, err := m.pool.Query(ctx, `
        SELECT
            o.order_id,
            o.market_id,
            o.wallet_address,
            o.original_action,
            o.original_outcome,
            o.original_price_tick,
            o.side,
            o.order_type,
            o.price_tick,
            o.remaining_spend_amount,
            o.remaining_qty,
            o.expire_time,
            o.signature,
            o.intent_hex,
            o.nonce,
            o.created_cmd_seq,
            EXTRACT(EPOCH FROM o.created_at)::BIGINT AS created_at,
            COALESCE(mk.market_pda, ''),
            EXTRACT(EPOCH FROM mk.close_time)::BIGINT AS close_time
        FROM orders o
        JOIN markets mk ON mk.market_id = o.market_id
        WHERE o.status IN (1, 2)
        ORDER BY o.market_id ASC, o.side ASC, o.price_tick ASC, o.created_cmd_seq ASC
    `)
	if err != nil {
		return err
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var (
			orderID           int64
			marketIDStr       string
			walletAddress     string
			originalAction    string
			originalOutcome   string
			originalPriceTick int16
			side              int16
			orderType         int16
			priceTick         int16
			remainingSpend    int64
			remainingQty      int64
			expireTime        int64
			signature         string
			intentHex         string
			nonce             int64
			createdCmdSeq     int64
			createdAt         int64
			marketPDA         string
			closeTime         int64
		)
		if err := rows.Scan(
			&orderID, &marketIDStr, &walletAddress, &originalAction, &originalOutcome, &originalPriceTick,
			&side, &orderType, &priceTick, &remainingSpend, &remainingQty, &expireTime, &signature,
			&intentHex, &nonce, &createdCmdSeq, &createdAt, &marketPDA, &closeTime,
		); err != nil {
			return err
		}
		marketIDU, _ := strconv.ParseUint(marketIDStr, 10, 64)
		actor := m.actors[marketIDU]
		if actor == nil {
			actor = NewMarketActor(marketIDU, marketPDA)
			actor.Book.CloseTime = closeTime
			if checkpoint := m.checkpoints[marketIDU]; checkpoint > actor.Book.LastProcessedSeq {
				actor.Book.LastProcessedSeq = checkpoint
			}
			m.actors[marketIDU] = actor
		}
		order := AcquireOrder()
		order.OrderID = uint64(orderID)
		order.MarketPDA = marketPDA
		order.WalletAddress = walletAddress
		order.OriginalAction = toMatchingSide(protocol.Side(originalAction))
		order.OriginalOutcome = toMatchingOutcome(protocol.Outcome(originalOutcome))
		order.OriginalPriceTick = uint8(originalPriceTick)
		order.Side = uint8(side)
		order.OrderType = uint8(orderType)
		order.PriceTick = uint8(priceTick)
		order.RemainingSpend = uint64(remainingSpend)
		order.RemainingQty = uint64(remainingQty)
		order.ExpireTime = expireTime
		order.Timestamp = createdAt
		order.Signature = signature
		order.IntentBytesHex = intentHex
		order.Nonce = uint64(nonce)
		order.CreatedCmdSeq = uint64(createdCmdSeq)
		actor.Book.RestoreOrder(order)
		if createdCmdSeq > 0 && uint64(createdCmdSeq) > actor.Book.LastProcessedSeq {
			actor.Book.LastProcessedSeq = uint64(createdCmdSeq)
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, actor := range m.actors {
		m.ensureActorStartedLocked(actor)
	}
	return nil
}

func (m *MarketManager) loadUserPositionRegistry(ctx context.Context) error {
	if m.positionRepo == nil || m.positionRegistry == nil {
		return nil
	}
	keys, err := m.positionRepo.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("matcher load user position registry: %w", err)
	}
	m.positionRegistry.Load(keys)
	logger.Infof("matcher loaded user position registry entries=%d", m.positionRegistry.Size())
	return nil
}

func (m *MarketManager) loadOrderStateRegistry(ctx context.Context) error {
	if m.orderRepo == nil || m.orderRegistry == nil {
		return nil
	}
	keys, err := m.orderRepo.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("matcher load order state registry: %w", err)
	}
	m.orderRegistry.Load(keys)
	logger.Infof("matcher loaded order state registry entries=%d", m.orderRegistry.Size())
	return nil
}

func (m *MarketManager) loadCheckpoints(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
		SELECT market_id, last_source_cmd_seq
		FROM consumer_cursors
		WHERE consumer_name = $1
	`, matcherReservedConsumer)
	if err != nil {
		return err
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var marketIDText string
		var lastSourceCmdSeq int64
		if err := rows.Scan(&marketIDText, &lastSourceCmdSeq); err != nil {
			return err
		}
		marketID, parseErr := strconv.ParseUint(marketIDText, 10, 64)
		if parseErr != nil || lastSourceCmdSeq <= 0 {
			continue
		}
		m.checkpoints[marketID] = uint64(lastSourceCmdSeq)
	}
	return rows.Err()
}

func (m *MarketManager) saveCheckpoint(ctx context.Context, marketID uint64, evtSeq uint64, sourceCmdSeq uint64) error {
	if m.pool == nil || sourceCmdSeq == 0 {
		m.setCheckpoint(marketID, sourceCmdSeq)
		return nil
	}
	marketIDStr := strconv.FormatUint(marketID, 10)
	evtSeqInt, err := toSafeInt64(evtSeq)
	if err != nil {
		return err
	}
	sourceSeqInt, err := toSafeInt64(sourceCmdSeq)
	if err != nil {
		return err
	}
	_, err = m.pool.Exec(ctx, `
		INSERT INTO consumer_cursors (consumer_name, market_id, last_evt_seq, last_source_cmd_seq, updated_at)
		VALUES ($1, $2::NUMERIC(20,0), $3, $4, NOW())
		ON CONFLICT (consumer_name, market_id) DO UPDATE SET
			last_evt_seq = GREATEST(consumer_cursors.last_evt_seq, EXCLUDED.last_evt_seq),
			last_source_cmd_seq = GREATEST(consumer_cursors.last_source_cmd_seq, EXCLUDED.last_source_cmd_seq),
			updated_at = NOW()
	`, matcherReservedConsumer, marketIDStr, evtSeqInt, sourceSeqInt)
	if err != nil {
		return err
	}
	m.setCheckpoint(marketID, sourceCmdSeq)
	return nil
}

func (m *MarketManager) setCheckpoint(marketID uint64, sourceCmdSeq uint64) {
	if sourceCmdSeq == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if sourceCmdSeq > m.checkpoints[marketID] {
		m.checkpoints[marketID] = sourceCmdSeq
	}
}

func (m *MarketManager) expireOrdersAtRecovery(ctx context.Context, nowUnix int64) (int64, error) {
	if m.pool == nil {
		return 0, nil
	}
	tag, err := m.pool.Exec(ctx, `
		UPDATE orders
		SET status = $1,
		    updated_at = NOW()
		WHERE status IN ($2, $3)
		  AND expire_time > 0
		  AND expire_time <= $4
	`,
		int16(StatusExpired),
		int16(StatusNew),
		int16(StatusPartiallyFilled),
		nowUnix,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (m *MarketManager) RunBootstrapTick(ctx context.Context) error {
	now := time.Now().UTC().Unix()
	_ = ctx
	for _, actor := range m.listActors() {
		batch := newPendingBatch(actor.MarketID, actor.MarketPDA, time.Now().UTC())
		actor.Book.ProcessCommand(&TickCommand{MarketID: actor.MarketID, Timestamp: now}, batch)
		if batch.hasPayload() {
			if _, err := m.publishMatcherOutputs(batch.freeze(time.Now().UTC())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *MarketManager) handleSettlementConfirmed(msg *nats.Msg) {
	var event protocol.SettlementConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logger.Warnf("matcher decode settlement confirmed failed: %v", err)
		_ = msg.Term()
		return
	}
	for _, wallet := range event.Wallets {
		m.positionRegistry.MarkExists(event.MarketID, wallet)
	}
	_ = msg.Ack()
}

func (m *MarketManager) ObserveProcessedSettlement(marketID uint64, wallets []string, matchEventJSON []byte) {
	for _, wallet := range wallets {
		m.positionRegistry.MarkExists(marketID, wallet)
	}
	if len(matchEventJSON) == 0 || m.orderRegistry == nil {
		return
	}
	var event MatchBatchEvent
	if err := json.Unmarshal(matchEventJSON, &event); err != nil {
		logger.Warnf("matcher observe processed settlement decode failed market=%d err=%v", marketID, err)
		return
	}
	for _, order := range event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		nonce := order.Execution.Nonce
		if wallet == "" || nonce == 0 {
			continue
		}
		m.orderRegistry.MarkExists(marketID, wallet, nonce)
	}
}

func (m *MarketManager) StartTickLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now().UTC().Unix()
				for _, actor := range m.listActors() {
					batch := newPendingBatch(actor.MarketID, actor.MarketPDA, time.Now().UTC())
					actor.Book.ProcessCommand(&TickCommand{MarketID: actor.MarketID, Timestamp: now}, batch)
					if batch.hasPayload() {
						if _, err := m.publishMatcherOutputs(batch.freeze(time.Now().UTC())); err != nil {
							logger.Warnf("tick publish failed market=%d err=%v", actor.MarketID, err)
						}
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (m *MarketManager) StartCheckpointLoop(ctx context.Context) {
	if m == nil || m.pool == nil {
		return
	}
	interval := m.checkpointInterval
	if interval <= 0 {
		interval = defaultMatcherCheckpointInterval
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				flushCtx, cancel := context.WithTimeout(context.Background(), checkpointFlushTimeout(interval))
				err := m.flushCheckpointBatch(flushCtx)
				cancel()
				if err != nil {
					logger.Warnf("matcher checkpoint batch flush failed: %v", err)
				}
			case <-ctx.Done():
				flushCtx, cancel := context.WithTimeout(context.Background(), checkpointFlushTimeout(interval))
				err := m.flushCheckpointBatch(flushCtx)
				cancel()
				if err != nil {
					logger.Warnf("matcher final checkpoint flush failed: %v", err)
				}
				return
			}
		}
	}()
}

type matcherCheckpointTarget struct {
	actor        *MarketActor
	marketID     uint64
	evtSeq       uint64
	sourceCmdSeq uint64
}

func (m *MarketManager) flushCheckpointBatch(ctx context.Context) error {
	if m == nil || m.pool == nil {
		return nil
	}
	targets := m.collectCheckpointTargets()
	if len(targets) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	queued := make([]matcherCheckpointTarget, 0, len(targets))
	for _, target := range targets {
		if target.sourceCmdSeq == 0 {
			continue
		}
		evtSeqInt, err := toSafeInt64(target.evtSeq)
		if err != nil {
			return fmt.Errorf("matcher checkpoint evt seq market=%d: %w", target.marketID, err)
		}
		sourceSeqInt, err := toSafeInt64(target.sourceCmdSeq)
		if err != nil {
			return fmt.Errorf("matcher checkpoint source seq market=%d: %w", target.marketID, err)
		}
		batch.Queue(`
			INSERT INTO consumer_cursors (consumer_name, market_id, last_evt_seq, last_source_cmd_seq, updated_at)
			VALUES ($1, $2::NUMERIC(20,0), $3, $4, NOW())
			ON CONFLICT (consumer_name, market_id) DO UPDATE SET
				last_evt_seq = GREATEST(consumer_cursors.last_evt_seq, EXCLUDED.last_evt_seq),
				last_source_cmd_seq = GREATEST(consumer_cursors.last_source_cmd_seq, EXCLUDED.last_source_cmd_seq),
				updated_at = NOW()
		`, matcherReservedConsumer, strconv.FormatUint(target.marketID, 10), evtSeqInt, sourceSeqInt)
		queued = append(queued, target)
	}
	if len(queued) == 0 {
		return nil
	}

	results := m.pool.SendBatch(ctx, batch)
	flushed := make([]matcherCheckpointTarget, 0, len(queued))
	var flushErr error
	for _, target := range queued {
		if _, err := results.Exec(); err != nil {
			flushErr = fmt.Errorf("matcher checkpoint flush market=%d source_cmd_seq=%d: %w", target.marketID, target.sourceCmdSeq, err)
			break
		}
		flushed = append(flushed, target)
	}
	if err := results.Close(); err != nil && flushErr == nil {
		flushErr = fmt.Errorf("matcher checkpoint batch close: %w", err)
	}
	for _, target := range flushed {
		m.setCheckpoint(target.marketID, target.sourceCmdSeq)
		ackCommandWrappers(target.actor.completeCheckpoint(matcherCheckpointSnapshot{
			evtSeq:       target.evtSeq,
			sourceCmdSeq: target.sourceCmdSeq,
		}))
	}
	return flushErr
}

func (m *MarketManager) collectCheckpointTargets() []matcherCheckpointTarget {
	actors := m.listActors()
	targets := make([]matcherCheckpointTarget, 0, len(actors))
	for _, actor := range actors {
		snapshot, ok := actor.checkpointSnapshot()
		if !ok {
			continue
		}
		targets = append(targets, matcherCheckpointTarget{
			actor:        actor,
			marketID:     actor.MarketID,
			evtSeq:       snapshot.evtSeq,
			sourceCmdSeq: snapshot.sourceCmdSeq,
		})
	}
	return targets
}

func checkpointFlushTimeout(interval time.Duration) time.Duration {
	timeout := interval * 4
	if timeout < time.Second {
		return time.Second
	}
	return timeout
}

// publishOrderReleasedEvents publishes an evt.order.released.{market_id} event for
// each order update with a terminal status (canceled / expired / rejected).
// This allows funds to release locked assets and writer to update order status without
// re-parsing the entire match batch.
func (m *MarketManager) publishOrderReleasedEvents(event MatchBatchEvent) {
	orderByIndex := make(map[uint16]MatchedOrder, len(event.Orders))
	for _, o := range event.Orders {
		orderByIndex[o.OrderIndex] = o
	}
	for _, update := range event.OrderUpdates {
		switch update.Status {
		case "canceled", "expired", "rejected":
		default:
			continue
		}
		order, ok := orderByIndex[update.OrderIndex]
		if !ok {
			continue
		}
		exec := order.Execution
		released := protocol.OrderReleasedEvent{
			MatchEventID:      event.EventID,
			OrderID:           order.OrderID,
			MarketID:          event.MarketID,
			MarketPDA:         event.MarketPDA,
			WalletAddress:     exec.WalletAddress,
			OriginalAction:    exec.OriginalAction,
			OriginalOutcome:   exec.OriginalOutcome,
			OriginalPriceTick: exec.OriginalPriceTick,
			OrderType:         exec.OrderType,
			RemainingQtyLots:  update.RemainingQtyLots,
			RefundAmount:      update.RefundAmount,
			Status:            update.Status,
			ReasonCode:        update.ReasonCode,
			ReleasedAt:        event.ProducedAt,
		}
		data, err := json.Marshal(released)
		if err != nil {
			logger.Warnf("marshal OrderReleased failed order=%d err=%v", order.OrderID, err)
			continue
		}
		subject := protocol.SubjectOrderReleasedMarket(event.MarketID)
		// Use idempotency key: match_event_id + order_id to prevent duplicate processing
		msgID := event.EventID + "-released-" + strconv.FormatUint(order.OrderID, 10)
		if _, err := m.client.JetStream().Publish(subject, data, nats.MsgId(msgID)); err != nil {
			logger.Warnf("publish OrderReleased failed order=%d err=%v", order.OrderID, err)
		}
	}
}

func toSafeInt64(v uint64) (int64, error) {
	const maxInt64 = ^uint64(0) >> 1
	if v > maxInt64 {
		return 0, fmt.Errorf("value %d overflows int64", v)
	}
	return int64(v), nil
}

func ackCommandWrappers(wrappers []*CommandWrapper) {
	for _, wrapper := range wrappers {
		if wrapper == nil || wrapper.Msg == nil {
			continue
		}
		_ = wrapper.Msg.Ack()
	}
}
