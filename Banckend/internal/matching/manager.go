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
	"blinkpredict/banckend/internal/walletshard"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var logger = logging.New("matcher")

type MarketManager struct {
	client  *natsjs.Client
	pool    *pgxpool.Pool
	wallets *walletshard.Manager
	mu      sync.RWMutex
	actors  map[uint64]*MarketActor
}

func NewMarketManager(client *natsjs.Client, pool *pgxpool.Pool) *MarketManager {
	return &MarketManager{
		client:  client,
		pool:    pool,
		wallets: walletshard.New(pool, 128),
		actors:  make(map[uint64]*MarketActor),
	}
}

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

func (m *MarketManager) StartConsumer(ctx context.Context) error {
	sub, err := m.client.JetStream().QueueSubscribe(protocol.SubjectPlaceOrder, "matcher_group", func(msg *nats.Msg) {
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
		placeCmd, ok := wrapper.Cmd.(*PlaceOrderCommand)
		if !ok {
			msg.Term()
			return
		}

		assetKind, reserveUnits := reserveRequirement(placeCmd)
		if _, err := m.wallets.TryReserveOpenOrder(ctx, placeCmd.WalletAddress, placeCmd.OrderID, placeCmd.MarketID, assetKind, reserveUnits); err != nil {
			batch := rejectedBatch(placeCmd, wrapper.SourceCmdSeq, assetKind, reserveUnits)
			if pubErr := m.publishBatch(batch); pubErr != nil {
				logger.Warnf("failed to publish rejected batch: %v", pubErr)
				msg.Nak()
				return
			}
			msg.Ack()
			return
		}

		actor := m.GetOrCreateMarket(wrapper.Cmd.GetMarketID())
		select {
		case actor.CmdChan <- wrapper:
		default:
			_, _ = m.wallets.ReleaseOpenReserve(ctx, placeCmd.WalletAddress, placeCmd.OrderID)
			logger.Warnf("backpressure market=%d channel full, rejecting message", actor.MarketID)
			msg.Nak()
		}
	}, nats.Durable("matcher_consumer"))
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	logger.Infof("Matcher consumer started on subject: %s", protocol.SubjectPlaceOrder)
	<-ctx.Done()
	sub.Unsubscribe()
	return nil
}

func (m *MarketManager) runMatcherEngine(actor *MarketActor) {
	logger.Infof("Market %d matcher engine started", actor.MarketID)
	for wrapper := range actor.CmdChan {
		currentSeq := wrapper.SourceCmdSeq
		if currentSeq > 0 && currentSeq <= actor.Book.LastProcessedSeq {
			if wrapper.Msg != nil {
				wrapper.Msg.Ack()
			}
			continue
		}

		batch := &BatchEventPayload{
			MarketID:     actor.MarketID,
			SourceCmdSeq: currentSeq,
			Timestamp:    wrapper.Cmd.GetTimestamp(),
			TradeEvents:  make([]TradeEvent, 0, 4),
			StateEvents:  make([]OrderStateEvent, 0, 4),
			DepthEvents:  make([]L2DepthEvent, 0, 4),
		}
		if cmd, ok := wrapper.Cmd.(*PlaceOrderCommand); ok {
			assetKind, reserveUnits := reserveRequirement(cmd)
			batch.SourceOrder = &FullOrderData{
				OrderID:            cmd.OrderID,
				WalletAddress:      cmd.WalletAddress,
				AssetKind:          assetKind,
				OriginalAction:     cmd.OriginalAction,
				OriginalOutcome:    cmd.OriginalOutcome,
				OriginalPriceTick:  cmd.OriginalPriceTick,
				Side:               cmd.Side,
				OrderType:          cmd.OrderType,
				PriceTick:          cmd.PriceTick,
				InitialQty:         cmd.QtyLots,
				InitialSpendAmount: cmd.SpendAmount,
				ReservedUnits:      reserveUnits,
				ExpireTime:         cmd.ExpireTime,
				Signature:          cmd.Signature,
				IntentBytesHex:     cmd.IntentBytesHex,
				Nonce:              cmd.Nonce,
				CreatedCmdSeq:      currentSeq,
			}
		}

		actor.Book.ProcessCommand(wrapper.Cmd, batch)
		if err := m.applyWalletSideEffects(context.Background(), batch); err != nil {
			logger.Warnf("wallet side effects failed market=%d err=%v", actor.MarketID, err)
			if wrapper.Msg != nil {
				wrapper.Msg.Nak()
			}
			continue
		}
		if err := m.publishBatch(batch); err != nil {
			logger.Warnf("failed to publish event stream: %v", err)
			if wrapper.Msg != nil {
				wrapper.Msg.Nak()
			}
			continue
		}
		if currentSeq > 0 {
			actor.Book.LastProcessedSeq = currentSeq
		}
		if wrapper.Msg != nil {
			wrapper.Msg.Ack()
		}
	}
}

