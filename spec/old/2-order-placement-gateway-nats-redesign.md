# BlinkPredict 市场已创建后的下单签名 -> Gateway -> NATS 设计文档

本文只讨论一个明确范围：**市场已经创建完成，前端如何生成订单签名，Gateway 如何接收并转换订单，然后直接投递到 NATS**。

本文不讨论的内容：

- 本期不讨论链上 `settle_match_batch` 的实际提交流程
- 本期不讨论 matcher 之后的落库闭环
- 本期不讨论订单、成交、持仓的最终数据库投影
- 本期不讨论链上结算时如何再次验证签名

因此，这份文档的重点是：

1. 前端签什么
2. Gateway 收什么
3. Gateway 做什么校验与字段转换
4. Gateway 如何直接投递到 NATS
5. 这条链路里前端、合约端、后端、DB 配置分别需要承担什么职责

这次设计严格遵循 `0-contractdesign` 的主线：

- 用户签的是**原始交易意图**，不是内部归一化后的撮合命令
- 限价单和市价单共用同一套 `OrderIntentV1`
- 市价单本质上是前端根据盘口计算滑点保护价后构造成的特殊订单
- Gateway **永远不做订单密码学验签**，而是只做协议合法性校验、钱包登录身份对齐、字段转换、NATS 投递
- Gateway 负责把签名订单解释成内部命令，但**不在本期承担链上结算职责**
- 这条链路当前**不经过数据库落库**，而是直接投入 NATS

相关背景文件与现有实现：

- 设计背景：`/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/0-contractdesign.md`
- 前端现有签名代码：`/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/lib/order-signature.ts`
- 前端现有下单入口：`/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/hooks/useTrading.ts`
- 后端 HTTP 入口：`/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/http/server.go`
- NATS JetStream：`/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/bus/natsjs/client.go`
- 命令协议：`/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/protocol/types.go`

---

## 一、前端

### 1.1 前端职责边界

在市场已经创建好的前提下，前端负责以下事情：

1. 加载市场交易所需上下文
2. 采集用户原始下单意图
3. 构造 `OrderIntentV1`
4. 用钱包对 `OrderIntentV1` 对应消息签名
5. 将订单请求直接提交给 Gateway

前端不负责：

- NO -> YES 的内部归一化
- matcher 内部命令格式构造
- NATS 投递
- 数据库落库
- 链上结算提交

也就是说，前端只负责生成**标准化原始意图**，而不是生成撮合引擎内部命令。

### 1.2 市场已创建后的前端前置数据

前端在用户进入市场详情页后，至少需要拿到以下数据：

| 字段 | 作用 |
| --- | --- |
| `market_pda` | `OrderIntentV1.market` |
| `market_id` | 页面路由、展示与后端查询辅助 |
| `program_id` | `OrderIntentV1.program_id` |
| `chain_id` | `OrderIntentV1.chain_id` |
| `status` | 是否允许交易 |
| `close_time` | 是否已停止接单 |
| `best_bid` / `best_ask` | 市价单滑点价计算 |
| `tick_size` | 价格步长 |
| `amount_decimals` | 份额精度，当前固定 2 |
| `price_decimals` | 价格精度，当前固定 2 |

前端钱包方案统一采用 `@solana/wallet-adapter`。

推荐组合：

- `@solana/wallet-adapter-react`
- `@solana/wallet-adapter-react-ui`
- 常用钱包适配器（Phantom / Solflare / Backpack）

推荐前端持有一个交易上下文对象：

```ts
interface TradingContext {
  chainId: number;
  programId: string;
  marketPda: string;
  marketId: string;
  closeTime: string;
  tickSize: number;       // 当前建议 0.01
  priceDecimals: number;  // 当前固定 2
  amountDecimals: number; // 当前固定 2
  bestBid?: number;
  bestAsk?: number;
}
```

### 1.3 `OrderIntentV1` 最终结构

本期最终采用的订单签名结构为：

