package source

import (
	"strings"
	"testing"

	"github.com/codeagent/backend/internal/domain"
)

// bootstrapRepos 构造与 env/bootstrap 示例同构的 8 个仓库，覆盖 gateway / frontend /
// service / middleware / library 各分层，用于验证多仓库分组匹配。
func bootstrapRepos() []*domain.Repository {
	return []*domain.Repository{
		{
			ID: "repo-gateway", TenantID: "t1", Name: "API 网关", Key: "api-gateway",
			URL: "https://git.example.com/acme/api-gateway.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerGateway, Language: "java",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PackagePrefixes:  []string{"com.acme.gateway"},
				PathPrefixes:     []string{"src/main/java/com/acme/gateway"},
				EndpointPatterns: []string{"/api/**"},
				ArtifactNames:    []string{"api-gateway.jar"},
				HostPatterns:     []string{"gateway.internal"},
				Keywords:         []string{"gateway", "zuul", "route"},
			},
		},
		{
			ID: "repo-web", TenantID: "t1", Name: "运营前端", Key: "web-frontend",
			URL: "https://git.example.com/acme/web-frontend.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerFrontend, Language: "typescript",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PathPrefixes:     []string{"src/views", "src/api"},
				EndpointPatterns: []string{"/api/order/**"},
				ArtifactNames:    []string{"web-frontend"},
				Keywords:         []string{"frontend", "vue"},
			},
		},
		{
			ID: "repo-order", TenantID: "t1", Name: "订单服务", Key: "order-service",
			URL: "https://git.example.com/acme/order-service.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "java",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PackagePrefixes:  []string{"com.acme.order"},
				PathPrefixes:     []string{"src/main/java/com/acme/order", "src/main/resources"},
				ArtifactNames:    []string{"order-service.jar"},
				HostPatterns:     []string{"order-svc", "order.internal"},
				EndpointPatterns: []string{"/api/order/**"},
				Keywords:         []string{"OrderService", "下单", "order"},
			},
		},
		{
			ID: "repo-payment", TenantID: "t1", Name: "支付服务", Key: "payment-service",
			URL: "https://git.example.com/acme/payment-service.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "java",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PackagePrefixes:  []string{"com.acme.payment"},
				PathPrefixes:     []string{"src/main/java/com/acme/payment"},
				ArtifactNames:    []string{"payment-service.jar"},
				HostPatterns:     []string{"payment.internal"},
				EndpointPatterns: []string{"/api/payment/**"},
				Keywords:         []string{"PaymentService", "支付"},
			},
		},
		{
			ID: "repo-inventory", TenantID: "t1", Name: "库存服务", Key: "inventory-service",
			URL: "https://git.example.com/acme/inventory-service.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "java",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PackagePrefixes: []string{"com.acme.inventory"},
				PathPrefixes:    []string{"src/main/java/com/acme/inventory"},
				Keywords:        []string{"InventoryService"},
			},
		},
		{
			ID: "repo-notify", TenantID: "t1", Name: "通知服务", Key: "notification-service",
			URL: "https://git.example.com/acme/notification-service.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "go",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PathPrefixes:  []string{"internal/notify", "cmd/notify"},
				ArtifactNames: []string{"notification-svc"},
				HostPatterns:  []string{"notify.internal"},
				Keywords:      []string{"notify"},
			},
		},
		{
			ID: "repo-user", TenantID: "t1", Name: "用户服务", Key: "user-service",
			URL: "https://git.example.com/acme/user-service.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerService, Language: "python",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PathPrefixes: []string{"app/user"},
				Keywords:     []string{"user_service", "user-service"},
			},
		},
		{
			ID: "repo-common", TenantID: "t1", Name: "公共中间件", Key: "common-middleware",
			URL: "https://git.example.com/acme/common-middleware.git", DefaultBranch: "main",
			Provider: domain.VCSGitLab, Layer: domain.LayerMiddleware, Language: "java",
			Status: domain.RepoActive,
			MatchRules: domain.RepoMatchRules{
				PackagePrefixes: []string{"com.acme.common"},
				PathPrefixes:    []string{"src/main/java/com/acme/common"},
				Keywords:        []string{"CommonFilter"},
			},
		},
	}
}

