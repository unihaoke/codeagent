package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/google/uuid"
)

// writeFile 在指定目录写入测试文件。
func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

// parseUUID 校验字符串是否为合法 UUID。
func parseUUID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeRecorder 记录 SkillCall 的 domain.Recorder 测试替身。
type fakeRecorder struct {
	mu     sync.Mutex
	calls  []domain.SkillCall
	audits []domain.AuditEvent
}

func (f *fakeRecorder) Audit(ev domain.AuditEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, ev)
}

func (f *fakeRecorder) SkillCall(c domain.SkillCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeRecorder) ModelCall(c domain.ModelCall) {}

func (f *fakeRecorder) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	return domain.Page[domain.AuditEvent]{}, nil
}

func (f *fakeRecorder) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.Page[domain.SkillCall]{Items: f.calls, Total: len(f.calls), Page: 1, PageSize: 20}, nil
}

func (f *fakeRecorder) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	return domain.Page[domain.ModelCall]{}, nil
}

func (f *fakeRecorder) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	return &domain.ObservabilitySummary{}, nil
}

// snapshot 返回已记录的技能调用（副本）。
func (f *fakeRecorder) snapshot() []domain.SkillCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.SkillCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// testConfig 返回测试用技能配置（关闭默认重试，避免干扰断言）。
func testConfig() config.SkillConfig {
	return config.SkillConfig{
		Enabled:           true,
		FailureThreshold:  3,
		CooldownSec:       30,
		DefaultTimeoutSec: 5,
		DefaultRetries:    0,
	}
}

// testManifest 构造一个自定义测试技能清单。
func testManifest(name, version string, schema map[string]any) domain.SkillManifest {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return domain.SkillManifest{
		Name:             name,
		Version:          version,
		Title:            name,
		Category:         domain.CatParse,
		Status:           domain.SkillEnabled,
		InputSchema:      schema,
		Timeout:          5,
		Retries:          0,
		FailureThreshold: 3,
		CooldownSeconds:  30,
		Builtin:          false,
	}
}

// echoHandler 返回固定输出的 handler。
func echoHandler(tag string) domain.SkillHandler {
	return func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		return map[string]any{"tag": tag, "echo": in}, nil
	}
}

// ---------------------------------------------------------------------------
// Schema 校验器
// ---------------------------------------------------------------------------

