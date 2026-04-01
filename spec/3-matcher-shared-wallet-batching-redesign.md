# BlinkPredict Matcher Shared Wallet + Market Batch 重构设计

本文是 BlinkPredict `matcher` 模块的专项设计文档，目标是在不改变 `0-contractdesign` 与 `2-order-placement-gateway-nats-redesign` 主约束的前提下，完成以下重构：

- 在 `matcher` 内引入 `shared wallet` 参与资金 / 持仓校验
- 使用**纯内存状态机**替代原有笨重的 Redis 锁思路
- 将 `matcher` 输出改造成可以被 `writer`、`settlement`、`pusher` **并行同步消费**的统一事件
- 明确 `matcher` 与 `settlement` 的职责边界，避免两边重复做 batch 组织
- 明确 market actor 内部的短窗口批处理策略，并把参数下沉到环境变量

本文只讨论：

1. `matcher` 如何消费 Gateway 发来的下单命令
2. `matcher` 如何与 `shared wallet` 配合做撮合前校验
3. `matcher` 如何在 market actor 内生成 market 级 batch
4. `matcher` 向下游发布的事件结构应该是什么
5. `writer` / `settlement` / `pusher` 应如何消费这些事件

本文不讨论：

- `shared wallet` 如何接收链上或其他服务的消息并刷新内存
- `settlement` 如何真正构造 Solana 交易、补 Ed25519 指令、提交上链
- `writer` 的数据库表结构迁移细节
- `pusher` 的前端 websocket 协议细节

---

## 0. 先说结论

本次重构后的主链路为：

1. 前端签名生成 `OrderIntentV1`
2. Gateway 做协议校验、字段拆分、生成内部下单命令，投递到 `cmd.order.place`
3. `matcher` 按 `market_id` 路由到对应 `market actor`
4. `market actor` 结合本地订单簿 + `shared wallet` 快照做纯内存校验与撮合
5. `market actor` 将短时间内的结果聚合为一个 **market batch**
6. `matcher` 发布 `market batch event`
7. `writer`、`settlement`、`pusher` 三方各自独立消费同一个 event stream

其中有两个看起来相似、但语义完全不同的 batch：

### 0.1 matcher batch

这是 `matcher` 输出的**业务事件批次**。

特点：

- 一定只包含**单个 market**
- 表达的是“这段极短时间内，这个市场发生了哪些撮合结果”
- 其核心内容是：
  - 去重订单表 `orders[]`
  - 成交对列表 `fills[]`
  - 订单状态增量 `order_updates[]`
  - 深度增量 `depth_updates[]`

### 0.2 settlement submission batch

这是 `settlement` 在消费 matcher batch 后，为了真正上链而组织的**提交批次**。

特点：

- 仍然只能是**单个 market**
- 需要考虑链上 tx 大小、compute、账户数量限制
- 可以把一个 matcher batch 再拆成 1 个或多个 submission batch
- 由 `settlement` 负责：
  - 拼 `SettleMatchBatchArgs`
  - 整理 `remaining_accounts`
  - 构造 ed25519 verify instruction
  - 提交与重试

因此：

- `matcher` 负责**业务批次组织**
- `settlement` 负责**链上提交批次组织**

`matcher` 不直接承担链上提交职责。

---

## 1. 与 0 号 / 2 号文档的一致性约束

本设计必须严格满足以下已拍板约束。

### 1.1 来自 0 号文档的核心约束

`0-contractdesign.md` 已经明确：

- 链下撮合完成后，后端按 market 聚合成 batch
- 链上结算接口最终需要：
  - `orders: Vec<OrderIntentV1>`
  - `fills: Vec<FillIndexPair>`
- `fill` 记录必须能够表达：
  - `maker_idx`
  - `taker_idx`
  - `fill_amount`
  - `fill_price`
- 链上分支只有三种：
  - `Match & Mint`
  - `Transfer`
  - `Merge & Burn`
- 链上仍然按**原始签名订单**校验，而不是按归一化后的内部视图校验
- 成本与手续费计算必须统一采用 **ceiling round up**

这意味着：

- `matcher` 的输出不能只保留松散 trade event
- 必须把原始订单与 fill 的索引关系带出来
- 不能要求 `settlement` 再从零反向推导一遍撮合语义

### 1.2 来自 2 号文档的核心约束

`2-order-placement-gateway-nats-redesign.md` 已经明确：

- Gateway 保留用户原始意图，不篡改原始签名订单
- Gateway 只负责将 `total_amount` 拆成：
  - `qty_lots`
  - `spend_amount`
- 是否做 `NO -> YES` 归一化，取决于 matcher 接口约定

这意味着：

- Gateway 输出给 matcher 的命令必须同时携带：
  - matcher 所需的最小业务字段
  - settlement 所需的签名透传字段
- `matcher` 内部可以只维护 YES book 视角
- 但下游 settlement 必须能基于透传的序列化字节恢复**完整原始订单**

### 1.3 精度约束

这一轮定稿后的内部精度统一为：

- `OriginalPriceTick` / `NormalizedPriceTick` / `FillPrice`
  - 都是 `1..99` 的价格 tick