```rust
#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct OrderIntentV1 {
    pub version: u8,
    // 协议版本号

    pub chain_id: u16,
    // 链环境标识，避免跨链重放

    pub program_id: Pubkey,
    // 绑定当前程序，避免跨程序重放

    pub market: Pubkey,
    // 订单所属市场 PDA

    pub user: Pubkey,
    // 签名用户地址

    pub side: Side,
    // 原始买 / 卖

    pub outcome: Outcome,
    // 原始 YES / NO

    pub order_type: OrderType,
    // Limit / Market

    pub limit_price: u64,
    // 价格 tick，取值 1..99
    // 含义是 $0.01 .. $0.99
    // Limit: 用户挂单价
    // Market: 前端基于盘口生成的滑点保护价

    pub total_amount: u64,
    // Limit / Market Sell: 份额 lots = shares * 100
    // Market Buy: 金额 cents = usdc * 100

    pub nonce: u64,
    // 唯一 nonce

    pub expiry_ts: i64,
    // 订单过期时间
}
```

这套结构的关键原则是：

- 记录的是**原始用户意图**
- 不记录 matcher 内部使用的归一化 side
- 不记录内部归一化后的 price tick
- 不区分“这是内部 YES book 视角还是用户原始视角”

### 1.4 字段精度规则

#### 1.4.1 价格精度

所有价格统一按两位小数处理：

- 前端展示：`0.01 ~ 0.99`
- 传输 / 签名 / 存储：转成 `1..99` 的整数 tick
- 计算或展示时再除 `100`

示例：

- `0.63` -> `63`
- `0.40` -> `40`

因此 `limit_price` 的业务语义不是浮点数，而是整数 tick。

#### 1.4.2 数量精度

份额统一按两位小数处理：

- 前端展示：`1.25`
- 传输 / 签名 / 存储：乘 `100`
- 计算或展示时再除 `100`

示例：

- `1.00 shares` -> `100`
- `2.50 shares` -> `250`

### 1.5 限价单与市价单的统一表达

本期 `OrderIntentV1` 同时支持限价单和市价单。

这里的核心思想是：

- **限价单与市价单在签名结构层不拆成两套结构**
- 市价单不是特殊消息体，而是由前端先根据盘口算出保护价，再按同一结构提交

#### 1.5.1 Limit Order

对于限价单：

- `order_type = Limit`
- `limit_price = 用户输入价格`
- `total_amount = 用户输入份额数量`

#### 1.5.2 Market Order

对于市价单：

- `order_type = Market`
- `limit_price = 前端按当前盘口生成的滑点保护价`
- `total_amount` 根据方向解释：
  - `market sell`：表示卖出份额数量
  - `market buy`：表示输入金额

这里要特别强调：

- `market buy` 的 `total_amount` 不是份额，而是金额
- `market sell` 的 `total_amount` 是份额
- 也就是说，`total_amount` 在原始意图层是“用户输入总量”，具体语义由 `order_type + side` 决定

### 1.6 市价单滑点保护价的前端计算规则

前端下市价单时，不是把订单标记为“无限价”，而是必须根据当前盘口生成一个保护价。

推荐规则：

- 前端先把 YES 订单簿的最优价换算回用户原始视角
- 再向不利方向额外放宽 `1` 个 tick
- 最终 `limit_price` 仍必须落在 `1..99`

示例：

- 当前 YES 最优 ask 为 `35`
- 用户发起 `market buy yes`
- 前端可将 `limit_price = 36`

- 当前 YES 最优 bid 为 `48`
- 用户发起 `market buy no`
- 原始 NO 视角最优 ask 为 `52`
- 前端可将 `limit_price = 53`

- 当前 YES 最优 bid 为 `48`
- 用户发起 `market sell yes`
- 前端可将 `limit_price = 47`

- 当前 YES 最优 ask 为 `63`
- 用户发起 `market sell no`
- 原始 NO 视角最优 bid 为 `37`
- 前端可将 `limit_price = 36`

如果盘口为空：

- 前端不应发起 market 单
- 应直接提示“当前无可成交对手盘”

