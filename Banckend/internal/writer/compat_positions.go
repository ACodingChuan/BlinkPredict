package writer

import "blinkpredict/banckend/internal/matching"

type positionDelta struct {
	MarketID              uint64
	WalletAddress         string
	YesFreeLotsDelta      int64
	YesLockedLotsDelta    int64
	NoFreeLotsDelta       int64
	NoLockedLotsDelta     int64
	CollateralFreeDelta   int64
	CollateralLockedDelta int64
}

type orderMeta struct {
	OrderID           uint64
	WalletAddress     string
	OriginalAction    string
	OriginalOutcome   string
	OriginalPriceTick uint8
	OrderType         uint8
}

func initialLockDelta(marketID uint64, meta orderMeta, initialQty uint64, initialSpend uint64) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		required := int64(initialSpend)
		if meta.OrderType == matching.OrderTypeLimit {
			required = int64(reservedUnitsForLots(initialQty, meta.OriginalPriceTick))
		}
		delta.CollateralFreeDelta -= required
		delta.CollateralLockedDelta += required
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesFreeLotsDelta -= int64(initialQty)
			delta.YesLockedLotsDelta += int64(initialQty)
		} else {
			delta.NoFreeLotsDelta -= int64(initialQty)
			delta.NoLockedLotsDelta += int64(initialQty)
		}
	}
	return delta
}

func settlementDelta(marketID uint64, meta orderMeta, qty uint64, normalizedMatchPrice uint8) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	actualUnits := int64(actualCollateralForTrade(meta, qty, normalizedMatchPrice))
	switch meta.OriginalAction {
	case "buy":
		if meta.OriginalOutcome == "yes" {
			delta.YesFreeLotsDelta += int64(qty)
		} else {
			delta.NoFreeLotsDelta += int64(qty)
		}
		if meta.OrderType == matching.OrderTypeLimit {
			reserved := int64(reservedUnitsForLots(qty, meta.OriginalPriceTick))
			delta.CollateralLockedDelta -= reserved
			delta.CollateralFreeDelta += reserved - actualUnits
		} else {
			delta.CollateralLockedDelta -= actualUnits
		}
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesLockedLotsDelta -= int64(qty)
		} else {
			delta.NoLockedLotsDelta -= int64(qty)
		}
		delta.CollateralFreeDelta += actualUnits
	}
	return delta
}

func unlockDelta(marketID uint64, meta orderMeta, remainingQty uint64, refundAmount uint64) positionDelta {
	delta := positionDelta{MarketID: marketID, WalletAddress: meta.WalletAddress}
	switch meta.OriginalAction {
	case "buy":
		unlock := refundAmount
		if meta.OrderType == matching.OrderTypeLimit {
			unlock = reservedUnitsForLots(remainingQty, meta.OriginalPriceTick)
		}
		delta.CollateralLockedDelta -= int64(unlock)
		delta.CollateralFreeDelta += int64(unlock)
	case "sell":
		if meta.OriginalOutcome == "yes" {
			delta.YesLockedLotsDelta -= int64(remainingQty)
			delta.YesFreeLotsDelta += int64(remainingQty)
		} else {
			delta.NoLockedLotsDelta -= int64(remainingQty)
			delta.NoFreeLotsDelta += int64(remainingQty)
		}
	}
	return delta
}

func actualCollateralForTrade(meta orderMeta, qty uint64, normalizedMatchPrice uint8) uint64 {
	if meta.OriginalOutcome == "no" {
		return reservedUnitsForLots(qty, uint8(100-normalizedMatchPrice))
	}
	return reservedUnitsForLots(qty, normalizedMatchPrice)
}

func reservedUnitsForLots(qtyLots uint64, priceTick uint8) uint64 {
	return (qtyLots * uint64(priceTick)) / 100
}
