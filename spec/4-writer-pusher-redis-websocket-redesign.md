# BlinkPredict Writer / Pusher / Redis / WebSocket 设计文档

本文承接：

- `0-contractdesign.md`
- `3-matcher-shared-wallet-batching-redesign.md`

本文的目标是把新的 `writer` 与 `pusher` 架构一次性讲清楚，重点覆盖：

1. `writer` 如何消费 `matcher` 输出的 `evt.match.batch.v2.{market_id}`
2. `writer` 的数据库投影职责与最小数据结构
3. `Redis` 读模型如何设计
4. `pusher` 如何直接消费 matcher 事件，不再依赖 writer 二次转推
5. WebSocket 的频道、消息结构、鉴权方式与客户端消费约定

本文不讨论：

- `settlement` 的链上交易构造
- `shared wallet` 的 webhook 来源细节
- 前端页面具体 UI 样式

---

## 0. 设计结论

新的链路定稿如下：

1. Gateway 发布 `cmd.order.place`
2. matcher 消费命令并输出 `evt.match.batch.v2.{market_id}`
3. `writer`、`settlement`、`pusher` 三方**并行消费**

其中：

- `writer` 负责：
  - 数据库落库
  - Redis 读模型维护
- `pusher` 负责：
  - 直接将 matcher 事件转换成 websocket 广播
- `settlement` 负责：
  - 直接拿 `orders[] + fills[]` 组织链上提交

因此，旧的：

- matcher -> writer -> push.subject -> pusher

不再是目标架构。

新的原则是：

- `writer` 不再是 `pusher` 的前置依赖
- `pusher` 可以比 `writer` 更快把增量广播给用户
- `Redis` 的职责是：
  - HTTP 查询加速
  - websocket 客户端断线重连后的冷恢复
  - 用户 open orders / 盘口 / 近期成交 / 持仓查询

---

## 1. matcher v2 事件回顾

`writer` 与 `pusher` 的一切设计都围绕 matcher v2 事件展开。

matcher 对下游发布：

- `evt.match.batch.v2.{market_id}`

顶层结构如下：

```go
type MatchBatchEventV2 struct {
    EventID       string `json:"event_id"`
    SchemaVersion int    `json:"schema_version"`

    MarketID   uint64 `json:"market_id,string"`
    MarketPDA  string `json:"market_pda"`
    ProducedAt int64  `json:"produced_at"`

    SourceCmdSeqMin uint64 `json:"source_cmd_seq_min"`
    SourceCmdSeqMax uint64 `json:"source_cmd_seq_max"`

    SourceCommandIDs []string `json:"source_command_ids,omitempty"`
    TraceIDs         []string `json:"trace_ids,omitempty"`

    Orders       []MatchedOrderV2 `json:"orders"`
    Fills        []MatchFillV2    `json:"fills"`
    OrderUpdates []OrderUpdateV2  `json:"order_updates"`
    DepthUpdates []DepthUpdateV2  `json:"depth_updates"`
}
```

其中：

- `orders[]`
  - 去重订单表
- `fills[]`
  - 成交结果
- `order_updates[]`
  - 订单状态变化
- `depth_updates[]`
  - 盘口档位增量

这四部分分别服务于：

- `writer`
- `pusher`
- `settlement`

---

## 2. writer 的职责边界

### 2.1 writer 负责什么

writer 负责把 matcher 事件投影成：

- 数据库中的最终可查询模型
- Redis 中的高频读模型

writer 需要消费的 matcher 字段：

- `orders[]`
- `fills[]`
- `order_updates[]`
- `depth_updates[]`

### 2.2 writer 不负责什么

writer 不负责：

- 给 pusher 组装单独的 push stream 作为唯一来源
- 重新解释撮合逻辑
- 组织链上 batch
- 主导 shared wallet 状态

### 2.3 writer 的一致性原则

writer 消费 matcher 事件时，必须遵守：

- 以 `EventID` 或 stream sequence 做幂等
- 同一个 market 的事件必须按顺序投影
- 若 Redis 写失败，不影响数据库事务回滚语义判断
- 数据库是主投影，Redis 是加速层

---

## 3. writer 消费流程

writer 的推荐消费流程为：

1. 从 `evt.match.batch.v2.*` 拉取一条 event
2. 校验 schema version
3. 用 event sequence 做幂等检查
4. 开启数据库事务
5. 依次处理：
   - `orders[]`
   - `fills[]`
   - `order_updates[]`
6. 提交事务
7. 更新 Redis 读模型
8. ACK NATS 消息

