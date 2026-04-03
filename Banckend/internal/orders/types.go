package orders

type DelegateRequest struct {
	MarketID uint64 `json:"market_id,string"`
	Side     string `json:"side"`
	Share    string `json:"share"`
	Amount   uint64 `json:"amount"`
	Decimals uint8  `json:"decimals"`
}

type TokenActionRequest struct {
	MarketID       uint64 `json:"market_id,string"`
	CollateralMint string `json:"collateral_mint"`
	Amount         uint64 `json:"amount"`
	Outcome        string `json:"outcome,omitempty"`
}

// PlaceOrderRequest 下单请求
// 根据订单系统全局规范 V1 (orderDesign.md)
type PlaceOrderRequest struct {
	Version       uint8  `json:"version"`
	ProgramID     string `json:"program_id"`
	Market        string `json:"market"`
	User          string `json:"user"`
	Side          string `json:"side"`
	Outcome       string `json:"outcome"`
	OrderType     string `json:"order_type"`
	LimitPrice    uint64 `json:"limit_price"`
	TotalAmount   uint64 `json:"total_amount"`
	Nonce         uint64 `json:"nonce,string"`
	ExpiryTs      int64  `json:"expiry_ts"`
	Signature     string `json:"signature"`

	MarketID          uint64 `json:"-"`
	WalletAddress     string `json:"-"`
	OriginalAction    string `json:"-"`
	OriginalOutcome   string `json:"-"`
	OriginalPriceTick uint8  `json:"-"`
	PriceTick         uint8  `json:"-"`
	QtyLots           uint64 `json:"-"`
	SpendAmount       uint64 `json:"-"`
	ExpireTime        int64  `json:"-"`
	NormalizedSide      string `json:"-"`
	NormalizedPriceTick uint64 `json:"-"`
}

type TransactionEnvelope struct {
	TxMessage string `json:"tx_message"`
	Message   string `json:"message"`
	Disabled  bool   `json:"disabled,omitempty"`
	Code      string `json:"code,omitempty"`
}
