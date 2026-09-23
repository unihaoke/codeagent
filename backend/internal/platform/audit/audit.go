// Package audit 实现审计与可观测记录器（domain.Recorder）。
//
// 每条记录同时落两份：
//   - store：审计事件、技能调用、模型调用（可观测汇总与轨迹查询的数据源）；
//   - EventBus：实时事件（SSE / WebSocket 推送的数据源）。
//
// 所有查询与统计都严格按 TenantID 隔离，跨租户不可见。
package audit

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/store"
	"github.com/google/uuid"
)

// 审计级别。
const (
	// LevelInfo 普通信息。
	LevelInfo = "info"
	// LevelWarn 警告。
	LevelWarn = "warn"
	// LevelError 错误。
	LevelError = "error"
)

// 审计分类（与 domain.AuditEvent.Category 的约定一致）。
const (
	// CatTask 任务生命周期。
	CatTask = "task"
	// CatRepo 仓库切换。
	CatRepo = "repo"
	// CatGroup 分组。
	CatGroup = "group"
	// CatSkill 技能调用。
	CatSkill = "skill"
	// CatModel 模型推理。
	CatModel = "model"
	// CatSource 源码拉取。
	CatSource = "source"
	// CatPatch 修复变更。
	CatPatch = "patch"
	// CatAuth 认证授权。
	CatAuth = "auth"
	// CatQuota 配额。
	CatQuota = "quota"
)

// 事件类型（与 CONTRACT.md §3 事件协议一致）。
const (
	// EventLog 结构化日志埋点。
	EventLog = "log"
	// EventTaskState 任务状态机迁移。
	EventTaskState = "task.state"
	// EventStage 阶段开始/结束。
	EventStage = "stage"
	// EventSkillCall 技能调用。
	EventSkillCall = "skill.call"
	// EventModelCall 模型推理。
	EventModelCall = "model.call"
	// EventRepoSwitch 仓库切换。
	EventRepoSwitch = "repo.switch"
	// EventSourceFetch 源码拉取。
	EventSourceFetch = "source.fetch"
	// EventPatchProposed 补丁产出。
	EventPatchProposed = "patch.proposed"
	// EventVerification 补丁验证。
	EventVerification = "verification"
	// EventReportReady 报告归档。
	EventReportReady = "report.ready"
)

// topLimit 榜单条数上限。
const topLimit = 5

// recentFailureLimit 最近失败运行条数上限。
const recentFailureLimit = 5

// Recorder 审计与可观测记录器。
type Recorder struct {
	st  *store.Store
	bus domain.EventBus
}

// 编译期校验：Recorder 必须满足 domain.Recorder 契约。
var _ domain.Recorder = (*Recorder)(nil)

// New 创建记录器；bus 允许为 nil（仅落库、不广播）。
func New(st *store.Store, bus domain.EventBus) *Recorder {
	return &Recorder{st: st, bus: bus}
}

// Store 返回底层存储（便于上层只读查询）。
func (r *Recorder) Store() *store.Store {
	if r == nil {
		return nil
	}
	return r.st
}

// ---------------------------------------------------------------------------
// domain.Recorder 实现
// ---------------------------------------------------------------------------

// Audit 写入一条审计事件并广播 log 事件。
func (r *Recorder) Audit(ev domain.AuditEvent) {
	if r == nil || r.st == nil {
		return
	}
	if ev.ID == "" {
		ev.ID = "aud-" + uuid.NewString()
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if ev.Level == "" {
		ev.Level = LevelInfo
	}
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type:     EventLog,
		TenantID: ev.TenantID,
		RunID:    ev.RunID,
		TaskID:   ev.TaskID,
		Level:    ev.Level,
		Message:  ev.Message,
		Payload:  ev,
		At:       ev.At,
	})
}

// SkillCall 记录技能调用并广播 skill.call 事件。
func (r *Recorder) SkillCall(call domain.SkillCall) {
	if r == nil || r.st == nil {
		return
	}
	if call.ID == "" {
		call.ID = "call-" + uuid.NewString()
	}
	if call.StartedAt.IsZero() {
		call.StartedAt = time.Now()
	}
	r.st.AppendSkillCall(call)
	r.emit(domain.Event{
		Type:     EventSkillCall,
		TenantID: call.TenantID,
		RunID:    call.RunID,
		Level:    callLevel(call.Status),
		Stage:    call.Stage,
		Message:  fmt.Sprintf("技能 %s 调用%s", call.Skill, callStatusText(call.Status)),
		Payload:  call,
		At:       call.StartedAt,
	})
}

