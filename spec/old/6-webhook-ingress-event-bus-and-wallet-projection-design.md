# 6. Webhook 事件入口、NATS 分流与 Shared Wallet 投影设计

本文是 BlinkPredict 系统最后一块核心设计文档，专门描述：

1. 所有 webhook 相关接口如何设计
2. 为什么采用 `Alchemy + Helius` 双入口
3. webhook 入口如何只做**分类与投递**
4. 后续如何按事件类型消费、入库、更新 Redis、更新 shared wallet
5. 为什么 shared wallet 的最终外部资产更新要来源于这里

本文建立在以下文档之上：

- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/0-contractdesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/3-matcher-shared-wallet-batching-redesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/4-writer-pusher-redis-websocket-redesign.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/5-settlement-contract-implementation.md`
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/7-crash-recovery-perfect-startup-and-cold-boot-order.md`

---

## 0. 设计结论

整个 webhook 系统最终定稿为：

- `alchemy.go`
  - 负责接收 **BlinkPredict 程序自定义事件**
  - 在这里完成事件识别 / 分类
  - 不直接做重业务
  - 只把标准化后的内部 domain event 投递到 NATS

- `helius.go`
  - 负责接收 **增强交易类型**
  - 重点处理：
    - `DepositSettled`
    - `Withdrawn`
    - 其他直接涉及 SPL Token 余额变化、但不是自定义 Anchor event 的场景
  - 同样只负责标准化与投递 NATS

- 后续所有真实业务处理都在消费者里做：
  - 数据库落库
  - 市场索引更新
  - `user_position_accounts` 更新
  - settlement 提交状态推进
  - shared wallet 外部资产状态更新
  - Redis / 读模型 / websocket 所需的后续投影

也就是说，webhook 层的职责应该被严格限定为：

- **provider adapter + event classifier + NATS publisher**

而不是：

- “直接写数据库”
- “直接更新 shared wallet”
- “直接更新 matcher 内存”

---

## 1. 这个方案是否符合业界主流

结论：**是的，而且是更稳的方案之一。**

你提出的思路本质上是：

- 把 webhook 入口做薄
- 把业务处理做成异步消费者
- 所有外部链上状态变化先进入总线，再由不同投影器消费

这和主流事件驱动系统非常一致，尤其适合你们现在这种：

- 链下撮合
- 链上最终结算
- 多个内存态 / 投影态并存
- 需要 crash recovery

的系统。

### 1.1 为什么这是对的

因为 webhook provider 本身天然不稳定：

- 可能重复推送
- 可能乱序
- 可能延迟
- 可能瞬时抖动

如果直接在 webhook handler 里做重业务，会有几个问题：

- handler 逻辑越来越重
- 重复消息幂等难做
- 不利于失败重试
- 不利于恢复期 replay

把 webhook 变成：

- `provider payload -> internal domain event -> NATS`

以后，整个系统会更清晰。

### 1.2 是否必须双 provider

不是“绝对必须”，但以你们当前需求看，`Alchemy + Helius` 双 provider 是合理的：

- `Alchemy`
  - 更适合盯 BlinkPredict program 自定义事件
  - 适合分类自定义 Anchor event

- `Helius`
  - 更适合盯增强后的 token / transaction 变化
  - 尤其适合 SPL token 余额变化、deposit / withdraw 这种不一定是你们自定义事件直接覆盖的场景

所以这不是“奇怪的双写设计”，而是：

- 一个负责程序语义
- 一个负责资产语义

这是完全合理的分工。

---

## 2. webhook 设计总原则

webhook 设计必须严格遵守以下原则。

### 2.1 原则一：webhook 层不做重业务

webhook handler 只做：

1. 鉴权
2. payload 解析
3. provider-specific -> internal event 转换
4. 投递到 NATS
5. 快速返回 200

不得在 webhook handler 中直接做：

- 复杂 DB 事务
- shared wallet 大量修改
- matcher 订单簿修正
- Redis 多模型更新

