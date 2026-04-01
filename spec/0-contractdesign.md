# BlinkPredict Contract Design v6

这份文档是 BlinkPredict 合约侧的定稿清洗版，目标是把以下内容放到一份文档里讲清楚：

- 整个系统和链上交互的全貌
- 账户类型的业务作用、初始化时机、更新时机
- 每个用户故事对应的链上指令、参数、完整 `#[derive(Accounts)]` 代码骨架、核心逻辑
- 签名格式、batch 编码格式、前后端序列化规则
- 后端监听逻辑设计
- 安全性分析
- 附录中的真实代码级数据结构、错误定义、事件结构

当前已经拍板并吸收进设计的结论：

- 全站统一一个真实 USDC 金库，不按市场拆物理金库
- 市场允许任何人创建
- 支持两种开奖模式：
  - `Creator`：创建者或其授权地址开奖
  - `Pyth`：由链上读取 Pyth 价格源开奖
- 不支持 `Invalid` outcome
- `claim_winnings` 允许直接提到用户钱包
- `settle_match_batch` 采用整批回滚，不做部分成功
- `relayer` 唯一
- 不支持 Creator 开奖权后续转让
- 不引入 `nonce_cursor` / 全局一键失效旧订单机制
- 引入 `cancel_all_before_ts` 实现 O(1) 一键全撤
- Creator / 平台手续费不再通过全局 FeeLedger 管理，而是下沉到 `MarketState`
- 手续费只从 taker 收取
- `expiry_ts` 保留，它和 `nonce` 语义不同
- `settle_match_batch` 中严禁使用 `init_if_needed`
- 用户第一次交易某个市场前，必须主动初始化 `UserPosition`
- 所有 PDA / 数据账户都应提供 close 回收指令
- 所有成本和手续费计算一律向上取整（ceiling）
- 热路径账户使用 zero-copy / 原始字节偏移读写

---

## 0. 总体设计结论

系统采用：

- 链下归一化订单簿
- 链下撮合
- 链上最终结算
- 链上最终现金账本
- 链上最终持仓账本
- 统一链上真实金库托管 USDC

也就是说：

- 下单 / 撤单 / 订单簿维护发生在链下
- Gateway 层先把用户的 NO 侧动作归一化，链下内存里只维护 YES 订单簿
- 批量成交最终由链上 `settle_match_batch` 落账
- 用户最终现金余额以链上 `UserLedger` 为准
- 用户最终市场持仓以链上 `UserPosition` 为准
- Creator / Pyth 开奖都在链上完成
- claim / withdraw / fee withdraw 都从链上最终状态出发

新的链上结算不再只有“买卖换手”一种，而是有三种分支：

1. `Match & Mint`：`Buy YES` 碰 `Buy NO`
2. `Transfer`：买单碰卖单
3. `Merge & Burn`：`Sell YES` 碰 `Sell NO`

这不是纯 CEX，因为：

- 数据库不是最终权益账本
- 链上 PDA 才是最终权益账本
- 真实资金由 PDA 控制的统一金库托管
- 冷启动时系统可以通过 `Match & Mint` 在没有任何先验持仓的情况下自然启动市场

### 0.1 Shared Wallet 的链下定位

为了支撑低延迟撮合，链下会维护一份 `shared wallet` 内存视图，供 matcher 做可用余额 / 可用持仓检查。

但必须明确：

- `shared wallet` 不是最终权益账本
- 最终现金余额仍以链上 `UserLedger` 为准
- 最终市场持仓仍以链上 `UserPosition` 为准

`shared wallet` 的职责是：

- 缓存链上 webhook 推进后的基础状态
- 对链下“意图类”操作做乐观更新
- 在 batch 成功 / 失败后做确认或回滚

其推荐结构固定为：

