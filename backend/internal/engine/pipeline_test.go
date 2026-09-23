package engine

import (
	"context"
	"errors"
	"fmt"
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
// fake 端口实现（domain.* 各端口的最小可控替身）
// ---------------------------------------------------------------------------

// fakeSource domain.SourceResolver 的 fake：版本锁定与代码加载全部可控。
type fakeSource struct {
	mu         sync.Mutex
	commit     string
	resolveErr error
	loadErr    error
	slices     map[string][]domain.CodeSlice

	resolveCalls int
	loadRepos    []string
}

func (f *fakeSource) Resolve(ctx context.Context, ref domain.RepoRef) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolveCalls++
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if f.commit == "" {
		return "commit-default", nil
	}
	return f.commit, nil
}

func (f *fakeSource) Load(ctx context.Context, ref domain.RepoRef, hints []domain.FileHint) ([]domain.CodeSlice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref.Repository != nil {
		f.loadRepos = append(f.loadRepos, ref.Repository.ID)
	}
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if ref.Repository == nil {
		return nil, nil
	}
	return append([]domain.CodeSlice{}, f.slices[ref.Repository.ID]...), nil
}

func (f *fakeSource) ReadFile(ctx context.Context, ref domain.RepoRef, path string) (*domain.CodeSlice, error) {
	return nil, store.ErrNotFound
}

func (f *fakeSource) Tree(ctx context.Context, ref domain.RepoRef) ([]domain.FileNode, error) {
	return nil, nil
}

func (f *fakeSource) Grep(ctx context.Context, ref domain.RepoRef, pattern string, limit int) ([]domain.FileHint, error) {
	return nil, nil
}

func (f *fakeSource) Invalidate(ctx context.Context, tenantID, repoID string) error { return nil }

func (f *fakeSource) Stats() domain.CacheStats { return domain.CacheStats{} }

func (f *fakeSource) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.loadRepos)
}

// fakeSkills domain.SkillRunner 的 fake：按技能名注册处理函数并统计调用次数。
type fakeSkills struct {
	mu       sync.Mutex
	calls    map[string]int
	inputs   map[string]map[string]any
	handlers map[string]func(in map[string]any) (*domain.SkillResult, error)
}

func newFakeSkills() *fakeSkills {
	return &fakeSkills{
		calls:    map[string]int{},
		inputs:   map[string]map[string]any{},
		handlers: map[string]func(in map[string]any) (*domain.SkillResult, error){},
	}
}

func (f *fakeSkills) on(skill string, h func(in map[string]any) (*domain.SkillResult, error)) *fakeSkills {
	f.mu.Lock()
	f.handlers[skill] = h
	f.mu.Unlock()
	return f
}

func (f *fakeSkills) Run(ctx context.Context, cc *domain.CallContext, inv domain.SkillInvocation) (*domain.SkillResult, error) {
	f.mu.Lock()
	f.calls[inv.Skill]++
	f.inputs[inv.Skill] = inv.Input
	h := f.handlers[inv.Skill]
	f.mu.Unlock()
	if h == nil {
		return &domain.SkillResult{Skill: inv.Skill, Status: domain.CallOK, Output: map[string]any{}}, nil
	}
	return h(inv.Input)
}

func (f *fakeSkills) count(skill string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[skill]
}

func (f *fakeSkills) input(skill string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inputs[skill]
}

// fakeMCP domain.MCPGateway 的 fake：可控返回结构化根因或错误。
type fakeMCP struct {
	mu         sync.Mutex
	structured map[string]any
	err        error
	calls      int
	lastReq    domain.ModelRequest
}

func (f *fakeMCP) Chat(ctx context.Context, cc *domain.CallContext, req domain.ModelRequest) (*domain.ModelResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	structured, err := f.structured, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &domain.ModelResponse{
		Structured: structured,
		Target:     domain.ModelTarget{Provider: "fake", Model: "fake-strong", Tier: domain.TierStrong},
		Usage:      domain.ModelUsage{PromptTokens: 120, CompletionTokens: 60, TotalTokens: 180},
		Attempts:   1,
		DurationMS: 12,
	}, nil
}

func (f *fakeMCP) Providers() []domain.ModelTarget { return nil }

func (f *fakeMCP) Health(ctx context.Context) []domain.ModelHealth { return nil }

func (f *fakeMCP) Stats(ctx context.Context) domain.ModelStats { return domain.ModelStats{} }

func (f *fakeMCP) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeHandle domain.SandboxHandle 的 fake。
type fakeHandle struct{ id, root string }

func (h *fakeHandle) ID() string                { return h.id }
func (h *fakeHandle) Root() string              { return h.root }
func (h *fakeHandle) RepoDir(key string) string { return h.root + "/" + key }
func (h *fakeHandle) Close() error              { return nil }

// fakeSandbox domain.SandboxManager 的 fake。
type fakeSandbox struct {
	mu          sync.Mutex
	openErr     error
	validateErr error
	result      *domain.Verification
	validates   int
	lastMeta    domain.SandboxMeta
}

func (f *fakeSandbox) Open(ctx context.Context, meta domain.SandboxMeta) (domain.SandboxHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMeta = meta
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &fakeHandle{id: "ws-1", root: "/tmp/ws-1"}, nil
}

func (f *fakeSandbox) Validate(ctx context.Context, handle domain.SandboxHandle, req domain.ValidateRequest) (*domain.Verification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validates++
	if f.validateErr != nil {
		return nil, f.validateErr
	}
	if f.result == nil {
		return &domain.Verification{Passed: true, ApplyResult: "applied"}, nil
	}
	cp := *f.result
	cp.Checks = append([]domain.VerificationCheck{}, f.result.Checks...)
	return &cp, nil
}

