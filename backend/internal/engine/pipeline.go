// Package engine 实现 Agent 核心调度层（第四层，系统大脑）。
//
// 本包由四部分组成：
//
//   - engine.go   调度核心：生命周期、任务受理、幂等去重、配额管控、worker pool、
//     取消与重跑，实现 domain.TaskEngine。
//   - pipeline.go 任务流水线：版本锁定 → 堆栈解析 → 仓库匹配 → 懒加载 → 根因推理 →
//     补丁合成 → 沙箱验证 → 报告 → 归档，实现 domain.TaskPipeline；
//     阶段命名严格遵循 CONTRACT §3 的阶段标准命名。
//   - report.go   报告构建与 Markdown 渲染（可直接贴工单/PR）。
//   - stats.go    运行统计聚合（EngineStats / 趋势桶）。
//
// 依赖方向：本包只依赖 domain / store / config / platform/logx / sandbox，
// 其余六层全部通过 domain 端口接口注入（Deps），便于单测注入 fake。
//
// 说明（契约适配）：domain.Recorder 为冻结契约，未定义 Stage / RepoSwitch 方法，
// 因此阶段与仓库切换埋点统一通过 Recorder.Audit(Category=stage|repo|source|patch) 落地，
// 语义与 CONTRACT §3/§6.10 要求的六类审计保持一致。
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/sandbox"
	"github.com/codeagent/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 阶段标准命名（CONTRACT §3）
// ---------------------------------------------------------------------------

// 阶段标准命名常量：与前端时间线、审计事件、事件总线严格一致，不得随意改动。
const (
	// StageResolve 阶段 1：任务级代码版本锁定。
	StageResolve = "resolve"
	// StageStackParse 阶段 2：堆栈/日志解析。
	StageStackParse = "stack_parse"
	// StageCandidates 阶段 3：候选仓库匹配打分。
	StageCandidates = "candidates"
	// StageCodeLoad 阶段 4：堆栈驱动懒加载代码切片。
	StageCodeLoad = "code_load"
	// StageCrossRepo 阶段 5 子阶段：跨仓库链路追踪（分组模式且候选 ≥2 时触发）。
	StageCrossRepo = "cross_repo"
	// StageRootCause 阶段 5：根因推理。
	StageRootCause = "root_cause"
	// StagePatchSynthesize 阶段 6：增量补丁合成。
	StagePatchSynthesize = "patch_synthesize"
	// StageSandboxVerify 阶段 7：沙箱闭环验证与迭代收敛。
	StageSandboxVerify = "sandbox_verify"
	// StageReport 阶段 8：报告生成与归档。
	StageReport = "report"
	// StageArchive 阶段 9：运行上下文落库与终态事件。
	StageArchive = "archive"
)

// MainStages 九个主阶段的规范顺序（cross_repo 为 root_cause 内的子阶段，不计入主阶段）。
var MainStages = []string{
	StageResolve,
	StageStackParse,
	StageCandidates,
	StageCodeLoad,
	StageRootCause,
	StagePatchSynthesize,
	StageSandboxVerify,
	StageReport,
	StageArchive,
}

// 引擎层的资源默认值：EngineConfig 未定义文件数/字符数配额，故由本层兜底。
const (
	defaultMaxFiles     = 40
	defaultMaxCodeChars = 120_000
	// maxFileSizeBytes 单文件上限 256KB，避免超大文件污染上下文。
	maxFileSizeBytes = 256 << 10
	defaultMaxDepth  = 12
	// candidateThreshold 候选仓库得分阈值，低于该值的仓库不进入懒加载范围。
	candidateThreshold = 0.15
	// maxCandidates 单任务最多加载的候选仓库数，保证评分与加载的确定性。
	maxCandidates = 8
	// maxHintsPerRepo 单仓库最多下发的文件线索数。
	maxHintsPerRepo = 12
)

// 运行上下文中的标记前缀：这些条目由 Engine 写入、Pipeline 读取，用于跨层传递少量控制语义。
const (
	// warnAutoVerifyOffPrefix 调用方关闭沙箱验证的警告前缀。
	warnAutoVerifyOffPrefix = "沙箱验证已关闭"
	// warnFeedbackPrefix 人工反馈前缀（RerunWithFeedback 写入）。
	warnFeedbackPrefix = "人工反馈："
)

// ---------------------------------------------------------------------------
// 依赖与流水线
// ---------------------------------------------------------------------------

// Deps 流水线依赖（全部为接口，便于单测注入 fake）。
type Deps struct {
	// Store 数据访问层（租户配额、分组视图、报告落库）。
	Store store.Store
	// Cfg 全系统配置。
	Cfg *config.Config
	// Source 源码解析与懒加载（版本锁定、文件切片）。
	Source domain.SourceResolver
	// Skills 技能执行器。
	Skills domain.SkillRunner
	// MCP 模型管控层入口。
	MCP domain.MCPGateway
	// Sandbox 源码沙箱执行层入口。
	Sandbox domain.SandboxManager
	// Packer 上下文智能治理。
	Packer domain.ContextPacker
	// Bus 实时事件总线。
	Bus domain.EventBus
	// Recorder 审计/埋点记录器。
	Recorder domain.Recorder
	// Log 结构化日志器。
	Log *logx.Logger
}

// StageHook 阶段进度回调：runID + 阶段名。Engine 借此让状态机随真实阶段推进，
// 而不是在流水线开始前"预迁移"，避免审计与前端时间线失真。
type StageHook func(runID, stage string)

// Pipeline 任务流水线实现（domain.TaskPipeline）。
type Pipeline struct {
	d Deps

	hookMu sync.RWMutex
	hook   StageHook
}

// NewPipeline 创建任务流水线。
func NewPipeline(d Deps) *Pipeline {
	if d.Log == nil {
		d.Log = logx.Default()
	}
	return &Pipeline{d: d}
}

// SetStageHook 注册阶段进度回调（Engine 在 Start 时调用；可重复注册，最后一次生效）。
func (p *Pipeline) SetStageHook(h StageHook) {
	p.hookMu.Lock()
	p.hook = h
	p.hookMu.Unlock()
}

func (p *Pipeline) notifyStage(runID, stage string) {
	p.hookMu.RLock()
	h := p.hook
	p.hookMu.RUnlock()
	if h != nil {
		h(runID, stage)
	}
}

func (p *Pipeline) log() *logx.Logger {
	if p.d.Log == nil {
		return logx.Nop()
	}
	return p.d.Log
}

func (p *Pipeline) publish(ev domain.Event) {
	if p.d.Bus == nil {
		return
	}
	p.d.Bus.Publish(ev)
}

func (p *Pipeline) audit(ev domain.AuditEvent) {
	if p.d.Recorder == nil {
		return
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	p.d.Recorder.Audit(ev)
}

// ---------------------------------------------------------------------------
// 运行内部上下文
// ---------------------------------------------------------------------------

// runCtx 一次流水线执行的内部可变状态，仅在本次 Execute 内使用。
//
// 并发约定：可并行阶段（如 resolve 与 stack_parse）会并发调用 warn/degrade/
// addTimeline，所有共享写入必须持 mu；阶段内部只写自己的局部结果，合并阶段串行执行。
type runCtx struct {
	// mu 保护 run/bundle/timeline 的共享写入（并行阶段可能同时降级或写时间线）。
	mu       sync.Mutex
	run      *domain.TaskRun
	bundle   *domain.EvidenceBundle
	timeline []domain.TimelineItem
	stage    string

	parent *domain.CallContext

	// 仓库索引
	reposByID   map[string]*domain.Repository
	reposByKey  map[string]*domain.Repository
	lockedRepos []*domain.Repository

	group      *domain.RepositoryGroup
	isGroup    bool
	candidates []domain.RepoCandidate

	// 降级标记
	skippedCode    bool // 堆栈线索不足：跳过阶段 4/6/7
	noCodeEvidence bool // 未加载到任何代码切片
	modelFailed    bool // 模型不可用

	// 修复轮次反馈（阶段 7 注入阶段 6 的模型提示）
	lastFeedback string

	// report 阶段 8 生成的报告（供 PipelineResult 返回）。
	report *domain.Report
}

func (rc *runCtx) addTimeline(stage, msg, level string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.timeline = append(rc.timeline, domain.TimelineItem{
		At:      time.Now(),
		Stage:   stage,
		Message: msg,
		Level:   level,
	})
}

// warn 追加运行警告并写入时间线（降级而非失败）。
//
// 串行阶段使用：阶段名取 rc.stage（由 runStage 设置）。
func (rc *runCtx) warn(msg string) { rc.warnStage(rc.stage, msg) }

// warnStage 指定阶段名追加警告：并行阶段必用（rc.stage 会被其他阶段覆盖）。
func (rc *runCtx) warnStage(stage, msg string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.warnLocked(stage, msg)
}

// warnLocked 在已持锁的前提下追加警告（供 degrade 复用，避免重复加锁）。
func (rc *runCtx) warnLocked(stage, msg string) {
	rc.run.Warnings = append(rc.run.Warnings, msg)
	rc.timeline = append(rc.timeline, domain.TimelineItem{
		At:      time.Now(),
		Stage:   stage,
		Message: msg,
		Level:   "warn",
	})
}

// degrade 标记本次运行降级并记录原因。
func (rc *runCtx) degrade(msg string) { rc.degradeStage(rc.stage, msg) }

// degradeStage 指定阶段名标记降级：并行阶段必用。
func (rc *runCtx) degradeStage(stage, msg string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.run.Degraded = true
	rc.warnLocked(stage, msg)
}

func (rc *runCtx) repoByID(id string) *domain.Repository {
	if id == "" {
		return nil
	}
	return rc.reposByID[id]
}

func (rc *runCtx) repoByKey(key string) *domain.Repository {
	if key == "" {
		return nil
	}
	if r := rc.reposByKey[normalizeIdent(key)]; r != nil {
		return r
	}
	return rc.reposByKey[key]
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

type stageFunc func(ctx context.Context, cc *domain.CallContext, rc *runCtx) error

type stageDef struct {
	name string
	fn   stageFunc
}

// stageGroup 一组阶段：parallel 为 true 时组内阶段并发执行。
type stageGroup struct {
	parallel bool
	stages   []stageDef
}

// stageGroups 返回按数据依赖切分的阶段组。
//
// 依赖分析（决定哪些阶段能并行）：
//   - resolve（版本锁定：git IO）与 stack_parse（堆栈解析：技能/模型 IO）是单次运行中
//     最耗时的两个 IO 阶段，且互不依赖（堆栈解析只读 run.Stacktrace/Logs），可并行；
//   - candidates 依赖 resolve 产出的仓库索引与 stack_parse 产出的堆栈分析结果；
//   - code_load 依赖候选仓库与锁定 commit，root_cause 依赖代码切片，
//     patch_synthesize 依赖根因，sandbox_verify 依赖补丁，report/archive 依赖全部产物。
//
// 因此只有第一组可并行，其余阶段存在严格顺序依赖，保持串行以免破坏"降级不失败"语义。
func (p *Pipeline) stageGroups() []stageGroup {
	return []stageGroup{
		{parallel: true, stages: []stageDef{
			{StageResolve, p.stageResolve},
			{StageStackParse, p.stageStackParse},
		}},
		{stages: []stageDef{{StageCandidates, p.stageCandidates}}},
		{stages: []stageDef{{StageCodeLoad, p.stageCodeLoad}}},
		{stages: []stageDef{{StageRootCause, p.stageRootCause}}},
		{stages: []stageDef{{StagePatchSynthesize, p.stagePatchSynthesize}}},
		{stages: []stageDef{{StageSandboxVerify, p.stageSandboxVerify}}},
		{stages: []stageDef{{StageReport, p.stageReport}}},
		{stages: []stageDef{{StageArchive, p.stageArchive}}},
	}
}

// runGroup 执行一个阶段组，返回 (是否中止, 错误)。
//
// 并行组的开始事件按声明顺序先行发布：阶段进度只前进不后退，
// 否则前端进度条会因完成顺序不同而来回跳变。
func (p *Pipeline) runGroup(ctx context.Context, cc *domain.CallContext, rc *runCtx, g stageGroup) (bool, error) {
	if !g.parallel || len(g.stages) <= 1 {
		return p.runSerial(ctx, cc, rc, g.stages)
	}

	// 1) 按声明顺序发布开始事件（含状态机阶段回调），保证阶段进度单调。
	for _, st := range g.stages {
		if rc.skipStage(st.name) {
			continue
		}
		p.notifyStage(rc.run.ID, st.name)
		p.stageEvent(rc, st.name, "start", fmt.Sprintf("阶段 %s 开始", stageTitle(st.name)), "info", nil)
		p.audit(domain.AuditEvent{
			TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
			Category: "stage", Action: st.name, Level: "info",
			Message: fmt.Sprintf("阶段 %s 开始（并行）", stageTitle(st.name)),
			Data:    map[string]any{"phase": "start", "parallel": true},
		})
	}

	// 2) 并发执行：每个 goroutine 只写自己的槽位，共享写入走 rc 的加锁方法。
	//
	// rc.stage 是共享字段（技能调用轨迹、时间线阶段名都取它），并发写会相互覆盖：
	// 由主线程执行最后一个声明的阶段（当前为 stack_parse，技能轨迹需要它），
	// 其余阶段在 goroutine 中并发，且这些阶段一律用 warnStage/degradeStage 显式指定阶段名。
	runs := make([]stageRun, len(g.stages))
	executed := make([]bool, len(g.stages))
	mainIdx := len(g.stages) - 1
	var wg sync.WaitGroup
	for i := range g.stages {
		st := g.stages[i]
		if rc.skipStage(st.name) || i == mainIdx {
			continue
		}
		executed[i] = true
		wg.Add(1)
		go func(idx int, def stageDef) {
			defer wg.Done()
			runs[idx] = p.execStage(ctx, cc, rc, def.name, def.fn, false)
		}(i, st)
	}
	if !rc.skipStage(g.stages[mainIdx].name) {
		executed[mainIdx] = true
		rc.stage = g.stages[mainIdx].name // 主线程独占，安全
		runs[mainIdx] = p.execStage(ctx, cc, rc, g.stages[mainIdx].name, g.stages[mainIdx].fn, false)
	}
	wg.Wait()

	// 3) 结束事件按声明顺序发布：阶段进度单调前进，不因完成先后而回退。
	for i := range g.stages {
		if executed[i] {
			p.emitStageEnd(rc, runs[i])
		}
	}

	// 4) 按声明顺序上报首个错误，保证失败归因稳定（不因调度顺序而变）。
	for i, st := range g.stages {
		if !executed[i] || runs[i].err == nil {
			continue
		}
		rc.run.Error = runs[i].err.Error()
		rc.addTimeline(st.name, "阶段执行失败："+runs[i].err.Error(), "error")
		return true, runs[i].err
	}
	return false, nil
}

// runSerial 顺序执行一组阶段。
func (p *Pipeline) runSerial(ctx context.Context, cc *domain.CallContext, rc *runCtx, stages []stageDef) (bool, error) {
	for _, st := range stages {
		if rc.skipStage(st.name) {
			rc.addTimeline(st.name, "本轮已降级为堆栈文本分析，跳过阶段 "+st.name, "warn")
			continue
		}
		if err := p.runStage(ctx, cc, rc, st.name, st.fn); err != nil {
			// 致命异常：交由 Engine 判定为 failed（上下文取消则判定为 cancelled）。
			rc.run.Error = err.Error()
			rc.addTimeline(st.name, "阶段执行失败："+err.Error(), "error")
			return true, err
		}
	}
	return false, nil
}

// Execute 执行完整流水线：证据准备 → 分析 → 修复 → 验证 → 报告 → 归档。
//
// 所有阶段遵循"降级不失败"原则：单阶段的可恢复异常一律转成 Warnings + 降级标记，
// 只有上下文取消、致命内部错误才会返回 error（由 Engine 判定为 failed/cancelled）。
func (p *Pipeline) Execute(ctx context.Context, cc *domain.CallContext, run *domain.TaskRun) (*domain.PipelineResult, error) {
	if run == nil {
		return nil, fmt.Errorf("任务运行为空，无法执行流水线")
	}
	if cc == nil {
		cc = &domain.CallContext{}
	}
	if cc.RunID == "" {
		cc.RunID = run.ID
	}
	if cc.TenantID == "" {
		cc.TenantID = run.TenantID
	}
	if cc.TaskID == "" {
		cc.TaskID = run.TaskID
	}
	if cc.PinnedCommits == nil {
		cc.PinnedCommits = run.PinnedCommits
	}

	rc := &runCtx{
		run:        run,
		bundle:     newEvidenceBundle(run),
		parent:     cc,
		reposByID:  map[string]*domain.Repository{},
		reposByKey: map[string]*domain.Repository{},
		isGroup:    run.Mode == domain.ModeGroup,
	}
	rc.lastFeedback = ExtractHumanFeedback(run)

	// 按阶段组推进：组内可并行（如 resolve 与 stack_parse），组间保持依赖顺序。
	for _, g := range p.stageGroups() {
		if _, err := p.runGroup(ctx, cc, rc, g); err != nil {
			// 致命异常：交由 Engine 判定为 failed（上下文取消则判定为 cancelled）。
			return p.buildResult(rc), err
		}
	}

	if rc.run.State == "" {
		rc.run.State = DecideState(rc.run)
	}
	if rc.run.EndedAt.IsZero() {
		rc.run.EndedAt = time.Now()
	}
	rc.run.UpdatedAt = time.Now()
	return p.buildResult(rc), nil
}

func (p *Pipeline) buildResult(rc *runCtx) *domain.PipelineResult {
	return &domain.PipelineResult{
		Run:      rc.run,
		Report:   rc.report,
		Evidence: rc.bundle,
		State:    rc.run.State,
		Degraded: rc.run.Degraded,
		Warnings: append([]string{}, rc.run.Warnings...),
	}
}

// skipStage 判定阶段是否因"堆栈线索不足"而降级跳过（阶段 4/6/7）。
func (rc *runCtx) skipStage(name string) bool {
	if !rc.skippedCode {
		return false
	}
	switch name {
	case StageCodeLoad, StagePatchSynthesize, StageSandboxVerify:
		return true
	default:
		return false
	}
}

// stageRun 一次阶段执行的结果（结束事件可延迟发布，见 runGroup）。
type stageRun struct {
	name  string
	err   error
	cost  time.Duration
	level string
	msg   string
}

// runStage 包裹单个阶段：开始/结束埋点 + 事件 + 时间线 + 状态机回调。
func (p *Pipeline) runStage(ctx context.Context, cc *domain.CallContext, rc *runCtx, name string, fn stageFunc) error {
	rc.stage = name
	r := p.execStage(ctx, cc, rc, name, fn, true)
	p.emitStageEnd(rc, r)
	return r.err
}

// execStage 执行阶段主体；announce=false 用于并行组（开始事件已由 runGroup 统一发布）。
//
// 注意：并行组不设置 rc.stage（否则并发写会互相覆盖），阶段内部的降级/警告
// 必须使用 rc.warnStage/degradeStage 显式指定阶段名。
func (p *Pipeline) execStage(ctx context.Context, cc *domain.CallContext, rc *runCtx, name string, fn stageFunc, announce bool) stageRun {
	if announce {
		rc.stage = name
	}
	start := time.Now()

	if announce {
		p.notifyStage(rc.run.ID, name)
		p.stageEvent(rc, name, "start", fmt.Sprintf("阶段 %s 开始", stageTitle(name)), "info", nil)
		p.audit(domain.AuditEvent{
			TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
			Category: "stage", Action: name, Level: "info",
			Message: fmt.Sprintf("阶段 %s 开始", stageTitle(name)),
			Data:    map[string]any{"phase": "start"},
		})
	}

	err := fn(ctx, cc, rc)
	cost := time.Since(start)

	level, msg := "info", fmt.Sprintf("阶段 %s 完成，耗时 %dms", stageTitle(name), cost.Milliseconds())
	if err != nil {
		level, msg = "error", fmt.Sprintf("阶段 %s 失败：%v", stageTitle(name), err)
	}
	rc.addTimeline(name, msg, level)
	// 结束事件交由调用方发布：并行组需要等全部阶段结束后按声明顺序发布，
	// 否则前端进度条会因完成顺序不同而来回跳变。
	return stageRun{name: name, err: err, cost: cost, level: level, msg: msg}
}

// emitStageEnd 发布阶段结束事件与审计。
func (p *Pipeline) emitStageEnd(rc *runCtx, r stageRun) {
	p.stageEvent(rc, r.name, "end", r.msg, r.level, map[string]any{"durationMs": r.cost.Milliseconds()})
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "stage", Action: r.name, Level: r.level, Message: r.msg,
		Data: map[string]any{"phase": "end", "durationMs": r.cost.Milliseconds()},
	})
}

