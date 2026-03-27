# 00 - 新架构总览

## 目标

- 热路径目标：`用户下单 -> 前端收到 pusher 更新` 单机 1w TPS。
- 保留当前 `matcher` 核心订单簿结构与 O(1) 主路径。
- PostgreSQL 落库、Redis 投影、链上提交与确认全部异步。

## 总体架构

### 1. Gateway

职责：
- 鉴权
- 验签
- 幂等
- 发布下单/撤单命令

不负责：
- 资金锁
- Redis 锁
- 链上余额仲裁

### 2. Matcher Service

由以下部分组成：
- `Matcher Ingress`
- `Wallet Shard`
- `Market Actor`
- `Realtime Event Publisher`

职责：
- 消费命令
- 先做钱包级 reservation 准入
- 再进入市场订单簿撮合
- 产出实时事件

### 3. Wallet Shard

职责：
- 按钱包分片
- 维护钱包运行时可用性状态
- 提供：
  - `try_reserve_open_order`
  - `apply_match_result`
  - `release_open_reserve`
  - `finalize_after_chain_confirm`
  - `rollback_pending_settlement`

约束：
- 是 matcher 进程内模块
- 不单独消费第二次 NATS

### 4. Pusher

职责：
- 直接消费 realtime event
- 推送 market / user 两类消息

约束：
- 不等 Writer
- 不回查数据库再推送

### 5. Writer

职责：
- 异步落库
- 异步更新 Redis read model
- 支持恢复与重放

### 6. Executor

职责：
- 异步消费成交
- 创建 settlement task
- 调链上程序提交结算

### 7. Indexer

职责：
- 同步用户链上资产与 delegate 状态
- 跟踪 settlement 链上确认结果
- 触发 invalidation

## 组件交互

### 下单热路径

1. 前端请求 Gateway
2. Gateway 验签后发布命令
3. Matcher Ingress 消费命令
4. Wallet Shard 先做 reservation
5. 成功后送入 Market Actor
6. Market Actor 撮合或挂簿
7. Wallet Shard 根据结果迁移 reservation
8. 发布 realtime event
9. Pusher 推给前端
10. Writer / Executor 异步处理

### 非热路径

- Writer：落 PostgreSQL + 更新 Redis
- Executor：提交链上 settlement
- Indexer：同步链上真实状态

## 数据结构

## PostgreSQL

### 保留并增强

#### `markets`
- `market_id`：市场 ID
- `condition_id`：条件事件 ID
- `yes_asset_id`：YES 资产 ID
- `no_asset_id`：NO 资产 ID
- `tick_size`：价格跳动单位
- `status`：市场状态
- `close_time`：关闭时间
- `resolved_at`：结算时间
- `updated_at`：更新时间

#### `orders`
- `order_id`：订单 ID
- `market_id`：市场 ID
- `wallet_address`：钱包地址
- `asset_kind`：资产类型
- `side`：买卖方向
- `order_type`：订单类型
- `price_tick`：价格档位
- `qty_lots`：原始数量
- `spend_amount`：金额字段
- `remaining_qty`：剩余数量
- `nonce`：防重放值
- `signature`：用户签名
- `status`：订单主状态
- `reservation_status`：预留状态
- `settlement_status`：结算状态
- `created_cmd_seq`：命令序号
- `expire_time`：过期时间
- `created_at`：创建时间
- `updated_at`：更新时间

#### `trades`
- `trade_id`：成交 ID
- `market_id`：市场 ID
- `maker_order_id`：maker 订单 ID
- `taker_order_id`：taker 订单 ID
- `maker_wallet_address`：maker 钱包
- `taker_wallet_address`：taker 钱包
- `match_price`：成交价
- `match_qty`：成交量
- `status`：成交状态
- `settlement_task_id`：结算任务 ID
- `chain_tx_sig`：链上交易签名
- `created_at`：创建时间
- `submitted_at`：提交时间
- `confirmed_at`：确认时间
- `failed_at`：失败时间
- `failure_reason`：失败原因

#### `positions`
- `market_id`：市场 ID
- `wallet_address`：钱包地址
- `yes_free_lots`：YES 可用仓位
- `yes_reserved_open_lots`：YES 开放订单占用
- `yes_pending_settlement_lots`：YES 待结算仓位
- `no_free_lots`：NO 可用仓位
- `no_reserved_open_lots`：NO 开放订单占用
- `no_pending_settlement_lots`：NO 待结算仓位
- `updated_at`：更新时间

### 下线或降级

#### `wallet_accounts`
- 迁移期可留作兼容投影
- 不再作为交易主账本

#### `order_locks`
- 整体删除

### 新增

#### `wallet_asset_balances`
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `asset_id`：资产标识
- `confirmed_units`：链上确认余额
- `delegated_units`：授权额度
- `last_observed_slot`：最新链上 slot
- `updated_at`：更新时间

#### `wallet_reservation_state`
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `reserved_open_units`：开放订单预留总额
- `reserved_pending_settlement_units`：待结算预留总额
- `version`：版本号
- `updated_at`：更新时间

#### `wallet_reservations`
- `reservation_id`：预留记录 ID
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `original_reserved_units`：初始预留
- `open_reserved_units`：当前开放预留
- `pending_settlement_units`：待结算预留
- `released_units`：已释放额度
- `finalized_units`：已最终结算额度
- `rolled_back_units`：已回滚额度
- `updated_at`：更新时间

#### `wallet_reservation_events`
- `id`：事件 ID
- `reservation_id`：预留记录 ID
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `event_type`：事件类型
- `delta_units`：额度变化
- `reason`：原因
- `source_event_id`：来源事件 ID
- `created_at`：时间