- `QtyLots` / `RemainingQtyLots` / `FillAmount`
  - 都是 `shares * 100`
- `SpendAmount` / `RemainingSpendAmount`
  - 都是 `usdc * 100`
- 所有 `cost` / `reserve` / `fee`
  - 继续统一使用 ceiling round up

---

## 2. 重构目标

本次 matcher 重构的目标有五个。

### 2.1 目标一：移除 Redis 锁依赖

旧模型中，Gateway / Writer / Redis 锁共同参与“余额冻结”和订单生命周期管理，路径偏重，状态切换分散。

新模型要求：

- `matcher` 仅依赖内存中的 `shared wallet` 快照做校验
- 订单被接受、撮合、取消、部分成交时的可用余额变化在 actor 内即时反映
- 不再把 Redis 作为撮合临界区里的强一致锁设施

### 2.2 目标二：把 per-market 聚合前移到 matcher

由于 `matcher` 本身已经是 market actor 模型，因此：

- 单 market 的事件聚合最自然发生在 actor 内部
- 不应把 trade event 放给 settlement 再二次聚合

### 2.3 目标三：统一下游消费入口

当前架构偏向：

- matcher -> writer
- writer -> pusher

新架构要求：

- matcher -> event stream
- writer / settlement / pusher 同时消费

也就是说，writer 不再是 pusher 的前置依赖。

### 2.4 目标四：保留低延迟，避免过重缓存

market actor 内允许做**短窗口批处理**，但不能演化为重型缓冲队列。

要求：

- 默认只做毫秒级等待
- 以数量阈值优先、时间阈值兜底
- flush 后立刻发布

### 2.5 目标五：明确职责边界

必须避免：

- matcher 负责上链 tx 组织
- settlement 再次做完整业务解释

最终职责边界：

- matcher：业务语义最完整的“撮合结果生产者”
- settlement：链上提交执行器
- writer：DB / cache 投影器
- pusher：实时广播器

---

## 3. 模块职责划分

### 3.1 Gateway 职责

Gateway 负责：

- 鉴权与身份对齐
- 校验 market 是否可交易
- 校验字段合法性
- 生成 matcher 所需的最小业务字段
- 透传 settlement 所需的签名字段
- 生成执行字段：
  - `qty_lots`
  - `spend_amount`
  - `normalized_side`
  - `normalized_price_tick`
- 发布 `cmd.order.place`

Gateway 不负责：

- 查询 shared wallet
- 决定是否可最终成交
- 冻结 Redis 锁
- 组织链上结算 batch

### 3.2 matcher 职责

matcher 负责：

- 按 market 路由命令
- 使用 market actor 串行处理该 market 的全部命令
- 结合 shared wallet 快照做**内存校验**
- 更新订单簿
- 生成 fills / depth updates / order updates
- 在 actor 内做**短窗口 market batch 聚合**
- 发布统一下游事件

matcher 不负责：

- 查询链上账户 PDA
- 组织 Solana instruction
- 做 tx 重试
- 构造 `remaining_accounts`

### 3.3 shared wallet 职责

shared wallet 是 matcher 的一个内存依赖，但必须区分两层状态：

1. **base state**
   - 由链上事件 / webhook 驱动更新
   - 是链下内存中的 ground truth mirror
2. **optimistic state**
   - 由 matcher / relayer 在链下交易流程中直接修改
   - 用于挂单锁定、撮合后 pending、失败回滚、强撤修正

本期定稿采用的 shared wallet 结构就是把这两类状态合并存放在内存对象里：

- `available_*`
- `locked_*`
- `pending_*`

其中：

- `available`：当前链下可继续发起新意图的余额 / 份额
- `locked`：已被挂单占用但尚未撮合释放的量
- `pending`：已发生链下乐观变化、但仍等待链上 batch 最终确认的增减量

shared wallet 为 matcher 提供的是：

- 内存级余额 / 持仓校验
- 挂单锁定
- 撤单释放
- 撮合后 pending 记账
- webhook 到达后的对账抹平

shared wallet 不负责：

- 订单簿排序
- maker/taker 选择
- market batch 组织

### 3.4 writer 职责

writer 负责：

- 持久化 source order / fills / 状态更新
- 更新数据库投影
- 更新 Redis 读模型

writer 不负责：

- 给 settlement 组织上链 batch
- 作为 pusher 的唯一上游

### 3.5 settlement 职责

settlement 负责：

- 消费 matcher batch 中的：
  - `orders[]`
  - `fills[]`
- 将其转换成链上 `SettleMatchBatchArgs`
- 按实际 tx 限制拆 submission batch
- 查询和组织 `remaining_accounts`
- 提交链上交易

settlement 不负责：

- 从零 dedupe orders
- 从零恢复 maker/taker 索引关系
- 重新解释撮合分支作为主逻辑来源

### 3.6 pusher 职责

pusher 负责：

- 消费 matcher batch 中的：
  - `fills[]`
  - `order_updates[]`
  - `depth_updates[]`
- 直接广播 websocket

pusher 不负责：