### 2.2 原则二：所有 provider 都先归一化成统一事件

无论来自：

- Alchemy
- Helius

进入系统后的第一件事都应该是：

- 转成统一的内部 domain event

这样后续消费者就不需要关心 provider 差异。

### 2.3 原则三：shared wallet 的外部状态更新必须来源于 webhook 事件消费

shared wallet 里有两类状态：

- 意图类、乐观类更新
  - matcher 可直接改
- 外部真实资产更新
  - 必须来源于 webhook 事件消费

这点必须彻底写死。

### 2.4 原则四：所有 webhook 事件必须幂等

任何一条链上事件都必须支持：

- 重复投递
- 重复消费

所以需要：

- webhook ingress receipt
- domain event id
- consumer cursor / processed marker

---

## 3. 提供方职责划分

## 3.1 `alchemy.go` 的职责

`alchemy.go` 定位为：

- BlinkPredict 程序事件入口
- 自定义事件分类器
- NATS 发布器

它应该重点识别：

- `MarketCreated`
- `MatchSettled`
- `OrderCanceled`
- `CancelAllBeforeUpdated`
- `UserPositionInitialized`
- `OrderStateClosed`
- `UserPositionClosed`
- `CreatorFeeWithdrawn`
- `PlatformFeeWithdrawn`
- 后续所有合约 emit 的自定义事件

也就是说：

- 只要是合约里自己 emit 的业务事件
- 都优先走 Alchemy program-event 路线

## 3.2 `helius.go` 的职责

`helius.go` 定位为：

- 资产变化入口
- 增强交易分类器
- SPL / 账户变化事件入口

它应重点处理：

- `DepositSettled`
- `Withdrawn`
- 未来如果有“链上直接 token movement 但合约事件不足以完整表达”的场景

原因：

- deposit / withdraw 最终直接体现在 SPL token 变化
- Helius 增强类型更容易直接拿到 token transfer 级别的信息

---

## 4. 内部统一事件模型

所有 provider 进入系统后，都先转换成统一内部 envelope。

推荐统一结构：

```go
type WebhookEventEnvelope struct {
    EventID        string    `json:"event_id"`
    Provider       string    `json:"provider"`
    ProviderEventID string   `json:"provider_event_id,omitempty"`
    Signature      string    `json:"signature,omitempty"`
    Slot           uint64    `json:"slot,omitempty"`
    BlockTime      int64     `json:"block_time,omitempty"`
    EventType      string    `json:"event_type"`
    SchemaVersion  int       `json:"schema_version"`
    ProducedAt     time.Time `json:"produced_at"`
    Payload        any       `json:"payload"`
}
```

字段说明：

- `event_id`
  - 系统内部唯一事件 ID，幂等主键
- `provider`
  - `alchemy` / `helius`
- `provider_event_id`
  - provider 原始事件 id，若有则保留
- `signature`
  - Solana tx signature
- `slot`
  - 链上 slot，便于排序与对账
- `event_type`
  - BlinkPredict 内部域事件类型
- `payload`
  - 已归一化后的业务数据

---

## 5. 事件类型总表

推荐内部 event type 定稿如下。

## 5.1 市场与账户生命周期类

- `webhook.market.created.v1`
- `webhook.market.resolved.v1`
- `webhook.user_position.initialized.v1`
- `webhook.order_state.closed.v1`
- `webhook.user_position.closed.v1`

## 5.2 结算与订单控制类

- `webhook.match.settled.v1`
- `webhook.order.canceled.v1`
- `webhook.cancel_all_before.updated.v1`

## 5.3 外部资产变化类

- `webhook.deposit.settled.v1`
- `webhook.withdraw.settled.v1`
- `webhook.split.executed.v1`
- `webhook.merge.executed.v1`
- `webhook.winnings.claimed.v1`
- `webhook.creator_fee.withdrawn.v1`
- `webhook.platform_fee.withdrawn.v1`

