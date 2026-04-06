# 0-当前稳定版收口与链上提交改造方向

## 1. 文档目的

本文档不是重新设计整套系统，而是对当前代码已经完成的工作做一次收口，并明确下一阶段链上提交的唯一推进方向。

当前结论以实际代码现状和最近一轮讨论为准：

1. 当前版本已经基本完成充值、创建市场、下单、撮合、资金投影、订单簿/成交推送、主要模块冷启动恢复。
2. 当前主缺口已经集中到 `settlement -> chain submit` 这一段。
3. 下一阶段不再继续大范围改架构，而是围绕“在不改 `market readonly` 的前提下，把单市场链上 TPS 做到最高”推进。

---

## 2. 当前稳定版边界

### 2.1 已完成并可视为本轮稳定版基础的部分

1. `funds`
   - 已补齐事件接线。
   - 已完成恢复路径收口。
   - 运行时以异步投影方式维护外部查询态。

2. `matcher`
   - 已具备按 market actor 串行撮合能力。
   - 已具备 batch flush、checkpoint、恢复收口。
   - 已增加 user position registry 概念，为后续冷热模板做准备。

3. `writer`
   - 已承担数据库/Redis 查询态写入。
   - 已完成与主事件流一致的恢复收口。

4. `pusher`
   - 已改为以市场公共 WS 热推为主。
   - 页面运行中主要依赖 WS 推送，HTTP 负责首屏静态信息与主动刷新。

5. `settlement`
   - 已完成提交队列、market lane、submitted/confirmed/failed 状态持久化。
   - 已完成 watcher/reconciler/recovery 基础收口。
   - 已暴露出当前真实瓶颈：链上交易包体、验签体积、lane 推进节奏。

6. 各类 confirm / projector / worker
   - 启动恢复、轮询补偿、事件补发已收口到统一恢复体系中。

### 2.2 当前稳定版明确不包含的内容

1. 不设计撤单新语义。
2. 不保留 webhook 路线。
3. 不做 `market readonly` 协议级大改。
4. 不做“一个链上 tx 合并多个连续 `match_event`”。
5. 不在本阶段拆成多实例微服务。

---

## 3. 当前链上提交的真实瓶颈

当前系统链上提交失败或 TPS 不高，不是因为撮合慢，也不是因为前后端 WS 慢，而是因为 `settlement` 这一跳仍然偏重。

主要问题集中在四类：

1. 单笔 tx 包体过大
   - 当前 settlement 参数仍然偏胖。
   - ed25519 指令、完整 order intent、账户列表一起叠加后，很容易碰到 Solana `1232 bytes` 限制。

2. 冷热路径没有彻底分离
   - `new order_state` 和 `old order_state` 仍然走得过于接近。
   - `new user_position` 与 `old user_position` 也没有彻底转成模板选择问题。

3. 同一 market 的 lane 推进仍然偏保守
   - 如果继续按“逐笔等待再发下一笔”的思路，Solana 的 slot 优势吃不满。

4. Matcher 的 batch 上限目前仍然主要按 fill 数量控制
   - 真正决定能否上链的，是 `bytes + CU + accounts` 的综合预算，而不是 fill 个数本身。

---

## 4. 已确认不采用的两条路线

### 4.1 不做 `market readonly`

原因：

1. 当前 `market` 在 settlement 中并不只是读取配置，而是承担：
   - `creator_unclaimed_fee`
   - `platform_unclaimed_fee`
   - `total_yes_open_interest`
   - `total_no_open_interest`
   - `total_matched_amount`
2. 若改成只读，需要把市场累计状态、手续费累计、关闭市场前置条件一起重构。
3. 这属于协议级大改，不适合当前“只差链上提交”的阶段。

结论：

- 当前阶段保留 `market mut`。
- 接受“单市场是一条严格顺序 lane”这一现实。

### 4.2 不做“一个 tx 合并多个 `match_event`”

原因：

1. 会显著增加 settlement 聚合器复杂度。
2. 会让 `match_event_id` 与链上提交单元脱钩，增加恢复、回写、排障成本。
3. 当前阶段应优先先把“一条 event 对应一条链上提交”的模型跑稳。