### 1.7 Nonce 生成规则

前端在生成订单签名时，`nonce` 必须严格采用“时间戳拼接随机值”的方案，不能使用纯随机数，也不能使用自增值。

推荐规则：

- 高位：毫秒级时间戳
- 低位：安全随机数
- 最终拼成 `u64`

目的：

- 保证订单 nonce 基本有序
- 降低同毫秒内碰撞概率
- 兼顾可追踪性与唯一性

文档要求：

- 前端必须本地生成 `nonce`
- 同一笔订单重试时必须复用同一个 `nonce`
- 不允许 Gateway 替用户重写 `nonce`

### 1.8 签名消息构造

虽然 Gateway 永远不验订单签名，但前端仍然必须按统一协议生成签名，原因是：

- 协议格式要先固定
- 后续链上或其他服务可复用
- 同一个前端接口不能因为本期不验签就取消签名字段

推荐签名流程保持为：

1. `OrderIntentV1` 序列化
2. 对序列化结果做哈希
3. 对哈希的十六进制文本签名
4. 将签名和结构化字段一起提交给 Gateway

注意：

- 本期 Gateway 不做密码学验签
- 但前端依然需要保留签名字段
- 这样后续升级不会破坏协议

### 1.9 前端下单请求体

推荐 Gateway 接收的请求体如下：

```json
{
  "version": 1,
  "chain_id": 101,
  "program_id": "<program_pubkey>",
  "market": "<market_pda>",
  "user": "<wallet_pubkey>",
  "side": "buy",
  "outcome": "no",
  "order_type": "market",
  "limit_price": 63,
  "total_amount": 1000,
  "nonce": "123456789012345678",
  "expiry_ts": 0,
  "signature": "<base64>"
}
```

Header：

- `Authorization: Bearer <auth_token>`
- `Idempotency-Key: <snowflake_id>`
- `X-Trace-Id: <snowflake_id>`（必传）

### 1.10 前端本地校验规则

前端发单前应至少校验：

- 钱包已连接
- 用户已登录并可取到 token
- 市场状态是 `open`
- 当前时间未超过 `close_time`
- `version` 正确
- `program_id` 已加载
- `market_pda` 已加载
- `limit_price` 范围合法
- `total_amount > 0`
- `expiry_ts` 合法

附加规则：

- `limit order`：必须输入份额数量和挂单价
- `market buy`：必须输入金额
- `market sell`：必须输入份额数量

---

## 二、合约端

### 2.1 本期合约端职责边界

本期文档聚焦的是“前端签名 -> Gateway -> NATS”，因此合约端在本期不是执行主角。

本期合约端只需要承担两件事：

1. 作为 `program_id` 的协议锚点存在
2. 作为 `market` PDA 的协议锚点存在

本期不要求：

- 合约参与下单签名校验
- 合约参与 order normalize
- 合约参与链上撮合或链上结算提交

也就是说，本期合约端主要是为签名协议提供“被绑定的链上对象”，而不是参与这条链路的运行时逻辑。

### 2.2 `program_id` 的作用

即使 Gateway 永远不做验签，`program_id` 仍然必须保留在 `OrderIntentV1` 中。

原因：

- 防跨程序重放
- 签名协议和具体合约绑定
- 后续若需要在其他阶段验证签名，不必改结构

要求：

- 前端从配置加载真实 `program_id`
- 不允许使用全 0 占位值
- 不允许使用临时假值

### 2.3 `market` 使用 PDA 地址

`OrderIntentV1.market` 采用市场 PDA 地址，而不是业务层数字 market id。

原因：

- PDA 与链上对象一一对应
- 签名层直接绑定真实市场对象
- 避免“数字 id 正确但链上目标对象被混淆”的问题

因此前端必须拿到：

- `market_pda`
- 而不是只拿 `market_id`

### 2.4 本期不要求合约侧处理归一化

由于本期只做到 Gateway -> NATS，所以：

- NO / YES 的内部归一化是后端内部解释问题
- 不是本期合约问题