### 3.1 为什么先 DB 后 Redis

原因：

- Redis 是缓存，不应成为主真相
- 若 DB 成功、Redis 失败，可以后续补写或重建
- 若 Redis 成功、DB 失败，会让查询出现假状态

### 3.2 market 级顺序

建议 writer 的 cursor 粒度仍然按 market 维护：

- `consumer_name + market_id -> last_processed_seq`

这样便于：

- 单 market 顺序推进
- 出问题时按市场回放
- 与 matcher market actor 模型一致

---

## 4. writer 最小数据库投影

本节不强行绑定你当前库表，但给出推荐最小模型。

### 4.1 orders 表

推荐至少保留：

```sql
orders (
  order_id               NUMERIC(20,0) PRIMARY KEY,
  market_id              NUMERIC(20,0) NOT NULL,
  wallet_address         TEXT NOT NULL,

  original_action        TEXT NOT NULL,
  original_outcome       TEXT NOT NULL,
  original_price_tick    SMALLINT NOT NULL,

  normalized_side        TEXT NOT NULL,
  normalized_price_tick  SMALLINT NOT NULL,
  order_type             TEXT NOT NULL,

  initial_qty_lots       BIGINT NOT NULL,
  initial_spend_amount   BIGINT NOT NULL,
  remaining_qty_lots     BIGINT NOT NULL,
  remaining_spend_amount BIGINT NOT NULL,

  status                 TEXT NOT NULL,
  nonce                  NUMERIC(20,0) NOT NULL,
  expire_time            BIGINT NOT NULL,

  intent_hex             TEXT NOT NULL,
  signature              TEXT NOT NULL,

  created_at             TIMESTAMPTZ NOT NULL,
  updated_at             TIMESTAMPTZ NOT NULL
)
```

说明：

- `intent_hex` / `signature` 保留，便于后续 settlement / 审计 / 排障
- `normalized_*` 与 `original_*` 同时保留，避免查询层反复推导
- 精度约束固定为：
  - `original_price_tick` / `normalized_price_tick` / `fill_price` 都是 `1..99`
  - `*_qty_lots` / `fill_amount` / `total_volume` 都是 `shares * 100`
  - `*_spend_amount` 都是 `usdc * 100`

### 4.2 trades 表

推荐：

```sql
trades (
  trade_id              TEXT PRIMARY KEY,
  market_id             NUMERIC(20,0) NOT NULL,

  maker_order_id        NUMERIC(20,0) NOT NULL,
  taker_order_id        NUMERIC(20,0) NOT NULL,
  maker_wallet_address  TEXT NOT NULL,
  taker_wallet_address  TEXT NOT NULL,

  fill_price            BIGINT NOT NULL,
  fill_amount           BIGINT NOT NULL,
  match_type            TEXT NOT NULL,

  notional_units        BIGINT NOT NULL,
  taker_fee_units       BIGINT NOT NULL,
  creator_fee_units     BIGINT NOT NULL,
  platform_fee_units    BIGINT NOT NULL,

  produced_at           TIMESTAMPTZ NOT NULL
)
```

### 4.3 consumer_cursors 表

推荐：

```sql
consumer_cursors (
  consumer_name         TEXT NOT NULL,
  market_id             NUMERIC(20,0) NOT NULL,
  last_evt_seq          BIGINT NOT NULL,
  updated_at            TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (consumer_name, market_id)
)
```

### 4.4 positions 表

若继续保留 positions 投影，建议字段与 shared wallet / 链上语义一致：

```sql
positions (
  market_id                  NUMERIC(20,0) NOT NULL,
  wallet_address             TEXT NOT NULL,

  yes_free_lots              BIGINT NOT NULL,
  yes_locked_lots            BIGINT NOT NULL,
  no_free_lots               BIGINT NOT NULL,
  no_locked_lots             BIGINT NOT NULL,
  collateral_free_units      BIGINT NOT NULL,
  collateral_locked_units    BIGINT NOT NULL,

  updated_at                 TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (market_id, wallet_address)
)
```

这里的 positions 是查询投影，不是 shared wallet 的权威内存本体。

---

## 5. writer 如何解释 matcher v2 字段

### 5.1 `orders[]`

writer 对 `orders[]` 的使用：

- 若 `order_id` 不存在，则插入 orders 表
- 如果已存在，则只做补充校验，不重复插入

推荐直接存：

- `execution.*`
- `settlement.intent_bytes_hex`
- `settlement.signature`

### 5.2 `fills[]`

writer 对 `fills[]` 的使用：

