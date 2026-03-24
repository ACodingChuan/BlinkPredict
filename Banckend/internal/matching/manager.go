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

// MarketManager 负责监听路由和管理所有市场Actor
type MarketManager struct {
	client *natsjs.Client
	pool   *pgxpool.Pool
	mu     sync.RWMutex
	actors map[uint64]*MarketActor
}

// NewMarketManager 创建市场管理器
func NewMarketManager(client *natsjs.Client, pool *pgxpool.Pool) *MarketManager {
	return &MarketManager{
		client: client,
		pool:   pool,
		actors: make(map[uint64]*MarketActor),
	}
}

// GetOrCreateMarket 获取或创建市场Actor（动态寻址）
func (m *MarketManager) GetOrCreateMarket(marketID uint64) *MarketActor {
	m.mu.Lock()
	defer m.mu.Unlock()
	actor, exists := m.actors[marketID]
	if !exists {
		actor = NewMarketActor(marketID)
		m.actors[marketID] = actor
		m.ensureActorStartedLocked(actor)
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
	// 订阅命令流 (Queue Group 保证单机处理)
	sub, err := m.client.JetStream().QueueSubscribe("cmd.order.place", "matcher_group", func(msg *nats.Msg) {
		wrapper, err := m.parseCommandFromJSON(msg.Data)
		if err != nil {
			logger.Warnf("failed to parse command: %v", err)
			msg.Nak()
			return
		}

		// 绑定NATS消息到wrapper
		wrapper.Msg = msg
		if meta, err := msg.Metadata(); err == nil {
			wrapper.SourceCmdSeq = meta.Sequence.Stream
		}

		actor := m.GetOrCreateMarket(wrapper.Cmd.GetMarketID())

		// 【防线：背压限流机制】
		select {
		case actor.CmdChan <- wrapper:
			// 成功投递到内存信箱，但绝对不在这里ACK
		default:
			logger.Warnf("backpressure market=%d channel full, rejecting message", actor.MarketID)
			msg.Nak() // 告诉NATS我吃不下了，一会再发
		}
	}, nats.Durable("matcher_consumer"))
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	logger.Infof("Matcher consumer started on subject: cmd.order.place")

	<-ctx.Done()
	sub.Unsubscribe()
	return nil
}

// runMatcherEngine 引擎主循环：安全ACK与幂等防重放
func (m *MarketManager) runMatcherEngine(actor *MarketActor) {
	logger.Infof("Market %d matcher engine started", actor.MarketID)

	for wrapper := range actor.CmdChan {
		currentSeq := wrapper.SourceCmdSeq

		// 【防线：幂等性双花拦截】
		if currentSeq > 0 && currentSeq <= actor.Book.LastProcessedSeq {
			logger.Infof("idempotency shield ignoring seq=%d last_processed=%d",
				currentSeq, actor.Book.LastProcessedSeq)
			if wrapper.Msg != nil {
				wrapper.Msg.Ack()
			}
			continue
		}

		// 创建批量事件
		batch := &BatchEventPayload{
			MarketID:     actor.MarketID,
			SourceCmdSeq: currentSeq,
			Timestamp:    wrapper.Cmd.GetTimestamp(),
			TradeEvents:  make([]TradeEvent, 0, 4),
			StateEvents:  make([]OrderStateEvent, 0, 4),
			DepthEvents:  make([]L2DepthEvent, 0, 4),
		}
		if cmd, ok := wrapper.Cmd.(*PlaceOrderCommand); ok {
			batch.SourceOrder = &FullOrderData{
				OrderID:            cmd.OrderID,
				WalletAddress:      cmd.WalletAddress,
				OriginalAction:     cmd.OriginalAction,
				OriginalOutcome:    cmd.OriginalOutcome,
				OriginalPriceTick:  cmd.OriginalPriceTick,
				Side:               cmd.Side,
				OrderType:          cmd.OrderType,
				PriceTick:          cmd.PriceTick,
				InitialQty:         cmd.QtyLots,
				InitialSpendAmount: cmd.SpendAmount,
				ExpireTime:         cmd.ExpireTime,
				Signature:          cmd.Signature,
				IntentBytesHex:     cmd.IntentBytesHex,
				Nonce:              cmd.Nonce,
				CreatedCmdSeq:      currentSeq,
			}
		}

		// 1. 核心推演
		actor.Book.ProcessCommand(wrapper.Cmd, batch)

		// 2. 广播事件流
		if err := m.publishBatch(batch); err != nil {
			logger.Warnf("failed to publish event stream: %v", err)
			if wrapper.Msg != nil {
				wrapper.Msg.Nak() // 发布失败，连带命令一起退回重来
			}
			continue
		}

		// 3. 【终极安全闭环】：事件已固化，更新游标，正式释放人质
		if currentSeq > 0 {
			actor.Book.LastProcessedSeq = currentSeq
		}
		if wrapper.Msg != nil {
			wrapper.Msg.Ack()
		}
	}
}

// publishBatch 发布批量事件到NATS事件流
func (m *MarketManager) publishBatch(batch *BatchEventPayload) error {
	if len(batch.TradeEvents) == 0 && len(batch.StateEvents) == 0 && len(batch.DepthEvents) == 0 {
		return nil // 无副作用，空耗
	}

	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("failed to marshal batch: %w", err)
	}

	// 发布到事件流: evt.trades.{market_id}
	subject := fmt.Sprintf("evt.trades.%d", batch.MarketID)
	_, err = m.client.JetStream().Publish(subject, data)
	if err != nil {
		return fmt.Errorf("failed to publish to %s: %w", subject, err)
	}

	return nil
}

