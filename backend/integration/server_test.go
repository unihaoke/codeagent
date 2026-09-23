// Package integration 是**跨层端到端集成测试**：用真实实现装配全部七层
// （不使用任何 fake），在进程内起 HTTP 服务并跑通完整排查链路。
//
// 与 cmd/server 的装配保持一致，覆盖：
//   - 健康检查 / 就绪检查 / Prometheus 指标
//   - 控制台登录、接入层 API Key 认证
//   - 多租户隔离（acme 看不到 demo 的仓库）
//   - 单仓库精准修复：版本锁定 → 堆栈解析 → 匹配 → 懒加载 → 根因 → 补丁 → 沙箱验证 → 报告
//   - 多仓库分组联合排查：跨仓库链路
//   - 幂等：同 idempotencyKey 复用同一 run
//   - 调用轨迹与可观测汇总
//   - SSE 事件流可读
//
// 依赖外部 git 的用例在缺少 git 时自动跳过（源码层会降级，链路仍然完整）。
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/app"
	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/engine"
	"github.com/codeagent/backend/internal/env"
	"github.com/codeagent/backend/internal/mcp"
	"github.com/codeagent/backend/internal/platform/audit"
	"github.com/codeagent/backend/internal/platform/eventbus"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/sandbox"
	"github.com/codeagent/backend/internal/skill"
	"github.com/codeagent/backend/internal/skill/builtin"
	"github.com/codeagent/backend/internal/source"
	"github.com/codeagent/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 测试装置
// ---------------------------------------------------------------------------

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	store   *store.Store
	auth    *env.Auth
	engine  domain.TaskEngine
	apiKey  string
	jwt     string
	workDir string
	// commit 本地演示仓库的固定提交；git 不可用时为空串（源码层降级）。
	commit string
}

// newHarness 用真实实现装配全部层并启动进程内 HTTP 服务。
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	cfg.Server.RateLimitPerMin = 100000
	cfg.Store.Driver = "memory"
	cfg.Log.Level = "error" // 避免测试输出被日志淹没
	cfg.Source.CacheDir = filepath.Join(dir, "cache")
	cfg.Source.WorkspaceDir = filepath.Join(dir, "workspace")
	cfg.Source.CloneTimeoutSec = 30
	cfg.Sandbox.KeepWorkspace = false
	// 沙箱不允许联网，允许本地命令执行（Go 校验可真实生效）
	cfg.Sandbox.NetworkIsolated = true
	cfg.Sandbox.AllowCommandExec = true
	// 集成测试使用 Mock 推理，保证无外网环境下全链路可跑通。
	cfg.MCP.Mock.Enabled = true
	cfg.MCP.Mock.LatencyMS = 0
	cfg.Engine.Workers = 2
	cfg.Engine.TaskTimeoutSec = 120

	logLevel := os.Getenv("CA_TEST_LOG")
	if logLevel == "" {
		logLevel = "error"
	}
	log, _, err := logx.New(logLevel, "text", "")
	if err != nil {
		t.Fatalf("初始化日志失败: %v", err)
	}
	bus := eventbus.New(256)
	st := store.New()

	box, err := env.NewCredentialBox(cfg.Security)
	if err != nil {
		t.Fatalf("初始化凭证加密箱失败: %v", err)
	}
	auth := env.NewAuth(cfg, st)
	recorder := audit.New(st, bus)

	secrets := &secretProvider{st: st, box: box}
	gitMgr := source.NewGitRepoManager(cfg.Source, log)
	t.Cleanup(func() { _ = gitMgr.Close() })
	resolver := source.NewResolver(cfg.Source, st, secrets, log)
	workspace := sandbox.NewManager(cfg.Sandbox, cfg.Source, &materializer{resolver: resolver}, log)

	registry := skill.NewRegistry(cfg.Skill, log)
	if err := builtin.Register(registry, builtin.Deps{}); err != nil {
		t.Fatalf("注册内置技能失败: %v", err)
	}
	runner := skill.NewRunner(registry, cfg.Skill, recorder, log)

	gateway, err := mcp.NewGateway(cfg.MCP, cfg.Engine, recorder, log)
	if err != nil {
		t.Fatalf("初始化模型管控层失败: %v", err)
	}
	packer := mcp.NewContextPacker(cfg.MCP.Budget, log)

	pipeline := engine.NewPipeline(engine.Deps{
		Store: st, Cfg: cfg, Source: resolver, Skills: runner, MCP: gateway,
		Sandbox: workspace, Packer: packer, Bus: bus, Recorder: recorder, Log: log,
	})
	taskEngine, err := engine.NewEngine(cfg, st, pipeline, recorder, bus, auth, log)
	if err != nil {
		t.Fatalf("初始化任务引擎失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := taskEngine.Start(ctx); err != nil {
		t.Fatalf("启动任务引擎失败: %v", err)
	}

	srv, err := app.NewServer(app.Deps{
		Cfg: cfg, Store: st, Auth: auth, Engine: taskEngine, Source: resolver,
		Skills: registry, Runner: runner, MCP: gateway, Recorder: recorder,
		Secrets: box, Bus: bus, Log: log,
	})
	if err != nil {
		t.Fatalf("初始化接入层失败: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = taskEngine.Stop()
		cancel()
	})

	h := &harness{t: t, srv: ts, store: st, auth: auth, engine: taskEngine, workDir: dir}
	h.seed()
	return h
}

