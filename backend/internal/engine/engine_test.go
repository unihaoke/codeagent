package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 调度核心测试用 fake 流水线
// ---------------------------------------------------------------------------

// scriptedPipeline domain.TaskPipeline 的 fake：可控延时、阻塞与结果。
type scriptedPipeline struct {
	mu     sync.Mutex
	delay  time.Duration
	block  chan struct{}
	state  domain.TaskState
	onRun  func(run *domain.TaskRun)
	err    error
	called int
}

func (p *scriptedPipeline) Execute(ctx context.Context, cc *domain.CallContext, run *domain.TaskRun) (*domain.PipelineResult, error) {
	p.mu.Lock()
	p.called++
	delay, block, target, onRun, pipeErr := p.delay, p.block, p.state, p.onRun, p.err
	p.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if pipeErr != nil {
		return nil, pipeErr
	}
	if onRun != nil {
		onRun(run)
	}
	if target == "" {
		target = domain.StateSucceeded
	}
	run.State = target
	run.RootCause = &domain.RootCause{Summary: "fake 根因", Category: "runtime", Confidence: 0.9, Severity: domain.SeverityMajor}
	return &domain.PipelineResult{Run: run, Evidence: &domain.EvidenceBundle{RunID: run.ID}, State: target, Warnings: run.Warnings}, nil
}

func (p *scriptedPipeline) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called
}

// newEngineForTest 构造带租户与仓库的引擎并启动。
func newEngineForTest(t *testing.T, cfg *config.Config, pipe domain.TaskPipeline, quota domain.TenantQuota) (*Engine, store.Store) {
	t.Helper()
	st := store.New()
	if err := st.CreateTenant(&domain.Tenant{
		ID: testTenantID, Name: "示例租户", Status: domain.TenantActive, Quota: quota,
	}); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	if err := st.CreateRepo(javaRepo(testRepoID, testRepoKey)); err != nil {
		t.Fatalf("创建仓库失败: %v", err)
	}
	eng, err := NewEngine(cfg, st, pipe, &fakeRecorder{}, &fakeBus{}, nil, logx.Nop())
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	return eng, st
}

func submitSubject() *domain.Subject {
	return &domain.Subject{TenantID: testTenantID, Scopes: []string{"task:write"}, KeyPrefix: "ca_test"}
}

func submitReq(stacktrace, idemKey string) domain.CreateTaskRequest {
	return domain.CreateTaskRequest{
		Mode: domain.ModeSingleRepo, RepoID: testRepoID, Ref: "main",
		Source: domain.TriggerManual, Title: "线上 NPE", Stacktrace: stacktrace,
		IdempotencyKey: idemKey, RequestID: "req-1",
	}
}

// waitFor 轮询等待条件成立（测试专用，避免依赖固定 sleep 造成偶发失败）。
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", desc)
}

// ---------------------------------------------------------------------------
// 幂等
// ---------------------------------------------------------------------------

func TestSubmitIdempotentReusesRun(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 2
	cfg.Engine.QueueSize = 8
	pipe := &scriptedPipeline{delay: 50 * time.Millisecond}
	eng, st := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	req := submitReq("boom-1", "idem-key-1")
	r1, err := eng.Submit(context.Background(), submitSubject(), req)
	if err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	r2, err := eng.Submit(context.Background(), submitSubject(), req)
	if err != nil {
		t.Fatalf("二次提交失败: %v", err)
	}
	if r1.ID != r2.ID {
		t.Errorf("幂等命中应复用同一运行：%s != %s", r1.ID, r2.ID)
	}
	if runs := st.AllRuns(testTenantID); len(runs) != 1 {
		t.Errorf("CreateRun 应只发生一次，实际落库 %d 条运行记录", len(runs))
	}
	if tasks := st.ListTasks(testTenantID, domain.PageQuery{}); tasks.Total != 1 {
		t.Errorf("应只创建一个逻辑任务，实际 %d 个", tasks.Total)
	}

	// 幂等键为空时由 sha256 自动生成，相同入参同样复用。
	auto1, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-auto", ""))
	if err != nil {
		t.Fatalf("自动幂等键提交失败: %v", err)
	}
	auto2, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-auto", ""))
	if err != nil {
		t.Fatalf("自动幂等键二次提交失败: %v", err)
	}
	if auto1.ID != auto2.ID || auto1.IdempotencyKey == "" || len(auto1.IdempotencyKey) != 32 {
		t.Errorf("自动幂等键不正确：%q，runId %s/%s", auto1.IdempotencyKey, auto1.ID, auto2.ID)
	}
}

