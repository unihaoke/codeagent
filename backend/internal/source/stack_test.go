package source

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/codeagent/backend/internal/domain"
)

const javaStack = `2024-05-01 10:00:00.123 ERROR [order-service,9f8e7d6c5b4a3210,9f8e7d6c5b4a3210] 1 --- [http-nio-8080-exec-3] c.a.o.OrderController : 下单失败
Exception in thread "http-nio-8080-exec-3" java.lang.NullPointerException: Cannot invoke "com.acme.order.Order.getAmount()" because "order" is null
	at com.acme.order.OrderService.create(OrderService.java:88)
	at com.acme.order.OrderController.submit(OrderController.java:42)
	at org.springframework.web.servlet.DispatcherServlet.doDispatch(DispatcherServlet.java:1067)
	at java.base/java.lang.Thread.run(Thread.java:840)
Caused by: java.lang.IllegalStateException: repository not initialized
	at com.acme.order.OrderRepository.save(OrderRepository.java:31)
	... 23 more
Suppressed: java.lang.IllegalArgumentException: bad amount
	at com.acme.order.OrderService.validate(OrderService.java:120)
`

const goStack = `panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x4b2c1a]

goroutine 1 [running]:
main.(*OrderService).Create(0xc0000b4000, {0x5a3f20, 0xc00010e000}, {0x4d2b8f, 0x3})
	/app/internal/order/service.go:42 +0x1a
main.main()
	/app/cmd/api/main.go:18 +0xb2
`

const pythonStack = `Traceback (most recent call last):
  File "/app/order/api.py", line 22, in submit
    result = service.create(order_id)
  File "/app/order/service.py", line 88, in create
    total = amount / count
ZeroDivisionError: division by zero
`

const nodeStack = `TypeError: Cannot read properties of undefined (reading 'id')
    at OrderService.create (/app/src/order.service.ts:88:12)
    at async OrderController.submit (/app/src/order.controller.ts:42:20)
    at processTicksAndRejections (node:internal/process/task_queues:95:5)
`

const phpStack = `PHP Fatal error:  Uncaught Error: Call to a member function save() on null in /app/src/OrderService.php:88
Stack trace:
#0 /app/src/OrderController.php(42): App\Service\OrderService->create()
#1 /app/src/OrderService.php(88): App\Service\OrderService->save()
#2 /app/public/index.php(12): App\Controller\OrderController->submit()
#3 {main}
  thrown in /app/src/OrderService.php on line 88
`

func firstFrame(t *testing.T, sa *domain.StackAnalysis) domain.StackFrame {
	t.Helper()
	if sa == nil {
		t.Fatalf("分析结果为空")
	}
	if len(sa.Frames) == 0 {
		t.Fatalf("未解析出任何堆栈帧（language=%s）", sa.Language)
	}
	return sa.Frames[0]
}

