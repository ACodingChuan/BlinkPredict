# 05 - Matcher 模块重构

## 目标

在不改动当前订单簿核心结构的前提下，完成 matcher 的入口、输出和模块边界重构。

保留：
- `FixedArrayOrderBook`
- `Market Actor`
- 市场级单线程撮合模型

重构：
- ingress
- Wallet Shard 接入
- realtime event 组装
- 与 Writer / Pusher / Executor 的边界

## 新结构

Matcher 模块由 4 部分组成：
- `Matcher Ingress`
- `Wallet Shard`
- `Market Actor`
- `Realtime Event Publisher`

## Ingress 职责

### 输入
- `place_order_cmd`
- `cancel_order_cmd`
- `tick_cmd`
- `invalidation_cmd`

### 流程
1. 收命令
2. 对 place order 先调 Wallet Shard
3. 准入成功后送入对应 Market Actor
4. 收到 Market Actor 输出
5. 再调 Wallet Shard 迁移 reservation
6. 组装 `Realtime Event`
7. 发布给 Pusher / Writer / Executor

## Market Actor 输出

每次处理命令后，输出统一结果：
- `order_updates[]`
- `trade_updates[]`
- `depth_updates[]`
- `affected_wallets[]`

说明：
- Market Actor 不直接组装最终 websocket payload
- 它输出的是 matcher 结果，不是 UI 消息

## 与 Wallet Shard 的接口

### 下单前
- `try_reserve_open_order(order)`

### 撮合后
- `apply_match_result(order_result, trades)`

### 撤单/过期
- `release_open_reserve(order_id, wallet_address)`

## 与 Writer 的边界

Matcher 不负责：
- 落库
- Redis 投影
- replay rebuild

Writer 只消费 matcher 发出的结果事件。

## 与 Pusher 的边界

Matcher 不直接写 websocket。

Pusher 只消费：
- `Realtime Event`

## 与 Executor 的边界

Executor 只消费：
- `trade_updates[]` 中需要上链结算的部分

## 当前结论

- matcher 重构核心在 ingress，不在 book 结构
- Wallet Shard 是 matcher 内模块，不是外部 hop
- Realtime Event 是 matcher 热路径最终输出
