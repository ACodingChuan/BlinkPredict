# 单市场 Devnet 最快发送方案（不改 Matcher）

## 0. 范围与约束

本方案只讨论你明确给出的边界：

1. 不改 `matcher`
2. 继续 `1 match_event_id = 1 on-chain tx`
3. 同一市场链上提交严格串行
4. 环境是 `devnet + 免费 RPC`
5. 不做 TPU，不做双 RPC 扇出
6. `blockhash manager` 只需要每 `15s` 轮询一次
7. 不做高频兜底轮询（尤其不做每秒级 per-tx polling）

本方案目标不是“理论极限吞吐”，而是在上述约束下把单市场发送节奏压到当前可实现的最快，并保证重启可恢复。

---

## 1. 目标与非目标

### 1.1 目标

1. 让单市场节拍从 `submitted -> confirmed` 改为 `submitted -> processed`
2. 把 `processed` 到下一笔发送的本地开销压到 `<=100ms` 量级
3. 保持重启后状态可恢复、事件可补发、不会卡死 lane
4. 不改变 matcher 的产出结构与分批逻辑

### 1.2 非目标

1. 不承诺在免费 devnet RPC 下严格稳定 `1 slot/tx`（外部网络抖动不可控）
2. 不在本期解决多市场 fee payer 冲突（多 relayer 分片后续再做）
3. 不在本期引入 TPU / RPC 扇出 / Jito

---

## 2. 总体架构（改造后）

核心是把 settlement 变成“两段推进”：

1. 快路径：`queued -> prepared -> submitted -> processed`
2. 慢路径：`processed -> confirmed/failed`

并且把市场 lane 的阻塞规则改成：

1. `submitted` 会阻塞
2. `processed` 不阻塞

这样同一市场可以出现：

1. `0 or 1` 条 `submitted`（唯一阻塞头）
2. `0..N` 条 `processed`（后台等终态）

---

## 3. 数据模型与表结构改造

当前 `settlement_submissions.status` 只有：

1. `queued`
2. `submitted`
3. `confirmed`
4. `failed`

本方案新增：

1. `prepared`
2. `processed`

建议字段增量：

1. `prepared_payload JSONB NOT NULL DEFAULT '{}'::jsonb`
2. `prepared_at TIMESTAMPTZ`
3. `processed_slot BIGINT NOT NULL DEFAULT 0`
4. `processed_at TIMESTAMPTZ`
5. `confirmed_at TIMESTAMPTZ`

建议索引增量：

1. `(market_id, created_at)` partial index for `status='prepared' AND market_lane_status='active'`
2. `(updated_at)` partial index for `status='processed'`
3. `(updated_at)` partial index for `status='submitted'`（保留）

说明：

1. `prepared_payload` 只存“可重建发送”的紧凑材料，不存 blockhash，不存签名
2. `processed_*` 只服务快路径放行和恢复，不对外发布新事件

---

## 4. 状态机与 CAS 迁移规则

## 4.1 主状态迁移

1. `queued -> prepared`
2. `prepared -> submitted`
3. `submitted -> processed`
4. `processed -> confirmed`
5. `submitted|processed -> failed`

## 4.2 CAS 约束（必须）

1. `prepared -> submitted` 必须 CAS 当前 `status='prepared'` 且 lane active
2. `submitted -> processed` 必须 CAS：
   - `status='submitted'`
   - `tx_signature == 当前监听签名`
3. `processed -> confirmed/failed` 必须 CAS `status='processed'`
4. 重签替换只允许在 `status='submitted'` 且 `old_signature` 匹配时发生

这样可以避免：

1. 旧签名晚到通知覆盖新签名状态
2. 并发协程重复推进同一条记录
3. 重启恢复时出现状态回退

---

## 5. 运行组件拆分

## 5.1 Ingress（保持）

`NATS evt.match.execution.* -> DB queued`

规则不变：

1. 先落库 `queued`
2. 成功后再 Ack
3. 失败 Nak

