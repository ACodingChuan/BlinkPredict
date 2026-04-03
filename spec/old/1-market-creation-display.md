# Market Creation & Display Development Design v4

范围：

- 创建市场
- 市场元数据存储
- 市场列表 / 详情展示
- Creator / Pyth 双模式的前后端与合约协作
- Webhook -> NATS -> Consumer 的异步索引链路
- 前端、后端、数据库、合约、webhook 的逐文件改造清单

本文件是这一部分的唯一设计依据。

当前固定结论：

- 链上存 `metadata_cid`
- `metadata_cid` 定长为 `96 bytes`
- 前端继续采用“先传图片，再把图片 URI 写入 metadata JSON，再上传 metadata JSON”的方式
- 创建市场成功后，不引入前端主动同步写库模块
- 市场索引完全依赖：`Webhook -> NATS -> Consumer -> DB/Redis`
- Webhook 接口职责收敛为：
  - 验证来源
  - 识别事件类型
  - 写入 NATS
  - 立即返回 200
- 单独设计消费 NATS 的模块做业务处理
- `creator_fee_bps` / `platform_fee_bps` 放在 `GlobalConfig`
- market create 不再单独传 fee bps

---

## 0. 本阶段开发结论

这一阶段的完整链路如下：

1. 前端上传图片到 IPFS
2. 前端组装 metadata JSON，并把图片 URI 写入 JSON
3. 前端上传 metadata JSON 到 IPFS，得到 `metadata_cid`
4. 前端发起链上 `create_market`
5. 链上发出 `MarketCreated` 事件，事件中带上 `metadata_cid`
6. Webhook 收到链上事件
7. Webhook 仅做：
   - 验证
   - 识别事件类型
   - 投递到 NATS
   - 返回 200
8. 独立的 NATS consumer 消费事件
9. Consumer 通过 `metadata_cid` 拉取 IPFS metadata
10. Consumer 写 PostgreSQL / Redis
11. 前端市场列表页与详情页从后端缓存层读取展示数据

这里的职责边界必须明确：

- 前端：创建内容 + 发起交易
- 合约：保存最小可信字段 + 发事件
- webhook：事件入口适配层，不做重业务
- NATS consumer：真正的索引和落库处理层
- 后端 API：提供展示读取接口

---

## 1. 前端开发设计

### 1.1 前端目标

前端需要完成：

- 创建 Creator / Pyth 两类市场
- 上传封面图到 IPFS
- 组装 metadata JSON 并上传到 IPFS
- 计算并传递 `metadata_cid`
- 发起链上 `create_market` 交易
- 市场列表页与详情页完整展示市场信息

前端不负责：

- 创建成功后同步写数据库
- 直接推动市场缓存更新

### 1.2 前端现有代码可复用部分

主要文件：

- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/app/markets/create/page.tsx`

当前可复用：

- `pinImageDataUrl`
- `pinJSONToIPFS`
- `buildMetadata`
- Hermes feed 校验逻辑
- Creator / Pyth 模式 UI

必须修改：

- `computeMarketId` 的输入从 `metadataUri` 改为 `metadataCid`
- 创建市场表单新增 `claim_deadline_time`
- 交易参数不再包含 market 级 fee bps
- 合约调用从旧版 `metadataUri` 切到 `metadataCid[96]`

### 1.3 前端表单字段设计

创建市场页必须包含：

#### 通用字段

- `title`
- `description`
- `image`
- `close_time`
- `resolve_after_time`
- `claim_deadline_time`

#### Creator 模式字段

- `resolution_authority`

#### Pyth 模式字段

- `oracle_feed_id`
- `oracle_condition`
- `oracle_target_price`

#### 不再由前端输入的字段

- `creator_fee_bps`
- `platform_fee_bps`

原因：

- 这两个值改为由 `GlobalConfig` 统一控制

### 1.4 前端 metadata JSON 设计

继续保留当前模式：

1. 图片上传到 IPFS，得到图片 CID
2. 图片字段写入 metadata JSON：`ipfs://<image_cid>`
3. metadata JSON 上传到 IPFS，得到 metadata CID
4. 链上只传 CID

