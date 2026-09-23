package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// skillView 技能视图：清单 + 健康度合并。
type skillView struct {
	Manifest domain.SkillManifest `json:"manifest"`
	Health   *domain.SkillHealth  `json:"health,omitempty"`
}

// skillStatusRequest 技能启停/灰度请求体。
type skillStatusRequest struct {
	// Version 目标版本，为空表示作用于最新启用版本。
	Version string `json:"version,omitempty"`
	// Status 目标状态：enabled / disabled / canary / deprecated。
	Status string `json:"status"`
	// CanaryPercent 灰度百分比 0-100（status=canary 时生效）。
	CanaryPercent int `json:"canaryPercent,omitempty"`
}

// rollbackResponse 回滚结果。
type rollbackResponse struct {
	Skill   string `json:"skill"`
	Version string `json:"version"`
	Message string `json:"message"`
}

// handleListSkills GET /api/v1/skills —— 技能清单（含健康度），支持 ?category= 过滤。
//
// 响应 data：`{items:[{manifest, health}], total, category, enabled}`。
// 注意 data 为对象而非裸数组：这样既满足分页/过滤语义，也便于前端渲染元信息。
func (d *Deps) handleListSkills(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "skill.list")
	if d.Skills == nil || isNilInterface(d.Skills) {
		end(d, r, reqID, "skill.list", errors.New("skills 未装配"))
		httpx.WriteJSON(w, r, map[string]any{
			"items": []skillView{}, "total": 0, "category": "", "enabled": false,
		})
		return
	}

	category := strings.TrimSpace(r.URL.Query().Get("category"))
	manifests := d.Skills.List(r.Context(), category)
	if manifests == nil {
		manifests = []domain.SkillManifest{}
	}
	// 健康度合并（按技能名索引）。
	healthBySkill := map[string]domain.SkillHealth{}
	for _, h := range d.Skills.Health(r.Context()) {
		healthBySkill[h.Skill] = h
	}
	// 只保留本次返回的技能健康度，避免索引残留导致张冠李戴。
	items := make([]skillView, 0, len(manifests))
	for _, m := range manifests {
		view := skillView{Manifest: m}
		if h, ok := healthBySkill[m.Name]; ok {
			hc := h
			view.Health = &hc
		}
		items = append(items, view)
	}
	httpx.WriteJSON(w, r, map[string]any{
		"items":    items,
		"total":    len(items),
		"category": category,
		"enabled":  true,
	})
	end(d, r, reqID, "skill.list", nil)
}

