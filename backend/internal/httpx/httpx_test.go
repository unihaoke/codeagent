package httpx_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/app"
	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/eventbus"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// 本文件是接入层（internal/httpx + internal/api + internal/app）的端到端单测。
//
// 约束：全部依赖用 fake 实现，不依赖网络、git、模型或任何其他内部分层；
// Store 使用 store.New() 内存实现；EventBus 使用进程内 eventbus.Bus
// （它提供 History 可选接口，用于验证历史事件补发）。

// ---------------------------------------------------------------------------
// fake 实现
// ---------------------------------------------------------------------------

// 测试用常量与哨兵错误。
const (
	// tenantDemo 假认证实现返回的租户（与匿名模式缺省租户一致）。
	tenantDemo = "t-demo"
	// apiKeyValue 测试用接入密钥。
	apiKeyValue = "ca_live_testkey"
)

// errRunNotFound 模拟 engine 的"运行不存在"错误（文本含 not found → 404 映射）。
var errRunNotFound = fmt.Errorf("not found: 运行不存在")

// fakeAuth 可注入错误的 domain.Authorizer 假实现。
type fakeAuth struct {
	// allowAPIKey 为 true 时接受任意 X-API-Key。
	allowAPIKey bool
	// allowJWT 为 true 时接受任意 Bearer Token。
	allowJWT bool
	// repoErr 非空时 CanAccessRepo 返回该错误（用于验证 403 映射与越权拦截）。
	repoErr error
}

func (f *fakeAuth) subject(tenant string) *domain.Subject {
	return &domain.Subject{
		TenantID: tenant, TenantName: tenant,
		Scopes: []string{"admin:all"}, Admin: true, Username: "tester",
	}
}

func (f *fakeAuth) Authenticate(_ context.Context, apiKey string) (*domain.Subject, error) {
	if !f.allowAPIKey || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("unauthorized: API Key 无效")
	}
	return f.subject(tenantDemo), nil
}

func (f *fakeAuth) Login(_ context.Context, tenantKey, username, password string) (string, *domain.Subject, error) {
	if tenantKey == "demo" && username == "admin" && password == "admin123" {
		return "jwt-test-token", f.subject(tenantDemo), nil
	}
	return "", nil, fmt.Errorf("unauthorized: 账号或密码错误")
}

func (f *fakeAuth) VerifyToken(_ context.Context, token string) (*domain.Subject, error) {
	if !f.allowJWT || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("unauthorized: token 无效")
	}
	return f.subject(tenantDemo), nil
}

func (f *fakeAuth) CanAccessRepo(_ context.Context, _ *domain.Subject, _ string) error {
	return f.repoErr
}

func (f *fakeAuth) CanAccessGroup(_ context.Context, _ *domain.Subject, _ string) error {
	return nil
}

// fakeEngine 可注入错误的 domain.TaskEngine 假实现。
type fakeEngine struct {
	// submitErr 非空时 Submit 直接返回该错误（用于验证错误码映射）。
	submitErr error
	submitted int
}

