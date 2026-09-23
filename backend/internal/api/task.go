package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/store"
)

// 入参清洗上限。
const (
	// maxStacktraceBytes 堆栈文本长度上限 200KB（超出直接 400，避免超大 body 拖垮解析）。
	maxStacktraceBytes = 200 * 1024
	// maxLogsBytes 附加日志长度上限。
	maxLogsBytes = 64 * 1024
	// maxTitleRunes 自动生成标题的最大字符数。
	maxTitleRunes = 120
	// maxFeedbackRunes 人工反馈长度上限。
	maxFeedbackRunes = 4000
	// runDetailCallLimit 运行详情内嵌的调用轨迹条数上限。
	runDetailCallLimit = 100
)

// createTaskRequest 提交任务请求体：直接复用领域模型（TenantID/RequestID 为 json:"-"，
// 必须由 handler 手动注入），保证契约字段零漂移。
type createTaskRequest = domain.CreateTaskRequest

// feedbackRequest 人工反馈请求体。
type feedbackRequest struct {
	Feedback string `json:"feedback"`
}

// taskDetail 任务详情（含最新 run 摘要）。
type taskDetail struct {
	Task      *domain.Task    `json:"task"`
	LatestRun *domain.TaskRun `json:"latestRun,omitempty"`
}

// runDetail 执行详情：任务上下文全量视图。
type runDetail struct {
	Run           *domain.TaskRun       `json:"run"`
	Resolution    []domain.RepoResolved `json:"resolution"`
	PinnedCommits map[string]string     `json:"pinnedCommits"`
	Patches       []patchView           `json:"patches"`
	Verification  *domain.Verification  `json:"verification,omitempty"`
	Usage         domain.RunUsage       `json:"usage"`
	Evidence      evidenceView          `json:"evidence"`
	SkillCalls    []domain.SkillCall    `json:"skillCalls"`
	ModelCalls    []domain.ModelCall    `json:"modelCalls"`
	Report        *domain.Report        `json:"report,omitempty"`
	Warnings      []string              `json:"warnings"`
	Degraded      bool                  `json:"degraded"`
	ElapsedMS     int64                 `json:"elapsedMs"`
	// IncludeCode 标记本次响应是否包含证据切片全文（?includeCode=1）。
	IncludeCode bool `json:"includeCode"`
}

// patchView 补丁视图：默认剥离 OldContent/NewContent（避免与 UnifiedDiff 重复放大响应），
// 仅在 ?includeCode=1 时保留全文。
type patchView struct {
	domain.Patch
}

// evidenceView 证据摘要。
//
// 默认只返回切片的元信息（path / 行号 / reason / chars），不回传完整源码内容，
// 避免运行详情响应体积失控；仅当 ?includeCode=1 时附带 content。
type evidenceView struct {
	FileCount      int             `json:"fileCount"`
	CodeChars      int             `json:"codeChars"`
	FilesLoaded    int             `json:"filesLoaded"`
	Truncated      bool            `json:"truncated"`
	Slices         []evidenceSlice `json:"slices"`
	Notes          []string        `json:"notes,omitempty"`
	CandidateRepos []candidateView `json:"candidateRepos,omitempty"`
	Available      bool            `json:"available"`
	Source         string          `json:"source,omitempty"`
}

// evidenceSlice 单个证据切片（元信息 + 可选全文）。
type evidenceSlice struct {
	RepositoryID string `json:"repositoryId,omitempty"`
	RepoKey      string `json:"repoKey,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Path         string `json:"path"`
	Language     string `json:"language,omitempty"`
	Reason       string `json:"reason,omitempty"`
	StartLine    int    `json:"startLine,omitempty"`
	EndLine      int    `json:"endLine,omitempty"`
	Chars        int    `json:"chars"`
	FocusLines   []int  `json:"focusLines,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	FromCache    bool   `json:"fromCache,omitempty"`
	Redacted     bool   `json:"redacted,omitempty"`
	Content      string `json:"content,omitempty"`
}