func TestParseStackJava(t *testing.T) {
	// 日志头里的服务名/链路 ID 也是重要线索：随 logs 一起传入。
	logs := "2024-05-01 10:00:00 INFO [order-service,9f8e7d6c5b4a3210] warmup done GET /api/order/list"
	sa := ParseStack(javaStack, logs, "")
	if sa.Language != "java" {
		t.Fatalf("language = %q, 期望 java", sa.Language)
	}
	if sa.ExceptionType != "java.lang.NullPointerException" {
		t.Fatalf("ExceptionType = %q", sa.ExceptionType)
	}
	if sa.Thread != "http-nio-8080-exec-3" {
		t.Fatalf("Thread = %q", sa.Thread)
	}
	if !strings.Contains(sa.Message, "because \"order\" is null") {
		t.Fatalf("Message = %q", sa.Message)
	}
	f := firstFrame(t, sa)
	if f.File != "OrderService.java" || f.Line != 88 || f.Method != "create" {
		t.Fatalf("Frames[0] = %+v", f)
	}
	if f.Class != "OrderService" || f.Package != "com.acme.order" {
		t.Fatalf("Frames[0] 类/包解析错误: %+v", f)
	}
	if f.Confidence < 0.9 {
		t.Fatalf("标准格式置信度应 >= 0.9，得到 %v", f.Confidence)
	}
	if sa.Category != "null_pointer" {
		t.Fatalf("Category = %q, 期望 null_pointer", sa.Category)
	}
	if len(sa.CausedBy) < 2 {
		t.Fatalf("Caused by / Suppressed 未收集齐全: %v", sa.CausedBy)
	}
	if !contains(sa.Symbols, "OrderService") || !contains(sa.Symbols, "create") {
		t.Fatalf("Symbols = %v", sa.Symbols)
	}
	if !contains(sa.FileHints, "OrderService.java") {
		t.Fatalf("FileHints 缺少类名形态: %v", sa.FileHints)
	}
	if !contains(sa.FileHints, "com/acme/order/OrderService.java") {
		t.Fatalf("FileHints 缺少包路径形态: %v", sa.FileHints)
	}
	if len(sa.Frames) != 5 {
		t.Fatalf("帧数 = %d，期望 5（含 Caused by 帧）", len(sa.Frames))
	}
	if !contains(sa.Services, "order-service") {
		t.Fatalf("Services = %v，期望包含 order-service", sa.Services)
	}
	if sa.TraceID != "9f8e7d6c5b4a3210" {
		t.Fatalf("TraceID = %q", sa.TraceID)
	}
}

func TestParseStackGo(t *testing.T) {
	sa := ParseStack(goStack, "", "")
	if sa.Language != "go" {
		t.Fatalf("language = %q, 期望 go", sa.Language)
	}
	if sa.ExceptionType != "panic" {
		t.Fatalf("ExceptionType = %q", sa.ExceptionType)
	}
	if !strings.Contains(sa.Message, "nil pointer dereference") {
		t.Fatalf("Message = %q", sa.Message)
	}
	if sa.Thread != "goroutine 1 [running]:" {
		t.Fatalf("Thread = %q", sa.Thread)
	}
	f := firstFrame(t, sa)
	if f.File != "/app/internal/order/service.go" || f.Line != 42 {
		t.Fatalf("Frames[0] 文件/行号错误: %+v", f)
	}
	if f.Method != "Create" || f.Class != "OrderService" {
		t.Fatalf("Frames[0] 方法/接收者错误: %+v", f)
	}
	if f.Confidence < 0.9 {
		t.Fatalf("Go 标准格式置信度应 >= 0.9，得到 %v", f.Confidence)
	}
	if sa.Category != "null_pointer" {
		t.Fatalf("Category = %q, 期望 null_pointer", sa.Category)
	}
	if len(sa.Frames) != 2 {
		t.Fatalf("帧数 = %d，期望 2", len(sa.Frames))
	}
	if !contains(sa.Symbols, "Create") || !contains(sa.Symbols, "main") {
		t.Fatalf("Symbols = %v", sa.Symbols)
	}
	if len(sa.FileHints) == 0 {
		t.Fatalf("FileHints 不应为空")
	}
	if !contains(sa.FileHints, "internal/order/service.go") || !contains(sa.FileHints, "service.go") {
		t.Fatalf("FileHints 应包含完整路径与尾部组合: %v", sa.FileHints)
	}
}

