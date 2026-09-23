# CodeAgent 部署与运维手册

本文档面向部署与运维角色，覆盖三种部署形态、配置项全表、可观测接入与故障处置。

---

## 1. 部署形态总览

| 形态 | 适用场景 | 形态要点 |
| --- | --- | --- |
| **A. 本地轻量化** | 个人 IDE 本地代码修复、调试 | 单二进制 + 内存存储；可选开启匿名调用；沙箱命令执行可关闭以避免本地副作用 |
| **B. 服务端私有化** | 企业团队多项目统一接入、故障自动化排查 | Nginx 托管前端 + 后端 API/SSE/WS；文件快照或 PostgreSQL 存储；接入企业大模型 |
| **C. 流水线集成** | CI/CD 提交自动检测、卡点预警 | 以 `ref=$GIT_COMMIT` + `idempotencyKey=$CI_PIPELINE_ID` 调用；`callbackUrl` 回传终态 |

---

## 2. 形态 A：本地轻量化

```bash
# 1) 构建
node scripts/build.mjs

# 2) 运行（关闭鉴权、关闭命令执行、内存存储）
CA_ALLOW_ANONYMOUS=true \
CA_SANDBOX_ALLOW_EXEC=false \
CA_STORE_DRIVER=memory \
./dist/codeagent-server --addr :8080 --web ./dist/web
```

要点与风险：

- `CA_ALLOW_ANONYMOUS=true` 时未携带凭证的请求会被注入匿名主体（租户取 `X-Tenant-Id`，缺省 `t-demo`，scope `admin:all`）。
  **仅限本机使用**，切勿在可被外部访问的地址上开启。
- `CA_SANDBOX_ALLOW_EXEC=false` 时沙箱只做静态校验（含 Go 语法解析、括号配平、危险操作扫描），
  验证结果的 `Verification.Degraded=true`，报告会明确标注"仅静态校验"。
- 内存存储进程退出即丢失；如需保留排查历史，改回 `CA_STORE_DRIVER=file`。

---

## 3. 形态 B：服务端私有化

### 3.1 单进程形态（后端同时托管 SPA）

```bash
node scripts/build.mjs
./dist/codeagent-server --config deploy/config.example.json --web ./dist/web
```

访问 `http://<host>:8080` 即为控制台；`/api/v1/*` 为接口。

### 3.2 Nginx + 后端分离形态（推荐）

```bash
pnpm --dir frontend run build        # 产出 frontend/dist
docker compose up -d --build         # 控制台 http://localhost:8090
```

`deploy/nginx.conf` 已处理三件容易踩坑的事：

1. SPA history 路由回退：`try_files $uri $uri/ /index.html`；
2. **SSE 不被缓冲截断**：`/api/` 关闭 `proxy_buffering`、`proxy_read_timeout 3600s`（否则任务实时事件流会"看起来卡住"）；
3. WebSocket 升级：`/api/v1/ws` 单独配置 `Upgrade/Connection` 头。

### 3.3 配置项全表

配置优先级：**默认值 < 配置文件 JSON < 环境变量 `CA_*`**。

