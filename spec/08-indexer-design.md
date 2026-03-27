# 08 - Indexer 设计

## 目标

Indexer 负责把链上真实状态同步回系统。

## 职责

- 监听 collateral 账户变化
- 监听 YES / NO token 账户变化
- 监听 delegate / authority 变化
- 监听 settlement 链上确认结果
- 在链上真实状态不足以支撑 live orders 时触发 invalidation

## 输出

Indexer 输出：
- `wallet_asset_balance_update`
- `settlement_confirmed`
- `settlement_failed`
- `order_invalidation_required`
- `chain_observation_record`

## 持久化

- `wallet_asset_balances`
- `chain_observations`
- `order_invalidations`

## 对 Wallet Shard 的影响

Indexer 不直接改订单簿。

它只做：
- 更新钱包确认态余额
- 冻结相关钱包
- 触发 invalidation 命令

## 当前结论

- Indexer 是链上真相回补模块
- 不参与热路径
