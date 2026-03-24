package httpapi

import (
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/matching"
)

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

type marketsResponse struct {
	Markets []markets.Market `json:"markets"`
}

type marketResponse struct {
	Market markets.Market `json:"market"`
}

type openOrdersResponse struct {
	Orders          []matching.OpenOrder `json:"orders"`
	MatchingEnabled bool                 `json:"matching_enabled"`
}

type tradesResponse struct {
	Trades          []matching.Trade `json:"trades"`
	MatchingEnabled bool             `json:"matching_enabled"`
}

type placeOrderAcceptedResponse struct {
	Message        string `json:"message"`
	CommandID      string `json:"command_id"`
	MarketID       string `json:"market_id"`
	OrderID        string `json:"order_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type cancelOrderAcceptedResponse struct {
	Message   string `json:"message"`
	CommandID string `json:"command_id"`
	MarketID  string `json:"market_id"`
	OrderID   string `json:"order_id"`
}

type errorResponse struct {
	Message string `json:"message"`
}

type readyResponse struct {
	Writer            string `json:"writer"`
	Matcher           string `json:"matcher"`
	Pusher            string `json:"pusher"`
	Settlement        string `json:"settlement"`
	GatewayWriteReady bool   `json:"gateway_write_ready"`
}

type positionResponse struct {
	MarketID              string `json:"market_id"`
	WalletAddress         string `json:"wallet_address"`
	YesFreeLots           string `json:"yes_free_lots"`
	YesLockedLots         string `json:"yes_locked_lots"`
	NoFreeLots            string `json:"no_free_lots"`
	NoLockedLots          string `json:"no_locked_lots"`
	CollateralFreeUnits   string `json:"collateral_free_units"`
	CollateralLockedUnits string `json:"collateral_locked_units"`
}

type walletAccountResponse struct {
	WalletAddress        string `json:"wallet_address"`
	CollateralTotalUnits string `json:"collateral_total_units"`
	CollateralFreeUnits  string `json:"collateral_free_units"`
}
