package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/skill"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// fixture 内置技能测试环境。
type fixture struct {
	reg    *skill.Registry
	runner *skill.Runner
}

// newFixture 注册 7 个内置技能并构造执行器。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := config.SkillConfig{
		Enabled:           true,
		FailureThreshold:  3,
		CooldownSec:       30,
		DefaultTimeoutSec: 30,
		DefaultRetries:    1,
	}
	reg := skill.NewRegistry(cfg, logx.Nop())
	if err := Register(reg, Deps{}); err != nil {
		t.Fatalf("注册内置技能失败: %v", err)
	}
	return &fixture{reg: reg, runner: skill.NewRunner(reg, cfg, nil, logx.Nop())}
}

// run 执行一次技能调用并要求成功。
func (f *fixture) run(t *testing.T, name string, input map[string]any) map[string]any {
	t.Helper()
	res, err := f.runner.Run(context.Background(), &domain.CallContext{TenantID: "t1", RunID: "r1", Stage: "test"},
		domain.SkillInvocation{Skill: name, Input: input})
	if err != nil {
		t.Fatalf("技能 %s 执行失败: %v", name, err)
	}
	if res.Status != domain.CallOK && res.Status != domain.CallFallback {
		t.Fatalf("技能 %s 状态异常: %s (%s)", name, res.Status, res.Error)
	}
	return res.Output
}

// javaNPEStack 真实 Java NPE 堆栈样例。
const javaNPEStack = `2024-05-01 10:12:33.221 ERROR [order-svc,9f1c2a] 1 --- [nio-8080-exec-3] c.a.o.OrderController : 更新订单失败
java.lang.NullPointerException: Cannot invoke "com.acme.order.OrderService.setStatus(String)" because "order" is null
	at com.acme.order.OrderService.updateStatus(OrderService.java:142)
	at com.acme.order.OrderController.update(OrderController.java:58)
	at org.springframework.web.servlet.DispatcherServlet.doDispatch(DispatcherServlet.java:1067)
	at java.base/java.lang.Thread.run(Thread.java:840)
Caused by: java.lang.IllegalStateException: order not found for id=1001
	at com.acme.order.OrderService.updateStatus(OrderService.java:139)
traceId=4f2a91c0d3e74b1f8a6c5d2e7b9a0c31 POST /api/order/update order-service:8080`

// orderServiceJava 含空指针风险的 Java 源码样例。
const orderServiceJava = `package com.acme.order;

public class OrderService {
    private final OrderRepository orderRepository;

    public void updateStatus(String orderId, String status) {
        Order order = orderRepository.findById(orderId);
        order.setStatus(status);
        orderRepository.save(order);
    }
}`

