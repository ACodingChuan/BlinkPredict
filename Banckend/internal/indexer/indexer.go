package indexer

import "context"

type Event struct {
	Kind     string `json:"kind"`
	MarketID uint64 `json:"market_id"`
}

type Listener interface {
	Start(context.Context) error
	Stop(context.Context) error
}
