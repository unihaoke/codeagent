# CodeAgent 开发契约（冻结版 v1）

> 本文件是各模块并行开发的**唯一接口契约**。所有类型与方法签名以
> `backend/internal/domain/`（`enums.go` / `models.go` / `ports.go`）中的 Go 定义为准，
> 本文件只补充**文件归属、HTTP/WS 协议、模块职责与验证方式**。
> 任何模块都不得修改 `internal/domain`、`internal/store`、`internal/config`、
> `internal/platform/*`、`internal/httpx/response.go`。

## 0. 技术基线

| 项 | 取值 |
| --- | --- |
| Go | 1.23（module `github.com/codeagent/backend`） |
| 第三方依赖 | **仅允许 `github.com/google/uuid`**；路由用标准库 `net/http` `ServeMux`（Go 1.22 模式语法），WebSocket 自行实现 RFC6455 |
| 前端 | Vue 3.4 + TypeScript + Vite 5 + Pinia + Vue Router 4 + ECharts 5（`frontend/`，包管理器 pnpm） |
| 认证 | 控制台 JWT（HS256，自实现）+ 接入层 API Key（SHA-256 哈希存储） |
| 持久化 | `store.Store` 内存索引 + JSON 快照，方法集即数据访问层契约 |

## 1. 目录与文件归属（并行开发不重叠）

```
backend/
  cmd/server/main.go                 [主控] 装配全部层
  internal/
    domain/        [已冻结] 领域模型与端口接口
    config/        [已冻结] 配置
    platform/logx/ [已冻结] 结构化日志
    platform/eventbus/ [已冻结] 事件总线
    platform/audit/audit.go        [S1] domain.Recorder 实现
    store/         [已冻结] 数据访问层
    env/                           [S1] 租户权限层
      crypto.go   AES-GCM 凭证加密箱 + 源码脱敏
      auth.go     API Key / JWT / 权限校验（domain.Authorizer）
      ratelimit.go 令牌桶限流
      bootstrap.go  示例租户/密钥/仓库/分组初始化（幂等）
    source/                        [S2] 源码管理
      resolver.go  SourceResolver：版本锁定 + 懒加载 + LRU 文件缓存
      gitrepo.go   git 镜像管理（clone/fetch/cat-file/show/ls-tree/grep）
      stack.go     堆栈解析（多语言：java/go/python/js-ts/php）
      astutil.go   轻量语言探测、符号提取、import 提取、行切片
      matcher.go   仓库匹配打分（domain.RepoMatchRules → []domain.RepoCandidate）
    skill/                         [S3] Skill 插件层
      registry.go  SkillRegistry：注册/热更新/版本/灰度/回滚
      runner.go    SkillRunner：Schema 强校验 + 沙箱隔离 + 重试 + 熔断 + 兜底 + 审计
      schema.go    极简 JSON Schema 校验器（type/required/properties/items/enum/oneOf）
      circuit.go   熔断器
      builtin/*.go 7 个内置技能（见 §4）
    mcp/                           [S4] MCP 模型管控层
      gateway.go   MCPGateway：调度 + 容错 + 熔断 + 兜底 + 记录
      provider.go  OpenAI 兼容客户端（SSE/JSON）+ Mock provider
      context.go   ContextPacker 上下文智能治理
      prompt.go    Prompt 模板库（输入标准化）
      schema.go    结构化输出 Schema 与解析/修复
    sandbox/                       [S5] 源码沙箱执行层
      manager.go   工作区隔离与物化 + 补丁验证
      patch.go     unified diff 生成/解析/应用（无外部依赖）
      runner.go    受限命令执行（超时/输出截断/白名单）
    engine/                        [S6] Agent 核心调度层
      engine.go    生命周期、提交、幂等、配额、worker pool、取消、重试
      pipeline.go  TaskPipeline：证据准备→分析→修复→验证→报告
      report.go    报告与 Markdown 渲染
      stats.go     EngineStats / ObservabilitySummary 聚合
    httpx/                         [S7] 接入层（中间件、WebSocket、响应信封、ID 工具）
      response.go  [已冻结] 统一响应信封（本文件由主控提供）
      ids.go       [已冻结] NewRequestID / NewResourceID 等跨包共用工具
      middleware.go reqID / recover / logging / CORS / auth / ratelimit / tenant
      ws.go        极简 RFC6455 WebSocket + 事件广播 Hub
    api/                           [S7] 各资源 HTTP 处理器（原 httpx/handler）
      deps.go      依赖集合与适配器（含 *store.Store → domain.RepoIndex 适配）
      routes.go    Register(mux, deps)：路由表与授权 scope
      system.go / auth.go / tenant.go / repo.go / group.go / task.go
      report.go / skill.go / model.go / observability.go
    app/                           [主控] 服务装配与 SPA 托管
      server.go    NewServer(Deps) / Handler() / Run(ctx)（中间件链与优雅关闭）
      static.go    前端构建产物托管 + history 路由回退
```

