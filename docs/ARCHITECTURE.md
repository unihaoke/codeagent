# CodeAgent 架构说明

> 本文件说明七层架构如何落地为代码、每个需求条款由哪个模块承接、以及关键机制（版本锁定、
> 分组懒加载、九阶段流水线、状态机、降级矩阵）的具体实现位置。

---

## 1. 七层架构 → 代码映射

| 层 | 职责（需求文档章节） | 代码位置 | 关键接口 |
| --- | --- | --- | --- |
| ① 接入层 | 统一入口、请求清洗、参数标准化、限流、任务路由（3.2.1） | `internal/httpx`（中间件/WS/响应信封）、`internal/api`（各资源处理器）、`internal/app`（服务装配与 SPA 托管） | `app.NewServer(app.Deps{...})` |
| ② 租户&安全权限层 | 多租户隔离、Git 凭证加密、源码访问鉴权、敏感代码脱敏（3.2.2 / 八） | `internal/env`、`internal/platform/audit` | `domain.Authorizer`、`domain.CredentialBox`、`domain.Recorder` |
| ③ 仓库分组索引层 | 仓库元数据、业务分组索引、动态注册与切换（3.2.3 / 4.2） | `internal/source/matcher.go` + `internal/store`（索引持久化） | `domain.RepoIndex`、`MatchInput/Match` |
| ④ Agent 核心调度层 | 任务拆解、状态管理、持久化、多轮迭代、上下文锁定、配额、去重、闭环收敛（3.2.4） | `internal/engine` | `domain.TaskEngine`、`domain.TaskPipeline` |
| ⑤ Skill 可插拔插件层 | 技能热更新、版本兼容、熔断降级、参数强校验、沙箱隔离（3.2.5 / 五） | `internal/skill`、`internal/skill/builtin` | `domain.SkillRegistry`、`domain.SkillRunner` |
| ⑥ MCP 模型管控层 | 输入标准化、输出强约束、上下文治理、多模型调度、推理容错熔断（3.2.6 / 六） | `internal/mcp` | `domain.MCPGateway`、`domain.ContextPacker` |
| ⑦ 源码&沙箱执行层 | 精准版本拉取、文件级缓存、AST 解析、沙箱验证、编译校验（3.2.7 / 七） | `internal/source`、`internal/sandbox` | `domain.SourceResolver`、`domain.SandboxManager` |

**分层解耦的落地方式**：所有跨层调用一律经由 `internal/domain/ports.go` 中的接口，
各层只依赖接口不依赖实现。`cmd/server/main.go` 是唯一的装配点（依赖注入根），
因此任何一层都可以被替换实现或注入 fake 做单测（`internal/engine`、`internal/httpx` 的测试即为此模式）。

```
                 ┌──────────────────────────────────────────────┐
  外部调用方 ───▶ │ ① 接入层 httpx（路由/中间件/SSE/WS/限流）        │
  IDE/告警/CI/运维 │   认证：JWT(控制台) / API Key(接入)             │
                 └───────────────┬──────────────────────────────┘
                                 ▼
                 ┌──────────────────────────────────────────────┐
                 │ ② 租户&安全权限层 env                          │
                 │   租户隔离 · AES-GCM 凭证箱 · 脱敏 · 配额/限流   │
                 └───────────────┬──────────────────────────────┘
                                 ▼
                 ┌──────────────────────────────────────────────┐
                 │ ④ Agent 调度核心层 engine                       │
                 │   受理/幂等/配额 → worker pool → 九阶段流水线    │
                 └───┬───────────┬───────────┬──────────┬───────┘
                     ▼           ▼           ▼          ▼
        ┌────────────────┐ ┌──────────┐ ┌─────────┐ ┌──────────────┐
        │ ③ 仓库分组索引   │ │⑤ Skill层 │ │⑥ MCP层  │ │⑦ 源码&沙箱层  │
        │ matcher + store │ │ registry │ │ gateway │ │ source/sandbox│
        │ （只索引不存码） │ │ runner   │ │ packer  │ │ 版本锁定+缓存 │
        └────────────────┘ └──────────┘ └─────────┘ └──────────────┘
                     └────────── 可观测：eventbus + audit + logx ──────────┘
```

---

## 2. 需求条款 → 实现要点对照

