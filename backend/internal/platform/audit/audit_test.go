package audit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/eventbus"
	"github.com/codeagent/backend/internal/store"
)

// 测试用租户。
const (
	tenantA = "t-1"
	tenantB = "t-2"
)

// newRecorder 构造带真实事件总线的记录器。
func newRecorder(t *testing.T) (*Recorder, store.Store, *eventbus.Bus) {
	t.Helper()
	st := store.New()
	bus := eventbus.New(128)
	return New(st, bus), st, bus
}

// seedRun 写入执行记录。
func seedRun(t *testing.T, st store.Store, run domain.TaskRun) {
	t.Helper()
	if err := st.CreateRun(&run); err != nil {
		t.Fatalf("写入运行 %s 失败: %v", run.ID, err)
	}
}

// seedTask 写入逻辑任务。
func seedTask(t *testing.T, st store.Store, task domain.Task) {
	t.Helper()
	if err := st.CreateTask(&task); err != nil {
		t.Fatalf("写入任务 %s 失败: %v", task.ID, err)
	}
}

// ---------------------------------------------------------------------------
// 落库与事件广播
// ---------------------------------------------------------------------------

func TestRecorderPersistsAndPublishes(t *testing.T) {
	rec, st, bus := newRecorder(t)
	ctx := context.Background()
	events, cancel := bus.Subscribe(tenantA)
	defer cancel()

	patch := domain.Patch{ID: "patch-1", RepositoryID: "repo-order", FilePath: "OrderService.java"}
	verification := &domain.Verification{
		Passed: true,
		Checks: []domain.VerificationCheck{{Name: "compile", Passed: true}},
	}

	rec.Info(tenantA, "run-1", CatTask, "submit", "任务已受理", map[string]any{"repo": "order-service"})
	rec.Warn(tenantA, "run-1", CatQuota, "quota", "代码字符数超限，已降级裁剪", nil)
	rec.Error(tenantA, "run-1", CatModel, "chat", "模型调用失败，走兜底", nil)
	rec.TaskState(tenantA, "task-1", "run-1", domain.StateQueued, domain.StateAnalyzing)
	rec.Stage(tenantA, "task-1", "run-1", "resolve", "开始版本锁定", map[string]any{"repos": 2})
	rec.RepoSwitch(tenantA, "run-1", "repo-order", "order-service", "abc1234567890", "web-mall", "order-service")
	rec.SourceFetch(tenantA, "run-1", "repo-order", "order-service", "abc1234567890", 3, nil)
	rec.SourceFetch(tenantA, "run-1", "repo-pay", "payment-service", "", 0, errors.New("远端不可达"))
	rec.PatchProposed(tenantA, "run-1", []domain.Patch{patch})
	rec.Verification(tenantA, "run-1", verification)
	rec.ReportReady(tenantA, "run-1", "report-1")
	rec.SkillCall(domain.SkillCall{RunID: "run-1", TenantID: tenantA, Skill: "stacktrace_parse", Status: domain.CallOK, DurationMS: 12})
	rec.ModelCall(domain.ModelCall{RunID: "run-1", TenantID: tenantA, Provider: "mock", Model: "mock-reasoner-v1", Status: domain.CallOK, TotalTokens: 10})
	// 其他租户的数据不应混入。
	rec.Info(tenantB, "run-9", CatTask, "submit", "另一个租户的任务", nil)

	// 审计落库：tenantA 11 条（含 Info/Warn/Error 与全部便捷方法）。
	page, err := rec.ListAudits(ctx, tenantA, domain.PageQuery{})
	if err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	if page.Total != 11 {
		t.Fatalf("审计条数错误: %d", page.Total)
	}
	for _, ev := range page.Items {
		if ev.ID == "" || ev.At.IsZero() || ev.Level == "" {
			t.Fatalf("审计事件缺少 ID/时间/级别: %+v", ev)
		}
	}
	byCat, err := rec.ListAudits(ctx, tenantA, domain.PageQuery{State: CatTask})
	if err != nil {
		t.Fatalf("按分类查询失败: %v", err)
	}
	if byCat.Total != 4 {
		t.Fatalf("task 分类条数错误: %d", byCat.Total)
	}
	other, err := rec.ListAudits(ctx, tenantB, domain.PageQuery{})
	if err != nil || other.Total != 1 {
		t.Fatalf("租户隔离失效: total=%d err=%v", other.Total, err)
	}

	// 技能 / 模型调用落库。
	skills, err := rec.ListSkillCalls(ctx, tenantA, "run-1", domain.PageQuery{})
	if err != nil || skills.Total != 1 {
		t.Fatalf("技能调用条数错误: %d %v", skills.Total, err)
	}
	if skills.Items[0].ID == "" || skills.Items[0].StartedAt.IsZero() {
		t.Fatalf("技能调用缺少 ID/时间: %+v", skills.Items[0])
	}
	models, err := rec.ListModelCalls(ctx, tenantA, "run-1", domain.PageQuery{})
	if err != nil || models.Total != 1 {
		t.Fatalf("模型调用条数错误: %d %v", models.Total, err)
	}
	if models.Items[0].ID == "" || models.Items[0].StartedAt.IsZero() {
		t.Fatalf("模型调用缺少 ID/时间: %+v", models.Items[0])
	}
	// runID 过滤生效。
	if empty, _ := rec.ListSkillCalls(ctx, tenantA, "run-not-exist", domain.PageQuery{}); empty.Total != 0 {
		t.Fatalf("runID 过滤失效: %d", empty.Total)
	}

	// 事件广播：3 条 log + 9 条领域事件。
	want := map[string]int{
		EventLog:           3,
		EventTaskState:     1,
		EventStage:         1,
		EventRepoSwitch:    1,
		EventSourceFetch:   2,
		EventPatchProposed: 1,
		EventVerification:  1,
		EventReportReady:   1,
		EventSkillCall:     1,
		EventModelCall:     1,
	}
	total := 0
	for _, n := range want {
		total += n
	}
	got := map[string]int{}
	var stateEvent, stageEvent *domain.Event
	deadline := time.After(3 * time.Second)
	received := 0
	for received < total {
		select {
		case ev := <-events:
			received++
			got[ev.Type]++
			if ev.Type == EventTaskState {
				e := ev
				stateEvent = &e
			}
			if ev.Type == EventStage {
				e := ev
				stageEvent = &e
			}
			if ev.TenantID != tenantA {
				t.Fatalf("事件租户错误: %+v", ev)
			}
			if ev.Seq == 0 || ev.At.IsZero() {
				t.Fatalf("事件缺少序号/时间: %+v", ev)
			}
		case <-deadline:
			t.Fatalf("等待事件超时，已收到 %d/%d 条: %v", received, total, got)
		}
	}
	for typ, n := range want {
		if got[typ] != n {
			t.Fatalf("事件类型 %s 数量错误: 期望 %d，实际 %d（全部: %v）", typ, n, got[typ], got)
		}
	}
	if stateEvent == nil || stateEvent.Payload == nil {
		t.Fatal("缺少 task.state 事件")
	}
	payload, ok := stateEvent.Payload.(map[string]any)
	if !ok || payload["from"] != domain.StateQueued || payload["to"] != domain.StateAnalyzing {
		t.Fatalf("task.state 事件载荷错误: %+v", stateEvent.Payload)
	}
	if stateEvent.RunID != "run-1" || stateEvent.TaskID != "task-1" {
		t.Fatalf("task.state 事件链路字段错误: %+v", stateEvent)
	}
	if stageEvent == nil || stageEvent.Stage != "resolve" || stageEvent.Message == "" {
		t.Fatalf("stage 事件内容错误: %+v", stageEvent)
	}

	// 上下文取消应快速失败。
	cctx, cancelCtx := context.WithCancel(ctx)
	cancelCtx()
	if _, err := rec.Observability(cctx, tenantA); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的上下文应返回错误，实际: %v", err)
	}
	if _, err := rec.ListAudits(cctx, tenantA, domain.PageQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的上下文应返回错误，实际: %v", err)
	}
	_ = st
}

