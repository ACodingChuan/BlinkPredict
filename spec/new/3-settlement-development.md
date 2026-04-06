# Settlement 开发文档

## 1. 文档目标

本文只解决一件事：

- 把 `settlement` 模块收敛成一套可以直接开发的最小方案。

本文覆盖并收敛此前讨论中关于 `attempt_id`、过多中间状态、`match batch` 重投与链上重试混淆的问题。

本文对 `settlement` 的最终约束是：

1. 业务主键是 `match_event_id`。
2. 不引入独立 `attempt_id`。
3. 显式状态只保留：
   - `queued`
   - `submitted`
   - `confirmed`
   - `failed`
4. 同一 market 同时只能有一条 `submitted`。
5. 只有旧交易已确认过期时，才允许生成新的 `tx_signature`。
6. 其他所有异常都只能继续监听旧 `tx_signature`，或重播同一份 `raw_tx`。

---

## 2. 模块最小架构

`settlement` 进程内只保留 4 个逻辑角色：

1. `ingress`
   - 消费 `evt.match.batch.v3.{market_id}`
   - 安全接单入库
   - Ack 原始消息

2. `scheduler`
   - 按 `market_id` 做单行道调度
   - 决定哪条 `queued` 可以进入 `submitted`

3. `submitter`
   - 构造、签名、广播链上交易
   - 把 `queued` 推进成 `submitted`

4. `watcher/reconciler`
   - 监听 `submitted`
   - 负责确认、失败、同源重播、过期换签、冷启动恢复

宏观数据流：

1. `matcher -> evt.match.batch.v3 -> settlement ingress`
2. `ingress -> settlement_submissions.status=queued`
3. `scheduler/submitter -> settlement_submissions.status=submitted`
4. `watcher -> settlement_submissions.status=confirmed|failed`
5. `settlement -> evt.settlement.submitted|confirmed|failed`

`settlement` 不负责：

1. 钱包账本
2. 仓位账本
3. 订单簿
4. orders/trades/depth/query/redis 投影

### 2.1 进程模型

V1 开发目标按：

1. 单进程
2. 多协程
3. 单二进制

落地。

也就是说：

- `ingress / scheduler / submitter / watcher / reconciler`
- 先全部跑在同一个 `settlement` 进程里

这样做的原因：

1. 开发复杂度最低
2. 可直接复用当前 backend 的启动方式
3. 能先把业务语义收稳
4. 后续拆成独立服务时，不需要改表结构和事件结构

V1 明确不做：

1. `settlement` 多实例并发部署
2. `submitter` 与 `watcher` 的跨进程拆分
3. 专门的分布式 market lane 协调器

V1 默认假设：

- 同一时刻只有一个 `settlement` 进程在工作

这意味着：

1. market 单行道主要靠进程内调度保证
2. 数据库的 `CAS/WHERE status=...` 主要用于防重与恢复，不作为多实例锁服务

### 2.2 协程模型

建议最小协程划分如下：

1. `ingressFetchLoop`
   - 从 JetStream Pull 拉取 `evt.match.batch.v3.*`
   - 把消息交给 ingress 处理

2. `ingressHandleWorker[N]`
   - 解析一批 payload
   - 校验最小字段
   - 微批 `INSERT ... status='queued'`
   - Ack 对应原始消息
   - 仅把对应 market 标记为 dirty

3. `schedulerLoop`
   - 维护一个带防抖的 `dirtyMarkets` 集合
   - 由单独的 `wake chan` 驱动调度
   - 决定哪些 market 需要尝试发车

4. `submitLoop`
   - 从 scheduler 接收一个具体 `market_id`
   - 检查 lane 是否空闲
   - 选择该 market 最老的一条 `queued`
   - build/sign/broadcast
   - 推进成 `submitted`

5. `wsRouterLoop`
   - 维护进程级共享 Solana WebSocket 连接
   - 给 deposit / market confirm / settlement watcher 复用
   - 负责订阅复用与结果分发

6. `watchRegistryLoop`
   - 管理所有当前 `submitted` 任务
   - 为新的 `submitted` 记录创建或刷新 `WatchTask`

7. `watchTask goroutine`
   - 针对一条 `submitted` 记录工作
   - 负责：
     - 初始 HTTP 查状态
     - 向 `WSRouter` 注册签名监听
     - 定时 HTTP fallback
     - 定时同源重播
     - 检查 block height 过期

8. `reconcileLoop`
   - 启动时做全量恢复
   - 运行中按 ticker 周期修复：
     - 漏监听
     - 终态未发布
     - 长时间无响应任务

### 2.3 进程内调度原则

进程内调度只遵守两个原则：

1. `ingress` 永远快进快出
2. `watcher` 永远异步运行

具体表现为：

1. 原始 `match batch` 只要被安全写入 `settlement_submissions`
   - 就立即 Ack
   - 不等待链上确认

2. `submitter` 不在消费 NATS 的 goroutine 中等待链上

3. `watcher` 不阻塞 `scheduler`

4. `scheduler` 不直接扫描整张表无限循环
   - 它优先消费“变脏的 market 通知”
   - 再用低频 ticker 做兜底扫描

5. `dirty market` 通知必须可丢重，不可阻塞
   - scheduler 只需要知道“这个 market 需要再看一眼”
   - 不需要知道“它在 1 秒内变脏了 10000 次”

### 2.4 market 单行道

V1 里，market 单行道是 settlement 的核心并发约束。

规则只有一条：

- 同一 `market_id` 同时最多只有一条 `submitted`

进程内建议维护：

```go
type MarketLane struct {
    MarketID uint64
    Paused bool
    CurrentMatchEventID string
}

var lanes map[uint64]*MarketLane
```

调度语义：

1. `CurrentMatchEventID == ""`
   - 表示 lane 空闲
   - scheduler 可以尝试发车

2. `CurrentMatchEventID != ""`
   - 表示该 market 当前有 batch 在飞
   - 后续 queued 不能推进

3. 当前在飞 batch 进入 `confirmed` 或 `failed`
   - `confirmed`：
     - lane 释放
     - scheduler 被唤醒
     - 尝试推进该 market 下一条 queued
   - `failed`：
     - 释放 `CurrentMatchEventID`
     - lane 保持 `Paused=true`
     - 不再自动推进后续 queued

### 2.5 NATS 接收与处理技术细节

#### 输入主题

`settlement` 只直接消费：

- `evt.match.batch.v3.{market_id}`

建议使用：

1. JetStream
2. Pull consumer
3. durable consumer

