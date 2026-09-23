# 交付说明与验收对照表

> 项目：基于 DeepSeek Harness 架构的 **多仓库分组代码修复 AI Agent**
> 需求来源：《基于 DeepSeek Harness 架构 多仓库分组代码修复 AI Agent 完整正式方案文档》V1.0 最终版
> 技术栈：**Go 1.23（后端） + Vue 3 + TypeScript + Vite（前端）**

---

## 一、交付物清单

### 1. 后端（`backend/`，Go 1.23，仅一个第三方依赖 `github.com/google/uuid`）

| 层 | 包 | 交付内容 |
| --- | --- | --- |
| 契约 | `internal/domain` | 领域模型、状态机、七层端口接口（全系统唯一契约，冻结） |
| 基础 | `internal/config`、`internal/platform/{logx,eventbus,audit}`、`internal/store` | 配置加载、结构化日志（敏感字段兜底脱敏）、实时事件总线、审计与可观测汇总、数据访问层（内存索引 + JSON 快照） |
| ① 接入层 | `internal/httpx`、`internal/api`、`internal/app` | 路由表、中间件链（Recover/RequestID/AccessLog/CORS/MaxBytes/RateLimit/Auth）、SSE、自研 RFC6455 WebSocket Hub、Prometheus `/metrics`、SPA 静态托管、优雅关闭 |
| ② 租户权限层 | `internal/env` | API Key 与 JWT（HS256 自实现）、AES-GCM 凭证加密箱、源码脱敏、令牌桶限流、配额守卫、演示数据种子 |
| ③ 仓库分组索引层 | `internal/source/matcher.go` + `internal/store` | 仓库元数据与分组索引、堆栈驱动匹配打分（8 类规则）、影响面分析数据 |
| ④ Agent 调度层 | `internal/engine` | 任务受理/幂等/配额、worker pool、取消与重跑、**九阶段流水线**、终态判定、报告与 Markdown 渲染、统计聚合 |
| ⑤ Skill 插件层 | `internal/skill`、`internal/skill/builtin` | 注册中心（版本/灰度/回滚/热加载）、执行器（Schema 强校验 + 隔离超时 + 重试 + 熔断 + 兜底 + 审计）、7 个内置技能 |
| ⑥ MCP 模型管控层 | `internal/mcp` | 网关（分级调度 + 加权择优 + 熔断 + 纠错重试 + 换模型兜底）、上下文打包器、Prompt 模板库、结构化输出解析与修复、Mock 推理器、OpenAI 兼容 provider |
| ⑦ 源码沙箱层 | `internal/source`、`internal/sandbox` | git 镜像与精准版本解析、LRU 文件级缓存、多语言堆栈解析、AST 工具、工作区隔离与物化、unified diff 生成/应用、受限命令执行与静态校验 |

### 2. 前端（`frontend/`，Vue 3 + TS + Vite + Pinia + ECharts）

10 个业务页面：运行监控台、任务中心、发起排查、任务执行详情（实时）、修复报告列表、报告详情、代码源仓库、业务分组列表、分组详情与影响面、Skill 插件治理、MCP 模型管控、租户与权限、可观测审计、登录页、404。
另含设计系统（`src/styles/global.css`，明暗双主题令牌）、组件库（Diff 视图、Markdown 渲染、阶段进度条、状态徽标、条形图、模态框、分页等）、API 客户端（认证头、响应解包、SSE、WebSocket 与断线重连）。

### 3. 工程与交付配套

| 文件 | 用途 |
| --- | --- |
| `docs/ARCHITECTURE.md` | 七层架构落地说明、需求条款对照、九阶段流水线、状态机、降级矩阵 |
| `docs/API.md` | 完整接入层 API、SSE/WebSocket 事件协议、错误码、三类集成示例 |
| `docs/CONTRACT.md` | 模块开发契约（目录归属、内置技能 I/O Schema、10 项强制验收项） |
| `docs/DEPLOYMENT.md` | 三种部署形态、配置项全表、Prometheus 接入、故障处置手册、扩容建议 |
| `deploy/config.example.json` | 全量配置样例（模型请在控制台「AI 设置」或 `mcp.providers` 中配置） |
| `deploy/nginx.conf`、`docker-compose.yml`、`Dockerfile` | 生产容器化部署（含 SSE 关闭缓冲、WebSocket 升级、SPA 回退） |
| `deploy/sample-task-group.json` | 分组联合排查示例请求体 |
| `scripts/dev.ps1` / `scripts/smoke.ps1` / `scripts/build.mjs` | 开发启动 / 端到端冒烟 / 生产构建 |

---

