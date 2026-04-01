package protocol

import (
	"context"
	"fmt"
	"time"
)

const (
	EventTypeOrderAccepted = "evt.order.accepted.v1"
	EventTypeOrderClosed   = "evt.order.closed.v1"
	EventTypeTradeExecuted = "evt.trade.executed.v1"
	EventTypeOrderbook     = "evt.orderbook.updated.v1"
)

const (
	SubjectOrderAccepted = "evt.order.accepted"
	SubjectOrderClosed   = "evt.order.closed"
	SubjectTradeExecuted = "evt.trade.executed"
	SubjectOrderbook     = "evt.orderbook.updated"
	SubjectBatchTrades   = "evt.trades"
	SubjectMatchBatchV2  = "evt.match.batch.v2"
)

func SubjectBatchTradesMarket(marketID uint64) string {
	return fmt.Sprintf("%s.%d", SubjectBatchTrades, marketID)
}

func SubjectMatchBatchV2Market(marketID uint64) string {
	return fmt.Sprintf("%s.%d", SubjectMatchBatchV2, marketID)
}

type OrderStatus string

const (
	OrderStatusOpen            OrderStatus = "open"
	OrderStatusPartiallyFilled OrderStatus = "partially_filled"
	OrderStatusFilled          OrderStatus = "filled"
	OrderStatusCanceled        OrderStatus = "canceled"
	OrderStatusRejected        OrderStatus = "rejected"
	OrderStatusExpired         OrderStatus = "expired"
)

type EventEnvelope[T any] struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	SchemaVersion int       `json:"schema_version"`
	MarketID      uint64    `json:"market_id"`
	Producer      string    `json:"producer"`
	TraceID       string    `json:"trace_id"`
	CommandID     string    `json:"command_id"`
	MarketSeq     int64     `json:"market_seq"`
	CreatedAt     time.Time `json:"created_at"`
	Payload       T         `json:"payload"`
}

type OrderAcceptedEvent struct {
	OrderID         string      `json:"order_id"`
	WalletAddress   string      `json:"wallet_address"`
	ClientOrderID   string      `json:"client_order_id"`
	IdempotencyKey  string      `json:"idempotency_key"`
	Side            Side        `json:"side"`
	Outcome         Outcome     `json:"outcome"`
	OrderType       OrderType   `json:"order_type"`
	TimeInForce     TimeInForce `json:"time_in_force"`
	LimitPriceTick  *int        `json:"limit_price_tick,omitempty"`
	QtyLots         int64       `json:"qty_lots"`
	OpenQtyLots     int64       `json:"open_qty_lots"`
	FilledQtyLots   int64       `json:"filled_qty_lots"`
	CanceledQtyLots int64       `json:"canceled_qty_lots"`
	Status          OrderStatus `json:"status"`
	RejectCode      string      `json:"reject_code"`
	RejectReason    string      `json:"reject_reason"`
	TakerFeeBPS     int         `json:"taker_fee_bps"`
	CreatorFeeBPS   int         `json:"creator_fee_bps"`
	PlatformFeeBPS  int         `json:"platform_fee_bps"`
}

type OrderClosedEvent struct {
	OrderID         string      `json:"order_id"`
	WalletAddress   string      `json:"wallet_address"`
	OpenQtyLots     int64       `json:"open_qty_lots"`
	FilledQtyLots   int64       `json:"filled_qty_lots"`
	CanceledQtyLots int64       `json:"canceled_qty_lots"`
	Status          OrderStatus `json:"status"`
	RejectCode      string      `json:"reject_code"`
	RejectReason    string      `json:"reject_reason"`
}

type TradeExecutedEvent struct {
	TradeID               string    `json:"trade_id"`
	TakerOrderID          string    `json:"taker_order_id"`
	MakerOrderID          string    `json:"maker_order_id"`
	TakerWalletAddress    string    `json:"taker_wallet_address"`
	MakerWalletAddress    string    `json:"maker_wallet_address"`
	TakerSide             Side      `json:"taker_side"`
	Outcome               Outcome   `json:"outcome"`
	PriceTick             int       `json:"price_tick"`
	QtyLots               int64     `json:"qty_lots"`
	NotionalUnits         int64     `json:"notional_units"`
	TakerFeeBPS           int       `json:"taker_fee_bps"`
	CreatorFeeBPS         int       `json:"creator_fee_bps"`
	PlatformFeeBPS        int       `json:"platform_fee_bps"`
	TakerFeeUnits         int64     `json:"taker_fee_units"`
	CreatorFeeUnits       int64     `json:"creator_fee_units"`
	PlatformFeeUnits      int64     `json:"platform_fee_units"`
	CreatorWalletAddress  string    `json:"creator_wallet_address"`
	PlatformWalletAddress string    `json:"platform_wallet_address"`
	ExecutedAt            time.Time `json:"executed_at"`
}

