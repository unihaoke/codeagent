# 第三方调用接入设计方案（OpenAPI：同步 / 异步+回调）

> 目标：让外部服务（监控、日志、告警平台等）以 **git 地址或主机标识** 定位仓库，传入**异常栈**，获得**结构化分析结果**，并支持 **同步等待** 与 **异步回调** 两种模式；结果用于后续推送飞书等告警。
> 本文档基于 `backend/internal` 真实代码盘点后给出，所有字段名与现有 `domain` 模型一致，可直接落地。

---

## 1. 需求澄清

| 维度 | 说明 |
|------|------|
| 调用方 | 其他服务（如 Prometheus/告警中心、日志平台），非本系统控制台 |
| 仓库定位 | **git 地址**（`url`）或 **主机标识**（`host`），而非内部 `repoId` |
| 入参 | 异常栈（`stacktrace`）+ 可选日志（`logs`）、嫌疑文件（`entryFiles`）、环境 |
| 出参 | 分析结果：根因（`rootCause`）、补丁（`patches`）、验证（`verification`）、报告（`report`） |
| 模式 | ① 同步（阻塞等结果）② 异步（提交即返回，终态回调） |
| 下游 | 结果作为告警推送飞书等，需要**结构清晰 + 可点击跳转**的形态 |

---

## 2. 现状盘点（避免重复造轮子）

**已具备**
- `domain.CreateTaskRequest` 已含 `CallbackURL` 字段；engine 终态时自动 `fireCallback()` 回调。
- `domain.RepoMatchRules`（hostPatterns/endpointPatterns/keywords/…）已存在，可复用其规则做仓库定位。
- `idempotencyKey` 幂等键、`autoVerify`、租户隔离、审计均已就绪；API Key 鉴权适合第三方长期使用。

**缺口（本次已补齐）**
1. 没有"按 `gitUrl`/`host` **反查** `repoId`"的入参解析。
2. 没有"**同步阻塞**等待 run 终态"的接口（原 `POST /tasks` 提交即返回）。
3. 回调 payload 过简（仅 summary）、不重试 → 已扩展为结构化 `rootCause`/`patches`/`reportUrl` 并加指数退避重试。

---

## 3. 决策（已确认）
- ① 同步超时默认 120s、上限 300s。
- ② 回调不强制 HMAC 签名。
- ③ 仅返回结构化数据，由第三方自行渲染飞书卡片。
- ④ 单独开 `/api/v1/openapi/` 路由组，不复用 `/tasks` 入参。

---

## 4. 设计要点

- **仓库定位器** `resolveOpenAPITarget`：入参三选一（`repoLocator` / `repoId` / `groupId`），互斥；按 locator 给租户内仓库打分取最高分（见 §6.2）。
- **同步接口** `/openapi/analyze`：解析 → 提交 → 轮询等待终态（500ms 间隔）→ 组装结构化摘要返回；超时转 `202` + `pollUrl`。
- **异步接口** `/openapi/tasks`：提交即返回 `202` + `runId`，引擎终态自动回调 `callbackUrl`（结构化）。
- **回调增强**：payload 扩展 `rootCause`/`patches`/`reportUrl`；失败指数退避重试最多 3 次。
- **安全**：外部接口强制 API Key + `task:write` scope；租户隔离；幂等键防告警风暴。

---

## 5. 已落地实现（2026-09-23）

### 5.1 路由与鉴权
- `POST /api/v1/openapi/analyze` —— 同步分析（默认 120s、上限 300s；超时返回 `202` + `pollUrl`）。
- `POST /api/v1/openapi/tasks` —— 异步提交（受理即返回 `202` + `runId`，终态回调 `callbackUrl`）。
- 鉴权：API Key（`X-API-Key`），要求 `task:write` scope（`admin:all` 通配）。

### 5.2 仓库定位（repoLocator）
入参三选一，互斥（优先级 `repoId` > `groupId` > `locator`）：
- `repoLocator`：`{ "gitUrl"?:"https://git.x/order.git", "host"?:"order-svc" }`
- `repoId` / `groupId`

按 locator 在租户内仓库列表打分取最高分：

