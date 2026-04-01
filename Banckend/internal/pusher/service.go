package pusher

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/nats-io/nats.go"
)

var logger = logging.New("pusher")

const (
	defaultConsumerName = "pusher-primary"
	pusherCatchUpBatch  = 64
	pusherRunBatch      = 32
)

type Service struct {
	client       *natsjs.Client
	hub          *Hub
	consumerName string
	sub          *nats.Subscription
}

func NewService(client *natsjs.Client, hub *Hub) *Service {
	return &Service{
		client:       client,
		hub:          hub,
		consumerName: defaultConsumerName,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s.client == nil || s.hub == nil {
		return nil
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	if err := s.catchUp(ctx); err != nil {
		return err
	}
	go s.run(ctx)
	return nil
}

func (s *Service) ensureSubscription() error {
	if s.sub != nil {
		return nil
	}
	sub, err := s.client.PullSubscribe(protocol.SubjectMatchBatchV2+".*", s.consumerName)
	if err != nil {
		return err
	}
	s.sub = sub
	return nil
}

func (s *Service) catchUp(ctx context.Context) error {
	_ = ctx
	for {
		msgs, err := s.sub.Fetch(pusherCatchUpBatch, nats.MaxWait(500*time.Millisecond))
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
			s.handleMessage(msg)
		}
	}
}

func (s *Service) run(ctx context.Context) {
	defer func() {
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := s.sub.Fetch(pusherRunBatch, nats.MaxWait(1500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			logger.Warnf("fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			s.handleMessage(msg)
		}
	}
}

func (s *Service) handleMessage(msg *nats.Msg) {
	var event matching.MatchBatchEventV2
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logger.Warnf("decode match batch failed: %v", err)
		_ = msg.Term()
		return
	}
	if event.SchemaVersion != 0 && event.SchemaVersion != 1 {
		logger.Warnf("unsupported schema version: %d", event.SchemaVersion)
		_ = msg.Term()
		return
	}

	orderByIndex := make(map[uint16]matching.MatchedOrderV2, len(event.Orders))
	for _, order := range event.Orders {
		orderByIndex[order.OrderIndex] = order
	}

	ts := time.Unix(event.ProducedAt, 0).UTC().Format(time.RFC3339)
	if len(event.DepthUpdates) > 0 {
		levels := compressDepthUpdates(event.DepthUpdates)
		msgBody := protocol.WSMarketDepthDelta{
			Type:     protocol.WSTypeMarketDepthDelta,
			MarketID: strconv.FormatUint(event.MarketID, 10),
			Ts:       ts,
			Payload:  protocol.WSMarketDepthPayload{Levels: levels},
		}
		if err := s.hub.PublishMarketMessage(event.MarketID, msgBody); err != nil {
			logger.Warnf("publish depth delta failed market=%d err=%v", event.MarketID, err)
		}
	}

	for _, fill := range event.Fills {
		maker, makerOK := orderByIndex[fill.MakerOrderIndex]
		taker, takerOK := orderByIndex[fill.TakerOrderIndex]
		if !makerOK || !takerOK {
			continue
		}
		msgBody := protocol.WSMarketTradeExecuted{
			Type:     protocol.WSTypeMarketTradeExecuted,
			MarketID: strconv.FormatUint(event.MarketID, 10),
			Ts:       ts,
			Payload: protocol.WSMarketTradePayload{
				TradeID:            tradeIDForFill(event.EventID, fill),
				MakerOrderID:       strconv.FormatUint(maker.OrderID, 10),
				TakerOrderID:       strconv.FormatUint(taker.OrderID, 10),
				MakerWalletAddress: maker.Execution.WalletAddress,
				TakerWalletAddress: taker.Execution.WalletAddress,
				PriceTick:          strconv.FormatUint(fill.FillPrice, 10),
				FillAmount:         strconv.FormatUint(fill.FillAmount, 10),
				MatchType:          fill.MatchType,
				ExecutedAt:         ts,
			},
		}
		if err := s.hub.PublishMarketMessage(event.MarketID, msgBody); err != nil {
			logger.Warnf("publish trade delta failed market=%d err=%v", event.MarketID, err)
		}
	}

	for _, update := range event.OrderUpdates {
		order, ok := orderByIndex[update.OrderIndex]
		if !ok || order.Execution.WalletAddress == "" {
			continue
		}
		msgBody := protocol.WSUserOrderUpdated{
			Type:     protocol.WSTypeUserOrderUpdated,
			MarketID: strconv.FormatUint(event.MarketID, 10),
			Ts:       ts,
			Payload: protocol.WSUserOrderPayload{
				OrderID:              strconv.FormatUint(order.OrderID, 10),
				Status:               update.Status,
				RemainingQtyLots:     strconv.FormatUint(update.RemainingQtyLots, 10),
				RemainingSpendAmount: strconv.FormatUint(update.RemainingSpendAmount, 10),
				RefundAmount:         strconv.FormatUint(update.RefundAmount, 10),
				UpdatedAt:            ts,
				OriginalAction:       order.Execution.OriginalAction,
				OriginalOutcome:      order.Execution.OriginalOutcome,
				OriginalPriceTick:    strconv.FormatUint(uint64(order.Execution.OriginalPriceTick), 10),
			},
		}
		if err := s.hub.PublishUserMessage(order.Execution.WalletAddress, msgBody); err != nil {
			logger.Warnf("publish user order delta failed wallet=%s err=%v", order.Execution.WalletAddress, err)
		}
	}

	_ = msg.Ack()
}

func compressDepthUpdates(updates []matching.DepthUpdateV2) []protocol.WSDepthLevel {
	type key struct {
		side      string
		priceTick uint8
	}
	latest := make(map[key]matching.DepthUpdateV2, len(updates))
	order := make([]key, 0, len(updates))
	for _, update := range updates {
		k := key{side: update.Side, priceTick: update.PriceTick}
		if _, ok := latest[k]; !ok {
			order = append(order, k)
		}
		latest[k] = update
	}

	levels := make([]protocol.WSDepthLevel, 0, len(order))
	for _, k := range order {
		update := latest[k]
		levels = append(levels, protocol.WSDepthLevel{
			Side:        update.Side,
			PriceTick:   update.PriceTick,
			TotalVolume: update.TotalVolume,
		})
	}
	return levels
}

func tradeIDForFill(eventID string, fill matching.MatchFillV2) string {
	if eventID == "" {
		return "fill-" + strconv.FormatUint(uint64(fill.FillIndex), 10)
	}
	return eventID + "-" + strconv.FormatUint(uint64(fill.FillIndex), 10)
}