func (e *fakeEngine) Submit(_ context.Context, sub *domain.Subject, req domain.CreateTaskRequest) (*domain.TaskRun, error) {
	if e.submitErr != nil {
		return nil, e.submitErr
	}
	e.submitted++
	now := time.Now()
	return &domain.TaskRun{
		ID:         "run-test-0001",
		TaskID:     "task-test-0001",
		TenantID:   sub.TenantID,
		State:      domain.StateQueued,
		Title:      req.Title,
		Stacktrace: req.Stacktrace,
		Mode:       req.Mode,
		RequestID:  req.RequestID,
		RepoIDs:    []string{req.RepoID},
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

func (e *fakeEngine) GetRun(_ context.Context, tenantID, runID string) (*domain.TaskRun, error) {
	return nil, fmt.Errorf("not found: run %s/%s", tenantID, runID)
}

func (e *fakeEngine) ListTasks(_ context.Context, _ string, q domain.PageQuery) (domain.Page[domain.Task], error) {
	q.Normalize()
	return domain.Page[domain.Task]{Items: []domain.Task{}, Total: 0, Page: q.Page, PageSize: q.PageSize}, nil
}

func (e *fakeEngine) ListRuns(_ context.Context, _ string, q domain.PageQuery) (domain.Page[domain.TaskRun], error) {
	q.Normalize()
	return domain.Page[domain.TaskRun]{Items: []domain.TaskRun{}, Total: 0, Page: q.Page, PageSize: q.PageSize}, nil
}

func (e *fakeEngine) Cancel(_ context.Context, _, _ string) error { return nil }

func (e *fakeEngine) Retry(_ context.Context, _, _ string) (*domain.TaskRun, error) {
	return nil, fmt.Errorf("not found: 运行不存在")
}

func (e *fakeEngine) RerunWithFeedback(_ context.Context, _, _, _ string) (*domain.TaskRun, error) {
	return nil, errRunNotFound
}

func (e *fakeEngine) Stats(_ context.Context, _ string) (*domain.EngineStats, error) {
	return &domain.EngineStats{ByState: map[domain.TaskState]int{}}, nil
}

func (e *fakeEngine) Start(_ context.Context) error { return nil }
func (e *fakeEngine) Stop() error                   { return nil }

// fakeSkills 假技能注册中心。
type fakeSkills struct {
	rollbackErr error
	statusCalls int
	lastStatus  domain.SkillStatus
}

func (s *fakeSkills) manifest(name string) domain.SkillManifest {
	return domain.SkillManifest{
		Name: name, Version: "1.0.0", Title: name, Description: "测试技能",
		Category: domain.CatParse, Status: domain.SkillEnabled, Builtin: true,
		InputSchema:  map[string]any{"type": "object", "required": []any{"stacktrace"}},
		OutputSchema: map[string]any{"type": "object"},
		Timeout:      30, Retries: 1, MaxConcurrency: 4,
	}
}

func (s *fakeSkills) List(_ context.Context, category string) []domain.SkillManifest {
	if category != "" && category != string(domain.CatParse) {
		return []domain.SkillManifest{}
	}
	return []domain.SkillManifest{s.manifest("stacktrace_parse"), s.manifest("code_ast_analyze")}
}

func (s *fakeSkills) Get(_ context.Context, name string) (*domain.SkillManifest, error) {
	if name != "stacktrace_parse" {
		return nil, fmt.Errorf("skill not found: %s", name)
	}
	m := s.manifest(name)
	return &m, nil
}

func (s *fakeSkills) Register(_ context.Context, _ domain.SkillManifest, _ domain.SkillHandler) error {
	return nil
}

func (s *fakeSkills) SetStatus(_ context.Context, _, _ string, status domain.SkillStatus) error {
	s.statusCalls++
	s.lastStatus = status
	return nil
}

func (s *fakeSkills) Rollback(_ context.Context, _ string) (string, error) {
	if s.rollbackErr != nil {
		return "", s.rollbackErr
	}
	return "0.9.0", nil
}

func (s *fakeSkills) Health(_ context.Context) []domain.SkillHealth {
	return []domain.SkillHealth{{
		Skill: "stacktrace_parse", Version: "1.0.0", Status: domain.SkillEnabled,
		Healthy: true, CircuitState: "closed", Successes: 3, TotalCalls: 3, AvgLatencyMS: 4.2,
	}}
}

func (s *fakeSkills) Names() []string { return []string{"stacktrace_parse", "code_ast_analyze"} }

// fakeMCP 假模型管控层。
type fakeMCP struct{}

func (m *fakeMCP) Chat(_ context.Context, _ *domain.CallContext, _ domain.ModelRequest) (*domain.ModelResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *fakeMCP) Providers() []domain.ModelTarget {
	return []domain.ModelTarget{{Provider: "mock", Model: "mock-reasoner-v1", Tier: domain.TierStrong}}
}

func (m *fakeMCP) Health(_ context.Context) []domain.ModelHealth {
	return []domain.ModelHealth{{Provider: "mock", Model: "mock-reasoner-v1", Healthy: true, CircuitState: "closed"}}
}

func (m *fakeMCP) Stats(_ context.Context) domain.ModelStats {
	return domain.ModelStats{TotalCalls: 2, TotalTokens: 128, ByModel: map[string]int64{"mock-reasoner-v1": 2}}
}

// fakeRecorder 假审计/轨迹记录器。
type fakeRecorder struct {
	mu     sync.Mutex
	audits []domain.AuditEvent
}

func (r *fakeRecorder) Audit(ev domain.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.audits = append(r.audits, ev)
}

func (r *fakeRecorder) SkillCall(_ domain.SkillCall) {}

func (r *fakeRecorder) ModelCall(_ domain.ModelCall) {}

func (r *fakeRecorder) ListAudits(_ context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	q.Normalize()
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]domain.AuditEvent, 0, len(r.audits))
	for _, ev := range r.audits {
		if ev.TenantID != tenantID {
			continue
		}
		if q.State != "" && ev.Category != q.State {
			continue
		}
		if q.Keyword != "" && !strings.Contains(ev.Message, q.Keyword) {
			continue
		}
		items = append(items, ev)
	}
	return domain.Page[domain.AuditEvent]{Items: items, Total: len(items), Page: q.Page, PageSize: q.PageSize}, nil
}

func (r *fakeRecorder) ListSkillCalls(_ context.Context, _, _ string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	q.Normalize()
	return domain.Page[domain.SkillCall]{Items: []domain.SkillCall{}, Page: q.Page, PageSize: q.PageSize}, nil
}

func (r *fakeRecorder) ListModelCalls(_ context.Context, _, _ string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	q.Normalize()
	return domain.Page[domain.ModelCall]{Items: []domain.ModelCall{}, Page: q.Page, PageSize: q.PageSize}, nil
}

func (r *fakeRecorder) Observability(_ context.Context, _ string) (*domain.ObservabilitySummary, error) {
	return &domain.ObservabilitySummary{
		TasksTotal: 1, RunsTotal: 1, RunsSucceeded: 1, FixRate: 1,
		StateDist: map[string]int{"succeeded": 1}, CategoryDist: map[string]int{"runtime": 1},
	}, nil
}

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// fixture 聚合一次测试所需的全部 fake 与真实内存组件。
type fixture struct {
	st     store.Store
	auth   *fakeAuth
	engine *fakeEngine
	skills *fakeSkills
	mcp    *fakeMCP
	rec    *fakeRecorder
	bus    *eventbus.Bus

	ts   *httptest.Server
	deps app.Deps
}