func (f *fakeSandbox) validateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.validates
}

// fakePacker domain.ContextPacker 的 fake。
type fakePacker struct {
	mu       sync.Mutex
	packed   domain.ContextPacked
	err      error
	packCall int
}

func (f *fakePacker) Pack(bundle *domain.EvidenceBundle, budget domain.ContextBudget) (domain.ContextPacked, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packCall++
	if f.err != nil {
		return domain.ContextPacked{}, f.err
	}
	text := f.packed.Text
	if text == "" {
		text = fmt.Sprintf("## 代码上下文\n切片数=%d，预算=%d 字符\n", len(bundle.Slices), budget.MaxChars)
	}
	return domain.ContextPacked{Text: text, Chars: len(text)}, nil
}

// fakeBus domain.EventBus 的 fake：记录全部事件供断言。
type fakeBus struct {
	mu     sync.Mutex
	events []domain.Event
}

func (b *fakeBus) Publish(ev domain.Event) {
	b.mu.Lock()
	b.events = append(b.events, ev)
	b.mu.Unlock()
}

func (b *fakeBus) Subscribe(tenantID string) (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, 16)
	return ch, func() { close(ch) }
}

func (b *fakeBus) types() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.events))
	for _, e := range b.events {
		out = append(out, e.Type)
	}
	return out
}

func (b *fakeBus) hasType(t string) bool {
	for _, v := range b.types() {
		if v == t {
			return true
		}
	}
	return false
}

// fakeRecorder domain.Recorder 的 fake：记录审计与调用轨迹。
type fakeRecorder struct {
	mu     sync.Mutex
	audits []domain.AuditEvent
	skills []domain.SkillCall
	models []domain.ModelCall
}

func (r *fakeRecorder) Audit(ev domain.AuditEvent) {
	r.mu.Lock()
	r.audits = append(r.audits, ev)
	r.mu.Unlock()
}

func (r *fakeRecorder) SkillCall(call domain.SkillCall) {
	r.mu.Lock()
	r.skills = append(r.skills, call)
	r.mu.Unlock()
}

func (r *fakeRecorder) ModelCall(call domain.ModelCall) {
	r.mu.Lock()
	r.models = append(r.models, call)
	r.mu.Unlock()
}

func (r *fakeRecorder) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	return domain.Page[domain.AuditEvent]{}, nil
}

func (r *fakeRecorder) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	return domain.Page[domain.SkillCall]{}, nil
}

func (r *fakeRecorder) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	return domain.Page[domain.ModelCall]{}, nil
}

func (r *fakeRecorder) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	return &domain.ObservabilitySummary{}, nil
}

// stageActions 返回阶段埋点的 Action 序列（Recorder 无 Stage 方法，阶段埋点走 Audit）。
func (r *fakeRecorder) stageActions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, a := range r.audits {
		if a.Category == "stage" {
			out = append(out, a.Action)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

const (
	testTenantID = "tenant-1"
	testRepoID   = "repo-1"
	testRepoKey  = "order-service"
	testFilePath = "src/main/java/com/acme/order/OrderService.java"
	testCommit   = "commit-abc123"
)

// testStack Java NPE 堆栈：包名与 order-service 的匹配规则一致。
const testStack = `java.lang.NullPointerException: Cannot invoke "com.acme.order.OrderDao.save(Object)" because "this.dao" is null
	at com.acme.order.OrderService.saveOrder(OrderService.java:42)
	at com.acme.order.OrderController.create(OrderController.java:88)`

// javaRepo 构造带匹配规则的 Java 仓库。
func javaRepo(id, key string) *domain.Repository {
	return &domain.Repository{
		ID: id, TenantID: testTenantID, Key: key, Name: key,
		URL: "https://git.example.com/" + key, DefaultBranch: "main",
		Provider: domain.VCSGit, Layer: domain.LayerService, Language: "java",
		Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{
			PackagePrefixes: []string{"com.acme.order"},
			PathPrefixes:    []string{"src/main/java/com/acme/order"},
			ArtifactNames:   []string{"order-service.jar"},
			HostPatterns:    []string{"order-svc"},
		},
	}
}

// javaContent 生成 60 行 Java 源码（第 42 行为待修复语句，第 43 行为空校验缺口）。
func javaContent() string {
	lines := make([]string, 0, 60)
	lines = append(lines,
		"package com.acme.order;",
		"",
		"import org.springframework.stereotype.Service;",
		"",
		"@Service",
		"public class OrderService {",
		"    private OrderDao dao;",
		"",
	)
	for i := len(lines) + 1; i < 42; i++ {
		lines = append(lines, fmt.Sprintf("    // filler line %d", i))
	}
	lines = append(lines,
		"        dao.save(order);", // 第 42 行
		"    }",
		"}",
	)
	for i := len(lines) + 1; i <= 60; i++ {
		lines = append(lines, fmt.Sprintf("    // tail line %d", i))
	}
	return strings.Join(lines, "\n") + "\n"
}

func newTestStore(t *testing.T, repos ...*domain.Repository) *store.Store {
	t.Helper()
	st := store.New()
	if err := st.CreateTenant(&domain.Tenant{
		ID: testTenantID, Name: "示例租户", Status: domain.TenantActive, Quota: domain.DefaultQuota(),
	}); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	for _, r := range repos {
		if err := st.CreateRepo(r); err != nil {
			t.Fatalf("创建仓库失败: %v", err)
		}
	}
	return st
}

func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Engine.AutoFix = true
	cfg.Engine.MaxRepairRounds = 1
	return cfg
}

// newStackParseSkills 构造返回 Java NPE 解析结果的技能 fake。
func newStackParseSkills() *fakeSkills {
	sk := newFakeSkills()
	sk.on(domain.SkillStacktraceParse, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{
			Skill: domain.SkillStacktraceParse, Version: "1.0.0", Status: domain.CallOK,
			Output: map[string]any{
				"language": "java",
				"stack": map[string]any{
					"language":      "java",
					"exceptionType": "java.lang.NullPointerException",
					"message":       `Cannot invoke "com.acme.order.OrderDao.save(Object)" because "this.dao" is null`,
					"category":      "null_pointer",
					"cleanedLog":    testStack,
					"frames": []any{
						map[string]any{"raw": "at com.acme.order.OrderService.saveOrder(OrderService.java:42)",
							"package": "com.acme.order", "class": "OrderService", "method": "saveOrder",
							"file": testFilePath, "line": 42, "language": "java", "confidence": 0.9},
						map[string]any{"raw": "at com.acme.order.OrderController.create(OrderController.java:88)",
							"package": "com.acme.order", "class": "OrderController", "method": "create",
							"file": "src/main/java/com/acme/order/OrderController.java", "line": 88,
							"language": "java", "confidence": 0.8},
					},
					"symbols":   []any{"OrderService", "saveOrder"},
					"fileHints": []any{"OrderService.java"},
				},
			},
		}, nil
	})
	sk.on(domain.SkillCodeASTAnalyze, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{
			Skill: domain.SkillCodeASTAnalyze, Status: domain.CallOK,
			Output: map[string]any{
				"symbols": []any{map[string]any{"path": testFilePath, "symbol": "saveOrder"}},
				"imports": []any{"org.springframework.stereotype.Service"},
				"risks":   []any{"dao 字段未做空值校验"},
			},
		}, nil
	})
	return sk
}

