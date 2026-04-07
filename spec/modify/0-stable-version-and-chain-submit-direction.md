# 链上签名与提交提 TPS 核心方案

## 0. 范围与约束

本文只讨论一条主链路：

1. 用户签名什么
2. 后端在 `settlement` 阶段如何组装交易
3. 合约如何验证签名并执行批量结算
4. 单市场如何把链上提交频率尽量压到接近 `processed` 级别

本文件明确采用以下约束，不再反复摇摆：

- 不做 `market readonly`
- 不做 `1 tx = 多个 match_event`
- 不做 Jito bundle
- 仍然保持 `1 match_event_id = 1 链上提交单元`
- `market` 账户继续是 `mut`，同一市场链上结算严格串行
- 允许为提吞吐把市场 lane 的推进门槛从 `confirmed` 前移到 `processed`
- 用户侧仍然是钱包签链下消息，relayer 负责真正发链上交易

结论先写在前面：

- 当前系统单市场 TPS 上不去，核心不是 matcher，而是 `settlement` 这条链路的字节体积、账户体积、RPC 往返和 `confirmed` 门槛。
- 要提 TPS，最核心的不是继续调 matcher，而是把“每个 fill 需要带上链的证明材料”彻底压缩。
- 当前最该做的是把“用户签名载荷”和“链上执行见证”拆开，并把 `OrderState` 变成真正可复用的热路径状态。

## 1. 当前实现为什么慢

### 1.1 当前交易体积膨胀点

当前热路径见：

- `Banckend/internal/settlement/intent.go`
- `Banckend/internal/settlement/submission.go`
- `Contract/programs/predix-program/src/lib.rs`
- `Contract/programs/predix-program/src/state/order.rs`
- `Contract/programs/predix-program/src/state/settlement.rs`

当前一笔结算交易的膨胀主要来自 5 个点。

#### A. 每个订单都把完整 `OrderIntentV1` 带上链

当前 `OrderIntentV1` 固定 `132` 字节：

- `version`: 1
- `program_id`: 32
- `market`: 32
- `user`: 32
- `nonce`: 8
- `side/outcome/order_type`: 3
- `limit_price`: 8
- `total_amount`: 8
- `expiry_ts`: 8

这意味着只要一个 batch 里有 `N` 个订单，光 `orders` 参数就先吃掉 `132 * N` 字节。

#### B. 每个新订单都要附一条 `ed25519` 指令

当前每个订单都走：

- `buildEd25519Instruction(...)`
- 消息体是 `hex(keccak256(intent))`

也就是每单一条单独的 ed25519 verify instruction，并且消息正文还是 `64` 字节 ASCII hex。

单条 ed25519 instruction 的 data 大约是：

- header/offset: 16
- signature: 64
- pubkey: 32
- message: 64

合计约 `176` 字节，还没算 instruction 自身 envelope。

#### C. `fill_price` 现在仍然按 `u64` 传

当前 `FillIndexPair` 是：

- `maker_idx u16`
- `taker_idx u16`
- `fill_amount u64`
- `fill_price u64`

但 `fill_price` 的取值域本来就是 `1..99`，这里直接浪费 7 个字节。

#### D. 缺失 `user_position` 时，当前是额外插一条初始化指令

当前 `BuildInstructions(...)` 会：

1. 先插 N 条 ed25519
2. 再插若干条 `init_user_position`
3. 最后才插 `settle_match_batch`

也就是冷路径不是“一个重一点的 settlement 指令”，而是“多条 instruction 拼接”，这对单 tx 可容纳 fills 数非常伤。

#### E. 每个 batch 构造前还会做一次链上 existence RPC

当前 `BuildUserPositionInitPlan(...)` 还在跑：

- `GetMultipleAccounts`

这会增加：

- 一个额外 RPC 往返
- 一个额外等待点
- 批次越多越容易 miss slot

### 1.2 当前确认门槛太高