## 5.2 Prepare Worker（新增）

输入：`queued`  
输出：`prepared`

执行内容：

1. 反序列化 `match_event_json`
2. 构建 `SubmissionBatch`（含 warm/cold 判定）
3. 生成可复用 `prepared_payload`：
   - 编码后的 settle args
   - account metas 顺序
   - 需要的 ed25519 指令数据
   - compute budget 参数
4. CAS 更新为 `prepared` + `prepared_at`

关键点：

1. 把重 CPU/重序列化工作前移
2. 真正发送时只做：取 blockhash + 签名 + 发链

## 5.3 Market Actor（改造）

每个市场一个顺序 actor，职责：

1. 若无 `submitted` 阻塞头，则取最早 `prepared`
2. 快速签名并提交 `prepared -> submitted`
3. 广播 raw tx
4. 挂接 processed 观察

调度兜底扫描可保留，但只做低成本补偿，不参与主节拍。

## 5.4 Blockhash Manager（按你的约束）

策略：

1. 固定 `15s` 拉一次 `GetLatestBlockhash(processed)`
2. 缓存：
   - `blockhash`
   - `last_valid_block_height`
   - `fetched_at`
3. 若签名前发现缓存空值，则同步拉一次补齐
4. 若发送失败提示 blockhash 过旧，再触发一次按需刷新并重签

说明：

1. 不做高频刷新
2. 不做 per-tx 获取
3. 在 devnet 足够覆盖常规生命周期

## 5.5 Processed Watcher（快路径）

快路径只做一件事：尽快把 `submitted` 推到 `processed`。

信号源：

1. `wsrouter signatureSubscribe(processed)` 为主
2. 不启用高频轮询兜底

触发后动作顺序：

1. CAS `submitted -> processed`，写 `processed_slot/processed_at`
2. 更新 registry（order_state + user_position observed）
3. 释放 market lane
4. 立即唤醒该市场 actor 发送下一条 `prepared`

## 5.6 Terminal Poller（慢路径）

低频批量轮询 `status='processed'`，推进到 `confirmed/failed`：

1. `confirmed/finalized & no err` -> `confirmed`
2. terminal err -> `failed + pause market`
3. 其余继续等待

这里可以保守低频，例如 `10~15s` 周期，因为它不影响下一笔发送。

---

## 6. 最快发送主链路（单市场）

单条 `match_event` 时间线：

1. ingress 落 `queued`
2. prepare worker 生成 `prepared_payload`，落 `prepared`
3. 市场 actor 检查无 `submitted` 阻塞头
4. 读取 blockhash cache
5. 用 relayer 快速签名
6. CAS `prepared -> submitted`（写 `tx_signature/raw_tx/last_valid_block_height`）
7. 立即 `SendEncodedTransactionWithOpts`
8. `wsrouter` 收到 processed 通知
9. CAS `submitted -> processed`
10. 更新 registry
11. release lane
12. actor 立刻发送下一条 `prepared`
13. terminal poller 后台推进 `processed -> confirmed/failed`

---

## 7. 发送参数建议（本期）

以“快路径最短时延”为目标：

1. `send_commitment = processed`
2. `preflight_commitment = processed`
3. `skip_preflight = true`（仅 fast lane；若你要更稳可配开关）
4. `blockhash_poll_interval = 15s`
5. `terminal_poll_interval = 10~15s`
6. `scheduler_fallback_scan = 200~500ms`
7. `reconcile_interval = 10s`（保留）

说明：

1. 快路径尽量不等待 preflight
2. 错误检测依赖链上状态与 terminal 收口

---

## 8. 重启恢复方案（必须保证）

启动恢复分两类：

1. `submitted`
2. `processed`

恢复流程：

1. 先加载所有 `submitted`
2. 批量查签名状态
3. 已 processed 的先补写 `processed`，更新 registry，解除 lane 阻塞
4. 仍未 processed 且未过期的，恢复 ws 观察
5. 已过期的做重签替换
6. `processed` 全部送入 terminal poller，不阻塞 lane
7. 最后重建 dirty market 集合