func (p *Pipeline) stageEvent(rc *runCtx, stage, phase, msg, level string, extra map[string]any) {
	payload := map[string]any{"phase": phase, "stage": stage}
	for k, v := range extra {
		payload[k] = v
	}
	p.publish(domain.Event{
		Type: "stage", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: stage, Level: level, Message: msg, Payload: payload,
	})
}

// stageTitle 返回阶段的中文标题（用于人读消息）。
func stageTitle(stage string) string {
	switch stage {
	case StageResolve:
		return "版本锁定(" + stage + ")"
	case StageStackParse:
		return "堆栈解析(" + stage + ")"
	case StageCandidates:
		return "仓库匹配(" + stage + ")"
	case StageCodeLoad:
		return "代码懒加载(" + stage + ")"
	case StageCrossRepo:
		return "跨仓库链路(" + stage + ")"
	case StageRootCause:
		return "根因推理(" + stage + ")"
	case StagePatchSynthesize:
		return "补丁合成(" + stage + ")"
	case StageSandboxVerify:
		return "沙箱验证(" + stage + ")"
	case StageReport:
		return "报告生成(" + stage + ")"
	case StageArchive:
		return "归档(" + stage + ")"
	default:
		return stage
	}
}

func newEvidenceBundle(run *domain.TaskRun) *domain.EvidenceBundle {
	return &domain.EvidenceBundle{
		RunID:     run.ID,
		TenantID:  run.TenantID,
		CreatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// 阶段 1：resolve —— 任务级代码版本锁定
// ---------------------------------------------------------------------------

func (p *Pipeline) stageResolve(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	p.indexRepos(rc)

	if len(rc.reposByID) == 0 {
		rc.degradeStage(StageResolve, "没有可锁定的目标仓库，无法获取代码证据")
		return nil
	}

	targets := p.resolveTargets(rc)
	if len(targets) == 0 {
		rc.degradeStage(StageResolve, "目标仓库为空，跳过版本锁定")
		return nil
	}
	if p.d.Source == nil {
		rc.degradeStage(StageResolve, "源码解析器不可用，无法锁定代码版本")
		return nil
	}
	if run.PinnedCommits == nil {
		run.PinnedCommits = map[string]string{}
	}

	// 并发锁定：每个仓库的版本解析是彼此独立的 IO（git fetch / rev-parse），
	// 串行执行会让分组模式（多入口仓库）的阶段耗时线性叠加。
	outcomes := make([]resolveOutcome, len(targets))
	var wg sync.WaitGroup
	for i, repo := range targets {
		wg.Add(1)
		go func(idx int, r *domain.Repository) {
			defer wg.Done()
			outcomes[idx] = p.resolveOne(ctx, rc, r)
		}(i, repo)
	}
	wg.Wait()

	// 按 target 声明顺序串行合并：时间线与 Resolution 顺序不受完成先后影响，
	// 保证同一次运行的产物可复现、可对照。
	for i := range outcomes {
		p.applyResolve(rc, outcomes[i])
	}
	return nil
}

// resolveOutcome 单个仓库的版本锁定结果。
//
// 并发阶段只写自己的结果槽位，不触碰 runCtx / run 的共享字段。
type resolveOutcome struct {
	repo   *domain.Repository
	commit string
	// reused 复用已固化 commit（多轮修复），无需再解析。
	reused bool
	res    *domain.RepoResolved
	err    error
}

// resolveOne 解析单个仓库的锁定 commit（并发安全：只读 rc/run）。
func (p *Pipeline) resolveOne(ctx context.Context, rc *runCtx, repo *domain.Repository) resolveOutcome {
	run := rc.run
	if c := run.PinnedCommits[repo.ID]; c != "" {
		// 多轮修复：复用已固化的 commit，保证同一份代码（CONTRACT §6.8）。
		return resolveOutcome{
			repo: repo, commit: c, reused: true,
			res: &domain.RepoResolved{
				RepositoryID: repo.ID, RepoKey: repo.Key, Name: repo.Name, Layer: repo.Layer,
				Commit: c, RequestedRef: run.RequestedRef, ResolvedFrom: resolveFrom(rc, run),
				ResolvedAt: time.Now(),
			},
		}
	}
	ref := p.repoRef(repo, run, rc)
	commit, err := p.d.Source.Resolve(ctx, ref)
	if err != nil {
		return resolveOutcome{repo: repo, err: err}
	}
	if commit == "" {
		return resolveOutcome{repo: repo}
	}
	return resolveOutcome{
		repo: repo, commit: commit,
		res: &domain.RepoResolved{
			RepositoryID: repo.ID, RepoKey: repo.Key, Name: repo.Name, Layer: repo.Layer,
			Commit: commit, RequestedRef: run.RequestedRef,
			ResolvedFrom: resolveFrom(rc, run), ResolvedAt: time.Now(),
		},
	}
}

// applyResolve 把一个仓库的锁定结果并入运行上下文（串行调用）。
func (p *Pipeline) applyResolve(rc *runCtx, o resolveOutcome) {
	run := rc.run
	if o.repo == nil {
		return
	}
	if o.err != nil {
		// 单仓库/入口仓库解析失败：记录警告并降级，后续阶段在无代码证据下继续。
		rc.degradeStage(StageResolve, fmt.Sprintf("仓库 %s 版本锁定失败（%v），本次分析将在无代码证据下继续", o.repo.Key, o.err))
		rc.bundle.Notes = append(rc.bundle.Notes, fmt.Sprintf("仓库 %s 版本锁定失败：%v", o.repo.Key, o.err))
		return
	}
	if o.commit == "" {
		rc.degradeStage(StageResolve, fmt.Sprintf("仓库 %s 未解析出可用 commit，本次分析将在无代码证据下继续", o.repo.Key))
		return
	}
	if o.reused {
		rc.lockedRepos = append(rc.lockedRepos, o.repo)
		rc.addTimeline(StageResolve, fmt.Sprintf("复用已固化版本：%s @ %s", o.repo.Key, shortCommit(o.commit)), "info")
		run.Resolution = appendResolution(run.Resolution, *o.res)
		return
	}

	run.PinnedCommits[o.repo.ID] = o.commit
	rc.lockedRepos = append(rc.lockedRepos, o.repo)
	run.Resolution = appendResolution(run.Resolution, *o.res)

	msg := fmt.Sprintf("仓库 %s 版本锁定成功：%s", o.repo.Key, shortCommit(o.commit))
	rc.addTimeline(StageResolve, msg, "info")
	p.audit(domain.AuditEvent{
		TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Category: "repo", Action: "repo.switch", Level: "info", Message: msg,
		RepoID: o.repo.ID, Commit: o.commit, Data: *o.res,
	})
	p.publish(domain.Event{
		Type: "repo.switch", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Stage: StageResolve, Message: msg, Payload: *o.res,
	})
	p.publish(domain.Event{
		Type: "source.fetch", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Stage: StageResolve, Message: msg,
		Payload: map[string]any{"repositoryId": o.repo.ID, "repoKey": o.repo.Key, "commit": o.commit, "action": "resolve"},
	})
}

// resolveTargets 返回需要在本阶段解析锁定的仓库。
//
// 单仓库模式：run.RepoIDs 中的仓库；分组模式：分组入口仓库（EntryRepositoryIDs，
// 为空时取成员前 2 个），其余成员仓库遵循懒加载原则，此阶段不解析、不加载。
func (p *Pipeline) resolveTargets(rc *runCtx) []*domain.Repository {
	if !rc.isGroup {
		out := make([]*domain.Repository, 0, len(rc.run.RepoIDs))
		for _, id := range rc.run.RepoIDs {
			if r := rc.repoByID(id); r != nil {
				out = append(out, r)
			}
		}
		return out
	}

	rc.group = p.loadGroup(rc)
	entryIDs := p.entryRepoIDs(rc)
	out := make([]*domain.Repository, 0, len(entryIDs))
	for _, id := range entryIDs {
		if r := rc.repoByID(id); r != nil {
			out = append(out, r)
			continue
		}
		if r := rc.repoByKey(id); r != nil {
			out = append(out, r)
		}
	}
	return out
}

// loadGroup 读取分组元数据（失败时返回 nil，由调用方按成员顺序兜底）。
func (p *Pipeline) loadGroup(rc *runCtx) *domain.RepositoryGroup {
	if p.d.Store == nil || rc.run.TaskID == "" {
		return nil
	}
	task, ok := p.d.Store.GetTask(rc.run.TenantID, rc.run.TaskID)
	if !ok || task.GroupID == "" {
		return nil
	}
	g, ok := p.d.Store.GetGroup(rc.run.TenantID, task.GroupID)
	if !ok {
		return nil
	}
	return g
}

// entryRepoIDs 计算分组入口仓库：EntryRepositoryIDs 为空时取成员前 2 个（按 Order 升序）。
func (p *Pipeline) entryRepoIDs(rc *runCtx) []string {
	if rc.group != nil && len(rc.group.EntryRepositoryIDs) > 0 {
		return append([]string{}, rc.group.EntryRepositoryIDs...)
	}
	members := p.groupMembers(rc)
	limit := len(members)
	if limit > 2 {
		limit = 2
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, members[i].RepositoryID)
	}
	if rc.group == nil {
		rc.addTimeline(StageResolve, "分组元数据不可用，按任务成员顺序推断入口仓库", "warn")
	}
	return out
}

// groupMembers 返回分组成员（含仓库详情，按 Order 升序）；分组不可用时退化为 run.RepoIDs。
func (p *Pipeline) groupMembers(rc *runCtx) []domain.GroupMemberView {
	if p.d.Store != nil && rc.group != nil {
		if views := p.d.Store.GroupMemberViews(rc.run.TenantID, rc.group.ID); len(views) > 0 {
			return views
		}
	}
	out := make([]domain.GroupMemberView, 0, len(rc.run.RepoIDs))
	for i, id := range rc.run.RepoIDs {
		v := domain.GroupMemberView{GroupMember: domain.GroupMember{RepositoryID: id, Order: i}}
		if r := rc.repoByID(id); r != nil {
			v.Repo = r
		}
		out = append(out, v)
	}
	return out
}

// indexRepos 建立本次任务范围内的仓库索引（单仓库 / 分组成员）。
func (p *Pipeline) indexRepos(rc *runCtx) {
	run := rc.run
	add := func(r *domain.Repository) {
		if r == nil {
			return
		}
		rc.reposByID[r.ID] = r
		rc.reposByKey[normalizeIdent(r.Key)] = r
		rc.reposByKey[normalizeIdent(r.Name)] = r
	}

	if p.d.Store != nil {
		if rc.isGroup {
			for _, v := range p.groupMembers(rc) {
				add(v.Repo)
			}
			// 分组元数据兜底：直接读 store 保证入口仓库可解析。
			if rc.group == nil {
				rc.group = p.loadGroup(rc)
			}
		}
		for _, id := range run.RepoIDs {
			if _, ok := rc.reposByID[id]; ok {
				continue
			}
			if r, ok := p.d.Store.GetRepo(run.TenantID, id); ok {
				add(r)
			}
		}
	}
	// 索引缺失时构造最小仓库占位，保证锁定流程不中断（例如流水线被直接调用）。
	for _, id := range run.RepoIDs {
		if _, ok := rc.reposByID[id]; ok {
			continue
		}
		if r := rc.repoByKey(id); r != nil {
			continue
		}
		add(&domain.Repository{ID: id, TenantID: run.TenantID, Key: id, Name: id, Layer: domain.LayerUnknown, Status: domain.RepoActive})
	}
}

// repoRef 构造仓库定位引用：优先使用请求 ref，其次仓库默认分支。
func (p *Pipeline) repoRef(repo *domain.Repository, run *domain.TaskRun, rc *runCtx) domain.RepoRef {
	ref := strings.TrimSpace(run.RequestedRef)
	commit := ""
	if ref == "" {
		ref = strings.TrimSpace(repo.DefaultBranch)
	}
	return domain.RepoRef{
		Repository: repo,
		Commit:     commit,
		Ref:        ref,
		Budget:     p.loadBudget(rc),
	}
}

// resolveFrom 返回版本解析来源标记（request | group_entry | repo_default | stack_hint）。
func resolveFrom(rc *runCtx, run *domain.TaskRun) string {
	switch {
	case strings.TrimSpace(run.RequestedRef) != "":
		return "request"
	case rc.isGroup:
		return "group_entry"
	default:
		return "repo_default"
	}
}

func appendResolution(list []domain.RepoResolved, item domain.RepoResolved) []domain.RepoResolved {
	for i := range list {
		if list[i].RepositoryID == item.RepositoryID {
			return list
		}
	}
	return append(list, item)
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// ---------------------------------------------------------------------------
// 阶段 2：stack_parse —— 堆栈/日志解析（宽容反序列化 + 本地兜底）
// ---------------------------------------------------------------------------

func (p *Pipeline) stageStackParse(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	sa := p.parseStacktrace(ctx, cc, rc)
	if sa == nil || (sa.ExceptionType == "" && len(sa.Frames) == 0 && sa.CleanedLog == "") {
		sa = localStackAnalysis(run)
		rc.warnStage(StageStackParse, "堆栈解析技能输出为空，已降级为本地规则解析（仅提取异常类型与日志）")
	}
	if sa.CleanedLog == "" {
		sa.CleanedLog = strings.TrimSpace(run.Stacktrace)
	}
	if sa.Frames == nil {
		sa.Frames = []domain.StackFrame{}
	}
	rc.bundle.Stack = sa

	if run.Severity == "" {
		run.Severity = severityForCategory(sa.Category)
	}
	rc.addTimeline(StageStackParse, fmt.Sprintf("堆栈解析完成：语言=%s，异常=%s，分类=%s，帧数=%d",
		orDefault(sa.Language, "unknown"), orDefault(sa.ExceptionType, "unknown"), orDefault(sa.Category, "unknown"), len(sa.Frames)), "info")
	return nil
}

// parseStacktrace 调用内置技能做堆栈解析，并做宽容反序列化（字段缺失不报错）。
func (p *Pipeline) parseStacktrace(ctx context.Context, cc *domain.CallContext, rc *runCtx) *domain.StackAnalysis {
	input := map[string]any{
		"stacktrace": rc.run.Stacktrace,
		"logs":       rc.run.Logs,
	}
	res, err := p.runSkill(ctx, cc, rc, domain.SkillStacktraceParse, input)
	if err != nil || res == nil || res.Output == nil {
		reason := "技能执行失败"
		if err != nil {
			reason = err.Error()
		}
		rc.warnStage(StageStackParse, "堆栈解析技能不可用（"+reason+"），已启用本地兜底解析")
		return nil
	}
	if res.Status != "" && res.Status != domain.CallOK && res.Status != domain.CallFallback {
		rc.warnStage(StageStackParse, "堆栈解析技能返回状态 "+string(res.Status)+"，已启用本地兜底解析")
		return nil
	}

	out := res.Output
	var sa domain.StackAnalysis
	raw, ok := out["stack"]
	if !ok || raw == nil {
		raw = out
	}
	if b, err := json.Marshal(raw); err == nil {
		// 宽容反序列化：字段缺失/类型不符不报错，仅尽力填充。
		_ = json.Unmarshal(b, &sa)
	}
	if sa.Category == "" {
		sa.Category = pickString(out, "category")
	}
	if sa.Language == "" {
		sa.Language = pickString(out, "language")
	}
	if len(sa.Frames) == 0 {
		sa.Frames = []domain.StackFrame{}
	}
	return &sa
}

// localStackAnalysis 本地兜底堆栈分析：技能不可用时仅做最小信息抽取，保证降级不失败。
func localStackAnalysis(run *domain.TaskRun) *domain.StackAnalysis {
	text := run.Stacktrace
	if strings.TrimSpace(text) == "" {
		text = run.Logs
	}
	sa := &domain.StackAnalysis{
		Language:   guessLanguage(text),
		CleanedLog: strings.TrimSpace(text),
		Frames:     []domain.StackFrame{},
		FileHints:  []string{},
		Symbols:    []string{},
	}
	if line := firstNonEmptyLine(text); line != "" {
		sa.Message = strings.TrimSpace(line)
		fields := strings.Fields(strings.TrimSpace(line))
		for _, f := range fields {
			token := strings.Trim(f, ":\t ")
			if idx := strings.Index(token, "Exception"); idx > 0 || strings.HasSuffix(token, "Error") {
				sa.ExceptionType = strings.TrimSuffix(token, ":")
				break
			}
		}
		if sa.ExceptionType == "" && len(fields) > 0 {
			sa.ExceptionType = strings.TrimSuffix(fields[0], ":")
		}
	}
	sa.Category = guessCategory(text)
	return sa
}

// severityForCategory 依据异常分类推断定级（统一经 domain.NormalizeSeverity 归一化）。
//
// 规则：null_pointer / index_out_of_bounds → major；dependency_missing / timeout → critical；
// unknown → minor；其余默认 major。
func severityForCategory(category string) domain.Severity {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "null_pointer", "index_out_of_bounds", "class_not_found", "version_conflict",
		"logic", "syntax", "config", "compat":
		return domain.NormalizeSeverity("major")
	case "dependency_missing", "timeout", "connection_refused", "resource_exhausted":
		return domain.NormalizeSeverity("critical")
	case "unknown":
		return domain.NormalizeSeverity("minor")
	default:
		return domain.NormalizeSeverity("major")
	}
}

func guessCategory(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "nullpointer"), strings.Contains(t, "nil pointer"),
		strings.Contains(t, "nonetype"), strings.Contains(t, "cannot read propert"):
		return "null_pointer"
	case strings.Contains(t, "indexoutofbounds"), strings.Contains(t, "index out of range"),
		strings.Contains(t, "outofrange"), strings.Contains(t, "indexerror"):
		return "index_out_of_bounds"
	case strings.Contains(t, "classnotfound"), strings.Contains(t, "noclassdeffound"),
		strings.Contains(t, "modulenotfound"), strings.Contains(t, "cannot find module"):
		return "class_not_found"
	case strings.Contains(t, "version conflict"), strings.Contains(t, "dependency convergence"),
		strings.Contains(t, "unsatisfied dependency"), strings.Contains(t, "incompatible version"):
		return "version_conflict"
	case strings.Contains(t, "dependency"), strings.Contains(t, "no such file or directory"),
		strings.Contains(t, "could not resolve dependencies"):
		return "dependency_missing"
	case strings.Contains(t, "timeout"), strings.Contains(t, "timed out"),
		strings.Contains(t, "deadline exceeded"):
		return "timeout"
	default:
		return "unknown"
	}
}

