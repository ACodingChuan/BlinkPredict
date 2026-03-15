package orders

type DelegateRequest struct {
	MarketID uint64 `json:"market_id"`
	Side     string `json:"side"`
	Share    string `json:"share"`
	Amount   uint64 `json:"amount"`
	Decimals uint8  `json:"decimals"`
}

type TokenActionRequest struct {
	MarketID       uint64 `json:"market_id"`
	CollateralMint string `json:"collateral_mint"`
	Amount         uint64 `json:"amount"`
}

type PlaceOrderRequest struct {
	MarketID       uint64  `json:"market_id"`
	CollateralMint string  `json:"collateral_mint"`
	Side           string  `json:"side"`
	Share          string  `json:"share"`
	Price          float64 `json:"price"`
	Quantity       float64 `json:"qty"`
}

type TransactionEnvelope struct {
	TxMessage string `json:"tx_message"`
	Message   string `json:"message"`
	Disabled  bool   `json:"disabled,omitempty"`
	Code      string `json:"code,omitempty"`
}
