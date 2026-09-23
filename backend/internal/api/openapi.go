package handler

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// ---------------------------------------------------------------------------
// 第三方开放接口（OpenAPI）：同步分析 + 异步提交（回调）
//
// 与控制台接口解耦，单独挂在 /api/v1/openapi/* 下，统一要求 task:write scope
// （由 API Key 携带）。核心差异：调用方无需感知内部 repoId，可用 git 地址或
// 主机标识定位仓库；结果以结构化形态返回，便于直接推送飞书等告警。
// ---------------------------------------------------------------------------

// openAPISubmitRequest 开放接口统一提交体（同步/异步共用）。
type openAPISubmitRequest struct {
	RepoLocator    *repoLocator `json:"repoLocator,omitempty"`
	RepoID         string       `json:"repoId,omitempty"`
	GroupID        string       `json:"groupId,omitempty"`
	Stacktrace     string       `json:"stacktrace"`
	Logs           string       `json:"logs,omitempty"`
	EntryFiles     []string     `json:"entryFiles,omitempty"`
	Environment    string       `json:"environment,omitempty"`
	AutoVerify     *bool        `json:"autoVerify,omitempty"`
	Priority       int          `json:"priority,omitempty"`
	IdempotencyKey string       `json:"idempotencyKey,omitempty"`
	CallbackURL    string       `json:"callbackUrl,omitempty"`
	// Timeout 同步等待上限（秒），默认 120，上限 300。
	Timeout int `json:"timeout,omitempty"`
}

// repoLocator 仓库定位器：git 地址或主机标识二选一。
type repoLocator struct {
	GitURL string `json:"gitUrl,omitempty"`
	Host   string `json:"host,omitempty"`
}

// openAPIAnalysisResult 分析摘要（对齐飞书等告警渲染）。
type openAPIAnalysisResult struct {
	RunID       string               `json:"runId"`
	TaskID      string               `json:"taskId"`
	Status      string               `json:"status"`
	Severity    string               `json:"severity,omitempty"`
	Repo        *openAPIRepoView     `json:"repo,omitempty"`
	RootCause   *domain.RootCause    `json:"rootCause,omitempty"`
	Patches     []patchView          `json:"patches,omitempty"`
	Verification *domain.Verification `json:"verification,omitempty"`
	ReportID    string               `json:"reportId,omitempty"`
	ReportURL   string               `json:"reportUrl,omitempty"`
	Markdown    string               `json:"markdown,omitempty"`
	ElapsedMs   int64                `json:"elapsedMs,omitempty"`
	Usage       domain.RunUsage      `json:"usage,omitempty"`
	Warnings    []string             `json:"warnings,omitempty"`
	Degraded    bool                 `json:"degraded,omitempty"`
	Error       string               `json:"error,omitempty"`
}

type openAPIRepoView struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// handleOpenAPIAnalyze POST /api/v1/openapi/analyze —— 同步：定位仓库并阻塞等待结果。
func (d *Deps) handleOpenAPIAnalyze(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "openapi.analyze")
	sub := subject(r)
	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "openapi.analyze", fmt.Errorf("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配，无法受理任务", nil))
		return
	}
	var req openAPISubmitRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "openapi.analyze", err)
		writeError(w, r, err)
		return
	}
	if req.Stacktrace == "" {
		end(d, r, reqID, "openapi.analyze", fmt.Errorf("stacktrace 为空"))
		writeError(w, r, errBadRequest("stacktrace 为必填"))
		return
	}
	if len(req.Stacktrace) > maxStacktraceBytes {
		end(d, r, reqID, "openapi.analyze", fmt.Errorf("stacktrace 过大"))
		writeError(w, r, errBadRequest(fmt.Sprintf("stacktrace 超出 %d 字节上限", maxStacktraceBytes)))
		return
	}

	repoID, groupID, warns, err := d.resolveOpenAPITarget(sub.TenantID, &req)
	if err != nil {
		end(d, r, reqID, "openapi.analyze", err)
		writeError(w, r, errNotFoundFn("仓库定位失败："+err.Error()))
		return
	}

	domainReq := buildOpenAPITaskRequest(sub, &req, repoID, groupID)
	run, err := d.Engine.Submit(r.Context(), sub, domainReq)
	if err != nil || run == nil {
		end(d, r, reqID, "openapi.analyze", err)
		writeError(w, r, err)
		return
	}

	// 同步等待终态；超时（默认 120s，上限 300s）转异步提示。
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 300 {
		timeout = 300
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		cur, gerr := d.Engine.GetRun(r.Context(), sub.TenantID, run.ID)
		if gerr == nil && cur != nil && cur.State.IsTerminal() {
			result := assembleOpenAPIResult(d, sub.TenantID, cur, warns)
			httpx.WriteJSON(w, r, result)
			end(d, r, reqID, "openapi.analyze", nil)
			return
		}
		if time.Now().After(deadline) {
			status := "running"
			if cur != nil {
				status = string(cur.State)
			}
			w.WriteHeader(http.StatusAccepted)
			httpx.WriteJSON(w, r, map[string]any{
				"runId":   run.ID,
				"taskId":  run.TaskID,
				"status":  status,
				"pollUrl": "/api/v1/runs/" + run.ID,
				"message": "分析未在超时内完成，请轮询 pollUrl 或提供 callbackUrl 等待回调",
			})
			end(d, r, reqID, "openapi.analyze", nil)
			return
		}
		select {
		case <-ticker.C:
		case <-r.Context().Done():
			end(d, r, reqID, "openapi.analyze", r.Context().Err())
			writeError(w, r, errUnavailable("客户端断开，分析在后台继续", nil))
			return
		}
	}
}