func guessLanguage(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "goroutine ") || strings.Contains(t, "panic:") || strings.Contains(t, ".go:"):
		return "go"
	case strings.Contains(t, "traceback (most recent call last)") || strings.Contains(t, ".py\""):
		return "python"
	case strings.Contains(t, "php fatal error") || strings.Contains(t, "stack trace:"):
		return "php"
	case strings.Contains(t, "at com.") || strings.Contains(t, ".java:") || strings.Contains(t, "caused by:"):
		return "java"
	case strings.Contains(t, "node_modules") || strings.Contains(t, "at object.") || strings.Contains(t, ".ts:"):
		return "typescript"
	default:
		return ""
	}
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 阶段 3：candidates —— 候选仓库匹配打分（不依赖 internal/source）
// ---------------------------------------------------------------------------

func (p *Pipeline) stageCandidates(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	scope := p.candidateScope(rc)
	if !rc.isGroup {
		// 单仓库模式：调用方显式指定的仓库必然进入候选范围。
		if c, ok := p.singleRepoCandidate(rc); ok {
			rc.candidates = []domain.RepoCandidate{c}
			rc.bundle.CandidateRepos = rc.candidates
			run.RepoIDs = []string{c.RepositoryID}
			rc.addTimeline(StageCandidates, fmt.Sprintf("单仓库模式：锁定仓库 %s 作为唯一候选", c.RepoKey), "info")
			return nil
		}
	}

	entrySet := map[string]bool{}
	if rc.isGroup {
		for _, id := range p.entryRepoIDs(rc) {
			entrySet[id] = true
		}
	}

	scored := make([]domain.RepoCandidate, 0, len(scope))
	for _, repo := range scope {
		score, reasons, hints := scoreRepo(repo, run, rc.bundle.Stack, entrySet[repo.ID])
		if score < candidateThreshold {
			continue
		}
		scored = append(scored, domain.RepoCandidate{
			RepositoryID: repo.ID,
			RepoKey:      repo.Key,
			Name:         repo.Name,
			Layer:        repo.Layer,
			Score:        score,
			MatchedBy:    reasons,
			Hints:        hints,
		})
	}

	// 稳定排序：得分降序 → 仓库 ID 升序，保证同一输入的候选顺序完全确定。
	sort.SliceStable(scored, func(i, j int) bool {
		if math.Abs(scored[i].Score-scored[j].Score) > 1e-9 {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].RepositoryID < scored[j].RepositoryID
	})
	if len(scored) > maxCandidates {
		scored = scored[:maxCandidates]
	}
	rc.candidates = scored
	rc.bundle.CandidateRepos = scored

	if len(scored) == 0 {
		// 堆栈线索不足：不加载任何代码，跳过阶段 4/6/7，直接做堆栈文本分析。
		rc.skippedCode = true
		rc.noCodeEvidence = true
		rc.degrade("堆栈线索不足，降级为堆栈文本分析")
		rc.bundle.Notes = append(rc.bundle.Notes, "未匹配到任何候选仓库，本次报告仅基于堆栈文本，无代码证据。")
		return nil
	}

	for _, c := range scored {
		rc.addTimeline(StageCandidates, fmt.Sprintf("候选仓库 %s（得分 %.2f）：%s", c.RepoKey, c.Score, strings.Join(c.MatchedBy, "；")), "info")
	}
	return nil
}

