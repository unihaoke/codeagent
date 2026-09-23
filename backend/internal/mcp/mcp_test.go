package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeOpenAI 模拟 OpenAI 兼容接口（不依赖真实网络）。
type fakeOpenAI struct {
	status int      // 非 0 且 >=400 时固定返回该状态码
	texts  []string // 依次返回的内容；耗尽后重复最后一条

	hits     atomic.Int64
	mu       sync.Mutex
	lastBody map[string]any
}

// newFakeOpenAI 启动模拟服务，返回服务与提供方配置。
func newFakeOpenAI(t *testing.T, name, model string, status int, texts ...string) (*fakeOpenAI, config.ProviderConfig) {
	t.Helper()
	f := &fakeOpenAI{status: status, texts: texts}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, config.ProviderConfig{
		Name:        name,
		Kind:        "openai_compatible",
		BaseURL:     srv.URL,
		APIKey:      "test-key-" + name,
		Models:      []string{model},
		Tier:        "strong",
		MaxTokens:   1024,
		Temperature: 0.1,
		Weight:      1,
	}
}

// handle 处理 /v1/chat/completions。
func (f *fakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.lastBody = body
	f.mu.Unlock()

	n := f.hits.Add(1)
	if f.status >= 400 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom","type":"server_error"}}`))
		return
	}
	text := ""
	if len(f.texts) > 0 {
		idx := int(n) - 1
		if idx >= len(f.texts) {
			idx = len(f.texts) - 1
		}
		text = f.texts[idx]
	}
	resp := map[string]any{
		"model": "fake-" + fmt.Sprint(n),
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// requestBody 返回最近一次请求体。
func (f *fakeOpenAI) requestBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// fakeRecorder 捕获模型调用记录。
type fakeRecorder struct {
	mu    sync.Mutex
	calls []domain.ModelCall
}

func (f *fakeRecorder) Audit(ev domain.AuditEvent)      {}
func (f *fakeRecorder) SkillCall(call domain.SkillCall) {}

func (f *fakeRecorder) ModelCall(call domain.ModelCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRecorder) ListAudits(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.AuditEvent], error) {
	return domain.Page[domain.AuditEvent]{}, nil
}

func (f *fakeRecorder) ListSkillCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.SkillCall], error) {
	return domain.Page[domain.SkillCall]{}, nil
}

func (f *fakeRecorder) ListModelCalls(ctx context.Context, tenantID, runID string, q domain.PageQuery) (domain.Page[domain.ModelCall], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return domain.Page[domain.ModelCall]{Items: append([]domain.ModelCall(nil), f.calls...), Total: len(f.calls)}, nil
}

func (f *fakeRecorder) Observability(ctx context.Context, tenantID string) (*domain.ObservabilitySummary, error) {
	return &domain.ObservabilitySummary{}, nil
}

// snapshot 返回记录快照。
func (f *fakeRecorder) snapshot() []domain.ModelCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ModelCall(nil), f.calls...)
}

// testMCPConfig 构造测试用模型层配置。
func testMCPConfig(providers ...config.ProviderConfig) config.MCPConfig {
	return config.MCPConfig{
		Enabled:    true,
		Default:    "p1",
		TimeoutSec: 5,
		MaxRetries: 0,
		Providers:  providers,
		Routing: config.RoutingConfig{
			StrongStages: []string{"root_cause", "patch_synthesize"},
			LightStages:  []string{"log_clean", "classify", "summary"},
			CostAware:    true,
		},
		Budget:  config.ContextBudgetConfig{MaxChars: 20000, MaxSlices: 12, MaxSliceChars: 2000, MaxLogChars: 1500, ReserveForOutput: 2000},
		Circuit: config.CircuitConfig{FailureThreshold: 3, CooldownSec: 30, HalfOpenProbes: 1},
		Mock:    config.MockConfig{Enabled: true, Model: "mock-reasoner-v1", LatencyMS: 1},
	}
}

// newTestGateway 构造测试用网关。
func newTestGateway(t *testing.T, cfg config.MCPConfig, rec domain.Recorder) *Gateway {
	t.Helper()
	g, err := NewGateway(cfg, config.EngineConfig{MaxRetries: cfg.MaxRetries}, rec, logx.Nop())
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
	return g
}

// callCtx 构造调用上下文。
func callCtx() *domain.CallContext {
	return &domain.CallContext{TenantID: "t-1", TaskID: "task-1", RunID: "run-1", Stage: "root_cause"}
}

// validRootCauseJSON 合法根因 JSON。
const validRootCauseJSON = `{"summary":"OrderService.java:104 处抛出 java.lang.NullPointerException","category":"null_pointer","detail":"orderRepo.findById 返回 null 后直接调用 toDTO","confidence":0.78,"evidence":["OrderService.java:104"],"blastRadius":["order-service"],"severity":"critical"}`

// ---------------------------------------------------------------------------
// ParseStructured / ValidateAgainst
// ---------------------------------------------------------------------------