// handleOpenAPICreateTask POST /api/v1/openapi/tasks —— 异步：受理即返回，终态回调。
func (d *Deps) handleOpenAPICreateTask(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "openapi.task.create")
	sub := subject(r)
	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "openapi.task.create", fmt.Errorf("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配，无法受理任务", nil))
		return
	}
	var req openAPISubmitRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "openapi.task.create", err)
		writeError(w, r, err)
		return
	}
	if req.Stacktrace == "" {
		end(d, r, reqID, "openapi.task.create", fmt.Errorf("stacktrace 为空"))
		writeError(w, r, errBadRequest("stacktrace 为必填"))
		return
	}
	if len(req.Stacktrace) > maxStacktraceBytes {
		end(d, r, reqID, "openapi.task.create", fmt.Errorf("stacktrace 过大"))
		writeError(w, r, errBadRequest(fmt.Sprintf("stacktrace 超出 %d 字节上限", maxStacktraceBytes)))
		return
	}
	if req.CallbackURL == "" {
		end(d, r, reqID, "openapi.task.create", fmt.Errorf("缺少 callbackUrl"))
		writeError(w, r, errBadRequest("异步模式必须提供 callbackUrl，终态将回调该地址"))
		return
	}

	repoID, groupID, warns, err := d.resolveOpenAPITarget(sub.TenantID, &req)
	if err != nil {
		end(d, r, reqID, "openapi.task.create", err)
		writeError(w, r, errNotFoundFn("仓库定位失败："+err.Error()))
		return
	}

	domainReq := buildOpenAPITaskRequest(sub, &req, repoID, groupID)
	run, err := d.Engine.Submit(r.Context(), sub, domainReq)
	if err != nil || run == nil {
		end(d, r, reqID, "openapi.task.create", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "task", "openapi.submit", "info", "受理开放接口任务 "+run.ID, map[string]any{
		"runId": run.ID, "taskId": run.TaskID, "warnings": warns,
	})
	w.WriteHeader(http.StatusAccepted)
	httpx.WriteJSON(w, r, map[string]any{
		"runId":      run.ID,
		"taskId":     run.TaskID,
		"status":     "queued",
		"acceptTime": run.CreatedAt,
	})
	end(d, r, reqID, "openapi.task.create", nil)
}

