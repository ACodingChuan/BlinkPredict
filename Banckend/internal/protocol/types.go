package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CommandTypePlaceOrder     = "cmd.order.place.v1"
	CommandTypeSubmitOrder    = "cmd.order.submit.v1"
	CommandTypeCancelOrder    = "cmd.order.cancel.v1"
	CommandTypeDepositConfirm = "cmd.tx.confirm.deposit.v1"
	CommandTypeMarketConfirm  = "cmd.tx.confirm.market_create.v1"
)

const (
	SubjectPlaceOrder         = "cmd.order.place"
	SubjectOrderSubmit        = "cmd.order.submit"
	SubjectCancelOrder        = "cmd.order.cancel"
	SubjectDepositConfirm     = "cmd.tx.confirm.deposit.v1"
	SubjectMarketConfirm      = "cmd.tx.confirm.market_create.v1"
	SubjectOrderReservedV1    = "evt.order.reserved.v1"
	SubjectOrderReserveReject = "evt.order.reserve_rejected.v1"
	SubjectSettlementConfirm  = "evt.settlement.confirmed.v1"
	SubjectDepositConfirmed   = "evt.deposit.confirmed.v1"
	SubjectDepositFailed      = "evt.deposit.failed.v1"
	SubjectMarketConfirmed    = "evt.market.confirmed.v1"
	SubjectMarketFailed       = "evt.market.failed.v1"
)

var ErrCommandBusDisabled = errors.New("command bus is not configured")

type Side string

type Outcome string

type OrderType string

type TimeInForce string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"

	OutcomeYes Outcome = "yes"
	OutcomeNo  Outcome = "no"

	OrderTypeLimit  OrderType = "limit"
	OrderTypeMarket OrderType = "market"

	TimeInForceGTC TimeInForce = "gtc"
	TimeInForceIOC TimeInForce = "ioc"
	TimeInForceFOK TimeInForce = "fok"
)

