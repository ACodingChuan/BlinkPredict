# 04 - Redis Read Model 重构

## 目标

定义 Redis 在新架构中的唯一职责：
- 查询加速
- websocket / 前端读取投影
- 可丢弃、可重建的临时模型

Redis 不负责：
- 资金锁
- 下单准入
- 钱包余额权威

## key 设计

### 市场类

#### `l2:depth:{market_id}`
- `bid:{price_tick}`：买档总量
- `ask:{price_tick}`：卖档总量
- `updated_at`：更新时间

#### `trades:latest:{market_id}`
- list 元素字段：`trade_id / price / quantity / status / executed_at / chain_tx_sig`

#### `price:history:{market_id}`
- zset 元素字段：`timestamp / price / quantity`

#### 市场缓存
- 沿用现有 market cache

### 用户类

#### `user:orders:{wallet}`
- zset member：`order_id`
- score：`created_cmd_seq` 或时间序号

#### `order:info:{order_id}`
- `market_id`
- `wallet_address`
- `side`
- `price_tick`
- `remaining_qty`
- `status`
- `reservation_state`
- `settlement_state`
- `invalid_reason`
- `updated_at`

#### `wallet-state:{wallet}`
- `collateral_confirmed_units`
- `collateral_reserved_open_units`
- `collateral_reserved_pending_settlement_units`
- `collateral_available_units`
- `source_slot`
- `updated_at`

#### `position:{market_id}:{wallet}`
- `yes_free_lots`
- `yes_reserved_open_lots`
- `yes_pending_settlement_lots`
- `no_free_lots`
- `no_reserved_open_lots`
- `no_pending_settlement_lots`
- `updated_at`

### 临时类

#### websocket ticket
- `ticket`
- `wallet_address`
- `expire_at`

## 删除的 key

- `wallet-account:{wallet}`
- `gateway:balance:vusdc:{wallet}`
- `locked:order:{order_id}`

## 更新来源

Redis 只由 Writer 统一更新。

输入事件：
- `Realtime Event`
- `settlement confirmed / failed`
- `order invalidation`

## 更新原则

- 增量更新，不做全量回刷
- 单事件只更新受影响的 market / wallet / order
- 允许短暂最终一致，不要求热路径内同步完成

## rebuild 策略

支持全量重建以下投影：
- `l2:depth:*`
- `user:orders:*`
- `order:info:*`
- `trades:latest:*`
- `price:history:*`
- `wallet-state:*`
- `position:*`

重建数据来源：
- PostgreSQL

## 当前结论

- Redis 是 projection，不是 authority
- 所有钱包可用性判断必须来自 Wallet Shard
- Writer 是 Redis 唯一业务写入口
