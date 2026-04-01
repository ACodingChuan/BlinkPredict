package webhooks

import (
	"encoding/json"
	"time"
)

const (
	ProviderAlchemy = "alchemy"
	ProviderHelius  = "helius"

	EventTypeMarketCreated           = "webhook.market.created.v1"
	EventTypeUserPositionInitialized = "webhook.user_position.initialized.v1"
	EventTypeMatchSettled            = "webhook.match.settled.v1"
	EventTypeOrderStateClosed        = "webhook.order_state.closed.v1"
	EventTypeUserPositionClosed      = "webhook.user_position.closed.v1"
	EventTypeSettlementBatchAccepted = "webhook.settlement_batch.accepted.v1"
	EventTypeDepositSettled          = "webhook.deposit.settled.v1"

	SubjectAlchemyMarketCreated           = "whk.alchemy.market.created"
	SubjectAlchemyUserPositionInitialized = "whk.alchemy.user_position.initialized"
	SubjectAlchemyMatchSettled            = "whk.alchemy.match.settled"
	SubjectAlchemyOrderStateClosed        = "whk.alchemy.order_state.closed"
	SubjectAlchemyUserPositionClosed      = "whk.alchemy.user_position.closed"
)

type WebhookEventEnvelope struct {
	EventID         string          `json:"event_id"`
	Provider        string          `json:"provider"`
	ProviderEventID string          `json:"provider_event_id,omitempty"`
	Signature       string          `json:"signature,omitempty"`
	Slot            uint64          `json:"slot,omitempty"`
	BlockTime       int64           `json:"block_time,omitempty"`
	EventType       string          `json:"event_type"`
	SchemaVersion   int             `json:"schema_version"`
	ProducedAt      time.Time       `json:"produced_at"`
	Payload         json.RawMessage `json:"payload"`
}

type MarketCreatedPayload struct {
	MarketID            uint64 `json:"market_id,string"`
	MarketPDA           string `json:"market_pda"`
	Creator             string `json:"creator"`
	MetadataCID         string `json:"metadata_cid"`
	MetadataURL         string `json:"metadata_url,omitempty"`
	ResolutionMode      string `json:"resolution_mode"`
	ResolutionAuthority string `json:"resolution_authority,omitempty"`
	OracleFeed          string `json:"oracle_feed,omitempty"`
	OracleCondition     string `json:"oracle_condition,omitempty"`
	OracleTargetPrice   uint64 `json:"oracle_target_price"`
	OracleTargetExpo    int32  `json:"oracle_target_expo"`
	CloseTS             int64  `json:"close_ts"`
	ResolveAfterTS      int64  `json:"resolve_after_ts"`
	ClaimDeadlineTS     int64  `json:"claim_deadline_ts"`
}

type UserPositionInitializedPayload struct {
	WalletAddress   string `json:"wallet_address"`
	MarketPDA       string `json:"market_pda"`
	UserPositionPDA string `json:"user_position_pda"`
	Payer           string `json:"payer,omitempty"`
}

type MatchSettledPayload struct {
	MarketPDA     string `json:"market_pda"`
	Branch        string `json:"branch"`
	MakerWallet   string `json:"maker_wallet"`
	TakerWallet   string `json:"taker_wallet"`
	FillAmount    uint64 `json:"fill_amount"`
	FillPrice     uint64 `json:"fill_price"`
	SettlementSeq uint32 `json:"settlement_seq"`
}

type OrderStateClosedPayload struct {
	WalletAddress string `json:"wallet_address"`
	MarketPDA     string `json:"market_pda"`
	Nonce         uint64 `json:"nonce"`
}

type UserPositionClosedPayload struct {
	WalletAddress string `json:"wallet_address"`
	MarketPDA     string `json:"market_pda"`
}

type DepositSettledPayload struct {
	WalletAddress    string `json:"wallet_address"`
	Mint             string `json:"mint"`
	AmountUnits      uint64 `json:"amount_units"`
	Signature        string `json:"signature"`
	Slot             uint64 `json:"slot"`
	BlockTime        int64  `json:"block_time"`
	FromTokenAccount string `json:"from_token_account,omitempty"`
	ToTokenAccount   string `json:"to_token_account,omitempty"`
}

type alchemyClassifiedEvent struct {
	Subject   string
	EventType string
	Payload   any
}