// seed 注入测试所需的租户、仓库、分组与密钥（不依赖 demo 镜像，全部指向本地临时仓库）。
func (h *harness) seed() {
	h.t.Helper()
	now := time.Now()
	for _, tn := range []*domain.Tenant{
		{ID: "t-demo", Name: "demo", Status: domain.TenantActive, Quota: domain.DefaultQuota(), CreatedAt: now, UpdatedAt: now},
		{ID: "t-acme", Name: "acme", Status: domain.TenantActive, Quota: domain.DefaultQuota(), CreatedAt: now, UpdatedAt: now},
	} {
		if err := h.store.CreateTenant(tn); err != nil {
			h.t.Fatalf("创建租户失败: %v", err)
		}
	}

	repoURL, commit := h.makeLocalRepo()
	order := &domain.Repository{
		ID: "repo-order-service", TenantID: "t-demo", Name: "order-service", Key: "order-service",
		URL: repoURL, DefaultBranch: "main", Provider: domain.VCSGit, Layer: domain.LayerService,
		Language: "java", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{
			PackagePrefixes:  []string{"com.acme.order"},
			PathPrefixes:     []string{"src/main/java/com/acme/order"},
			ArtifactNames:    []string{"order-service.jar"},
			HostPatterns:     []string{"order-svc", "order-service"},
			EndpointPatterns: []string{"/api/order/**"},
			Keywords:         []string{"订单", "order"},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	gateway := &domain.Repository{
		ID: "repo-api-gateway", TenantID: "t-demo", Name: "api-gateway", Key: "api-gateway",
		URL: "https://git.acme.internal/mall/api-gateway.git", DefaultBranch: "main",
		Provider: domain.VCSGit, Layer: domain.LayerGateway, Language: "go", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{
			HostPatterns:     []string{"api-gateway"},
			EndpointPatterns: []string{"/api/**"},
			Keywords:         []string{"gateway", "网关"},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	for _, r := range []*domain.Repository{order, gateway} {
		if err := h.store.CreateRepo(r); err != nil {
			h.t.Fatalf("创建仓库失败: %v", err)
		}
	}

	group := &domain.RepositoryGroup{
		ID: "grp-mall-core", TenantID: "t-demo", Name: "商城核心域", Key: "mall-core",
		Status: domain.GroupActive, EntryRepositoryIDs: []string{gateway.ID},
		CreatedAt: now, UpdatedAt: now,
	}
	members := []domain.GroupMember{
		{RepositoryID: gateway.ID, Order: 1},
		{RepositoryID: order.ID, Order: 2},
	}
	if err := h.store.CreateGroup(group, members); err != nil {
		h.t.Fatalf("创建分组失败: %v", err)
	}

	plain, _, err := h.auth.GenerateAPIKey("t-demo", "integration-test",
		[]string{"task:write", "repo:read", "group:read", "admin:all"}, 24*time.Hour)
	if err != nil {
		h.t.Fatalf("生成 API Key 失败: %v", err)
	}
	h.apiKey = plain
	h.commit = commit
}

// demoCommit 返回本地演示仓库的固定 commit（未创建时为 ""）。
func (h *harness) demoCommit() string { return h.commit }

// makeLocalRepo 在临时目录创建一个真实 git 仓库（含待修复缺陷代码）；
// 无 git 可执行文件时返回不可达 URL 并跳过依赖 git 的断言。
func (h *harness) makeLocalRepo() (string, string) {
	h.t.Helper()
	if _, err := lookGit(); err != nil {
		h.t.Logf("未发现 git，源码层将以降级模式运行: %v", err)
		return "https://git.acme.internal/mall/order-service.git", ""
	}
	work := filepath.Join(h.workDir, "order-service-src")
	srcDir := filepath.Join(work, "src", "main", "java", "com", "acme", "order", "service")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		h.t.Fatalf("创建目录失败: %v", err)
	}
	code := `package com.acme.order.service;

import com.acme.order.entity.Order;

public class OrderService {

    public OrderDetail toDetail(Order order) {
        OrderDetail detail = new OrderDetail();
        detail.setAmount(order.getAmount());
        detail.setStatus(order.getStatus());
        return detail;
    }
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "OrderService.java"), []byte(code), 0o644); err != nil {
		h.t.Fatalf("写入源码失败: %v", err)
	}
	runGit(h.t, work, "init", "-q")
	runGit(h.t, work, "checkout", "-q", "-b", "main")
	runGit(h.t, work, "config", "user.name", "CodeAgent Test")
	runGit(h.t, work, "config", "user.email", "test@acme.internal")
	runGit(h.t, work, "add", "-A")
	runGit(h.t, work, "commit", "-q", "-m", "test: baseline")
	commit := strings.TrimSpace(runGit(h.t, work, "rev-parse", "HEAD"))
	return filepath.ToSlash(work), commit
}

// ---------------------------------------------------------------------------
// HTTP 辅助
// ---------------------------------------------------------------------------

type envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"requestId"`
	Data      json.RawMessage `json:"data"`
}

func (h *harness) do(method, path string, body any, headers map[string]string) (int, envelope) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("序列化请求失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("请求失败 %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		h.t.Fatalf("解析响应失败 %s %s: %v", method, path, err)
	}
	return resp.StatusCode, env
}

func (h *harness) authHeaders() map[string]string {
	return map[string]string{"X-API-Key": h.apiKey}
}

func (h *harness) decode(env envelope, dst any) {
	h.t.Helper()
	if err := json.Unmarshal(env.Data, dst); err != nil {
		h.t.Fatalf("解析 data 失败: %v (raw=%s)", err, truncateRaw(env.Data))
	}
}

// decodeRun 解析运行详情响应。
//
// 接入层为便于前端一次拿到上下文，把运行详情包装为
// `{run, resolution, evidence, patches, skillCalls, modelCalls, report, ...}`；
// 这里同时兼容"直接返回 TaskRun"的形态（例如按 run 的其它读接口）。
func (h *harness) decodeRun(env envelope) domain.TaskRun {
	h.t.Helper()
	var wrapper struct {
		Run *domain.TaskRun `json:"run"`
	}
	if err := json.Unmarshal(env.Data, &wrapper); err == nil && wrapper.Run != nil {
		return *wrapper.Run
	}
	var run domain.TaskRun
	h.decode(env, &run)
	return run
}

// decodeReport 解析报告详情响应（兼容 `{report: {...}}` 与直接返回两种形态）。
func (h *harness) decodeReport(env envelope) domain.Report {
	h.t.Helper()
	var wrapper struct {
		Report *domain.Report `json:"report"`
	}
	if err := json.Unmarshal(env.Data, &wrapper); err == nil && wrapper.Report != nil {
		return *wrapper.Report
	}
	var rep domain.Report
	h.decode(env, &rep)
	return rep
}

func truncateRaw(raw []byte) string {
	const n = 400
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "..."
}

// waitRun 轮询任务直到终态。
func (h *harness) waitRun(runID string, timeout time.Duration) domain.TaskRun {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var last domain.TaskRun
	var lastStatus int
	var lastMsg string
	for time.Now().Before(deadline) {
		status, env := h.do(http.MethodGet, "/api/v1/runs/"+runID, nil, h.authHeaders())
		lastStatus, lastMsg = status, env.Message
		if env.Code != 0 || len(env.Data) == 0 || string(env.Data) == "null" {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		run := h.decodeRun(env)
		last = run
		if run.State.IsTerminal() {
			return run
		}
		time.Sleep(120 * time.Millisecond)
	}
	h.t.Fatalf("任务 %s 在 %s 内未进入终态：httpStatus=%d msg=%q state=%q warnings=%v err=%q",
		runID, timeout, lastStatus, lastMsg, last.State, last.Warnings, last.Error)
	return last
}

const npeStack = `2024-06-11 10:23:41.883 ERROR [order-service,8f2c1d9a4b7e6f01] 1 --- [http-nio-8080-exec-3] c.a.o.web.OrderController : 订单详情查询失败

java.lang.NullPointerException: Cannot invoke "com.acme.order.entity.Order.getAmount()" because "order" is null
	at com.acme.order.service.OrderService.toDetail(OrderService.java:8)
	at com.acme.order.service.OrderService.queryDetail(OrderService.java:6)
	at com.acme.order.web.OrderController.detail(OrderController.java:41)
Caused by: com.acme.common.exception.RemoteCallException: inventory-service 调用失败
	at com.acme.order.client.InventoryClient.query(InventoryClient.java:52)
	... 23 more
`

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestHealthReadyAndMetrics(t *testing.T) {
	h := newHarness(t)

	status, env := h.do(http.MethodGet, "/healthz", nil, nil)
	if status != http.StatusOK || env.Code != 0 {
		t.Fatalf("/healthz 期望 200/0，实际 %d/%d", status, env.Code)
	}

	resp, err := h.srv.Client().Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("请求 /metrics 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("读取 /metrics 失败: %v", err)
	}
	if !strings.Contains(buf.String(), "codeagent_") {
		t.Fatalf("/metrics 未包含 codeagent_ 前缀指标:\n%s", buf.String()[:min(400, buf.Len())])
	}
}

func TestAuthRequiredAndAPIKeyWorks(t *testing.T) {
	h := newHarness(t)

	// 无凭证 → 401
	if status, _ := h.do(http.MethodGet, "/api/v1/repos", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("未认证访问 /repos 期望 401，实际 %d", status)
	}
	// 带接入层密钥 → 200
	status, env := h.do(http.MethodGet, "/api/v1/repos", nil, h.authHeaders())
	if status != http.StatusOK || env.Code != 0 {
		t.Fatalf("API Key 访问 /repos 期望 200/0，实际 %d/%d（%s）", status, env.Code, env.Message)
	}
	var page domain.Page[domain.Repository]
	h.decode(env, &page)
	if page.Total != 2 {
		t.Fatalf("demo 租户应有 2 个仓库，实际 %d", page.Total)
	}

	// 控制台登录（默认密码 admin123）
	status, env = h.do(http.MethodPost, "/api/v1/auth/login",
		map[string]string{"tenantKey": "demo", "username": "admin", "password": "admin123"}, nil)
	if status != http.StatusOK || env.Code != 0 {
		t.Fatalf("控制台登录失败: %d/%d %s", status, env.Code, env.Message)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	acmeKey, _, err := h.auth.GenerateAPIKey("t-acme", "acme-test", []string{"repo:read", "task:write"}, time.Hour)
	if err != nil {
		t.Fatalf("生成 acme 密钥失败: %v", err)
	}
	status, env := h.do(http.MethodGet, "/api/v1/repos", nil, map[string]string{"X-API-Key": acmeKey})
	if status != http.StatusOK {
		t.Fatalf("acme 查询仓库期望 200，实际 %d", status)
	}
	var page domain.Page[domain.Repository]
	h.decode(env, &page)
	if page.Total != 0 {
		t.Fatalf("租户隔离失效：acme 看到了 %d 个 demo 仓库", page.Total)
	}

	// 直接用 demo 的 repoId 提交任务也必须被拒（403/404）
	status, _ = h.do(http.MethodPost, "/api/v1/tasks", domain.CreateTaskRequest{
		Mode: domain.ModeSingleRepo, RepoID: "repo-order-service", Stacktrace: npeStack,
	}, map[string]string{"X-API-Key": acmeKey})
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("跨租户提交任务期望 403/404，实际 %d", status)
	}
}

func TestSingleRepoEndToEnd(t *testing.T) {
	h := newHarness(t)

	status, env := h.do(http.MethodPost, "/api/v1/tasks", domain.CreateTaskRequest{
		Mode:        domain.ModeSingleRepo,
		RepoID:      "repo-order-service",
		Ref:         h.demoCommit(),
		Source:      domain.TriggerManual,
		Title:       "订单详情接口 NPE",
		Environment: "prod",
		Stacktrace:  npeStack,
		Logs:        "2024-06-11 10:23:41.812 WARN [order-service] InventoryClient host=inventory-svc timeout=2000ms",
	}, h.authHeaders())
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("提交任务期望 201/200，实际 %d（%s）", status, env.Message)
	}
	created := h.decodeRun(env)
	if created.ID == "" {
		t.Fatal("返回的 TaskRun 缺少 ID")
	}

	run := h.waitRun(created.ID, 90*time.Second)
	t.Logf("终态=%s 定级=%s 耗时=%dms 加载文件=%d 补丁=%d",
		run.State, run.Severity, run.ElapsedMS(), run.Usage.FilesLoaded, len(run.Patches))

	// 1) 根因必须产出（模型不可用时也应有兜底）
	if run.RootCause == nil || run.RootCause.Summary == "" {
		t.Fatalf("根因缺失: %+v", run.RootCause)
	}
	if run.RootCause.Category == "" {
		t.Fatal("根因分类为空")
	}
	// 2) 状态必须由 DecideState 规则产生，且处于合法终态
	switch run.State {
	case domain.StateSucceeded, domain.StateNeedsReview, domain.StateDegraded:
	default:
		t.Fatalf("非预期终态: %s (warnings=%v err=%s)", run.State, run.Warnings, run.Error)
	}
	// 3) 版本锁定：有 git 时必须以传入 commit 为准
	if h.demoCommit() != "" {
		if len(run.Resolution) == 0 {
			t.Fatal("缺少版本锁定结果")
		}
		if run.Resolution[0].Commit != h.demoCommit() {
			t.Fatalf("版本锁定错误：期望 %s 实际 %s", h.demoCommit(), run.Resolution[0].Commit)
		}
		if run.PinnedCommits["repo-order-service"] != h.demoCommit() {
			t.Fatalf("PinnedCommits 未固化: %+v", run.PinnedCommits)
		}
	}
	// 4) 若产出补丁，必须是可应用的标准 unified diff 且带理由与风险
	for _, p := range run.Patches {
		if !strings.Contains(p.UnifiedDiff, "--- ") || !strings.Contains(p.UnifiedDiff, "+++ ") {
			t.Fatalf("补丁不是标准 unified diff: %q", p.UnifiedDiff[:min(120, len(p.UnifiedDiff))])
		}
		if p.Rationale == "" || p.Risk == "" || p.FilePath == "" {
			t.Fatalf("补丁缺少理由/风险/路径: %+v", p)
		}
	}
	// 5) 验证结果：有补丁时必须有明确的校验结论；无补丁时允许跳过验证（无可验证内容）
	if len(run.Patches) > 0 {
		if run.Verification == nil {
			t.Fatal("有补丁但缺少沙箱验证结果")
		}
		if len(run.Verification.Checks) == 0 {
			t.Fatal("验证结果没有任何 check")
		}
	} else {
		t.Logf("本次未产出补丁（warnings=%v），跳过验证断言", run.Warnings)
		if run.State != domain.StateNeedsReview && run.State != domain.StateDegraded {
			t.Fatalf("无补丁时状态应为 needs_review/degraded，实际 %s", run.State)
		}
		if len(run.Warnings) == 0 {
			t.Fatal("无补丁时必须给出人工介入警告")
		}
	}
	// 6) 报告必须归档，且 Markdown 含关键章节
	if run.ReportID == "" {
		t.Fatal("任务未归档报告")
	}
	_, env = h.do(http.MethodGet, "/api/v1/reports/"+run.ReportID, nil, h.authHeaders())
	report := h.decodeReport(env)
	if !strings.Contains(report.Markdown, "根因") {
		t.Fatalf("报告 Markdown 缺少根因章节:\n%s", report.Markdown[:min(400, len(report.Markdown))])
	}
	if len(report.Suggestions) == 0 {
		t.Fatal("报告缺少人工复核建议")
	}

	// 7) 调用轨迹：堆栈解析与补丁合成由 Skill 层执行；
	//    根因推理由 MCP 层完成（记录在 model-calls），因此技能轨迹不要求出现 root_cause。
	_, env = h.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/skill-calls?pageSize=50", nil, h.authHeaders())
	var calls domain.Page[domain.SkillCall]
	h.decode(env, &calls)
	if calls.Total == 0 {
		t.Fatal("技能调用轨迹为空")
	}
	stages := map[string]bool{}
	for _, c := range calls.Items {
		stages[c.Stage] = true
		if c.Skill == "" || c.Version == "" {
			t.Fatalf("技能调用记录缺少技能名或版本: %+v", c)
		}
	}
	for _, want := range []string{"stack_parse", "code_load"} {
		if !stages[want] {
			t.Fatalf("技能轨迹缺少阶段 %s（实际 %v）", want, stages)
		}
	}
	_, env = h.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/model-calls?pageSize=50", nil, h.authHeaders())
	var mcs domain.Page[domain.ModelCall]
	h.decode(env, &mcs)
	if mcs.Total == 0 {
		t.Fatal("模型调用轨迹为空")
	}
	// 结构化输出约束必须可观测
	if !mcs.Items[0].SchemaValid {
		t.Fatalf("模型调用未标记结构化输出校验结果: %+v", mcs.Items[0])
	}
	modelStages := map[string]bool{}
	for _, m := range mcs.Items {
		modelStages[m.Stage] = true
	}
	if !modelStages["root_cause"] {
		t.Fatalf("模型轨迹缺少根因推理阶段（实际 %v）", modelStages)
	}
}

func TestGroupCrossRepoEndToEnd(t *testing.T) {
	h := newHarness(t)

	_, env := h.do(http.MethodPost, "/api/v1/tasks", domain.CreateTaskRequest{
		Mode:        domain.ModeGroup,
		GroupID:     "grp-mall-core",
		Source:      domain.TriggerAlert,
		Title:       "订单链路跨服务故障",
		Environment: "prod",
		Stacktrace:  npeStack,
		Logs:        "api-gateway -> order-service -> inventory-service 链路异常 traceId=8f2c1d9a4b7e6f01",
		EntryFiles:  []string{"src/main/java/com/acme/order/service/OrderService.java"},
	}, h.authHeaders())
	if env.Code != 0 {
		t.Fatalf("分组任务提交失败: code=%d msg=%s", env.Code, env.Message)
	}
	created := h.decodeRun(env)

	run := h.waitRun(created.ID, 90*time.Second)
	if run.Mode != domain.ModeGroup {
		t.Fatalf("模式错误: %s", run.Mode)
	}
	if run.RootCause == nil || run.RootCause.Summary == "" {
		t.Fatal("分组任务缺少根因")
	}
	// 懒加载验收：分组含 2 个仓库，加载文件数必须是有限的小量
	if run.Usage.FilesLoaded > 20 {
		t.Fatalf("懒加载失效：加载了 %d 个文件", run.Usage.FilesLoaded)
	}
	// 该分组只有 order-service 有匹配规则命中；api-gateway 无本地源码，
	// 整体仍必须产出报告（降级不失败）
	if run.ReportID == "" {
		t.Fatalf("分组任务未归档报告，state=%s warnings=%v", run.State, run.Warnings)
	}
}

func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	req := domain.CreateTaskRequest{
		Mode: domain.ModeSingleRepo, RepoID: "repo-order-service",
		Stacktrace: npeStack, IdempotencyKey: "idem-fixed-key-001",
		Source: domain.TriggerCICD,
	}
	_, env1 := h.do(http.MethodPost, "/api/v1/tasks", req, h.authHeaders())
	r1 := h.decodeRun(env1)
	_, env2 := h.do(http.MethodPost, "/api/v1/tasks", req, h.authHeaders())
	r2 := h.decodeRun(env2)

	if r1.ID == "" || r2.ID == "" {
		t.Fatalf("任务创建失败: %v / %v", env1.Message, env2.Message)
	}
	if r1.ID != r2.ID {
		t.Fatalf("幂等失效：两次提交产生不同 run %s != %s", r1.ID, r2.ID)
	}
	// 只应存在一条运行记录
	_, env := h.do(http.MethodGet, "/api/v1/runs?pageSize=50", nil, h.authHeaders())
	var page domain.Page[domain.TaskRun]
	h.decode(env, &page)
	if page.Total != 1 {
		t.Fatalf("幂等失效：存在 %d 条运行记录", page.Total)
	}
}

func TestRetryReusesPinnedCommits(t *testing.T) {
	h := newHarness(t)
	_, env := h.do(http.MethodPost, "/api/v1/tasks", domain.CreateTaskRequest{
		Mode: domain.ModeSingleRepo, RepoID: "repo-order-service",
		Ref: h.demoCommit(), Stacktrace: npeStack, Title: "重跑用例",
	}, h.authHeaders())
	created := h.decodeRun(env)
	first := h.waitRun(created.ID, 90*time.Second)

	_, env = h.do(http.MethodPost, "/api/v1/runs/"+first.ID+"/retry", nil, h.authHeaders())
	if env.Code != 0 {
		t.Fatalf("重跑失败: %s", env.Message)
	}
	second := h.decodeRun(env)
	if second.ID == first.ID {
		t.Fatal("重跑应产生新的 run")
	}
	if second.Attempt != first.Attempt+1 {
		t.Fatalf("重跑计数错误：%d → %d", first.Attempt, second.Attempt)
	}
	if h.demoCommit() != "" {
		if second.PinnedCommits["repo-order-service"] != first.PinnedCommits["repo-order-service"] {
			t.Fatalf("重跑未复用版本锁定：%v vs %v", first.PinnedCommits, second.PinnedCommits)
		}
	}
}

func TestObservabilityAndSSE(t *testing.T) {
	h := newHarness(t)
	_, env := h.do(http.MethodPost, "/api/v1/tasks", domain.CreateTaskRequest{
		Mode: domain.ModeSingleRepo, RepoID: "repo-order-service", Stacktrace: npeStack,
	}, h.authHeaders())
	created := h.decodeRun(env)
	run := h.waitRun(created.ID, 90*time.Second)

	_, env = h.do(http.MethodGet, "/api/v1/observability/summary", nil, h.authHeaders())
	// 接入层把汇总与引擎统计一起返回：{summary, engine, generatedAt}；同时兼容直接返回汇总的形态。
	var obs struct {
		Summary *domain.ObservabilitySummary `json:"summary"`
	}
	if err := json.Unmarshal(env.Data, &obs); err != nil || obs.Summary == nil {
		var direct domain.ObservabilitySummary
		h.decode(env, &direct)
		obs.Summary = &direct
	}
	sum := *obs.Summary
	if sum.RunsTotal != 1 {
		t.Fatalf("可观测汇总运行数错误: %d", sum.RunsTotal)
	}
	if sum.SkillCalls == 0 || sum.ModelCalls == 0 {
		t.Fatalf("可观测汇总调用统计为空: skills=%d models=%d", sum.SkillCalls, sum.ModelCalls)
	}
	if len(sum.StateDist) == 0 || len(sum.CategoryDist) == 0 {
		t.Fatalf("可观测汇总分布为空: state=%v category=%v", sum.StateDist, sum.CategoryDist)
	}

	// SSE：应能在若干秒内读到至少一条 data: 事件
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.srv.URL+"/api/v1/runs/"+run.ID+"/events?token="+h.apiKey, nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type 错误: %q", ct)
	}
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("SSE 未推送任何 data 事件")
	}
}