原因：

1. 可以手动 Ack
2. 可以控制 fetch batch size
3. 可重放
4. 宕机恢复简单

建议参数：

1. `fetch batch size = 16~64`
2. `max wait = 500ms~1500ms`
3. malformed message 直接 `Term`
4. 数据库瞬时错误 `NakWithDelay`
5. 成功入库后立即 `Ack`

#### ingress 对每批 NATS 消息的处理顺序

1. 一次 `Fetch(N)` 拉取一批消息
2. 逐条反序列化与字段校验
3. malformed 的消息直接 `Term`
4. 合法消息在 Go 内存里先整理成一批待插入行
5. 使用一条微批 SQL 执行：
   - `INSERT ... VALUES (...), (...), (...) ON CONFLICT DO NOTHING RETURNING match_event_id, market_id`
6. 根据插入结果逐条 Ack：
   - 新插入成功的消息：Ack
   - 冲突的重复消息：Ack
   - 数据库瞬时失败的消息：NakWithDelay
7. 仅对新插入成功的 market 做 dirty 标记

这样做的目的：

1. 避免极端 burst 时一条消息抢一个数据库连接
2. 把 `50` 次小事务压成 `1` 次批量事务
3. 把 ingress 的瓶颈从连接池争抢改成顺序批量落库

#### 内部事件

`evt.settlement.submitted.v1.{market_id}` 的定位是：

1. watcher 加速通知
2. pusher 低延迟展示
3. 观测日志

它不是唯一真相源。  
即使这个事件丢了，watcher/reconciler 仍然要通过扫表恢复所有 `submitted`。

### 2.6 RPC 处理技术细节

`settlement` 需要使用两类 RPC：

1. 提交类 RPC
2. 观察类 RPC

#### 提交类 RPC

submitter 需要调用：

1. `GetLatestBlockhash`
2. 构造交易
3. 本地签名
4. `SendTransactionWithOpts`

提交阶段的关键顺序：

1. 先拿 blockhash
2. 再 build/sign
3. 再把：
   - `tx_signature`
   - `raw_tx_base64`
   - `last_valid_block_height`
   写入 `settlement_submissions`
4. 最后才执行物理广播

这条顺序不能颠倒。

#### 观察类 RPC

watcher 需要调用：

1. `GetSignatureStatuses`
2. `signatureSubscribe`
3. `GetBlockHeight`
4. 必要时再次 `SendTransactionWithOpts`

观察阶段的关键原则：

1. 先查状态，再订阅
2. WS 失败后必须有 HTTP fallback
3. HTTP fallback 不能替代终态发布幂等
4. 同源重播使用完全相同的 `raw_tx`
5. 只有 `current_block_height > last_valid_block_height` 时才允许换签
6. `watchTask` 不直接各自维护独立 WS 连接，而是统一向进程级 `WSRouter` 注册

#### `WSRouter` 约束

1. V1 接受把 settlement watcher 挂到统一的进程级 websocket 基础设施里
2. 这个 `WSRouter` 应同时服务：
   - deposit confirm
   - market confirm
   - settlement watcher
3. V1 不把它单独拆成新服务
4. `WSRouter` 负责：
   - 复用少量底层 WS 连接
   - 维护 `signature -> subscribers`
   - 把节点推送 fan-out 到具体 watcher
5. `watchTask` 只关心“注册/取消注册/接收结果”，不关心底层连接管理

#### `WSRouter` 详细实现方案

V1 不建议直接把业务并发量暴露给第三方 `solana-go/rpc/ws.SignatureSubscription` 对象。

原因：

1. 第三方库的单订阅对象默认带超大缓冲 channel
2. settlement 后续可能同时观察大量 `submitted`
3. 如果沿用“每个 watcher 直接创建一个重量级 ws subscription 对象”的方式，内存会先成为瓶颈，而不是网络连接

因此 V1 建议：

1. 统一引入进程级 `WSRouter`
2. `WSRouter` 直接维护少量原生 websocket 连接
3. `WSRouter` 自己维护：
   - request id
   - ws subscription id
   - signature
   - local subscribers
4. 业务方只拿到轻量级本地通知 channel，不直接持有底层 websocket 订阅对象

#### `WSRouter` 连接模型

V1 推荐：

1. 默认 `2` 条底层 Solana WS 连接
2. 同一条 `signature` 固定哈希到某一个 shard
3. 一个 shard 对应一条长连接和一个事件循环 goroutine

原因：

1. `1` 条连接虽然功能上可行，但单点风险太高
2. `2` 条连接已经足够支撑当前 deposit / market confirm / settlement 三类业务
3. 后续如果观察量继续提升，可扩到 `4` 条，不需要改上层接口

#### `WSRouter` 与三类业务的关系

三类业务可以合并到同一个 `WSRouter`。

不是因为它们“同一时间发生”，而是因为：

1. 三类业务最终都只是在等待 `signature` 的链上终态
2. `WSRouter` 是动态注册/注销模型，不要求所有订阅同时创建
3. 不同业务只是：
   - 注册时机不同
   - 收到结果后的业务动作不同

具体分工：

1. `deposit confirm`
   - 注册一个签名
   - 等 confirmed / failed
   - 结果落库并发事件

2. `market confirm`
   - 注册一个签名
   - 等 confirmed / failed
   - 结果更新 market submissions / markets 表

3. `settlement watcher`
   - 注册一个签名
   - 同时配合 HTTP fallback
   - 同时配合 block height expiry
   - 同时配合同源重播 / 换签

所以：

- 可以共用一套底层 `WSRouter`
- 不能共用上层状态机

#### `WSRouter` 容量目标

V1 的目标不是追求“几千条 websocket 连接”，而是：

1. 少量物理连接
2. 大量逻辑订阅

建议的工程目标：

1. 默认底层连接数：`2`
2. 单进程保守目标：`1000` 个活跃 `signature` 观察
3. 优化后可扩展目标：`5000` 个活跃 `signature` 观察

这里的“活跃观察数”是：

- deposit confirm
- market confirm
- settlement watcher

三类业务合并后的总数。

如果超过这个量级：

1. 先增加 shard 数到 `4`
2. 再评估是否需要把 `WSRouter` 单独拆成服务

#### `WSRouter` 失败语义

`WSRouter` 只是低延迟观察器，不是真相源。

任何时候：

1. websocket 推送命中
   - 可以加速业务推进
2. websocket 推送失败、断线、丢通知
   - 不能改变业务 correctness
   - 业务仍然必须依赖 HTTP fallback / 扫表恢复 / block height 规则