// candidateView 候选仓库（不含 Hints 明细以控制体积）。
type candidateView struct {
	RepositoryID string   `json:"repositoryId"`
	RepoKey      string   `json:"repoKey"`
	Name         string   `json:"name"`
	Layer        string   `json:"layer"`
	Score        float64  `json:"score"`
	MatchedBy    []string `json:"matchedBy"`
	HintCount    int      `json:"hintCount"`
}

// ---------------------------------------------------------------------------
// 任务
// ---------------------------------------------------------------------------

// handleCreateTask POST /api/v1/tasks —— 核心：提交分析任务。
//
// 处理链：严格解析 → 租户/请求 ID 注入 → 入参清洗 → 租户归属校验 → Engine.Submit
// → 201 + TaskRun。
func (d *Deps) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "task.create")
	sub := subject(r)

	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "task.create", errors.New("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配，无法受理任务", nil))
		return
	}

	var req createTaskRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "task.create", err)
		writeError(w, r, err)
		return
	}

	// 租户与链路 ID 必须由接入层注入（domain 中为 json:"-"）。
	req.TenantID = sub.TenantID
	req.RequestID = reqID

	if err := normalizeCreateTask(&req); err != nil {
		end(d, r, reqID, "task.create", err)
		writeError(w, r, err)
		return
	}
	// 租户隔离：RepoID / GroupID 必须属于当前租户。
	if req.RepoID != "" {
		if _, ok := d.Store.GetRepo(sub.TenantID, req.RepoID); !ok {
			if repo, ok2 := d.Store.GetRepoByKey(sub.TenantID, req.RepoID); ok2 {
				req.RepoID = repo.ID // 兼容以业务 key 提交
			} else {
				end(d, r, reqID, "task.create", store.ErrNotFound)
				writeError(w, r, errNotFoundFn("repoId 不存在或不属于当前租户"))
				return
			}
		}
	}
	if req.GroupID != "" {
		if _, ok := d.Store.GetGroup(sub.TenantID, req.GroupID); !ok {
			end(d, r, reqID, "task.create", store.ErrNotFound)
			writeError(w, r, errNotFoundFn("groupId 不存在或不属于当前租户"))
			return
		}
	}

	run, err := d.Engine.Submit(r.Context(), sub, req)
	if err != nil || run == nil {
		end(d, r, reqID, "task.create", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "task", "submit", "info", "受理任务 "+run.ID, map[string]any{
		"runId": run.ID, "taskId": run.TaskID, "repoId": req.RepoID, "groupId": req.GroupID,
	})
	w.WriteHeader(http.StatusCreated)
	httpx.WriteJSON(w, r, run)
	end(d, r, reqID, "task.create", nil)
}

// handleListTasks GET /api/v1/tasks —— 逻辑任务列表（分页/关键字）。
func (d *Deps) handleListTasks(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "task.list")
	page, err := d.provider().ListTasks(r.Context(), subject(r).TenantID, pageQuery(r))
	if err != nil {
		end(d, r, reqID, "task.list", err)
		writeError(w, r, err)
		return
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "task.list", nil)
}

// handleGetTask GET /api/v1/tasks/{id} —— 任务详情（含最新 run 摘要）。
func (d *Deps) handleGetTask(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "task.get")
	tenantID := subject(r).TenantID
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "task.get", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	task, ok := d.Store.GetTask(tenantID, id)
	if !ok {
		// Engine 可能提供更权威的任务视图（可选能力断言）。
		if getter, ok2 := d.Engine.(interface {
			GetTask(context.Context, string, string) (*domain.Task, error)
		}); ok2 && d.Engine != nil && !isNilInterface(d.Engine) {
			if t, err := getter.GetTask(r.Context(), tenantID, id); err == nil && t != nil {
				task, ok = t, true
			}
		}
	}
	if !ok || task == nil {
		end(d, r, reqID, "task.get", store.ErrNotFound)
		writeError(w, r, errNotFoundFn("任务不存在"))
		return
	}
	detail := taskDetail{Task: task}
	if task.LatestRunID != "" {
		if run, err := d.provider().GetRun(r.Context(), tenantID, task.LatestRunID); err == nil && run != nil {
			detail.LatestRun = run
		}
	}
	httpx.WriteJSON(w, r, detail)
	end(d, r, reqID, "task.get", nil)
}