- 读数据库后再拼事件
- 等 writer 成功后才能工作

---

## 4. Gateway -> matcher 的命令协议调整

为了让 matcher 能正确产出 settlement-ready 事件，当前下单命令必须升级为：

- matcher 可直接消费的**最小业务字段**
- settlement 需要的**签名透传字段**

matcher 本身不需要解析完整 `OrderIntentV1` 结构，也不需要理解全部链上签名细节。

推荐结构：

```go
type PlaceOrderCommandV2 struct {
    CommandID      string `json:"command_id"`
    TraceID        string `json:"trace_id"`
    IdempotencyKey string `json:"idempotency_key"`
    Timestamp      int64  `json:"timestamp"`

    MarketID  uint64 `json:"market_id"`
    MarketPDA string `json:"market_pda"`

    Execution PlaceOrderExecution `json:"execution"`

    Settlement SettlementPayload `json:"settlement"`
}
```

其中执行字段：

```go
type PlaceOrderExecution struct {
    OrderID              uint64 `json:"order_id"`
    WalletAddress        string `json:"wallet_address"`

    OriginalAction       string `json:"original_action"`
    OriginalOutcome      string `json:"original_outcome"`
    OriginalPriceTick    uint8  `json:"original_price_tick"`

    OrderType            string `json:"order_type"`
    NormalizedSide      string `json:"normalized_side"`
    NormalizedPriceTick uint8  `json:"normalized_price_tick"`
    QtyLots             uint64 `json:"qty_lots"`
    SpendAmount         uint64 `json:"spend_amount"`
    ExpireTime          int64  `json:"expire_time"`
    Nonce               uint64 `json:"nonce"`
}
```

签名透传字段：

```go
type SettlementPayload struct {
    IntentBytesHex string `json:"intent_bytes_hex"`
    Signature      string `json:"signature"`
}
```

### 4.1 为什么必须升级

如果只保留当前更旧的简化字段，matcher 无法稳定产出：

- `orders[]` 去重表
- `fills[]` 中的稳定 `maker_idx / taker_idx`
- settlement 需要的签名透传材料

但同时，matcher 也**不应该**为了 settlement 的需求去持有完整原始订单对象。

因此上游命令的正确拆法不是“完整 raw intent 全量展开”，而是：

- `execution`：给 matcher 用
- `settlement`：给 settlement 透传用

### 4.2 当前实际签名形式

当前订单签名对象统一描述为：

```text
utf8(hex(keccak256(serialize(OrderIntentV1))))
```

其中：

- 本节以**当前实际实现**为准；若与 0 号 / 2 号文档中的旧描述存在差异，应以后续统一修订后的签名规范为准
- `serialize(OrderIntentV1)` 在文档里可以继续近似称为 `borsh(OrderIntentV1)`
- 但必须特别注明：当前前端实际实现不是标准库直接调用的通用 Borsh 编码器，而是**按约定好的固定字段顺序与小端序手动序列化**
- 只要前后端、后端、链上对这个固定布局的理解一致，它在协议层就是有效的

因此，文档里的严谨写法建议统一为：

- “订单签名基于约定好的固定字段顺序和小端序编码的 `OrderIntentV1` 序列化结果”

如果需要沿用简写，也可以写作：

- `keccak256(borsh(OrderIntentV1))`

但需同时注明：

- “这里的 `borsh` 表示协议约定序列化布局，不强调具体前端实现必须调用标准 Borsh 库”

### 4.3 为什么签名字段只保留两个

在当前签名协议下，settlement 最少只需要两样材料：

1. `IntentBytesHex`
2. `Signature`

原因：

- `IntentBytesHex` 可以恢复 `OrderIntentV1` 的原始序列化字节
- `Signature` 是对签名对象的签名结果
- settlement 基于 `IntentBytesHex` 就可以自行计算：
  - `keccak256(serialize(OrderIntentV1))`
  - 再得到对应的 hex 文本
  - 再拼出最终被签名的 `utf8(hex(...))`

因此：

- `OrderHashHex` 不是必须字段
- 只要保留 `IntentBytesHex + Signature` 即可
- 如果未来为了日志、缓存或索引优化想冗余带 `order_hash_hex`，那也应视为可选优化，而不是协议必填字段

---

## 5. shared wallet 与 matcher 的协作模型

这里按本期定稿，明确采用：

- **内存乐观直接更新**
- **链上 webhook 异步对账**

也就是说：

- 交易意图类动作，允许链下立即改内存
- 真实资产型动作，必须等 webhook
- 所有链下乐观状态最终都要被链上事件确认或回滚

### 5.1 固定数据结构

`shared wallet` 的内存结构定稿如下。

#### 5.1.1 用户全局现金账本

```rust
struct UserWallet {
    pub available_usdc: u64,
    pub locked_usdc: u64,
    pub pending_usdc: i64,
    pub cancel_all_before_ts: i64,
}
```

字段语义：

- `available_usdc`
  - 当前可继续发起新下单意图的 USDC
- `locked_usdc`
  - 当前因挂买单而锁定的 USDC，包含协议要求的预估手续费占用