// candidates 前端 → 网关 → 订单服务的候选集。
func candidates() []map[string]any {
	return []map[string]any{
		{
			"repositoryId": "repo-web", "repoKey": "order-web", "name": "订单前端",
			"layer": "frontend", "score": 0.6,
			"matchedBy": []string{"order-web"},
			"hints": []map[string]any{
				{"path": "src/pages/order/detail.ts", "reason": "entry", "priority": 0.8},
			},
		},
		{
			"repositoryId": "repo-gw", "repoKey": "api-gateway", "name": "API 网关",
			"layer": "gateway", "score": 0.7,
			"matchedBy": []string{"/api/order/**"},
			"hints": []map[string]any{
				{"path": "src/main/resources/routes.yml", "reason": "endpoint", "priority": 0.7},
			},
		},
		{
			"repositoryId": "repo-order", "repoKey": "order-service", "name": "订单服务",
			"layer": "service", "score": 0.9,
			"matchedBy": []string{"com.acme.order"},
			"hints": []map[string]any{
				{"path": "src/main/java/com/acme/order/OrderService.java", "reason": "frame", "line": 142, "priority": 1.0},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// 注册与清单
// ---------------------------------------------------------------------------

func TestRegisterAllBuiltins(t *testing.T) {
	f := newFixture(t)
	names := f.reg.Names()
	if len(names) < 7 {
		t.Fatalf("内置技能注册数量不足: %v", names)
	}
	want := []string{
		domain.SkillStacktraceParse, domain.SkillCodeASTAnalyze, domain.SkillErrorLocalize,
		domain.SkillCrossRepoTrace, domain.SkillCodePatchSynthesize, domain.SkillDependencyRepair,
		domain.SkillPatchVerify,
	}
	for _, name := range want {
		found := false
		for _, got := range names {
			if got == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("技能 %s 未注册: %v", name, names)
		}
		m, err := f.reg.Get(context.Background(), name)
		if err != nil {
			t.Fatalf("Get(%s) 失败: %v", name, err)
		}
		if m.Version != "1.0.0" || !m.Builtin || m.Timeout <= 0 || m.Retries != 1 ||
			m.FailureThreshold != 3 || m.CooldownSeconds != 30 {
			t.Fatalf("技能 %s 清单字段不符合契约: %+v", name, m)
		}
		if m.InputSchema == nil {
			t.Fatalf("技能 %s 缺少 InputSchema", name)
		}
	}
	// 清单数量与 Manifests() 一致
	if len(Manifests()) != 7 {
		t.Fatalf("内置技能清单应为 7 个: %d", len(Manifests()))
	}
}

// ---------------------------------------------------------------------------
// stacktrace_parse
// ---------------------------------------------------------------------------

func TestStacktraceParseJavaNPE(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, domain.SkillStacktraceParse, map[string]any{"stacktrace": javaNPEStack, "logs": ""})

	if got := out["category"]; got != "null_pointer" {
		t.Fatalf("category 应为 null_pointer, got=%v", got)
	}
	if got := out["language"]; got != "java" {
		t.Fatalf("language 应为 java, got=%v", got)
	}
	stack, ok := out["stack"].(map[string]any)
	if !ok {
		t.Fatalf("stack 输出应为对象: %T", out["stack"])
	}
	frames, _ := stack["frames"].([]any)
	if len(frames) == 0 {
		t.Fatalf("frames 不应为空: %+v", stack)
	}
	hints, _ := stack["fileHints"].([]any)
	if len(hints) == 0 {
		t.Fatalf("fileHints 不应为空: %+v", stack)
	}
	if stack["exceptionType"] != "java.lang.NullPointerException" {
		t.Fatalf("exceptionType 解析错误: %v", stack["exceptionType"])
	}
	first, _ := frames[0].(map[string]any)
	if first["class"] != "OrderService" || first["method"] != "updateStatus" || first["file"] != "OrderService.java" {
		t.Fatalf("首帧解析错误: %+v", first)
	}
	if line, _ := first["line"].(float64); int(line) != 142 {
		t.Fatalf("首帧行号解析错误: %+v", first)
	}
	if conf, _ := out["confidence"].(float64); conf <= 0.5 {
		t.Fatalf("置信度过低: %v", out["confidence"])
	}
	if tid, _ := stack["traceId"].(string); tid == "" {
		t.Fatalf("traceId 未提取: %+v", stack)
	}
	endpoints, _ := stack["endpoints"].([]any)
	if len(endpoints) == 0 {
		t.Fatalf("端点为提取: %+v", stack)
	}
	assertMarshalable(t, domain.SkillStacktraceParse, out)
}

func TestStacktraceParseCategories(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		text string
		want string
	}{
		{"java-npe", javaNPEStack, "null_pointer"},
		{"java-oob", "java.lang.ArrayIndexOutOfBoundsException: Index 5 out of bounds for length 3\n\tat com.acme.List.get(List.java:20)", "index_out_of_bounds"},
		{"java-class", "java.lang.NoClassDefFoundError: com/alibaba/fastjson/JSON\n\tat com.acme.Json.parse(Json.java:11)", "class_not_found"},
		{"go-nil", "panic: runtime error: invalid memory address or nil pointer dereference\n[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x4a1b2c]\n\ngoroutine 1 [running]:\nmain.processOrder(0xc0000b4000)\n\t/app/internal/order/service.go:42 +0x1a5\nmain.main()\n\t/app/main.go:12 +0x30", "null_pointer"},
		{"go-refused", "panic: dial tcp 10.0.0.8:8080: connect: connection refused\n\ngoroutine 1 [running]:\nmain.call(0x1)\n\t/app/main.go:31 +0x99", "connection_refused"},
		{"py-index", "Traceback (most recent call last):\n  File \"/app/service/order.py\", line 42, in handler\n    order.set_status(status)\nIndexError: list index out of range", "index_out_of_bounds"},
		{"py-module", "Traceback (most recent call last):\n  File \"/app/main.py\", line 3, in <module>\n    import requests\nModuleNotFoundError: No module named 'requests'", "dependency_missing"},
		{"node-undefined", "TypeError: Cannot read properties of undefined (reading 'items')\n    at OrderService.update (/app/src/services/order.ts:23:5)\n    at /app/src/index.js:10:15", "type_error"},
		{"node-timeout", "Error: connect ETIMEDOUT 10.0.0.9:443\n    at TCPConnectWrap.afterConnect [as oncomplete] (net.js:1141:16)", "timeout"},
		{"java-oom", "java.lang.OutOfMemoryError: Java heap space\n\tat com.acme.Big.load(Big.java:1)", "oom"},
		{"php-error", "PHP Fatal error:  Uncaught Error: Call to a member function setStatus() on null in /var/www/src/Order.php:42\nStack trace:\n#0 /var/www/src/Order.php(42): OrderService->updateStatus('1')\n#1 {main}", "null_pointer"},
		{"java-syntax", "java.lang.IllegalStateException: Compilation failed: invalid syntax at line 3", "syntax_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := f.run(t, domain.SkillStacktraceParse, map[string]any{"stacktrace": tc.text})
			if out["category"] != tc.want {
				t.Fatalf("category 应为 %s, got=%v", tc.want, out["category"])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// code_ast_analyze
// ---------------------------------------------------------------------------

func TestCodeASTAnalyze(t *testing.T) {
	f := newFixture(t)
	slices := []map[string]any{
		{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "src/main/java/com/acme/order/OrderService.java", "language": "java",
			"content": orderServiceJava, "startLine": 1, "endLine": 10,
		},
		{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "internal/order/service.go", "language": "go",
			"content":   "package order\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\n// TODO: 补充重试\nfunc Load(path string) error {\n\tf, err := os.Open(path)\n\tif err != nil {\n\t\treturn err\n\t}\n\tfmt.Println(\"loaded\", path)\n\tdata := f.Name()\n\t_ = data[0]\n\treturn nil\n}",
			"startLine": 1, "endLine": 17,
		},
	}
	out := f.run(t, domain.SkillCodeASTAnalyze, map[string]any{"slices": slices, "focusLines": []any{7}})

	symbols, _ := out["symbols"].([]any)
	if len(symbols) == 0 {
		t.Fatalf("symbols 不应为空: %+v", out)
	}
	imports, _ := out["imports"].([]any)
	if len(imports) == 0 {
		t.Fatalf("imports 不应为空: %+v", out)
	}
	risks, _ := out["risks"].([]any)
	if len(risks) == 0 {
		t.Fatalf("risks 不应为空: %+v", out)
	}
	complexity, _ := out["complexity"].(map[string]any)
	if complexity == nil {
		t.Fatalf("complexity 缺失: %+v", out)
	}
	if _, ok := complexity["maxNesting"]; !ok {
		t.Fatalf("complexity 字段不完整: %+v", complexity)
	}
	// 至少识别出 nil_deref（Java 查询后解引用）与 debug_residue（fmt.Println）
	types := map[string]bool{}
	for _, r := range risks {
		m, _ := r.(map[string]any)
		types[stringValue(m["type"])] = true
	}
	if !types["nil_deref"] {
		t.Fatalf("未识别空指针风险: %+v", risks)
	}
	if !types["debug_residue"] {
		t.Fatalf("未识别调试残留: %+v", risks)
	}
	assertMarshalable(t, domain.SkillCodeASTAnalyze, out)
}

// ---------------------------------------------------------------------------
// error_localize
// ---------------------------------------------------------------------------

func TestErrorLocalize(t *testing.T) {
	f := newFixture(t)
	stackOut := f.run(t, domain.SkillStacktraceParse, map[string]any{"stacktrace": javaNPEStack})
	out := f.run(t, domain.SkillErrorLocalize, map[string]any{
		"stack":      stackOut["stack"],
		"candidates": candidates(),
	})

	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("hits 不应为空: %+v", out)
	}
	first, _ := hits[0].(map[string]any)
	if first["repoKey"] != "order-service" {
		t.Fatalf("首个命中仓库应为 order-service: %+v", first)
	}
	if path, _ := first["path"].(string); !strings.Contains(path, "OrderService.java") {
		t.Fatalf("命中路径错误: %+v", first)
	}
	reason, _ := first["reason"].(string)
	if len(reason) < 6 {
		t.Fatalf("命中原因不可读: %+v", first)
	}
	if score, _ := first["score"].(float64); score <= 0 {
		t.Fatalf("命中评分异常: %+v", first)
	}
	if conf, _ := out["confidence"].(float64); conf < 0.4 {
		t.Fatalf("有命中时置信度不应低于 0.4: %v", out["confidence"])
	}
	assertMarshalable(t, domain.SkillErrorLocalize, out)

	// 无候选仓库时应降级且置信度 < 0.4
	empty := f.run(t, domain.SkillErrorLocalize, map[string]any{
		"stack": stackOut["stack"], "candidates": []any{},
	})
	if conf, _ := empty["confidence"].(float64); conf >= 0.4 {
		t.Fatalf("无命中时置信度应小于 0.4: %v", empty["confidence"])
	}
}

// ---------------------------------------------------------------------------
// cross_repo_trace
// ---------------------------------------------------------------------------

func TestCrossRepoTrace(t *testing.T) {
	f := newFixture(t)
	stackOut := f.run(t, domain.SkillStacktraceParse, map[string]any{"stacktrace": javaNPEStack})
	slices := []map[string]any{
		{
			"repositoryId": "repo-web", "repoKey": "order-web", "path": "src/api/order.ts", "language": "typescript",
			"content":   "export async function updateOrder(id: string) {\n  const res = await axios.post('http://order-service:8080/api/order/update', { id });\n  return res.data;\n}",
			"startLine": 1, "endLine": 4,
		},
	}
	out := f.run(t, domain.SkillCrossRepoTrace, map[string]any{
		"stack": stackOut["stack"], "candidates": candidates(), "slices": slices,
	})

	chain, _ := out["chain"].([]any)
	if len(chain) == 0 {
		t.Fatalf("chain 不应为空: %+v", out)
	}
	edges, _ := out["edges"].([]any)
	if len(edges) == 0 {
		t.Fatalf("edges 不应为空: %+v", out)
	}
	// 骨架链路应体现 frontend → gateway → service
	if chain[0] != "order-web" {
		t.Fatalf("链路入口应为前端: %+v", chain)
	}
	var hasCodeEvidence bool
	for _, e := range edges {
		m, _ := e.(map[string]any)
		if ev, _ := m["evidence"].(string); strings.HasPrefix(ev, "代码证据") {
			hasCodeEvidence = true
		}
	}
	if !hasCodeEvidence {
		t.Fatalf("未从代码中建立跨仓库边: %+v", edges)
	}
	bp, _ := out["breakPoint"].(map[string]any)
	if bp == nil || bp["repoKey"] == "" {
		t.Fatalf("breakPoint 不应为空: %+v", out)
	}
	if reason, _ := bp["reason"].(string); reason == "" {
		t.Fatalf("breakPoint 缺少可解释原因: %+v", bp)
	}
	if conf, _ := out["confidence"].(float64); conf <= 0 {
		t.Fatalf("置信度异常: %v", out["confidence"])
	}
	assertMarshalable(t, domain.SkillCrossRepoTrace, out)
}

// ---------------------------------------------------------------------------
// code_patch_synthesize
// ---------------------------------------------------------------------------

func TestCodePatchSynthesizeJavaNPE(t *testing.T) {
	f := newFixture(t)
	slices := []map[string]any{
		{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "src/main/java/com/acme/order/OrderService.java", "language": "java",
			"content": orderServiceJava, "startLine": 1, "endLine": 10,
		},
	}
	out := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
		"rootCause": map[string]any{
			"summary":  "orderRepository.findById 返回 null，后续 order.setStatus 触发 NPE",
			"category": "null_pointer",
			"detail":   "java.lang.NullPointerException: Cannot invoke setStatus because order is null",
			"evidence": []any{"OrderService.java:142"},
		},
		"slices":   slices,
		"category": "null_pointer",
	})

	patches, _ := out["patches"].([]any)
	if len(patches) == 0 {
		t.Fatalf("未产出补丁: %+v", out)
	}
	for _, p := range patches {
		m, _ := p.(map[string]any)
		old, _ := m["oldSnippet"].(string)
		newSnippet, _ := m["newSnippet"].(string)
		if old == "" || newSnippet == "" {
			t.Fatalf("补丁片段为空: %+v", m)
		}
		if old == newSnippet {
			t.Fatalf("newSnippet 与 oldSnippet 相同: %+v", m)
		}
		// 核心断言：oldSnippet 必须是文件内容的精确子串（引擎层 strings.Index 定位）
		if !strings.Contains(orderServiceJava, old) {
			t.Fatalf("oldSnippet 不是原文精确子串:\n%s", old)
		}
		if m["action"] != "modify" {
			t.Fatalf("action 应为 modify: %+v", m)
		}
		if m["filePath"] != "src/main/java/com/acme/order/OrderService.java" {
			t.Fatalf("filePath 错误: %+v", m)
		}
		if _, ok := m["line"].(float64); !ok {
			t.Fatalf("line 缺失: %+v", m)
		}
		if newSnippet == "" || !strings.Contains(newSnippet, "order == null") {
			t.Fatalf("补丁未包含空指针守卫: %s", newSnippet)
		}
	}
	rationale, _ := out["rationale"].(string)
	if rationale == "" {
		t.Fatalf("rationale 不应为空: %+v", out)
	}
	if risk, _ := out["risk"].(string); risk == "" {
		t.Fatalf("risk 不应为空: %+v", out)
	}
	assertMarshalable(t, domain.SkillCodePatchSynthesize, out)
}

func TestCodePatchSynthesizeNoFabrication(t *testing.T) {
	f := newFixture(t)
	slices := []map[string]any{
		{
			"repositoryId": "repo-x", "repoKey": "svc-x", "path": "src/main/java/com/acme/Clean.java",
			"language": "java", "content": "public class Clean {\n    public int add(int a, int b) {\n        return a + b;\n    }\n}",
			"startLine": 1, "endLine": 5,
		},
	}
	out := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
		"rootCause": map[string]any{"summary": "无风险代码"},
		"slices":    slices,
		"category":  "syntax_error",
	})
	patches, _ := out["patches"].([]any)
	if len(patches) != 0 {
		t.Fatalf("无匹配规则时不应编造补丁: %+v", patches)
	}
	if risk, _ := out["risk"].(string); risk != "low" {
		t.Fatalf("无补丁时风险应为 low: %v", risk)
	}
	if rationale, _ := out["rationale"].(string); !strings.Contains(rationale, "人工介入") {
		t.Fatalf("rationale 应说明需人工介入: %v", rationale)
	}
}

func TestCodePatchSynthesizeRuleLibrary(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name     string
		category string
		path     string
		lang     string
		content  string
		wantPart string
	}{
		{
			name: "index_out_of_bounds", category: "index_out_of_bounds",
			path: "src/main/java/com/acme/List.java", lang: "java",
			content:  "public class List {\n    public String pick(String[] items, int i) {\n        return items[i];\n    }\n}",
			wantPart: "i < 0 || i >= items.length",
		},
		{
			name: "divide_by_zero", category: "divide_by_zero",
			path: "src/main/java/com/acme/Calc.java", lang: "java",
			content:  "public class Calc {\n    public int avg(int total, int count) {\n        int value = total / count;\n        return value;\n    }\n}",
			wantPart: "count == 0",
		},
		{
			name: "concurrent_modification", category: "concurrent_modification",
			path: "src/main/java/com/acme/Cache.java", lang: "java",
			content:  "public class Cache {\n    private Map<String, String> cache = new HashMap<>();\n    public Map<String, String> all() {\n        return cache;\n    }\n}",
			wantPart: "ConcurrentHashMap",
		},
		{
			name: "resource_leak", category: "resource_leak",
			path: "internal/order/load.go", lang: "go",
			content:  "package order\n\nimport \"os\"\n\nfunc Load(path string) error {\n\tf, err := os.Open(path)\n\tif err != nil {\n\t\treturn err\n\t}\n\t_ = f\n\treturn nil\n}",
			wantPart: "defer f.Close()",
		},
		{
			name: "timeout", category: "timeout",
			path: "src/api/order.ts", lang: "typescript",
			content:  "export async function load(id: string) {\n  const res = await axios.get('/api/order/' + id);\n  return res.data;\n}",
			wantPart: "timeout: 5000",
		},
		{
			name: "type_error", category: "type_error",
			path: "src/api/detail.ts", lang: "typescript",
			content:  "export function pick(response: any) {\n  const items = response.data.items;\n  return items.map((x: any) => x.id);\n}",
			wantPart: "response?.data?.items",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
				"rootCause": map[string]any{"summary": tc.name},
				"slices": []map[string]any{{
					"repositoryId": "repo-1", "repoKey": "svc-1", "path": tc.path,
					"language": tc.lang, "content": tc.content, "startLine": 1,
				}},
				"category": tc.category,
			})
			patches, _ := out["patches"].([]any)
			if len(patches) == 0 {
				t.Fatalf("规则 %s 未产出补丁: %+v", tc.name, out)
			}
			found := false
			for _, p := range patches {
				m, _ := p.(map[string]any)
				old, _ := m["oldSnippet"].(string)
				newSnippet, _ := m["newSnippet"].(string)
				if !strings.Contains(tc.content, old) {
					t.Fatalf("规则 %s 的 oldSnippet 非精确子串:\n%s", tc.name, old)
				}
				if strings.Contains(newSnippet, tc.wantPart) {
					found = true
				}
			}
			if !found {
				t.Fatalf("规则 %s 未产出预期修复片段 %q: %+v", tc.name, tc.wantPart, patches)
			}
			assertMarshalable(t, domain.SkillCodePatchSynthesize+"/"+tc.name, out)
		})
	}
}