const crossRepoStack = `2024-05-01 10:00:00.123 ERROR [api-gateway,9f8e7d6c5b4a3210,1122334455667788] 1 --- [http-nio-8080-exec-3] c.a.g.GatewayFilter : 请求链路失败
Exception in thread "http-nio-8080-exec-3" java.lang.NullPointerException: Cannot invoke "com.acme.order.Order.getAmount()" because "order" is null
	at com.acme.order.OrderService.create(OrderService.java:88)
	at com.acme.order.OrderController.submit(OrderController.java:42)
	at org.springframework.web.servlet.DispatcherServlet.doDispatch(DispatcherServlet.java:1067)
Caused by: java.lang.IllegalStateException: order-service.jar 调用失败
	at com.acme.order.OrderRepository.save(OrderRepository.java:31)
`

func findCandidate(list []domain.RepoCandidate, repoKey string) (domain.RepoCandidate, bool) {
	for _, c := range list {
		if c.RepoKey == repoKey {
			return c, true
		}
	}
	return domain.RepoCandidate{}, false
}

func TestMatchGroupStackPrefersOrderService(t *testing.T) {
	repos := bootstrapRepos()
	stack := ParseStack(crossRepoStack, "POST /api/order/create failed, target http://order-svc:8080/api/order/1", "")

	got := Match(MatchInput{
		Stack:        stack,
		GroupRepos:   repos,
		EntryRepoIDs: []string{"repo-gateway", "repo-web"},
		EntryFiles:   []string{"src/main/java/com/acme/order/OrderController.java"},
		Logs:         "POST /api/order/create failed",
	})
	if len(got) == 0 {
		t.Fatalf("未匹配到任何候选仓库")
	}
	first := got[0]
	if first.RepoKey != "order-service" {
		t.Fatalf("第一名 = %q（%.4f），期望 order-service；完整结果=%v", first.RepoKey, first.Score, keysOf(got))
	}
	if first.Score <= 0 || first.Score > 1 {
		t.Fatalf("得分应在 (0,1]，得到 %.4f", first.Score)
	}
	// MatchedBy 中文原因需包含包名命中
	joined := strings.Join(first.MatchedBy, " | ")
	if !strings.Contains(joined, "com.acme.order") {
		t.Fatalf("MatchedBy 缺少包名命中原因: %v", first.MatchedBy)
	}
	// Hints 需含 OrderService.java
	if !hintContains(first.Hints, "OrderService.java") {
		t.Fatalf("Hints 缺少 OrderService.java: %+v", first.Hints)
	}
	if len(first.Hints) > maxHintsPerRepo {
		t.Fatalf("Hints 数量超限: %d", len(first.Hints))
	}
	// 同路径去重
	seen := map[string]bool{}
	for _, h := range first.Hints {
		if seen[h.Path] {
			t.Fatalf("Hints 存在重复路径 %q", h.Path)
		}
		seen[h.Path] = true
		if h.Reason == "" || h.Priority <= 0 {
			t.Fatalf("Hint 字段不完整: %+v", h)
		}
	}
	// 入口仓库（网关）应出现但排在 order-service 之后
	if gw, ok := findCandidate(got, "api-gateway"); ok {
		if gw.Score >= first.Score {
			t.Fatalf("入口仓库得分应低于直接命中仓库: gateway=%.4f order=%.4f", gw.Score, first.Score)
		}
	} else {
		t.Fatalf("入口仓库 api-gateway 应进入结果: %v", keysOf(got))
	}
	// 完全无关的仓库不应进入结果
	if _, ok := findCandidate(got, "inventory-service"); ok {
		t.Fatalf("无关仓库 inventory-service 不应进入结果: %v", keysOf(got))
	}
	if len(got) > maxCandidates {
		t.Fatalf("候选数超限: %d", len(got))
	}
	// 得分降序
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("结果未按得分降序: %v", scoresOf(got))
		}
	}
}