```rust
struct UserWallet {
    pub available_usdc: u64,
    pub locked_usdc: u64,
    pub pending_usdc: i64,
    pub cancel_all_before_ts: i64,
}

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

- `available_*`
  - 当前可继续发起新意图的可用量
- `locked_*`
  - 已被链下挂单占用但尚未释放的量
- `pending_*`
  - 已被链下撮合乐观修改、但仍等待链上 batch 最终确认的增减量

状态推进原则：

- 充值 / 提现 / split / merge 等真实资产型动作，严格依赖 webhook 更新
- 下单 / 撤单 / 撮合等意图类动作，允许链下直接乐观更新内存
- 所有 `cost` / `fee` 计算都必须和链上严格一致，统一采用 ceiling round up
- batch 失败时必须支持按 batch 维度精确回滚；极端情况下允许通过 RPC 重建用户真实快照

因此，系统对链下 shared wallet 的正式定性是：

- **内存乐观直接更新 + 链上 webhook 异步对账**

---

## 1. 所有账户类型

本节只描述账户的业务作用、初始化时机、更新时机。

### 1.1 GlobalConfig

**作用**

协议全局配置：

- 全站统一 collateral mint
- 统一真实金库地址
- 唯一 relayer 地址
- 平台手续费接收地址
- 协议管理员地址
- 协议暂停开关
- 市场创建暂停开关

**初始化时机**

- 协议部署后初始化一次

**更新时机**

- 管理员更新协议配置时

### 1.2 GlobalVaultAuthority（PDA signer）

**作用**

统一金库的 owner，由 PDA 控制，没有私钥。

**初始化时机**

- 协议初始化时确定

**更新时机**

- 不更新，它是确定性的 PDA signer

### 1.3 GlobalVault（真实 USDC 金库）

**作用**

所有用户真实充值资金进入这个账户。

这个账户同时承载：

- 用户未提现资金
- 用户对赌铸造时锁定的准备金
- 市场平仓销毁后待返还的基础资金来源
- Creator 手续费累计资金
- 平台手续费累计资金
- 尚未 claim 的奖金池资金

注意：

- 不额外新建 Creator fee vault / platform fee vault
- 真实 token 始终留在统一金库里
- 费用归属通过市场级字段区分，而不是通过全局 FeeLedger 区分

**初始化时机**

- 协议初始化时创建一次

**更新时机**

- 用户充值时增加
- 用户提现时减少
- 用户 claim 时减少
- `split` / `merge` / `Match & Mint` / `Merge & Burn` 改变金库对应的准备金义务结构

### 1.4 MarketState

**作用**

市场主状态：

- 谁创建的
- 谁能开奖
- Creator / Pyth 哪种模式
- 市场是否还可交易
- 最终结果是什么
- 市场级手续费参数是什么
- 市场当前 open interest 和累计成交量是多少
- 当前市场累计未提现 Creator fee / Platform fee 是多少
- 市场何时允许被关闭和回收

**初始化时机**

- 用户创建市场时初始化

**更新时机**

- `settle_match_batch` 时更新统计信息与 fee 累计值
- `split` / `merge` 时更新 open interest
- 开奖时更新最终状态
- Creator / 平台提取手续费时更新未提现 fee 余额
- 到达回收窗口后允许 close

### 1.5 UserLedger

**作用**

用户全局链上现金账本，记录用户当前在协议里的最终可用 USDC。

它是：

- 成交现金结算依据
- 提现依据
- 用户“一键全撤”的时间戳基准持有者

**初始化时机**

- 用户第一次充值时初始化
- 用户第一次需要链上现金账本时初始化

**更新时机**

- 充值时增加
- 买入成交 / split 时减少
- 卖出成交 / merge / Merge & Burn 时增加
- 提现时减少
- 用户执行一键全撤时更新时间戳阈值

### 1.6 UserPosition

**作用**

用户在某个市场里的最终持仓：

- YES 持仓
- NO 持仓
- 已领奖数量

**初始化时机**

- 用户在第一次交易某个市场前，由前端显式引导调用 `init_user_position` 初始化
- 不允许在 `settle_match_batch` 中动态初始化

**更新时机**

- `split`
- `merge`
- `settle_match_batch`
- `claim`

### 1.7 OrderState

**作用**

用来实现：

- 防重放
- 防超发
- 支持一笔订单分多次成交
- 支持链上取消已经参与过链上结算的订单
- 首次验签后缓存订单哈希，后续部分成交直接走哈希比对

**初始化时机**

- 某个签名订单第一次参与链上 `settle_match_batch` 时初始化
- 但前提是交易所需的 `UserPosition` 等账户已经由用户预初始化完成

**更新时机**

- 每次部分成交时更新 `filled_amount`
- 用户链上 cancel 时更新 `canceled`

### 1.8 SettlementBatch

**作用**

已废弃。

理由：

- 彻底废弃宏观批次序号和批次执行凭证
- 链上防重放只依赖微观 `OrderState`
- relayer 可以乱序、高并发地提交任意 batch，只要订单级约束成立即可

**初始化时机**

- 不再存在

**更新时机**

- 不再存在

### 1.9 FeeLedger

**作用**

已废弃。

理由：

- 全局 FeeLedger 会形成全局单点写锁
- 手续费累计值下沉到 `MarketState`
- Creator 和平台提取收益时，直接从 `MarketState.creator_unclaimed_fee` / `MarketState.platform_unclaimed_fee` 扣减

**初始化时机**

- 不再存在

**更新时机**

- 不再存在

---

## 2. 用户故事

每个用户故事包含：

- 故事描述
- 链上指令参数
- Anchor 账户体系规则（完整代码骨架）
- 指令核心逻辑设计

### 2.1 用户故事一：协议初始化

#### 2.1.1 故事描述

部署完成后，管理员初始化协议，绑定：

- 统一 collateral mint
- 统一真实金库
- 唯一 relayer
- 平台手续费接收地址

#### 2.1.2 链上指令与参数

调用指令：`initialize_config`

```rust
pub struct InitializeConfigArgs {
    pub relayer_signer: Pubkey,
    pub platform_fee_recipient: Pubkey,
}
```

#### 2.1.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
pub struct InitializeConfig<'info> {
    #[account(mut)]
    pub admin: Signer<'info>,

    #[account(
        init,
        payer = admin,
        space = 8 + GlobalConfig::INIT_SPACE,
        seeds = [b"config"],
        bump
    )]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        init,
        payer = admin,
        token::mint = collateral_mint,
        token::authority = global_vault_authority,
        seeds = [b"global_vault"],
        bump
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    pub collateral_mint: InterfaceAccount<'info, Mint>,
    pub token_program: Program<'info, Token2022>,
    pub system_program: Program<'info, System>,
}
```

#### 2.1.4 指令核心逻辑设计

- 初始化 `GlobalConfig`
- 固定 `collateral_mint`
- 固定 `global_vault`
- 固定 `relayer_signer`
- 固定 `platform_fee_recipient`
- 默认协议未暂停

### 2.2 用户故事二：任何人创建市场

#### 2.2.1 故事描述

任何用户都可以创建市场。

前端先把市场文案、图片、规则保存到链下（数据库 / IPFS / 文件服务），再把规则的 hash 和链上开奖必要参数提交到合约。

#### 2.2.2 链上指令与参数

调用指令：`create_market`

```rust
pub struct CreateMarketArgs {
    pub market_id: u64,
    pub metadata_hash: [u8; 32],
    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub resolution_mode: ResolutionMode,
    pub resolution_authority: Pubkey,
    pub oracle_feed_id: [u8; 32],
    pub oracle_condition: OracleCondition,
    pub oracle_target_price_int: u64,
    pub oracle_target_expo: i32,
    pub creator_fee_bps: u16,
    pub platform_fee_bps: u16,
}
```

说明：

- `oracle_observation_ts` 已废除，直接复用 `resolve_after_ts`
- `claim_deadline_ts` 用于控制：
  - 用户最晚 claim 时间
  - 市场最早可 close 回收时间
- `creator_fee_bps + platform_fee_bps <= 10000`

#### 2.2.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: CreateMarketArgs)]
pub struct CreateMarket<'info> {
    #[account(mut)]
    pub creator: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        init,
        payer = creator,
        space = 8 + MarketState::INIT_SPACE,
        seeds = [b"market", &args.market_id.to_le_bytes()],
        bump
    )]
    pub market: Account<'info, MarketState>,

    pub system_program: Program<'info, System>,
}
```

#### 2.2.4 指令核心逻辑设计

- 检查市场创建开关未关闭
- 检查 `close_ts > now`
- 检查 `resolve_after_ts >= close_ts`
- 检查 `claim_deadline_ts > resolve_after_ts`
- 检查 `creator_fee_bps + platform_fee_bps <= 10000`
- Creator 模式：
  - `resolution_authority` 必须合法且非零
- Pyth 模式：
  - `oracle_feed_id` 非零
  - `oracle_target_price_int > 0`
  - 实际开奖时直接使用 `resolve_after_ts` 作为观测时点
- 初始化 `MarketState`
- `creator = signer`
- 市场状态初始为 `Trading`
- outcome 初始为 `Undecided`
- 不支持 `Invalid`

### 2.3 用户故事三：用户充值 USDC

#### 2.3.1 故事描述

用户把钱包里的 USDC 充值到协议里。

充值后：

- 真实 token 进入统一金库
- `UserLedger.available_usdc` 增加
- 前端刷新显示协议内余额
- 后端监听事件并更新读模型

#### 2.3.2 链上指令与参数

调用指令：`deposit`

```rust
pub struct DepositArgs {
    pub amount: u64,
}
```

#### 2.3.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: DepositArgs)]
pub struct Deposit<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        init_if_needed,
        payer = user,
        space = 8 + UserLedger::INIT_SPACE,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump
    )]
    pub user_ledger: Account<'info, UserLedger>,

    #[account(mut)]
    pub user_token_account: InterfaceAccount<'info, TokenAccount>,

    #[account(
        mut,
        seeds = [b"global_vault"],
        bump,
        constraint = global_vault.key() == config.global_vault,
        constraint = global_vault.mint == config.collateral_mint,
        constraint = global_vault.owner == global_vault_authority.key(),
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    pub token_program: Program<'info, Token2022>,
    pub system_program: Program<'info, System>,
}
```