// ---------------------------------------------------------------------------
// 运行
// ---------------------------------------------------------------------------

// handleListRuns GET /api/v1/runs —— 执行记录列表（state / keyword / 分页）。
func (d *Deps) handleListRuns(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.list")
	page, err := d.provider().ListRuns(r.Context(), subject(r).TenantID, pageQuery(r))
	if err != nil {
		end(d, r, reqID, "run.list", err)
		writeError(w, r, err)
		return
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "run.list", nil)
}

// handleGetRun GET /api/v1/runs/{runId} —— 执行详情（任务上下文全量）。
//
// 默认不返回证据切片全文（仅 path/行号/reason/chars 元信息），
// 传 ?includeCode=1 时才附带 content。
func (d *Deps) handleGetRun(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.get")
	tenantID := subject(r).TenantID
	runID := pathValue(r, "runId")
	if runID == "" {
		end(d, r, reqID, "run.get", errors.New("runId 为空"))
		writeError(w, r, errBadRequest("runId 为必填"))
		return
	}
	run, err := d.provider().GetRun(r.Context(), tenantID, runID)
	if err != nil || run == nil {
		end(d, r, reqID, "run.get", err)
		writeError(w, r, err)
		return
	}
	includeCode := queryBool(r, "includeCode")

	detail := runDetail{
		Run:           run,
		Resolution:    nonNilResolved(run.Resolution),
		PinnedCommits: run.PinnedCommits,
		Verification:  run.Verification,
		Usage:         run.Usage,
		Warnings:      nonNilStrings(run.Warnings),
		Degraded:      run.Degraded || run.State == domain.StateDegraded,
		ElapsedMS:     run.ElapsedMS(),
		IncludeCode:   includeCode,
	}
	for _, p := range run.Patches {
		pv := patchView{Patch: p}
		if !includeCode {
			pv.Patch.OldContent = ""
			pv.Patch.NewContent = ""
		}
		detail.Patches = append(detail.Patches, pv)
	}
	if detail.Patches == nil {
		detail.Patches = []patchView{}
	}

	// 技能/模型轨迹（详情内嵌前 N 条，完整轨迹走专用分页接口）。
	skillPage, _ := d.provider().ListSkillCalls(r.Context(), tenantID, runID,
		domain.PageQuery{Page: 1, PageSize: runDetailCallLimit})
	modelPage, _ := d.provider().ListModelCalls(r.Context(), tenantID, runID,
		domain.PageQuery{Page: 1, PageSize: runDetailCallLimit})
	detail.SkillCalls = nonNilSkillCalls(skillPage.Items)
	detail.ModelCalls = nonNilModelCalls(modelPage.Items)

	// 证据摘要：Store 不持久化 EvidenceBundle，故从技能调用输出中还原元信息。
	detail.Evidence = buildEvidence(skillPage.Items, includeCode)

	if run.ReportID != "" {
		if rep, ok := d.Store.GetReport(tenantID, run.ReportID); ok {
			detail.Report = rep
		}
	} else if rep, ok := d.Store.GetReportByRun(tenantID, runID); ok {
		detail.Report = rep
	}

	httpx.WriteJSON(w, r, detail)
	end(d, r, reqID, "run.get", nil)
}

