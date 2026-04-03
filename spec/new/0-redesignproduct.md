# BlinkPredict 当前代码基线下的重构产品方案

本文不是旧 spec 的补充，而是**以当前代码真实现状为基线**，重新定义下一轮重构的架构、边界、推进顺序与手测停点。

本文目标只有一个：

- 先把 `下单 -> 资金冻结/校验 -> 撮合 -> 事件投影 -> settlement 提交前置条件` 这条链路收成一条未来可扩展为多节点高可用的主线。

本文明确不追求：

- 一轮内把所有服务完全拆成独立仓库
- 一轮内把链上 / 链下状态漂移问题彻底消灭
- 一轮内实现完美的自动回滚与全量重放修复

本期原则是：

- 先收口权威边界
- 再迁移热路径
- 每一步都保留可手测状态
- 每一步都删掉已经被替代的旧逻辑，避免双轨运行太久

---

## 本轮恢复边界

本轮允许完成的恢复能力边界是：

- `matcher`
  - 继续从 `orders` 表中的活跃订单恢复 orderbook
  - 启动后执行 bootstrap tick，清理过期订单并重新进入可撮合状态
- `funds / shared wallet`
  - 从 `wallet_accounts` 恢复 `free / locked / pending usdc`
  - 从 `positions` 恢复 `free / locked / pending yes/no lots`
- `writer`
  - 继续依赖 `consumer_cursors` 防止 matcher batch 重复投影

本轮**不在这里实现**的内容：

- `matcher` 独立持久化 snapshot/checkpoint
- `funds` 独立持久化 snapshot/checkpoint
- 跨模块统一的“宕机瞬间精确恢复到最后一条已提交事件”的机制
- 脱离 Postgres 投影表、仅靠各模块自有 durable log + snapshot 恢复

原因很直接：

- 你现在的手测主目标仍然是 `下单 -> 撮合 -> settlement 前置条件`
- 如果同时上完整模块级 snapshot/checkpoint，会把本轮范围从“修主链路”扩大成“重做 Phase 7”
- 当前代码里 `matcher` 与 `funds` 还没有统一的模块级持久化抽象，强行一起做会增加不必要的迁移风险

因此，本轮的恢复目标定义为：

- **可手测恢复**
- **可重启后继续挂单/撮合**
- **资金与仓位读模型不再明显漂移**

而不是：

- **高可用拆服务后的完美热恢复**

后续若进入 Phase 7，再补：

- `funds_checkpoints / funds_snapshots`
- `matcher_checkpoints / matcher_snapshots`
- 模块级 replay 起点
- 模块独立重启顺序与跨模块 catch-up 协议

---

## 0. 当前代码现状摘要

当前主链路已经具备以下模块：

- `gateway/http`
  - 接收下单请求
  - 验签 / 标准化订单意图
  - 发布 `cmd.order.place`
- `matcher`
  - `market actor` + 内存 orderbook
  - 内置 `SharedWalletManager`
  - 消费 `cmd.order.place`
  - 产出 `evt.match.batch.v2.{market_id}`
- `writer`
  - 消费 matcher batch
  - 写 Postgres / Redis
- `pusher`
  - 消费 matcher batch
  - 推 websocket
- `settlement`
  - 消费 matcher batch
  - 组织链上 settlement 指令
- `webhook/deposit projector`
  - 消费 Helius webhook
  - 更新 `wallet_accounts`
  - 更新 Redis
  - 同步 matcher 的 shared wallet

当前最大问题不是某个实现细节，而是**权威边界错位**：

1. `gateway` 还在做余额硬校验，但它看到的是 `wallet_accounts/Redis/ATA`，不是撮合真正用的资金视图。
2. `matcher` 把资金状态内嵌在自己内部，但未来又要按 market 拆多节点，资金的一致性维度和 market 维度并不一致。
3. `writer` / `wallet_accounts` / `positions` / Redis 目前更像读模型，但历史上又承担了一部分“准入判断”的职责。
4. `ApplyLocalFill()` 当前把未结算资产过早放回 `available`，存在二次消费风险。
5. 代码里还遗留一批旧的 gateway 余额锁与 `order_locks` 思路，但 live 主路径已经不是靠它在运转。

结论：

- 当前系统不是不能继续演进
- 但继续在现状上加补丁，会把“理想的异步解耦”变成“多套余额真相长期并存”

因此，本次重构必须先回答一个问题：

**谁是撮合阶段唯一有资格决定“这个钱包还能不能继续下单”的模块？**

答案必须是：

**独立的 Funds 服务，而不是 gateway，也不是 market matcher，也不是 Redis。**

---

## 1. 重构后的目标架构

### 1.1 服务边界

重构后的逻辑服务边界如下：

1. `gateway`
- 职责：鉴权、验签、字段标准化、幂等校验、基础业务校验、发命令
- 不负责：最终余额判断、订单簿查询、链上状态解释

2. `funds`
- 职责：钱包级资金/持仓权威内存状态机
- 职责范围：
  - `available/locked/pending usdc`
  - `available/locked/pending yes/no lots`
  - deposit 增量
  - reserve / release / fill pending / settlement confirm
