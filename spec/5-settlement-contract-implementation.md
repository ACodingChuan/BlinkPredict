# 5. settlement 合约实现与 UserPosition 存在性设计

## 0. 文档目的

本文是在 `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/0-contractdesign.md` 与 `/Users/guohaochuan/Documents/web3project/BlinkPredict/spec/3-matcher-shared-wallet-batching-redesign.md` 基础上的落地补充，专门回答本阶段真正要开发的两部分：

- **合约侧**：`settle_match_batch`、`init_user_position`、热路径 zero-copy 读写、以及各类 close 指令的实现约束
- **后端 settlement 侧**：如何在不污染链上热路径的前提下，为同一笔 tx 按需前置拼接 `init_user_position`

本文不覆盖：

- 开奖
- 撤单
- claim
- 其他与本阶段无关的非核心链上指令

这些能力仍以 0 号文档为总设计基线，但不在本阶段开发范围内。

---

## 1. 本阶段总原则

### 1.1 与 0 号文档保持一致的原则

以下约束继续完全成立：

- `settle_match_batch` 内部**绝不**使用 `init_if_needed`
- `settle_match_batch` 仍然是唯一的批量落账入口
- 热路径账户 `UserLedger`、`UserPosition`、`OrderState` 继续按 0 号文档要求，采用 **zero-copy / 原始字节偏移读写**
- close 回收路径必须补齐，避免长期租金沉淀

### 1.2 相对 0 号文档的唯一关键变化

0 号文档原始设定为：

- 用户第一次交易某个市场前，由用户自己主动调用 `init_user_position`

本阶段调整为：

- 用户仍然只签链下 `OrderIntentV1`
- `UserPosition` 仍然不允许在 `settle_match_batch` 内动态初始化
- 但当 settlement 后端准备提交某个 submission tx 时，如果发现某些 `(market, user)` 的 `UserPosition` 尚未存在，则在**同一个 tx** 中，把 `init_user_position` 作为前置指令拼进去
- `init_user_position` 的租金支付者改为 `relayer/admin`

因此，本阶段不是放弃 0 号文档的“结算热路径不开户”原则，而是把开户动作从：

- 用户显式预初始化

改成：

- relayer 在同一笔 settlement tx 中按需前置初始化

### 1.3 本阶段继续沿用的精度模型

合约和 settlement 后端本阶段统一遵守：

- `limit_price`
  - 原始订单价格 tick，范围 `1..99`
- `fill_price`
  - 成交价 tick，范围 `1..99`
- `total_amount`
  - `limit` / `market sell` 时是 `shares * 100`
  - `market buy` 时是 `usdc * 100`
- `UserPosition.yes_shares / no_shares`
  - 都按 `shares * 100` 存储
- `cost` / `refund` / `fee`
  - 继续统一向上取整

---

## 2. 为什么采用“同一 tx 指令拼接”

推荐的 submission tx 结构如下：

1. 可选：`ComputeBudget` 指令
2. 可选：`Ed25519` 验签指令
3. 0~N 条：`init_user_position`
4. 1 条：`settle_match_batch`

这样做的原因：

- **不污染热路径**：`settle_match_batch` 内部不做开户判断，不引入额外分支和 CU 波动
- **经济模型闭环**：只有真正进入成交结算的用户才会触发开户，relayer 可以用成交手续费覆盖租金，不会在“下单后又撤单”的路径上被白嫖 SOL
- **用户无感**：用户仍然只需签链下订单；链上开户与批量结算完全由 relayer 组合完成
- **更符合职责边界**：按照 3 号文档，submission batch 的组织、账户准备、链上交易构造，本来就应属于 settlement 职责

---

## 3. 本阶段开发难点的优先级

本阶段真正的难点，优先级如下：

### 3.1 第一优先级：合约热路径实现

重点不是后端缓存本身，而是合约端：

- `settle_match_batch` 需要处理大量热账户
- `UserLedger`、`UserPosition`、`OrderState` 都在高频更新路径上
- 必须避免 Anchor 常规 `Account<'info, T>` 自动反序列化带来的开销
- 必须自己处理 zero-copy / 原始字节偏移读写、安全校验、账户布局和写回

换句话说，本阶段最难的是：

- **如何把 0 号文档里的业务语义，真的翻译成可维护、可验证、可扩展的 zero-copy 链上实现**

### 3.2 第二优先级：close 回收路径补齐

除热路径外，本阶段合约还需要补齐数据账户 close 逻辑，至少包括：