当前 `settlement` 的 lane 释放基本等价于等 `confirmed`，代码在：

- `Banckend/internal/settlement/service.go`
- `Banckend/internal/chainconfirm/router.go`

这会把单市场节奏从“接近 slot cadence”拖成“接近多 slot cadence”。

只要同一市场继续串行，这个确认门槛就直接决定单市场上限。

### 1.3 现在真正的瓶颈不是 matcher

matcher 只是把订单撮成 `match_event`。

真正限制单市场 TPS 的，是下面这条链：

1. 从 `match_event` 恢复全量 intent
2. 判定 `user_position` 是否存在
3. 构造一堆 ed25519 + init 指令
4. 拿 blockhash
5. 签 relayer tx
6. 发链
7. 等 `confirmed`
8. 下一批才能继续

所以优化重点必须放在签名协议、合约入参、交易模板和确认策略，不是继续在 matcher 里微调。

## 2. 收口后的核心思路

### 2.1 把“用户签名载荷”和“链上执行载荷”拆成两层

这是整个提 TPS 的核心。

当前的问题是：用户签了什么，链上就把那份大对象再次完整带上去。

正确做法应该是两层：

#### 第一层：用户签名载荷

用户签的是一个稳定、可重建、面向安全语义的订单摘要，不是未来每次结算都要原样塞进 tx 的大对象。

#### 第二层：链上执行载荷

链上只带“本次执行真正需要的最小见证”。

这层见证必须满足：

- 能在第一次触链时初始化 `OrderState`
- 能在后续触链时完全不再重复带完整订单
- 冷热误判只影响体积，不影响正确性

## 3. 新的签名协议

### 3.1 用户实际签名内容

建议引入新的签名域 `OrderSignV2`，逻辑上包含：

- 固定 domain tag
- 固定 program id
- `market`
- `user`
- `nonce`
- `side`
- `outcome`
- `order_type`
- `limit_price`
- `total_amount`
- `expiry_ts`

注意：

- `program_id` 不再作为动态字段从前端传来，而是协议常量参与 hash
- `market` 与 `user` 仍然参与签名
- `nonce` 暂时继续保留 `u64`
  - 当前链上已经用 `nonce >> 22` 提取时间语义，服务于 `cancel_all_before_ts`
  - 在这套语义没重构前，不接受 `u64 -> u32`
- `limit_price` 改为 `u8`
- `total_amount` 继续保留 `u64`
  - 对限价单/市价卖出，它表示总份额
  - 对市价买入，它表示总 spend amount
- `expiry_ts` 改为 `u32`
  - 当前业务是 Unix 秒级时间，`u32` 到 2106 年足够

建议的 canonical bytes：

```text
"predix-order-v2"           // 固定 domain
+ program_id(32)
+ market_pubkey(32)
+ user_pubkey(32)
+ nonce_le(8)
+ flags_u8(1)              // bit0 side, bit1 outcome, bit2 order_type
+ limit_price_u8(1)
+ total_amount_le(8)
+ expiry_ts_le(4)
```

### 3.2 钱包实际签名的 message

不建议继续签 `64` 字节 hex。

建议签：

```text
"bp1:" + base64url_no_pad(keccak256(canonical_bytes))
```

原因：

- 仍然是 UTF-8 文本，兼容当前钱包签消息习惯
- 比 `64` 字节 hex 更短
- 链上可重建
- HTTP 层可重建

这里的目标不是省 200 字节，而是把“每个冷订单的 ed25519 指令消息”从 `64` 字节继续压短一截。

### 3.3 后端落库保存什么

这层先不展开完整 schema 改造，但核心语义必须固定下来：

- `orders.signature` 继续保存用户签名
- `orders.intent_hex` 不再代表旧的 `OrderIntentV1` 大对象，而是保存 `OrderSignV2` 的 canonical payload bytes
- settlement 构造交易时，不需要把 `intent_hex` 整体原封不动带上链

也就是说：

