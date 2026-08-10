# QA-04 鉴权与安全审计报告

- **项目**: ocfreelay-go (OpenAI 兼容 LLM 网关)
- **范围**: internal/auth、internal/server、internal/config、cmd/relay 的登录/鉴权/安全边界
- **方法**: 源码审读 + 本地起服实测（端口 9903，全新 settings.json，upstream 指向死地址以便观察放行行为）
- **日期**: 2026-08-10
- **结论**: 核心鉴权设计正确（PBKDF2 + 随机 token + 12h 会话 + 全 API 鉴权），但存在 **3 个严重问题**（logout 不吊销、池密码明文返回、env 覆盖密码明文落盘）和 **7 个一般问题**，共 **10 个问题**。

---

## 1. PBKDF2 密码哈希 ✅（建议增强）

**实测**: settings.json 落盘值为 `pbkdf2-sha256$210000$<16B salt b64>$<32B key b64>`，格式正确。登录走 210k 次迭代，比较为常数时间（自实现 `subtleConstantTimeEq`，逐字节异或）。

**安全风险**: 低。实现正确：随机 16B salt、PBKDF2-SHA256、210k 迭代、无明文。两点次要问题：
- OWASP 2023 对 PBKDF2-HMAC-SHA256 建议 **600k** 迭代，210k 偏旧但仍属可接受基线。
- 常数时间比较在长度不等时提前返回（长度泄露，可接受，业界通行）。
- `randomToken` 丢弃 `rand.Read` 的 error（理论上有全零 token 的可能，实际 Linux 上不会发生）。

**修复建议**: 迭代数提升至 600k（改 `iterations` 常量即可，旧 hash 可平滑兼容——`verifyPassword` 按存储值里的迭代数验证）；`randomToken` 检查 rand.Read error。

## 2. 登录 token 生成（随机性）✅

**实测**: 实测 token 为 64 位 hex（32 字节），`crypto/rand` 生成。token 长度 64 实测通过。

**安全风险**: 低。256-bit 熵，无顺序、无时间戳可预测性。无 session fixation（每次登录签发新 token）。

## 3. Session 过期（12h）✅

**实测**: 源码 `SessionTTL = 12 * time.Hour`；`ValidSession` 过期即删并返回 false。Cookie Expires 与之一致（实测 `Expires=Mon, 10 Aug 2026 17:10:38 GMT`，登录时间 05:10 → 12h）。

**安全风险**: 低-中。过期会话只在被访问时才惰性删除；会话 map 无上限，持密码者可无限签发 session（内存缓慢增长）。属小问题。

**修复建议**: 登录时若 sessions 超过阈值（如 1000）清理已过期项。

## 4. X-Admin-Token 与 Cookie 双认证 ✅

**实测**: 同一 token 分别经 `X-Admin-Token` 头和 `Cookie: ocfr_session=` 访问 `/admin/api/status` 均 200。Cookie 带 `HttpOnly; SameSite=Lax`，**无 `Secure` 标志**。

**安全风险**: 中。
- 无 Secure 标志：若部署在 HTTPS 反代后，cookie 仍会在 HTTP 上传输；应用本身无 TLS（纯明文 HTTP）。
- SameSite=Lax 已挡住跨站 POST CSRF（Lax 只在顶层 GET 导航带 cookie，而写操作全是 POST/PUT）——CSRF 基本缓解，可接受。
- 前端把 token 存 `localStorage`（app.js `TOKEN_KEY`），若管理页存在任何 XSS 向量则 token 可被窃取。当前 UI 不渲染用户内容，风险低。

**修复建议**: cookie 加 `Secure`（配合反代 TLS）；token 可考虑移入 HttpOnly cookie 单通道；UI token 存储至少标注风险。

## 5. setup 模式（无密码时只放行 settings）✅（附注一个风险）

**实测**: 无密码时 `/admin/api/settings` GET/PUT 匿名可达（200），其余 `/admin/api/status`、`/admin/api/pool`、`/admin/api/callkeys` 等全部 401。设密码后 `/admin/api/settings` 匿名立即 401。流程完整：`PUT settings {password}` → 200 → 登录成功。

