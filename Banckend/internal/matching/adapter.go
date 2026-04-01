package matching

import (
	"blinkpredict/banckend/internal/protocol"
)

// ConvertProtocolToPlaceOrderCommand 将protocol.PlaceOrderCommand转换为matching.PlaceOrderCommand
func ConvertProtocolToPlaceOrderCommand(cmd protocol.PlaceOrderCommand) *PlaceOrderCommand {
	side := uint8(SideBuy)
	if protocol.Side(cmd.Execution.NormalizedSide) == protocol.SideSell {
		side = SideSell
	}

	orderType := uint8(OrderTypeLimit)
	if protocol.OrderType(cmd.Execution.OrderType) == protocol.OrderTypeMarket {
		orderType = OrderTypeMarket
	}

	return &PlaceOrderCommand{
		CommandID:         cmd.CommandID,
		TraceID:           cmd.TraceID,
		IdempotencyKey:    cmd.IdempotencyKey,
		OrderID:           cmd.Execution.OrderID,
		MarketID:          cmd.MarketID,
		MarketPDA:         cmd.MarketPDA,
		WalletAddress:     cmd.Execution.WalletAddress,
		OriginalAction:    toMatchingSide(protocol.Side(cmd.Execution.OriginalAction)),
		OriginalOutcome:   toMatchingOutcome(protocol.Outcome(cmd.Execution.OriginalOutcome)),
		OriginalPriceTick: cmd.Execution.OriginalPriceTick,
		Side:              side,
		OrderType:         orderType,
		PriceTick:         cmd.Execution.NormalizedPriceTick,
		QtyLots:           cmd.Execution.QtyLots,
		SpendAmount:       cmd.Execution.SpendAmount,
		ExpireTime:        cmd.Execution.ExpireTime,
		Signature:         cmd.Settlement.Signature,
		IntentBytesHex:    cmd.Settlement.IntentBytesHex,
		Nonce:             cmd.Execution.Nonce,
		Timestamp:         cmd.Timestamp,
	}
}

func toMatchingSide(side protocol.Side) uint8 {
	if side == protocol.SideSell {
		return SideSell
	}
	return SideBuy
}

func toMatchingOutcome(outcome protocol.Outcome) uint8 {
	if outcome == protocol.OutcomeNo {
		return 1
	}
	return 0
}