建议 metadata JSON 结构：

```ts
type MetadataPayload = {
  title: string;
  description: string;
  image?: string;
  close_time: string;
  resolve_after_time: string;
  claim_deadline_time: string;
  resolution:
    | {
        mode: "creator";
        authority: string;
      }
    | {
        mode: "pyth";
        oracle_feed_id: string;
        oracle_condition: "gte" | "gt" | "lt" | "lte";
        oracle_target_price: string;
      };
  version: string;
};
```

### 1.5 前端 CID / URI 处理规则

固定规则：

- 链上只传 `metadata_cid`
- 前端自己派生：
  - `metadata_uri = ipfs://<metadata_cid>`
- 图片也统一写成：
  - `ipfs://<image_cid>`

### 1.6 前端 create_market 交易入参设计

```ts
type CreateMarketArgs = {
  marketId: bigint;
  metadataCid: string;
  closeTime: bigint;
  resolveAfterTime: bigint;
  claimDeadlineTime: bigint;
  resolutionMode: number;
  resolutionAuthority: string;
  oracleFeedId: Uint8Array;
  oracleCondition: number;
  oracleTargetPriceInt: bigint;
  oracleTargetExpo: number;
};
```

说明：

- `metadataCid` 在前端需要编码成固定 96 bytes
- 不再传 `creator_fee_bps` / `platform_fee_bps`

### 1.7 前端 market id 生成规则

应修改为：

```ts
async function computeMarketId(input: {
  creator: string;
  title: string;
  closeTime: string;
  metadataCid: string;
}): Promise<bigint>
```

不再依赖 `metadataUri`。

### 1.8 前端创建成功后的行为

创建市场交易确认后：

- 前端只展示成功状态和交易签名
- 前端不再调用后端同步写库接口
- 市场何时出现在列表页，取决于 webhook -> NATS -> consumer 的异步处理完成

### 1.9 前端列表页与详情页展示新增项

市场列表页新增：

- `Last claim time`
- `Creator / Pyth` 标签

市场详情页新增：

- `Last claim time`
- `Resolution authority`（Creator）
- `Feed id / Oracle condition / Target price`（Pyth）
- `Metadata CID`（建议开发者视图显示）

### 1.10 前端逐文件改造清单

#### 文件 1
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/app/markets/create/page.tsx`

改动：

- 新增 `claim_deadline_time` 表单字段
- `buildMetadata` 增加 `claim_deadline_time`
- `computeMarketId` 改为依赖 `metadataCid`
- 交易指令编码从 `metadataUri` 改为 `metadataCid[96]`
- 移除 market create 中 fee bps 输入和传参
- 成功后不再调用后端同步写库接口

#### 文件 2
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/types/market.ts`

改动：

- 新增 `metadata_cid?: string`
- 新增 `claim_deadline_time?: string`
- 保留 `metadata_url?: string` 作为派生展示字段

#### 文件 3
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/app/page.tsx`

改动：

- 市场卡片增加 `claim_deadline_time` 展示
- 增加 Creator / Pyth 可视标签

#### 文件 4
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Frontened/app/markets/[id]/page.tsx`

改动：

- 增加 `claim_deadline_time` 展示
- 增加 `metadata_cid` 开发者视图展示
- 完善 Creator / Pyth 规则说明

---

## 2. 后端开发设计

### 2.1 后端目标

后端在这一阶段是：

- 市场展示缓存层
- 链上事件索引层
- 高性能列表页 / 详情页数据提供方

后端不是唯一数据真源。

真实恢复路径应当是：

- 扫链上事件
- 通过 `metadata_cid` 去 IPFS 拉数据
- 重建 PostgreSQL / Redis

### 2.2 后端数据库字段设计建议

当前 `markets` 表和 `markets.Market` 模型应扩充/调整。

建议结构：