- 只按 `wallet_address` 分片，不按 market 分片
- 是撮合前唯一的资金准入权威

3. `matcher`
- 职责：市场级 orderbook 与撮合
- 只按 `market_id` 分片
- 只处理已经 reserve 成功的订单
- 不再自己持有全局资金真相

4. `writer`
- 职责：把 funds / matcher / settlement 的事件投影到 Postgres / Redis
- 只做 read model
- 不参与热路径准入

5. `query`
- 职责：对外 HTTP 查询 / WS 恢复快照
- 只读 Redis / Postgres
- 不读 matcher / funds 内存

6. `pusher`
- 职责：实时广播事件给前端
- 可直接消费 matcher/funds/settlement 事件
- 不负责全量查询

7. `settlement`
- 职责：消费 matcher batch，组织并提交链上 settlement
- 成功后发 settlement confirmed 事件给 funds / writer

### 1.2 pusher 与 query 的关系

逻辑上：

- `query` = 冷读 / REST 查询 / 重连后的快照来源
- `pusher` = 热增量 / websocket 实时广播

实现与部署上，必须区分两层：

- 代码模块层：现在就拆成两个独立模块
- 进程部署层：初期允许仍在同一个 backend 进程内装配

也就是说，短期可以是“同进程”，但不能再是“同一套混杂代码职责”。

建议从这一轮开始固定为：

- `internal/query`
  - 只提供只读用例
  - 只依赖 `Redis/Postgres/repository`
  - 不依赖 matcher actor 内存
  - 未来若拆微服务，可自然转成 gRPC/HTTP query service
- `internal/pusher`
  - 只消费事件并向 websocket 广播
  - 不承担冷查询与快照拼装
  - 可依赖 `query` 做断线恢复辅助，但不能反过来让 `query` 依赖 websocket 状态

部署上，在初期可以仍然是一个 backend 进程：

- 同一个进程里既装配 query API，也装配 websocket pusher
- 但必须通过不同模块入口与不同依赖边界实现

原因：

- 这次重构的核心难点是资金与撮合主链路，不是先把查询服务物理拆出来
- 若一开始就同时物理拆 `query` 与 `pusher`，会无谓增加迁移面

因此，短期推荐：

- 模块分层：`query` 与 `pusher` 分目录、分接口、分依赖
- 部署分层：先允许共进程

结论：

- 这轮就拆代码模块
- 这轮不强求拆独立进程
- 后续若拆微服务，`query` 是天然读服务，`pusher` 是天然推送服务

### 1.2.1 query / pusher 的目录结构与依赖禁止项

从这一轮开始，后端目录边界固定为：

```text
internal/query/
  service.go
  market_read_service.go
  order_read_service.go
  wallet_read_service.go
  repository.go
  dto.go

internal/pusher/
  service.go
  hub.go
  market_stream.go
  user_stream.go
  serializer.go
  ticket_store.go
```

允许的依赖方向：

- `http -> query`
- `http -> pusher`
- `pusher -> query`
- `query -> repository(redis/postgres)`
- `pusher -> protocol/nats/ws hub`

明确禁止的依赖方向：

- `query -> matching`
- `query -> funds`
- `query -> pusher`
- `pusher -> matching actor`
- `pusher -> funds` 的内存状态读取
- `http handler -> matching query engine`
- `http handler -> funds` 内部可变状态

明确禁止的实现方式：

- 不允许继续通过 `matching/query_engine.go` 直接读 matcher actor 内存对外提供查询
- 不允许 websocket 断线恢复依赖 matcher 当前内存快照
- 不允许为了“省一次查询”而让 pusher 直接拼接数据库查询职责
- 不允许把 `query` 写成另一个 writer；它只能读，不能承担事件写入

允许的短期耦合只有一种：

- `pusher` 在需要补快照时调用 `query` 的只读接口

但反向依赖仍然禁止：

- `query` 不能依赖 `pusher`

这条规则的目的不是形式主义，而是确保以后拆微服务时：

- `query` 可以自然拆成纯读服务
- `pusher` 可以自然拆成纯推送服务
- 当前单体实现不会提前把未来的网络边界耦死

### 1.3 NATS 主链路

本次修改不再做“兼容旧 subject 的增量修补”，而是明确要求：

- 全量重构现有 NATS subjects
- 全量重构各模块 consumer 定义
- 旧 `cmd.order.place` / `evt.match.batch.v2` 不再作为新架构基线
- 可以保留短期迁移代码，但文档与新代码都必须以新 subject 为准

新的主链路固定为：

- `gateway -> funds -> matcher -> settlement/writer/pusher`

### 1.3.1 NATS 选型总原则

这轮不允许“看起来都能用就先接上”，必须先把通信语义定死。

#### 1. 状态变更型消息统一使用 JetStream

包括：

- command
- 会驱动状态机推进的 event
- 需要可重放、可追 ACK、可恢复的 event

原因：

- 这些消息不能因为进程重启、订阅暂时不可用、瞬时网络抖动而丢失
- 单体宕机后的恢复，也必须依赖可持久化的消息日志
- 这些消息都需要明确 ack / retry / dead letter 策略