// ModelCall 记录模型推理并广播 model.call 事件。
func (r *Recorder) ModelCall(call domain.ModelCall) {
	if r == nil || r.st == nil {
		return
	}
	if call.ID == "" {
		call.ID = "mcall-" + uuid.NewString()
	}
	if call.StartedAt.IsZero() {
		call.StartedAt = time.Now()
	}
	r.st.AppendModelCall(call)
	r.emit(domain.Event{
		Type:     EventModelCall,
		TenantID: call.TenantID,
		RunID:    call.RunID,
		Level:    callLevel(call.Status),
		Stage:    call.Stage,
		Message:  fmt.Sprintf("模型 %s/%s 推理%s（%d tokens）", call.Provider, call.Model, callStatusText(call.Status), call.TotalTokens),
		Payload:  call,
		At:       call.StartedAt,
	})
}

// ListAudits 查询审计事件（委托存储层，租户隔离）。
func (r *Recorder) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	if err := ctxErr(ctx); err != nil {
		return domain.Page[domain.AuditEvent]{}, err
	}
	if r == nil || r.st == nil {
		return domain.Page[domain.AuditEvent]{Items: []domain.AuditEvent{}}, nil
	}
	return r.st.ListAudits(tenantID, q), nil
}

// ListSkillCalls 查询技能调用轨迹（runID 为空表示该租户全部）。
func (r *Recorder) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	if err := ctxErr(ctx); err != nil {
		return domain.Page[domain.SkillCall]{}, err
	}
	if r == nil || r.st == nil {
		return domain.Page[domain.SkillCall]{Items: []domain.SkillCall{}}, nil
	}
	return r.st.ListSkillCalls(tenantID, runID, q), nil
}

// ListModelCalls 查询模型推理轨迹（runID 为空表示该租户全部）。
func (r *Recorder) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	if err := ctxErr(ctx); err != nil {
		return domain.Page[domain.ModelCall]{}, err
	}
	if r == nil || r.st == nil {
		return domain.Page[domain.ModelCall]{Items: []domain.ModelCall{}}, nil
	}
	return r.st.ListModelCalls(tenantID, runID, q), nil
}

// ---------------------------------------------------------------------------
// 便捷埋点方法
// ---------------------------------------------------------------------------

// Info 记录 info 级审计事件，kv 进入 Data。
func (r *Recorder) Info(tenantID, runID, category, action, msg string, kv map[string]any) {
	r.levelAudit(LevelInfo, tenantID, runID, "", category, action, msg, kv)
}

// Warn 记录 warn 级审计事件。
func (r *Recorder) Warn(tenantID, runID, category, action, msg string, kv map[string]any) {
	r.levelAudit(LevelWarn, tenantID, runID, "", category, action, msg, kv)
}

// Error 记录 error 级审计事件。
func (r *Recorder) Error(tenantID, runID, category, action, msg string, kv map[string]any) {
	r.levelAudit(LevelError, tenantID, runID, "", category, action, msg, kv)
}

// TaskInfo 记录带 taskID 的 info 级审计事件。
func (r *Recorder) TaskInfo(tenantID, taskID, runID, category, action, msg string, kv map[string]any) {
	r.levelAudit(LevelInfo, tenantID, runID, taskID, category, action, msg, kv)
}

// levelAudit 组装并写入审计事件。
func (r *Recorder) levelAudit(level, tenantID, runID, taskID, category, action, msg string, kv map[string]any) {
	r.Audit(domain.AuditEvent{
		TenantID: tenantID,
		RunID:    runID,
		TaskID:   taskID,
		Category: category,
		Action:   action,
		Level:    level,
		Message:  msg,
		Data:     kv,
	})
}

