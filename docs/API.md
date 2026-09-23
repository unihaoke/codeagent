# CodeAgent 接入层 API

- **Base URL**：`http://<host>:8080`
- **业务前缀**：`/api/v1`（健康检查与指标在根路径）
- **认证**：控制台 `Authorization: Bearer <jwt>`；接入方 `X-API-Key: ca_live_xxx`（也支持 `Authorization: ApiKey ca_live_xxx`）
- **响应信封**：所有 `/api/v1` 接口统一返回

```json
{ "code": 0, "message": "ok", "requestId": "r-1a2b3c4d5e6f", "data": { } }
```

分页数据统一为 `{ "items": [], "total": 0, "page": 1, "pageSize": 20 }`。

| code | HTTP | 含义 |
| --- | --- | --- |
| 0 | 200 | 成功 |
| 400 | 400 | 参数错误（含未知字段） |
| 401 | 401 | 未认证 / 凭证失效 |
| 403 | 403 | 无权限 / 跨租户越权 |
| 404 | 404 | 资源不存在 |
| 409 | 409 | 冲突（幂等复用、唯一键、无版本可回滚） |
| 422 | 422 | 语义校验失败（技能入参 Schema 不合法） |
| 429 | 429 | 限流 / 配额超限 / 队列已满 |
| 500 | 500 | 内部错误 |
| 503 | 503 | 熔断降级 / 依赖不可用 |

---

## 1. 系统

### `GET /healthz`
免认证。`{ "status": "ok", "version": "1.0.0", "uptime": 123.4, "time": "2024-06-11T10:00:00+08:00" }`

### `GET /readyz`
免认证。逐项依赖检查，任一关键项失败返回 503：

```json
[ { "name": "store", "ok": true, "detail": "可读写" },
  { "name": "skills", "ok": true, "detail": "已注册 7 个技能" },
  { "name": "engine", "ok": true, "detail": "worker=4" } ]
```

### `GET /metrics`
Prometheus 文本格式。指标：`codeagent_tasks_total{state}`、`codeagent_runs_total`、
`codeagent_runs_degraded_total`、`codeagent_skill_calls_total{status}`、`codeagent_model_calls_total`、
`codeagent_model_tokens_total`、`codeagent_cache_hit_ratio`、`codeagent_uptime_seconds`。

---

## 2. 认证与租户

### `POST /api/v1/auth/login`
```json
// 请求
{ "tenantKey": "demo", "username": "admin", "password": "admin123" }
// 响应 data
{ "token": "eyJhbGciOi...", "subject": { "tenantId": "t-demo", "admin": true, "scopes": ["admin:all"] } }
```

### `GET /api/v1/auth/profile`
返回当前主体 `Subject`。

### `GET /api/v1/tenants` / `POST /api/v1/tenants`
租户列表 / 创建租户（`{name, description, status}`，返回含默认配额的 `Tenant`）。

### `GET /api/v1/tenants/current` / `PUT /api/v1/tenants/current`
读取 / 更新当前租户（名称、状态、`quota` 五项配额上限）。

### `GET /api/v1/api-keys` / `POST /api/v1/api-keys` / `DELETE /api/v1/api-keys/{id}`
创建密钥请求 `{ "name": "ci-integration", "scopes": ["task:write","repo:read"], "ttlHours": 720 }`。
**响应中的 `plainKey` 明文仅返回一次**；列表接口只返回 `keyPrefix`，永不返回明文与哈希。

### `GET /api/v1/credentials` / `POST /api/v1/credentials` / `DELETE /api/v1/credentials/{id}`
创建凭证 `{ "name": "gitlab-token", "type": "token", "username": "ci", "secret": "glpat-xxx" }`。
密钥以 AES-GCM 加密落库，响应与日志永不回显明文。

---

## 3. 仓库（③ 仓库分组索引层）

### `GET /api/v1/repos?page=1&pageSize=20&keyword=order`
分页返回 `Repository`（含 `matchRules` 匹配规则，供堆栈驱动匹配使用）。

