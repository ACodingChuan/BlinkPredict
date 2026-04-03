package writer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

var logger = logging.New("writer")

const defaultConsumerName = "writer-primary"
const (
	recentTradesLimit     = 100
	priceHistoryHotWindow = 90 * 24 * time.Hour
	priceHistoryMaxPoints = 100000
	writerCatchUpBatch    = 64
	writerRunBatch        = 32
)

type Writer struct {
	consumerName  string
	pool          *pgxpool.Pool
	client        *natsjs.Client
	rdb           *redis.Client
	sub           *nats.Subscription
	settlementSub *nats.Subscription
}

type pushMessages struct {
	marketDepths []protocol.MarketDepthPush
	marketTrades []protocol.MarketTradePush
	userOrders   []protocol.UserOrderPush
}

type positionDelta struct {
	MarketID              uint64
	WalletAddress         string
	YesFreeLotsDelta      int64
	YesLockedLotsDelta    int64
	YesPendingLotsDelta   int64
	NoFreeLotsDelta       int64
	NoLockedLotsDelta     int64
	NoPendingLotsDelta    int64
	CollateralLockedDelta int64
}

type orderMeta struct {
	OrderID           uint64
	WalletAddress     string
	OriginalAction    string
	OriginalOutcome   string
	OriginalPriceTick uint8
	OrderType         uint8
}

func New(pool *pgxpool.Pool, client *natsjs.Client, rdb *redis.Client, consumerName string) *Writer {
	if consumerName == "" {
		consumerName = defaultConsumerName
	}
	return &Writer{
		consumerName: consumerName,
		pool:         pool,
		client:       client,
		rdb:          rdb,
	}
}

func (w *Writer) Start(ctx context.Context) error {
	if w.pool == nil || w.client == nil {
		return nil
	}
	if w.rdb != nil {
		if err := w.rebuildRedisModels(ctx); err != nil {
			return fmt.Errorf("writer rebuild redis models: %w", err)
		}
	}
	if err := w.ensureSubscription(); err != nil {
		return err
	}
	if err := w.ensureSettlementSubscription(); err != nil {
		return err
	}
	if err := w.catchUp(ctx); err != nil {
		return err
	}
	go w.run(ctx)
	return nil
}

func (w *Writer) ensureSubscription() error {
	if w.sub != nil {
		return nil
	}
	sub, err := w.client.PullSubscribe(protocol.SubjectMatchBatchV2+".*", w.consumerName)
	if err != nil {
		return fmt.Errorf("writer subscribe: %w", err)
	}
	w.sub = sub
	return nil
}

func (w *Writer) ensureSettlementSubscription() error {
	if w.settlementSub != nil {
		return nil
	}
	sub, err := w.client.JetStream().QueueSubscribe(
		protocol.SubjectSettlementConfirm+".*",
		"writer_settlement_group",
		w.handleSettlementConfirmedMessage,
		nats.Durable("writer-settlement-confirmed"),
		nats.ManualAck(),
		nats.DeliverNew(),
	)
	if err != nil {
		return fmt.Errorf("writer settlement subscribe: %w", err)
	}
	w.settlementSub = sub
	return nil
}