- 数据库存的是“可重建签名语义的原始材料”
- 链上带的是“最小执行见证”

## 4. 合约侧要怎么改

### 4.1 不再把完整订单数组作为 `settle_match_batch` 的输入

建议直接废弃当前语义，改成新的 `settle_match_batch_v2`。

新的 args 形状建议如下：

```rust
pub struct SettleMatchBatchV2Args {
    pub user_count: u8,
    pub orders: Vec<OrderSlotV2>,
    pub cold_witnesses: Vec<ColdOrderWitnessV2>,
    pub fills: Vec<CompactFillV2>,
}

pub struct OrderSlotV2 {
    pub user_idx: u8,
    pub cold_witness_idx: u8, // 255 表示 warm order
}

pub struct ColdOrderWitnessV2 {
    pub nonce: u64,
    pub total_amount: u64,
    pub expiry_ts: u32,
    pub limit_price: u8,
    pub flags: u8, // side/outcome/order_type
}

pub struct CompactFillV2 {
    pub maker_idx: u8,
    pub taker_idx: u8,
    pub fill_amount: u32,
    pub fill_price: u8,
}
```

字段收口说明：

- `ColdOrderWitnessV2`
  - 当前先收口到 `22 bytes`
  - 即：`nonce u64 + total_amount u64 + expiry_ts u32 + limit_price u8 + flags u8`
  - 不继续把 `nonce` 降到 `u32`
- `CompactFillV2`
  - 当前收口到 `7 bytes`
  - 即：`maker_idx u8 + taker_idx u8 + fill_amount u32 + fill_price u8`
  - 接受 `fill_amount u32` 的前提是：当前系统里 `fill_amount` 永远表示 `shares * 100`
  - 也就是两位小数份额，单次 fill 上限约 `42,949,672.95 shares`

### 4.2 为什么这套结构成立

因为对链上结算来说，真正需要的不是“完整订单对象”，而是以下几类信息：

- 这个 order 属于哪个用户
- 这个 order 的不可变语义是什么
  - side
  - outcome
  - order_type
  - limit_price
  - total_amount
  - expiry_ts
  - nonce
- 这个 order 当前已成交到哪了
- 这个 fill 的对手方是谁、数量多少、价格多少

其中：

- “不可变语义”
  - 第一次触链时从 `ColdOrderWitnessV2` 写进 `OrderState`
- “当前已成交到哪了”
  - 以后都从 `OrderState` 读取
- “这个 fill 的增量”
  - 由 `CompactFillV2` 提供

所以完整 `OrderIntentV1` 没必要在每笔 tx 里反复出现。

### 4.3 新的 `OrderState` 必须承载什么

当前 `OrderState` 不够热路径化，因为它仍然依赖完整 `OrderIntentV1` 去做很多验证。

建议把 `OrderState` 收口成真正可复用的热状态：

```rust
pub struct OrderStateV2 {
    pub owner: Pubkey,
    pub nonce: u64,
    pub total_amount: u64,
    pub filled_amount: u64,
    pub paid_cash: u64,
    pub expiry_ts: u32,
    pub limit_price: u8,
    pub flags: u8,          // side/outcome/order_type/canceled
    pub cash_remainder: u8,
    pub bump: u8,
    pub _reserved: [u8; 6],
}
```

关键点：

- 以后校验 fill 价格，不再依赖外部 `OrderIntentV1`
- 以后判断 `market buy` 的现金累计规则，不再依赖外部 `OrderIntentV1`
- 以后 branch 分类，也不再依赖外部 `OrderIntentV1`
- 不再存 `digest / order_hash`
  - 不是因为 PDA 足够，而是因为 `OrderStateV2` 自己已经持有全部不可变订单语义
- 不再存 `paid_creator_fee / paid_platform_fee`
  - fee delta 由 `old_paid_cash -> new_paid_cash` 结合 market fee bps 现场重算
- 第一次初始化后，后续所有链上结算都只依赖 `OrderStateV2`

