package natsjs

import (
	"context"

	"blinkpredict/banckend/internal/protocol"
)

type CommandPublisher struct {
	client *Client
}

type EventPublisher struct {
	client *Client
}

func NewCommandPublisher(client *Client) *CommandPublisher {
	return &CommandPublisher{client: client}
}

func NewEventPublisher(client *Client) *EventPublisher {
	return &EventPublisher{client: client}
}

func (p *CommandPublisher) PublishPlaceOrder(ctx context.Context, env protocol.CommandEnvelope[protocol.PlaceOrderCommand]) error {
	return p.client.publishJSON(ctx, protocol.SubjectPlaceOrder, env.ID, env)
}

func (p *CommandPublisher) PublishCancelOrder(ctx context.Context, env protocol.CommandEnvelope[protocol.CancelOrderCommand]) error {
	return p.client.publishJSON(ctx, protocol.SubjectCancelOrder, env.ID, env)
}

var _ protocol.CommandPublisher = (*CommandPublisher)(nil)

func (p *EventPublisher) PublishOrderAccepted(ctx context.Context, env protocol.EventEnvelope[protocol.OrderAcceptedEvent]) error {
	return p.client.publishJSON(ctx, protocol.SubjectOrderAccepted, env.ID, env)
}

func (p *EventPublisher) PublishOrderClosed(ctx context.Context, env protocol.EventEnvelope[protocol.OrderClosedEvent]) error {
	return p.client.publishJSON(ctx, protocol.SubjectOrderClosed, env.ID, env)
}

func (p *EventPublisher) PublishTradeExecuted(ctx context.Context, env protocol.EventEnvelope[protocol.TradeExecutedEvent]) error {
	return p.client.publishJSON(ctx, protocol.SubjectTradeExecuted, env.ID, env)
}

func (p *EventPublisher) PublishOrderbook(ctx context.Context, env protocol.EventEnvelope[protocol.OrderbookUpdatedEvent]) error {
	return p.client.publishJSON(ctx, protocol.SubjectOrderbook, env.ID, env)
}

var _ protocol.EventPublisher = (*EventPublisher)(nil)
