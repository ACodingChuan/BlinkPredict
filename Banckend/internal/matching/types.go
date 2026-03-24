package matching

import (
	"context"
	"github.com/nats-io/nats.go"
)

// ==========================================
// 向后兼容的查询接口
// ==========================================
const (
	ErrMatchingDisabled = "matching_not_enabled"
)

// Engine 查询引擎接口（向后兼容）
type Engine interface {
	GetOrderbook(context.Context, uint64) OrderbookSnapshot
	GetTrades(context.Context, uint64) []Trade
	GetOpenOrders(context.Context, string, uint64) []OpenOrder
	GetPriceHistory(context.Context, uint64, PriceHistoryRange) PriceHistory
}

// OrderbookSnapshot 订单簿快照
type OrderbookSnapshot struct {
	Bids            []OrderLevel `json:"bids"`
	Asks            []OrderLevel `json:"asks"`
	BestBidPrice    string       `json:"best_bid_price,omitempty"`
	BestAskPrice    string       `json:"best_ask_price,omitempty"`
	MatchingEnabled bool         `json:"matching_enabled"`
}

// OrderLevel 价格档位
type OrderLevel struct {
	Price       string `json:"price"`
	TotalVolume string `json:"total_volume"`
}

// Trade 交易记录
type Trade struct {
	ID         string `json:"id"`
	Price      string `json:"price,omitempty"`
	Quantity   string `json:"quantity,omitempty"`
	ExecutedAt string `json:"executed_at,omitempty"`
}

type PriceHistoryRange string

const (
	PriceHistoryRange1H  PriceHistoryRange = "1H"
	PriceHistoryRange6H  PriceHistoryRange = "6H"
	PriceHistoryRange1D  PriceHistoryRange = "1D"
	PriceHistoryRange1W  PriceHistoryRange = "1W"
	PriceHistoryRange1M  PriceHistoryRange = "1M"
	PriceHistoryRangeAll PriceHistoryRange = "ALL"
)

type PricePoint struct {
	Timestamp string `json:"timestamp"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity,omitempty"`
}

type PriceHistory struct {
	Range  PriceHistoryRange `json:"range"`
	Points []PricePoint      `json:"points"`
}