// candidateScope 返回候选仓库范围：单仓库 = 锁定仓库；分组 = 分组成员全体（只读索引）。
func (p *Pipeline) candidateScope(rc *runCtx) []*domain.Repository {
	out := []*domain.Repository{}
	seen := map[string]bool{}
	push := func(r *domain.Repository) {
		if r == nil || seen[r.ID] {
			return
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	if rc.isGroup {
		for _, v := range p.groupMembers(rc) {
			push(v.Repo)
		}
	}
	for _, id := range rc.run.RepoIDs {
		push(rc.repoByID(id))
	}
	return out
}

// singleRepoCandidate 单仓库模式候选：仓库必然在范围内，得分不低于 0.5。
func (p *Pipeline) singleRepoCandidate(rc *runCtx) (domain.RepoCandidate, bool) {
	if len(rc.run.RepoIDs) == 0 {
		return domain.RepoCandidate{}, false
	}
	repo := rc.repoByID(rc.run.RepoIDs[0])
	if repo == nil {
		return domain.RepoCandidate{}, false
	}
	score, reasons, hints := scoreRepo(repo, rc.run, rc.bundle.Stack, false)
	reasons = append([]string{"单仓库模式：调用方显式指定仓库"}, reasons...)
	if score < 0.5 {
		score = 0.5
	}
	return domain.RepoCandidate{
		RepositoryID: repo.ID, RepoKey: repo.Key, Name: repo.Name, Layer: repo.Layer,
		Score: score, MatchedBy: reasons, Hints: hints,
	}, true
}

// scoreRepo 依据 CONTRACT §6 的匹配规则给仓库打分，返回归一化得分（0-1）、中文命中原因与文件线索。
//
// 权重：PathPrefixes 0.45、PackagePrefixes 0.4、类名↔仓库 Key 相似 0.35、
// ArtifactNames 0.3、EntryFiles 命中 0.3、HostPatterns 0.25、EndpointPatterns 0.2、
// 分组入口加成 0.15、Keywords 0.1、分层/语言加成 0.05。
func scoreRepo(repo *domain.Repository, run *domain.TaskRun, sa *domain.StackAnalysis, isGroupEntry bool) (float64, []string, []domain.FileHint) {
	if repo == nil {
		return 0, nil, nil
	}
	if sa == nil {
		sa = &domain.StackAnalysis{Frames: []domain.StackFrame{}}
	}
	rules := repo.MatchRules
	text := strings.ToLower(run.Stacktrace + "\n" + run.Logs)
	logText := strings.ToLower(sa.CleanedLog)

	score := 0.0
	seen := map[string]bool{}
	reasons := make([]string, 0, 6)
	// substantive 标记是否命中"实质规则"（路径/包名/类名/制品/嫌疑文件/主机/接口/关键词）。
	// 仅有加成项（分层、语言、分组入口）不足以让仓库成为候选，否则阈值的语义会被稀释。
	substantive := false
	add := func(weight float64, reason string) {
		if seen[reason] {
			return
		}
		seen[reason] = true
		substantive = true
		score += weight
		reasons = append(reasons, reason)
	}
	bonus := func(weight float64, reason string) {
		if seen[reason] {
			return
		}
		seen[reason] = true
		score += weight
		reasons = append(reasons, reason)
	}

	// 1) 路径前缀（+0.45）
	for _, pp := range rules.PathPrefixes {
		pp = strings.TrimSpace(pp)
		if pp == "" {
			continue
		}
		if hitPathPrefix(pp, sa, logText) {
			add(0.45, "路径前缀命中："+pp)
			break
		}
	}

	// 2) 包名前缀（+0.4，注意 `/` ↔ `.` 归一化）
	for _, pp := range rules.PackagePrefixes {
		pfx := normalizePkg(pp)
		if pfx == "" {
			continue
		}
		if hitPackagePrefix(pfx, sa, logText) {
			add(0.4, "包名前缀命中："+pp)
			break
		}
	}

	// 3) 类名/符号与仓库 Key 归一化相似（+0.35，如 order-service ↔ OrderService.java）
	keyNorm, nameNorm := normalizeIdent(repo.Key), normalizeIdent(repo.Name)
	for _, sym := range symbolCandidates(sa) {
		sn := identBase(normalizeIdent(sym))
		if identSimilar(sn, keyNorm) || identSimilar(sn, nameNorm) {
			add(0.35, "类名与仓库 Key 相似："+sym+" ↔ "+repo.Key)
			break
		}
	}

	// 4) 制品名（+0.3）
	for _, a := range rules.ArtifactNames {
		al := strings.ToLower(strings.TrimSpace(a))
		if al == "" {
			continue
		}
		base := artifactBase(al)
		if strings.Contains(text, al) || (base != "" && strings.Contains(text, base)) {
			add(0.3, "制品名命中："+a)
			break
		}
	}

	// 5) 调用方指定的嫌疑文件（+0.3）
	for _, ef := range run.EntryFiles {
		if entryFileHitsRepo(ef, repo) {
			add(0.3, "调用方嫌疑文件命中："+ef)
			break
		}
	}

	// 6) 主机名/域名（+0.25）
	for _, h := range rules.HostPatterns {
		hl := strings.ToLower(strings.TrimSpace(h))
		if hl == "" {
			continue
		}
		if strings.Contains(text, hl) {
			add(0.25, "主机名命中："+h)
			break
		}
	}

	// 7) 接口路径（+0.2，支持 ** 通配）
	for _, ep := range rules.EndpointPatterns {
		if matchEndpoint(ep, sa, text) {
			add(0.2, "接口路径命中："+ep)
			break
		}
	}

	// 8) 分组入口加成（+0.15，仅在已命中实质规则时生效）
	if isGroupEntry && substantive {
		bonus(0.15, "分组入口仓库加权")
	}

	// 9) 关键词兜底（+0.1）
	for _, kw := range rules.Keywords {
		kl := strings.ToLower(strings.TrimSpace(kw))
		if kl == "" {
			continue
		}
		if strings.Contains(text, kl) {
			add(0.1, "关键词命中："+kw)
			break
		}
	}

	// 10) 分层 / 语言加成（各 +0.05，仅在已命中实质规则时生效）
	if substantive && repo.Layer != "" && repo.Layer != domain.LayerUnknown {
		bonus(0.05, "已登记分层："+string(repo.Layer))
	}
	if substantive && repo.Language != "" && sa.Language != "" && strings.EqualFold(repo.Language, sa.Language) {
		bonus(0.05, "语言与堆栈一致："+repo.Language)
	}

	if !substantive {
		// 只有加成项命中（或无任何命中）：不构成候选，避免阈值为 0.15 时被加成"刷分"。
		return 0, nil, nil
	}
	return math.Min(score, 1.0), reasons, buildHints(repo, run, sa)
}

func framesText(sa *domain.StackAnalysis) string {
	if sa == nil {
		return ""
	}
	var b strings.Builder
	for _, f := range sa.Frames {
		b.WriteString(f.Raw)
		b.WriteString("\n")
		b.WriteString(f.File)
		b.WriteString("\n")
	}
	return b.String()
}

func hitPathPrefix(prefix string, sa *domain.StackAnalysis, logText string) bool {
	p := normalizePath(prefix)
	if p == "" {
		return false
	}
	for _, f := range sa.Frames {
		if fp := normalizePath(f.File); fp != "" && strings.Contains(fp, p) {
			return true
		}
	}
	return strings.Contains(normalizePath(logText), p)
}

func hitPackagePrefix(pfx string, sa *domain.StackAnalysis, logText string) bool {
	for _, f := range sa.Frames {
		fp := normalizePkg(f.Package)
		if fp != "" && packageMatch(fp, pfx) {
			return true
		}
		if fromFile := pkgFromPath(f.File); fromFile != "" && packageMatch(fromFile, pfx) {
			return true
		}
	}
	if strings.Contains(normalizePkg(logText), pfx) {
		return true
	}
	for _, h := range sa.FileHints {
		if strings.Contains(normalizePkg(h), pfx) {
			return true
		}
	}
	return false
}

func packageMatch(framePkg, prefix string) bool {
	if framePkg == "" || prefix == "" {
		return false
	}
	return framePkg == prefix || strings.HasPrefix(framePkg, prefix+".") || strings.HasPrefix(prefix, framePkg+".")
}

func symbolCandidates(sa *domain.StackAnalysis) []string {
	out := make([]string, 0, len(sa.Symbols)+len(sa.Frames))
	out = append(out, sa.Symbols...)
	for _, f := range sa.Frames {
		if f.Class != "" {
			out = append(out, f.Class)
		}
		if f.Method != "" && f.Class != "" {
			out = append(out, f.Class+"."+f.Method)
		}
	}
	return out
}

// entryFileHitsRepo 判断调用方给出的嫌疑文件是否属于该仓库：
// 路径包含仓库 Key，或命中仓库登记的路径前缀（不做跨仓库的文件名兜底匹配，
// 否则同名文件会把无关仓库误判为候选）。
func entryFileHitsRepo(entryFile string, repo *domain.Repository) bool {
	ef := normalizePath(entryFile)
	if ef == "" || repo == nil {
		return false
	}
	if key := normalizePath(repo.Key); key != "" && strings.Contains(ef, key) {
		return true
	}
	for _, pp := range repo.MatchRules.PathPrefixes {
		p := normalizePath(pp)
		if p != "" && strings.Contains(ef, p) {
			return true
		}
	}
	return false
}

// matchEndpoint 接口路径匹配：支持 `**`、`*` 通配与 {id} 路径变量。
func matchEndpoint(pattern string, sa *domain.StackAnalysis, text string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	for _, ep := range sa.Endpoints {
		if wildcardMatch(p, strings.ToLower(strings.TrimSpace(ep))) {
			return true
		}
	}
	literal := strings.ReplaceAll(strings.ReplaceAll(p, "**", ""), "*", "")
	if len(literal) >= 6 && strings.Contains(text, literal) {
		return true
	}
	for _, f := range sa.Frames {
		if wildcardMatch(p, strings.ToLower(f.Raw)) {
			return true
		}
	}
	return false
}

// wildcardMatch 极简通配匹配：`**` 匹配任意字符（含分隔符），`*` 匹配任意字符。
func wildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*{") {
		return strings.Contains(s, pattern)
	}
	// 将 ** 视作 .*，* 视作 .*，{x} 视作 .*，逐段匹配。
	parts := strings.FieldsFunc(pattern, func(r rune) bool { return r == '*' || r == '{' || r == '}' })
	if len(parts) == 0 {
		return true
	}
	idx := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		pos := strings.Index(s[idx:], part)
		if pos < 0 {
			return false
		}
		if i == 0 && !strings.HasPrefix(pattern, "*") && !strings.HasPrefix(pattern, "{") && pos != 0 {
			return false
		}
		idx += pos + len(part)
	}
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(pattern, "}") {
		return strings.HasSuffix(s, last)
	}
	return true
}

// buildHints 依据堆栈帧、包名、符号、制品名与调用方嫌疑文件生成文件加载线索。
func buildHints(repo *domain.Repository, run *domain.TaskRun, sa *domain.StackAnalysis) []domain.FileHint {
	hints := make([]domain.FileHint, 0, maxHintsPerRepo)
	seen := map[string]bool{}
	push := func(h domain.FileHint) {
		if h.Path == "" || seen[h.Reason+"|"+h.Path] {
			return
		}
		seen[h.Reason+"|"+h.Path] = true
		hints = append(hints, h)
	}

	for _, f := range sa.Frames {
		if f.File != "" && strings.ContainsAny(f.File, "/\\") {
			push(domain.FileHint{Path: f.File, Reason: "frame", Line: f.Line, Priority: 0.95})
		}
		if f.Package == "" {
			continue
		}
		pkgPath := strings.ReplaceAll(f.Package, ".", "/")
		class := identBase(f.Class)
		if class != "" {
			push(domain.FileHint{Path: pkgPath + "/" + class + ".java", Reason: "frame", Line: f.Line, Priority: 0.7})
			push(domain.FileHint{Path: class + ".java", Reason: "symbol", Line: f.Line, Priority: 0.5})
		}
		push(domain.FileHint{Path: pkgPath + "/", Reason: "package_prefix", Priority: 0.6})
	}
	for _, h := range sa.FileHints {
		push(domain.FileHint{Path: h, Reason: "package_prefix", Priority: 0.55})
	}
	for _, a := range repo.MatchRules.ArtifactNames {
		push(domain.FileHint{Path: strings.TrimSpace(a), Reason: "artifact", Priority: 0.35})
	}
	for _, ef := range run.EntryFiles {
		if entryFileHitsRepo(ef, repo) {
			push(domain.FileHint{Path: ef, Reason: "entry", Priority: 0.85})
		}
	}

	sort.SliceStable(hints, func(i, j int) bool {
		if math.Abs(hints[i].Priority-hints[j].Priority) > 1e-9 {
			return hints[i].Priority > hints[j].Priority
		}
		return hints[i].Path < hints[j].Path
	})
	if len(hints) > maxHintsPerRepo {
		hints = hints[:maxHintsPerRepo]
	}
	return hints
}

// ---------------------------------------------------------------------------
// 阶段 4：code_load —— 堆栈驱动懒加载 + 配额
// ---------------------------------------------------------------------------

func (p *Pipeline) stageCodeLoad(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	if len(rc.candidates) == 0 {
		rc.noCodeEvidence = true
		rc.warn("没有候选仓库，跳过代码加载")
		return nil
	}
	if p.d.Source == nil {
		rc.noCodeEvidence = true
		rc.degrade("源码解析器不可用，无法加载代码切片")
		return nil
	}

	budget := p.loadBudget(rc)
	filesLeft := budget.MaxFiles - run.Usage.FilesLoaded
	charsLeft := budget.MaxChars - run.Usage.CodeChars
	truncated := false

	for i := range rc.candidates {
		c := &rc.candidates[i]
		if filesLeft <= 0 || charsLeft <= 0 {
			truncated = true
			break
		}
		repo := rc.repoByID(c.RepositoryID)
		if repo == nil {
			rc.warn("候选仓库 " + c.RepoKey + " 元数据缺失，跳过代码加载")
			continue
		}
		commit := run.PinnedCommits[repo.ID]
		if commit == "" {
			// 分组模式下非入口仓库：命中堆栈后才做懒加载与版本锁定。
			commit = p.resolveLazily(ctx, cc, rc, repo)
		}
		if commit == "" {
			rc.warn(fmt.Sprintf("候选仓库 %s 无法锁定版本，跳过代码加载", repo.Key))
			continue
		}

		ref := domain.RepoRef{
			Repository: repo,
			Commit:     commit,
			Ref:        run.RequestedRef,
			Budget: domain.LoadBudget{
				MaxFiles:    filesLeft,
				MaxChars:    charsLeft,
				MaxFileSize: maxFileSizeBytes,
				MaxDepth:    budget.MaxDepth,
			},
		}
		slices, err := p.d.Source.Load(ctx, ref, c.Hints)
		if err != nil {
			rc.warn(fmt.Sprintf("仓库 %s 代码加载失败（%v），已跳过该仓库", repo.Key, err))
			continue
		}

		loaded := 0
		chars := 0
		for _, s := range slices {
			if s.Chars <= 0 {
				s.Chars = len(s.Content)
			}
			if filesLeft <= 0 || charsLeft <= 0 || s.Chars > charsLeft {
				truncated = true
				break
			}
			if s.RepositoryID == "" {
				s.RepositoryID = repo.ID
			}
			if s.RepoKey == "" {
				s.RepoKey = repo.Key
			}
			if s.Commit == "" {
				s.Commit = commit
			}
			filesLeft--
			charsLeft -= s.Chars
			loaded++
			chars += s.Chars
			run.Usage.FilesLoaded++
			run.Usage.CodeChars += s.Chars
			if s.FromCache {
				run.Usage.CacheHits++
			}
			rc.bundle.Slices = append(rc.bundle.Slices, s)
			if isDependencyFile(s.Path) {
				rc.bundle.DependencyFiles = append(rc.bundle.DependencyFiles, s)
			}
		}

		msg := fmt.Sprintf("仓库 %s 加载 %d 个关联文件（%d 字符）", repo.Key, loaded, chars)
		rc.addTimeline(StageCodeLoad, msg, "info")
		p.audit(domain.AuditEvent{
			TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Category: "source", Action: "source.fetch", Level: "info", Message: msg,
			RepoID: repo.ID, Commit: commit,
			Data: map[string]any{"files": loaded, "chars": chars},
		})
		p.publish(domain.Event{
			Type: "source.fetch", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Stage: StageCodeLoad, Message: msg,
			Payload: map[string]any{"repositoryId": repo.ID, "repoKey": repo.Key, "commit": commit,
				"files": loaded, "chars": chars, "action": "load"},
		})
	}

	rc.bundle.FilesLoaded = run.Usage.FilesLoaded
	rc.bundle.CodeChars = run.Usage.CodeChars

	if truncated {
		rc.bundle.Truncated = true
		note := fmt.Sprintf("代码加载触达配额上限（文件 %d / 字符 %d），已停止加载并裁剪上下文", budget.MaxFiles, budget.MaxChars)
		rc.bundle.Notes = append(rc.bundle.Notes, note)
		rc.warn(note)
	}
	if len(rc.bundle.Slices) == 0 {
		rc.noCodeEvidence = true
		rc.degrade("未加载到任何代码切片，降级为堆栈文本分析")
		rc.bundle.Notes = append(rc.bundle.Notes, "未加载到任何代码切片：候选仓库无命中文件或源码不可用。")
		return nil
	}

	// AST 补充分析（失败不阻断）。
	p.enrichWithAST(ctx, cc, rc)

	if len(rc.bundle.DependencyFiles) > 0 {
		rc.addTimeline(StageCodeLoad, fmt.Sprintf("识别到 %d 个依赖清单文件", len(rc.bundle.DependencyFiles)), "info")
	}
	return nil
}

// resolveLazily 分组模式下按堆栈命中结果对非入口仓库做懒加载前的版本锁定。
func (p *Pipeline) resolveLazily(ctx context.Context, cc *domain.CallContext, rc *runCtx, repo *domain.Repository) string {
	if p.d.Source == nil {
		return ""
	}
	ref := domain.RepoRef{Repository: repo, Ref: strings.TrimSpace(rc.run.RequestedRef), Budget: p.loadBudget(rc)}
	if ref.Ref == "" {
		ref.Ref = repo.DefaultBranch
	}
	commit, err := p.d.Source.Resolve(ctx, ref)
	if err != nil || commit == "" {
		rc.warn(fmt.Sprintf("候选仓库 %s 懒加载版本锁定失败（%v）", repo.Key, err))
		return ""
	}
	if rc.run.PinnedCommits == nil {
		rc.run.PinnedCommits = map[string]string{}
	}
	rc.run.PinnedCommits[repo.ID] = commit
	rc.run.Resolution = appendResolution(rc.run.Resolution, domain.RepoResolved{
		RepositoryID: repo.ID, RepoKey: repo.Key, Name: repo.Name, Layer: repo.Layer,
		Commit: commit, RequestedRef: rc.run.RequestedRef, ResolvedFrom: "stack_hint",
		MatchScore: candidateScore(rc, repo.ID), ResolvedAt: time.Now(),
	})
	rc.lockedRepos = append(rc.lockedRepos, repo)
	_ = cc
	msg := fmt.Sprintf("仓库 %s 依据堆栈线索锁定版本：%s", repo.Key, shortCommit(commit))
	rc.addTimeline(StageCodeLoad, msg, "info")
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "repo", Action: "repo.switch", Level: "info", Message: msg, RepoID: repo.ID, Commit: commit,
	})
	p.publish(domain.Event{
		Type: "repo.switch", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: StageCodeLoad, Message: msg,
		Payload: map[string]any{"repositoryId": repo.ID, "repoKey": repo.Key, "commit": commit, "resolvedFrom": "stack_hint"},
	})
	return commit
}