func (w *Writer) catchUp(ctx context.Context) error {
	for {
		msgs, err := w.sub.Fetch(writerCatchUpBatch, nats.MaxWait(500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, msg := range msgs {
			w.handleMessage(ctx, msg)
		}
	}
}

func (w *Writer) run(ctx context.Context) {
	defer func() {
		if w.sub != nil {
			_ = w.sub.Unsubscribe()
		}
		if w.settlementSub != nil {
			_ = w.settlementSub.Unsubscribe()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := w.sub.Fetch(writerRunBatch, nats.MaxWait(1500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			logger.Warnf("fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}

		for _, msg := range msgs {
			w.handleMessage(ctx, msg)
		}
	}
}

func (w *Writer) handleMessage(ctx context.Context, msg *nats.Msg) {
	startedAt := time.Now()
	meta, err := msg.Metadata()
	if err != nil {
		logger.Warnf("metadata failed: %v", err)
		_ = msg.Nak()
		return
	}

	var event matching.MatchBatchEventV2
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logger.Warnf("decode failed stream_seq=%d deliveries=%d err=%v", meta.Sequence.Stream, meta.NumDelivered, err)
		_ = msg.Term()
		return
	}
	if event.SchemaVersion != 0 && event.SchemaVersion != 1 {
		logger.Warnf("unsupported schema version stream_seq=%d schema=%d", meta.Sequence.Stream, event.SchemaVersion)
		_ = msg.Term()
		return
	}
	batch := legacyBatchFromV2(event)
	logger.Infof("writer processing stream_seq=%d deliveries=%d market=%d source_cmd_seq=%d bytes=%d",
		meta.Sequence.Stream, meta.NumDelivered, batch.MarketID, batch.SourceCmdSeq, len(msg.Data))

	evtSeq, err := toInt64(meta.Sequence.Stream)
	if err != nil {
		logger.Warnf("evt_seq overflow: %v", err)
		_ = msg.Term()
		return
	}
	sourceCmdSeq, err := toInt64(batch.SourceCmdSeq)
	if err != nil {
		logger.Warnf("source_cmd_seq overflow: %v", err)
		_ = msg.Term()
		return
	}

	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		logger.Warnf("begin tx failed: %v", err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	lastEvtSeq, err := w.lockCursorRow(ctx, tx, batch.MarketID)
	if err != nil {
		// 如果 market 不存在，直接跳过这个 batch（不要一直重试）
		if strings.Contains(err.Error(), "does not exist in database") {
			logger.Warnf("skipping batch for non-existent market=%d", batch.MarketID)
			_ = msg.Ack()
			return
		}
		logger.Warnf("lock cursor failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if evtSeq <= lastEvtSeq {
		_ = msg.Ack()
		return
	}

	if err := w.upsertBatchOrders(ctx, tx, &batch); err != nil {
		logger.Warnf("upsert batch orders failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := w.persistTrades(ctx, tx, &batch); err != nil {
		logger.Warnf("persist trades failed stream_seq=%d market=%d deliveries=%d err=%v", meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := w.applyStateEvents(ctx, tx, &batch); err != nil {
		logger.Warnf("apply state events failed stream_seq=%d market=%d deliveries=%d err=%v", meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := w.applyPositionDeltas(ctx, tx, &batch); err != nil {
		logger.Warnf("apply position deltas failed stream_seq=%d market=%d deliveries=%d err=%v", meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := w.advanceCursor(ctx, tx, batch.MarketID, evtSeq, sourceCmdSeq); err != nil {
		logger.Warnf("advance cursor failed stream_seq=%d market=%d deliveries=%d err=%v", meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Warnf("commit failed stream_seq=%d market=%d deliveries=%d err=%v", meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	_, err = w.updateRedisReadModels(ctx, &batch)
	if err != nil {
		logger.Warnf("redis sync failed market=%d err=%v", batch.MarketID, err)
		_ = msg.Ack()
		return
	}
	_ = msg.Ack()
	elapsed := time.Since(startedAt)
	if elapsed > 250*time.Millisecond || meta.NumDelivered > 1 {
		logger.Infof("writer acked stream_seq=%d market=%d deliveries=%d elapsed=%s trades=%d states=%d depths=%d",
			meta.Sequence.Stream, batch.MarketID, meta.NumDelivered, elapsed.Round(time.Millisecond), len(batch.TradeEvents), len(batch.StateEvents), len(batch.DepthEvents))
	}
}

func (w *Writer) handleSettlementConfirmedMessage(msg *nats.Msg) {
	var event protocol.SettlementConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logger.Warnf("writer settlement decode failed: %v", err)
		_ = msg.Term()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		logger.Warnf("writer settlement begin tx failed market=%d err=%v", event.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	marketIDStr := strconv.FormatUint(event.MarketID, 10)
	for _, wallet := range event.Wallets {
		wallet = strings.TrimSpace(wallet)
		if wallet == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE positions
			SET
				yes_free_lots = yes_free_lots + yes_pending_lots,
				yes_pending_lots = 0,
				no_free_lots = no_free_lots + no_pending_lots,
				no_pending_lots = 0,
				updated_at = NOW()
			WHERE market_id = $1::NUMERIC(20,0) AND wallet_address = $2
		`, marketIDStr, wallet); err != nil {
			logger.Warnf("writer settlement apply positions failed market=%d wallet=%s err=%v", event.MarketID, wallet, err)
			_ = msg.NakWithDelay(time.Second)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		logger.Warnf("writer settlement commit failed market=%d err=%v", event.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if w.rdb != nil {
		if err := w.syncPositionRedis(ctx, &matching.BatchEventPayload{MarketID: event.MarketID}); err != nil {
			logger.Warnf("writer settlement redis sync failed market=%d err=%v", event.MarketID, err)
		}
	}
	_ = msg.Ack()
}

func (w *Writer) lockCursorRow(ctx context.Context, tx pgx.Tx, marketID uint64) (int64, error) {
	// 将 market_id 转换为字符串，避免 int64 溢出
	marketIDStr := strconv.FormatUint(marketID, 10)

	// 检查 market 是否存在于数据库中
	var marketExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM markets WHERE market_id = $1::NUMERIC(20,0)
		)
	`, marketIDStr).Scan(&marketExists); err != nil {
		return 0, fmt.Errorf("check market exists failed: %w", err)
	}

	if !marketExists {
		return 0, fmt.Errorf("market %s does not exist in database", marketIDStr)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO consumer_cursors (consumer_name, market_id, last_evt_seq, last_source_cmd_seq)
		VALUES ($1, $2::NUMERIC(20,0), 0, 0)
		ON CONFLICT (consumer_name, market_id) DO NOTHING
	`, w.consumerName, marketIDStr); err != nil {
		return 0, err
	}

	var lastEvtSeq int64
	if err := tx.QueryRow(ctx, `
		SELECT last_evt_seq
		FROM consumer_cursors
		WHERE consumer_name = $1 AND market_id = $2::NUMERIC(20,0)
		FOR UPDATE
	`, w.consumerName, marketIDStr).Scan(&lastEvtSeq); err != nil {
		return 0, err
	}
	return lastEvtSeq, nil
}

func (w *Writer) upsertSourceOrder(ctx context.Context, tx pgx.Tx, marketID uint64, timestamp int64, order *matching.FullOrderData) error {
	if order == nil {
		return nil
	}
	orderID, err := toInt64(order.OrderID)
	if err != nil {
		return err
	}
	marketIDStr := strconv.FormatUint(marketID, 10)
	initialQty, err := toInt64(order.InitialQty)
	if err != nil {
		return err
	}
	initialSpend, err := toInt64(order.InitialSpendAmount)
	if err != nil {
		return err
	}
	nonce, err := toInt64(order.Nonce)
	if err != nil {
		return err
	}
	createdCmdSeq, err := toInt64(order.CreatedCmdSeq)
	if err != nil {
		return err
	}
	eventTime := timestampToTime(timestamp)
	expireTime := order.ExpireTime
	_, err = tx.Exec(ctx, `
		INSERT INTO orders (
			order_id, market_id, wallet_address, original_action, original_outcome, original_price_tick, side, order_type, price_tick,
			initial_qty, initial_spend_amount, remaining_qty, remaining_spend_amount, expire_time,
			status, signature, intent_hex, nonce, created_cmd_seq, created_at, updated_at
		) VALUES (
			$1, $2::NUMERIC(20,0), $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $10, $11, $12,
			$13, $14, $15, $16, $17, $18, $18
		)
		ON CONFLICT (order_id) DO NOTHING
	`,
		orderID,
		marketIDStr,
		order.WalletAddress,
		sideLabel(order.OriginalAction),
		outcomeLabel(order.OriginalOutcome),
		int16(order.OriginalPriceTick),
		int16(order.Side),
		int16(order.OrderType),
		int16(order.PriceTick),
		initialQty,
		initialSpend,
		expireTime,
		int16(matching.StatusNew),
		order.Signature,
		order.IntentBytesHex,
		nonce,
		createdCmdSeq,
		eventTime,
	)
	return err
}

func (w *Writer) upsertBatchOrders(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	orders := sourceOrdersForBatch(batch)
	for i := range orders {
		order := orders[i]
		if err := w.upsertSourceOrder(ctx, tx, batch.MarketID, batch.Timestamp, &order); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) persistTrades(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	if len(batch.TradeEvents) == 0 {
		return nil
	}
	marketIDStr := strconv.FormatUint(batch.MarketID, 10)
	sourceCmdSeq, err := toInt64(batch.SourceCmdSeq)
	if err != nil {
		return err
	}
	executedAt := timestampToTime(batch.Timestamp)
	for _, trade := range batch.TradeEvents {
		makerOrderID, err := toInt64(trade.MakerOrderID)
		if err != nil {
			return err
		}
		takerOrderID, err := toInt64(trade.TakerOrderID)
		if err != nil {
			return err
		}
		matchQty, err := toInt64(trade.MatchQty)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO trades (
				trade_id, market_id, source_cmd_seq, match_price, match_qty,
				maker_order_id, taker_order_id,
				maker_wallet_address, taker_wallet_address,
				maker_signature, taker_signature,
				maker_intent_hex, taker_intent_hex, executed_at
			) VALUES (
				$1, $2::NUMERIC(20,0), $3, $4, $5,
				$6, $7,
				$8, $9,
				$10, $11,
				$12, $13, $14
			)
			ON CONFLICT (trade_id) DO NOTHING
		`,
			trade.TradeID,
			marketIDStr,
			sourceCmdSeq,
			int16(trade.MatchPrice),
			matchQty,
			makerOrderID,
			takerOrderID,
			trade.MakerPubKey,
			trade.TakerPubKey,
			trade.MakerSignature,
			trade.TakerSignature,
			trade.MakerIntentHex,
			trade.TakerIntentHex,
			executedAt,
		); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) applyStateEvents(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	now := timestampToTime(batch.Timestamp)
	for _, state := range batch.StateEvents {
		orderID, err := toInt64(state.OrderID)
		if err != nil {
			return err
		}
		remainingQty, err := toInt64(state.RemainingQty)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE orders
			SET remaining_qty = $1,
				remaining_spend_amount = $2,
				status = $3,
				updated_at = $4
			WHERE order_id = $5
		`, remainingQty, int64(state.RemainingSpendAmount), int16(state.Status), now, orderID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("state event references unknown order_id=%d", state.OrderID)
		}
	}
	return nil
}

func (w *Writer) applyPositionDeltas(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	deltas, err := w.buildPositionDeltas(ctx, tx, batch)
	if err != nil {
		return err
	}
	for _, delta := range deltas {
		if err := w.applySinglePositionDelta(ctx, tx, delta); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) buildPositionDeltas(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) ([]positionDelta, error) {
	metaByOrder, err := w.loadOrderMeta(ctx, tx, batch)
	if err != nil {
		return nil, err
	}
	acc := map[string]*positionDelta{}
	add := func(delta positionDelta) {
		key := fmt.Sprintf("%d:%s", delta.MarketID, delta.WalletAddress)
		if acc[key] == nil {
			copy := delta
			acc[key] = &copy
			return
		}
		existing := acc[key]
		existing.YesFreeLotsDelta += delta.YesFreeLotsDelta
		existing.YesLockedLotsDelta += delta.YesLockedLotsDelta
		existing.YesPendingLotsDelta += delta.YesPendingLotsDelta
		existing.NoFreeLotsDelta += delta.NoFreeLotsDelta
		existing.NoLockedLotsDelta += delta.NoLockedLotsDelta
		existing.NoPendingLotsDelta += delta.NoPendingLotsDelta
		existing.CollateralLockedDelta += delta.CollateralLockedDelta
	}

	for _, sourceOrder := range sourceOrdersForBatch(batch) {
		meta := metaFromSourceOrder(&sourceOrder)
		add(initialLockDelta(batch.MarketID, meta, sourceOrder.InitialQty, sourceOrder.InitialSpendAmount))
	}

	for _, trade := range batch.TradeEvents {
		makerMeta := metaByOrder[trade.MakerOrderID]
		takerMeta := metaByOrder[trade.TakerOrderID]
		if makerMeta != nil {
			add(settlementDelta(batch.MarketID, *makerMeta, trade.MatchQty, trade.MatchPrice))
		}
		if takerMeta != nil {
			add(settlementDelta(batch.MarketID, *takerMeta, trade.MatchQty, trade.MatchPrice))
		}
	}

	for _, state := range batch.StateEvents {
		if state.Status != matching.StatusCanceled && state.Status != matching.StatusExpired && state.Status != matching.StatusRejected {
			continue
		}
		meta := metaByOrder[state.OrderID]
		if meta == nil {
			continue
		}
		add(unlockDelta(batch.MarketID, *meta, state.RemainingQty, state.RefundAmount))
	}

	result := make([]positionDelta, 0, len(acc))
	for _, delta := range acc {
		result = append(result, *delta)
	}
	return result, nil
}

func (w *Writer) loadOrderMeta(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) (map[uint64]*orderMeta, error) {
	metaByOrder := map[uint64]*orderMeta{}
	if batch.SourceOrder != nil {
		meta := metaFromSourceOrder(batch.SourceOrder)
		metaByOrder[meta.OrderID] = &meta
	}
	orderIDs := make([]uint64, 0)
	addID := func(id uint64) {
		if _, exists := metaByOrder[id]; exists {
			return
		}
		for _, existing := range orderIDs {
			if existing == id {
				return
			}
		}
		orderIDs = append(orderIDs, id)
	}
	for _, trade := range batch.TradeEvents {
		addID(trade.MakerOrderID)
		addID(trade.TakerOrderID)
	}
	for _, state := range batch.StateEvents {
		addID(state.OrderID)
	}
	if len(orderIDs) == 0 {
		return metaByOrder, nil
	}
	args := make([]any, 0, len(orderIDs))
	placeholders := make([]string, 0, len(orderIDs))
	for idx, orderID := range orderIDs {
		args = append(args, int64(orderID))
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx+1))
	}
	query := fmt.Sprintf(`
		SELECT order_id, wallet_address, original_action, original_outcome, original_price_tick, order_type
		FROM orders
		WHERE order_id IN (%s)
	`, strings.Join(placeholders, ","))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			orderID           int64
			walletAddress     string
			originalAction    string
			originalOutcome   string
			originalPriceTick int16
			orderType         int16
		)
		if err := rows.Scan(&orderID, &walletAddress, &originalAction, &originalOutcome, &originalPriceTick, &orderType); err != nil {
			return nil, err
		}
		metaByOrder[uint64(orderID)] = &orderMeta{
			OrderID:           uint64(orderID),
			WalletAddress:     walletAddress,
			OriginalAction:    originalAction,
			OriginalOutcome:   originalOutcome,
			OriginalPriceTick: uint8(originalPriceTick),
			OrderType:         uint8(orderType),
		}
	}
	return metaByOrder, rows.Err()
}

func (w *Writer) applySinglePositionDelta(ctx context.Context, tx pgx.Tx, delta positionDelta) error {
	if delta.WalletAddress == "" {
		return nil
	}
	marketIDStr := strconv.FormatUint(delta.MarketID, 10)
	_, err := tx.Exec(ctx, `
		INSERT INTO positions (
			market_id, wallet_address,
			yes_free_lots, yes_locked_lots, yes_pending_lots,
			no_free_lots, no_locked_lots, no_pending_lots,
			collateral_locked_units,
			updated_at
		) VALUES ($1::NUMERIC(20,0), $2, 0, 0, 0, 0, 0, 0, 0, NOW())
		ON CONFLICT (market_id, wallet_address) DO NOTHING
	`, marketIDStr, delta.WalletAddress)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE positions
		SET
			yes_free_lots = yes_free_lots + $1,
			yes_locked_lots = yes_locked_lots + $2,
			yes_pending_lots = yes_pending_lots + $3,
			no_free_lots = no_free_lots + $4,
			no_locked_lots = no_locked_lots + $5,
			no_pending_lots = no_pending_lots + $6,
			collateral_locked_units = collateral_locked_units + $7,
			updated_at = NOW()
		WHERE market_id = $8::NUMERIC(20,0) AND wallet_address = $9
	`, delta.YesFreeLotsDelta, delta.YesLockedLotsDelta, delta.YesPendingLotsDelta, delta.NoFreeLotsDelta, delta.NoLockedLotsDelta, delta.NoPendingLotsDelta, delta.CollateralLockedDelta, marketIDStr, delta.WalletAddress)
	return err
}

func (w *Writer) advanceCursor(ctx context.Context, tx pgx.Tx, marketID uint64, evtSeq, sourceCmdSeq int64) error {
	marketIDStr := strconv.FormatUint(marketID, 10)
	_, err := tx.Exec(ctx, `
		UPDATE consumer_cursors
		SET last_evt_seq = $1,
			last_source_cmd_seq = $2,
			updated_at = NOW()
		WHERE consumer_name = $3 AND market_id = $4::NUMERIC(20,0)
	`, evtSeq, sourceCmdSeq, w.consumerName, marketIDStr)
	return err
}

func (w *Writer) rebuildRedisModels(ctx context.Context) error {
	if w.rdb == nil {
		return nil
	}
	if err := w.deleteByPattern(ctx, "l2:depth:*"); err != nil {
		return err
	}
	if err := w.deleteByPattern(ctx, "user:orders:*"); err != nil {
		return err
	}
	if err := w.deleteByPattern(ctx, "order:info:*"); err != nil {
		return err
	}
	if err := w.deleteByPattern(ctx, "position:*"); err != nil {
		return err
	}
	if err := w.deleteByPattern(ctx, "trades:latest:*"); err != nil {
		return err
	}
	if err := w.deleteByPattern(ctx, "price:history:*"); err != nil {
		return err
	}
	if err := w.rebuildDepths(ctx); err != nil {
		return err
	}
	if err := w.rebuildActiveOrders(ctx); err != nil {
		return err
	}
	if err := w.rebuildPositions(ctx); err != nil {
		return err
	}
	if err := w.rebuildRecentTrades(ctx); err != nil {
		return err
	}
	if err := w.rebuildPriceHistory(ctx); err != nil {
		return err
	}
	return nil
}

func (w *Writer) rebuildDepths(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `
		SELECT market_id, side, price_tick, SUM(remaining_qty) AS total_volume
		FROM orders
		WHERE status IN (1, 2)
		GROUP BY market_id, side, price_tick
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var marketIDStr string
		var side int16
		var priceTick int16
		var totalVolume int64
		if err := rows.Scan(&marketIDStr, &side, &priceTick, &totalVolume); err != nil {
			return err
		}
		key := fmt.Sprintf("l2:depth:%s", marketIDStr)
		field := depthField(uint8(side), uint8(priceTick))
		pipe.HSet(ctx, key, field, totalVolume)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (w *Writer) rebuildActiveOrders(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `
		SELECT order_id, market_id, wallet_address, original_action, original_outcome, original_price_tick, side, order_type, price_tick,
		       initial_qty, initial_spend_amount, remaining_qty, remaining_spend_amount, expire_time,
		       status, created_cmd_seq, EXTRACT(EPOCH FROM updated_at)::BIGINT
		FROM orders
		WHERE status IN (1, 2)
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var (
			orderID            int64
			marketIDStr        string
			walletAddress      string
			originalAction     string
			originalOutcome    string
			originalPriceTick  int16
			side               int16
			orderType          int16
			priceTick          int16
			initialQty         int64
			initialSpendAmount int64
			remainingQty       int64
			remainingSpend     int64
			expireTime         int64
			status             int16
			createdCmdSeq      int64
			updatedAtUnix      int64
		)
		if err := rows.Scan(
			&orderID, &marketIDStr, &walletAddress, &originalAction, &originalOutcome, &originalPriceTick, &side, &orderType, &priceTick,
			&initialQty, &initialSpendAmount, &remainingQty, &remainingSpend, &expireTime,
			&status, &createdCmdSeq, &updatedAtUnix,
		); err != nil {
			return err
		}
		w.writeOpenOrderToRedis(ctx, pipe, openOrderProjection{
			OrderID:              orderID,
			MarketID:             marketIDStr,
			WalletAddress:        walletAddress,
			OriginalAction:       originalAction,
			OriginalOutcome:      originalOutcome,
			OriginalPriceTick:    int(originalPriceTick),
			Side:                 int(side),
			OrderType:            int(orderType),
			PriceTick:            int(priceTick),
			InitialQty:           initialQty,
			InitialSpendAmount:   initialSpendAmount,
			RemainingQty:         remainingQty,
			RemainingSpendAmount: remainingSpend,
			ExpireTime:           expireTime,
			Status:               int(status),
			CreatedCmdSeq:        createdCmdSeq,
			UpdatedAtUnix:        updatedAtUnix,
		})
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (w *Writer) rebuildRecentTrades(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `
		SELECT trade_id, market_id, match_price, match_qty, executed_at
		FROM (
			SELECT trade_id, market_id, match_price, match_qty, executed_at,
			       ROW_NUMBER() OVER (PARTITION BY market_id ORDER BY executed_at DESC) AS rn
			FROM trades
		) t
		WHERE rn <= $1
		ORDER BY market_id ASC, executed_at ASC
	`, recentTradesLimit)
	if err != nil {
		return err
	}
	defer rows.Close()

	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var (
			tradeID     string
			marketIDStr string
			price       int16
			qty         int64
			executedAt  time.Time
		)
		if err := rows.Scan(&tradeID, &marketIDStr, &price, &qty, &executedAt); err != nil {
			return err
		}
		entry, err := json.Marshal(matching.Trade{
			ID:         tradeID,
			Price:      strconv.FormatInt(int64(price), 10),
			Quantity:   strconv.FormatInt(qty, 10),
			ExecutedAt: executedAt.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return err
		}
		key := fmt.Sprintf("trades:latest:%s", marketIDStr)
		pipe.RPush(ctx, key, entry)
		pipe.LTrim(ctx, key, -recentTradesLimit, -1)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (w *Writer) rebuildPositions(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `
		SELECT market_id, wallet_address,
		       yes_free_lots, yes_locked_lots, yes_pending_lots,
		       no_free_lots, no_locked_lots, no_pending_lots,
		       collateral_locked_units,
		       EXTRACT(EPOCH FROM updated_at)::BIGINT
		FROM positions
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var (
			marketIDStr           string
			walletAddress         string
			yesFreeLots           int64
			yesLockedLots         int64
			yesPendingLots        int64
			noFreeLots            int64
			noLockedLots          int64
			noPendingLots         int64
			collateralLockedUnits int64
			updatedAtUnix         int64
		)
		if err := rows.Scan(
			&marketIDStr, &walletAddress,
			&yesFreeLots, &yesLockedLots, &yesPendingLots,
			&noFreeLots, &noLockedLots, &noPendingLots,
			&collateralLockedUnits,
			&updatedAtUnix,
		); err != nil {
			return err
		}
		key := fmt.Sprintf("position:%s:%s", marketIDStr, walletAddress)
		pipe.HSet(ctx, key, map[string]any{
			"yes_free_lots":           yesFreeLots,
			"yes_locked_lots":         yesLockedLots,
			"yes_pending_lots":        yesPendingLots,
			"no_free_lots":            noFreeLots,
			"no_locked_lots":          noLockedLots,
			"no_pending_lots":         noPendingLots,
			"collateral_locked_units": collateralLockedUnits,
			"updated_at":              updatedAtUnix,
		})
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (w *Writer) rebuildPriceHistory(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `
		SELECT market_id, trade_id, match_price, match_qty, executed_at
		FROM trades
		WHERE executed_at >= $1
		ORDER BY market_id ASC, executed_at ASC
	`, time.Now().UTC().Add(-priceHistoryHotWindow))
	if err != nil {
		return err
	}
	defer rows.Close()

	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var (
			marketIDStr string
			tradeID     string
			price       int16
			qty         int64
			executedAt  time.Time
		)
		if err := rows.Scan(&marketIDStr, &tradeID, &price, &qty, &executedAt); err != nil {
			return err
		}
		key := fmt.Sprintf("price:history:%s", marketIDStr)
		member, err := json.Marshal(matching.PricePoint{
			Timestamp: executedAt.UTC().Format(time.RFC3339),
			Price:     strconv.FormatInt(int64(price), 10),
			Quantity:  strconv.FormatInt(qty, 10),
		})
		if err != nil {
			return err
		}
		pipe.ZAdd(ctx, key, redis.Z{
			Score:  float64(executedAt.UTC().UnixMilli()),
			Member: member,
		})
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (w *Writer) updateRedisReadModels(ctx context.Context, batch *matching.BatchEventPayload) (pushMessages, error) {
	pushes := buildPushMessages(batch)
	if w.rdb == nil {
		return pushes, nil
	}
	pipe := w.rdb.Pipeline()

	depthKey := fmt.Sprintf("l2:depth:%d", batch.MarketID)
	for _, depth := range batch.DepthEvents {
		field := depthField(depth.Side, depth.PriceTick)
		if depth.TotalVolume == 0 {
			pipe.HDel(ctx, depthKey, field)
		} else {
			pipe.HSet(ctx, depthKey, field, depth.TotalVolume)
		}
	}

	for _, sourceOrder := range sourceOrdersForBatch(batch) {
		w.writeOpenOrderToRedis(ctx, pipe, openOrderProjection{
			OrderID:              mustInt64(sourceOrder.OrderID),
			MarketID:             strconv.FormatUint(batch.MarketID, 10),
			WalletAddress:        sourceOrder.WalletAddress,
			OriginalAction:       sideLabel(sourceOrder.OriginalAction),
			OriginalOutcome:      outcomeLabel(sourceOrder.OriginalOutcome),
			OriginalPriceTick:    int(sourceOrder.OriginalPriceTick),
			NormalizedSide:       sideLabel(sourceOrder.Side),
			NormalizedPriceTick:  int(sourceOrder.PriceTick),
			Side:                 int(sourceOrder.Side),
			OrderType:            int(sourceOrder.OrderType),
			PriceTick:            int(sourceOrder.PriceTick),
			InitialQty:           mustInt64(sourceOrder.InitialQty),
			InitialSpendAmount:   mustInt64(sourceOrder.InitialSpendAmount),
			RemainingQty:         mustInt64(sourceOrder.InitialQty),
			RemainingSpendAmount: mustInt64(sourceOrder.InitialSpendAmount),
			ExpireTime:           sourceOrder.ExpireTime,
			Status:               int(matching.StatusNew),
			CreatedCmdSeq:        mustInt64(sourceOrder.CreatedCmdSeq),
			UpdatedAtUnix:        batch.Timestamp,
		})
	}

	for _, state := range batch.StateEvents {
		w.applyStateToRedis(ctx, pipe, batch.MarketID, batch.Timestamp, batch, state)
	}

	if len(batch.TradeEvents) > 0 {
		tradesKey := fmt.Sprintf("trades:latest:%d", batch.MarketID)
		priceHistoryKey := fmt.Sprintf("price:history:%d", batch.MarketID)
		executedAt := timestampToTime(batch.Timestamp)
		for _, trade := range batch.TradeEvents {
			entry, err := json.Marshal(matching.Trade{
				ID:         trade.TradeID,
				Price:      strconv.FormatUint(uint64(trade.MatchPrice), 10),
				Quantity:   strconv.FormatUint(trade.MatchQty, 10),
				ExecutedAt: executedAt.Format(time.RFC3339),
			})
			if err == nil {
				pipe.LPush(ctx, tradesKey, entry)
			}
			point, err := json.Marshal(matching.PricePoint{
				Timestamp: executedAt.Format(time.RFC3339),
				Price:     strconv.FormatUint(uint64(trade.MatchPrice), 10),
				Quantity:  strconv.FormatUint(trade.MatchQty, 10),
			})
			if err == nil {
				pipe.ZAdd(ctx, priceHistoryKey, redis.Z{
					Score:  float64(executedAt.UnixMilli()),
					Member: point,
				})
			}
		}
		pipe.LTrim(ctx, tradesKey, 0, recentTradesLimit-1)
		pipe.ZRemRangeByScore(ctx, priceHistoryKey, "-inf", strconv.FormatInt(time.Now().UTC().Add(-priceHistoryHotWindow).UnixMilli(), 10))
		pipe.ZRemRangeByRank(ctx, priceHistoryKey, 0, -priceHistoryMaxPoints-1)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return pushMessages{}, err
	}
	if err := w.syncPositionRedis(ctx, batch); err != nil {
		return pushMessages{}, err
	}
	return pushes, nil
}

type openOrderProjection struct {
	OrderID              int64
	MarketID             string
	WalletAddress        string
	OriginalAction       string
	OriginalOutcome      string
	OriginalPriceTick    int
	NormalizedSide       string
	NormalizedPriceTick  int
	Side                 int
	OrderType            int
	PriceTick            int
	InitialQty           int64
	InitialSpendAmount   int64
	RemainingQty         int64
	RemainingSpendAmount int64
	ExpireTime           int64
	Status               int
	CreatedCmdSeq        int64
	UpdatedAtUnix        int64
}

type positionProjection struct {
	YesFreeLots           int64
	YesLockedLots         int64
	YesPendingLots        int64
	NoFreeLots            int64
	NoLockedLots          int64
	NoPendingLots         int64
	CollateralLockedUnits int64
	UpdatedAtUnix         int64
}

func (w *Writer) writeOpenOrderToRedis(ctx context.Context, pipe redis.Pipeliner, order openOrderProjection) {
	orderIDStr := strconv.FormatInt(order.OrderID, 10)
	userOrdersKey := fmt.Sprintf("user:orders:%s", order.WalletAddress)
	orderInfoKey := fmt.Sprintf("order:info:%s", orderIDStr)

	pipe.ZAdd(ctx, userOrdersKey, redis.Z{
		Score:  float64(order.CreatedCmdSeq),
		Member: orderIDStr,
	})
	pipe.HSet(ctx, orderInfoKey, map[string]any{
		"market_id":              order.MarketID,
		"wallet_address":         order.WalletAddress,
		"original_action":        order.OriginalAction,
		"original_outcome":       order.OriginalOutcome,
		"original_price_tick":    order.OriginalPriceTick,
		"normalized_side":        order.NormalizedSide,
		"normalized_price_tick":  order.NormalizedPriceTick,
		"side":                   order.Side,
		"order_type":             order.OrderType,
		"price_tick":             order.PriceTick,
		"initial_qty":            order.InitialQty,
		"initial_qty_lots":       order.InitialQty,
		"initial_spend_amount":   order.InitialSpendAmount,
		"remaining_qty":          order.RemainingQty,
		"remaining_qty_lots":     order.RemainingQty,
		"remaining_spend_amount": order.RemainingSpendAmount,
		"expire_time":            order.ExpireTime,
		"status":                 order.Status,
		"status_text":            orderStatusLabel(uint8(order.Status)),
		"created_cmd_seq":        order.CreatedCmdSeq,
		"updated_at":             order.UpdatedAtUnix,
	})
	pipe.Persist(ctx, orderInfoKey)
}

func (w *Writer) applyStateToRedis(ctx context.Context, pipe redis.Pipeliner, marketID uint64, timestamp int64, batch *matching.BatchEventPayload, state matching.OrderStateEvent) {
	orderIDStr := strconv.FormatUint(state.OrderID, 10)
	orderInfoKey := fmt.Sprintf("order:info:%s", orderIDStr)
	walletAddress := ""
	createdCmdSeq := float64(0)
	for _, sourceOrder := range sourceOrdersForBatch(batch) {
		if sourceOrder.OrderID == state.OrderID {
			walletAddress = sourceOrder.WalletAddress
			createdCmdSeq = float64(sourceOrder.CreatedCmdSeq)
			break
		}
	}
	if walletAddress == "" {
		info, err := w.rdb.HGetAll(ctx, orderInfoKey).Result()
		if err != nil || len(info) == 0 {
			return
		}
		walletAddress = info["wallet_address"]
		createdCmdSeq, _ = strconv.ParseFloat(info["created_cmd_seq"], 64)
	}
	if walletAddress == "" {
		return
	}
	userOrdersKey := fmt.Sprintf("user:orders:%s", walletAddress)
	pipe.HSet(ctx, orderInfoKey, map[string]any{
		"market_id":              marketID,
		"remaining_qty":          state.RemainingQty,
		"remaining_qty_lots":     state.RemainingQty,
		"remaining_spend_amount": state.RemainingSpendAmount,
		"status":                 state.Status,
		"status_text":            orderStatusLabel(state.Status),
		"updated_at":             timestamp,
	})
	if state.Status == matching.StatusNew || state.Status == matching.StatusPartiallyFilled {
		pipe.ZAdd(ctx, userOrdersKey, redis.Z{Score: createdCmdSeq, Member: orderIDStr})
		pipe.Persist(ctx, orderInfoKey)
		return
	}
	pipe.ZRem(ctx, userOrdersKey, orderIDStr)
	pipe.Expire(ctx, orderInfoKey, time.Hour)
}

func (w *Writer) syncPositionRedis(ctx context.Context, batch *matching.BatchEventPayload) error {
	if w.rdb == nil || w.pool == nil {
		return nil
	}
	marketIDStr := strconv.FormatUint(batch.MarketID, 10)
	rows, err := w.pool.Query(ctx, `
		SELECT market_id, wallet_address,
		       yes_free_lots, yes_locked_lots, yes_pending_lots,
		       no_free_lots, no_locked_lots, no_pending_lots,
		       collateral_locked_units,
		       EXTRACT(EPOCH FROM updated_at)::BIGINT
		FROM positions
		WHERE market_id = $1::NUMERIC(20,0)
	`, marketIDStr)
	if err != nil {
		return err
	}
	defer rows.Close()
	pipe := w.rdb.Pipeline()
	for rows.Next() {
		var (
			marketIDStr           string
			walletAddress         string
			yesFreeLots           int64
			yesLockedLots         int64
			yesPendingLots        int64
			noFreeLots            int64
			noLockedLots          int64
			noPendingLots         int64
			collateralLockedUnits int64
			updatedAtUnix         int64
		)
		if err := rows.Scan(
			&marketIDStr, &walletAddress,
			&yesFreeLots, &yesLockedLots, &yesPendingLots,
			&noFreeLots, &noLockedLots, &noPendingLots,
			&collateralLockedUnits,
			&updatedAtUnix,
		); err != nil {
			return err
		}
		writePositionToRedis(ctx, pipe, marketIDStr, walletAddress, positionProjection{
			YesFreeLots:           yesFreeLots,
			YesLockedLots:         yesLockedLots,
			YesPendingLots:        yesPendingLots,
			NoFreeLots:            noFreeLots,
			NoLockedLots:          noLockedLots,
			NoPendingLots:         noPendingLots,
			CollateralLockedUnits: collateralLockedUnits,
			UpdatedAtUnix:         updatedAtUnix,
		})
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	_, err = pipe.Exec(ctx)
	return err
}

func writePositionToRedis(ctx context.Context, pipe redis.Pipeliner, marketID string, walletAddress string, position positionProjection) {
	key := fmt.Sprintf("position:%s:%s", marketID, walletAddress)
	pipe.HSet(ctx, key, map[string]any{
		"yes_free_lots":           position.YesFreeLots,
		"yes_locked_lots":         position.YesLockedLots,
		"yes_pending_lots":        position.YesPendingLots,
		"no_free_lots":            position.NoFreeLots,
		"no_locked_lots":          position.NoLockedLots,
		"no_pending_lots":         position.NoPendingLots,
		"collateral_locked_units": position.CollateralLockedUnits,
		"updated_at":              position.UpdatedAtUnix,
	})
}

func (w *Writer) deleteByPattern(ctx context.Context, pattern string) error {
	var cursor uint64
	for {
		keys, nextCursor, err := w.rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := w.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			return nil
		}
	}
}

func depthField(side uint8, priceTick uint8) string {
	prefix := "bid"
	if side == matching.SideSell {
		prefix = "ask"
	}
	return fmt.Sprintf("%s:%d", prefix, priceTick)
}

func buildPushMessages(batch *matching.BatchEventPayload) pushMessages {
	updatedAt := timestampToTime(batch.Timestamp)
	pushes := pushMessages{}
	if len(batch.DepthEvents) > 0 {
		pushes.marketDepths = append(pushes.marketDepths, buildMarketDepthPush(
			batch.MarketID,
			batch.SourceCmdSeq,
			updatedAt,
			compressDepthEvents(batch.DepthEvents),
		))
	}
	for _, trade := range batch.TradeEvents {
		pushes.marketTrades = append(pushes.marketTrades, buildMarketTradePush(batch.MarketID, updatedAt, trade))
	}
	for _, state := range batch.StateEvents {
		if state.WalletAddress == "" {
			continue
		}
		pushes.userOrders = append(pushes.userOrders, buildUserOrderPush(batch.MarketID, updatedAt, batch.SourceOrder, state))
	}
	return pushes
}

func compressDepthEvents(events []matching.L2DepthEvent) []matching.L2DepthEvent {
	type key struct {
		side      uint8
		priceTick uint8
	}
	latest := make(map[key]matching.L2DepthEvent, len(events))
	order := make([]key, 0, len(events))
	for _, event := range events {
		k := key{side: event.Side, priceTick: event.PriceTick}
		if _, exists := latest[k]; !exists {
			order = append(order, k)
		}
		latest[k] = event
	}
	result := make([]matching.L2DepthEvent, 0, len(latest))
	for _, k := range order {
		result = append(result, latest[k])
	}
	return result
}

func (w *Writer) publishPushMessages(pushes pushMessages) error {
	if w.client == nil {
		return nil
	}
	for _, payload := range pushes.marketDepths {
		marketID, err := strconv.ParseUint(payload.MarketID, 10, 64)
		if err != nil {
			return err
		}
		if err := w.client.PublishCoreJSON(protocol.SubjectPushMarketDepth(marketID), payload); err != nil {
			return err
		}
	}
	for _, payload := range pushes.marketTrades {
		marketID, err := strconv.ParseUint(payload.MarketID, 10, 64)
		if err != nil {
			return err
		}
		if err := w.client.PublishCoreJSON(protocol.SubjectPushMarketTrade(marketID), payload); err != nil {
			return err
		}
	}
	for _, payload := range pushes.userOrders {
		if err := w.client.PublishCoreJSON(protocol.SubjectPushUserOrder(payload.WalletAddress), payload); err != nil {
			return err
		}
	}
	return nil
}

func buildMarketDepthPush(marketID uint64, sourceCmdSeq uint64, updatedAt time.Time, events []matching.L2DepthEvent) protocol.MarketDepthPush {
	levels := make([]protocol.MarketDepthLevel, 0, len(events))
	for _, event := range events {
		levels = append(levels, protocol.MarketDepthLevel{
			Side:        event.Side,
			PriceTick:   event.PriceTick,
			TotalVolume: event.TotalVolume,
		})
	}
	return protocol.MarketDepthPush{
		MarketID:     strconv.FormatUint(marketID, 10),
		UpdatedAt:    updatedAt.UTC().Format(time.RFC3339),
		SourceCmdSeq: strconv.FormatUint(sourceCmdSeq, 10),
		Levels:       levels,
	}
}

func buildMarketTradePush(marketID uint64, updatedAt time.Time, trade matching.TradeEvent) protocol.MarketTradePush {
	return protocol.MarketTradePush{
		MarketID:           strconv.FormatUint(marketID, 10),
		TradeID:            trade.TradeID,
		MakerOrderID:       strconv.FormatUint(trade.MakerOrderID, 10),
		TakerOrderID:       strconv.FormatUint(trade.TakerOrderID, 10),
		MakerWalletAddress: trade.MakerPubKey,
		TakerWalletAddress: trade.TakerPubKey,
		PriceTick:          strconv.FormatUint(uint64(trade.MatchPrice), 10),
		MatchQty:           strconv.FormatUint(trade.MatchQty, 10),
		ExecutedAt:         updatedAt.UTC().Format(time.RFC3339),
	}
}

func buildUserOrderPush(marketID uint64, updatedAt time.Time, sourceOrder *matching.FullOrderData, state matching.OrderStateEvent) protocol.UserOrderPush {
	push := protocol.UserOrderPush{
		MarketID:      strconv.FormatUint(marketID, 10),
		WalletAddress: state.WalletAddress,
		Order: protocol.UserOrderPatch{
			ID:           strconv.FormatUint(state.OrderID, 10),
			Quantity:     strconv.FormatUint(state.RemainingQty, 10),
			Status:       state.Status,
			RefundAmount: strconv.FormatUint(state.RefundAmount, 10),
			UpdatedAt:    updatedAt.UTC().Format(time.RFC3339),
		},
	}
	if sourceOrder != nil && sourceOrder.OrderID == state.OrderID {
		push.Order.Side = sideLabel(sourceOrder.OriginalAction)
		push.Order.Outcome = outcomeLabel(sourceOrder.OriginalOutcome)
		push.Order.Price = strconv.FormatUint(uint64(sourceOrder.OriginalPriceTick), 10)
	}
	return push
}

func sideLabel(side uint8) string {
	if side == matching.SideSell {
		return "sell"
	}
	return "buy"
}

func outcomeLabel(outcome uint8) string {
	if outcome == 1 {
		return "no"
	}
	return "yes"
}

func metaFromSourceOrder(order *matching.FullOrderData) orderMeta {
	return orderMeta{
		OrderID:           order.OrderID,
		WalletAddress:     order.WalletAddress,
		OriginalAction:    sideLabel(order.OriginalAction),
		OriginalOutcome:   outcomeLabel(order.OriginalOutcome),
		OriginalPriceTick: order.OriginalPriceTick,
		OrderType:         order.OrderType,
	}
}

func sourceOrdersForBatch(batch *matching.BatchEventPayload) []matching.FullOrderData {
	if batch == nil {
		return nil
	}
	if len(batch.SourceOrders) > 0 {
		return batch.SourceOrders
	}
	if batch.SourceOrder == nil {
		return nil
	}
	return []matching.FullOrderData{*batch.SourceOrder}
}

func legacyBatchFromV2(event matching.MatchBatchEventV2) matching.BatchEventPayload {
	batch := matching.BatchEventPayload{
		MarketID:     event.MarketID,
		SourceCmdSeq: event.SourceCmdSeqMax,
		Timestamp:    event.ProducedAt,
		TradeEvents:  make([]matching.TradeEvent, 0, len(event.Fills)),
		StateEvents:  make([]matching.OrderStateEvent, 0, len(event.OrderUpdates)),
		DepthEvents:  make([]matching.L2DepthEvent, 0, len(event.DepthUpdates)),
		SourceOrders: make([]matching.FullOrderData, 0, len(event.Orders)),
	}

	orderByIndex := make(map[uint16]matching.MatchedOrderV2, len(event.Orders))
	createdCmdSeq := event.SourceCmdSeqMin
	if createdCmdSeq == 0 {
		createdCmdSeq = event.SourceCmdSeqMax
	}

	for _, order := range event.Orders {
		orderByIndex[order.OrderIndex] = order
		sourceOrder := matching.FullOrderData{
			OrderID:            order.OrderID,
			WalletAddress:      order.Execution.WalletAddress,
			OriginalAction:     sideCode(order.Execution.OriginalAction),
			OriginalOutcome:    outcomeCode(order.Execution.OriginalOutcome),
			OriginalPriceTick:  order.Execution.OriginalPriceTick,
			Side:               sideCode(order.Execution.NormalizedSide),
			OrderType:          orderTypeCode(order.Execution.OrderType),
			PriceTick:          order.Execution.NormalizedPriceTick,
			InitialQty:         order.Execution.QtyLots,
			InitialSpendAmount: order.Execution.SpendAmount,
			ExpireTime:         order.Execution.ExpireTime,
			Signature:          order.Settlement.Signature,
			IntentBytesHex:     order.Settlement.IntentBytesHex,
			Nonce:              order.Execution.Nonce,
			CreatedCmdSeq:      createdCmdSeq,
		}
		batch.SourceOrders = append(batch.SourceOrders, sourceOrder)
	}
	if len(batch.SourceOrders) > 0 {
		batch.SourceOrder = &batch.SourceOrders[0]
	}

	for _, fill := range event.Fills {
		maker, makerOK := orderByIndex[fill.MakerOrderIndex]
		taker, takerOK := orderByIndex[fill.TakerOrderIndex]
		if !makerOK || !takerOK {
			continue
		}
		batch.TradeEvents = append(batch.TradeEvents, matching.TradeEvent{
			TradeID:        event.EventID + "-" + strconv.FormatUint(uint64(fill.FillIndex), 10),
			MatchPrice:     uint8(fill.FillPrice),
			MatchQty:       fill.FillAmount,
			MakerOrderID:   maker.OrderID,
			MakerPubKey:    maker.Execution.WalletAddress,
			MakerSignature: maker.Settlement.Signature,
			MakerIntentHex: maker.Settlement.IntentBytesHex,
			TakerOrderID:   taker.OrderID,
			TakerPubKey:    taker.Execution.WalletAddress,
			TakerSignature: taker.Settlement.Signature,
			TakerIntentHex: taker.Settlement.IntentBytesHex,
		})
	}

	for _, update := range event.OrderUpdates {
		order, ok := orderByIndex[update.OrderIndex]
		if !ok {
			continue
		}
		batch.StateEvents = append(batch.StateEvents, matching.OrderStateEvent{
			OrderID:              order.OrderID,
			WalletAddress:        order.Execution.WalletAddress,
			Status:               orderStatusCode(update.Status),
			RemainingQty:         update.RemainingQtyLots,
			RemainingSpendAmount: update.RemainingSpendAmount,
			RefundAmount:         update.RefundAmount,
		})
	}

	for _, depth := range event.DepthUpdates {
		batch.DepthEvents = append(batch.DepthEvents, matching.L2DepthEvent{
			Side:        depthSideCode(depth.Side),
			PriceTick:   depth.PriceTick,
			TotalVolume: depth.TotalVolume,
		})
	}

	return batch
}

func sideCode(label string) uint8 {
	if strings.EqualFold(label, "sell") || strings.EqualFold(label, "ask") {
		return matching.SideSell
	}
	return matching.SideBuy
}

func depthSideCode(label string) uint8 {
	if strings.EqualFold(label, "ask") {
		return matching.SideSell
	}
	return matching.SideBuy
}

func outcomeCode(label string) uint8 {
	if strings.EqualFold(label, "no") {
		return 1
	}
	return 0
}

func orderTypeCode(label string) uint8 {
	if strings.EqualFold(label, "market") {
		return matching.OrderTypeMarket
	}
	return matching.OrderTypeLimit
}

func orderStatusCode(label string) uint8 {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "partially_filled":
		return matching.StatusPartiallyFilled
	case "filled":
		return matching.StatusFilled
	case "canceled":
		return matching.StatusCanceled
	case "expired":
		return matching.StatusExpired
	case "rejected":
		return matching.StatusRejected
	default:
		return matching.StatusNew
	}
}

func orderStatusLabel(status uint8) string {
	switch status {
	case matching.StatusPartiallyFilled:
		return "partially_filled"
	case matching.StatusFilled:
		return "filled"
	case matching.StatusCanceled:
		return "canceled"
	case matching.StatusExpired:
		return "expired"
	case matching.StatusRejected:
		return "rejected"
	default:
		return "open"
	}
}

func initialLockDelta(marketID uint64, meta orderMeta, initialQty uint64, initialSpend uint64) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		required := int64(initialSpend)
		if meta.OrderType == matching.OrderTypeLimit {
			required = int64(reservedUnitsForLots(initialQty, meta.OriginalPriceTick))
		}
		delta.CollateralLockedDelta += required
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesFreeLotsDelta -= int64(initialQty)
			delta.YesLockedLotsDelta += int64(initialQty)
		} else {
			delta.NoFreeLotsDelta -= int64(initialQty)
			delta.NoLockedLotsDelta += int64(initialQty)
		}
	}
	return delta
}

func settlementDelta(marketID uint64, meta orderMeta, qty uint64, normalizedMatchPrice uint8) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		if meta.OriginalOutcome == "yes" {
			delta.YesPendingLotsDelta += int64(qty)
		} else {
			delta.NoPendingLotsDelta += int64(qty)
		}
		if meta.OrderType == matching.OrderTypeLimit {
			reserved := int64(reservedUnitsForLots(qty, meta.OriginalPriceTick))
			delta.CollateralLockedDelta -= reserved
		} else {
			actualUnits := int64(actualCollateralForTrade(meta, qty, normalizedMatchPrice))
			delta.CollateralLockedDelta -= actualUnits
		}
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesLockedLotsDelta -= int64(qty)
		} else {
			delta.NoLockedLotsDelta -= int64(qty)
		}
	}
	return delta
}

func unlockDelta(marketID uint64, meta orderMeta, remainingQty uint64, refundAmount uint64) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		unlock := refundAmount
		if meta.OrderType == matching.OrderTypeLimit {
			unlock = reservedUnitsForLots(remainingQty, meta.OriginalPriceTick)
		}
		delta.CollateralLockedDelta -= int64(unlock)
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesLockedLotsDelta -= int64(remainingQty)
			delta.YesFreeLotsDelta += int64(remainingQty)
		} else {
			delta.NoLockedLotsDelta -= int64(remainingQty)
			delta.NoFreeLotsDelta += int64(remainingQty)
		}
	}
	return delta
}

func actualCollateralForTrade(meta orderMeta, qty uint64, normalizedMatchPrice uint8) uint64 {
	if meta.OriginalOutcome == "no" {
		noPrice := uint8(100 - normalizedMatchPrice)
		return reservedUnitsForLots(qty, noPrice)
	}
	return reservedUnitsForLots(qty, normalizedMatchPrice)
}

func reservedUnitsForLots(qtyLots uint64, priceTick uint8) uint64 {
	if qtyLots == 0 || priceTick == 0 {
		return 0
	}
	return (qtyLots*uint64(priceTick) + 99) / 100
}

func timestampToTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(ts, 0).UTC()
}

func toInt64(v uint64) (int64, error) {
	return int64(v), nil
}

func mustInt64(v uint64) int64 {
	return int64(v)
}

// handleOrderLocks 处理订单锁定状态变化
// 根据订单状态（Filled, Canceled, Expired, PartiallyFilled）处理锁定余额的释放或消费
func (w *Writer) handleOrderLocks(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	if len(batch.StateEvents) == 0 {
		return nil
	}

	for _, state := range batch.StateEvents {
		if err := w.processOrderLockByState(ctx, tx, state, batch.MarketID); err != nil {
			return fmt.Errorf("process order lock failed order_id=%d: %w", state.OrderID, err)
		}
	}

	return nil
}

// processOrderLockByState 根据订单状态处理锁定
func (w *Writer) processOrderLockByState(ctx context.Context, tx pgx.Tx, state matching.OrderStateEvent, marketID uint64) error {
	// 只处理需要余额操作的订单状态
	switch state.Status {
	case matching.StatusFilled:
		// 完全成交：消费锁定余额
		return w.consumeLock(ctx, state.OrderID, state.WalletAddress, marketID)
	case matching.StatusPartiallyFilled:
		// 部分成交：部分消费锁定余额
		return w.partialConsumeLock(ctx, tx, state)
	case matching.StatusCanceled, matching.StatusExpired, matching.StatusRejected:
		// 取消/过期/拒绝：释放锁定余额
		return w.releaseLock(ctx, state.OrderID, state.WalletAddress, marketID)
	default:
		// StatusNew 不需要特殊处理
		return nil
	}
}

// consumeLock 消费锁定余额（订单完全成交）
// 将 locked_units 转换为实际成交的 collateral，差额退回到 free_units
func (w *Writer) consumeLock(ctx context.Context, orderID uint64, walletAddress string, marketID uint64) error {
	if w.rdb == nil {
		return nil // Redis 未配置，跳过
	}

	// 从数据库获取锁定记录
	lockData, err := w.loadOrderLockFromDB(ctx, orderID)
	if err != nil {
		// 锁定记录不存在，可能不是限价买入订单，跳过
		return nil
	}

	if lockData.Status != "pending" && lockData.Status != "active" {
		// 已经处理过，跳过
		return nil
	}

	// 获取订单信息以计算实际成交金额
	orderInfo, err := w.loadOrderForLock(ctx, orderID)
	if err != nil {
		return fmt.Errorf("load order info failed: %w", err)
	}

	// 计算实际成交金额
	actualAmount := w.calculateActualCollateral(orderInfo, lockData.LockedAmount)

	// 更新 Redis：locked_units -> 0, free_units += refund
	refundAmount := lockData.LockedAmount - actualAmount
	if refundAmount > 0 {
		if _, _, err := w.releaseBalanceAtomic(ctx, walletAddress, refundAmount); err != nil {
			logger.Warnf("release balance failed order=%d: %v", orderID, err)
		}
	}

	// 更新锁定状态为 consumed
	if err := w.updateOrderLockStatus(ctx, orderID, "consumed"); err != nil {
		logger.Warnf("update lock status failed order=%d: %v", orderID, err)
	}

	return nil
}

// partialConsumeLock 部分消费锁定余额（订单部分成交）
func (w *Writer) partialConsumeLock(ctx context.Context, tx pgx.Tx, state matching.OrderStateEvent) error {
	if w.rdb == nil {
		return nil
	}

	// 从数据库获取锁定记录
	lockData, err := w.loadOrderLockFromDB(ctx, state.OrderID)
	if err != nil {
		return nil // 不是限价买入订单
	}

	if lockData.Status != "pending" && lockData.Status != "active" {
		return nil // 已经处理过
	}

	// 获取订单信息
	orderInfo, err := w.loadOrderForLock(ctx, state.OrderID)
	if err != nil {
		return fmt.Errorf("load order info failed: %w", err)
	}

	// 计算已成交数量
	filledQty := orderInfo.InitialQty - state.RemainingQty
	if filledQty == 0 {
		return nil // 没有成交
	}

	// 计算已消费金额
	consumedAmount := w.calculateConsumedAmount(orderInfo, filledQty, lockData.LockedAmount)

	// 更新锁定：减少 locked_units，增加 free_units（退回部分）
	refundAmount := lockData.LockedAmount - consumedAmount - int64(reservedUnitsForLots(state.RemainingQty, orderInfo.OriginalPriceTick))
	if refundAmount > 0 {
		if _, _, err := w.releaseBalanceAtomic(ctx, state.WalletAddress, refundAmount); err != nil {
			logger.Warnf("release balance failed order=%d: %v", state.OrderID, err)
		}
	}

	// 更新锁定状态
	if err := w.updateOrderLockStatus(ctx, state.OrderID, "active"); err != nil {
		logger.Warnf("update lock status failed order=%d: %v", state.OrderID, err)
	}

	return nil
}

// releaseLock 释放锁定余额（订单取消/过期/拒绝）
func (w *Writer) releaseLock(ctx context.Context, orderID uint64, walletAddress string, marketID uint64) error {
	if w.rdb == nil {
		return nil
	}

	// 从数据库获取锁定记录
	lockData, err := w.loadOrderLockFromDB(ctx, orderID)
	if err != nil {
		return nil // 不是限价买入订单
	}

	if lockData.Status != "pending" && lockData.Status != "active" {
		return nil // 已经处理过
	}

	// 释放全部锁定金额
	if _, _, err := w.releaseBalanceAtomic(ctx, walletAddress, lockData.LockedAmount); err != nil {
		logger.Warnf("release balance failed order=%d: %v", orderID, err)
		return err
	}

	// 更新锁定状态为 released
	if err := w.updateOrderLockStatus(ctx, orderID, "released"); err != nil {
		logger.Warnf("update lock status failed order=%d: %v", orderID, err)
	}

	return nil
}

// releaseBalanceAtomic 释放余额到 Redis
func (w *Writer) releaseBalanceAtomic(ctx context.Context, walletAddress string, releaseAmount int64) (bool, int64, error) {
	if w.rdb == nil {
		return false, 0, errors.New("redis client not configured")
	}

	cacheKey := fmt.Sprintf("wallet-account:%s", walletAddress)
	updatedAt := strconv.FormatInt(time.Now().UTC().Unix(), 10)

	pipe := w.rdb.Pipeline()
	pipe.HIncrBy(ctx, cacheKey, "collateral_locked_units", -releaseAmount)
	pipe.HIncrBy(ctx, cacheKey, "collateral_free_units", releaseAmount)
	pipe.HSet(ctx, cacheKey, "updated_at", updatedAt)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("redis pipeline exec failed: %w", err)
	}

	freeUnits, err := w.rdb.HGet(ctx, cacheKey, "collateral_free_units").Int64()
	if err != nil {
		return true, 0, nil
	}

	return true, freeUnits, nil
}

// loadOrderLockFromDB 从数据库加载订单锁定记录
type orderLockRecord struct {
	OrderID       uint64
	WalletAddress string
	MarketID      uint64
	LockedAmount  int64
	Status        string
	CreatedAt     time.Time
}

func (w *Writer) loadOrderLockFromDB(ctx context.Context, orderID uint64) (*orderLockRecord, error) {
	if w.pool == nil {
		return nil, errors.New("database pool not configured")
	}

	row := w.pool.QueryRow(ctx, `
		SELECT order_id, wallet_address, market_id, locked_amount, status, created_at
		FROM order_locks
		WHERE order_id = $1
	`, int64(orderID))

	var record orderLockRecord
	err := row.Scan(&record.OrderID, &record.WalletAddress, &record.MarketID, &record.LockedAmount, &record.Status, &record.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &record, nil
}

// loadOrderForLock 加载订单信息用于锁定计算
type orderLockInfo struct {
	OrderID           uint64
	OriginalAction    string
	OriginalOutcome   string
	OriginalPriceTick uint8
	OrderType         uint8
	InitialQty        uint64
	RemainingQty      uint64
}

func (w *Writer) loadOrderForLock(ctx context.Context, orderID uint64) (*orderLockInfo, error) {
	if w.pool == nil {
		return nil, errors.New("database pool not configured")
	}

	row := w.pool.QueryRow(ctx, `
		SELECT order_id, original_action, original_outcome, original_price_tick,
		       order_type, initial_qty, remaining_qty
		FROM orders
		WHERE order_id = $1
	`, int64(orderID))

	var info orderLockInfo
	err := row.Scan(&info.OrderID, &info.OriginalAction, &info.OriginalOutcome,
		&info.OriginalPriceTick, &info.OrderType, &info.InitialQty, &info.RemainingQty)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// calculateActualCollateral 计算实际成交的抵押品金额
func (w *Writer) calculateActualCollateral(order *orderLockInfo, lockedAmount int64) int64 {
	// 对于限价买入单，实际成交金额可能小于锁定金额
	// 这里简化处理，假设实际成交金额等于锁定金额
	// 实际应该根据成交价格和数量计算
	return lockedAmount
}

// calculateConsumedAmount 计算部分成交的消费金额
func (w *Writer) calculateConsumedAmount(order *orderLockInfo, filledQty uint64, lockedAmount int64) int64 {
	// 按比例计算：consumed = locked * (filled / initial)
	if order.InitialQty == 0 {
		return 0
	}

	return int64(uint64(lockedAmount) * filledQty / order.InitialQty)
}

// updateOrderLockStatus 更新订单锁定状态
func (w *Writer) updateOrderLockStatus(ctx context.Context, orderID uint64, status string) error {
	if w.pool == nil {
		return errors.New("database pool not configured")
	}

	_, err := w.pool.Exec(ctx, `
		UPDATE order_locks
		SET status = $1, updated_at = NOW()
		WHERE order_id = $2
	`, status, int64(orderID))

	return err
}
