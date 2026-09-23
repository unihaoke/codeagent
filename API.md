# CodeAgent 第三方接口文档

> 适用版本：`dev`（响应 `version` 字段）。所有业务接口挂载于前缀 `/api/v1`，健康检查/指标同时挂载根路径（`/healthz`、`/readyz`、`/metrics`）。
> 本文档基于后端 `backend/internal/api` 路由与 handler 整理，字段以代码为准。

---

## 1. 通用约定

### 1.1 认证（Authentication）
所有业务接口需要认证（除 `/auth/login`、`/healthz`、`/readyz`、`/metrics` 外）。支持两种方式，二选一放在请求头：

| 方式 | 请求头 | 说明 |
|------|--------|------|
| API Key | `X-API-Key: <plainKey>` | 由 `POST /api/v1/api-keys` 创建，明文仅返回一次 |
| JWT | `Authorization: Bearer <token>` | 由 `POST /api/v1/auth/login` 登录获取 |

WebSocket / SSE 因浏览器限制无法自定义头，改用查询参数：`?token=<jwt>` 或 `?apiKey=<plainKey>`。

认证失败后返回 `401`；权限不足返回 `403`。

### 1.2 统一响应信封
所有 JSON 响应包裹在统一信封中：

```json
{ "code": 0, "message": "", "requestId": "req_xxx", "data": { } }
```

- `code == 0` 表示成功；非 0 为业务错误（`message` 描述，`requestId` 用于排障）。
- 列表类接口 `data` 为 `{ "items": [...], "total": N, "page": 1, "pageSize": 20 }`（部分接口额外携带聚合字段，见各端点）。
- 部分成功接口（如 `POST /tasks`、`POST /repos`）直接返回资源对象（`data` 即资源），HTTP 状态为 `201`。

### 1.3 分页与过滤
列表（GET）接口支持查询参数：

| 参数 | 说明 |
|------|------|
| `page` | 页码，从 1 开始，默认 1 |
| `pageSize` | 每页条数，默认 20 |
| `keyword` | 关键字模糊匹配（名称 / key / 描述等） |
| `state` | 按状态过滤（各资源含义不同，见端点） |
| `category` | 仅 `/skills`，按技能分类过滤 |
| `sort` | 排序字段（可选，视资源支持） |

### 1.4 租户隔离
所有数据按 `subject.TenantID` 隔离。登录默认租户为 `t-demo`。跨租户访问返回 `404`/`403`。多租户管理操作（租户、API Key、凭证）要求 `admin:all`。

### 1.5 权限 Scope
| Scope | 含义 |
|-------|------|
| `admin:all` | 超级权限（通配所有 scope） |
| `repo:read` / `repo:write` | 仓库读 / 写 |
| `group:read` / `group:write` | 分组读 / 写 |
| `task:read` / `task:write` | 任务读 / 写（含运行） |
| `report:read` | 报告读 |
| `skill:admin` | 技能启停 / 回滚 |
| `model:read` / `model:admin` | 模型读 / AI 设置写 |
| `observability:read` | 可观测与审计只读 |

---

## 2. 认证与租户

### 2.1 登录
`POST /api/v1/auth/login` （免认证）

请求体：
```json
{ "tenantKey": "t-demo", "username": "admin", "password": "****" }
```
响应 `data`：
```json
{ "token": "<jwt>", "subject": { "tenantId": "t-demo", "tenantName": "demo",
    "username": "admin", "admin": true, "scopes": ["admin:all"] } }
```

### 2.2 当前主体
`GET /api/v1/auth/profile` （需任意已认证主体）
响应 `data`：即上面的 `subject` 对象（含 `tenantId`、`scopes`、`admin` 等）。

### 2.3 租户列表
`GET /api/v1/tenants` （`admin:all`）
响应 `data`：`{ "items": [Tenant], "total": N }`。
`Tenant` 主要字段：`id`、`name`、`status`(active)、`description`、`quota`(并发/文件/Token/时长配额)、`createdAt`、`updatedAt`。

### 2.4 创建租户
`POST /api/v1/tenants` （`admin:all`）
请求体：`{ "name": "string", "description": "string?", "quota": { ... }? }`
创建成功返回 `201` + `Tenant`。