**安全风险**: 中。setup 模式下**先到先得**——任何人能在管理员完成初始化前抢注 admin 密码（同时还能改 `requireCallKeyAuth`、port、baseUrl）。由于服务默认绑定 0.0.0.0（见 #12），LAN 上可被劫持。初始化窗口期是常态风险，但绑定所有网卡显著放大。

**修复建议**: 默认只绑 127.0.0.1 或 setup 加一次性随机 claim token（日志打印）。

## 6. logout ❌ —— 服务端不吊销会话

**实测**: `POST /admin/api/logout`（带 token）→ 200；**随后用同一 token 访问 `/admin/api/status` 仍返回 200**。源码中 `handleLogout` 只清空浏览器 cookie（`MaxAge:-1`），从未调用 `auth.RevokeSession`（该方法存在但全项目只有测试引用）。

**安全风险**: 高。logout 是纯客户端行为：被盗的 token（如泄露的 X-Admin-Token）在用户 logout 后 12h 内依然有效；"退出登录"无法真正终止会话，安全事件响应失效。

**修复建议**: `handleLogout` 解析当前 token（cookie 或 header）并调用 `RevokeSession`；logout 可保持免鉴权（幂等），但必须吊销。

## 7. 调用 Key 鉴权（/v1）✅

**实测**:
- `requireCallKeyAuth=false`：无 key 请求 `/v1/chat/completions` → 502（转发至死 upstream，证明放行）。
- 开启后：无 key → 401；`Authorization: Bearer wrong` → 401；`Bearer ck-test-123` → 502（鉴权通过，转发失败）；小写 `bearer` 前缀同样通过（EqualFold）。
- Bearer 解析 `len(v)>7 && EqualFold(v[:7],"Bearer ")`，正确。

**安全风险**: 中（默认配置）。**默认 `requireCallKeyAuth=false` + 默认绑 0.0.0.0 = 默认就是一个开放转发代理**，任何可达者可用上游 key 刷量。401 响应不含额外信息，无泄露。

**修复建议**: 默认开启 key 鉴权，或至少默认绑 127.0.0.1；README 显著警示。

## 8. 管理 API 全覆盖鉴权 ✅

**实测**: 无 token 访问 `pool`、`pool/probe`、`pool/prune`、`workers`、`stats/reset`、`pool/import`、`/admin/api/whatever` → 全部 401。免鉴权仅：`/admin/api/login`（认证本身）、`/admin/api/setup`（只回布尔）、`/admin/api/status`（**注意：实测需鉴权**，比任务预期更严）、`/health`。

**安全风险**: 低。认证检查在所有分支前执行，未知路径也是先 401 再 404（不泄露路由存在性）。

## 9. /admin/static/ 静态文件免鉴权 ⚠️（符合设计但需确认边界）

**实测**: `/admin/`（UI 壳）与 `/admin/static/app.js` 匿名 200；`/admin/static/../api/login` 被 FileServer 规范化重定向（301），无目录穿越。静态资源 grep 无硬编码密钥。

**安全风险**: 低-中。UI 壳 + 静态资源免鉴权是设计意图（SPA 通过 `/admin/api/status` 判断登录态），API 均需鉴权——边界正确。但注意：**SPA 壳被匿名加载意味着管理面板的 JS 逻辑公开可见**（可被用来研究攻击面），且 `localStorage` 存 token 依赖 CSP/无 XSS 前提（当前无 CSP 头）。

**修复建议**: 给 `/admin/` 响应加 `Content-Security-Policy: default-src 'self'` 与 `X-Content-Type-Options: nosniff`。

## 10. 密码哈希写 settings.json ❌（env 路径明文）+ ⚠️（文件权限）