func TestValidatorKeywords(t *testing.T) {
	v := &Validator{}
	schema := map[string]any{
		"type":     "object",
		"required": []any{"stacktrace", "language"},
		"properties": map[string]any{
			"stacktrace": map[string]any{"type": "string", "minLength": 5},
			"language":   map[string]any{"type": "string", "enum": []any{"java", "go"}},
			"mode":       map[string]any{"type": []any{"string", "number"}},
			"retries":    map[string]any{"type": "integer", "minimum": float64(0), "maximum": float64(5)},
			"patternKey": map[string]any{"type": "string", "pattern": "^[a-z]+$"},
			"frames": map[string]any{
				"type":     "array",
				"minItems": float64(1),
				"maxItems": float64(3),
				"items":    map[string]any{"type": "object", "required": []any{"file"}},
			},
			"shape": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "object", "required": []any{"a"}},
					map[string]any{"type": "object", "required": []any{"b"}},
				},
			},
			"unknownKw": map[string]any{"type": "string", "weirdKeyword": 42},
		},
	}

	// 全量合法文档
	ok := map[string]any{
		"stacktrace": "java.lang.NullPointerException: boom",
		"language":   "java",
		"mode":       float64(3),
		"retries":    float64(2),
		"patternKey": "abc",
		"frames":     []any{map[string]any{"file": "A.java"}},
		"shape":      map[string]any{"a": 1},
		"unknownKw":  "x",
	}
	if errs := v.Validate(schema, ok); len(errs) != 0 {
		t.Fatalf("合法文档不应报错, got=%v", errs)
	}

	// required
	errs := v.Validate(schema, map[string]any{"language": "java"})
	if !hasMessage(errs, "字段 stacktrace 必填") {
		t.Fatalf("required 校验未命中: %v", errs)
	}

	// type + enum
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "rust"})
	if !hasMessage(errs, "字段 language 取值必须是 [java go] 之一") {
		t.Fatalf("enum 校验未命中: %v", errs)
	}
	errs = v.Validate(schema, map[string]any{"stacktrace": 123456, "language": "java"})
	if !hasMessage(errs, "字段 stacktrace 类型必须是 string") {
		t.Fatalf("type 校验未命中: %v", errs)
	}

	// minLength
	errs = v.Validate(schema, map[string]any{"stacktrace": "abc", "language": "go"})
	if !hasMessage(errs, "字段 stacktrace 长度不得小于 5") {
		t.Fatalf("minLength 校验未命中: %v", errs)
	}

	// pattern
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go", "patternKey": "ABC"})
	if !hasMessage(errs, "字段 patternKey 必须匹配正则 ^[a-z]+$") {
		t.Fatalf("pattern 校验未命中: %v", errs)
	}

	// 多类型允许
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go", "mode": "fast"})
	if len(errs) != 0 {
		t.Fatalf("多类型 type 应放行字符串: %v", errs)
	}

	// integer / minimum / maximum
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go", "retries": float64(9)})
	if !hasMessage(errs, "字段 retries 不得大于 5") {
		t.Fatalf("maximum 校验未命中: %v", errs)
	}
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go", "retries": 1.5})
	if !hasMessage(errs, "字段 retries 类型必须是 integer") {
		t.Fatalf("integer 校验未命中: %v", errs)
	}

	// items / minItems
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go", "frames": []any{}})
	if !hasMessage(errs, "字段 frames 元素个数不得少于 1") {
		t.Fatalf("minItems 校验未命中: %v", errs)
	}
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go",
		"frames": []any{map[string]any{}}})
	if !hasMessage(errs, "字段 frames[0].file 必填") {
		t.Fatalf("items.required 校验未命中: %v", errs)
	}

	// oneOf
	errs = v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go",
		"shape": map[string]any{"a": 1, "b": 2}})
	if !hasMessage(errs, "字段 shape 必须且只能匹配 oneOf 中的一种模式") {
		t.Fatalf("oneOf 校验未命中: %v", errs)
	}

	// additionalProperties=false
	strict := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"a": map[string]any{"type": "string"}},
	}
	errs = v.Validate(strict, map[string]any{"a": "x", "b": 1})
	if !hasMessage(errs, "字段 b 不允许出现未声明的字段") {
		t.Fatalf("additionalProperties 校验未命中: %v", errs)
	}

	// 未知关键字忽略 + nil schema 通过
	if errs := v.Validate(schema, map[string]any{"stacktrace": "123456", "language": "go"}); len(errs) != 0 {
		t.Fatalf("未知关键字不应报错: %v", errs)
	}
	if errs := v.Validate(nil, map[string]any{"whatever": 1}); errs != nil {
		t.Fatalf("nil schema 必须返回 nil, got=%v", errs)
	}
}

func TestValidatorConstAnyOfAndFill(t *testing.T) {
	v := &Validator{}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":    map[string]any{"const": "patch"},
			"timeout": map[string]any{"type": "integer", "default": 30},
			"target":  map[string]any{"type": "string", "anyOf": []any{map[string]any{"const": "a"}, map[string]any{"const": "b"}}},
		},
	}
	doc := map[string]any{"kind": "patch", "target": "b"}
	if errs := v.ValidateAndFill(schema, doc); len(errs) != 0 {
		t.Fatalf("ValidateAndFill 不应报错: %v", errs)
	}
	if got, ok := doc["timeout"]; !ok || got != float64(30) {
		t.Fatalf("default 未填充: %v", doc)
	}
	if errs := v.Validate(schema, map[string]any{"kind": "other"}); !hasMessage(errs, "字段 kind 取值必须等于 patch") {
		t.Fatalf("const 校验未命中: %v", errs)
	}
	if errs := v.Validate(schema, map[string]any{"kind": "patch", "target": "c"}); !hasMessage(errs, "字段 target 必须至少匹配 anyOf 中的一种模式") {
		t.Fatalf("anyOf 校验未命中: %v", errs)
	}
}