```go
type Market struct {
    ID                string
    MarketID          uint64
    MarketPDA         string

    MetadataCID       string
    MetadataURL       string

    Title             string
    Description       string
    ImageURL          string

    Status            MarketStatus
    Outcome           MarketOutcome
    Resolution        ResolutionConfig

    CloseTime         time.Time
    ClaimDeadlineTime time.Time
    ResolvedAt        *time.Time

    CreatedAt         time.Time
    UpdatedAt         time.Time
}
```

### 2.3 后端读取接口设计

市场展示接口继续保留：

- `GET /api/markets`
- `GET /api/markets/{marketId}`

但返回字段要补：

- `metadata_cid`
- `claim_deadline_time`

不再依赖前端主动同步创建结果，后端只读数据库缓存。

### 2.4 Webhook 设计改造

新设计下 webhook 只负责：

- 验证来源
- 识别事件类型
- 投递到 NATS
- 返回 200

Webhook 不负责：

- 拉 IPFS
- 写 PostgreSQL
- 写 Redis
- 复杂业务逻辑处理

### 2.5 NATS Consumer 设计

新增独立 consumer 模块，负责：

- 消费 MarketCreated 事件
- 从事件中读出 `metadata_cid`
- 构造 `ipfs://<cid>` / gateway URL
- 拉 metadata JSON
- 解析 title/description/image
- 写入 PostgreSQL
- 写入 Redis cache

这部分才是新的“索引处理层”。

### 2.6 Webhook 事件总线建议

建议 NATS topic 设计为类似：

- `webhook.market.created`
- `webhook.market.resolved`
- `webhook.market.updated`

市场创建阶段本阶段必须实现的是：

- `webhook.market.created`

### 2.7 后端逐文件改造清单

#### 文件 1
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/markets/types.go`

改动：

- 新增 `MetadataCID string`
- 新增 `ClaimDeadlineTime time.Time`
- 保留 `MetadataURL string` 作为派生/缓存字段
- 弱化旧的 `CollateralVault` / `YesMint` / `NoMint` 在展示域中的重要性

#### 文件 2
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/db/schema.sql`

改动：

- `markets` 表新增：
  - `metadata_cid TEXT`
  - `claim_deadline_time TIMESTAMPTZ`
- 视情况保留 `metadata_url`

#### 文件 3
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/markets/postgres_store.go`

改动：

- 插入/读取/更新 SQL 补齐 `metadata_cid`
- 插入/读取/更新 SQL 补齐 `claim_deadline_time`

#### 文件 4
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/cache/market_cache.go`

改动：

- cache 结构新增 `metadata_cid`
- cache 结构新增 `claim_deadline_time`
- Redis 编码/解码逻辑同步更新

#### 文件 5
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/webhooks/alchemy.go`

改动：

- 不再直接拉 IPFS 并写 DB
- 改成：
  - 解析 MarketCreated 事件
  - 识别类型
  - 投递 NATS
  - 返回 200

#### 文件 6
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/webhooks/types.go`

改动：

- 事件结构从 `metadata_uri` 切到 `metadata_cid`
- 增加 `claim_deadline_ts`

#### 文件 7
- 新增 NATS consumer 模块，例如：
  - `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/internal/marketindexer/consumer.go`

功能：

- 消费 `webhook.market.created`
- 拉 IPFS metadata
- 写 PostgreSQL
- 写 Redis

#### 文件 8
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Banckend/cmd/api/main.go`

改动：

- 注入 webhook -> NATS publisher
- 启动 market created consumer

---

## 3. 合约开发设计

### 3.1 合约目标

合约在“创建市场 / 展示生态”阶段的目标是：

- 保存市场最小可信信息
- 让任意索引器都能恢复市场 metadata
- 为 Creator / Pyth 模式提供明确链上规则锚点

因此合约必须保存：

- `metadata_cid`
- `close_ts`
- `resolve_after_ts`
- `claim_deadline_ts`
- Creator/Pyth 规则参数

### 3.2 `GlobalConfig` 字段调整

你已经要求：

- `creator_fee_bps`
- `platform_fee_bps`

放在全局 config 账户里。

因此 `GlobalConfig` 应增加：

```rust
pub creator_fee_bps: u16,
pub platform_fee_bps: u16,
```

### 3.3 合约 `MarketState` 字段建议（本阶段相关部分）

创建市场与展示生态相关字段建议最终至少有：

```rust
#[account]
pub struct MarketState {
    pub market_id: u64,
    pub creator: Pubkey,
    pub resolution_authority: Pubkey,
    pub status: MarketStatus,
    pub outcome: MarketOutcome,
    pub resolution_mode: ResolutionMode,