// patchDescOutput 生成一个合法的补丁描述（oldSnippet 命中真实代码）。
func patchDescOutput(oldSnippet, newSnippet string) map[string]any {
	return map[string]any{
		"patches": []any{
			map[string]any{
				"repositoryId": testRepoID,
				"repoKey":      testRepoKey,
				"filePath":     testFilePath,
				"action":       "modify",
				"line":         42,
				"oldSnippet":   oldSnippet,
				"newSnippet":   newSnippet,
				"rationale":    "补充空值校验，避免 NPE",
				"risk":         "medium",
				"confidence":   0.8,
			},
		},
	}
}

func newPipelineDeps(st *store.Store, cfg *config.Config, src domain.SourceResolver, sk domain.SkillRunner,
	mcp domain.MCPGateway, sb domain.SandboxManager, packer domain.ContextPacker,
	bus domain.EventBus, rec domain.Recorder) Deps {
	return Deps{Store: st, Cfg: cfg, Source: src, Skills: sk, MCP: mcp, Sandbox: sb,
		Packer: packer, Bus: bus, Recorder: rec, Log: logx.Nop()}
}

// newSingleRepoRun 构造单仓库运行上下文。
func newSingleRepoRun(stack string) *domain.TaskRun {
	return &domain.TaskRun{
		ID: "run-1", TaskID: "task-1", TenantID: testTenantID,
		Attempt: 1, Mode: domain.ModeSingleRepo, State: domain.StateAnalyzing,
		Title: "线上 NPE 修复", Environment: "prod", Stacktrace: stack,
		RepoIDs: []string{testRepoID}, CreatedAt: time.Now(),
	}
}

// assertStageOrder 校验 Recorder 收到的阶段名按 CONTRACT §3 的顺序完整出现。
func assertStageOrder(t *testing.T, actions []string) {
	t.Helper()
	idx := 0
	for _, got := range actions {
		if idx < len(MainStages) && got == MainStages[idx] {
			idx++
		}
	}
	if idx != len(MainStages) {
		t.Fatalf("阶段序列不完整：期望按顺序包含 %v，实际收到 %v（匹配到 %d 个）",
			MainStages, actions, idx)
	}
}

// ---------------------------------------------------------------------------
// 用例 1：单仓库 + Java NPE + 技能产出补丁 + 沙箱通过 → succeeded
// ---------------------------------------------------------------------------