- 每条 fill 写一条 trade
- `maker_order_index` / `taker_order_index` 通过 `orders[]` 反查成真实 `order_id`

### 5.3 `order_updates[]`

writer 对 `order_updates[]` 的使用：

- 更新 orders 表中的：
  - `status`
  - `remaining_qty_lots`
  - `remaining_spend_amount`
  - `updated_at`

### 5.4 `depth_updates[]`

writer 对 `depth_updates[]` 的使用：

- 不必落主表
- 优先写入 Redis 深度读模型
- 如需落库，可单独做 snapshot 表，不建议每次增量都写 PostgreSQL

---

## 6. Redis 读模型设计

Redis 的目标是：

1. 提供 HTTP 高频查询
2. 提供 websocket 断线重连后的快速补状态
3. 减少数据库查询压力

Redis 不是：

- 资金最终账本
- settlement 数据源

### 6.1 盘口深度

key：

```text
l2:depth:{market_id}
```

类型：

- `HASH`

field 格式：

```text
bid:{price_tick}
ask:{price_tick}
```

value：

- 该价位总量

示例：

```text
HSET l2:depth:1001 bid:60 700
HSET l2:depth:1001 ask:63 1200
```

### 6.2 用户 open orders 索引

key：

```text
user:orders:{wallet_address}
```

类型：

- `ZSET`

member：

- `order_id`

score：

- 推荐用订单创建时间或首次出现 sequence

用途：

- 查某个用户的 open orders 列表

### 6.3 订单详情

key：

```text
order:info:{order_id}
```

类型：

- `HASH`

建议字段：

```text
market_id
wallet_address

original_action
original_outcome
original_price_tick

normalized_side
normalized_price_tick
order_type

initial_qty_lots
initial_spend_amount
remaining_qty_lots
remaining_spend_amount

status
nonce
expire_time
updated_at
```

说明：

- 已关闭订单不必永久保留
- 推荐保留短 TTL，例如 1 小时，便于刚完成订单的用户侧查询

### 6.4 用户持仓快照

key：

```text
position:{market_id}:{wallet_address}
```

类型：

- `HASH`

字段：

```text
yes_free_lots
yes_locked_lots
no_free_lots
no_locked_lots
collateral_free_units
collateral_locked_units
updated_at
```

### 6.5 最新成交列表

key：

```text
trades:latest:{market_id}
```

类型：

- `LIST`

value：

- 每条 trade 的 JSON

推荐只保留最近 N 条：

- `N = 100` 或 `200`

### 6.6 价格历史

key：

```text
price:history:{market_id}
```

类型：

- `ZSET`

score：

- `executed_at_unix_ms`

member：

- price point JSON

建议：

- 定时裁剪窗口
- 控制最大点数

---

## 7. writer 对 Redis 的更新规则

### 7.1 处理 `depth_updates[]`

对每个深度事件：

- `total_volume == 0`
  - 删除对应 field
- `total_volume > 0`
  - 直接 `HSET`

### 7.2 处理 `orders[]`

当订单首次进入批次：

- 写 `order:info:{order_id}`
- 若状态仍可打开，则加入 `user:orders:{wallet}`

### 7.3 处理 `order_updates[]`

当订单状态变化：

- 更新 `remaining_qty_lots`
- 更新 `remaining_spend_amount`
- 更新 `status`
- 更新 `updated_at`

若状态变为：

- `filled`
- `canceled`
- `expired`
- `rejected`

则：

- 从 `user:orders:{wallet}` 中移除
- 对 `order:info:{order_id}` 设置短 TTL

### 7.4 处理 `fills[]`

对每笔成交：

- `LPUSH trades:latest:{market_id}`
- `ZADD price:history:{market_id}`

并做裁剪：

- `LTRIM trades:latest:{market_id} 0 N-1`
- `ZREMRANGEBYRANK` / `ZREMRANGEBYSCORE`

### 7.5 处理 positions

如果 writer 继续维护 positions 投影：

- 根据 fill 和状态变化增量更新数据库中的 positions
- 再回写到 Redis `position:{market_id}:{wallet}`

推荐保留“以数据库为准，再同步 Redis”的主从关系。

---

## 8. pusher 的职责边界

### 8.1 pusher 负责什么

pusher 负责：

- 直接消费 matcher v2 事件
- 把其中适合实时广播的部分转成 websocket 消息

pusher 使用：

- `fills[]`
- `order_updates[]`
- `depth_updates[]`

### 8.2 pusher 不负责什么

pusher 不负责：