#### 2.3.4 指令核心逻辑设计

- 检查金额大于 0
- 真实 token 从 `user_token_account` 转到 `global_vault`
- `UserLedger` 不存在则初始化
- 增加 `UserLedger.available_usdc`
- 发出 `DepositSettled`

### 2.4 用户故事四：用户初始化某个市场的持仓账户

#### 2.4.1 故事描述

为了避免在 `settle_match_batch` 中使用 `init_if_needed`，用户在第一次交易某个市场前，必须由前端引导先发一笔极简交易创建 `UserPosition`。

这一步的目标：

- 让用户自己承担开荒租金和 compute unit 消耗
- 保证批量结算路径尽可能轻量

#### 2.4.2 链上指令与参数

调用指令：`init_user_position`

```rust
pub struct InitUserPositionArgs {}
```

#### 2.4.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
pub struct InitUserPosition<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,

    #[account(
        init,
        payer = user,
        space = 8 + UserPosition::INIT_SPACE,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump
    )]
    pub user_position: Account<'info, UserPosition>,

    pub system_program: Program<'info, System>,
}
```

#### 2.4.4 指令核心逻辑设计

- 初始化空的 `UserPosition`
- 初始 yes/no 持仓为 0
- 初始 claimed yes/no 为 0

### 2.5 用户故事五：用户链下下单

#### 2.5.1 故事描述

用户在前端下单。

这里有一个关键设计变化：

- Gateway 层先把订单归一化
- 链下内存里只维护 YES 订单簿

也就是：

- `Buy NO` 会被转换成“价格镜像后的另一侧 YES 订单语义”
- 最终 matcher 只需要处理一套归一化簿
- 链上再根据原始签名订单的 side/outcome 判断结算走哪条分支

#### 2.5.2 链上指令与参数

- 无链上指令

#### 2.5.3 Anchor 框架下账户体系规则

- 无

#### 2.5.4 指令核心逻辑设计

- 无链上逻辑
- 这是纯链下行为
- 合约直到最终批量结算时才知道这个订单存在

### 2.6 用户故事六：用户链下一键全撤 / 链上取消订单

#### 2.6.1 故事描述

撤单分三种：

情况 A：订单从未参与过链上结算
- 只是链下订单簿订单
- 直接链下删除即可
- 不需要链上交互

情况 B：订单已经至少一部分参与过链上结算
- 这时链上已存在 `OrderState`
- 如果用户希望阻止后续继续成交，就需要调用链上 `cancel_order`

情况 C：用户在极端行情中希望一键全撤
- 通过更新 `UserLedger.cancel_all_before_ts`
- 让某时间戳之前的订单全部作废

#### 2.6.2 链上指令与参数

调用指令一：`cancel_order`

```rust
pub struct CancelOrderArgs {
    pub nonce: u64,
}
```

调用指令二：`cancel_all_orders_before`

```rust
pub struct CancelAllOrdersBeforeArgs {
    pub cutoff_ts: i64,
}
```

#### 2.6.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: CancelOrderArgs)]
pub struct CancelOrder<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(
        mut,
        seeds = [b"order", user.key().as_ref(), market.key().as_ref(), &args.nonce.to_le_bytes()],
        bump = order_state.bump,
        constraint = order_state.owner == user.key(),
    )]
    pub order_state: Account<'info, OrderState>,

    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
}

#[derive(Accounts)]
#[instruction(args: CancelAllOrdersBeforeArgs)]
pub struct CancelAllOrdersBefore<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(
        mut,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump = user_ledger.bump,
        constraint = user_ledger.owner == user.key(),
    )]
    pub user_ledger: Account<'info, UserLedger>,
}
```

#### 2.6.4 指令核心逻辑设计

`cancel_order`：
- 用户必须是订单 owner
- `order_state.canceled == false`
- 设置 `canceled = true`
- 发出 `OrderCanceled`

`cancel_all_orders_before`：
- 更新 `user_ledger.cancel_all_before_ts = cutoff_ts`
- 所有 nonce 内嵌时间戳早于该阈值的订单，在未来结算时都必须被拒绝

### 2.7 用户故事七：链下撮合成功，后端批量上链结算

#### 2.7.1 故事描述

这是系统最核心的故事。

链下撮合完成后：

- matcher 产出 fills
- 后端按 market 聚合成 batch
- relayer 把 batch 提交到链上
- 合约最终完成现金、持仓、订单使用量、费用归属的更新

注意：

- 链上不再依赖 `SettlementBatch` 或宏观结算序号防重放
- relayer 可以乱序、高并发地把 batch 轰炸上链
- 失败了直接重试
- 是否可成交完全由订单级 `OrderState` 和余额/持仓约束决定

#### 2.7.2 链上指令与参数

调用指令：`settle_match_batch`

```rust
pub struct FillIndexPair {
    pub maker_idx: u16,
    pub taker_idx: u16,
    pub fill_amount: u64,
    pub fill_price: u64,
}

pub struct SettleMatchBatchArgs {
    pub orders: Vec<OrderIntentV1>,
    pub fills: Vec<FillIndexPair>,
}
```

#### 2.7.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: SettleMatchBatchArgs)]
pub struct SettleMatchBatch<'info> {
    #[account(mut)]
    pub relayer: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: AccountInfo<'info>,

    /// CHECK: instruction sysvar for ed25519 introspection
    #[account(address = anchor_lang::solana_program::sysvar::instructions::ID)]
    pub instruction_sysvar: UncheckedAccount<'info>,

    pub system_program: Program<'info, System>,
}
```

`remaining_accounts` 必须严格按以下一维数组排布：

1. `[全表现金账本]`：所有参与订单的用户 `UserLedger`
2. `[全表持仓账本]`：所有参与订单的用户 `UserPosition`
3. `[全表订单状态]`：所有参与订单的 `OrderState`

链上通过 O(1) 偏移公式定位账户，例如：

- `ledger_account = remaining_accounts[user_idx]`
- `position_account = remaining_accounts[ledger_count + user_idx]`
- `order_state_account = remaining_accounts[ledger_count + position_count + order_idx]`

说明：

- 不再传 `creator_fee_ledger` / `platform_fee_ledger`
- 费用直接累计到 `MarketState.creator_unclaimed_fee` / `MarketState.platform_unclaimed_fee`
- 也不再创建 `SettlementBatch`
- 高频修改的热账户（`UserLedger`、`UserPosition`、`OrderState`）在实现上禁止使用 `Account<'info, T>` 自动反序列化，改用 zero-copy / 原始字节偏移读写

#### 2.7.4 指令核心逻辑设计

A. 市场状态校验
- `market.status == Trading`
- 当前时间 `< market.close_ts`
- 市场尚未开奖

B. relayer 校验
- `relayer.key == config.relayer_signer`
- relayer 唯一

