package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/httpx"
)

// readyCheck 单项就绪检查结果。
type readyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// handleHealthz GET /healthz —— 健康检查（免认证，返回进程存活信息）。
func (d *Deps) handleHealthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, map[string]any{
		"status":  "ok",
		"version": d.version(),
		"uptime":  int64(time.Since(d.startedAt()).Seconds()),
		"time":    nowISO(),
	})
}

// handleReadyz GET /readyz —— 就绪检查：store 可读 + engine 存在 + 技能注册数 > 0。
//
// 任一关键项失败返回 503，并在 checks 中给出逐项结果，便于运维定位。
func (d *Deps) handleReadyz(w http.ResponseWriter, r *http.Request) {
	reqID := begin(d, r, "system.readyz")

	checks := make([]readyCheck, 0, 5)
	critical := true

	// 1) Store 可读：用一次真实读取验证索引可访问。
	storeDetail := "内存索引可读，租户数 "
	okStore := false
	switch {
	case d.Store == nil:
		storeDetail = "store 未装配"
	default:
		tenants := d.Store.ListTenants()
		storeDetail = fmt.Sprintf("内存索引可读，租户数 %d", len(tenants))
		okStore = true
	}
	checks = append(checks, readyCheck{Name: "store", OK: okStore, Detail: storeDetail})
	critical = critical && okStore

	// 2) Engine 存在（任务调度能力）。
	okEngine := d.Engine != nil && !isNilInterface(d.Engine)
	engineDetail := "调度层已装配"
	if !okEngine {
		engineDetail = "调度层未装配，任务接口不可用"
	}
	checks = append(checks, readyCheck{Name: "engine", OK: okEngine, Detail: engineDetail})
	critical = critical && okEngine

	// 3) 技能注册数 > 0。
	skillCount := 0
	okSkills := false
	if d.Skills != nil && !isNilInterface(d.Skills) {
		skillCount = len(d.Skills.List(r.Context(), ""))
		okSkills = skillCount > 0
	}
	skillDetail := fmt.Sprintf("已注册技能 %d 个", skillCount)
	if !okSkills {
		skillDetail = "未注册任何技能或技能层未装配"
	}
	checks = append(checks, readyCheck{Name: "skills", OK: okSkills, Detail: skillDetail})
	critical = critical && okSkills

	// 4) 非关键项：源码解析与模型层缺失只降级不致命（契约 §6.6 降级不失败）。
	sourceOK := d.Source != nil && !isNilInterface(d.Source)
	sourceDetail := "源码解析已装配"
	if !sourceOK {
		sourceDetail = "源码解析未装配（/repos/{id}/probe 降级）"
	}
	checks = append(checks, readyCheck{Name: "source", OK: sourceOK, Detail: sourceDetail})

	mcpOK := d.MCP != nil && !isNilInterface(d.MCP)
	mcpDetail := "模型管控层已装配"
	if !mcpOK {
		mcpDetail = "模型管控层未装配（/models 返回空态）"
	}
	checks = append(checks, readyCheck{Name: "mcp", OK: mcpOK, Detail: mcpDetail})

	status := "ready"
	if !critical {
		status = "not_ready"
	}
	payload := map[string]any{
		"status":  status,
		"version": d.version(),
		"time":    nowISO(),
		"checks":  checks,
	}
	if !critical {
		end(d, r, reqID, "system.readyz", fmt.Errorf("关键依赖未就绪"))
		// 503：附完整检查明细，便于运维定位；用 APIError 保留业务码 503。
		writeError(w, r, newAPIError(codeUnavailable,
			"服务未就绪: "+string(jsonRaw(payload)), nil))
		return
	}
	httpx.WriteJSON(w, r, payload)
	end(d, r, reqID, "system.readyz", nil)
}