// ---------------------------------------------------------------------------
// dependency_repair
// ---------------------------------------------------------------------------

func TestDependencyRepair(t *testing.T) {
	f := newFixture(t)
	pom := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<project>\n    <dependencies>\n        <dependency>\n            <groupId>org.springframework.boot</groupId>\n            <artifactId>spring-boot-starter-web</artifactId>\n        </dependency>\n    </dependencies>\n</project>"
	out := f.run(t, domain.SkillDependencyRepair, map[string]any{
		"dependencyFiles": []map[string]any{{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "pom.xml", "language": "xml", "content": pom, "startLine": 1, "endLine": 9,
		}},
		"errorMessage": "java.lang.NoClassDefFoundError: com/alibaba/fastjson/JSON\n\tat com.acme.order.Json.parse(Json.java:11)",
	})

	findings, _ := out["findings"].([]any)
	if len(findings) == 0 {
		t.Fatalf("findings 不应为空: %+v", out)
	}
	first, _ := findings[0].(map[string]any)
	if first["kind"] != "missing" {
		t.Fatalf("首个 finding 应为 missing: %+v", first)
	}
	if name, _ := first["name"].(string); !strings.Contains(name, "fastjson") {
		t.Fatalf("finding 名称错误: %+v", first)
	}
	actions, _ := out["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("actions 不应为空: %+v", out)
	}
	patches, _ := out["patches"].([]any)
	if len(patches) == 0 {
		t.Fatalf("应产出清单补丁: %+v", out)
	}
	for _, p := range patches {
		m, _ := p.(map[string]any)
		old, _ := m["oldSnippet"].(string)
		if !strings.Contains(pom, old) {
			t.Fatalf("依赖补丁 oldSnippet 非精确子串:\n%s", old)
		}
		if newSnippet, _ := m["newSnippet"].(string); !strings.Contains(newSnippet, "fastjson") {
			t.Fatalf("依赖补丁未包含目标依赖: %s", newSnippet)
		}
	}
	assertMarshalable(t, domain.SkillDependencyRepair, out)

	// 无清单文件时返回空补丁 + 说明
	empty := f.run(t, domain.SkillDependencyRepair, map[string]any{
		"dependencyFiles": []any{}, "errorMessage": "Cannot find module 'lodash'",
	})
	if ps, _ := empty["patches"].([]any); len(ps) != 0 {
		t.Fatalf("无清单时不应产出补丁: %+v", ps)
	}
	if fs, _ := empty["findings"].([]any); len(fs) == 0 {
		t.Fatalf("无清单时仍应有说明性 finding: %+v", empty)
	}
}

