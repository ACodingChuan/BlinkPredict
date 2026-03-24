package pusher

import (
	"context"
	"encoding/json"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
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
		{subject: "push.market.*.depth", handler: s.handleMarketDepth},
		{subject: "push.market.*.trade", handler: s.handleMarketTrade},
		{subject: "push.user.*.order", handler: s.handleUserOrder},
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

func (s *Service) handleMarketDepth(msg *nats.Msg) {
	var payload protocol.MarketDepthPush
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		logger.Warnf("decode market depth failed: %v", err)
		return
	}
	if err := s.hub.PublishMarketDepth(payload); err != nil {
		logger.Warnf("publish market depth failed: %v", err)
	}
}

func (s *Service) handleMarketTrade(msg *nats.Msg) {
	var payload protocol.MarketTradePush
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		logger.Warnf("decode market trade failed: %v", err)
		return
	}
	if err := s.hub.PublishMarketTrade(payload); err != nil {
		logger.Warnf("publish market trade failed: %v", err)
	}
}

func (s *Service) handleUserOrder(msg *nats.Msg) {
	var payload protocol.UserOrderPush
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		logger.Warnf("decode user order failed: %v", err)
		return
	}
	if err := s.hub.PublishUserOrder(payload); err != nil {
		logger.Warnf("publish user order failed: %v", err)
	}
}