// newFixture 构建测试夹具：anonymous 控制是否开启匿名模式，withMCP 控制是否装配模型层。
func newFixture(t *testing.T, anonymous bool, withMCP bool) *fixture {
	t.Helper()

	cfg := config.Default()
	cfg.Server.RateLimitPerMin = 100000
	cfg.Server.CORSOrigins = []string{"*"}
	cfg.Auth.AllowAnonymous = anonymous

	st := store.New()
	bus := eventbus.New(64)
	f := &fixture{
		st:     st,
		auth:   &fakeAuth{allowAPIKey: true, allowJWT: true},
		engine: &fakeEngine{},
		skills: &fakeSkills{},
		mcp:    &fakeMCP{},
		rec:    &fakeRecorder{},
		bus:    bus,
	}

	deps := app.Deps{
		Cfg: cfg, Store: st, Auth: f.auth, Engine: f.engine, Repos: nil,
		Skills: f.skills, MCP: nil, Recorder: f.rec, Bus: bus, Log: logx.Nop(),
	}
	if withMCP {
		deps.MCP = f.mcp
	}
	f.deps = deps

	srv, err := app.NewServer(deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	f.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(f.ts.Close)
	return f
}

// seedRun 在 Store 中预置一次运行（供 /runs/{id} 与 SSE 用例使用）。
func (f *fixture) seedRun(runID string) *domain.TaskRun {
	now := time.Now()
	run := &domain.TaskRun{
		ID: runID, TaskID: "task-1", TenantID: tenantDemo, Attempt: 1,
		Mode: domain.ModeSingleRepo, State: domain.StateSucceeded, Severity: domain.SeverityMajor,
		Title: "空指针异常", Stacktrace: "java.lang.NullPointerException\n\tat a.b.C.d(C.java:10)",
		RepoIDs:       []string{"repo-1"},
		PinnedCommits: map[string]string{"repo-1": "abc1234"},
		Usage:         domain.RunUsage{FilesLoaded: 2, CodeChars: 120, TotalTokens: 64},
		CreatedAt:     now, StartedAt: now, EndedAt: now, UpdatedAt: now,
	}
	if err := f.st.CreateRun(run); err != nil {
		panic(err)
	}
	return run
}

// seedRepo 在 Store 中预置一个仓库（提交任务前必须先注册仓库，否则接入层按租户隔离返回 404）。
func (f *fixture) seedRepo(repoID, key string) *domain.Repository {
	now := time.Now()
	repo := &domain.Repository{
		ID: repoID, TenantID: tenantDemo, Name: key, Key: key,
		URL: "https://git.example.com/acme/" + key + ".git", DefaultBranch: "main",
		Provider: domain.VCSGit, Layer: domain.LayerService, Language: "java",
		Status: domain.RepoActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.st.CreateRepo(repo); err != nil {
		panic(err)
	}
	return repo
}

// request 发起一次请求并按需附加认证头。
func (f *fixture) request(t *testing.T, method, path string, body any, auth bool) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("X-API-Key", apiKeyValue)
	}
	resp, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// envelope 解析统一响应信封。
type envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"requestId"`
	Data      json.RawMessage `json:"data"`
}

// decodeEnvelope 读取并解析响应体（同时校验 JSON 可解析）。
func decodeEnvelope(t *testing.T, resp *http.Response) envelope {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("响应不是合法信封 JSON: %v; body=%s", err, string(raw))
	}
	return env
}

// ---------------------------------------------------------------------------
// 1) 健康检查与 404
// ---------------------------------------------------------------------------

func TestHealthzAndUnknownPath(t *testing.T) {
	f := newFixture(t, true, true)

	resp := f.request(t, http.MethodGet, "/healthz", nil, false)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz 状态码 = %d, 期望 200", resp.StatusCode)
	}
	if env.Code != 0 {
		t.Fatalf("GET /healthz code = %d, 期望 0", env.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("解析 healthz data: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("healthz status = %v, 期望 ok", payload["status"])
	}
	for _, key := range []string{"version", "uptime", "time"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("healthz 响应缺少字段 %s: %v", key, payload)
		}
	}
	if got := resp.Header.Get("X-Request-Id"); !strings.HasPrefix(got, "r-") {
		t.Fatalf("X-Request-Id = %q, 期望以 r- 开头", got)
	}

	// 未知路径（匿名模式，避免被 401 拦截）→ 404 信封。
	unknown := f.request(t, http.MethodGet, "/api/v1/does-not-exist", nil, false)
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路径状态码 = %d, 期望 404", unknown.StatusCode)
	}
	unknown.Body.Close()
}

// ---------------------------------------------------------------------------
// 2) 认证与授权
// ---------------------------------------------------------------------------

func TestAuthRequiredAndAPIKey(t *testing.T) {
	// 关闭匿名模式：无凭证 → 401。
	closed := newFixture(t, false, true)
	resp := closed.request(t, http.MethodGet, "/api/v1/tasks", nil, false)
	body := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证访问 /api/v1/tasks 状态码 = %d, 期望 401", resp.StatusCode)
	}
	if body.Code != 401 {
		t.Fatalf("未认证 code = %d, 期望 401", body.Code)
	}

	// 合法 X-API-Key → 200。
	okResp := closed.request(t, http.MethodGet, "/api/v1/tasks", nil, true)
	okBody := decodeEnvelope(t, okResp)
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("带 API Key 状态码 = %d, 期望 200", okResp.StatusCode)
	}
	if okBody.Code != 0 {
		t.Fatalf("带 API Key code = %d, 期望 0", okBody.Code)
	}

	// 合法 Bearer JWT → 200。
	req, _ := http.NewRequest(http.MethodGet, closed.ts.URL+"/api/v1/auth/profile", nil)
	req.Header.Set("Authorization", "Bearer jwt-test")
	jwtResp, err := closed.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("profile 请求失败: %v", err)
	}
	if jwtResp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer 认证状态码 = %d, 期望 200", jwtResp.StatusCode)
	}
	profile := decodeEnvelope(t, jwtResp)
	if !bytes.Contains(profile.Data, []byte("t-demo")) {
		t.Fatalf("profile data 未包含租户: %s", string(profile.Data))
	}

	// 免认证路径不要求凭证。
	health := closed.request(t, http.MethodGet, "/healthz", nil, false)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("免认证 /healthz 状态码 = %d, 期望 200", health.StatusCode)
	}
	health.Body.Close()
}

// ---------------------------------------------------------------------------
// 3) 提交任务：成功 / 缺 stacktrace / 未知字段
// ---------------------------------------------------------------------------

func TestCreateTask(t *testing.T) {
	f := newFixture(t, true, true)
	f.seedRepo("repo-x", "order-service")

	// 正常提交 → 201 且含 runId。
	resp := f.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"repoId":     "repo-x",
		"stacktrace": "java.lang.NullPointerException\n\tat com.acme.OrderService.pay(OrderService.java:42)",
		"ref":        "deadbeef",
		"priority":   3,
	}, true)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/tasks 状态码 = %d, 期望 201; body=%s", resp.StatusCode, string(env.Data))
	}
	if env.Code != 0 {
		t.Fatalf("创建任务 code = %d, 期望 0", env.Code)
	}
	var run domain.TaskRun
	if err := json.Unmarshal(env.Data, &run); err != nil {
		t.Fatalf("解析 TaskRun: %v", err)
	}
	if run.ID == "" {
		t.Fatalf("响应缺少 runId: %s", string(env.Data))
	}
	if run.TenantID != tenantDemo {
		t.Fatalf("run.TenantID = %q, 期望由接入层注入 t-demo", run.TenantID)
	}
	if run.Title == "" {
		t.Fatalf("标题应由堆栈首行自动生成: %s", string(env.Data))
	}
	if run.RequestID == "" {
		t.Fatalf("run.RequestID 应由接入层注入: %s", string(env.Data))
	}
	if f.engine.submitted != 1 {
		t.Fatalf("engine.Submit 调用次数 = %d, 期望 1", f.engine.submitted)
	}

	// 缺 stacktrace → 400。
	missing := f.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{"repoId": "repo-x"}, true)
	missingEnv := decodeEnvelope(t, missing)
	if missing.StatusCode != http.StatusBadRequest || missingEnv.Code != 400 {
		t.Fatalf("缺 stacktrace: status=%d code=%d, 期望 400/400", missing.StatusCode, missingEnv.Code)
	}

	// 未知字段 → 400（DisallowUnknownFields）。
	unknown := f.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"repoId": "repo-x", "stacktrace": "boom", "nopeField": 1,
	}, true)
	unknownEnv := decodeEnvelope(t, unknown)
	if unknown.StatusCode != http.StatusBadRequest || unknownEnv.Code != 400 {
		t.Fatalf("未知字段: status=%d code=%d, 期望 400/400", unknown.StatusCode, unknownEnv.Code)
	}
}

// ---------------------------------------------------------------------------
// 4) 租户隔离 / 越权错误映射
// ---------------------------------------------------------------------------

func TestTenantIsolationAndErrorMapping(t *testing.T) {
	// 4.1 引擎返回"跨租户"类错误 → 403。
	f := newFixture(t, true, true)
	f.seedRepo("repo-x", "order-service")
	f.engine.submitErr = fmt.Errorf("forbidden: 跨租户访问被拒绝")
	resp := f.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"repoId": "repo-x", "stacktrace": "boom",
	}, true)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusForbidden || env.Code != 403 {
		t.Fatalf("跨租户错误: status=%d code=%d, 期望 403/403", resp.StatusCode, env.Code)
	}

	// 4.2 认证层 CanAccessRepo 拒绝 → 403。
	g := newFixture(t, true, true)
	g.seedRepo("repo-1", "order-service")
	g.auth.repoErr = fmt.Errorf("permission denied: 无权访问该仓库")
	denied := g.request(t, http.MethodGet, "/api/v1/repos/repo-1", nil, true)
	deniedEnv := decodeEnvelope(t, denied)
	if denied.StatusCode != http.StatusForbidden || deniedEnv.Code != 403 {
		t.Fatalf("越权读仓库: status=%d code=%d, 期望 403/403", denied.StatusCode, deniedEnv.Code)
	}

	// 4.3 配额/限流类错误 → 429；语义校验类 → 422。
	h := newFixture(t, true, true)
	h.seedRepo("repo-x", "order-service")
	h.engine.submitErr = fmt.Errorf("quota exceeded: 单租户并发配额超限")
	limited := h.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"repoId": "repo-x", "stacktrace": "boom",
	}, true)
	if got := decodeEnvelope(t, limited); limited.StatusCode != http.StatusTooManyRequests || got.Code != 429 {
		t.Fatalf("配额超限: status=%d code=%d, 期望 429/429", limited.StatusCode, got.Code)
	}

	i := newFixture(t, true, true)
	i.seedRepo("repo-x", "order-service")
	i.engine.submitErr = fmt.Errorf("schema 校验失败: 入参不合法")
	invalid := i.request(t, http.MethodPost, "/api/v1/tasks", map[string]any{
		"repoId": "repo-x", "stacktrace": "boom",
	}, true)
	if got := decodeEnvelope(t, invalid); invalid.StatusCode != http.StatusBadRequest || got.Code != 422 {
		t.Fatalf("语义校验失败: status=%d code=%d, 期望 400/422", invalid.StatusCode, got.Code)
	}

	// 4.4 租户隔离：带 API Key 的主体固定为 t-demo，运行记录写入该租户。
	j := newFixture(t, true, true)
	run := j.seedRun("run-tenant-demo")
	if run.TenantID != tenantDemo {
		t.Fatalf("seed 运行租户 = %q", run.TenantID)
	}
	okResp := j.request(t, http.MethodGet, "/api/v1/runs/run-tenant-demo", nil, true)
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("本租户读取运行状态码 = %d, 期望 200", okResp.StatusCode)
	}
	okResp.Body.Close()

	// 另一租户记录不可见 → 404（不泄露存在性）。
	other := &domain.TaskRun{ID: "run-other-tenant", TenantID: "t-other", State: domain.StateQueued, CreatedAt: time.Now()}
	if err := j.st.CreateRun(other); err != nil {
		t.Fatalf("create other run: %v", err)
	}
	crossResp := j.request(t, http.MethodGet, "/api/v1/runs/run-other-tenant", nil, true)
	if crossResp.StatusCode != http.StatusNotFound {
		t.Fatalf("跨租户读取状态码 = %d, 期望 404", crossResp.StatusCode)
	}
	crossResp.Body.Close()
}

// ---------------------------------------------------------------------------
// 5) 分页信封字段
// ---------------------------------------------------------------------------

func TestPaginationEnvelope(t *testing.T) {
	f := newFixture(t, true, true)
	resp := f.request(t, http.MethodGet, "/api/v1/tasks?page=1&pageSize=5", nil, true)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/tasks 状态码 = %d", resp.StatusCode)
	}
	var page struct {
		Items    []json.RawMessage `json:"items"`
		Total    *int              `json:"total"`
		Page     *int              `json:"page"`
		PageSize *int              `json:"pageSize"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("解析分页信封: %v; data=%s", err, string(env.Data))
	}
	if page.Items == nil || page.Total == nil || page.Page == nil || page.PageSize == nil {
		t.Fatalf("分页字段缺失（items/total/page/pageSize）: %s", string(env.Data))
	}
	if *page.Page != 1 || *page.PageSize != 5 {
		t.Fatalf("分页回显 page=%d pageSize=%d, 期望 1/5", *page.Page, *page.PageSize)
	}
}

