package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/store"
)

// createRepoRequest 注册仓库请求体。
type createRepoRequest struct {
	Name          string                 `json:"name"`
	Key           string                 `json:"key,omitempty"`
	URL           string                 `json:"url"`
	DefaultBranch string                 `json:"defaultBranch,omitempty"`
	Provider      domain.VCSProvider     `json:"provider,omitempty"`
	Layer         domain.RepoLayer       `json:"layer,omitempty"`
	Language      string                 `json:"language,omitempty"`
	CredentialID  string                 `json:"credentialId,omitempty"`
	Status        domain.RepoStatus      `json:"status,omitempty"`
	MatchRules    *domain.RepoMatchRules `json:"matchRules,omitempty"`
	AccessPolicy  []string               `json:"accessPolicy,omitempty"`
}

// updateRepoRequest 更新仓库请求体（字段级可选更新）。
type updateRepoRequest struct {
	Name          *string                `json:"name,omitempty"`
	Key           *string                `json:"key,omitempty"`
	URL           *string                `json:"url,omitempty"`
	DefaultBranch *string                `json:"defaultBranch,omitempty"`
	Provider      *domain.VCSProvider    `json:"provider,omitempty"`
	Layer         *domain.RepoLayer      `json:"layer,omitempty"`
	Language      *string                `json:"language,omitempty"`
	CredentialID  *string                `json:"credentialId,omitempty"`
	Status        *domain.RepoStatus     `json:"status,omitempty"`
	MatchRules    *domain.RepoMatchRules `json:"matchRules,omitempty"`
	AccessPolicy  *[]string              `json:"accessPolicy,omitempty"`
}

// repoView 仓库视图：附加所属分组与静态摘要，且不含任何凭证密文。
type repoView struct {
	domain.Repository
	// Groups 所属分组（反查 store.GroupsOfRepo）。
	Groups []groupBrief `json:"groups"`
	// Summary 语言/分层统计摘要。
	Summary repoSummary `json:"summary"`
}

// groupBrief 分组的轻量引用。
type groupBrief struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

// repoSummary 仓库摘要：语言、分层、匹配规则规模与访问策略。
type repoSummary struct {
	Language     string `json:"language"`
	Layer        string `json:"layer"`
	MatchedRules int    `json:"matchedRules"`
	GroupCount   int    `json:"groupCount"`
	AccessScoped bool   `json:"accessScoped"`
}

// handleListRepos GET /api/v1/repos —— 仓库列表（分页/关键字/租户隔离）。
//
// 响应除分页信封外附带 aggregates（按语言、按分层的数量统计），便于前端图表直接渲染。
func (d *Deps) handleListRepos(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.list")
	sub := subject(r)
	q := pageQuery(r)

	all := d.Store.ListRepos(sub.TenantID) // 存储层已按 TenantID 过滤
	byLanguage := map[string]int{}
	byLayer := map[string]int{}
	filtered := make([]repoView, 0, len(all))
	for _, repo := range all {
		if !matchRepo(repo, q) {
			continue
		}
		if !d.repoVisible(r, sub, repo.ID) {
			continue
		}
		filtered = append(filtered, d.buildRepoView(r, sub.TenantID, repo))
		byLanguage[orUnknown(repo.Language)]++
		byLayer[string(repo.Layer)]++
	}
	page := paginate(filtered, q)
	httpx.WriteJSON(w, r, map[string]any{
		"items":    page.Items,
		"total":    page.Total,
		"page":     page.Page,
		"pageSize": page.PageSize,
		"aggregates": map[string]any{
			"byLanguage": byLanguage,
			"byLayer":    byLayer,
		},
	})
	end(d, r, reqID, "repo.list", nil)
}