#### 2. 前端实时广播型消息可以使用 Core NATS fan-out

包括：

- 非权威的 websocket 增量广播
- 前端可丢弃、可通过 query 快照重建的通知类消息

原因：

- 这类消息不要求持久化回放作为系统权威
- 丢一条消息时，客户端可以用 query 快照恢复
- Core NATS 延迟更低、路径更轻

#### 3. 热路径不使用 Request-Reply

本轮明确禁止把下单、撮合、资金校验、settlement 主链路做成同步 Request-Reply。

原因：

- 会把服务边界重新耦回同步调用
- 会让未来的多节点扩展回到 RPC 耦合
- 会在高 TPS 下引入额外超时与级联故障面

Request-Reply 仅允许用于：

- 运维管理接口
- 调试命令
- 非热路径的只读探测

#### 4. 生产热路径统一使用 JetStream Pull + Durable

本轮默认规则：

- 生产状态机 consumer 使用 `JetStream Pull Consumer`
- consumer 必须是 `Durable`
- 不使用 `Ephemeral` 作为正式生产消费者
- 不使用 `Push Consumer` 承担核心状态机推进

原因：

- Pull 更容易做背压、批量获取、显式并发控制
- Durable 便于单体崩溃后继续从上次 sequence 恢复
- Push 在高并发下更容易把消费节奏交给 broker，难控
- Ephemeral 不适合恢复与审计

#### 5. Queue Groups 只用于无状态横向扩展，不作为权威恢复机制

可用场景：

- Core NATS 下的无状态 fan-out worker
- 非权威辅助消费者

不推荐场景：

- 核心资金状态机
- 核心撮合状态机
- 需要精确恢复位置的 consumer

原因：

- Queue Group 关注的是“谁来处理”
- 但本轮更关心的是“处理到了哪里、如何恢复”

### 1.3.2 本轮目标 subject 清单

#### Commands

- `cmd.order.submit.v1`
  - producer: `gateway`
  - consumer: `funds`
- `cmd.order.cancel.submit.v1`
  - producer: `gateway`
  - consumer: `funds`
- `cmd.deposit.confirmed.v1`
  - producer: `webhook/deposit projector`
  - consumer: `funds`
- `cmd.settlement.confirmed.v1`
  - producer: `settlement`
  - consumer: `funds`
- `cmd.settlement.failed.v1`
  - producer: `settlement`
  - consumer: `funds`

#### Events

- `evt.order.reserved.v1.{market_id}`
  - producer: `funds`
  - consumers: `matcher`, `writer`, `pusher`
- `evt.order.reserve_rejected.v1.{market_id}`
  - producer: `funds`
  - consumers: `writer`, `query projector`, `pusher`
- `evt.order.released.v1.{market_id}`
  - producer: `funds`
  - consumers: `writer`, `query projector`, `pusher`
- `evt.wallet.snapshot.updated.v1.{wallet}`
  - producer: `funds`
  - consumers: `writer`, `query projector`
- `evt.match.batch.v3.{market_id}`
  - producer: `matcher`
  - consumers: `writer`, `settlement`, `pusher`
- `evt.settlement.submitted.v1.{market_id}`
  - producer: `settlement`
  - consumers: `writer`, `query projector`, `pusher`
- `evt.settlement.confirmed.v1.{market_id}`
  - producer: `settlement`
  - consumers: `funds`, `writer`, `query projector`, `pusher`
- `evt.settlement.failed.v1.{market_id}`
  - producer: `settlement`
  - consumers: `funds`, `writer`, `query projector`, `pusher`

### 1.3.3 每类 subject 的消费形式

| subject 类型 | 形式 | consumer 类型 | 选择理由 |
| --- | --- | --- | --- |
| `cmd.*` | JetStream | Pull + Durable | 命令必须持久化、可 ack、可恢复、可限流 |
| `evt.order.*` | JetStream | Pull + Durable | 会推进 funds/matcher/writer 的状态机，不能丢 |
| `evt.match.batch.v3.*` | JetStream | Pull + Durable | 是 settlement/writer 的权威输入，必须可重放 |
| `evt.settlement.*` | JetStream | Pull + Durable | 会决定 pending 是否转 available，必须可恢复 |
| websocket 实时广播镜像 | Core NATS Pub/Sub Fan-out | 普通 Subscribe | 非权威、低延迟、允许客户端丢后重建 |
| 运维调试 rpc | Core NATS Request-Reply | request/reply | 只限非热路径管理操作 |

### 1.3.4 每个核心 consumer 的推荐模式