// handleCancelRun POST /api/v1/runs/{runId}/cancel —— 取消任务。
func (d *Deps) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.cancel")
	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "run.cancel", errors.New("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配", nil))
		return
	}
	runID := pathValue(r, "runId")
	if runID == "" {
		end(d, r, reqID, "run.cancel", errors.New("runId 为空"))
		writeError(w, r, errBadRequest("runId 为必填"))
		return
	}
	if err := d.Engine.Cancel(r.Context(), subject(r).TenantID, runID); err != nil {
		end(d, r, reqID, "run.cancel", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "task", "cancel", "warn", "取消运行 "+runID, map[string]any{"runId": runID})
	httpx.WriteJSON(w, r, map[string]any{"runId": runID, "cancelled": true})
	end(d, r, reqID, "run.cancel", nil)
}

// handleRetryRun POST /api/v1/runs/{runId}/retry —— 基于固化上下文重跑。
func (d *Deps) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.retry")
	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "run.retry", errors.New("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配", nil))
		return
	}
	runID := pathValue(r, "runId")
	run, err := d.Engine.Retry(r.Context(), subject(r).TenantID, runID)
	if err != nil || run == nil {
		end(d, r, reqID, "run.retry", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "task", "retry", "info", "重跑运行 "+runID,
		map[string]any{"runId": runID, "newRunId": run.ID})
	httpx.WriteJSON(w, r, run)
	end(d, r, reqID, "run.retry", nil)
}

// handleRunFeedback POST /api/v1/runs/{runId}/feedback —— 携带人工反馈重跑。
func (d *Deps) handleRunFeedback(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.feedback")
	if d.Engine == nil || isNilInterface(d.Engine) {
		end(d, r, reqID, "run.feedback", errors.New("engine 未装配"))
		writeError(w, r, errUnavailable("任务调度层未装配", nil))
		return
	}
	runID := pathValue(r, "runId")
	if runID == "" {
		end(d, r, reqID, "run.feedback", errors.New("runId 为空"))
		writeError(w, r, errBadRequest("runId 为必填"))
		return
	}

	var req feedbackRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "run.feedback", err)
		writeError(w, r, err)
		return
	}
	feedback := strings.TrimSpace(req.Feedback)
	if feedback == "" {
		end(d, r, reqID, "run.feedback", errors.New("feedback 为空"))
		writeError(w, r, errBadRequest("feedback 为必填且不能为空"))
		return
	}
	if len([]rune(feedback)) > maxFeedbackRunes {
		end(d, r, reqID, "run.feedback", errors.New("feedback 超长"))
		writeError(w, r, errBadRequest(fmt.Sprintf("feedback 长度不能超过 %d 字符", maxFeedbackRunes)))
		return
	}
	run, err := d.Engine.RerunWithFeedback(r.Context(), subject(r).TenantID, runID, feedback)
	if err != nil || run == nil {
		end(d, r, reqID, "run.feedback", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "task", "feedback_rerun", "info", "人工反馈重跑 "+runID,
		map[string]any{"runId": runID, "newRunId": run.ID})
	httpx.WriteJSON(w, r, run)
	end(d, r, reqID, "run.feedback", nil)
}

// handleRunSkillCalls GET /api/v1/runs/{runId}/skill-calls —— 技能调用轨迹（分页）。
func (d *Deps) handleRunSkillCalls(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.skillCalls")
	page, err := d.provider().ListSkillCalls(r.Context(), subject(r).TenantID,
		pathValue(r, "runId"), pageQuery(r))
	if err != nil {
		end(d, r, reqID, "run.skillCalls", err)
		writeError(w, r, err)
		return
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "run.skillCalls", nil)
}

// handleRunModelCalls GET /api/v1/runs/{runId}/model-calls —— 模型推理轨迹（分页）。
func (d *Deps) handleRunModelCalls(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "run.modelCalls")
	page, err := d.provider().ListModelCalls(r.Context(), subject(r).TenantID,
		pathValue(r, "runId"), pageQuery(r))
	if err != nil {
		end(d, r, reqID, "run.modelCalls", err)
		writeError(w, r, err)
		return
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "run.modelCalls", nil)
}

