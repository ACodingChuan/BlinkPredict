package matching

import "time"

// MarketActor 是单个市场的并发壳（Actor模型）
type MarketActor struct {
	MarketID  uint64
	MarketPDA string
	CmdChan   chan *CommandWrapper
	Book      *FixedArrayOrderBook
	Pending   *pendingBatch
	started   bool
}

// NewMarketActor 创建市场Actor
func NewMarketActor(marketID uint64, marketPDA string) *MarketActor {
	return &MarketActor{
		MarketID:  marketID,
		MarketPDA: marketPDA,
		CmdChan:   make(chan *CommandWrapper, 2048), // 缓冲2048个命令
		Book:      NewFixedArrayOrderBook(marketID),
		Pending:   newPendingBatch(marketID, marketPDA, time.Now().UTC()),
	}
}
