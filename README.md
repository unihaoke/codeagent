# CodeAgent · 多仓库分组代码修复 AI Agent

> 基于 **DeepSeek Harness** 工程化架构（分层解耦 / 任务编排 / 插件沙箱 / 模型可控 / 闭环验证 / 容错可观测 / 资源隔离）
> 实现的企业级代码报错分析与自动修复 Agent。
> 后端 **Go 1.23**，前端 **Vue 3 + TypeScript + Vite**。

---

## 1. 这是什么

面向研发故障排查、线上异常自愈、代码 BUG 自动修复的工程化 Agent（**不是**通用对话机器人）：

| 能力 | 说明 |
| --- | --- |
| 代码报错自动溯源 | 多语言堆栈解析（Java/Go/Python/Node/PHP）→ 异常分类 → 文件行号定位 |
| 根因智能分析 | 堆栈 + 命中代码 + 跨仓库链路 → MCP 强约束结构化推理（根因/证据/影响面/定级） |
| 增量代码自动修复 | 仅生成 **unified diff 增量补丁**，禁止暴力重写；补丁带理由与风险等级 |
| 多模型可控推理 | MCP 层统一治理：输入标准化、输出 JSON 强约束、上下文智能裁剪、分级调度、超时熔断、换模型兜底 |
| 可插拔 Skill | 7 个内置技能 + 外部清单热加载；入参 Schema 强校验、沙箱隔离、版本灰度、熔断降级、备用兜底 |
| 多 Git 仓库动态切换 | 任务级**代码版本锁定**（仓库 + Commit 快照），杜绝"最新代码 ≠ 线上版本"导致的误分析 |
| 微服务多仓库分组 | 业务域分组（前端/网关/微服务/中间件/公共库）→ 堆栈驱动**懒加载** → 跨仓库全链路根因定位 |

**严格能力边界**：仅处理代码层与业务逻辑层 BUG（不处理网络/硬件/中间件底层故障）；仅增量 Patch；分组分析按需精准加载（不全量拉取）；修复结果仅自动化验证，上线需人工或流水线复核。

---

## 2. 目录结构

```
codeagent/
├── backend/                    Go 后端（七层架构，见 docs/ARCHITECTURE.md）
│   ├── cmd/server/             进程入口与全层装配
│   └── internal/
│       ├── domain/             领域模型与端口接口（各层唯一契约，冻结）
│       ├── config/             配置加载（默认值 < JSON < 环境变量 CA_*）
│       ├── platform/           logx 结构化日志 / eventbus 事件总线 / audit 审计埋点
│       ├── store/              数据访问层（内存索引 + JSON 快照，可替换为 PG）
│       ├── env/                ② 租户&安全权限层：认证授权、AES-GCM 凭证加密、脱敏、限流配额
│       ├── source/             ③⑦ 仓库索引/源码管理：版本锁定、懒加载、LRU 文件缓存、堆栈解析、仓库匹配
│       ├── skill/              ⑤ Skill 插件层：注册中心、Schema 校验、熔断、兜底 + builtin 七技能
│       ├── mcp/                ⑥ MCP 模型管控层：调度、结构化输出、上下文治理、Prompt 模板、Mock 推理
│       ├── sandbox/            ⑦ 源码沙箱执行层：工作区隔离、diff 生成/应用、受限命令执行与验证
│       ├── engine/             ④ Agent 调度核心层：任务生命周期、幂等、配额、worker pool、九阶段流水线
│       └── httpx/              ① 接入层：路由、中间件、SSE、WebSocket、各资源处理器
├── frontend/                   Vue 3 控制台（10 个业务页面）
├── deploy/                     配置样例、示例任务、Nginx 配置、演示源码仓库
├── scripts/                    演示仓库初始化、开发启动、冒烟测试、构建脚本
└── docs/                       架构说明、开发契约、API 说明
```

---

## 3. 快速开始（3 分钟跑通全链路）

### 3.1 环境要求