也就是说：

- `WSRouter` 负责快
- HTTP + DB 状态机负责准

#### 广播错误分类

V1 开发时，submitter 和 watcher 都要把 RPC 错误分两类：

1. 不确定错误
   - 连接断开
   - 502
   - timeout
   - upstream unavailable

处理：

- 不推进 `failed`
- 保持 `submitted`
- 继续由 watcher 接管

2. 确定性错误
   - simulation failed
   - invalid account layout
   - instruction data invalid
   - program returned deterministic error

处理：

- 可以直接推进 `failed`
- 不再同源重播

---

## 3. 状态机

### 3.1 显式状态

`settlement_submissions.status` 只允许：

1. `queued`
2. `submitted`
3. `confirmed`
4. `failed`

### 3.2 状态含义

#### `queued`

含义：

- 这条 `match batch` 已经被 settlement 安全接住
- 原始 NATS 消息已经可以被 Ack
- 还没有当前在飞的链上交易

#### `submitted`

含义：

- 这条 `match batch` 当前已经绑定一笔在飞的链上交易
- 表中必须存在：
  - `tx_signature`
  - `raw_tx_base64`
  - `last_valid_block_height`

注意：

- “同源重播”
- “websocket 重挂”
- “HTTP fallback”
- “过期后原地换签”

都属于 `submitted` 的内部处理，不产生新的显式状态。

#### `confirmed`

含义：

- 当前 `match_event_id` 对应的 batch 已链上确认成功
- 已经拿到 `confirmation_slot`
- 可以向 `funds / writer / pusher` 发布成功终态

#### `failed`

含义：

- 当前 `match_event_id` 已经是终态失败
- 不会再自动推进
- 可以向 `funds / writer / pusher` 发布失败终态

### 3.3 合法状态流转

只允许：

1. `queued -> submitted`
2. `submitted -> confirmed`
3. `submitted -> failed`

不允许：

1. `confirmed -> submitted`
2. `failed -> submitted`
3. `confirmed -> failed`
4. `failed -> confirmed`

### 3.4 `submitted` 内部允许发生的事

以下操作不改变显式状态：

1. 重新挂 WebSocket 监听
2. HTTP 查询签名状态
3. 重播同一份 `raw_tx`
4. 旧签名过期后，用新 blockhash 重新签名并原地替换：
   - `tx_signature`
   - `raw_tx_base64`
   - `last_valid_block_height`
   - `retry_count`

---

## 4. 主表：`settlement_submissions`

这是 settlement 唯一必须的持久化表。

### 4.1 建表建议

```sql
CREATE TABLE settlement_submissions (
    match_event_id TEXT PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL,
    market_pda TEXT NOT NULL,

    status TEXT NOT NULL CHECK (status IN ('queued', 'submitted', 'confirmed', 'failed')),
    market_lane_status TEXT NOT NULL DEFAULT 'active' CHECK (market_lane_status IN ('active', 'paused')),

    match_event_json JSONB NOT NULL,
    wallets_json JSONB NOT NULL,

    tx_signature TEXT NOT NULL DEFAULT '',
    raw_tx_base64 TEXT NOT NULL DEFAULT '',
    last_valid_block_height BIGINT NOT NULL DEFAULT 0,
    retry_count INT NOT NULL DEFAULT 0,

    confirmation_slot BIGINT NOT NULL DEFAULT 0,
    reason_code TEXT NOT NULL DEFAULT '',

    submitted_event_published BOOLEAN NOT NULL DEFAULT FALSE,
    terminal_event_published BOOLEAN NOT NULL DEFAULT FALSE,

    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX settlement_submissions_tx_signature_uidx
ON settlement_submissions (tx_signature)
WHERE tx_signature <> '';

CREATE INDEX settlement_submissions_hot_queue_idx
ON settlement_submissions (market_id, created_at)
WHERE status = 'queued' AND market_lane_status = 'active';

CREATE INDEX settlement_submissions_hot_submitted_idx
ON settlement_submissions (updated_at)
WHERE status = 'submitted';

CREATE INDEX settlement_submissions_terminal_publish_idx
ON settlement_submissions (updated_at)
WHERE status IN ('confirmed', 'failed') AND terminal_event_published = FALSE;
```

### 4.2 字段说明

#### `match_event_id`

类型：

- `TEXT`

来源：

- `evt.match.batch.v3` 的 `match_event_id`

作用：

1. settlement 业务主键
2. 幂等主键
3. 终态事件的业务关联键
4. funds/writer 后续重算的主关联键

规则：

1. 全局唯一
2. 插入冲突代表该 batch 已被 settlement 接住
3. 后续任何重投都只允许命中这同一行

#### `market_id`

类型：

- `NUMERIC(20,0)`

来源：

- `evt.match.batch.v3.market_id`

作用：

1. market 级单行道调度键
2. NATS subject 分片键
3. 同市场串行推进判定键

规则：

1. 不可为空
2. 不允许在生命周期中修改

#### `market_pda`

类型：

- `TEXT`

来源：

- `evt.match.batch.v3.market_pda`

作用：

1. 构造 settlement 指令
2. 下游 `funds/writer` 消费终态事件时使用

规则：

1. 不可为空
2. 不允许生命周期中修改

#### `status`

类型：

- `TEXT`

取值：

1. `queued`
2. `submitted`
3. `confirmed`
4. `failed`

作用：

1. 表示 batch 在 settlement 模块中的显式生命周期

规则：

1. 初始插入必须是 `queued`
2. 只有 submitter 能把 `queued` 改为 `submitted`
3. 只有 watcher/reconciler 能把 `submitted` 改为 `confirmed|failed`

#### `market_lane_status`

类型：

- `TEXT`

取值：

1. `active`
2. `paused`

作用：

1. 表示该行所在 market 当前是否允许继续推进后续 queued

说明：

- 这是 market 级熔断辅助字段
- 不替代 `status`
- 当前行进入 `failed` 后，可由 watcher/reconciler 把该 market 相关后续记录标记为 `paused`

#### `match_event_json`

类型：

- `JSONB`

来源：

- 原始 `evt.match.batch.v3` 完整载荷

作用：

1. 过期换签时重建交易
2. 失败时人工审计
3. 后续 `funds/writer` 精确回滚或重算时可直接使用

规则：

1. ingress 首次接单时写入
2. 生命周期中不修改
3. `match_event_json` 可能很大，热路径扫描绝不能顺手把它整列读出来

#### `wallets_json`

类型：

- `JSONB`

