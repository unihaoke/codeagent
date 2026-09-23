package handler

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
	"github.com/codeagent/backend/internal/store"
)

// groupMemberInput 分组成员入参。
type groupMemberInput struct {
	RepositoryID string `json:"repositoryId"`
	Order        int    `json:"order,omitempty"`
	Note         string `json:"note,omitempty"`
}

// groupPayload 分组创建/更新请求体。
//
// 同时兼容两种写法：
//  1. 嵌套写法：`{"group": {...}, "members": [{"repositoryId":"r1","order":0}]}`
//  2. 扁平写法：把 domain.RepositoryGroup 的字段直接放在顶层 + `members`
//
// 解析优先级：嵌套的 group 非空时以嵌套为准，未提供的字段回落顶层字段。
type groupPayload struct {
	Group *domain.RepositoryGroup `json:"group,omitempty"`
	// Members 为指针以区分"未提供"（保留原成员）与"显式清空"（传 []）。
	Members *[]groupMemberInput `json:"members,omitempty"`

	// 扁平写法字段
	ID                 string   `json:"id,omitempty"`
	Name               string   `json:"name,omitempty"`
	Key                string   `json:"key,omitempty"`
	Description        string   `json:"description,omitempty"`
	Status             string   `json:"status,omitempty"`
	EntryRepositoryIDs []string `json:"entryRepositoryIds,omitempty"`
	TenantID           string   `json:"tenantId,omitempty"`
}

// groupDetail 分组详情：分组元数据 + 成员聚合视图。
type groupDetail struct {
	Group   *domain.RepositoryGroup  `json:"group"`
	Members []domain.GroupMemberView `json:"members"`
	// Summary 分组摘要统计。
	Summary groupSummary `json:"summary"`
}

// groupSummary 分组摘要。
type groupSummary struct {
	MemberCount int            `json:"memberCount"`
	ByLayer     map[string]int `json:"byLayer"`
	ByLanguage  map[string]int `json:"byLanguage"`
}

// impactAnalysis 影响面分析结果。
type impactAnalysis struct {
	RepoCount  int            `json:"repoCount"`
	ByLayer    map[string]int `json:"byLayer"`
	ByLanguage map[string]int `json:"byLanguage"`
	EntryRepos []string       `json:"entryRepos"`
	SharedLibs []string       `json:"sharedLibs"`
	RiskScore  float64        `json:"riskScore"`
	// RiskFormula 风险分计算说明（保证前端可解释）。
	RiskFormula string   `json:"riskFormula"`
	Notes       []string `json:"notes,omitempty"`
}

// handleListGroups GET /api/v1/groups —— 分组列表（分页）。
func (d *Deps) handleListGroups(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.list")
	sub := subject(r)
	q := pageQuery(r)

	all := d.Store.ListGroups(sub.TenantID)
	views := make([]map[string]any, 0, len(all))
	for _, g := range all {
		if q.State != "" && string(g.Status) != q.State {
			continue
		}
		if q.Keyword != "" && !containsFold(g.Name, q.Keyword) && !containsFold(g.Key, q.Keyword) &&
			!containsFold(g.Description, q.Keyword) {
			continue
		}
		members := d.Store.GroupMemberViews(sub.TenantID, g.ID)
		views = append(views, map[string]any{
			"group":   g,
			"summary": summarizeMembers(members, &g),
		})
	}
	httpx.WriteJSON(w, r, map[string]any{
		"items":    views,
		"total":    len(views),
		"page":     q.Page,
		"pageSize": q.PageSize,
	})
	end(d, r, reqID, "group.list", nil)
}

// handleCreateGroup POST /api/v1/groups —— 创建分组与成员。
func (d *Deps) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.create")

	var payload groupPayload
	if err := decodeBody(d, r, &payload); err != nil {
		end(d, r, reqID, "group.create", err)
		writeError(w, r, err)
		return
	}
	sub := subject(r)
	group, members, err := d.buildGroup(payload, nil, sub.TenantID, true)
	if err != nil {
		end(d, r, reqID, "group.create", err)
		writeError(w, r, err)
		return
	}
	if err := d.Repos.CreateGroup(r.Context(), group, members); err != nil {
		end(d, r, reqID, "group.create", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "group", "create", "info", "创建分组 "+group.Key, map[string]any{"groupId": group.ID})
	w.WriteHeader(http.StatusCreated)
	httpx.WriteJSON(w, r, d.groupDetail(sub.TenantID, group))
	end(d, r, reqID, "group.create", nil)
}