**实测**: 正常路径：settings.json 存的是 PBKDF2 hash（非明文），`.gitignore` 排除 `data/`（git check-ignore 命中、git ls-files 无 data 文件）✅。但：
- **env 覆盖路径是明文**：`config.applyEnvOverrides` 对 `OCFREELAY_ADMIN_PASSWORD` 调 `hashPlain`，而全项目从未调用 `config.SetHashFunc`（grep 仅定义处），默认实现返回 `"plain:"+password` → 用 env 设密码会以**明文前缀形式**写入 `adminPasswordHash`，且 `verifyPassword` 格式校验失败 → **管理员将永远无法登录**。双重故障。
- 文件权限：settings.json 落盘 `0644`（实测 `-rw-r--r--`），任何本机用户可读——文件内含 proxy 密码、上游 API key、call key。

**安全风险**: 高（env 路径下 admin 密码明文 + 锁死）；中（0644 权限）。

**修复建议**: main.go 启动时调用 `config.SetHashFunc`（包装 `auth.HashPassword`）或直接移除占位逻辑；`Store.Save` 写 0600（`os.WriteFile(tmp, data, 0o600)`）。

## 11. 代理池 API 返回脱敏 ❌ —— 密码明文返回

**实测**: 导入 `http://user1:secretpw@5.6.7.8:3128` 后，`GET /admin/api/pool`（带 token）返回：
```json
{"ID":"px_...","Username":"user1","Password":"secretpw",...}
```
`/admin/api/status` 的 `pool` 字段同样返回明文密码（`pool.All()` 直接序列化，item 无 json tag）。

**安全风险**: 高（会话泄露即全池凭据泄露；日志/抓包/前端缓存都可能留下凭据）。虽然需要 admin 会话，但凭据应默认脱敏。

**修复建议**: 池输出加 `Password` → 空字符串 + `passwordSet: true`；仅在明确"编辑"动作时按需回填（或永不回填，只允许覆盖）。

## 12. 请求体大小限制 ⚠️（admin 达标，/v1 静默截断）

**实测**: admin 端 5MiB body → 400 `bad JSON body: http: request body too large`（MaxBytesReader 4MiB）✅。`/v1` 33MiB body → 502（`io.LimitReader(32<<20)` 静默截断，不报错；server.go 里 `ReadBody` 的 "too large" 分支实际永远不会触发——`ReadBody` 从不返回该错误）。截断后的 JSON 被原样转发上游，由上游报错。

**安全风险**: 中。32MiB 上限挡住超大包，但截断而非拒绝：合法 JSON 可能被切断成非法负载上行，行为不可预期；错误提示与实际不符。

**修复建议**: `/v1` 读取超过 32MiB 时显式返回 413（`ReadBody` 读 32MiB+1 字节判断溢出）。

## 13. 超时 ⚠️

**实测**: `http.Server` 仅设 `ReadHeaderTimeout: 15s`——**无 ReadTimeout / WriteTimeout / IdleTimeout / MaxHeaderBytes**。上游 `http.Client{Timeout: 0}`（SSE 需要长连接，可理解），代理 transport 只有 Dial 15s、TLS 15s，**无 ResponseHeaderTimeout**。

**安全风险**: 中。头部之后 body 慢速滴灌无截止时间（慢速 DoS）；上游响应头永远不返回时连接悬挂（依赖 worker 调度，无整体上限）。SSE 长连接是正当需求，但应区分流式/非流式超时。

**修复建议**: server 加 `ReadTimeout`/`WriteTimeout`/`IdleTimeout`（如 60s/0/120s，SSE 写超时可豁免）；transport 加 `ResponseHeaderTimeout`（如 30s）。

## 14. 默认监听 0.0.0.0 ⚠️

**实测**: 日志 `addr=:9903`；通过容器外网 IP `172.16.119.142:9903/health` 直接访问成功 → 绑定所有接口。日志里 `admin=http://127.0.0.1:9903/admin/` 的提示有误导性。

**安全风险**: 高（结合默认关闭 key 鉴权 = 开放转发 + 可被 setup 劫持的管理面板暴露在 LAN 上）。