#### `settlement_tasks`
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

#### `chain_observations`
- `id`：观察记录 ID
- `wallet_address`：钱包地址
- `observation_type`：观察类型
- `slot`：链上 slot
- `signature`：链上签名
- `payload`：原始负载
- `created_at`：时间

#### `order_invalidations`
- `id`：失效记录 ID
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `reason_code`：失效原因
- `observed_slot`：观测 slot
- `created_at`：时间

## Redis

### 保留

#### `l2:depth:{market_id}`
- `bid:{price_tick}`：买档总量
- `ask:{price_tick}`：卖档总量
- `updated_at`：更新时间

#### `user:orders:{wallet}`
- `order_id`：订单索引 member
- `score`：排序值，建议命令序号

#### `order:info:{order_id}`
- `market_id`：市场 ID
- `wallet_address`：钱包地址
- `side`：方向
- `price_tick`：价格档位
- `remaining_qty`：剩余数量
- `status`：订单状态
- `reservation_state`：预留状态
- `settlement_state`：结算状态
- `invalid_reason`：失效原因
- `updated_at`：更新时间

#### `trades:latest:{market_id}`
- `trade_id`：成交 ID
- `price`：价格
- `quantity`：数量
- `status`：状态
- `chain_tx_sig`：链上签名
- `executed_at`：时间

#### `price:history:{market_id}`
- `timestamp`：时间点
- `price`：价格
- `quantity`：成交量

#### `position:{market_id}:{wallet}`
- `yes_free_lots`：YES 可用仓位
- `yes_reserved_open_lots`：YES 开放占用
- `yes_pending_settlement_lots`：YES 待结算
- `no_free_lots`：NO 可用仓位
- `no_reserved_open_lots`：NO 开放占用
- `no_pending_settlement_lots`：NO 待结算
- `updated_at`：更新时间

#### `wallet-state:{wallet}`
- `collateral_confirmed_units`：确认余额
- `collateral_reserved_open_units`：开放预留
- `collateral_reserved_pending_settlement_units`：待结算预留
- `collateral_available_units`：可用余额
- `source_slot`：来源 slot
- `updated_at`：更新时间

#### 市场缓存 key
- 继续保留现有 market cache

#### websocket ticket key
- `ticket`：票据
- `wallet_address`：钱包地址
- `expire_at`：过期时间

### 删除

- `wallet-account:{wallet}`：旧内部账本投影
- `gateway:balance:vusdc:{wallet}`：旧 Gateway 余额缓存
- `locked:order:{order_id}`：旧锁单结构

## 运行时核心结构

### Wallet Shard
- `confirmed_collateral`：确认 collateral
- `reserved_open_collateral`：开放预留 collateral
- `reserved_pending_collateral`：待结算 collateral
- `available_collateral`：可用 collateral
- `confirmed_yes_by_market`：各市场 YES 已确认仓位
- `reserved_open_yes_by_market`：各市场 YES 开放预留
- `pending_yes_by_market`：各市场 YES 待结算
- `confirmed_no_by_market`：各市场 NO 已确认仓位
- `reserved_open_no_by_market`：各市场 NO 开放预留
- `pending_no_by_market`：各市场 NO 待结算
- `last_chain_slot`：最新链上 slot

### Market Actor
- `FixedArrayOrderBook`：订单簿主结构
- `Bids[100]`：买盘数组
- `Asks[100]`：卖盘数组
- `Orders`：订单索引
- `BestBidPrice`：最优买价
- `BestAskPrice`：最优卖价

### Realtime Event
- `event_id`：事件 ID
- `market_id`：市场 ID
- `wallets_involved[]`：受影响钱包
- `order_updates[]`：订单变更
- `trade_updates[]`：成交变更
- `depth_updates[]`：深度变更
- `wallet_state_preview_updates[]`：钱包预览变更
- `position_preview_updates[]`：仓位预览变更
- `emitted_at`：发出时间

### Pusher 消息
- `market_depth_update`：盘口更新
- `market_trade_update`：成交更新
- `user_order_update`：用户订单更新
- `user_wallet_state_update`：用户钱包状态更新
- `user_position_update`：用户仓位更新
- `user_settlement_update`：用户结算状态更新

## 基于当前代码的改造范围

### Banckend
- `internal/http`：删 Redis 锁，入口化
- `internal/matching`：加 Wallet Shard 准入与 realtime event
- `internal/writer`：删 lock 逻辑，改异步落库 + Redis 投影
- `internal/pusher`：改消费 realtime event
- `internal/faucet`：改成直接发用户控制账户
- `internal/indexer`：按新链上同步模型重写
- `db/schema.sql`：整体重构
- `cmd/api/main.go`：支持多 mode 启动

### Frontened
- 适配 wallet state / position / order / settlement 新状态模型

### Contract
- 增加 settlement 指令
- 明确用户控制账户模型
- 整理 split / merge / redeem 与 settlement 的关系

## 运行方式建议

建议方案：
- 单仓库
- 单 Go 二进制
- 多 mode 启动
- 生产拆成多个进程

推荐 mode：
- `api`
- `matcher`
- `writer`
- `pusher`
- `executor`
- `indexer`
- `all`

说明：
- 本地开发可用 `all`
- 生产不建议一个进程承载全部角色

## 后续文档顺序

1. `01-hot-path-and-realtime-event.md`
2. `02-wallet-shard-design.md`
3. 数据库 schema 重构
4. Redis read model 重构
5. Writer 重构
6. Pusher 重构
7. Indexer 设计
8. Executor 与 Contract 设计
9. Frontend 状态模型
10. 启动与部署
