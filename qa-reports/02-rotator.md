# QA Report 02 — Rotator 调度与状态机

**项目**: /root/ocfreelay-go · **日期**: 2026-08-10 · **范围**: `internal/rotator/` + 调用方 `internal/relayproxy/relay.go`
**执行**: `go test -race -count=1 ./internal/rotator/` → **ok (1.016s)**；`go vet ./internal/rotator/` → 干净
**测试规模**: 原有 9 个 + 本次新增 12 个边界测试（`internal/rotator/edge_test.go`），共 21 个全部通过，含 `-race`

---

## 1. 状态机理解 ✅

`internal/rotator/rotator.go` 实现 per-worker 调度，核心是 `pickLocked` 四步选择逻辑 + `MarkCooldown` / `MarkBan` / `MarkSuccess` / `Sync` 状态机：

- **Ready(now)** = `now.After(CooldownUntil) && now.After(BannedUntil)`（严格 `After`，等于边界算未就绪）
- **Status** 优先级：`banned-24h` > `cooldown` > `ready`
- 失败路径：`MarkCooldown` 指数退避 5/10/20/40s → 第 5 次触发 10 分钟自动禁用并清零计数；`MarkBan` 封禁 24h 并清空 cooldown；`MarkError` 只记错误不改变调度状态
- 调用方 `relay.go Forward`：每请求最多 `MaxWorkerAttempts=3` 次尝试，`tried` map + `PickExcluding` 轮换 worker；429 在 `attemptOne` 内分类（FreeUsageLimitError → 24h ban，否则 cooldown）

## 2. 机制检查

| 机制 | 结果 | 测试 |
|---|---|---|
| 粘性亲和（cursor 优先） | ✅ cursor worker ready 且未 exclude 时直接返回，失败前一直打同一个 | `TestStickyPick` |
| PickExcluding 跳过已试 worker | ✅ 正常路径严格轮换；全部 exclude 时 fallback 到任一 ready worker（文档已声明） | `TestPickExcluding`、`TestEdgeExcludeAllFallback` |
| 封禁 24h | ✅ `MarkBan` 设 BannedUntil=now+24h，Ready/Status/Pick 均生效；到期自动恢复 | `TestBan24h` |
| 冷却指数退避 | ⚠️ 序列 5/10/20/40s 正确，但 **cooldownMax=60s 不可达**（见 P3） | `TestEdgeBackoffSequence` |
| 连续 5 次自动禁用 10 分钟 | ✅ 第 5 次失败 → CooldownUntil=now+10min，计数清零 | `TestAutoDisableAfterConsecutiveFails`、`TestEdgeBackoffSequence` |
| MarkSuccess 重置 | ✅ 计数清零、退避重新从 5s 起；不清除 cooldown/ban 时间（时间门控，符合设计） | `TestEdgeMarkSuccessResetsBackoff` |
| Sync 保留状态 | ✅ 按 ID 保留 cooldown/ban/错误史；移除的 worker 状态丢弃、新 worker 全新 | `TestSyncPreservesState`、`TestEdgeSyncPreservesBanAndCooldown` |

## 3. 边界情况

- **worker=0** ✅ Pick/PickExcluding 返回 nil，Forward 返回 `ErrNoWorker`（`TestEdgeZeroWorkers`）
- **worker=1** ✅ 3 次重试全打同一个（粘性 + fallback）；cooldown/ban 中仍被 fallback 返回（`TestEdgeSingleWorkerRetriesRepeat`）→ 见 P1
- **worker=2** ✅ 前 2 次轮换不同 worker，第 3 次 fallback 重复打第一个：实测序列 `[a b b]`（`TestEdgeTwoWorkersThreeAttempts`）→ 见 P2
- **worker=3** ✅ 3 次尝试 3 个不同 worker（`TestPickExcluding`）
- **全部封禁** ✅ ReadyCount=0，Pick 仍返回 cursor（被封 worker）→ 见 P1
- **exclude 全部排除** ✅ fallback 返回任一 ready worker，不返回 nil（文档行为，`TestEdgeExcludeAllFallback`）
- **banned vs cooldown 优先级** ✅ Status 显示 banned-24h；`MarkBan` 清空 cooldown（先冷却后封禁 → 封禁到期即 ready）；`MarkCooldown` 不清 ban（先封禁后冷却 → 双门控，封禁到期后 cooldown 仍生效）(`TestEdgeBanAndCooldownPriority`)
- **并发安全** ✅ 8 goroutine × 200 轮混合 Pick/PickExcluding/MarkCooldown/MarkSuccess/MarkBan/MarkError/ReadyCount/Snapshots，`-race` 零报告（`TestEdgeConcurrentPickMark`）。分析：可变字段只在 `r.mu` 内写，外部读的 `*State` 字段（ID/APIKey/ProxyID）创建后不可变，当前代码路径无竞争

## 4. 数据竞争检查 ✅

`go test -race -count=1 ./internal/rotator/` → **ok**，无竞争报告；`go vet` 干净。

## 5. 额外边界测试 ✅

新增 `internal/rotator/edge_test.go`，12 个测试全过：