### 4.4 冷订单怎么初始化

当某个 `order_state` 账户还不存在时：

1. 根据 `OrderSlotV2.user_idx` 找到对应用户
2. 读取 `ColdOrderWitnessV2`
3. 用：
   - 固定 domain
   - 固定 program id
   - 当前 market
   - 当前 user
   - witness 中的 nonce / flags / limit / total / expiry
   重建 canonical bytes
4. 重建 expected message
5. 在 instruction sysvar 中查找匹配的 ed25519 instruction
6. 验过以后创建并初始化 `OrderStateV2`
7. 把 `flags / limit_price / total_amount / expiry_ts / nonce` 直接写入 state

当 `order_state` 已存在时：

- 直接读取 `OrderStateV2`
- 不再需要完整 witness
- 即使 tx 里保守地多带了一份 cold witness，也不会影响正确性

这点很重要：

> 冷热判断允许偏保守，最坏只是 tx 变胖，不会打坏业务语义。

这会极大降低 settlement 对外部 registry 同步精确度的依赖。

## 5. `user_position` 的处理方式

### 5.1 不再单独插 `init_user_position` 指令

建议改为：

- `settle_match_batch_v2` 内手动 `load_or_init_user_position`
- 不使用 Anchor 的 `init_if_needed`
- 也不保留独立的 `init_user_position` 热路径依赖

也就是说：

- 热路径：position 已存在，直接读写
- 冷路径：position 不存在，就在同一条 settlement 指令里创建

### 5.2 为什么这样更适合高 TPS

因为当前最伤的不是“初始化这件事本身”，而是“初始化被拆成额外 instruction”。

把它 inline 到 settlement 指令内部以后：

- 少了 instruction 个数
- 少了 instruction envelope
- 少了 builder 层复杂度
- 少了“先建 position 再结算”的额外依赖

### 5.3 热冷定义要重新固定

这里的 HOT/COLD 不要再只看 fill 数。

建议统一成：

- HOT batch
  - 所有参与钱包的 `user_position` 都已存在
- COLD batch
  - 至少有一个参与钱包的 `user_position` 缺失

然后额外再引入一个独立维度：

- `new_order_state_count`

因为 `order_state` 是否首触链，会直接影响 witness / ed25519 条数，从而影响 tx 体积。

但这里要收口一个边界：

- `estimator` 只负责估算完整 `raw tx bytes`
- 不负责估算 `CU`
- 也不负责判断是否需要更保守的 cold-path 限流

所以最终 settlement / matcher 共用的 byte estimator 真正看的输入是：

- `fill_count`
- `order_count`
- `unique_wallet_count`
- `new_order_state_count`
- 以及 Solana 提交本身的固定开销：
  - payer signature
  - recent blockhash
  - instruction envelope
  - v0 / ALT lookup bytes

`user_position` 是否存在，当前不进入 estimator。

原因是：

- 现在 `user_position` 已经 inline init 到 settlement 指令内部
- 它主要影响的是执行期 `CU`
- 对提交字节体积几乎不产生结构性差异

如果后面要对 cold path 做更保守的节流，那应该走独立策略或 env 档位，而不是把 CU 估算混进 byte estimator。

### 5.4 `user_position / order_state` 内存 registry 仍然需要

这部分不是不要了，而是角色变了。

需要继续维护两套内存 registry：

- `user_position registry`
- `order_state registry`

但它们的定位不再是“没有它就无法正确执行”，而是“builder 的容量优化与热冷判定输入”。

#### `user_position registry`

职责：

- 启动时从 `user_position_accounts` 恢复
- 运行时作为 `HOT/COLD` 判定输入
- 估算 `new_user_position_count`
- 决定批次更接近 HOT 模板还是 COLD 模板

它对正确性的影响已经下降，因为：

- 即使 registry 漏了某个账户
- settlement 里也能 inline init
- 最坏只是把一个本可 HOT 的批次按 COLD 处理，tx 更胖、fills 更少

