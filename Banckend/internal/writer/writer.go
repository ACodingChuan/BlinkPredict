package writer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

var logger = logging.New("writer")

const defaultConsumerName = "writer-primary"
const recentTradesLimit = 100

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

func New(pool *pgxpool.Pool, client *natsjs.Client, rdb *redis.Client, consumerName string) *Writer {
	if consumerName == "" {
		consumerName = defaultConsumerName
	}
	return &Writer{consumerName: consumerName, pool: pool, client: client, rdb: rdb}
}

func (w *Writer) Start(ctx context.Context) error {
	if w.pool == nil || w.client == nil {
		return nil
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
		msgs, err := w.sub.Fetch(64, nats.MaxWait(500*time.Millisecond))
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
		msgs, err := w.sub.Fetch(32, nats.MaxWait(1500*time.Millisecond))
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
		_ = msg.Nak()
		return
	}
	var batch matching.BatchEventPayload
	if err := json.Unmarshal(msg.Data, &batch); err != nil {
		_ = msg.Term()
		return
	}
	evtSeq := int64(meta.Sequence.Stream)
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	lastEvtSeq, err := w.lockCursorRow(ctx, tx, batch.MarketID)
	if err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if evtSeq <= lastEvtSeq {
		_ = msg.Ack()
		return
	}
	if batch.SourceOrder != nil {
		if err := w.upsertSourceOrder(ctx, tx, &batch); err != nil {
			_ = msg.NakWithDelay(time.Second)
			return
		}
	}
	if err := w.persistTrades(ctx, tx, &batch); err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := w.applyStateEvents(ctx, tx, &batch); err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := w.upsertReservations(ctx, tx, &batch); err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := w.advanceCursor(ctx, tx, batch.MarketID, evtSeq, int64(batch.SourceCmdSeq)); err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		_ = msg.NakWithDelay(time.Second)
		return
	}
	_, _ = w.updateRedisReadModels(ctx, &batch)
	_ = msg.Ack()
}

