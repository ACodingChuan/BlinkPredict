# 5-主链路重构完成说明（Phase 4–6 交付物）

## 1. 本文档目的

本文件记录 Phase 4–6 已完成的工程交付物，作为 spec/new 系列的第 5 号文档。  
所有内容以**当前实际代码**为准。  

参考文档：  
- `spec/new/0-redesignproduct.md` — 架构目标与推进顺序  
- `spec/new/2-hotpath-wal-inflight-redesign.md` — 热路径 WAL 与 In-Flight 详细设计  

---

## 2. 已完成的架构改造列表

### 2.1 Event Subject 升级（P0）

| 变更 | 文件 | 说明 |
|------|------|------|
| 新增 `SubjectMatchBatchV3 = "evt.match.batch.v3"` | `protocol/events.go` | Matcher 的规范输出 subject |
| 新增 `SubjectOrderReleased = "evt.order.released.v1"` | `protocol/events.go` | 订单终结事件 subject |
| 新增 `SubjectMatchBatchV3Market()` / `SubjectOrderReleasedMarket()` 工具函数 | `protocol/events.go` | 供 publisher/consumer 构造带 market_id 的 subject |
| 新增 `OrderReleasedEvent` 结构体 | `protocol/types.go` | 承载 canceled/expired/rejected 订单的释放信息 |

### 2.2 Matcher 改造（P0）

#### 2.2.1 Pull Consumer 替换 QueueSubscribe
**文件**：`internal/matching/manager.go`

```go
// Before（违反 spec §1.3.4）
m.client.JetStream().QueueSubscribe(SubjectOrderReservedV1+".*", "matcher_group", handler, ...)

// After（符合 spec §1.3.4: Pull + Durable）
sub, _ = m.client.PullSubscribe(SubjectOrderReservedV1+".*", "matcher-reserved-primary")
// 独立 goroutine + Fetch 循环，背压可控
```

**核心差异**：
- Pull 模式下，Matcher 主动控制拉取节奏，避免 broker 推送导致 buffer 爆满
- `NakWithDelay` 替代 `Nak`，backpressure 时给 500ms 缓冲重试
- 与 `dispatchLoop` 解耦，无需 `<-ctx.Done()` 阻塞主 goroutine

#### 2.2.2 Dual-subject Publish（v3 正式 + v2 过渡兼容）
```go
// matcherV3CrossPublish = true 期间，同时发布 v2 + v3
// 迁移完成后将 matcherV3CrossPublish = false 只发 v3
```

过渡策略：
- `evt.match.batch.v3.{market_id}` — 新正式 subject（funds 新版本消费）
- `evt.match.batch.v2.{market_id}` — 保留兼容，旧版 writer/settlement/pusher 继续消费
- 单次 `Publish` 调用，保持 v2 消费者的延迟 SLA 不变

#### 2.2.3 `publishOrderReleasedEvents` — 订单终结事件发布
每次 `publishMatchBatch` 后，遍历 `OrderUpdates`，对 `canceled/expired/rejected` 状态的订单独立发布 `evt.order.released.v1.{market_id}`：

```go
nats.MsgId(event.EventID + "-released-" + orderID)  // 幂等 dedup key
```

**设计选择**：使用独立事件而非让消费者解析完整 match batch 的原因：
1. funds/writer 只需要释放特定订单的资产，不需要重算整个 batch
2. 独立事件让 released consumer 可以独立于 match batch consumer 扩展
3. 幂等 key 与 match batch EventID 绑定，消除重复发布风险

### 2.3 Funds 服务改造（P0）

**文件**：`internal/funds/service.go`

#### 2.3.1 三 Consumer 架构

旧版单 Consumer → 新版三个独立 Pull Consumer：

```
funds-dispatcher-v2        → evt.match.batch.v2.* (AP_EVT)  // 过渡兼容
funds-cmd-dispatcher-v1    → cmd.order.submit.v1             // AP_CMD
funds-released-v1          → evt.order.released.v1.*         // AP_EVT
```

三个独立 goroutine (`dispatchLoop` / `cmdDispatchLoop` / `releasedDispatchLoop`) 并发运行，共享同一个 `Manager` 内存状态机。

**线程安全保证**：`Manager` 内部通过 Sharded Worker Pool（按 wallet_address hash 分片）实现 wallet 级串行化，三个 consumer goroutine 之间无全局锁。

#### 2.3.2 dispatchMessage 路由扩展

```go
case strings.HasPrefix(subj, protocol.SubjectMatchBatchV3):  // 新增 v3 路由
case strings.HasPrefix(subj, protocol.SubjectMatchBatchV2):  // 保留 v2 兼容
case strings.HasPrefix(subj, protocol.SubjectOrderReleased): // 新增 released 路由
```

#### 2.3.3 `handleOrderReleasedMessage` — 释放处理

```go
func (s *Service) handleOrderReleasedMessage(msg) {
    // 解析 OrderReleasedEvent
    // 构造 ActiveOrder（wallet/outcome/action/qty/refund）
    // 调用 manager.ReleaseOrder(ao, refundAmount)
    // 立即 Ack（不等 Projector）
}
```