## 二、需求条款 → 实现对照

| 需求文档条款 | 实现位置 | 验收方式 |
| --- | --- | --- |
| 1.2 智能修复能力（语法/运行时/逻辑/依赖/兼容） | 技能 `code_patch_synthesize`（8 类修复规则库）+ `dependency_repair` | 提交对应堆栈 → 报告含 unified diff 补丁与理由 |
| 1.2 Agent 工程化（上下文遗忘、任务发散、重复执行、不收敛、结果不可控） | ④ 层：`TaskRun` 上下文固化 + 幂等键 + 状态机 + `MaxRepairRounds` 收敛上限 + `DecideState` | 同 `idempotencyKey` 重复提交复用同一 run；`/runs/{id}` 返回完整上下文 |
| 1.2 Skill 插件体系（扩展/熔断/版本/参数校验） | ⑤ 层注册中心 + 执行器 | `/api/v1/skills` 查看版本、灰度、熔断态；非法入参返回 422 且不执行 |
| 1.2 MCP 模型管控（输入输出约束、推理稳定、上下文、多模型） | ⑥ 层 `Gateway` + `ContextPacker` + `Prompt` + `schema` | `/api/v1/models` 看分级与熔断；`ModelCall.SchemaValid`/`Repaired` 证据 |
| 1.2 多代码源（动态切换、独立调用） | ③ + ⑦ 层：每仓库独立 `MatchRules` 与凭证，按 `repoId` 路由 | 两条任务并发，`/runs/{id}` 的 `resolution` 互不干扰 |
| 1.2 仓库分组（跨仓库联合分析） | ③ 层分组索引 + `matcher` + `cross_repo_trace` 技能 | 分组模式任务返回 `chainFlow` 链路与断点 |
| 2.1 单仓库精准修复 | `mode=single_repo` 流程 | 冒烟脚本场景一 |
| 2.2 分组跨仓库联合排查 | `mode=group` 流程（入口仓库先解析，其余懒加载） | 冒烟脚本场景二 |
| 2.3 能力边界（仅代码/逻辑 BUG、仅增量 Patch、按需加载、人工复核） | 补丁必须为 unified diff 且规模受限；`LoadBudget` 约束；报告强制人工复核建议 | 检查 `patches[].unifiedDiff` 与报告 `suggestions` |
| 3.2 七层架构 | 见 `docs/ARCHITECTURE.md` §1 映射表 | 代码目录与接口一一对应 |
| 4.1 任务级版本锁定 | ④ 层 `resolve` 阶段写 `PinnedCommits`，⑦ 层只按该 commit 读取 | `/runs/{id}` 的 `resolution[].commit` 与 `requestedRef` |
| 4.2 分组纯索引 + 堆栈驱动懒加载 | `matcher.Match`（只读元数据）+ `code_load` 仅加载命中文件 | `/runs/{id}` 的 `usage.filesLoaded` 远小于分组代码总量 |
| 4.3 智能任务上下文 | `TaskRun` + `EvidenceBundle` + `SkillCall`/`ModelCall` 全量持久化 | 三个查询接口：`/runs/{id}`、`/skill-calls`、`/model-calls` |
| 5.1 五个核心技能 | `stacktrace_parse`、`code_ast_analyze`、`error_localize`、`code_patch_synthesize`、`dependency_repair`、`patch_verify` | `/api/v1/skills` |
| 5.2 多仓库扩展技能 | `cross_repo_trace`（堆栈定位 + 跨仓库链路） | 分组任务报告的链路区块 |
| 5.3 Skill 容错治理 | 执行器五道防线：Schema 校验 → 熔断 → 隔离超时 → 重试 → 兜底，全程审计 | `/api/v1/skills` 的 `health` 与 `/runs/{id}/skill-calls` |
| 6.1 MCP 五项管控 | 见 `docs/ARCHITECTURE.md` §2 对应行 | `ModelStats`（兜底/修复/Token） |
| 7.1 精准版本拉取 | ⑦ 层 `Resolve` + `git show <commit>:<path>` | `probe` 接口返回锁定 commit |
| 7.2 文件级智能缓存 | LRU + TTL，键 `repoID|commit|path`，容量上限 | `/metrics` 的缓存命中率、`CacheStats` |
| 八 安全隔离 | ② 层租户校验、AES-GCM、脱敏、scope 授权 | 冒烟脚本租户隔离用例；`/credentials` 永不回显密文 |
| 9.1 全链路容错 | 见 `docs/ARCHITECTURE.md` §5 降级矩阵 | 断开 git / 关闭沙箱执行 / 模型故障三种注入验证 |
| 9.2 资源配额 | `TenantQuota` 五项 + 限流 + 队列上限 | 降低 `maxConcurrentTasks` 后并发提交返回 429 |
| 9.3 智能降级 | 线索不足 / 代码超限 / 模型异常 → `degraded` 仍出报告 | 用无匹配堆栈提交 → 状态 `degraded` 且报告完整 |
| 十 可观测 | `eventbus` + `audit` + `logx` + `/metrics` + 控制台审计页 | `/api/v1/observability/summary` 与审计分类回溯 |
| 11.1/11.2 核心流程 | 九阶段流水线（单仓库与分组共用，差异仅候选范围） | 任务详情页阶段进度条与事件时间线 |
| 十二 三种部署形态 | 见 `docs/DEPLOYMENT.md` | 单进程 / Nginx + 容器 / CI 集成 |
| 十三 方案优势 | 分层解耦 + 场景全覆盖 + 五大痛点解法 + 安全高可用 + 可扩展 | 见本文 §三 验收清单 |