- `pending_usdc`
  - 已发生链下乐观变更、但仍等待链上 batch 最终确认的增减量
  - 允许为负，表示未来链上落账后需要减少
- `cancel_all_before_ts`
  - 用户链上一键全撤阈值的内存缓存

#### 5.1.2 用户市场持仓账本

```rust
struct MarketPosition {
    pub available_yes_shares: u64,
    pub locked_yes_shares: u64,
    pub pending_yes_shares: i64,

    pub available_no_shares: u64,
    pub locked_no_shares: u64,
    pub pending_no_shares: i64,
}
```

字段语义：

- `available_yes_shares`
  - 当前可继续卖出或参与后续链下撮合的 YES 份额
- `locked_yes_shares`
  - 已被挂 `Sell YES` 单锁定但未最终释放的份额
- `pending_yes_shares`
  - 已在链下撮合中乐观记账、等待链上确认的 YES 增减量
- `available_no_shares`
  - 当前可继续卖出或参与后续链下撮合的 NO 份额
- `locked_no_shares`
  - 已被挂 `Sell NO` 单锁定的份额
- `pending_no_shares`
  - 已在链下撮合中乐观记账、等待链上确认的 NO 增减量

#### 5.1.3 全局管理器

```rust
struct SharedWalletManager {
    ledgers: HashMap<Pubkey, UserWallet>,
    positions: HashMap<(Pubkey, Pubkey), MarketPosition>,
}
```

实现建议：

- 按 `User Pubkey` 或 `User Pubkey + Market Pubkey` 做 sharding 锁
- 避免一个全局 mutex 成为瓶颈
- 单 market actor 内仍维持串行处理，但 shared wallet 本身需要支持跨 market 并发访问

### 5.2 为什么这个模型可行

这个模型是可行的，但有前提。

可行的原因：

1. 你们的系统本来就是“链下撮合 + 链上最终结算”
2. 为了获得 CEX 级低延迟，链下必须允许：
   - 挂单即锁定
   - 撤单即释放
   - 撮合成功即进入下一轮可用视图
3. `pending_*` 字段天然提供了：
   - 乐观更新
   - webhook 对账
   - batch 失败回滚

它成立的前提是：

1. matcher 与合约使用**完全一致**的数学规则
2. 所有 fee 和 cost 都严格复刻链上 **ceiling round up**
3. relayer 必须能在 batch 成功 / 失败时提供内部反馈
4. 极端失败时允许触发用户级或 market 级资金快照重建

### 5.3 哪些更新严格依赖 webhook

以下动作不得由 matcher 抢跑记账，必须以链上事件为准。

#### 5.3.1 用户充值 `DepositSettled`

- 收到 webhook 后增加 `available_usdc`

#### 5.3.2 用户提现 `Withdrawn`

- 默认以 webhook 为准，从 `available_usdc` 扣减
- 若后续为了体验要在发起提现时先临时锁定，也只能算可选优化，不改变最终以 webhook 落账的原则

#### 5.3.3 主动铸造 / 合并 `SplitExecuted` / `MergeExecuted`

- 这属于一级市场操作
- 必须在 webhook 到达后更新：
  - `available_usdc`
  - `available_yes_shares`
  - `available_no_shares`

#### 5.3.4 领奖 `WinningsClaimed`

- 当前 v6 允许直接提钱包
- 该事件主要更新链下展示，不参与 matcher 可用余额计算

#### 5.3.5 手续费提取

- 由收益统计模块处理
- 不影响 shared wallet 撮合状态

### 5.4 哪些更新可在内存中直接进行

以下动作属于链下“意图类”操作，可以由 matcher 立即改内存。

#### 5.4.1 下限价单 / 市价单

##### Buy YES / Buy NO

- 校验：
  - `available_usdc >= max_cost + fee_ceiling`
- 通过后：
  - `available_usdc -= reserve`
  - `locked_usdc += reserve`

##### Sell YES

- 校验：
  - `available_yes_shares >= amount`
- 通过后：
  - `available_yes_shares -= amount`
  - `locked_yes_shares += amount`

##### Sell NO

- 校验：
  - `available_no_shares >= amount`
- 通过后：
  - `available_no_shares -= amount`
  - `locked_no_shares += amount`

极度重要：

- 内存中的 `cost` 和 `fee` 计算必须与链上完全一致
- 否则会出现链下通过、链上 batch 被 reject 的致命分叉

#### 5.4.2 用户链下撤单

- 直接把 `locked_*` 释放回 `available_*`

#### 5.4.3 matcher 本地撮合成功生成 fills

一旦撮合产出 fill，不应等链上确认后才更新链下可用视图。

推荐立即做：

- 卖方：
  - 扣减 `locked_yes_shares` 或 `locked_no_shares`
  - 将卖出所得记入 `pending_usdc`
- 买方：
  - 扣减 `locked_usdc`
  - 将获得份额记入 `pending_yes_shares` 或 `pending_no_shares`

对于 `Match & Mint` / `Transfer` / `Merge & Burn` 的差异，shared wallet 内部只需按资产增减结果处理，不必重复承担链上分支职责。