### 2.5 当前租户
`GET /api/v1/tenants/current` （已认证）
`PUT /api/v1/tenants/current` （`admin:all`）— 更新名称/描述/配额，请求体字段均可选。

### 2.6 API Key 管理
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/api-keys` | `admin:all` | 列表（脱敏，含 `keyPrefix`、`scopes`、`expiresAt`、`revoked`） |
| POST | `/api/v1/api-keys` | `admin:all` | 创建，请求 `{ "name", "scopes?[]", "ttlHours?":0 }`；响应 `data` 含 `plainKey`（**仅此一次返回**）与 `apiKey` 视图 |
| DELETE | `/api/v1/api-keys/{id}` | `admin:all` | 吊销，响应 `{ "id", "revoked": true }` |

### 2.7 Git 凭证管理
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/credentials` | `admin:all` | 列表（**绝不含密文**，仅 `hasSecret` 布尔） |
| POST | `/api/v1/credentials` | `admin:all` | 创建，请求 `{ "name", "type": "ssh_key|token|basic|none", "username?", "secret" }`；明文仅入参，加密后落库 |
| DELETE | `/api/v1/credentials/{id}` | `admin:all` | 删除，响应 `{ "id", "deleted": true }` |

---

## 3. 仓库与分组

### 3.1 仓库
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/repos` | `repo:read` | 列表；`data` 含 `items`、`total`、`aggregates`(按语言/分层统计) |
| POST | `/api/v1/repos` | `repo:write` | 注册仓库（见下） |
| GET | `/api/v1/repos/{id}` | `repo:read` | 详情；`data` 含 `repo`(含 `groups`、`summary`)、`cache`(源码缓存统计) |
| PUT | `/api/v1/repos/{id}` | `repo:write` | 更新（字段级可选） |
| DELETE | `/api/v1/repos/{id}` | `repo:write` | 删除，响应 `{ "id", "deleted": true }` |
| POST | `/api/v1/repos/{id}/probe` | `repo:write` | 连通性探测，可选 `?ref=`；响应 `{ "ok", "commit", "elapsedMs", "probedAt" }` |

创建仓库请求体：
```json
{
  "name": "订单服务", "key?": "order-service", "url": "https://git.xxx/order.git",
  "defaultBranch?": "main", "provider?": "git|github|gitlab|gitee",
  "layer?": "frontend|gateway|service|middleware|library|unknown",
  "language?": "go", "credentialId?": "cred_xxx", "status?": "active",
  "matchRules?": { "packagePrefixes":[], "pathPrefixes":[], "artifactNames":[],
                    "hostPatterns":[], "endpointPatterns":[], "keywords":[] },
  "accessPolicy?": ["repoId..."]
}
```
`Repository` 主要字段：`id`、`tenantId`、`key`、`name`、`url`、`defaultBranch`、`provider`、`layer`、`language`、`credentialId`、`status`、`matchRules`、`accessPolicy`、`createdAt`、`updatedAt`。

### 3.2 分组
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/groups` | `group:read` | 列表；`data.items[]` 为 `{ "group", "summary": {memberCount, byLayer, byLanguage} }` |
| POST | `/api/v1/groups` | `group:write` | 创建（见下） |
| GET | `/api/v1/groups/{id}` | `group:read` | 详情；`data` 含 `group`、`members[]`、`summary` |
| PUT | `/api/v1/groups/{id}` | `group:write` | 更新 |
| DELETE | `/api/v1/groups/{id}` | `group:write` | 删除 |
| GET | `/api/v1/groups/{id}/impact` | `group:read` | 影响面分析（见下） |

创建/更新分组请求体（兼容嵌套与扁平两种写法）：
```json
{
  "group": { "name": "商城核心", "key?": "mall-core", "description?": "",
             "status?": "active|disabled", "entryRepositoryIds?": ["r1","r2"] },
  "members": [ { "repositoryId": "r1", "order?": 0, "note?"": "" } ]
}
```
影响面分析响应 `data`：
```json
{ "repoCount": 3, "byLayer": {...}, "byLanguage": {...},
  "entryRepos": ["r1"], "sharedLibs": ["lib-a"],
  "riskScore": 6.3,
  "riskFormula": "riskScore = repoCount×1.0 + layerCount×1.5 + entryRepos×0.5 + sharedLibs×0.8（保留两位小数）",
  "notes?": [] }
```

---

## 4. 任务与运行（核心）