> **包结构修正说明（重要）**：契约最初把处理器放在 `internal/httpx/handler`，实装时发现会形成
> `httpx → handler → httpx` 的 import cycle（处理器必须复用 httpx 的响应信封与上下文工具）。
> 因此拆分为三个包：`httpx`（可复用中间件与协议实现）、`api`（处理器，只依赖 httpx 与 domain）、
> `app`（装配，被 `cmd/server` 调用）。职责边界未变，仅消除了循环依赖。

## 2. HTTP 协议（接入层）

### 2.1 统一响应信封

```json
{ "code": 0, "message": "ok", "requestId": "r-xxx", "data": { ... } }
```

- 成功：`code = 0`
- 失败：`code` 为下表错误码，`message` 为人类可读信息，HTTP 状态码同步设置。
- 分页数据统一为 `{ "items": [...], "total": n, "page": 1, "pageSize": 20 }`。

| code | HTTP | 含义 |
| --- | --- | --- |
| 0 | 200 | 成功 |
| 400 | 400 | 参数错误 |
| 401 | 401 | 未认证 |
| 403 | 403 | 无权限 / 越权访问 |
| 404 | 404 | 资源不存在 |
| 409 | 409 | 冲突（幂等/唯一键） |
| 422 | 422 | 语义校验失败（如技能入参 Schema 不合法） |
| 429 | 429 | 限流 / 配额超限 |
| 500 | 500 | 内部错误 |
| 503 | 503 | 熔断降级 / 不可用 |

### 2.2 认证方式

- 控制台：`Authorization: Bearer <jwt>`，作用域含 `admin:all`
- 接入层：`X-API-Key: ca_live_xxx`（或 `Authorization: ApiKey ca_live_xxx`）