// RepoSwitch 记录仓库切换（分析链路上从一个仓库切换到另一个仓库）并广播 repo.switch。
func (r *Recorder) RepoSwitch(tenantID, runID string, repoID, repoName, commit string, from, to string) {
	data := map[string]any{"repoId": repoID, "repoName": repoName, "commit": commit, "from": from, "to": to}
	ev := domain.AuditEvent{
		TenantID: tenantID,
		RunID:    runID,
		Category: CatRepo,
		Action:   "switch",
		Level:    LevelInfo,
		Message:  fmt.Sprintf("仓库切换 %s → %s（%s@%s）", orDash(from), orDash(to), repoName, shortCommit(commit)),
		RepoID:   repoID,
		Commit:   commit,
		Data:     data,
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventRepoSwitch, TenantID: tenantID, RunID: runID, Level: ev.Level,
		Message: ev.Message, Payload: ev, At: ev.At,
	})
}

// SourceFetch 记录一次源码拉取（版本锁定或文件加载）。
func (r *Recorder) SourceFetch(tenantID, runID, repoID, repoKey, commit string, files int, err error) {
	level := LevelInfo
	data := map[string]any{"repoId": repoID, "repoKey": repoKey, "commit": commit, "files": files}
	msg := fmt.Sprintf("源码拉取 %s@%s，命中文件 %d 个", repoKey, shortCommit(commit), files)
	if err != nil {
		level = LevelWarn
		data["error"] = err.Error()
		msg = fmt.Sprintf("源码拉取失败 %s@%s: %v", repoKey, shortCommit(commit), err)
	}
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, Category: CatSource, Action: "fetch",
		Level: level, Message: msg, RepoID: repoID, Commit: commit, Data: data,
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventSourceFetch, TenantID: tenantID, RunID: runID, Level: ev.Level,
		Message: ev.Message, Payload: ev, At: ev.At,
	})
}

// PatchProposed 记录补丁产出并广播 patch.proposed（Payload 为补丁列表）。
func (r *Recorder) PatchProposed(tenantID, runID string, patches []domain.Patch) {
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, Category: CatPatch, Action: "propose", Level: LevelInfo,
		Message: fmt.Sprintf("产出增量补丁 %d 个", len(patches)), Data: patches,
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventPatchProposed, TenantID: tenantID, RunID: runID, Level: ev.Level,
		Message: ev.Message, Payload: patches, At: ev.At,
	})
}

// Verification 记录沙箱验证结果并广播 verification。
func (r *Recorder) Verification(tenantID, runID string, v *domain.Verification) {
	level := LevelInfo
	passed := false
	var checks int
	if v != nil {
		passed = v.Passed
		checks = len(v.Checks)
		if !passed {
			level = LevelWarn
		}
	}
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, Category: CatPatch, Action: "verify", Level: level,
		Message: fmt.Sprintf("沙箱验证%s，校验项 %d 个", passText(passed), checks), Data: v,
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventVerification, TenantID: tenantID, RunID: runID, Level: ev.Level,
		Message: ev.Message, Payload: v, At: ev.At,
	})
}

// ReportReady 记录报告归档并广播 report.ready。
func (r *Recorder) ReportReady(tenantID, runID, reportID string) {
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, Category: CatTask, Action: "report_ready", Level: LevelInfo,
		Message: "分析报告已归档: " + reportID, Data: map[string]any{"reportId": reportID},
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventReportReady, TenantID: tenantID, RunID: runID, Level: ev.Level,
		Message: ev.Message, Payload: map[string]any{"reportId": reportID}, At: ev.At,
	})
}

// TaskState 记录任务状态机迁移并广播 task.state（Payload 为 {from,to}）。
func (r *Recorder) TaskState(tenantID, taskID, runID string, from, to domain.TaskState) {
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, TaskID: taskID, Category: CatTask, Action: "state",
		Level:   LevelInfo,
		Message: fmt.Sprintf("任务状态迁移 %s → %s", from, to),
		Data:    map[string]any{"from": from, "to": to},
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventTaskState, TenantID: tenantID, RunID: runID, TaskID: taskID, Level: ev.Level,
		Message: ev.Message, Payload: map[string]any{"from": from, "to": to}, At: ev.At,
	})
}

// Stage 记录流水线阶段进展并广播 stage 事件。
func (r *Recorder) Stage(tenantID, taskID, runID, stage, msg string, kv map[string]any) {
	ev := domain.AuditEvent{
		TenantID: tenantID, RunID: runID, TaskID: taskID, Category: CatTask, Action: "stage",
		Level:   LevelInfo,
		Message: msg,
		Data:    kv,
	}
	if r == nil || r.st == nil {
		return
	}
	r.fill(&ev)
	r.st.AppendAudit(ev)
	r.emit(domain.Event{
		Type: EventStage, TenantID: tenantID, RunID: runID, TaskID: taskID, Level: ev.Level,
		Stage: stage, Message: msg, Payload: kv, At: ev.At,
	})
}

