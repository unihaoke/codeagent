# AI代码分析接口文档

> 版本：v1（2026-09-23）
> 适用对象：希望将「异常栈」自动转为「根因 + 补丁 + 报告」并推送告警（如飞书）的第三方服务。
> 能力概述：第三方只需提供 **git 地址** 或 **主机标识** 即可定位仓库，无需感知系统内部 `repoId`；支持 **同步等待结果** 与 **异步回调** 两种接入方式。

---

## 1. 接入概览

| 模式 | 接口 | 行为 | 适用场景 |
|------|------|------|----------|
| 同步 | `POST /api/v1/openapi/analyze` | 提交后阻塞等待分析完成，直接返回结构化结果 | 调用方需要立即拿到根因/补丁（如实时告警富化） |
| 异步 | `POST /api/v1/openapi/tasks` | 提交即返回 `runId`，分析完成时回调 `callbackUrl` | 分析耗时较长、调用方不想阻塞（如监控告警风暴） |

两种模式共享相同的**仓库定位**与**分析结果结构**。

---

## 2. 鉴权

所有接口使用 **API Key** 鉴权（适合第三方服务长期持有）：

```http
X-API-Key: <your_api_key>
```

- 密钥在服务端「凭证管理」中创建，绑定到具体租户与主体。
- 所需权限：`task:write`（`admin:all` 可通配放行）。缺少权限返回 `403`。
- 未携带或无效密钥返回 `401`。

---

## 3. 公共约定

- **Base URL**：`https://<你的服务域名>`（由部署决定，下文以 `<BASE>` 代指）。
- **请求头**：`Content-Type: application/json`。
- **租户隔离**：API Key 隐式绑定租户，第三方无需在 body 中传租户 ID。
- **幂等**：建议携带 `idempotencyKey`；相同 key 在有效期内重复提交会复用同一分析任务，避免告警风暴触发多次分析。
- **错误码**：

| HTTP | 含义 | 处理建议 |
|------|------|----------|
| 400 | 请求参数不合法（如缺 `stacktrace`、异步缺 `callbackUrl`、栈过大） | 检查请求体 |
| 401 | 未认证（缺/错 API Key） | 补充 `X-API-Key` |
| 403 | 无权限 | 申请 `task:write` 权限 |
| 404/422 | 仓库定位失败（无匹配仓库） | 先在控制台注册仓库并配置 `matchRules` |
| 429 | 配额/队列超限 | 退避后重试 |
| 202 | 同步超时（结果未就绪）或异步受理 | 见各接口说明 |
| 200 | 成功 | — |

- **栈大小限制**：`stacktrace` 单字段上限 `maxStacktraceBytes`（默认 64KB），超出返回 `400`。

---

## 4. 仓库定位（repoLocator）

调用方通过三种方式之一指定目标仓库（互斥，**优先级 `repoId` > `groupId` > `repoLocator`**）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `repoLocator.gitUrl` | string | 仓库 git 地址，如 `https://git.x/order.git` |
| `repoLocator.host` | string | 主机/服务标识，如 `order-svc`、`order.internal` |
| `repoId` | string | 系统内部仓库 ID 或 `repoKey`（已知时直接传，最精确） |
| `groupId` | string | 分组 ID，走「分组多仓」分析模式 |

若只给 `repoLocator`，服务端在租户内仓库列表按以下规则**打分取最高分**：

| 命中规则 | 分值 |
|---|---|
| `gitUrl` 精确匹配 `repo.url` | 100 |
| 命中 `matchRules.hostPatterns`（如 `order-svc`） | 80 |
| `gitUrl` 片段命中 `repo.url` | 70 |
| 命中 `matchRules.endpointPatterns` | 60 |
| 命中 `matchRules.keywords` | 50 |
| `host`/`gitUrl` 片段命中 `repo.key`/`repo.name` | 40 |

- 无匹配 → 返回 `422`，提示「请先在控制台注册仓库并配置 `matchRules`」。
- 多个仓库同分 → 取首个，并在响应 `warnings` 中提示实际选用了哪个 `key`。

> 推荐：控制台注册仓库时填写 `hostPatterns`/`keywords`，可让第三方仅凭 `host` 稳定命中。

---

## 5. 接口一：同步分析 `POST /api/v1/openapi/analyze`

提交异常栈并**阻塞等待**分析完成，直接返回结构化结果。

### 5.1 请求

**Header**
```http
POST /api/v1/openapi/analyze
Content-Type: application/json
X-API-Key: <your_api_key>
```