func TestPipelineSingleRepoSuccess(t *testing.T) {
	content := javaContent()
	oldSnippet := "        dao.save(order);"
	newSnippet := "        if (dao != null) {\n            dao.save(order);\n        }"

	st := newTestStore(t, javaRepo(testRepoID, testRepoKey))
	src := &fakeSource{
		commit: testCommit,
		slices: map[string][]domain.CodeSlice{
			testRepoID: {{
				RepositoryID: testRepoID, RepoKey: testRepoKey, Commit: testCommit,
				Path: testFilePath, Language: "java", Content: content,
				StartLine: 1, EndLine: 60, FocusLines: []int{42},
				Reason: "frame", Chars: len(content),
			}},
		},
	}
	sk := newStackParseSkills()
	sk.on(domain.SkillCodePatchSynthesize, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillCodePatchSynthesize, Status: domain.CallOK,
			Output: patchDescOutput(oldSnippet, newSnippet)}, nil
	})
	mcp := &fakeMCP{structured: map[string]any{
		"summary":     "OrderService.saveOrder 未校验 dao 导致空指针",
		"category":    "runtime",
		"detail":      "第 42 行直接调用 dao.save，缺少空值校验。",
		"confidence":  0.86,
		"evidence":    []any{"order-service " + testFilePath + ":42"},
		"blastRadius": []any{"order-service", "/api/order/create"},
		"severity":    "major",
	}}
	sb := &fakeSandbox{result: &domain.Verification{
		Passed: true, ApplyResult: "applied",
		Checks: []domain.VerificationCheck{{Name: "compile", Passed: true, Command: "mvn -q -o compile", DurationMS: 1200}},
	}}
	bus := &fakeBus{}
	rec := &fakeRecorder{}
	cfg := testConfig()

	run := newSingleRepoRun(testStack)
	p := NewPipeline(newPipelineDeps(st, cfg, src, sk, mcp, sb, &fakePacker{}, bus, rec))

	res, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run)
	if err != nil {
		t.Fatalf("流水线执行失败: %v", err)
	}
	if res == nil || res.Run == nil || res.Evidence == nil {
		t.Fatalf("流水线返回值不完整: %+v", res)
	}

	if got := run.PinnedCommits[testRepoID]; got != testCommit {
		t.Errorf("PinnedCommits 未被填充，期望 %s，实际 %q", testCommit, got)
	}
	if len(run.Resolution) != 1 || run.Resolution[0].ResolvedFrom != "repo_default" {
		t.Errorf("Resolution 不正确: %+v", run.Resolution)
	}
	if len(run.Patches) != 1 {
		t.Fatalf("期望 1 个补丁，实际 %d 个（warnings=%v）", len(run.Patches), run.Warnings)
	}
	patch := run.Patches[0]
	if !strings.Contains(patch.UnifiedDiff, "@@") {
		t.Errorf("补丁缺少 unified diff 内容: %q", patch.UnifiedDiff)
	}
	if patch.Status != domain.PatchApplied {
		t.Errorf("验证通过后补丁状态应为 applied，实际 %s", patch.Status)
	}
	if patch.Risk != domain.RiskMedium {
		t.Errorf("补丁风险等级应为 medium，实际 %s", patch.Risk)
	}
	if run.Verification == nil || !run.Verification.Passed {
		t.Fatalf("沙箱验证结果不正确: %+v", run.Verification)
	}
	if run.Degraded {
		t.Errorf("正常路径不应降级，warnings=%v", run.Warnings)
	}
	if got := DecideState(run); got != domain.StateSucceeded {
		t.Errorf("终态判定错误：期望 succeeded，实际 %s（warnings=%v）", got, run.Warnings)
	}
	if len(res.Evidence.Slices) == 0 {
		t.Error("EvidenceBundle.Slices 不应为空")
	}
	if res.Evidence.Stack == nil || res.Evidence.Stack.Category != "null_pointer" {
		t.Errorf("EvidenceBundle.Stack 不正确: %+v", res.Evidence.Stack)
	}
	if run.Severity != domain.SeverityMajor {
		t.Errorf("定级应为 major，实际 %s", run.Severity)
	}

	assertStageOrder(t, rec.stageActions())
	if !bus.hasType("repo.switch") || !bus.hasType("source.fetch") || !bus.hasType("patch.proposed") ||
		!bus.hasType("verification") || !bus.hasType("report.ready") || !bus.hasType("task.terminal") {
		t.Errorf("事件总线缺少关键事件: %v", bus.types())
	}

	if res.Report == nil {
		t.Fatal("未生成报告")
	}
	if run.ReportID == "" {
		t.Error("run.ReportID 未回填")
	}
	if _, ok := st.GetReport(testTenantID, run.ReportID); !ok {
		t.Error("报告未落库")
	}
	if !strings.Contains(res.Report.Markdown, "```diff") {
		t.Error("Markdown 报告缺少 diff 代码块")
	}
	if !strings.Contains(res.Report.Markdown, "## 七、证据与命中代码切片") {
		t.Error("Markdown 报告缺少证据章节")
	}
}

// ---------------------------------------------------------------------------
// 用例 2：候选仓库无命中 → 降级为堆栈文本分析
// ---------------------------------------------------------------------------

