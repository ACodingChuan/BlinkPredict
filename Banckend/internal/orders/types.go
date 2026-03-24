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
	MarketID          uint64 `json:"market_id,string"`    // 市场ID
	WalletAddress     string `json:"wallet_address"`      // 用户钱包地址 (base58)
	OriginalAction    string `json:"original_action"`     // 用户原始动作: buy | sell
	OriginalOutcome   string `json:"original_outcome"`    // 用户原始标的: yes | no
	OriginalPriceTick uint8  `json:"original_price_tick"` // 用户原始价格/滑点边界
	Side              string `json:"side"`                // 归一化后的 "buy" | "sell" (只面向 YES)
	OrderType         string `json:"order_type"`          // "limit" | "market"
	PriceTick         uint8  `json:"price_tick"`          // 归一化后的 1-99
	QtyLots           uint64 `json:"qty_lots"`            // 份额 (乘100后的整数，市价买入为0)
	SpendAmount       uint64 `json:"spend_amount"`        // 金额 (乘100后的整数，仅市价买入有值)
	ExpireTime        int64  `json:"expire_time"`         // Unix秒级时间戳 (0=GTC)
	Nonce             uint64 `json:"nonce,string"`        // 防碰撞nonce (42位时间戳+22位随机数)
	Signature         string `json:"signature"`           // base64 Ed25519签名 (对Keccak256(Borsh(Intent))签名)
}

type TransactionEnvelope struct {
	TxMessage string `json:"tx_message"`
	Message   string `json:"message"`
	Disabled  bool   `json:"disabled,omitempty"`
	Code      string `json:"code,omitempty"`
}