| 模块 | 订阅 subject | 形式 | 是否 Durable | 是否 Pull | 理由 |
| --- | --- | --- | --- | --- | --- |
| `funds` | `cmd.order.submit.v1` `cmd.order.cancel.submit.v1` `cmd.deposit.confirmed.v1` `cmd.settlement.*.v1` | JetStream | 是 | 是 | 资金状态机必须可恢复、可顺序控制 |
| `matcher` | `evt.order.reserved.v1.*` `evt.order.released.v1.*` | JetStream | 是 | 是 | 撮合输入不能丢，且要受控消费 |
| `writer` | `evt.order.*` `evt.match.batch.v3.*` `evt.settlement.*` `evt.wallet.snapshot.updated.v1.*` | JetStream | 是 | 是 | 写库投影必须可重放 |
| `settlement` | `evt.match.batch.v3.*` | JetStream | 是 | 是 | 结算必须可重试、可恢复 |
| `pusher` | 权威 event 的 JetStream 输入；对外广播使用 Core NATS 或直接 WS | JetStream + Core NATS | 是 | 是 | 内部消费要可靠，对外广播可轻量 |

### 1.3.5 明确禁止项

- 不允许新的核心状态机继续消费旧 `cmd.order.place`
- 不允许继续以 `evt.match.batch.v2` 作为新投影与新 settlement 的基线输入
- 不允许在核心状态机上使用 `Ephemeral Consumer`
- 不允许在资金、撮合、结算主链路上使用 `Push Consumer`
- 不允许为图省事把主链路重新改回同步 Request-Reply

---

## 2. 核心设计原则

### 2.1 资金与市场必须拆分维度

这是这次重构最重要的结论。

- 订单簿一致性维度：`market_id`
- 资金一致性维度：`wallet_address`

因此：

- `market actor` 负责撮合顺序
- `wallet actor` 负责资金顺序

不能再让 market actor 同时承担钱包级别的全局资金一致性。

### 2.2 先 reserve，再撮合

最终热路径必须固定为：

1. `gateway` 接收下单
2. `funds` 判断余额 / 持仓够不够
3. 若够：先把 `available -> locked`
4. 再发 `evt.order.reserved`
5. `matcher` 只处理已 reserved 的订单

这样可以保证：

- 同一钱包跨多个市场不会超花
- gateway 不需要自己当资金裁判
- matcher 不需要做跨市场余额同步

### 2.3 pending 资产不能提前回到 available

撮合后但未链上 settlement 前：

- 买到的 shares 进入 `pending_yes/no`
- 卖出所得进入 `pending_usdc`

只有 settlement confirmed 后，才能：

- `pending_yes/no -> available_yes/no`
- `pending_usdc -> available_usdc`

因此必须明确删除当前错误语义：

- `ApplyLocalFill()` 中把 pending 资产同步计入 available 的逻辑

### 2.4 Redis 不是热路径权威，只是查询权威

Redis 在新架构中的定位：

- 查询权威
- 快照权威
- 重启恢复辅助
- websocket 断线重连恢复源

Redis 不负责：

- 最终资金准入
- 撮合期并发锁
- 钱包级强一致决策

### 2.5 当前 webhook 只监听 global vault 的方向可以保留

但必须把记账条件改严：

- 不是“只要看到 token 进 `global_vault` 就记账”
- 而是“只要能确认这是 program `deposit` 成功导致的 inflow，才记账”

否则会出现：

- offchain 认为用户可用余额增加
- chain 上 `UserLedger.available_usdc` 没有增加
- settlement 最终失败

---

## 3. 统一状态模型

### 3.1 Funds 权威状态

Funds 服务内部统一维护：

```text
WalletLedger {
  available_usdc
  locked_usdc
  pending_usdc
  cancel_all_before_ts
}

MarketPosition {
  available_yes_lots
  locked_yes_lots
  pending_yes_lots
  available_no_lots
  locked_no_lots
  pending_no_lots
}
```

### 3.2 状态变更规则

#### deposit confirmed
- `available_usdc += amount`

#### buy reserved
- `available_usdc -= reserve`
- `locked_usdc += reserve`

#### sell reserved
- `available_yes/no -= qty`
- `locked_yes/no += qty`

#### order canceled / expired / rejected
- `locked -> available`

#### buy fill happened
- `locked_usdc -= reserved_part`
- 多锁部分退回 `available_usdc`
- 买到的份额进入 `pending_yes/no`

#### sell fill happened
- `locked_yes/no -= filled_qty`
- 卖出所得进入 `pending_usdc`

#### settlement confirmed
- `pending_yes/no -> available_yes/no`
- `pending_usdc -> available_usdc`

### 3.3 Writer / Redis / Postgres 的定位

投影层只镜像：

- 当前查询需要的数据
- 启动恢复需要的快照数据
- 审计与历史所需事件结果

投影层不应再承担：

- 真正的资金准入判断
- market matcher 的即时锁定逻辑

---

## 4. 当前代码需要删除或迁移的旧逻辑

这部分必须明确写，因为这次重构不是“继续加功能”，而是“替换错误职责”。

### 4.1 Gateway 侧应删除/降级的逻辑

现状问题：

- `precheckPlaceOrder()` 同时看 `wallet_accounts/Redis/ATA`
- 这会和 future funds 权威冲突

改造要求：

- gateway 只保留：
  - 市场是否 open
  - 下单字段是否合法
  - 签名是否正确
  - 用户身份是否匹配