// resolveOpenAPITarget 解析提交目标，返回 (repoID, groupID, warnings, err)。
// 三者互斥：groupId 优先走分组模式；否则 repoId 或 locator 解析单仓库。
func (d *Deps) resolveOpenAPITarget(tenantID string, req *openAPISubmitRequest) (repoID, groupID string, warns []string, err error) {
	if req.GroupID != "" {
		if _, ok := d.Store.GetGroup(tenantID, req.GroupID); !ok {
			return "", "", nil, fmt.Errorf("groupId 不存在或不属于当前租户")
		}
		return "", req.GroupID, nil, nil
	}
	if req.RepoID != "" {
		if repo, ok := d.Store.GetRepo(tenantID, req.RepoID); ok {
			return repo.ID, "", nil, nil
		}
		if repo, ok := d.Store.GetRepoByKey(tenantID, req.RepoID); ok {
			return repo.ID, "", nil, nil
		}
		return "", "", nil, fmt.Errorf("repoId 不存在或不属于当前租户")
	}
	if req.RepoLocator == nil || (req.RepoLocator.GitURL == "" && req.RepoLocator.Host == "") {
		return "", "", nil, fmt.Errorf("必须提供 repoLocator(gitUrl/host)、repoId 或 groupId 之一")
	}
	repos := d.Store.ListRepos(tenantID)
	type cand struct {
		repo  *domain.Repository
		score int
		by    string
	}
	var cands []cand
	for i := range repos {
		sc, by := matchRepoLocator(&repos[i], req.RepoLocator)
		if sc > 0 {
			cands = append(cands, cand{&repos[i], sc, by})
		}
	}
	if len(cands) == 0 {
		return "", "", nil, fmt.Errorf("未匹配到仓库：请先在控制台注册仓库并配置 matchRules(hostPatterns/endpointPatterns/keywords)")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	best := cands[0]
	if len(cands) > 1 && cands[1].score == best.score {
		warns = append(warns, "多个仓库匹配，已选用 key="+best.repo.Key+"，by="+best.by)
	}
	return best.repo.ID, "", warns, nil
}

// matchRepoLocator 按 git 地址/主机标识给仓库打分（越高越优）。
//
// 优先级：git 地址精确匹配 URL(100) > 主机模式(80) > URL 片段(70) >
// 端点模式(60) > 关键词(50) > 仓库 key/name 片段(40)。
func matchRepoLocator(repo *domain.Repository, loc *repoLocator) (int, string) {
	best, by := 0, ""
	if loc.GitURL != "" && repo.URL != "" && strings.EqualFold(repo.URL, loc.GitURL) {
		return 100, "url"
	}
	for _, p := range repo.MatchRules.HostPatterns {
		if matchTerm(loc.GitURL, p) || matchTerm(loc.Host, p) {
			if 80 > best {
				best, by = 80, "hostPattern"
			}
		}
	}
	if best < 70 {
		if loc.GitURL != "" && ciHas(repo.URL, loc.GitURL) {
			best, by = 70, "urlFragment"
		}
	}
	if best < 60 {
		for _, p := range repo.MatchRules.EndpointPatterns {
			if matchTerm(loc.GitURL, p) || matchTerm(loc.Host, p) {
				best, by = 60, "endpointPattern"
				break
			}
		}
	}
	if best < 50 {
		for _, k := range repo.MatchRules.Keywords {
			if matchTerm(loc.GitURL, k) || matchTerm(loc.Host, k) {
				best, by = 50, "keyword"
				break
			}
		}
	}
	if best < 40 {
		if (loc.Host != "" && (ciHas(repo.Key, loc.Host) || ciHas(repo.Name, loc.Host))) ||
			(loc.GitURL != "" && (ciHas(repo.Key, loc.GitURL) || ciHas(repo.Name, loc.GitURL))) {
			best, by = 40, "nameOrKey"
		}
	}
	return best, by
}

// matchTerm 大小写不敏感地判断 hay 是否包含 needle（needle 非空）。
func matchTerm(hay, needle string) bool {
	return needle != "" && ciHas(hay, needle)
}

func ciHas(hay, needle string) bool {
	return needle != "" && strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// buildOpenAPITaskRequest 将开放接口请求转换为领域任务请求。
func buildOpenAPITaskRequest(sub *domain.Subject, req *openAPISubmitRequest, repoID, groupID string) domain.CreateTaskRequest {
	title := "开放接口异常分析"
	if req.RepoLocator != nil {
		if req.RepoLocator.Host != "" {
			title = "异常分析@" + req.RepoLocator.Host
		} else if req.RepoLocator.GitURL != "" {
			title = "异常分析@" + req.RepoLocator.GitURL
		}
	}
	return domain.CreateTaskRequest{
		RepoID:         repoID,
		GroupID:        groupID,
		Source:         domain.TriggerOpenAPI,
		Title:          title,
		Stacktrace:     req.Stacktrace,
		Logs:           req.Logs,
		EntryFiles:     req.EntryFiles,
		IdempotencyKey: req.IdempotencyKey,
		Priority:       req.Priority,
		Environment:    req.Environment,
		AutoVerify:     req.AutoVerify,
		CallbackURL:    req.CallbackURL,
		TenantID:       sub.TenantID,
		RequestID:      "",
	}
}

// assembleOpenAPIResult 将终态 run 组装为分析摘要。
func assembleOpenAPIResult(d *Deps, tenantID string, run *domain.TaskRun, warns []string) *openAPIAnalysisResult {
	res := &openAPIAnalysisResult{
		RunID:        run.ID,
		TaskID:       run.TaskID,
		Status:       string(run.State),
		Severity:     string(run.Severity),
		RootCause:    run.RootCause,
		Patches:      toPatchViews(run.Patches),
		Verification: run.Verification,
		ReportID:     run.ReportID,
		ElapsedMs:    run.ElapsedMS(),
		Usage:        run.Usage,
		Warnings:     warns,
		Degraded:     run.Degraded,
		Error:        run.Error,
	}
	// 仓库视图：优先用 run 关联的仓库 ID 取完整信息，缺失则退回 Resolution。
	if len(run.RepoIDs) > 0 {
		if repo, ok := d.Store.GetRepo(tenantID, run.RepoIDs[0]); ok {
			res.Repo = &openAPIRepoView{ID: repo.ID, Key: repo.Key, Name: repo.Name, URL: repo.URL}
		}
	}
	if res.Repo == nil && len(run.Resolution) > 0 {
		rr := run.Resolution[0]
		res.Repo = &openAPIRepoView{ID: rr.RepositoryID, Key: rr.RepoKey, Name: rr.Name}
	}
	if d.Cfg != nil && d.Cfg.Server.PublicURL != "" && run.ReportID != "" {
		res.ReportURL = strings.TrimRight(d.Cfg.Server.PublicURL, "/") + "/r/" + run.ReportID
	}
	if run.ReportID != "" {
		if rep, ok := d.Store.GetReport(tenantID, run.ReportID); ok {
			res.Markdown = rep.Markdown
		}
	}
	return res
}

func toPatchViews(ps []domain.Patch) []patchView {
	out := make([]patchView, 0, len(ps))
	for i := range ps {
		out = append(out, patchView{ps[i]})
	}
	return out
}