**Body 字段**

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `repoLocator` | object | 三选一 | `{ "gitUrl"?, "host"? }` |
| `repoId` | string | 三选一 | 内部仓库 ID / repoKey |
| `groupId` | string | 三选一 | 分组 ID（分组多仓模式） |
| `stacktrace` | string | 是 | 异常栈文本（≤ maxStacktraceBytes） |
| `logs` | string | 否 | 关联日志，辅助定位 |
| `entryFiles` | string[] | 否 | 嫌疑入口文件，缩小范围 |
| `environment` | string | 否 | 环境标识，如 `prod` |
| `autoVerify` | bool | 否 | 是否自动跑沙箱验证（默认按系统配置） |
| `priority` | int | 否 | 任务优先级 |
| `idempotencyKey` | string | 否 | 幂等键，防重复分析 |
| `timeout` | int | 否 | 同步等待上限（秒），默认 120，上限 300 |

### 5.2 请求示例

```bash
curl -X POST '<BASE>/api/v1/openapi/analyze' \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <your_api_key>' \
  -d '{
    "repoLocator": { "gitUrl": "https://git.x/order.git" },
    "stacktrace": "java.lang.NullPointerException\n\tat com.acme.order.OrderService.create(OrderService.java:42)",
    "logs": "trace_id=abc123 timeout=2000ms",
    "environment": "prod",
    "idempotencyKey": "alert-2026-001",
    "timeout": 120
  }'
```

### 5.3 响应：分析完成（HTTP 200）

| 字段 | 类型 | 说明 |
|------|------|------|
| `runId` | string | 运行 ID |
| `taskId` | string | 任务 ID |
| `status` | string | 终态：`succeeded` / `needs_review` / `failed` / `degraded` / `cancelled` |
| `severity` | string | 严重程度 `critical`/`high`/`medium`/`low`/`unknown` |
| `repo` | object | `{ id, key, name, url }` 命中的仓库 |
| `rootCause` | object | 根因分析（见 §5.5） |
| `patches` | object[] | 候选补丁（见 §5.5） |
| `verification` | object | `{ passed, checks[], errors[] }` 沙箱验证结果 |
| `reportId` | string | 报告 ID（可后续拉取） |
| `reportUrl` | string | 报告跳转链接（由 `PUBLIC_URL` 生成） |
| `markdown` | string | 完整报告 Markdown，可直接贴飞书/工单 |
| `elapsedMs` | int | 耗时（毫秒） |
| `usage` | object | 模型 token 用量 |
| `warnings` | string[] | 定位/分析过程提示 |
| `degraded` | bool | 是否降级（部分能力不可用） |
| `error` | string | 失败原因（仅 `failed`/`degraded` 时有值） |

**200 响应示例**
```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "succeeded",
  "severity": "critical",
  "repo": { "id": "r1", "key": "order-service", "name": "订单服务", "url": "https://git.x/order.git" },
  "rootCause": {
    "summary": "OrderService.create 在未判空的情况下解引用了 order 对象",
    "category": "null_pointer",
    "confidence": 0.92,
    "detail": "入参 order 在并发场景下可能为 null …",
    "evidence": [],
    "blastRadius": []
  },
  "patches": [
    {
      "repositoryId": "r1",
      "repoKey": "order-service",
      "filePath": "service/order.go",
      "action": "modify",
      "unifiedDiff": "--- a/service/order.go\n+++ b/service/order.go\n@@ …",
      "rationale": "增加空值保护，避免 NPE"
    }
  ],
  "verification": { "passed": true, "checks": [], "errors": [] },
  "reportId": "rep_01HX...",
  "reportUrl": "https://console.x/r/rep_01HX...",
  "markdown": "# 分析报告 …",
  "elapsedMs": 12345,
  "usage": {},
  "warnings": [],
  "degraded": false,
  "error": ""
}
```

### 5.4 响应：同步超时（HTTP 202）

当分析在 `timeout` 内未完成，返回 `202` 并提示轮询或使用回调：

```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "running",
  "pollUrl": "/api/v1/runs/run_01HX...",
  "message": "分析未在超时内完成，请轮询 pollUrl 或提供 callbackUrl 等待回调"
}
```

> 若调用方在请求中携带了 `callbackUrl`，即使同步超时，分析完成后仍会异步回调该地址（见 §6）。

---

## 6. 接口二：异步提交 `POST /api/v1/openapi/tasks`

提交即返回受理结果，分析完成（终态）时**回调** `callbackUrl`。

### 6.1 请求

Body 与同步接口基本一致，但 **`callbackUrl` 为必填**（否则返回 `400`）。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `callbackUrl` | string | 是 | 终态回调地址，服务端将以 `POST` 推送结构化结果 |
| 其余字段 | — | — | 同 §5.1（不含 `timeout`） |

