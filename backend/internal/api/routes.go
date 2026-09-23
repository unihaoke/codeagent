package handler

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/codeagent/backend/internal/httpx"
)

// 授权 scope 常量（与契约 §2.2 / 权限模型对齐）。
const (
	scopeAdminAll   = "admin:all"
	scopeRepoRead   = "repo:read"
	scopeRepoWrite  = "repo:write"
	scopeGroupRead  = "group:read"
	scopeGroupWrite = "group:write"
	scopeTaskRead   = "task:read"
	scopeTaskWrite  = "task:write"
	scopeReportRead = "report:read"
	scopeSkillAdmin = "skill:admin"
	scopeModelRead  = "model:read"
	scopeModelAdmin = "model:admin"
	scopeObsRead    = "observability:read"
)

// Register 注册全部 API 路由。
//
// 路由表严格对齐 CONTRACT §2.3（路径与语义不得增删改动）：
//   - 健康检查与指标挂在根路径 `/healthz`、`/readyz`、`/metrics`，
//     同时保留 `/api/v1` 前缀别名，兼容以 API 根为基准的前端调用；
//   - 其余全部资源接口挂在 `/api/v1` 前缀下（Go 1.22 ServeMux 模式语法）。
//
// 授权策略：管理类路由要求 admin:all；业务路由要求对应业务 scope
// （admin:all 通配，见 domain.Subject.Has）。
func Register(mux *http.ServeMux, d *Deps) error {
	if mux == nil {
		return fatal("mux 不能为空")
	}
	if d == nil {
		return fatal("deps 不能为空")
	}
	if d.Store == nil || d.Repos == nil {
		return fatal("deps.Store 与 deps.Repos 不能为空")
	}
	if d.Log == nil {
		d.Log = nil // 由 d.log() 兜底为 no-op
	}

	// ------------------------------ 系统 ------------------------------
	mux.HandleFunc("GET /healthz", d.handleHealthz)
	mux.HandleFunc("GET /readyz", d.handleReadyz)
	mux.HandleFunc("GET /metrics", d.handleMetrics)
	mux.HandleFunc("GET /api/v1/healthz", d.handleHealthz)
	mux.HandleFunc("GET /api/v1/readyz", d.handleReadyz)
	mux.HandleFunc("GET /api/v1/metrics", d.handleMetrics)

	// ------------------------------ 认证 ------------------------------
	// /auth/login 免认证（见 internal/httpx 的 publicPaths 白名单）。
	mux.HandleFunc("POST /api/v1/auth/login", d.handleLogin)
	mux.HandleFunc("POST /auth/login", d.handleLogin)
	mux.Handle("GET /api/v1/auth/profile", requireScope("")(http.HandlerFunc(d.handleProfile)))
	mux.Handle("GET /auth/profile", requireScope("")(http.HandlerFunc(d.handleProfile)))

	// ------------------------------ 租户 ------------------------------
	mux.Handle("GET /api/v1/tenants", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleListTenants)))
	mux.Handle("POST /api/v1/tenants", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleCreateTenant)))
	mux.Handle("GET /api/v1/tenants/current", http.HandlerFunc(d.handleGetCurrentTenant))
	mux.Handle("PUT /api/v1/tenants/current", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleUpdateCurrentTenant)))

	// ------------------------------ API Key ------------------------------
	mux.Handle("GET /api/v1/api-keys", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleListAPIKeys)))
	mux.Handle("POST /api/v1/api-keys", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleCreateAPIKey)))
	mux.Handle("DELETE /api/v1/api-keys/{id}", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleRevokeAPIKey)))

	// ------------------------------ 凭证 ------------------------------
	mux.Handle("GET /api/v1/credentials", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleListCredentials)))
	mux.Handle("POST /api/v1/credentials", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleCreateCredential)))
	mux.Handle("DELETE /api/v1/credentials/{id}", requireScope(scopeAdminAll)(http.HandlerFunc(d.handleDeleteCredential)))

	// ------------------------------ 仓库 ------------------------------
	mux.Handle("GET /api/v1/repos", requireScope(scopeRepoRead)(http.HandlerFunc(d.handleListRepos)))
	mux.Handle("POST /api/v1/repos", requireScope(scopeRepoWrite)(http.HandlerFunc(d.handleCreateRepo)))
	mux.Handle("GET /api/v1/repos/{id}", requireScope(scopeRepoRead)(http.HandlerFunc(d.handleGetRepo)))
	mux.Handle("PUT /api/v1/repos/{id}", requireScope(scopeRepoWrite)(http.HandlerFunc(d.handleUpdateRepo)))
	mux.Handle("DELETE /api/v1/repos/{id}", requireScope(scopeRepoWrite)(http.HandlerFunc(d.handleDeleteRepo)))
	mux.Handle("POST /api/v1/repos/{id}/probe", requireScope(scopeRepoWrite)(http.HandlerFunc(d.handleProbeRepo)))

	// ------------------------------ 分组 ------------------------------
	mux.Handle("GET /api/v1/groups", requireScope(scopeGroupRead)(http.HandlerFunc(d.handleListGroups)))
	mux.Handle("POST /api/v1/groups", requireScope(scopeGroupWrite)(http.HandlerFunc(d.handleCreateGroup)))
	mux.Handle("GET /api/v1/groups/{id}", requireScope(scopeGroupRead)(http.HandlerFunc(d.handleGetGroup)))
	mux.Handle("PUT /api/v1/groups/{id}", requireScope(scopeGroupWrite)(http.HandlerFunc(d.handleUpdateGroup)))
	mux.Handle("DELETE /api/v1/groups/{id}", requireScope(scopeGroupWrite)(http.HandlerFunc(d.handleDeleteGroup)))
	mux.Handle("GET /api/v1/groups/{id}/impact", requireScope(scopeGroupRead)(http.HandlerFunc(d.handleGroupImpact)))

	// ------------------------------ 任务 / 运行（核心） ------------------------------
	mux.Handle("POST /api/v1/tasks", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleCreateTask)))
	mux.Handle("GET /api/v1/tasks", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleListTasks)))
	mux.Handle("GET /api/v1/tasks/{id}", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleGetTask)))

	mux.Handle("GET /api/v1/runs", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleListRuns)))
	mux.Handle("GET /api/v1/runs/{runId}", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleGetRun)))
	mux.Handle("POST /api/v1/runs/{runId}/cancel", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleCancelRun)))
	mux.Handle("POST /api/v1/runs/{runId}/retry", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleRetryRun)))
	mux.Handle("POST /api/v1/runs/{runId}/feedback", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleRunFeedback)))
	mux.Handle("GET /api/v1/runs/{runId}/events", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleRunEvents)))
	mux.Handle("GET /api/v1/runs/{runId}/skill-calls", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleRunSkillCalls)))
	mux.Handle("GET /api/v1/runs/{runId}/model-calls", requireScope(scopeTaskRead)(http.HandlerFunc(d.handleRunModelCalls)))

	// ------------------------------ 报告 ------------------------------
	mux.Handle("GET /api/v1/reports", requireScope(scopeReportRead)(http.HandlerFunc(d.handleListReports)))
	mux.Handle("GET /api/v1/reports/{id}", requireScope(scopeReportRead)(http.HandlerFunc(d.handleGetReport)))
	mux.Handle("GET /api/v1/reports/{id}/markdown", requireScope(scopeReportRead)(http.HandlerFunc(d.handleReportMarkdown)))

	// ------------------------------ 技能 ------------------------------
	mux.Handle("GET /api/v1/skills", requireScope(scopeRepoRead)(http.HandlerFunc(d.handleListSkills)))
	mux.Handle("GET /api/v1/skills/{name}", requireScope(scopeRepoRead)(http.HandlerFunc(d.handleGetSkill)))
	mux.Handle("POST /api/v1/skills/{name}/status", requireScope(scopeSkillAdmin)(http.HandlerFunc(d.handleSkillStatus)))
	mux.Handle("POST /api/v1/skills/{name}/rollback", requireScope(scopeSkillAdmin)(http.HandlerFunc(d.handleSkillRollback)))

	// ------------------------------ 模型 ------------------------------
	// 只读视图要求 model:read；AI 设置（写）要求更高权限 model:admin（admin:all 通配）。
	mux.Handle("GET /api/v1/models", requireScope(scopeModelRead)(http.HandlerFunc(d.handleListModels)))
	mux.Handle("GET /api/v1/models/stats", requireScope(scopeModelRead)(http.HandlerFunc(d.handleModelStats)))
	mux.Handle("GET /api/v1/models/config", requireScope(scopeModelAdmin)(http.HandlerFunc(d.handleGetModelConfig)))
	mux.Handle("PUT /api/v1/models/config", requireScope(scopeModelAdmin)(http.HandlerFunc(d.handleUpdateModelConfig)))
	mux.Handle("POST /api/v1/models/test", requireScope(scopeModelAdmin)(http.HandlerFunc(d.handleTestModelProvider)))

	// ------------------------------ 可观测 ------------------------------
	mux.Handle("GET /api/v1/observability/summary", requireScope(scopeObsRead)(http.HandlerFunc(d.handleObservabilitySummary)))
	mux.Handle("GET /api/v1/observability/audits", requireScope(scopeObsRead)(http.HandlerFunc(d.handleObservabilityAudits)))

	// ------------------------------ 第三方开放接口（OpenAPI） ------------------------------
	// 与控制台接口解耦：调用方无需感知内部 repoId，可用 git 地址或主机标识定位仓库；
	// 统一要求 task:write scope（由 API Key 携带）。同步 analyze 阻塞等结果，异步 tasks 受理即返回并终态回调。
	mux.Handle("POST /api/v1/openapi/analyze", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleOpenAPIAnalyze)))
	mux.Handle("POST /api/v1/openapi/tasks", requireScope(scopeTaskWrite)(http.HandlerFunc(d.handleOpenAPICreateTask)))

	// ------------------------------ WebSocket ------------------------------
	mux.Handle("GET /api/v1/ws", requireScope("")(http.HandlerFunc(d.handleWS)))

	// 路由兜底：任何未匹配请求统一返回 JSON 信封 404。
	//
	// 背景：Go 1.22 ServeMux 默认回 text/plain 的 `404 page not found`，
	// 前端 ApiError 无法解析，界面只能显示"未知错误"，排障时也无法区分
	// "路径写错/代理没转发"与"业务资源确实不存在"。此处统一为项目信封格式，
	// 并在 message 中回显方法与路径（不含请求体），便于直接定位。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, httpx.ErrNotFoundFn(fmt.Sprintf(
			"接口不存在：%s %s（请检查路径、方法或网关转发规则）", r.Method, r.URL.Path)))
	})

	return nil
}