- 查数据库后再拼接实时消息
- 主导 Redis 更新
- 组织链上信息

### 8.3 为什么 pusher 直接消费 matcher

因为实时广播的首要目标是低延迟：

- matcher 一产生成交，就应该尽快推给客户端
- 不应该再经过 writer 落库成功这个同步环节

writer 失败与否，不应阻塞用户看到最新盘口和成交。

---

## 9. WebSocket 频道设计

建议保留两类 websocket：

### 9.1 市场频道

URL：

```text
/ws/markets/{marketId}
```

用途：

- 订阅某个 market 的：
  - depth 增量
  - 成交增量

### 9.2 用户频道

URL：

```text
/ws/orders?ticket=...
```

用途：

- 订阅当前登录用户的订单状态更新

鉴权方式：

- 后端先发一次短期 websocket ticket
- 客户端再带 ticket 建立 websocket

这样可以避免：

- 直接把长期 Bearer token 暴露在 websocket URL

---

## 10. WebSocket 广播协议设计

为了让前端消费简单，建议所有 websocket 消息都采用统一 envelope。

```json
{
  "type": "market.depth.delta",
  "market_id": "1001",
  "ts": "2026-03-31T10:00:00Z",
  "payload": {}
}
```

推荐消息类型如下。

### 10.1 市场深度增量

```json
{
  "type": "market.depth.delta",
  "market_id": "1001",
  "ts": "2026-03-31T10:00:00Z",
  "payload": {
    "levels": [
      { "side": "bid", "price_tick": 60, "total_volume": 700 },
      { "side": "ask", "price_tick": 63, "total_volume": 1200 }
    ]
  }
}
```

字段说明：

- `levels`
  - 本次发生变化的档位列表

### 10.2 市场成交增量

```json
{
  "type": "market.trade.executed",
  "market_id": "1001",
  "ts": "2026-03-31T10:00:00Z",
  "payload": {
    "trade_id": "t-1",
    "maker_order_id": "11",
    "taker_order_id": "12",
    "price_tick": "60",
    "fill_amount": "300",
    "match_type": "transfer"
  }
}
```

建议字段：

- `trade_id`
- `maker_order_id`
- `taker_order_id`
- `price_tick`
- `fill_amount`
- `match_type`
- `executed_at`

其中：

- `price_tick = "60"` 表示 `0.60 USDC / share`
- `fill_amount = "300"` 表示 `3.00 shares`

是否带 maker/taker wallet：

- 市场频道默认可以带
- 如果担心前端不需要或隐私暴露，可不带

### 10.3 用户订单增量

```json
{
  "type": "user.order.updated",
  "market_id": "1001",
  "ts": "2026-03-31T10:00:00Z",
  "payload": {
    "order_id": "12",
    "status": "partially_filled",
    "remaining_qty_lots": "700",
    "remaining_spend_amount": "0",
    "refund_amount": "0"
  }
}
```

建议字段：

- `order_id`
- `status`
- `remaining_qty_lots`
- `remaining_spend_amount`
- `refund_amount`
- `updated_at`

其中：

- `remaining_qty_lots = "700"` 表示 `7.00 shares`
- `remaining_spend_amount = "250"` 表示 `2.50 USDC`

若事件中能直接拿到，也可以附带：

- `original_action`
- `original_outcome`
- `original_price_tick`

这样前端更新 open orders 更方便。

---

## 11. pusher 如何从 matcher v2 组消息

### 11.1 处理 `depth_updates[]`

一个 matcher batch 中可能有多条 depth update。

建议：

- 先按 `(side, price_tick)` 压缩为最新值
- 再一次广播一个 `market.depth.delta`

优点：

- 减少同一批次内重复档位消息
- 前端更容易一次 apply patch

### 11.2 处理 `fills[]`

每条 fill 单独广播一个：

- `market.trade.executed`

原因：

- 前端成交列表天然按条 append
- 用户对“每一笔成交”感知更直观

### 11.3 处理 `order_updates[]`

每条 user order update 单独广播一个：

- `user.order.updated`

并按 `wallet_address` 维度分房间发送。

---

## 12. Redis 与 WebSocket 的协同方式

### 12.1 为什么需要 Redis

WebSocket 只适合推增量，不适合做全量恢复。

Redis 用于：

- 用户打开页面时拉初始盘口
- 用户断线重连时快速恢复当前状态
- 用户刷新页面时查询 open orders / positions / trades

### 12.2 推荐前端时序

市场页推荐这样加载：

1. HTTP 读 Redis 读模型：
   - orderbook
   - trades