结论：

- 保持 `1 match_event_id = 1 chain submit unit`。
- 先把单 event tx 做轻、做快、做稳。

---

## 5. 下一阶段唯一推进方向

在不改 `market readonly`、不合并多个 `match_event` 的前提下，单市场链上 TPS 要尽量拉高，必须走下面这条路线：

### 5.1 Settlement 走冷热分离

必须明确拆成两类模板：

1. `COLD`
   - 适用于 `order_state` 不确定存在、必须带验签的场景。
   - 单笔 fills 更少，优先保证可落地。

2. `HOT`
   - 适用于 `order_state` 已确认存在、可跳过额外验签的场景。
   - 单笔 fills 更多，优先追求 throughput。

核心原则：

1. 热路径只建立在“已确认存在”的白名单之上。
2. 无法确认存在时，一律退回冷路径。
3. 判断错误时只能性能退化，不能影响正确性。

### 5.2 `user_position` 与 `order_state` 的 existence 只做保守白名单

这里不追求“链上链下强同步”，只追求“安全地选择模板”。

1. `user_position`
   - `(user, market)` 级别维护存在注册表。
   - 由已确认 settlement 结果回写。
   - 重启时从持久层恢复。

2. `order_state`
   - `(user, market, nonce)` 级别维护存在注册表。
   - 仅把已经链上成功出现过的订单加入热路径白名单。
   - 其他订单统一按冷路径处理。

结论：

- 后端不需要实时查询链上每个账户是否存在。
- 后端只维护“可以安全走 HOT 模板的已知集合”。

### 5.3 `init_user_position` 内联

保留此前已达成的一致意见：

1. 不再单独把 `init_user_position` 作为独立链上提交策略去放大复杂度。
2. 应当内联到 settlement 主指令。
3. `new user_position` 不再成为是否能上链的主瓶颈。

### 5.4 交易结构必须继续压缩

必须继续做：

1. `compact witness`
2. 更短的签名消息表示
3. `v0 + ALT`
4. `fill` / `order arg` 的字段压缩

目标不是“理论上好看”，而是：

1. 降低 raw tx bytes
2. 提高单 tx 可容纳 fills 数
3. 降低 miss slot 概率

### 5.5 Matcher 上限改成按真实提交预算驱动

后续 matcher / settlement 之间必须统一预算口径：

1. `max bytes`
2. `max compute`
3. `max accounts`
4. `hot/cold lane` 对应不同 fill 上限

不能继续只按“固定 fill 数”理解 batch 大小。

---

## 6. 单市场 TPS 目标判断

在以下前提下：

1. 不做 `market readonly`
2. 不做多 `match_event` 合并
3. 做冷热分离
4. 做结构压缩
5. 做 `v0 + ALT`
6. 使用更激进的 Solana 提交流程

当前对单市场链上 TPS 的粗略目标区间判断为：

1. 保守可达：
   - `30-50 fills/s`

2. 目标区间：
   - `50-100 fills/s`

3. 若不做冷热分离、仍然逐笔保守推进：
   - 很难达到上述区间

这里的口径统一为：

- `fills/s`
- 不是 `orders/s`
- 不是前端热推显示速度
- 不是链下 matcher 吞吐

---

## 7. 当前阶段的工程原则

为了尽快形成第一版稳定版本，后续修改必须遵守：

1. 优先收口，不重新发散架构。
2. 优先保证正确恢复、正确提交、正确失败处理。
3. 不为了追求理论极限 TPS 引入新的协议级重构。
4. 单市场先把 HOT/COLD 跑通，多市场并行能力后续在此基础上再放大。

---

## 8. 本文档对应的版本定位

本文档对应的版本定位是：

1. 当前代码已形成第一版“主链路基本完成”的稳定基线。
2. 下一阶段的唯一主任务是链上提交优化与落地。
3. 后续 `spec/modify` 文档将只继续记录围绕链上提交与 TPS 优化的修改，不再重新推翻当前主架构。