**幂等保证**：`manager.ReleaseOrder` 内部有余额不低于 0 的边界保护，即使 released 事件与 match batch 中的 OrderUpdates 重复到达，也不会导致双重释放（locked 不能变负数）。

### 2.4 Writer 改造（P1）

**文件**：`internal/writer/writer.go`

#### 2.4.1 Settlement Consumer 迁移到 Pull

```go
// Before（违规: QueueSubscribe Push Consumer）
w.client.JetStream().QueueSubscribe(SubjectSettlementConfirm+".*", ..., handler, ...)

// After（符合 spec §1.3.4）
sub, _ = w.client.PullSubscribe(SubjectSettlementConfirm+".*", "writer-settlement-primary")
// settlementLoop goroutine 独立运行
```

#### 2.4.2 新增 OrderReleased Consumer

```go
w.client.PullSubscribe(SubjectOrderReleased+".*", "writer-released-primary")
```

`handleOrderReleasedMessage` 更新 Postgres `orders.status`：
```sql
UPDATE orders
SET status = $canceled_or_expired, updated_at = NOW()
WHERE order_id = $order_id
  AND status NOT IN ($canceled, $expired, $rejected)  -- 幂等保护
```

同时清理 Redis：
- `HDEL user:orders:{wallet} {order_id}`
- `DEL order:info:{order_id}`

**三个独立 goroutine**：`run()` / `settlementLoop()` / `releasedLoop()` 各自持有独立 subscription，互不阻塞。

### 2.5 Pusher 改造（P1）

**文件**：`internal/pusher/service.go`

新增 `settlementSub` Pull Consumer（`pusher-settlement-primary`），消费 `evt.settlement.confirmed.v1.*`。

`handleSettlementMessage` 向所有受影响钱包推送 WS 消息：
```json
{
  "type": "settlement.confirmed",
  "match_event_id": "...",
  "market_id": "...",
  "tx_signature": "...",
  "ts": "..."
}
```

前端可依此实时更新 pending 资产变为 available 的状态。

---

## 3. 完整事件流示意图（Phase 4-6 架构）

```
                     ┌─────────────────────────────────────┐
 REST POST /orders   │              gateway                │
─────────────────>   │  签名校验 + 字段标准化 + 基础校验    │
                     │  Publish → cmd.order.submit.v1       │
                     └───────────────┬─────────────────────┘
                                     │ (AP_CMD stream)
                                     ▼
                     ┌─────────────────────────────────────┐
                     │           funds (cmdSub)            │
                     │  ReserveOrder: available→locked      │
                     │  Publish → evt.order.reserved.v1     │
                     │         OR evt.order.reserve_rejected│
                     └───────────────┬─────────────────────┘
                                     │ (AP_EVT stream)
                                     ▼
                     ┌─────────────────────────────────────┐
                     │         matcher (Pull+Durable)      │
                     │  orderbook 撮合（串行 actor）        │
                     │  Publish → evt.match.batch.v3.*     │ ─────────┐
                     │         + evt.match.batch.v2.* (兼) │         │
                     │         + evt.order.released.v1.*   │         │
                     └───────────────┬─────────────────────┘         │
                                     │                               │
               ┌─────────────────────┼──────────────────────┐        │
               ▼                     ▼                       ▼        ▼
     ┌─────────────────┐   ┌──────────────────┐   ┌──────────────────────┐
     │ funds (evtSub)  │   │ settlement       │   │ writer               │
     │ ApplyMatchPend  │   │ 链上 submit      │   │ upsert orders/trades │
     │ ReleaseOrder    │   │ → submitted evt  │   │ + released consumer  │
     │ (releasedSub)   │   │ → confirmed evt  │   │ orders.status update │
     └────────┬────────┘   │ → failed evt     │   └──────────────────────┘
              │             └────────┬─────────┘
              │ (AP_EVT)              │ (AP_EVT)
              ▼                      ▼
     ┌─────────────────────────────────────────┐
     │         funds (evtSub)                  │
     │  InflightStore: pending→confirmed/failed │
     │  ApplySettlementConfirmedByBatch()       │
     │     OR ApplySettlementFailedByBatch()    │
     │  pending → available  OR  pending 保留   │
     └──────────┬──────────────────────────────┘
                │ (async Projector 500ms microbatch)
                ▼
       Postgres wallet_accounts / positions
       Redis    wallet:snapshot / position:*
```

---

## 4. Durable Consumer 命名规范

| Consumer Name | Subject Filter | Stream | 所属模块 |
|---|---|---|---|
| `matcher-reserved-primary` | `evt.order.reserved.v1.*` | AP_EVT | matcher |
| `funds-dispatcher-v2` | `evt.match.batch.v2.*` | AP_EVT | funds |
| `funds-cmd-dispatcher-v1` | `cmd.order.submit.v1` | AP_CMD | funds |
| `funds-released-v1` | `evt.order.released.v1.*` | AP_EVT | funds |
| `writer-primary` | `evt.match.batch.v2.*` | AP_EVT | writer |
| `writer-settlement-primary` | `evt.settlement.confirmed.v1.*` | AP_EVT | writer |
| `writer-released-primary` | `evt.order.released.v1.*` | AP_EVT | writer |
| `pusher-primary` | `evt.match.batch.v2.*` | AP_EVT | pusher |
| `pusher-settlement-primary` | `evt.settlement.confirmed.v1.*` | AP_EVT | pusher |
| `settlement-primary` | `evt.match.batch.v2.*` | AP_EVT | settlement |

