package webhooks

import (
	"time"
)

// HeliusWebhookPayload Helius 发送的完整 payload
type HeliusWebhookPayload struct {
	Type        string             `json:"type"`      // "TRANSACTION", "ACCOUNT", etc.
	Timestamp   int64              `json:"timestamp"` // Unix timestamp
	Slot        uint64             `json:"slot"`
	Transaction *HeliusTransaction `json:"transaction"` // 交易数据
	Account     *HeliusAccount     `json:"account"`     // 账户数据（如果是 account 类型）
}

// HeliusTransaction 交易信息
type HeliusTransaction struct {
	Signature       string                 `json:"signature"` // 交易签名
	Type            string                 `json:"type"`      // "CREATE", "INVOKE", etc.
	Timestamp       int64                  `json:"timestamp"` // 交易时间
	Slot            uint64                 `json:"slot"`
	AccountData     []HeliusAccountData    `json:"accountData"`     // 涉及的账户数据
	Events          interface{}            `json:"events"`          // 🎯 可能是数组或空对象
	TokenTransfers  []HeliusTokenTransfer  `json:"tokenTransfers"`  // Token 转账（如果有）
	NativeTransfers []HeliusNativeTransfer `json:"nativeTransfers"` // SOL 转账（如果有）
	Fee             int64                  `json:"fee"`             // 交易费用
	Status          string                 `json:"status"`          // 交易状态
	Err             interface{}            `json:"err"`             // 错误信息（如果有）
	Instructions    []HeliusInstruction    `json:"instructions"`    // 指令列表
}

// HeliusEvent 事件数据（最重要！）
type HeliusEvent struct {
	Name string                 `json:"name"` // 🎯 事件名称，如 "MarketInitialized"
	Data map[string]interface{} `json:"data"` // 🎯 事件数据（键值对，需要解析）
}

// HeliusAccountData 账户数据
type HeliusAccountData struct {
	Account             string `json:"account"`
	NativeBalanceChange int64  `json:"nativeBalanceChange"`
	TokenBalanceChanges []struct {
		Mint   string `json:"mint"`
		Amount int64  `json:"amount"`
	} `json:"tokenBalanceChanges"`
}

// HeliusAccount 账户信息（webhook type 为 account 时）
type HeliusAccount struct {
	Account  string `json:"account"`
	Lamports int64  `json:"lamports"`
}

// HeliusTokenTransfer Token 转账信息
type HeliusTokenTransfer struct {
	FromTokenAccount string `json:"fromTokenAccount"`
	ToTokenAccount   string `json:"toTokenAccount"`
	FromUserAccount  string `json:"fromUserAccount"`
	ToUserAccount    string `json:"toUserAccount"`
	Mint             string `json:"mint"`
	Amount           int64  `json:"amount"`
}

// HeliusNativeTransfer SOL 转账信息
type HeliusNativeTransfer struct {
	FromUser string `json:"fromUser"`
	ToUser   string `json:"toUser"`
	Amount   int64  `json:"amount"`
}

// HeliusInstruction 指令信息
type HeliusInstruction struct {
	ProgramID         string              `json:"programId"`
	Accounts          []string            `json:"accounts"`
	Data              string              `json:"data"`
	InnerInstructions []HeliusInstruction `json:"innerInstructions"`
}

// MarketCreatedEvent MarketCreated 事件（市场创建后的链上事件）
type MarketCreatedEvent struct {
	MarketID             uint64 `json:"market_id"`
	MarketPDA            string `json:"market_pda"`
	Authority            string `json:"authority"`
	MetadataCID          string `json:"metadata_cid"`
	MetadataURI          string `json:"metadata_uri"`
	CollateralMint       string `json:"collateral_mint"`
	CollateralVault      string `json:"collateral_vault"`
	YesMint              string `json:"yes_mint"`
	NoMint               string `json:"no_mint"`
	CloseTime            int64  `json:"close_ts"`
	ResolveAfterTS       int64  `json:"resolve_after_ts"`
	ClaimDeadlineTS      int64  `json:"claim_deadline_ts"`
	ResolutionMode       string `json:"resolution_mode"`
	OracleFeedID         string `json:"oracle_feed_id"`
	OracleCondition      string `json:"oracle_condition"`
	OracleTargetPriceInt uint64 `json:"oracle_target_price_int"`
	OracleTargetExpo     int32  `json:"oracle_target_expo"`
}

type MarketInitializedEvent = MarketCreatedEvent

// IPFSMetadata IPFS 中的市场元数据
type IPFSMetadata struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	ImageURL    string `json:"image_url"`
}

// WebhookProcessingResult webhook 处理结果
type WebhookProcessingResult struct {
	Success     bool      `json:"success"`
	ProcessedAt time.Time `json:"processed_at"`
	MarketID    uint64    `json:"market_id"`
	Message     string    `json:"message"`
	Error       string    `json:"error,omitempty"`
}