| 需求条款 | 实现位置与做法 |
| --- | --- |
| 4.1 任务级代码版本锁定 | `engine` 阶段 `resolve`：每个仓库解析出 commit 写入 `TaskRun.PinnedCommits`；后续所有源码读取、补丁生成、沙箱物化**只认这个 commit**。重跑（`Retry`）复用同一快照，保证多轮修复不漂移。 |
| 4.1 多项目并发隔离 | 每个 run 独立 `domain.CallContext`（租户/任务/运行/阶段/版本快照/日志器），源码缓存键为 `repoID|commit|path`，沙箱工作区目录为 `workspace/<runID>/`，互不污染。 |
| 4.2 分组为纯逻辑索引 | 分组只存 `GroupMember` 索引与链路入口；`matcher.Match` 只读仓库元数据打分，**不加载任何代码**。 |
| 4.2 堆栈驱动懒加载 | 阶段 `candidates` 打分 → 阶段 `code_load` 仅对命中的候选仓库按 `FileHint` 加载少量文件，受 `LoadBudget` 约束；未命中的仓库永不 clone、永不读取。 |
| 4.3 任务上下文固化 | `TaskRun` 持久化绑定：租户、任务 ID、`Resolution/PinnedCommits`、`RootCause`、`Patches`、`Verification`、`ReportID`、`Usage`、`Warnings`、时间线；`EvidenceBundle` 记录命中切片元信息。 |
| 五 内置技能与容错治理 | `skill/builtin` 注册 7 个技能（`stacktrace_parse`/`code_ast_analyze`/`error_localize`/`cross_repo_trace`/`code_patch_synthesize`/`dependency_repair`/`patch_verify`）；`skill.Runner` 负责 Schema 强校验 → 熔断检查 → 隔离超时执行 → 重试 → 兜底链 → 熔断更新 → `SkillCall` 审计。 |
| 六 MCP 管控 | `mcp.Gateway.Chat`：分级调度（strong/light/fallback + 加权 + 健康度）→ 超时重试 → 结构化输出解析（容错 `ParseStructured`）→ 校验失败纠错重试 → 换模型兜底 → `ModelCall` 审计；`mcp.ContextPacker` 按预算裁剪并记录 `Notes`。 |
| 七 精准版本 + 文件级缓存 | `source.Resolver` 以 `CacheDir/mirrors/<tenant>/<repo>.git` 维护 bare 镜像，`git show <commit>:<path>` 读取文件；LRU + TTL 缓存键含 commit，仅缓存实际访问的文件。 |
| 八 安全隔离 | 见 README §9 与 `internal/env`（租户校验、AES-GCM、脱敏、scope 授权）。 |
| 九 容错/配额/降级 | 见本文 §5 降级矩阵；配额来自 `TenantQuota`，超限写 `EvidenceBundle.Notes` 与 `TaskRun.Warnings`。 |
| 十 可观测 | `platform/eventbus`（实时事件）+ `platform/audit`（审计与 `ObservabilitySummary`）+ `logx`（结构化日志，敏感字段兜底脱敏）+ `/metrics`（Prometheus 文本）。 |
| 十一 核心流程 | 见本文 §3 九阶段流水线（单仓库与分组共用同一流水线，差异只在候选范围）。 |

---

## 3. 九阶段任务流水线

阶段名固定，前后端与事件协议共用（`docs/CONTRACT.md` §3）：

| # | Stage | 输入 | 动作 | 输出 |
| --- | --- | --- | --- | --- |
| 1 | `resolve` | 请求的 repo/group + ref | 解析并锁定 commit（单仓库解析全部；分组只解析入口仓库） | `Run.Resolution`、`Run.PinnedCommits` |
| 2 | `stack_parse` | 原始堆栈 + 日志 | 技能解析异常类型/帧/符号/端点/服务名/分类 + 脱敏清洗 | `EvidenceBundle.Stack` |
| 3 | `candidates` | 堆栈 + 分组索引 | 仓库匹配打分（包/路径/类名/产物/主机/端点/关键词/分层/入口） | `EvidenceBundle.CandidateRepos` |
| 4 | `code_load` | 候选 + 锁定版本 | 堆栈驱动懒加载命中文件（LRU 缓存），受文件数/字符数预算约束 | `EvidenceBundle.Slices`、`DependencyFiles` |
| 5 | `root_cause` | 打包上下文 | MCP 强约束结构化推理；分组模式追加跨仓库链路技能 | `Run.RootCause`、`EvidenceBundle.Edges` |
| 6 | `patch_synthesize` | 根因 + 真实代码切片 | 技能产出补丁描述 → 用 `sandbox.BuildPatch` 对**真实文件内容**生成 unified diff（片段不吻合即丢弃） | `Run.Patches` |
| 7 | `sandbox_verify` | 补丁 + 锁定版本 | 隔离工作区物化 → 应用补丁 → 编译/静态校验 → 失败则带反馈重合成（≤ `MaxRepairRounds`） | `Run.Verification` |
| 8 | `report` | 全部上下文 | 生成结构化 `Report` + Markdown | `Report`、`Run.ReportID` |
| 9 | `archive` | - | 落库固化 + 回调 + `task.terminal` 事件 | 终态 |