func TestPipelineDegradesWhenNoCandidateHit(t *testing.T) {
	// 分组下两个仓库，匹配规则与堆栈包名完全无关（仅分层/语言加成 0.10，低于阈值 0.15）。
	repoA := &domain.Repository{
		ID: "repo-a", TenantID: testTenantID, Key: "billing-service", Name: "billing-service",
		DefaultBranch: "main", Layer: domain.LayerService, Language: "java", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"com.acme.billing"}},
	}
	repoB := &domain.Repository{
		ID: "repo-b", TenantID: testTenantID, Key: "user-center", Name: "user-center",
		DefaultBranch: "main", Layer: domain.LayerService, Language: "java", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"com.acme.user"}},
	}
	st := newTestStore(t, repoA, repoB)
	if err := st.CreateGroup(&domain.RepositoryGroup{
		ID: "group-1", TenantID: testTenantID, Key: "trade", Name: "交易域", Status: domain.GroupActive,
		CreatedAt: time.Now(),
	}, []domain.GroupMember{
		{RepositoryID: "repo-a", Order: 1}, {RepositoryID: "repo-b", Order: 2},
	}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	if err := st.CreateTask(&domain.Task{
		ID: "task-2", TenantID: testTenantID, Title: "其他包异常", Mode: domain.ModeGroup,
		GroupID: "group-1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	otherStack := `java.lang.IllegalStateException: boom
	at com.other.module.OtherService.run(OtherService.java:10)`
	src := &fakeSource{commit: testCommit}
	sk := newStackParseSkills()
	// 堆栈包名与所有仓库的匹配规则都不相关，确保匹配阶段零命中。
	sk.on(domain.SkillStacktraceParse, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillStacktraceParse, Status: domain.CallOK,
			Output: map[string]any{
				"language": "java",
				"stack": map[string]any{
					"language":      "java",
					"exceptionType": "java.lang.IllegalStateException",
					"message":       "boom",
					"category":      "unknown",
					"cleanedLog":    otherStack,
					"frames": []any{
						map[string]any{"raw": "at com.other.module.OtherService.run(OtherService.java:10)",
							"package": "com.other.module", "class": "OtherService", "method": "run",
							"file": "OtherService.java", "line": 10, "language": "java", "confidence": 0.7},
					},
				},
			}}, nil
	})
	mcp := &fakeMCP{structured: map[string]any{
		"summary": "证据不足，无法定位", "category": "runtime", "detail": "无代码证据",
		"confidence": 0.2, "evidence": []any{}, "blastRadius": []any{}, "severity": "minor",
	}}
	sb := &fakeSandbox{}
	bus := &fakeBus{}
	rec := &fakeRecorder{}

	run := &domain.TaskRun{
		ID: "run-2", TaskID: "task-2", TenantID: testTenantID, Attempt: 1,
		Mode: domain.ModeGroup, State: domain.StateAnalyzing, Title: "分组降级",
		Stacktrace: otherStack, RepoIDs: []string{"repo-a", "repo-b"}, CreatedAt: time.Now(),
	}
	p := NewPipeline(newPipelineDeps(st, testConfig(), src, sk, mcp, sb, &fakePacker{}, bus, rec))
	res, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run)
	if err != nil {
		t.Fatalf("降级路径不应返回错误: %v", err)
	}

	if len(res.Evidence.CandidateRepos) != 0 {
		t.Errorf("不应匹配到候选仓库: %+v", res.Evidence.CandidateRepos)
	}
	if len(res.Evidence.Slices) != 0 {
		t.Errorf("降级时不应加载代码切片: %d 个", len(res.Evidence.Slices))
	}
	if src.loadCount() != 0 {
		t.Errorf("降级时不应调用 Source.Load，实际调用 %d 次", src.loadCount())
	}
	if sb.validateCount() != 0 {
		t.Errorf("降级时不应调用沙箱验证，实际调用 %d 次", sb.validateCount())
	}
	if len(run.Patches) != 0 {
		t.Errorf("降级时不应产出补丁: %d 个", len(run.Patches))
	}
	if len(run.Warnings) == 0 {
		t.Fatal("降级时必须写入 Warnings")
	}
	found := false
	for _, w := range run.Warnings {
		if strings.Contains(w, "堆栈线索不足") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings 缺少降级说明: %v", run.Warnings)
	}
	if got := DecideState(run); got != domain.StateDegraded {
		t.Errorf("终态判定错误：期望 degraded，实际 %s", got)
	}
	if res.Report == nil || !res.Report.Degraded {
		t.Error("报告应标记为降级")
	}
	// 入口仓库仍应完成版本锁定（懒加载原则：其余成员此时不加载）。
	if len(run.PinnedCommits) != 2 {
		t.Errorf("入口仓库应完成锁定，实际 PinnedCommits=%v", run.PinnedCommits)
	}
}

// ---------------------------------------------------------------------------
// 用例 3：沙箱验证失败 + MaxRepairRounds=1 → 收敛轮数受限，状态 needs_review
// ---------------------------------------------------------------------------

func TestPipelineRepairLoopIsBounded(t *testing.T) {
	content := javaContent()
	oldSnippet := "        dao.save(order);"
	newSnippet := "        requireNonNull(dao);\n        dao.save(order);"

	st := newTestStore(t, javaRepo(testRepoID, testRepoKey))
	src := &fakeSource{commit: testCommit, slices: map[string][]domain.CodeSlice{
		testRepoID: {{RepositoryID: testRepoID, RepoKey: testRepoKey, Commit: testCommit,
			Path: testFilePath, Language: "java", Content: content, StartLine: 1, EndLine: 60,
			FocusLines: []int{42}, Chars: len(content)}},
	}}
	sk := newStackParseSkills()
	sk.on(domain.SkillCodePatchSynthesize, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillCodePatchSynthesize, Status: domain.CallOK,
			Output: patchDescOutput(oldSnippet, newSnippet)}, nil
	})
	sb := &fakeSandbox{result: &domain.Verification{
		Passed: false, ApplyResult: "applied",
		Checks: []domain.VerificationCheck{{
			Name: "compile", Passed: false, Command: "mvn -q -o compile",
			Output: strings.Repeat("compile error: cannot find symbol\n", 200),
		}},
	}}
	cfg := testConfig()
	cfg.Engine.MaxRepairRounds = 1
	run := newSingleRepoRun(testStack)
	p := NewPipeline(newPipelineDeps(st, cfg, src, sk, &fakeMCP{structured: map[string]any{
		"summary": "空指针", "category": "runtime", "detail": "d", "confidence": 0.5,
		"evidence": []any{}, "blastRadius": []any{}, "severity": "major",
	}}, sb, &fakePacker{}, &fakeBus{}, &fakeRecorder{}))

	if _, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run); err != nil {
		t.Fatalf("流水线执行失败: %v", err)
	}

	if got := sk.count(domain.SkillCodePatchSynthesize); got != 2 {
		t.Errorf("补丁合成技能应被调用 2 次（首轮 + 1 轮收敛），实际 %d 次", got)
	}
	if got := sb.validateCount(); got != 2 {
		t.Errorf("沙箱验证应执行 2 次（首轮 + 1 轮收敛），实际 %d 次", got)
	}
	if got := DecideState(run); got != domain.StateNeedsReview {
		t.Errorf("终态判定错误：期望 needs_review，实际 %s", got)
	}
	if len(run.Patches) == 0 || run.Patches[0].Status != domain.PatchFailed {
		t.Errorf("收敛失败后补丁应保留并标记 failed: %+v", run.Patches)
	}
	joined := strings.Join(run.Warnings, " | ")
	if !strings.Contains(joined, "第 1 轮修复验证失败") {
		t.Errorf("Warnings 未记录收敛轮次结果: %v", run.Warnings)
	}
	// 反馈注入：模型提示中必须包含上一轮失败信息（输出截断到 2000 字符）。
	input := sk.input(domain.SkillCodePatchSynthesize)
	feedback, _ := input["feedback"].(string)
	if !strings.Contains(feedback, "compile") {
		t.Errorf("收敛轮次未注入上一轮验证失败信息: %q", feedback)
	}
	if len(feedback) > 2500 {
		t.Errorf("验证输出未按 2000 字符截断，长度 %d", len(feedback))
	}
}

// ---------------------------------------------------------------------------
// 用例 4：模型不可用 → 兜底根因 + 仍产出报告 + 标记降级
// ---------------------------------------------------------------------------

func TestPipelineModelUnavailableFallsBack(t *testing.T) {
	content := javaContent()
	st := newTestStore(t, javaRepo(testRepoID, testRepoKey))
	src := &fakeSource{commit: testCommit, slices: map[string][]domain.CodeSlice{
		testRepoID: {{RepositoryID: testRepoID, RepoKey: testRepoKey, Commit: testCommit,
			Path: testFilePath, Language: "java", Content: content, StartLine: 1, EndLine: 60,
			FocusLines: []int{42}, Chars: len(content)}},
	}}
	sk := newStackParseSkills()
	mcp := &fakeMCP{err: errors.New("模型服务不可用：connection refused")}
	run := newSingleRepoRun(testStack)
	p := NewPipeline(newPipelineDeps(st, testConfig(), src, sk, mcp, &fakeSandbox{}, &fakePacker{}, &fakeBus{}, &fakeRecorder{}))

	res, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run)
	if err != nil {
		t.Fatalf("模型不可用属于降级场景，不应返回错误: %v", err)
	}
	if !run.Degraded {
		t.Error("模型不可用时必须标记 run.Degraded")
	}
	if run.RootCause == nil {
		t.Fatal("模型不可用时必须生成兜底根因")
	}
	if !strings.Contains(run.RootCause.Summary, "模型推理不可用") ||
		!strings.Contains(run.RootCause.Summary, "NullPointerException") {
		t.Errorf("兜底根因格式不符合约定: %q", run.RootCause.Summary)
	}
	if run.RootCause.Confidence < 0 || run.RootCause.Confidence > 1 {
		t.Errorf("兜底根因置信度越界: %v", run.RootCause.Confidence)
	}
	if res.Report == nil || res.Report.Markdown == "" {
		t.Fatal("模型不可用时仍必须产出报告")
	}
	if _, ok := st.GetReportByRun(testTenantID, run.ID); !ok {
		t.Error("报告未落库")
	}
	if !strings.Contains(res.Report.Markdown, "降级") {
		t.Error("报告应包含降级警告章节")
	}
	joined := strings.Join(run.Warnings, " | ")
	if !strings.Contains(joined, "模型推理不可用") {
		t.Errorf("Warnings 应说明模型不可用: %v", run.Warnings)
	}
}

// ---------------------------------------------------------------------------
// 用例 5：补丁 oldSnippet 不是真实代码子串 → 丢弃且不产生假补丁
// ---------------------------------------------------------------------------

func TestPipelineDropsPatchWithUnknownSnippet(t *testing.T) {
	content := javaContent()
	st := newTestStore(t, javaRepo(testRepoID, testRepoKey))
	src := &fakeSource{commit: testCommit, slices: map[string][]domain.CodeSlice{
		testRepoID: {{RepositoryID: testRepoID, RepoKey: testRepoKey, Commit: testCommit,
			Path: testFilePath, Language: "java", Content: content, StartLine: 1, EndLine: 60,
			FocusLines: []int{42}, Chars: len(content)}},
	}}
	sk := newStackParseSkills()
	sk.on(domain.SkillCodePatchSynthesize, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillCodePatchSynthesize, Status: domain.CallOK,
			Output: patchDescOutput(
				"        imaginaryLineThatDoesNotExist();", // 不存在的原文片段
				"        fixed();")}, nil
	})
	sb := &fakeSandbox{}
	run := newSingleRepoRun(testStack)
	p := NewPipeline(newPipelineDeps(st, testConfig(), src, sk, &fakeMCP{structured: map[string]any{
		"summary": "空指针", "category": "runtime", "detail": "d", "confidence": 0.5,
		"evidence": []any{}, "blastRadius": []any{}, "severity": "major",
	}}, sb, &fakePacker{}, &fakeBus{}, &fakeRecorder{}))

	if _, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run); err != nil {
		t.Fatalf("流水线执行失败: %v", err)
	}
	if len(run.Patches) != 0 {
		t.Fatalf("伪造补丁必须被丢弃，实际产出 %d 个", len(run.Patches))
	}
	joined := strings.Join(run.Warnings, " | ")
	if !strings.Contains(joined, "snippet not found") {
		t.Errorf("Warnings 未说明丢弃原因: %v", run.Warnings)
	}
	if sb.validateCount() != 0 {
		t.Errorf("无补丁时不应调用沙箱验证，实际 %d 次", sb.validateCount())
	}
	if got := DecideState(run); got != domain.StateNeedsReview {
		t.Errorf("无补丁时的终态应为 needs_review，实际 %s", got)
	}
}

// ---------------------------------------------------------------------------
// 用例 6：分组模式 —— 入口仓库锁定 + 非入口仓库堆栈驱动懒加载 + 跨仓库链路
// ---------------------------------------------------------------------------

func TestPipelineGroupLazyLoadAndCrossRepo(t *testing.T) {
	content := javaContent()
	oldSnippet := "        dao.save(order);"
	newSnippet := "        if (dao == null) {\n            throw new IllegalStateException(\"dao 未注入\");\n        }\n        dao.save(order);"

	repoA := javaRepo("repo-a", testRepoKey)
	repoB := javaRepo("repo-b", "order-api")
	repoC := &domain.Repository{
		ID: "repo-c", TenantID: testTenantID, Key: "notify-service", Name: "notify-service",
		DefaultBranch: "main", Layer: domain.LayerService, Language: "java", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"com.acme.notify"}},
	}
	st := newTestStore(t, repoA, repoB, repoC)
	// 仅 repo-a 是显式入口仓库，repo-b 为成员前 2 个之一（隐式入口），repo-c 为普通成员。
	if err := st.CreateGroup(&domain.RepositoryGroup{
		ID: "group-1", TenantID: testTenantID, Key: "trade", Name: "交易域",
		Status: domain.GroupActive, EntryRepositoryIDs: []string{"repo-a"}, CreatedAt: time.Now(),
	}, []domain.GroupMember{
		{RepositoryID: "repo-a", Order: 1}, {RepositoryID: "repo-b", Order: 2}, {RepositoryID: "repo-c", Order: 3},
	}); err != nil {
		t.Fatalf("创建分组失败: %v", err)
	}
	if err := st.CreateTask(&domain.Task{
		ID: "task-3", TenantID: testTenantID, Title: "分组联合排查", Mode: domain.ModeGroup,
		GroupID: "group-1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	slice := domain.CodeSlice{
		RepositoryID: "repo-a", RepoKey: testRepoKey, Commit: testCommit,
		Path: testFilePath, Language: "java", Content: content,
		StartLine: 1, EndLine: 60, FocusLines: []int{42}, Chars: len(content),
	}
	src := &fakeSource{commit: testCommit, slices: map[string][]domain.CodeSlice{"repo-a": {slice}, "repo-b": {slice}}}
	sk := newStackParseSkills()
	sk.on(domain.SkillCrossRepoTrace, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillCrossRepoTrace, Status: domain.CallOK,
			Output: map[string]any{
				"edges": []any{map[string]any{
					"fromRepo": "order-api", "toRepo": testRepoKey, "protocol": "http",
					"endpoint": "/api/order/create", "confidence": 0.72,
					"evidence": "OrderController.create 调用 order-service",
				}},
				"chain": []any{"order-api", testRepoKey},
			}}, nil
	})
	sk.on(domain.SkillCodePatchSynthesize, func(in map[string]any) (*domain.SkillResult, error) {
		return &domain.SkillResult{Skill: domain.SkillCodePatchSynthesize, Status: domain.CallOK,
			Output: patchDescOutput(oldSnippet, newSnippet)}, nil
	})
	sb := &fakeSandbox{result: &domain.Verification{Passed: true, ApplyResult: "applied"}}
	rec := &fakeRecorder{}
	bus := &fakeBus{}

	run := &domain.TaskRun{
		ID: "run-3", TaskID: "task-3", TenantID: testTenantID, Attempt: 1,
		Mode: domain.ModeGroup, State: domain.StateAnalyzing, Title: "分组联合排查",
		Stacktrace: testStack, RepoIDs: []string{"repo-a", "repo-b", "repo-c"}, CreatedAt: time.Now(),
	}
	p := NewPipeline(newPipelineDeps(st, testConfig(), src, sk, &fakeMCP{structured: map[string]any{
		"summary": "order-service 未校验 dao", "category": "runtime", "detail": "d", "confidence": 0.7,
		"evidence": []any{}, "blastRadius": []any{"order-service"}, "severity": "major",
	}}, sb, &fakePacker{}, bus, rec))

	res, err := p.Execute(context.Background(), &domain.CallContext{TenantID: testTenantID, RunID: run.ID}, run)
	if err != nil {
		t.Fatalf("流水线执行失败: %v", err)
	}

	// 候选范围 = 分组成员全体，但只有命中堆栈的 a/b 进入懒加载范围。
	if len(res.Evidence.CandidateRepos) != 2 {
		t.Fatalf("期望 2 个候选仓库，实际 %+v", res.Evidence.CandidateRepos)
	}
	for _, c := range res.Evidence.CandidateRepos {
		if c.RepositoryID == "repo-c" {
			t.Error("未命中堆栈的成员仓库不应成为候选")
		}
	}

	// 入口仓库在阶段 1 锁定；非入口但命中的仓库在阶段 4 懒加载时锁定（resolvedFrom=stack_hint）。
	if run.PinnedCommits["repo-a"] != testCommit {
		t.Errorf("入口仓库应完成版本锁定: %v", run.PinnedCommits)
	}
	if len(run.Resolution) != 2 {
		t.Fatalf("期望 2 条版本锁定记录，实际 %+v", run.Resolution)
	}
	sources := map[string]string{}
	for _, r := range run.Resolution {
		sources[r.RepositoryID] = r.ResolvedFrom
	}
	if sources["repo-a"] != "group_entry" {
		t.Errorf("入口仓库 resolvedFrom 应为 group_entry，实际 %q", sources["repo-a"])
	}
	if sources["repo-b"] != "stack_hint" {
		t.Errorf("懒加载仓库 resolvedFrom 应为 stack_hint，实际 %q", sources["repo-b"])
	}

	// 跨仓库链路：技能输出的边与链路被采纳。
	if len(res.Evidence.Edges) != 1 || res.Evidence.Edges[0].ToRepo != testRepoKey {
		t.Errorf("跨仓库链路边未采纳: %+v", res.Evidence.Edges)
	}
	if !strings.Contains(strings.Join(res.Evidence.Notes, " "), "跨仓库调用链：order-api → "+testRepoKey) {
		t.Errorf("跨仓库链路节点未写入 Notes: %v", res.Evidence.Notes)
	}
	crossRepoAudited := false
	for _, action := range rec.stageActions() {
		if action == StageCrossRepo {
			crossRepoAudited = true
		}
	}
	if !crossRepoAudited {
		t.Errorf("cross_repo 阶段未落审计: %v", rec.stageActions())
	}

	// 补丁只落在已加载的真实代码上，验证通过 → succeeded。
	if len(run.Patches) != 1 || run.Patches[0].RepositoryID != "repo-a" {
		t.Errorf("补丁不正确: %+v", run.Patches)
	}
	if got := DecideState(run); got != domain.StateSucceeded {
		t.Errorf("终态应为 succeeded，实际 %s（warnings=%v）", got, run.Warnings)
	}
	if res.Report == nil || len(res.Report.ChainFlow) != 1 {
		t.Error("报告应包含跨仓库链路")
	}
}

// ---------------------------------------------------------------------------
// 附加：打分规则与终态判定单元测试
// ---------------------------------------------------------------------------

func TestScoreRepoRules(t *testing.T) {
	repo := javaRepo(testRepoID, testRepoKey)
	repo.MatchRules = domain.RepoMatchRules{
		PackagePrefixes:  []string{"com.acme.order"},
		PathPrefixes:     []string{"src/main/java/com/acme/order"},
		ArtifactNames:    []string{"order-service.jar"},
		HostPatterns:     []string{"order-svc"},
		EndpointPatterns: []string{"/api/order/**"},
		Keywords:         []string{"saveorder"},
	}
	stack := &domain.StackAnalysis{
		Language:      "java",
		Endpoints:     []string{"/api/order/create"},
		CleanedLog:    "order-svc 500",
		ExceptionType: "java.lang.NullPointerException",
		Frames: []domain.StackFrame{
			{Raw: "at com.acme.order.OrderService.saveOrder(OrderService.java:42)",
				Package: "com.acme.order", Class: "OrderService", Method: "saveOrder",
				File: testFilePath, Line: 42, Confidence: 0.9},
		},
	}
	run := &domain.TaskRun{Stacktrace: testStack, Logs: "order-svc 500 at /api/order/create, artifact=order-service.jar", EntryFiles: []string{testFilePath}}

	score, reasons, hints := scoreRepo(repo, run, stack, true)
	if score <= candidateThreshold {
		t.Fatalf("完整命中得分应高于阈值，实际 %.2f", score)
	}
	if score > 1.0 {
		t.Fatalf("得分必须归一化到 0-1，实际 %.2f", score)
	}
	joined := strings.Join(reasons, "；")
	for _, want := range []string{"包名前缀命中", "路径前缀命中", "类名与仓库 Key 相似", "制品名命中", "主机名命中", "接口路径命中", "分组入口仓库加权", "调用方嫌疑文件命中"} {
		if !strings.Contains(joined, want) {
			t.Errorf("命中原因缺少 %q：%s", want, joined)
		}
	}
	if len(hints) == 0 {
		t.Fatal("命中仓库应产出文件线索")
	}
	if hints[0].Reason != "frame" {
		t.Errorf("文件线索应按优先级降序（frame=0.95 优先），实际首条 %+v", hints[0])
	}
	hasEntry := false
	for i, h := range hints {
		if i > 0 && hints[i-1].Priority < h.Priority {
			t.Errorf("文件线索未按优先级降序: %+v", hints)
			break
		}
		if h.Reason == "entry" {
			hasEntry = true
		}
	}
	if !hasEntry {
		t.Errorf("调用方嫌疑文件应生成 entry 线索: %+v", hints)
	}

	// 语言一致但包名不匹配的仓库：仅语言加成，应低于阈值。
	other := &domain.Repository{ID: "repo-x", TenantID: testTenantID, Key: "unrelated", Name: "unrelated",
		Language: "java", Layer: domain.LayerService}
	if s, _, _ := scoreRepo(other, run, stack, false); s >= candidateThreshold {
		t.Errorf("无关仓库得分应低于阈值，实际 %.2f", s)
	}
}

func TestDecideStateRules(t *testing.T) {
	base := func() *domain.TaskRun {
		return &domain.TaskRun{
			Patches:      []domain.Patch{{ID: "p1", Risk: domain.RiskLow}},
			Verification: &domain.Verification{Passed: true},
		}
	}

	if got := DecideState(base()); got != domain.StateSucceeded {
		t.Errorf("验证通过且非降级应判定 succeeded，实际 %s", got)
	}

	failed := base()
	failed.Verification.Passed = false
	if got := DecideState(failed); got != domain.StateNeedsReview {
		t.Errorf("验证失败应判定 needs_review，实际 %s", got)
	}

	degradedVerify := base()
	degradedVerify.Verification.Degraded = true
	if got := DecideState(degradedVerify); got != domain.StateNeedsReview {
		t.Errorf("验证降级应判定 needs_review，实际 %s", got)
	}

	highRisk := base()
	highRisk.Patches[0].Risk = domain.RiskHigh
	if got := DecideState(highRisk); got != domain.StateNeedsReview {
		t.Errorf("high 风险补丁应判定 needs_review，实际 %s", got)
	}

	noPatch := &domain.TaskRun{Verification: &domain.Verification{Passed: true}}
	if got := DecideState(noPatch); got != domain.StateNeedsReview {
		t.Errorf("无补丁应判定 needs_review，实际 %s", got)
	}

	degraded := &domain.TaskRun{Degraded: true}
	if got := DecideState(degraded); got != domain.StateDegraded {
		t.Errorf("全程降级应判定 degraded，实际 %s", got)
	}

	fatal := &domain.TaskRun{Error: "panic", Degraded: true}
	if got := DecideState(fatal); got != domain.StateFailed {
		t.Errorf("致命异常应判定 failed，实际 %s", got)
	}
}