// handleMetrics GET /metrics —— Prometheus 文本格式指标。
//
// 指标来源：store 聚合 + Recorder（可选）+ Source 缓存统计（可选）+ 进程运行时长。
// 输出严格遵守 Prometheus 文本协议：每个指标先 HELP/TYPE，再输出样本行。
func (d *Deps) handleMetrics(w http.ResponseWriter, r *http.Request) {
	tenantID := subject(r).TenantID
	ctx := r.Context()

	var b strings.Builder

	// --- 任务状态分布 ---
	stateCounts := map[domain.TaskState]int{}
	runsTotal := 0
	degraded := 0
	var tokenTotal int64
	if page, err := d.provider().ListRuns(ctx, tenantID, domain.PageQuery{Page: 1, PageSize: 1}); err == nil {
		runsTotal = page.Total
	}
	// 逐条统计（租户内运行量级为内存索引规模，成本可控）。
	allRuns := d.allRuns(tenantID)
	for _, run := range allRuns {
		stateCounts[run.State]++
		if run.Degraded || run.State == domain.StateDegraded {
			degraded++
		}
		tokenTotal += int64(run.Usage.TotalTokens)
	}

	b.WriteString("# HELP codeagent_tasks_total 任务数量（按状态机状态分组）。\n")
	b.WriteString("# TYPE codeagent_tasks_total gauge\n")
	// 固定输出全部合法状态（含 0），保证 Prometheus 时间序列稳定、前端图表不留空洞。
	for _, st := range []domain.TaskState{
		domain.StateQueued, domain.StateAnalyzing, domain.StateRepairing, domain.StateVerifying,
		domain.StateSucceeded, domain.StateNeedsReview, domain.StateFailed, domain.StateCancelled,
		domain.StateDegraded,
	} {
		fmt.Fprintf(&b, "codeagent_tasks_total{state=%q} %d\n", string(st), stateCounts[st])
	}

	b.WriteString("# HELP codeagent_runs_total 执行记录总数。\n")
	b.WriteString("# TYPE codeagent_runs_total gauge\n")
	fmt.Fprintf(&b, "codeagent_runs_total %d\n", runsTotal)

	// --- 技能调用 ---
	skillCalls := d.allSkillCalls(tenantID)
	skillByStatus := map[string]int{}
	for _, c := range skillCalls {
		skillByStatus[string(c.Status)]++
	}
	b.WriteString("# HELP codeagent_skill_calls_total 技能调用次数（按调用状态分组）。\n")
	b.WriteString("# TYPE codeagent_skill_calls_total gauge\n")
	// 固定输出全部合法调用状态（含 0）。
	for _, st := range []domain.CallStatus{
		domain.CallOK, domain.CallFailed, domain.CallTimeout, domain.CallBlocked,
		domain.CallSkipped, domain.CallFallback,
	} {
		fmt.Fprintf(&b, "codeagent_skill_calls_total{status=%q} %d\n", string(st), skillByStatus[string(st)])
	}

	// --- 模型 Token ---
	modelCalls := d.allModelCalls(tenantID)
	modelByStatus := map[string]int{}
	for _, c := range modelCalls {
		modelByStatus[string(c.Status)]++
	}
	if tokenTotal == 0 {
		for _, c := range modelCalls {
			tokenTotal += int64(c.TotalTokens)
		}
	}
	b.WriteString("# HELP codeagent_model_tokens_total 模型 Token 消耗总量。\n")
	b.WriteString("# TYPE codeagent_model_tokens_total counter\n")
	fmt.Fprintf(&b, "codeagent_model_tokens_total %d\n", tokenTotal)

	b.WriteString("# HELP codeagent_model_calls_total 模型推理调用次数（按状态分组）。\n")
	b.WriteString("# TYPE codeagent_model_calls_total gauge\n")
	for _, st := range []domain.CallStatus{
		domain.CallOK, domain.CallFailed, domain.CallTimeout, domain.CallBlocked,
		domain.CallSkipped, domain.CallFallback,
	} {
		fmt.Fprintf(&b, "codeagent_model_calls_total{status=%q} %d\n", string(st), modelByStatus[string(st)])
	}

	// --- 降级运行 ---
	b.WriteString("# HELP codeagent_runs_degraded_total 降级运行数量（配额/线索不足导致）。\n")
	b.WriteString("# TYPE codeagent_runs_degraded_total gauge\n")
	fmt.Fprintf(&b, "codeagent_runs_degraded_total %d\n", degraded)

	// --- 运行时长 ---
	b.WriteString("# HELP codeagent_uptime_seconds 服务运行时长（秒）。\n")
	b.WriteString("# TYPE codeagent_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "codeagent_uptime_seconds %d\n", int64(time.Since(d.startedAt()).Seconds()))

	// --- 源码缓存命中率（Source 提供 Stats 时输出） ---
	if d.Source != nil && !isNilInterface(d.Source) {
		stats := d.Source.Stats()
		b.WriteString("# HELP codeagent_cache_hit_ratio 源码文件缓存命中率（0-1）。\n")
		b.WriteString("# TYPE codeagent_cache_hit_ratio gauge\n")
		fmt.Fprintf(&b, "codeagent_cache_hit_ratio %s\n", formatMetricFloat(stats.HitRate))
		b.WriteString("# HELP codeagent_cache_entries 源码文件缓存条目数。\n")
		b.WriteString("# TYPE codeagent_cache_entries gauge\n")
		fmt.Fprintf(&b, "codeagent_cache_entries %d\n", stats.Entries)
	}

	// --- WebSocket 连接数（可选，便于容量观测） ---
	b.WriteString("# HELP codeagent_ws_connections 当前 WebSocket 连接数。\n")
	b.WriteString("# TYPE codeagent_ws_connections gauge\n")
	fmt.Fprintf(&b, "codeagent_ws_connections %d\n", wsConnections(d))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// allRuns 返回租户全部运行（优先 Store 内存索引，避免分页截断）。
func (d *Deps) allRuns(tenantID string) []domain.TaskRun {
	if d.Store == nil {
		return nil
	}
	return d.Store.AllRuns(tenantID)
}

// allSkillCalls 返回租户全部技能调用记录。
func (d *Deps) allSkillCalls(tenantID string) []domain.SkillCall {
	if d.Store == nil {
		return nil
	}
	return d.Store.AllSkillCalls(tenantID)
}

// allModelCalls 返回租户全部模型调用记录。
func (d *Deps) allModelCalls(tenantID string) []domain.ModelCall {
	if d.Store == nil {
		return nil
	}
	return d.Store.AllModelCalls(tenantID)
}

// wsConnections 返回当前 WebSocket 连接数（Hub 未提供能力时返回 0）。
func wsConnections(d *Deps) int {
	if c, ok := d.Hub.(interface{ Connections() int }); ok {
		return c.Connections()
	}
	return 0
}

// formatMetricFloat 输出合法 Prometheus 浮点文本（避免 NaN/Inf 破坏解析）。
func formatMetricFloat(v float64) string {
	if v != v || v > 1e308 || v < -1e308 {
		return "0"
	}
	return fmt.Sprintf("%g", v)
}

// version 返回服务版本号。
func (d *Deps) version() string {
	if d.Version == "" {
		return "dev"
	}
	return d.Version
}

// startedAt 返回服务启动时间（未设置时退化为当前时间）。
func (d *Deps) startedAt() time.Time {
	if d.Started.IsZero() {
		return time.Now()
	}
	return d.Started
}
