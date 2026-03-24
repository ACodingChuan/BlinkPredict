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
)

type Writer struct {
	consumerName string
	pool         *pgxpool.Pool
	client       *natsjs.Client
	rdb          *redis.Client
	sub          *nats.Subscription
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
	NoFreeLotsDelta       int64
	NoLockedLotsDelta     int64
	CollateralFreeDelta   int64
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
	sub, err := w.client.PullSubscribe(protocol.SubjectBatchTrades+".*", w.consumerName)
	if err != nil {
		return fmt.Errorf("writer subscribe: %w", err)
	}
	w.sub = sub
	return nil
}

func (w *Writer) catchUp(ctx context.Context) error {
	for {
		msgs, err := w.sub.Fetch(8, nats.MaxWait(500*time.Millisecond))
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
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := w.sub.Fetch(1, nats.MaxWait(1500*time.Millisecond))
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
	meta, err := msg.Metadata()
	if err != nil {
		logger.Warnf("metadata failed: %v", err)
		_ = msg.Nak()
		return
	}

	var batch matching.BatchEventPayload
	if err := json.Unmarshal(msg.Data, &batch); err != nil {
		logger.Warnf("decode failed: %v", err)
		_ = msg.Term()
		return
	}

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
		logger.Warnf("lock cursor failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if evtSeq <= lastEvtSeq {
		_ = msg.Ack()
		return
	}

	if batch.SourceOrder != nil {
		if err := w.upsertSourceOrder(ctx, tx, batch.MarketID, batch.Timestamp, batch.SourceOrder); err != nil {
			logger.Warnf("upsert source order failed market=%d order=%d err=%v", batch.MarketID, batch.SourceOrder.OrderID, err)
			_ = msg.NakWithDelay(time.Second)
			return
		}
	}

	if err := w.persistTrades(ctx, tx, &batch); err != nil {
		logger.Warnf("persist trades failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := w.applyStateEvents(ctx, tx, &batch); err != nil {
		logger.Warnf("apply state events failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := w.applyPositionDeltas(ctx, tx, &batch); err != nil {
		logger.Warnf("apply position deltas failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := w.advanceCursor(ctx, tx, batch.MarketID, evtSeq, sourceCmdSeq); err != nil {
		logger.Warnf("advance cursor failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Warnf("commit failed market=%d err=%v", batch.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}

	pushes, err := w.updateRedisReadModels(ctx, &batch)
	if err != nil {
		logger.Warnf("redis sync failed market=%d err=%v", batch.MarketID, err)
		_ = msg.Ack()
		return
	}
	if err := w.publishPushMessages(pushes); err != nil {
		logger.Warnf("push publish failed market=%d err=%v", batch.MarketID, err)
	}
	_ = msg.Ack()
}

func (w *Writer) lockCursorRow(ctx context.Context, tx pgx.Tx, marketID uint64) (int64, error) {
	marketIDInt, err := toInt64(marketID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO consumer_cursors (consumer_name, market_id, last_evt_seq, last_source_cmd_seq)
		VALUES ($1, $2, 0, 0)
		ON CONFLICT (consumer_name, market_id) DO NOTHING
	`, w.consumerName, marketIDInt); err != nil {
		return 0, err
	}

	var lastEvtSeq int64
	if err := tx.QueryRow(ctx, `
		SELECT last_evt_seq
		FROM consumer_cursors
		WHERE consumer_name = $1 AND market_id = $2
		FOR UPDATE
	`, w.consumerName, marketIDInt).Scan(&lastEvtSeq); err != nil {
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
	marketIDInt, err := toInt64(marketID)
	if err != nil {
		return err
	}
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
			initial_qty, initial_spend_amount, remaining_qty, expire_time,
			status, signature, intent_hex, nonce, created_cmd_seq, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $10, $12,
			$13, $14, $15, $16, $17, $18, $18
		)
		ON CONFLICT (order_id) DO NOTHING
	`,
		orderID,
		marketIDInt,
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

func (w *Writer) persistTrades(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	if len(batch.TradeEvents) == 0 {
		return nil
	}
	marketID, err := toInt64(batch.MarketID)
	if err != nil {
		return err
	}
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
				$1, $2, $3, $4, $5,
				$6, $7,
				$8, $9,
				$10, $11,
				$12, $13, $14
			)
			ON CONFLICT (trade_id) DO NOTHING
		`,
			trade.TradeID,
			marketID,
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
				status = $2,
				updated_at = $3
			WHERE order_id = $4
		`, remainingQty, int16(state.Status), now, orderID)
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
		existing.NoFreeLotsDelta += delta.NoFreeLotsDelta
		existing.NoLockedLotsDelta += delta.NoLockedLotsDelta
		existing.CollateralFreeDelta += delta.CollateralFreeDelta
		existing.CollateralLockedDelta += delta.CollateralLockedDelta
	}

	if batch.SourceOrder != nil {
		meta := metaFromSourceOrder(batch.SourceOrder)
		add(initialLockDelta(batch.MarketID, meta, batch.SourceOrder.InitialQty, batch.SourceOrder.InitialSpendAmount))
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
	marketIDInt, err := toInt64(delta.MarketID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO positions (
			market_id, wallet_address,
			yes_free_lots, yes_locked_lots,
			no_free_lots, no_locked_lots,
			collateral_free_units, collateral_locked_units,
			updated_at
		) VALUES ($1, $2, 0, 0, 0, 0, 0, 0, NOW())
		ON CONFLICT (market_id, wallet_address) DO NOTHING
	`, marketIDInt, delta.WalletAddress)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE positions
		SET
			yes_free_lots = yes_free_lots + $1,
			yes_locked_lots = yes_locked_lots + $2,
			no_free_lots = no_free_lots + $3,
			no_locked_lots = no_locked_lots + $4,
			collateral_free_units = collateral_free_units + $5,
			collateral_locked_units = collateral_locked_units + $6,
			updated_at = NOW()
		WHERE market_id = $7 AND wallet_address = $8
	`, delta.YesFreeLotsDelta, delta.YesLockedLotsDelta, delta.NoFreeLotsDelta, delta.NoLockedLotsDelta, delta.CollateralFreeDelta, delta.CollateralLockedDelta, marketIDInt, delta.WalletAddress)
	return err
}

func (w *Writer) advanceCursor(ctx context.Context, tx pgx.Tx, marketID uint64, evtSeq, sourceCmdSeq int64) error {
	marketIDInt, err := toInt64(marketID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE consumer_cursors
		SET last_evt_seq = $1,
			last_source_cmd_seq = $2,
			updated_at = NOW()
		WHERE consumer_name = $3 AND market_id = $4
	`, evtSeq, sourceCmdSeq, w.consumerName, marketIDInt)
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
		       initial_qty, initial_spend_amount, remaining_qty, expire_time,
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
			expireTime         int64
			status             int16
			createdCmdSeq      int64
			updatedAtUnix      int64
		)
		if err := rows.Scan(
			&orderID, &marketIDStr, &walletAddress, &originalAction, &originalOutcome, &originalPriceTick, &side, &orderType, &priceTick,
			&initialQty, &initialSpendAmount, &remainingQty, &expireTime,
			&status, &createdCmdSeq, &updatedAtUnix,
		); err != nil {
			return err
		}
		marketID, _ := strconv.ParseUint(marketIDStr, 10, 64)
		w.writeOpenOrderToRedis(ctx, pipe, openOrderProjection{
			OrderID:            orderID,
			MarketID:           int64(marketID),
			WalletAddress:      walletAddress,
			OriginalAction:     originalAction,
			OriginalOutcome:    originalOutcome,
			OriginalPriceTick:  int(originalPriceTick),
			Side:               int(side),
			OrderType:          int(orderType),
			PriceTick:          int(priceTick),
			InitialQty:         initialQty,
			InitialSpendAmount: initialSpendAmount,
			RemainingQty:       remainingQty,
			ExpireTime:         expireTime,
			Status:             int(status),
			CreatedCmdSeq:      createdCmdSeq,
			UpdatedAtUnix:      updatedAtUnix,
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
		       yes_free_lots, yes_locked_lots,
		       no_free_lots, no_locked_lots,
		       collateral_free_units, collateral_locked_units,
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
			noFreeLots            int64
			noLockedLots          int64
			collateralFreeUnits   int64
			collateralLockedUnits int64
			updatedAtUnix         int64
		)
		if err := rows.Scan(
			&marketIDStr, &walletAddress,
			&yesFreeLots, &yesLockedLots,
			&noFreeLots, &noLockedLots,
			&collateralFreeUnits, &collateralLockedUnits,
			&updatedAtUnix,
		); err != nil {
			return err
		}
		key := fmt.Sprintf("position:%s:%s", marketIDStr, walletAddress)
		pipe.HSet(ctx, key, map[string]any{
			"yes_free_lots":           yesFreeLots,
			"yes_locked_lots":         yesLockedLots,
			"no_free_lots":            noFreeLots,
			"no_locked_lots":          noLockedLots,
			"collateral_free_units":   collateralFreeUnits,
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

	if batch.SourceOrder != nil {
		w.writeOpenOrderToRedis(ctx, pipe, openOrderProjection{
			OrderID:            mustInt64(batch.SourceOrder.OrderID),
			MarketID:           mustInt64(batch.MarketID),
			WalletAddress:      batch.SourceOrder.WalletAddress,
			OriginalAction:     sideLabel(batch.SourceOrder.OriginalAction),
			OriginalOutcome:    outcomeLabel(batch.SourceOrder.OriginalOutcome),
			OriginalPriceTick:  int(batch.SourceOrder.OriginalPriceTick),
			Side:               int(batch.SourceOrder.Side),
			OrderType:          int(batch.SourceOrder.OrderType),
			PriceTick:          int(batch.SourceOrder.PriceTick),
			InitialQty:         mustInt64(batch.SourceOrder.InitialQty),
			InitialSpendAmount: mustInt64(batch.SourceOrder.InitialSpendAmount),
			RemainingQty:       mustInt64(batch.SourceOrder.InitialQty),
			ExpireTime:         batch.SourceOrder.ExpireTime,
			Status:             int(matching.StatusNew),
			CreatedCmdSeq:      mustInt64(batch.SourceOrder.CreatedCmdSeq),
			UpdatedAtUnix:      batch.Timestamp,
		})
	}

	for _, state := range batch.StateEvents {
		w.applyStateToRedis(ctx, pipe, batch.MarketID, batch.Timestamp, batch.SourceOrder, state)
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
	OrderID            int64
	MarketID           int64
	WalletAddress      string
	OriginalAction     string
	OriginalOutcome    string
	OriginalPriceTick  int
	Side               int
	OrderType          int
	PriceTick          int
	InitialQty         int64
	InitialSpendAmount int64
	RemainingQty       int64
	ExpireTime         int64
	Status             int
	CreatedCmdSeq      int64
	UpdatedAtUnix      int64
}

type positionProjection struct {
	YesFreeLots           int64
	YesLockedLots         int64
	NoFreeLots            int64
	NoLockedLots          int64
	CollateralFreeUnits   int64
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
		"market_id":            order.MarketID,
		"wallet_address":       order.WalletAddress,
		"original_action":      order.OriginalAction,
		"original_outcome":     order.OriginalOutcome,
		"original_price_tick":  order.OriginalPriceTick,
		"side":                 order.Side,
		"order_type":           order.OrderType,
		"price_tick":           order.PriceTick,
		"initial_qty":          order.InitialQty,
		"initial_spend_amount": order.InitialSpendAmount,
		"remaining_qty":        order.RemainingQty,
		"expire_time":          order.ExpireTime,
		"status":               order.Status,
		"created_cmd_seq":      order.CreatedCmdSeq,
		"updated_at":           order.UpdatedAtUnix,
	})
	pipe.Persist(ctx, orderInfoKey)
}

func (w *Writer) applyStateToRedis(ctx context.Context, pipe redis.Pipeliner, marketID uint64, timestamp int64, sourceOrder *matching.FullOrderData, state matching.OrderStateEvent) {
	orderIDStr := strconv.FormatUint(state.OrderID, 10)
	orderInfoKey := fmt.Sprintf("order:info:%s", orderIDStr)
	walletAddress := ""
	createdCmdSeq := float64(0)
	if sourceOrder != nil && sourceOrder.OrderID == state.OrderID {
		walletAddress = sourceOrder.WalletAddress
		createdCmdSeq = float64(sourceOrder.CreatedCmdSeq)
	} else {
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
		"market_id":     marketID,
		"remaining_qty": state.RemainingQty,
		"status":        state.Status,
		"updated_at":    timestamp,
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
	rows, err := w.pool.Query(ctx, `
		SELECT market_id, wallet_address,
		       yes_free_lots, yes_locked_lots,
		       no_free_lots, no_locked_lots,
		       collateral_free_units, collateral_locked_units,
		       EXTRACT(EPOCH FROM updated_at)::BIGINT
		FROM positions
		WHERE market_id = $1
	`, int64(batch.MarketID))
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
			noFreeLots            int64
			noLockedLots          int64
			collateralFreeUnits   int64
			collateralLockedUnits int64
			updatedAtUnix         int64
		)
		if err := rows.Scan(
			&marketIDStr, &walletAddress,
			&yesFreeLots, &yesLockedLots,
			&noFreeLots, &noLockedLots,
			&collateralFreeUnits, &collateralLockedUnits,
			&updatedAtUnix,
		); err != nil {
			return err
		}
		writePositionToRedis(ctx, pipe, marketIDStr, walletAddress, positionProjection{
			YesFreeLots:           yesFreeLots,
			YesLockedLots:         yesLockedLots,
			NoFreeLots:            noFreeLots,
			NoLockedLots:          noLockedLots,
			CollateralFreeUnits:   collateralFreeUnits,
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
		"no_free_lots":            position.NoFreeLots,
		"no_locked_lots":          position.NoLockedLots,
		"collateral_free_units":   position.CollateralFreeUnits,
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

func initialLockDelta(marketID uint64, meta orderMeta, initialQty uint64, initialSpend uint64) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		required := int64(initialSpend)
		if meta.OrderType == matching.OrderTypeLimit {
			required = int64(reservedUnitsForLots(initialQty, meta.OriginalPriceTick))
		}
		delta.CollateralFreeDelta -= required
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
	actualUnits := int64(actualCollateralForTrade(meta, qty, normalizedMatchPrice))
	switch meta.OriginalAction {
	case "buy":
		if meta.OriginalOutcome == "yes" {
			delta.YesFreeLotsDelta += int64(qty)
		} else {
			delta.NoFreeLotsDelta += int64(qty)
		}
		if meta.OrderType == matching.OrderTypeLimit {
			reserved := int64(reservedUnitsForLots(qty, meta.OriginalPriceTick))
			delta.CollateralLockedDelta -= reserved
			delta.CollateralFreeDelta += reserved - actualUnits
		} else {
			delta.CollateralLockedDelta -= actualUnits
		}
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesLockedLotsDelta -= int64(qty)
		} else {
			delta.NoLockedLotsDelta -= int64(qty)
		}
		delta.CollateralFreeDelta += actualUnits
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
		delta.CollateralFreeDelta += int64(unlock)
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
	return (qtyLots * uint64(priceTick)) / 100
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
