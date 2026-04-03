package matching

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/protocol"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var logger = logging.New("matcher")

const matcherReservedConsumer = "matcher-reserved-primary"

type ManagerConfig struct {
	Batch BatchConfig
}

// MarketManager 负责监听路由和管理所有市场Actor
type MarketManager struct {
	client      *natsjs.Client
	pool        *pgxpool.Pool
	batchConfig BatchConfig
	mu          sync.RWMutex
	actors      map[uint64]*MarketActor
}

// NewMarketManager 创建市场管理器
func NewMarketManager(client *natsjs.Client, pool *pgxpool.Pool, cfg ManagerConfig) *MarketManager {
	batchCfg := cfg.Batch
	if batchCfg == (BatchConfig{}) {
		batchCfg = DefaultBatchConfig()
	}
	return &MarketManager{
		client:      client,
		pool:        pool,
		batchConfig: batchCfg,
		actors:      make(map[uint64]*MarketActor),
	}
}

// GetOrCreateMarket 获取或创建市场Actor（动态寻址）
func (m *MarketManager) GetOrCreateMarket(marketID uint64, marketPDA string) *MarketActor {
	m.mu.Lock()
	defer m.mu.Unlock()
	actor, exists := m.actors[marketID]
	if !exists {
		actor = NewMarketActor(marketID, marketPDA)
		m.actors[marketID] = actor
		m.ensureActorStartedLocked(actor)
	} else if actor.MarketPDA == "" && marketPDA != "" {
		actor.MarketPDA = marketPDA
		if actor.Pending != nil && actor.Pending.event.MarketPDA == "" {
			actor.Pending.event.MarketPDA = marketPDA
		}
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

// StartConsumer 启动NATS消费者（防撑爆与人质劫持）
func (m *MarketManager) StartConsumer(ctx context.Context) error {
	sub, err := m.client.JetStream().QueueSubscribe(protocol.SubjectOrderReservedV1+".*", "matcher_group", func(msg *nats.Msg) {
		wrapper, err := m.parseCommandFromJSON(msg.Data)
		if err != nil {
			logger.Warnf("failed to parse command: %v", err)
			msg.Nak()
			return
		}
		wrapper.Msg = msg
		if meta, metaErr := msg.Metadata(); metaErr == nil {
			wrapper.SourceCmdSeq = meta.Sequence.Stream
		}
		cmd, _ := wrapper.Cmd.(*PlaceOrderCommand)
		actor := m.GetOrCreateMarket(wrapper.Cmd.GetMarketID(), cmd.MarketPDA)
		select {
		case actor.CmdChan <- wrapper:
		default:
			logger.Warnf("backpressure market=%d channel full, rejecting message", actor.MarketID)
			msg.Nak()
		}
	}, nats.Durable(matcherReservedConsumer), nats.ManualAck(), nats.DeliverNew())
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	logger.Infof("Matcher consumer started on subject: %s.*", protocol.SubjectOrderReservedV1)

	<-ctx.Done()
	_ = sub.Unsubscribe()
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
			if actor.Pending.shouldFlush(time.Now().UTC(), m.batchConfig, false) {
				_ = m.flushActorBatch(actor, time.Now().UTC())
			}
		case <-ticker.C:
			_ = m.flushActorBatch(actor, time.Now().UTC())
		}
	}
}

func (m *MarketManager) flushActorBatch(actor *MarketActor, now time.Time) error {
	if actor.Pending == nil || !actor.Pending.shouldFlush(now, m.batchConfig, true) {
		return nil
	}
	event := actor.Pending.freeze(now)
	if err := m.publishMatchBatch(event); err != nil {
		logger.Warnf("failed to publish match batch market=%d: %v", actor.MarketID, err)
		return err
	}
	for _, wrapper := range actor.Pending.wrappers {
		if wrapper.SourceCmdSeq > actor.Book.LastProcessedSeq {
			actor.Book.LastProcessedSeq = wrapper.SourceCmdSeq
		}
		if wrapper.Msg != nil {
			_ = wrapper.Msg.Ack()
		}
	}
	actor.Pending = newPendingBatch(actor.MarketID, actor.MarketPDA, now)
	return nil
}

func (m *MarketManager) publishMatchBatch(event MatchBatchEventV2) error {
	if len(event.Fills) == 0 && len(event.OrderUpdates) == 0 && len(event.DepthUpdates) == 0 {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal match batch: %w", err)
	}
	subject := protocol.SubjectMatchBatchV2Market(event.MarketID)
	if _, err := m.client.JetStream().Publish(subject, data); err != nil {
		return fmt.Errorf("failed to publish to %s: %w", subject, err)
	}
	return nil
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
			marketPDA         string
			closeTime         int64
		)
		if err := rows.Scan(
			&orderID, &marketIDStr, &walletAddress, &originalAction, &originalOutcome, &originalPriceTick,
			&side, &orderType, &priceTick, &remainingSpend, &remainingQty, &expireTime, &signature,
			&intentHex, &nonce, &marketPDA, &closeTime,
		); err != nil {
			return err
		}
		marketIDU, _ := strconv.ParseUint(marketIDStr, 10, 64)
		actor := m.actors[marketIDU]
		if actor == nil {
			actor = NewMarketActor(marketIDU, marketPDA)
			actor.Book.CloseTime = closeTime
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
		order.Signature = signature
		order.IntentBytesHex = intentHex
		order.Nonce = uint64(nonce)
		actor.Book.RestoreOrder(order)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, actor := range m.actors {
		m.ensureActorStartedLocked(actor)
	}
	return nil
}

func (m *MarketManager) RunBootstrapTick(ctx context.Context) error {
	now := time.Now().UTC().Unix()
	_ = ctx
	for _, actor := range m.listActors() {
		batch := newPendingBatch(actor.MarketID, actor.MarketPDA, time.Now().UTC())
		actor.Book.ProcessCommand(&TickCommand{MarketID: actor.MarketID, Timestamp: now}, batch)
		if batch.hasPayload() {
			if err := m.publishMatchBatch(batch.freeze(time.Now().UTC())); err != nil {
				return err
			}
		}
	}
	return nil
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
						if err := m.publishMatchBatch(batch.freeze(time.Now().UTC())); err != nil {
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