// handleGetGroup GET /api/v1/groups/{id} —— 分组详情（成员聚合视图）。
func (d *Deps) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.get")
	sub := subject(r)
	id := pathValue(r, "id")
	if !d.groupVisible(r, sub, id) {
		end(d, r, reqID, "group.get", errors.New("无权访问该分组"))
		writeError(w, r, errForbidden("无权访问该分组"))
		return
	}
	group, err := d.Repos.GetGroup(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "group.get", err)
		writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, d.groupDetail(sub.TenantID, group))
	end(d, r, reqID, "group.get", nil)
}

// handleUpdateGroup PUT /api/v1/groups/{id} —— 更新分组与成员。
func (d *Deps) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.update")
	sub := subject(r)
	id := pathValue(r, "id")

	existing, err := d.Repos.GetGroup(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "group.update", err)
		writeError(w, r, err)
		return
	}
	var payload groupPayload
	if err := decodeBody(d, r, &payload); err != nil {
		end(d, r, reqID, "group.update", err)
		writeError(w, r, err)
		return
	}
	group, members, err := d.buildGroup(payload, existing, sub.TenantID, false)
	if err != nil {
		end(d, r, reqID, "group.update", err)
		writeError(w, r, err)
		return
	}
	// 请求未提供 members 时保持原成员不变（store.UpdateGroup 以 nil 表示不修改）。
	var memberArg []domain.GroupMember
	if payload.Members != nil {
		memberArg = members
	}
	if err := d.Repos.UpdateGroup(r.Context(), group, memberArg); err != nil {
		end(d, r, reqID, "group.update", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "group", "update", "info", "更新分组 "+group.Key, map[string]any{"groupId": group.ID})
	httpx.WriteJSON(w, r, d.groupDetail(sub.TenantID, group))
	end(d, r, reqID, "group.update", nil)
}

// handleDeleteGroup DELETE /api/v1/groups/{id} —— 删除分组与成员关系。
func (d *Deps) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.delete")
	sub := subject(r)
	id := pathValue(r, "id")
	if err := d.Repos.DeleteGroup(r.Context(), sub.TenantID, id); err != nil {
		end(d, r, reqID, "group.delete", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "group", "delete", "warn", "删除分组 "+id, map[string]any{"groupId": id})
	httpx.WriteJSON(w, r, map[string]any{"id": id, "deleted": true})
	end(d, r, reqID, "group.delete", nil)
}

