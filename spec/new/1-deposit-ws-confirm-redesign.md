# Deposit WebSocket 确认链路改造

## 目标

围绕当前手动测试主线，建立一条真实可用的 `deposit` 主链路：

1. 前端钱包直接发送链上 `deposit`
2. 前端拿到交易 `signature` 后提交给 gateway
3. gateway 将待确认记录写入数据库，并发布 NATS 命令
4. deposit confirm worker 通过 Solana WebSocket `signatureSubscribe` 等待确认
5. worker 在链上确认后，进一步校验这笔交易确实是当前程序的真实 `deposit`
6. worker 发布 `evt.deposit.confirmed.v1`
7. funds 消费事件并更新 shared wallet 内存余额
8. writer 消费事件并更新 Postgres / Redis 投影

本次改造中，Helius webhook 不参与主链路。

## 本轮边界

本轮只处理 `deposit`。

本轮不处理：

1. split / merge / claim 的前端直发交易确认
2. 所有业务动作的统一链上确认框架完全体
3. phrase 7 的完整重启恢复体系
4. webhook 驱动的余额写入

## 模块职责边界

### Gateway

职责：

1. 提供 `POST /api/deposits`
2. 只做轻量参数校验
3. upsert `deposit_submissions`
4. 发布 `cmd.tx.confirm.deposit.v1`

不负责：

1. 本轮不做强 auth wallet 绑定
2. 不等待链上确认
3. 不直接改余额

### ChainConfirm

这是共享基础设施模块。

职责：

1. 等待某个 signature 达到 `confirmed`
2. 优先使用 Solana WebSocket `signatureSubscribe`
3. 必要时回退到 HTTP 状态检查
4. 封装 ws 连接、复用、失败重置与超时逻辑

不负责：

1. 不做业务交易解析
2. 不写数据库
3. 不发布 NATS

### Deposit Confirm Worker

职责：

1. 消费 `cmd.tx.confirm.deposit.v1`
2. 驱动 `deposit_submissions` 状态流转：`submitted -> watching -> confirmed|failed|expired`
3. 调用 `chainconfirm`
4. 拉取并校验链上交易内容
5. 发布 `evt.deposit.confirmed.v1` 或 `evt.deposit.failed.v1`

### Funds

职责：

1. 消费 `evt.deposit.confirmed.v1`
2. 调用 `funds.Manager.ApplyDepositConfirmed(wallet, amount)`

这里只负责 shared wallet 内存状态。

### Writer

职责：

1. 消费 `evt.deposit.confirmed.v1`
2. 将 `deposit_submissions.status` 更新为 `confirmed`
3. upsert `wallet_accounts`
4. 刷新 Redis `wallet-account:{wallet}`

### Query

职责：

1. 读取 Postgres / Redis 投影
2. 提供只读查询接口

Query 不消费 deposit 事件。

## NATS 主题

### 命令主题

主题：

`cmd.tx.confirm.deposit.v1`

消息体：

```json
{
  "signature": "5abc...",
  "wallet_address": "...",
  "amount_units": 100
}
```

设计理由：

1. 这是典型工作队列语义
2. 同一条确认命令只应由一个 worker 实例处理
3. payload 保持最小
4. program id / mint / vault 这类固定配置直接从本地 env 获取，不进入 NATS

选择：

1. JetStream
2. durable consumer
3. queue consumer
4. command stream 继续使用 work-queue retention

### 确认成功事件

主题：

`evt.deposit.confirmed.v1`

消息体：

```json
{
  "signature": "5abc...",
  "wallet_address": "...",
  "amount_units": 100,
  "slot": 123456789
}
```

设计理由：

1. 这是事实事件，应该允许多个模块各自消费
2. funds 和 writer 都需要同一条确认事实

选择：

1. JetStream
2. durable fan-out consumer
3. event stream 继续使用 limits-based retention

### 确认失败事件

主题：

`evt.deposit.failed.v1`

消息体：

```json
{
  "signature": "5abc...",
  "wallet_address": "...",
  "reason": "transaction_not_deposit"
}
```

这个事件只用于可观测性与恢复分析，不参与余额修改。

## 数据库表

新增表：`deposit_submissions`

用途：

1. gateway intake 幂等
2. worker 状态跟踪
3. 重启后的恢复基础
4. 测试与排障可观测性

目标表结构：

```sql
CREATE TABLE IF NOT EXISTS deposit_submissions (
    signature TEXT PRIMARY KEY,
    wallet_address VARCHAR(44) NOT NULL,
    amount_units BIGINT NOT NULL CHECK (amount_units > 0),
    status TEXT NOT NULL CHECK (status IN ('submitted', 'watching', 'confirmed', 'failed', 'expired')),
    failure_reason TEXT NOT NULL DEFAULT '',
    slot BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ
);
```

索引：

```sql
CREATE INDEX IF NOT EXISTS idx_deposit_submissions_status_created
    ON deposit_submissions (status, created_at);

CREATE INDEX IF NOT EXISTS idx_deposit_submissions_wallet_created
    ON deposit_submissions (wallet_address, created_at DESC);
```

## Gateway 接口

接口：

`POST /api/deposits`

请求体：

```json
{
  "signature": "5abc...",
  "wallet_address": "...",
  "amount_units": 100
}
```

本轮校验规则：

1. `signature` 必填，且能解析为 Solana signature
2. `wallet_address` 必填，且能解析为 Solana public key
3. `amount_units > 0`

本轮不做强 auth wallet 绑定。

## Confirm Worker 状态机

状态：

1. `submitted`
2. `watching`
3. `confirmed`
4. `failed`
5. `expired`

流程：

1. 消费确认命令
2. 如果已是 `confirmed`，直接 ack
3. 标记为 `watching`
4. 先做一次 HTTP 快速状态检查
5. 未确认则走 WebSocket 等待
6. ws 失败或超时则回退到 HTTP 再检查一次
7. 如果链上已确认，则继续校验交易内容
8. 校验通过则发布 confirmed；校验失败则发布 failed
9. 更新 submission 行状态

## Deposit 交易校验规则

只有“signature 已 confirmed”还不够。

worker 必须继续校验：

1. 交易执行成功
2. 交易中包含当前 `PROGRAM_ID`
3. 交易中包含 Anchor `deposit` 指令 discriminator
4. 指令账户中使用的是当前配置下的 global vault 和 mint
5. 指令中的 user 与提交记录中的 `wallet_address` 一致
6. 指令里的 amount 与提交记录中的 `amount_units` 一致

只有全部通过，才允许发布 `evt.deposit.confirmed.v1`。

## ChainConfirm 的扩展性

本次虽然只落地 `deposit`，但 `chainconfirm` 从一开始就按可复用基础设施设计。

后续可以沿同一模式扩展：

1. `cmd.tx.confirm.split.v1`
2. `cmd.tx.confirm.merge.v1`
3. `cmd.tx.confirm.claim.v1`

未来每一种业务确认 worker 都应复用同一套确认层，只替换业务交易解析与校验逻辑。
