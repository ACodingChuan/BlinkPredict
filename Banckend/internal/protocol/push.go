package protocol

import (
	"fmt"
)

func SubjectPushMarketDepth(marketID uint64) string {
	return fmt.Sprintf("push.market.%d.depth", marketID)
}

func SubjectPushMarketTrade(marketID uint64) string {
	return fmt.Sprintf("push.market.%d.trade", marketID)
}

func SubjectPushUserOrder(walletAddress string) string {
	return fmt.Sprintf("push.user.%s.order", walletAddress)
}

type MarketDepthLevel struct {
	Side        uint8  `json:"side"`
	PriceTick   uint8  `json:"price_tick"`
	TotalVolume uint64 `json:"total_volume"`
}

type MarketDepthPush struct {
	MarketID     string             `json:"market_id"`
	UpdatedAt    string             `json:"updated_at"`
	SourceCmdSeq string             `json:"source_cmd_seq,omitempty"`
	Levels       []MarketDepthLevel `json:"levels"`
}

type MarketTradePush struct {
	MarketID           string `json:"market_id"`
	TradeID            string `json:"trade_id"`
	MakerOrderID       string `json:"maker_order_id"`
	TakerOrderID       string `json:"taker_order_id"`
	MakerWalletAddress string `json:"maker_wallet_address"`
	TakerWalletAddress string `json:"taker_wallet_address"`
	PriceTick          string `json:"price_tick"`
	MatchQty           string `json:"match_qty"`
	ExecutedAt         string `json:"executed_at"`
}

type UserOrderPatch struct {
	ID           string `json:"id"`
	Side         string `json:"side,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	Price        string `json:"price,omitempty"`
	Quantity     string `json:"quantity,omitempty"`
	Status       uint8  `json:"status"`
	RefundAmount string `json:"refund_amount,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

type UserOrderPush struct {
	MarketID      string         `json:"market_id"`
	WalletAddress string         `json:"wallet_address"`
	Order         UserOrderPatch `json:"order"`
}

const (
	WSTypeMarketDepthDelta    = "market.depth.delta"
	WSTypeMarketTradeExecuted = "market.trade.executed"
	WSTypeUserOrderUpdated    = "user.order.updated"
)

type WSMarketDepthDelta struct {
	Type     string               `json:"type"`
	MarketID string               `json:"market_id"`
	Ts       string               `json:"ts"`
	Payload  WSMarketDepthPayload `json:"payload"`
}

type WSMarketDepthPayload struct {
	Levels []WSDepthLevel `json:"levels"`
}

type WSDepthLevel struct {
	Side        string `json:"side"`
	PriceTick   uint8  `json:"price_tick"`
	TotalVolume uint64 `json:"total_volume"`
}

type WSMarketTradeExecuted struct {
	Type     string               `json:"type"`
	MarketID string               `json:"market_id"`
	Ts       string               `json:"ts"`
	Payload  WSMarketTradePayload `json:"payload"`
}

type WSMarketTradePayload struct {
	TradeID            string `json:"trade_id"`
	MakerOrderID       string `json:"maker_order_id"`
	TakerOrderID       string `json:"taker_order_id"`
	MakerWalletAddress string `json:"maker_wallet_address,omitempty"`
	TakerWalletAddress string `json:"taker_wallet_address,omitempty"`
	PriceTick          string `json:"price_tick"`
	FillAmount         string `json:"fill_amount"`
	MatchType          string `json:"match_type"`
	ExecutedAt         string `json:"executed_at"`
}

type WSUserOrderUpdated struct {
	Type     string             `json:"type"`
	MarketID string             `json:"market_id"`
	Ts       string             `json:"ts"`
	Payload  WSUserOrderPayload `json:"payload"`
}

type WSUserOrderPayload struct {
	OrderID              string `json:"order_id"`
	Status               string `json:"status"`
	RemainingQtyLots     string `json:"remaining_qty_lots"`
	RemainingSpendAmount string `json:"remaining_spend_amount"`
	RefundAmount         string `json:"refund_amount"`
	UpdatedAt            string `json:"updated_at"`
	OriginalAction       string `json:"original_action,omitempty"`
	OriginalOutcome      string `json:"original_outcome,omitempty"`
	OriginalPriceTick    string `json:"original_price_tick,omitempty"`
}