// ---------------------------------------------------------------------------
// 6) 人工反馈校验
// ---------------------------------------------------------------------------

func TestRunFeedbackValidation(t *testing.T) {
	f := newFixture(t, true, true)
	f.seedRun("run-feedback-1")

	// 空 feedback → 400（不进入 engine）。
	empty := f.request(t, http.MethodPost, "/api/v1/runs/run-feedback-1/feedback",
		map[string]any{"feedback": "   "}, true)
	emptyEnv := decodeEnvelope(t, empty)
	if empty.StatusCode != http.StatusBadRequest || emptyEnv.Code != 400 {
		t.Fatalf("空 feedback: status=%d code=%d, 期望 400/400", empty.StatusCode, emptyEnv.Code)
	}

	// 超长 feedback → 400。
	long := f.request(t, http.MethodPost, "/api/v1/runs/run-feedback-1/feedback",
		map[string]any{"feedback": strings.Repeat("测", 4001)}, true)
	longEnv := decodeEnvelope(t, long)
	if long.StatusCode != http.StatusBadRequest || longEnv.Code != 400 {
		t.Fatalf("超长 feedback: status=%d code=%d, 期望 400/400", long.StatusCode, longEnv.Code)
	}

	// 合法 feedback：引擎 fake 返回 not found → 404（验证错误映射链路）。
	ok := f.request(t, http.MethodPost, "/api/v1/runs/run-feedback-1/feedback",
		map[string]any{"feedback": "补丁未覆盖空值分支，请补充防御性判空"}, true)
	if got := decodeEnvelope(t, ok); ok.StatusCode != http.StatusNotFound || got.Code != 404 {
		t.Fatalf("合法 feedback（engine not found）: status=%d code=%d, 期望 404/404", ok.StatusCode, got.Code)
	}
}

