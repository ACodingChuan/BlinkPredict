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