// handleGetSkill GET /api/v1/skills/{name} —— 技能详情（含 InputSchema / OutputSchema）。
func (d *Deps) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "skill.get")
	name := pathValue(r, "name")
	if name == "" {
		end(d, r, reqID, "skill.get", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	if d.Skills == nil || isNilInterface(d.Skills) {
		end(d, r, reqID, "skill.get", errors.New("skills 未装配"))
		writeError(w, r, errUnavailable("技能层未装配", nil))
		return
	}
	m, err := d.Skills.Get(r.Context(), name)
	if err != nil || m == nil {
		end(d, r, reqID, "skill.get", err)
		writeError(w, r, errNotFoundFn("技能不存在: "+name))
		return
	}
	payload := map[string]any{
		"manifest":     m,
		"inputSchema":  m.InputSchema,
		"outputSchema": m.OutputSchema,
	}
	for _, h := range d.Skills.Health(r.Context()) {
		if h.Skill == name {
			hc := h
			payload["health"] = &hc
			break
		}
	}
	httpx.WriteJSON(w, r, payload)
	end(d, r, reqID, "skill.get", nil)
}

// handleSkillStatus POST /api/v1/skills/{name}/status —— 启停 / 灰度技能。
//
// 非法 status 返回 400；技能不存在返回 404；CanaryPercent 超出 0-100 返回 400。
func (d *Deps) handleSkillStatus(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "skill.status")
	name := pathValue(r, "name")
	if name == "" {
		end(d, r, reqID, "skill.status", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	if d.Skills == nil || isNilInterface(d.Skills) {
		end(d, r, reqID, "skill.status", errors.New("skills 未装配"))
		writeError(w, r, errUnavailable("技能层未装配", nil))
		return
	}

	var req skillStatusRequest
	if err := decodeBody(d, r, &req); err != nil {
		end(d, r, reqID, "skill.status", err)
		writeError(w, r, err)
		return
	}
	status := domain.SkillStatus(strings.ToLower(strings.TrimSpace(req.Status)))
	if !validSkillStatus(status) {
		end(d, r, reqID, "skill.status", errors.New("status 非法"))
		writeError(w, r, errBadRequest("status 非法，可选 enabled/disabled/canary/deprecated"))
		return
	}
	if req.CanaryPercent < 0 || req.CanaryPercent > 100 {
		end(d, r, reqID, "skill.status", errors.New("canaryPercent 越界"))
		writeError(w, r, errBadRequest("canaryPercent 取值范围为 0-100"))
		return
	}

	m, err := d.Skills.Get(r.Context(), name)
	if err != nil || m == nil {
		end(d, r, reqID, "skill.status", err)
		writeError(w, r, errNotFoundFn("技能不存在: "+name))
		return
	}
	version := strings.TrimSpace(req.Version)
	if version == "" {
		version = m.Version
	}
	if err := d.Skills.SetStatus(r.Context(), name, version, status); err != nil {
		end(d, r, reqID, "skill.status", err)
		writeError(w, r, err)
		return
	}
	d.audit(r, "skill", "set_status", "warn", "调整技能状态 "+name,
		map[string]any{"skill": name, "version": version, "status": string(status), "canaryPercent": req.CanaryPercent})
	httpx.WriteJSON(w, r, map[string]any{
		"name":          name,
		"version":       version,
		"status":        status,
		"canaryPercent": req.CanaryPercent,
	})
	end(d, r, reqID, "skill.status", nil)
}

// handleSkillRollback POST /api/v1/skills/{name}/rollback —— 回滚到上一可用版本。
//
// 无可用回滚版本时（错误文本含 no rollback / 无回滚）返回 409，其余错误走统一映射。
func (d *Deps) handleSkillRollback(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "skill.rollback")
	name := pathValue(r, "name")
	if name == "" {
		end(d, r, reqID, "skill.rollback", errors.New("name 为空"))
		writeError(w, r, errBadRequest("name 为必填"))
		return
	}
	if d.Skills == nil || isNilInterface(d.Skills) {
		end(d, r, reqID, "skill.rollback", errors.New("skills 未装配"))
		writeError(w, r, errUnavailable("技能层未装配", nil))
		return
	}
	version, err := d.Skills.Rollback(r.Context(), name)
	if err != nil {
		end(d, r, reqID, "skill.rollback", err)
		if isNoRollback(err) {
			writeError(w, r, errConflictFn("技能 "+name+" 没有可回滚的历史版本"))
			return
		}
		writeError(w, r, err)
		return
	}
	d.audit(r, "skill", "rollback", "warn", "回滚技能 "+name,
		map[string]any{"skill": name, "version": version})
	httpx.WriteJSON(w, r, rollbackResponse{Skill: name, Version: version, Message: "已回滚到上一可用版本"})
	end(d, r, reqID, "skill.rollback", nil)
}

// validSkillStatus 校验技能状态合法性。
func validSkillStatus(s domain.SkillStatus) bool {
	switch s {
	case domain.SkillEnabled, domain.SkillDisabled, domain.SkillCanary, domain.SkillDeprecated:
		return true
	default:
		return false
	}
}

// isNoRollback 判断"无可用回滚版本"错误（技能层错误变量尚未定稿，按文本兜底匹配）。
func isNoRollback(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "no rollback") || strings.Contains(low, "norollback") ||
		strings.Contains(err.Error(), "无回滚") || strings.Contains(err.Error(), "没有可回滚") ||
		strings.Contains(err.Error(), "无可回滚")
}