// OpenOrder 开仓订单
type OpenOrder struct {
	ID       string `json:"id"`
	Side     string `json:"side,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Price    string `json:"price,omitempty"`
	Quantity string `json:"quantity,omitempty"`
}

// DisabledEngine 禁用的引擎
type DisabledEngine struct{}

func NewDisabledEngine() DisabledEngine { return DisabledEngine{} }

func (DisabledEngine) GetOrderbook(context.Context, uint64) OrderbookSnapshot {
	return OrderbookSnapshot{Bids: []OrderLevel{}, Asks: []OrderLevel{}, MatchingEnabled: false}
}
func (DisabledEngine) GetTrades(context.Context, uint64) []Trade { return []Trade{} }
func (DisabledEngine) GetOpenOrders(context.Context, string, uint64) []OpenOrder {
	return []OpenOrder{}
}
func (DisabledEngine) GetPriceHistory(context.Context, uint64, PriceHistoryRange) PriceHistory {
	return PriceHistory{Range: PriceHistoryRange1D, Points: []PricePoint{}}
}

type DisabledError struct{}

func (e *DisabledError) Error() string { return ErrMatchingDisabled }

var _ Engine = DisabledEngine{}

// ==========================================
// 1. 常量字典
// ==========================================
const (
	SideBuy  uint8 = 0
	SideSell uint8 = 1

	OrderTypeLimit  uint8 = 0
	OrderTypeMarket uint8 = 1

	StatusNew             uint8 = 1
	StatusPartiallyFilled uint8 = 2
	StatusFilled          uint8 = 3
	StatusCanceled        uint8 = 4
	StatusExpired         uint8 = 5
	StatusRejected        uint8 = 6
)

// ==========================================
// 2. Command 多态体系 (输入)
// ==========================================
type CommandType uint8

const (
	CmdTypePlaceOrder CommandType = iota
	CmdTypeTick
	CmdTypeHaltMarket
)

type Command interface {
	GetType() CommandType
	GetMarketID() uint64
	GetTimestamp() int64
}

// PlaceOrderCommand 下单命令
type PlaceOrderCommand struct {
	OrderID           uint64 `json:"order_id"`
	MarketID          uint64 `json:"market_id"`
	WalletAddress     string `json:"wallet_address"`
	OriginalAction    uint8  `json:"original_action"`
	OriginalOutcome   uint8  `json:"original_outcome"`
	OriginalPriceTick uint8  `json:"original_price_tick"`
	Side              uint8  `json:"side"`
	OrderType         uint8  `json:"order_type"`
	PriceTick         uint8  `json:"price_tick"`
	QtyLots           uint64 `json:"qty_lots"`
	SpendAmount       uint64 `json:"spend_amount"`
	ExpireTime        int64  `json:"expire_time"`
	Signature         string `json:"signature"`
	IntentBytesHex    string `json:"intent_bytes"`
	Nonce             uint64 `json:"nonce"` // 防碰撞nonce
	Timestamp         int64  `json:"timestamp"`
}

func (c *PlaceOrderCommand) GetType() CommandType { return CmdTypePlaceOrder }
func (c *PlaceOrderCommand) GetMarketID() uint64  { return c.MarketID }
func (c *PlaceOrderCommand) GetTimestamp() int64  { return c.Timestamp }

// TickCommand 时间驱动命令（用于过期订单清理）
type TickCommand struct {
	MarketID  uint64 `json:"market_id"`
	Timestamp int64  `json:"timestamp"`
}

func (c *TickCommand) GetType() CommandType { return CmdTypeTick }
func (c *TickCommand) GetMarketID() uint64  { return c.MarketID }
func (c *TickCommand) GetTimestamp() int64  { return c.Timestamp }

// HaltMarketCommand 熔断命令
type HaltMarketCommand struct {
	MarketID  uint64 `json:"market_id"`
	Timestamp int64  `json:"timestamp"`
}

func (c *HaltMarketCommand) GetType() CommandType { return CmdTypeHaltMarket }
func (c *HaltMarketCommand) GetMarketID() uint64  { return c.MarketID }
func (c *HaltMarketCommand) GetTimestamp() int64  { return c.Timestamp }

// CommandWrapper 用于绑定指令和底层的 NATS 消息 (为人质 ACK 准备)
type CommandWrapper struct {
	Cmd          Command
	Msg          *nats.Msg
	SourceCmdSeq uint64
}

// ==========================================
// 3. BatchEventPayload 事件体系 (输出)
// ==========================================

// FullOrderData 是 source order 的完整静态快照，仅用于首次入库和恢复。
type FullOrderData struct {
	OrderID            uint64 `json:"order_id"`
	WalletAddress      string `json:"wallet_address"`
	OriginalAction     uint8  `json:"original_action"`
	OriginalOutcome    uint8  `json:"original_outcome"`
	OriginalPriceTick  uint8  `json:"original_price_tick"`
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

// BatchEventPayload 批量事件载体，保证原子性发布
type BatchEventPayload struct {
	MarketID     uint64            `json:"market_id,string"`
	SourceCmdSeq uint64            `json:"source_cmd_seq"`
	Timestamp    int64             `json:"timestamp"`
	SourceOrder  *FullOrderData    `json:"source_order,omitempty"`
	TradeEvents  []TradeEvent      `json:"trade_events"`
	StateEvents  []OrderStateEvent `json:"state_events"`
	DepthEvents  []L2DepthEvent    `json:"depth_events"`
}

// AddTradeEvent 添加交易事件
func (b *BatchEventPayload) AddTradeEvent(maker, taker *MemoryOrder, price uint8, qty uint64) {
	b.TradeEvents = append(b.TradeEvents, TradeEvent{
		TradeID:        GenerateTradeID(),
		MatchPrice:     price,
		MatchQty:       qty,
		MakerOrderID:   maker.OrderID,
		MakerPubKey:    maker.WalletAddress,
		MakerSignature: maker.Signature,
		MakerIntentHex: maker.IntentBytesHex,
		TakerOrderID:   taker.OrderID,
		TakerPubKey:    taker.WalletAddress,
		TakerSignature: taker.Signature,
		TakerIntentHex: taker.IntentBytesHex,
	})
}

// AddStateEvent 添加订单状态事件
func (b *BatchEventPayload) AddStateEvent(orderID uint64, walletAddress string, status uint8, remainingQty uint64, refund uint64) {
	b.StateEvents = append(b.StateEvents, OrderStateEvent{
		OrderID:       orderID,
		WalletAddress: walletAddress,
		Status:        status,
		RemainingQty:  remainingQty,
		RefundAmount:  refund,
	})
}

// AddDepthEvent 添加深度事件
func (b *BatchEventPayload) AddDepthEvent(side uint8, price uint8, vol uint64) {
	b.DepthEvents = append(b.DepthEvents, L2DepthEvent{
		Side:        side,
		PriceTick:   price,
		TotalVolume: vol,
	})
}

// TradeEvent 交易执行事件
type TradeEvent struct {
	TradeID        string `json:"trade_id"`
	MatchPrice     uint8  `json:"match_price"`
	MatchQty       uint64 `json:"match_qty"`
	MakerOrderID   uint64 `json:"maker_order_id"`
	MakerPubKey    string `json:"maker_pubkey"`
	MakerSignature string `json:"maker_sig"`
	MakerIntentHex string `json:"maker_intent_hex"`
	TakerOrderID   uint64 `json:"taker_order_id"`
	TakerPubKey    string `json:"taker_pubkey"`
	TakerSignature string `json:"taker_sig"`
	TakerIntentHex string `json:"taker_intent_hex"`
}

// OrderStateEvent 订单状态变更事件
type OrderStateEvent struct {
	OrderID       uint64 `json:"order_id"`
	WalletAddress string `json:"wallet_address,omitempty"`
	Status        uint8  `json:"status"`
	RemainingQty  uint64 `json:"remaining_qty"`
	RefundAmount  uint64 `json:"refund_amount"`
}

// L2DepthEvent L2深度变更事件
type L2DepthEvent struct {
	Side        uint8  `json:"side"`
	PriceTick   uint8  `json:"price_tick"`
	TotalVolume uint64 `json:"total_volume"`
}