内容：

- 去重、排序后的钱包地址数组

作用：

1. 给 `evt.settlement.submitted`
2. 给 `evt.settlement.confirmed`
3. 给 `evt.settlement.failed`
4. 提供下游最小消费集合

规则：

1. ingress 首次接单时写入
2. 生命周期中不修改

#### `tx_signature`

类型：

- `TEXT`

来源：

- 当前在飞 `raw_tx` 的交易签名

作用：

1. watcher 监听主键
2. 链上观察主键
3. 原地换签 CAS 锁字段

规则：

1. `queued` 时可为空字符串
2. `submitted` 时必须非空
3. 旧签名过期时允许在同一行原地替换
4. 同一时刻一条记录只允许有一个当前有效签名

#### `raw_tx_base64`

类型：

- `TEXT`

来源：

- 已签名交易的 base64 编码

作用：

1. watcher/reconciler 同源重播
2. 重启恢复后继续广播

规则：

1. `queued` 时为空字符串
2. `submitted` 时必须非空
3. 与 `tx_signature` 必须一一对应
4. 换签时必须和新 `tx_signature` 一起原地替换

#### `last_valid_block_height`

类型：

- `BIGINT`

来源：

- 构造交易时获取的最新 blockhash 元数据

作用：

1. 判断当前 `tx_signature` 是否已过期
2. 是不是允许换签的唯一链上寿命依据

规则：

1. `submitted` 时必须大于 0
2. 只有换签时更新

#### `retry_count`

类型：

- `INT`

作用：

1. 表示该 batch 已经合法换签过多少次

规则：

1. 初始为 `0`
2. 只有旧签名已确认过期时才加 `1`
3. 同源重播不增加

#### `confirmation_slot`

类型：

- `BIGINT`

作用：

1. 记录链上确认 slot

规则：

1. 仅 `confirmed` 时写入
2. 其他状态保持 `0`

#### `reason_code`

类型：

- `TEXT`

作用：

1. 记录终态失败原因

推荐取值：

1. `simulation_failed`
2. `chain_execution_failed`
3. `max_retry_exceeded`
4. `invalid_batch`
5. `manual_paused`

规则：

1. 仅 `failed` 时必须非空
2. `confirmed` 时为空字符串

#### `submitted_event_published`

类型：

- `BOOLEAN`

作用：

1. 表示 `evt.settlement.submitted` 是否已成功发布

说明：

- 这是加速器事件发布标记
- 不是 correctness 依赖
- watcher 恢复仍然要依赖扫表

#### `terminal_event_published`

类型：

- `BOOLEAN`

作用：

1. 表示 `evt.settlement.confirmed` 或 `evt.settlement.failed` 是否已成功发布

说明：

- reconciler 需要依赖这个字段做终态补发

#### `version`

类型：

- `BIGINT`

作用：

1. 乐观锁版本号
2. CAS 更新辅助字段

规则：

1. 首次插入为 `1`
2. 每次状态推进、换签、终态发布补记都加 `1`

#### `created_at`

类型：

- `TIMESTAMPTZ`

作用：

1. queued 顺序调度
2. 冷启动恢复时排序

#### `updated_at`

类型：

- `TIMESTAMPTZ`

作用：

1. watcher/reconciler 识别长期无响应任务
2. 观察活跃度

### 4.3 热路径查询约束

`settlement_submissions` 同时承担：

1. 冷数据存档
2. 热状态推进

所以必须强制区分热查询与冷查询。

规则：

1. scheduler / watcher / reconciler 的热路径查询禁止 `SELECT *`
2. 热路径只允许选择当前阶段真正需要的列
3. `match_event_json` 只允许在以下场景读取：
   - submitter build/sign
   - watcher 过期换签
   - 人工排障
4. 终态扫描、lane 扫描、submitted 扫描都必须只读小列集

示例：

```sql
SELECT match_event_id
FROM settlement_submissions
WHERE market_id = $1
  AND status = 'queued'
  AND market_lane_status = 'active'
ORDER BY created_at ASC
LIMIT 1;

SELECT match_event_id, market_id, tx_signature, raw_tx_base64, retry_count, last_valid_block_height
FROM settlement_submissions
WHERE status = 'submitted'
  AND updated_at < NOW() - INTERVAL '15 seconds';
```

---

## 5. 事件定义

### 5.1 输入事件：`evt.match.batch.v3.{market_id}`

settlement 依赖以下字段必须存在：

1. `match_event_id`
2. `market_id`
3. `market_pda`
4. `orders`
5. `fills`
6. `order_updates`

要求：

1. 该事件必须足够完整，能够独立重建链上 settlement 指令
2. 该事件必须足够完整，能够在后续 `failed` 时支撑 funds/writer 精确回滚

### 5.2 输出事件：`evt.settlement.submitted.v1.{market_id}`

用途：

1. watcher 加速接管
2. pusher 低延迟展示
3. 运行观测

不是 correctness 唯一依据。

#### subject 定义

```go
const (
    SubjectSettlementSubmittedPrefix = "evt.settlement.submitted.v1"
    SubjectSettlementConfirmedPrefix = "evt.settlement.confirmed.v1"
    SubjectSettlementFailedPrefix    = "evt.settlement.failed.v1"
)

func SubjectSettlementSubmittedMarket(marketID uint64) string {
    return fmt.Sprintf("%s.%d", SubjectSettlementSubmittedPrefix, marketID)
}

func SubjectSettlementConfirmedMarket(marketID uint64) string {
    return fmt.Sprintf("%s.%d", SubjectSettlementConfirmedPrefix, marketID)
}

func SubjectSettlementFailedMarket(marketID uint64) string {
    return fmt.Sprintf("%s.%d", SubjectSettlementFailedPrefix, marketID)
}
```

#### Go struct

```go
type SettlementSubmittedEvent struct {
    EventID              string   `json:"event_id"`
    SchemaVersion        int      `json:"schema_version"`
    MatchEventID         string   `json:"match_event_id"`
    MarketID             uint64   `json:"market_id"`
    MarketPDA            string   `json:"market_pda"`
    TxSignature          string   `json:"tx_signature"`
    RetryCount           int      `json:"retry_count"`
    LastValidBlockHeight uint64   `json:"last_valid_block_height"`
    Wallets              []string `json:"wallets"`
    SubmittedAt          int64    `json:"submitted_at"`
}
```

建议结构：

