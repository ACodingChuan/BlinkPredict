# 06 - Writer 重构

## 目标

将 Writer 改造成异步持久化与 projection 引擎。

Writer 负责：
- 落 PostgreSQL
- 更新 Redis read model
- 负责重放与恢复基础

Writer 不负责：
- 资金锁
- reservation 权威判断
- 直接 websocket 推送

## 输入

Writer 消费以下事件：
- `Realtime Event`
- `settlement confirmed`
- `settlement failed`
- `order invalidation`
- `chain observation derived updates`（如需要）

## 持久化内容

### 订单
- `orders`

### 成交
- `trades`

### reservation
- `wallet_reservations`
- `wallet_reservation_events`
- `wallet_reservation_state`

### 仓位
- `positions`

### 结算
- `settlement_tasks`

### 失效
- `order_invalidations`

## Redis 更新内容

- `l2:depth:*`
- `user:orders:*`
- `order:info:*`
- `trades:latest:*`
- `price:history:*`
- `wallet-state:*`
- `position:*`

## 关键原则

- 按事件增量更新
- 只更新受影响 key
- 失败重试不回滚热路径
- 必须支持从 PostgreSQL 全量 rebuild Redis

## 删除的旧职责

- `order_locks` 处理
- Redis balance release
- 市场维度全量 position 回刷

## 当前结论

- Writer 是异步 durable + projection 层
- 热路径结束点不是 Writer commit