func TestParseStructured(t *testing.T) {
	nested := `{"summary":"嵌套","category":"type_error","detail":"含 } 与 { 的字符串","confidence":0.6,"evidence":["a.ts:1"],"blastRadius":["web"],"severity":"major","extra":{"inner":{"deep":"}"}}}`

	cases := []struct {
		name    string
		text    string
		schema  map[string]any
		wantErr bool
		check   func(t *testing.T, doc map[string]any)
	}{
		{
			name:   "纯 JSON",
			text:   validRootCauseJSON,
			schema: RootCauseSchema,
			check: func(t *testing.T, doc map[string]any) {
				if doc["category"] != "null_pointer" {
					t.Fatalf("category = %v", doc["category"])
				}
			},
		},
		{
			name:   "json 代码围栏",
			text:   "```json\n" + validRootCauseJSON + "\n```",
			schema: RootCauseSchema,
			check: func(t *testing.T, doc map[string]any) {
				if doc["severity"] != "critical" {
					t.Fatalf("severity = %v", doc["severity"])
				}
			},
		},
		{
			name:   "无语言标记围栏",
			text:   "```\n" + validRootCauseJSON + "\n```\n",
			schema: RootCauseSchema,
		},
		{
			name:   "前后夹杂解释文字",
			text:   "好的，下面是我的分析结果：\n" + validRootCauseJSON + "\n以上就是根因，请复核。",
			schema: RootCauseSchema,
		},
		{
			name:   "单引号加尾随逗号",
			text:   "{'category': 'timeout', 'severity': 'major', 'confidence': 0.61,}",
			schema: ClassifySchema,
			check: func(t *testing.T, doc map[string]any) {
				if doc["category"] != "timeout" || doc["severity"] != "major" {
					t.Fatalf("解析结果异常: %#v", doc)
				}
			},
		},
		{
			name:   "嵌套花括号与字符串内的右花括号",
			text:   "分析如下：\n" + nested,
			schema: nil,
			check: func(t *testing.T, doc map[string]any) {
				inner, ok := doc["extra"].(map[string]any)
				if !ok {
					t.Fatalf("嵌套对象解析失败: %#v", doc["extra"])
				}
				deep, _ := inner["inner"].(map[string]any)
				if deep["deep"] != "}" {
					t.Fatalf("字符串内的 } 处理错误: %#v", deep)
				}
			},
		},
		{
			name:   "Python 字面量与全角引号",
			text:   "{“category”: “dependency_missing”, “severity”: “critical”, “confidence”: 0.7, “passed”: True, “note”: None}",
			schema: nil,
			check: func(t *testing.T, doc map[string]any) {
				if doc["passed"] != true {
					t.Fatalf("True 未转换为 true: %#v", doc["passed"])
				}
				if v, ok := doc["note"]; !ok || v != nil {
					t.Fatalf("None 未转换为 null: %#v", v)
				}
			},
		},
		{
			name:    "非法输入返回错误",
			text:    "这段文本里没有任何 JSON 结构，只有中文说明。",
			schema:  RootCauseSchema,
			wantErr: true,
		},
		{
			name:    "空输入返回错误",
			text:    "   ",
			schema:  nil,
			wantErr: true,
		},
		{
			name:    "缺少必填字段触发校验失败",
			text:    `{"summary":"只有一个字段"}`,
			schema:  RootCauseSchema,
			wantErr: true,
		},
		{
			name:    "枚举外的取值触发校验失败",
			text:    `{"summary":"x","category":"不存在的分类","detail":"d","confidence":0.5,"evidence":[],"blastRadius":[],"severity":"major"}`,
			schema:  RootCauseSchema,
			wantErr: true,
		},
		{
			name: "多余字段被忽略而不失败",
			text: `{"summary":"s","category":"timeout","detail":"d","confidence":0.5,"evidence":[],"blastRadius":[],` +
				`"severity":"major","unknownField":{"a":1},"another":"x"}`,
			schema: RootCauseSchema,
			check: func(t *testing.T, doc map[string]any) {
				if doc["category"] != "timeout" {
					t.Fatalf("category = %v", doc["category"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := ParseStructured(tc.text, tc.schema)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际解析成功: %#v", doc)
				}
				if !strings.Contains(err.Error(), ErrUnstructuredOutput.Error()) {
					t.Fatalf("错误应可识别为 ErrUnstructuredOutput，实际: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if tc.check != nil {
				tc.check(t, doc)
			}
		})
	}
}

func TestValidateAgainstChineseMessages(t *testing.T) {
	doc := map[string]any{
		"patches": []any{
			map[string]any{
				"repositoryId": "repo-1",
				"repoKey":      "order-service",
				"filePath":     "OrderService.java",
				"action":       "rewrite", // 非法枚举
				"line":         10,
				"oldSnippet":   "a",
				"newSnippet":   "b",
				"rationale":    "r",
				"risk":         "medium",
				"confidence":   "0.8", // 类型错误
			},
		},
		"rationale": "总说明",
		"risk":      "medium",
	}
	errs := ValidateAgainst(PatchSchema, doc)
	if len(errs) < 2 {
		t.Fatalf("期望至少两条校验错误，实际 %v", errs)
	}
	joined := strings.Join(errs, "；")
	for _, want := range []string{"action", "confidence", "枚举", "类型"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("校验错误缺少 %q: %s", want, joined)
		}
	}
	if got := ValidateAgainst(PatchSchema, map[string]any{"patches": []any{}, "rationale": "x", "risk": "low"}); len(got) != 0 {
		t.Fatalf("空补丁数组应通过校验，实际: %v", got)
	}
}

// ---------------------------------------------------------------------------
// ContextPacker
// ---------------------------------------------------------------------------

// buildLargeBundle 构造含 30 个切片与超长日志的证据包。
func buildLargeBundle() *domain.EvidenceBundle {
	lines := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		lines = append(lines, fmt.Sprintf("2024-05-01 10:00:%02d INFO [order-service] DEBUG pool stats active=3 idle=%d", i%60, i))
	}
	stack := &domain.StackAnalysis{
		Language:      "java",
		ExceptionType: "java.lang.NullPointerException",
		Message:       "Cannot invoke \"com.acme.order.OrderDTO.getId()\" because \"order\" is null",
		Thread:        "http-nio-8080-exec-3",
		CausedBy:      []string{"java.lang.NullPointerException: order is null", "at com.acme.order.OrderService.getOrder(OrderService.java:104)"},
		Frames: []domain.StackFrame{
			{Raw: "at com.acme.order.OrderService.getOrder(OrderService.java:104)", File: "OrderService.java", Line: 104, Confidence: 0.9},
			{Raw: "at com.acme.order.OrderController.detail(OrderController.java:57)", File: "OrderController.java", Line: 57, Confidence: 0.8},
		},
		FileHints:  []string{"src/main/java/com/acme/order/OrderService.java"},
		Symbols:    []string{"OrderService", "getOrder"},
		Category:   "null_pointer",
		CleanedLog: strings.Join(lines, "\n"),
	}

	slices := make([]domain.CodeSlice, 0, 30)
	for i := 0; i < 30; i++ {
		big := i == 29 // 第 30 个切片内容超长，用于验证焦点窗口裁剪
		var sb strings.Builder
		lineCount := 60
		if big {
			lineCount = 300
		}
		for l := 0; l < lineCount; l++ {
			if big {
				sb.WriteString(fmt.Sprintf("// pad %03d placeholder\n", l))
			} else {
				sb.WriteString(fmt.Sprintf("int value%d = compute(%d);\n", l, l))
			}
		}
		content := sb.String()
		if big {
			content = strings.Replace(content, "// pad 060 placeholder", "return order.toDTO();", 1)
		}
		reason := "other"
		focus := []int(nil)
		if i == 29 {
			reason = "frame"
			focus = []int{160}
		} else if i >= 26 {
			reason = "symbol"
			focus = []int{102}
		}
		slices = append(slices, domain.CodeSlice{
			RepositoryID: fmt.Sprintf("repo-%d", i),
			RepoKey:      fmt.Sprintf("service-%d", i),
			Commit:       fmt.Sprintf("commit%024d", i),
			Path:         fmt.Sprintf("src/main/java/com/acme/service%d/Handler%d.java", i, i),
			Language:     "java",
			Content:      content,
			StartLine:    100,
			EndLine:      100 + strings.Count(content, "\n"),
			FocusLines:   focus,
			Reason:       reason,
			Chars:        len(content),
		})
	}

	return &domain.EvidenceBundle{
		RunID:     "run-ctx-1",
		TenantID:  "tenant-1",
		CreatedAt: time.Now(),
		Stack:     stack,
		Slices:    slices,
		Edges: []domain.CallEdge{
			{FromRepo: "web-portal", ToRepo: "order-service", Protocol: "http", Endpoint: "/api/order/{id}", Confidence: 0.82},
		},
		FilesLoaded: 30,
		CodeChars:   400000,
		Notes:       []string{"源码懒加载已按 LoadBudget 命中 30 个文件"},
	}
}

func TestContextPackerBudget(t *testing.T) {
	packer := NewContextPacker(config.ContextBudgetConfig{}, logx.Nop())
	bundle := buildLargeBundle()
	budget := domain.ContextBudget{MaxChars: 20000, MaxSlices: 12, MaxSliceChars: 2000, MaxLogChars: 1500, ReserveForOutput: 2000}

	packed, err := packer.Pack(bundle, budget)
	if err != nil {
		t.Fatalf("Pack 失败: %v", err)
	}
	limit := budget.MaxChars - budget.ReserveForOutput
	if len(packed.Text) > limit {
		t.Fatalf("上下文长度 %d 超出预算 %d", len(packed.Text), limit)
	}
	if packed.Chars != len(packed.Text) {
		t.Fatalf("Chars 与实际长度不一致: %d vs %d", packed.Chars, len(packed.Text))
	}
	if !packed.Truncated {
		t.Fatal("发生裁剪时必须置 Truncated=true")
	}
	if len(packed.Notes) == 0 {
		t.Fatal("裁剪动作必须写入 Notes")
	}
	var noteText string
	for _, n := range packed.Notes {
		noteText += n + "\n"
	}
	if !strings.Contains(noteText, "裁剪") {
		t.Fatalf("Notes 应包含中文裁剪说明，实际: %s", noteText)
	}

	// 分节：五个章节齐备，且切片章节被标记为已裁剪。
	if len(packed.Sections) != 5 {
		t.Fatalf("期望 5 个分节，实际 %d: %#v", len(packed.Sections), packed.Sections)
	}
	names := make([]string, 0, 5)
	dropped := map[string]bool{}
	for _, s := range packed.Sections {
		names = append(names, s.Name)
		dropped[s.Name] = s.Dropped
	}
	wantOrder := []string{sectionStack, sectionSlices, sectionEdges, sectionDeps, sectionEnv}
	for i, want := range wantOrder {
		if names[i] != want {
			t.Fatalf("分节顺序错误: %v", names)
		}
	}
	if !dropped[sectionSlices] {
		t.Fatalf("切片章节应标记 Dropped=true: %#v", packed.Sections)
	}
	if dropped[sectionEnv] {
		t.Fatalf("总预算充足时环境信息不应被丢弃: %#v", packed.Sections)
	}

	// 切片数量被裁剪到 MaxSlices 以内。
	blocks := strings.Count(packed.Text, "--- 文件:")
	if blocks == 0 || blocks > budget.MaxSlices {
		t.Fatalf("切片数量应被裁剪到 1..%d，实际 %d", budget.MaxSlices, blocks)
	}
	// 高优先级切片（命中堆栈 + 焦点行）必须保留，且被窗口裁剪并带标记。
	if !strings.Contains(packed.Text, "service-29/") {
		t.Fatal("命中堆栈的高优先级切片未被保留")
	}
	if !strings.Contains(packed.Text, "已裁剪") {
		t.Fatal("超长切片应保留焦点窗口并插入裁剪标记")
	}
	// 焦点行 = 160（绝对行号），窗口为前后各 40 行，即 120-200。
	start, end := fileRangeOf(t, packed.Text, "service-29/")
	if start != 120 || end != 200 {
		t.Fatalf("焦点窗口行号区间错误: %d-%d（期望 120-200）", start, end)
	}
	if !strings.Contains(packed.Text, "return order.toDTO();") {
		t.Fatal("焦点行必须包含在窗口内")
	}
	if strings.Contains(packed.Text, "service-20/") {
		t.Fatal("低优先级切片应在预算裁剪中被丢弃")
	}

	// 堆栈超长日志：保留头部与首个 Caused by。
	if !strings.Contains(packed.Text, "java.lang.NullPointerException") {
		t.Fatal("堆栈头部必须保留")
	}
	if !strings.Contains(packed.Text, "Caused by") {
		t.Fatal("首个 Caused by 必须保留")
	}
	// 文件块头部格式固定，便于模型引用真实路径。
	if !strings.Contains(packed.Text, "--- 文件: service-29/src/main/java/com/acme/service29/Handler29.java @ commit") {
		t.Fatalf("文件块头部格式不符合约定: %s", firstFileHeader(packed.Text))
	}
}

// firstFileHeader 返回第一个文件块头部（用于失败信息）。
func firstFileHeader(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "--- 文件:") {
			return line
		}
	}
	return "(无文件块)"
}