| JSON 路径 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `server.addr` | `CA_ADDR` | `:8080` | 监听地址 |
| `server.readTimeoutSec` | - | `30` | 读头超时 |
| `server.corsOrigins` | - | `["*"]` | 生产应收敛为具体域名 |
| `server.rateLimitPerMin` | `CA_RATE_LIMIT` | `600` | 单租户/来源 IP 每分钟请求上限 |
| `server.maxBodyBytes` | - | `4194304` | 请求体上限（大堆栈可调大） |
| `auth.jwtSecret` | `CA_JWT_SECRET` | 开发默认值 | **生产必须替换** |
| `auth.jwtTtlHours` | - | `12` | 控制台会话有效期 |
| `auth.adminUsername` | `CA_ADMIN_USERNAME` | `admin` | 控制台账号 |
| `auth.adminPasswordHash` | `CA_ADMIN_PASSWORD_HASH` | 空（默认 `admin123`） | bcrypt 哈希；生产必须设置 |
| `auth.allowAnonymous` | `CA_ALLOW_ANONYMOUS` | `false` | 匿名调用（仅形态 A） |
| `security.encryptionKey` | `CA_ENCRYPTION_KEY` | 开发默认值 | **生产必须替换**；更换后已存凭证需重新录入 |
| `security.maskEnabled` | `CA_MASK_ENABLED` | `true` | 送模型前源码脱敏 |
| `security.maskRules` | - | `[]` | 追加的自定义脱敏正则 |
| `engine.workers` | `CA_WORKERS` | `4` | 任务并发 worker 数 |
| `engine.queueSize` | - | `256` | 任务队列长度 |
| `engine.taskTimeoutSec` | `CA_TASK_TIMEOUT_SEC` | `900` | 单任务超时 |
| `engine.maxRepairRounds` | - | `2` | 修复-验证最大迭代轮数（收敛上限） |
| `engine.autoFix` | - | `true` | 关闭则只做根因分析不产补丁 |
| `engine.idempotencyTtlSec` | - | `3600` | 幂等复用窗口 |
| `source.cacheDir` | `CA_CACHE_DIR` | `data/cache` | git bare 镜像目录 |
| `source.workspaceDir` | `CA_WORKSPACE_DIR` | `data/workspace` | 沙箱工作区目录（建议独立卷） |
| `source.cacheCapacity` | - | `2000` | 文件级 LRU 缓存条目上限 |
| `source.cacheTtlSec` | - | `1800` | 文件缓存有效期 |
| `source.gitBinary` | `CA_GIT_BINARY` | `git` | git 可执行文件路径 |
| `source.cloneTimeoutSec` | - | `180` | 单仓库拉取超时 |
| `sandbox.enabled` | `CA_SANDBOX_ENABLED` | `true` | 是否启用沙箱验证 |
| `sandbox.allowCommandExec` | `CA_SANDBOX_ALLOW_EXEC` | `true` | 是否允许编译/测试命令 |
| `sandbox.commandTimeoutSec` | - | `120` | 单条校验命令超时 |
| `sandbox.networkIsolated` | - | `true` | 声明沙箱无网络：`npm install` 等联网命令直接拒绝并降级为静态校验 |
| `sandbox.keepWorkspace` | - | `false` | 保留工作区供人工复核（占磁盘） |
| `mcp.providers[]` | `CA_MODEL_*` | `[]` | 模型提供方（OpenAI 兼容协议） |
| `mcp.default` | `CA_MODEL_PROVIDER` | `mock` | 默认 provider |
| `mcp.routing.strongStages` | - | `root_cause, patch_synthesize` | 必须走强力模型的阶段 |
| `mcp.routing.costAware` | - | `true` | 轻任务走轻量模型降本 |
| `mcp.budget.maxChars` | - | `120000` | 上下文总预算（字符） |
| `mcp.circuit.failureThreshold` | - | `3` | 连续失败熔断阈值 |
| `mcp.mock.enabled` | - | `false` | 内置 Mock 推理（仅离线联调开启；生产必须配置真实模型） |
| `skill.failureThreshold` | - | `3` | 技能熔断阈值 |
| `skill.overrides[]` | - | `[]` | 技能级覆盖（灰度/超时/兜底链） |
| `store.driver` | `CA_STORE_DRIVER` | `file` | `memory` \| `file` |
| `store.dataFile` | `CA_STORE_FILE` | `data/state.json` | 快照文件 |
| `log.level` / `log.format` | `CA_LOG_LEVEL` / `CA_LOG_FORMAT` | `info` / `text` | 生产建议 `info` + `json` |

---

## 4. 形态 C：CI/CD 集成

```yaml
# .gitlab-ci.yml 片段
codeagent-fix:
  stage: verify
  script:
    - |
      curl -sS -X POST "$CODEAGENT/api/v1/tasks" \
        -H "X-API-Key: $CODEAGENT_KEY" -H 'Content-Type: application/json' \
        -d "{\"mode\":\"single_repo\",\"repoId\":\"$CODEAGENT_REPO_ID\",
             \"ref\":\"$CI_COMMIT_SHA\",\"source\":\"cicd\",
             \"idempotencyKey\":\"$CI_PIPELINE_ID\",\"environment\":\"staging\",
             \"callbackUrl\":\"$CI_CALLBACK\",\"stacktrace\":$(jq -Rs . < build-error.log)}"
  allow_failure: true
```

建议策略：

