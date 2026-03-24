package matching

import (
	"sync"
)

// ==========================================
// MemoryOrder 侵入式双向链表节点
// ==========================================

// MemoryOrder 是侵入式双向链表的节点，也是纯内存业务载体
type MemoryOrder struct {
	OrderID           uint64
	WalletAddress     string
	OriginalAction    uint8
	OriginalOutcome   uint8
	OriginalPriceTick uint8
	Side              uint8
	OrderType         uint8
	PriceTick         uint8
	RemainingQty      uint64
	RemainingSpend    uint64
	ExpireTime        int64
	Signature         string
	IntentBytesHex    string
	Nonce             uint64 // 防碰撞nonce

	Next *MemoryOrder // 指向后一个订单 (比我晚的)
	Prev *MemoryOrder // 指向前一个订单 (比我早的)
}

// IsFilled 判断订单是否已完全成交
func (o *MemoryOrder) IsFilled() bool {
	if o.OrderType == OrderTypeMarket && o.Side == SideBuy {
		return o.RemainingSpend == 0
	}
	return o.RemainingQty == 0
}

// InitFromCmd 从命令初始化订单
func (o *MemoryOrder) InitFromCmd(cmd *PlaceOrderCommand) {
	o.OrderID = cmd.OrderID
	o.WalletAddress = cmd.WalletAddress
	o.OriginalAction = cmd.OriginalAction
	o.OriginalOutcome = cmd.OriginalOutcome
	o.OriginalPriceTick = cmd.OriginalPriceTick
	o.Side = cmd.Side
	o.OrderType = cmd.OrderType
	o.PriceTick = cmd.PriceTick
	o.RemainingQty = cmd.QtyLots
	o.RemainingSpend = cmd.SpendAmount
	o.ExpireTime = cmd.ExpireTime
	o.Signature = cmd.Signature
	o.IntentBytesHex = cmd.IntentBytesHex
	o.Nonce = cmd.Nonce
}

// ==========================================
// 对象池管理 (零GC优化)
// ==========================================

var orderPool = sync.Pool{
	New: func() interface{} { return &MemoryOrder{} },
}

// AcquireOrder 从对象池获取订单
func AcquireOrder() *MemoryOrder {
	return orderPool.Get().(*MemoryOrder)
}

// ReleaseOrder 归还订单到对象池
func ReleaseOrder(order *MemoryOrder) {
	// 【核心优化】：瞬间抹平所有的历史数据和指针引用，杜绝内存泄漏
	*order = MemoryOrder{}
	orderPool.Put(order)
}