所以：

- `user_position registry` 仍然需要
- 但它更像性能优化缓存，不再是正确性硬前提

#### `order_state registry`

职责：

- 启动时恢复“哪些订单已经首触链成功”
- 估算 `new_order_state_count`
- 决定哪些订单必须携带 `cold witness + ed25519`
- 决定单 tx 可容纳 fill 数

它比 `user_position registry` 更重要，因为：

- 如果某个 `order_state` 实际不存在
- builder 却错误地把它当 warm order，并省掉 witness
- 那链上就无法初始化这个 order state，交易会失败

所以这里的原则是：

- `order_state registry` 必须做成单调真相来源
- unknown 一律按 cold 处理
- 宁可多带 witness，也不要错误省略 witness

结论：

- 这两个内存合集仍然要做
- 只是现在不再把它们设计成“链上正确性的唯一兜底”
- 它们主要服务于：容量估算、HOT/COLD 分流、减少不必要的 witness 与重 RPC

## 6. 新的交易模板

### 6.1 建议只保留两套外部配置模板

外部 env 先保持两套：

- HOT
- COLD

但内部估算时，必须把 `new_order_state_count` 算进去。

理由：

- `user_position` 是最直观的冷热分界
- `order_state` 首次落链是高频事件，必须影响单 tx 容量估算
- 但没必要把 env 扩成 4 套，把复杂度炸开

### 6.2 HOT tx 形状

HOT tx 目标是：

- 不做链上 existence RPC
- 不带 `init_user_position` instruction
- 能不带 cold witness 就不带
- 只放：
  - compute budget
  - settlement 指令

如果某些订单是第一次触链，允许带少量 cold witness 和对应 ed25519。

### 6.3 COLD tx 形状

COLD tx 允许：

- 同一条 settlement 指令里 inline 创建缺失的 `user_position`
- 带更多 cold witness
- fill 上限更低
- compute unit limit 更高

也就是说：

- HOT/COLD 的差异，不是“是不是两套完全不同业务逻辑”
- 而是“同一语义，不同容量模板”

## 7. v0 / ALT 的位置

### 7.1 v0 该做

v0 transaction 本身应该切。

因为后面无论是否上 ALT，统一走 v0 都是对的。

### 7.2 ALT 不要被神化

在我们当前约束下，ALT 不是第一优先级，也不应被写成救世主。

原因很简单：

- `market`
- `config`
- `system`
- `instruction_sysvar`

这些静态账户适合 ALT。

但真正大的动态账户集合是：

- user ledger
- user position
- order state

其中尤其是：

- 新订单的 `order_state`
- 新用户的 `user_position`

这些地址天然是动态的，不可能都提前塞进 ALT。

所以真实结论是：

- `v0` 要做
- `ALT` 先做静态部分
- 不要把单市场 TPS 的核心提升寄希望于 ALT

真正提升更大的，还是：

- witness 压缩
- 不再重复传完整订单
- inline init
- processed gate

## 8. blockhash 与发送链路

### 8.1 为什么 `GetLatestBlockhash` 仍然必须存在

Solana 每笔交易都必须带 recent blockhash。

这不是 SDK 自动替你在网络层补上的字段，而是你构造交易消息时就必须放进去的内容。

所以：

- 前端直接发链上交易，需要自己拿 blockhash
- 后端 relayer 发链上交易，也一样要自己拿

### 8.2 但不能继续“每批都现查一次”

建议做一个 `blockhash cache manager`，规则如下：

- 空闲时不轮询，不消耗 RPC
- 只有当：
  - 有 queued settlement
  - 或有 submitted settlement 需要重签
  才进入 active 模式
- active 模式下缓存：
  - `blockhash`
  - `last_valid_block_height`
  - `fetched_at`
  - `slot`
- 当缓存过旧或接近失效时再刷新

建议的初始策略：