// fileRangeOf 解析指定仓库前缀的文件块行号区间。
func fileRangeOf(t *testing.T, text, repoPrefix string) (int, int) {
	t.Helper()
	re := regexp.MustCompile(`^--- 文件: (\S+) @ (\S+) \(行 (\d+)-(\d+)\) ---$`)
	for _, line := range strings.Split(text, "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !strings.HasPrefix(m[1], repoPrefix) {
			continue
		}
		start, _ := strconv.Atoi(m[3])
		end, _ := strconv.Atoi(m[4])
		return start, end
	}
	t.Fatalf("未找到 %s 的文件块头部:\n%s", repoPrefix, firstFileHeader(text))
	return 0, 0
}

func TestContextPackerTotalBudget(t *testing.T) {
	packer := NewContextPacker(config.ContextBudgetConfig{}, logx.Nop())
	bundle := buildLargeBundle()
	budget := domain.ContextBudget{MaxChars: 3000, MaxSlices: 24, MaxSliceChars: 3000, MaxLogChars: 200000, ReserveForOutput: 1000}

	packed, err := packer.Pack(bundle, budget)
	if err != nil {
		t.Fatalf("Pack 失败: %v", err)
	}
	limit := budget.MaxChars - budget.ReserveForOutput
	if len(packed.Text) > limit {
		t.Fatalf("总预算兜底失效: %d > %d", len(packed.Text), limit)
	}
	if !packed.Truncated {
		t.Fatal("总预算裁剪必须置 Truncated=true")
	}
	anyDropped := false
	for _, s := range packed.Sections {
		if s.Dropped {
			anyDropped = true
		}
	}
	if !anyDropped {
		t.Fatalf("总预算不足时应丢弃章节: %#v", packed.Sections)
	}
	if !strings.Contains(strings.Join(packed.Notes, "\n"), "超出预算") {
		t.Fatalf("总预算裁剪需写入 Notes: %v", packed.Notes)
	}
}

func TestContextPackerNilBundle(t *testing.T) {
	packer := NewContextPacker(config.ContextBudgetConfig{}, logx.Nop())
	packed, err := packer.Pack(nil, domain.ContextBudget{})
	if err != nil {
		t.Fatalf("空证据包应降级而非报错: %v", err)
	}
	if packed.Text == "" || len(packed.Notes) == 0 {
		t.Fatalf("空证据包应给出可读的降级上下文: %#v", packed)
	}
}

func TestEstimateTokensAndFitToTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("空文本应为 0 token，实际 %d", got)
	}
	en := EstimateTokens("abcdefgh") // 8/4 = 2
	if en != 2 {
		t.Fatalf("英文估算错误: %d", en)
	}
	cn := EstimateTokens("中文中文") // 4/1.6 = 2.5 -> 3
	if cn != 3 {
		t.Fatalf("中文估算错误: %d", cn)
	}
	mixed := EstimateTokens("修复 null pointer 异常") // 中文 6 + 其余 13
	if mixed <= cn {
		t.Fatalf("混合文本估算应大于纯中文: %d", mixed)
	}

	long := strings.Repeat("a", 200) // 200/4 = 50 token
	for _, maxTokens := range []int{5, 10, 20, 40} {
		out, truncated := FitToTokens(long, maxTokens)
		if !truncated {
			t.Fatalf("maxTokens=%d 时应当裁剪", maxTokens)
		}
		if got := EstimateTokens(out); got > maxTokens {
			t.Fatalf("裁剪后仍超预算: maxTokens=%d got=%d", maxTokens, got)
		}
	}
	if _, truncated := FitToTokens(long, 50); truncated {
		t.Fatal("恰好等于预算时不应裁剪")
	}
	if out, truncated := FitToTokens("短文本", 100); truncated {
		t.Fatalf("预算充足时不应裁剪: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Gateway + httptest
// ---------------------------------------------------------------------------

// structuredRequest 构造带 Schema 的结构化请求。
func structuredRequest(stage Stage, schema map[string]any, contextText string) domain.ModelRequest {
	msgs, _ := BuildMessages(stage, map[string]any{"context": contextText, "question": "定位根因"})
	return domain.ModelRequest{Stage: string(stage), Tier: domain.TierStrong, Messages: msgs, Schema: schema}
}

// TestGatewayStructuredOK ①正常 JSON 返回 → Structured 非空且 SchemaValid 语义正确。
func TestGatewayStructuredOK(t *testing.T) {
	fake, pc := newFakeOpenAI(t, "p1", "model-1", 0, validRootCauseJSON)
	rec := &fakeRecorder{}
	g := newTestGateway(t, testMCPConfig(pc), rec)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈..."))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp.Structured == nil {
		t.Fatal("Structured 不应为空")
	}
	if resp.Structured["category"] != "null_pointer" {
		t.Fatalf("结构化结果异常: %#v", resp.Structured)
	}
	if resp.Repaired {
		t.Fatal("首次即成功不应标记 Repaired")
	}
	if resp.Attempts != 1 {
		t.Fatalf("Attempts 应为 1，实际 %d", resp.Attempts)
	}
	if resp.Usage.TotalTokens != 18 {
		t.Fatalf("Usage 应来自提供方响应: %#v", resp.Usage)
	}
	if got := fake.hits.Load(); got != 1 {
		t.Fatalf("服务端应被调用 1 次，实际 %d", got)
	}
	// JSONMode 下应携带 response_format。
	body := fake.requestBody()
	if _, ok := body["response_format"]; !ok {
		t.Fatalf("结构化请求应携带 response_format: %#v", body)
	}
	if body["stream"] != false {
		t.Fatalf("stream 必须为 false: %#v", body["stream"])
	}

	// ModelCall 记录必须体现 SchemaValid。
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应写入 1 条模型调用记录，实际 %d", len(calls))
	}
	if !calls[0].SchemaValid || calls[0].Status != domain.CallOK {
		t.Fatalf("模型调用记录异常: %#v", calls[0])
	}
	if calls[0].RunID != "run-1" || calls[0].TenantID != "t-1" || calls[0].Stage != "root_cause" {
		t.Fatalf("记录缺少链路字段: %#v", calls[0])
	}
}

// TestGatewayRepairRetry ②第一次非法 JSON、第二次合法 → Repaired=true。
func TestGatewayRepairRetry(t *testing.T) {
	fake, pc := newFakeOpenAI(t, "p1", "model-1", 0, "抱歉，这不是 JSON：无法解析", validRootCauseJSON)
	g := newTestGateway(t, testMCPConfig(pc), nil)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈..."))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if !resp.Repaired {
		t.Fatal("纠错重试成功后必须置 Repaired=true")
	}
	if resp.Structured == nil {
		t.Fatal("纠错后应得到结构化结果")
	}
	if resp.Attempts != 2 {
		t.Fatalf("应尝试 2 次（原始 + 纠错），实际 %d", resp.Attempts)
	}
	if got := fake.hits.Load(); got != 2 {
		t.Fatalf("服务端应被调用 2 次，实际 %d", got)
	}
	if !strings.Contains(fmt.Sprint(fake.requestBody()["messages"]), "只输出合法 JSON") {
		t.Fatal("纠错重试必须回灌'请只输出合法 JSON'的指令")
	}
}

// TestGatewayFallback ③第一个 provider 持续 500 → 切换第二个且 FallbackFrom 被设置。
func TestGatewayFallback(t *testing.T) {
	bad, badCfg := newFakeOpenAI(t, "p1", "model-bad", http.StatusInternalServerError)
	good, goodCfg := newFakeOpenAI(t, "p2", "model-good", 0, validRootCauseJSON)
	rec := &fakeRecorder{}
	g := newTestGateway(t, testMCPConfig(badCfg, goodCfg), rec)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈..."))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp.Structured == nil {
		t.Fatal("兜底后应拿到结构化结果")
	}
	if resp.FallbackFrom == "" || !strings.Contains(resp.FallbackFrom, "p1") {
		t.Fatalf("FallbackFrom 应指向首选提供方，实际 %q", resp.FallbackFrom)
	}
	if resp.Target.Provider != "p2" {
		t.Fatalf("最终应由 p2 完成调用，实际 %q", resp.Target.Provider)
	}
	if bad.hits.Load() == 0 || good.hits.Load() == 0 {
		t.Fatalf("两个提供方都应被调用: bad=%d good=%d", bad.hits.Load(), good.hits.Load())
	}

	// 统计与记录一致性。
	stats := g.Stats(context.Background())
	totalHits := bad.hits.Load() + good.hits.Load()
	if stats.TotalCalls != totalHits {
		t.Fatalf("Stats.TotalCalls=%d 与实际调用 %d 不一致", stats.TotalCalls, totalHits)
	}
	if stats.FailedCalls != bad.hits.Load() {
		t.Fatalf("Stats.FailedCalls=%d 与实际失败 %d 不一致", stats.FailedCalls, bad.hits.Load())
	}
	if stats.FallbackCalls != 1 {
		t.Fatalf("Stats.FallbackCalls 应为 1，实际 %d", stats.FallbackCalls)
	}
	if stats.PromptTokens == 0 || stats.TotalTokens == 0 {
		t.Fatalf("Token 统计缺失: %#v", stats)
	}
	var byModelSum int64
	for _, v := range stats.ByModel {
		byModelSum += v
	}
	if byModelSum != stats.TotalCalls {
		t.Fatalf("ByModel 计数与总调用不一致: %d vs %d", byModelSum, stats.TotalCalls)
	}
	if _, ok := stats.ByTier[string(domain.TierStrong)]; !ok {
		t.Fatalf("ByTier 缺少 strong 维度: %#v", stats.ByTier)
	}
	calls := rec.snapshot()
	if len(calls) != int(totalHits) {
		t.Fatalf("模型调用记录数 %d 与实际调用 %d 不一致", len(calls), totalHits)
	}
	if calls[0].Status != domain.CallFailed {
		t.Fatalf("首条记录应为失败: %#v", calls[0])
	}
}

// TestGatewayAllFailed ④全部失败 → 不 panic，返回降级响应且 error 语义清晰。
func TestGatewayAllFailed(t *testing.T) {
	_, cfg1 := newFakeOpenAI(t, "p1", "model-1", http.StatusBadGateway)
	_, cfg2 := newFakeOpenAI(t, "p2", "model-2", http.StatusInternalServerError)
	g := newTestGateway(t, testMCPConfig(cfg1, cfg2), nil)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈..."))
	if err == nil {
		t.Fatal("全部调用失败时必须返回错误")
	}
	if !strings.Contains(err.Error(), "模型调用全部失败") {
		t.Fatalf("错误语义应明确，实际: %v", err)
	}
	if resp != nil {
		if resp.Structured != nil {
			t.Fatal("失败时不应给出结构化结果")
		}
		if resp.Text != "" {
			t.Fatalf("失败时不应有文本产出: %q", resp.Text)
		}
	}

	// 无任何提供方且 Mock 关闭 → 允许构造出"空网关"（不阻断启动），
	// 由控制台 AI 设置页引导补齐配置；实际推理时返回明确的不可用错误。
	cfg := testMCPConfig()
	cfg.Mock.Enabled = false
	empty, err := NewGateway(cfg, config.EngineConfig{}, nil, logx.Nop())
	if err != nil {
		t.Fatalf("未配置模型时不应阻断启动: %v", err)
	}
	if got := len(empty.Providers()); got != 0 {
		t.Fatalf("空网关不应有提供方，实际 %d", got)
	}
	if _, err := empty.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈...")); err == nil {
		t.Fatal("空网关发起推理时必须返回明确的不可用错误")
	}
}