**修复建议**: 默认 `127.0.0.1:<port>`，配置项 `listenHost` 显式开启外网绑定；或至少默认开启 `requireCallKeyAuth`。

## 15. 登录无速率限制 + 全局互斥锁 DoS ⚠️

**实测/审读**: `/admin/api/login` 无任何限流/锁定。`VerifyPassword` 在持有 `m.mu` 期间执行 210k 次 PBKDF2（约 50-100ms）；而 `/v1` 的 `CallKeyOK`、管理 API 的 `ValidSession` 共用同一把锁 → 攻击者并发刷 login，可让**整个网关（包括 /v1 鉴权）阻塞排队**。

**安全风险**: 中-高。暴力破解防护仅靠 PBKDF2 计算成本（串行化后约 10 次/秒，弱密码仍可被离线/在线撞）；更严重的是认证路径 DoS。

**修复建议**: 登录限流（IP + 全局，指数退避）；PBKDF2 移出锁外（先复制 hash 再计算）；或将 auth manager 拆读写锁/分锁。

## 16. .gitignore 排除 data/ ✅

**实测**: `.gitignore` 含 `data/`、`dist/`、`relay`、`*.log`；`git check-ignore -v data/settings.json` 命中；`git ls-files` 无任何 data/ 文件 → settings.json（含 admin hash、proxy 密码、上游 key）**不会进 git**。

**安全风险**: 无。⚠️ 唯一残余：settings.json 0644 权限（见 #10）与 env 明文路径。

---

## 问题清单汇总（10）

| # | 级别 | 问题 | 位置 |
|---|------|------|------|
| 1 | ❌ 高 | logout 不吊销服务端会话，token 失效需等 12h | server.go `handleLogout` |
| 2 | ❌ 高 | `/admin/api/pool`、`/admin/api/status` 明文返回 proxy 密码 | adminpool.go / adminapi.go |
| 3 | ❌ 高 | `OCFREELAY_ADMIN_PASSWORD` env 路径存明文 `plain:` hash 且登录必失败（SetHashFunc 无人调用） | config.go `applyEnvOverrides` |
| 4 | ⚠️ 高 | 默认 0.0.0.0 绑定 + 默认关闭 key 鉴权 = 开放转发 | main.go |
| 5 | ⚠️ 中 | 登录无限流 + PBKDF2 持全局锁 → 暴力破解/认证 DoS | auth.go `VerifyPassword` |
| 6 | ⚠️ 中 | http.Server 无 Read/Write/IdleTimeout；上游无 ResponseHeaderTimeout | main.go / proxydial.go |
| 7 | ⚠️ 中 | /v1 32MiB 静默截断不返回 413，错误路径形同虚设 | relayproxy/relay.go ReadBody |
| 8 | ⚠️ 中 | setup 先到先得 + 0.0.0.0 = LAN 可劫持初始化 | adminapi.go |
| 9 | ⚠️ 中 | settings.json 0644 世界可读（含池密码/上游 key）；cookie 无 Secure；UI 无 CSP；token 存 localStorage | config.go / server.go |
| 10 | ⚠️ 低 | PBKDF2 210k 迭代低于 OWASP 现行建议 600k；会话 map 无上限 | auth.go |

## 复测命令备忘

```bash
go build -o /tmp/ocfr-qa04 ./cmd/relay
OCFREELAY_SETTINGS_PATH=/tmp/qa04/settings.json OCFREELAY_STATS_PATH=/tmp/qa04/worker-stats.json nohup /tmp/ocfr-qa04 &
# setup claim
curl -X PUT localhost:9903/admin/api/settings -d '{"password":"admin123"}'
# login → token
TOK=$(curl -s -X POST localhost:9903/admin/api/login -d '{"password":"admin123"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
# logout 后同 token 仍可用（问题 #1）
curl -s -X POST -H "X-Admin-Token: $TOK" localhost:9903/admin/api/logout
curl -s -o /dev/null -w "%{http_code}" -H "X-Admin-Token: $TOK" localhost:9903/admin/api/status  # 200 = 未吊销
```
