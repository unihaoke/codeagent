package handler

import (
	"net/http"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// handleObservabilitySummary GET /api/v1/observability/summary —— 可观测汇总。
//
// 数据来源：Recorder.Observability（装配时）；未装配 Recorder 时由 Store 聚合兜底。
// 汇总内容涵盖修复率、Top 技能/模型、状态分布、最近失败，支撑前端大盘与复盘。
func (d *Deps) handleObservabilitySummary(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "observability.summary")
	tenantID := subject(r).TenantID

	sum, err := d.provider().Observability(r.Context(), tenantID)
	if err != nil || sum == nil {
		end(d, r, reqID, "observability.summary", err)
		writeError(w, r, err)
		return
	}
	// 附带引擎实时统计（Engine 装配时可用），便于大盘展示排队/运行中数量。
	payload := map[string]any{"summary": sum}
	if d.Engine != nil && !isNilInterface(d.Engine) {
		if stats, err := d.Engine.Stats(r.Context(), tenantID); err == nil && stats != nil {
			payload["engine"] = stats
		}
	}
	payload["generatedAt"] = nowISO()
	httpx.WriteJSON(w, r, payload)
	end(d, r, reqID, "observability.summary", nil)
}

// handleObservabilityAudits GET /api/v1/observability/audits —— 审计事件（分类/关键字分页）。
//
// `?state=<category>` 复用统一分页语义过滤审计分类（task/repo/group/skill/model/source/patch/auth/quota）。
func (d *Deps) handleObservabilityAudits(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "observability.audits")
	tenantID := subject(r).TenantID
	q := pageQuery(r)

	page, err := d.provider().ListAudits(r.Context(), tenantID, q)
	if err != nil {
		end(d, r, reqID, "observability.audits", err)
		writeError(w, r, err)
		return
	}
	if page.Items == nil {
		page.Items = []domain.AuditEvent{}
	}
	httpx.WritePage(w, r, page)
	end(d, r, reqID, "observability.audits", nil)
}