// fill 补齐审计事件的 ID/时间/级别。
func (r *Recorder) fill(ev *domain.AuditEvent) {
	if ev.ID == "" {
		ev.ID = "aud-" + uuid.NewString()
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	if ev.Level == "" {
		ev.Level = LevelInfo
	}
}

// emit 广播事件（bus 为空时静默跳过）。
func (r *Recorder) emit(ev domain.Event) {
	if r == nil || r.bus == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.bus.Publish(ev)
}

// ---------------------------------------------------------------------------
// 可观测汇总
// ---------------------------------------------------------------------------

// Observability 聚合租户维度的可观测汇总。
func (r *Recorder) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	sum := &domain.ObservabilitySummary{
		TopSkills:      []domain.SkillMetric{},
		TopModels:      []domain.ModelMetric{},
		StateDist:      map[string]int{},
		CategoryDist:   map[string]int{},
		RecentFailures: []domain.RunBrief{},
	}
	if r == nil || r.st == nil {
		return sum, nil
	}

	// 任务总数：借分页 Total 取全量计数（PageSize=1 只回传 1 条）。
	sum.TasksTotal = r.st.ListTasks(tenantID, domain.PageQuery{Page: 1, PageSize: 1}).Total

	runs := r.st.AllRuns(tenantID)
	sum.RunsTotal = len(runs)

	for state, n := range r.st.CountRunsByState(tenantID) {
		sum.StateDist[string(state)] += n
	}

	var elapsedSum int64
	var elapsedCount int64
	failures := make([]domain.RunBrief, 0, recentFailureLimit)
	for i := range runs {
		run := runs[i]
		switch run.State {
		case domain.StateSucceeded:
			sum.RunsSucceeded++
		case domain.StateNeedsReview:
			sum.RunsNeedsReview++
		case domain.StateFailed:
			sum.RunsFailed++
		case domain.StateDegraded:
			sum.RunsDegraded++
		}
		if !run.StartedAt.IsZero() {
			elapsedSum += run.ElapsedMS()
			elapsedCount++
		}
		// 缓存命中与仓库切换数（累加 resolution 数量）。
		sum.CacheHits += run.Usage.CacheHits
		sum.RepoSwitches += len(run.Resolution)
		if run.RootCause != nil && run.RootCause.Category != "" {
			sum.CategoryDist[run.RootCause.Category]++
		}
		if run.State == domain.StateFailed || run.State == domain.StateDegraded {
			failures = append(failures, domain.RunBrief{
				RunID:    run.ID,
				Title:    run.Title,
				State:    run.State,
				Severity: run.Severity,
				Summary:  failureSummary(run),
				At:       runTime(run),
			})
		}
	}
	if elapsedCount > 0 {
		sum.AvgElapsedMS = elapsedSum / elapsedCount
	}
	// 修复率 = 成功 / (成功 + 待复核 + 失败 + 降级)，降级视为未成功修复，无分母时为 0。
	if denom := sum.RunsSucceeded + sum.RunsNeedsReview + sum.RunsFailed + sum.RunsDegraded; denom > 0 {
		sum.FixRate = float64(sum.RunsSucceeded) / float64(denom)
	}

	// 技能指标。
	skillAgg := map[string]*domain.SkillMetric{}
	var skillLatency = map[string]int64{}
	for _, call := range r.st.AllSkillCalls(tenantID) {
		sum.SkillCalls++
		if isFailedStatus(call.Status) {
			sum.SkillFailures++
		}
		m := skillAgg[call.Skill]
		if m == nil {
			m = &domain.SkillMetric{Skill: call.Skill}
			skillAgg[call.Skill] = m
		}
		m.Calls++
		if isFailedStatus(call.Status) {
			m.Failures++
		}
		skillLatency[call.Skill] += call.DurationMS
	}
	if sum.SkillCalls > 0 {
		sum.SkillFailureRate = float64(sum.SkillFailures) / float64(sum.SkillCalls)
	}
	for name, m := range skillAgg {
		if m.Calls > 0 {
			m.AvgLatencyMS = float64(skillLatency[name]) / float64(m.Calls)
		}
		sum.TopSkills = append(sum.TopSkills, *m)
	}
	sort.SliceStable(sum.TopSkills, func(i, j int) bool {
		if sum.TopSkills[i].Calls != sum.TopSkills[j].Calls {
			return sum.TopSkills[i].Calls > sum.TopSkills[j].Calls
		}
		return sum.TopSkills[i].Skill < sum.TopSkills[j].Skill
	})
	if len(sum.TopSkills) > topLimit {
		sum.TopSkills = sum.TopSkills[:topLimit]
	}

	// 模型指标。
	modelAgg := map[string]*domain.ModelMetric{}
	var modelLatency = map[string]int64{}
	for _, call := range r.st.AllModelCalls(tenantID) {
		sum.ModelCalls++
		sum.ModelTokens += call.TotalTokens
		if call.FallbackFrom != "" || call.Status == domain.CallFallback || call.Tier == domain.TierFallback {
			sum.ModelFallbacks++
		}
		m := modelAgg[call.Model]
		if m == nil {
			m = &domain.ModelMetric{Model: call.Model}
			modelAgg[call.Model] = m
		}
		m.Calls++
		m.TotalTokens += call.TotalTokens
		modelLatency[call.Model] += call.DurationMS
	}
	for name, m := range modelAgg {
		if m.Calls > 0 {
			m.AvgLatencyMS = float64(modelLatency[name]) / float64(m.Calls)
		}
		sum.TopModels = append(sum.TopModels, *m)
	}
	sort.SliceStable(sum.TopModels, func(i, j int) bool {
		if sum.TopModels[i].Calls != sum.TopModels[j].Calls {
			return sum.TopModels[i].Calls > sum.TopModels[j].Calls
		}
		return sum.TopModels[i].Model < sum.TopModels[j].Model
	})
	if len(sum.TopModels) > topLimit {
		sum.TopModels = sum.TopModels[:topLimit]
	}

	// 最近失败运行。
	sort.SliceStable(failures, func(i, j int) bool { return failures[i].At.After(failures[j].At) })
	if len(failures) > recentFailureLimit {
		failures = failures[:recentFailureLimit]
	}
	sum.RecentFailures = failures

	return sum, nil
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// ctxErr 快速感知上下文取消。
func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// isFailedStatus 判断调用是否属于失败（超时计入失败；熔断/跳过不计）。
func isFailedStatus(s domain.CallStatus) bool {
	return s == domain.CallFailed || s == domain.CallTimeout
}

// callLevel 由调用状态推导审计级别。
func callLevel(s domain.CallStatus) string {
	switch s {
	case domain.CallFailed, domain.CallTimeout:
		return LevelError
	case domain.CallBlocked, domain.CallFallback, domain.CallSkipped:
		return LevelWarn
	default:
		return LevelInfo
	}
}

// callStatusText 调用状态的中文描述。
func callStatusText(s domain.CallStatus) string {
	switch s {
	case domain.CallOK:
		return "成功"
	case domain.CallFailed:
		return "失败"
	case domain.CallTimeout:
		return "超时"
	case domain.CallBlocked:
		return "被熔断拦截"
	case domain.CallSkipped:
		return "跳过"
	case domain.CallFallback:
		return "兜底完成"
	default:
		return string(s)
	}
}

// passText 验证结论文本。
func passText(passed bool) string {
	if passed {
		return "通过"
	}
	return "未通过"
}

// orDash 空值占位。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortCommit 截断 commit 便于阅读。
func shortCommit(commit string) string {
	if commit == "" {
		return "-"
	}
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// failureSummary 失败运行的摘要：优先根因结论，其次错误信息。
func failureSummary(run domain.TaskRun) string {
	if run.RootCause != nil && run.RootCause.Summary != "" {
		return run.RootCause.Summary
	}
	return run.Error
}

// runTime 运行的关键时间点：结束时间 > 更新时间 > 创建时间。
func runTime(run domain.TaskRun) time.Time {
	if !run.EndedAt.IsZero() {
		return run.EndedAt
	}
	if !run.UpdatedAt.IsZero() {
		return run.UpdatedAt
	}
	return run.CreatedAt
}
