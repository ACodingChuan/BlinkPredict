package pusher

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/nats-io/nats.go"
)

var logger = logging.New("pusher")

type Service struct {
	client *natsjs.Client
	hub    *Hub
	subs   []*nats.Subscription
}

func NewService(client *natsjs.Client, hub *Hub) *Service {
	return &Service{client: client, hub: hub}
}

func (s *Service) Start(ctx context.Context) error {
	if s.client == nil || s.hub == nil {
		return nil
	}
	subscriptions := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{subject: "evt.trades.*", handler: s.handleRealtimeBatch},
	}
	for _, subscription := range subscriptions {
		sub, err := s.client.Conn().Subscribe(subscription.subject, subscription.handler)
		if err != nil {
			return err
		}
		s.subs = append(s.subs, sub)
	}
	go func() {
		<-ctx.Done()
		for _, sub := range s.subs {
			_ = sub.Unsubscribe()
		}
	}()
	return nil
}

func (s *Service) handleRealtimeBatch(msg *nats.Msg) {
	var batch matching.BatchEventPayload
	if err := json.Unmarshal(msg.Data, &batch); err != nil {
		logger.Warnf("decode realtime batch failed: %v", err)
		return
	}
	updatedAt := matchingTimestamp(batch.Timestamp)
	if len(batch.DepthEvents) > 0 {
		levels := make([]protocol.MarketDepthLevel, 0, len(batch.DepthEvents))
		for _, event := range batch.DepthEvents {
			levels = append(levels, protocol.MarketDepthLevel{
				Side:        event.Side,
				PriceTick:   event.PriceTick,
				TotalVolume: event.TotalVolume,
			})
		}
		payload := protocol.MarketDepthPush{
			MarketID:     jsonUint(batch.MarketID),
			UpdatedAt:    updatedAt,
			SourceCmdSeq: jsonUint(batch.SourceCmdSeq),
			Levels:       levels,
		}
		if err := s.hub.PublishMarketDepth(payload); err != nil {
			logger.Warnf("publish market depth failed: %v", err)
		}
	}
	for _, trade := range batch.TradeEvents {
		payload := protocol.MarketTradePush{
			MarketID:           jsonUint(batch.MarketID),
			TradeID:            trade.TradeID,
			MakerOrderID:       jsonUint(trade.MakerOrderID),
			TakerOrderID:       jsonUint(trade.TakerOrderID),
			MakerWalletAddress: trade.MakerPubKey,
			TakerWalletAddress: trade.TakerPubKey,
			PriceTick:          jsonUint(uint64(trade.MatchPrice)),
			MatchQty:           jsonUint(trade.MatchQty),
			ExecutedAt:         updatedAt,
		}
		if err := s.hub.PublishMarketTrade(payload); err != nil {
			logger.Warnf("publish market trade failed: %v", err)
		}
	}
	for _, state := range batch.StateEvents {
		if state.WalletAddress == "" {
			continue
		}
		payload := protocol.UserOrderPush{
			MarketID:      jsonUint(batch.MarketID),
			WalletAddress: state.WalletAddress,
			Order: protocol.UserOrderPatch{
				ID:           jsonUint(state.OrderID),
				Quantity:     jsonUint(state.RemainingQty),
				Status:       state.Status,
				RefundAmount: jsonUint(state.RefundAmount),
				UpdatedAt:    updatedAt,
			},
		}
		if batch.SourceOrder != nil && batch.SourceOrder.OrderID == state.OrderID {
			payload.Order.Side = matchingSideLabel(batch.SourceOrder.OriginalAction)
			payload.Order.Outcome = matchingOutcomeLabel(batch.SourceOrder.OriginalOutcome)
			payload.Order.Price = jsonUint(uint64(batch.SourceOrder.OriginalPriceTick))
		}
		if err := s.hub.PublishUserOrder(payload); err != nil {
			logger.Warnf("publish user order failed: %v", err)
		}
	}
}

func jsonUint(v uint64) string {
	return strconv.FormatUint(v, 10)
}

func matchingTimestamp(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func matchingSideLabel(side uint8) string {
	if side == matching.SideSell {
		return "sell"
	}
	return "buy"
}

func matchingOutcomeLabel(outcome uint8) string {
	if outcome == 1 {
		return "no"
	}
	return "yes"
}
