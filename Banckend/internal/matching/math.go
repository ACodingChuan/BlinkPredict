package matching

func ceilMulDiv(a, b, div uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	product := a * b
	return (product + div - 1) / div
}

func actualUnitsForOrder(order *MemoryOrder, qty uint64, normalizedPrice uint8) uint64 {
	if order.OriginalOutcome == 1 {
		return ceilMulDiv(qty, uint64(100-normalizedPrice), 100)
	}
	return ceilMulDiv(qty, uint64(normalizedPrice), 100)
}