func (m *MarketManager) publishBatch(batch *BatchEventPayload) error {
	if len(batch.TradeEvents) == 0 && len(batch.StateEvents) == 0 && len(batch.DepthEvents) == 0 {
		return nil
	}
	data, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("failed to marshal batch: %w", err)
	}
	subject := fmt.Sprintf("evt.trades.%d", batch.MarketID)
	_, err = m.client.JetStream().Publish(subject, data)
	if err != nil {
		return fmt.Errorf("failed to publish to %s: %w", subject, err)
	}
	return nil
}

func (m *MarketManager) parseCommandFromJSON(data []byte) (*CommandWrapper, error) {
	var env protocol.CommandEnvelope[protocol.PlaceOrderCommand]
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse command envelope: %w", err)
	}
	return &CommandWrapper{Cmd: ConvertProtocolToPlaceOrderCommand(env.Payload)}, nil
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
			o.asset_kind,
			o.side,
			o.order_type,
			o.price_tick,
			o.spend_amount,
			o.remaining_qty,
			o.expire_time,
			o.signature,
			o.nonce,
			EXTRACT(EPOCH FROM mk.close_time)::BIGINT AS close_time
		FROM orders o
		JOIN markets mk ON mk.market_id = o.market_id
		WHERE o.status IN ('live', 'partially_filled')
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
			assetKind     string
			side          string
			orderType     string
			priceTick     int16
			spendAmount   int64
			remainingQty  int64
			expireTime    int64
			signature     string
			nonce         int64
			closeTime     int64
		)
		if err := rows.Scan(&orderID, &marketIDStr, &walletAddress, &assetKind, &side, &orderType, &priceTick, &spendAmount, &remainingQty, &expireTime, &signature, &nonce, &closeTime); err != nil {
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
		order.Side = parseSide(side)
		order.OrderType = parseOrderType(orderType)
		order.PriceTick = uint8(priceTick)
		order.RemainingSpend = uint64(spendAmount)
		order.RemainingQty = uint64(remainingQty)
		order.ExpireTime = expireTime
		order.Signature = signature
		order.Nonce = uint64(nonce)
		actor.Book.RestoreOrder(order)
		reserveUnits := recoveredReserveUnits(assetKind, uint64(remainingQty), uint8(priceTick), uint64(spendAmount), order.OrderType)
		if err := m.wallets.ReserveRecoveredOrder(ctx, walletAddress, uint64(orderID), marketIDU, assetKind, reserveUnits); err != nil {
			return err
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
					case actor.CmdChan <- &CommandWrapper{Cmd: &TickCommand{MarketID: actor.MarketID, Timestamp: t.UTC().Unix()}, SourceCmdSeq: 0}:
					default:
						logger.Warnf("tick backpressure market=%d", actor.MarketID)
					}
				}
			}
		}
	}()
}

func reserveRequirement(cmd *PlaceOrderCommand) (string, uint64) {
	if cmd.OriginalAction == SideBuy {
		if cmd.OrderType == OrderTypeMarket {
			return walletshard.AssetCollateral, cmd.SpendAmount
		}
		return walletshard.AssetCollateral, (cmd.QtyLots * uint64(cmd.OriginalPriceTick)) / 100
	}
	if cmd.OriginalOutcome == 0 {
		return walletshard.AssetYes, cmd.QtyLots
	}
	return walletshard.AssetNo, cmd.QtyLots
}