### 2.3 路由表（全部挂在 `/api/v1`）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/healthz` | 健康检查（免认证） |
| GET | `/readyz` | 就绪检查（依赖健康） |
| POST | `/auth/login` | 控制台登录 `{tenantKey, username, password}` → `{token, subject}` |
| GET | `/auth/profile` | 当前主体 |
| GET | `/tenants` | 租户列表 |
| POST | `/tenants` | 创建租户 |
| GET | `/tenants/current` | 当前租户详情 |
| PUT | `/tenants/current` | 更新当前租户（名称/配额） |
| GET | `/api-keys` | 密钥列表（脱敏） |
| POST | `/api-keys` | 创建密钥 → 返回一次明文 |
| DELETE | `/api-keys/{id}` | 吊销密钥 |
| GET | `/credentials` | Git 凭证列表（不含密文） |
| POST | `/credentials` | 创建凭证（明文仅入参） |
| DELETE | `/credentials/{id}` | 删除凭证 |
| GET | `/repos` | 仓库列表（分页/关键字/租户隔离） |
| POST | `/repos` | 注册仓库 |
| GET | `/repos/{id}` | 仓库详情（含所属分组、缓存统计） |
| PUT | `/repos/{id}` | 更新仓库 |
| DELETE | `/repos/{id}` | 删除仓库 |
| POST | `/repos/{id}/probe` | 连通性探测与版本解析（走 SourceResolver.Resolve） |
| GET | `/groups` | 分组列表 |
| POST | `/groups` | 创建分组 `{group, members:[{repositoryId, order}]}` |
| GET | `/groups/{id}` | 分组详情（成员聚合视图） |
| PUT | `/groups/{id}` | 更新分组与成员 |
| DELETE | `/groups/{id}` | 删除分组 |
| GET | `/groups/{id}/impact` | 分组影响面分析（成员仓库/语言/分层统计） |
| POST | `/tasks` | **核心**：提交分析任务（body = `domain.CreateTaskRequest`） → 返回 `TaskRun` |
| GET | `/tasks` | 逻辑任务列表 |
| GET | `/tasks/{id}` | 任务详情（含最新 run 摘要） |
| GET | `/runs` | 执行记录列表（`state` / `keyword` / 分页） |
| GET | `/runs/{runId}` | 执行详情（任务上下文全量：resolution/patches/verification/usage） |
| POST | `/runs/{runId}/cancel` | 取消任务 |
| POST | `/runs/{runId}/retry` | 基于固化上下文重跑 |
| POST | `/runs/{runId}/feedback` | 携带人工反馈重跑 `{feedback}` |
| GET | `/runs/{runId}/events` | 运行时事件（历史 + SSE 实时流） |
| GET | `/runs/{runId}/skill-calls` | 该次运行的技能调用轨迹 |
| GET | `/runs/{runId}/model-calls` | 该次运行的模型推理轨迹 |
| GET | `/reports` | 报告列表 |
| GET | `/reports/{id}` | 报告详情（含 Markdown） |
| GET | `/reports/{id}/markdown` | 纯文本 Markdown（`text/markdown`） |
| GET | `/skills` | 技能清单（含健康度） |
| GET | `/skills/{name}` | 技能详情（含 Schema） |
| POST | `/skills/{name}/status` | 启停/灰度 `{version, status, canaryPercent}` |
| POST | `/skills/{name}/rollback` | 回滚到上一可用版本 |
| GET | `/models` | 模型提供方与健康度 |
| GET | `/models/stats` | 模型层统计 |
| GET | `/observability/summary` | 可观测汇总（修复率、Top 技能/模型、状态分布） |
| GET | `/observability/audits` | 审计事件（分类/关键字分页） |
| GET | `/ws?token=<jwt>` | WebSocket 事件推送（`domain.Event`） |
| GET | `/metrics` | Prometheus 文本指标 |

## 3. 事件协议（可观测 + 前端实时）

`domain.Event.Type` 取值与出现时机：

| Type | Stage | 说明 |
| --- | --- | --- |
| `task.created` | - | 任务受理，Payload = `TaskRun` |
| `task.state` | - | 状态机迁移，Payload = `{from,to}` |
| `stage` | 各阶段 | 阶段开始/结束，Message = 人类可读描述 |
| `skill.call` | 技能 | Payload = `domain.SkillCall` |
| `model.call` | 模型 | Payload = `domain.ModelCall` |
| `source.fetch` | 源码 | 仓库版本锁定 / 文件加载 |
| `repo.switch` | 源码 | 仓库切换记录 |
| `patch.proposed` | 修复 | Payload = `[]domain.Patch` |
| `verification` | 验证 | Payload = `domain.Verification` |
| `report.ready` | 报告 | Payload = `{reportId}` |
| `log` | - | 结构化日志埋点 |
| `task.terminal` | - | 终态，前端据此关闭流 |

阶段（Stage）标准命名：`resolve` → `stack_parse` → `candidates` → `code_load` → `cross_repo` → `root_cause` → `patch_synthesize` → `sandbox_verify` → `report` → `archive`。

## 4. 内置技能契约（Skill 层与 Pipeline 必须一致）

技能名常量见 `domain.SkillXXX`。每个技能的 `InputSchema` / 输出字段如下：