// ---------------------------------------------------------------------------
// 入参清洗
// ---------------------------------------------------------------------------

// normalizeCreateTask 入参清洗：标题兜底、长度限制、priority 夹取、mode 推断、AutoVerify 默认。
func normalizeCreateTask(req *domain.CreateTaskRequest) error {
	req.RepoID = strings.TrimSpace(req.RepoID)
	req.GroupID = strings.TrimSpace(req.GroupID)
	req.Ref = strings.TrimSpace(req.Ref)
	req.Title = strings.TrimSpace(req.Title)
	req.Environment = strings.TrimSpace(req.Environment)

	if len(req.Stacktrace) > maxStacktraceBytes {
		return errBadRequest(fmt.Sprintf("stacktrace 超过 %d KB 上限", maxStacktraceBytes/1024))
	}
	if len(req.Logs) > maxLogsBytes {
		return errBadRequest(fmt.Sprintf("logs 超过 %d KB 上限", maxLogsBytes/1024))
	}
	if strings.TrimSpace(req.Stacktrace) == "" {
		return errBadRequest("stacktrace 为必填（核心线索）")
	}
	if req.RepoID == "" && req.GroupID == "" {
		return errBadRequest("repoId 与 groupId 至少提供一个")
	}

	// mode 自动推断：GroupID 优先（一次排查一条链路）。
	if req.Mode == "" {
		switch {
		case req.GroupID != "":
			req.Mode = domain.ModeGroup
		case req.RepoID != "":
			req.Mode = domain.ModeSingleRepo
		}
	}
	if req.Mode != domain.ModeSingleRepo && req.Mode != domain.ModeGroup {
		return errBadRequest("mode 非法，可选 single_repo/group")
	}
	if req.Mode == domain.ModeSingleRepo && req.RepoID == "" {
		return errBadRequest("single_repo 模式必须提供 repoId")
	}
	if req.Mode == domain.ModeGroup && req.GroupID == "" {
		return errBadRequest("group 模式必须提供 groupId")
	}

	// title 缺省：取堆栈首个非空行截断生成。
	if req.Title == "" {
		req.Title = deriveTitle(req.Stacktrace)
	}
	if len([]rune(req.Title)) > maxTitleRunes {
		req.Title = string([]rune(req.Title)[:maxTitleRunes])
	}

	// priority 夹取到 0-9。
	if req.Priority < 0 {
		req.Priority = 0
	}
	if req.Priority > 9 {
		req.Priority = 9
	}

	// Source 缺省为 openapi（接入层调用）。
	if req.Source == "" {
		req.Source = domain.TriggerOpenAPI
	}
	if !validTrigger(req.Source) {
		return errBadRequest("source 非法，可选 alert/ide/cicd/manual/openapi")
	}

	// AutoVerify 默认 true。
	if req.AutoVerify == nil {
		enabled := true
		req.AutoVerify = &enabled
	}
	if req.EntryFiles != nil {
		files := make([]string, 0, len(req.EntryFiles))
		for _, f := range req.EntryFiles {
			if v := strings.TrimSpace(f); v != "" {
				files = append(files, v)
			}
		}
		req.EntryFiles = files
	}
	return nil
}

// deriveTitle 从堆栈文本首行生成任务标题。
func deriveTitle(stacktrace string) string {
	for _, line := range strings.Split(stacktrace, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len([]rune(line)) > maxTitleRunes {
			line = string([]rune(line)[:maxTitleRunes])
		}
		return line
	}
	return "未命名任务"
}