这里面：

- `deposit / withdraw`
  - 优先由 `helius.go` 产生
- 其他自定义业务事件
  - 优先由 `alchemy.go` 产生

---

## 6. NATS subject 设计

建议单独开一条 webhook domain stream，不和 matcher 事件混在一起。

推荐 stream：

- `AP_WHK`：`whk.>`

subject 设计如下：

### 6.1 原始 provider 归一化事件

- `whk.alchemy.market.created`
- `whk.alchemy.match.settled`
- `whk.alchemy.order.canceled`
- `whk.alchemy.cancel_all_before.updated`
- `whk.alchemy.user_position.initialized`
- `whk.alchemy.creator_fee.withdrawn`
- `whk.alchemy.platform_fee.withdrawn`

- `whk.helius.deposit.settled`
- `whk.helius.withdraw.settled`
- `whk.helius.split.executed`
- `whk.helius.merge.executed`
- `whk.helius.winnings.claimed`

### 6.2 可选的 provider-agnostic 统一 subject

如果后面消费者不想按 provider 订阅，可以在 ingress 后二次标准化成：

- `whk.market.created`
- `whk.match.settled`
- `whk.order.canceled`
- `whk.cancel_all_before.updated`
- `whk.deposit.settled`
- `whk.withdraw.settled`
- `whk.split.executed`
- `whk.merge.executed`
- `whk.winnings.claimed`

本设计更推荐：

- provider 入口先发 provider-specific subject
- 再由统一 normalizer 发 provider-agnostic subject

这样链路更可观测。

---

## 7. `alchemy.go` 的详细设计

## 7.1 输入

`alchemy.go` 接收：

- Alchemy webhook POST 请求

它必须完成：

1. HMAC 校验
2. payload 解析
3. 提取 tx signature / logs / slot / block time
4. 从 logs 中识别 Anchor event
5. 映射成内部 domain event
6. 投递 NATS

## 7.2 事件分类职责

`alchemy.go` 中必须有一个显式的 event classifier：

```go
func classifyAlchemyProgramEvent(...) (eventType string, payload any, err error)
```

它负责把 program data 解出后，映射到内部域事件。

### 7.2.1 为什么必须集中分类

因为后续合约事件会越来越多：

- 市场类
- 结算类
- 订单控制类
- 费率类

如果每个 handler 各自乱解析，后期会失控。

## 7.3 `alchemy.go` 只发布，不落库

`alchemy.go` 不直接：

- `markets.Save`
- 更新 shared wallet
- 更新 `user_position_accounts`

这些都应该移动到消费者里。

也就是说，当前 `alchemy.go` 里已经存在的“直接处理市场创建”的做法，在新设计下应收敛为：

- 只发布 `whk.alchemy.market.created`

然后由 market consumer 去写库。

---

## 8. `helius.go` 的详细设计

## 8.1 输入

`helius.go` 接收：

- Helius webhook POST 请求

它必须完成：

1. token / auth 校验
2. payload 解析
3. 增强交易分类
4. 将资产变化类事件映射为内部 domain event
5. 投递 NATS

## 8.2 deposit / withdraw 的判定原则

这类事件不应只看“有 token transfer”。

必须同时结合：

- 目标 mint 是否是系统 collateral mint
- from / to 是否涉及：
  - GlobalVault
  - 用户钱包 / 用户 ATA
- 指令上下文是否匹配系统 deposit / withdraw 路径

推荐分类器：

```go
func classifyHeliusEnhancedTx(...) (eventType string, payload any, ok bool, err error)
```

### 8.2.1 `DepositSettled`

推荐判定规则：

- 用户钱包 / ATA -> 系统 GlobalVault
- mint == collateral mint
- 数量 > 0
- 交易成功

产生 payload：

