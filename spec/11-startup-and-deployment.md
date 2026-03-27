# 11 - 启动与部署

## 目标

定义运行形态与部署建议。

## 建议方案

- 单仓库
- 单 Go 二进制
- 多 mode 启动
- 生产拆成多个进程

## 推荐 mode

- `api`
- `matcher`
- `writer`
- `pusher`
- `executor`
- `indexer`
- `all`

## 环境建议

### 本地开发
- 可使用 `all`
- 快速联调前后端与合约

### 集成测试
- 建议拆成至少：
  - `matcher`
  - `writer`
  - `pusher`
  - `indexer`

### 生产
- 不建议单进程跑全部
- 至少拆出：
  - `api`
  - `matcher`
  - `writer`
  - `pusher`
  - `executor`
  - `indexer`

## 当前结论

- 代码保持单仓库最合适
- 运行时必须模块化
- 1w TPS 热路径要求 matcher 与 pusher 不能被 writer / executor / indexer 拖慢