// TestGatewayDegradedStructuredOutput 结构化输出反复失败 → 降级响应（err 为 nil 但标记降级）。
func TestGatewayDegradedStructuredOutput(t *testing.T) {
	// 模型始终返回纯文本，永远无法满足 RootCauseSchema。
	_, pc := newFakeOpenAI(t, "p1", "model-1", 0, "我无法输出 JSON，只能给出自然语言结论。")
	g := newTestGateway(t, testMCPConfig(pc), nil)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "异常堆栈..."))
	if err != nil {
		t.Fatalf("结构化失败应降级而不是报错: %v", err)
	}
	if resp.Structured != nil {
		t.Fatal("降级响应不应包含 Structured")
	}
	if resp.Repaired {
		t.Fatal("降级响应必须保持 Repaired=false 作为降级标记")
	}
	if !strings.Contains(resp.Text, "自然语言结论") {
		t.Fatalf("降级响应应回传原始文本: %q", resp.Text)
	}
}

// TestGatewayCircuitBreaker ⑤连续失败超过阈值 → 熔断并在 Health 中暴露。
func TestGatewayCircuitBreaker(t *testing.T) {
	_, pc := newFakeOpenAI(t, "p1", "model-1", http.StatusInternalServerError)
	cfg := testMCPConfig(pc)
	cfg.Circuit = config.CircuitConfig{FailureThreshold: 2, CooldownSec: 30, HalfOpenProbes: 1}
	g := newTestGateway(t, cfg, nil)

	for i := 0; i < 2; i++ {
		if _, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "x")); err == nil {
			t.Fatalf("第 %d 次调用应失败", i+1)
		}
	}
	health := g.Health(context.Background())
	if len(health) == 0 {
		t.Fatal("Health 不应为空")
	}
	if health[0].CircuitState != circuitOpen {
		t.Fatalf("连续失败达到阈值后应处于 open，实际 %q", health[0].CircuitState)
	}
	if health[0].Healthy {
		t.Fatal("熔断后 Healthy 应为 false")
	}
	if health[0].TotalCalls != 2 {
		t.Fatalf("Health.TotalCalls 应为 2，实际 %d", health[0].TotalCalls)
	}
	if health[0].LastError == "" {
		t.Fatal("Health 应暴露最近失败原因")
	}

	// 熔断期间不再发起真实调用，直接返回"无可用模型"。
	before := health[0].TotalCalls
	_, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "x"))
	if err == nil {
		t.Fatal("熔断后应快速失败")
	}
	var unavailable *ModelUnavailableError
	if !strings.Contains(err.Error(), "没有可用的模型提供方") {
		t.Fatalf("熔断后应给出可用性错误，实际: %v", err)
	}
	if g.Health(context.Background())[0].TotalCalls != before {
		t.Fatal("熔断期间不应产生真实调用")
	}
	_ = unavailable
}

// TestGatewayTokenBudget 配额超限 → Truncated + *TokenBudgetError。
func TestGatewayTokenBudget(t *testing.T) {
	_, pc := newFakeOpenAI(t, "p1", "model-1", 0, validRootCauseJSON)
	g := newTestGateway(t, testMCPConfig(pc), nil)
	g.SetTokenBudget(1) // 任何一次调用都超出 1 token 预算

	if _, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "x")); err != nil {
		t.Fatalf("首次调用不应被拦截: %v", err)
	}
	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema, "x"))
	if err == nil {
		t.Fatal("超出 Token 预算时应返回错误")
	}
	var budgetErr *TokenBudgetError
	if !asTokenBudgetError(err, &budgetErr) {
		t.Fatalf("错误类型应为 *TokenBudgetError，实际 %T: %v", err, err)
	}
	if resp == nil || !resp.Truncated {
		t.Fatalf("超限响应应置 Truncated=true: %#v", resp)
	}
	if budgetErr.Used == 0 || budgetErr.Limit != 1 {
		t.Fatalf("TokenBudgetError 数据不正确: %#v", budgetErr)
	}
	g.ResetRunTokens("run-1")
}