---

## 二·补 实测结果（本机真实执行记录）

| 验证项 | 命令 | 结果 |
| --- | --- | --- |
| 后端编译 | `go build ./...` | 通过（零错误） |
| 后端静态检查 | `go vet ./...` | 通过 |
| 后端格式 | `gofmt -l ./cmd ./internal ./integration` | 无输出（全部已格式化） |
| 后端全量单测 | `go test ./... -count=1` | **13 个包全部 ok**（含跨层端到端集成测试 `backend/integration`，54s） |
| 前端类型检查 | `pnpm run typecheck`（`vue-tsc --noEmit`） | 零错误 |
| 前端生产构建 | `pnpm run build` | 成功（10 个业务页面独立 chunk + echarts 分包） |
| 部署构建 | `node scripts/build.mjs` | 产出 `dist/codeagent-server.exe`（13.4 MB）与 `dist/web` |
| 端到端冒烟 | `powershell -File scripts/smoke.ps1` | **exit 0**，两项主链路 + 幂等 + 租户隔离 + 可观测全部 OK |

端到端冒烟的真实输出要点（无外网、内置 Mock 推理器、本地演示 git 镜像）：

```
==> 场景一：单仓库精准修复
    OK  终态 state=needs_review  锁定仓库: order-service@02d356f6
    根因: OrderService.java:88 处抛出 java.lang.NullPointerException … 分类: null_pointer 置信度=0.64
    增量补丁: 1 个 → order-service/…/OrderService.java 风险=medium 状态=applied
    沙箱验证: passed=True apply=applied
      - patch_apply passed=True
      - command_exec passed=True skipped=True（未找到 ./mvnw，降级静态校验）
      - static_syntax / go_parse / json_valid / yaml_lint / danger_scan 全部 passed
    报告已归档 Markdown 长度=5438

==> 场景二：多仓库分组联合排查
    OK  候选/锁定仓库: web-mall, order-service（懒加载，仅加载 2 个文件）
    跨仓库链路边: 3 条（api-gateway → order-service → inventory-service → common-lib）

==> 幂等验证：两次提交复用同一 run    ==> 租户隔离：acme 看不到 demo 的仓库
==> 可观测汇总：任务总数=3 运行总数=3 技能调用=10 失败=0 模型调用=3 Token=13542 缓存命中=6
    OK  已注册技能 7 个：patch_verify, dependency_repair, code_patch_synthesize,
        cross_repo_trace, error_localize, code_ast_analyze, stacktrace_parse
```

> 补充说明：演示仓库 `order-service` 是**完整可编译**的 Java 工程（含 entity/repository/client/exception 支撑类与 `pom.xml`），
> 执行 `mvn -o compile` 可 **BUILD SUCCESS**；沙箱在检测到离线环境缺少 Maven 依赖时会自动把编译校验降级为静态校验，
> 并在 `verification.degraded` 与报告中明确标注，符合"降级不失败"要求。

---

## 三、验收清单（可逐条执行）

### A. 构建与测试

```bash
cd backend  && go build ./... && go vet ./... && go test ./... -count=1
cd frontend && pnpm install --registry=https://registry.npmjs.org && pnpm run build   # 含 vue-tsc 类型检查
node scripts/build.mjs                                                                # 产出 dist/
```

### B. 端到端功能（离线可完成）

```bash
cd backend && go run ./cmd/server --config ../deploy/config.example.json
# 控制台：AI 设置中配置模型 → 录入至少一个仓库（分组场景需再建分组）
powershell -File scripts/smoke.ps1                                # 自动跑完下列 7 项
```