func TestParseStackPython(t *testing.T) {
	sa := ParseStack(pythonStack, "", "")
	if sa.Language != "python" {
		t.Fatalf("language = %q, 期望 python", sa.Language)
	}
	if sa.ExceptionType != "ZeroDivisionError" {
		t.Fatalf("ExceptionType = %q", sa.ExceptionType)
	}
	if sa.Message != "division by zero" {
		t.Fatalf("Message = %q", sa.Message)
	}
	f := firstFrame(t, sa)
	if f.File != "/app/order/api.py" || f.Line != 22 || f.Method != "submit" {
		t.Fatalf("Frames[0] = %+v", f)
	}
	if f.Class != "api" {
		t.Fatalf("Frames[0].Class = %q，期望按文件名推导为 api", f.Class)
	}
	if sa.Category != "divide_by_zero" {
		t.Fatalf("Category = %q, 期望 divide_by_zero", sa.Category)
	}
	if len(sa.Frames) != 2 {
		t.Fatalf("帧数 = %d，期望 2", len(sa.Frames))
	}
	if !contains(sa.Symbols, "create") || !contains(sa.Symbols, "service") {
		t.Fatalf("Symbols = %v", sa.Symbols)
	}
	if !contains(sa.FileHints, "app/order/service.py") || !contains(sa.FileHints, "service.py") {
		t.Fatalf("FileHints = %v", sa.FileHints)
	}
}

func TestParseStackNode(t *testing.T) {
	sa := ParseStack(nodeStack, "", "")
	if sa.Language != "node" {
		t.Fatalf("language = %q, 期望 node", sa.Language)
	}
	if sa.ExceptionType != "TypeError" {
		t.Fatalf("ExceptionType = %q", sa.ExceptionType)
	}
	if !strings.Contains(sa.Message, "Cannot read properties of undefined") {
		t.Fatalf("Message = %q", sa.Message)
	}
	f := firstFrame(t, sa)
	if f.File != "/app/src/order.service.ts" || f.Line != 88 {
		t.Fatalf("Frames[0] = %+v", f)
	}
	if f.Method != "create" || f.Class != "OrderService" {
		t.Fatalf("Frames[0] 方法/类错误: %+v", f)
	}
	if f.Confidence < 0.9 {
		t.Fatalf("Node 标准格式置信度应 >= 0.9，得到 %v", f.Confidence)
	}
	// Node 的 TypeError 且消息为 "Cannot read properties of undefined" 属空值访问，
	// 分类结果应落在 null_pointer（与 NPE 语义一致，便于技能层统一处理）。
	if sa.Category != "null_pointer" {
		t.Fatalf("Category = %q, 期望 null_pointer", sa.Category)
	}
	if len(sa.Frames) != 3 {
		t.Fatalf("帧数 = %d，期望 3", len(sa.Frames))
	}
	if !contains(sa.Symbols, "OrderService") || !contains(sa.Symbols, "create") {
		t.Fatalf("Symbols = %v", sa.Symbols)
	}
	if !contains(sa.FileHints, "src/order.service.ts") {
		t.Fatalf("FileHints = %v", sa.FileHints)
	}
}

func TestParseStackPHP(t *testing.T) {
	sa := ParseStack(phpStack, "", "")
	if sa.Language != "php" {
		t.Fatalf("language = %q, 期望 php", sa.Language)
	}
	if sa.ExceptionType != "Error" {
		t.Fatalf("ExceptionType = %q", sa.ExceptionType)
	}
	f := firstFrame(t, sa)
	if f.File != "/app/src/OrderController.php" || f.Line != 42 || f.Method != "create" {
		t.Fatalf("Frames[0] = %+v", f)
	}
	if f.Class != "App\\Service\\OrderService" {
		t.Fatalf("Frames[0].Class = %q", f.Class)
	}
	if sa.Category != "null_pointer" {
		t.Fatalf("Category = %q, 期望 null_pointer", sa.Category)
	}
	if !contains(sa.FileHints, "OrderService.php") {
		t.Fatalf("FileHints = %v", sa.FileHints)
	}
}