// enrichWithAST 调用 code_ast_analyze 技能补充符号与风险信息（失败不阻断）。
func (p *Pipeline) enrichWithAST(ctx context.Context, cc *domain.CallContext, rc *runCtx) {
	slices := p.slicesForSkill(rc)
	focus := focusLines(rc.bundle.Slices)
	res, err := p.runSkill(ctx, cc, rc, domain.SkillCodeASTAnalyze, map[string]any{
		"slices":     slices,
		"focusLines": focus,
	})
	if err != nil || res == nil || res.Output == nil {
		rc.warn("代码 AST 分析技能不可用，已跳过符号补充（不影响主流程）")
		return
	}
	p.mergeASTOutput(rc, res.Output)
}

// mergeASTOutput 把 AST 技能输出合并回代码切片（宽容解析）。
func (p *Pipeline) mergeASTOutput(rc *runCtx, out map[string]any) {
	if syms, ok := out["symbols"].([]any); ok {
		for _, item := range syms {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			file := pickString(m, "path", "file", "filePath")
			sym := pickString(m, "symbol", "name")
			for i := range rc.bundle.Slices {
				if file == "" || strings.HasSuffix(rc.bundle.Slices[i].Path, file) {
					if sym != "" {
						rc.bundle.Slices[i].Symbols = appendUnique(rc.bundle.Slices[i].Symbols, sym)
					}
				}
			}
		}
	}
	if imports, ok := out["imports"].([]any); ok {
		for _, item := range imports {
			switch v := item.(type) {
			case string:
				for i := range rc.bundle.Slices {
					rc.bundle.Slices[i].Imports = appendUnique(rc.bundle.Slices[i].Imports, v)
				}
			case map[string]any:
				file := pickString(v, "path", "file")
				imp := pickString(v, "import", "name", "package")
				for i := range rc.bundle.Slices {
					if imp != "" && (file == "" || strings.HasSuffix(rc.bundle.Slices[i].Path, file)) {
						rc.bundle.Slices[i].Imports = appendUnique(rc.bundle.Slices[i].Imports, imp)
					}
				}
			}
		}
	}
	if risks, ok := out["risks"].([]any); ok && len(risks) > 0 {
		notes := make([]string, 0, len(risks))
		for _, r := range risks {
			switch v := r.(type) {
			case string:
				notes = append(notes, v)
			case map[string]any:
				if s := pickString(v, "message", "detail", "reason", "risk"); s != "" {
					notes = append(notes, s)
				}
			}
		}
		if len(notes) > 0 {
			rc.bundle.Notes = append(rc.bundle.Notes, "AST 风险提示："+strings.Join(notes, "；"))
		}
	}
}

func candidateScore(rc *runCtx, repoID string) float64 {
	for _, c := range rc.candidates {
		if c.RepositoryID == repoID {
			return c.Score
		}
	}
	return 0
}

// isDependencyFile 判断是否为依赖清单文件。
func isDependencyFile(p string) bool {
	switch strings.ToLower(path.Base(p)) {
	case "pom.xml", "build.gradle", "build.gradle.kts", "package.json", "requirements.txt", "go.mod", "cargo.toml":
		return true
	default:
		return false
	}
}

// loadBudget 计算加载预算：租户配额优先（MaxFilesPerTask/MaxCodeChars），否则引擎层默认。
func (p *Pipeline) loadBudget(rc *runCtx) domain.LoadBudget {
	b := domain.LoadBudget{
		MaxFiles:    defaultMaxFiles,
		MaxChars:    defaultMaxCodeChars,
		MaxFileSize: maxFileSizeBytes,
		MaxDepth:    defaultMaxDepth,
	}
	if p.d.Store == nil || rc == nil || rc.run == nil {
		return b
	}
	if t, ok := p.d.Store.GetTenant(rc.run.TenantID); ok {
		if t.Quota.MaxFilesPerTask > 0 {
			b.MaxFiles = t.Quota.MaxFilesPerTask
		}
		if t.Quota.MaxCodeChars > 0 {
			b.MaxChars = t.Quota.MaxCodeChars
		}
	}
	return b
}

// contextBudget 把 Cfg.MCP.Budget 映射为 ContextPacker 预算。
func (p *Pipeline) contextBudget() domain.ContextBudget {
	cb := domain.ContextBudget{
		MaxChars:         120_000,
		MaxSlices:        24,
		MaxSliceChars:    12_000,
		MaxLogChars:      20_000,
		ReserveForOutput: 8_000,
	}
	if p.d.Cfg == nil {
		return cb
	}
	src := p.d.Cfg.MCP.Budget
	if src.MaxChars > 0 {
		cb.MaxChars = src.MaxChars
	}
	if src.MaxSlices > 0 {
		cb.MaxSlices = src.MaxSlices
	}
	if src.MaxSliceChars > 0 {
		cb.MaxSliceChars = src.MaxSliceChars
	}
	if src.MaxLogChars > 0 {
		cb.MaxLogChars = src.MaxLogChars
	}
	if src.ReserveForOutput > 0 {
		cb.ReserveForOutput = src.ReserveForOutput
	}
	return cb
}