C. 原生 Ed25519 验签 + 合约内省
- 绝不在 Rust 合约内手写椭圆曲线验签
- 后端将原生 Ed25519 验证指令排在交易前面
- 合约通过 `instruction_sysvar` 回看前置指令，确认数据匹配且验签成功

D. 业务级“验签缓存（Signature Caching）”
- 首次上链的订单：
  - 校验原生 Ed25519 指令
  - 验签通过后初始化 `OrderState`
  - 写入 `order_hash`
- 二次/多次上链的订单（部分成交）：
  - 直接跳过密码学验签
  - 读取 `OrderState.order_hash`
  - 仅做 `current_hash == stored_hash` 对比

E. 订单一致性与微观防重放
- `OrderState` 种子必须包含 market：
  - `seeds = [b"order", user, market, nonce]`
- 后续结算必须校验：
  - `filled_amount + fill_amount <= total_amount`
  - `canceled == false`
  - 订单时间戳不得早于 `UserLedger.cancel_all_before_ts`
- `nonce` 生成必须采用：
  - 时间戳 + 随机数 拼接
  - 以确保空间和时间上的唯一性

F. 链上三分支结算逻辑

**分支一：Match & Mint（对赌铸造）**

触发条件：
- `Buy YES` 碰 `Buy NO`

逻辑：
- 双方都不需要预先持仓
- 双方各自扣减 USDC
- 金库准备金锁定增加
- 凭空为双方各自铸造一份最终持仓：
  - 买 YES 方增加 `yes_shares`
  - 买 NO 方增加 `no_shares`

用途：
- 解决冷启动时双方持仓都为 0 仍然可以成交的问题
- 这是预测市场最关键的“原生对赌铸造”路径

**分支二：Transfer（传统换手）**

触发条件：
- 买单碰卖单

逻辑：
- 买方扣 USDC，获得份额
- 卖方扣份额，获得 USDC
- 这是标准二级市场换手

**分支三：Merge & Burn（平仓销毁）**

触发条件：
- `Sell YES` 碰 `Sell NO`

逻辑：
- 双方都必须已有对应份额
- 双方各自扣减份额
- 份额对消并销毁
- 从金库中解锁对应 USDC 返还给双方

用途：
- 允许用户把成对的 YES/NO 头寸重新合并为现金

G. 手续费逻辑
- creator fee 从成交金额中按 `market.creator_fee_bps` 计算
- platform fee 从成交金额中按 `market.platform_fee_bps` 计算
- 手续费只从 taker 成交现金侧扣除
- 费用不记全局 FeeLedger，而是直接增加：
  - `market.creator_unclaimed_fee`
  - `market.platform_unclaimed_fee`

H. 精度与数学安全
- 所有 `cost`、手续费都必须使用 **向上取整（ceiling）**
- 所有中间乘除法都使用 checked math
- 这样可以彻底堵死微小份额造成的粉尘下溢攻击（dust attack）

I. 状态更新
- 更新双方 `UserLedger`
- 更新双方 `UserPosition`
- 更新双方 `OrderState`
- 更新市场 `total_yes_open_interest`
- 更新市场 `total_no_open_interest`
- 更新市场 `total_matched_amount`
- 更新市场未提现费用字段

### 2.8 用户故事八：用户主动 split

#### 2.8.1 故事描述

用户把 1 USDC 拆成一对完整份额：

- YES 1 份
- NO 1 份

这是用户主动一级铸造，不依赖撮合。

#### 2.8.2 链上指令与参数

调用指令：`split_position`

```rust
pub struct SplitPositionArgs {
    pub amount: u64,
}
```

#### 2.8.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: SplitPositionArgs)]
pub struct SplitPosition<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(seeds = [b"market", &market.market_id.to_le_bytes()], bump = market.bump)]
    pub market: Account<'info, MarketState>,

    #[account(
        mut,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump = user_ledger.bump,
        constraint = user_ledger.owner == user.key(),
    )]
    pub user_ledger: AccountInfo<'info>,

    #[account(
        mut,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump = user_position.bump,
        constraint = user_position.owner == user.key(),
        constraint = user_position.market == market.key(),
    )]
    pub user_position: AccountInfo<'info>,
}
```

#### 2.8.4 指令核心逻辑设计

- 用户扣减 `available_usdc`
- 增加 `yes_shares`
- 增加 `no_shares`
- 更新市场 open interest
- 不发生真实 token 转账，只是协议内资金锁定形态变化

### 2.9 用户故事九：用户主动 merge

#### 2.9.1 故事描述

用户把一对完整份额重新合并回现金：

- 扣减一份 YES
- 扣减一份 NO
- 解锁 1 USDC 到可用余额

#### 2.9.2 链上指令与参数

调用指令：`merge_position`

```rust
pub struct MergePositionArgs {
    pub amount: u64,
}
```

#### 2.9.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: MergePositionArgs)]
pub struct MergePosition<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(seeds = [b"market", &market.market_id.to_le_bytes()], bump = market.bump)]
    pub market: Account<'info, MarketState>,

    #[account(
        mut,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump = user_ledger.bump,
        constraint = user_ledger.owner == user.key(),
    )]
    pub user_ledger: AccountInfo<'info>,

    #[account(
        mut,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump = user_position.bump,
        constraint = user_position.owner == user.key(),
        constraint = user_position.market == market.key(),
    )]
    pub user_position: AccountInfo<'info>,
}
```

#### 2.9.4 指令核心逻辑设计

- 要求用户同时持有足够 YES 和 NO
- 扣减 yes/no 份额
- 增加 `available_usdc`
- 更新市场 open interest
- 不发生真实 token 转账，只是协议内资金解锁

### 2.10 用户故事十：Creator 模式开奖

#### 2.10.1 故事描述

Creator 模式下：

- 创建者或其指定地址开奖
- 开奖后市场不可再结算成交

#### 2.10.2 链上指令与参数

调用指令：`resolve_market_by_creator`

```rust
pub struct ResolveMarketByCreatorArgs {
    pub outcome: MarketOutcome,
}
```

说明：

- 当前已确认不支持 `Invalid`
- 所以 outcome 只能是 `Yes` 或 `No`

#### 2.10.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: ResolveMarketByCreatorArgs)]
pub struct ResolveMarketByCreator<'info> {
    #[account(mut)]
    pub authority: Signer<'info>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
        constraint = market.resolution_mode == ResolutionMode::Creator,
        constraint = market.resolution_authority == authority.key(),
    )]
    pub market: Account<'info, MarketState>,
}
```

#### 2.10.4 指令核心逻辑设计

- `resolution_mode == Creator`
- `authority == resolution_authority`
- 当前时间 `>= resolve_after_ts`
- 市场尚未开奖
- outcome 只能是 `Yes` 或 `No`
- 写入 `market.status = Resolved`
- 写入 `market.outcome`
- 写入 `market.resolved_at`

### 2.11 用户故事十一：Pyth 模式开奖

#### 2.11.1 故事描述

Pyth 模式下：

- 任何人都可以触发开奖
- 合约直接读取指定 feed
- 根据链上记录好的目标阈值和条件判断 YES / NO

#### 2.11.2 链上指令与参数

调用指令：`resolve_market_by_pyth`

```rust
pub struct ResolveMarketByPythArgs {}
```

#### 2.11.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
pub struct ResolveMarketByPyth<'info> {
    #[account(mut)]
    pub caller: Signer<'info>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
        constraint = market.resolution_mode == ResolutionMode::Pyth,
    )]
    pub market: Account<'info, MarketState>,

    /// CHECK: validated against market.oracle_feed_id in instruction logic
    pub pyth_price_account: UncheckedAccount<'info>,
}
```