func TestParseStackCategoryRules(t *testing.T) {
	cases := []struct {
		name  string
		stack string
		want  string
	}{
		{"索引越界", "java.lang.ArrayIndexOutOfBoundsException: Index 3 out of bounds for length 3\n\tat com.acme.A.f(A.java:1)", "index_out_of_bounds"},
		{"类型转换", "TypeError: Cannot convert undefined or null to object\n    at /app/src/a.js:1:1", "type_error"},
		{"类缺失", "java.lang.NoClassDefFoundError: com/acme/Missing\n\tat com.acme.A.f(A.java:1)", "class_not_found"},
		{"依赖缺失", "ModuleNotFoundError: No module named 'requests'", "dependency_missing"},
		{"连接拒绝", "ConnectionRefusedError: [Errno 111] Connection refused", "connection_refused"},
		{"超时", "java.net.SocketTimeoutException: Read timed out\n\tat com.acme.A.f(A.java:1)", "timeout"},
		{"除零", "java.lang.ArithmeticException: / by zero\n\tat com.acme.A.f(A.java:1)", "divide_by_zero"},
		{"并发修改", "java.util.ConcurrentModificationException\n\tat com.acme.A.f(A.java:1)", "concurrent_modification"},
		{"序列化", "JsonParseException: Unexpected character\n\tat com.acme.A.f(A.java:1)", "serialization_error"},
		{"OOM", "java.lang.OutOfMemoryError: Java heap space", "oom"},
		{"语法错误", "SyntaxError: unexpected token '<'\n    at /app/src/a.js:1:1", "syntax_error"},
		{"未知", "some totally unknown failure", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sa := ParseStack(c.stack, "", "")
			if sa.Category != c.want {
				t.Fatalf("Category = %q，期望 %q（language=%s）", sa.Category, c.want, sa.Language)
			}
		})
	}
}

func TestParseStackRobustness(t *testing.T) {
	// 空输入
	sa := ParseStack("", "", "")
	if sa == nil {
		t.Fatalf("空输入不应返回 nil")
	}
	if len(sa.Frames) != 0 || sa.Category != "unknown" {
		t.Fatalf("空输入结果异常: frames=%d category=%s", len(sa.Frames), sa.Category)
	}
	if sa.FileHints == nil || sa.Symbols == nil {
		t.Fatalf("空输入时 FileHints/Symbols 应为非 nil 空切片")
	}

	// 纯噪音
	noise := strings.Repeat("===== separator =====\n   \n\n", 50)
	sa = ParseStack(noise, noise, "")
	if sa == nil {
		t.Fatalf("噪音输入不应返回 nil")
	}
	if len(sa.Frames) > 0 {
		t.Fatalf("纯分隔线不应产生帧: %+v", sa.Frames[0])
	}

	// 乱码 / 非法 UTF-8 / 控制字符
	garbage := string([]byte{0xff, 0xfe, 0x00, 0x01, 0x02}) + "\n" + strings.Repeat("\x01\x02乱码\n", 100)
	sa = ParseStack(garbage, garbage, "")
	if sa == nil {
		t.Fatalf("乱码输入不应返回 nil")
	}

	// 超长单行
	sa = ParseStack(strings.Repeat("x", 200000), "", "")
	if sa == nil {
		t.Fatalf("超长单行不应返回 nil")
	}

	// 声明的语言与实际不符
	sa = ParseStack(pythonStack, "", "java")
	if len(sa.Frames) == 0 {
		t.Fatalf("语言不匹配时也应兜底解析出帧")
	}

	// 截断的堆栈（只有文件行）
	sa = ParseStack("panic: boom\n\n/only/file.go:12 +0x1", "", "go")
	if len(sa.Frames) == 0 {
		t.Fatalf("截断堆栈应兜底出帧")
	}
}