### 6.2 请求示例

```bash
curl -X POST '<BASE>/api/v1/openapi/tasks' \
  -H 'Content-Type: application/json' \
  -H 'X-API-Key: <your_api_key>' \
  -d '{
    "repoLocator": { "host": "order-svc" },
    "stacktrace": "java.lang.NullPointerException …",
    "callbackUrl": "https://hook.your-svc.com/codeagent/callback"
  }'
```

### 6.3 受理响应（HTTP 202）

```json
{
  "runId": "run_01HX...",
  "taskId": "task_01HX...",
  "status": "queued",
  "acceptTime": "2026-09-23T10:00:00Z"
}
```

### 6.4 终态回调

分析进入终态后，服务端以 **`POST {callbackUrl}`** 推送结果：

- **Content-Type**：`application/json`
- **重试**：回调失败采用指数退避重试**最多 3 次**（间隔 1s / 2s）；全部失败会记录审计（`action=callback.failed`），调用方可通过 `GET /api/v1/runs/{runId}` 主动拉取。
- **签名**：当前**不强制** HMAC 签名；如需强约束请在 `callbackUrl` 侧自行校验来源 IP 或加自定义 token 查询参数。

**回调报文结构**

| 字段 | 类型 | 说明 |
|------|------|------|
| `runId` | string | 运行 ID |
| `state` | string | 终态：`succeeded` / `needs_review` / `failed` / `degraded` / `cancelled` |
| `reportId` | string | 报告 ID |
| `severity` | string | 严重程度 |
| `summary` | string | 根因摘要 |
| `tenantId` | string | 租户 ID |
| `taskId` | string | 任务 ID |
| `attempt` | int | 重试次数 |
| `error` | string | 失败原因（可选） |
| `rootCause` | object | 完整根因（同 §5.5） |
| `patches` | object[] | 补丁列表（同 §5.5） |
| `reportUrl` | string | 报告跳转链接 |

**回调报文示例**
```json
{
  "runId": "run_01HX...",
  "state": "succeeded",
  "reportId": "rep_01HX...",
  "severity": "critical",
  "summary": "OrderService.create 未判空解引用",
  "tenantId": "t-demo",
  "taskId": "task_01HX...",
  "attempt": 1,
  "error": "",
  "rootCause": {
    "summary": "OrderService.create 在未判空的情况下解引用了 order 对象",
    "category": "null_pointer",
    "confidence": 0.92,
    "detail": "…",
    "evidence": [],
    "blastRadius": []
  },
  "patches": [
    {
      "repositoryId": "r1",
      "repoKey": "order-service",
      "filePath": "service/order.go",
      "action": "modify",
      "unifiedDiff": "--- a/service/order.go\n+++ …",
      "rationale": "增加空值保护"
    }
  ],
  "reportUrl": "https://console.x/r/rep_01HX..."
}
```

> 飞书渲染建议：用 `markdown` + `reportUrl` 生成「查看完整报告」按钮；用 `rootCause.summary`/`category` 作为卡片标题，`patches[].filePath` 列出受影响文件。若异步回调未携带 `markdown`，可用 `reportUrl` 在飞书卡片中以链接形式展示。

---

## 7. 配置说明（服务端）

- **`PUBLIC_URL`**（环境变量）或 `server.publicUrl`（JSON 配置）：对外可访问的基础地址（含协议与端口，如 `https://console.x`）。
  - 用于生成 `reportUrl`（`{PUBLIC_URL}/r/{reportId}`），让第三方在飞书/工单中点击跳转到完整报告。
  - 未配置时 `reportUrl` 为空，但 `reportId` 仍返回，调用方可自行拼装。

---

## 8. 对接检查清单

1. 申请 API Key，确认具备 `task:write` 权限。
2. 在控制台注册目标仓库，并填写 `hostPatterns`/`keywords`，使第三方仅凭 `host` 即可命中。
3. 同步场景：调用 `/api/v1/openapi/analyze`，设置合理 `timeout`（建议 ≤120）。
4. 异步场景：调用 `/api/v1/openapi/tasks` 并填 `callbackUrl`，做好 `POST` 接收与失败重放（轮询 `GET /api/v1/runs/{runId}`）。
5. 服务端配置 `PUBLIC_URL`，确保回调/响应中的 `reportUrl` 可点击。

---

## 9. 相关参考

- 设计决策与落地记录：`docs/openapi-design.md`
- 仓库匹配规则字段：`domain.RepoMatchRules`（hostPatterns / endpointPatterns / keywords / …）
- 根因与补丁模型：`domain.RootCause` / `domain.Patch`
