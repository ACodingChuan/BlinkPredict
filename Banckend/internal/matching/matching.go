package matching

import "context"

const ErrMatchingDisabled = "matching_not_enabled"

type Engine interface {
	PlaceOrder(context.Context, any) error
	CancelOrder(context.Context, string) error
	GetOrderbook(context.Context, uint64) OrderbookSnapshot
	GetTrades(context.Context, uint64) []Trade
	GetOpenOrders(context.Context, string, uint64) []OpenOrder
}

type Repository interface{}

type OrderLevel struct {
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
}

type BookSide struct {
	Bids []OrderLevel `json:"bids"`
	Asks []OrderLevel `json:"asks"`
}

type OrderbookSnapshot struct {
	Yes             BookSide `json:"yes"`
	No              BookSide `json:"no"`
	MatchingEnabled bool     `json:"matching_enabled"`
}

type Trade struct {
	ID string `json:"id"`
}

type OpenOrder struct {
	ID string `json:"id"`
}

type DisabledEngine struct{}

func NewDisabledEngine() DisabledEngine { return DisabledEngine{} }

func (DisabledEngine) PlaceOrder(context.Context, any) error     { return &DisabledError{} }
func (DisabledEngine) CancelOrder(context.Context, string) error { return &DisabledError{} }
func (DisabledEngine) GetOrderbook(context.Context, uint64) OrderbookSnapshot {
	return OrderbookSnapshot{Yes: BookSide{Bids: []OrderLevel{}, Asks: []OrderLevel{}}, No: BookSide{Bids: []OrderLevel{}, Asks: []OrderLevel{}}, MatchingEnabled: false}
}
func (DisabledEngine) GetTrades(context.Context, uint64) []Trade { return []Trade{} }
func (DisabledEngine) GetOpenOrders(context.Context, string, uint64) []OpenOrder {
	return []OpenOrder{}
}

type DisabledError struct{}

func (e *DisabledError) Error() string { return ErrMatchingDisabled }