```json
{
  "event_id": "settlement-submitted:match-A:0",
  "schema_version": 1,
  "match_event_id": "match-A",
  "market_id": 5256853293881055289,
  "market_pda": "4eou3xKsVyMA8qWvEDvrYcjkzcjd33F2Ehx6X1VHm8NY",
  "tx_signature": "5abc...",
  "retry_count": 0,
  "last_valid_block_height": 123456,
  "wallets": ["wallet-a", "wallet-b"],
  "submitted_at": 1775
}
```

字段要求：

1. `event_id`
   - 固定使用 `settlement-submitted:{match_event_id}:{retry_count}`
2. `match_event_id`
   - 业务关联主键
3. `tx_signature`
   - 当前在飞签名
4. `retry_count`
   - 当前已经合法换签次数
5. `last_valid_block_height`
   - watcher 判断寿命

### 5.3 输出事件：`evt.settlement.confirmed.v1.{market_id}`

用途：

1. funds 做最终转正
2. writer 标记链上成功终态

#### Go struct

```go
type SettlementConfirmedEvent struct {
    EventID       string   `json:"event_id"`
    SchemaVersion int      `json:"schema_version"`
    MatchEventID  string   `json:"match_event_id"`
    MarketID      uint64   `json:"market_id"`
    MarketPDA     string   `json:"market_pda"`
    TxSignature   string   `json:"tx_signature"`
    RetryCount    int      `json:"retry_count"`
    Slot          uint64   `json:"slot"`
    Wallets       []string `json:"wallets"`
    ConfirmedAt   int64    `json:"confirmed_at"`
}
```

建议结构：

```json
{
  "event_id": "settlement-confirmed:match-A",
  "schema_version": 1,
  "match_event_id": "match-A",
  "market_id": 5256853293881055289,
  "market_pda": "4eou3xKsVyMA8qWvEDvrYcjkzcjd33F2Ehx6X1VHm8NY",
  "tx_signature": "5abc...",
  "retry_count": 0,
  "slot": 452790030,
  "wallets": ["wallet-a", "wallet-b"],
  "confirmed_at": 1775
}
```

字段要求：

1. 必须带 `match_event_id`
2. 必须带当前成功的 `tx_signature`
3. 必须带 `wallets`
4. `event_id` 固定使用 `settlement-confirmed:{match_event_id}`

### 5.4 输出事件：`evt.settlement.failed.v1.{market_id}`

用途：

1. funds 做最终回滚
2. writer 标记链上失败终态
3. market lane 决定是否暂停后续 queued

#### Go struct

```go
type SettlementFailedEvent struct {
    EventID       string   `json:"event_id"`
    SchemaVersion int      `json:"schema_version"`
    MatchEventID  string   `json:"match_event_id"`
    MarketID      uint64   `json:"market_id"`
    MarketPDA     string   `json:"market_pda"`
    TxSignature   string   `json:"tx_signature"`
    RetryCount    int      `json:"retry_count"`
    ReasonCode    string   `json:"reason_code"`
    Wallets       []string `json:"wallets"`
    FailedAt      int64    `json:"failed_at"`
}
```

建议结构：

```json
{
  "event_id": "settlement-failed:match-A",
  "schema_version": 1,
  "match_event_id": "match-A",
  "market_id": 5256853293881055289,
  "market_pda": "4eou3xKsVyMA8qWvEDvrYcjkzcjd33F2Ehx6X1VHm8NY",
  "tx_signature": "5abc...",
  "retry_count": 3,
  "reason_code": "chain_execution_failed",
  "wallets": ["wallet-a", "wallet-b"],
  "failed_at": 1775
}
```

字段要求：

1. 必须带 `match_event_id`
2. 必须带最后一次有效 `tx_signature`
3. 必须带 `reason_code`
4. `event_id` 固定使用 `settlement-failed:{match_event_id}`

---

## 6. 内存结构

### 6.1 `market lanes`

```go
type MarketLane struct {
    MarketID uint64
    Paused bool
    CurrentMatchEventID string
}

type DirtyMarketSet struct {
    Mu   sync.Mutex
    Set  map[uint64]struct{}
    Wake chan struct{}
}
```

说明：

1. `CurrentMatchEventID` 为空表示该 market 当前没有 `submitted`
2. 非空表示该 market 正有一条 batch 在飞
3. `Paused=true` 表示该 market 后续 queued 不再自动推进

### 6.2 `watch tasks`

```go
type WatchTask struct {
    MatchEventID string
    MarketID uint64
    MarketPDA string
    TxSignature string
    RawTxBase64 string
    RetryCount int
    LastValidBlockHeight uint64
    Wallets []string
}
```

说明：

1. watcher 以 `TxSignature` 观察链上
2. watcher 以 `MatchEventID` 更新业务终态
3. `WatchTask` 是内存缓存，不是真相源
4. 真相源始终是 `settlement_submissions`

### 6.3 `WSRouter`

```go
type WSRouter interface {
    SubscribeSignature(signature string, ch chan<- SignatureResult) (unsubscribe func(), err error)
}
```

说明：

1. `WSRouter` 是进程级共享基础设施，不是 settlement 独占组件
2. settlement watcher 只向它注册订阅，不直接管理底层 websocket 连接
3. deposit / market confirm / settlement 后续都应复用这一层

建议的最小内存结构：

```go
type SignatureResult struct {
    Signature string
    Slot uint64
    Err string
    Commitment string
    ObservedAt time.Time
}

type SignatureSubscriber struct {
    SubscriberID string
    Ch chan<- SignatureResult
    Kind string // deposit | market_confirm | settlement
}

type SignatureWatch struct {
    Signature string
    Shard int
    WSSubID uint64
    RequestID uint64
    Subscribers map[string]SignatureSubscriber
    TerminalCached *SignatureResult
    CreatedAt time.Time
    UpdatedAt time.Time
}

type WSShard struct {
    Index int
    Conn *websocket.Conn
    PendingByRequestID map[uint64]string
    SignatureByWSSubID map[uint64]string
}

type WSRouterImpl struct {
    Shards []*WSShard
    Watches map[string]*SignatureWatch
    RegisterCh chan subscribeCmd
    UnregisterCh chan unsubscribeCmd
    IncomingCh chan wsMessage
    ReconnectCh chan int
}
```

结构要求：

1. `Watches` 的 key 必须是 `signature`
2. 同一个 `signature` 在同一个进程内只允许存在一个上游 websocket 订阅
3. 本地多个业务订阅者只能复用同一个 `SignatureWatch`
4. 每个业务订阅者拿到的是小缓冲本地 channel
   - 建议容量 `1~8`
   - 禁止超大缓冲

