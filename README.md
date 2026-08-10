# opencode-free-gateway

OpenAI-compatible LLM gateway for **OpenCode Zen free models**, written in Go
with **zero third-party dependencies** (standard library only).

The gateway accepts any `/v1/*` request, dispatches it to a healthy worker
(OpenCode account key), optionally egresses through that worker's bound
HTTP/SOCKS5 proxy, and streams the response back untouched. Responses are
passed through byte-for-byte — including SSE streams — with no protocol
translation.

```
client ──/v1/*──→ gateway (auth → worker schedule → proxy egress) ──→ opencode.ai/zen/v1
```

## Features

- **Transparent `/v1/*` passthrough** — chat/completions, models, and any
  future OpenAI-compatible endpoint work without gateway-side parsing
- **Multi-worker scheduling with sticky affinity** — a worker keeps serving
  until it fails, keeping the upstream prompt cache warm
- **429 auto-ban** — `FreeUsageLimitError` 429 hard-bans the worker for 24h;
  other 429s get short exponential cooldowns
- **Auto-disable** — 5 consecutive failures disable a worker for 10 minutes
- **Per-worker error visibility** — the admin UI shows ready/cooldown/banned
  status plus the last error message for every worker
- **Proxy pool** — import HTTP & SOCKS5 proxies from a `.txt` file
  (`http://user:pass@host:port` or `socks5://user:pass@host:port`, one per
  line), dedupe by host:port, batch probe, prune dead entries
- **Worker ↔ proxy binding** — every worker can have a dedicated egress IP
- **Admin panel** — password-protected management UI (warm Anthropic-style
  design), with worker / proxy pool / call-key / usage management
- **Call keys** — optional bearer-key allow-list for `/v1/*` clients
- **Stats** — per-worker request counts, tokens, cache hit/write; deleting a
  worker never deletes its history
- **Zero dependencies** — one static ~10 MB binary, no runtime, no supply
  chain

## Quick start

```bash
go build -o relay ./cmd/relay
./relay
```

- Admin UI: http://127.0.0.1:9876/admin/
- First visit: set the admin password (no default password is shipped)
- OpenAI-compatible endpoint: http://127.0.0.1:9876/v1

## Configuration

Settings live in `data/settings.json` (auto-created, env-overridable):

| Env var | Purpose |
|---|---|
| `OCFREELAY_SETTINGS_PATH` | settings file path |
| `OCFREELAY_STATS_PATH` | worker stats file path |
| `OCFREELAY_DATA_DIR` | data directory (default `data/`) |
| `OCFREELAY_ADMIN_PASSWORD` | set admin password at boot (first run) |

Everything else (upstream URL, workers, proxies, call keys) is managed from
the admin UI.

## Architecture

```
cmd/relay            entrypoint (config load, wiring, graceful shutdown)
internal/config      settings model + JSON persistence
internal/relayproxy  /v1 passthrough: worker pick, proxy egress, io.Copy
internal/rotator     worker state machine (sticky/ban/cooldown/auto-disable)
internal/pool        proxy pool: TXT import, probe, prune
internal/auth        admin sessions (PBKDF2) + call-key allow-list
internal/stats       per-worker usage counters, JSON persistence
internal/server      HTTP routes + admin API
internal/adminui     embedded management UI (go:embed)
```

## License

MIT
