# 09 - Executor 与 Contract 设计

## 目标

定义链上 settlement 的执行模块与合约配合方式。

## Executor 职责

- 消费成交结果
- 创建 `settlement_tasks`
- 构造 Solana 交易
- 提交链上 settlement
- 发布 success / failure 结果

## settlement task 生命周期

- `pending`
- `submitting`
- `submitted`
- `confirmed`
- `failed_retryable`
- `failed_terminal`

## Contract 目标

合约需要支持：
- settlement 指令
- 用户控制账户模型
- 与 outcome token / collateral 的一致结算规则
- 与 split / merge / redeem 的后续兼容

## 边界

- Executor 不决定订单是否能下
- Wallet Shard 不负责链上提交
- Indexer 负责最终链上结果回补

## 当前结论

- settlement 完全异步
- 合约设计应服务于 executor 批量与重试策略