### `POST /api/v1/repos`
```json
{
  "name": "订单服务", "key": "order-service",
  "url": "https://git.acme.internal/mall/order-service.git",
  "defaultBranch": "main", "provider": "gitlab",
  "layer": "service", "language": "java",
  "credentialId": "cred-xxx",
  "matchRules": {
    "packagePrefixes": ["com.acme.order"],
    "pathPrefixes": ["src/main/java/com/acme/order"],
    "artifactNames": ["order-service.jar"],
    "hostPatterns": ["order-svc"],
    "endpointPatterns": ["/api/order/**"],
    "keywords": ["订单"]
  }
}
```

### `GET|PUT|DELETE /api/v1/repos/{id}`
详情（含所属分组）/ 更新 / 删除（同时清理分组成员关系）。

### `POST /api/v1/repos/{id}/probe`
连通性探测与版本解析：`{ "ref": "main" }` → `{ "ok": true, "commit": "6e1155fd…", "ref": "main", "durationMs": 812 }`。
探测失败返回 503 并附带已脱敏的原因；**不影响索引维护**，任务执行时才真正需要解析成功。

---

## 4. 业务分组（4.2 仓库分组聚合机制）

### `GET /api/v1/groups` / `POST /api/v1/groups`
```json
// 创建
{
  "group": { "name": "商城核心域", "key": "mall-core", "description": "前端+网关+订单/支付/库存",
             "entryRepositoryIds": ["repo-web-mall"] },
  "members": [ { "repositoryId": "repo-web-mall", "order": 1 },
               { "repositoryId": "repo-order", "order": 3, "note": "故障主责服务" } ]
}
```

### `GET|PUT|DELETE /api/v1/groups/{id}`
详情返回 `{ group, members }`，`members[].repo` 为仓库详情（按 `order` 升序，即链路顺序）。

### `GET /api/v1/groups/{id}/impact`
影响面分析：
```json
{ "repoCount": 6, "byLayer": {"frontend":1,"gateway":1,"service":3,"library":1},
  "byLanguage": {"java":4,"go":1,"typescript":1},
  "entryRepos": ["web-mall","api-gateway"], "sharedLibs": ["common-lib"],
  "riskScore": 0.62, "riskNotes": ["跨 4 个分层，链路较长", "含公共库，变更影响面广"] }
```

---

## 5. 任务（④ Agent 调度核心层，核心接口）

### `POST /api/v1/tasks`
请求体 = `CreateTaskRequest`：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `mode` | `single_repo` \| `group` | 否 | 留空按 `repoId`/`groupId` 自动推断 |
| `repoId` | string | 单仓库模式必填 | 目标仓库 ID |
| `groupId` | string | 分组模式必填 | 业务分组 ID |
| `ref` | string | **强烈建议** | 线上报错对应的 commit/tag/分支；留空用默认分支 HEAD |
| `refs` | map | 否 | 分组模式按仓库指定版本 |
| `source` | `alert`\|`ide`\|`cicd`\|`manual`\|`openapi` | 否 | 触发来源，默认 `manual` |
| `title` | string | 否 | 留空用堆栈首行截断生成 |
| `stacktrace` | string | **是** | 原始异常堆栈（≤ 200KB） |
| `logs` | string | 否 | 附加日志 |
| `entryFiles` | string[] | 否 | 已知嫌疑文件 |
| `idempotencyKey` | string | 否 | 幂等键；留空由 租户+模式+目标+ref+堆栈 哈希生成 |
| `priority` | int | 否 | 0-9，越大越优先 |
| `environment` | string | 否 | `prod`/`staging`/`dev` |
| `autoVerify` | bool | 否 | 是否沙箱验证，默认 `true` |
| `callbackUrl` | string | 否 | 终态回调地址 |

**响应**：`201` + 完整 `TaskRun`（含 `id`、`state`、`taskId`、`pinnedCommits` 等）。