### 5.5 内存与 webhook 的协同对账

这是 shared wallet 最关键、也最容易出错的部分。

#### 5.5.1 批量结算成功 `MatchSettled`

当 relayer 提交的 batch 成功上链并收到 webhook：

- 清理对应订单 / fill 占用的 `pending_*`
- 将其正式抹平进最终可用状态
- 做一致性校验并归档

简化理解：

- 内存里已经先乐观走了一步
- webhook 到达后负责“确认这一步是对的”

#### 5.5.2 批量结算失败 `BatchFailed`

如果 relayer 发现：

- timeout
- dropped
- reverted
- 合约 reject

则必须向 shared wallet 发内部失败信号。

shared wallet 必须：

- 回滚此前 matcher 对该 batch 做的乐观更新
- 把应返还的资金与份额退回 `available_*`
- 撤销相应的 `pending_*`

如果失败后已经出现依赖这些未确认资产继续下出的后续订单，则必须：

- 级联作废这些脏订单
- 重新构建相关用户的资金快照

#### 5.5.3 用户链上强撤 / 一键全撤

收到链上的：

- `OrderCanceled`
- `CancelAllBeforeUpdated`

shared wallet 必须以最高优先级处理：

- 主动从订单簿中剔除相关订单
- 将对应 `locked_*` 强制释放回 `available_*`
- 更新 `cancel_all_before_ts`
- 拒绝该阈值之前的所有订单继续参与结算

### 5.6 推荐策略：乐观连环花费

本期推荐采用：

- **激进派 / 乐观连环花费**

即：

- 一旦撮合产生 fill
- 立刻释放旧 lock
- 立刻把新获得资产计入可继续交易的视图

这里有两种实现口径：

1. 保守口径
   - 先记入 `pending_*`
   - 下单校验时将 `pending_*` 纳入有效可用量
2. 激进口径
   - 直接把正向增量打入 `available_*`
   - `pending_*` 主要记录待对账差额

本设计更推荐第一种描述方式：

- 账面更清楚
- 回滚语义更明确
- 文档与实现更容易对齐

但如果实现层确认使用第二种，也必须保证：

- 失败回滚可精确按 batch 撤销
- 能级联清理依赖脏资产的后续订单

### 5.7 对 matcher 的接口建议

建议 matcher 依赖 shared wallet 的接口语义固定为：

```go
type SharedWallet interface {
    Snapshot(wallet string, marketID uint64) WalletSnapshot

    ReserveOrder(req ReserveOrderRequest) error
    ReleaseOrder(req ReleaseOrderRequest) error

    ApplyLocalFill(req ApplyLocalFillRequest) error
    ConfirmBatch(req ConfirmBatchRequest) error
    RollbackBatch(req RollbackBatchRequest) error

    ApplyChainEvent(req ChainEventRequest) error
}
```

其中：

- `ReserveOrder`
  - 下单后把资产从 `available_*` 移到 `locked_*`
- `ReleaseOrder`
  - 撤单 / 过期 / 强撤时释放 lock
- `ApplyLocalFill`
  - 产生 fill 后做链下乐观更新
- `ConfirmBatch`
  - 收到成功信号后清理 pending
- `RollbackBatch`
  - 收到失败信号后撤销乐观变更
- `ApplyChainEvent`
  - webhook 驱动的 base truth 更新

### 5.8 一致性原则

最终定稿为：

- shared wallet 的**真实资金来源**以链上事件 / webhook 为准
- matcher 允许对“意图类操作”直接做内存乐观更新
- `pending_*` 是链下高性能体验与链上最终一致之间的桥梁
- 所有乐观更新都必须能按 batch 精确确认或精确回滚
- 极端情况下允许通过 RPC 重建用户真实快照作为兜底

---

## 6. market actor 内部处理流程

### 6.1 为什么坚持 market actor

原因有三个：

1. market 内订单簿天然是单线程顺序语义
2. shared wallet 的校验和更新更容易与撮合同步
3. market 级 batch 聚合只需在 actor 内累积即可

### 6.2 单条命令处理流程

每个 `PlaceOrderCommandV2` 的处理顺序建议为：

1. 基础时效检查
2. 查询 / 获取 shared wallet 快照
3. 检查是否有足够余额或持仓
4. 在 shared wallet 中预留资产（`available_* -> locked_*`）
5. 创建 taker order
6. 与 order book 撮合
7. 每次产生 fill 时即时执行链下乐观更新（优先体现为 `pending_*` 变化）
8. 生成 fill / order update / depth update
9. 若有剩余：
   - limit 单进入簿
   - market 单取消剩余并释放剩余锁定
10. 把本次结果并入 actor 内的 pending market batch
11. 满足 flush 条件时发布 event

### 6.3 pending market batch 的定位

每个 actor 在内存里持有一个 `pending batch`，用于把极短时间内的结果合并。

它的目的不是“长期蓄水”，而是：

- 降低消息碎片化
- 让 settlement 更容易消费
- 减少 writer / pusher 的消息风暴

### 6.4 pending batch 的数据结构

建议：