// asTokenBudgetError 判断错误是否为 *TokenBudgetError（避免直接依赖 errors.As 的泛型写法）。
func asTokenBudgetError(err error, target **TokenBudgetError) bool {
	for err != nil {
		if e, ok := err.(*TokenBudgetError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestGatewayTierRouting LightStages + CostAware 走轻量模型。
func TestGatewayTierRouting(t *testing.T) {
	_, strongCfg := newFakeOpenAI(t, "strong-p", "model-strong", 0, `{"patches":[],"rationale":"无需改动","risk":"low"}`)
	strongCfg.Tier = "strong"
	_, lightCfg := newFakeOpenAI(t, "light-p", "model-light", 0, `{"category":"timeout","severity":"major","confidence":0.66}`)
	lightCfg.Tier = "light"
	g := newTestGateway(t, testMCPConfig(strongCfg, lightCfg), nil)

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageClassify, ClassifySchema, "日志"))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp.Target.Provider != "light-p" {
		t.Fatalf("classify 阶段应走轻量模型，实际 %q", resp.Target.Provider)
	}
	if resp.Target.Tier != domain.TierLight {
		t.Fatalf("Tier 应为 light，实际 %q", resp.Target.Tier)
	}
	if resp.Structured == nil {
		t.Fatal("轻量模型应返回结构化结果")
	}

	// StrongStages 强制走强力模型（即便 CostAware 打开）。
	resp2, err := g.Chat(context.Background(), callCtx(), structuredRequest(StagePatchSynthesize, PatchSchema, "代码"))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if resp2.Target.Provider != "strong-p" {
		t.Fatalf("patch_synthesize 应走强力模型，实际 %q", resp2.Target.Provider)
	}
	if resp2.Structured == nil {
		t.Fatal("强力模型应返回结构化结果")
	}
}

// TestGatewayMockInjection 无 provider 时注入内置 Mock。
func TestGatewayMockInjection(t *testing.T) {
	cfg := testMCPConfig()
	cfg.Providers = []config.ProviderConfig{{Name: "broken", Kind: "openai", APIKey: "", Models: []string{"m"}}}
	g := newTestGateway(t, cfg, nil)

	providers := g.Providers()
	if len(providers) != 1 || providers[0].Provider != "mock" {
		t.Fatalf("应注入 mock 提供方，实际 %#v", providers)
	}
	if providers[0].Model != "mock-reasoner-v1" {
		t.Fatalf("Mock 模型名应取自配置，实际 %q", providers[0].Model)
	}
	if providers[0].Tier != domain.TierStrong {
		t.Fatalf("注入的 Mock 必须是 strong 层，实际 %q", providers[0].Tier)
	}
	health := g.Health(context.Background())
	if len(health) != 1 || !health[0].Healthy || health[0].CircuitState != circuitClosed {
		t.Fatalf("内置 Mock 应始终健康: %#v", health)
	}

	resp, err := g.Chat(context.Background(), callCtx(), structuredRequest(StageRootCause, RootCauseSchema,
		"### 异常堆栈\njava.lang.NullPointerException: order is null\n\tat com.acme.OrderService.get(OrderService.java:88)\n"))
	if err != nil {
		t.Fatalf("Mock 链路应可用: %v", err)
	}
	if resp.Structured == nil {
		t.Fatal("Mock 应产出可通过校验的结构化结果")
	}
	if !strings.Contains(fmt.Sprint(resp.Structured["summary"]), "OrderService.java") {
		t.Fatalf("Mock 根因应引用真实文件: %#v", resp.Structured["summary"])
	}
}

// TestGatewayConcurrentCalls 并发调用下调度、统计与熔断必须安全。
//
// 说明：部分 Windows 环境的 race 运行时无法加载（0xc0000139），
// 因此这里用显式并发压测兜底，保证统计口径在并发下依然一致。
func TestGatewayConcurrentCalls(t *testing.T) {
	_, pc := newFakeOpenAI(t, "p1", "model-1", 0, validRootCauseJSON)
	g := newTestGateway(t, testMCPConfig(pc), nil)

	const goroutines, perGoroutine = 16, 4
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				resp, err := g.Chat(context.Background(), callCtx(),
					structuredRequest(StageRootCause, RootCauseSchema, fmt.Sprintf("并发输入 %d-%d", i, j)))
				if err != nil {
					errCh <- err
					continue
				}
				if resp.Structured == nil {
					errCh <- fmt.Errorf("并发调用未拿到结构化结果")
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发调用失败: %v", err)
	}

	stats := g.Stats(context.Background())
	if want := int64(goroutines * perGoroutine); stats.TotalCalls != want {
		t.Fatalf("并发下统计不一致: %d != %d", stats.TotalCalls, want)
	}
	if stats.FailedCalls != 0 {
		t.Fatalf("并发下不应出现失败调用: %d", stats.FailedCalls)
	}

	var hwg sync.WaitGroup
	for i := 0; i < 8; i++ {
		hwg.Add(1)
		go func() {
			defer hwg.Done()
			_ = g.Health(context.Background())
			_ = g.Providers()
			_ = g.Stats(context.Background())
		}()
	}
	hwg.Wait()
	if h := g.Health(context.Background()); len(h) != 1 || h[0].CircuitState != circuitClosed {
		t.Fatalf("并发成功后熔断器应保持 closed: %#v", h)
	}
}

// ---------------------------------------------------------------------------
// Mock provider
// ---------------------------------------------------------------------------

// mockChat 通过 Mock 提供方执行一次指定阶段的推理。
func mockChat(t *testing.T, stage Stage, vars map[string]any) map[string]any {
	t.Helper()
	msgs, schema := BuildMessages(stage, vars)
	if schema == nil {
		t.Fatalf("阶段 %s 的 Schema 不应为空", stage)
	}
	prov := NewMockProvider(config.ProviderConfig{Name: "mock", Kind: "mock", Models: []string{"mock-reasoner-v1"}}, 1, logx.Nop())
	resp, err := prov.Chat(context.Background(), ProviderRequest{
		Model:    "mock-reasoner-v1",
		Messages: msgs,
		JSONMode: true,
		Schema:   schema,
		Stage:    string(stage),
	})
	if err != nil {
		t.Fatalf("Mock 调用失败: %v", err)
	}
	doc, err := ParseStructured(resp.Text, schema)
	if err != nil {
		t.Fatalf("Mock 输出未通过 %s 的解析与校验: %v\n输出: %s", stage, err, resp.Text)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("Mock 应给出字符估算的 Usage")
	}
	return doc
}

func TestMockProviderRootCause(t *testing.T) {
	javaStack := "Exception in thread \"http-nio-8080-exec-3\" java.lang.NullPointerException: Cannot invoke \"com.acme.order.OrderDTO.getId()\" because \"order\" is null\n" +
		"\tat com.acme.order.OrderService.getOrder(OrderService.java:104)\n" +
		"\tat com.acme.order.OrderController.detail(OrderController.java:57)\n"
	doc := mockChat(t, StageRootCause, map[string]any{"context": javaStack, "question": "定位根因"})

	if doc["category"] != "null_pointer" {
		t.Fatalf("category 判定错误: %v", doc["category"])
	}
	if doc["severity"] != "critical" {
		t.Fatalf("severity 判定错误: %v", doc["severity"])
	}
	conf, ok := doc["confidence"].(float64)
	if !ok || conf < 0.55 || conf > 0.85 {
		t.Fatalf("confidence 应在 0.55~0.85，实际 %#v", doc["confidence"])
	}
	summary, _ := doc["summary"].(string)
	if !strings.Contains(summary, "OrderService.java:104") || !strings.Contains(summary, "空指针") {
		t.Fatalf("summary 必须是引用真实文件行号的中文文本: %q", summary)
	}
	detail, _ := doc["detail"].(string)
	if !strings.Contains(detail, "OrderService.java:104") {
		t.Fatalf("detail 必须引用真实堆栈帧: %q", detail)
	}
	if evidence, ok := doc["evidence"].([]any); !ok || len(evidence) == 0 {
		t.Fatalf("evidence 不应为空: %#v", doc["evidence"])
	}

	// 确定性：同样输入必须得到同样输出。
	again := mockChat(t, StageRootCause, map[string]any{"context": javaStack, "question": "定位根因"})
	if fmt.Sprint(again) != fmt.Sprint(doc) {
		t.Fatal("Mock 输出必须确定性")
	}

	// 其它异常类型的判定。
	py := mockChat(t, StageRootCause, map[string]any{"context": "IndexError: list index out of range\n  File \"/srv/app/main.py\", line 42, in <module>\n"})
	if py["category"] != "index_out_of_bounds" {
		t.Fatalf("Python 越界判定错误: %v", py["category"])
	}
	js := mockChat(t, StageRootCause, map[string]any{"context": "TypeError: Cannot read properties of undefined (reading 'id')\n    at handler (/srv/web/src/api.ts:31:9)\n"})
	if js["category"] != "type_error" {
		t.Fatalf("JS 类型错误判定错误: %v", js["category"])
	}
	deps := mockChat(t, StageRootCause, map[string]any{"context": "ModuleNotFoundError: No module named 'requests'\n  File \"app.py\", line 3\n"})
	if deps["category"] != "dependency_missing" {
		t.Fatalf("依赖缺失判定错误: %v", deps["category"])
	}
}

func TestMockProviderPatchSynthesize(t *testing.T) {
	slice := domain.CodeSlice{
		RepositoryID: "repo-order",
		RepoKey:      "order-service",
		Commit:       "a1b2c3d4e5f6a7b8c9d0",
		Path:         "src/main/java/com/acme/order/OrderService.java",
		Language:     "java",
		StartLine:    100,
		EndLine:      103,
		Content: "public OrderDTO getOrder(String id) {\n" +
			"    Order order = orderRepo.findById(id);\n" +
			"    return order.toDTO();\n" +
			"}\n",
		FocusLines: []int{102},
		Reason:     "frame",
	}
	bundle := &domain.EvidenceBundle{
		RunID:    "run-patch",
		TenantID: "t-1",
		Stack: &domain.StackAnalysis{
			Language:      "java",
			ExceptionType: "java.lang.NullPointerException",
			Message:       "order is null",
			Frames:        []domain.StackFrame{{Raw: "at com.acme.order.OrderService.getOrder(OrderService.java:102)", File: "OrderService.java", Line: 102}},
			Category:      "null_pointer",
		},
		Slices: []domain.CodeSlice{slice},
	}
	packer := NewContextPacker(config.ContextBudgetConfig{}, logx.Nop())
	packed, err := packer.Pack(bundle, domain.ContextBudget{MaxChars: 20000, MaxSlices: 8, MaxSliceChars: 12000, MaxLogChars: 4000, ReserveForOutput: 2000})
	if err != nil {
		t.Fatalf("Pack 失败: %v", err)
	}
	if !strings.Contains(packed.Text, "--- 文件: order-service/src/main/java/com/acme/order/OrderService.java @ a1b2c3d4 (行 100-103) ---") {
		t.Fatalf("文件块头部不符合约定:\n%s", packed.Text)
	}

	doc := mockChat(t, StagePatchSynthesize, map[string]any{"context": packed.Text, "question": "生成补丁"})
	patches, ok := doc["patches"].([]any)
	if !ok || len(patches) == 0 {
		t.Fatalf("有代码输入时 patches 不应为空: %#v", doc["patches"])
	}
	patch, _ := patches[0].(map[string]any)
	oldSnippet, _ := patch["oldSnippet"].(string)
	newSnippet, _ := patch["newSnippet"].(string)
	if strings.TrimSpace(oldSnippet) == "" {
		t.Fatal("oldSnippet 不应为空")
	}
	if !strings.Contains(oldSnippet, "order.toDTO()") {
		t.Fatalf("oldSnippet 应包含可疑行: %q", oldSnippet)
	}
	if !strings.Contains(newSnippet, "order.toDTO()") {
		t.Fatalf("newSnippet 必须保留 oldSnippet 的关键行: %q", newSnippet)
	}
	if !strings.Contains(newSnippet, "== null") {
		t.Fatalf("空指针分类必须补充 nil 检查: %q", newSnippet)
	}
	if patch["repoKey"] != "order-service" || patch["filePath"] != "src/main/java/com/acme/order/OrderService.java" {
		t.Fatalf("补丁定位信息错误: %#v", patch)
	}
	if line, ok := patch["line"].(float64); !ok || line != 100 {
		t.Fatalf("补丁行号应为 100，实际 %#v", patch["line"])
	}
	if patch["action"] != "modify" {
		t.Fatalf("action 应为 modify: %#v", patch["action"])
	}
	if risk, _ := patch["risk"].(string); risk != "low" && risk != "medium" && risk != "high" {
		t.Fatalf("risk 非法: %#v", patch["risk"])
	}

	// 无代码输入 → 空补丁数组（而不是 null，避免 Schema 校验失败）。
	empty := mockChat(t, StagePatchSynthesize, map[string]any{"context": "只有一段自然语言描述，没有任何代码片段。"})
	emptyPatches, ok := empty["patches"].([]any)
	if !ok {
		t.Fatalf("patches 必须是数组: %#v", empty["patches"])
	}
	if len(emptyPatches) != 0 {
		t.Fatalf("无代码输入时 patches 应为空数组: %#v", emptyPatches)
	}
	if empty["rationale"] != "上下文缺少可修改代码片段" {
		t.Fatalf("无代码输入时应给出明确说明: %#v", empty["rationale"])
	}
}

func TestMockProviderClassifyLogCleanAndOthers(t *testing.T) {
	doc := mockChat(t, StageClassify, map[string]any{"context": "java.lang.NullPointerException: order is null"})
	if doc["category"] != "null_pointer" || doc["severity"] != "critical" {
		t.Fatalf("classify 结果错误: %#v", doc)
	}

	clean := mockChat(t, StageLogClean, map[string]any{"context": "INFO start\nDEBUG pool stats active=3\nERROR NullPointerException at OrderService.java:104\nINFO /healthz 200\n"})
	ratio, ok := clean["noiseRatio"].(float64)
	if !ok || ratio <= 0 || ratio > 1 {
		t.Fatalf("noiseRatio 应在 (0,1]: %#v", clean["noiseRatio"])
	}
	cleaned, _ := clean["cleaned"].(string)
	if !strings.Contains(cleaned, "NullPointerException") {
		t.Fatalf("清洗后必须保留异常行: %q", cleaned)
	}
	if strings.Contains(cleaned, "DEBUG") || strings.Contains(cleaned, "/healthz") {
		t.Fatalf("清洗后不应保留噪声行: %q", cleaned)
	}

	sum := mockChat(t, StageSummary, map[string]any{"context": "java.lang.NullPointerException: order is null at OrderService.java:104"})
	if actions, ok := sum["actions"].([]any); !ok || len(actions) == 0 {
		t.Fatalf("summary.actions 不应为空: %#v", sum["actions"])
	}
	ver := mockChat(t, StageVerifyExplain, map[string]any{"context": "补丁应用成功，编译通过"})
	if next, ok := ver["nextActions"].([]any); !ok || len(next) == 0 {
		t.Fatalf("verify_explain.nextActions 不应为空: %#v", ver["nextActions"])
	}

	// 未识别阶段：不编造内容。
	prov := NewMockProvider(config.ProviderConfig{Name: "mock", Kind: "mock"}, 1, logx.Nop())
	resp, err := prov.Chat(context.Background(), ProviderRequest{
		Messages: []domain.ModelMessage{{Role: "system", Content: "[STAGE:unknown_stage]"}, {Role: "user", Content: "hi"}},
		JSONMode: true,
	})
	if err != nil {
		t.Fatalf("未识别阶段不应报错: %v", err)
	}
	var unknown map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &unknown); err != nil {
		t.Fatalf("未识别阶段输出必须是合法 JSON: %v", err)
	}
	if unknown["note"] != "mock provider: 未识别的阶段" {
		t.Fatalf("未识别阶段应如实说明: %#v", unknown)
	}
	if h, _ := unknown["echoHash"].(string); len(h) != 12 {
		t.Fatalf("echoHash 应为 sha256 前 12 位: %#v", unknown["echoHash"])
	}
}

