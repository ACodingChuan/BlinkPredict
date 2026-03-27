# 02 - Wallet Shard 设计

## 目标

Wallet Shard 是热路径里的钱包级状态机，负责两件事：

- 在订单进入 `Market Actor` 之前做准入；
- 在撮合之后立即迁移 reservation 状态。

它的设计目标是：

- 不额外增加总线 hop；
- 单钱包串行，避免并发超卖；
- 提供稳定的内存态；
- 支撑热路径到 pusher 的 1w TPS 目标。

## 角色定位

Wallet Shard 是 `Matcher Service` 进程内模块，不是独立服务。

调用关系：

- `Matcher Ingress -> Wallet Shard.try_reserve_open_order()`
- `Market Actor -> Wallet Shard.apply_match_result()`
- `Cancel / Expire -> Wallet Shard.release_open_reserve()`
- `Indexer / Executor -> Wallet Shard.finalize_after_chain_confirm()`

其中：

- `try_reserve_open_order`、`apply_match_result`、`release_open_reserve` 属于热路径；
- `finalize_after_chain_confirm`、`rollback_pending_settlement` 属于异步路径。

## 分片策略

## 1. 基本规则

按钱包地址做一致性分片：

- `shard_id = hash(wallet_address) % N`

说明：

- 同一钱包的所有命令必须进入同一个 shard；
- 不同钱包之间可以并行；
- 一个 shard 内部单线程处理。

## 2. 推荐初始参数

建议先支持可配置分片数：

- `N = 64 / 128 / 256`

经验原则：

- 钱包数量多、单钱包并发低时，增大 `N` 有利于并行；
- 如果市场数远大于钱包数，仍然优先按钱包分片，不按市场分片。

## 3. 与 Market Actor 的关系

- `Wallet Shard` 按钱包分片；
- `Market Actor` 按市场分片。

两者解决的问题不同：

- Wallet Shard：防超卖 / 防超买；
- Market Actor：价格时间优先撮合。

## 核心状态

## 1. 钱包级状态

每个钱包至少维护以下内存字段：

- `wallet_address`：钱包地址
- `last_chain_slot`：当前状态对应的最新链上 slot
- `frozen`：是否冻结新下单
- `confirmed_collateral`：确认过的 collateral 余额
- `reserved_open_collateral`：开放订单占用的 collateral
- `reserved_pending_collateral`：待结算占用的 collateral
- `available_collateral`：当前可用 collateral

说明：

- `available_collateral = confirmed_collateral - reserved_open_collateral - reserved_pending_collateral`

## 2. outcome 仓位状态

按 `market_id` 维护 outcome 维度状态：

- `confirmed_yes_lots`：YES 已确认仓位
- `reserved_open_yes_lots`：YES 开放订单占用
- `pending_yes_lots`：YES 待结算仓位
- `confirmed_no_lots`：NO 已确认仓位
- `reserved_open_no_lots`：NO 开放订单占用
- `pending_no_lots`：NO 待结算仓位

## 3. 订单级 reservation 状态

Wallet Shard 内部还需要能按订单快速定位 reservation：

- `order_id`：订单 ID
- `asset_type`：占用资产类型，`collateral / yes / no`
- `market_id`：市场 ID
- `original_reserved_units`：初始预留额度
- `open_reserved_units`：当前开放预留额度
- `pending_settlement_units`：待结算额度
- `released_units`：已释放额度
- `finalized_units`：已最终确认额度
- `rolled_back_units`：已回滚额度

## 准入规则

## 1. 买单

### `buy yes` / `buy no`

占用：

- `collateral`

准入条件：

- `available_collateral >= required_units`

成功后：

- `reserved_open_collateral += required_units`
- `available_collateral -= required_units`

## 2. 卖单

### `sell yes`

占用：

- `confirmed_yes_lots`

准入条件：

- `confirmed_yes_lots - reserved_open_yes_lots - pending_yes_lots >= required_lots`

成功后：

- `reserved_open_yes_lots += required_lots`

### `sell no`

占用：

- `confirmed_no_lots`

准入条件：

- `confirmed_no_lots - reserved_open_no_lots - pending_no_lots >= required_lots`

成功后：

- `reserved_open_no_lots += required_lots`

## 3. 冻结状态

当钱包处于 `frozen = true` 时：

- 禁止新的 `try_reserve_open_order`；
- 允许处理已有订单的取消、过期、结算回补；
- 允许异步修复状态。

## 核心接口

## 1. `try_reserve_open_order(order)`

用途：

- 新订单准入

输入：

- `order_id`：订单 ID
- `wallet_address`：钱包地址
- `market_id`：市场 ID
- `asset_type`：占用资产类型
- `required_units`：需要预留的额度或仓位

输出：

- `success`：是否成功
- `failure_reason`：失败原因
- `wallet_state_preview`：更新后的钱包预览状态
- `reservation_snapshot`：该订单对应 reservation 快照

语义：

- 成功即表示 open reservation 已在内存中生效；
- 失败则订单不能进入 `Market Actor`。

## 2. `apply_match_result(order_result, trades)`

用途：