// slicesForSkill 序列化代码切片供技能使用（超长内容按单切片预算截断）。
func (p *Pipeline) slicesForSkill(rc *runCtx) []any {
	maxChars := p.contextBudget().MaxSliceChars
	out := make([]any, 0, len(rc.bundle.Slices))
	for _, s := range rc.bundle.Slices {
		cp := s
		if maxChars > 0 && len(cp.Content) > maxChars {
			cp.Content = cp.Content[:maxChars] + "\n/* ... 已按上下文预算截断 ... */"
			cp.Truncated = true
		}
		b, err := json.Marshal(cp)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func focusLines(slices []domain.CodeSlice) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, s := range slices {
		for _, l := range s.FocusLines {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	sort.Ints(out)
	return out
}

// ---------------------------------------------------------------------------
// 阶段 5：root_cause —— MCP 强约束推理
// ---------------------------------------------------------------------------

// RootCauseSchema 根因推理的结构化输出约束（字段与 CONTRACT §4 完全一致）。
//
// 说明：MCP 层的等价 Schema 常量定义在 internal/mcp 包（并行开发中）。
// 为避免 engine 反向依赖 mcp，这里在本包内联同一份 Schema；两处字段必须保持一致，
// 由 CONTRACT §4 的字段清单兜底对齐。
//
// category 取值使用**细粒度口径**（与 domain 的技能常量、堆栈解析分类、Mock 推理器一致）：
// null_pointer / index_out_of_bounds / type_error / class_not_found / dependency_missing /
// version_conflict / connection_refused / timeout / divide_by_zero / concurrent_modification /
// serialization_error / resource_leak / syntax_error / oom，另保留 syntax / runtime / logic /
// dependency / compat / config 作为粗粒度兜底，unknown 为未知分类。
var RootCauseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"summary": map[string]any{"type": "string"},
		"category": map[string]any{"type": "string", "enum": []any{
			"null_pointer", "index_out_of_bounds", "type_error", "class_not_found",
			"dependency_missing", "version_conflict", "connection_refused", "timeout",
			"divide_by_zero", "concurrent_modification", "serialization_error",
			"resource_leak", "syntax_error", "oom",
			"syntax", "runtime", "logic", "dependency", "compat", "config", "unknown",
		}},
		"detail": map[string]any{"type": "string"},
		"confidence": map[string]any{
			"type": "number",
		},
		"evidence":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"blastRadius": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"severity":    map[string]any{"type": "string", "enum": []any{"blocker", "critical", "major", "minor", "info"}},
	},
	"required":             []any{"summary", "category", "detail", "confidence", "evidence", "blastRadius", "severity"},
	"additionalProperties": true,
}

func (p *Pipeline) stageRootCause(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	// 跨仓库链路（分组模式且候选 ≥2）：作为 root_cause 的子阶段，命名沿用 CONTRACT §3。
	if rc.isGroup && len(rc.bundle.CandidateRepos) >= 2 {
		p.stageCrossRepo(ctx, cc, rc)
	}

	packedText, err := p.packContext(rc)
	if err != nil {
		rc.warn("上下文组装失败（" + err.Error() + "），改用精简上下文继续推理")
		packedText = fallbackContext(rc.bundle, rc.run)
	}

	rcx := p.d.MCP
	if rcx == nil {
		p.fallbackRootCause(rc, "模型管控层不可用")
		return nil
	}

	msgs := buildRootCauseMessages(packedText, rc)
	res, err := rcx.Chat(ctx, cc.Child(StageRootCause), domain.ModelRequest{
		Stage:       StageRootCause,
		Tier:        domain.TierStrong,
		Messages:    msgs,
		Schema:      RootCauseSchema,
		Temperature: 0.1,
	})
	if err != nil || res == nil {
		reason := "模型返回空结果"
		if err != nil {
			reason = err.Error()
		}
		p.fallbackRootCause(rc, reason)
		return nil
	}

	rc.run.Usage.ModelCalls++
	rc.run.Usage.PromptTokens += res.Usage.PromptTokens
	rc.run.Usage.CompletionTokens += res.Usage.CompletionTokens
	rc.run.Usage.TotalTokens += res.Usage.TotalTokens
	if res.FallbackFrom != "" {
		rc.run.Usage.ModelFallbacks++
	}
	p.publishModelCall(rc, res)
	p.checkTokenQuota(rc)

	root := parseRootCause(res, rc)
	rc.run.RootCause = root
	rc.run.Severity = domain.NormalizeSeverity(string(root.Severity))
	rc.addTimeline(StageRootCause, fmt.Sprintf("根因推理完成：[%s] %s（置信度 %.2f）", root.Category, root.Summary, root.Confidence), "info")
	return nil
}

// publishModelCall 发布模型调用实时事件（审计由 MCP 层负责，避免重复落审计）。
func (p *Pipeline) publishModelCall(rc *runCtx, res *domain.ModelResponse) {
	level := "info"
	status := domain.CallOK
	if res.FallbackFrom != "" {
		status = domain.CallFallback
		level = "warn"
	}
	p.publish(domain.Event{
		Type: "model.call", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: StageRootCause, Level: level,
		Message: fmt.Sprintf("模型推理完成：%s/%s（%d tokens）", res.Target.Provider, res.Target.Model, res.Usage.TotalTokens),
		Payload: domain.ModelCall{
			ID: uuid.NewString(), RunID: rc.run.ID, TenantID: rc.run.TenantID,
			Stage: StageRootCause, Provider: res.Target.Provider, Model: res.Target.Model,
			Tier: res.Target.Tier, Status: status, Attempt: res.Attempts,
			FallbackFrom: res.FallbackFrom,
			PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens,
			TotalTokens: res.Usage.TotalTokens, DurationMS: res.DurationMS,
			SchemaValid: res.Structured != nil, StartedAt: time.Now(),
		},
	})
}

// checkTokenQuota Token 配额管控：超限时降级记录，不中断任务。
func (p *Pipeline) checkTokenQuota(rc *runCtx) {
	if p.d.Store == nil {
		return
	}
	t, ok := p.d.Store.GetTenant(rc.run.TenantID)
	if !ok || t.Quota.MaxTokensPerTask <= 0 {
		return
	}
	if rc.run.Usage.TotalTokens > t.Quota.MaxTokensPerTask {
		note := fmt.Sprintf("模型 Token 消耗（%d）已超出租户单任务配额（%d），后续推理将受管控",
			rc.run.Usage.TotalTokens, t.Quota.MaxTokensPerTask)
		rc.bundle.Notes = append(rc.bundle.Notes, note)
		rc.warn(note)
	}
}

// packContext 组装模型上下文；Packed 结果会写入 EvidenceBundle.Notes 供报告说明。
func (p *Pipeline) packContext(rc *runCtx) (string, error) {
	if p.d.Packer == nil {
		return fallbackContext(rc.bundle, rc.run), nil
	}
	packed, err := p.d.Packer.Pack(rc.bundle, p.contextBudget())
	if err != nil {
		return "", err
	}
	if packed.Truncated {
		rc.bundle.Truncated = true
		note := "上下文超预算已裁剪：" + strings.Join(packed.Notes, "；")
		rc.bundle.Notes = append(rc.bundle.Notes, note)
		rc.warn(note)
	}
	if packed.Text == "" {
		return fallbackContext(rc.bundle, rc.run), nil
	}
	return packed.Text, nil
}

// fallbackContext 本地兜底上下文：Packer 不可用时按预算拼装精简证据文本。
func fallbackContext(b *domain.EvidenceBundle, run *domain.TaskRun) string {
	var sb strings.Builder
	sb.WriteString("### 异常堆栈\n")
	sb.WriteString(truncate(run.Stacktrace, 4000))
	sb.WriteString("\n### 代码证据\n")
	for _, s := range b.Slices {
		sb.WriteString(fmt.Sprintf("- %s:%d-%d（仓库 %s）\n", s.Path, s.StartLine, s.EndLine, s.RepoKey))
		sb.WriteString(truncate(s.Content, 1200))
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildRootCauseMessages 构造根因推理消息。
//
// 取舍说明：CONTRACT §1 规定 BuildMessages/Prompt 模板位于 internal/mcp（并行开发中），
// 而 engine 不得反向 import mcp，故此处在本包内联等价 prompt 模板；
// system 中固定包含 [STAGE:root_cause] 标记与"只输出 JSON"约束，与 MCP 层约定一致。
func buildRootCauseMessages(contextText string, rc *runCtx) []domain.ModelMessage {
	run := rc.run
	sa := rc.bundle.Stack

	var sys strings.Builder
	sys.WriteString("你是资深线上故障根因分析专家，服务于企业级多仓库代码分析平台。\n")
	sys.WriteString("[STAGE:root_cause]\n")
	sys.WriteString("硬性约束：\n")
	sys.WriteString("1. 只输出 JSON，禁止输出任何解释性文字、Markdown 代码块或多余空行。\n")
	sys.WriteString("2. 必须严格满足给定 JSON Schema，字段 summary/category/detail/confidence/evidence/blastRadius/severity 缺一不可。\n")
	sys.WriteString("3. 结论必须基于下方堆栈与代码证据，禁止臆造不存在的文件、行号、调用链。\n")
	sys.WriteString("4. 证据不足时必须降低 confidence，并在 detail 中明确说明证据缺口。\n")

	var user strings.Builder
	user.WriteString("## 任务信息\n")
	user.WriteString(fmt.Sprintf("- 模式：%s；环境：%s；语言：%s\n", run.Mode, orDefault(run.Environment, "unknown"), orDefault(stackLanguage(sa), "unknown")))
	user.WriteString(fmt.Sprintf("- 任务标题：%s\n", orDefault(run.Title, run.ID)))
	if len(run.PinnedCommits) > 0 {
		keys := make([]string, 0, len(run.PinnedCommits))
		for k := range run.PinnedCommits {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		user.WriteString("- 已锁定版本：\n")
		for _, k := range keys {
			user.WriteString(fmt.Sprintf("  - %s @ %s\n", k, shortCommit(run.PinnedCommits[k])))
		}
	}

	user.WriteString("\n## 堆栈分析\n")
	if sa != nil {
		user.WriteString(fmt.Sprintf("- 异常类型：%s\n- 异常消息：%s\n- 分类：%s\n",
			orDefault(sa.ExceptionType, "unknown"), orDefault(sa.Message, "unknown"), orDefault(sa.Category, "unknown")))
		if len(sa.CausedBy) > 0 {
			user.WriteString("- Caused by：" + strings.Join(sa.CausedBy, " | ") + "\n")
		}
		frames := sa.Frames
		if len(frames) > 12 {
			frames = frames[:12]
		}
		if len(frames) > 0 {
			user.WriteString("- 关键帧：\n")
			for _, f := range frames {
				user.WriteString("  - " + strings.TrimSpace(f.Raw) + "\n")
			}
		}
	} else {
		user.WriteString("- 堆栈解析不可用\n")
	}

	user.WriteString("\n## 证据与上下文\n")
	user.WriteString(contextText)

	if fb := ExtractHumanFeedback(run); fb != "" {
		user.WriteString("\n## 人工反馈（必须优先采纳）\n")
		user.WriteString(fb + "\n")
	}
	if rc.lastFeedback != "" {
		user.WriteString("\n### 上一轮验证失败信息\n")
		user.WriteString(rc.lastFeedback + "\n")
	}

	user.WriteString("\n## 输出要求\n")
	user.WriteString("仅输出如下 JSON 对象（不要包裹代码块）：\n")
	user.WriteString(`{"summary":"一句话根因","category":"null_pointer|index_out_of_bounds|type_error|class_not_found|`)
	user.WriteString(`dependency_missing|version_conflict|connection_refused|timeout|divide_by_zero|`)
	user.WriteString(`concurrent_modification|serialization_error|resource_leak|syntax_error|oom|unknown",`)
	user.WriteString(`"detail":"推理过程与证据链","confidence":0.0,"evidence":["文件:行 或 调用链"],`)
	user.WriteString(`"blastRadius":["受影响仓库或接口"],"severity":"blocker|critical|major|minor|info"}`)

	return []domain.ModelMessage{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: user.String()},
	}
}

func stackLanguage(sa *domain.StackAnalysis) string {
	if sa == nil {
		return ""
	}
	return sa.Language
}

// parseRootCause 解析模型结构化输出，缺失字段用堆栈信息补全，置信度截断到 0-1。
func parseRootCause(res *domain.ModelResponse, rc *runCtx) *domain.RootCause {
	root := &domain.RootCause{}
	if res.Structured != nil {
		if b, err := json.Marshal(res.Structured); err == nil {
			_ = json.Unmarshal(b, root)
		}
	}
	if strings.TrimSpace(root.Summary) == "" && strings.TrimSpace(res.Text) != "" {
		// 结构化失败时尝试从纯文本中提取第一个 JSON 对象（宽容修复）。
		candidate := &domain.RootCause{}
		if err := json.Unmarshal([]byte(extractJSONObject(res.Text)), candidate); err == nil {
			root = candidate
		}
		if strings.TrimSpace(root.Summary) == "" {
			root = &domain.RootCause{Summary: strings.TrimSpace(res.Text)}
		}
	}
	return completeRootCause(root, rc)
}

// completeRootCause 用堆栈与证据补全根因字段，并做确定性裁剪。
func completeRootCause(root *domain.RootCause, rc *runCtx) *domain.RootCause {
	if root == nil {
		root = &domain.RootCause{}
	}
	sa := rc.bundle.Stack
	if strings.TrimSpace(root.Summary) == "" {
		root.Summary = stackSummary(sa, rc.run)
	}
	if strings.TrimSpace(root.Category) == "" {
		if sa != nil && sa.Category != "" {
			root.Category = sa.Category
		} else {
			root.Category = "unknown"
		}
	}
	if root.Confidence < 0 {
		root.Confidence = 0
	}
	if root.Confidence > 1 {
		root.Confidence = 1
	}
	if root.Confidence == 0 && hasStackOrSlices(rc) {
		root.Confidence = 0.4
	}
	if len(root.Evidence) == 0 {
		root.Evidence = defaultEvidence(rc.bundle)
	}
	if len(root.BlastRadius) == 0 {
		root.BlastRadius = defaultBlastRadius(rc)
	}
	if root.Severity == "" {
		root.Severity = severityForCategory(root.Category)
	}
	if strings.TrimSpace(root.Detail) == "" {
		root.Detail = "模型未给出详细推理，已依据堆栈与命中代码切片补全结论。"
	}
	return root
}

// hasStackOrSlices 判断是否存在可用于推断默认置信度的证据。
func hasStackOrSlices(rc *runCtx) bool {
	return rc.bundle != nil && (len(rc.bundle.Slices) > 0 || rc.bundle.Stack != nil)
}

func stackSummary(sa *domain.StackAnalysis, run *domain.TaskRun) string {
	if sa == nil {
		return "未能解析异常堆栈，需人工补充堆栈信息"
	}
	typ := orDefault(sa.ExceptionType, "未知异常")
	msg := orDefault(sa.Message, "")
	msg = truncate(strings.TrimSpace(msg), 160)
	if msg == "" {
		return fmt.Sprintf("检测到 %s（缺少异常消息）", typ)
	}
	return fmt.Sprintf("检测到 %s: %s", typ, msg)
}

// fallbackRootCause 模型完全失败时的确定性兜底根因（降级不失败）。
func (p *Pipeline) fallbackRootCause(rc *runCtx, reason string) {
	sa := rc.bundle.Stack
	typ := "未知异常"
	if sa != nil && sa.ExceptionType != "" {
		typ = sa.ExceptionType
	}
	msg := ""
	if sa != nil {
		msg = truncate(strings.TrimSpace(sa.Message), 160)
	}
	summary := fmt.Sprintf("检测到 %s", typ)
	if msg != "" {
		summary = fmt.Sprintf("检测到 %s: %s", typ, msg)
	}
	summary += "（模型推理不可用，基于堆栈规则推断）"

	root := &domain.RootCause{
		Summary:     summary,
		Category:    categoryOrDefault(sa),
		Detail:      "模型推理不可用（" + reason + "），已按堆栈规则生成确定性兜底结论；" + "建议人工核对命中代码切片后确认根因。",
		Confidence:  0.25,
		Evidence:    defaultEvidence(rc.bundle),
		BlastRadius: defaultBlastRadius(rc),
		Severity:    severityForCategory(categoryOrDefault(sa)),
	}
	rc.run.RootCause = root
	rc.run.Severity = domain.NormalizeSeverity(string(root.Severity))
	rc.modelFailed = true
	rc.degrade("模型推理不可用（" + reason + "），已降级为堆栈规则推断")
}

func categoryOrDefault(sa *domain.StackAnalysis) string {
	if sa != nil && sa.Category != "" {
		return sa.Category
	}
	return "unknown"
}

// defaultEvidence 模型未给出证据时的兜底证据链（切片清单由报告统一渲染）。
func defaultEvidence(b *domain.EvidenceBundle) []string {
	out := []string{}
	if b == nil {
		return out
	}
	if b.Stack != nil {
		if b.Stack.ExceptionType != "" {
			out = append(out, "异常类型："+b.Stack.ExceptionType)
		}
		for i, f := range b.Stack.Frames {
			if i >= 5 {
				break
			}
			if strings.TrimSpace(f.Raw) != "" {
				out = append(out, "堆栈帧："+strings.TrimSpace(f.Raw))
			}
		}
	}
	if len(b.Slices) > 0 {
		out = append(out, fmt.Sprintf("命中代码切片 %d 个（详见报告「证据与命中代码切片」章节）", len(b.Slices)))
	}
	return out
}

func defaultBlastRadius(rc *runCtx) []string {
	out := []string{}
	for _, c := range rc.bundle.CandidateRepos {
		out = append(out, c.RepoKey)
	}
	if rc.bundle.Stack != nil {
		for _, ep := range rc.bundle.Stack.Endpoints {
			out = append(out, ep)
		}
	}
	return out
}

// stageCrossRepo 跨仓库链路追踪（分组模式且候选 ≥2）。
func (p *Pipeline) stageCrossRepo(ctx context.Context, cc *domain.CallContext, rc *runCtx) {
	rc.stage = StageCrossRepo
	p.notifyStage(rc.run.ID, StageCrossRepo)
	p.stageEvent(rc, StageCrossRepo, "start", "阶段 跨仓库链路(cross_repo) 开始", "info", nil)
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "stage", Action: StageCrossRepo, Level: "info",
		Message: "阶段 " + stageTitle(StageCrossRepo) + " 开始",
		Data:    map[string]any{"phase": "start"},
	})

	res, err := p.runSkill(ctx, cc, rc, domain.SkillCrossRepoTrace, map[string]any{
		"stack":      stackToAny(rc.bundle.Stack),
		"candidates": candidatesToAny(rc.bundle.CandidateRepos),
		"slices":     p.slicesForSkill(rc),
	})
	edges := []domain.CallEdge{}
	chain := []string{}
	if err == nil && res != nil && res.Output != nil {
		edges = parseEdges(res.Output["edges"])
		chain = parseChain(res.Output["chain"])
	}
	if len(edges) == 0 {
		// 技能失败：按仓库分层与分组顺序构造骨架边（置信度低，供人工核对）。
		edges = skeletonEdges(rc)
		rc.warn("跨仓库链路技能不可用，已按分层顺序构造骨架链路（置信度较低）")
	}
	if len(chain) == 0 {
		// 技能未给出 chain 时，按边的拓扑顺序还原链路节点，保证报告可读。
		chain = chainFromEdges(edges)
	}
	rc.bundle.Edges = edges
	if len(chain) > 0 {
		// EvidenceBundle 没有独立的 chain 字段（domain 冻结），链路节点写入 Notes 供报告展示。
		rc.bundle.Notes = append(rc.bundle.Notes, "跨仓库调用链："+strings.Join(chain, " → "))
	}
	rc.addTimeline(StageCrossRepo, fmt.Sprintf("跨仓库链路分析完成：%d 条边", len(edges)), "info")
	p.stageEvent(rc, StageCrossRepo, "end", fmt.Sprintf("跨仓库链路分析完成：%d 条边", len(edges)), "info", nil)
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "stage", Action: StageCrossRepo, Level: "info",
		Message: fmt.Sprintf("阶段 %s 完成，共 %d 条链路边", stageTitle(StageCrossRepo), len(edges)),
		Data:    map[string]any{"phase": "end", "edges": len(edges), "chain": chain},
	})
	rc.stage = StageRootCause
}