### 6.4 `WSRouter` 协程划分

建议最小协程：

1. `routerRegisterLoop`
   - 处理注册/注销命令
   - 管理 `Watches`

2. `shardWriteLoop[N]`
   - 串行向指定 shard 发送 subscribe/unsubscribe 请求

3. `shardReadLoop[N]`
   - 读取 websocket 推送
   - 解析为 `subscription_id -> signature -> SignatureResult`

4. `routerDispatchLoop`
   - 把结果 fan-out 给本地订阅者
   - 负责 terminal cache 与自动清理

5. `routerReconnectLoop`
   - shard 断线后重连
   - 重放所有当前活跃订阅

### 6.5 `WSRouter` 关键约束

1. `SubscribeSignature` 必须是幂等的
   - 同一业务重复注册同一签名，不得创建第二个上游订阅

2. `Unsubscribe` 必须是引用计数语义
   - 只有最后一个本地订阅者离开时，才向上游发 `signatureUnsubscribe`

3. terminal result 必须短暂缓存
   - 建议保留 `30s~120s`
   - 目的不是持久化，而是吸收“刚确认后立刻重复注册”的抖动

4. fan-out 必须非阻塞
   - 如果某个订阅者 channel 满了，不能拖死整个 router
   - 处理策略：
     - 记录告警日志
     - 丢弃该订阅者本次加速通知
     - correctness 交给 HTTP fallback

5. websocket 断线时不得丢失业务真相
   - 所有业务方继续按各自 fallback 逻辑运行
   - router 重连成功后再把还活着的 `signature` 全量补订阅

---

## 7. 处理规则

### 7.1 ingress

#### 输入

1. JetStream 消息：`evt.match.batch.v3.{market_id}`

#### 前置条件

1. payload 能正常反序列化
2. 必须字段存在：
   - `match_event_id`
   - `market_id`
   - `market_pda`
   - `orders`
   - `fills`

#### 数据库动作

对一批合法消息执行一次微批插入：

```sql
INSERT INTO settlement_submissions (
    match_event_id,
    market_id,
    market_pda,
    status,
    market_lane_status,
    match_event_json,
    wallets_json,
    version
)
VALUES (
    ...
), (
    ...
)
ON CONFLICT (match_event_id) DO NOTHING
RETURNING match_event_id, market_id;
```

其中：

1. `match_event_json`
   - 存完整原始事件
2. `wallets_json`
   - 存去重排序后的钱包列表
3. `market_lane_status`
   - 正常市场写 `active`
   - 已熔断 market 写 `paused`
   - 判断依据来自进程内 `lanes[market_id].Paused` 的快照
4. 实际实现应带：
   - `RETURNING match_event_id, market_id`
   - 用返回集合识别“真正新插入”的行

#### 成功分支

1. 新插入成功的消息
   - 说明是新 batch
   - 对应 `market_id` 写入 `dirtyMarkets.Set`
   - 非阻塞唤醒 scheduler
   - 立即 Ack 原始 NATS 消息

2. 冲突未插入的消息
   - 说明 `match_event_id` 已存在
   - 这是重复投递或恢复期重复扫描
   - 直接 Ack 原始消息

#### 失败分支

1. payload 格式错误或缺字段
   - `Term`
   - 不重试

2. 数据库连接/事务临时错误
   - `NakWithDelay`
   - 允许稍后重试

#### 幂等规则

1. 幂等键是 `match_event_id`
2. ingress 绝不自己决定“重新签交易”
3. ingress 唯一职责是安全接单入库

#### 技术要求

1. ingress goroutine 不等待链上确认
2. ingress goroutine 不调用 Solana 提交 RPC
3. ingress 成功入库后必须尽快 Ack，避免把 `match batch` 处理时间绑死在链上确认周期上
4. dirty 通知必须去重且非阻塞，不能因为同市场高频突发把 ingress 卡死

### 7.2 scheduler

#### 输入

1. ingress 发来的 `market dirty` 通知
2. watcher/reconciler 在终态时发来的 `market release` 通知
3. 低频兜底 ticker

#### 职责

1. 只决定“哪个 market 现在可以发车”
2. 不 build 交易
3. 不监听签名

#### 进程内调度

建议维护两个结构：

```go
dirtyMarkets DirtyMarketSet
lanes map[uint64]*MarketLane
```

调度顺序：

1. 收到 `market_id`
2. 查看对应 lane
3. 如果 `Paused=true`
   - 不推进
4. 如果 `CurrentMatchEventID != ""`
   - 说明已有 `submitted`
   - 不推进
5. 如果 lane 空闲
   - 查询最老的 `queued`
   - 交给 submitter

#### 数据库查询

只从下面集合里挑：

```sql
SELECT match_event_id
FROM settlement_submissions
WHERE market_id = $1
  AND status = 'queued'
  AND market_lane_status = 'active'
ORDER BY created_at ASC
LIMIT 1;
```

要求：

1. 禁止 `SELECT *`
2. 这里只读取最小列集

#### 成功分支

1. 找到最老 queued
2. 设置内存 lane：
   - `CurrentMatchEventID = match_event_id`
3. 投递给 submitter

#### 空转分支

1. 当前 market 没有 queued
2. 当前 market 已有 submitted
3. 当前 market 已被 paused

这些情况都不报错，直接返回。

#### 幂等规则

1. scheduler 不改变数据库终态
2. scheduler 只是发车决策层
3. 真正从 `queued -> submitted` 的状态推进必须由 submitter 的数据库 CAS 完成

#### 技术要求

1. scheduler 只能存在一个主循环
2. V1 不做多进程 market 分布式调度
3. scheduler 必须允许“重复收到同一个 market dirty 通知”，但推进结果仍然幂等

### 7.3 submitter

#### 输入

1. scheduler 交给它的一条具体 `match_event_id`

#### 前置条件

1. 对应 row 当前必须还是 `status='queued'`
2. 对应 market lane 当前必须由 scheduler 标记为在处理这条 batch

#### 读取数据

submitter 需要加载这行的：

1. `match_event_json`
2. `market_id`
3. `market_pda`
4. `wallets_json`
5. `retry_count`
6. `version`

要求：

1. 只有真正开始 build/sign 时才读取 `match_event_json`
2. 其他热路径不允许顺手读取整列 JSONB

#### 构造与签名

顺序固定：

1. 解析 `match_event_json`
2. 生成 `SubmissionBatch`
3. 查询最新 blockhash
4. 构造交易
5. 本地签名
6. 得到：
   - `raw_tx_base64`
   - `tx_signature`
   - `last_valid_block_height`