// ---------------------------------------------------------------------------
// 7) 技能与模型
// ---------------------------------------------------------------------------

func TestSkillsAndModels(t *testing.T) {
	f := newFixture(t, true, true)

	// /skills：数组 + 健康度。
	resp := f.request(t, http.MethodGet, "/api/v1/skills", nil, true)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/skills 状态码 = %d", resp.StatusCode)
	}
	var list struct {
		Items []struct {
			Manifest domain.SkillManifest `json:"manifest"`
			Health   *domain.SkillHealth  `json:"health"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("解析技能列表: %v; data=%s", err, string(env.Data))
	}
	if len(list.Items) == 0 || list.Total == 0 {
		t.Fatalf("技能列表为空: %s", string(env.Data))
	}
	hasHealth := false
	for _, item := range list.Items {
		if item.Manifest.Name == "" {
			t.Fatalf("技能项缺少 manifest: %s", string(env.Data))
		}
		if item.Health != nil {
			hasHealth = true
		}
	}
	if !hasHealth {
		t.Fatalf("技能列表未合并健康度: %s", string(env.Data))
	}

	// 技能详情：含 Schema。
	detail := f.request(t, http.MethodGet, "/api/v1/skills/stacktrace_parse", nil, true)
	detailEnv := decodeEnvelope(t, detail)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("技能详情状态码 = %d", detail.StatusCode)
	}
	if !bytes.Contains(detailEnv.Data, []byte("inputSchema")) {
		t.Fatalf("技能详情缺少 inputSchema: %s", string(detailEnv.Data))
	}

	// 非法 status → 400；合法 status → 200。
	bad := f.request(t, http.MethodPost, "/api/v1/skills/stacktrace_parse/status",
		map[string]any{"status": "whatever"}, true)
	if got := decodeEnvelope(t, bad); bad.StatusCode != http.StatusBadRequest || got.Code != 400 {
		t.Fatalf("非法技能状态: status=%d code=%d, 期望 400/400", bad.StatusCode, got.Code)
	}
	good := f.request(t, http.MethodPost, "/api/v1/skills/stacktrace_parse/status",
		map[string]any{"version": "1.0.0", "status": "canary", "canaryPercent": 20}, true)
	if good.StatusCode != http.StatusOK {
		t.Fatalf("合法技能状态状态码 = %d, 期望 200", good.StatusCode)
	}
	good.Body.Close()
	if f.skills.lastStatus != domain.SkillCanary {
		t.Fatalf("SetStatus 收到状态 = %q, 期望 canary", f.skills.lastStatus)
	}

	// 回滚：错误文本含 no rollback → 409。
	f.skills.rollbackErr = fmt.Errorf("ErrNoRollback: 没有可回滚的历史版本")
	rollback := f.request(t, http.MethodPost, "/api/v1/skills/stacktrace_parse/rollback", nil, true)
	if got := decodeEnvelope(t, rollback); rollback.StatusCode != http.StatusConflict || got.Code != 409 {
		t.Fatalf("无回滚版本: status=%d code=%d, 期望 409/409", rollback.StatusCode, got.Code)
	}

	// /models：正常返回提供方与健康度。
	models := f.request(t, http.MethodGet, "/api/v1/models", nil, true)
	modelsEnv := decodeEnvelope(t, models)
	if models.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models 状态码 = %d", models.StatusCode)
	}
	if !bytes.Contains(modelsEnv.Data, []byte("mock-reasoner-v1")) {
		t.Fatalf("模型列表缺少提供方: %s", string(modelsEnv.Data))
	}

	// /models/stats。
	stats := f.request(t, http.MethodGet, "/api/v1/models/stats", nil, true)
	if stats.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/models/stats 状态码 = %d", stats.StatusCode)
	}
	stats.Body.Close()
}

// TestModelsEmptyState MCP 为 nil 时必须返回 200 空态（便于前端渲染）。
func TestModelsEmptyState(t *testing.T) {
	f := newFixture(t, true, false) // withMCP=false
	resp := f.request(t, http.MethodGet, "/api/v1/models", nil, true)
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusOK || env.Code != 0 {
		t.Fatalf("MCP 未装配时 /models: status=%d code=%d, 期望 200/0", resp.StatusCode, env.Code)
	}
	var payload struct {
		Enabled bool              `json:"enabled"`
		Items   []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Fatalf("解析空态: %v", err)
	}
	if payload.Enabled {
		t.Fatalf("MCP 未装配时 enabled 应为 false: %s", string(env.Data))
	}
	if payload.Items == nil || len(payload.Items) != 0 {
		t.Fatalf("MCP 未装配时 items 应为空数组: %s", string(env.Data))
	}

	// 就绪检查：MCP 缺失不致命，但 skills/engine 已装配 → 仍应 ready。
	ready := f.request(t, http.MethodGet, "/readyz", nil, false)
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("就绪检查状态码 = %d, 期望 200", ready.StatusCode)
	}
	ready.Body.Close()
}

// TestReadyzDegraded 缺少关键依赖（engine/skills）时 /readyz 返回 503。
func TestReadyzDegraded(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.AllowAnonymous = true
	srv, err := app.NewServer(app.Deps{
		Cfg: cfg, Store: store.New(), Log: logx.Nop(), Bus: eventbus.New(8),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || env.Code != 503 {
		t.Fatalf("关键依赖缺失时 /readyz: status=%d code=%d, 期望 503/503", resp.StatusCode, env.Code)
	}
}

// ---------------------------------------------------------------------------
// 8) Prometheus 指标
// ---------------------------------------------------------------------------

func TestMetrics(t *testing.T) {
	f := newFixture(t, true, true)
	f.seedRun("run-metrics-1")

	resp := f.request(t, http.MethodGet, "/metrics", nil, false)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 /metrics: %v", err)
	}
	body := string(raw)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics 状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, 期望含 text/plain", ct)
	}
	for _, metric := range []string{
		"codeagent_tasks_total", "codeagent_runs_total", "codeagent_skill_calls_total",
		"codeagent_model_tokens_total", "codeagent_runs_degraded_total",
		"codeagent_uptime_seconds",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("/metrics 缺少指标 %s\n%s", metric, body)
		}
	}
	if !strings.Contains(body, "# TYPE codeagent_uptime_seconds gauge") {
		t.Fatalf("/metrics 缺少 Prometheus TYPE 行:\n%s", body)
	}
	if !strings.Contains(body, `codeagent_tasks_total{state="succeeded"} 1`) {
		t.Fatalf("任务状态分布未正确输出:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// 9) SSE 事件流
// ---------------------------------------------------------------------------

func TestRunEventsSSE(t *testing.T) {
	f := newFixture(t, true, true)
	f.seedRun("run-sse-1")

	// 先发布一条本 run 的事件，验证历史回放。
	f.bus.Publish(domain.Event{
		Type: "stage", TenantID: tenantDemo, RunID: "run-sse-1", TaskID: "task-1",
		Stage: "resolve", Message: "阶段开始：版本锁定", At: time.Now(),
	})

	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/api/v1/runs/run-sse-1/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-API-Key", apiKeyValue)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q, 期望含 text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("SSE Cache-Control = %q, 期望含 no-cache", cc)
	}
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Fatalf("SSE X-Accel-Buffering = %q, 期望 no", xab)
	}

	// 读取直到出现 data: 行（历史事件应在订阅后立刻补发）。
	reader := bufio.NewReader(resp.Body)
	gotData := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !gotData {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			var ev domain.Event
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				t.Fatalf("SSE data 不是合法 domain.Event: %v; %s", err, payload)
			}
			if ev.RunID != "run-sse-1" || ev.Type != "stage" {
				t.Fatalf("SSE 事件不匹配: %+v", ev)
			}
			gotData = true
		}
	}
	if !gotData {
		t.Fatalf("未在超时前收到任何 SSE data: 行")
	}
}

// TestRunEventsDegraded Bus 为 nil 时 SSE 降级为一次性 JSON。
func TestRunEventsDegraded(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.AllowAnonymous = true
	st := store.New()
	now := time.Now()
	if err := st.CreateRun(&domain.TaskRun{
		ID: "run-no-bus", TenantID: tenantDemo, State: domain.StateQueued, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	srv, err := app.NewServer(app.Deps{
		Cfg: cfg, Store: st, Engine: &fakeEngine{}, Skills: &fakeSkills{}, Log: logx.Nop(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/runs/run-no-bus/events")
	if err != nil {
		t.Fatalf("SSE 降级请求失败: %v", err)
	}
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("降级状态码 = %d, 期望 200", resp.StatusCode)
	}
	if !bytes.Contains(env.Data, []byte(`"degraded":true`)) {
		t.Fatalf("降级响应缺少 degraded 说明: %s", string(env.Data))
	}
}

// ---------------------------------------------------------------------------
// 10) WebSocket 握手与 ping/pong
// ---------------------------------------------------------------------------

// wsHandshake 用 net.Dial 手写 RFC6455 握手，返回连接与握手响应。
func wsHandshake(t *testing.T, url, key string) (net.Conn, *http.Response) {
	t.Helper()
	rawURL := strings.TrimPrefix(url, "http://")
	conn, err := net.DialTimeout("tcp", rawURL, 3*time.Second)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	req := "GET /api/v1/ws?runId=run-ws-1 HTTP/1.1\r\n" +
		"Host: " + rawURL + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"X-API-Key: " + apiKeyValue + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		t.Fatalf("写入握手请求: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		t.Fatalf("set deadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		conn.Close()
		t.Fatalf("读取握手响应: %v", err)
	}
	return conn, resp
}

// writeMaskedTextFrame 发送一个带掩码的文本帧（客户端→服务端必须加掩码）。
func writeMaskedTextFrame(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	data := []byte(payload)
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := make([]byte, 0, 6+len(data))
	frame = append(frame, 0x81, byte(0x80|len(data))) // FIN + text，MASK + 长度
	frame = append(frame, mask[:]...)
	for i, b := range data {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("写入掩码文本帧: %v", err)
	}
}

// readServerFrame 读取一个服务端帧（服务端不加掩码）。
func readServerFrame(t *testing.T, reader *bufio.Reader) (opcode int, payload []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(reader, head[:]); err != nil {
		t.Fatalf("读帧头: %v", err)
	}
	opcode = int(head[0] & 0x0F)
	masked := head[1]&0x80 != 0
	length := int(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			t.Fatalf("读 16 位长度: %v", err)
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(reader, ext[:]); err != nil {
			t.Fatalf("读 64 位长度: %v", err)
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(reader, maskKey[:]); err != nil {
			t.Fatalf("读掩码: %v", err)
		}
	}
	payload = make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(reader, payload); err != nil {
			t.Fatalf("读载荷: %v", err)
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload
}

func TestWebSocketHandshakeAndPingPong(t *testing.T) {
	f := newFixture(t, true, true)
	f.seedRun("run-ws-1")

	const wsKey = "dGhlIHNhbXBsZSBub25jZQ=="
	conn, resp := wsHandshake(t, f.ts.URL, wsKey)
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("握手状态码 = %d, 期望 101", resp.StatusCode)
	}
	wantAccept := func() string {
		sum := sha1.Sum([]byte(wsKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		return base64.StdEncoding.EncodeToString(sum[:])
	}()
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, 期望 %q", got, wantAccept)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("握手响应缺少 Upgrade: websocket（实际 %q）", resp.Header.Get("Upgrade"))
	}

	// 发送带掩码的文本帧 "ping"，期望收到文本帧 "pong"。
	writeMaskedTextFrame(t, conn, "ping")
	reader := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	opcode, payload := readServerFrame(t, reader)
	if opcode != 0x1 {
		t.Fatalf("服务端帧 opcode = %d, 期望 1（文本帧）", opcode)
	}
	if string(payload) != "pong" {
		t.Fatalf("服务端帧载荷 = %q, 期望 pong", string(payload))
	}
}

// TestWebSocketRejectsBadHandshake 非法握手必须被拒绝（400）。
func TestWebSocketRejectsBadHandshake(t *testing.T) {
	f := newFixture(t, false, true)
	f.seedRun("run-ws-1")

	req, err := http.NewRequest(http.MethodGet, f.ts.URL+"/api/v1/ws", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-API-Key", apiKeyValue)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "not-base64-key")

	resp, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("握手请求失败: %v", err)
	}
	env := decodeEnvelope(t, resp)
	if resp.StatusCode != http.StatusBadRequest || env.Code != 400 {
		t.Fatalf("非法握手: status=%d code=%d, 期望 400/400", resp.StatusCode, env.Code)
	}
}

// ---------------------------------------------------------------------------
// 附加：报告 Markdown 与可观测接口（保证路由表完整可用）
// ---------------------------------------------------------------------------

func TestReportMarkdownAndObservability(t *testing.T) {
	f := newFixture(t, true, true)
	now := time.Now()
	if err := f.st.CreateReport(&domain.Report{
		ID: "rep-1", RunID: "run-1", TaskID: "task-1", TenantID: tenantDemo,
		Title: "订单空指针修复报告", State: domain.StateSucceeded, Severity: domain.SeverityMajor,
		Summary: "修复完成", Markdown: "# 修复报告\n\n- 根因：空指针\n", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}
	f.rec.Audit(domain.AuditEvent{
		ID: "audit-1", TenantID: tenantDemo, Category: "task", Action: "submit",
		Level: "info", Message: "受理任务", At: now,
	})

	md := f.request(t, http.MethodGet, "/api/v1/reports/rep-1/markdown", nil, true)
	defer md.Body.Close()
	raw, err := io.ReadAll(md.Body)
	if err != nil {
		t.Fatalf("读取 markdown: %v", err)
	}
	if md.StatusCode != http.StatusOK {
		t.Fatalf("markdown 状态码 = %d", md.StatusCode)
	}
	if ct := md.Header.Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Fatalf("markdown Content-Type = %q, 期望含 text/markdown", ct)
	}
	if !strings.HasPrefix(string(raw), "# 修复报告") {
		t.Fatalf("markdown 正文不符: %q", string(raw))
	}

	// ?download=1 触发下载头。
	dl := f.request(t, http.MethodGet, "/api/v1/reports/rep-1/markdown?download=1", nil, true)
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("download=1 缺少 Content-Disposition: %q", cd)
	}
	dl.Body.Close()

	// 可观测汇总。
	sum := f.request(t, http.MethodGet, "/api/v1/observability/summary", nil, true)
	sumEnv := decodeEnvelope(t, sum)
	if sum.StatusCode != http.StatusOK {
		t.Fatalf("observability summary 状态码 = %d", sum.StatusCode)
	}
	if !bytes.Contains(sumEnv.Data, []byte("summary")) {
		t.Fatalf("observability summary 响应异常: %s", string(sumEnv.Data))
	}

	// 审计列表（分类过滤）。
	audits := f.request(t, http.MethodGet, "/api/v1/observability/audits?state=task&page=1&pageSize=10", nil, true)
	auditsEnv := decodeEnvelope(t, audits)
	if audits.StatusCode != http.StatusOK {
		t.Fatalf("observability audits 状态码 = %d", audits.StatusCode)
	}
	if !bytes.Contains(auditsEnv.Data, []byte("items")) {
		t.Fatalf("审计列表缺少分页字段: %s", string(auditsEnv.Data))
	}
}

// ---------------------------------------------------------------------------
// 附加：CORS 预检与中间件链
// ---------------------------------------------------------------------------

func TestCORSPreflight(t *testing.T) {
	f := newFixture(t, true, true)
	req, err := http.NewRequest(http.MethodOptions, f.ts.URL+"/api/v1/tasks", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := f.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("预检请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("预检状态码 = %d, 期望 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, 期望 *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-API-Key") {
		t.Fatalf("Access-Control-Allow-Headers = %q, 期望含 X-API-Key", got)
	}
}