- `close_empty_order_state`
- `close_empty_user_position`
- 与本阶段实现范围相关的必要 close 事件 / 约束检查

close 不是附属功能，而是和数据结构设计强耦合：

- 账户何时可以 safely 回收
- 回收给谁
- 是否要求账户已经归零
- 是否要求业务生命周期彻底结束

这些都必须在实现时说死。

### 3.3 第三优先级：settlement 后端装配细节

后端也不轻松，但和合约相比，核心难点更偏“工程组织”，主要是：

- 从 matcher batch 恢复链上 submission 需要的数据
- 组织 `remaining_accounts`
- 构造前置 `Ed25519`
- 决定哪些用户需要前置 `init_user_position`
- 按链上限制切分 submission batch

但需明确：

- “tx 成功后把新建 `UserPosition` 写内存并落库” 这部分最终由 **webhook** 负责
- settlement 提交器本身**不负责**最终确认后的持久化落库

---

## 4. 合约侧设计定稿

## 4.1 `init_user_position` 设计

### 4.1.1 目标

`init_user_position` 继续保持为一个极简指令，只负责：

- 为某个 `(user, market)` 创建 `UserPosition` PDA
- 初始化空持仓
- 不做任何结算逻辑

### 4.1.2 支付者变更

本阶段必须把支付者从 user 改为 relayer/admin：

- 原方案：`payer = user`
- 新方案：`payer = relayer` 或受控 admin

### 4.1.3 推荐账户模型

推荐形态：

```rust
#[derive(Accounts)]
pub struct InitUserPosition<'info> {
    #[account(mut)]
    pub payer: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    /// CHECK: only used as PDA seed / owner field
    pub user: UncheckedAccount<'info>,

    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,

    #[account(
        init,
        payer = payer,
        space = 8 + UserPosition::INIT_SPACE,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump
    )]
    pub user_position: AccountLoader<'info, UserPosition>,

    pub system_program: Program<'info, System>,
}
```

### 4.1.4 权限要求

为避免任意第三方代付乱开户，指令内必须校验：

- `payer.key() == config.relayer_signer`

如后续需要多 relayer，可再扩展为 allowlist；本阶段先按唯一 relayer 处理。

### 4.1.5 初始化内容

`UserPosition` 初始化时只写：

- `owner = user`
- `market = market.key()`
- `yes_shares = 0`
- `no_shares = 0`
- `claimed_yes_shares = 0`
- `claimed_no_shares = 0`
- `bump`

除此之外不做任何业务逻辑。

---

## 4.2 `settle_match_batch` 设计

### 4.2.1 保持纯结算指令

`settle_match_batch` 必须继续保持“纯结算”属性：

- 不做 `init_if_needed`
- 不尝试创建 `UserPosition`
- 不尝试创建 `OrderState` 之外的其它热路径账户
- 假设 submission tx 中需要的 `UserPosition` 在进入该指令前已经准备完毕

### 4.2.2 remaining_accounts 组织原则不变

继续沿用 0 号文档：

1. `[全表 UserLedger]`
2. `[全表 UserPosition]`
3. `[全表 OrderState]`

通过固定偏移 O(1) 索引，不在链上做哈希表或账户搜索。

### 4.2.3 `OrderState` 仍可按需初始化

本阶段要特别区分：

- `UserPosition`：禁止在 `settle_match_batch` 内动态创建
- `OrderState`：仍然允许按 0 号文档的语义，在订单首次真正上链时初始化

原因是两者的角色不同：

- `UserPosition` 是用户-市场维度热账户，若在结算路径中混入开户分支，会显著增加复杂度与 CU 波动
- `OrderState` 本来就是订单首次上链即创建的微观防重放载体

因此：

- 本阶段对 `UserPosition` 和 `OrderState` 的处理策略**不能混为一谈**

---

## 4.3 zero-copy 实现要求

### 4.3.1 适用对象

至少以下热路径账户按 zero-copy 方案实现：

- `UserLedger`
- `UserPosition`
- `OrderState`

其中：

- `UserPosition`
- `OrderState`

在 0 号文档中已经明确是 zero-copy

而本阶段实现时，`UserLedger` 也应按同一类热路径思路统一处理，避免结算主路径上反复走 Anchor 常规账户反序列化。

### 4.3.2 实现目标

需要同时满足：

- 账户布局固定且可审计
- 读写成本低
- 所有字段更新都能做精确 checked math
- 能在批量结算场景下稳定工作
- 不因为 Anchor 自动序列化带来额外 CU 与 borrow 冲突

### 4.3.3 代码层面需要单独设计的点