阻塞重建规则：

1. 市场存在 `submitted` 才阻塞
2. 仅有 `processed` 不阻塞

---

## 9. 静态时间估算（本方案）

在“预构建 + 单 RPC + WS processed”下，本地可控延迟：

1. `processed 通知 -> 下一笔发出`：`40~110ms`
2. `签名 + CAS + send`：`25~70ms`

外部不可控延迟（免费 devnet RPC）：

1. `send -> observed processed`：常见 `250~650ms`

所以单市场周期大致：

1. 常见 `290~760ms`
2. 接近 `1 slot` 只会发生在较好网络窗口

结论：

1. 本方案是你当前约束下“最快可落地”的发送路径
2. 能显著快于 `confirmed gate` 方案
3. 不能承诺严格稳定 `1 slot/tx`

---

## 10. 实施分解（建议）

### 第 1 步：数据层

1. 扩展 `status` 枚举（加 `prepared` / `processed`）
2. 增加 `prepared_payload/prepared_at/processed_slot/processed_at/confirmed_at`
3. 增加 partial indexes

### 第 2 步：repo 层

1. 新增 `MarkPreparedCAS`
2. 新增 `LoadNextPreparedByMarket`
3. 新增 `MarkProcessedCAS`
4. 新增 `ListProcessed`
5. 调整 `MarkConfirmedCAS` 从 `processed` 推进

### 第 3 步：service 层

1. 增加 prepare worker
2. 改 scheduler/actor 从 `prepared` 取任务
3. watchSubmission 从“等 confirmed”改成“等 processed”
4. confirmed 迁移交给 terminal poller

### 第 4 步：chainconfirm/ws

1. `wsrouter` 支持 `signatureSubscribe` 传入 commitment（至少 `processed`/`confirmed`）
2. settlement 快路径统一订阅 `processed`

### 第 5 步：恢复与回归

1. 补全 submitted/processed 双集合恢复
2. 验证崩溃点：
   - 已广播未落库（应被消除）
   - 已 processed 未写库（启动补写）
   - 重签后旧通知晚到（CAS 不应误推进）

---

## 11. 对你三个问题的明确回答

### 11.1 这次改动和已完成 Phase 1/2 是否冲突？

明确结论：**不冲突**。

原因：

1. Phase 1/2 解决的是“单 tx 数据结构压缩与 builder 语义”
2. 本方案解决的是“发送状态机与推进门槛（processed gate）”
3. 两者是正交关系，Phase 1/2 产物会直接提高本方案效果

### 11.2 这次是否包含 Phase 4？后面还要不要做？

明确结论：**本方案不包含完整 Phase 4，后面仍建议做 Phase 4**。

本方案只覆盖：

1. Phase 3 主体（processed gate + 快慢路径拆分 + blockhash 管理）
2. 少量 Phase 4 前置（registry 在 processed 时及时更新）

Phase 4 仍有价值的部分：

1. 更完整的 `user_position/order_state registry` 热路径利用
2. matcher/settlement estimator 的统一（虽然你本期不改 matcher，后续仍可做）
3. v0/ALT 进一步压缩消息与账户开销

### 11.3 WS 订阅是复用公共 wsrouter，还是在 settlement 单独实现？

明确结论：**复用公共 `wsrouter`，只做小改造支持 processed commitment**。

理由：

1. 现有 `wsrouter` 已有重连、分片、订阅管理，稳定性更高
2. 单独在 settlement 再造一套 WS 连接，维护成本更高，且运行时速度收益极小
3. 本期目标是“最快落地且可恢复”，复用更稳、更快交付

只需改造点：

1. `signatureSubscribe` 的 commitment 参数从硬编码 `confirmed` 改为可配置
2. 通知结果里回传真实 commitment（至少区分 processed/confirmed）