| 命中规则 | 分值 |
|---|---|
| git 地址精确匹配 `repo.url` | 100 |
| 命中 `matchRules.hostPatterns` | 80 |
| git 地址片段命中 `repo.url` | 70 |
| 命中 `matchRules.endpointPatterns` | 60 |
| 命中 `matchRules.keywords` | 50 |
| 主机/地址片段命中 `repo.key`/`repo.name` | 40 |

同分多匹配记 `warnings` 并取首个；无匹配返回 `422` 提示先注册仓库并配置 `matchRules`。

### 5.3 同步请求 / 响应示例
请求：
```json
POST /api/v1/openapi/analyze
{
  "repoLocator": { "gitUrl": "https://git.x/order.git" },
  "stacktrace": "java.lang.NullPointerException at com.acme.order...",
  "logs": "...", "environment": "prod",
  "idempotencyKey": "alert-001", "timeout": 120
}
```
终态响应 `200`：
```json
{
  "runId": "run_xxx", "taskId": "task_xxx", "status": "succeeded", "severity": "critical",
  "repo": { "id": "r1", "key": "order-service", "name": "订单服务", "url": "https://git.x/order.git" },
  "rootCause": { "summary": "空指针…", "category": "null_pointer", "confidence": 0.92, "detail": "…", "evidence": [], "blastRadius": [] },
  "patches": [{ "repositoryId": "r1", "repoKey": "order-service", "filePath": "service/order.go", "action": "modify", "unifiedDiff": "…", "rationale": "…" }],
  "verification": { "passed": true, "checks": [], "errors": [] },
  "reportId": "rep_xxx", "reportUrl": "https://console.x/r/rep_xxx", "markdown": "…",
  "elapsedMs": 12345, "usage": {}, "warnings": [], "degraded": false, "error": ""
}
```
超时响应 `202`：`{ "runId", "taskId", "status":"running", "pollUrl":"/api/v1/runs/{id}", "message":"分析未在超时内完成，请轮询 pollUrl 或提供 callbackUrl 等待回调" }`

### 5.4 异步请求 / 回调示例
请求：
```json
POST /api/v1/openapi/tasks
{ "repoLocator": { "host": "order-svc" }, "stacktrace": "…", "callbackUrl": "https://hook.your-svc/cb" }
```
受理响应 `202`：`{ "runId", "taskId", "status":"queued", "acceptTime" }`

终态回调 `POST {callbackUrl}`（结构化，含完整根因与补丁）：
```json
{
  "runId": "run_xxx", "state": "succeeded", "reportId": "rep_xxx", "severity": "critical",
  "summary": "空指针…", "tenantId": "t-demo", "taskId": "task_xxx", "attempt": 1, "error": "",
  "rootCause": { "summary": "…", "category": "null_pointer", "confidence": 0.92, "detail": "…" },
  "patches": [{ "repositoryId": "r1", "repoKey": "order-service", "filePath": "service/order.go", "action": "modify", "unifiedDiff": "…", "rationale": "…" }],
  "reportUrl": "https://console.x/r/rep_xxx"
}
```
回调失败指数退避重试最多 3 次（1s/2s），全部失败记 `audit`（category=task, action=callback.failed）。

### 5.5 配置
- `PUBLIC_URL`（环境变量）或 `server.publicUrl`（JSON）：对外可访问基础地址（含协议与端口），用于生成 `reportUrl` 跳转链接。

### 5.6 代码改动
- `backend/internal/api/openapi.go`（新增：仓库定位器 `resolveOpenAPITarget`/`matchRepoLocator` + 同步 `handleOpenAPIAnalyze` + 异步 `handleOpenAPICreateTask`）
- `backend/internal/api/routes.go`（注册两条路由）
- `backend/internal/engine/engine.go`（`callbackPayload` 扩展 `rootCause`/`patches`/`reportUrl` + `fireCallback` 重试 + `SetPublicURL`）
- `backend/internal/config/config.go`（`ServerConfig.PublicURL` + `PUBLIC_URL` 注入）
- `backend/cmd/server/main.go`（装配 `taskEngine.SetPublicURL(cfg.Server.PublicURL)`）
- `backend/internal/api/openapi_test.go`（定位器单测 `TestMatchRepoLocator`）