因此文档必须明确写清楚：

- 本期合约端不实现 normalize
- 本期合约端不消费下单消息
- 本期链上不接收该订单直接执行结算

这样可以避免后面阅读者误以为这篇文档已经覆盖了链上结算设计。

### 2.5 为后续阶段保留的协议兼容性

虽然本期合约不参与执行，但 `OrderIntentV1` 仍然要具备可扩展性，因此保留：

- `version`
- `chain_id`
- `program_id`
- `market`
- `user`
- `signature`

这是为了保证后续若需要链上验证或其他服务验证时，不必推翻前端签名协议。

---

## 三、后端

### 3.1 后端总体职责

本期后端的角色是：

- 接收前端订单请求
- 做协议层合法性校验
- 做身份对齐
- 将原始订单解释成内部命令
- 直接投入 NATS

后端本期不做：

- 密码学验签
- 数据库落库
- 链上结算提交

因此本期后端更准确的定位是 **Gateway + Command Publisher**。

### 3.2 钱包登录与 Auth Token

这次设计明确决定：**Gateway 永远不做密码学验签**。

理由很直接：

- 验签会带来额外 CPU 开销
- 会影响高并发接入效率
- 当前这条链路的目标是尽可能高效地把用户订单投入 NATS
- 本期 Gateway 的职责是协议校验和命令转换，而不是安全终裁

因此 Gateway 对 `signature` 的处理方式是：

- 接收该字段
- 透传该字段
- 不在 Gateway 层做 `ed25519 verify`

但这不代表 Gateway 什么都不校验。

### 3.4 Gateway 必须做的协议校验

Gateway 本期必须做的是**协议合法性校验**，而不是密码学验签。

至少包括：

#### 3.3.1 身份对齐

- 通过 `Authorization: Bearer <auth_token>` 解析当前登录会话
- `auth_token` 由钱包登录流程签发
- token 有效期固定 7 天
- 校验请求体中的 `user` 必须等于当前登录钱包地址

这一步必须保留，否则任何人都可以伪造别人的 `user` 字段投单。

#### 3.3.2 版本校验

- `version` 必须是当前支持版本
- 当前建议固定为 `1`

#### 3.3.3 链环境校验

- `chain_id` 必须等于当前环境配置
- 不允许来自其他链环境的订单进入当前 Gateway

#### 3.3.4 程序校验

- `program_id` 必须等于服务配置中的目标程序地址
- 不能接受任意程序 ID

#### 3.3.5 市场校验

- `market` 必须是已存在且可交易的市场 PDA
- 市场状态必须为 `open`
- 当前时间不得超过市场 `close_time`

#### 3.3.6 枚举与数值范围校验

- `side` 只能是 `buy` / `sell`
- `outcome` 只能是 `yes` / `no`
- `order_type` 只能是 `limit` / `market`
- `limit_price` 必须在合法 tick 范围内
- `total_amount > 0`
- `expiry_ts` 合法

#### 3.3.7 market 单额外校验

- 盘口必须存在有效对手价
- 若盘口为空，不允许 `market` 单进入 NATS

### 3.5 Gateway 不做验签，但必须做字段解释

Gateway 核心职责不是验证签名，而是把原始 `OrderIntentV1` 解释成内部命令。

这一步必须非常清楚，因为 `total_amount` 在不同单型下语义不同。

### 3.6 Gateway 内部字段转换规则

Gateway 在发布到 NATS 前，应将原始订单转换成内部字段。

#### 3.5.1 原始层字段保留

内部命令必须保留原始字段，至少包括：

- `version`
- `chain_id`
- `program_id`
- `market`
- `user`
- `side`
- `outcome`
- `order_type`
- `limit_price`
- `total_amount`
- `nonce`
- `expiry_ts`
- `signature`

#### 3.5.2 执行层字段拆分

同时，Gateway 要额外拆出执行层字段：

- `price_tick`
- `qty_lots`
- `spend_amount`

转换规则如下：

##### Limit Order