### 4.1 提交任务（最重要）
`POST /api/v1/tasks` （`task:write`）→ `201` + `TaskRun`

请求体 `CreateTaskRequest`：
```json
{
  "mode": "single_repo | group",          // 二选一由 repoId/groupId 自动推断
  "repoId": "repo_xxx",                   // single_repo 必填
  "groupId": "grp_xxx",                   // group 必填
  "ref": "main 或精确 commit",            // 报错对应版本
  "refs": { "repoId": "commit" },         // 分组模式各仓库版本
  "source": "alert|ide|cicd|manual|openapi",
  "title?": "人读标题（缺省取堆栈首行）",
  "stacktrace": "原始异常堆栈（必填，核心线索，≤200KB）",
  "logs?": "附加日志（≤64KB）",
  "entryFiles?": ["service/order.go"],
  "idempotencyKey?": "同租户同键复用未完成任务",
  "priority?": 0,                         // 0-9
  "environment?": "prod|staging|dev",
  "autoVerify?": true,                    // 是否沙箱验证
  "callbackUrl?": "异步回调地址"
}
```
> `tenantId`、`requestId` 由接入层从凭证注入，**调用方勿传**。响应 `data` 为本次创建的 `TaskRun`。

### 4.2 任务列表 / 详情
`GET /api/v1/tasks` （`task:read`）→ 分页 `Task` 列表。
`GET /api/v1/tasks/{id}` （`task:read`）→ `data`：`{ "task": Task, "latestRun?": TaskRun }`。
`Task` 主要字段：`id`、`title`、`mode`、`groupId?`、`source`、`idempotencyKey`、`latestRunId`、`runCount`、`createdAt`、`updatedAt`。

### 4.3 运行列表 / 详情
`GET /api/v1/runs` （`task:read`）→ 分页 `TaskRun` 列表（支持 `?state=`、`?keyword=`）。
`GET /api/v1/runs/{runId}` （`task:read`）→ `data` 为运行全量详情（见下）；支持 `?includeCode=1` 附带证据/补丁源码全文。

`TaskRun` 主要字段：
```json
{
  "id", "taskId", "tenantId", "attempt", "mode", "state", "severity?",
  "title", "environment?", "stacktrace", "logs?",
  "requestedRef?", "repoIds?":[], "entryFiles?":[],
  "resolution?": [ RepoResolved ],          // 仓库→commit 锁定
  "pinnedCommits?": { "repoId": "commit" },
  "rootCause?": { "summary","category","detail","confidence":0.0-1.0,"evidence":[] },
  "patches?": [ Patch ],                    // oldSnippet/newSnippet/unifiedDiff
  "verification?": { "passed":bool, "checks":[], "errors":[] },
  "reportId?", "usage": { "totalTokens":0, "skillCalls":0, "modelCalls":0, "durationMs":0 },
  "degraded": false, "warnings?":[], "error?",
  "createdAt","startedAt?","endedAt?","updatedAt"
}
```
`state` 枚举：`queued` / `analyzing` / `repairing` / `verifying` / `succeeded` / `needs_review` / `failed` / `cancelled` / `degraded`。
`category`（根因分类）示例：`null_pointer` / `timeout` / `syntax` / `concurrency` / `dependency` 等。

运行详情 `data` 还内嵌：`resolution`、`patches`、`verification`、`usage`、`evidence`(证据切片元信息)、`skillCalls[]`、`modelCalls[]`、`report?`、以及 `degraded`、`elapsedMs`、`includeCode`。

### 4.4 运行控制
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| POST | `/api/v1/runs/{runId}/cancel` | `task:write` | 取消；响应 `{ "runId", "cancelled": true }` |
| POST | `/api/v1/runs/{runId}/retry` | `task:write` | 基于固化上下文重跑；响应新 `TaskRun` |
| POST | `/api/v1/runs/{runId}/feedback` | `task:write` | 携带人工反馈重跑；请求 `{ "feedback": "string" }`；响应新 `TaskRun` |
| GET | `/api/v1/runs/{runId}/events` | `task:read` | 运行时事件流（SSE，见 §10） |
| GET | `/api/v1/runs/{runId}/skill-calls` | `task:read` | 技能调用轨迹（分页 `SkillCall`） |
| GET | `/api/v1/runs/{runId}/model-calls` | `task:read` | 模型推理轨迹（分页 `ModelCall`） |