func TestCleanLogs(t *testing.T) {
	raw := "\x1b[31mERROR\x1b[0m something failed\n" +
		"==========\n" +
		"same line\n" +
		"same line\n" +
		"same line\n" +
		"same line\n" +
		"same line\n" +
		"DEBUG noisy\n"
	out := CleanLogs(raw, 2000)
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("ANSI 转义未清除: %q", out)
	}
	if strings.Contains(out, "==========") {
		t.Fatalf("分隔线未清除: %q", out)
	}
	if strings.Contains(out, "[重复日志已折叠]") == false {
		t.Fatalf("重复行未折叠: %q", out)
	}
	if strings.Count(out, "same line") != 4 {
		t.Fatalf("重复行保留数量异常: %q", out)
	}
	if !strings.Contains(out, "ERROR something failed") {
		t.Fatalf("正文丢失: %q", out)
	}

	// 截断
	long := CleanLogs(strings.Repeat("a long log line\n", 2000), 500)
	if len(long) > 500 {
		t.Fatalf("CleanedLog 未截断: %d", len(long))
	}
	if got := CleanLogs("", 100); got != "" {
		t.Fatalf("空日志应返回空串")
	}
}

func TestExtractEndpointsAndServices(t *testing.T) {
	sa := ParseStack("java.lang.RuntimeException: call failed\n\tat com.acme.A.f(A.java:1)", `2024-05-01 INFO POST /api/order/create failed, target http://order-service.internal:8080/api/order/1 traceId=abc123def456 service order_svc`, "")
	if !contains(sa.Endpoints, "POST /api/order/create") {
		t.Fatalf("Endpoints = %v", sa.Endpoints)
	}
	if !contains(sa.Endpoints, "/api/order/1") {
		t.Fatalf("Endpoints 缺少 URL 中的路径: %v", sa.Endpoints)
	}
	if !contains(sa.Services, "order-service") || !contains(sa.Services, "order_svc") {
		t.Fatalf("Services = %v", sa.Services)
	}
	if sa.TraceID != "abc123def456" {
		t.Fatalf("TraceID = %q", sa.TraceID)
	}
}

func TestParseStackLargeInputPerformance(t *testing.T) {
	// 1MB 级重复堆栈：必须 < 1s 且不 panic。
	unit := javaStack
	var b strings.Builder
	for b.Len() < 1<<20 {
		b.WriteString(unit)
	}
	big := b.String()
	t.Logf("输入大小 = %d 字节", len(big))

	start := time.Now()
	sa := ParseStack(big, big, "")
	elapsed := time.Since(start)
	t.Logf("1MB 堆栈解析耗时 = %v", elapsed)
	if sa == nil || len(sa.Frames) == 0 {
		t.Fatalf("大输入解析失败")
	}
	if elapsed > time.Second {
		t.Fatalf("1MB 堆栈解析耗时 %v，超过 1s", elapsed)
	}

	// 常规堆栈必须 < 100ms
	start = time.Now()
	ParseStack(javaStack, javaStack, "")
	if e := time.Since(start); e > 100*time.Millisecond {
		t.Fatalf("常规堆栈解析耗时 %v，超过 100ms", e)
	}
}

func TestParseStackNoisePerformance(t *testing.T) {
	// 无语言特征的噪音输入同样必须快速：覆盖 5 种解析器逐行扫描的最坏情况。
	var b strings.Builder
	for i := 0; i < 4000; i++ {
		b.WriteString("2024-05-01 10:00:00 INFO some ordinary application log line without any frame\n")
	}
	noise := b.String()
	t.Logf("噪音输入大小 = %d 字节", len(noise))
	start := time.Now()
	sa := ParseStack(noise, noise, "")
	elapsed := time.Since(start)
	t.Logf("噪音堆栈解析耗时 = %v", elapsed)
	if sa == nil {
		t.Fatalf("不应返回 nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("噪音解析耗时 %v，过慢", elapsed)
	}
}

func TestStackFrameSize(t *testing.T) {
	// 守卫：StackFrame 体积不应随本层实现膨胀（缓存与序列化都依赖它保持精简）。
	if size := int(unsafe.Sizeof(domain.StackFrame{})); size > 256 {
		t.Fatalf("StackFrame 体积 = %d，过大", size)
	}
}