func TestBadRequestAndUnknownField(t *testing.T) {
	h := newHarness(t)

	// 缺少 stacktrace
	status, _ := h.do(http.MethodPost, "/api/v1/tasks", map[string]any{
		"mode": "single_repo", "repoId": "repo-order-service",
	}, h.authHeaders())
	if status != http.StatusBadRequest {
		t.Fatalf("缺少堆栈期望 400，实际 %d", status)
	}

	// 未知字段必须被拒绝（DecodeJSON 使用 DisallowUnknownFields）
	status, _ = h.do(http.MethodPost, "/api/v1/tasks", map[string]any{
		"mode": "single_repo", "repoId": "repo-order-service",
		"stacktrace": npeStack, "unknownField": "x",
	}, h.authHeaders())
	if status != http.StatusBadRequest {
		t.Fatalf("未知字段期望 400，实际 %d", status)
	}

	// 不存在的实体 → 404
	status, _ = h.do(http.MethodGet, "/api/v1/runs/not-exist", nil, h.authHeaders())
	if status != http.StatusNotFound {
		t.Fatalf("不存在的运行期望 404，实际 %d", status)
	}
}

func TestSkillRegistryAndModelsExposed(t *testing.T) {
	h := newHarness(t)

	_, env := h.do(http.MethodGet, "/api/v1/skills", nil, h.authHeaders())
	var skillPage struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	h.decode(env, &skillPage)
	if skillPage.Total < 7 || len(skillPage.Items) < 7 {
		t.Fatalf("内置技能应至少 7 个，实际 total=%d items=%d", skillPage.Total, len(skillPage.Items))
	}

	_, env = h.do(http.MethodGet, "/api/v1/models", nil, h.authHeaders())
	if env.Code != 0 {
		t.Fatalf("/models 失败: %s", env.Message)
	}
	// 兼容两种形状：{enabled,targets,health} 与 {enabled,items:[{target,health}],health}
	var models struct {
		Enabled bool                 `json:"enabled"`
		Targets []domain.ModelTarget `json:"targets"`
		Health  []domain.ModelHealth `json:"health"`
		Items   []struct {
			Target domain.ModelTarget `json:"target"`
			Health domain.ModelHealth `json:"health"`
		} `json:"items"`
	}
	h.decode(env, &models)
	targets := models.Targets
	if len(targets) == 0 {
		for _, it := range models.Items {
			targets = append(targets, it.Target)
		}
	}
	if len(targets) == 0 {
		t.Fatalf("模型层未返回任何模型（应至少有内置 Mock 推理器），响应=%s", truncateRaw(env.Data))
	}
	if !models.Enabled {
		t.Fatal("模型管控层应处于启用状态")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestModelProvidersConfigAndTest 回归：AI 设置契约必须前后端一致。
//
// 重点防止「前端传 id / testModel 等后端结构体没有的字段 → 400 unknown field」这类问题：
//   - GET  /models/config 返回 items 数组（name 为标识），脱敏不回显明文；
//   - PUT  /models/config 接受 items 数组（无 id 字段）；
//   - POST /models/test 接受 name/kind/baseUrl/models/testModel/apiKey（无 id 字段）。
// 这里用 mock 提供方，避免真实外网依赖，专注验证契约。
func TestModelProvidersConfigAndTest(t *testing.T) {
	h := newHarness(t)

	// GET 配置清单：脱敏，不回显明文密钥。
	status, env := h.do(http.MethodGet, "/api/v1/models/config", nil, h.authHeaders())
	if status != http.StatusOK || env.Code != 0 {
		t.Fatalf("GET /models/config 失败: %d/%d %s", status, env.Code, env.Message)
	}
	var getResp struct {
		Enabled     bool `json:"enabled"`
		SecretReady bool `json:"secretReady"`
		Items       []struct {
			Name   string   `json:"name"`
			Kind   string   `json:"kind"`
			Models []string `json:"models"`
			HasKey bool     `json:"hasKey"`
		} `json:"items"`
	}
	h.decode(env, &getResp)
	if !getResp.Enabled {
		t.Fatal("模型管控层应启用")
	}
	if !getResp.SecretReady {
		t.Fatal("凭证加密箱应已装配（保存密钥需要）")
	}

	// PUT 保存：契约为 items 数组、name 为标识（不应 400 unknown field）。
	putBody := map[string]any{
		"items": []map[string]any{
			{
				"name":    "mock1",
				"kind":    "mock",
				"models":  []string{"mock-reasoner-v1"},
				"enabled": true,
				"tier":    "strong",
			},
		},
	}
	status, env = h.do(http.MethodPut, "/api/v1/models/config", putBody, h.authHeaders())
	if status == http.StatusBadRequest {
		t.Fatalf("PUT /models/config 返回 400（契约不匹配）: %s", env.Message)
	}
	if status != http.StatusOK || env.Code != 0 {
		t.Fatalf("PUT /models/config 失败: %d/%d %s", status, env.Code, env.Message)
	}
	var putResp struct {
		Items []struct {
			Name   string `json:"name"`
			HasKey bool   `json:"hasKey"`
		} `json:"items"`
	}
	h.decode(env, &putResp)
	if len(putResp.Items) != 1 || putResp.Items[0].Name != "mock1" {
		t.Fatalf("保存后应返回 1 个名为 mock1 的提供方，实际 %+v", putResp.Items)
	}

	// POST 连通性测试：请求体不含 id，使用 name；不应 400 unknown field。
	testBody := map[string]any{
		"name":      "mock1",
		"kind":      "mock",
		"baseUrl":   "",
		"models":    []string{"mock-reasoner-v1"},
		"testModel": "mock-reasoner-v1",
		"apiKey":    "sk-test-dummy",
	}
	status, env = h.do(http.MethodPost, "/api/v1/models/test", testBody, h.authHeaders())
	if status == http.StatusBadRequest {
		t.Fatalf("POST /models/test 返回 400（契约不匹配）: %s", env.Message)
	}
	if status != http.StatusOK {
		t.Fatalf("POST /models/test 非预期状态码: %d (%s)", status, env.Message)
	}
	var testResp struct {
		OK bool `json:"ok"`
	}
	h.decode(env, &testResp)
	if !testResp.OK {
		t.Fatalf("mock 提供方连通性测试应成功: %+v", testResp)
	}
}