func validTrigger(s domain.TriggerSource) bool {
	switch s {
	case domain.TriggerAlert, domain.TriggerIDE, domain.TriggerCICD, domain.TriggerManual, domain.TriggerOpenAPI:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 证据还原与视图工具
// ---------------------------------------------------------------------------

// buildEvidence 从技能调用输出中还原证据摘要。
//
// 依据契约 §4：`code_ast_analyze` 输出 slices、`error_localize` 输出 hits、
// `cross_repo_trace` 输出 edges/chain。这里只提取元信息，includeCode=true 时才带 content。
func buildEvidence(calls []domain.SkillCall, includeCode bool) evidenceView {
	view := evidenceView{Slices: []evidenceSlice{}, Source: "skill_calls"}
	seen := map[string]bool{}
	for _, c := range calls {
		out, ok := c.Output.(map[string]any)
		if !ok || len(out) == 0 {
			continue
		}
		view.Available = true
		if raw, ok := out["slices"]; ok {
			for _, s := range toMapList(raw) {
				es := evidenceSlice{
					RepositoryID: strField(s, "repositoryId"),
					RepoKey:      strField(s, "repoKey"),
					Commit:       strField(s, "commit"),
					Path:         strField(s, "path"),
					Language:     strField(s, "language"),
					Reason:       strField(s, "reason"),
					StartLine:    intField(s, "startLine"),
					EndLine:      intField(s, "endLine"),
					Chars:        intField(s, "chars"),
					FocusLines:   intListField(s, "focusLines"),
					Truncated:    boolField(s, "truncated"),
					FromCache:    boolField(s, "fromCache"),
					Redacted:     boolField(s, "redacted"),
				}
				if es.Path == "" {
					continue
				}
				key := es.RepositoryID + "|" + es.Path
				if seen[key] {
					continue
				}
				seen[key] = true
				if includeCode {
					// 仅显式请求时才回传源码全文（避免响应过大）。
					es.Content = strField(s, "content")
					if es.Chars == 0 {
						es.Chars = len(es.Content)
					}
				}
				view.FileCount++
				view.CodeChars += es.Chars
				view.Slices = append(view.Slices, es)
			}
		}
		if raw, ok := out["candidateRepos"]; ok {
			for _, cd := range toMapList(raw) {
				view.CandidateRepos = append(view.CandidateRepos, candidateView{
					RepositoryID: strField(cd, "repositoryId"),
					RepoKey:      strField(cd, "repoKey"),
					Name:         strField(cd, "name"),
					Layer:        strField(cd, "layer"),
					Score:        floatField(cd, "score"),
					MatchedBy:    strListField(cd, "matchedBy"),
					HintCount:    len(toMapList(cd["hints"])),
				})
			}
		}
		view.Notes = append(view.Notes, strListField(out, "notes")...)
		if b, ok := out["truncated"].(bool); ok && b {
			view.Truncated = true
		}
		if n, ok := out["filesLoaded"]; ok {
			view.FilesLoaded = intField(map[string]any{"v": n}, "v")
		}
	}
	if view.FilesLoaded == 0 {
		view.FilesLoaded = view.FileCount
	}
	return view
}

func toMapList(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

func floatField(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func intListField(m map[string]any, key string) []int {
	list, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(list))
	for _, item := range list {
		if f, ok := item.(float64); ok {
			out = append(out, int(f))
		}
	}
	return out
}

func strListField(m map[string]any, key string) []string {
	if ss, ok := m[key].([]string); ok {
		return ss
	}
	list, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// queryBool 解析 ?flag=1 / =true / =yes / =on。
func queryBool(r *http.Request, key string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func nonNilResolved(in []domain.RepoResolved) []domain.RepoResolved {
	if in == nil {
		return []domain.RepoResolved{}
	}
	return in
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilSkillCalls(in []domain.SkillCall) []domain.SkillCall {
	if in == nil {
		return []domain.SkillCall{}
	}
	return in
}

func nonNilModelCalls(in []domain.ModelCall) []domain.ModelCall {
	if in == nil {
		return []domain.ModelCall{}
	}
	return in
}
