# 07 - Pusher 重构

## 目标

让 Pusher 成为热路径后的实时分发层，而不是数据库结果广播层。

## 输入

Pusher 只消费：
- `Realtime Event`

Pusher 不消费：
- 数据库查询结果
- Redis rebuild 结果
- matcher 私有内部状态

## 推送模型

### 市场频道
- `market:{market_id}`

消息：
- `market_depth_update`
- `market_trade_update`

### 用户频道
- `user:{wallet}`

消息：
- `user_order_update`
- `user_wallet_state_update`
- `user_position_update`
- `user_settlement_update`

## 约束

- 不阻塞热路径
- 对慢连接要断开或丢弃
- 支持消息合并
- 支持断线重连后的快照恢复

## 与 Redis 的关系

Pusher 不依赖 Redis 生成热路径消息。

Redis 只用于：
- 重连后补快照
- 用户主动查询

## 当前结论

- Pusher 是 realtime bus 的订阅者
- 不是 Writer 的从属广播器