type OrderbookLevel struct {
	Price       string `json:"price"`
	TotalVolume string `json:"total_volume"`
}

type OrderbookUpdatedEvent struct {
	Bids            []OrderbookLevel `json:"bids"`
	Asks            []OrderbookLevel `json:"asks"`
	BestBidPrice    string           `json:"best_bid_price,omitempty"`
	BestAskPrice    string           `json:"best_ask_price,omitempty"`
	MatchingEnabled bool             `json:"matching_enabled"`
}

type BatchEventPayload struct {
	MarketID     string            `json:"market_id"`
	SourceCmdSeq uint64            `json:"source_cmd_seq"`
	Timestamp    int64             `json:"timestamp"`
	SourceOrder  *FullOrderData    `json:"source_order,omitempty"`
	TradeEvents  []TradeEvent      `json:"trade_events"`
	StateEvents  []OrderStateEvent `json:"state_events"`
	DepthEvents  []L2DepthEvent    `json:"depth_events"`
}

type FullOrderData struct {
	OrderID            string `json:"order_id"`
	WalletAddress      string `json:"wallet_address"`
	Side               uint8  `json:"side"`
	OrderType          uint8  `json:"order_type"`
	PriceTick          uint8  `json:"price_tick"`
	InitialQty         uint64 `json:"initial_qty"`
	InitialSpendAmount uint64 `json:"initial_spend_amount"`
	ExpireTime         int64  `json:"expire_time"`
	Signature          string `json:"signature"`
	IntentBytesHex     string `json:"intent_hex"`
	Nonce              uint64 `json:"nonce"`
	CreatedCmdSeq      uint64 `json:"created_cmd_seq"`
}

type TradeEvent struct {
	TradeID        string `json:"trade_id"`
	MakerOrderID   string `json:"maker_order_id"`
	MakerPubKey    string `json:"maker_pubkey"`
	MakerSignature string `json:"maker_sig"`
	TakerOrderID   string `json:"taker_order_id"`
	TakerPubKey    string `json:"taker_pubkey"`
	TakerSignature string `json:"taker_sig"`
	MatchPrice     uint64 `json:"match_price"`
	MatchQty       uint64 `json:"match_qty"`
}

type OrderStateEvent struct {
	OrderID       string `json:"order_id"`
	WalletAddress string `json:"wallet_address,omitempty"`
	Status        uint8  `json:"status"`
	RemainingQty  uint64 `json:"remaining_qty"`
}

type L2DepthEvent struct {
	Side        uint8  `json:"side"`
	Price       uint64 `json:"price"`
	TotalVolume uint64 `json:"total_volume"`
}

const (
	L2DepthSideBid uint8 = 0
	L2DepthSideAsk uint8 = 1

	OrderStateNew             uint8 = 1
	OrderStatePartiallyFilled uint8 = 2
	OrderStateFilled          uint8 = 3
	OrderStateCanceled        uint8 = 4
	OrderStateExpired         uint8 = 5
)

type EventPublisher interface {
	PublishOrderAccepted(context.Context, EventEnvelope[OrderAcceptedEvent]) error
	PublishOrderClosed(context.Context, EventEnvelope[OrderClosedEvent]) error
	PublishTradeExecuted(context.Context, EventEnvelope[TradeExecutedEvent]) error
	PublishOrderbook(context.Context, EventEnvelope[OrderbookUpdatedEvent]) error
}

type BatchEventPublisher interface {
	PublishBatchTrades(context.Context, uint64, BatchEventPayload) error
}

type DisabledEventPublisher struct{}

func (DisabledEventPublisher) PublishOrderAccepted(context.Context, EventEnvelope[OrderAcceptedEvent]) error {
	return ErrCommandBusDisabled
}

func (DisabledEventPublisher) PublishOrderClosed(context.Context, EventEnvelope[OrderClosedEvent]) error {
	return ErrCommandBusDisabled
}

func (DisabledEventPublisher) PublishTradeExecuted(context.Context, EventEnvelope[TradeExecutedEvent]) error {
	return ErrCommandBusDisabled
}

func (DisabledEventPublisher) PublishOrderbook(context.Context, EventEnvelope[OrderbookUpdatedEvent]) error {
	return ErrCommandBusDisabled
}

func (DisabledEventPublisher) PublishBatchTrades(context.Context, uint64, BatchEventPayload) error {
	return ErrCommandBusDisabled
}