// ---------------------------------------------------------------------------
// patch_verify
// ---------------------------------------------------------------------------

func TestPatchVerify(t *testing.T) {
	f := newFixture(t)
	synth := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
		"rootCause": map[string]any{"summary": "NPE"},
		"slices": []map[string]any{{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "src/main/java/com/acme/order/OrderService.java", "language": "java",
			"content": orderServiceJava, "startLine": 1, "endLine": 10,
		}},
		"category": "null_pointer",
	})
	out := f.run(t, domain.SkillPatchVerify, map[string]any{
		"patches":  synth["patches"],
		"language": "java",
	})
	checks, _ := out["checks"].([]any)
	if len(checks) == 0 {
		t.Fatalf("checks 不应为空: %+v", out)
	}
	names := map[string]bool{}
	for _, c := range checks {
		m, _ := c.(map[string]any)
		names[stringValue(m["name"])] = true
	}
	for _, want := range []string{"snippet_integrity", "balance", "static_syntax", "danger_scan", "scale"} {
		if !names[want] {
			t.Fatalf("缺少校验项 %s: %+v", want, names)
		}
	}
	if passed, _ := out["passed"].(bool); !passed {
		t.Fatalf("自动产出的补丁应通过静态校验: %+v", out)
	}
	assertMarshalable(t, domain.SkillPatchVerify, out)

	// 危险操作：System.exit(0)
	bad := map[string]any{
		"patches": []map[string]any{{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"filePath":   "src/main/java/com/acme/order/OrderService.java",
			"oldSnippet": "        order.setStatus(status);",
			"newSnippet": "        if (order == null) {\n            System.exit(0);\n        }\n        order.setStatus(status);",
			"risk":       "high",
		}},
		"language": "java",
	}
	badOut := f.run(t, domain.SkillPatchVerify, bad)
	if passed, _ := badOut["passed"].(bool); passed {
		t.Fatalf("含 System.exit 的补丁必须判定为不通过: %+v", badOut)
	}
	dangerFailed := false
	for _, c := range badOut["checks"].([]any) {
		m, _ := c.(map[string]any)
		if m["name"] == "danger_scan" && m["passed"] == false {
			dangerFailed = true
			if out, _ := m["output"].(string); !strings.Contains(out, "System.exit") {
				t.Fatalf("danger_scan 输出应说明命中原因: %v", out)
			}
		}
	}
	if !dangerFailed {
		t.Fatalf("danger_scan 应判定失败: %+v", badOut)
	}
	if score, _ := badOut["score"].(float64); score > 0.5 {
		t.Fatalf("未通过时评分应低于 0.5: %v", score)
	}
}