// handleGroupImpact GET /api/v1/groups/{id}/impact —— 分组影响面分析。
//
// 返回成员仓库数、分层/语言分布、链路入口仓库、共享库候选与风险分。
// riskScore 计算说明见响应中的 riskFormula 字段：
//
//	riskScore = 成员数权重(×1.0) + 跨层数权重(×1.5) + 入口仓库数(×0.5) + 共享库数(×0.8)
//	（结果四舍五入保留两位小数，越大代表改动影响面越广）
func (d *Deps) handleGroupImpact(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "group.impact")
	sub := subject(r)
	id := pathValue(r, "id")

	group, err := d.Repos.GetGroup(r.Context(), sub.TenantID, id)
	if err != nil {
		end(d, r, reqID, "group.impact", err)
		writeError(w, r, err)
		return
	}
	members := d.Store.GroupMemberViews(sub.TenantID, id)

	byLayer := map[string]int{}
	byLanguage := map[string]int{}
	sharedLibs := []string{}
	for _, m := range members {
		if m.Repo == nil {
			continue
		}
		byLayer[string(m.Repo.Layer)]++
		byLanguage[orUnknown(m.Repo.Language)]++
		if m.Repo.Layer == domain.LayerLibrary || m.Repo.Layer == domain.LayerMiddleware {
			sharedLibs = append(sharedLibs, m.Repo.Key)
		}
	}
	sort.Strings(sharedLibs)

	entryRepos := append([]string{}, group.EntryRepositoryIDs...)
	if entryRepos == nil {
		entryRepos = []string{}
	}

	repoCount := len(members)
	layerCount := len(byLayer)
	risk := float64(repoCount)*1.0 + float64(layerCount)*1.5 +
		float64(len(entryRepos))*0.5 + float64(len(sharedLibs))*0.8
	risk = float64(int64(risk*100+0.5)) / 100

	result := impactAnalysis{
		RepoCount:   repoCount,
		ByLayer:     byLayer,
		ByLanguage:  byLanguage,
		EntryRepos:  entryRepos,
		SharedLibs:  sharedLibs,
		RiskScore:   risk,
		RiskFormula: "riskScore = repoCount×1.0 + layerCount×1.5 + entryRepos×0.5 + sharedLibs×0.8（保留两位小数）",
	}
	if repoCount == 0 {
		result.Notes = append(result.Notes, "分组暂无成员仓库，影响面为 0")
	}
	if len(entryRepos) == 0 {
		result.Notes = append(result.Notes, "未配置链路入口仓库（entryRepositoryIds），跨仓库链路推断将从全部成员开始")
	}
	httpx.WriteJSON(w, r, result)
	end(d, r, reqID, "group.impact", nil)
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// buildGroup 归一化分组请求体（兼容嵌套/扁平两种写法）。
func (d *Deps) buildGroup(payload groupPayload, existing *domain.RepositoryGroup, tenantID string, creating bool) (*domain.RepositoryGroup, []domain.GroupMember, error) {
	now := time.Now()
	group := &domain.RepositoryGroup{TenantID: tenantID}
	if existing != nil {
		cp := *existing
		group = &cp
	} else {
		group.ID = httpx.NewResourceID("grp")
		group.Status = domain.GroupActive
		group.CreatedAt = now
	}

	// 1) 嵌套写法优先。
	if payload.Group != nil {
		src := payload.Group
		if v := strings.TrimSpace(src.Name); v != "" {
			group.Name = v
		}
		if v := strings.TrimSpace(src.Key); v != "" {
			group.Key = v
		}
		if src.Description != "" {
			group.Description = src.Description
		}
		if src.Status != "" {
			group.Status = src.Status
		}
		if src.EntryRepositoryIDs != nil {
			group.EntryRepositoryIDs = src.EntryRepositoryIDs
		}
	}
	// 2) 扁平写法（未在嵌套中提供的字段生效）。
	if v := strings.TrimSpace(payload.Name); v != "" {
		group.Name = v
	}
	if v := strings.TrimSpace(payload.Key); v != "" {
		group.Key = v
	}
	if payload.Description != "" {
		group.Description = payload.Description
	}
	if payload.Status != "" {
		group.Status = domain.GroupStatus(payload.Status)
	}
	if payload.EntryRepositoryIDs != nil {
		group.EntryRepositoryIDs = payload.EntryRepositoryIDs
	}

	if creating && group.Key == "" {
		group.Key = group.Name
	}
	if strings.TrimSpace(group.Name) == "" && creating {
		return nil, nil, errBadRequest("name 为必填")
	}
	if group.Status != domain.GroupActive && group.Status != domain.GroupDisabled {
		return nil, nil, errBadRequest("status 非法，可选 active/disabled")
	}
	group.UpdatedAt = now

	// 3) 成员解析：校验仓库存在且属于当前租户（租户隔离验收项）。
	var members []domain.GroupMember
	if payload.Members != nil {
		seen := map[string]bool{}
		for i, m := range *payload.Members {
			repoID := strings.TrimSpace(m.RepositoryID)
			if repoID == "" {
				return nil, nil, errBadRequest("members[].repositoryId 为必填")
			}
			if seen[repoID] {
				return nil, nil, errConflictFn("members 中存在重复仓库: " + repoID)
			}
			seen[repoID] = true
			if _, ok := d.Store.GetRepo(tenantID, repoID); !ok {
				return nil, nil, store.ErrNotFound
			}
			order := m.Order
			if order == 0 {
				order = i
			}
			members = append(members, domain.GroupMember{
				GroupID:      group.ID,
				RepositoryID: repoID,
				Order:        order,
				Note:         m.Note,
				CreatedAt:    now,
			})
		}
		if members == nil {
			members = []domain.GroupMember{} // 显式清空
		}
	}
	return group, members, nil
}

// groupDetail 组装分组详情（成员聚合视图 + 摘要）。
func (d *Deps) groupDetail(tenantID string, group *domain.RepositoryGroup) groupDetail {
	members := d.Store.GroupMemberViews(tenantID, group.ID)
	if members == nil {
		members = []domain.GroupMemberView{}
	}
	return groupDetail{Group: group, Members: members, Summary: summarizeMembers(members, group)}
}

// summarizeMembers 统计成员的分层/语言分布。
func summarizeMembers(members []domain.GroupMemberView, group *domain.RepositoryGroup) groupSummary {
	sum := groupSummary{MemberCount: len(members), ByLayer: map[string]int{}, ByLanguage: map[string]int{}}
	for _, m := range members {
		if m.Repo == nil {
			sum.ByLayer[string(domain.LayerUnknown)]++
			sum.ByLanguage["unknown"]++
			continue
		}
		sum.ByLayer[string(m.Repo.Layer)]++
		sum.ByLanguage[orUnknown(m.Repo.Language)]++
	}
	if group != nil {
		sum.MemberCount = len(members)
	}
	return sum
}

// groupVisible 校验主体是否有权访问该分组。
func (d *Deps) groupVisible(r *http.Request, sub *domain.Subject, groupID string) bool {
	if groupID == "" {
		return false
	}
	if sub.Has(scopeAdminAll) {
		return true
	}
	if d.Auth == nil || isNilInterface(d.Auth) {
		return true
	}
	return d.Auth.CanAccessGroup(r.Context(), sub, groupID) == nil
}