// hasMessage 判断错误集合中是否存在包含指定文本的消息。
func hasMessage(errs []SchemaError, want string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 熔断器
// ---------------------------------------------------------------------------

func TestCircuitTripAndRecover(t *testing.T) {
	c := NewCircuit(3, 40*time.Millisecond, 1)
	if c.State() != CircuitClosed {
		t.Fatalf("初始状态应为 closed, got=%s", c.State())
	}
	for i := 0; i < 3; i++ {
		if !c.Allow() {
			t.Fatalf("第 %d 次调用应放行", i+1)
		}
		c.FailureWith(errors.New("boom"))
	}
	if c.State() != CircuitOpen {
		t.Fatalf("连续失败 3 次后应断开, got=%s", c.State())
	}
	if c.Allow() {
		t.Fatal("open 状态必须拒绝放行")
	}
	snap := c.Snapshot()
	if snap.Failures != 3 || snap.LastError != "boom" || snap.OpenUntil.IsZero() {
		t.Fatalf("快照字段不完整: %+v", snap)
	}

	time.Sleep(60 * time.Millisecond)
	if c.State() != CircuitHalfOpen {
		t.Fatalf("冷却结束后应进入 half_open, got=%s", c.State())
	}
	if !c.Allow() {
		t.Fatal("half_open 首个探测应放行")
	}
	if c.Allow() {
		t.Fatal("half_open 探测名额用尽后应拒绝")
	}
	c.Success()
	if c.State() != CircuitClosed {
		t.Fatalf("探测成功后应闭合, got=%s", c.State())
	}
	if got := c.Snapshot().Failures; got != 0 {
		t.Fatalf("成功后失败计数应清零, got=%d", got)
	}

	// half_open 探测失败重新断开并重置冷却
	for i := 0; i < 3; i++ {
		if c.Allow() {
			c.Failure()
		}
	}
	if c.State() != CircuitOpen {
		t.Fatalf("再次失败应回到 open, got=%s", c.State())
	}
}

func TestCircuitDefaultsAndConcurrency(t *testing.T) {
	var zero Circuit
	if zero.Allow() != true || zero.State() != CircuitClosed {
		t.Fatal("零值熔断器应可用且默认闭合")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if !zero.Allow() {
				return
			}
			if i%3 == 0 {
				zero.FailureWith(errors.New("x"))
				return
			}
			zero.Success()
			_ = zero.State()
			_ = zero.Snapshot()
		}(i)
	}
	wg.Wait()
	if got := zero.Snapshot().TotalCalls; got <= 0 {
		t.Fatalf("并发调用统计异常: %d", got)
	}

	// 非法构造参数回落到默认值 3 / 30s / 1
	c := NewCircuit(-1, 0, 0)
	if c.failureThreshold != 3 || c.cooldown != 30*time.Second || c.halfOpenProbes != 1 {
		t.Fatalf("默认参数未生效: %+v", c)
	}
}

// ---------------------------------------------------------------------------
// 注册中心
// ---------------------------------------------------------------------------