#### 2.11.4 指令核心逻辑设计

- `resolution_mode == Pyth`
- 当前时间 `>= resolve_after_ts`
- 市场尚未开奖
- 直接把 `resolve_after_ts` 当作观测时点
- 从 `pyth_price_account` 读取价格
- 与 `oracle_target_price_int + oracle_target_expo` 组合出的阈值比较
- 根据 `oracle_condition` 判断 `Yes` 或 `No`
- 写入最终 outcome 和 resolved_at

### 2.12 用户故事十二：用户领取奖金（直接提钱包）

#### 2.12.1 故事描述

用户在市场开奖后点击 claim。

当前已确认：

- claim 允许直接提钱包
- 不再先回 `UserLedger`

#### 2.12.2 链上指令与参数

调用指令：`claim_winnings`

```rust
pub struct ClaimWinningsArgs {}
```

#### 2.12.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
pub struct ClaimWinnings<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(
        mut,
        seeds = [b"config"],
        bump = config.bump,
    )]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
        constraint = market.status == MarketStatus::Resolved,
    )]
    pub market: Account<'info, MarketState>,

    #[account(
        mut,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump = user_position.bump,
        constraint = user_position.owner == user.key(),
        constraint = user_position.market == market.key(),
    )]
    pub user_position: AccountInfo<'info>,

    #[account(
        mut,
        seeds = [b"global_vault"],
        bump,
        constraint = global_vault.key() == config.global_vault,
        constraint = global_vault.owner == global_vault_authority.key(),
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    #[account(mut)]
    pub user_token_account: InterfaceAccount<'info, TokenAccount>,

    pub token_program: Program<'info, Token2022>,
}
```

#### 2.12.4 指令核心逻辑设计

- 市场必须已开奖
- 当前时间必须 `<= claim_deadline_ts`
- 根据 market outcome 判断哪一边是获胜份额
- 计算 `未领取的获胜份额数量`
- 按 1 share = 1 USDC 计算可领金额
- 更新 `claimed_yes_shares` 或 `claimed_no_shares`
- 通过 `invoke_signed` 从统一金库把真实 token 打到用户钱包
- 不经过 `UserLedger.available_usdc`
- 超过 `claim_deadline_ts` 后收益失效，不再可领

### 2.13 用户故事十三：用户提现未用于结算的可用余额

#### 2.13.1 故事描述

用户希望把协议里未用于持仓和结算的可用 USDC 提回钱包。

#### 2.13.2 链上指令与参数

调用指令：`withdraw`

```rust
pub struct WithdrawArgs {
    pub amount: u64,
}
```

#### 2.13.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: WithdrawArgs)]
pub struct Withdraw<'info> {
    #[account(mut)]
    pub user: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        mut,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump = user_ledger.bump,
        constraint = user_ledger.owner == user.key(),
    )]
    pub user_ledger: AccountInfo<'info>,

    #[account(
        mut,
        seeds = [b"global_vault"],
        bump,
        constraint = global_vault.key() == config.global_vault,
        constraint = global_vault.owner == global_vault_authority.key(),
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    #[account(mut)]
    pub user_token_account: InterfaceAccount<'info, TokenAccount>,

    pub token_program: Program<'info, Token2022>,
}
```

#### 2.13.4 指令核心逻辑设计

- 检查 `user_ledger.available_usdc >= amount`
- 扣减 `available_usdc`
- 从统一金库直接打回用户钱包

### 2.14 用户故事十四：Creator 提取手续费收益

#### 2.14.1 故事描述

某个 Creator 创建的市场已经产生手续费。

因为真实 token 都在统一金库里，所以 Creator 的收益不会自动拆成单独金库，而是直接累计在 `MarketState.creator_unclaimed_fee` 中。

#### 2.14.2 链上指令与参数

调用指令：`withdraw_creator_fee`

```rust
pub struct WithdrawCreatorFeeArgs {
    pub amount: u64,
}
```