```go
type PendingMarketBatch struct {
    MarketID   uint64
    MarketPDA  string
    StartedAt  int64
    LastAddAt  int64

    OrdersByKey   map[string]uint16
    Orders        []MatchedOrderV2
    Fills         []MatchFillV2
    OrderUpdates  []OrderUpdateV2
    DepthUpdates  []DepthUpdateV2

    SourceCmdSeqMin uint64
    SourceCmdSeqMax uint64
    SourceCommandIDs []string
}
```

`OrdersByKey` 用于去重。推荐去重 key：

- `wallet + market_pda + nonce`

这与链上 `OrderState` 的核心唯一性保持一致。

---

## 7. matcher 内部短窗口批处理策略

### 7.1 为什么要在 matcher 聚合

因为 `matcher` 是唯一同时知道这些信息的模块：

- 当前 market 内的时序
- 哪些订单参与过 fill
- 哪些 fill 属于同一次极短时间窗口
- 哪些深度更新其实可以压缩

因此 market batch 聚合应前移到 matcher。

### 7.2 flush 原则

推荐采用：

- 数量阈值优先
- 时间阈值兜底
- actor 空闲时尽量 flush

默认触发任一条件即 flush：

1. `fills` 数量达到阈值
2. `orders` 数量达到阈值
3. 估算 payload 字节数达到阈值
4. 距离 batch 开始时间超过阈值
5. 距离最近一次结果写入超过阈值

### 7.3 环境变量配置

以下参数全部落到环境变量中。

```env
MATCHER_BATCH_MAX_FILLS=64
MATCHER_BATCH_MAX_ORDERS=96
MATCHER_BATCH_MAX_BYTES=262144
MATCHER_BATCH_MAX_AGE_MS=40
MATCHER_BATCH_IDLE_FLUSH_MS=15
MATCHER_BATCH_FLUSH_TICK_MS=10
```

字段解释：

- `MATCHER_BATCH_MAX_FILLS`
  - 单个 matcher batch 允许累计的最大 fill 数
- `MATCHER_BATCH_MAX_ORDERS`
  - 单个 matcher batch 允许累计的最大去重订单数
- `MATCHER_BATCH_MAX_BYTES`
  - 估算序列化后 payload 的最大字节数，超过即提前 flush
- `MATCHER_BATCH_MAX_AGE_MS`
  - 从 batch 开始累计到现在的最长生存时间
- `MATCHER_BATCH_IDLE_FLUSH_MS`
  - 自最近一次添加结果后，空闲多久自动 flush
- `MATCHER_BATCH_FLUSH_TICK_MS`
  - actor 内部定时检查 flush 条件的 tick 周期

### 7.4 默认值选择理由

这些值偏保守，目的是：

- 先控制延迟
- 再控制吞吐
- 为 settlement 留出后续按链上限制二次拆分的空间

默认不建议把时间窗口拉到 100ms 以上，否则：

- pusher 延迟会明显感知
- settlement 上链延迟也会被放大

### 7.5 失败处理

如果 matcher 已经构造好 batch，但发布到 NATS 失败：

- 当前 flush 不算完成
- 不应清空 pending batch
- 允许重试发布
- 对应命令的 ACK 不能提前释放

这样可以保持“事件未发布成功，就不算命令处理完成”的原则。

---

## 8. 撮合数学与价格语义

### 8.1 内部继续维护 YES book 视角

matcher 内部仍然可以只维护 YES 视角订单簿：

- YES 单按原语义进簿
- NO 单在 Gateway 或 matcher 中归一化为 YES 视角

但下游事件必须同时保留：

- 原始语义
- 执行语义

### 8.2 fill price 的语义

`fills[].fill_price` 推荐统一记录为：

- **归一化后的 YES price**

理由：

- 订单簿内部一致
- 与当前 matcher 价格档位实现一致
- writer / pusher 可以直接展示盘口成交价

而每个用户真实的经济语义，由原始订单 `side/outcome` 再解释。

### 8.3 match type 必须显式写出

虽然链上理论上可以根据双方原始订单重新推导：

- `Match & Mint`
- `Transfer`
- `Merge & Burn`

但 matcher event 里仍建议显式带出：

- `match_type`

原因：

- writer 不必重复推导
- settlement 可把它作为主要输入，并做一次校验
- debug 更直观

### 8.4 ceiling round up 约束

`0-contractdesign` 已经明确要求：

- 成本与手续费统一用 ceiling round up

因此 matcher 在以下计算中必须升级为向上取整口径：

- `cost_for_lots`
- `max_fill_qty_for_spend`
- taker fee
- creator fee
- platform fee

不允许继续使用简单向下整除，否则 shared wallet 内存扣减会与链上 settlement 不一致。

---

## 9. matcher 向下游发布的统一事件

### 9.1 Subject 设计

推荐新增统一 subject：

- `evt.match.batch.v2.{market_id}`

三方消费者分别使用独立 durable / queue group：

- writer consumer group
- settlement consumer group
- pusher consumer group

这样它们彼此独立，不相互阻塞。

### 9.2 顶层结构

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

### 9.3 `orders[]`