// ---------------------------------------------------------------------------
// 可观测汇总
// ---------------------------------------------------------------------------

func TestObservabilityAggregation(t *testing.T) {
	rec, st, _ := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	seedTask(t, st, domain.Task{ID: "task-1", TenantID: tenantA, Title: "订单接口 500", CreatedAt: base, UpdatedAt: base})
	seedTask(t, st, domain.Task{ID: "task-2", TenantID: tenantA, Title: "库存扣减异常", CreatedAt: base, UpdatedAt: base})
	seedTask(t, st, domain.Task{ID: "task-9", TenantID: tenantB, Title: "其他租户任务", CreatedAt: base, UpdatedAt: base})

	newRun := func(id string, state domain.TaskState, repos int, category string) domain.TaskRun {
		run := domain.TaskRun{
			ID: id, TaskID: "task-1", TenantID: tenantA, State: state, Severity: domain.SeverityCritical,
			Title: "订单接口 500", CreatedAt: base, StartedAt: base, EndedAt: base.Add(2 * time.Second),
			UpdatedAt: base.Add(2 * time.Second),
		}
		for i := 0; i < repos; i++ {
			run.Resolution = append(run.Resolution, domain.RepoResolved{
				RepositoryID: fmt.Sprintf("repo-%d", i), Commit: "c0ffee", ResolvedAt: base,
			})
		}
		if category != "" {
			run.RootCause = &domain.RootCause{Category: category, Summary: "根因-" + id}
		}
		return run
	}

	run1 := newRun("run-1", domain.StateSucceeded, 3, "logic")
	run1.Usage = domain.RunUsage{CacheHits: 2}
	seedRun(t, st, run1)
	seedRun(t, st, newRun("run-2", domain.StateNeedsReview, 1, "dependency"))
	run3 := newRun("run-3", domain.StateFailed, 0, "logic")
	run3.Error = "编译失败"
	run3.RootCause.Summary = "依赖版本冲突"
	seedRun(t, st, run3)
	seedRun(t, st, newRun("run-4", domain.StateDegraded, 0, "config"))

	// 另一租户：只应有 1 次成功运行，不得混入 t-1 的统计。
	other := domain.TaskRun{
		ID: "run-9", TaskID: "task-9", TenantID: tenantB, State: domain.StateSucceeded,
		Title: "其他租户任务", CreatedAt: base, StartedAt: base, EndedAt: base.Add(time.Second),
		UpdatedAt: base.Add(time.Second), Usage: domain.RunUsage{CacheHits: 9},
	}
	seedRun(t, st, other)

	// 技能调用：4 次，其中 1 次失败。
	for i, call := range []domain.SkillCall{
		{RunID: "run-1", TenantID: tenantA, Skill: "patch_verify", Status: domain.CallOK, DurationMS: 100},
		{RunID: "run-1", TenantID: tenantA, Skill: "patch_verify", Status: domain.CallOK, DurationMS: 300},
		{RunID: "run-1", TenantID: tenantA, Skill: "stacktrace_parse", Status: domain.CallFailed, DurationMS: 200},
		{RunID: "run-1", TenantID: tenantA, Skill: "stacktrace_parse", Status: domain.CallOK, DurationMS: 100},
		{RunID: "run-9", TenantID: tenantB, Skill: "patch_verify", Status: domain.CallOK, DurationMS: 1},
	} {
		call.StartedAt = base.Add(time.Duration(i) * time.Second)
		rec.SkillCall(call)
	}
	// 模型调用：2 次，其中 1 次兜底。
	for i, call := range []domain.ModelCall{
		{RunID: "run-1", TenantID: tenantA, Provider: "mock", Model: "mock-reasoner-v1", Tier: domain.TierStrong,
			Status: domain.CallOK, TotalTokens: 100, DurationMS: 30},
		{RunID: "run-1", TenantID: tenantA, Provider: "mock", Model: "mock-reasoner-v1", Tier: domain.TierFallback,
			Status: domain.CallFallback, FallbackFrom: "strong-model", TotalTokens: 50, DurationMS: 10},
		{RunID: "run-9", TenantID: tenantB, Provider: "mock", Model: "other-model", Status: domain.CallOK, TotalTokens: 7},
	} {
		call.StartedAt = base.Add(time.Duration(i) * time.Second)
		rec.ModelCall(call)
	}

	sum, err := rec.Observability(ctx, tenantA)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if sum.TasksTotal != 2 || sum.RunsTotal != 4 {
		t.Fatalf("任务/运行总数错误: tasks=%d runs=%d", sum.TasksTotal, sum.RunsTotal)
	}
	if sum.RunsSucceeded != 1 || sum.RunsNeedsReview != 1 || sum.RunsFailed != 1 || sum.RunsDegraded != 1 {
		t.Fatalf("状态计数错误: %+v", sum)
	}
	if math.Abs(sum.FixRate-1.0/4.0) > 1e-9 {
		t.Fatalf("修复率错误: %v", sum.FixRate)
	}
	if sum.AvgElapsedMS != 2000 {
		t.Fatalf("平均耗时错误: %d", sum.AvgElapsedMS)
	}
	if sum.SkillCalls != 4 || sum.SkillFailures != 1 || math.Abs(sum.SkillFailureRate-0.25) > 1e-9 {
		t.Fatalf("技能指标错误: calls=%d failures=%d rate=%v", sum.SkillCalls, sum.SkillFailures, sum.SkillFailureRate)
	}
	if sum.ModelCalls != 2 || sum.ModelTokens != 150 || sum.ModelFallbacks != 1 {
		t.Fatalf("模型指标错误: calls=%d tokens=%d fallbacks=%d", sum.ModelCalls, sum.ModelTokens, sum.ModelFallbacks)
	}
	if sum.CacheHits != 2 {
		t.Fatalf("缓存命中错误: %d", sum.CacheHits)
	}
	if sum.RepoSwitches != 4 {
		t.Fatalf("仓库切换数错误: %d", sum.RepoSwitches)
	}
	if len(sum.TopSkills) != 2 {
		t.Fatalf("Top 技能数量错误: %+v", sum.TopSkills)
	}
	if sum.TopSkills[0].Skill != "patch_verify" || sum.TopSkills[0].Calls != 2 || sum.TopSkills[0].AvgLatencyMS != 200 {
		t.Fatalf("Top 技能内容错误: %+v", sum.TopSkills[0])
	}
	if sum.TopSkills[1].Skill != "stacktrace_parse" || sum.TopSkills[1].Failures != 1 || sum.TopSkills[1].AvgLatencyMS != 150 {
		t.Fatalf("Top 技能内容错误: %+v", sum.TopSkills[1])
	}
	if len(sum.TopModels) != 1 || sum.TopModels[0].Model != "mock-reasoner-v1" ||
		sum.TopModels[0].Calls != 2 || sum.TopModels[0].TotalTokens != 150 || sum.TopModels[0].AvgLatencyMS != 20 {
		t.Fatalf("Top 模型内容错误: %+v", sum.TopModels)
	}
	if sum.StateDist[string(domain.StateSucceeded)] != 1 || sum.StateDist[string(domain.StateFailed)] != 1 ||
		sum.StateDist[string(domain.StateDegraded)] != 1 || sum.StateDist[string(domain.StateNeedsReview)] != 1 {
		t.Fatalf("状态分布错误: %+v", sum.StateDist)
	}
	if sum.CategoryDist["logic"] != 2 || sum.CategoryDist["dependency"] != 1 || sum.CategoryDist["config"] != 1 {
		t.Fatalf("根因分类分布错误: %+v", sum.CategoryDist)
	}
	if len(sum.RecentFailures) != 2 {
		t.Fatalf("最近失败运行数量错误（应含 failed 与 degraded）: %+v", sum.RecentFailures)
	}
	// 校验降级运行（run-4）也进入了最近失败/需关注列表。
	var fail3, fail4 domain.RunBrief
	found3, found4 := false, false
	for _, r := range sum.RecentFailures {
		if r.RunID == "run-3" {
			fail3, found3 = r, true
		}
		if r.RunID == "run-4" {
			fail4, found4 = r, true
		}
	}
	if !found3 || fail3.State != domain.StateFailed ||
		fail3.Severity != domain.SeverityCritical || fail3.Summary != "依赖版本冲突" {
		t.Fatalf("最近失败运行内容错误: %+v", sum.RecentFailures)
	}
	if !found4 || fail4.State != domain.StateDegraded {
		t.Fatalf("降级运行应进入最近失败列表: %+v", sum.RecentFailures)
	}

	// 租户隔离：t-2 只看到自己的数据。
	sumB, err := rec.Observability(ctx, tenantB)
	if err != nil {
		t.Fatalf("租户 B 汇总失败: %v", err)
	}
	if sumB.TasksTotal != 1 || sumB.RunsTotal != 1 || sumB.RunsSucceeded != 1 {
		t.Fatalf("租户 B 运行统计错误: %+v", sumB)
	}
	if sumB.SkillCalls != 1 || sumB.ModelCalls != 1 || sumB.ModelTokens != 7 || sumB.CacheHits != 9 {
		t.Fatalf("租户 B 调用统计错误: %+v", sumB)
	}
	if len(sumB.RecentFailures) != 0 || sumB.RepoSwitches != 0 {
		t.Fatalf("租户 B 不应包含其他租户数据: %+v", sumB)
	}
	if sumB.FixRate != 1 || sumB.AvgElapsedMS != 1000 {
		t.Fatalf("租户 B 修复率/耗时错误: %v %d", sumB.FixRate, sumB.AvgElapsedMS)
	}

	// 无数据的租户：全零且不 panic。
	empty, err := rec.Observability(ctx, "t-empty")
	if err != nil {
		t.Fatalf("空租户汇总失败: %v", err)
	}
	if empty.RunsTotal != 0 || empty.FixRate != 0 || empty.AvgElapsedMS != 0 ||
		len(empty.TopSkills) != 0 || len(empty.TopModels) != 0 || len(empty.RecentFailures) != 0 {
		t.Fatalf("空租户汇总应为零值: %+v", empty)
	}
	if empty.StateDist == nil || empty.CategoryDist == nil {
		t.Fatal("空租户的分布 map 不应为 nil")
	}
	// 最近失败按时间倒序，最多 5 条。
	for i := 0; i < 7; i++ {
		run := domain.TaskRun{
			ID: fmt.Sprintf("run-fail-%d", i), TaskID: "task-1", TenantID: tenantA, State: domain.StateFailed,
			Title: "失败运行", CreatedAt: base, StartedAt: base,
			EndedAt: base.Add(time.Duration(i) * time.Minute), UpdatedAt: base, Error: "boom",
		}
		seedRun(t, st, run)
	}
	sum2, err := rec.Observability(ctx, tenantA)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if len(sum2.RecentFailures) != 5 {
		t.Fatalf("最近失败运行应截断为 5 条: %d", len(sum2.RecentFailures))
	}
	if sum2.RecentFailures[0].RunID != "run-fail-6" {
		t.Fatalf("最近失败运行未按时间倒序: %+v", sum2.RecentFailures)
	}
	if sum2.RecentFailures[0].Summary != "boom" {
		t.Fatalf("失败摘要应回退到 Error: %+v", sum2.RecentFailures[0])
	}
}