#### 2.14.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: WithdrawCreatorFeeArgs)]
pub struct WithdrawCreatorFee<'info> {
    #[account(mut)]
    pub creator: Signer<'info>,

    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
        constraint = market.creator == creator.key(),
        constraint = market.status == MarketStatus::Resolved,
    )]
    pub market: Account<'info, MarketState>,

    #[account(
        mut,
        seeds = [b"global_vault"],
        bump,
        constraint = global_vault.key() == config.global_vault,
        constraint = global_vault.owner == global_vault_authority.key(),
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    #[account(mut)]
    pub creator_token_account: InterfaceAccount<'info, TokenAccount>,

    pub token_program: Program<'info, Token2022>,
}
```

#### 2.14.4 指令核心逻辑设计

- 当前时间必须 `>= claim_deadline_ts`
- 检查 `market.creator_unclaimed_fee >= amount`
- 扣减 `creator_unclaimed_fee`
- 从统一金库打款到 Creator 钱包

### 2.15 用户故事十五：平台提取手续费收益

#### 2.15.1 故事描述

平台作为协议运营方，会累积平台手续费。

平台收益直接累计在 `MarketState.platform_unclaimed_fee` 中，提取时从统一金库转出。

#### 2.15.2 链上指令与参数

调用指令：`withdraw_platform_fee`

```rust
pub struct WithdrawPlatformFeeArgs {
    pub amount: u64,
}
```

#### 2.15.3 Anchor 框架下账户体系规则

```rust
#[derive(Accounts)]
#[instruction(args: WithdrawPlatformFeeArgs)]
pub struct WithdrawPlatformFee<'info> {
    #[account(mut)]
    pub admin: Signer<'info>,

    #[account(
        seeds = [b"config"],
        bump = config.bump,
        constraint = config.admin == admin.key(),
    )]
    pub config: Account<'info, GlobalConfig>,

    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
        constraint = market.status == MarketStatus::Resolved,
    )]
    pub market: Account<'info, MarketState>,

    #[account(
        mut,
        seeds = [b"global_vault"],
        bump,
        constraint = global_vault.key() == config.global_vault,
        constraint = global_vault.owner == global_vault_authority.key(),
    )]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,

    /// CHECK: PDA signer only
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,

    #[account(mut)]
    pub platform_token_account: InterfaceAccount<'info, TokenAccount>,

    pub token_program: Program<'info, Token2022>,
}
```

#### 2.15.4 指令核心逻辑设计

- 当前时间必须 `>= claim_deadline_ts`
- 只有 admin 可提平台费用
- 检查 `market.platform_unclaimed_fee >= amount`
- 从统一金库打到平台地址
- 扣减 `platform_unclaimed_fee`

### 2.16 用户故事十六：关闭和回收账户

#### 2.16.1 故事描述

所有 PDA / 数据账户都需要提供 close 回收路径，防止长期租金沉淀。

#### 2.16.2 链上指令与参数

至少应有：

- `close_empty_order_state`
- `close_empty_user_position`
- `close_resolved_market`

#### 2.16.3 Anchor 框架下账户体系规则

每个 close 指令都应：

- 明确 owner 或 admin 权限
- 明确 close 到谁
- 明确 close 前的约束条件

#### 2.16.4 指令核心逻辑设计

- `OrderState` 仅当：
  - `filled_amount == total_amount` 或 `canceled == true`
  - 且不再有后续业务依赖时允许 close
- `UserPosition` 仅当：
  - `yes_shares == 0`
  - `no_shares == 0`
  - `claimed_yes_shares == 0`
  - `claimed_no_shares == 0`
  时允许 close
- `MarketState` 仅当：
  - 市场已开奖
  - 当前时间 `>= claim_deadline_ts`
  - 未提现费用为 0
  - 不再有可索赔收益
  才允许 close
- close 后 rent 返还给指定地址

---

## 3. 后端监听逻辑设计

### 3.1 必须监听的事件

后端至少监听：

- `MarketCreated`
- `DepositSettled`
- `SplitExecuted`
- `MergeExecuted`
- `MatchSettled`
- `OrderCanceled`
- `CancelAllBeforeUpdated`
- `MarketResolved`
- `WinningsClaimed`
- `Withdrawn`
- `CreatorFeeWithdrawn`
- `PlatformFeeWithdrawn`
- 各类 `Closed` 事件

### 3.2 监听后的链下动作

A. `MarketCreated`
- 写入数据库 `markets`
- 关联链下 metadata
- 通知前端市场可见

B. `DepositSettled`
- 更新用户余额读模型
- 更新 Redis / 内存余额缓存

C. `SplitExecuted` / `MergeExecuted`
- 更新用户持仓和协议内余额读模型

D. `MatchSettled`
- 更新订单状态
- 更新用户现金和持仓读模型
- 更新 Creator / 平台手续费收益读模型

E. `OrderCanceled`
- 更新链下订单状态

F. `CancelAllBeforeUpdated`
- 更新用户的链下一键全撤阈值缓存

G. `MarketResolved`
- 切换市场为已开奖状态
- 开启 claim 流程
- 前端开始显示 claim deadline 倒计时

H. `WinningsClaimed`
- 更新用户持仓可领奖状态
- 更新用户链上 token 流出记录

I. `Withdrawn`
- 更新用户余额读模型

J. `CreatorFeeWithdrawn` / `PlatformFeeWithdrawn`
- 更新收益读模型
- 更新 Creator / 平台收益页展示

K. 各类 `Closed` 事件
- 更新链下索引，标记账户已归档关闭

### 3.3 后端不需要监听的内容

- 用户钱包任意 USDC 变化
- 多 market vault 变化
- YES/NO SPL Token 变化
- 链上订单簿状态
- 宏观 settlement 序列号变化（已经废除）

---

## 4. 签名与 Batch 格式

这一节把原本应拆出去的签名与 batch 文档直接放进主文档。

### 4.1 订单签名结构

```rust
#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct OrderIntentV1 {
    pub version: u8,
    // 版本号，后续升级兼容用

    pub chain_id: u16,
    // 链标识，避免跨链重放

    pub program_id: Pubkey,
    // 绑定当前合约程序，避免跨程序重放

    pub market: Pubkey,
    // 订单所属市场

    pub user: Pubkey,
    // 签名用户地址

    pub side: Side,
    // 买 / 卖

    pub outcome: Outcome,
    // YES / NO

    pub limit_price: u64,
    // 用户授权价格 tick，取值 1..99，表示 $0.01 .. $0.99

    pub total_amount: u64,
    // Limit / Market Sell: 份额 lots = shares * 100
    // Market Buy: 金额 cents = usdc * 100

    pub nonce: u64,
    // 用户订单唯一 nonce；必须采用“时间戳 + Random随机数”的拼接方案

    pub expiry_ts: i64,
    // 限价单过期时间；保留，和 nonce 语义不同
}
```

精度模型定稿：

- `limit_price`
  - 永远是整数价格 tick，范围 `1..99`
- `total_amount`
  - `limit` / `market sell` 时表示 `shares * 100`
  - `market buy` 时表示 `usdc * 100`
- `fill_amount`
  - 永远是 `shares * 100`
- `fill_price`
  - 永远是成交价 tick，范围 `1..99`

### 4.2 签名消息编码规则

建议规则：

- 前后端统一按协议约定的固定字段顺序和小端序对 `OrderIntentV1` 做序列化
- 然后对该序列化字节做 `keccak256`
- 用户签名的是这个 hash 的十六进制文本的 UTF-8 字节

```text
signed_message = utf8(hex(keccak256(serialize(OrderIntentV1))))
```

### 4.3 前端签名规则

前端下单时：

1. 组装 `OrderIntentV1`
2. 按协议约定序列化
3. 计算 `keccak256`
4. 转成 hex 文本后取 UTF-8 字节
5. 钱包签名
6. 把：
   - 原始订单字段
   - 签名
   - 序列化字节
   发给后端

Gateway 还要做一件额外的事：

- 对订单做归一化
- 链下内存簿只保留 YES 订单簿视角
- 但原始签名订单不得被篡改，链上仍按原始订单校验

### 4.4 后端 batch 编码规则

后端在链下撮合成功后：

- 按 market 聚合 fills
- 一个 batch 只包含同一 market 的 fills
- `orders` 和 `fills` 分开传输
- `orders` 是去重后的订单列表
- `fills` 只保存 maker/taker 的索引对和成交信息

```rust
#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct FillIndexPair {
    pub maker_idx: u16,
    pub taker_idx: u16,
    pub fill_amount: u64,
    pub fill_price: u64,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct SettleMatchBatchArgs {
    pub orders: Vec<OrderIntentV1>,
    pub fills: Vec<FillIndexPair>,
}
```

### 4.5 原生 Ed25519 验签 + 合约内省 + 业务级验签缓存

链上绝不手写椭圆曲线验签。

做法：

- 后端将原生 `Ed25519Program` 验证指令排在交易前面
- 合约内部通过 `instruction_sysvar` 回看上一条或前置指令，确认：
  - 验签成功
  - 消息内容匹配

为了提升性能，引入业务级“验签缓存”：

- 首次上链的订单：
  - 校验原生 Ed25519 指令
  - 验签通过后初始化 `OrderState`
  - 写入 `order_hash`
- 二次/多次上链的订单（部分成交）：
  - 直接跳过密码学验签
  - 读取 `OrderState.order_hash`
  - 只做 `current_hash == stored_hash` 对比

### 4.6 前后端序列化规则定稿

必须统一：

- 整数全部小端序
- `OrderIntentV1` 的签名序列化采用协议约定的固定字段顺序和小端序编码
- `fill` / `batch` 参数的编码规则必须与链上保持一致
- 枚举值映射前后端完全一致
- 字段顺序严禁前后端各自定义
- `cost` 与手续费计算统一用 ceiling round up

建议：

- 前端和后端共用同一份 order schema 文档
- 后端和合约共用同一份 Rust 对应结构定义

---

## 5. 当前设计安全性分析

### 5.1 冷启动无法成交问题

旧方案如果只有 `Buy YES` 和 `Sell YES`，双方 `UserPosition` 初始都为 0，会直接失败。

现在通过三分支解决：

- `Buy YES` 碰 `Buy NO` -> `Match & Mint`

因此：

- 冷启动不再依赖已有持仓
- 市场可以从零自然启动

### 5.2 超卖 / 重卖

链上在 `settle_match_batch` 拦截：

- `Transfer` 时卖 YES 检查 `yes_shares >= fill_amount`
- `Transfer` 时卖 NO 检查 `no_shares >= fill_amount`
- `Merge & Burn` 时双方也必须各自持有足够份额

### 5.3 超买 / 余额不足

链上在 `settle_match_batch` / `split` 拦截：

- 买方 `UserLedger.available_usdc >= cost + fee`
- `Match & Mint` 双方都必须余额足够

### 5.4 订单签名重放

链上依赖：

- `OrderState(user, market, nonce)`
- `order_hash`
- `filled_amount <= total_amount`

### 5.5 链下撤单后继续被结算

链上依赖：

- `OrderState.canceled`
- `UserLedger.cancel_all_before_ts`

### 5.6 relayer 被盗

链上仍拦截：

- 无法伪造用户签名
- 无法突破订单上限
- 无法突破余额和持仓约束

### 5.7 Creator 恶意开奖

这是产品信任风险，不是合约数学漏洞。

链上只能确保：

- 只有 `resolution_authority` 能开奖

### 5.8 Pyth 参数错误

链上拦截：

- feed 必须匹配
- 观察时间必须达到（现在直接是 `resolve_after_ts`）
- 比较逻辑固定

### 5.9 统一金库资产不能被平台随意挪用

链上保障：

- `GlobalVault.owner == GlobalVaultAuthority PDA`
- 没有私钥能直接搬走钱
- 只能通过 `withdraw / claim / fee withdraw` 等受规则约束的指令出金

### 5.10 Creator 和平台费用不会被重复提取

链上保障：

- 费用在 `MarketState` 内单独记账
- 提取时减少 `creator_unclaimed_fee` / `platform_unclaimed_fee`
- 再次提取必须余额足够

### 5.11 粉尘攻击与数学下溢

强制要求：

- `cost` 计算用向上取整
- 手续费计算用向上取整
- 所有中间乘除法使用 checked math

这样可以堵死利用极小份额反复套取舍入误差的 dust attack。

### 5.12 账户热点与性能风险

性能设计要求：

- `UserLedger`、`UserPosition`、`OrderState` 热路径必须用 zero-copy / 偏移量读写
- `remaining_accounts` 必须严格一维阵列排布
- 不允许在 `settle_match_batch` 中动态初始化用户持仓账户

---

## 6. 待讨论的问题

旧问题按你已确认的内容已删除，当前只保留新的问题。

### 6.1 `close_resolved_market` 后，未 claim 收益失效的资产最终归属

当前已确定：

- 用户必须在 `claim_deadline_ts` 前 claim
- 到期后可关闭市场

但还需要拍板：

- 逾期未 claim 的那部分资产归谁？
  - 平台
  - creator
  - 按某种比例分配
  - 销毁语义（实际上无法销毁 USDC，只能归集）

### 6.2 `expiry_ts` 是否所有订单都要求，还是只对限价单要求

你已经明确：

- `expiry_ts` 不能删

但实现上还要拍板：

- 市价单是否一律要求 `expiry_ts = now + short ttl`
- 还是市价单走固定内部过期规则

### 6.3 `close_empty_order_state` 的关闭条件是否需要更严格

当前建议：

- 满额成交或 canceled 即可 close

但如果后续需要更强审计留痕，可能要增加最短保留期。

---

## 7. 数据结构附录汇总

这一节写真实代码级别的结构定义，包括：

- 账户结构
- 枚举结构
- 错误定义
- 事件结构

### 7.1 账户结构附录

#### 7.1.1 GlobalConfig

```rust
#[account]
#[derive(InitSpace)]
pub struct GlobalConfig {
    pub admin: Pubkey,
    // 协议管理员地址