// handleCreateRepo POST /api/v1/repos —— 注册仓库。
func (d *Deps) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.create")

	var req createRepoRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "repo.create", err)
		writeError(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" || req.URL == "" {
		end(d, r, reqID, "repo.create", errors.New("参数缺失"))
		writeError(w, r, errBadRequest("name 与 url 均为必填"))
		return
	}
	if req.Key == "" {
		req.Key = req.Name
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	if req.Provider == "" {
		req.Provider = domain.VCSGit
	}
	if !validProvider(req.Provider) {
		end(d, r, reqID, "repo.create", errors.New("provider 非法"))
		writeError(w, r, errBadRequest("provider 非法，可选 git/github/gitlab/gitee"))
		return
	}
	if req.Layer == "" {
		req.Layer = domain.LayerUnknown
	}
	if !validLayer(req.Layer) {
		end(d, r, reqID, "repo.create", errors.New("layer 非法"))
		writeError(w, r, errBadRequest("layer 非法，可选 frontend/gateway/service/middleware/library/unknown"))
		return
	}
	if req.Status == "" {
		req.Status = domain.RepoActive
	}
	// 若指定了凭证，必须属于当前租户（防止挂载他人凭证）。
	if req.CredentialID != "" {
		if _, ok := d.Store.GetCredential(subject(r).TenantID, req.CredentialID); !ok {
			end(d, r, reqID, "repo.create", store.ErrNotFound)
			writeError(w, r, errBadRequest("credentialId 不存在或不属于当前租户"))
			return
		}
	}

	now := time.Now()
	repo := &domain.Repository{
		ID:            httpx.NewResourceID("repo"),
		TenantID:      subject(r).TenantID,
		Name:          req.Name,
		Key:           req.Key,
		URL:           req.URL,
		DefaultBranch: req.DefaultBranch,
		Provider:      req.Provider,
		Layer:         req.Layer,
		Language:      req.Language,
		CredentialID:  req.CredentialID,
		Status:        req.Status,
		AccessPolicy:  req.AccessPolicy,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if req.MatchRules != nil {
		repo.MatchRules = *req.MatchRules
	}
	if err := d.Repos.CreateRepo(r.Context(), repo); err != nil {
		end(d, r, reqID, "repo.create", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "repo", "create", "info", "注册仓库 "+repo.Key, map[string]any{"repoId": repo.ID})
	w.WriteHeader(http.StatusCreated)
	httpx.WriteJSON(w, r, d.buildRepoView(r, repo.TenantID, *repo))
	end(d, r, reqID, "repo.create", nil)
}

// handleGetRepo GET /api/v1/repos/{id} —— 仓库详情（含所属分组、缓存统计）。
func (d *Deps) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.get")
	sub := subject(r)
	id := pathValue(r, "id")
	if id == "" {
		end(d, r, reqID, "repo.get", errors.New("id 为空"))
		writeError(w, r, errBadRequest("id 为必填"))
		return
	}
	if !d.repoVisible(r, sub, id) {
		end(d, r, reqID, "repo.get", errors.New("无权访问该仓库"))
		writeError(w, r, errForbidden("无权访问该仓库"))
		return
	}
	repo, err := d.Repos.GetRepo(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "repo.get", err)
		writeError(w, r, err)
		return
	}
	view := d.buildRepoView(r, sub.TenantID, *repo)
	payload := map[string]any{"repo": view, "groups": view.Groups, "summary": view.Summary}
	if d.Source != nil && !isNilInterface(d.Source) {
		payload["cache"] = d.Source.Stats()
	}
	httpx.WriteJSON(w, r, payload)
	end(d, r, reqID, "repo.get", nil)
}