func rejectedBatch(cmd *PlaceOrderCommand, sourceCmdSeq uint64, assetKind string, reservedUnits uint64) *BatchEventPayload {
	batch := &BatchEventPayload{
		MarketID:     cmd.MarketID,
		SourceCmdSeq: sourceCmdSeq,
		Timestamp:    cmd.Timestamp,
		TradeEvents:  make([]TradeEvent, 0, 1),
		StateEvents:  make([]OrderStateEvent, 0, 1),
		DepthEvents:  make([]L2DepthEvent, 0),
	}
	batch.SourceOrder = &FullOrderData{
		OrderID:            cmd.OrderID,
		WalletAddress:      cmd.WalletAddress,
		AssetKind:          assetKind,
		OriginalAction:     cmd.OriginalAction,
		OriginalOutcome:    cmd.OriginalOutcome,
		OriginalPriceTick:  cmd.OriginalPriceTick,
		Side:               cmd.Side,
		OrderType:          cmd.OrderType,
		PriceTick:          cmd.PriceTick,
		InitialQty:         cmd.QtyLots,
		InitialSpendAmount: cmd.SpendAmount,
		ReservedUnits:      reservedUnits,
		ExpireTime:         cmd.ExpireTime,
		Signature:          cmd.Signature,
		IntentBytesHex:     cmd.IntentBytesHex,
		Nonce:              cmd.Nonce,
		CreatedCmdSeq:      sourceCmdSeq,
	}
	batch.AddStateEvent(cmd.OrderID, cmd.WalletAddress, StatusRejected, cmd.QtyLots, cmd.SpendAmount)
	return batch
}

func reservationSnapshotToData(snapshot *walletshard.ReservationSnapshot) ReservationData {
	return ReservationData{
		OrderID:                snapshot.OrderID,
		WalletAddress:          snapshot.WalletAddress,
		AssetKind:              snapshot.AssetKind,
		MarketID:               snapshot.MarketID,
		OriginalReservedUnits:  snapshot.OriginalReservedUnits,
		OpenReservedUnits:      snapshot.OpenReservedUnits,
		PendingSettlementUnits: snapshot.PendingSettlementUnits,
		ReleasedUnits:          snapshot.ReleasedUnits,
		FinalizedUnits:         snapshot.FinalizedUnits,
		RolledBackUnits:        snapshot.RolledBackUnits,
	}
}

func (m *MarketManager) applyWalletSideEffects(ctx context.Context, batch *BatchEventPayload) error {
	if m.wallets == nil {
		return nil
	}
	seen := map[uint64]struct{}{}
	for _, trade := range batch.TradeEvents {
		pairs := []struct {
			orderID uint64
			wallet  string
		}{
			{orderID: trade.MakerOrderID, wallet: trade.MakerPubKey},
			{orderID: trade.TakerOrderID, wallet: trade.TakerPubKey},
		}
		for _, pair := range pairs {
			snapshot, err := m.wallets.ApplyTrade(ctx, pair.wallet, pair.orderID, batch.MarketID, trade.MatchQty)
			if err != nil {
				return err
			}
			if snapshot != nil {
				seen[pair.orderID] = struct{}{}
				batch.Reservations = append(batch.Reservations, reservationSnapshotToData(snapshot))
			}
		}
	}
	for _, state := range batch.StateEvents {
		if state.Status != StatusCanceled && state.Status != StatusExpired && state.Status != StatusRejected {
			continue
		}
		snapshot, err := m.wallets.ReleaseOpenReserve(ctx, state.WalletAddress, state.OrderID)
		if err != nil {
			return err
		}
		if snapshot != nil {
			if _, ok := seen[state.OrderID]; ok {
				continue
			}
			batch.Reservations = append(batch.Reservations, reservationSnapshotToData(snapshot))
		}
	}
	return nil
}

func parseSide(v string) uint8 {
	if v == "sell" {
		return SideSell
	}
	return SideBuy
}

func parseOrderType(v string) uint8 {
	if v == "market" {
		return OrderTypeMarket
	}
	return OrderTypeLimit
}

func recoveredReserveUnits(assetKind string, remainingQty uint64, priceTick uint8, spendAmount uint64, orderType uint8) uint64 {
	if assetKind == walletshard.AssetCollateral {
		if orderType == OrderTypeMarket {
			return spendAmount
		}
		return (remainingQty * uint64(priceTick)) / 100
	}
	return remainingQty
}