// ---------------------------------------------------------------------------
// 通用健壮性
// ---------------------------------------------------------------------------

func TestAllSkillsTolerateEmptyInputAndMarshal(t *testing.T) {
	f := newFixture(t)
	inputs := map[string]map[string]any{
		domain.SkillStacktraceParse:     {"stacktrace": "java.lang.NullPointerException: x\n\tat com.acme.A.b(A.java:1)"},
		domain.SkillCodeASTAnalyze:      {},
		domain.SkillErrorLocalize:       {},
		domain.SkillCrossRepoTrace:      {},
		domain.SkillCodePatchSynthesize: {},
		domain.SkillDependencyRepair:    {},
		domain.SkillPatchVerify:         {},
	}
	for name, in := range inputs {
		out := f.run(t, name, in)
		assertMarshalable(t, name, out)
	}
}

// assertMarshalable 断言技能输出可直接 JSON 序列化，且 JSON 往返后结构稳定。
func assertMarshalable(t *testing.T, name string, out map[string]any) {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("技能 %s 输出不可 JSON 序列化: %v", name, err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("技能 %s 输出无法反序列化: %v", name, err)
	}
}

// TestOutputShapesMatchEngineParsers 锁定技能输出形态与 engine/pipeline.go 的取数逻辑一致：
// parsePatchDescs/parseChecks 要求 []any，mergeASTOutput 要求 symbols 为对象数组，
// parseChain 要求 chain 为 []any。进程内调用与 HTTP JSON 调用必须拿到同样的形态。
func TestOutputShapesMatchEngineParsers(t *testing.T) {
	f := newFixture(t)

	synth := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
		"rootCause": map[string]any{"summary": "NPE"},
		"slices": []map[string]any{{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "src/main/java/com/acme/order/OrderService.java", "language": "java",
			"content": orderServiceJava, "startLine": 1, "endLine": 10,
		}},
		"category": "null_pointer",
	})
	patches, ok := synth["patches"].([]any)
	if !ok || len(patches) == 0 {
		t.Fatalf("patches 必须是 []any（engine parsePatchDescs 依赖）: %T", synth["patches"])
	}
	for _, p := range patches {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("patches 元素必须是对象: %T", p)
		}
		for _, key := range []string{"repositoryId", "repoKey", "filePath", "action",
			"line", "oldSnippet", "newSnippet", "rationale", "risk", "confidence"} {
			if _, exists := m[key]; !exists {
				t.Fatalf("补丁缺少字段 %s: %+v", key, m)
			}
		}
	}

	verify := f.run(t, domain.SkillPatchVerify, map[string]any{
		"patches": synth["patches"], "language": "java",
	})
	checks, ok := verify["checks"].([]any)
	if !ok || len(checks) == 0 {
		t.Fatalf("checks 必须是 []any（engine parseChecks 依赖）: %T", verify["checks"])
	}
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if name, _ := m["name"].(string); name == "" {
			t.Fatalf("check 缺少 name: %+v", m)
		}
		if _, exists := m["passed"]; !exists {
			t.Fatalf("check 缺少 passed: %+v", m)
		}
	}

	ast := f.run(t, domain.SkillCodeASTAnalyze, map[string]any{
		"slices": []map[string]any{{
			"repositoryId": "repo-order", "repoKey": "order-service",
			"path": "src/main/java/com/acme/order/OrderService.java", "language": "java",
			"content": orderServiceJava, "startLine": 1, "endLine": 10,
		}},
	})
	syms, ok := ast["symbols"].([]any)
	if !ok || len(syms) == 0 {
		t.Fatalf("symbols 必须是 []any: %T", ast["symbols"])
	}
	for _, s := range syms {
		m, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("symbols 元素必须是对象（engine mergeASTOutput 依赖）: %T", s)
		}
		if name, _ := m["symbol"].(string); name == "" {
			t.Fatalf("symbols 元素缺少 symbol: %+v", m)
		}
		if p, _ := m["path"].(string); p == "" {
			t.Fatalf("symbols 元素缺少 path: %+v", m)
		}
	}
	risks, _ := ast["risks"].([]any)
	if len(risks) == 0 {
		t.Fatalf("risks 不应为空: %+v", ast)
	}
	if m, _ := risks[0].(map[string]any); stringValue(m["message"]) == "" {
		t.Fatalf("risks 元素需要 message 字段供 engine 读取: %+v", risks[0])
	}

	trace := f.run(t, domain.SkillCrossRepoTrace, map[string]any{"candidates": candidates()})
	if _, ok := trace["chain"].([]any); !ok {
		t.Fatalf("chain 必须是 []any: %T", trace["chain"])
	}
	if _, ok := trace["edges"].([]any); !ok {
		t.Fatalf("edges 必须是 []any: %T", trace["edges"])
	}

	dep := f.run(t, domain.SkillDependencyRepair, map[string]any{
		"dependencyFiles": []any{}, "errorMessage": "Cannot find module 'lodash'",
	})
	for _, key := range []string{"findings", "actions", "patches"} {
		if _, ok := dep[key].([]any); !ok {
			t.Fatalf("%s 必须是 []any: %T", key, dep[key])
		}
	}
}