    pub collateral_mint: Pubkey,
    // 全站统一使用的抵押资产 mint

    pub global_vault: Pubkey,
    // 统一物理金库地址

    pub relayer_signer: Pubkey,
    // 唯一允许提交批量结算的 relayer

    pub platform_fee_recipient: Pubkey,
    // 平台手续费最终接收地址

    pub protocol_paused: bool,
    // 协议总暂停开关

    pub market_create_paused: bool,
    // 市场创建暂停开关

    pub vault_authority_bump: u8,
    // 统一金库 PDA signer 的 bump

    pub bump: u8,
    // config PDA bump
}
```

#### 7.1.2 MarketState

```rust
#[account]
#[derive(InitSpace)]
pub struct MarketState {
    pub market_id: u64,
    // 市场唯一 ID

    pub creator: Pubkey,
    // 市场创建者地址

    pub resolution_authority: Pubkey,
    // Creator 模式开奖地址

    pub status: MarketStatus,
    // 市场状态：Trading / Resolved

    pub outcome: MarketOutcome,
    // 最终结果：Undecided / Yes / No

    pub resolution_mode: ResolutionMode,
    // 开奖模式：Creator / Pyth

    pub metadata_hash: [u8; 32],
    // 链下 metadata 内容哈希

    pub close_ts: i64,
    // 停止接受新结算的时间

    pub resolve_after_ts: i64,
    // 最早允许开奖时间；Pyth 模式下也作为观测时间

    pub claim_deadline_ts: i64,
    // 用户最晚 claim 时间，也是市场最早可 close 的时间边界

    pub resolved_at: i64,
    // 实际开奖时间

    pub oracle_feed_id: [u8; 32],
    // Pyth feed id；Creator 模式可为零

    pub oracle_condition: OracleCondition,
    // Pyth 开奖条件

    pub oracle_target_price_int: u64,
    // 目标价格整数部分

    pub oracle_target_expo: i32,
    // 目标价格指数部分

    pub creator_fee_bps: u16,
    // Creator 手续费比例，单位 bps

    pub platform_fee_bps: u16,
    // 平台手续费比例，单位 bps

    pub creator_unclaimed_fee: u64,
    // 当前市场尚未被 creator 提走的手续费金额

    pub platform_unclaimed_fee: u64,
    // 当前市场尚未被平台提走的手续费金额