func TestRegistryHotUpdateAndList(t *testing.T) {
	reg := NewRegistry(testConfig(), logx.Nop())
	ctx := context.Background()

	a := testManifest("t_hot", "1.0.0", nil)
	if err := reg.Register(ctx, a, echoHandler("A")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	e, err := reg.SelectHandler("t_hot", "")
	if err != nil {
		t.Fatalf("选择 handler 失败: %v", err)
	}
	out, _ := e.handler(ctx, nil, nil)
	if out["tag"] != "A" {
		t.Fatalf("首次注册 handler 未生效: %v", out)
	}

	// 同版本覆盖 = 热更新
	if err := reg.Register(ctx, a, echoHandler("B")); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	e, _ = reg.SelectHandler("t_hot", "")
	out, _ = e.handler(ctx, nil, nil)
	if out["tag"] != "B" {
		t.Fatalf("热更新后 handler 应为 B: %v", out)
	}

	// 非法清单
	if err := reg.Register(ctx, domain.SkillManifest{Name: "", Version: "1.0.0"}, echoHandler("X")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("空技能名应报 ErrInvalidManifest, got=%v", err)
	}
	if err := reg.Register(ctx, domain.SkillManifest{Name: "x", Version: "1.0.0", Builtin: true}, echoHandler("X")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("未知内置名应报 ErrInvalidManifest, got=%v", err)
	}
	if err := reg.Register(ctx, testManifest("t_nil", "1.0.0", nil), nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("nil handler 应报 ErrInvalidManifest, got=%v", err)
	}
}

func TestRegistryListFilterAndPriority(t *testing.T) {
	reg := NewRegistry(testConfig(), logx.Nop())
	ctx := context.Background()

	p30 := testManifest("t_p30", "1.0.0", nil)
	p30.Priority = 30
	p10 := testManifest("t_p10", "1.0.0", nil)
	p10.Priority = 10
	p20 := testManifest("t_p20", "1.0.0", nil)
	p20.Priority = 20
	p20.Status = domain.SkillDisabled

	for _, m := range []domain.SkillManifest{p30, p10, p20} {
		if err := reg.Register(ctx, m, echoHandler(m.Name)); err != nil {
			t.Fatalf("注册 %s 失败: %v", m.Name, err)
		}
	}

	list := reg.List(ctx, "")
	if len(list) != 2 {
		t.Fatalf("disabled 技能不应出现: %+v", list)
	}
	if list[0].Name != "t_p10" || list[1].Name != "t_p30" {
		t.Fatalf("Priority 排序错误: %s,%s", list[0].Name, list[1].Name)
	}
	if _, err := reg.Get(ctx, "t_p20"); err != nil {
		t.Fatalf("Get 应可查看被禁用技能（供控制台恢复）: %v", err)
	}
	if e, err := reg.SelectHandler("t_p20", ""); err == nil {
		t.Fatalf("被禁用技能不应可执行: %+v", e)
	}
	names := reg.Names()
	if len(names) != 3 {
		t.Fatalf("Names 应包含 disabled 技能: %v", names)
	}

	// 按分类过滤
	if got := reg.List(ctx, string(domain.CatParse)); len(got) != 2 {
		t.Fatalf("分类过滤失败: %+v", got)
	}
	if got := reg.List(ctx, string(domain.CatRepair)); len(got) != 0 {
		t.Fatalf("不匹配分类应为空: %+v", got)
	}
}

func TestRegistryCanaryAndRollback(t *testing.T) {
	reg := NewRegistry(testConfig(), logx.Nop())
	ctx := context.Background()

	v1 := testManifest("t_roll", "1.0.0", nil)
	v2 := testManifest("t_roll", "2.0.0", nil)
	v3 := testManifest("t_roll", "1.1.0", nil)
	v3.Status = domain.SkillCanary
	v3.CanaryPercent = 50
	if err := reg.Register(ctx, v1, echoHandler("v1")); err != nil {
		t.Fatalf("注册 v1 失败: %v", err)
	}
	if err := reg.Register(ctx, v2, echoHandler("v2")); err != nil {
		t.Fatalf("注册 v2 失败: %v", err)
	}
	if e, err := reg.SelectHandler("t_roll", ""); err != nil || e.manifest.Version != "2.0.0" {
		t.Fatalf("当前生效版本应为 2.0.0: %v %v", e, err)
	}
	if err := reg.Register(ctx, v3, echoHandler("v3")); err != nil {
		t.Fatalf("注册 canary 失败: %v", err)
	}
	// 灰度命中 → canary 版本成为当前生效版本（抽样决策注入，保证测试确定性）
	reg.SetCanaryDecision("t_roll", true)
	if e, err := reg.SelectHandler("t_roll", ""); err != nil || e.manifest.Version != "1.1.0" {
		t.Fatalf("灰度命中应使用 canary 版本 1.1.0: %v %v", e, err)
	}

	// 灰度抽样未命中 → 回退最高的 enabled 版本
	reg.SetCanaryDecision("t_roll", false)
	if e, err := reg.SelectHandler("t_roll", ""); err != nil || e.manifest.Version != "2.0.0" {
		t.Fatalf("灰度未命中应回退 2.0.0: %v %v", e, err)
	}

	// SetStatus 显式启用低版本
	if err := reg.SetStatus(ctx, "t_roll", "1.0.0", domain.SkillEnabled); err != nil {
		t.Fatalf("SetStatus 失败: %v", err)
	}
	if _, err := reg.Get(ctx, "t_roll"); err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if err := reg.SetStatus(ctx, "t_roll", "1.0.0", domain.SkillStatus("bogus")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("非法状态应报错, got=%v", err)
	}
	if err := reg.SetStatus(ctx, "t_roll", "9.9.9", domain.SkillEnabled); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("不存在版本应报 ErrSkillNotFound, got=%v", err)
	}

	// 回滚：当前 1.0.0 → 切回 2.0.0（按语义化版本降序第一个可用且不同于当前的版本）
	got, err := reg.Rollback(ctx, "t_roll")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got != "2.0.0" {
		t.Fatalf("回滚版本错误: %s", got)
	}
	if e, _ := reg.SelectHandler("t_roll", ""); e.manifest.Version != "2.0.0" {
		t.Fatalf("回滚后当前版本应为 2.0.0: %v", e.manifest.Version)
	}

	// 单版本技能无历史可回滚
	solo := testManifest("t_solo", "1.0.0", nil)
	if err := reg.Register(ctx, solo, echoHandler("solo")); err != nil {
		t.Fatalf("注册 solo 失败: %v", err)
	}
	if _, err := reg.Rollback(ctx, "t_solo"); !errors.Is(err, ErrNoRollback) {
		t.Fatalf("应返回 ErrNoRollback, got=%v", err)
	}
	if _, err := reg.Rollback(ctx, "t_missing"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("未知技能应返回 ErrSkillNotFound, got=%v", err)
	}
}