// TestPatchVerifyAcceptsDomainPatchShape 校验 patch_verify 兼容 engine 传入的 domain.Patch 形态
// （patchesToAny 序列化后使用 oldContent/newContent 字段名）。
func TestPatchVerifyAcceptsDomainPatchShape(t *testing.T) {
	f := newFixture(t)
	out := f.run(t, domain.SkillPatchVerify, map[string]any{
		"patches": []map[string]any{{
			"id": "p1", "repositoryId": "repo-order", "repoKey": "order-service",
			"filePath":   "src/main/java/com/acme/order/OrderService.java",
			"action":     "modify",
			"oldContent": "        order.setStatus(status);",
			"newContent": "        if (order == null) {\n            java.util.Objects.requireNonNull(order);\n        }\n        order.setStatus(status);",
			"risk":       "medium",
		}},
		"language": "java",
	})
	if passed, _ := out["passed"].(bool); !passed {
		t.Fatalf("合法的 domain.Patch 形态应通过静态校验: %+v", out)
	}
	for _, c := range out["checks"].([]any) {
		m, _ := c.(map[string]any)
		if m["name"] == "snippet_integrity" {
			if text, _ := m["output"].(string); strings.Contains(text, "为空") {
				t.Fatalf("未识别 oldContent/newContent 字段: %v", text)
			}
		}
	}
}