#### 先落库，再广播

submitter 必须先执行数据库 CAS：

```sql
UPDATE settlement_submissions
SET tx_signature = $2,
    raw_tx_base64 = $3,
    last_valid_block_height = $4,
    status = 'submitted',
    version = version + 1,
    updated_at = NOW()
WHERE match_event_id = $1
  AND status = 'queued'
  AND market_lane_status = 'active';
```

只有 `RowsAffected == 1` 才说明它真正拿到了这条任务。

#### 广播动作

CAS 成功后才允许广播。

广播后分两类处理：

1. 网络类不确定错误
   - 保持 `submitted`
   - 不回滚状态
   - 交给 watcher 继续查状态/重播

2. 确定性错误
   - 例如 simulation failed
   - 可以直接推进 `failed`
   - 必须走和 watcher 一样的“failed + pause queued”原子事务
   - 将 lane 标记为 `Paused=true`
   - 只释放 `CurrentMatchEventID`

#### 事件动作

submitter 在成功进入 `submitted` 后，最佳努力发布：

- `evt.settlement.submitted.v1.{market_id}`

发布失败：

1. 不回滚 `submitted`
2. 不重新签交易
3. 由 watcher/reconciler 扫表恢复

#### 幂等规则

1. submitter 绝不处理重复 `match_event_id`
2. submitter 绝不因为广播超时而新签
3. submitter 绝不因为内部 submitted 事件发布失败而新签

#### 技术要求

1. submitter goroutine 不长期等待确认
2. submitter 完成广播或广播尝试后就应退出该条任务
3. 当前 lane 的后续推进由 watcher/reconciler 在终态时触发

### 7.4 watcher

#### 输入

1. `evt.settlement.submitted`
2. reconciler 恢复出来的 `submitted` 行

#### 观察对象

watcher 只处理：

- `status='submitted'`

每个 `submitted` 对应一个 `WatchTask`，字段至少包含：

1. `match_event_id`
2. `market_id`
3. `market_pda`
4. `tx_signature`
5. `raw_tx_base64`
6. `retry_count`
7. `last_valid_block_height`
8. `wallets`

#### 单条 WatchTask 的处理顺序

1. 先做一次 HTTP `GetSignatureStatuses`
2. 如果已经 confirmed
   - 直接推进终态
3. 如果已经返回链上确定性失败
   - 直接推进 `failed`
4. 如果还未看到结果
   - 通过 `WSRouter` 注册 `signatureSubscribe`
5. 同时启动两个 ticker：
   - HTTP fallback ticker
   - same-raw-tx rebroadcast ticker
6. 还需要一个 block height ticker

#### 同源重播

同源重播的定义：

- 对完全相同的 `raw_tx_base64` 再次广播

特点：

1. `tx_signature` 不变
2. `retry_count` 不变
3. `status` 不变
4. 不产生新的业务对象

#### 终态推进

1. confirmed：
   - 执行 `submitted -> confirmed` CAS
   - 填充 `confirmation_slot`
   - 发布 `evt.settlement.confirmed`
   - 释放 market lane

2. 确定性失败：
   - 必须在同一个数据库事务中完成：
     - 当前行 `submitted -> failed`
     - 同 market 仍处于 `queued` 的行全部 `market_lane_status='paused'`
   - 填写 `reason_code`
   - 将内存 lane 标记为 `Paused=true`
   - 发布 `evt.settlement.failed`
   - 释放当前 `CurrentMatchEventID`

#### 过期换签

过期判断标准只有一个：

- `current_block_height > last_valid_block_height`

满足条件后 watcher 才允许换签。

换签顺序：

1. 从当前 row 读取完整 `match_event_json`
2. 获取新的 blockhash
3. 重建并重签交易
4. 原地执行换签 CAS：
   - 锁住旧 `tx_signature`
   - 替换成新 `tx_signature`
   - 替换成新 `raw_tx_base64`
   - 替换成新 `last_valid_block_height`
   - `retry_count + 1`
5. 广播新的 `raw_tx`
6. 刷新内存 `WatchTask`
7. 最佳努力重新发布一次 `evt.settlement.submitted`

#### 幂等规则

1. 同一个 `match_event_id` 最多只有一个当前有效 `WatchTask`
2. HTTP 与 WS 同时返回终态时，只有一个 CAS 能成功
3. 过期换签时，只有锁住旧 `tx_signature` 的那个协程能成功替换
4. 确定性失败时，“标记失败”和“暂停后续 queued”必须原子完成，不能分两步异步处理

#### 技术要求

1. watcher 与 submitter 必须解耦
2. watcher 不能阻塞 ingress
3. watcher 必须允许：
   - WS 断线重连
   - HTTP fallback
   - 进程重启后重建任务

### 7.5 reconciler

#### 输入

1. 进程启动事件
2. 周期性 ticker

#### 职责

1. 冷启动恢复
2. 终态补发
3. 漏监听修复
4. 长时间无响应任务修复

#### 启动恢复

进程启动时执行：

1. 扫描所有 `market_lane_status='paused'`
   - 先在内存 `lanes` 中恢复 `Paused=true`

2. 扫描所有 `status='queued'`
   - 通知 scheduler 恢复 market 调度

3. 扫描所有 `status='submitted'`
   - 重新构建 `WatchTask`
   - 交给 watcher

4. 扫描所有 `status in ('confirmed','failed') and terminal_event_published=false`
   - 做终态补发

#### 运行时周期任务

每个 ticker 周期执行：

1. 扫描长时间未更新的 `submitted`
   - 如果 watcher 不在内存中，重新挂回去

2. 扫描终态未发布的 `confirmed/failed`
   - 重新发布终态事件

3. 扫描 `queued` 且 lane 空闲的 market
   - 通知 scheduler 再试一次

#### 幂等规则

1. reconciler 不直接修改 `queued -> submitted`
2. reconciler 不直接发布成功之前的链上业务结果
3. reconciler 的所有修复动作必须依赖现有 row 当前状态

#### 技术要求

1. reconciler 必须存在
2. 不能只依赖 `evt.settlement.submitted`
3. 不能假设 watcher 永远在线
4. 不能假设终态事件发布永远成功

### 7.6 `WSRouter`

#### 输入

1. `SubscribeSignature(signature, subscriber)`
2. `Unsubscribe(signature, subscriber_id)`
3. shard websocket 推送
4. shard 断线/重连事件

#### 注册规则