- gateway 删除或降级为 soft check：
  - `wallet_accounts` 硬校验
  - ATA 余额 fallback 硬校验
  - 基于 DB/Redis 的最终下单拦截

### 4.2 Matcher 侧应迁移的逻辑

现状问题：

- `SharedWalletManager` 内嵌在 matcher
- `wallet.ReserveOrder()` 在 market actor 里直接执行

改造要求：

- 把 `SharedWalletManager` 迁出 matcher，提升为 funds 服务核心状态机
- matcher 不再消费 `cmd.order.place`
- matcher 改为消费 `evt.order.reserved`

### 4.3 Shared wallet 内部错误语义必须修正

现状问题：

- fill 后把未 settlement 资产提前写回 available

改造要求：

- 保留 `pending_*`
- 删除 “pending 同时记入 available” 的行为

### 4.4 Gateway Redis 锁余额逻辑必须退出主路径

现状问题：

- 代码仍有 `lockBalanceAtomic` / `persistOrderLock` / `order_locks`
- 但 live 主路径实际上已不靠它驱动

改造要求：

- 不要再把它重新拉回主路径
- 在 funds 服务成熟前，可暂时保留 `order_locks` 作为审计辅助
- 但它不再是主热路径的资金真相

### 4.5 wallet_accounts / positions 的职责要收口

改造要求：

- `wallet_accounts`：钱包级 read model / snapshot
- `positions`：市场级仓位 read model / snapshot
- 它们只做查询与恢复，不再做最终准入裁决

### 4.6 数据库表与 Redis 结构必须同步重构

本次修改中，数据库表结构与 Redis key 结构不允许继续沿用“先写代码、后补表”的方式，必须和模块边界一起重构。

明确要求：

1. 所有 read model / snapshot / recovery 相关表，都要重新审视职责与字段，不默认沿用旧表。
2. 新的建表语句、变更后的索引、约束、注释，要逐步统一落到 `Banckend/db/schema.sql`。
3. Redis 设计必须单独成文，统一落到 `Banckend/db/rediskey.md`。
4. 每引入一个新模块或新事件，就必须同时明确：
- PostgreSQL 表如何投影
- Redis key 如何组织
- 哪些是权威快照，哪些只是查询缓存
- TTL、回收策略、重建来源是什么

这条要求的目的，是避免再次出现：

- 代码主链路已经变了，但库表还是旧语义
- Redis 里堆了很多 key，却没人能说清楚哪个是权威、哪个只是缓存
- 启动恢复依赖某些表或 key，但结构里根本没有恢复所需字段

### 4.7 `Banckend/db/schema.sql` 的内容要求

`Banckend/db/schema.sql` 从这一轮开始应作为“新的统一 SQL 基线文件”，至少逐步覆盖以下内容：

- markets
- orders
- trades
- wallet_accounts
- positions
- user_position_accounts
- settlement_batches
- settlement_attempts
- funds_wallet_snapshots
- funds_position_snapshots
- consumer_checkpoints

每张表在加入 `DB/schema.sql` 时，必须同时给出：

- 主键
- 唯一约束
- 核心查询索引
- 恢复/扫描索引
- 与事件幂等相关的唯一键

明确禁止：

- 只加表不加索引
- 只加功能字段不写唯一约束
- 恢复依赖某字段，但 schema 中没有对应索引

### 4.8 `Banckend/db/rediskey.md` 的内容要求

`Banckend/db/rediskey.md` 必须按 key 维度写清楚以下信息：

- key 名称
- key 类型
  - string
  - hash
  - zset
  - stream
  - set
- value 结构
- 生产者
- 消费者/读取方
- 业务用途
- 是否权威
- TTL
- 重建来源

至少应逐步覆盖以下 key 族：

- 市场订单簿快照
- 市场最新成交
- K 线/价格历史缓存
- 用户 open orders
- 用户订单状态
- 用户钱包查询快照
- 用户市场仓位查询快照
- websocket ticket
- 幂等键 / 去重键
- query 层分页游标或辅助索引

明确禁止：

- 同一个业务对象在 Redis 中出现多套无文档定义的 key 命名
- 让 Redis 既当缓存、又当权威、又当锁，但文档里没有说明
- 没有重建来源的 Redis 权威语义

---

## 5. 推荐推进顺序

以下推进顺序按“每一步都能停下来手测”的原则设计。

---

## Phase 0：冻结旧 spec，建立新基线

### 目标

先明确：从这一轮开始，所有重构以当前代码为准，不再试图同时满足旧 spec 中已经偏离 live 代码的假设。

### 要做的事

1. 新建本文件，作为重构主控文档。
2. 标记旧 spec 2/3/4/6 中已经不再适合作为实现基线的段落。
3. 统一团队共识：
- 本轮先解资金与撮合边界
- 不先追求“全服务物理拆分”

### 手测停点

- 无代码改动
- 只确认团队理解与目标统一

---

## Phase 1：修正共享资金语义，但先不拆服务

### 目标

在不拆服务的前提下，先把**错误资金语义修正掉**，避免继续在错误状态模型上开发。

### 要做的事