// handleWS GET /api/v1/ws?token=<jwt>&runId=<runId> —— WebSocket 实时事件推送。
//
// 认证：WebSocket 无法由浏览器自定义头，故认证中间件额外支持 `?token=` / `?apiKey=`
// 查询参数；此处再做一次会话级校验，确保订阅的租户与主体一致（防越权订阅）。
func (d *Deps) handleWS(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "ws.connect")
	sub := subject(r)
	runID := r.URL.Query().Get("runId")

	tenantID := sub.TenantID
	if t := r.URL.Query().Get("tenantId"); t != "" && t != tenantID {
		// 仅管理主体可跨租户订阅。
		if !sub.Has(scopeAdminAll) {
			end(d, r, reqID, "ws.connect", errors.New("跨租户订阅被拒绝"))
			writeError(w, r, errForbidden("无权订阅其他租户的事件流"))
			return
		}
		tenantID = t
	}
	if d.Hub == nil {
		end(d, r, reqID, "ws.connect", errors.New("hub 未装配"))
		writeError(w, r, errUnavailable("WebSocket 推送未启用", nil))
		return
	}
	d.log().Debug("ws.upgrade", "req", reqID, "tenant", tenantID, "run", runID)
	d.Hub.HandleWS(w, r, tenantID, runID)
	end(d, r, reqID, "ws.connect", nil)
}