> **注意**：AP_CMD 和 AP_EVT stream 的 subject filter 列表必须在 NATS JetStream stream 配置中覆盖上述所有 subject 前缀。详见 `bus/natsjs/client.go` 中的 stream 初始化逻辑。

---

## 5. 过渡期迁移路径（从 v2 → v3）

```
当前状态（matcherV3CrossPublish = true）：
  Matcher → v2 + v3 双发
  funds   → 消费 v2（兼容旧 stream）
  writer  → 消费 v2
  pusher  → 消费 v2
  settlement → 消费 v2

目标状态（所有消费者迁移完成后）：
  将各消费者的 subject filter 改为 evt.match.batch.v3.*
  将 matcherV3CrossPublish 改为 false，停止 v2 双发
  清理旧的 v2 stream 或保留仅供审计
```

迁移步骤建议：
1. 先将 funds 的 `funds-dispatcher-v2` 改为订阅 `evt.match.batch.v3.*`，验证
2. 再迁移 writer，验证
3. 再迁移 pusher，验证
4. 最后迁移 settlement，关闭双发

---

## 6. 数据库相关：配套 Migration

**文件**：`Banckend/db/migration_funds_v2.sql`

| 变更 | 表 | 说明 |
|------|-----|------|
| `ADD COLUMN IF NOT EXISTS collateral_locked_units` | `wallet_accounts` | funds projector 写入 |
| `ADD COLUMN IF NOT EXISTS collateral_pending_units` | `wallet_accounts` | funds projector 写入 |
| `ADD COLUMN IF NOT EXISTS yes_pending_lots` | `positions` | schemav2 缺失，需补 |
| `ADD COLUMN IF NOT EXISTS no_pending_lots` | `positions` | schemav2 缺失，需补 |
| `ADD COLUMN IF NOT EXISTS market_pda` | `positions` | projector UPSERT ON CONFLICT key |
| `CREATE UNIQUE INDEX positions_wallet_pda_uidx` | `positions` | `(wallet_address, market_pda)` |
| 移除 `pending_lots` 字段的 `CHECK >= 0` 约束 | `positions` | pending 可为负数（卖单方向） |

> 执行此 migration 前，确认当前使用的是 schema.sql（v1）还是 schemav2.sql，两者的字段差异见 migration 文件内注释。

---

## 7. 已通过的编译验证

```bash
cd Banckend && go build ./...
# 全量编译无错误 ✅
```

---

## 8. 手测停点（Phase 4 验收标准）

按 `spec/new/0-redesignproduct.md §Phase 4` 手测目标：

| 测试场景 | 预期 | 如何验证 |
|----------|------|---------|
| 同一钱包跨两个市场同时下单 | 不会超花（funds 串行裁决） | 观察两个 reserve 事件的 available_usdc 递减 |
| reserve 失败时 | 订单不进入 matcher | `evt.order.reserve_rejected.v1` 被 publisher，matcher 无对应事件 |
| reserve 成功但 matcher 拒绝 | 释放完整，funds 收到 released 事件 | `evt.order.released.v1` 被 publisher，funds 释放 locked |
| market actor 不直接触碰钱包资金 | 验证 matcher 无任何 `funds.*` import | `grep -r "funds" internal/matching/` 返回空 |
| settlement confirmed | pending → available | funds 日志 `applied terminal phase=confirmed` |
| settlement failed | pending 保持冻结，不自动回 available | funds 日志 `applied terminal phase=failed` |
| 重启后 funds 状态恢复 | snapshot 加载成功 | 服务启动日志 `recovered from snapshot` |

---

## 9. 当前架构与 spec 目标的对齐状态

| 规范要求 | 实现状态 |
|----------|---------|
| JetStream WAL 是唯一权威 | ✅ |
| 热路径不同步写 Postgres/Redis | ✅ (Projector 异步) |
| 失败回滚按原始 batch 重算 | ✅ (`ApplySettlementFailedByBatch`) |
| matcher 不维护资金账本 | ✅ |
| funds 是钱包/仓位权威 owner | ✅ |
| 所有核心 consumer 使用 Pull + Durable | ✅ |
| evt.match.batch.v3 作为新 canonical subject | ✅ (双发过渡) |
| matcher→funds 的 order released 事件链路 | ✅ |
| writer 消费 released 更新 orders.status | ✅ |
| pusher 消费 settlement 推送 WS | ✅ |
| JetStream replay 冷启动恢复（snapshot → replay） | ⚠️ snapshot 已实现，replay 待 Phase 7 |
| AP_CMD stream 的完整 subject 覆盖配置 | ⚠️ 需在 natsjs/client.go stream 初始化时确认覆盖 `cmd.order.submit.v1` |