1. 修正 `SharedWalletManager` 的 fill 语义：
- fill 后买到的 shares 只进 `pending_yes/no`
- fill 后卖出所得只进 `pending_usdc`
- settlement confirmed 之前不允许回到 `available`

2. 把共享资金状态的变更路径梳理成显式接口：
- `ApplyDepositConfirmed`
- `ReserveOrder`
- `ReleaseOrder`
- `ApplyMatchPending`
- `ApplySettlementConfirmed`

3. 删除/废弃当前隐式混杂逻辑：
- `ApplyLocalFill()` 里的“pending + available 双写”

4. 给 shared wallet 增加单元测试：
- 买单 reserve
- 买单部分成交
- 买单完全成交
- 卖单部分成交
- settlement confirmed
- cancel/release

### 本阶段不做

- 不拆 `funds service`
- 不改 NATS 主 subject
- 不动 gateway 入口协议

### 手测停点

手测目标：

1. deposit 后可下单
2. 买单部分成交后，未 settlement 资产不能再拿来下卖单
3. 卖单成交后，未 settlement 的收益不能再立即买入
4. cancel / expire 后 locked 释放正常

只有这四条都稳定，才进入下一阶段。

---

## Phase 2：把 gateway 的资金硬校验降级

### 目标

先消除 gateway 与 matcher/funds 的双裁判问题。

### 要做的事

1. 改造 `precheckPlaceOrder()`：
- 保留市场状态检查
- 保留字段和订单类型检查
- 移除基于 `wallet_accounts/positions/ATA` 的最终资金拦截

2. 把资金校验改成 soft hint：
- 如果 Redis 有快照，可返回前端提示信息
- 但不作为拒单最终依据

3. 调整 API 语义：
- gateway 返回的是“命令已接收”
- 最终是否 accepted/open/rejected 以异步事件为准

4. 更新前端交互：
- 接受 `202 accepted`
- 然后依赖 websocket / order status 更新

### 本阶段不做

- 不拆 matcher
- 不拆 funds

### 手测停点

手测目标：

1. 同一钱包快速连续下多笔买单
2. gateway 全部 `202 accepted`
3. 最终只有余额足够的单能进入 open，其余会在异步状态里变 rejected
4. 不再出现 gateway 说“能下”，但后续状态又与它的硬校验矛盾的体验

如果这一阶段体验不能接受，再回头讨论是否加 “soft precheck reason”，而不是重新加硬拦截。

---

## Phase 3：把 SharedWalletManager 抽成 Funds 模块，但先共进程

### 目标

先把代码职责拆开，再决定是否物理拆服务。

### 要做的事

1. 新建 `internal/funds` 模块。
2. 把当前 `SharedWalletManager` 与相关状态机逻辑迁入 `funds`。
3. 在当前单进程内，先通过明确接口/内部队列驱动 funds，而不是让 matcher 直接 new 一个 wallet manager。
4. 统一 funds 的输入输出模型：
- 输入：submit/reserve/release/fill/settlement confirmed/deposit confirmed
- 输出：reserved/rejected/released/snapshot updated

5. writer / webhook / settlement 直接依赖 `funds` 的事件定义，而不是继续依赖 matcher 内部结构。

### 本阶段不做

- 不要求物理独立进程
- 不要求立刻跨进程 gRPC

### 手测停点

手测目标：

1. 当前单进程里，funds 与 matcher 已经是代码上解耦的两个模块
2. funds 崩溃或关闭时，matcher 不能再自己偷偷接管资金判断
3. deposit -> reserve -> cancel/release 整条链路仍正常

通过后，才进入真正的异步拆链路。

---

## Phase 4：主链路改成 Gateway -> Funds -> Matcher

### 目标

把热路径切到未来架构上。

### 要做的事

1. gateway 改发：
- `cmd.order.submit.v1`
- 不再直接发给 matcher

2. funds 新增命令消费者：
- 消费 `cmd.order.submit.v1`
- 钱包级 reserve 成功后发 `evt.order.reserved.v1.{market_id}`
- reserve 失败后发 `evt.order.reserve_rejected.v1.{market_id}`

3. matcher 改为只消费：
- `evt.order.reserved.v1.{market_id}`

4. 取消路径同理：
- cancel submit -> funds release -> matcher/orderbook remove

5. 过渡期兼容：
- 保留旧 `cmd.order.place` 读路径，但仅作为 feature flag fallback
- 主链路切换完成后尽快删除旧入口

### 手测停点

手测目标：

1. 同一钱包跨两个市场同时下单，不会超花
2. reserve 失败时，订单不会进入 matcher
3. reserve 成功但 matcher 拒单时，释放逻辑完整
4. market actor 不再直接触碰钱包级权威资金状态

这一步通过后，系统主结构才算真正站稳。

---

## Phase 5：拆分 Matcher 读写职责

### 目标

把 matcher 彻底收缩成写服务。

### 要做的事

1. 明确 `query` 与 `pusher` 的代码边界。
2. 所有 HTTP 查询统一只读 Redis / Postgres。
3. 不允许前端 / API 高并发读取 matcher 内部状态。
4. 若保留 matcher 管理接口，只用于：
- debug
- 管理
- snapshot 导出

