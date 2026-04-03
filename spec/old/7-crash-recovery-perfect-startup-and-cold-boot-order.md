# 7. 基于 NATS + 数据库的完美启动恢复与系统冷启动顺序设计

本文是在以下文档基础上的专项补充：

- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/0-contractdesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/3-matcher-shared-wallet-batching-redesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/4-writer-pusher-redis-websocket-redesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/5-settlement-contract-implementation.md`

本文要回答两个问题：

1. **需要新增哪些机制，才能让系统在宕机后基于 NATS + 数据库做到“完美启动恢复”**
2. **整个系统重启时，各个模块应该按什么顺序冷启动**

本文先只做设计定稿：

- 先把恢复与启动顺序写清楚
- 先把必须新增的表、事件、ACK 规则、启动阶段定义写死
- 本阶段**不要求立即实现**

---

## 0. 先说结论

如果只依赖：

- 数据库
- Redis
- 链上 webhook

那么系统只能做到：

- 恢复到“最近一次 durable 投影状态”

但如果要做到：

- **重启后尽可能无损恢复到宕机前逻辑状态**

则必须把 **NATS** 纳入正式恢复体系，而且必须补齐以下三件事：

1. **批次生命周期持久化**
2. **恢复期可重放的事件流**
3. **严格的 ACK 边界**

只有同时满足这三件事，才能让：

- matcher 订单簿
- shared wallet
- writer 数据投影
- settlement 提交状态
- Redis 读模型

在冷启动后回到一致状态。

---

## 1. 什么叫“完美启动恢复”

本文里“完美启动恢复”不是数学意义上的绝对零损，而是工程上的强定义：

> 在任意一次进程宕机后，系统重启时能够基于 NATS + 数据库恢复出一个**业务可继续运行且与已持久化时序完全一致**的状态，不会出现：
>
> - 订单簿与数据库不一致
> - shared wallet 与订单簿不一致
> - settlement 不知道哪些 batch 已提交 / 未确认 / 已失败
> - Redis 与数据库长期不一致
> - 同一条命令被重复应用但没有幂等保护

这里的核心不是“把内存字节级恢复”，而是：

- 通过日志重放和状态机恢复，重建出**等价逻辑状态**

---

## 2. 当前设计离“完美恢复”还缺什么

结合 3 号与 5 号文档，当前设计已经具备：

- `cmd.order.place` 命令流
- `evt.match.batch.v2.{market_id}` 事件流
- 数据库中的 orders / trades / positions / cursors
- settlement 模块对 submission 的运行时构造思路

但当前还不足以做到完美恢复，原因在于缺少：

### 2.1 缺少 batch 生命周期真相

目前 matcher 会产出 batch，settlement 会消费 batch，但系统没有一个统一持久化状态去回答：

- 这个 batch 是否已经 publish？
- 是否已经被 settlement 接收？
- 是否已经提交链上？
- 是否已经链上确认？
- 是否已经失败并回滚？

如果没有这层状态，shared wallet 的 `pending_*` 就无法在重启后精确恢复。

### 2.2 缺少 settlement 生命周期事件流

仅有：

- `cmd.order.place`
- `evt.match.batch.v2`

还不够。

因为缺少：

- `evt.settlement.submission.created`
- `evt.settlement.submission.sent`
- `evt.settlement.submission.confirmed`
- `evt.settlement.submission.failed`

否则系统无法知道：

- 哪些 matcher batch 已经进入 pending 提交期
- 哪些仍只是链下已撮合
- 哪些已经被链上确认
- 哪些必须回滚

### 2.3 缺少 shared wallet 的可恢复 journal

虽然 shared wallet 是内存状态，但其恢复并不要求单独把整个 shared wallet dump 到磁盘。

真正需要的是：

- 能从 **base truth + logs** 重建 shared wallet

当前缺少的是“足够的信息”，而不是“一个单独的 snapshot 文件”。

### 2.4 缺少系统级启动阶段定义

当前各模块更像“都启动起来再说”，而不是一个清晰的 staged boot。

这会导致：

- writer 还没恢复，matcher 已经开始吃命令
- settlement 还没恢复 batch 状态，就开始提交流水
- pusher 过早对外开放 websocket

这些都会破坏恢复的一致性。

---

## 3. 完美恢复需要新增哪些持久化对象

为了做到基于 NATS + DB 的完美启动恢复，建议新增以下数据库对象。

## 3.1 `match_batches`

用于持久化 matcher 产出的业务批次。

推荐结构：

```sql
CREATE TABLE match_batches (
    batch_id              TEXT PRIMARY KEY,
    market_id             NUMERIC(20,0) NOT NULL,
    market_pda            VARCHAR(44) NOT NULL,

    source_cmd_seq_min    BIGINT NOT NULL,
    source_cmd_seq_max    BIGINT NOT NULL,

    produced_at           TIMESTAMPTZ NOT NULL,
    schema_version        INTEGER NOT NULL,

    status                TEXT NOT NULL,
    -- produced | projected | submission_created | submitted | confirmed | failed | rolled_back

    payload_json          JSONB NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

用途：

- 作为 matcher 批次的 durable snapshot
- 启动恢复时可直接知道有哪些 batch 存在
- 可与 NATS replay 互为校验

### 3.1.1 为什么 `payload_json` 必须保留

因为 recovery 时最怕的是：

- 想恢复某个 batch，但只能看到 summary，拿不到完整内容

保留完整 `payload_json` 的好处：

- settlement 可以重建 submission
- shared wallet 可以按 batch 回滚或确认
- writer / Redis rebuild 可以补重放

---

## 3.2 `settlement_submissions`

用于持久化 settlement 的提交生命周期。

推荐结构：

```sql
CREATE TABLE settlement_submissions (
    submission_id         TEXT PRIMARY KEY,
    batch_id              TEXT NOT NULL REFERENCES match_batches(batch_id),
    market_id             NUMERIC(20,0) NOT NULL,

    status                TEXT NOT NULL,
    -- created | sent | confirmed | failed

    tx_sig                TEXT,
    failure_reason        TEXT,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at               TIMESTAMPTZ,
    finalized_at          TIMESTAMPTZ
);
```

用途：

- 恢复时判断：
  - 哪些 batch 还没做 submission
  - 哪些已 sent 但还没 confirm
  - 哪些已 failed 要回滚

---

## 3.3 `recovery_cursors`

当前已有 `consumer_cursors` 的思路，但若要系统级恢复，建议增加更通用的 recovery cursor。

推荐：

```sql
CREATE TABLE recovery_cursors (
    module_name           TEXT NOT NULL,
    stream_name           TEXT NOT NULL,
    market_id             NUMERIC(20,0),
    last_stream_seq       BIGINT NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (module_name, stream_name, market_id)
);
```

用途：

- matcher 恢复自己吃到哪
- writer 恢复自己投影到哪
- settlement 恢复自己处理到哪

如果后续继续用 `consumer_cursors` 统一承载，也可以不新增表，但语义必须扩展清楚。

---

## 3.4 `system_boot_epochs`

建议新增系统启动 epoch 表。

```sql
CREATE TABLE system_boot_epochs (
    boot_id               TEXT PRIMARY KEY,
    started_at            TIMESTAMPTZ NOT NULL,
    ready_at              TIMESTAMPTZ,
    status                TEXT NOT NULL
    -- starting | recovering | ready | failed
);
```

用途：

- 标记一次完整冷启动过程
- 便于排查恢复失败与启动阶段卡住的问题
- 未来做运维告警和 dashboard 很有价值

---

## 4. 完美恢复需要新增哪些 NATS 事件

NATS 不只是命令总线，也应是恢复日志的一部分。

建议新增以下事件。

## 4.1 matcher 已产出批次

现有：

- `evt.match.batch.v2.{market_id}`

继续保留，作为 matcher 的业务结果真相流。

## 4.2 settlement submission 生命周期

建议新增：

```text
evt.settlement.submission.created.{market_id}
evt.settlement.submission.sent.{market_id}
evt.settlement.submission.confirmed.{market_id}
evt.settlement.submission.failed.{market_id}
```

推荐结构：

```json
{
  "submission_id": "sub-001",
  "batch_id": "batch-001",
  "market_id": "1001",
  "status": "sent",
  "tx_sig": "...",
  "failure_reason": "",
  "ts": "2026-04-01T10:00:00Z"
}
```

这些事件的作用不是给前端看，而是给：

- 恢复系统
- shared wallet 对账
- settlement 自身幂等恢复

---

## 5. 完美恢复需要新增哪些 ACK 规则

ACK 规则是本设计最关键的部分。

### 5.1 Gateway -> `cmd.order.place`

Gateway 发布命令成功后即可返回 `202 Accepted`，这没有问题。

### 5.2 matcher 消费 `cmd.order.place`

matcher **不能**在以下情况前 ACK：

- 还没成功发布对应的 `evt.match.batch.v2`

更严格地说：

- 只有当 batch 已成功 publish 到 NATS，才能 ACK 本批关联的命令消息

否则宕机时可能出现：

- 命令已 ACK
- 事件没留下
- 内存状态丢失

这是不可恢复的。

### 5.3 writer 消费 `evt.match.batch.v2`

writer 只能在以下都成功后 ACK：

1. 数据库事务提交成功
2. 对应 cursor 持久化成功

Redis 是否成功可以不作为 ACK 的必要条件，但：

- Redis 失败要告警
- 必须支持后续重建

### 5.4 settlement 消费 `evt.match.batch.v2`

settlement 只能在以下成功后 ACK：

1. 对应 `match_batches` / `settlement_submissions` 已持久化
2. submission 生命周期至少推进到当前 durable 节点

举例：

- 如果 settlement 只是创建了 submission 计划但还没 durable 记录，就不能 ACK

### 5.5 webhook 处理

webhook 也必须做到：

- 先 durable 写库
- 再更新内存 / cache

其结果要能驱动：

- settlement submission 状态确认
- shared wallet pending 清理

---

## 6. shared wallet 如何基于 NATS + DB 完美恢复

shared wallet 的恢复必须分三层。

## 6.1 第 1 层：恢复 base truth

来源：

- 链上最终已确认的状态
- webhook 已落库结果

恢复内容：

- `available_usdc`
- `available_yes_shares`
- `available_no_shares`
- `cancel_all_before_ts`

这里不能从 matcher 的乐观结果直接恢复 base。

## 6.2 第 2 层：恢复 `locked_*`

来源：

- 数据库中的 open orders

恢复规则：

- 所有 still-open 的 buy order
  - 累加到 `locked_usdc`
- 所有 still-open 的 `Sell YES`
  - 累加到 `locked_yes_shares`
- 所有 still-open 的 `Sell NO`
  - 累加到 `locked_no_shares`

这一层从 DB 可稳定恢复。

## 6.3 第 3 层：恢复 `pending_*`

来源：

- `match_batches`
- `settlement_submissions`
- settlement lifecycle events

恢复规则：

- `confirmed`
  - 不再算 pending
- `failed / rolled_back`
  - 不再算 pending
- `submitted but not confirmed`
  - 恢复为 pending
- `produced but not submitted`
  - 需要策略决策：
    - 要么丢弃并回滚
    - 要么重放后重新进入 submission

### 6.3.1 推荐策略

本文建议：

- `produced but not submitted`
  - 在恢复期统一重新交给 settlement 判断，不直接丢弃

这样不会平白损失 matcher 已产出的交易结果。

---

## 7. 订单簿如何基于 NATS + DB 完美恢复

订单簿恢复建议采用“两段式”。

### 7.1 先从 DB snapshot 恢复

步骤：

- 从 `orders` 中捞出 `new / partially_filled`
- 按 market / side / price / created order 排序
- 重建内存订单簿

这一步快，适合作为冷启动基线。

### 7.2 再用 NATS replay 补齐尾部增量

从 matcher 自己的恢复 cursor 开始：

- replay 尚未 durable apply 的命令或批次事件

这里的关键是：

- DB snapshot 不是唯一真相
- NATS 用于补齐 snapshot 之后的尾部窗口

这才是“完美恢复”的关键。

---

## 8. Redis 如何基于 NATS + DB 完美恢复

Redis 恢复相对简单，因为它本来就是读模型。

推荐恢复策略：

### 8.1 全量 rebuild

启动时优先：

- 清空 Redis 相关 key
- 从数据库全量 rebuild：
  - `l2:depth:*`
  - `user:orders:*`
  - `order:info:*`
  - `position:*`
  - `trades:latest:*`
  - `price:history:*`

### 8.2 再消费未处理的 matcher 事件补尾

从 writer cursor 之后继续消费：

- `evt.match.batch.v2.*`

这样 Redis 就能追到最新。

### 8.3 为什么 Redis 不需要独立 journal

因为：

- DB + NATS 足够重建 Redis
- Redis 不承担最终真相职责

---

## 9. 系统冷启动顺序定稿

这一节是本文的第二核心。

启动顺序不能再是“所有服务一起起”。

必须采用分阶段冷启动。

## 9.1 启动阶段定义

建议整个系统定义为以下阶段：

1. `BOOT_INIT`
2. `BOOT_RECOVER_DB`
3. `BOOT_RECOVER_NATS`
4. `BOOT_REBUILD_MEMORY`
5. `BOOT_REBUILD_REDIS`
6. `BOOT_SETTLEMENT_RECONCILE`
7. `BOOT_OPEN_FOR_WRITE`
8. `BOOT_READY`

---

## 9.2 阶段一：`BOOT_INIT`

启动内容：

- 读取配置
- 初始化日志
- 初始化数据库连接
- 初始化 NATS 连接
- 初始化 Redis 连接

此阶段不允许：

- 对外开放下单
- matcher 开始消费命令
- settlement 开始提交链上

---

## 9.3 阶段二：`BOOT_RECOVER_DB`

启动内容：

- 读取核心持久化表：
  - orders
  - trades
  - positions
  - match_batches
  - settlement_submissions
  - consumer/recovery cursors
  - user_position_accounts

目的：

- 得到最新 durable snapshot

---

## 9.4 阶段三：`BOOT_RECOVER_NATS`

启动内容：

- 读取各流的 consumer state
- 确认每个模块从哪里继续 replay

此阶段不做正式实时消费，只做：

- recovery range 计算

即明确：

- DB 到哪里
- NATS 还剩哪一段没追上

---

## 9.5 阶段四：`BOOT_REBUILD_MEMORY`

启动内容：

- 重建 matcher book
- 重建 shared wallet
- 重建 settlement registry

顺序建议：

1. open orders -> order book
2. base truth -> shared wallet available
3. open orders -> shared wallet locked
4. unconfirmed submissions -> shared wallet pending
5. user_position_accounts -> settlement registry

此阶段完成后，系统已有可运行的内存状态，但还未对外开放。

---

## 9.6 阶段五：`BOOT_REBUILD_REDIS`

启动内容：

- 从 DB 全量 rebuild Redis 读模型

理由：

- Redis rebuild 不应该阻塞前面核心业务恢复
- 但必须在对外开放 websocket / 高频查询前完成基线重建

---

## 9.7 阶段六：`BOOT_SETTLEMENT_RECONCILE`

启动内容：

- 扫描 `submitted but unconfirmed` 的 settlement submissions
- 检查是否：
  - 已链上确认但 webhook 未处理
  - 已失败但状态未落库
  - 仍在 pending

这一阶段的目标是：

- 把 submission 生命周期收敛到一致状态

这是 shared wallet 能否正确恢复 `pending_*` 的关键。

---

## 9.8 阶段七：`BOOT_OPEN_FOR_WRITE`

只有到这一阶段，才允许：

- Gateway 打开下单入口
- matcher 开始正式消费新命令

要求：

- writer 已可用
- matcher 已恢复
- settlement 已恢复
- Redis 已完成基础 rebuild

如果其中任何一项未完成，则系统只能保持只读或降级模式。

---

## 9.9 阶段八：`BOOT_READY`

此阶段表示：

- 所有恢复完成
- 所有模块已切到实时运行
- 可以对外宣告 ready

健康检查应在此阶段才返回：

- `gateway_write_ready = true`

---

## 10. 各模块冷启动先后顺序

这一节给出模块级严格顺序。

### 10.1 第一组：基础设施

顺序：

1. config
2. logger
3. DB
4. NATS
5. Redis

### 10.2 第二组：恢复层

顺序：

1. recovery coordinator
2. DB snapshot loader
3. cursor loader
4. NATS replay planner

### 10.3 第三组：内存状态层

顺序：

1. settlement registry
2. shared wallet
3. matcher order books

原因：

- matcher 依赖 shared wallet
- settlement 依赖 user position registry

### 10.4 第四组：投影层

顺序：

1. writer rebuild
2. Redis rebuild

### 10.5 第五组：对账层

顺序：

1. settlement reconcile
2. webhook catch-up

### 10.6 第六组：实时消费层

顺序：

1. writer start consuming
2. settlement start consuming
3. matcher start consuming
4. pusher start consuming

这里特别注意：

- matcher 不能早于 writer / settlement 打开实时命令消费
- 否则新命令会跑进一个还没完全恢复好的系统

### 10.7 第七组：对外接口层

顺序：

1. HTTP 只读接口
2. WebSocket
3. Gateway 写接口

严格要求：

- Gateway 下单入口必须最后打开

---

## 11. 推荐的启动模式

建议系统支持三种启动模式。

### 11.1 `safe_recover`

默认模式。

行为：

- 执行完整 DB + NATS 恢复
- 完成后再开放写流量

适合：

- 生产环境

### 11.2 `read_only_recover`

行为：

- 完成 DB / Redis / 内存恢复
- 只开放查询
- 不开放下单

适合：

- 故障排查
- 大版本迁移期

### 11.3 `fast_boot`

行为：

- 只按 DB snapshot 恢复
- 不做完整 NATS replay
- 标记系统为 degraded

适合：

- 开发环境
- 临时调试

生产环境不推荐作为默认。

---

## 12. 实现优先级建议

虽然本文阶段不要求立即实现，但建议未来按这个顺序推进：

### 第一步：补齐 settlement lifecycle 持久化

先补：

- `match_batches`
- `settlement_submissions`

因为这是恢复体系的主骨架。

### 第二步：补齐 recovery coordinator

实现统一的：

- boot stage state machine
- 模块 gating

### 第三步：补齐 shared wallet rebuild

目标：

- base / locked / pending 三层恢复清楚

### 第四步：补齐 NATS replay

目标：

- 能从 cursor 之后补齐尾部事件

### 第五步：最后再开放“写流量 gating”

目标：

- 只有恢复完成才开放下单

---

## 13. 最终定稿

为了保证系统能够从 **NATS + 数据库** 完美启动恢复，必须新增以下措施：

1. 数据库新增：
   - `match_batches`
   - `settlement_submissions`
   - `recovery_cursors`（或扩展现有 cursor 体系）
   - `system_boot_epochs`
2. NATS 新增 settlement 生命周期事件
3. 各模块统一采用“先 durable，后 ACK”的严格边界
4. shared wallet 按：
   - base
   - locked
   - pending
   三层进行恢复
5. 系统启动采用严格分阶段冷启动
6. Gateway 写入口必须在所有恢复完成后最后打开

本文对冷启动顺序的最终定稿为：

1. 基础设施初始化
2. DB snapshot 恢复
3. NATS recovery range 规划
4. 内存状态重建
5. Redis rebuild
6. settlement 对账
7. writer / settlement / matcher / pusher 依次打开实时消费
8. 最后开放 Gateway 写入口

这套方案先作为架构文档冻结，后续实现时必须严格按此顺序推进，而不能在恢复路径上继续“边启动边凑状态”。