| 依赖 | 版本 | 说明 |
| --- | --- | --- |
| Go | ≥ 1.23 | 后端 |
| Node.js | ≥ 20 | 前端与演示脚本 |
| pnpm | ≥ 9 | 前端包管理（也可用 npm/yarn，改 `scripts/dev.ps1`） |
| git | ≥ 2.20 | 源码层拉取版本（**无 git / 无网络时系统自动降级，不会失败**） |

### 3.2 启动后端

```bash
cd backend
go mod tidy
go run ./cmd/server --config ../deploy/config.example.json
```

服务地址：`http://127.0.0.1:8080`。首次启动会自动创建**默认租户**（`demo`），除此之外不再注入任何演示数据。

### 3.3 配置 AI 模型（必做）

系统不再内置任何模拟数据，也没有"未配置就自动启用 Mock"的回退行为 —— 请二选一完成模型配置：

| 方式 | 操作 |
| --- | --- |
| 控制台（推荐） | 登录后进入 **MCP 模型管控 → AI 设置**，填写 Provider / BaseURL / API Key / 模型名并"测试连通"，保存后立即生效 |
| 配置下发 | 在 `deploy/config.example.json` 的 `mcp.providers` 中写入，或设置 `CA_MODEL_BASE_URL` / `CA_MODEL_API_KEY` 等环境变量 |

控制台配置保存在服务端（密钥经 AES-256-GCM 加密后落盘），优先级高于配置文件，且重启后保留。

### 3.4 启动前端控制台

```bash
cd frontend
pnpm install
pnpm run dev
```

打开 `http://127.0.0.1:5173`，使用默认账号登录：

| 字段 | 值 |
| --- | --- |
| 租户 | `demo` |
| 用户名 | `admin` |
| 密码 | `admin123` |

> 接入层（IDE / CI）使用的 API Key 需在控制台 **租户 → 接入密钥** 中自助创建，明文仅展示一次。

### 3.5 一键脚本

```powershell
# 同时拉起后端与前端
powershell -File scripts/dev.ps1

# 仅后端
powershell -File scripts/dev.ps1 -SkipFrontend

# 端到端冒烟：跑通「单仓库精准修复 + 分组联合排查 + 幂等 + 租户隔离 + 可观测」
powershell -File scripts/smoke.ps1
```

### 3.6 生产构建

```bash
# 一键产出 dist/codeagent-server[.exe] 与 dist/web（前端静态资源）
node scripts/build.mjs

# 单进程形态：后端同时托管 API 与前端 SPA
./dist/codeagent-server --config deploy/config.example.json --web ./dist/web

# 容器化（Nginx 托管前端 + 后端 API/SSE/WebSocket 反代）
docker compose up -d --build     # 控制台 http://localhost:8090
```

---

## 4. 提交第一个排查任务

### 4.0 实测效果（内置 Mock 推理器，离线可复现）

```
==> 场景一：单仓库精准修复
    OK  终态 state=needs_review  锁定仓库: order-service@02d356f6
    根因: OrderService.java:88 抛出 java.lang.NullPointerException … 分类 null_pointer 置信度 0.64
    增量补丁: 1 个（unified diff） 风险=medium 状态=applied
    沙箱验证: passed=True apply=applied（patch_apply / 静态校验全部通过）
    报告已归档（Markdown 5438 字符）

==> 场景二：多仓库分组联合排查
    OK  候选/锁定仓库: web-mall, order-service（懒加载，仅 2 个文件）
    跨仓库链路: api-gateway → order-service → inventory-service → common-lib

==> 幂等：两次提交复用同一 run    ==> 租户隔离：acme 看不到 demo 的仓库
==> 可观测：技能调用 10 次失败 0 次，模型 3 次调用 13542 Token，缓存命中 6 次
```

### 4.1 控制台（推荐）

「发起排查」页面 → 选择「多仓库分组联合排查」→ 分组选 `mall-core` →
堆栈框点「填充 Java 示例」→ 提交 → 在任务详情页实时观看九阶段执行、根因、增量补丁与沙箱验证结果。

### 4.2 命令行（接入层 API）