```bash
curl -X POST http://127.0.0.1:8080/api/v1/tasks \
  -H "X-API-Key: $CA_KEY" -H 'Content-Type: application/json' \
  -d @deploy/sample-task-group.json
```

### `GET /api/v1/tasks?page=&pageSize=&keyword=`
逻辑任务列表（`Task`）。

### `GET /api/v1/tasks/{id}`
任务详情，含最新运行摘要。

### `GET /api/v1/runs?state=needs_review&keyword=&page=`
执行记录列表。`state` 取值：`queued|analyzing|repairing|verifying|succeeded|needs_review|failed|cancelled|degraded`。

### `GET /api/v1/runs/{runId}?includeCode=1`
**任务上下文全量快照**，是本系统最关键的读接口：

```jsonc
{
  "id": "run-xxx", "taskId": "task-xxx", "state": "succeeded", "severity": "major",
  "mode": "group", "attempt": 1,
  "resolution": [ { "repositoryId": "r1", "repoKey": "order-service", "commit": "6e1155fd…",
                    "requestedRef": "6e1155fd…", "resolvedFrom": "request",
                    "matchScore": 0.78, "matchedBy": ["堆栈包名命中 com.acme.order"] } ],
  "pinnedCommits": { "r1": "6e1155fd…" },
  "rootCause": { "summary": "…", "category": "null_pointer", "confidence": 0.72,
                 "evidence": ["OrderService.java:88"], "blastRadius": ["/api/order/detail/{id}"],
                 "severity": "major" },
  "patches": [ { "id": "p1", "repoKey": "order-service",
                 "filePath": "src/main/java/com/acme/order/service/OrderService.java",
                 "action": "modify", "risk": "low", "status": "proposed",
                 "rationale": "为 order 增加空值守卫，避免 NPE",
                 "touchedLines": [{"start": 36, "end": 41}],
                 "unifiedDiff": "diff --git a/… b/…\n--- a/…\n+++ b/…\n@@ …" } ],
  "verification": { "passed": true, "applyResult": "applied", "durationMs": 1240,
                    "degraded": false,
                    "checks": [ { "name": "static_syntax", "passed": true, "durationMs": 3 },
                                { "name": "go_parse", "passed": true, "skipped": true,
                                  "skipReason": "非 Go 文件" } ] },
  "usage": { "filesLoaded": 4, "codeChars": 18234, "skillCalls": 5, "modelCalls": 2,
             "totalTokens": 5210, "cacheHits": 3 },
  "warnings": [], "reportId": "rep-xxx"
}
```

> 默认**不回传源码正文**（`EvidenceBundle.Slices[].Content`），只回传路径/行号/原因/字符数等元信息；
> 需要正文时带 `includeCode=1`。

### `POST /api/v1/runs/{runId}/cancel`
取消运行中的任务；已完成任务返回 409。

### `POST /api/v1/runs/{runId}/retry`
基于**固化的 PinnedCommits** 重跑（保证同一份代码），返回新 `TaskRun`（`attempt` 递增）。

### `POST /api/v1/runs/{runId}/feedback`
```json
{ "feedback": "补丁没有处理库存服务超时的情况，请同时为 InventoryClient 增加超时与降级返回。" }
```
反馈会注入根因推理与补丁合成阶段的模型提示，用于修复不收敛时的定向迭代，返回新 `TaskRun`。

### `GET /api/v1/runs/{runId}/events`（SSE）
`Content-Type: text/event-stream`。连接建立后先补发历史事件，再持续推送：

```
data: {"seq":12,"type":"stage","stage":"code_load","message":"已加载 4 个故障关联文件","runId":"run-xxx","at":"…"}

data: {"seq":13,"type":"patch.proposed","payload":[{"repoKey":"order-service","risk":"low"}],"at":"…"}

: keepalive
```

事件类型：`task.created` / `task.state` / `stage` / `skill.call` / `model.call` / `source.fetch` /
`repo.switch` / `patch.proposed` / `verification` / `report.ready` / `log` / `task.terminal`。