```go
type DepositSettledPayload struct {
    WalletAddress string `json:"wallet_address"`
    Mint          string `json:"mint"`
    AmountUnits   uint64 `json:"amount_units"`
    Signature     string `json:"signature"`
    Slot          uint64 `json:"slot"`
    BlockTime     int64  `json:"block_time"`
}
```

### 8.2.2 `Withdrawn`

推荐判定规则：

- 系统 GlobalVault -> 用户钱包 / ATA
- mint == collateral mint
- 数量 > 0
- 交易成功

产生 payload：

```go
type WithdrawSettledPayload struct {
    WalletAddress string `json:"wallet_address"`
    Mint          string `json:"mint"`
    AmountUnits   uint64 `json:"amount_units"`
    Signature     string `json:"signature"`
    Slot          uint64 `json:"slot"`
    BlockTime     int64  `json:"block_time"`
}
```

## 8.3 Helius 的定位

在最终架构里，Helius 不负责：

- 业务最终解释
- 直接写 DB

它只负责：

- 从增强交易视角把“真实资产变化”捕获进系统

---

## 9. 消费者划分

webhook 进入 NATS 后，建议按领域拆消费者，而不是一个大 webhook worker 全做完。

## 9.1 `market-webhook-consumer`

消费：

- `whk.*.market.created`
- `whk.*.market.resolved`

职责：

- 更新 `markets` 表
- 更新 market cache / Redis

## 9.2 `settlement-webhook-consumer`

消费：

- `whk.*.match.settled`
- `whk.*.order.canceled`
- `whk.*.cancel_all_before.updated`

职责：

- 推进 `settlement_submissions` 状态
- 更新 `match_batches` 生命周期
- 更新 `user_position_accounts`
- 对接 shared wallet pending 确认 / 回滚所需状态

## 9.3 `wallet-webhook-consumer`

消费：

- `whk.*.deposit.settled`
- `whk.*.withdraw.settled`
- `whk.*.split.executed`
- `whk.*.merge.executed`
- `whk.*.winnings.claimed`

职责：

- 更新 shared wallet 的外部资产基线
- 更新 DB 中的钱包 / 持仓投影（若保留）
- 更新 Redis 查询缓存

### 9.3.1 这是 shared wallet 的真实外部来源

必须写死：

- shared wallet 的外部资产变化只接受这一路消费者更新

matcher 不能直接“伪造” deposit / withdraw / split / merge 的真相。

## 9.4 `fee-webhook-consumer`

消费：

- `whk.*.creator_fee.withdrawn`
- `whk.*.platform_fee.withdrawn`

职责：

- 更新收益统计
- 修正市场收益展示

---

## 10. shared wallet 更新规则在 webhook 里的落点

这部分是本文最关键的业务结论之一。

## 10.1 哪些必须来源于 webhook consumer

以下变化必须由 webhook consumer 推动 shared wallet：

- `DepositSettled`
- `Withdrawn`
- `SplitExecuted`
- `MergeExecuted`
- `MatchSettled` 的最终确认
- `OrderCanceled` / `CancelAllBeforeUpdated` 的链上强制纠偏

### 10.1.1 `DepositSettled`

- 增加 `available_usdc`

### 10.1.2 `Withdrawn`

- 扣减 `available_usdc`

### 10.1.3 `SplitExecuted`

- 扣 `available_usdc`
- 加 `available_yes_shares`
- 加 `available_no_shares`

这里的 `*_shares` 字段都不是浮点数，而是 `shares * 100` 的整数 lots。

### 10.1.4 `MergeExecuted`

- 扣对应 YES / NO 份额
- 加 `available_usdc`

### 10.1.5 `MatchSettled`

- 清理对应 `pending_*`
- 将本地乐观状态与链上最终状态抹平

### 10.1.6 `OrderCanceled` / `CancelAllBeforeUpdated`

- 强制释放相关 lock
- 更新 `cancel_all_before_ts`
- 必要时踢掉 matcher 簿中的脏订单

## 10.2 哪些不应由 webhook 更新 shared wallet