// parseChain 解析技能输出的链路节点序列（宽容解析：支持字符串与对象两种形态）。
func parseChain(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		switch v := item.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				out = append(out, strings.TrimSpace(v))
			}
		case map[string]any:
			if s := pickString(v, "repo", "repoKey", "name", "repositoryId"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// chainFromEdges 由调用边还原链路节点顺序（起点优先，BFS 展开）。
func chainFromEdges(edges []domain.CallEdge) []string {
	if len(edges) == 0 {
		return nil
	}
	next := map[string]string{}
	hasPrev := map[string]bool{}
	order := []string{}
	for _, e := range edges {
		if e.FromRepo == "" || e.ToRepo == "" || hasPrev[e.ToRepo] {
			continue
		}
		next[e.FromRepo] = e.ToRepo
		hasPrev[e.ToRepo] = true
		order = append(order, e.FromRepo)
	}
	out := []string{}
	for _, start := range order {
		if hasPrev[start] {
			continue
		}
		for node := start; node != ""; node = next[node] {
			out = append(out, node)
			if len(out) > 16 {
				return out
			}
		}
	}
	return out
}

// skeletonEdges 按分组顺序与仓库分层构造骨架调用链。
func skeletonEdges(rc *runCtx) []domain.CallEdge {
	members := chainRepos(rc)
	edges := []domain.CallEdge{}
	for i := 1; i < len(members); i++ {
		from, to := members[i-1], members[i]
		if from == nil || to == nil {
			continue
		}
		edges = append(edges, domain.CallEdge{
			FromRepo: from.Key, ToRepo: to.Key,
			Protocol:   "http",
			Evidence:   "依据分组 Order 顺序推断（无直接调用证据）",
			Confidence: 0.2,
			Anomaly:    anomalyFor(to, rc),
		})
	}
	return edges
}

// chainRepos 返回跨仓库链路推断使用的仓库顺序（候选优先，其次已锁定仓库）。
func chainRepos(rc *runCtx) []*domain.Repository {
	out := []*domain.Repository{}
	for _, c := range rc.bundle.CandidateRepos {
		if r := rc.repoByID(c.RepositoryID); r != nil {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		for _, r := range rc.lockedRepos {
			out = append(out, r)
		}
	}
	return out
}

func anomalyFor(repo *domain.Repository, rc *runCtx) string {
	if rc.run.RootCause != nil && repo != nil {
		for _, br := range rc.run.RootCause.BlastRadius {
			if strings.Contains(br, repo.Key) {
				return rc.run.RootCause.Summary
			}
		}
	}
	return ""
}

func parseEdges(raw any) []domain.CallEdge {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]domain.CallEdge, 0, len(list))
	for _, item := range list {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var e domain.CallEdge
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if e.FromRepo == "" && e.ToRepo == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func stackToAny(sa *domain.StackAnalysis) any {
	if sa == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(sa)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func candidatesToAny(cs []domain.RepoCandidate) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// 阶段 6：patch_synthesize —— 增量补丁合成
// ---------------------------------------------------------------------------

func (p *Pipeline) stagePatchSynthesize(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	if p.d.Cfg != nil && !p.d.Cfg.Engine.AutoFix {
		rc.addTimeline(StagePatchSynthesize, "自动修复已关闭（engine.autoFix=false），仅输出根因分析", "info")
		return nil
	}
	if len(rc.bundle.Slices) == 0 {
		rc.warn("缺少代码切片，跳过补丁合成")
		return nil
	}
	p.synthesizePatches(ctx, cc, rc, "")
	return nil
}

// synthesizePatches 执行一轮补丁合成（阶段 6 主体，阶段 7 的收敛轮次复用同一条路径）。
func (p *Pipeline) synthesizePatches(ctx context.Context, cc *domain.CallContext, rc *runCtx, feedback string) {
	rootCause := rc.run.RootCause
	category := "unknown"
	if rootCause != nil && rootCause.Category != "" {
		category = rootCause.Category
	}
	input := map[string]any{
		"rootCause": rootCauseToAny(rootCause),
		"slices":    p.slicesForSkill(rc),
		"category":  category,
	}
	// 反馈注入：人工反馈（RerunWithFeedback）优先，其次为本轮收敛的验证失败信息。
	feedback = combineFeedback(ExtractHumanFeedback(rc.run), feedback)
	if feedback != "" {
		// 作为附加字段注入模型提示（Skill 层 Schema 为最小校验，附加字段被容忍；
		// 若被严格校验拒绝，则回退为不带 feedback 的调用，保证流程不被阻断）。
		input["feedback"] = feedback
	}

	res, err := p.runSkill(ctx, cc, rc, domain.SkillCodePatchSynthesize, input)
	if err != nil && feedback != "" {
		delete(input, "feedback")
		res, err = p.runSkill(ctx, cc, rc, domain.SkillCodePatchSynthesize, input)
	}

	descs := []patchDesc{}
	if err == nil && res != nil && res.Output != nil {
		descs = append(descs, parsePatchDescs(res.Output["patches"], domain.SkillCodePatchSynthesize)...)
	} else {
		rc.warn("补丁合成技能不可用，本轮未产出补丁")
	}

	// 依赖类根因：追加依赖修复技能产出的补丁。
	if isDependencyCategory(category) {
		depRes, depErr := p.runSkill(ctx, cc, rc, domain.SkillDependencyRepair, map[string]any{
			"dependencyFiles": slicesToAny(rc.bundle.DependencyFiles),
			"errorMessage":    dependencyErrorMessage(rc),
		})
		if depErr == nil && depRes != nil && depRes.Output != nil {
			descs = append(descs, parsePatchDescs(depRes.Output["patches"], domain.SkillDependencyRepair)...)
		} else {
			rc.warn("依赖修复技能不可用，未产出依赖类补丁")
		}
	}

	patches, drops := p.buildPatches(rc, descs)
	for _, d := range drops {
		rc.warn(d)
	}

	if len(patches) == 0 {
		rc.run.Patches = nil
		rc.warn("未匹配到可自动修复模式，建议人工介入")
		return
	}

	rc.run.Patches = patches
	rc.addTimeline(StagePatchSynthesize, fmt.Sprintf("产出 %d 个增量补丁", len(patches)), "info")
	msg := fmt.Sprintf("生成 %d 个候选补丁", len(patches))
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "patch", Action: "patch.proposed", Level: "info", Message: msg,
		Data: map[string]any{"count": len(patches)},
	})
	p.publish(domain.Event{
		Type: "patch.proposed", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: StagePatchSynthesize, Message: msg, Payload: patches,
	})
}

// combineFeedback 合并人工反馈与本轮收敛的验证失败信息，供技能/模型提示使用。
func combineFeedback(human, round string) string {
	human = strings.TrimSpace(human)
	round = strings.TrimSpace(round)
	switch {
	case human == "" && round == "":
		return ""
	case round == "":
		return "## 人工反馈\n" + human
	case human == "":
		return round
	default:
		return "## 人工反馈\n" + human + "\n\n### 上一轮验证失败信息\n" + round
	}
}

func rootCauseToAny(rc *domain.RootCause) map[string]any {
	if rc == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(rc)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func slicesToAny(slices []domain.CodeSlice) []any {
	out := make([]any, 0, len(slices))
	for _, s := range slices {
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func isDependencyCategory(category string) bool {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "dependency_missing", "class_not_found", "version_conflict", "dependency":
		return true
	default:
		return false
	}
}

func dependencyErrorMessage(rc *runCtx) string {
	parts := []string{}
	if rc.bundle.Stack != nil {
		if rc.bundle.Stack.Message != "" {
			parts = append(parts, rc.bundle.Stack.Message)
		}
		if rc.bundle.Stack.ExceptionType != "" {
			parts = append(parts, rc.bundle.Stack.ExceptionType)
		}
	}
	if rc.run.RootCause != nil && rc.run.RootCause.Summary != "" {
		parts = append(parts, rc.run.RootCause.Summary)
	}
	if len(parts) == 0 {
		parts = append(parts, truncate(rc.run.Stacktrace, 500))
	}
	return strings.Join(parts, " | ")
}

// patchDesc 技能产出的补丁描述（尚未与真实代码比对，禁止直接信任）。
type patchDesc struct {
	RepositoryID string
	RepoKey      string
	FilePath     string
	Action       domain.PatchAction
	Line         int
	OldSnippet   string
	NewSnippet   string
	Rationale    string
	Risk         domain.PatchRisk
	Confidence   float64
	ProducedBy   string
}

func parsePatchDescs(raw any, producedBy string) []patchDesc {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]patchDesc, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		action := domain.PatchAction(strings.ToLower(pickString(m, "action")))
		switch action {
		case domain.ActionModify, domain.ActionCreate, domain.ActionDelete:
		default:
			action = domain.ActionModify
		}
		risk := normalizeRisk(pickString(m, "risk"))
		d := patchDesc{
			RepositoryID: pickString(m, "repositoryId", "repositoryID", "repository_id", "repoId"),
			RepoKey:      pickString(m, "repoKey", "repo_key", "repositoryKey"),
			FilePath:     pickString(m, "filePath", "file_path", "path", "file"),
			Action:       action,
			Line:         pickInt(m, "line", "lineNumber", "startLine"),
			OldSnippet:   pickString(m, "oldSnippet", "old_snippet", "oldContent", "old"),
			NewSnippet:   pickString(m, "newSnippet", "new_snippet", "newContent", "new"),
			Rationale:    pickString(m, "rationale", "reason", "description"),
			Risk:         risk,
			Confidence:   pickFloat(m, "confidence", "score"),
			ProducedBy:   producedBy,
		}
		if d.FilePath == "" {
			continue
		}
		if d.NewSnippet == "" && action != domain.ActionDelete {
			continue
		}
		out = append(out, d)
	}
	return out
}

func normalizeRisk(raw string) domain.PatchRisk {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(domain.RiskLow):
		return domain.RiskLow
	case string(domain.RiskHigh):
		return domain.RiskHigh
	case string(domain.RiskMedium):
		return domain.RiskMedium
	default:
		return domain.RiskMedium
	}
}

// buildPatches 把技能补丁描述转换为可验证的增量补丁。
//
// 关键约束：
//  1. 目标文件必须命中已加载的真实代码切片，否则丢弃（禁止凭空造补丁）；
//  2. 统一走 sandbox.BuildPatch/BuildCreatePatch 生成标准 unified diff，
//     ErrSnippetNotFound 一律丢弃并记录原因；
//  3. 规模校验：单文件新增行 > 60 或多文件 > 5 时升级为 high 风险（保留补丁交人工复核）；
//  4. 按 repoKey+filePath+oldSnippet 去重，只保留第一个。
func (p *Pipeline) buildPatches(rc *runCtx, descs []patchDesc) ([]domain.Patch, []string) {
	if len(descs) == 0 {
		return nil, nil
	}
	patches := make([]domain.Patch, 0, len(descs))
	drops := []string{}
	seen := map[string]bool{}
	fileSet := map[string]bool{}

	type pending struct {
		patch *domain.Patch
		file  string
	}
	prepared := make([]pending, 0, len(descs))

	for _, d := range descs {
		repo := rc.repoByID(d.RepositoryID)
		if repo == nil {
			repo = rc.repoByKey(d.RepoKey)
		}
		if repo == nil {
			drops = append(drops, fmt.Sprintf("补丁目标仓库（%s/%s）不在本次任务范围内，已丢弃", d.RepositoryID, d.RepoKey))
			continue
		}
		slice := findSlice(rc.bundle.Slices, repo.ID, d.FilePath)
		if slice == nil {
			drops = append(drops, fmt.Sprintf("补丁目标文件 %s 未在已加载代码切片中命中，已丢弃该补丁（禁止伪造补丁）", d.FilePath))
			continue
		}
		key := repo.Key + "|" + slice.Path + "|" + d.OldSnippet
		if seen[key] {
			continue
		}
		seen[key] = true

		var (
			patch *domain.Patch
			err   error
		)
		if d.Action == domain.ActionCreate || strings.TrimSpace(d.OldSnippet) == "" {
			content := d.NewSnippet
			patch, err = sandbox.BuildCreatePatch(repo, slice.Path, content, d.Rationale, d.Risk)
		} else {
			patch, err = sandbox.BuildPatch(repo, slice.Path, slice.Content, d.OldSnippet, d.NewSnippet, d.Rationale, d.Risk)
		}
		if err != nil {
			if errors.Is(err, sandbox.ErrSnippetNotFound) {
				drops = append(drops, fmt.Sprintf("补丁引用的原始代码片段未在 %s 中命中（snippet not found），已丢弃该补丁", slice.Path))
			} else {
				drops = append(drops, fmt.Sprintf("补丁生成失败（%s）：%v，已丢弃", slice.Path, err))
			}
			continue
		}
		if patch == nil {
			drops = append(drops, fmt.Sprintf("补丁生成返回空结果（%s），已丢弃", slice.Path))
			continue
		}
		if patch.ID == "" {
			patch.ID = uuid.NewString()
		}
		if patch.Status == "" {
			patch.Status = domain.PatchProposed
		}
		if patch.ProducedBy == "" {
			patch.ProducedBy = d.ProducedBy
		}
		if patch.RepositoryID == "" {
			patch.RepositoryID = repo.ID
		}
		if patch.RepoKey == "" {
			patch.RepoKey = repo.Key
		}
		if added := countSnippetLines(d.NewSnippet); added > 60 {
			patch.Risk = domain.RiskHigh
			rc.warn(fmt.Sprintf("补丁 %s 单文件新增行数 %d 超过 60，已升级为 high 风险并交人工复核", slice.Path, added))
		}
		fileSet[slice.Path] = true
		prepared = append(prepared, pending{patch: patch, file: slice.Path})
	}

	// 单文件去重：同一文件只保留一个补丁。
	//
	// 多个补丁同时改写同一文件时，除第一个之外的补丁上下文都基于**原始文件**生成，
	// 无法与前一个补丁在沙箱中叠加应用（会判定为冲突，进而让整次验证失败）。
	// 因此这里按 仓库+文件 去重，保留置信度最高者；被丢弃的补丁写入 drops，
	// 由报告显式提示"同文件多缺陷建议合并为一个补丁后重跑"，绝不静默丢弃。
	byFile := map[string]int{} // 仓库|文件 → prepared 下标
	deduped := make([]pending, 0, len(prepared))
	for _, item := range prepared {
		key := item.patch.RepoKey + "|" + item.file
		if idx, ok := byFile[key]; ok {
			kept := deduped[idx]
			if patchConfidence(item.patch) > patchConfidence(kept.patch) {
				drops = append(drops, fmt.Sprintf(
					"文件 %s 存在多个补丁，已保留置信度更高的一个（丢弃另一处修改，同文件多缺陷建议合并为一个补丁后重跑）", item.file))
				deduped[idx] = item
			} else {
				drops = append(drops, fmt.Sprintf(
					"文件 %s 存在多个补丁，已保留置信度更高的一个（丢弃另一处修改，同文件多缺陷建议合并为一个补丁后重跑）", item.file))
			}
			continue
		}
		byFile[key] = len(deduped)
		deduped = append(deduped, item)
	}
	prepared = deduped

	// 多文件规模校验：超过 5 个文件时整体升级为 high 风险。
	fileSet = map[string]bool{}
	for _, item := range prepared {
		fileSet[item.file] = true
	}
	multiFile := len(fileSet) > 5
	if multiFile {
		rc.warn(fmt.Sprintf("本次修复涉及 %d 个文件（> 5），补丁风险整体升级为 high，需人工复核", len(fileSet)))
	}
	for _, item := range prepared {
		if multiFile {
			item.patch.Risk = domain.RiskHigh
		}
		patches = append(patches, *item.patch)
	}
	return patches, drops
}

// findSlice 在已加载切片中定位补丁目标文件（支持完整路径或路径后缀匹配）。
func findSlice(slices []domain.CodeSlice, repoID, filePath string) *domain.CodeSlice {
	target := normalizePath(filePath)
	if target == "" {
		return nil
	}
	for i := range slices {
		if repoID != "" && slices[i].RepositoryID != "" && slices[i].RepositoryID != repoID {
			continue
		}
		sp := normalizePath(slices[i].Path)
		if sp == target || strings.HasSuffix(sp, "/"+target) || strings.HasSuffix(target, "/"+sp) {
			return &slices[i]
		}
	}
	return nil
}