// TestSubmitEchoesCallerIdempotencyKey 守住跨系统对号契约。
//
// 调用方（如 nightjar）提交时带出去的 idempotencyKey 会作为它本地的任务主键，
// 本服务必须原样落到运行上并在终态回调里回显，调用方才对得上号。
// 曾经 Submit 把 req.IdempotencyKey 丢掉（传了空 submitOpts），退化成 sha256
// 自生成的 32 位十六进制键，回调方只能报"任务不存在"。
func TestSubmitEchoesCallerIdempotencyKey(t *testing.T) {
	const callerKey = "mwo-1-abc"
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		select {
		case got <- m:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Engine.Workers = 1
	eng, st := newEngineForTest(t, cfg, &scriptedPipeline{state: domain.StateSucceeded}, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	req := submitReq("boom-idem-echo", callerKey)
	req.CallbackURL = srv.URL
	run, err := eng.Submit(context.Background(), submitSubject(), req)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if run.IdempotencyKey != callerKey {
		t.Fatalf("运行的幂等键必须是调用方传入的原值，实际 %q", run.IdempotencyKey)
	}
	if _, ok := st.FindRunByIdempotencyKey(testTenantID, callerKey); !ok {
		t.Fatalf("幂等索引应按调用方键命中：%q", callerKey)
	}

	var payload map[string]any
	select {
	case payload = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到终态回调")
	}
	if payload["idempotencyKey"] != callerKey {
		t.Errorf("回调必须原样回显调用方幂等键，实际 %v", payload["idempotencyKey"])
	}
	if payload["runId"] != run.ID {
		t.Errorf("回调 runId 错误: %v", payload["runId"])
	}
}

// ---------------------------------------------------------------------------
// 配额
// ---------------------------------------------------------------------------

func TestSubmitQuotaExceeded(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 4
	block := make(chan struct{})
	pipe := &scriptedPipeline{block: block}
	quota := domain.DefaultQuota()
	quota.MaxConcurrentTasks = 1
	eng, _ := newEngineForTest(t, cfg, pipe, quota)
	defer func() { _ = eng.Stop() }()

	first, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-1", "k1"))
	if err != nil {
		t.Fatalf("首个任务提交失败: %v", err)
	}
	waitFor(t, 2*time.Second, "首个任务进入执行", func() bool { return eng.RunningCount() == 1 })

	if _, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-2", "k2")); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("超出并发配额应返回 ErrQuotaExceeded，实际 %v", err)
	}

	close(block)
	waitFor(t, 3*time.Second, "首个任务结束", func() bool {
		run, ok := eng.GetRun(context.Background(), testTenantID, first.ID)
		return ok == nil && run.State.IsTerminal()
	})
}

func TestSubmitQueueFullFailsFast(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 1
	block := make(chan struct{})
	pipe := &scriptedPipeline{block: block}
	eng, st := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	if _, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-1", "q1")); err != nil {
		t.Fatalf("首个任务提交失败: %v", err)
	}
	waitFor(t, 2*time.Second, "首个任务进入执行", func() bool { return eng.RunningCount() == 1 })
	if _, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-2", "q2")); err != nil {
		t.Fatalf("第二个任务应进入队列: %v", err)
	}

	_, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-3", "q3"))
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("队列已满应返回 ErrQuotaExceeded，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "队列已满") {
		t.Errorf("队列已满应有中文说明: %v", err)
	}
	// 队列满的任务必须以 failed 终态落库，不能停留在 queued。
	found := false
	for _, r := range st.AllRuns(testTenantID) {
		if r.Stacktrace == "boom-3" {
			found = true
			if r.State != domain.StateFailed {
				t.Errorf("队列满的任务应标记 failed，实际 %s", r.State)
			}
		}
	}
	if !found {
		t.Error("队列满的任务未落库")
	}
	close(block)
}