// ---------------------------------------------------------------------------
// Prompt 模板
// ---------------------------------------------------------------------------

func TestBuildMessagesForAllStages(t *testing.T) {
	for _, stage := range AllStages() {
		t.Run(string(stage), func(t *testing.T) {
			msgs, schema := BuildMessages(stage, map[string]any{"context": "上下文内容", "question": "问题"})
			if len(msgs) != 2 {
				t.Fatalf("应返回 system + user 两条消息，实际 %d", len(msgs))
			}
			if msgs[0].Role != "system" || msgs[1].Role != "user" {
				t.Fatalf("消息角色错误: %#v", msgs)
			}
			marker := "[STAGE:" + string(stage) + "]"
			if !strings.Contains(msgs[0].Content, marker) {
				t.Fatalf("system 模板必须包含 %s", marker)
			}
			if !strings.Contains(RenderSystem(stage), marker) {
				t.Fatalf("RenderSystem(%s) 必须包含阶段标记", stage)
			}
			if schema == nil {
				t.Fatalf("阶段 %s 的 Schema 不应为空", stage)
			}
			sys := msgs[0].Content
			for _, want := range []string{"只输出符合给定 JSON Schema 的 JSON", "不要使用 Markdown 代码块", "禁止暴力重写代码，仅做最小增量修改"} {
				if !strings.Contains(sys, want) {
					t.Fatalf("system 模板缺少约束 %q", want)
				}
			}
			if !strings.Contains(msgs[1].Content, "上下文内容") {
				t.Fatal("user 模板必须注入 context")
			}
		})
	}

	// 变量缺失保护：不 panic、不产生空模板。
	msgs, schema := BuildMessages(StageRootCause, nil)
	if schema == nil {
		t.Fatal("Schema 不应为空")
	}
	user := msgs[1].Content
	for _, want := range []string{"(上下文缺失)", "(未提供问题描述)", "(无额外约束)", "(无人工反馈)"} {
		if !strings.Contains(user, want) {
			t.Fatalf("缺少变量缺失兜底文案 %q: %s", want, user)
		}
	}

	// 人工反馈必须注入（用于重跑收敛）。
	msgs2, _ := BuildMessages(StageRootCause, map[string]any{"context": "c", "feedback": "上次补丁破坏了分页逻辑"})
	if !strings.Contains(msgs2[1].Content, "上次补丁破坏了分页逻辑") {
		t.Fatal("feedback 必须注入 user 模板")
	}
}

