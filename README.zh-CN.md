# opencode-free-gateway

面向 **OpenCode Zen 免费模型** 的 OpenAI 兼容 LLM 网关，使用 Go 编写，
**零第三方依赖**（仅标准库）。

> **致谢** — 本项目受
> [OCFreeRelay](https://github.com/kirafishy/OCFreeRelay)（MIT）启发。
> 这是一次**独立的 Go 重写**：未使用原 TypeScript 项目的任何代码，API
> 表面保持兼容。原项目的设计思路（仅免费模型、粘性亲和、429 免费额度封禁）
> 在此从零重新实现。

网关接受任意 `/v1/*` 请求，将其调度到健康的 worker（OpenCode 账号 Key），
可选择通过该 worker 绑定的 HTTP/SOCKS5 代理出站，并将响应原样流式返回。
响应（包括 SSE 流）**逐字节透传**，不做任何协议翻译。

```
客户端 ──/v1/*──→ 网关（鉴权 → worker 调度 → 代理出站）──→ opencode.ai/zen/v1
```

## 特性

- **透明 `/v1/*` 透传** — chat/completions、models 以及上游未来新增的
  OpenAI 兼容端点均可直接使用，无需网关解析
- **多 worker 粘性调度** — 一个 worker 持续服务直到失败，保持上游
  prompt 缓存温度
- **429 自动封禁** — `FreeUsageLimitError` 429 会将该 worker 硬封禁 24 小时；
  其他 429 采用短指数退避冷却
- **自动禁用** — 连续 5 次失败将禁用该 worker 10 分钟
- **worker 错误可视化** — 管理面板实时显示每个 worker 的
  ready/cooldown/banned 状态及最近错误信息
- **代理池** — 支持从 `.txt` 文件批量导入 HTTP 与 SOCKS5 代理
  （每行一条 `http://user:pass@host:port` 或 `socks5://user:pass@host:port`），
  按 host:port 去重、批量探活、一键清理失效条目
- **worker ↔ 代理绑定** — 每个 worker 可绑定独立出口 IP
- **管理面板** — 密码保护的 Web 管理界面（暖色 Anthropic 风格），
  包含 worker / 代理池 / 调用 Key / 用量管理
- **调用 Key** — 可选为 `/v1/*` 客户端启用 Bearer Key 白名单
- **统计** — 按 worker 统计请求数、token、缓存命中/写入；删除 worker
  不会删除其历史统计
- **零依赖** — 单个约 10 MB 静态二进制，无运行时，无供应链风险

## 快速开始

```bash
go build -o relay ./cmd/relay
./relay
```

- 管理界面：http://127.0.0.1:9876/admin/
- 首次访问：设置管理员密码（不内置默认密码）
- OpenAI 兼容端点：http://127.0.0.1:9876/v1

## 配置

设置存放在 `data/settings.json`（自动创建，可用环境变量覆盖）：

| 环境变量 | 作用 |
|---|---|
| `OCFREELAY_SETTINGS_PATH` | settings 文件路径 |
| `OCFREELAY_STATS_PATH` | worker 统计文件路径 |
| `OCFREELAY_DATA_DIR` | 数据目录（默认 `data/`） |
| `OCFREELAY_ADMIN_PASSWORD` | 启动时设置管理员密码（首次运行） |

其余配置（上游地址、worker、代理、调用 Key）全部在管理界面操作。

## 架构

```
cmd/relay            入口（配置加载、装配、优雅退出）
internal/config      设置模型 + JSON 持久化
internal/relayproxy  /v1 透传：worker 选择、代理出站、io.Copy
internal/rotator     worker 状态机（粘性/封禁/冷却/自动禁用）
internal/pool        代理池：TXT 导入、探活、清理
internal/auth        管理员会话（PBKDF2）+ 调用 Key 白名单
internal/stats       按 worker 用量计数、JSON 持久化
internal/server      HTTP 路由 + 管理 API
internal/adminui     内嵌管理界面（go:embed）
```

## 请求处理

- **请求体**：透传 + 仅一项兼容修正——移除 `client_metadata` 字段
  （上游会拒绝该字段）
- **请求头**：按白名单构造而非原样复制——
  - 始终设置 `Content-Type`、`Authorization`（Bearer worker Key）
  - 仅转发 `x-opencode-*` / `x-session-id` / `x-title` / 客户端 UA
  - 会话亲和合成（`x-session-affinity`/`x-session-id` →
    `x-opencode-session` + 请求 UUID）
  - 可选 CLI 身份合成（UA/client/project + UUID）
  - 其余客户端头全部丢弃
- **响应**：含 SSE 流在内逐字节透传，不做任何转换

## License

MIT