`SkillCall` 主要字段：`id`、`runId`、`skill`、`version`、`stage`、`status`、`inputDigest`、`outputDigest`、`error?`、`attempts`、`durationMs`、`fromFallback`、`output`、`startedAt`。`status`：`ok`/`failed`/`timeout`/`blocked`/`skipped`/`fallback`。
`ModelCall` 主要字段：`id`、`runId`、`provider`、`model`、`tier`、`status`、`attempt`、`fallbackFrom?`、`promptTokens`、`completionTokens`、`totalTokens`、`durationMs`、`error?`、`schemaValid`、`startedAt`。

---

## 5. 报告

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/reports` | `report:read` | 分页 `Report` 列表 |
| GET | `/api/v1/reports/{id}` | `report:read` | 报告详情（含 `markdown` 正文） |
| GET | `/api/v1/reports/{id}/markdown` | `report:read` | 纯 `text/markdown` 响应；`?download=1` 触发下载 |

`Report` 主要字段：`id`、`tenantId`、`runId`、`taskId`、`title`、`markdown`、`createdAt`。

---

## 6. 技能

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/skills` | `repo:read` | 列表；`data`：`{ "items":[{ "manifest", "health?" }], "total", "category", "enabled" }`；支持 `?category=` |
| GET | `/api/v1/skills/{name}` | `repo:read` | 详情；`data`：`{ "manifest", "inputSchema", "outputSchema", "health?" }` |
| POST | `/api/v1/skills/{name}/status` | `skill:admin` | 启停/灰度（见下） |
| POST | `/api/v1/skills/{name}/rollback` | `skill:admin` | 回滚到上一可用版本；响应 `{ "skill", "version", "message" }` |

技能启停请求体：
```json
{ "version?": "1.0.0", "status": "enabled|disabled|canary|deprecated", "canaryPercent?": 0-100 }
```
`SkillManifest` 主要字段：`name`、`version`、`title`、`description`、`category`、`status`、`timeout`、`retries`、`failureThreshold`、`cooldownSeconds`、`fallbackSkills[]`、`builtin`、`priority`、`tags[]`、`inputSchema`、`outputSchema`。内置技能 7 个：`stacktrace_parse`、`code_ast_analyze`、`error_localize`、`cross_repo_trace`、`code_patch_synthesize`、`dependency_repair`、`patch_verify`。

---

## 7. 模型 / AI 设置

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/models` | `model:read` | 模型提供方与健康度；`data` 含 `enabled`、`targets[]`、`health[]`、`items[]` |
| GET | `/api/v1/models/stats` | `model:read` | 模型层统计；`data`：`{ "enabled", "stats": ModelStats }` |
| GET | `/api/v1/models/config` | `model:admin` | AI 设置（脱敏）；`data` 见下 |
| PUT | `/api/v1/models/config` | `model:admin` | 保存并热重载（见下） |
| POST | `/api/v1/models/test` | `model:admin` | 连通性测试（不保存） |

`GET /models/config` 响应 `data`：
```json
{ "enabled": true, "secretReady": true, "hotReload": true,
  "items": [ providerSetting ], "total": N,
  "kinds": ["openai","deepseek","azure","ollama","openai_compatible","mock"],
  "tiers": ["light","strong","fallback"], "persisted": true }
```
`providerSetting`：`name`、`kind`、`baseUrl`、`models[]`、`tier`、`maxTokens`、`temperature`、`weight`、`enabled`、`hasKey`、`keyHint?`(末4位掩码)、`source`(store/config)、`updatedAt?`。

保存 AI 设置 `PUT /models/config` 请求体（**全量替换**，缺失即删除）：
```json
{ "items": [ {
    "name": "deepseek-flash", "kind": "deepseek", "baseUrl": "https://...",
    "apiKey": "sk-...（留空则保留旧密钥）", "models": ["deepseek-chat"],
    "tier": "strong", "maxTokens?": 8192, "temperature?": 0.1,
    "weight?": 1, "enabled": true
} ] }
```
响应 `data`：`{ "items":[providerSetting], "total", "loaded":N, "skipped":N, "notice" }`。

连通性测试 `POST /models/test` 请求体：`{ "name","kind","baseUrl","apiKey?","models":[],"testModel?","tier?","maxTokens?","temperature?" }`；响应 `{ "ok":bool, "durationMs", "model", "message" }`。

---

## 8. 可观测与审计

| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/api/v1/observability/summary` | `observability:read` | 可观测汇总（见下） |
| GET | `/api/v1/observability/audits` | `observability:read` | 审计事件（分页 `AuditEvent`，`?state=` 按分类过滤） |