| # | 验收项 | 期望结果 |
| --- | --- | --- |
| 1 | 健康检查 | `/healthz` 200；`/readyz` 各项 ok |
| 2 | 单仓库精准修复 | 状态 `succeeded`/`needs_review`；`resolution[].commit` == 传入 ref；`patches[]` 含 unified diff；`verification.passed` 有明确结论（命令校验或静态校验降级） |
| 3 | 分组联合排查 | 返回跨仓库 `chainFlow` 与断点；`usage.filesLoaded` 远小于分组文件总量（懒加载生效） |
| 4 | 幂等 | 同 `idempotencyKey` 两次提交得到同一 `runId` |
| 5 | 租户隔离 | `acme` 租户查询仓库总数为 0 |
| 6 | 可观测 | `fixRate`/`stateDist`/`categoryDist`/`topSkills`/`topModels` 有真实数值 |
| 7 | 调用轨迹 | `/runs/{id}/skill-calls` 覆盖九阶段所用技能，`/model-calls` 含 `schemaValid` 字段 |

### C. 控制台功能（`http://127.0.0.1:5173`）

| 页面 | 验收点 |
| --- | --- |
| 运行监控台 | 指标卡、状态/根因分布、7 天趋势、实时事件流随任务推进自动滚动 |
| 发起排查 | 双场景切换、堆栈示例一键填充、提交后跳转任务详情 |
| 任务详情 | 九阶段进度、实时时间线、版本锁定表、根因、增量补丁 Diff、沙箱校验明细、调用轨迹、取消/重跑/带反馈重跑 |
| 修复报告 | 列表 + 详情（Markdown 原文、跨仓库链路、补丁、时间线、建议、下载/复制） |
| 代码源仓库 | 注册仓库（含 6 类匹配规则）、连通性探测、语言/分层统计 |
| 业务分组 | 成员链路排序与入口标记、影响面风险评分 |
| Skill 插件 | 治理参数、健康度与熔断态、启停/灰度/回滚/Schema 查看 |
| MCP 模型管控 | 分级调度视图、熔断态、调用与 Token 统计、兜底次数 |
| 租户与权限 | 密钥一次性明文、配额编辑、凭证加密管理、安全基线说明 |
| 可观测审计 | 汇总指标、审计事件分类回溯、JSON 导出 |

### D. 安全基线抽查

| 验收项 | 操作 | 期望 |
| --- | --- | --- |
| 凭证不外泄 | `/api/v1/credentials` 与后端日志 | 只有 `hasSecret`，无密文/明文 |
| 越权读取 | 用 acme 租户 token 访问 demo 仓库 ID | 403 或 404，不返回内容 |
| 源码脱敏 | 在代码中写入 `password=abc123` 与 `AKIA...` 后提交任务 | 模型调用记录中不出现原文（`Redacted` 标记可见） |
| 配额拦截 | 把租户 `maxConcurrentTasks` 调成 1 后并发提交 | 第二次返回 429 语义错误 |
| 沙箱隔离 | `CA_SANDBOX_ALLOW_EXEC=false` 重跑 | `verification.degraded=true`，报告标注仅静态校验 |

---

## 四、已知边界与后续迭代建议

1. **存储实现**：默认 JSON 快照适用于单机与演示；水平扩展需按 `store.Store` 方法集实现 PostgreSQL 版本（接口已固定，替换实现即可）。
2. **堆栈解析范围**：内置解析覆盖 Java/Go/Python/Node/PHP 标准格式；小众框架（如 Erlang、Elixir）会退化为原始帧 + 低置信度，建议按需扩展 `source/stack.go` 的规则表。
3. **补丁自动生成边界**：`code_patch_synthesize` 是**规则库 + 模型**双通道；规则库未覆盖的缺陷会返回"未匹配到可自动修复模式"并转人工（这是刻意的安全设计，不伪造补丁）。
4. **沙箱执行能力**：受 `sandbox.networkIsolated` 限制，依赖下载类命令会被拒绝并降级为静态校验；完整编译验证需在具备离线依赖缓存的镜像中运行。
5. **跨仓库链路推断**：`cross_repo_trace` 基于 imports、HTTP/gRPC 客户端调用与端口串的静态推断，对动态路由（如注册中心发现）置信度较低，报告会显式标注置信度。
6. **前端分页**：仓库/分组列表当前按单页 200 条拉取后前端筛选，仓库规模上千时应改为服务端分页与按层筛选。
7. **模型成本护栏**：MCP 层已累计 Token 与兜底次数，建议在生产按租户接入预算熔断（`RunUsage.TotalTokens` 已具备数据基础）。