// TestReapStaleRunsReleasesQuota 验证僵尸运行会被回收，从而释放租户并发额度。
//
// 背景：countActive 把 queued/analyzing/repairing/verifying 全部计入占用，进程重启
// 遗留的非终态记录会让额度永久泄漏，表现为"跑一段时间后提交一直报配额上限"。
func TestReapStaleRunsReleasesQuota(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 4
	cfg.Engine.TaskTimeoutSec = 60 // 缩短超时窗口，避免测试依赖真实的 900s
	eng, st := newEngineForTest(t, cfg, &scriptedPipeline{}, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	// 僵尸：远超（任务超时 + 宽限）未更新。
	stale := &domain.TaskRun{
		ID: "run-stale", TaskID: "task-stale", TenantID: testTenantID,
		Attempt: 1, Mode: domain.ModeSingleRepo, State: domain.StateAnalyzing,
		Stacktrace: "stale-stack",
		CreatedAt:  time.Now().Add(-2 * time.Hour),
		UpdatedAt:  time.Now().Add(-2 * time.Hour),
	}
	if err := st.CreateRun(stale); err != nil {
		t.Fatalf("构造僵尸运行失败: %v", err)
	}
	// 进行中：刚更新过，不应被误回收。
	fresh := &domain.TaskRun{
		ID: "run-fresh", TaskID: "task-fresh", TenantID: testTenantID,
		Attempt: 1, Mode: domain.ModeSingleRepo, State: domain.StateAnalyzing,
		Stacktrace: "fresh-stack",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := st.CreateRun(fresh); err != nil {
		t.Fatalf("构造进行中运行失败: %v", err)
	}

	if n := eng.reapStaleRuns(); n != 1 {
		t.Fatalf("应回收 1 条僵尸运行，实际 %d", n)
	}
	got, ok := st.GetRunRaw("run-stale")
	if !ok {
		t.Fatal("僵尸运行记录丢失")
	}
	if got.State != domain.StateFailed {
		t.Errorf("僵尸运行应置 failed，实际 %s", got.State)
	}
	if got.EndedAt.IsZero() {
		t.Error("回收后的运行应写入 EndedAt")
	}
	still, ok := st.GetRunRaw("run-fresh")
	if !ok || still.State != domain.StateAnalyzing {
		t.Errorf("进行中的运行不应被回收，实际 %+v", still)
	}
}

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

func TestCancelRunningTask(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 4
	block := make(chan struct{})
	pipe := &scriptedPipeline{block: block}
	eng, _ := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	run, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-cancel", "c1"))
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitFor(t, 2*time.Second, "任务进入执行", func() bool { return eng.RunningCount() == 1 })

	if err := eng.Cancel(context.Background(), testTenantID, run.ID); err != nil {
		t.Fatalf("取消任务失败: %v", err)
	}
	waitFor(t, 2*time.Second, "任务状态变为 cancelled", func() bool {
		got, err := eng.GetRun(context.Background(), testTenantID, run.ID)
		return err == nil && got.State == domain.StateCancelled
	})

	// 释放流水线后，终态必须保持 cancelled，不能被成功结果覆盖。
	close(block)
	time.Sleep(100 * time.Millisecond)
	got, err := eng.GetRun(context.Background(), testTenantID, run.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.State != domain.StateCancelled {
		t.Errorf("取消后的终态应保持 cancelled，实际 %s", got.State)
	}
	// 已终态任务再次取消应返回冲突。
	if err := eng.Cancel(context.Background(), testTenantID, run.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("已终态任务取消应返回 ErrConflict，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 重跑
// ---------------------------------------------------------------------------

func TestRetryReusesPinnedCommits(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 8
	pinned := map[string]string{testRepoID: "commit-pinned-1"}
	pipe := &scriptedPipeline{
		state: domain.StateSucceeded,
		onRun: func(run *domain.TaskRun) {
			run.PinnedCommits = map[string]string{testRepoID: pinned[testRepoID]}
			run.Resolution = []domain.RepoResolved{{RepositoryID: testRepoID, RepoKey: testRepoKey,
				Commit: pinned[testRepoID], ResolvedFrom: "request"}}
		},
	}
	eng, _ := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	origin, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-retry", "r1"))
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitFor(t, 3*time.Second, "首次运行结束", func() bool {
		got, err := eng.GetRun(context.Background(), testTenantID, origin.ID)
		return err == nil && got.State.IsTerminal()
	})

	// 未结束的运行不可重跑（此处已结束，构造一个执行中的运行做反向校验）。
	block := make(chan struct{})
	pipe.mu.Lock()
	pipe.block = block
	pipe.delay = 0
	pipe.mu.Unlock()
	running, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-running", "r2"))
	if err != nil {
		t.Fatalf("提交执行中任务失败: %v", err)
	}
	waitFor(t, 2*time.Second, "任务进入执行", func() bool { return eng.RunningCount() == 1 })
	if _, err := eng.Retry(context.Background(), testTenantID, running.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("执行中的任务重跑应返回 ErrConflict，实际 %v", err)
	}
	close(block)
	waitFor(t, 3*time.Second, "执行中任务结束", func() bool {
		got, err := eng.GetRun(context.Background(), testTenantID, running.ID)
		return err == nil && got.State.IsTerminal()
	})

	// 正式重跑：必须复用锁定的 commit，且 attempt 递增。
	retried, err := eng.Retry(context.Background(), testTenantID, origin.ID)
	if err != nil {
		t.Fatalf("重跑失败: %v", err)
	}
	if retried.Attempt != 2 {
		t.Errorf("重跑应递增 attempt 为 2，实际 %d", retried.Attempt)
	}
	if retried.PinnedCommits[testRepoID] != pinned[testRepoID] {
		t.Errorf("重跑必须复用原 PinnedCommits，期望 %v，实际 %v", pinned, retried.PinnedCommits)
	}
	if retried.IdempotencyKey == origin.IdempotencyKey {
		t.Error("重跑必须生成新的幂等键，避免自我去重")
	}
	if retried.TaskID != origin.TaskID {
		t.Errorf("重跑应复用同一逻辑任务，%s != %s", retried.TaskID, origin.TaskID)
	}

	// 人工反馈重跑：反馈写入 Warnings 并作为新幂等键输入。
	fb, err := eng.RerunWithFeedback(context.Background(), testTenantID, origin.ID, "请重点检查 dao 的注入路径")
	if err != nil {
		t.Fatalf("人工反馈重跑失败: %v", err)
	}
	joined := strings.Join(fb.Warnings, " | ")
	if !strings.Contains(joined, "人工反馈：请重点检查 dao 的注入路径") {
		t.Errorf("反馈未写入 Warnings: %v", fb.Warnings)
	}
	if ExtractHumanFeedback(fb) != "请重点检查 dao 的注入路径" {
		t.Errorf("反馈提取失败: %q", ExtractHumanFeedback(fb))
	}
	if fb.IdempotencyKey == retried.IdempotencyKey {
		t.Error("带反馈的重跑必须生成不同的幂等键")
	}
	if _, err := eng.RerunWithFeedback(context.Background(), testTenantID, origin.ID, "  "); !errors.Is(err, ErrBadRequest) {
		t.Errorf("空反馈应返回参数错误，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 受理校验
// ---------------------------------------------------------------------------

func TestSubmitValidationAndAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 4
	eng, _ := newEngineForTest(t, cfg, &scriptedPipeline{}, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	ctx := context.Background()

	// 1) 无 scope 的主体 → 403
	noScope := &domain.Subject{TenantID: testTenantID, Scopes: []string{"repo:read"}}
	if _, err := eng.Submit(ctx, noScope, submitReq("s1", "v1")); !errors.Is(err, ErrForbidden) {
		t.Errorf("缺少 task:write 应返回 ErrForbidden，实际 %v", err)
	}

	// 2) 匿名提交且未开启 → 401
	if _, err := eng.Submit(ctx, nil, submitReq("s2", "v2")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("未开启匿名时应返回 ErrUnauthorized，实际 %v", err)
	}

	// 3) 非法模式 / 缺少目标 → 400
	bad := submitReq("s3", "v3")
	bad.Mode = domain.ModeSingleRepo
	bad.RepoID = ""
	if _, err := eng.Submit(ctx, submitSubject(), bad); !errors.Is(err, ErrBadRequest) {
		t.Errorf("缺少 repoId 应返回 ErrBadRequest，实际 %v", err)
	}
	var ve *ValidationError
	if _, err := eng.Submit(ctx, submitSubject(), bad); !errors.As(err, &ve) || ve.Field != "repoId" {
		t.Errorf("应返回 *ValidationError{Field:repoId}，实际 %v", err)
	}

	// 4) 仓库不存在 / 不属于该租户 → 404
	missing := submitReq("s4", "v4")
	missing.RepoID = "repo-not-exist"
	if _, err := eng.Submit(ctx, submitSubject(), missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("仓库不存在应返回 ErrNotFound，实际 %v", err)
	}

	// 5) 分组为空 → 400；分组存在但无成员 → 400
	groupReq := domain.CreateTaskRequest{Mode: domain.ModeGroup, GroupID: "group-x", Stacktrace: "s5", IdempotencyKey: "v5"}
	if _, err := eng.Submit(ctx, submitSubject(), groupReq); !errors.Is(err, ErrNotFound) {
		t.Errorf("分组不存在应返回 ErrNotFound，实际 %v", err)
	}

	// 6) 已终态/不存在的运行查询 → 404
	if _, err := eng.GetRun(ctx, testTenantID, "run-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("查询不存在的运行应返回 ErrNotFound，实际 %v", err)
	}
	// 7) 跨租户查询 → 404（租户隔离）
	if _, err := eng.GetRun(ctx, "other-tenant", "run-x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("跨租户查询应返回 ErrNotFound，实际 %v", err)
	}
}

func TestSubmitAnonymousAllowed(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.AllowAnonymous = true
	cfg.Engine.Workers = 1
	eng, _ := newEngineForTest(t, cfg, &scriptedPipeline{}, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	req := submitReq("anon-1", "anon-key")
	req.TenantID = testTenantID
	run, err := eng.Submit(context.Background(), nil, req)
	if err != nil {
		t.Fatalf("开启匿名后应允许提交: %v", err)
	}
	if run.TenantID != testTenantID {
		t.Errorf("匿名任务租户错误: %s", run.TenantID)
	}
}

// ---------------------------------------------------------------------------
// 统计与状态机
// ---------------------------------------------------------------------------

func TestBuildEngineStatsAndOverviewByDay(t *testing.T) {
	st := store.New()
	now := time.Now()
	mkRun := func(id string, state domain.TaskState, sev domain.Severity, created time.Time, elapsed time.Duration, repos int, degraded bool) {
		run := &domain.TaskRun{
			ID: id, TaskID: "task-" + id, TenantID: testTenantID, Mode: domain.ModeSingleRepo,
			State: state, Severity: sev, CreatedAt: created, UpdatedAt: created,
		}
		if elapsed > 0 {
			run.StartedAt = created
			run.EndedAt = created.Add(elapsed)
		}
		for i := 0; i < repos; i++ {
			run.Resolution = append(run.Resolution, domain.RepoResolved{RepositoryID: "repo", Commit: "c"})
		}
		run.Degraded = degraded
		if err := st.CreateRun(run); err != nil {
			t.Fatalf("创建运行失败: %v", err)
		}
	}
	mkRun("r1", domain.StateSucceeded, domain.SeverityMajor, now, time.Second, 2, false)
	mkRun("r2", domain.StateDegraded, domain.SeverityMinor, now, 3*time.Second, 1, true)
	mkRun("r3", domain.StateAnalyzing, domain.SeverityCritical, now, 0, 0, false)

	stats, err := BuildEngineStats(st, testTenantID)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if stats.Total != 3 || stats.Running != 1 || stats.Queued != 0 {
		t.Errorf("Running/Queued/Total 错误: %+v", stats)
	}
	if stats.ByState[domain.StateSucceeded] != 1 || stats.ByState[domain.StateDegraded] != 1 || stats.ByState[domain.StateAnalyzing] != 1 {
		t.Errorf("ByState 错误: %+v", stats.ByState)
	}
	if stats.BySeverity[domain.SeverityMajor] != 1 || stats.BySeverity[domain.SeverityMinor] != 1 || stats.BySeverity[domain.SeverityCritical] != 1 {
		t.Errorf("BySeverity 错误: %+v", stats.BySeverity)
	}
	if stats.SuccessRate < 0.333 || stats.SuccessRate > 0.334 {
		t.Errorf("SuccessRate 应为 1/3，实际 %v", stats.SuccessRate)
	}
	if stats.AvgElapsedMS != 2000 {
		t.Errorf("AvgElapsedMS 应为 2000（仅统计已结束运行），实际 %d", stats.AvgElapsedMS)
	}
	if stats.RepoSwitches != 3 {
		t.Errorf("RepoSwitches 应为 3，实际 %d", stats.RepoSwitches)
	}
	if stats.DegradedRuns != 1 {
		t.Errorf("DegradedRuns 应为 1，实际 %d", stats.DegradedRuns)
	}
	if _, err := BuildEngineStats(nil, testTenantID); err == nil {
		t.Error("store 为空时应返回错误")
	}

	// 趋势桶：近 3 天，含无数据的空桶（时间锚定到本地零点，避免跨越自然日导致抖动）。
	midnight := time.Now()
	midnight = time.Date(midnight.Year(), midnight.Month(), midnight.Day(), 0, 0, 0, 0, midnight.Location())
	todayAt := midnight.Add(time.Hour)
	yesterdayAt := midnight.AddDate(0, 0, -1).Add(time.Hour)
	runs := []domain.TaskRun{
		{ID: "d1", TenantID: testTenantID, State: domain.StateSucceeded, CreatedAt: todayAt},
		{ID: "d2", TenantID: testTenantID, State: domain.StateFailed, CreatedAt: todayAt.Add(time.Minute)},
		{ID: "d3", TenantID: testTenantID, State: domain.StateDegraded, CreatedAt: yesterdayAt},
		{ID: "d4", TenantID: testTenantID, State: domain.StateSucceeded, CreatedAt: midnight.AddDate(0, 0, -10)},
	}
	buckets := OverviewByDay(runs, 3)
	if len(buckets) != 3 {
		t.Fatalf("应返回 3 个趋势桶，实际 %d", len(buckets))
	}
	if buckets[0].Total != 0 {
		t.Errorf("前天应无数据，实际 %+v", buckets[0])
	}
	if buckets[1].Total != 1 || buckets[1].Degraded != 1 {
		t.Errorf("昨天桶错误: %+v", buckets[1])
	}
	today := buckets[2]
	if today.Total != 2 || today.Succeeded != 1 || today.Failed != 1 {
		t.Errorf("今日桶错误: %+v", today)
	}
	if buckets[2].Date == "" || len(buckets[2].Date) != 10 {
		t.Errorf("日期格式应为 2006-01-02，实际 %q", buckets[2].Date)
	}
	if empty := OverviewByDay(nil, 0); len(empty) != 7 {
		t.Errorf("days<=0 时应默认 7 天，实际 %d", len(empty))
	}
}

func TestTaskStateMachineTransitions(t *testing.T) {
	cases := []struct {
		from, to domain.TaskState
		ok       bool
	}{
		{domain.StateQueued, domain.StateAnalyzing, true},
		{domain.StateQueued, domain.StateCancelled, true},
		{domain.StateQueued, domain.StateFailed, true},
		{domain.StateQueued, domain.StateSucceeded, false},
		{domain.StateQueued, domain.StateVerifying, false},
		{domain.StateAnalyzing, domain.StateRepairing, true},
		{domain.StateAnalyzing, domain.StateDegraded, true},
		{domain.StateAnalyzing, domain.StateSucceeded, false},
		{domain.StateRepairing, domain.StateVerifying, true},
		{domain.StateRepairing, domain.StateSucceeded, false},
		{domain.StateVerifying, domain.StateSucceeded, true},
		{domain.StateVerifying, domain.StateNeedsReview, true},
		{domain.StateVerifying, domain.StateDegraded, true},
		{domain.StateVerifying, domain.StateFailed, true},
		{domain.StateVerifying, domain.StateCancelled, true},
		{domain.StateVerifying, domain.StateQueued, false},
		{domain.StateSucceeded, domain.StateAnalyzing, false},
		{domain.StateSucceeded, domain.StateFailed, false},
		{domain.StateFailed, domain.StateAnalyzing, false},
		{domain.StateCancelled, domain.StateAnalyzing, false},
		{domain.StateDegraded, domain.StateAnalyzing, false},
		{domain.StateNeedsReview, domain.StateSucceeded, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.ok {
			t.Errorf("CanTransitionTo(%s → %s) = %v，期望 %v", c.from, c.to, got, c.ok)
		}
	}

	// 引擎按最短路推进状态机：analyzing → succeeded 必须经过 repairing/verifying。
	path, ok := statePath(domain.StateAnalyzing, domain.StateSucceeded)
	if !ok {
		t.Fatal("analyzing → succeeded 应存在合法路径")
	}
	want := []domain.TaskState{domain.StateRepairing, domain.StateVerifying, domain.StateSucceeded}
	if len(path) != len(want) {
		t.Fatalf("状态路径错误：期望 %v，实际 %v", want, path)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Fatalf("状态路径错误：期望 %v，实际 %v", want, path)
		}
	}
	if _, ok := statePath(domain.StateSucceeded, domain.StateAnalyzing); ok {
		t.Error("终态不应再迁移")
	}
	if p, ok := statePath(domain.StateQueued, domain.StateCancelled); !ok || len(p) != 1 {
		t.Errorf("queued → cancelled 应为一跳，实际 %v %v", p, ok)
	}
}

func TestTerminalCallback(t *testing.T) {
	type captured struct {
		body map[string]any
		ct   string
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		select {
		case got <- captured{body: m, ct: r.Header.Get("Content-Type")}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Engine.Workers = 1
	eng, _ := newEngineForTest(t, cfg, &scriptedPipeline{state: domain.StateNeedsReview}, domain.DefaultQuota())
	defer func() { _ = eng.Stop() }()

	req := submitReq("boom-callback", "cb1")
	req.CallbackURL = srv.URL
	run, err := eng.Submit(context.Background(), submitSubject(), req)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	var payload map[string]any
	select {
	case c := <-got:
		payload = c.body
		if c.ct != "application/json" {
			t.Errorf("回调 Content-Type 应为 application/json，实际 %q", c.ct)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到终态回调")
	}
	if payload["runId"] != run.ID {
		t.Errorf("回调 runId 错误: %v", payload["runId"])
	}
	if payload["state"] != string(domain.StateNeedsReview) {
		t.Errorf("回调 state 错误: %v", payload["state"])
	}
	for _, k := range []string{"state", "runId", "severity", "summary"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("回调报文缺少字段 %s: %v", k, payload)
		}
	}
}

// ---------------------------------------------------------------------------
// 生命周期幂等
// ---------------------------------------------------------------------------

func TestStartStopIdempotent(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	pipe := &scriptedPipeline{}
	eng, _ := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())

	if err := eng.Start(context.Background()); err != nil {
		t.Errorf("重复 Start 应幂等: %v", err)
	}
	if err := eng.Stop(); err != nil {
		t.Errorf("Stop 失败: %v", err)
	}
	if err := eng.Stop(); err != nil {
		t.Errorf("重复 Stop 应幂等: %v", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Errorf("停止后 Start 应安全返回: %v", err)
	}
}

func TestStopFailsQueuedRuns(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.Workers = 1
	cfg.Engine.QueueSize = 4
	block := make(chan struct{})
	pipe := &scriptedPipeline{block: block}
	eng, st := newEngineForTest(t, cfg, pipe, domain.DefaultQuota())

	first, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-s1", "s1"))
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitFor(t, 2*time.Second, "首个任务进入执行", func() bool { return eng.RunningCount() == 1 })
	queued, err := eng.Submit(context.Background(), submitSubject(), submitReq("boom-s2", "s2"))
	if err != nil {
		t.Fatalf("提交排队任务失败: %v", err)
	}

	close(block)
	waitFor(t, 3*time.Second, "在途任务结束", func() bool {
		run, err := eng.GetRun(context.Background(), testTenantID, first.ID)
		return err == nil && run.State.IsTerminal()
	})
	if err := eng.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}

	// 停止时仍在排队的任务必须被标记为 failed，不能停留在 queued。
	got, ok := st.GetRun(testTenantID, queued.ID)
	if !ok {
		t.Fatal("查询失败：运行记录不存在")
	}
	if !got.State.IsTerminal() {
		t.Errorf("停止后排队任务应进入终态，实际 %s", got.State)
	}
}