```bash
# 1. 用演示密钥调用（密钥见后端启动日志）
export CA_KEY=ca_live_xxxxxxxx

# 2. 单仓库精准修复：显式传线上报错对应的 commit
curl -X POST http://127.0.0.1:8080/api/v1/tasks \
  -H "X-API-Key: $CA_KEY" -H 'Content-Type: application/json' \
  -d '{
    "mode": "single_repo",
    "repoId": "<order-service 的仓库ID>",
    "ref": "<目标 commit，可选；不传则使用默认分支最新版本>",
    "source": "alert",
    "title": "订单详情接口 NPE",
    "environment": "prod",
    "stacktrace": "java.lang.NullPointerException: Cannot invoke \"com.acme.order.entity.Order.getAmount()\" because \"order\" is null\n\tat com.acme.order.service.OrderService.toDetail(OrderService.java:88)\n\tat com.acme.order.service.OrderService.queryDetail(OrderService.java:64)\n\tat com.acme.order.web.OrderController.detail(OrderController.java:41)"
  }'

# 3. 多仓库分组联合排查（示例请求体见 deploy/sample-task-group.json）
curl -X POST http://127.0.0.1:8080/api/v1/tasks \
  -H "X-API-Key: $CA_KEY" -H 'Content-Type: application/json' \
  -d @deploy/sample-task-group.json

# 4. 查询结果
curl -H "X-API-Key: $CA_KEY" http://127.0.0.1:8080/api/v1/runs/<runId>
curl -H "X-API-Key: $CA_KEY" http://127.0.0.1:8080/api/v1/reports/<reportId>/markdown
```

### 4.3 对接 CI/CD

```bash
curl -X POST $CODEAGENT/api/v1/tasks \
  -H "X-API-Key: $CA_KEY" -H 'Content-Type: application/json' \
  -d "{\"mode\":\"single_repo\",\"repoId\":\"$REPO_ID\",\"ref\":\"$GIT_COMMIT\",
       \"source\":\"cicd\",\"idempotencyKey\":\"$CI_PIPELINE_ID\",
       \"callbackUrl\":\"$CI_CALLBACK\",\"stacktrace\":\"$(cat build-error.log)\"}"
```

`idempotencyKey` 保证同一次构建重复触发不会重复执行；`callbackUrl` 会在任务终态时收到
`{runId, state, reportId, severity, summary}` 回调，可用于卡点预警。

---

## 5. 配置

配置优先级：**默认值 < 配置文件(JSON) < 环境变量(`CA_*`)**。样例见 `deploy/config.example.json`。

### 接入真实模型（OpenAI 兼容协议）

```jsonc
{
  "mcp": {
    "default": "deepseek",
    "providers": [
      {
        "name": "deepseek",
        "kind": "openai",                       // openai | deepseek | azure | ollama | openai_compatible | mock
        "baseUrl": "https://api.deepseek.com",
        "apiKey": "",                            // 建议改用环境变量 CA_MODEL_API_KEY
        "models": ["deepseek-chat"],
        "tier": "strong",                        // light 轻量 / strong 强力 / fallback 兜底
        "maxTokens": 8192,
        "temperature": 0.1,
        "weight": 1
      },
      {
        "name": "qwen-light",
        "kind": "openai",
        "baseUrl": "https://dashscope.aliyuncs.com/compatible-mode",
        "models": ["qwen-plus"],
        "tier": "light",
        "weight": 1
      }
    ]
  }
}
```

或用环境变量注入单个 provider：`CA_MODEL_API_KEY` / `CA_MODEL_BASE_URL` / `CA_MODEL_NAME` / `CA_MODEL_PROVIDER`。

### 常用环境变量