func TestMatchNoHitsReturnsEmpty(t *testing.T) {
	repos := bootstrapRepos()
	if got := Match(MatchInput{}); len(got) != 0 {
		t.Fatalf("空输入应返回空切片，得到 %d 项", len(got))
	}
	if got := Match(MatchInput{GroupRepos: repos}); len(got) != 0 {
		t.Fatalf("无任何线索时应返回空切片，得到 %v", keysOf(got))
	}
	// 返回的必须是空切片而非 nil（便于 JSON 序列化为 []）
	if got := Match(MatchInput{}); got == nil {
		t.Fatalf("应返回非 nil 空切片")
	}
}

func TestMatchClassAndServiceNormalization(t *testing.T) {
	cases := []struct {
		name  string
		repo  domain.Repository
		stack string
		key   string
	}{
		{
			name: "order-service ↔ OrderService.java",
			repo: domain.Repository{ID: "r1", Key: "order-service", Name: "订单服务", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"com.acme.order"}}},
			stack: "java.lang.RuntimeException: x\n\tat com.acme.order.OrderService.create(OrderService.java:88)",
			key:   "order-service",
		},
		{
			name: "order_service ↔ OrderService",
			repo: domain.Repository{ID: "r2", Key: "order_service", Name: "订单服务", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"com/acme/order"}}},
			stack: "java.lang.RuntimeException: x\n\tat com.acme.order.OrderService.create(OrderService.java:88)",
			key:   "order_service",
		},
		{
			name: "orderservice ↔ OrderService",
			repo: domain.Repository{ID: "r3", Key: "orderservice", Name: "订单服务", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{HostPatterns: []string{"order.internal"}}},
			stack: "java.lang.RuntimeException: x\n\tat com.acme.order.OrderService.create(OrderService.java:88)",
			key:   "orderservice",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := c.repo
			got := Match(MatchInput{Stack: ParseStack(c.stack, "", ""), GroupRepos: []*domain.Repository{&repo}})
			if len(got) != 1 {
				t.Fatalf("应命中 1 个仓库，得到 %d", len(got))
			}
			if got[0].RepoKey != c.key {
				t.Fatalf("RepoKey = %q", got[0].RepoKey)
			}
		})
	}
}

func TestMatchArtifactHostEndpointKeyword(t *testing.T) {
	cases := []struct {
		name string
		repo domain.Repository
		logs string
		want string
	}{
		{
			name: "产物名命中",
			repo: domain.Repository{ID: "r1", Key: "order-service", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{ArtifactNames: []string{"order-service.jar"}}},
			logs: "java.lang.IllegalStateException: order-service.jar 调用失败",
			want: "构建产物",
		},
		{
			name: "主机模式命中",
			repo: domain.Repository{ID: "r2", Key: "payment-service", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{HostPatterns: []string{"payment.internal"}}},
			logs: "connect to http://payment.internal:8080/api/payment/1 failed",
			want: "主机标识",
		},
		{
			name: "接口通配命中",
			repo: domain.Repository{ID: "r3", Key: "order-service", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{EndpointPatterns: []string{"/api/order/**"}}},
			logs: "POST /api/order/create 500",
			want: "接口路径命中",
		},
		{
			name: "关键词命中（叠加路径前缀达到阈值）",
			repo: domain.Repository{ID: "r4", Key: "inventory-service", Layer: domain.LayerService,
				MatchRules: domain.RepoMatchRules{
					PathPrefixes: []string{"internal/inventory"},
					Keywords:     []string{"InventoryService"},
				}},
			logs: "InventoryService stock not enough at internal/inventory/dao.go:42",
			want: "关键词",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := c.repo
			stack := ParseStack("java.lang.RuntimeException: x\n\tat com.other.Cls.run(Cls.java:1)", c.logs, "")
			got := Match(MatchInput{Stack: stack, GroupRepos: []*domain.Repository{&repo}, Logs: c.logs})
			if len(got) != 1 {
				t.Fatalf("应命中 1 个仓库，得到 %d", len(got))
			}
			joined := strings.Join(got[0].MatchedBy, " | ")
			if !strings.Contains(joined, c.want) {
				t.Fatalf("MatchedBy = %v，期望包含 %q", got[0].MatchedBy, c.want)
			}
		})
	}
}