// ---------------------------------------------------------------------------
// Provider 构造校验
// ---------------------------------------------------------------------------

func TestNewProviderValidation(t *testing.T) {
	if _, err := NewProvider(config.ProviderConfig{Name: "x", Kind: "openai", Models: []string{"m"}}, time.Second, logx.Nop()); err == nil {
		t.Fatal("非 mock 提供方缺少 apiKey 应返回错误")
	}
	if _, err := NewProvider(config.ProviderConfig{Name: "x", Kind: "mock"}, time.Second, logx.Nop()); err != nil {
		t.Fatalf("mock 提供方无需 apiKey: %v", err)
	}
	if _, err := NewProvider(config.ProviderConfig{Name: "x", Kind: "unknown_kind", APIKey: "k", Models: []string{"m"}}, time.Second, logx.Nop()); err == nil {
		t.Fatal("未知 kind 应返回错误")
	}
	azure, err := NewProvider(config.ProviderConfig{Name: "az", Kind: "azure", BaseURL: "https://x.openai.azure.com", APIKey: "k", Models: []string{"gpt-4o"}}, time.Second, logx.Nop())
	if err != nil {
		t.Fatalf("azure 构造失败: %v", err)
	}
	if !strings.Contains(fmt.Sprint(azure.Target().Endpoint), "x.openai.azure.com") {
		t.Fatalf("azure Endpoint 记录错误: %#v", azure.Target())
	}
	ollama, err := NewProvider(config.ProviderConfig{Name: "ol", Kind: "ollama", APIKey: "placeholder", Models: []string{"llama3"}}, time.Second, logx.Nop())
	if err != nil {
		t.Fatalf("ollama 构造失败: %v", err)
	}
	if ollama.Target().Model != "llama3" {
		t.Fatalf("ollama 模型名错误: %#v", ollama.Target())
	}
}

// TestProviderAzureOllamaAndHeaders 覆盖 azure / ollama / 自定义请求头三类协议差异。
func TestProviderAzureOllamaAndHeaders(t *testing.T) {
	var (
		mu                                  sync.Mutex
		path, query, apiKeyHeader, authHead string
		tenantHead                          string
		body                                map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		path, query = r.URL.Path, r.URL.RawQuery
		apiKeyHeader = r.Header.Get("api-key")
		authHead = r.Header.Get("Authorization")
		tenantHead = r.Header.Get("x-tenant")
		body = b
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/chat") {
			_, _ = w.Write([]byte(`{"model":"llama3","message":{"role":"assistant","content":"ollama-ok"},"prompt_eval_count":5,"eval_count":3,"done":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"azure-ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer srv.Close()

	// azure：api-key 头 + ?api-version= + deployments 路径。
	az, err := NewProvider(config.ProviderConfig{
		Name: "az", Kind: "azure", BaseURL: srv.URL, APIKey: "azure-key", Models: []string{"gpt-4o"},
	}, 2*time.Second, logx.Nop())
	if err != nil {
		t.Fatalf("azure 构造失败: %v", err)
	}
	resp, err := az.Chat(context.Background(), ProviderRequest{Model: "gpt-4o", Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("azure 调用失败: %v", err)
	}
	if resp.Text != "azure-ok" {
		t.Fatalf("azure 响应解析错误: %q", resp.Text)
	}
	mu.Lock()
	gotPath, gotQuery, gotKey, gotAuth := path, query, apiKeyHeader, authHead
	mu.Unlock()
	if gotPath != "/openai/deployments/gpt-4o/chat/completions" {
		t.Fatalf("azure 路径错误: %s", gotPath)
	}
	if !strings.Contains(gotQuery, "api-version=") {
		t.Fatalf("azure 必须携带 api-version: %s", gotQuery)
	}
	if gotKey != "azure-key" {
		t.Fatalf("azure 必须使用 api-key 头: %q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("azure 不应重复携带 Authorization: %q", gotAuth)
	}

	// ollama：/api/chat + options 字段 + 无鉴权头。
	ol, err := NewProviderWithHeaders(config.ProviderConfig{
		Name: "ol", Kind: "ollama", BaseURL: srv.URL, APIKey: "placeholder", Models: []string{"llama3"},
	}, 2*time.Second, logx.Nop(), map[string]string{"x-tenant": "tenant-9"})
	if err != nil {
		t.Fatalf("ollama 构造失败: %v", err)
	}
	resp2, err := ol.Chat(context.Background(), ProviderRequest{Model: "llama3", Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}}, Temperature: 0.3, MaxTokens: 128})
	if err != nil {
		t.Fatalf("ollama 调用失败: %v", err)
	}
	if resp2.Text != "ollama-ok" {
		t.Fatalf("ollama 响应解析错误: %q", resp2.Text)
	}
	if resp2.Usage.TotalTokens != 8 {
		t.Fatalf("ollama usage 应为 prompt_eval_count + eval_count: %#v", resp2.Usage)
	}
	mu.Lock()
	gotPath, gotAuth, gotTenant, gotBody := path, authHead, tenantHead, body
	mu.Unlock()
	if gotPath != "/api/chat" {
		t.Fatalf("ollama 路径错误: %s", gotPath)
	}
	if gotAuth != "" {
		t.Fatal("ollama 不应发送 Authorization 头")
	}
	if gotTenant != "tenant-9" {
		t.Fatalf("自定义请求头未生效: %q", gotTenant)
	}
	if _, ok := gotBody["options"]; !ok {
		t.Fatalf("ollama 请求体应含 options: %#v", gotBody)
	}
	if gotBody["stream"] != false {
		t.Fatalf("ollama stream 必须为 false: %#v", gotBody["stream"])
	}
}

// TestProviderAPIKeyNotLeaked 非 2xx 错误信息必须截断且不含 API Key。
func TestProviderAPIKeyNotLeaked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("SECRET-BODY ", 100)))
	}))
	defer srv.Close()

	const key = "sk-super-secret-key"
	prov, err := NewProvider(config.ProviderConfig{
		Name: "p", Kind: "openai_compatible", BaseURL: srv.URL, APIKey: key, Models: []string{"m"},
	}, 2*time.Second, logx.Nop())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	_, err = prov.Chat(context.Background(), ProviderRequest{Model: "m", Messages: []domain.ModelMessage{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("错误信息泄露 API Key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应包含状态码: %v", err)
	}
	if len(err.Error()) > 700 {
		t.Fatalf("响应体应被截断（≤512 字符），实际长度 %d", len(err.Error()))
	}
	if isRetryableError(err) {
		t.Fatal("401 属于参数/鉴权错误，不应重试")
	}
	if prov.Healthy() {
		t.Fatal("401 后提供方应被判定为不健康")
	}
}
