# 03 - PostgreSQL Schema 重构

## 目标

定义新架构下 PostgreSQL 的职责与表结构。

原则：
- PostgreSQL 负责 durable facts 和恢复基线；
- 不参与热路径资金锁；
- 不承担 Redis 锁补偿；
- 要支撑 Wallet Shard 恢复、Writer 落库、Executor 重试、Indexer 回补。

## 职责边界

PostgreSQL 负责：
- 市场元数据
- 订单生命周期
- 成交生命周期
- reservation 明细与聚合快照
- settlement task
- 链上观察记录
- 订单失效记录

PostgreSQL 不负责：
- 下单时资金仲裁
- Redis 锁
- 热路径可用余额计算

## 表结构

## 1. 保留并调整

### `markets`

用途：市场主表。

建议字段：
- `id`：UUID 主键
- `market_id`：业务市场 ID
- `condition_id`：条件事件 ID
- `market_pda`：链上市场地址
- `yes_asset_id`：YES 资产 ID
- `no_asset_id`：NO 资产 ID
- `collateral_mint`：抵押资产 mint
- `tick_size`：价格跳动单位
- `status`：市场状态
- `close_time`：关闭时间
- `resolved_at`：结算时间
- `created_at`：创建时间
- `updated_at`：更新时间

索引建议：
- `market_id unique`
- `(status, close_time)`

### `orders`

用途：订单主生命周期表。

建议字段：
- `order_id`：订单 ID，主键
- `market_id`：市场 ID
- `wallet_address`：钱包地址
- `asset_kind`：资产类型，`collateral / yes / no`
- `side`：买卖方向
- `order_type`：订单类型
- `price_tick`：价格档位
- `qty_lots`：原始数量
- `spend_amount`：金额字段
- `remaining_qty`：剩余数量
- `nonce`：防重放 nonce
- `signature`：用户签名
- `status`：订单状态
- `reservation_status`：预留状态
- `settlement_status`：结算状态
- `expire_time`：过期时间
- `created_cmd_seq`：创建命令序号
- `created_at`：创建时间
- `updated_at`：更新时间

状态建议：
- `status`：`accepted / live / partially_filled / filled / cancelled / expired / rejected`
- `reservation_status`：`none / open_reserved / mixed / pending_settlement_only / released / finalized`
- `settlement_status`：`none / pending / submitted / confirmed / failed`

索引建议：
- `(market_id, status, price_tick, created_cmd_seq)`
- `(wallet_address, created_at desc)`
- `(wallet_address, market_id, status)`

### `trades`

用途：成交与链上结算生命周期表。

建议字段：
- `trade_id`：成交 ID，主键
- `market_id`：市场 ID
- `maker_order_id`：maker 订单 ID
- `taker_order_id`：taker 订单 ID
- `maker_wallet_address`：maker 钱包
- `taker_wallet_address`：taker 钱包
- `match_price`：成交价
- `match_qty`：成交量
- `status`：成交状态
- `settlement_task_id`：结算任务 ID
- `chain_tx_sig`：链上签名
- `created_at`：成交时间
- `submitted_at`：提交时间
- `confirmed_at`：确认时间
- `failed_at`：失败时间
- `failure_reason`：失败原因

状态建议：
- `matched / submitted / confirmed / failed`

索引建议：
- `(market_id, created_at desc)`
- `(maker_wallet_address, created_at desc)`
- `(taker_wallet_address, created_at desc)`
- `(status, created_at)`

### `positions`

用途： outcome 仓位聚合快照。

建议字段：
- `market_id`：市场 ID
- `wallet_address`：钱包地址
- `yes_free_lots`：YES 可用仓位
- `yes_reserved_open_lots`：YES 开放占用
- `yes_pending_settlement_lots`：YES 待结算
- `no_free_lots`：NO 可用仓位
- `no_reserved_open_lots`：NO 开放占用
- `no_pending_settlement_lots`：NO 待结算
- `updated_at`：更新时间

主键建议：
- `(market_id, wallet_address)`

索引建议：
- `(wallet_address)`

### `consumer_cursors`

保留，用于消费者恢复。

## 2. 降级或删除

### `wallet_accounts`

处理方式：
- 从交易主账本降级为兼容表；
- 新逻辑不再依赖它做准入；
- 最终可删除。

### `order_locks`

处理方式：
- 整体删除；
- Writer 和 Gateway 均不再依赖。

### `positions` 中 collateral 字段

处理方式：
- 删除 `collateral_free_units`
- 删除 `collateral_locked_units`

