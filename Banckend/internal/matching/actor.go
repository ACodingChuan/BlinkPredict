package matching

// MarketActor 是单个市场的并发壳（Actor模型）
type MarketActor struct {
	MarketID uint64
	CmdChan  chan *CommandWrapper
	Book     *FixedArrayOrderBook
	started  bool
}

// NewMarketActor 创建市场Actor
func NewMarketActor(marketID uint64) *MarketActor {
	return &MarketActor{
		MarketID: marketID,
		CmdChan:  make(chan *CommandWrapper, 2048), // 缓冲2048个命令
		Book:     NewFixedArrayOrderBook(marketID),
	}
}