| 技能 | 输入字段 | 输出字段 |
| --- | --- | --- |
| `stacktrace_parse` | `stacktrace`(string,必填)、`logs`(string)、`language`(string) | `stack`(StackAnalysis)、`language`、`category`、`confidence` |
| `code_ast_analyze` | `slices`(array)、`focusLines`(array) | `symbols`(array)、`imports`(array)、`risks`(array)、`complexity`(object) |
| `error_localize` | `stack`(object)、`candidates`(array) | `hits`(array of `{repositoryId, path, line, reason, score}`)、`confidence` |
| `cross_repo_trace` | `stack`(object)、`candidates`(array)、`slices`(array) | `edges`(array of CallEdge)、`chain`(array)、`breakPoint`(object)、`confidence` |
| `code_patch_synthesize` | `rootCause`(object)、`slices`(array)、`category`(string) | `patches`(array of Patch)、`rationale`、`risk` |
| `dependency_repair` | `dependencyFiles`(array)、`errorMessage`(string) | `findings`(array)、`patches`(array of Patch)、`actions`(array) |
| `patch_verify` | `patches`(array)、`language`(string) | `checks`(array of VerificationCheck)、`passed`(bool)、`score`(number) |

技能实现要求：**确定性**（同样入参同样出参）、无网络、无文件系统副作用、超时可中断（检查 `ctx.Err()`）。

## 5. 端口接口速查（细节见 `internal/domain/ports.go`）

- `domain.Authorizer`：认证授权、登录、越权拦截
- `domain.CredentialBox`：AES-GCM 加密箱 + 脱敏
- `domain.RepoIndex`：仓库/分组索引
- `domain.SourceResolver`：版本锁定 + 懒加载 + 文件级缓存
- `domain.SandboxManager` / `domain.SandboxHandle`：沙箱工作区与验证
- `domain.SkillRegistry` / `domain.SkillRunner`：技能注册与执行治理
- `domain.ContextPacker` / `domain.MCPGateway`：上下文治理与模型调度
- `domain.TaskEngine`：任务受理、查询、取消、重跑、统计
- `domain.Recorder` / `domain.EventBus`：审计埋点与实时事件

## 6. 各层强制要求（验收项）

1. **租户隔离**：任何查询/写入必须携带 `TenantID`，跨租户访问返回 `403`。
2. **凭证安全**：密钥/Token 落库前必须 AES-GCM 加密；日志与 API 响应中一律脱敏（`logx` 已对敏感 key 兜底）。
3. **版本精准**：源码分析严格使用 `TaskRun.PinnedCommits` 中的 commit，禁止使用默认分支最新代码。
4. **懒加载**：分组分析只加载 `EvidenceBundle.CandidateRepos[].Hints` 命中的少量文件；受 `LoadBudget` 约束。
5. **配额管控**：文件数、代码字符数、Token、超时四类配额超限时降级而非失败（`EvidenceBundle.Notes` + `Warnings` 记录）。
6. **降级不失败**：堆栈线索不足 / 代码量超限 / 模型异常 → `StateDegraded`，仍输出堆栈文本分析报告。
7. **增量补丁**：补丁必须是标准 unified diff，禁止整文件重写；`Patch.Rationale`/`Risk`/`TouchedLines` 必填。
8. **收敛**：单任务"修复-验证"迭代不超过 `EngineConfig.MaxRepairRounds`；同任务多次重跑共用同一 `PinnedCommits`。
9. **无阻塞**：所有外部调用（git / 模型 / 命令）必须受 `context` 超时约束；worker pool 不得因单任务阻塞。
10. **可观测**：任务生命周期、仓库切换、技能调用、模型推理、源码拉取、修复变更六类事件必须落审计。

## 7. 验证方式（每个模块自测通过后才算完成）

```bash
cd backend
go build ./...
go vet ./...
go test ./...            # 各模块自带 _test.go
```

```bash
cd frontend
pnpm install
pnpm run build           # 必须零 TS 错误
```

端到端冒烟（主控负责）：

```bash
cd backend && go run ./cmd/server --config ../../deploy/config.example.json
# 另开终端
curl -X POST localhost:8080/api/v1/auth/login -d '{"tenantKey":"demo","username":"admin","password":"admin123"}'
curl -X POST localhost:8080/api/v1/tasks -H "X-API-Key: <key>" -d @task.json
```