原因：
- collateral 属于钱包全局维度，不属于 market-local position。

## 3. 新增表

### `wallet_asset_balances`

用途：钱包链上确认余额基线。

建议字段：
- `wallet_address`：钱包地址
- `asset_type`：`collateral / yes / no`
- `market_id`：市场 ID，collateral 可为空
- `asset_id`：资产标识
- `confirmed_units`：确认余额
- `delegated_units`：已授权额度
- `last_observed_slot`：最近链上 slot
- `updated_at`：更新时间

主键建议：
- `(wallet_address, asset_type, market_id)`

索引建议：
- `(wallet_address)`
- `(market_id, asset_type)`

### `wallet_reservation_state`

用途：钱包级 reservation 聚合快照。

建议字段：
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `reserved_open_units`：开放订单预留总额
- `reserved_pending_settlement_units`：待结算总额
- `version`：版本号
- `updated_at`：更新时间

主键建议：
- `(wallet_address, asset_type, market_id)`

### `wallet_reservations`

用途：订单级 reservation 明细。

建议字段：
- `reservation_id`：主键
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `original_reserved_units`：初始预留
- `open_reserved_units`：当前开放预留
- `pending_settlement_units`：当前待结算预留
- `released_units`：已释放额度
- `finalized_units`：已确认额度
- `rolled_back_units`：已回滚额度
- `updated_at`：更新时间

索引建议：
- `(order_id)`
- `(wallet_address, market_id)`
- `(wallet_address, updated_at desc)`

### `wallet_reservation_events`

用途：reservation 事实日志。

建议字段：
- `id`：主键
- `reservation_id`：reservation ID
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `event_type`：事件类型
- `delta_units`：变化额度
- `reason`：原因
- `source_event_id`：来源事件 ID
- `created_at`：时间

索引建议：
- `(reservation_id, created_at)`
- `(order_id, created_at)`

### `settlement_tasks`

用途：Executor durable 队列。

建议字段：
- `task_id`：任务 ID
- `trade_id`：成交 ID
- `market_id`：市场 ID
- `status`：任务状态
- `attempt_count`：尝试次数
- `next_retry_at`：下次重试时间
- `chain_tx_sig`：链上签名
- `last_error`：错误信息
- `created_at`：创建时间
- `updated_at`：更新时间

状态建议：
- `pending / submitting / submitted / confirmed / failed_retryable / failed_terminal`

索引建议：
- `(status, next_retry_at)`
- `(trade_id)`

### `chain_observations`

用途：Indexer 观察记录。

建议字段：
- `id`：主键
- `wallet_address`：钱包地址
- `observation_type`：观察类型
- `slot`：链上 slot
- `signature`：链上签名
- `payload`：原始负载
- `created_at`：时间

索引建议：
- `(wallet_address, slot desc)`
- `(signature)`
- `(observation_type, slot desc)`

### `order_invalidations`

用途：订单失效记录。

建议字段：
- `id`：主键
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `reason_code`：失效原因
- `observed_slot`：触发 slot
- `created_at`：时间

索引建议：
- `(order_id)`
- `(wallet_address, created_at desc)`

## 迁移策略

## 1. 第一阶段

- 新增新表，不删旧表；
- Writer 开始双写；
- 旧查询继续可用；
- 新逻辑开始读新表。

## 2. 第二阶段

- Gateway 停止依赖 `wallet_accounts`；
- 全面移除 `order_locks` 路径；
- `positions` 不再写 collateral 字段。

## 3. 第三阶段

- 删除旧路径；
- 清理兼容字段和旧查询；
- 保留必要历史数据迁移脚本。

## 与后续文档关系

这份文档解决的是 PostgreSQL。

你刚问的“matcher 模块的重构文档，是不是 Redis read model 重构那一步”，答案是：

- **不是。**

更准确地拆分是：

- `01`：热路径与 realtime event
- `02`：Wallet Shard
- `03`：PostgreSQL schema
- `04`：Redis read model
- **后面还应该单独有一篇 `matcher 模块重构文档`**

因为 matcher 重构关注的是：
- ingress 如何接命令
- 怎么调用 Wallet Shard
- Market Actor 输出什么结果
- 怎么组装 Realtime Event
- 与 Writer / Pusher / Executor 的边界

而 `04 - Redis read model` 只关注：
- key 设计
- projection 更新策略
- rebuild 策略

两者不是一回事。

## 下一步建议

下一篇建议写：

- `04-redis-read-model-redesign.md`

然后再补：

- `05-matcher-module-redesign.md`