| 变量 | 作用 | 默认 |
| --- | --- | --- |
| `CA_ADDR` | 监听地址 | `:8080` |
| `CA_JWT_SECRET` | 控制台 JWT 密钥（**生产必须修改**） | 开发默认值 |
| `CA_ENCRYPTION_KEY` | Git 凭证 AES-256 加密密钥（**生产必须修改**） | 开发默认值 |
| `CA_ALLOW_ANONYMOUS` | 允许匿名调用（仅本地 IDE 场景） | `false` |
| `CA_WORKERS` | 任务 worker 数 | `4` |
| `CA_TASK_TIMEOUT_SEC` | 单任务超时 | `900` |
| `CA_CACHE_DIR` / `CA_WORKSPACE_DIR` | 源码镜像/沙箱工作区目录 | `data/cache` / `data/workspace` |
| `CA_SANDBOX_ALLOW_EXEC` | 允许沙箱执行编译/校验命令 | `true` |
| `CA_MODEL_API_KEY` 等 | 模型 provider 注入 | - |
| `CA_STORE_DRIVER` | `memory` 或 `file` | `file` |
| `CA_LOG_LEVEL` / `CA_LOG_FORMAT` | 日志级别与格式 | `info` / `text` |

---

## 6. 部署形态

| 形态 | 做法 |
| --- | --- |
| **本地轻量化**（IDE 本地修复） | `CA_ALLOW_ANONYMOUS=true`、`CA_STORE_DRIVER=memory`、`CA_SANDBOX_ALLOW_EXEC=false`，单二进制直接运行 |
| **服务端私有化**（团队多项目接入） | `docker compose up -d --build`：前端 dist 在镜像内构建并交给 `webconsole` 阶段的 Nginx 托管；后端 `go build -o codeagent-server ./cmd/server`；配置真实模型 provider 与 PostgreSQL 版 store 实现 |
| **流水线集成**（CI/CD 卡点） | CI 中以子进程或服务方式调用 `POST /api/v1/tasks`，传 `ref=$GIT_COMMIT` 与 `idempotencyKey`，用 `callbackUrl` 回传结果做卡点预警 |

生产加固清单：

- 修改 `CA_JWT_SECRET` 与 `CA_ENCRYPTION_KEY`（凭证加密密钥一旦更换，已存密文需重新录入）
- 关闭 `CA_ALLOW_ANONYMOUS`，为每个调用方签发独立 API Key 并限定 scopes
- 收敛 `server.corsOrigins`，前置反向代理启用 TLS
- 沙箱工作区目录挂载为独立卷，并按 `sandbox.keepWorkspace=false` 定期清理
- `/metrics` 接入 Prometheus，按任务修复率/技能失败率/模型兜底率建立告警

---

## 7. 验证与测试

```bash
# 后端：编译 + 静态检查 + 全量单测
cd backend
go build ./...
go vet ./...
go test ./... -count=1

# 前端：类型检查 + 生产构建
cd frontend
pnpm run build
```

前端开发态分离启动：

```bash
cd frontend && pnpm run dev     # Vite 已配置 /api、/healthz、/metrics 代理到 :8080，含 WebSocket
```

---

## 8. 文档索引

| 文档 | 内容 |
| --- | --- |
| `docs/ARCHITECTURE.md` | 七层架构与需求条款逐条对应、九阶段流水线、状态机、降级矩阵 |
| `docs/API.md` | 完整 HTTP API、SSE/WebSocket 事件协议、错误码、示例 |
| `docs/CONTRACT.md` | 模块开发契约（目录归属、内置技能 I/O Schema、验收项） |

---

## 9. 安全基线（验收项）

1. **租户隔离**：所有仓库、分组、任务、报告、审计均按 `tenantId` 隔离，跨租户访问返回 403。
2. **凭证安全**：Git 密钥 AES-GCM 加密存储，接口响应与日志永不回显明文（`logx` 对敏感 key 兜底）。
3. **访问鉴权**：每次任务执行校验仓库/分组访问权限，防越权读取私有代码；API Key 支持 scopes 与吊销。
4. **源码脱敏**：送模型前自动清除密码、Token、AK/SK、私钥、连接串凭证，并保留行结构（不影响行号定位）。
5. **资源隔离**：单任务限制最大解析文件数、代码字符量、Token 消耗、执行超时；超限自动降级而非失败。
6. **人工兜底**：所有修复结果仅做自动化验证，最终上线必须经人工或流水线复核。

---

> 需求来源：《基于 DeepSeek Harness 架构 多仓库分组代码修复 AI Agent 完整正式方案文档》V1.0 最终版。