func TestObservabilityTopLimit(t *testing.T) {
	rec, st, _ := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	// 7 个技能：skill-0 调用 7 次 …… skill-6 调用 1 次。
	for i := 0; i < 7; i++ {
		for n := 0; n < 7-i; n++ {
			rec.SkillCall(domain.SkillCall{
				TenantID: tenantA, RunID: "run-1", Skill: fmt.Sprintf("skill-%d", i),
				Status: domain.CallOK, DurationMS: 10, StartedAt: base,
			})
		}
	}
	// 6 个模型：model-0 调用 6 次 …… model-5 调用 1 次。
	for i := 0; i < 6; i++ {
		for n := 0; n < 6-i; n++ {
			rec.ModelCall(domain.ModelCall{
				TenantID: tenantA, RunID: "run-1", Provider: "mock", Model: fmt.Sprintf("model-%d", i),
				Status: domain.CallOK, TotalTokens: 1, StartedAt: base,
			})
		}
	}
	_ = st

	sum, err := rec.Observability(ctx, tenantA)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if len(sum.TopSkills) != topLimit || len(sum.TopModels) != topLimit {
		t.Fatalf("Top 榜单应截断为 %d 条: %d/%d", topLimit, len(sum.TopSkills), len(sum.TopModels))
	}
	if sum.TopSkills[0].Skill != "skill-0" || sum.TopSkills[0].Calls != 7 {
		t.Fatalf("Top 技能排序错误: %+v", sum.TopSkills[0])
	}
	if sum.TopSkills[topLimit-1].Skill != "skill-4" || sum.TopSkills[topLimit-1].Calls != 3 {
		t.Fatalf("Top 技能截断错误: %+v", sum.TopSkills[topLimit-1])
	}
	if sum.TopModels[0].Model != "model-0" || sum.TopModels[0].Calls != 6 {
		t.Fatalf("Top 模型排序错误: %+v", sum.TopModels[0])
	}
	if sum.SkillCalls != 28 || sum.ModelCalls != 21 {
		t.Fatalf("调用总数错误: skill=%d model=%d", sum.SkillCalls, sum.ModelCalls)
	}
}