`observability/summary` 响应 `data`（即 `ObservabilitySummary`）：
```json
{
  "tasksTotal": 0, "runsTotal": 0,
  "runsSucceeded": 0, "runsNeedsReview": 0, "runsFailed": 0, "runsDegraded": 0,
  "fixRate": 0.0, "avgElapsedMs": 0,
  "skillCalls": 0, "skillFailures": 0, "skillFailureRate": 0.0,
  "modelCalls": 0, "modelTokens": 0, "modelFallbacks": 0,
  "cacheHits": 0, "repoSwitches": 0,
  "topSkills": [ { "skill", "calls", "failures", "avgDurationMs" } ],
  "topModels": [ { "provider", "model", "calls", "totalTokens", "avgDurationMs" } ],
  "stateDist": { "succeeded": 0, "failed": 0, "degraded": 0 },
  "categoryDist": { "null_pointer": 0 },
  "recentFailures": [ { "runId", "taskId", "title", "state", "rootCause", "endedAt" } ]
}
```
> 统计基于当前租户下**运行记录（TaskRun）**；无任何运行时各计数与 `fixRate` 均为 0。

`AuditEvent` 主要字段：`id`、`tenantId`、`category`(task/repo/group/skill/model/source/patch/auth/quota)、`action`、`level`(info/warn/error)、`message`、`requestId`、`at`、`data?`、`runId?`、`taskId?`、`repoId?`、`commit?`、`skill?`、`model?`。

---

## 9. 系统 & 实时流

### 9.1 健康检查 / 就绪 / 指标
| 方法 | 路径 | 权限 | 说明 |
|------|------|------|------|
| GET | `/healthz`（或 `/api/v1/healthz`） | 免认证 | `{ "status":"ok", "version", "uptime", "time" }` |
| GET | `/readyz`（或 `/api/v1/readyz`） | 免认证 | `{ "status":"ready|not_ready", "checks":[{name,ok,detail}] }`；关键项失败时 `503` |
| GET | `/metrics`（或 `/api/v1/metrics`） | 免认证 | Prometheus 文本格式指标（`codeagent_tasks_total`、`codeagent_runs_total`、`codeagent_skill_calls_total`、`codeagent_model_tokens_total`、`codeagent_runs_degraded_total`、`codeagent_uptime_seconds`、`codeagent_cache_hit_ratio`、`codeagent_ws_connections` 等） |

### 9.2 WebSocket 实时事件
`GET /api/v1/ws?token=<jwt>&runId=<runId>` — 全局/单运行事件推送（`requireScope("")` 即已认证即可）。事件体为 `AgentEvent`（`type`、`tenantId`、`runId`、`taskId`、`level`、`message`、`seq`、`data`）。

### 9.3 运行事件流（SSE）
`GET /api/v1/runs/{runId}/events` （`task:read`）— `text/event-stream` 实时推送该运行事件；首帧 `: connected`，每 15s `: keepalive` 心跳，收到 `task.terminal` 类型事件后服务端主动收流。事件结构同 §9.2 的 `AgentEvent`。

---

## 10. 典型调用流程
1. `POST /auth/login` 获取 `token`。
2. 后续请求头带 `Authorization: Bearer <token>`（或用 `POST /api-keys` 创建 `X-API-Key`）。
3. （可选）`POST /repos` 注册仓库、`POST /groups` 建分组。
4. `POST /tasks` 提交排查任务（传 `stacktrace` + `repoId`/`groupId`）→ 得到 `runId`。
5. 轮询 `GET /runs/{runId}` 或订阅 `GET /runs/{runId}/events`（SSE）获取根因/补丁；
   需要时 `POST /runs/{runId}/retry` 或 `/feedback` 重跑。
6. `GET /reports/{id}/markdown` 读取最终报告；`GET /observability/summary` 看大盘统计。

> 更多字段细节以 `backend/internal/domain` 领域模型与 `backend/internal/api` handler 源码为准。