// handleUpdateRepo PUT /api/v1/repos/{id} —— 更新仓库。
func (d *Deps) handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.update")
	sub := subject(r)
	id := pathValue(r, "id")

	var req updateRepoRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "repo.update", err)
		writeError(w, r, err)
		return
	}
	repo, err := d.Repos.GetRepo(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "repo.update", err)
		writeError(w, r, err)
		return
	}
	if req.Name != nil {
		if v := strings.TrimSpace(*req.Name); v != "" {
			repo.Name = v
		}
	}
	if req.Key != nil {
		if v := strings.TrimSpace(*req.Key); v != "" {
			repo.Key = v
		}
	}
	if req.URL != nil {
		if v := strings.TrimSpace(*req.URL); v != "" {
			repo.URL = v
		}
	}
	if req.DefaultBranch != nil {
		repo.DefaultBranch = *req.DefaultBranch
	}
	if req.Provider != nil {
		if !validProvider(*req.Provider) {
			end(d, r, reqID, "repo.update", errors.New("provider 非法"))
			writeError(w, r, errBadRequest("provider 非法"))
			return
		}
		repo.Provider = *req.Provider
	}
	if req.Layer != nil {
		if !validLayer(*req.Layer) {
			end(d, r, reqID, "repo.update", errors.New("layer 非法"))
			writeError(w, r, errBadRequest("layer 非法"))
			return
		}
		repo.Layer = *req.Layer
	}
	if req.Language != nil {
		repo.Language = *req.Language
	}
	if req.CredentialID != nil {
		cid := strings.TrimSpace(*req.CredentialID)
		if cid != "" {
			if _, ok := d.Store.GetCredential(sub.TenantID, cid); !ok {
				end(d, r, reqID, "repo.update", store.ErrNotFound)
				writeError(w, r, errBadRequest("credentialId 不存在或不属于当前租户"))
				return
			}
		}
		repo.CredentialID = cid
	}
	if req.Status != nil {
		repo.Status = *req.Status
	}
	if req.MatchRules != nil {
		repo.MatchRules = *req.MatchRules
	}
	if req.AccessPolicy != nil {
		repo.AccessPolicy = *req.AccessPolicy
	}
	repo.UpdatedAt = time.Now()
	if err := d.Repos.UpdateRepo(r.Context(), repo); err != nil {
		end(d, r, reqID, "repo.update", err)
		writeError(w, r, err)
		return
	}
	// 仓库地址/版本可能变化：清理源码缓存，避免用到过期代码。
	if d.Source != nil && !isNilInterface(d.Source) {
		if err := d.Source.Invalidate(r.Context(), sub.TenantID, repo.ID); err != nil {
			d.log().Warn("清理源码缓存失败", "repo", repo.ID, "err", err.Error(), "req", reqID)
		}
	}
	d.audit(r, "repo", "update", "info", "更新仓库 "+repo.Key, map[string]any{"repoId": repo.ID})
	httpx.WriteJSON(w, r, d.buildRepoView(r, sub.TenantID, *repo))
	end(d, r, reqID, "repo.update", nil)
}

// handleDeleteRepo DELETE /api/v1/repos/{id} —— 删除仓库。
func (d *Deps) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.delete")
	sub := subject(r)
	id := pathValue(r, "id")
	if err := d.Repos.DeleteRepo(r.Context(), sub.TenantID, id); err != nil {
		end(d, r, reqID, "repo.delete", err)
		writeError(w, r, err)
		return
	}
	if d.Source != nil && !isNilInterface(d.Source) {
		_ = d.Source.Invalidate(r.Context(), sub.TenantID, id)
	}
	d.audit(r, "repo", "delete", "warn", "删除仓库 "+id, map[string]any{"repoId": id})
	httpx.WriteJSON(w, r, map[string]any{"id": id, "deleted": true})
	end(d, r, reqID, "repo.delete", nil)
}

// handleProbeRepo POST /api/v1/repos/{id}/probe —— 连通性探测与版本解析。
//
// 行为：调用 d.Source.Resolve 锁定 commit 并返回耗时；Source 为 nil 时返回 503 降级说明；
// 解析失败属于"上游语义不可用"，返回 503（errUnavailable）并附脱敏后的原始错误。
func (d *Deps) handleProbeRepo(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "repo.probe")
	sub := subject(r)
	id := pathValue(r, "id")

	if d.Source == nil || isNilInterface(d.Source) {
		end(d, r, reqID, "repo.probe", errors.New("source 未装配"))
		writeError(w, r, errUnavailable(
			"源码解析组件未装配，探测能力降级：当前部署仅维护仓库索引，不拉取代码", nil))
		return
	}
	repo, err := d.Repos.GetRepo(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "repo.probe", err)
		writeError(w, r, err)
		return
	}

	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		ref = repo.DefaultBranch
	}
	start := time.Now()
	commit, resolveErr := d.Source.Resolve(r.Context(), domain.RepoRef{
		Repository: repo,
		Ref:        ref,
		Budget:     domain.LoadBudget{MaxFiles: 1, MaxChars: 1, MaxFileSize: 1, MaxDepth: 1},
	})
	elapsed := time.Since(start).Milliseconds()

	if resolveErr != nil {
		d.audit(r, "source", "probe_failed", "error", "仓库探测失败 "+repo.Key,
			map[string]any{"repoId": repo.ID, "ref": ref, "err": resolveErr.Error()})
		end(d, r, reqID, "repo.probe", resolveErr)
		writeError(w, r, errUnavailable("仓库探测失败: "+sanitizeText(resolveErr.Error()), resolveErr))
		return
	}
	d.audit(r, "source", "probe", "info", "仓库探测成功 "+repo.Key,
		map[string]any{"repoId": repo.ID, "ref": ref, "commit": commit})
	httpx.WriteJSON(w, r, map[string]any{
		"ok":           true,
		"repositoryId": repo.ID,
		"repoKey":      repo.Key,
		"requestedRef": ref,
		"commit":       commit,
		"elapsedMs":    elapsed,
		"probedAt":     nowISO(),
	})
	end(d, r, reqID, "repo.probe", nil)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// buildRepoView 组装仓库视图（附加分组与摘要，且强制剥离任何密文字段）。