    pub metadata_cid: [u8; 96],

    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub resolved_at: i64,

    pub oracle_feed_id: [u8; 32],
    pub oracle_condition: OracleCondition,
    pub oracle_target_price_int: u64,
    pub oracle_target_expo: i32,

    pub bump: u8,
}
```

### 3.4 合约 `create_market` 指令设计（本阶段相关）

#### 参数设计

```rust
pub struct CreateMarketArgs {
    pub market_id: u64,
    pub metadata_cid: [u8; 96],
    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub resolution_mode: ResolutionMode,
    pub resolution_authority: Pubkey,
    pub oracle_feed_id: [u8; 32],
    pub oracle_condition: OracleCondition,
    pub oracle_target_price_int: u64,
    pub oracle_target_expo: i32,
}
```

#### Anchor 账户体系规则

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

#### 核心逻辑设计

- `close_ts > now`
- `resolve_after_ts >= close_ts`
- `claim_deadline_ts > resolve_after_ts`
- Creator 模式下：
  - `resolution_authority` 必须有效
- Pyth 模式下：
  - `oracle_feed_id != 0`
  - `oracle_target_price_int > 0`
- 读取 `GlobalConfig.creator_fee_bps`
- 读取 `GlobalConfig.platform_fee_bps`
- 保存 `metadata_cid`
- 初始化市场状态

### 3.5 合约事件设计（本阶段相关）

为了支持展示生态，创建市场事件建议为：

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
    pub metadata_cid: [u8; 96],
}
```

这样：

- webhook 无需依赖中心化 API
- 只靠链上事件就能恢复 metadata 地址

### 3.6 合约逐文件改造清单

#### 文件 1
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Contract/programs/predix-program/src/state/market.rs`

改动：

- 增加 `claim_deadline_ts`
- `metadata_uri` 改为 `metadata_cid: [u8; 96]`
- 移除旧的 yes/no mint / collateral vault 依赖描述

#### 文件 2
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Contract/programs/predix-program/src/lib.rs`

改动：

- `initialize_market` / `create_market` 参数改为接收 `metadata_cid`
- 增加 `claim_deadline_ts`
- 创建市场时从 `GlobalConfig` 读取 fee bps

#### 文件 3
- `/Users/guohaochuan/Documents/web3project/BlinkPredict/Contract/programs/predix-program/src/state/config.rs`（如果尚未独立）

改动：

- 新增：
  - `creator_fee_bps`
  - `platform_fee_bps`

#### 文件 4
- 合约事件定义文件

改动：

- `MarketCreated` 事件改为抛出 `metadata_cid`
- 增加 `claim_deadline_ts`

---

## 4. 最终结论

这部分专项开发的正式方案现在可以固定为：

- 合约链上存 `metadata_cid[96]`
- 前端继续上传图片和 metadata 到 IPFS
- 前端和后端根据 CID 派生 URI
- 前端不再主动推动数据库写入作为主链路，而是等待 webhook -> NATS -> consumer 推进
- Webhook 只做轻量识别和投递
- Consumer 才是索引落库模块
- `creator_fee_bps / platform_fee_bps` 统一放在 `GlobalConfig`

---

## 5. 下一步建议

下一步最合适直接做的是：

- 基于这份文档继续写“逐文件改造清单 v2”，把每个文件级别的改动进一步细化成字段、函数、接口、SQL 变更明细

如果你要，我下一步可以直接继续输出：

- 前端逐文件改造清单 v2
- 后端逐文件改造清单 v2
- 合约逐文件改造清单 v2