**降级分支**：阶段 3 无命中 → 跳过 4/6/7，直接以"堆栈文本 + 无代码上下文"完成 5/8，最终状态 `degraded`，报告显式说明缺失证据与补全建议。

---

## 4. 任务状态机

```
queued ──▶ analyzing ──▶ repairing ──▶ verifying ──┬──▶ succeeded
   │            │             │              │      ├──▶ needs_review
   │            │             │              │      └──▶ degraded
   ├──▶ cancelled            │              │
   └──▶ failed ◀─────────────┴──────────────┘
```

- 迁移合法性由 `domain.TaskState.CanTransitionTo` 强校验，非法迁移拒绝执行并记错误日志。
- 终态集合：`succeeded / needs_review / failed / cancelled / degraded`（不可再迁移）。
- 终态判定集中在 `engine.DecideState`（便于单测）：有补丁且验证通过且非降级 → `succeeded`；
  有补丁但验证未过/验证降级/高风险 → `needs_review`；无补丁 → `needs_review`；全程无代码证据或模型不可用 → `degraded`；致命异常 → `failed`。

---

## 5. 容错与降级矩阵（九.1/9.3）

| 故障点 | 现象 | 系统行为 | 用户可见 |
| --- | --- | --- | --- |
| 远程 Git 不可达 / 无 git | `Resolve`/`Load` 失败 | 跳过代码证据，继续堆栈分析 | `Warnings` + `degraded` + 报告建议补 Commit 重跑 |
| 堆栈线索不足 | 候选仓库为空 | 不加载代码，直接基于堆栈推理根因 | `degraded`，报告标注"线索不足" |
| 代码量超预算 | 文件数/字符数超 `TenantQuota` | 截断加载、保留焦点窗口 | `EvidenceBundle.Truncated` + `Notes` |
| 模型超时 / 5xx | provider 失败 | 重试 → 换同层模型 → 兜底模型 | `ModelCall.fallbackFrom`、`ModelStats.fallbackCalls` |
| 模型输出非 JSON | `ParseStructured` 失败 | 纠错重试（回灌校验错误）→ 换模型 → 降级为文本 | `ModelResponse.Repaired` / 文本模式 |
| 全部模型不可用 | 推理失败 | 用堆栈规则构造确定性根因，跳过补丁 | `degraded` + 报告说明 |
| 技能连续失败 | 熔断阈值触发 | 熔断打开 → 走备用技能链 → 仍失败则跳过该技能 | `/skills` 显示 `circuitState=open`、`SkillCall.status=blocked` |
| 补丁片段与真实代码不匹配 | `ErrSnippetNotFound` | **丢弃该补丁**并记录原因（绝不伪造补丁） | `Warnings` + 补丁数减少 |
| 验证失败 | 编译/静态校验不通过 | 带失败输出反馈重合成，≤ `MaxRepairRounds` 轮 | 每轮结果写入 `Warnings`，最终 `needs_review` |
| 沙箱命令执行被禁用 | `AllowCommandExec=false` | 仅做静态校验（含 Go 语法解析等） | `Verification.Degraded=true` |
| 队列满 / 并发超限 | 提交被拒 | 返回 `429` 语义错误，提示稍后重试 | 错误码 + 中文说明 |

---

## 6. 并行开发组织方式（本项目的工程实践）

1. 先冻结 `internal/domain`（模型 + 端口接口）与 `docs/CONTRACT.md`（文件归属、HTTP/WS 协议、技能 I/O Schema、验收项）。
2. 七个后端层 + 四个前端页面组**并行开发**，每层只允许新建自己目录下的文件，跨层只通过 `domain` 接口交互。
3. 每层必须自带 `_test.go`（fake 注入，不依赖网络/git），`go build ./... && go vet ./... && go test ./...` 全绿。
4. `cmd/server/main.go` 作为唯一装配点最后完成，避免并行期间的接口漂移。
5. 失败面收敛：所有跨层错误以 `errors.Is` 可识别的包级错误暴露，由接入层映射为 HTTP 语义码。

该方式直接对应"架构无耦合、可扩展、可热更新、可长期迭代"的架构目标。