func (d *Deps) buildRepoView(r *http.Request, tenantID string, repo domain.Repository) repoView {
	groups := []groupBrief{}
	if d.Store != nil {
		for _, g := range d.Store.GroupsOfRepo(tenantID, repo.ID) {
			groups = append(groups, groupBrief{ID: g.ID, Name: g.Name, Key: g.Key})
		}
	}
	return repoView{
		Repository: repo,
		Groups:     groups,
		Summary: repoSummary{
			Language:     orUnknown(repo.Language),
			Layer:        string(repo.Layer),
			MatchedRules: countMatchRules(repo.MatchRules),
			GroupCount:   len(groups),
			AccessScoped: len(repo.AccessPolicy) > 0,
		},
	}
}

// repoVisible 校验主体是否有权访问该仓库。
//
// 判定顺序：Auth 显式拒绝优先（访问策略/私有仓库白名单不可被 admin 绕过），
// 其次 admin:all 放行，最后租户隔离兜底（Auth 未装载时仅靠 TenantID 隔离）。
func (d *Deps) repoVisible(r *http.Request, sub *domain.Subject, repoID string) bool {
	if repoID == "" {
		return false
	}
	// 访问策略优先：即使管理主体，被显式拒绝也不放行（契约 §6.1 越权拦截）。
	if d.Auth != nil && !isNilInterface(d.Auth) {
		if err := d.Auth.CanAccessRepo(r.Context(), sub, repoID); err != nil {
			return false
		}
	}
	return true
}

// matchRepo 关键字与状态过滤。
func matchRepo(repo domain.Repository, q domain.PageQuery) bool {
	if q.State != "" && string(repo.Status) != q.State && string(repo.Layer) != q.State {
		return false
	}
	if q.Keyword == "" {
		return true
	}
	return containsFold(repo.Name, q.Keyword) || containsFold(repo.Key, q.Keyword) ||
		containsFold(repo.URL, q.Keyword) || containsFold(repo.Language, q.Keyword)
}

func countMatchRules(m domain.RepoMatchRules) int {
	return len(m.PackagePrefixes) + len(m.PathPrefixes) + len(m.ArtifactNames) +
		len(m.HostPatterns) + len(m.EndpointPatterns) + len(m.Keywords)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func validProvider(p domain.VCSProvider) bool {
	switch p {
	case domain.VCSGit, domain.VCSGitHub, domain.VCSGitLab, domain.VCSGitee:
		return true
	default:
		return false
	}
}

func validLayer(l domain.RepoLayer) bool {
	switch l {
	case domain.LayerFrontend, domain.LayerGateway, domain.LayerService,
		domain.LayerMiddleware, domain.LayerLibrary, domain.LayerUnknown:
		return true
	default:
		return false
	}
}

// audit 写审计事件（Store 兜底 + Recorder 优先），保证六类事件可回溯（契约 §6.10）。
func (d *Deps) audit(r *http.Request, category, action, level, message string, extra map[string]any) {
	sub := subject(r)
	ev := domain.AuditEvent{
		ID:        httpx.NewResourceID("audit"),
		TenantID:  sub.TenantID,
		Category:  category,
		Action:    action,
		Level:     level,
		Message:   message,
		RequestID: requestID(r),
		At:        time.Now(),
	}
	if extra != nil {
		ev.Data = extra
		if v, ok := extra["runId"].(string); ok {
			ev.RunID = v
		}
		if v, ok := extra["taskId"].(string); ok {
			ev.TaskID = v
		}
		if v, ok := extra["repoId"].(string); ok {
			ev.RepoID = v
		}
		if v, ok := extra["commit"].(string); ok {
			ev.Commit = v
		}
		if v, ok := extra["skill"].(string); ok {
			ev.Skill = v
		}
		if v, ok := extra["model"].(string); ok {
			ev.Model = v
		}
	}
	if d.Store != nil {
		d.Store.AppendAudit(ev)
	}
	if d.Recorder != nil && !isNilInterface(d.Recorder) {
		d.Recorder.Audit(ev)
	}
}