- idle: 不请求
- active:
  - 最近一次获取时间超过 `250ms` 才刷新
  - 或当前高度接近 `last_valid_block_height - safety_margin`

这样可以把：

- 每批一次 `GetLatestBlockhash`

改成：

- 多个批次复用同一个活跃 blockhash 窗口

### 8.3 当前环境下的发送策略

在“devnet + 当前免费专用 RPC + 不上 Jito”的前提下，发送链路先收口为：

1. settlement 只负责构造 raw tx
2. 直接 `SendEncodedTransactionWithOpts`
3. 保留 rebroadcast / resign
4. 不做多 RPC 扇出

这里的关键优化不是“多打几个 endpoint”，而是：

- tx 更小
- miss slot 更少
- lane 更早推进

## 9. 市场 lane 的推进门槛

### 9.1 不能再拿 `confirmed` 当单市场节拍器

如果同一市场还是：

- 一个 tx 发出去
- 非要等 `confirmed`
- 才发下一个

那单市场节奏一定偏慢。

### 9.2 建议拆成两道门

#### 门 1：lane 推进门

建议从 `confirmed` 前移到 `processed`。

用途：

- 只是决定“同一市场下一批可不可以继续发”

#### 门 2：业务终态门

继续保留 `confirmed`。

用途：

- 发布 `evt.settlement.confirmed`
- 驱动 funds / writer 的真正终态投影

### 9.3 这样做的含义

这意味着同一市场的链上提交流程变成：

1. tx1 发出
2. 一旦 tx1 进入 `processed`，lane 允许继续发 tx2
3. tx1 之后达到 `confirmed`，再发终态事件

这样才能把单市场节奏从“等多 slot”压到“接近 slot”。

### 9.4 风险怎么兜

`processed` 不是终态，这点必须正视。

但在我们当前约束下，它仍然是正确方向，因为：

- 同一市场本来串行
- 后续 tx 会基于前序 tx 修改后的账户状态执行
- 如果前序 tx 最终没有站稳，后序 tx 大概率也会在链上失败或无法确认
- settlement 已经有 submitted / failed / rebroadcast / resign 这套状态机

所以：

- `processed` 用来提吞吐
- `confirmed` 用来定最终账

这是必须拆开的两层语义。

## 10. 新旧数据体积对比

以下只看最关键的数据体积，不把所有 tx envelope 都算进去。

### 10.1 当前方案

假设一个 batch：

- 4 个订单
- 2 个 fill
- 4 个订单都是第一次触链

仅核心数据大致就是：

- `orders`: `4 * 132 = 528`
- `fills`: `2 * 20 = 40`
- `ed25519`: `4 * 176 = 704`

总计约 `1272` 字节。

这还没算：

- instruction envelope
- account metas
- 可能存在的 `init_user_position`
- compute budget 指令

也就是说，这种规模已经非常容易超 `1232` 限制。

### 10.2 新方案

同样假设：

- 4 个订单
- 2 个 fill
- 4 个订单第一次触链

核心数据大致变成：

- `orders`: `4 * 2 = 8`
- `cold_witnesses`: `4 * 22 = 88`
- `fills`: `2 * 7 = 14`
- `ed25519`: `4 * (16 + 64 + 32 + 47) ≈ 636`

总计约 `746` 字节量级。

而且：

- 不再额外插 `init_user_position`
- 热路径下 warm order 完全不需要再带 witness

这就是为什么核心必须从“完整 intent 上链”改成“冷 witness + 热 state”。

## 11. 对 TPS 的实际意义

### 11.1 单市场的真实上限仍然受串行限制

因为我们明确不做：

- `market readonly`
- 同市场并行多 tx
- 一笔 tx 吃多个 `match_event`

所以同一市场的上限，本质上还是：

- 每秒能推进多少笔 settlement tx
  乘以
- 每笔 tx 能吃多少 fill

### 11.2 这次改造真正能抬的，是第二项和部分第一项

能直接提升的是：