| 测试 | 验证点 |
|---|---|
| `TestEdgeZeroWorkers` | 0 worker 时 Pick/PickExcluding/ReadyCount |
| `TestEdgeSingleWorkerRetriesRepeat` | 单 worker 3 次重试重复打同一个（含 cooldown/ban 中 fallback） |
| `TestEdgeSyncPreservesBanAndCooldown` | Sync 保留 ban+cooldown、新 worker 全新 |
| `TestEdgeBanAndCooldownPriority` | ban/cooldown 双门控与优先级（两种时序） |
| `TestEdgeAllBannedPick` | 全部封禁时 Pick fallback 行为 |
| `TestEdgeExcludeAllFallback` | exclude 全排除时 fallback 到 ready worker |
| `TestEdgeMarkSuccessResetsBackoff` | 成功重置计数，下次退避回到 5s |
| `TestEdgeBackoffSequence` | 退避 5/10/20/40s + 第 5 次 10min 禁用 |
| `TestEdgeTwoWorkersThreeAttempts` | 2 worker 3 次尝试序列 `[a b b]` |
| `TestEdgeConcurrentPickMark` | 并发混合 Pick/Mark 无竞争 |

> 注：编写过程中修掉了自己 1 个测试场景错误（5s cooldown 与 24h ban 时间尺度不同，改为"封禁末段发生失败"验证双门控），非代码问题。

## 6. max_iters / 循环配置检查 ⚠️

**结论：不存在配置项。** `MaxWorkerAttempts = 3` 硬编码于 `internal/relayproxy/relay.go:34`；`internal/config/config.go` 的 `Settings` 结构体无任何 retry/attempts/max_iters 字段；`cmd/relay/main.go` 与 `server.adminapi.go` 的配置加载路径均不涉及。运行时重试次数固定为 3，无法通过配置文件调整（git log 显示该逻辑由 commit `035c9ea "relay: rotate workers on failure, surface error only after 3 attempts"` 引入）。若旧 Python 版支持 `max_iters` 配置，此为行为回归。

---

## 问题清单

### P1（中）全部封禁/单 worker 封禁时，会对被封 worker 真实发 3 次上游请求
`pickLocked` 第 4 步无条件返回 cursor worker，无视 banned/cooldown；`Forward` 拿到后照常执行 `attemptOne`。单 worker 命中 FreeUsageLimit 被 ban 24h 后，**每个**客户端请求仍会向该账号打 3 次真实请求（429 → 重新 MarkBan → 循环），浪费延迟与账号负载，且 `worker == nil` 分支直接 `return nil, ErrNoWorker` 会丢掉最后一次真实 429 响应信息。
**修复建议**：`pickLocked` 第 4 步仅在 cooldown（非 banned）时 fallback，全 banned 返回 nil；`Forward` 中 `worker == nil` 时 `break` 并 surface `lastResult`/`lastErr`（而非立即 ErrNoWorker）。

### P2（中）2 worker 时第 3 次重试重复打第一个 worker；退避对请求内重试无效
`MaxWorkerAttempts=3` 固定，而 exclude fallback 会重复已试 worker（实测 `[a b b]`），与 `Forward` 注释 "trying up to MaxWorkerAttempts **different** workers" 不符；同时 cooldown worker 被 fallback 后立即重打，5s 退避在同一请求的 3 次尝试内形同虚设。
**修复建议**：循环内检查 `worker.Ready(now)`，不 ready 即 break 并 surface 错误；或将 attempts 上限改为 `min(MaxWorkerAttempts, ReadyCount)`。

### P3（低）cooldownMax=60s 不可达（死常量）
退避为 `5s << (fails-1)`，但 fails 在第 5 次被 auto-disable 分支清零，最大有效退避是第 4 次的 40s；`cooldownMax = 60s` 永远不生效，误导读者以为有 60s 档。
**修复建议**：删除 cooldownMax 或将上限改为 40s；若想保留 60s 档，需把 `AutoDisableAfter` 提到 6。

### P4（低）pickLocked 第 3 步硬重置 `r.nextIdx = 0`
exclude 全部命中时 cursor 被重置到 0 号 worker，而不是所选中 worker 的 `idx`，导致后续请求的粘性起点跳跃，多 worker 下亲和性波动。
**修复建议**：改为 `r.nextIdx = idx`。

### P5（低）MarkSuccess 不清 LastError / LastErrorAt
成功恢复后 admin UI 仍显示旧错误信息与时间戳，容易误判。
**修复建议**：`MarkSuccess` 同时清空 `LastError`/`LastErrorAt`。

---

## 设计备注（非问题）

- **P6** `Pick` 返回共享 `*State` 指针：当前调用方只读不可变字段（ID/APIKey/ProxyID），`-race` 验证安全；但若未来调用方在锁外读 `CooldownUntil`/`LastError` 会产生竞争。建议在注释中声明"可变状态只能经 `Snapshots()` 读取"。
- **P7** `Sync` 保留状态按 ID；worker 被删除后重加会丢失原状态（符合预期）；worker 重排序时 `nextIdx` 只做越界归零，不保证指向同一 worker（影响极小）。

---

**结论：核心状态机实现正确、测试覆盖良好、无数据竞争。3 个实际问题（P1–P3）+ 2 个低优先级整洁问题（P4–P5）。** 关键修复优先级：P1（封禁后停止无效重试）> P2（重试上限与 ready 数对齐）> P3/P4/P5。