func countSnippetLines(snippet string) int {
	n := 0
	for _, line := range strings.Split(snippet, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// patchConfidence 补丁置信度（缺省 0.5），用于同文件多补丁时择优保留。
func patchConfidence(p *domain.Patch) float64 {
	if p == nil {
		return 0
	}
	if p.Verification != nil {
		// 已有验证结论的补丁优先级更高（避免重复验证开销与结论漂移）。
		return 0.9
	}
	if p.Risk == domain.RiskLow {
		return 0.8
	}
	if p.Risk == domain.RiskMedium {
		return 0.6
	}
	return 0.4
}

// ---------------------------------------------------------------------------
// 阶段 7：sandbox_verify —— 闭环验证 + 迭代收敛
// ---------------------------------------------------------------------------

func (p *Pipeline) stageSandboxVerify(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	if len(run.Patches) == 0 {
		rc.addTimeline(StageSandboxVerify, "无补丁需要验证，跳过沙箱验证", "info")
		return nil
	}
	if hasWarningPrefix(run, warnAutoVerifyOffPrefix) {
		rc.addTimeline(StageSandboxVerify, "调用方已关闭沙箱验证，补丁状态保持 proposed", "warn")
		return nil
	}
	if p.d.Sandbox == nil {
		run.Verification = p.staticFallbackVerification(ctx, cc, rc, "沙箱执行层不可用")
		p.publishVerification(rc)
		return nil
	}
	if p.d.Cfg != nil && !p.d.Cfg.Sandbox.Enabled {
		// 无命令执行能力时降级为静态校验：通过也算 Passed=true，但 Degraded=true。
		run.Verification = p.staticFallbackVerification(ctx, cc, rc, "沙箱功能已关闭（sandbox.enabled=false）")
		p.publishVerification(rc)
		return nil
	}

	meta := domain.SandboxMeta{
		RunID:     run.ID,
		TenantID:  run.TenantID,
		TaskID:    run.TaskID,
		Pinned:    run.PinnedCommits,
		Repos:     rc.lockedRepos,
		Timeout:   p.taskTimeout(),
		AllowExec: p.d.Cfg == nil || p.d.Cfg.Sandbox.AllowCommandExec,
	}
	handle, err := p.d.Sandbox.Open(ctx, meta)
	if err != nil || handle == nil {
		run.Verification = p.staticFallbackVerification(ctx, cc, rc, fmt.Sprintf("沙箱工作区创建失败：%v", err))
		p.publishVerification(rc)
		return nil
	}
	defer func() {
		if cerr := handle.Close(); cerr != nil {
			rc.warn("沙箱工作区释放失败：" + cerr.Error())
		}
	}()
	// 额外打开的工作区（每轮修复验证一份），在函数退出时统一释放。
	extraHandles := []domain.SandboxHandle{}
	defer func() {
		for _, h := range extraHandles {
			if cerr := h.Close(); cerr != nil {
				rc.warn("沙箱工作区释放失败：" + cerr.Error())
			}
		}
	}()

	verify := func(h domain.SandboxHandle) *domain.Verification {
		res, verr := p.d.Sandbox.Validate(ctx, h, domain.ValidateRequest{
			Patches: run.Patches,
			Timeout: p.stageTimeout(),
		})
		if verr != nil || res == nil {
			return degradedVerification(rc, fmt.Sprintf("沙箱验证执行失败：%v", verr))
		}
		if res.Workspace == "" {
			res.Workspace = h.Root()
		}
		return res
	}
	// openFreshWorkspace 为下一轮修复验证打开**全新工作区**。
	//
	// 关键原因：补丁的上下文来自原始代码，必须应用到"未打补丁的原始工作区"。
	// 若复用同一个工作区，上一轮已落地的改动会让下一轮补丁全部判定为上下文不匹配
	// （表现为"第 2 轮 ApplyResult=conflict"），使多轮修复永远无法收敛。
	openFreshWorkspace := func(roundNo int) (domain.SandboxHandle, error) {
		m := meta
		m.RunID = fmt.Sprintf("%s-r%d", run.ID, roundNo)
		h, oerr := p.d.Sandbox.Open(ctx, m)
		if oerr != nil || h == nil {
			return nil, fmt.Errorf("第 %d 轮工作区创建失败: %w", roundNo, oerr)
		}
		extraHandles = append(extraHandles, h)
		return h, nil
	}

	run.Verification = verify(handle)
	p.publishVerification(rc)

	maxRounds := 0
	if p.d.Cfg != nil {
		maxRounds = p.d.Cfg.Engine.MaxRepairRounds
	}
	round := 0
	for !run.Verification.Passed && round < maxRounds && len(run.Patches) > 0 {
		round++
		feedback := verificationFeedback(run.Verification)
		rc.lastFeedback = feedback
		msg := fmt.Sprintf("第 %d 轮修复验证失败：%s", round, firstLine(feedback))
		rc.warn(msg)
		p.publish(domain.Event{
			Type: "stage", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Stage: StagePatchSynthesize, Level: "warn",
			Message: fmt.Sprintf("第 %d 轮修复：基于验证失败信息重新合成补丁", round),
		})
		rc.stage = StagePatchSynthesize
		p.synthesizePatches(ctx, cc, rc, feedback)
		rc.stage = StageSandboxVerify
		if len(run.Patches) == 0 {
			rc.warn(fmt.Sprintf("第 %d 轮修复未产出可用补丁，收敛终止", round))
			break
		}
		// 本轮补丁必须在全新的原始工作区上验证。
		fresh, oerr := openFreshWorkspace(round)
		if oerr != nil {
			rc.warn("第 " + fmt.Sprint(round) + " 轮无法创建独立工作区（" + oerr.Error() + "），沿用上一轮工作区结果")
			break
		}
		run.Verification = verify(fresh)
		p.publishVerification(rc)
	}

	// 收敛结束：未通过时保留最后一轮补丁并标记 failed，交由人工复核。
	if run.Verification != nil && !run.Verification.Passed {
		for i := range run.Patches {
			run.Patches[i].Status = domain.PatchFailed
		}
		rc.warn("多轮修复后仍未通过验证，补丁已标记 failed，需人工复核")
	} else {
		for i := range run.Patches {
			run.Patches[i].Status = domain.PatchApplied
		}
	}
	return nil
}

// staticFallbackVerification 沙箱不可用时的静态校验兜底（通过也算 Passed，但标记 Degraded）。
func (p *Pipeline) staticFallbackVerification(ctx context.Context, cc *domain.CallContext, rc *runCtx, reason string) *domain.Verification {
	v := degradedVerification(rc, reason)
	res, err := p.runSkill(ctx, cc, rc, domain.SkillPatchVerify, map[string]any{
		"patches":  patchesToAny(rc.run.Patches),
		"language": stackLanguage(rc.bundle.Stack),
	})
	if err != nil || res == nil || res.Output == nil {
		return v
	}
	passed := false
	if b, ok := res.Output["passed"].(bool); ok {
		passed = b
	}
	v.Passed = passed
	checks := parseChecks(res.Output["checks"])
	if len(checks) > 0 {
		v.Checks = checks
	}
	if score, ok := res.Output["score"]; ok {
		v.Log = fmt.Sprintf("静态校验（patch_verify）得分：%v", score)
	}
	return v
}

func degradedVerification(rc *runCtx, reason string) *domain.Verification {
	rc.warn(reason + "，验证降级为静态校验")
	return &domain.Verification{
		Passed:   false,
		Degraded: true,
		Checks: []domain.VerificationCheck{{
			Name: "static_fallback", Passed: false, Skipped: true, SkipReason: reason,
		}},
		Log: reason,
	}
}

func (p *Pipeline) publishVerification(rc *runCtx) {
	v := rc.run.Verification
	if v == nil {
		return
	}
	level := "info"
	msg := "沙箱验证通过"
	if !v.Passed {
		level, msg = "warn", "沙箱验证未通过"
	}
	if v.Degraded {
		level = "warn"
		msg += "（降级为静态校验）"
	}
	rc.addTimeline(StageSandboxVerify, msg, level)
	p.audit(domain.AuditEvent{
		TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Category: "patch", Action: "verification", Level: level, Message: msg, Data: v,
	})
	p.publish(domain.Event{
		Type: "verification", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: StageSandboxVerify, Level: level, Message: msg, Payload: v,
	})
}

// verificationFeedback 生成修复轮次的失败反馈（单项输出截断 2000 字符）。
func verificationFeedback(v *domain.Verification) string {
	if v == nil {
		return "验证结果缺失"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("验证未通过（ApplyResult=%s，耗时 %dms）\n", orDefault(v.ApplyResult, "unknown"), v.DurationMS))
	for _, c := range v.Checks {
		state := "通过"
		if !c.Passed {
			state = "失败"
		}
		if c.Skipped {
			state = "跳过（" + c.SkipReason + "）"
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s：%s\n", c.Name, state, truncate(strings.TrimSpace(c.Output), 2000)))
	}
	if len(v.Checks) == 0 && v.Log != "" {
		sb.WriteString(truncate(v.Log, 2000))
	}
	return sb.String()
}

func firstLine(s string) string {
	line := firstNonEmptyLine(s)
	return truncate(strings.TrimSpace(line), 200)
}

func parseChecks(raw any) []domain.VerificationCheck {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]domain.VerificationCheck, 0, len(list))
	for _, item := range list {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var c domain.VerificationCheck
		if err := json.Unmarshal(b, &c); err != nil {
			continue
		}
		if c.Name == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func patchesToAny(patches []domain.Patch) []any {
	out := make([]any, 0, len(patches))
	for _, pt := range patches {
		b, err := json.Marshal(pt)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// 阶段 8/9：report / archive
// ---------------------------------------------------------------------------

func (p *Pipeline) stageReport(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	// 报告需要携带最终状态：此处先做终态判定（Engine 会按状态机校验并迁移）。
	run.State = DecideState(run)

	rep := BuildReport(run, rc.bundle, rc.timeline)
	if p.d.Store != nil {
		if err := p.d.Store.CreateReport(rep); err != nil {
			rc.warn("报告归档失败：" + err.Error())
		}
	}
	run.ReportID = rep.ID
	rc.report = rep

	p.publish(domain.Event{
		Type: "report.ready", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Stage: StageReport, Message: "分析报告已生成",
		Payload: map[string]any{"reportId": rep.ID},
	})
	rc.addTimeline(StageReport, "报告已生成："+rep.ID, "info")
	_ = ctx
	_ = cc
	return nil
}

func (p *Pipeline) stageArchive(ctx context.Context, cc *domain.CallContext, rc *runCtx) error {
	run := rc.run
	if p.d.Store != nil {
		if err := p.d.Store.UpdateRun(run); err != nil {
			rc.warn("运行上下文落库失败：" + err.Error())
		}
	}
	p.publish(domain.Event{
		Type: "task.terminal", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Stage: StageArchive, Message: "任务进入终态：" + string(run.State),
		Payload: map[string]any{"state": string(run.State), "reportId": run.ReportID},
	})
	_ = ctx
	_ = cc
	return nil
}

// ---------------------------------------------------------------------------
// 终态判定
// ---------------------------------------------------------------------------

// DecideState 依据验证结果与补丁情况判定终态。
//
// 规则（CONTRACT §6.6）：
//   - 有补丁 且 验证通过 且 非降级 → succeeded
//   - 有补丁 且（验证未通过 或 验证降级 或 补丁 risk=high 或 全程降级）→ needs_review
//   - 无补丁 → needs_review（根因已定位但无可自动修复项）
//   - 全程降级（无代码证据 或 模型不可用）且无补丁 → degraded
//   - 致命异常（run.Error 非空）→ failed
func DecideState(run *domain.TaskRun) domain.TaskState {
	if run == nil {
		return domain.StateFailed
	}
	if run.State == domain.StateCancelled {
		return domain.StateCancelled
	}
	if strings.TrimSpace(run.Error) != "" {
		return domain.StateFailed
	}

	hasPatches := len(run.Patches) > 0
	if hasPatches {
		v := run.Verification
		switch {
		case v == nil:
			return domain.StateNeedsReview
		case !v.Passed, v.Degraded, run.Degraded, hasHighRisk(run.Patches):
			return domain.StateNeedsReview
		default:
			return domain.StateSucceeded
		}
	}
	if run.Degraded {
		return domain.StateDegraded
	}
	return domain.StateNeedsReview
}

func hasHighRisk(patches []domain.Patch) bool {
	for _, p := range patches {
		if p.Risk == domain.RiskHigh {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 技能调用与上下文工具
// ---------------------------------------------------------------------------

// runSkill 统一执行技能：统计用量、发布实时事件；技能失败不抛错，由调用方决定降级策略。
//
// 审计记录（Recorder.SkillCall）由 Skill 层负责，本层只做实时事件与用量统计，避免重复落审计。
func (p *Pipeline) runSkill(ctx context.Context, cc *domain.CallContext, rc *runCtx, name string, input map[string]any) (*domain.SkillResult, error) {
	if p.d.Skills == nil {
		return nil, fmt.Errorf("技能执行器不可用")
	}
	start := time.Now()
	res, err := p.d.Skills.Run(ctx, cc.Child(rc.stage), domain.SkillInvocation{Skill: name, Input: input})
	rc.run.Usage.SkillCalls++

	status := domain.CallOK
	level := "info"
	errMsg := ""
	if err != nil {
		status = domain.CallFailed
		errMsg = err.Error()
	}
	if res != nil {
		if res.Status != "" {
			status = res.Status
		}
		if res.Error != "" {
			errMsg = res.Error
		}
		if res.FromFallback {
			level = "warn"
		}
	}
	if status != domain.CallOK && status != domain.CallSkipped {
		rc.run.Usage.SkillFailures++
		if level != "warn" {
			level = "error"
		}
	}

	call := domain.SkillCall{
		ID: uuid.NewString(), RunID: rc.run.ID, TenantID: rc.run.TenantID,
		Skill: name, Stage: rc.stage, Status: status, Error: errMsg,
		DurationMS: time.Since(start).Milliseconds(), StartedAt: start,
		FromFallback: res != nil && res.FromFallback,
	}
	if res != nil {
		call.Version = res.Version
		call.Attempts = res.Attempts
		call.Output = res.Output
	}
	p.publish(domain.Event{
		Type: "skill.call", TenantID: rc.run.TenantID, RunID: rc.run.ID, TaskID: rc.run.TaskID,
		Stage: rc.stage, Level: level, Message: "技能调用：" + name, Payload: call,
	})
	return res, err
}

func (p *Pipeline) taskTimeout() time.Duration {
	if p.d.Cfg != nil && p.d.Cfg.Engine.TaskTimeoutSec > 0 {
		return p.d.Cfg.TaskTimeout()
	}
	return 15 * time.Minute
}

func (p *Pipeline) stageTimeout() time.Duration {
	if p.d.Cfg != nil && p.d.Cfg.Engine.StageTimeoutSec > 0 {
		return p.d.Cfg.StageTimeout()
	}
	return 3 * time.Minute
}

// ExtractHumanFeedback 提取人工反馈（RerunWithFeedback 写入的 "人工反馈：" 前缀条目），
// 用于在阶段 5/6 的模型提示中注入定向修复指令。
func ExtractHumanFeedback(run *domain.TaskRun) string {
	if run == nil {
		return ""
	}
	out := []string{}
	for _, w := range run.Warnings {
		if strings.HasPrefix(w, warnFeedbackPrefix) {
			if v := strings.TrimSpace(strings.TrimPrefix(w, warnFeedbackPrefix)); v != "" {
				out = append(out, v)
			}
		}
	}
	return strings.Join(out, "\n")
}

func hasWarningPrefix(run *domain.TaskRun, prefix string) bool {
	if run == nil {
		return false
	}
	for _, w := range run.Warnings {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 字符串工具
// ---------------------------------------------------------------------------

// normalizePkg 归一化包名：统一小写、`/` → `.`、去掉首尾点。
func normalizePkg(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "/", ".")
	s = strings.ReplaceAll(s, "\\", ".")
	return strings.Trim(s, ".")
}

// normalizePath 归一化路径：统一小写与分隔符，去掉首尾斜杠。
func normalizePath(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "\\", "/")
	return strings.Trim(s, "/")
}

// normalizeIdent 归一化标识符：仅保留字母数字，用于类名 ↔ 仓库 Key 的模糊比对。
func normalizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// identBase 去除方法名后缀（com.acme.OrderService.save → orderservice）。
func identBase(s string) string {
	if s == "" {
		return ""
	}
	if idx := strings.Index(s, "."); idx >= 0 {
		return s[:idx]
	}
	return s
}

// identSimilar 判断归一化后的标识符是否相似（互为子串且长度足够）。
func identSimilar(a, b string) bool {
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	return a == b || strings.Contains(a, b) || strings.Contains(b, a)
}

func pkgFromPath(file string) string {
	p := normalizePath(file)
	for _, marker := range []string{"src/main/java/", "src/test/java/", "src/main/kotlin/", "src/main/scala/"} {
		if idx := strings.Index(p, marker); idx >= 0 {
			rest := p[idx+len(marker):]
			if slash := strings.LastIndex(rest, "/"); slash > 0 {
				return strings.ReplaceAll(rest[:slash], "/", ".")
			}
		}
	}
	return ""
}

func artifactBase(name string) string {
	base := strings.TrimSpace(name)
	for _, ext := range []string{".jar", ".war", ".zip", ".tar.gz", ".so"} {
		base = strings.TrimSuffix(base, ext)
	}
	// 去掉版本号后缀：order-service-1.2.3 → order-service
	parts := strings.Split(base, "-")
	for len(parts) > 1 {
		last := parts[len(parts)-1]
		if last != "" && last[0] >= '0' && last[0] <= '9' {
			parts = parts[:len(parts)-1]
			continue
		}
		break
	}
	joined := strings.Join(parts, "-")
	if len(joined) < 4 {
		return base
	}
	return joined
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "…（已截断）"
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch tv := v.(type) {
		case string:
			if strings.TrimSpace(tv) != "" {
				return tv
			}
		case json.Number:
			return tv.String()
		}
	}
	return ""
}

func pickInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch tv := v.(type) {
		case float64:
			return int(tv)
		case int:
			return tv
		case int64:
			return int(tv)
		case json.Number:
			if n, err := tv.Int64(); err == nil {
				return int(n)
			}
		case string:
			var n int
			if _, err := fmt.Sscanf(tv, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

func pickFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch tv := v.(type) {
		case float64:
			return tv
		case int:
			return float64(tv)
		case json.Number:
			if f, err := tv.Float64(); err == nil {
				return f
			}
		}
	}
	return 0
}

// extractJSONObject 从模型纯文本中提取第一个 JSON 对象（结构化输出修复兜底）。
func extractJSONObject(text string) string {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return "{}"
	}
	return text[start : end+1]
}
