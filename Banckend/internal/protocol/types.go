package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CommandTypePlaceOrder  = "cmd.order.place.v1"
	CommandTypeCancelOrder = "cmd.order.cancel.v1"
)

const (
	SubjectPlaceOrder  = "cmd.order.place"
	SubjectCancelOrder = "cmd.order.cancel"
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

// PlaceOrderCommand 下单命令 (投入 NATS 的 Payload)
// 根据订单系统全局规范 V1 (orderDesign.md)
type PlaceOrderCommand struct {
	OrderID           uint64    `json:"order_id"`            // 雪花ID (单调递增)
	MarketID          uint64    `json:"market_id"`           // 市场ID
	WalletAddress     string    `json:"wallet_address"`      // 用户钱包地址 (base58)
	OriginalAction    Side      `json:"original_action"`     // 用户原始动作
	OriginalOutcome   Outcome   `json:"original_outcome"`    // 用户原始标的
	OriginalPriceTick uint8     `json:"original_price_tick"` // 用户原始价格/滑点边界
	Side              Side      `json:"side"`                // "buy" | "sell" (已归一化为 Yes)
	OrderType         OrderType `json:"order_type"`          // "limit" | "market"
	PriceTick         uint8     `json:"price_tick"`          // 1-99 (归一化后的挂单价或滑点底线)
	QtyLots           uint64    `json:"qty_lots"`            // 份额 (乘100后的整数，市价买入为0)
	SpendAmount       uint64    `json:"spend_amount"`        // 金额 (乘100后的整数，仅市价买入有值)
	ExpireTime        int64     `json:"expire_time"`         // Unix秒级时间戳 (0=GTC)
	Signature         string    `json:"signature"`           // base64 Ed25519签名
	Nonce             uint64    `json:"nonce"`               // 防碰撞nonce
	IntentBytesHex    string    `json:"intent_bytes_hex"`    // Borsh编码的Hex (链上结算用)
	Timestamp         int64     `json:"timestamp"`           // 逻辑时钟 (Unix秒级)
}

type CancelOrderCommand struct {
	OrderID       string `json:"order_id"`
	WalletAddress string `json:"wallet_address"`
	Reason        string `json:"reason"`
}

type CommandPublisher interface {
	PublishPlaceOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error
	PublishCancelOrder(context.Context, CommandEnvelope[CancelOrderCommand]) error
}

type DisabledCommandPublisher struct{}

func (DisabledCommandPublisher) PublishPlaceOrder(context.Context, CommandEnvelope[PlaceOrderCommand]) error {
	return ErrCommandBusDisabled
}

func (DisabledCommandPublisher) PublishCancelOrder(context.Context, CommandEnvelope[CancelOrderCommand]) error {
	return ErrCommandBusDisabled
}

func ValidatePlaceOrderCommand(cmd PlaceOrderCommand) error {
	// 基础字段验证
	if cmd.OrderID == 0 {
		return errors.New("order_id is required")
	}
	if cmd.MarketID == 0 {
		return errors.New("market_id is required")
	}
	if strings.TrimSpace(cmd.WalletAddress) == "" {
		return errors.New("wallet_address is required")
	}
	switch cmd.OriginalAction {
	case SideBuy, SideSell:
	default:
		return fmt.Errorf("invalid original_action: %s", cmd.OriginalAction)
	}
	switch cmd.OriginalOutcome {
	case OutcomeYes, OutcomeNo:
	default:
		return fmt.Errorf("invalid original_outcome: %s", cmd.OriginalOutcome)
	}
	if strings.TrimSpace(cmd.Signature) == "" {
		return errors.New("signature is required")
	}
	if cmd.Nonce == 0 {
		return errors.New("nonce is required")
	}
	if strings.TrimSpace(cmd.IntentBytesHex) == "" {
		return errors.New("intent_bytes_hex is required")
	}
	if cmd.Timestamp == 0 {
		return errors.New("timestamp is required")
	}

	// Side 验证
	switch cmd.Side {
	case SideBuy, SideSell:
	default:
		return fmt.Errorf("invalid side: %s", cmd.Side)
	}
	if cmd.OriginalPriceTick < 1 || cmd.OriginalPriceTick > 99 {
		return errors.New("original_price_tick must be between 1 and 99")
	}

	// OrderType 验证
	switch cmd.OrderType {
	case OrderTypeLimit:
		// 限价单验证
		if cmd.PriceTick < 1 || cmd.PriceTick > 99 {
			return errors.New("price_tick must be between 1 and 99 for limit order")
		}
		if cmd.ExpireTime == 0 {
			return errors.New("expire_time is required for limit order")
		}
		if cmd.QtyLots == 0 {
			return errors.New("qty_lots must be greater than 0 for limit order")
		}
		if cmd.SpendAmount != 0 {
			return errors.New("spend_amount must be 0 for limit order")
		}
	case OrderTypeMarket:
		// 市价单验证
		if cmd.PriceTick < 1 || cmd.PriceTick > 99 {
			return errors.New("price_tick must be between 1 and 99 for market order (slippage limit)")
		}
		// 市价单两种模式：买入用 spendAmount，卖出用 qtyLots
		if cmd.Side == SideBuy {
			// 市价买入：spendAmount > 0, qtyLots = 0
			if cmd.SpendAmount == 0 {
				return errors.New("spend_amount must be greater than 0 for market buy order")
			}
			if cmd.QtyLots != 0 {
				return errors.New("qty_lots must be 0 for market buy order")
			}
		} else {
			// 市价卖出：qtyLots > 0, spendAmount = 0
			if cmd.QtyLots == 0 {
				return errors.New("qty_lots must be greater than 0 for market sell order")
			}
			if cmd.SpendAmount != 0 {
				return errors.New("spend_amount must be 0 for market sell order")
			}
		}
		if cmd.ExpireTime != 0 {
			return errors.New("expire_time must be 0 for market order")
		}
	default:
		return fmt.Errorf("invalid order_type: %s", cmd.OrderType)
	}

	return nil
}