以下不应该由 webhook 直接碰 shared wallet：

- 市场创建
- 市场 resolve
- fee withdraw 这类非用户资产相关事件

---

## 11. webhook 幂等设计

所有 webhook 事件必须具备幂等性。

推荐 event id 生成规则：

### 11.1 对自定义 program event

推荐：

```text
event_id = provider + ":" + signature + ":" + event_type + ":" + event_index
```

### 11.2 对增强 token transfer 事件

推荐：

```text
event_id = provider + ":" + signature + ":" + mint + ":" + wallet + ":" + amount + ":" + direction
```

### 11.3 receipt 表

建议新增：

```sql
CREATE TABLE webhook_receipts (
    event_id            TEXT PRIMARY KEY,
    provider            TEXT NOT NULL,
    event_type          TEXT NOT NULL,
    signature           TEXT,
    slot                BIGINT,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload_json        JSONB NOT NULL
);
```

用途：

- provider 重复推送时做幂等
- recovery 时可追查 webhook 是否入站

---

## 12. webhook 与 7 号恢复文档的关系

7 号文档里已经定义：

- shared wallet 的恢复需要：
  - base
  - locked
  - pending

webhook 在这里扮演的是：

- 推进 `base`
- 推进 `pending` 的最终确认 / 回滚

因此在系统恢复期：

- webhook consumer 必须早于 Gateway 写流量开放完成 catch-up
- 否则 shared wallet 的外部基线会落后

也就是说，webhook 系统是：

- shared wallet 正确性
- balance 校验正确性
- crash recovery 最终闭环

的核心组成部分。

---

## 13. 冷启动时 webhook 模块的顺序

结合 7 号文档，webhook 模块在冷启动中应处于：

- `BOOT_SETTLEMENT_RECONCILE` 之前开始准备
- `BOOT_OPEN_FOR_WRITE` 之前完成 catch-up

推荐顺序：

1. DB / NATS 初始化
2. recovery cursor 恢复
3. webhook consumer 恢复并 replay 未处理事件
4. shared wallet base / pending 修正
5. settlement registry / batch 生命周期修正
6. 最后开放 Gateway 写入口

注意：

- webhook HTTP 入口可以早起
- 但真正的业务 consumer 必须先进入恢复模式，再进入实时模式

---

## 14. 推荐的最终模块划分

推荐最终保留如下模块：

### 14.1 ingress 层

- `alchemy.go`
- `helius.go`

职责：

- provider adapter
- auth verify
- payload parse
- classify
- publish to NATS

### 14.2 consumer 层

- `market-webhook-consumer`
- `wallet-webhook-consumer`
- `settlement-webhook-consumer`
- `fee-webhook-consumer`

职责：

- 按领域落库与更新内存投影

### 14.3 projection 层

- DB
- Redis
- shared wallet
- `user_position_accounts`
- `match_batches`
- `settlement_submissions`

---

## 15. 最终定稿

系统所有 webhook 相关接口的最终设计定为：

- `alchemy.go`
  - 负责 BlinkPredict program 自定义事件分类
  - 在这里做事件识别与 NATS 投递
- `helius.go`
  - 负责增强交易类型解析
  - 重点承接 `deposit / withdraw` 这类直接涉及 SPL token 余额变化的场景
- webhook handler 本身不做重业务
- 所有真实业务更新都通过 NATS 下游消费者完成
- shared wallet 的外部资产更新来源于 webhook 事件消费，而不是 matcher
- `deposit / withdraw` 走 Helius 是合理且推荐的方案
- 这套设计符合主流事件驱动架构，也最有利于：
  - 幂等
  - 恢复
  - shared wallet 正确性
  - 后续扩展

这份文档作为 webhook 系统设计总纲冻结，后续实现必须围绕：

- ingress 轻量化
- domain event 统一化
- consumer 领域拆分
- shared wallet 来源单一化

这四条原则推进。
