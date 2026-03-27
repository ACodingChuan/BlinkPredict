# 10 - Frontend 状态模型

## 目标

让前端正确表达新架构下的三层状态：
- open
- pending settlement
- confirmed

## 需要展示的核心对象

### 钱包状态
- confirmed collateral
- reserved open collateral
- reserved pending collateral
- available collateral

### 订单状态
- accepted
- live
- partially_filled
- filled
- cancelled
- expired
- rejected

### 结算状态
- none
- pending
- submitted
- confirmed
- failed

### 仓位状态
- free lots
- reserved open lots
- pending settlement lots

## 前端接入点

优先调整：
- `useTrading.ts`
- `useUSDCBalance.ts`
- `user-open-orders.tsx`
- `user-positions.tsx`
- `orderbook.tsx`
- `recent-trades.tsx`

## 数据来源

- 实时：Pusher websocket
- 查询：Redis read model 对应 API

## 当前结论

- 前端不能再只展示单一“余额”与单一“仓位”
- 必须分层展示 open / pending / confirmed