2. 建立 market websocket
3. 持续应用 depth / trade 增量

用户订单页推荐：

1. HTTP 读 Redis：
   - open orders
   - positions
2. 建立 user websocket
3. 持续应用 order update 增量

### 12.3 Redis 与 WS 的一致性容忍

允许出现短时间内：

- WS 已经推到
- Redis 还没更新

也允许反过来：

- Redis 已更新
- 某个 WS 客户端刚好错过一条消息

只要：

- Redis 最终可恢复
- WS 增量足够及时

整体体验就是稳定的。

---

## 13. pusher 房间模型

### 13.1 市场房间

```text
marketRooms[marketID] -> set(connection)
```

只广播：

- `market.depth.delta`
- `market.trade.executed`

### 13.2 用户房间

```text
userRooms[wallet] -> set(connection)
```

只广播：

- `user.order.updated`

### 13.3 背压处理

每条连接都要有有限 send queue。

推荐：

- 队列满时直接断开连接

不要：

- 让一个慢客户端拖垮整个房间广播

---

## 14. 消息数据结构定稿

为了和 matcher v2 对齐，建议新增 websocket 输出结构，而不是继续沿用旧的 `push.market.*` 结构作为唯一标准。

### 14.1 市场深度消息

```go
type WSMarketDepthDelta struct {
    Type     string                `json:"type"`
    MarketID string                `json:"market_id"`
    Ts       string                `json:"ts"`
    Payload  WSMarketDepthPayload  `json:"payload"`
}

type WSMarketDepthPayload struct {
    Levels []WSDepthLevel `json:"levels"`
}

type WSDepthLevel struct {
    Side        string `json:"side"`
    PriceTick   uint8  `json:"price_tick"`
    TotalVolume uint64 `json:"total_volume"`
}
```

### 14.2 市场成交消息

```go
type WSMarketTradeExecuted struct {
    Type     string               `json:"type"`
    MarketID string               `json:"market_id"`
    Ts       string               `json:"ts"`
    Payload  WSMarketTradePayload `json:"payload"`
}

type WSMarketTradePayload struct {
    TradeID      string `json:"trade_id"`
    MakerOrderID string `json:"maker_order_id"`
    TakerOrderID string `json:"taker_order_id"`
    PriceTick    uint64 `json:"price_tick"`
    FillAmount   uint64 `json:"fill_amount"`
    MatchType    string `json:"match_type"`
    ExecutedAt   string `json:"executed_at"`
}
```

### 14.3 用户订单消息

```go
type WSUserOrderUpdated struct {
    Type     string               `json:"type"`
    MarketID string               `json:"market_id"`
    Ts       string               `json:"ts"`
    Payload  WSUserOrderPayload   `json:"payload"`
}

type WSUserOrderPayload struct {
    OrderID               string `json:"order_id"`
    Status                string `json:"status"`
    RemainingQtyLots      uint64 `json:"remaining_qty_lots"`
    RemainingSpendAmount  uint64 `json:"remaining_spend_amount"`
    RefundAmount          uint64 `json:"refund_amount"`
    UpdatedAt             string `json:"updated_at"`
}
```

---

## 15. 实施顺序建议

### 第一步：writer 切 matcher v2

- 先让 writer 直接订阅 `evt.match.batch.v2.*`
- 保持 DB 投影与 Redis 投影先跑通

### 第二步：pusher 切 matcher v2

- 让 pusher 直接订阅 `evt.match.batch.v2.*`
- 按新 websocket envelope 广播

### 第三步：保留兼容期

兼容期内可以同时保留：

- 旧 push subject
- 新 websocket 结构

待前端完成切换后再收敛。

### 第四步：补重建工具

建议补一个 Redis rebuild 工具：

- 从 DB orders / trades / positions 重建 Redis 读模型

这样线上恢复会更稳。

---

## 16. 最终定稿

writer / pusher / Redis / websocket 的最终设计定为：

- `writer` 与 `pusher` 都直接消费 `evt.match.batch.v2.{market_id}`
- `writer` 负责：
  - DB 投影
  - Redis 读模型
- `pusher` 负责：
  - websocket 实时广播
- Redis 作为：
  - HTTP 查询加速层
  - websocket 断线恢复层
- websocket 分为：
  - market 房间
  - user 房间
- websocket 消息统一采用 envelope，类型至少包括：
  - `market.depth.delta`
  - `market.trade.executed`
  - `user.order.updated`

这样，整个系统形成清晰分层：

- matcher：业务结果生产
- writer：投影
- settlement：链上执行
- pusher：实时广播