func (w *Writer) lockCursorRow(ctx context.Context, tx pgx.Tx, marketID uint64) (int64, error) {
	marketIDStr := strconv.FormatUint(marketID, 10)
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

func (w *Writer) advanceCursor(ctx context.Context, tx pgx.Tx, marketID uint64, evtSeq, sourceCmdSeq int64) error {
	marketIDStr := strconv.FormatUint(marketID, 10)
	_, err := tx.Exec(ctx, `
		UPDATE consumer_cursors
		SET last_evt_seq = $1, last_source_cmd_seq = $2, updated_at = NOW()
		WHERE consumer_name = $3 AND market_id = $4::NUMERIC(20,0)
	`, evtSeq, sourceCmdSeq, w.consumerName, marketIDStr)
	return err
}

func (w *Writer) upsertSourceOrder(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	order := batch.SourceOrder
	status := "accepted"
	reservationStatus := "none"
	settlementStatus := "none"
	for _, state := range batch.StateEvents {
		if state.OrderID != order.OrderID {
			continue
		}
		status = mapOrderStatus(state.Status)
		if state.Status == matching.StatusNew || state.Status == matching.StatusPartiallyFilled {
			reservationStatus = "open_reserved"
		}
		if state.Status == matching.StatusFilled {
			settlementStatus = "pending"
		}
	}
	for _, reservation := range batch.Reservations {
		if reservation.OrderID != order.OrderID {
			continue
		}
		reservationStatus = reservationStatusFromSnapshot(reservation)
		if reservation.PendingSettlementUnits > 0 {
			settlementStatus = "pending"
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO orders (
			order_id, market_id, wallet_address, asset_kind, side, order_type, price_tick,
			qty_lots, spend_amount, remaining_qty, nonce, signature, status,
			reservation_status, settlement_status, expire_time, created_cmd_seq, created_at, updated_at
		) VALUES (
			$1, $2::NUMERIC(20,0), $3, $4, $5, $6, $7,
			$8, $9, $8, $10, $11, $12,
			$13, $14, $15, $16, $17, $17
		)
		ON CONFLICT (order_id) DO NOTHING
	`, int64(order.OrderID), strconv.FormatUint(batch.MarketID, 10), order.WalletAddress, order.AssetKind, sideLabel(order.OriginalAction), orderTypeLabel(order.OrderType), int16(order.PriceTick), int64(order.InitialQty), int64(order.InitialSpendAmount), strconv.FormatUint(order.Nonce, 10), order.Signature, status, reservationStatus, settlementStatus, order.ExpireTime, int64(order.CreatedCmdSeq), timestampToTime(batch.Timestamp))
	return err
}

func (w *Writer) persistTrades(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	for _, trade := range batch.TradeEvents {
		_, err := tx.Exec(ctx, `
			INSERT INTO trades (
				trade_id, market_id, maker_order_id, taker_order_id,
				maker_wallet_address, taker_wallet_address,
				match_price, match_qty, status, created_at
			) VALUES (
				$1, $2::NUMERIC(20,0), $3, $4,
				$5, $6,
				$7, $8, 'matched', $9
			)
			ON CONFLICT (trade_id) DO NOTHING
		`, trade.TradeID, strconv.FormatUint(batch.MarketID, 10), int64(trade.MakerOrderID), int64(trade.TakerOrderID), trade.MakerPubKey, trade.TakerPubKey, int16(trade.MatchPrice), int64(trade.MatchQty), timestampToTime(batch.Timestamp))
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) applyStateEvents(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	for _, state := range batch.StateEvents {
		reservationStatus := "none"
		settlementStatus := "none"
		for _, reservation := range batch.Reservations {
			if reservation.OrderID != state.OrderID {
				continue
			}
			reservationStatus = reservationStatusFromSnapshot(reservation)
			if reservation.PendingSettlementUnits > 0 {
				settlementStatus = "pending"
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE orders
			SET remaining_qty = $1, status = $2, reservation_status = $3, settlement_status = $4, updated_at = $5
			WHERE order_id = $6
		`, int64(state.RemainingQty), mapOrderStatus(state.Status), reservationStatus, settlementStatus, timestampToTime(batch.Timestamp), int64(state.OrderID))
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) upsertReservations(ctx context.Context, tx pgx.Tx, batch *matching.BatchEventPayload) error {
	for _, reservation := range batch.Reservations {
		reservationID := uuid.NewSHA1(uuid.Nil, []byte(strconv.FormatUint(reservation.OrderID, 10))).String()
		_, err := tx.Exec(ctx, `
			INSERT INTO wallet_reservations (
				reservation_id, order_id, wallet_address, asset_type, market_id,
				original_reserved_units, open_reserved_units, pending_settlement_units,
				released_units, finalized_units, rolled_back_units, updated_at
			) VALUES (
				$1, $2, $3, $4, $5::NUMERIC(20,0),
				$6, $7, $8, $9, $10, $11, $12
			)
			ON CONFLICT (order_id) DO UPDATE SET
				asset_type = EXCLUDED.asset_type,
				market_id = EXCLUDED.market_id,
				original_reserved_units = EXCLUDED.original_reserved_units,
				open_reserved_units = EXCLUDED.open_reserved_units,
				pending_settlement_units = EXCLUDED.pending_settlement_units,
				released_units = EXCLUDED.released_units,
				finalized_units = EXCLUDED.finalized_units,
				rolled_back_units = EXCLUDED.rolled_back_units,
				updated_at = EXCLUDED.updated_at
		`, reservationID, int64(reservation.OrderID), reservation.WalletAddress, reservation.AssetKind, strconv.FormatUint(reservation.MarketID, 10), int64(reservation.OriginalReservedUnits), int64(reservation.OpenReservedUnits), int64(reservation.PendingSettlementUnits), int64(reservation.ReleasedUnits), int64(reservation.FinalizedUnits), int64(reservation.RolledBackUnits), timestampToTime(batch.Timestamp))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO wallet_reservation_state (
				wallet_address, asset_type, market_id, reserved_open_units, reserved_pending_settlement_units, version, updated_at
			) VALUES ($1, $2, $3::NUMERIC(20,0), $4, $5, 1, $6)
			ON CONFLICT (wallet_address, asset_type, market_id) DO UPDATE SET
				reserved_open_units = EXCLUDED.reserved_open_units,
				reserved_pending_settlement_units = EXCLUDED.reserved_pending_settlement_units,
				version = wallet_reservation_state.version + 1,
				updated_at = EXCLUDED.updated_at
		`, reservation.WalletAddress, reservation.AssetKind, strconv.FormatUint(reservation.MarketID, 10), int64(reservation.OpenReservedUnits), int64(reservation.PendingSettlementUnits), timestampToTime(batch.Timestamp))
		if err != nil {
			return err
		}
	}
	return nil
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
		w.writeOpenOrderToRedis(ctx, pipe, batch)
	}
	for _, state := range batch.StateEvents {
		w.applyStateToRedis(ctx, pipe, batch, state)
	}
	if len(batch.TradeEvents) > 0 {
		tradesKey := fmt.Sprintf("trades:latest:%d", batch.MarketID)
		priceHistoryKey := fmt.Sprintf("price:history:%d", batch.MarketID)
		for _, trade := range batch.TradeEvents {
			entry, _ := json.Marshal(map[string]any{
				"id":          trade.TradeID,
				"price":       strconv.FormatUint(uint64(trade.MatchPrice), 10),
				"quantity":    strconv.FormatUint(trade.MatchQty, 10),
				"executed_at": timestampToTime(batch.Timestamp).Format(time.RFC3339),
			})
			pipe.LPush(ctx, tradesKey, entry)
			point, _ := json.Marshal(map[string]any{
				"timestamp": timestampToTime(batch.Timestamp).Format(time.RFC3339),
				"price":     strconv.FormatUint(uint64(trade.MatchPrice), 10),
				"quantity":  strconv.FormatUint(trade.MatchQty, 10),
			})
			pipe.ZAdd(ctx, priceHistoryKey, redis.Z{
				Score:  float64(timestampToTime(batch.Timestamp).UnixMilli()),
				Member: point,
			})
		}
		pipe.LTrim(ctx, tradesKey, 0, recentTradesLimit-1)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return pushes, err
	}
	if err := w.syncWalletStateRedis(ctx, batch); err != nil {
		return pushes, err
	}
	if err := w.syncPositionsRedis(ctx, batch); err != nil {
		return pushes, err
	}
	return pushes, nil
}

func (w *Writer) syncWalletStateRedis(ctx context.Context, batch *matching.BatchEventPayload) error {
	if w.pool == nil || w.rdb == nil {
		return nil
	}
	wallets := map[string]struct{}{}
	if batch.SourceOrder != nil && batch.SourceOrder.WalletAddress != "" {
		wallets[batch.SourceOrder.WalletAddress] = struct{}{}
	}
	for _, reservation := range batch.Reservations {
		if reservation.WalletAddress != "" {
			wallets[reservation.WalletAddress] = struct{}{}
		}
	}
	for _, state := range batch.StateEvents {
		if state.WalletAddress != "" {
			wallets[state.WalletAddress] = struct{}{}
		}
	}
	for wallet := range wallets {
		var confirmed int64
		var slot int64
		_ = w.pool.QueryRow(ctx, `
			SELECT confirmed_units, last_observed_slot
			FROM wallet_asset_balances
			WHERE wallet_address = $1 AND asset_type = 'collateral' AND market_id IS NULL
		`, wallet).Scan(&confirmed, &slot)
		var reservedOpen int64
		var reservedPending int64
		_ = w.pool.QueryRow(ctx, `
			SELECT COALESCE(reserved_open_units, 0), COALESCE(reserved_pending_settlement_units, 0)
			FROM wallet_reservation_state
			WHERE wallet_address = $1 AND asset_type = 'collateral' AND market_id IS NULL
		`, wallet).Scan(&reservedOpen, &reservedPending)
		available := confirmed - reservedOpen - reservedPending
		if available < 0 {
			available = 0
		}
		if err := w.rdb.HSet(ctx, fmt.Sprintf("wallet-state:%s", wallet), map[string]any{
			"collateral_confirmed_units":                   confirmed,
			"collateral_reserved_open_units":               reservedOpen,
			"collateral_reserved_pending_settlement_units": reservedPending,
			"collateral_available_units":                   available,
			"source_slot":                                  slot,
			"updated_at":                                   batch.Timestamp,
		}).Err(); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) syncPositionsRedis(ctx context.Context, batch *matching.BatchEventPayload) error {
	if w.pool == nil || w.rdb == nil {
		return nil
	}
	wallets := map[string]struct{}{}
	if batch.SourceOrder != nil && batch.SourceOrder.WalletAddress != "" {
		wallets[batch.SourceOrder.WalletAddress] = struct{}{}
	}
	for _, reservation := range batch.Reservations {
		if reservation.WalletAddress != "" {
			wallets[reservation.WalletAddress] = struct{}{}
		}
	}
	for _, wallet := range mapKeys(wallets) {
		var (
			yesFree, yesOpen, yesPending int64
			noFree, noOpen, noPending    int64
		)
		err := w.pool.QueryRow(ctx, `
			SELECT yes_free_lots, yes_reserved_open_lots, yes_pending_settlement_lots,
			       no_free_lots, no_reserved_open_lots, no_pending_settlement_lots
			FROM positions
			WHERE market_id = $1::NUMERIC(20,0) AND wallet_address = $2
		`, strconv.FormatUint(batch.MarketID, 10), wallet).Scan(&yesFree, &yesOpen, &yesPending, &noFree, &noOpen, &noPending)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := w.rdb.HSet(ctx, fmt.Sprintf("position:%d:%s", batch.MarketID, wallet), map[string]any{
				"yes_free_lots":               yesFree,
				"yes_reserved_open_lots":      yesOpen,
				"yes_pending_settlement_lots": yesPending,
				"no_free_lots":                noFree,
				"no_reserved_open_lots":       noOpen,
				"no_pending_settlement_lots":  noPending,
				"updated_at":                  batch.Timestamp,
			}).Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

func mapKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (w *Writer) writeOpenOrderToRedis(ctx context.Context, pipe redis.Pipeliner, batch *matching.BatchEventPayload) {
	order := batch.SourceOrder
	userOrdersKey := fmt.Sprintf("user:orders:%s", order.WalletAddress)
	orderInfoKey := fmt.Sprintf("order:info:%d", order.OrderID)
	pipe.ZAdd(ctx, userOrdersKey, redis.Z{Score: float64(order.CreatedCmdSeq), Member: strconv.FormatUint(order.OrderID, 10)})
	pipe.HSet(ctx, orderInfoKey, map[string]any{
		"market_id":         strconv.FormatUint(batch.MarketID, 10),
		"wallet_address":    order.WalletAddress,
		"side":              sideLabel(order.OriginalAction),
		"price_tick":        order.PriceTick,
		"remaining_qty":     order.InitialQty,
		"status":            matching.StatusNew,
		"created_cmd_seq":   order.CreatedCmdSeq,
		"reservation_state": reservationStatusFromBatch(batch, order.OrderID),
		"settlement_state":  settlementStatusFromBatch(batch, order.OrderID),
		"updated_at":        batch.Timestamp,
	})
}

func (w *Writer) applyStateToRedis(ctx context.Context, pipe redis.Pipeliner, batch *matching.BatchEventPayload, state matching.OrderStateEvent) {
	orderInfoKey := fmt.Sprintf("order:info:%d", state.OrderID)
	wallet := state.WalletAddress
	createdCmdSeq := float64(0)
	if batch.SourceOrder != nil && batch.SourceOrder.OrderID == state.OrderID {
		wallet = batch.SourceOrder.WalletAddress
		createdCmdSeq = float64(batch.SourceOrder.CreatedCmdSeq)
	} else if w.rdb != nil {
		if info, err := w.rdb.HGetAll(ctx, orderInfoKey).Result(); err == nil && len(info) > 0 {
			if wallet == "" {
				wallet = info["wallet_address"]
			}
			if raw := info["created_cmd_seq"]; raw != "" {
				if parsed, err := strconv.ParseFloat(raw, 64); err == nil {
					createdCmdSeq = parsed
				}
			}
		}
	}
	pipe.HSet(ctx, orderInfoKey, map[string]any{
		"market_id":         strconv.FormatUint(batch.MarketID, 10),
		"remaining_qty":     state.RemainingQty,
		"status":            state.Status,
		"reservation_state": reservationStatusFromBatch(batch, state.OrderID),
		"settlement_state":  settlementStatusFromBatch(batch, state.OrderID),
		"updated_at":        batch.Timestamp,
	})
	if state.Status == matching.StatusNew || state.Status == matching.StatusPartiallyFilled {
		if wallet != "" {
			pipe.ZAdd(ctx, fmt.Sprintf("user:orders:%s", wallet), redis.Z{Score: createdCmdSeq, Member: strconv.FormatUint(state.OrderID, 10)})
		}
		return
	}
	if state.Status == matching.StatusFilled || state.Status == matching.StatusCanceled || state.Status == matching.StatusExpired || state.Status == matching.StatusRejected {
		if wallet != "" {
			pipe.ZRem(ctx, fmt.Sprintf("user:orders:%s", wallet), strconv.FormatUint(state.OrderID, 10))
		}
		pipe.Expire(ctx, orderInfoKey, time.Hour)
	}
}

func buildPushMessages(batch *matching.BatchEventPayload) pushMessages {
	updatedAt := timestampToTime(batch.Timestamp)
	pushes := pushMessages{}
	if len(batch.DepthEvents) > 0 {
		levels := make([]protocol.MarketDepthLevel, 0, len(batch.DepthEvents))
		for _, event := range batch.DepthEvents {
			levels = append(levels, protocol.MarketDepthLevel{Side: event.Side, PriceTick: event.PriceTick, TotalVolume: event.TotalVolume})
		}
		pushes.marketDepths = append(pushes.marketDepths, protocol.MarketDepthPush{MarketID: strconv.FormatUint(batch.MarketID, 10), UpdatedAt: updatedAt.Format(time.RFC3339), SourceCmdSeq: strconv.FormatUint(batch.SourceCmdSeq, 10), Levels: levels})
	}
	for _, trade := range batch.TradeEvents {
		pushes.marketTrades = append(pushes.marketTrades, protocol.MarketTradePush{MarketID: strconv.FormatUint(batch.MarketID, 10), TradeID: trade.TradeID, MakerOrderID: strconv.FormatUint(trade.MakerOrderID, 10), TakerOrderID: strconv.FormatUint(trade.TakerOrderID, 10), MakerWalletAddress: trade.MakerPubKey, TakerWalletAddress: trade.TakerPubKey, PriceTick: strconv.FormatUint(uint64(trade.MatchPrice), 10), MatchQty: strconv.FormatUint(trade.MatchQty, 10), ExecutedAt: updatedAt.Format(time.RFC3339)})
	}
	for _, state := range batch.StateEvents {
		if state.WalletAddress == "" {
			continue
		}
		pushes.userOrders = append(pushes.userOrders, protocol.UserOrderPush{MarketID: strconv.FormatUint(batch.MarketID, 10), WalletAddress: state.WalletAddress, Order: protocol.UserOrderPatch{ID: strconv.FormatUint(state.OrderID, 10), Quantity: strconv.FormatUint(state.RemainingQty, 10), Status: state.Status, RefundAmount: strconv.FormatUint(state.RefundAmount, 10), UpdatedAt: updatedAt.Format(time.RFC3339)}})
	}
	return pushes
}

func (w *Writer) publishPushMessages(pushes pushMessages) error {
	if w.client == nil {
		return nil
	}
	for _, payload := range pushes.marketDepths {
		marketID, _ := strconv.ParseUint(payload.MarketID, 10, 64)
		if err := w.client.PublishCoreJSON(protocol.SubjectPushMarketDepth(marketID), payload); err != nil {
			return err
		}
	}
	for _, payload := range pushes.marketTrades {
		marketID, _ := strconv.ParseUint(payload.MarketID, 10, 64)
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

func depthField(side uint8, priceTick uint8) string {
	if side == matching.SideSell {
		return fmt.Sprintf("ask:%d", priceTick)
	}
	return fmt.Sprintf("bid:%d", priceTick)
}

func sideLabel(side uint8) string {
	if side == matching.SideSell {
		return "sell"
	}
	return "buy"
}

func orderTypeLabel(orderType uint8) string {
	if orderType == matching.OrderTypeMarket {
		return "market"
	}
	return "limit"
}

func mapOrderStatus(status uint8) string {
	switch status {
	case matching.StatusNew:
		return "live"
	case matching.StatusPartiallyFilled:
		return "partially_filled"
	case matching.StatusFilled:
		return "filled"
	case matching.StatusCanceled:
		return "cancelled"
	case matching.StatusExpired:
		return "expired"
	default:
		return "rejected"
	}
}

func reservationStatusFromBatch(batch *matching.BatchEventPayload, orderID uint64) string {
	for _, reservation := range batch.Reservations {
		if reservation.OrderID == orderID {
			return reservationStatusFromSnapshot(reservation)
		}
	}
	return "none"
}

func settlementStatusFromBatch(batch *matching.BatchEventPayload, orderID uint64) string {
	for _, reservation := range batch.Reservations {
		if reservation.OrderID == orderID && reservation.PendingSettlementUnits > 0 {
			return "pending"
		}
	}
	return "none"
}

func reservationStatusFromSnapshot(reservation matching.ReservationData) string {
	switch {
	case reservation.OpenReservedUnits > 0 && reservation.PendingSettlementUnits > 0:
		return "mixed"
	case reservation.OpenReservedUnits > 0:
		return "open_reserved"
	case reservation.PendingSettlementUnits > 0:
		return "pending_settlement_only"
	case reservation.FinalizedUnits > 0:
		return "finalized"
	case reservation.ReleasedUnits > 0 || reservation.RolledBackUnits > 0:
		return "released"
	default:
		return "none"
	}
}

func timestampToTime(ts int64) time.Time {
	if ts <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(ts, 0).UTC()
}