- **卡点预警**：任务终态为 `needs_review` / `failed` 时告警，`degraded` 时仅记录；
- **自动修复**：终态 `succeeded` 且 `patches[].risk == "low"` 时自动创建修复分支与 MR（**仍需人工合并**）；
- **幂等**：`idempotencyKey` 用流水线 ID，重复触发不会重复执行与重复计费。

---

## 5. 可观测接入

### 5.1 Prometheus

```yaml
scrape_configs:
  - job_name: codeagent
    metrics_path: /metrics
    static_configs:
      - targets: ['codeagent-server:8080']
```

关键指标与建议告警：

| 指标 | 含义 | 建议告警 |
| --- | --- | --- |
| `codeagent_runs_degraded_total` | 降级运行数 | 5 分钟内增长 > 3（说明源码或模型链路异常） |
| `codeagent_skill_calls_total{status="failed"}` | 技能失败数 | 失败率 > 20% |
| `codeagent_model_calls_total` / `codeagent_model_tokens_total` | 模型调用与 Token | Token 速率异常增长（成本护栏） |
| `codeagent_cache_hit_ratio` | 源码缓存命中率 | 持续 < 0.3（缓存容量或版本漂移问题） |
| `codeagent_uptime_seconds` | 运行时长 | 频繁归零（异常重启） |

### 5.2 日志

`log.format=json` 时每条日志带 `tenant / task / run / req / stage` 字段，可直接按 `run` 聚合出一次排查的完整轨迹；
敏感字段（`password`/`secret`/`token`/`apiKey`/`authorization`/`private_key`/`credential`）由 `logx` 兜底替换为 `***`。

### 5.3 审计与控制台

- `/api/v1/observability/summary`：修复率、技能失败率、模型兜底率、缓存命中、仓库切换、Top 技能/模型、最近失败；
- `/api/v1/observability/audits?state=skill`：按分类（task/repo/group/skill/model/source/patch/auth/quota）回溯；
- 控制台「可观测审计」页面提供同数据的可视化与 JSON 导出。

---

## 6. 故障处置手册

| 现象 | 排查步骤 | 处置 |
| --- | --- | --- |
| 任务全部 `degraded` | 看 `TaskRun.warnings` 与 `/runs/{id}` 的 `resolution` | 多为 git 不可达或 ref 不存在：检查 `data/cache/mirrors` 与 `ref` 是否为线上真实 commit |
| 候选仓库为空 | 检查仓库 `matchRules` 是否覆盖堆栈中的包名/路径/主机名 | 补全 `packagePrefixes`/`pathPrefixes`/`hostPatterns`，或传 `entryFiles` |
| 无补丁但有根因 | 报告 `suggestions` 会说明 | 属于"未匹配到可自动修复模式"，人工介入；可携带 `feedback` 重跑 |
| 补丁未通过验证 | 看 `verification.checks[].output` | 沙箱缺少编译环境时属正常：配置 `sandbox.checks` 或接受静态校验降级 |
| 模型一直兜底 | `/api/v1/models` 看 `circuitState` 与 `lastError` | 检查 provider 地址/密钥/额度；熔断会在冷却后半开自动恢复 |
| SSE 看不到实时事件 | 检查反向代理是否关闭了缓冲 | 参照 `deploy/nginx.conf` 的 `/api/` 段 |
| 队列持续满 | `/api/v1/tasks` 统计 + `engine.workers` | 提高 `engine.workers` 或收紧 `TenantQuota.maxConcurrentTasks` |
| 磁盘增长快 | `data/cache/mirrors` 与 `data/workspace` | 调小 `source.cacheCapacity`、保持 `sandbox.keepWorkspace=false`、定期清理 `data/workspace` |

---

## 7. 扩容与高可用建议

1. **无状态化**：把 `store.driver` 从 `file` 换成 PostgreSQL 实现（方法集已在 `store.Store` 固定，替换实现即可），
   即可水平扩展多个后端实例；
2. **源码缓存**：`source.cacheDir` 挂载共享卷，多实例共享 bare 镜像，避免重复克隆；
3. **沙箱隔离**：生产建议把沙箱执行下沉到独立 worker（独立命名空间/容器），主服务仅做分析与编排；
4. **模型配额**：按租户维度在 MCP 层累计 Token（`RunUsage.TotalTokens`）并接入计费/护栏；
5. **备份**：备份 `data/state.json`（或数据库）与 `security.encryptionKey`（丢失后凭证不可解密）。