func TestRegistryHealthAndManifestLoad(t *testing.T) {
	reg := NewRegistry(testConfig(), logx.Nop())
	ctx := context.Background()
	m := testManifest("t_health", "1.0.0", nil)
	if err := reg.Register(ctx, m, echoHandler("h")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	reg.recordCall("t_health", "1.0.0", 40, false, "")
	reg.recordCall("t_health", "1.0.0", 20, true, "boom")
	health := reg.Health(ctx)
	if len(health) != 1 {
		t.Fatalf("Health 应返回 1 条: %+v", health)
	}
	h := health[0]
	if !h.Healthy || h.TotalCalls != 2 || h.AvgLatencyMS != 30 || h.LastError != "boom" {
		t.Fatalf("健康度统计错误: %+v", h)
	}

	// 覆盖配置生效
	cfg := testConfig()
	cfg.Overrides = []config.SkillOverride{{
		Name: "t_health", TimeoutSec: 12, Retries: 2, FailureThreshold: 9,
		CooldownSec: 7, FallbackSkills: []string{"t_other"}, CanaryPercent: 30,
	}}
	reg2 := NewRegistry(cfg, logx.Nop())
	if err := reg2.Register(ctx, testManifest("t_health", "1.0.0", nil), echoHandler("h")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	got, err := reg2.Get(ctx, "t_health")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.Timeout != 12 || got.Retries != 2 || got.FailureThreshold != 9 || got.CooldownSeconds != 7 ||
		len(got.FallbackSkills) != 1 || got.Status != domain.SkillCanary {
		t.Fatalf("覆盖配置未生效: %+v", got)
	}

	// 默认值补齐
	reg3 := NewRegistry(config.SkillConfig{}, logx.Nop())
	bare := domain.SkillManifest{Name: "t_bare", Version: "1.0.0", Builtin: false}
	if err := reg3.Register(ctx, bare, echoHandler("b")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	gotBare, _ := reg3.Get(ctx, "t_bare")
	if gotBare.Status != domain.SkillEnabled || gotBare.Timeout != 60 ||
		gotBare.FailureThreshold != 3 || gotBare.CooldownSeconds != 30 {
		t.Fatalf("默认值补齐失败: %+v", gotBare)
	}
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// newRunnerFixture 构造带假 Recorder 的执行环境。
func newRunnerFixture(t *testing.T) (*Registry, *Runner, *fakeRecorder) {
	t.Helper()
	cfg := testConfig()
	reg := NewRegistry(cfg, logx.Nop())
	rec := &fakeRecorder{}
	runner := NewRunner(reg, cfg, rec, logx.Nop())
	return reg, runner, rec
}

func TestRunnerSchemaValidationSkips(t *testing.T) {
	reg, runner, rec := newRunnerFixture(t)
	ctx := context.Background()
	schema := map[string]any{
		"type":     "object",
		"required": []any{"stacktrace"},
		"properties": map[string]any{
			"stacktrace": map[string]any{"type": "string", "minLength": 3},
		},
	}
	var called int32
	m := testManifest("t_schema", "1.0.0", schema)
	if err := reg.Register(ctx, m, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		atomic.AddInt32(&called, 1)
		return map[string]any{"ok": true}, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	res, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_schema", Input: map[string]any{}})
	var sve *SchemaValidationError
	if !errors.As(err, &sve) {
		t.Fatalf("应返回 *SchemaValidationError, got=%v", err)
	}
	if res == nil || res.Status != domain.CallSkipped || res.Attempts != 0 {
		t.Fatalf("非法入参应返回 CallSkipped: %+v", res)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("入参非法时 handler 不应被调用")
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].Status != domain.CallSkipped || calls[0].InputDigest == "" {
		t.Fatalf("审计记录错误: %+v", calls)
	}
}

func TestRunnerRetriesAndAudit(t *testing.T) {
	reg, runner, rec := newRunnerFixture(t)
	ctx := context.Background()
	var attempts int32
	m := testManifest("t_retry", "1.0.0", nil)
	m.Retries = 2
	if err := reg.Register(ctx, m, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			return nil, errors.New("临时故障")
		}
		return map[string]any{"token": "secret-value", "ok": true}, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	cc := &domain.CallContext{TenantID: "t1", TaskID: "task1", RunID: "run1", Stage: "root_cause",
		PinnedCommits: map[string]string{"r1": "abc"}}
	res, err := runner.Run(ctx, cc, domain.SkillInvocation{Skill: "t_retry", Input: map[string]any{"a": 1}})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if res.Attempts != 3 || res.Status != domain.CallOK {
		t.Fatalf("重试次数应为 Retries+1=3: %+v", res)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("handler 实际调用次数应为 3, got=%d", got)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应记录 1 条技能调用: %+v", calls)
	}
	c := calls[0]
	if c.RunID != "run1" || c.TenantID != "t1" || c.Stage != "root_cause" || c.Status != domain.CallOK {
		t.Fatalf("审计上下文字段错误: %+v", c)
	}
	if len(c.InputDigest) != digestLength || len(c.OutputDigest) != digestLength {
		t.Fatalf("摘要长度应为 16: %q %q", c.InputDigest, c.OutputDigest)
	}
	outMap, _ := c.Output.(map[string]any)
	if outMap == nil || outMap["token"] != "***" {
		t.Fatalf("审计输出应脱敏: %+v", c.Output)
	}
	if _, err := parseUUID(c.ID); err != nil {
		t.Fatalf("审计 ID 应为 uuid: %q", c.ID)
	}
}

func TestRunnerCircuitBlocks(t *testing.T) {
	reg, runner, _ := newRunnerFixture(t)
	ctx := context.Background()
	m := testManifest("t_breaker", "1.0.0", nil)
	m.FailureThreshold = 2
	if err := reg.Register(ctx, m, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		return nil, errors.New("持续失败")
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		res, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_breaker"})
		if err == nil || res.Status != domain.CallFailed {
			t.Fatalf("第 %d 次调用应失败: %+v %v", i+1, res, err)
		}
	}
	res, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_breaker"})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("熔断后应返回 ErrCircuitOpen, got=%v", err)
	}
	if res == nil || !res.Blocked || res.Status != domain.CallBlocked {
		t.Fatalf("熔断后应返回 Blocked 结果: %+v", res)
	}
	if e, _ := reg.SelectHandler("t_breaker", ""); e.circuit.State() != CircuitOpen {
		t.Fatalf("熔断器状态应为 open: %s", e.circuit.State())
	}
}

func TestRunnerFallbackChain(t *testing.T) {
	reg, runner, rec := newRunnerFixture(t)
	ctx := context.Background()
	primary := testManifest("t_primary", "1.0.0", nil)
	primary.FallbackSkills = []string{"t_fb"}
	if err := reg.Register(ctx, primary, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		return nil, errors.New("主技能不可用")
	}); err != nil {
		t.Fatalf("注册主技能失败: %v", err)
	}
	if err := reg.Register(ctx, testManifest("t_fb", "1.0.0", nil), echoHandler("fb")); err != nil {
		t.Fatalf("注册兜底技能失败: %v", err)
	}

	res, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_primary", Input: map[string]any{"x": 1}})
	if err != nil {
		t.Fatalf("兜底成功时不应返回错误: %v", err)
	}
	if !res.FromFallback || res.FallbackFrom != "t_primary" || res.Status != domain.CallFallback {
		t.Fatalf("兜底结果字段错误: %+v", res)
	}
	if res.Skill != "t_fb" || res.Output["tag"] != "fb" {
		t.Fatalf("兜底应使用 t_fb 的输出: %+v", res)
	}
	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("应记录 2 条调用（主 + 兜底）: %+v", calls)
	}
	if !calls[1].FromFallback || calls[1].Status != domain.CallFallback {
		t.Fatalf("兜底调用审计错误: %+v", calls[1])
	}
}

func TestRunnerPanicAndNilContext(t *testing.T) {
	reg, runner, rec := newRunnerFixture(t)
	ctx := context.Background()
	if err := reg.Register(ctx, testManifest("t_panic", "1.0.0", nil),
		func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
			panic("handler 崩溃")
		}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	res, err := runner.Run(nil, nil, domain.SkillInvocation{Skill: "t_panic"})
	if err == nil {
		t.Fatal("panic 应转为错误返回")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("错误信息应说明 panic: %v", err)
	}
	if res == nil || res.Status != domain.CallFailed {
		t.Fatalf("panic 后应返回 failed 结果: %+v", res)
	}
	if len(rec.snapshot()) != 1 {
		t.Fatal("panic 也应留下审计记录")
	}

	// cc 为 nil、cc.Sandbox 为 nil 均不得 panic，且入参深拷贝不可篡改调用方数据
	seen := map[string]string(nil)
	if err := reg.Register(ctx, testManifest("t_ctx", "1.0.0", nil),
		func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
			if cc == nil {
				t.Error("handler 不应收到 nil CallContext")
			}
			if cc.Sandbox != nil {
				t.Error("Sandbox 应为 nil")
			}
			seen = cc.PinnedCommits
			in["injected"] = "hacked"
			return map[string]any{"ok": true}, nil
		}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	cc := &domain.CallContext{TenantID: "t", PinnedCommits: map[string]string{"r": "c"}}
	input := map[string]any{"keep": "original"}
	if _, err := runner.Run(ctx, cc, domain.SkillInvocation{Skill: "t_ctx", Input: input}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if _, mutated := input["injected"]; mutated {
		t.Fatal("handler 不应篡改调用方入参")
	}
	if seen["r"] != "c" {
		t.Fatalf("PinnedCommits 应透传给 handler: %v", seen)
	}
}

func TestRunnerTimeout(t *testing.T) {
	reg, runner, rec := newRunnerFixture(t)
	ctx := context.Background()

	// 场景一：注入 40ms 超时，handler 响应 ctx 取消；重试受“超时最多 1 次”限制。
	m := testManifest("t_timeout", "1.0.0", nil)
	m.Timeout = 30
	m.Retries = 3
	if err := reg.Register(ctx, m, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return map[string]any{"never": true}, nil
		}
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	runner.SetTimeoutOverride("t_timeout", 40*time.Millisecond)
	start := time.Now()
	res, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_timeout"})
	if !errors.Is(err, ErrSkillTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回超时错误, got=%v", err)
	}
	if res.Status != domain.CallTimeout {
		t.Fatalf("状态应为 timeout: %+v", res)
	}
	if res.Attempts != 2 {
		t.Fatalf("超时最多重试 1 次（共 2 次尝试），got=%d", res.Attempts)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未及时中断: %s", elapsed)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0].Status != domain.CallTimeout {
		t.Fatalf("超时审计错误: %+v", calls)
	}

	// 场景二：清单 Timeout=1 秒，handler 忽略 ctx，runner 仍须按超时返回。
	m2 := testManifest("t_timeout_real", "1.0.0", nil)
	m2.Timeout = 1
	m2.Retries = 0
	if err := reg.Register(ctx, m2, func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		time.Sleep(1500 * time.Millisecond)
		return map[string]any{"late": true}, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	start = time.Now()
	res, err = runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_timeout_real"})
	if err == nil || res.Status != domain.CallTimeout {
		t.Fatalf("清单 Timeout 未生效: %+v %v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 1400*time.Millisecond {
		t.Fatalf("应在 1s 左右中断, got=%s", elapsed)
	}
}

func TestRunnerStatsAndRegistryBackedSelection(t *testing.T) {
	reg, runner, _ := newRunnerFixture(t)
	ctx := context.Background()
	if err := reg.Register(ctx, testManifest("t_stats", "1.0.0", nil), echoHandler("s")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := reg.Register(ctx, testManifest("t_fb2", "1.0.0", nil), echoHandler("fb")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "t_stats", Input: map[string]any{}}); err != nil {
			t.Fatalf("执行失败: %v", err)
		}
	}
	stats := runner.Stats()
	if len(stats) != 1 || stats[0].Skill != "t_stats" || stats[0].Calls != 3 || stats[0].Failures != 0 {
		t.Fatalf("Stats 统计错误: %+v", stats)
	}
	if stats[0].AvgLatencyMS < 0 {
		t.Fatalf("平均延迟异常: %+v", stats[0])
	}
	if _, err := runner.Run(ctx, nil, domain.SkillInvocation{Skill: "nope"}); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("未知技能应报 ErrSkillNotFound, got=%v", err)
	}
	health := reg.Health(ctx)
	found := false
	for _, h := range health {
		if h.Skill == "t_stats" && h.TotalCalls == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("Registry.Health 应反映 runner 统计: %+v", health)
	}
}

func TestRegistryLoadManifests(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(testConfig(), logx.Nop())
	ctx := context.Background()
	if err := reg.Register(ctx, testManifest("t_ext", "1.0.0", nil), echoHandler("builtin")); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	content := `[
	  {"name":"t_ext","version":"1.0.0","title":"覆盖超时","category":"parse","timeout":9,"priority":5,"builtin":false},
	  {"name":"t_custom","version":"0.1.0","title":"外部技能","category":"repair","builtin":false}
	]`
	if err := writeFile(dir, "skills.json", content); err != nil {
		t.Fatalf("写入清单失败: %v", err)
	}
	loaded, err := reg.LoadManifests(dir)
	if err != nil {
		t.Fatalf("加载清单失败: %v", err)
	}
	if loaded != 2 {
		t.Fatalf("应加载 2 条清单, got=%d", loaded)
	}
	m, err := reg.Get(ctx, "t_ext")
	if err != nil || m.Timeout != 9 {
		t.Fatalf("清单覆盖未生效: %+v %v", m, err)
	}
	if _, err := reg.Get(ctx, "t_custom"); err != nil {
		t.Fatalf("自定义技能应注册成功: %v", err)
	}
	// 目录不存在时不报错
	if n, err := reg.LoadManifests(dir + "/not-exists"); n != 0 || err != nil {
		t.Fatalf("目录缺失应静默跳过: %d %v", n, err)
	}
}