- 撮合后迁移 reservation

输入：

- `order_result`：订单处理结果
- `trades`：成交列表

输出：

- `wallet_state_preview`：钱包预览状态
- `position_preview`：仓位预览状态
- `reservation_updates[]`：reservation 变化列表

语义：

- 把已成交部分从 `open_reserved` 迁到 `pending_settlement`；
- 把已取消或价格改善的部分释放；
- 不等待链上确认。

## 3. `release_open_reserve(order_id, wallet_address)`

用途：

- 撤单 / 过期释放 open reservation

输入：

- `order_id`：订单 ID
- `wallet_address`：钱包地址

输出：

- `wallet_state_preview`：钱包预览状态
- `reservation_snapshot`：更新后的 reservation 快照

## 4. `finalize_after_chain_confirm(settlement_result)`

用途：

- 链上确认后，把 pending settlement 转成 confirmed state

输入：

- `trade_id`：成交 ID
- `wallet_address`：钱包地址
- `asset_deltas`：确认后的资产变化
- `slot`：确认 slot

输出：

- `wallet_state_confirmed`：新的确认态钱包状态
- `position_state_confirmed`：新的确认态仓位状态

## 5. `rollback_pending_settlement(settlement_result)`

用途：

- 链上失败后回滚 pending settlement

输入：

- `trade_id`：成交 ID
- `wallet_address`：钱包地址
- `rollback_reason`：回滚原因

输出：

- `wallet_state_preview`：回滚后的钱包预览状态
- `position_preview`：回滚后的仓位预览状态

## reservation 状态迁移

## 1. 初始状态

新订单准入成功后：

- `original_reserved_units = X`
- `open_reserved_units = X`
- `pending_settlement_units = 0`
- `released_units = 0`
- `finalized_units = 0`
- `rolled_back_units = 0`

## 2. 部分成交

例如：

- 初始预留 `400`
- 成交后应进入 `pending_settlement = 220`
- 剩余继续挂单 `open_reserved = 160`
- 价格改善释放 `20`

那么更新为：

- `open_reserved_units = 160`
- `pending_settlement_units = 220`
- `released_units = 20`

## 3. 完全成交

更新为：

- `open_reserved_units = 0`
- `pending_settlement_units = consumed_units`
- 其余差额进入 `released_units`

## 4. 撤单 / 过期

更新为：

- `open_reserved_units = 0`
- 原 open 部分进入 `released_units`

## 5. 链上确认

更新为：

- `pending_settlement_units = 0`
- 对应额度进入 `finalized_units`

## 6. 链上失败

更新为：

- `pending_settlement_units = 0`
- 对应额度进入 `rolled_back_units`
- 同时按失败策略恢复到：
  - `released`，或
  - 重新回到 `open_reserved`

## 与 Realtime Event 的关系

Wallet Shard 不直接向前端推送消息。

它的职责是输出以下数据给 Realtime Event 组装层：

- `wallet_state_preview`
- `position_preview`
- `reservation_updates[]`
- `affected_wallets[]`

最终由 `Matcher Ingress / Realtime Event Publisher` 统一拼成：

- `wallet_state_preview_updates[]`
- `position_preview_updates[]`
- `order_updates[]`
- `trade_updates[]`

## 崩溃恢复

## 1. 恢复来源

Wallet Shard 重启后，恢复依据来自：

- `wallet_asset_balances`：链上确认余额基线
- `wallet_reservation_state`：钱包聚合 reservation 状态
- `wallet_reservations`：订单级 reservation 明细
- 未完成的 `settlement_tasks`：待结算任务

## 2. 恢复顺序

建议顺序：

1. 加载钱包确认态余额
2. 加载 wallet 级 reservation 聚合态
3. 加载 order 级 reservation 明细
4. 加载未完成 settlement task
5. 对聚合态与明细做一致性校验
6. 初始化 shard 内存状态

## 3. 恢复后的保护

恢复完成前：

- shard 对外不接受新下单

恢复完成后：

- 才允许 `try_reserve_open_order`

## 与其他组件的边界

### 对 Gateway
- Gateway 不直接调用 Wallet Shard
- Gateway 只发命令

### 对 Matcher Ingress
- Matcher Ingress 是 Wallet Shard 的唯一热路径入口

### 对 Market Actor
- Market Actor 不知道链上余额，只处理订单簿

### 对 Writer
- Writer 不拥有 reservation 权威
- Writer 只消费结果并落库

### 对 Executor / Indexer
- 只通过异步确认或失败结果更新 Wallet Shard

## 当前冻结结论

- Wallet Shard 是 matcher 进程内的钱包级状态机
- 分片维度按钱包，不按市场
- 新订单必须先过 Wallet Shard，再进 Market Actor
- 热路径内只处理 preview state，不等待链上确认
- pending settlement 必须单独建模
- 恢复基于链上确认余额 + reservation 状态 + 未完成 settlement task

## 下一步

下一篇建议文档：

- `03-database-schema-redesign.md`

因为 Wallet Shard 已经定义完成，接下来就应该把支撑它恢复与持久化的 PostgreSQL 结构定下来。