合约实现时，需要优先把以下基础设施设计清楚：

- 账户头部与数据体的布局约定
- discriminator 校验方式
- 可变借用与只读借用的边界
- zero-copy 结构体对齐与填充
- 原始字节切片转结构的安全封装
- 通用的 `load_*` / `load_mut_*` 工具
- 批量处理时的账户索引映射工具

建议不要把这些逻辑散落在业务代码里，而是抽成统一的 `state` / `loaders` / `account_utils` 层。

### 4.3.4 开发原则

本阶段链上代码的正确姿势不是“先把业务 if/else 写出来”，而是：

1. 先把 zero-copy 数据布局与读写工具打牢
2. 再把 `settle_match_batch` 的三大分支逐个落进去
3. 最后再补 close 指令

否则后期会因为 borrow、布局、序列化和回写方式不统一而反复返工。

---

## 4.4 close 指令设计

### 4.4.1 本阶段必须实现的 close 范围

本阶段至少覆盖：

- `close_empty_order_state`
- `close_empty_user_position`

`close_resolved_market` 仍保留在总设计中，但如与本阶段开发边界冲突，可先只保留接口与文档约束，不要求立即完成完整实现。

### 4.4.2 `close_empty_order_state`

关闭条件延续 0 号文档原则：

- `filled_amount == total_amount` 或 `canceled == true`
- 不再有后续业务依赖

建议本阶段进一步收敛为：

- 仅允许 owner 主动关闭，或 admin/relayer 在文档明确授权后关闭
- 关闭前再次校验该订单不会再参与任何结算路径
- rent 返还目标必须固定并写死在接口约束中

### 4.4.3 `close_empty_user_position`

关闭条件继续采用 0 号文档：

- `yes_shares == 0`
- `no_shares == 0`
- `claimed_yes_shares == 0`
- `claimed_no_shares == 0`

本阶段要额外强调：

- 只有在账户真的归零时才允许关闭
- 关闭逻辑必须与 zero-copy 布局兼容
- close 前后都需要事件，便于 webhook / indexer 同步

### 4.4.4 close 事件

继续保留并真正落地以下事件：

- `OrderStateClosed`
- `UserPositionClosed`

原因：

- webhook / indexer / 后端恢复逻辑需要通过事件感知账户生命周期
- 如果后续数据库中维护“链上 `UserPosition` 是否存在”的索引，则 close 事件也是索引修正的重要来源

---

## 5. 后端 settlement 侧设计定稿

## 5.1 职责边界

按照 3 号文档，本阶段 settlement 负责：

- 消费 matcher batch
- 将其切成一个或多个 submission batch
- 恢复 `orders` 与 `fills`
- 组织 `remaining_accounts`
- 构造前置 `Ed25519`
- 决定哪些用户要在同一 tx 中前置 `init_user_position`
- 提交链上交易

本阶段 settlement **不负责**：

- tx 成功确认后的最终落库
- tx 成功确认后的内存写回

这部分由 **webhook** 负责。

也就是说，settlement 提交器只负责：

- “做出正确的交易”

而不是：

- “确认交易成功后维护最终真相”

---

## 5.2 `UserPosition` 存在性索引的目标

后端只需要维护一个轻量问题：

- 某个 `(market_id, wallet_address)` 的链上 `UserPosition PDA` 是否已经存在

这个索引的唯一用途是：

- 决定本次 submission tx 是否要前置拼接 `init_user_position`

它不是业务持仓投影，不等同于数据库 `positions` 表。

因此必须和业务持仓分层：

- `positions`：表示业务仓位结果
- `user_position_accounts`：表示链上 `UserPosition PDA` 的存在性索引

---

## 5.3 运行时查询策略

本阶段采用最简策略：

- 进程启动时，从数据库恢复全部已知存在的 `UserPosition` 到内存
- 运行期热路径中，**只查内存**
- 内存 miss 的用户，才进入本次 submission 的链上批量 existence 查询

不采用：

- Redis existence cache
- 热路径中再查 DB
- pending 状态

原因：

- 启动恢复后的内存已经足够作为热路径索引
- 再查 DB 只会变成重复缓存
- `pending` 不是本阶段必要复杂度

---

## 5.4 启动恢复

settlement 模块启动时：

1. 查询 `user_position_accounts` 全表
2. 将所有 `(market_id, wallet_address)` 加载进内存 registry

推荐内存结构：

```go
type UserPositionKey struct {
    MarketID uint64
    Wallet   string
}

type UserPositionRegistry struct {
    mu     sync.RWMutex
    exists map[UserPositionKey]struct{}
}
```

