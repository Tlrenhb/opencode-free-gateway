# QA-03 代理池检查摘要

**方法**: 源码审读 + 本地起服实测（导入/删除/重启全流程）

## 实测通过项

| 检查项 | 结果 |
|---|---|
| 代理解析（http/socks5/bare host:port/https 归一化） | ✅ 单测覆盖（pool_test.go） |
| 非法行报告 | ✅ `invalidLines` 返回 |
| 导入去重（host:port） | ✅ 重复导入 skipped |
| **导入自动建 worker（apiKey=public + 绑定）** | ✅ 实测 `workersCreated:2` |
| **删除代理自动删 worker（统计保留）** | ✅ 实测删除后 worker 消失 |
| **重启绑定保留（修复后）** | ✅ 实测重启后 proxyId 一致 |
| 探活（TCP 连通性） | ✅ ProbeAll 并发 |
| prune 清理 | ✅ 删 disabled/unusable + 对应 worker |
| **密码脱敏（修复后）** | ✅ API 返回 `Password:""` + `passwordSet:true` |

## 遗留建议（非阻塞）

1. 探活目前是 TCP 连通性检查，可升级为真实 HTTP CONNECT/SOCKS5 握手（roadmap #2）
2. 导入后新代理 `Usable=false`，需手动探活才会标记可用——可加"导入即自动探活"

## 结论

代理池功能完整，核心流程全部实测通过，无阻塞问题。