`orders[]` 是本批次参与过：

- 成交
- 新挂单
- 状态变化

的去重订单列表。

```go
type MatchedOrderV2 struct {
    OrderIndex uint16 `json:"order_index"`
    OrderID    uint64 `json:"order_id"`

    Execution  ExecutionSnapshotV2 `json:"execution"`
    Settlement SettlementPayload   `json:"settlement"`

    CreatedAt int64 `json:"created_at"`
}
```

字段语义：

- `order_index`
  - 本批次内稳定索引，供 `fills[]` 引用
- `order_id`
  - 后端内部雪花订单号，便于投影与日志
- `execution`
  - matcher 内部执行视图，供 writer/pusher/排障使用
- `settlement`
  - settlement 专用透传字段；matcher 不必解释其中签名逻辑
- `created_at`
  - 原始命令时间戳

### 9.4 `fills[]`

`fills[]` 是 settlement 的核心输入。

```go
type MatchFillV2 struct {
    FillIndex uint32 `json:"fill_index"`

    MakerOrderIndex uint16 `json:"maker_order_index"`
    TakerOrderIndex uint16 `json:"taker_order_index"`

    FillAmount uint64 `json:"fill_amount"`
    FillPrice  uint64 `json:"fill_price"`

    MatchType string `json:"match_type"`

    NotionalUnits    uint64 `json:"notional_units"`
    TakerFeeUnits    uint64 `json:"taker_fee_units"`
    CreatorFeeUnits  uint64 `json:"creator_fee_units"`
    PlatformFeeUnits uint64 `json:"platform_fee_units"`
}
```

字段语义：

- `fill_index`
  - 本批次 fill 的局部序号
- `maker_order_index`
  - 指向 `orders[]` 中 maker 的索引
- `taker_order_index`
  - 指向 `orders[]` 中 taker 的索引
- `fill_amount`
  - 本次成交份额数量，单位是 `shares * 100`
- `fill_price`
  - 归一化后的 YES 价格 tick，范围 `1..99`
- `match_type`
  - `match_mint | transfer | merge_burn`
- `notional_units`
  - 该 fill 对应的名义金额，便于 writer / settlement 对账
- 各类 `fee_units`
  - 已按同一口径算出的费用结果

### 9.5 `order_updates[]`

```go
type OrderUpdateV2 struct {
    OrderIndex uint16 `json:"order_index"`

    Status string `json:"status"`

    RemainingQtyLots     uint64 `json:"remaining_qty_lots"`
    RemainingSpendAmount uint64 `json:"remaining_spend_amount"`
    RefundAmount         uint64 `json:"refund_amount"`

    ReasonCode string `json:"reason_code,omitempty"`
}
```

字段语义：

- `status`
  - `new | partially_filled | filled | canceled | expired | rejected`
- `remaining_qty_lots`
  - 订单剩余份额量，单位是 `shares * 100`
- `remaining_spend_amount`
  - 对 market buy 特别有用，表示剩余可花金额，单位是 `usdc * 100`
- `refund_amount`
  - 因取消、过期、滑点保护未用完等需要释放给用户的锁定量
- `reason_code`
  - 可选，用于 rejected / canceled 的原因

### 9.6 `depth_updates[]`

```go
type DepthUpdateV2 struct {
    Side        string `json:"side"`
    PriceTick   uint8  `json:"price_tick"`
    TotalVolume uint64 `json:"total_volume"`
}
```

字段语义：

- `side`
  - `bid | ask`
- `price_tick`
  - 受影响的档位，范围 `1..99`
- `total_volume`
  - 该档位最新总量，单位是 `shares * 100`

### 9.7 订单去重规则

一个 batch 中，同一订单不允许在 `orders[]` 出现多次。

建议去重优先级：

1. `wallet + market_pda + nonce`
2. 若已有内部 `order_id`，则用 `order_id`

一旦某订单被分配 `order_index`，在整个 batch 生命周期内不可变化。

---

## 10. 下游消费规范

### 10.1 writer 如何消费

writer 直接订阅 `evt.match.batch.v2.*`。

writer 使用：

- `orders[].execution`
  - 首次写入 source order / open order 投影
- `fills[]`
  - 持久化成交记录
- `order_updates[]`
  - 更新订单状态
- `depth_updates[]`
  - 更新 L2 读模型

writer 不需要再做：

- 从 trade event 中反向 dedupe order
- 从 maker/taker 签名串中找原始订单

### 10.2 settlement 如何消费

settlement 直接订阅 `evt.match.batch.v2.*`。

settlement 只关心：

- `orders[].execution` 中的最小业务语义
- `orders[].settlement`
- `fills[]`

其处理流程：

1. 基于 `orders[].settlement.intent_bytes_hex` 恢复 `OrderIntentV1`
2. 基于 `orders[].settlement.signature` 组织签名验证材料
3. 将恢复后的订单映射为 `Vec<OrderIntentV1>`
4. 将 `fills[]` 映射为 `Vec<FillIndexPair>`
5. 若一个 matcher batch 太大，则按链上限制拆成多个 submission
6. 查询本次 submission 需要的：
   - `UserLedger`
   - `UserPosition`
   - `OrderState`