- 每笔 tx 可容纳 fill 数
- 每笔 tx 构造与发送耗时
- lane 从 `confirmed` 改到 `processed` 后的推进速度

### 11.3 在当前约束下的保守目标

在“devnet + 当前免费专用 RPC + 不上 Jito + 不改 market readonly”的前提下，这份方案更合理的目标不是喊极限数字，而是先把系统收口到下面这个量级：

- HOT batch:
  - 单 tx 可容纳 fill 数显著高于现在
  - 目标先做到 `8-12 fills / tx`
- COLD batch:
  - 目标先做到 `3-5 fills / tx`
- 单市场 lane:
  - 以 `processed` 推进后，目标逼近 `0.4-0.8s / tx` 的节奏，而不是卡在 `confirmed` 的多 slot 等待

换成吞吐量语言，先追求：

- HOT 市场单市场稳定 `16-30 fills/s` 量级
- COLD 市场单市场稳定 `4-12 fills/s` 量级

这不是最终极限，只是当前约束下可落地、可验证、可逐步逼近的目标。

真正的决定因素将是：

- 动态账户数
- 实际 CU
- 实际 fill 分布
- 当前 RPC 对 `processed` 可见性的质量

## 12. 这次方案里，哪些是核心，哪些是依赖

### 12.1 核心，必须先定

下面这些就是本次必须优先拍板的主协议：

1. 用户签名协议改成 `OrderSignV2`
2. 合约入口改成 `settle_match_batch_v2`
3. `OrderState` 改成真正的热路径状态
4. `user_position` 改成 inline init
5. lane 推进门从 `confirmed` 拆成 `processed` + `confirmed`

### 12.2 依赖，但不是本文件主角

这些后续要跟上，但不影响本文件先把核心定下来：

1. `order_state registry` 如何持久化
2. `user_position registry` 如何恢复
3. matcher / settlement 共用 estimator 如何接线
4. schema 如何补充 registry 字段
5. ALT 是否进一步扩到热钱包集合

也就是说，依赖项后面再落，但核心协议现在就要固定。

## 13. 建议实施顺序

### Phase 1

先改协议和合约，不碰外围恢复：

1. 定义 `OrderSignV2`
2. HTTP 下单改为校验 `OrderSignV2`
3. 新建 `settle_match_batch_v2`
4. 新建 `OrderStateV2`
5. `fill_price` / `limit_price` 全链路改 `u8`
6. `expiry_ts` 全链路改 `u32`
7. `fill_amount` 全链路改 `u32`

### Phase 2

把 settlement builder 切过去：

1. 新的 `SubmissionBatchV2`
2. 新的 compact witness 编码
3. 新的 HOT/COLD builder
4. 去掉 runtime `GetMultipleAccounts` 依赖

### Phase 3

把发送节奏提起来：

1. blockhash cache manager
2. processed lane gate
3. confirmed terminal gate
4. 继续保留 rebroadcast / resign / reconcile

### Phase 4

最后再补外围依赖：

1. `order_state registry`
2. `user_position registry`
3. matcher / settlement unified estimator
4. v0 + 静态 ALT

## 14. 最终结论

围绕提高 TPS，这个项目现在最该定下来的主线只有一句话：

> 不再让完整订单对象反复上链，而是把“签名语义”压进第一次触链的冷 witness，把“后续执行语义”沉淀进可复用的 `OrderState`。

对应到工程上，就是 5 件事：

1. 新签名协议 `OrderSignV2`
2. 新合约入口 `settle_match_batch_v2`
3. `OrderStateV2` 承载全部热路径校验字段
4. `user_position` inline init
5. 单市场 lane 从 `confirmed` 推进改成 `processed` 推进

只要这 5 件事不落，其他外围怎么修，单市场 TPS 都上不去。

只要这 5 件事落稳了，后面的 registry、恢复、冷热估算、ALT 都是在这个主协议上继续加速，而不是再推翻重来。