    pub total_yes_open_interest: u64,
    // 当前市场 YES 总持仓

    pub total_no_open_interest: u64,
    // 当前市场 NO 总持仓

    pub total_matched_amount: u64,
    // 当前市场历史累计成交量

    pub bump: u8,
    // market PDA bump
}
```

#### 7.1.3 UserLedger

```rust
#[account]
#[derive(InitSpace)]
pub struct UserLedger {
    pub owner: Pubkey,
    // 该账本属于哪个用户

    pub available_usdc: u64,
    // 当前协议内可用 USDC

    pub cancel_all_before_ts: i64,
    // 该时间戳之前的所有订单默认失效，用于一键全撤

    pub bump: u8,
    // user_ledger PDA bump
}
```

#### 7.1.4 UserPosition

```rust
#[account(zero_copy)]
pub struct UserPosition {
    pub owner: Pubkey,
    // 持仓所属用户

    pub market: Pubkey,
    // 对应市场

    pub yes_shares: u64,
    // 最终 settled 的 YES 持仓

    pub no_shares: u64,
    // 最终 settled 的 NO 持仓

    pub claimed_yes_shares: u64,
    // 已领取奖金的 YES 份额

    pub claimed_no_shares: u64,
    // 已领取奖金的 NO 份额

    pub bump: u8,
    // user_position PDA bump
}
```

#### 7.1.5 OrderState

```rust
#[account(zero_copy)]
pub struct OrderState {
    pub owner: Pubkey,
    // 订单所属用户

    pub nonce: u64,
    // 订单 nonce；应采用“时间戳 + 随机数”方案生成

    pub order_hash: [u8; 32],
    // 原始签名订单哈希

    pub total_amount: u64,
    // 用户授权最大成交量

    pub filled_amount: u64,
    // 当前已成交量

    pub canceled: bool,
    // 是否已链上取消

    pub bump: u8,
    // order_state PDA bump
}
```

### 7.2 枚举结构附录

```rust
#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum MarketStatus {
    Trading,
    Resolved,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum MarketOutcome {
    Undecided,
    Yes,
    No,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum ResolutionMode {
    Creator,
    Pyth,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum OracleCondition {
    GreaterThan,
    GreaterThanOrEqual,
    LessThan,
    LessThanOrEqual,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum Side {
    Buy,
    Sell,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum Outcome {
    Yes,
    No,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Copy, Debug, PartialEq, Eq, InitSpace)]
pub enum SettleBranch {
    MatchAndMint,
    Transfer,
    MergeAndBurn,
}
```

### 7.3 错误定义结构附录

```rust
#[error_code]
pub enum ErrorCode {
    #[msg("Protocol paused")]
    ProtocolPaused,

    #[msg("Market creation paused")]
    MarketCreatePaused,

    #[msg("Invalid amount")]
    InvalidAmount,

    #[msg("Invalid relayer")]
    InvalidRelayer,

    #[msg("Invalid resolution mode")]
    InvalidResolutionMode,

    #[msg("Invalid resolution authority")]
    InvalidResolutionAuthority,

    #[msg("Invalid oracle feed")]
    InvalidOracleFeed,

    #[msg("Invalid oracle target")]
    InvalidOracleTarget,

    #[msg("Invalid close time")]
    InvalidCloseTime,

    #[msg("Invalid resolve time")]
    InvalidResolveTime,

    #[msg("Invalid claim deadline")]
    InvalidClaimDeadline,

    #[msg("Market not trading")]
    MarketNotTrading,

    #[msg("Market already resolved")]
    MarketAlreadyResolved,

    #[msg("Market not resolved")]
    MarketNotResolved,

    #[msg("Claim deadline passed")]
    ClaimDeadlinePassed,

    #[msg("Insufficient available balance")]
    InsufficientAvailableBalance,

    #[msg("Insufficient yes shares")]
    InsufficientYesShares,

    #[msg("Insufficient no shares")]
    InsufficientNoShares,

    #[msg("Signature verification missing")]
    SignatureVerificationMissing,

    #[msg("Signature verification failed")]
    SignatureVerificationFailed,

    #[msg("Order canceled")]
    OrderCanceled,

    #[msg("Order already canceled")]
    OrderAlreadyCanceled,

    #[msg("Order expired")]
    OrderExpired,

    #[msg("Order overfilled")]
    OrderOverfilled,

    #[msg("Invalid order hash")]
    InvalidOrderHash,

    #[msg("Invalid price")]
    InvalidPrice,

    #[msg("Invalid side")]
    InvalidSide,

    #[msg("Invalid outcome")]
    InvalidOutcome,

    #[msg("Position account not initialized")]
    PositionNotInitialized,

    #[msg("Nothing to claim")]
    NothingToClaim,

    #[msg("Insufficient fee balance")]
    InsufficientFeeBalance,

    #[msg("Cannot close account")]
    CannotCloseAccount,

    #[msg("Math overflow")]
    MathOverflow,
}
```

### 7.4 事件结构附录

```rust
#[event]
pub struct MarketCreated {
    pub market_id: u64,
    pub market: Pubkey,
    pub creator: Pubkey,
    pub resolution_mode: ResolutionMode,
    pub resolution_authority: Pubkey,
    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub creator_fee_bps: u16,
    pub platform_fee_bps: u16,
}

#[event]
pub struct DepositSettled {
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct SplitExecuted {
    pub market: Pubkey,
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct MergeExecuted {
    pub market: Pubkey,
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct MatchSettled {
    pub market: Pubkey,
    pub branch: SettleBranch,
    pub maker: Pubkey,
    pub taker: Pubkey,
    pub fill_amount: u64,
    pub fill_price: u64,
}

#[event]
pub struct OrderCanceled {
    pub market: Pubkey,
    pub user: Pubkey,
    pub nonce: u64,
}

#[event]
pub struct CancelAllBeforeUpdated {
    pub user: Pubkey,
    pub cutoff_ts: i64,
}

#[event]
pub struct MarketResolved {
    pub market: Pubkey,
    pub resolution_mode: ResolutionMode,
    pub outcome: MarketOutcome,
    pub resolved_at: i64,
}

#[event]
pub struct WinningsClaimed {
    pub market: Pubkey,
    pub user: Pubkey,
    pub payout: u64,
    pub outcome: MarketOutcome,
}

#[event]
pub struct Withdrawn {
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct CreatorFeeWithdrawn {
    pub market: Pubkey,
    pub creator: Pubkey,
    pub amount: u64,
}

#[event]
pub struct PlatformFeeWithdrawn {
    pub market: Pubkey,
    pub recipient: Pubkey,
    pub amount: u64,
}

#[event]
pub struct OrderStateClosed {
    pub owner: Pubkey,
    pub market: Pubkey,
    pub nonce: u64,
}

#[event]
pub struct UserPositionClosed {
    pub owner: Pubkey,
    pub market: Pubkey,
}

#[event]
pub struct MarketClosed {
    pub market: Pubkey,
    pub closed_at: i64,
}
```