- `price_tick = limit_price`
- `qty_lots = total_amount`
- `spend_amount = 0`

##### Market Sell

- `price_tick = limit_price`
- `qty_lots = total_amount`
- `spend_amount = 0`

##### Market Buy

- `price_tick = limit_price`
- `qty_lots = 0`
- `spend_amount = total_amount`

这样做的原因是：

- 前端签名结构保持统一
- Gateway 内部执行字段保持清晰
- 后续 matcher 不需要理解 `total_amount` 的多重语义

### 3.7 Gateway 是否做归一化

本期文档里不把“链上级别的 NO/YES 归一化”作为重点，但 Gateway 可以按撮合引擎需要在内部生成标准化命令。

需要区分两个概念：

1. **原始意图解释**：本期必须做
2. **内部订单簿归一化**：若 matcher 只维护 YES book，则可在 Gateway 或 matcher 内部做

为了不超出本期讨论范围，本文只要求 Gateway 做到：

- 先把 `total_amount` 正确拆成 `qty_lots` / `spend_amount`
- 是否继续做 `NO -> YES` 的内部归一化，由 matcher 接口约定决定

如果当前 matcher 仍要求统一 YES 账本，则 Gateway 发布的命令中应补充：

- `normalized_side`
- `normalized_price_tick`

但这属于内部命令层字段，不属于 `OrderIntentV1`。

### 3.8 Gateway 到 NATS 的投递模型

本期核心链路如下：

1. 前端发单到 Gateway
2. Gateway 做协议校验
3. Gateway 做字段拆分 / 内部命令构造
4. Gateway 直接发布到 NATS JetStream
5. 返回 `202 Accepted`

因此 Gateway 的职责在 NATS 发布成功后即结束。

### 3.9 ID 生成规则与幂等

本期所有核心 ID 统一采用雪花算法生成，不使用 UUID。

适用范围：

- 登录 challenge id
- auth session id（若需要）
- command_id
- trace_id
- idempotency_key

采用雪花算法的原因：

- 具备时间有序性
- 便于日志排查
- 保证消息之间序号连贯
- 适合高并发生成

具体规则：

- `command_id`：Gateway 生成，使用雪花算法，保证每条命令唯一
- `trace_id`：必须来自请求头 `X-Trace-Id`，前端生成并传入，使用雪花算法
- `idempotency_key`：必须来自请求头 `Idempotency-Key`，前端生成并传入，使用雪花算法
- NATS `Msg-Id = idempotency_key`

幂等策略：

- 前端同一笔订单的重试必须复用同一个 `idempotency_key`
- Gateway 不重写 `idempotency_key`
- Gateway 发布到 JetStream 时，将 `Msg-Id` 设置为 `idempotency_key`
- 依赖 JetStream 的消息去重窗口实现接入层幂等

这套规则的含义是：

- `command_id` 用于标识“这次 Gateway 生成的命令”
- `trace_id` 用于串联整条请求链路
- `idempotency_key` 用于识别“这是不是同一笔前端业务请求的重复提交”

### 3.10 NATS 命令结构建议

推荐 Gateway 投递的命令 envelope 结构如下：

```json
{
  "id": "<snowflake_command_id>",
  "type": "cmd.order.place.v1",
  "schema_version": 1,
  "market_id": 2222363171854875225,
  "producer": "gateway",
  "trace_id": "<snowflake_trace_id>",
  "idempotency_key": "<snowflake_idempotency_key>",
  "created_at": "2026-03-31T10:00:00Z",
  "payload": {
    "order_id": 947395839201280,
    "market_id": 2222363171854875225,
    "market_pda": "<market_pda>",
    "wallet_address": "<wallet_pubkey>",
    "user": "<wallet_pubkey>",
    "version": 1,
    "chain_id": 101,
    "program_id": "<program_pubkey>",
    "original_action": "buy",
    "original_outcome": "no",
    "original_price_tick": 36,
    "raw_limit_price": 36,
    "raw_total_amount": 1000,
    "normalized_side": "sell",
    "normalized_price_tick": 64,
    "side": "sell",
    "outcome": "no",
    "order_type": "market",
    "price_tick": 64,
    "qty_lots": 0,
    "spend_amount": 1000,
    "nonce": 123456789012345678,
    "expiry_ts": 0,
    "expire_time": 0,
    "intent_bytes_hex": "<hex>",
    "signature": "<base64>",
    "timestamp": 1760000000
  }
}
```