或按 market 分桶：

```go
map[uint64]map[string]struct{}
```

二者都可以，本阶段以实现简单、读性能稳定为先。

---

## 5.5 submission 处理流程

对于每个 submission batch：

1. 提取该 submission 涉及的唯一用户列表
2. 逐个查内存 registry
3. 命中的直接视为 `UserPosition` 已存在
4. 未命中的统一归入 `unknownUsers`
5. 对 `unknownUsers` 一次性派生 PDA，并用一次链上批量查询检查是否存在
6. 查询结果分成：
   - `alreadyExistsUsers`
   - `needInitUsers`
7. 用 `needInitUsers` 生成若干条 `init_user_position`
8. 在同一 tx 中按顺序拼接：
   - `ComputeBudget`（可选）
   - `Ed25519`（可选）
   - `init_user_position * N`
   - `settle_match_batch`
9. 提交链上

### 5.5.1 关于查询结果的处理边界

需要特别说明：

- settlement 模块可以基于链上 existence 查询，决定本次 tx 如何构造
- 但**不负责**在 tx 成功后把这些结果写回最终状态
- tx 成功确认后的内存更新和数据库 upsert，由 webhook 负责

因此本阶段 settlement 提交器的责任边界是：

- **构造正确交易**
- **不承担最终一致性落库**

---

## 5.6 为什么不需要 pending

本阶段不引入 `pending`，理由如下：

- settlement 主流程是围绕 submission batch 做串行构造与提交，不是高并发共享写入同一个索引的场景
- 真正的最终落库由 webhook 驱动，settlement 本身不保存“即将成功”的假设状态
- `pending` 的主要价值是降低并发竞争，而不是保证业务正确性
- 在本阶段先不引入 `pending`，可以避免把职责边界搅乱

如未来出现：

- 多实例 settlement 同时并发处理同一 market
- 高频重复 miss 同一 `(market, user)`

再考虑引入 `pending` 或更细的分布式锁。

---

## 5.7 数据库表建议

建议新增表：

```sql
CREATE TABLE user_position_accounts (
    market_id NUMERIC(20,0) NOT NULL,
    wallet_address VARCHAR(44) NOT NULL,
    user_position_pda VARCHAR(44) NOT NULL,
    created_by_relayer VARCHAR(44),
    created_tx_sig TEXT,
    first_confirmed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address),
    UNIQUE (user_position_pda)
);
```

说明：

- 本表只记录“链上存在性事实”
- 不记录仓位数值
- 不和业务 `positions` 混用
- 启动恢复时，settlement 只需要读取 `(market_id, wallet_address)` 即可

是否写入本表、何时写入本表，由 webhook 负责，不由 settlement 提交器负责。

---

## 6. 实现顺序建议

本阶段建议按以下顺序推进，而不是前后端平均铺开。

### 6.1 第一步：打通合约底层数据层

先完成：

- zero-copy 数据结构定义
- 通用 load / load_mut 工具
- 热账户布局与偏移封装
- checked math 工具
- 通用 close 工具

这是整个阶段最关键的地基。

### 6.2 第二步：落 `init_user_position`

完成：

- `payer` 改 relayer/admin
- 权限校验
- `UserPosition` 初始化逻辑
- 事件补齐

### 6.3 第三步：落 `settle_match_batch`

完成：

- `remaining_accounts` 映射
- 验签内省
- `OrderState` 首次初始化 / 重复使用逻辑
- 三分支结算逻辑
- 费用累计
- 市场统计更新

### 6.4 第四步：补 close 指令

完成：

- `close_empty_order_state`
- `close_empty_user_position`
- 事件与权限边界

### 6.5 第五步：再补 settlement 后端装配

最后再接：

- submission batch builder
- unknown user 批量 existence 查询
- tx 指令拼装
- webhook 对接

这样做的原因很简单：

- 本阶段真正最容易返工的是合约数据层，不是后端装配层

---

## 7. 一句话定稿

本阶段的最终方案可以概括为：

- `settle_match_batch` 继续保持纯结算、绝不开户
- `UserPosition` 由 relayer 在同一笔 settlement tx 中按需前置 `init_user_position` 创建
- 合约实现重点放在 zero-copy 热路径和 close 回收路径
- settlement 运行期只查内存；内存 miss 时再对本 submission 做一次链上批量 existence 查询
- tx 成功后的 `UserPosition` 存在性落库与内存更新，由 webhook 负责，不由 settlement 提交器负责