### `GET /api/v1/runs/{runId}/skill-calls` / `GET /api/v1/runs/{runId}/model-calls`
技能调用轨迹 / 模型推理轨迹（分页）。用于复盘"哪个技能在哪个阶段失败、模型是否兜底"。

---

## 6. 报告

### `GET /api/v1/reports?page=&pageSize=&keyword=`
报告列表。 ### `GET /api/v1/reports/{id}`
报告详情（结构化字段 + `markdown`）。 ### `GET /api/v1/reports/{id}/markdown`
纯文本 Markdown，`Content-Type: text/markdown; charset=utf-8`，可直接贴工单/PR；`?download=1` 触发下载。

---

## 7. Skill 与模型治理

### `GET /api/v1/skills?category=repair`
返回 `[{ manifest, health }]`：技能清单（含 `inputSchema`/`outputSchema`/超时/重试/熔断阈值/兜底链）
与实时健康度（`circuitState`、调用数、失败数、平均延迟）。

### `GET /api/v1/skills/{name}` / `POST /api/v1/skills/{name}/status` / `POST /api/v1/skills/{name}/rollback`
```json
// 灰度发布
{ "version": "1.1.0", "status": "canary", "canaryPercent": 20 }
```
回滚到上一可用版本；无可回滚版本返回 409。

### `GET /api/v1/models` / `GET /api/v1/models/stats`
```json
{ "enabled": true,
  "targets": [ { "provider": "deepseek", "model": "deepseek-chat", "tier": "strong" } ],
  "health":  [ { "provider": "deepseek", "model": "deepseek-chat", "tier": "strong",
                 "healthy": true, "circuitState": "closed", "avgLatencyMs": 1840, "totalCalls": 42 } ] }
```
`/models/stats` 返回 `ModelStats`（调用数、失败数、兜底次数、结构化修复次数、Token 累计、平均延迟、按模型/层级分布）。

---

## 8. 可观测

### `GET /api/v1/observability/summary`
`ObservabilitySummary`：任务/运行总数、成功/待复核/失败/降级数、修复率 `fixRate`、平均耗时、
技能调用数与失败率、模型调用/Token/兜底、缓存命中、仓库切换次数、Top 技能、Top 模型、
状态分布、根因分类分布、最近失败运行。

### `GET /api/v1/observability/audits?state=skill&keyword=&page=`
审计事件分页。分类：`task` / `repo` / `group` / `skill` / `model` / `source` / `patch` / `auth` / `quota`。

---

## 9. WebSocket 实时通道

### `GET /api/v1/ws?token=<jwt 或 api key>`
标准 RFC6455 握手（服务端自实现，无第三方依赖）。连接后：
1. 推送全局事件流（按 token 所属租户过滤）；
2. 每 30s 服务端 ping，客户端可回 `"ping"` 文本帧，服务端回 `"pong"`；
3. 最多 500 个并发连接，超出以关闭码 1013 拒绝。

消息体即 `domain.Event` JSON，与控制台 SSE 完全一致，便于统一渲染。

---

## 10. 典型集成

### IDE / 本地插件（单文件快速修复）
```bash
curl -X POST $CODEAGENT/api/v1/tasks -H "X-API-Key: $CA_KEY" -H 'Content-Type: application/json' -d '{
  "mode":"single_repo","repoId":"repo-order","ref":"'"$(git rev-parse HEAD)"'",
  "source":"ide","stacktrace":"'"$(cat /tmp/err.log)"'"}'
```

### 告警平台（自动溯源 + 修复建议）
上报堆栈 + `environment: "prod"` + `source: "alert"` + `callbackUrl` 回传报告到告警工单。

### CI/CD 卡点
`source: "cicd"` + `idempotencyKey: $CI_PIPELINE_ID` + `ref: $GIT_COMMIT`；
拿到 `state=succeeded` 与 `patches` 后按策略自动开 PR 或仅告警。