7. 构造链上交易并提交

settlement 可以校验但不主导推导：

- `match_type`
- `fill_price`
- 订单索引合法性

### 10.3 pusher 如何消费

pusher 直接订阅 `evt.match.batch.v2.*`。

pusher 使用：

- `fills[]`
  - 推市场成交流
- `order_updates[]`
  - 推用户订单状态
- `depth_updates[]`
  - 推盘口深度增量

pusher 不再依赖 writer 先把 push 消息二次发布到其他 subject。

### 10.4 为什么三方直接消费更合理

原因：

- 降低链路耦合
- writer 失败不阻塞 pusher
- pusher 实时性更高
- settlement 不依赖 writer 的二次加工

---

## 11. matcher 与 settlement 的边界定稿

这是本次设计里最容易混淆、但必须说死的一部分。

### 11.1 matcher 做什么

- 按 market 串行处理命令
- 和 shared wallet 协同做链下可用性检查
- 做纯内存撮合
- 生成去重 `orders[]`
- 生成 `fills[]`
- 生成 `order_updates[]`
- 生成 `depth_updates[]`
- 以短窗口聚合为 `matcher batch`

### 11.2 matcher 不做什么

- 不构造 `SettleMatchBatchArgs` 作为最终链上提交实体
- 不查询 `remaining_accounts`
- 不控制单笔链上 tx 大小
- 不提交链上交易

### 11.3 settlement 做什么

- 消费 matcher batch
- 把 matcher batch 转成 submission batch
- 按链上限制切分
- 准备账户
- 提交上链

### 11.4 settlement 不做什么

- 不重新解释整个撮合过程
- 不从散落 trade event 中重建订单表
- 不重新生成 maker/taker 索引作为主逻辑

### 11.5 一句话总结边界

matcher 负责：

- **把业务结果组织对**

settlement 负责：

- **把链上交易组织对**

---

## 12. 实现建议

### 12.1 代码层重构顺序建议

建议按以下顺序推进。

#### 第一步：升级命令结构

先把 Gateway -> matcher 的命令升级成 `PlaceOrderCommandV2`，保证：

- matcher 拿到最小业务语义
- settlement 拿到必要签名透传字段

#### 第二步：抽象 shared wallet 接口

在 matcher 内新增 shared wallet adapter，但先不关心它背后的消息同步来源。

#### 第三步：改造 MemoryOrder

`MemoryOrder` 需要同时保留：

- 原始语义
- 执行语义
- shared wallet 预留/消费所需字段

#### 第四步：引入 pending market batch

在每个 market actor 上新增：

- `pendingBatch`
- `flush ticker`
- `maybeFlush()`

#### 第五步：新增 `evt.match.batch.v2`

在 matcher 发布新 subject，并让 writer / settlement / pusher 直接订阅它。

#### 第六步：逐步下掉旧链路

旧的：

- writer -> push subject

可在新链路稳定后再移除。

### 12.2 向后兼容策略

建议在迁移期保留双发：

- 新事件：`evt.match.batch.v2.{market_id}`
- 旧事件：如现有 `evt.trades.{market_id}` 仍保留短期兼容

待下游三方完成切换后，再移除旧事件。

---

## 13. 风险与注意事项

### 13.1 shared wallet 不是最终账本

必须始终记住：

- shared wallet 只是链下可用性视图
- 最终权益仍以链上成功结算为准

因此 settlement 仍然需要面对链上失败、账户缺失、状态变化等现实问题。

### 13.2 actor 内存批次不能过大

如果 batch 阈值配太高，会导致：

- websocket 延迟变高
- 事件体过大
- 单次失败重发开销过大

### 13.3 数学口径必须统一

Gateway、matcher、writer、settlement、合约必须统一：

- 精度
- 价格语义
- ceiling round up 规则

否则 shared wallet 预留和链上实际扣减会出现偏差。

### 13.4 索引稳定性非常关键

`orders[]` 的顺序和 `fills[]` 的索引引用一旦生成，整个 batch 生命周期不可再变化。

否则 settlement 会提交错误的 maker/taker 配对。

---

## 14. 最终定稿

本次 matcher 模块重构的最终设计定为：

- matcher 继续采用 **per-market actor**
- matcher 引入 **shared wallet** 做链下可用性检查、乐观锁定与 webhook 对账
- matcher 使用**短窗口 market batch 聚合**
- 聚合参数由环境变量控制
- matcher 输出统一事件 `evt.match.batch.v2.{market_id}`
- 事件中直接包含 settlement 需要的：
  - `orders[]`
  - `fills[]`
- 以及 writer / pusher 需要的：
  - `order_updates[]`
  - `depth_updates[]`
- writer / settlement / pusher 同步独立消费，不再串联
- settlement 仍然保留链上 submission 的最终控制权

这套方案的核心收益是：

- 撮合路径更轻
- 状态边界更清晰
- 下游耦合更低
- 更贴近 0 号文档要求的链上 batch 形状
- 为 shared wallet + 内存撮合方案提供了稳定基础