func TestMatchEntryFilesAndLayerOrdering(t *testing.T) {
	repos := []*domain.Repository{
		{ID: "g", Key: "api-gateway", Layer: domain.LayerGateway, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"gateway/src"}}},
		{ID: "f", Key: "web-frontend", Layer: domain.LayerFrontend, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"web/src"}}},
		{ID: "s", Key: "order-service", Layer: domain.LayerService, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"order/src"}}},
		{ID: "m", Key: "common-middleware", Layer: domain.LayerMiddleware, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"middleware/src"}}},
	}
	base := MatchInput{
		Stack:      ParseStack("java.lang.RuntimeException: x", "", ""),
		GroupRepos: repos,
		Logs:       "paths gateway/src/A.java web/src/B.ts order/src/C.java middleware/src/D.java",
	}
	got := Match(base)
	if len(got) != 4 {
		t.Fatalf("应命中 4 个仓库，得到 %v", keysOf(got))
	}
	// 四者均只命中路径前缀，差异仅来自 Layer 加成：gateway 0.08 > frontend 0.06 > service 0.04 > middleware 0.02
	wantOrder := []string{"api-gateway", "web-frontend", "order-service", "common-middleware"}
	for i, want := range wantOrder {
		if got[i].RepoKey != want {
			t.Fatalf("Layer 加成排序不符: %v，期望 %v", keysOf(got), wantOrder)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Score <= got[i].Score {
			t.Fatalf("Layer 加成应体现为得分递减: %v", scoresOf(got))
		}
	}
	// 同分时按 Layer 顺序与仓库 Key 稳定排序
	sameScore := []*domain.Repository{
		{ID: "z", Key: "zzz-service", Layer: domain.LayerService, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"same"}}},
		{ID: "a", Key: "aaa-service", Layer: domain.LayerService, MatchRules: domain.RepoMatchRules{PathPrefixes: []string{"same"}}},
	}
	sameGot := Match(MatchInput{
		Stack:      base.Stack,
		GroupRepos: sameScore,
		Logs:       "file same/Main.java",
	})
	if len(sameGot) != 2 {
		t.Fatalf("同分配置应命中 2 个仓库，得到 %v", keysOf(sameGot))
	}
	if sameGot[0].RepoKey != "aaa-service" || sameGot[1].RepoKey != "zzz-service" {
		t.Fatalf("同分排序应按仓库 Key 稳定: %v", keysOf(sameGot))
	}
	// EntryFiles 命中 PathPrefixes 时应额外加权，使 order-service 反超网关
	got2 := Match(MatchInput{
		Stack:      base.Stack,
		GroupRepos: repos,
		EntryFiles: []string{"order/src/main/java/OrderController.java"},
	})
	if len(got2) == 0 || got2[0].RepoKey != "order-service" {
		t.Fatalf("EntryFiles 命中应让 order-service 排第一，得到 %v", keysOf(got2))
	}
	if !strings.Contains(strings.Join(got2[0].MatchedBy, " | "), "嫌疑文件") {
		t.Fatalf("MatchedBy 缺少入口文件原因: %v", got2[0].MatchedBy)
	}
	// Hints 应包含入口文件本身（reason=entry）
	if !hintContains(got2[0].Hints, "order/src/main/java/OrderController.java") {
		t.Fatalf("Hints 缺少入口文件: %+v", got2[0].Hints)
	}
	// 确定性：重复调用结果一致
	for i := 0; i < 5; i++ {
		again := Match(base)
		if len(again) != len(got) {
			t.Fatalf("结果不稳定")
		}
		for j := range again {
			if again[j].RepoKey != got[j].RepoKey || again[j].Score != got[j].Score {
				t.Fatalf("第 %d 次调用结果不稳定: %v vs %v", i, keysOf(again), keysOf(got))
			}
		}
	}
}