// parseCommandFromJSON 从JSON反序列化命令
func (m *MarketManager) parseCommandFromJSON(data []byte) (*CommandWrapper, error) {
	// 解析protocol.CommandEnvelope
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse command envelope: %w", err)
	}

	// 转换为matching.PlaceOrderCommand
	cmd := ConvertProtocolToPlaceOrderCommand(env.Payload)

	wrapper := &CommandWrapper{
		Cmd: cmd,
	}

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
			o.side,
			o.order_type,
			o.price_tick,
			o.initial_spend_amount,
			o.remaining_qty,
			o.expire_time,
			o.signature,
			o.intent_hex,
			o.nonce,
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
			orderID       int64
			marketIDStr   string
			walletAddress string
			side          int16
			orderType     int16
			priceTick     int16
			initialSpend  int64
			remainingQty  int64
			expireTime    int64
			signature     string
			intentHex     string
			nonce         int64
			closeTime     int64
		)
		if err := rows.Scan(
			&orderID, &marketIDStr, &walletAddress, &side, &orderType, &priceTick,
			&initialSpend, &remainingQty, &expireTime, &signature, &intentHex, &nonce, &closeTime,
		); err != nil {
			return err
		}
		marketIDU, _ := strconv.ParseUint(marketIDStr, 10, 64)
		actor := m.actors[marketIDU]
		if actor == nil {
			actor = NewMarketActor(marketIDU)
			actor.Book.CloseTime = closeTime
			m.actors[marketIDU] = actor
		}
		order := AcquireOrder()
		order.OrderID = uint64(orderID)
		order.WalletAddress = walletAddress
		order.Side = uint8(side)
		order.OrderType = uint8(orderType)
		order.PriceTick = uint8(priceTick)
		order.RemainingSpend = uint64(initialSpend)
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
	for _, actor := range m.listActors() {
		batch := &BatchEventPayload{
			MarketID:     actor.MarketID,
			SourceCmdSeq: 0,
			Timestamp:    now,
			TradeEvents:  make([]TradeEvent, 0, 4),
			StateEvents:  make([]OrderStateEvent, 0, 4),
			DepthEvents:  make([]L2DepthEvent, 0, 8),
		}
		actor.Book.ProcessCommand(&TickCommand{MarketID: actor.MarketID, Timestamp: now}, batch)
		if err := m.publishBatch(batch); err != nil {
			return err
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
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				for _, actor := range m.listActors() {
					select {
					case actor.CmdChan <- &CommandWrapper{
						Cmd: &TickCommand{
							MarketID:  actor.MarketID,
							Timestamp: t.UTC().Unix(),
						},
						SourceCmdSeq: 0,
					}:
					default:
						logger.Warnf("tick backpressure market=%d", actor.MarketID)
					}
				}
			}
		}
	}()
}

// ==========================================
// TODO: 以下方法为预留接口
// ==========================================

// MarketMeta 市场元数据
type MarketMeta struct {
	CloseTime int64
}

// fetchMarketMetadata 从Redis/DB冷读取市场元数据（TODO）
// func (m *MarketManager) fetchMarketMetadata(marketID uint64) MarketMeta {
// 	// 实际生产中调用 Redis: HGET market_info:{marketID} close_time
// 	// 或者从PostgreSQL的markets表读取
// 	return MarketMeta{CloseTime: 0}
// }