type CommandEnvelope[T any] struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	SchemaVersion  int       `json:"schema_version"`
	MarketID       uint64    `json:"market_id"`
	Producer       string    `json:"producer"`
	TraceID        string    `json:"trace_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
	Payload        T         `json:"payload"`
}

type PlaceOrderExecution struct {
	OrderID             uint64 `json:"order_id"`
	WalletAddress       string `json:"wallet_address"`
	OriginalAction      string `json:"original_action"`
	OriginalOutcome     string `json:"original_outcome"`
	OriginalPriceTick   uint8  `json:"original_price_tick"`
	OrderType           string `json:"order_type"`
	NormalizedSide      string `json:"normalized_side"`
	NormalizedPriceTick uint8  `json:"normalized_price_tick"`
	QtyLots             uint64 `json:"qty_lots"`
	SpendAmount         uint64 `json:"spend_amount"`
	ExpireTime          int64  `json:"expire_time"`
	Nonce               uint64 `json:"nonce"`
}

type SettlementPayload struct {
	IntentBytesHex string `json:"intent_bytes_hex"`
	Signature      string `json:"signature"`
}

type PlaceOrderCommand struct {
	CommandID      string              `json:"command_id"`
	TraceID        string              `json:"trace_id"`
	IdempotencyKey string              `json:"idempotency_key"`
	Timestamp      int64               `json:"timestamp"`
	MarketID       uint64              `json:"market_id"`
	MarketPDA      string              `json:"market_pda"`
	Execution      PlaceOrderExecution `json:"execution"`
	Settlement     SettlementPayload   `json:"settlement"`
}

type CancelOrderCommand struct {
	OrderID       string `json:"order_id"`
	WalletAddress string `json:"wallet_address"`
	Reason        string `json:"reason"`
}

type DepositConfirmCommand struct {
	Signature     string `json:"signature"`
	WalletAddress string `json:"wallet_address"`
	AmountUnits   uint64 `json:"amount_units"`
}

type DepositConfirmedEvent struct {
	Signature     string `json:"signature"`
	WalletAddress string `json:"wallet_address"`
	AmountUnits   uint64 `json:"amount_units"`
	Slot          uint64 `json:"slot"`
}

type DepositFailedEvent struct {
	Signature     string `json:"signature"`
	WalletAddress string `json:"wallet_address"`
	Reason        string `json:"reason"`
}

type MarketConfirmCommand struct {
	Signature string `json:"signature"`
}

type MarketConfirmedEvent struct {
	Signature           string `json:"signature"`
	Slot                uint64 `json:"slot"`
	MarketID            uint64 `json:"market_id"`
	MarketPDA           string `json:"market_pda"`
	Creator             string `json:"creator"`
	MetadataCID         string `json:"metadata_cid"`
	MetadataURL         string `json:"metadata_url"`
	Title               string `json:"title"`
	Description         string `json:"description"`
	Category            string `json:"category"`
	ImageURL            string `json:"image_url"`
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

type MarketFailedEvent struct {
	Signature string `json:"signature"`
	Reason    string `json:"reason"`
}

type OrderReserveRejectedEvent struct {
	CommandID      string `json:"command_id"`
	TraceID        string `json:"trace_id"`
	IdempotencyKey string `json:"idempotency_key"`
	MarketID       uint64 `json:"market_id"`
	MarketPDA      string `json:"market_pda"`
	OrderID        uint64 `json:"order_id"`
	WalletAddress  string `json:"wallet_address"`
	ReasonCode     string `json:"reason_code"`
	ReasonMessage  string `json:"reason_message"`
	CreatedAt      int64  `json:"created_at"`
}

type SettlementConfirmedEvent struct {
	EventID               string   `json:"event_id"`
	SchemaVersion         int      `json:"schema_version"`
	MarketID              uint64   `json:"market_id"`
	MarketPDA             string   `json:"market_pda"`
	SettlementTxSignature string   `json:"settlement_tx_signature"`
	Wallets               []string `json:"wallets"`
	ConfirmedAt           int64    `json:"confirmed_at"`
}

type CommandPublisher interface {
	PublishSubmitOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error
	PublishPlaceOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error
	PublishCancelOrder(context.Context, CommandEnvelope[CancelOrderCommand]) error
	PublishDepositConfirm(context.Context, DepositConfirmCommand) error
	PublishMarketConfirm(context.Context, MarketConfirmCommand) error
}

type DisabledCommandPublisher struct{}

func (DisabledCommandPublisher) PublishSubmitOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error {
	return ErrCommandBusDisabled
}

func (DisabledCommandPublisher) PublishPlaceOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error {
	return ErrCommandBusDisabled
}

func (DisabledCommandPublisher) PublishCancelOrder(context.Context, CommandEnvelope[CancelOrderCommand]) error {
	return ErrCommandBusDisabled
}

func (DisabledCommandPublisher) PublishDepositConfirm(context.Context, DepositConfirmCommand) error {
	return ErrCommandBusDisabled
}

func (DisabledCommandPublisher) PublishMarketConfirm(context.Context, MarketConfirmCommand) error {
	return ErrCommandBusDisabled
}

func SubjectOrderReservedMarket(marketID uint64) string {
	return fmt.Sprintf("%s.%d", SubjectOrderReservedV1, marketID)
}

func SubjectOrderReserveRejectedMarket(marketID uint64) string {
	return fmt.Sprintf("%s.%d", SubjectOrderReserveReject, marketID)
}

func SubjectSettlementConfirmedMarket(marketID uint64) string {
	return fmt.Sprintf("%s.%d", SubjectSettlementConfirm, marketID)
}

func ValidatePlaceOrderCommand(cmd PlaceOrderCommand) error {
	if strings.TrimSpace(cmd.CommandID) == "" {
		return errors.New("command_id is required")
	}
	if cmd.MarketID == 0 {
		return errors.New("market_id is required")
	}
	if strings.TrimSpace(cmd.MarketPDA) == "" {
		return errors.New("market_pda is required")
	}
	if strings.TrimSpace(cmd.Execution.WalletAddress) == "" {
		return errors.New("wallet_address is required")
	}
	switch Side(cmd.Execution.OriginalAction) {
	case SideBuy, SideSell:
	default:
		return fmt.Errorf("invalid original_action: %s", cmd.Execution.OriginalAction)
	}
	switch Outcome(cmd.Execution.OriginalOutcome) {
	case OutcomeYes, OutcomeNo:
	default:
		return fmt.Errorf("invalid original_outcome: %s", cmd.Execution.OriginalOutcome)
	}
	if strings.TrimSpace(cmd.Settlement.Signature) == "" {
		return errors.New("signature is required")
	}
	if cmd.Execution.Nonce == 0 {
		return errors.New("nonce is required")
	}
	if strings.TrimSpace(cmd.Settlement.IntentBytesHex) == "" {
		return errors.New("intent_bytes_hex is required")
	}
	if cmd.Timestamp == 0 {
		return errors.New("timestamp is required")
	}

	switch Side(cmd.Execution.NormalizedSide) {
	case SideBuy, SideSell:
	default:
		return fmt.Errorf("invalid normalized_side: %s", cmd.Execution.NormalizedSide)
	}
	if cmd.Execution.OriginalPriceTick < 1 || cmd.Execution.OriginalPriceTick > 99 {
		return errors.New("original_price_tick must be between 1 and 99")
	}

	switch OrderType(cmd.Execution.OrderType) {
	case OrderTypeLimit:
		if cmd.Execution.NormalizedPriceTick < 1 || cmd.Execution.NormalizedPriceTick > 99 {
			return errors.New("normalized_price_tick must be between 1 and 99 for limit order")
		}
		if cmd.Execution.ExpireTime == 0 {
			return errors.New("expire_time is required for limit order")
		}
		if cmd.Execution.QtyLots == 0 {
			return errors.New("qty_lots must be greater than 0 for limit order")
		}
		if cmd.Execution.SpendAmount != 0 {
			return errors.New("spend_amount must be 0 for limit order")
		}
	case OrderTypeMarket:
		if cmd.Execution.NormalizedPriceTick < 1 || cmd.Execution.NormalizedPriceTick > 99 {
			return errors.New("normalized_price_tick must be between 1 and 99 for market order")
		}
		if Side(cmd.Execution.OriginalAction) == SideBuy {
			if cmd.Execution.SpendAmount == 0 {
				return errors.New("spend_amount must be greater than 0 for market buy order")
			}
			if cmd.Execution.QtyLots != 0 {
				return errors.New("qty_lots must be 0 for market buy order")
			}
		} else {
			if cmd.Execution.QtyLots == 0 {
				return errors.New("qty_lots must be greater than 0 for market sell order")
			}
			if cmd.Execution.SpendAmount != 0 {
				return errors.New("spend_amount must be 0 for market sell order")
			}
		}
		if cmd.Execution.ExpireTime != 0 {
			return errors.New("expire_time must be 0 for market order")
		}
	default:
		return fmt.Errorf("invalid order_type: %s", cmd.Execution.OrderType)
	}

	return nil
}
