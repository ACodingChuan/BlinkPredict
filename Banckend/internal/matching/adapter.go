package matching

import (
	"blinkpredict/banckend/internal/protocol"
)

// ConvertProtocolToPlaceOrderCommand 将protocol.PlaceOrderCommand转换为matching.PlaceOrderCommand
func ConvertProtocolToPlaceOrderCommand(cmd protocol.PlaceOrderCommand) *PlaceOrderCommand {
	side := uint8(SideBuy)
	if cmd.Side == protocol.SideSell {
		side = SideSell
	}

	orderType := uint8(OrderTypeLimit)
	if cmd.OrderType == protocol.OrderTypeMarket {
		orderType = OrderTypeMarket
	}

	return &PlaceOrderCommand{
		OrderID:           cmd.OrderID,
		MarketID:          cmd.MarketID,
		WalletAddress:     cmd.WalletAddress,
		OriginalAction:    toMatchingSide(cmd.OriginalAction),
		OriginalOutcome:   toMatchingOutcome(cmd.OriginalOutcome),
		OriginalPriceTick: cmd.OriginalPriceTick,
		Side:              side,
		OrderType:         orderType,
		PriceTick:         cmd.PriceTick,
		QtyLots:           cmd.QtyLots,
		SpendAmount:       cmd.SpendAmount,
		ExpireTime:        cmd.ExpireTime,
		Signature:         cmd.Signature,
		IntentBytesHex:    cmd.IntentBytesHex,
		Nonce:             cmd.Nonce,
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