func TestMatchThresholdAndCap(t *testing.T) {
	// 得分低于阈值的仓库不进入结果
	low := &domain.Repository{ID: "low", Key: "x", Layer: domain.LayerUnknown,
		MatchRules: domain.RepoMatchRules{Keywords: []string{"nonexistent-keyword"}}}
	if got := Match(MatchInput{Stack: ParseStack("java.lang.RuntimeException: x", "", ""), GroupRepos: []*domain.Repository{low}}); len(got) != 0 {
		t.Fatalf("低分仓库不应进入结果: %v", scoresOf(got))
	}
	// 结果最多 8 个
	many := make([]*domain.Repository, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, &domain.Repository{
			ID:    "r" + itoa(i),
			Key:   "svc-" + itoa(i),
			Layer: domain.LayerService,
			MatchRules: domain.RepoMatchRules{
				PathPrefixes: []string{"src/" + itoa(i)},
			},
		})
	}
	var logs []string
	for i := 0; i < 12; i++ {
		logs = append(logs, "src/"+itoa(i)+"/Main.java")
	}
	got := Match(MatchInput{
		Stack:      ParseStack("java.lang.RuntimeException: x", "", ""),
		GroupRepos: many,
		Logs:       strings.Join(logs, " "),
	})
	if len(got) > maxCandidates {
		t.Fatalf("候选数 %d 超过上限 %d", len(got), maxCandidates)
	}
}

func TestMatchNilStackDoesNotPanic(t *testing.T) {
	repos := bootstrapRepos()
	if got := Match(MatchInput{GroupRepos: repos, Stack: nil}); len(got) != 0 {
		t.Fatalf("Stack 为 nil 且有日志无命中时应返回空: %v", keysOf(got))
	}
	repos = append(repos, nil)
	stack := ParseStack(crossRepoStack, "", "")
	got := Match(MatchInput{GroupRepos: repos, Stack: stack})
	if len(got) == 0 {
		t.Fatalf("含 nil 仓库时仍应正常匹配")
	}
}

func TestGlobSimpleMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"/api/order/**", "/api/order/create", true},
		{"/api/order/**", "/api/order/1/items", true},
		{"/api/order/*", "/api/order/create", true},
		{"/api/order/*", "/api/order/1/items", false},
		{"/api/**/detail", "/api/order/1/detail", true},
		{"/api/*/detail", "/api/order/detail", true},
		{"/api/order", "/api/order", true},
		{"/api/order", "/api/orders", false},
		{"*.min.js", "app.min.js", true},
		{"*.min.js", "app.js", false},
	}
	for _, c := range cases {
		if got := globSimpleMatch(c.pattern, c.s); got != c.want {
			t.Fatalf("globSimpleMatch(%q, %q) = %v，期望 %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestNormKey(t *testing.T) {
	cases := map[string]string{
		"order-service":         "orderservice",
		"OrderService.java":     "orderservice",
		"order_service":         "orderservice",
		"orderservice":          "orderservice",
		"com/acme/OrderService": "orderservice",
		"OrderService":          "orderservice",
		"order.service.ts":      "orderservice",
	}
	for in, want := range cases {
		if got := normKey(in); got != want {
			t.Fatalf("normKey(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func hintContains(hints []domain.FileHint, path string) bool {
	for _, h := range hints {
		if h.Path == path || strings.HasSuffix(h.Path, "/"+path) {
			return true
		}
	}
	return false
}

func keysOf(list []domain.RepoCandidate) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.RepoKey)
	}
	return out
}

func scoresOf(list []domain.RepoCandidate) []float64 {
	out := make([]float64, 0, len(list))
	for _, c := range list {
		out = append(out, c.Score)
	}
	return out
}
