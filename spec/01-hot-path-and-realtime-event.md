# 01 - 热路径与 Realtime Event

## 目标

定义唯一的热路径：

`用户下单 -> Gateway -> Matcher Ingress -> Wallet Shard -> Market Actor -> Realtime Event -> Pusher -> 前端收到更新`

说明：
- 这里的 1w TPS，只统计到前端收到实时消息；
- PostgreSQL、Redis 投影、链上 settlement、Indexer 回补都不在热路径里。

## 热路径组件

### Gateway

职责：
- 鉴权
- 验签
- 幂等
- 生成内部命令并发到总线

不负责：
- 资金锁
- reservation
- 持久化

### Matcher Ingress

职责：
- 作为热路径唯一命令消费者
- 调用 Wallet Shard 做准入
- 调用 Market Actor 做撮合
- 组装 Realtime Event

约束：
- 整条热路径只经过一次总线
- 不再把命令二次 publish 给别的准入服务

### Wallet Shard

职责：
- 钱包级可用性判断
- open reservation 建立
- 撮合后的 reservation 迁移
- 撤单/过期释放 reservation

约束：
- matcher 进程内模块
- 不单独消费 NATS
- 同一钱包必须串行处理

### Market Actor

职责：
- 保持市场订单簿
- 做撮合
- 输出 order / trade / depth 变化

约束：
- 保留当前 `FixedArrayOrderBook`
- 不负责钱包余额判断

### Pusher

职责：
- 直接消费 Realtime Event
- 推送市场和用户消息

约束：
- 不等 Writer
- 不回查数据库或 Redis 再生成热路径消息

## 下单流程

### 1. Gateway 接单

输入：
- 用户请求
- 签名订单

处理：
1. 鉴权
2. 验签
3. 幂等检查
4. 发布 `place_order_cmd`

输出：
- `place_order_cmd`

### 2. Matcher Ingress 收单

处理：
1. 解析命令
2. 计算该订单需要预留的资产与额度
3. 调用 `WalletShard.try_reserve_open_order()`

预留对象：
- `buy yes` / `buy no`：预留 collateral
- `sell yes`：预留 YES 仓位
- `sell no`：预留 NO 仓位

### 3. Wallet Shard 准入

判断依据：
- `confirmed_balance`
- `reserved_open`
- `reserved_pending_settlement`
- `available = confirmed_balance - reserved_open - reserved_pending_settlement`

成功：
- 建立 open reservation
- 返回 success

失败：
- 直接生成 `order_rejected`
- 不进入 Market Actor

### 4. Market Actor 撮合

成功准入后：
1. 尝试撮合
2. 无对手盘则挂簿
3. 有对手盘则输出：
   - `order_update`
   - `trade_update`
   - `depth_update`

### 5. Wallet Shard 迁移 reservation

根据撮合结果：
- 完全挂簿：reservation 继续留在 `open_reserved`
- 部分成交：
  - 已成交部分转到 `pending_settlement`
  - 剩余部分继续留在 `open_reserved`
  - 多余部分释放
- 完全成交：
  - 清空 open reservation
  - 转为 `pending_settlement`

### 6. 提交 Realtime Event

当以下两件事都完成后，热路径视为提交成功：
- Market Actor 已完成本次命令处理
- Wallet Shard 已完成 reservation 更新

此时生成统一 `Realtime Event`。

### 7. Pusher 推送前端

Pusher 收到 `Realtime Event` 后：
- 市场频道推：
  - depth update
  - trade update
- 用户频道推：
  - order update
  - wallet state preview update
  - position preview update

到这里，热路径结束。

## Realtime Event 结构

### 顶层字段

- `event_id`：事件 ID
- `market_id`：市场 ID
- `command_id`：来源命令 ID
- `source_cmd_seq`：来源命令序号
- `wallets_involved[]`：受影响钱包
- `order_updates[]`：订单变化
- `trade_updates[]`：成交变化
- `depth_updates[]`：盘口变化
- `wallet_state_preview_updates[]`：钱包预览变化
- `position_preview_updates[]`：仓位预览变化
- `emitted_at`：发出时间

### `order_updates[]`

- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `market_id`：市场 ID
- `status`：订单状态
- `remaining_qty`：剩余数量
- `reservation_state`：预留状态
- `settlement_state`：结算状态
- `updated_at`：更新时间

### `trade_updates[]`

- `trade_id`：成交 ID
- `maker_order_id`：maker 订单 ID
- `taker_order_id`：taker 订单 ID
- `price_tick`：成交价格
- `match_qty`：成交数量
- `status`：成交状态
- `executed_at`：成交时间

### `depth_updates[]`

- `side`：bid / ask
- `price_tick`：价格档位
- `total_volume`：该档位总量
- `updated_at`：更新时间

### `wallet_state_preview_updates[]`

- `wallet_address`：钱包地址
- `collateral_confirmed_units`：当前已知确认余额
- `collateral_reserved_open_units`：开放预留
- `collateral_reserved_pending_settlement_units`：待结算预留
- `collateral_available_units`：当前可用余额
- `updated_at`：更新时间

### `position_preview_updates[]`

- `wallet_address`：钱包地址
- `market_id`：市场 ID
- `yes_free_lots`：YES 可用仓位
- `yes_reserved_open_lots`：YES 开放占用
- `yes_pending_settlement_lots`：YES 待结算
- `no_free_lots`：NO 可用仓位
- `no_reserved_open_lots`：NO 开放占用
- `no_pending_settlement_lots`：NO 待结算
- `updated_at`：更新时间

## Wallet Shard 接口

### `try_reserve_open_order(order)`

输入：
- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `asset_type`：资产类型
- `market_id`：市场 ID
- `required_units`：需要预留的额度

输出：
- `success`：是否成功
- `failure_reason`：失败原因
- `wallet_state_preview`：钱包预览状态

### `apply_match_result(order_result, trades)`

输入：
- `order_result`：订单处理结果
- `trades`：本次成交列表

输出：
- 更新后的 reservation
- 钱包预览状态
- 仓位预览状态

### `release_open_reserve(order_id, wallet_address)`

输入：
- `order_id`：订单 ID
- `wallet_address`：钱包地址

输出：
- 钱包预览状态

## 失败处理原则

### reservation 失败
- 不进入 Market Actor
- 直接发 `order_rejected`
- Pusher 通知前端

### matcher 处理失败
- 回滚本次 reservation
- 生成 `order_failed` 或 `order_rejected`
- 异步记录诊断信息

### pusher 推送失败
- 不回滚热路径
- 慢连接由 pusher 自己丢弃或断开
- 前端靠重连和快照追平

### writer 落库失败
- 不回滚热路径
- 通过重试与重放修复

## 当前冻结结论

- 热路径唯一命令消费者是 Matcher Ingress
- Wallet Shard 是 matcher 进程内同步模块
- Market Actor 保持当前 O(1) 结构
- 热路径提交点是 Realtime Event 发出
- Pusher 直接消费 Realtime Event
- Writer、Executor、Indexer 全异步