5. `pusher` 继续直接消费事件做增量广播；`query` 提供快照。

### 手测停点

手测目标：

1. 前端盘口、最近成交、用户 open orders 都只来自 Redis / query API
2. websocket 增量 + Redis 快照能完成断线恢复
3. matcher 重启不影响查询面继续工作

---

## Phase 6：收口 settlement 与 funds 的最终一致性交接

### 目标

把 “撮合成功但未 settlement” 的 pending 资产，真正闭环到 settlement confirmed 上。

### 要做的事

1. settlement 成功后发布 `evt.settlement.confirmed.v1`
2. funds 消费该事件：
- `pending_yes/no -> available_yes/no`
- `pending_usdc -> available_usdc`

3. settlement 失败时：
- 先保守处理为 pending 保持冻结
- 不自动回 available
- 交给重试或人工修复

4. webhook / 链上事件只负责“真实资产结果确认”，不直接改写撮合期语义

### 手测停点

手测目标：

1. 买单成交后，在 settlement 前不能卖出新得到的份额
2. 卖单成交后，在 settlement 前不能复用卖出所得
3. settlement 成功后资产马上进入 available
4. settlement 失败时不会导致资产被双花

---

## Phase 7：模块化单体下的独立恢复能力

### 目标

在仍保持单体部署的前提下，把每个核心模块做成“可独立恢复的状态单元”，为后续高可用与物理拆服务打基础。

这一阶段的设计理念固定为：

- 模块化单体
- 每个模块有自己的 consumer / checkpoint / snapshot
- 每个核心状态模块都能独立从 `durable log + snapshot + checkpoint` 恢复
- 恢复编排由单体 coordinator 负责，但恢复能力本身属于各模块，而不是属于 coordinator

这一阶段明确不是：

- 立刻把所有模块拆成独立进程
- 立刻引入跨服务恢复依赖
- 通过“启动时互相拉内存状态”来完成恢复

### 要做的事

1. 为 `funds` 增加：
- durable consumer
- snapshot 持久化
- checkpoint 持久化
- restart replay

2. 为 `matcher` 增加：
- durable consumer
- market 级 snapshot
- checkpoint 持久化
- restart replay

3. 为 `settlement` 增加：
- pending batch 恢复
- submitted / confirmed / failed 生命周期恢复
- checkpoint 持久化

4. 为 `writer` 增加：
- projection checkpoint
- 重启后补投影能力

5. 明确 `query` / `pusher` 的恢复边界：
- `query` 不承担权威状态恢复，只依赖 DB / Redis
- `pusher` 不承担权威恢复，重启后重新订阅并通过 query/Redis 协助客户端补快照

6. 由单体 `coordinator` 固定恢复顺序：
- 先恢复读模型基线
- 再恢复 funds
- 再恢复 matcher
- 再恢复 settlement
- 最后开放 gateway 写流量

### 手测停点

手测目标：

1. 整个 backend 单体重启后，funds 状态可恢复
2. 整个 backend 单体重启后，matcher 状态可恢复
3. settlement 未完成批次在重启后不会丢失语义
4. gateway 只会在关键模块 ready 后重新开放写流量
5. 同钱包跨市场并发下单依旧无超花

### 本阶段明确暂不做

- 不在这一阶段直接物理拆分 `funds/matcher/settlement`
- 不把“跨服务互拉状态”作为主恢复机制
- 不在业务主干尚未稳定前提前编写高可用编排代码

### 进入条件

`Phase 7` 只能在本文件第 `9` 节里列出的剩余核心业务问题先完成收口后，才开始真正进入代码实现。

原因：

- 如果市场生命周期、用户资产生命周期、claim/withdraw、projector 完整性、恢复语义本身都还没收口，就提前写高可用恢复逻辑，只会把未定型业务复杂度固化到基础设施层
- 高可用编排应该建立在稳定的业务状态机之上，而不是反过来替代业务设计

---

## 6. 第一轮实际建议只做到哪里

为了降低风险，第一轮建议只做到：

- `Phase 1`
- `Phase 2`
- `Phase 3`

也就是：

1. 先修正 pending/available 语义
2. 先把 gateway 的资金硬校验降级
3. 先把 funds 从 matcher 内部代码上抽出来，但仍然允许单进程运行

为什么先停在这里：

- 这是最小但必要的结构收口
- 能先把当前最危险的“未结算资产可复用”问题解决
- 能避免一口气同时改 subject、服务边界、部署形态
- 能在不破坏太多现有可跑代码的前提下，为 `gateway -> funds -> matcher` 铺路

只有 Phase 1-3 稳定通过手测后，再继续做 Phase 4。

---

## 7. 当前代码对应的改造落点

这一节只列关键落点，不追求面面俱到。

### 7.1 gateway

重点文件：

- `Banckend/internal/http/server.go`

需要改造：

- `precheckPlaceOrder()`
- 下单入口的命令发布逻辑
- 去掉对 `wallet_accounts/ATA` 的硬拒绝职责

### 7.2 funds

当前来源：

