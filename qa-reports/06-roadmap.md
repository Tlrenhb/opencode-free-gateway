# 06 · 功能体检与路线图调研报告

> 对象：`ocfreelay-go`（OpenCode 免费模型网关，Go / 零依赖）
> 范围：`internal/` 全部源码 + 管理 UI（`internal/adminui/static/`）+ 同类项目调研
> 日期：2026-08-10

---

## 第一部分 · 鸡肋排查（10 项）

### A. 死配置 / 死代码 / 假兼容

**1. `freeModelsFilter` —— UI 开关 + settings 字段 + API 字段，但零实现** 🔴
- 证据：`internal/config/config.go:71` 定义字段；`internal/server/adminapi.go:162/173/198` 读写；`internal/adminui/static/app.js:416/430` 复选框"`/v1/models 仅返回免费模型`"。全仓库 grep：**relayproxy 从不读取**，`/v1/models` 原样透传，付费/已下架模型照样返回。
- 讽刺的是：原版 [OCFreeRelay](https://github.com/kirafishy/OCFreeRelay) 的**头号卖点**正是 free-only serving（启动时爬 Zen 定价页，只暴露免费模型，`/v1/chat/completions` 对非免费模型直接 403 `model_not_allowed`，不做上游调用）。Go 重写把它做成了一个没有任何效果的开关。
- 建议：**实现它**（见新功能 #1），而不是删掉。

**2. `server.LoadPoolFromConfig` —— 从未被调用的死方法** 🟡
- 证据：`internal/server/adminapi.go:495`，全仓库无调用点（`cmd/relay/main.go` 用的是 `cmd/relay/poolimport.go` 的同名函数）。实现内还留着 `_ = added; _ = ids; _ = ids2` 的残废占位，且不恢复 `Enabled/Usable/Source` 三标志。
- 建议：删除。留着只会误导后来人。

**3. `formatHostPort` / `joinNonEmpty` —— 死助手函数** 🟡
- 证据：`internal/server/adminpool.go:58-70`，无任何调用点。
- 建议：删除。

**4. `LegacyAccounts`（`accounts` 字段）—— 假兼容** 🟡
- 证据：`internal/config/config.go:73-74` 注释声称"kept for imports"，但全仓库没有任何代码消费它。TS→Go 迁移时旧 `accounts` 数组会被**静默丢弃**，字段只是自欺。
- 建议：删除，或真正实现 accounts→Workers 导入逻辑。

### B. 失真统计（面板在展示假数据）

**5. worker 级"缓存率"列永远显示"—"** 🔴
- 证据：`internal/stats/stats.go:48` `rate *float64` 字段**从未被赋值**（只有 `Rate()` 方法计算，但 JSON 序列化用的是裸字段）；`ForAccounts` 返回的副本里 `rate` 恒为 nil → 概览页和用量页的"缓存率"列永远渲染"—"。
- 建议：要么在 `ForAccounts` 里填充 `Rate()` 结果，要么删掉该列（总体缓存率 `Overall.CacheRate` 是好的）。

**6. 流式响应完全不统计 token —— 用量页大部分时间全是 0** 🔴
- 证据：`internal/server/server.go` 的 `extractUsage` 只在**非流式 JSON 响应**（`!isStreaming && content-type: application/json`）时解析 usage。chat 默认就是 SSE 流式 → prompt/completion/cache token 永远不计，token 图表、总计、缓存率全部失真。
- 建议：SSE 流结束时解析末尾 `data:` usage 块（见新功能 #3），或至少在 UI 标注"仅统计非流式"。

### C. 反直觉行为（用户会被坑）

**7. 重启后 worker↔代理绑定全部失效（真 bug，最坑）** 🔴
- 证据：`cmd/relay/poolimport.go:17-29` 每次启动对池条目调用 `pm.Import()` → `pool.go` 的 `Import` 用 `newID("px")` **重新生成随机 ID**；而 worker 的 `proxyId` 持久化的是旧 ID（`data/settings.json` 中可见）。重启后 `FindPoolProxy` 查不到 → 所有绑定**静默退回直连**。
- 建议：加载时沿用 settings.json 里的池条目 ID（P0 修复，见新功能 #2）。

**8. 新导入代理 `Usable=false` → 绑定的 worker 静默直连；且"探活"只是 TCP 连通性** 🟡
- 证据：`internal/relayproxy/relay.go` `resolveWorkerProxy` 要求 `pp.Enabled && pp.Usable` 才走代理；导入后 `Usable` 默认 false，不点"全部探活"就永远直连（用户以为走了代理，实际裸奔）。而 `pool.Probe` 只做 `net.DialTimeout` TCP 握手——TCP 通但拒绝 CONNECT/鉴权失败的代理会被标记为可用。
- 建议：导入后自动探活；探活升级为真实 HTTP CONNECT / SOCKS5 握手。

**9. 全部 worker 封禁/冷却时仍继续打上游** 🟡
- 证据：`internal/rotator/rotator.go` `pickLocked` fallback #4 在所有 worker 不可用时仍返回 cursor worker，`relay.Forward` 会照发请求（最多 3 次）——对刚 24h 封禁的账号继续打，可能加重封禁、浪费配额。
- 建议：全不可用时直接短路，给客户端 429/503 + 状态说明。

**10. 端口字段"重启生效"但无任何提示联动** 🟢
- 证据：UI 标签写了"监听端口（重启生效）"，保存后不重启不生效，status 里展示的 `port` 与真实监听可能不一致。
- 建议：保存时 toast 明确"需重启生效"，或 P2 做监听热更新。

---

## 第二部分 · 生态调研与新功能建议（12 项）

### 调研结论（同类项目用户真正在意的点）

**OpenCode 免费模型生态痛点（社区实测）**
- **免费额度限制**：账号限时免费（如 DeepSeek V4 Flash Free 等 8 个模型"在 OpenCode 上限时免费"），`FreeUsageLimitError` 429 是常态（opencode issue #15585）→ 多账号轮换是刚需，本网关 24h 封禁设计正确。
- **模型频繁变动/下架**：glm-4.7-free、minimax2.1 等被移除（opencode #10176、oh-my-openagent #2101）→ 静态模型列表会快速腐烂，需要动态发现。
- **IP 风控**：OpenCode 免费账号 IP 限制严重 → 代理池是命脉；原版 OCFreeRelay 为此支持 Clash 订阅拉取 + Clash bridge（vless/hysteria2/tuic 协议节点），Go 版只有 http/socks5 手动导入。
- **模型可用性差**：Linux.do 实测"大部分模型用不了，只有 deepseek-v4-flash-free、mimo-v2.5-free 能用"→ 免费模型过滤/黑名单的价值被低估。

**one-api / new-api 杀手级功能（用户依赖度排序）**
- 令牌管理：**过期时间、额度、模型白名单、IP 白名单**（one-api 官方 FAQ 反复强调令牌额度与账户额度分离）
- 渠道管理：批量导入、权重/优先级、自动拉模型列表、失败自动重试/熔断
- 运营：用户分组/渠道分组/倍率定价、额度明细、兑换码、模型映射、日志查询、审计、密钥加密存储、限流
- new-api 额外：模型广场（一键启停模型）、实时用量/成本看板、渠道健康度图表、全量 REST API

### P0 —— 必修（解决最痛的点）

**#1 免费模型过滤 + 请求模型校验（实现已死的 freeModelsFilter）**
- 痛点：付费/已下架模型暴露给客户端，误调用烧免费额度、报错；`/v1/models` 返回一堆不可用模型。
- 方案：静态内置免费模型清单 + 可选爬取 Zen 定价页刷新；`GET /v1/models` 过滤，`POST /v1/chat/completions` 对非白名单 model 提前 403（原版 OCFreeRelay 行为）。
- 难度：中（透传架构需轻量解析请求体 model 字段）· 价值：极高（原版核心卖点，本版唯一名存实亡的配置）

**#2 代理体系修复：持久化 ID + 自动探活 + CONNECT 级探活**
- 痛点：重启后绑定全失效（鸡肋 #7）；导入后静默直连（鸡肋 #8）；TCP 探活假阳性。
- 方案：settings.json 池条目沿用原 ID；导入即异步探活；探活改为 HTTP CONNECT/SOCKS5 真实握手。
- 难度：低-中 · 价值：极高（IP 隔离是免费额度的命脉）

**#3 流式响应 token 统计（SSE usage 解析）**
- 痛点：用量页/缓存率在流式下全是 0（鸡肋 #6），多账号配额管理无从谈起。
- 方案：流式传输时在尾部拦截 SSE `data:` usage 块（或 io.TeeReader 边透传边解析），计入 stats。
- 难度：中 · 价值：高（用量监控是本类网关标配）

### P1 —— 建议做（对齐 one-api/new-api 高频需求）

**#4 按调用 Key 限额与过期：RPM / TPM / 日额度 / 过期时间**
- 痛点：多账号/多用户分发时无法控制单 key 用量，一个失控客户端可打爆整个池子。
- 方案：CallKey 增加 `rpm/tpm/quota/expiresAt` 字段，转发前检查 + 计数（内存计数即可）。
- 难度：中 · 价值：高（one-api 令牌管理最小版）

**#5 per-key 模型白名单/黑名单 + 模型映射**
- 痛点：只允许某 key 用免费模型、或把 `deepseek-chat` 映射到免费模型。
- 方案：CallKey 增加模型列表；请求体 model 字段校验/改写。
- 难度：中 · 价值：高

**#6 免费模型列表自动刷新（爬 Zen 定价页 + 静态 fallback）**
- 痛点：模型下架频繁，静态清单腐烂（社区多起模型消失 issue）。
- 方案：启动 + 定时（如 6h）抓 `opencode.ai/docs/zen`，失败用上次缓存/内置基线；与 #1 复用同一数据源。
- 难度：中 · 价值：高

**#7 请求日志 / 审计（时间、key、worker、模型、状态、耗时、token）**
- 痛点：现在只有聚合统计，出问题无法定位是哪个 key/哪个模型在打爆哪个 worker。
- 方案：环形缓冲内存日志 + `GET /admin/api/logs`，可加 CSV 导出；不需要数据库。
- 难度：低-中 · 价值：高（new-api 日志脱敏/审计的简化版）

**#8 per-worker 并发限制（单账号同时 N 请求）**
- 痛点：一个 worker 同时多个请求会互相踩 session、加速触发上游限流；现在只有粘性轮询，无并发控制。
- 方案：每 worker 信号量（如 2），排队或超时返回。
- 难度：低 · 价值：中-高

**#9 Clash 订阅导入 / vless·hysteria2 支持（或 Clash bridge）**
- 痛点：代理来源单一（手动 TXT），而免费账号 IP 隔离才是刚需；原版 OCFreeRelay 已有此能力，是差距项。
- 方案：订阅 URL 拉取（多 UA 尝试）+ Clash bridge 模式。
- 难度：高 · 价值：中（依赖场景，有代理需求的人很需要）

### P2 —— 锦上添花

**#10 用量看板增强：按 key/按模型统计、趋势图、配额告警（webhook/通知）**
- 痛点：只有按 worker 的聚合；不知道哪个 key 烧了多少、免费额度将尽无感知。
- 方案：stats 增加维度（key、model），UI 加趋势；接近限额/全 worker 封禁时打 webhook。
- 难度：中 · 价值：中

**#11 管理 REST API + 独立 API Token**
- 痛点：批量加 worker/导入代理只能点 UI，无法脚本化（new-api 全量 REST API 是高赞点）。
- 方案：给现有 `/admin/api/*` 加 Token 鉴权通道 + 幂等批量端点。
- 难度：中 · 价值：中

**#12 多上游/多 BaseURL（渠道化）**
- 痛点：绑定死 opencode zen；上游换域名/加新 provider 要改配置重启。
- 方案：BaseURL 从全局配置下沉到 worker 级。
- 难度：低 · 价值：中

---

## 总结

- **鸡肋的本质**：不是功能太多，而是**"承诺了但没实现"**——freeModelsFilter 是原版核心功能的空壳、缓存率/用量统计在常见路径下是假的、代理体系在重启后是断的。先修这四件事，比加任何新功能都值。
- **一句话路线图**：P0 让现有功能真实生效（过滤、代理、统计）→ P1 对齐 one-api 的令牌/模型管控最小集 → P2 看板与渠道化。