1. deposit / market confirm / settlement 三类业务全部通过同一个 `WSRouter` 注册
2. 注册参数至少包含：
   - `signature`
   - `subscriber_id`
   - `kind`
   - `result channel`
3. 若该 `signature` 已存在：
   - 只追加本地订阅者
   - 不重复向 Solana 发 `signatureSubscribe`
4. 若该 `signature` 不存在：
   - 根据 `hash(signature) % shard_count` 选择 shard
   - 创建 `SignatureWatch`
   - 生成 `request_id`
   - 向 shard 发送一次 `signatureSubscribe`

#### 注销规则

1. 单个订阅者退出时，只移除本地 subscriber
2. 若该 `signature` 仍有其他本地 subscriber：
   - 不向 Solana 退订
3. 若最后一个 subscriber 退出：
   - 向对应 shard 发送 `signatureUnsubscribe`
   - 删除本地 `SignatureWatch`

#### 消息分发规则

1. shard 收到 websocket 推送后：
   - 先用 `subscription_id` 找到 `signature`
   - 再构造 `SignatureResult`
2. `routerDispatchLoop` 再把结果 fan-out 给所有本地 subscriber
3. 对 `signatureSubscribe` 来说，收到终态后应：
   - 缓存 terminal result
   - 自动关闭上游订阅
   - 在 TTL 到期后清理本地 watch

#### 回压规则

1. 本地 subscriber channel 必须是小缓冲
   - 建议容量 `1`
2. fan-out 时必须使用非阻塞写
3. 如果写失败：
   - 记录日志：
     - `signature`
     - `subscriber_id`
     - `kind`
   - 不阻塞 router 主循环
   - 不影响其他 subscriber

#### 断线重连规则

1. 任一 shard websocket 断线：
   - 把该 shard 标记为 `disconnected`
   - 触发重连循环
2. 重连成功后：
   - 遍历该 shard 当前所有仍然活跃的 `SignatureWatch`
   - 重新发送 `signatureSubscribe`
   - 更新：
     - `request_id`
     - `ws subscription id`
3. 断线期间：
   - deposit / market confirm 继续做 HTTP fallback
   - settlement watcher 继续做 HTTP fallback + expiry 判断

#### 与 deposit / market confirm 的接入方式

这两个模块都不应再自己管理 websocket 连接。

它们的流程应改成：

1. 先做一次 HTTP `GetSignatureStatuses`
2. 若未终态：
   - 向 `WSRouter` 注册
3. 等本地 result channel 或 context timeout
4. 若超时或 router 失败：
   - 再走 HTTP fallback

这样做的结果是：

1. deposit confirm 与 market confirm 共用一套底层连接
2. 不再各自维护独立 `Waiter.ws`
3. 不会因为同时有多个模块等待确认而线性增加 websocket 连接数

#### 与 settlement watcher 的接入方式

settlement watcher 不直接调用原始 websocket 客户端。

它的顺序固定为：

1. 初始 HTTP 查询
2. 若未终态：
   - 向 `WSRouter` 注册
3. 同时保留：
   - HTTP fallback ticker
   - rebroadcast ticker
   - block height ticker
4. 收到 router 推送后：
   - 尝试做终态 CAS
5. 若 watcher 因换签换了 `tx_signature`
   - 先退订旧签名
   - 再订阅新签名

#### 正确性边界

1. `WSRouter` 不是 WAL
2. `WSRouter` 不落库
3. `WSRouter` 不发布业务终态事件
4. `WSRouter` 只负责把链上 websocket 结果尽快送达本地业务协程
5. 任何 correctness 都必须仍然依赖：
   - Postgres 状态
   - HTTP 查询
   - reconciler 恢复

---

## 8. CAS 更新模板

### 8.1 `queued -> submitted`

```sql
UPDATE settlement_submissions
SET tx_signature = $2,
    raw_tx_base64 = $3,
    last_valid_block_height = $4,
    status = 'submitted',
    version = version + 1,
    updated_at = NOW()
WHERE match_event_id = $1
  AND status = 'queued'
  AND market_lane_status = 'active';
```

### 8.2 `submitted -> confirmed`

```sql
UPDATE settlement_submissions
SET status = 'confirmed',
    confirmation_slot = $3,
    version = version + 1,
    updated_at = NOW()
WHERE match_event_id = $1
  AND status = 'submitted'
  AND tx_signature = $2;
```

### 8.3 `submitted -> failed`

```sql
BEGIN;

WITH failed_row AS (
    UPDATE settlement_submissions
    SET status = 'failed',
        market_lane_status = 'paused',
        reason_code = $3,
        version = version + 1,
        updated_at = NOW()
    WHERE match_event_id = $1
      AND status = 'submitted'
      AND tx_signature = $2
    RETURNING market_id
)
UPDATE settlement_submissions
SET market_lane_status = 'paused',
    version = version + 1,
    updated_at = NOW()
WHERE market_id IN (SELECT market_id FROM failed_row)
  AND status = 'queued'
  AND market_lane_status = 'active';

COMMIT;
```

### 8.4 `submitted` 内原地换签

```sql
UPDATE settlement_submissions
SET tx_signature = $3,
    raw_tx_base64 = $4,
    last_valid_block_height = $5,
    retry_count = retry_count + 1,
    version = version + 1,
    updated_at = NOW()
WHERE match_event_id = $1
  AND status = 'submitted'
  AND tx_signature = $2;
```

说明：

1. 只有 watcher/reconciler 认定旧签名已过期时，才允许执行
2. 条件里必须锁住旧 `tx_signature`

---

## 9. 必须满足的开发约束

1. 不允许因为 NATS 重投而重新签一笔新交易
2. 不允许因为 watcher 重启而重新签一笔新交易
3. 不允许因为 `evt.settlement.confirmed` 发布失败而重新签一笔新交易
4. 只有区块高度已超过 `last_valid_block_height`，才允许换签
5. 同一 market 同时不允许两条 `submitted`
6. 所有终态事件都必须带 `match_event_id`
7. `funds` 和 `writer` 必须把 `match_event_id` 作为 settlement 终态消费主键

---

## 10. 本文覆盖关系

本文对 settlement 开发层面覆盖以下旧表述：

1. 不再使用独立 `attempt_id`
2. 不再把 `prepared / expired_retryable` 作为必须的显式状态
3. `submitted` 内部允许：
   - 同源重播
   - 原地换签
   - 冷启动恢复
4. settlement 的最小可落地状态机固定为：
   - `queued`
   - `submitted`
   - `confirmed`
   - `failed`