- `Banckend/internal/matching/shared_wallet.go`

需要迁移到：

- `Banckend/internal/funds/*`

### 7.3 matcher

重点文件：

- `Banckend/internal/matching/manager.go`
- `Banckend/internal/matching/book.go`

需要改造：

- 不再直接 `ReserveOrder()`
- 只消费 reserve 成功事件

### 7.4 webhook deposit

重点文件：

- `Banckend/internal/webhooks/helius.go`
- `Banckend/internal/webhooks/deposit_projector.go`

需要改造：

- 把“global vault inflow”与“program deposit confirmed”区分开
- 明确哪些事件有资格进入 funds `available_usdc`

### 7.5 writer / query / pusher

重点文件：

- `Banckend/internal/writer/writer.go`
- `Banckend/internal/pusher/service.go`

需要改造：

- 继续保留事件驱动
- 逐步消费 funds 事件
- query 逻辑与 pusher 逻辑分层

---

## 8. 这轮重构完成边界

如果本文件定义的 `Phase 0-6` 全部完成，则本轮明确视为“交易主干完成”。

本轮完成后，系统应当已经具备：

1. 用户提交限价单时，`gateway` 已完成签名校验、市场绑定校验、基础字段校验。
2. 撮合前资金准入只由 `funds` 决定，`gateway` 不再做最终余额裁判。
3. `matcher` 只处理已 reserve 成功的订单，不再直接维护钱包级权威余额。
4. 买到的份额、卖出的所得，在 settlement confirmed 前都只能停留在 `pending`，不能提前复用。
5. `writer` / Redis / Postgres 只承担投影与查询恢复职责，不再承担热路径强一致资金判断。
6. `query` 与 `pusher` 已经拆成两个独立代码模块，即使暂时共进程部署。
7. settlement 不再以“拿到 tx signature”视为成功，而是必须区分：
- submitted
- confirmed
- failed
8. 只有真实链上确认成功后，funds 才会把 `pending` 资产转回 `available`。
9. 针对“提交限价单 -> 链下撮合 -> 链上 settlement”的手动测试，系统已经具备一条语义自洽、可排障、可继续演进的主链路。

本轮完成后，系统明确不保证：

- 完整开奖与 claim 流程
- 完整 withdraw / fee withdraw 产品体验
- 完整 webhook 覆盖所有链上资产事件
- 完美重启恢复、全量 replay、自愈修复
- 多节点高可用部署

换句话说，本轮交付的是：

- 可测试的交易主干

而不是：

- 旧 spec 0 里定义的完整生产系统

## 9. 做完本轮后，离完整系统还差什么

相对于旧 [0-contractdesign.md](/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/0-contractdesign.md) 的完整目标，本轮完成后仍然至少还差以下内容：

### 9.1 市场全生命周期

- 市场创建的完整产品化流程
- Creator / Pyth 开奖闭环
- market close / account close
- 管理员级暂停、恢复、配置更新能力

### 9.2 用户资产全生命周期

- deposit 的完整观测、确认、对账与异常恢复
- withdraw 全流程
- `UserLedger` / `UserPosition` 初始化策略与失败恢复
- lamports 回收与账户 close 流程

### 9.3 结算后的权益兑现

- claim winnings
- creator fee withdraw
- platform fee withdraw
- resolution 后的资金归属完整闭环

### 9.4 webhook / projector / indexer 完整化

- 不只是 deposit
- 还包括 settlement 最终状态
- withdraw
- claim
- fee 变化
- resolution 结果

### 9.5 完整恢复与自愈

- funds 快照
- matcher 快照
- pending batch 恢复
- 冷启动 replay
- 数据校验与修复工具

### 9.6 真正的高可用与物理拆服务

- funds 按 wallet 分片
- matcher 按 market 分片
- query 独立服务
- pusher 独立服务
- writer 独立服务
- settlement 独立服务

这一项是建立在 `Phase 7` 已经完成之后的后续阶段，而不是当前阶段直接实现的内容。

也就是说，顺序固定为：

1. 先把核心模块做成模块化单体下可独立恢复的状态单元
2. 再考虑把这些模块物理拆成独立服务

因此，本轮与完整系统的关系应当明确理解为：

- 本轮解决“交易主干正确性”
- 后续阶段继续补“资产生命周期完整性”
- 最后才补“高可用与大规模部署形态”

## 10. 最终结论

这次重构不应该再问：

- “gateway 要不要读 matcher shared wallet？”
- “Redis 能不能直接继续当准入权威？”

而应该明确为：

1. **撮合前资金准入权威必须是 funds。**
2. **matcher 只做 market orderbook 与撮合。**
3. **Redis/DB 只做查询与恢复。**
4. **pusher 是增量广播层，query 是快照读取层；前期可共进程，逻辑上必须拆开。**
5. **推进顺序必须先修语义，再抽模块，再换主链路，最后再物理拆服务。**

如果后续实现仍继续在 gateway / matcher / Redis 三处同时保留资金判断，那么这次重构最终只会把系统做成“更多模块 + 更多漂移”，而不是“更可扩展”。