// TestPatchSynthesizeUsesFeedback 校验 pipeline 注入的 feedback 能影响修复方向。
func TestPatchSynthesizeUsesFeedback(t *testing.T) {
	f := newFixture(t)
	content := "public class Calc {\n    public int avg(int total, int count) {\n        int value = total / count;\n        return value;\n    }\n}"
	out := f.run(t, domain.SkillCodePatchSynthesize, map[string]any{
		"rootCause": map[string]any{"summary": "计算异常"},
		"slices": []map[string]any{{
			"repositoryId": "repo-1", "repoKey": "svc-1", "path": "src/main/java/com/acme/Calc.java",
			"language": "java", "content": content, "startLine": 1,
		}},
		// category 缺失，仅凭反馈「除零」定位修复方向
		"feedback": "上一轮验证失败：怀疑存在除零风险，请补充校验",
	})
	patches, _ := out["patches"].([]any)
	if len(patches) == 0 {
		t.Fatalf("反馈中的「除零」未触发修复规则: %+v", out)
	}
	m, _ := patches[0].(map[string]any)
	if newSnippet, _ := m["newSnippet"].(string); !strings.Contains(newSnippet, "count == 0") {
		t.Fatalf("反馈驱动的补丁内容不正确: %v", newSnippet)
	}
	if rationale, _ := out["rationale"].(string); !strings.Contains(rationale, "人工反馈") {
		t.Fatalf("rationale 应说明已结合人工反馈: %v", rationale)
	}
}
