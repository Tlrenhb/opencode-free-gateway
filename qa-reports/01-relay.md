# QA-01 转发链路验证摘要（子代理测试数据 + 复查）

**来源**: 子代理留下的 mock 上游实测日志（mock-requests.log / relay.log），05:10-05:14 运行，经本地 mock 上游（127.0.0.1:9902）验证。

## 实测通过项

| 测试 | 证据 | 结论 |
|---|---|---|
| `/v1/models` 透传 | 网关转发到 mock 上游，方法/路径原样 | ✅ |
| query 保留 | `/v1/models?bar=hello+world&foo=1` 完整转发 | ✅ |
| chat/completions 转发 | POST body `{"messages":[...],"model":"m1"}` 原样到达 | ✅ |
| **Authorization 注入** | `Bearer key-w1`（worker key 正确替换） | ✅ |
| **CLI 头合成** | X-Opencode-Client/Project/Request/Session 全部生成 | ✅ |
| 非 JSON body 透传 | `raw non-json payload` 原样转发（stripClientMetadata 不破坏） | ✅ |
| 数组 body | `[1,2,{"a":1}]` 原样转发 | ✅ |
| stream 查询参数 | `?stream=true` 保留 | ✅ |

## 复查补充（05:30 代码状态）

- stripClientMetadata：仅删除 `client_metadata` 键，其余字段重序列化保留（JSON 语义等价）
- 3 次换 worker 重试 + PickExcluding 已实测（rotator 报告 + 本地验证）
- 429 封禁 24h / 冷却 / 自动禁用 10min：rotator 测试覆盖

## 结论

转发链路（透传 + body 修正 + 头构造 + worker 调度）实测正常，无功能性问题。