字段说明：

- envelope 层：
  - `id`：Gateway 生成的雪花 `command_id`
  - `trace_id`：直接取请求头 `X-Trace-Id`
  - `idempotency_key`：直接取请求头 `Idempotency-Key`
  - `market_id`：由 Gateway 根据 `market_pda` 解析出的内部数字市场 ID
- payload 原始字段层：
  - `version / chain_id / program_id / market_pda / user / original_action / original_outcome / raw_limit_price / raw_total_amount / nonce / expiry_ts / signature / intent_bytes_hex`
  - 这部分反映前端签名时的原始意图
- payload 归一化字段层：
  - `normalized_side`
  - `normalized_price_tick`
  - 这部分是 Gateway 针对 YES 统一订单簿做出的内部解释结果
- payload 执行字段层：
  - `side`
  - `price_tick`
  - `qty_lots`
  - `spend_amount`
  - `expire_time`
  - `timestamp`
  - 这部分是后续 matcher 直接消费的字段

归一化规则示例：

- 原始单：`buy no @ 36¢`
- 因为内部只维护 YES 订单簿，所以 Gateway 转换为：
  - `normalized_side = sell`
  - `normalized_price_tick = 64`
- 同时为了兼容当前 matcher，继续下沉为：
  - `side = sell`
  - `price_tick = 64`

也就是说，**一条 NATS 命令同时保留原始语义、归一化语义和 matcher 执行语义**。这条消息已经足够让后续 matcher 直接消费。

### 3.11 NATS 配置建议

继续沿用 JetStream 两条 stream：

- `AP_CMD`：`cmd.>`
- `AP_EVT`：`evt.>`

本期这条链路真正使用的是：

- `cmd.order.place`

如果撤单也走同一路径，则保留：

- `cmd.order.cancel`

### 3.12 Gateway 返回值

Gateway 对前端返回：

- `202 Accepted`
- `command_id`
- `idempotency_key`
- `market`
- `message`

示例：

```json
{
  "message": "order command accepted",
  "command_id": "947395839201281",
  "market": "<market_pda>",
  "idempotency_key": "947395839201282"
}
```

需要明确：

- 这不代表已成交
- 这只代表命令已进入 NATS

### 3.13 并发与性能设计重点

由于 Gateway 永远不做订单验签，本期性能优化重点如下：

- 不做 `ed25519 verify`
- 尽量避免 CPU 重操作
- 只做必要协议校验
- 直接投递 NATS
- 用 `Idempotency-Key` 做幂等保护
- 让 NATS 承接削峰与异步消费

这也是本期架构核心收益之一。

---

## 结论

本期最终设计可以收敛为以下原则：

### 1. 签名层

- 使用 `OrderIntentV1`
- 记录原始用户意图
- 市场使用 `market PDA`
- 限价单和市价单共用同一结构
- `order_type` 必须显式存在
- 价格使用 `1..99` tick
- 份额与金额仍使用 `*100` 的整数

### 2. 市价单表达

- 市价单通过前端计算滑点保护价来表达
- `limit_price` 对市价单来说是保护边界价
- `market buy` 的 `total_amount` 记录为输入金额
- `market sell` 的 `total_amount` 记录为卖出份额

### 3. Gateway

- 永远不做密码学验签
- 只做协议合法性校验和身份对齐
- 将原始 `total_amount` 拆成内部 `qty_lots / spend_amount`
- 直接投递 NATS

### 4. NATS

- 本期是主写路径
- Gateway 成功发布后即返回 `202 Accepted`
- 后续 matcher 再异步处理
