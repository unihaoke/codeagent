// 本文件实现多语言堆栈解析（Java/JVM、Go、Python、Node/TypeScript、PHP）。
//
// 全部为纯函数实现，不依赖 domain 接口，便于技能层（stacktrace_parse）与引擎层复用。
package source

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 解析限额（保证超长输入稳健且耗时可控）
// ---------------------------------------------------------------------------

const (
	// maxStackBytes 堆栈文本解析上限（超出部分丢弃，避免 1MB+ 输入拖慢主流程）。
	maxStackBytes = 1 << 20
	// maxStackLines 参与解析的最大行数。
	maxStackLines = 20000
	// maxFrames 最多保留的堆栈帧数。
	maxFrames = 200
	// maxFileHints 最多产出的文件线索数。
	maxFileHints = 32
	// maxSymbolsOut 最多产出的符号数。
	maxSymbolsOut = 64
	// maxEndpoints / maxServices / maxCausedBy 产出上限。
	maxEndpoints = 24
	maxServices  = 24
	maxCausedBy  = 32
	// cleanedLogLimit 清洗后日志的字符上限。
	cleanedLogLimit = 8000
	// debugLineLimit DEBUG 级别日志保留行数。
	debugLineLimit = 200
)

// 帧置信度取值。
const (
	confExact = 0.9 // 标准格式精确匹配
	confLoose = 0.5 // 模糊匹配
	confRaw   = 0.3 // 兜底（原始行）
)

// ---------------------------------------------------------------------------
// 正则表
// ---------------------------------------------------------------------------

var (
	// Java：at com.acme.order.OrderService.create(OrderService.java:88)
	javaFrameRe = regexp.MustCompile(`^\s*at\s+([\w$.]+)\.([\w$<>]+|init|<clinit>)\(([^()]*)\)`)
	// Java：Exception in thread "http-nio-8080-exec-3" java.lang.NullPointerException: msg
	javaThreadExcRe = regexp.MustCompile(`^\s*Exception in thread "([^"]*)"\s+([\w$.]+)(?::\s*(.*))?$`)
	// Java：java.lang.NullPointerException: msg（全限定名，或裸类名如 JsonParseException: msg）
	javaExcRe = regexp.MustCompile(`^\s*([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*\.[A-Z]\w*(?:Exception|Error|Throwable)|[A-Z]\w*(?:Exception|Error|Throwable))(?::\s*(.*))?$`)

	// Go：goroutine 1 [running]: / panic: runtime error: ...
	goGoroutineRe = regexp.MustCompile(`^\s*goroutine\s+(\d+)\s*(\[[^\]]*\])?\s*:`)
	goPanicRe     = regexp.MustCompile(`^\s*(?:panic|fatal error):\s*(.*)$`)
	// Go：main.foo(0x1, 0x2) 或 pkg.(*T).Method(...) 或 created by ...
	goFuncLineRe = regexp.MustCompile(`^\s*(created by\s+)?([\w$@./*()\[\]-]+\.[\w$]+(?:\.func\d+(?:\.\d+)*)?)(\(.*)?$`)
	// Go：\t/path/to/file.go:42 +0x1a
	goFileLineRe = regexp.MustCompile(`^\s+(.+?\.go):(\d+)(?:\s+\+0x[0-9a-fA-F]+)?\s*$`)

	// Python：File "/app/order/service.py", line 88, in create
	pyFrameRe = regexp.MustCompile(`^\s*File\s+"([^"]+)"\s*,\s*line\s+(\d+)(?:\s*,\s*in\s+(.+?))?\s*$`)
	// Python：ValueError: xxx
	pyExcRe = regexp.MustCompile(`^\s*([A-Za-z_][\w.]*(?:Error|Exception|Warning|Interrupt|Exit|Fault|BaseException|StopIteration|KeyboardInterrupt|SystemExit|GeneratorExit))(?::\s*(.*))?$`)

	// Node/TypeScript：at OrderService.create (/app/src/order.service.ts:88:12)
	nodeFuncFrameRe = regexp.MustCompile(`^\s*at\s+(?:async\s+)?(?:new\s+)?([\w$.<>\[\] ]+?)\s+\(([^()]+?):(\d+):(\d+)\)\s*$`)
	// Node：at /app/src/order.service.ts:88:12
	nodeBareFrameRe = regexp.MustCompile(`^\s*at\s+(?:async\s+)?(.+?):(\d+):(\d+)\s*$`)
	// Node：TypeError: Cannot read properties of undefined
	nodeExcRe = regexp.MustCompile(`^\s*([A-Z]\w*(?:Error|Exception)|Error|Exception)(?::\s*(.*))?$`)

	// 兜底：Error: message（message 不跨行，且不允许尾部再出现引号）
	looseErrRe = regexp.MustCompile(`^\s*([A-Z]\w*(?:Error|Exception))(?::\s*([^\n"]{0,400}))?\s*$`)

	// PHP：#0 /app/src/Order.php(88): create()
	phpFrameRe = regexp.MustCompile(`^\s*#(\d+)\s+([^()]+?)\((\d+)\)(?::\s*(.*))?\s*$`)
	// PHP：PHP Fatal error:  Uncaught Error: xxx in /path on line N（未锚定，消息可含冒号）
	phpUncaughtRe = regexp.MustCompile(`(?i)Uncaught\s+(Error|Exception|[A-Za-z_]\w+(?:Error|Exception))(?::\s*([^\n]*?))?(?:\s+in\s+/|$)`)
	// PHP：XxxError: message（行首锚定，允许前置修饰词）
	phpExcRe = regexp.MustCompile(`(?i)^(?:[A-Za-z]+\s+)*?(Error|Exception|[A-Za-z_]\w+(?:Error|Exception))(?::\s*(.*))?$`)

	// 通用：Caused by / Suppressed / ... N more
	causedByRe = regexp.MustCompile(`^\s*Caused by:\s*(.*)$`)
	suppressRe = regexp.MustCompile(`^\s*Suppressed:\s*(.*)$`)
	moreRe     = regexp.MustCompile(`^\s*\.\.\.\s+\d+\s+more\s*$`)
	// 通用异常头：XxxError: message（驼峰命名）
	camelErrRe = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*(?:Error|Exception))(?::\s*(.*))?$`)
)

// 行内文件位置（语言不明时的兜底）。
var inlineLocRe = regexp.MustCompile(`([^\s()"']+\.[A-Za-z0-9_]{1,8}):(\d+)(?::\d+)?`)

// ---------------------------------------------------------------------------
// 文本特征正则
// ---------------------------------------------------------------------------

var (
	ansiRe      = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	debugLineRe = regexp.MustCompile(`(?i)\b(DEBUG|TRACE)\b|\bdebug\b`)

	// 端点：GET /api/order/1、POST http://svc/api/order
	endpointMethodRe = regexp.MustCompile(`(?i)\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(https?://[^\s"',]+|/[A-Za-z0-9._~%\-/{}]*)`)
	// 端点：/api/order/** 形态路径
	endpointPathRe = regexp.MustCompile(`(/api/[A-Za-z0-9._~%\-/{}$*]*)`)
	// 服务名：order-service / order_svc / order.internal / order-service:8080
	serviceRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]*(?:[-_.][a-z0-9]+)*(?:-service|-svc|-server|-gateway|-api|-worker|-consumer|_service|_svc))\b`)
	// 服务名：a.b.internal / a.b.svc.cluster.local
	serviceFQDNRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*){1,4}\.(?:internal|local|svc|cluster\.local|corp|lan))\b`)
	// TraceID：traceId=xxx / trace_id: xxx / X-B3-TraceId: xxx / [app,traceId,spanId]
	//
	// 实现说明：本引擎对「有界量词 {n,m} + 尾随字面量」的组合存在匹配异常，
	// 因此这里统一采用「简单前缀 + 捕获即结束」的写法，避免尾随逗号/引号。
	traceIDRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\[[\w.-]+,([0-9a-fA-F]{8,64})`),
		regexp.MustCompile(`(?i)(?:x-b3-)?trace[-_]?id\W{0,3}([0-9a-fA-F-]{8,64})`),
		regexp.MustCompile(`(?i)\btrace\W{0,3}([0-9a-fA-F-]{8,64})`),
	}
)

// ---------------------------------------------------------------------------
// ParseStack
// ---------------------------------------------------------------------------

// ParseStack 解析堆栈文本，返回标准化分析结果。
//
// stacktrace 为原始异常堆栈（可为空），logs 为附加日志（可为空）。
// language 为空时自动探测；取值 java/go/python/node/php，其它值按自动探测处理。
// 函数对空输入、乱码、超长输入（1MB 级）均保持稳健，不 panic。
func ParseStack(stacktrace, logs, language string) *domain.StackAnalysis {
	stack := truncateText(stacktrace, maxStackBytes)
	logText := truncateText(logs, maxStackBytes)

	lines := splitLines(stack)
	if len(lines) > maxStackLines {
		lines = lines[:maxStackLines]
	}
	lang := normalizeStackLanguage(language)
	if lang == "" {
		lang = detectStackLanguage(strings.Join(headLines(lines, 80), "\n"), stack)
	}

	sa := &domain.StackAnalysis{Language: lang}
	if lang == unknownLanguage {
		sa.ExceptionType, sa.Message = firstErrorLike(lines)
		parseRawFrames(sa, lines, confRaw)
	} else {
		dispatchStackParser(sa, lines, lang)
		if len(sa.Frames) == 0 {
			// 声明的语言与内容不符：按原始行兜底，保证不丢线索。
			parseRawFrames(sa, lines, confRaw)
		}
	}
	finalizeStack(sa, logText)
	return sa
}

const unknownLanguage = "unknown"

// normalizeStackLanguage 归一化调用方传入的语言名。
func normalizeStackLanguage(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "java", "jvm", "kotlin", "scala", "groovy":
		return "java"
	case "go", "golang":
		return "go"
	case "python", "python3", "py":
		return "python"
	case "node", "nodejs", "javascript", "js", "typescript", "ts":
		return "node"
	case "php":
		return "php"
	default:
		return ""
	}
}

// detectStackLanguage 自动探测堆栈语言（优先级：强特征 > 行特征 > 兜底）。
func detectStackLanguage(text, full string) string {
	hay := text
	if hay == "" {
		hay = truncateText(full, 8192)
	}
	switch {
	case strings.Contains(hay, "Exception in thread"), strings.Contains(hay, "\n\tat "),
		strings.HasPrefix(hay, "\tat "), strings.Contains(hay, "Caused by:"),
		javaExcRe.MatchString(hay):
		return "java"
	case strings.Contains(hay, "goroutine "), strings.Contains(hay, "panic:"),
		strings.Contains(hay, "runtime error:"):
		return "go"
	case strings.Contains(hay, "Traceback (most recent call last)"),
		pyFrameRe.MatchString(hay) && strings.Contains(hay, "line "):
		return "python"
	case strings.Contains(hay, "node_modules"), strings.Contains(hay, "processTicksAndRejections"),
		strings.Contains(hay, "at async "), nodeFuncFrameRe.MatchString(hay),
		nodeBareFrameRe.MatchString(hay), nodeExcRe.MatchString(hay):
		return "node"
	case phpFrameRe.MatchString(hay), phpUncaughtRe.MatchString(hay), strings.Contains(hay, "PHP Fatal error"),
		strings.Contains(hay, "PHP Warning"), strings.Contains(hay, "Stack trace:"):
		return "php"
	case pyExcRe.MatchString(hay):
		return "python"
	case camelErrRe.MatchString(hay), looseErrRe.MatchString(hay):
		return "node"
	}
	return unknownLanguage
}

// dispatchStackParser 按语言分发解析。
func dispatchStackParser(sa *domain.StackAnalysis, lines []string, lang string) {
	switch lang {
	case "java":
		parseJavaStack(sa, lines)
	case "go":
		parseGoStack(sa, lines)
	case "python":
		parsePythonStack(sa, lines)
	case "node":
		parseNodeStack(sa, lines)
	case "php":
		parsePHPStack(sa, lines)
	default:
		parseRawFrames(sa, lines, confRaw)
	}
}

// ---------------------------------------------------------------------------
// Java
// ---------------------------------------------------------------------------

func parseJavaStack(sa *domain.StackAnalysis, lines []string) {
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := causedByRe.FindStringSubmatch(line); m != nil {
			sa.CausedBy = appendLimited(sa.CausedBy, strings.TrimSpace(m[1]), maxCausedBy)
			if sa.ExceptionType == "" {
				sa.ExceptionType, sa.Message = splitTypeMessage(m[1])
			}
			continue
		}
		if m := suppressRe.FindStringSubmatch(line); m != nil {
			sa.CausedBy = appendLimited(sa.CausedBy, "Suppressed: "+strings.TrimSpace(m[1]), maxCausedBy)
			continue
		}
		if moreRe.MatchString(line) {
			continue
		}
		if m := javaFrameRe.FindStringSubmatch(line); m != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			sa.Frames = append(sa.Frames, buildFrame(strings.TrimSpace(line), m[1], m[2], m[3], "java", confExact))
			continue
		}
		if m := javaThreadExcRe.FindStringSubmatch(line); m != nil {
			sa.Thread = strings.TrimSpace(m[1])
			if sa.ExceptionType == "" {
				sa.ExceptionType = strings.TrimSpace(m[2])
				sa.Message = strings.TrimSpace(m[3])
			}
			continue
		}
		if m := javaExcRe.FindStringSubmatch(line); m != nil {
			if sa.ExceptionType == "" {
				sa.ExceptionType = strings.TrimSpace(m[1])
				sa.Message = strings.TrimSpace(m[2])
			}
			continue
		}
	}
}

// ---------------------------------------------------------------------------
// Go
// ---------------------------------------------------------------------------

func parseGoStack(sa *domain.StackAnalysis, lines []string) {
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := goGoroutineRe.FindStringSubmatch(line); m != nil {
			if sa.Thread == "" {
				sa.Thread = trimmed
			}
			continue
		}
		if m := goPanicRe.FindStringSubmatch(line); m != nil {
			reason := strings.TrimSpace(m[1])
			if sa.ExceptionType == "" {
				sa.ExceptionType = "panic"
				sa.Message = reason
			}
			continue
		}
		// 函数行 + 文件行配对
		if fm := goFuncLineRe.FindStringSubmatch(line); fm != nil && i+1 < len(lines) {
			if fileM := goFileLineRe.FindStringSubmatch(strings.TrimRight(lines[i+1], "\r")); fileM != nil {
				if len(sa.Frames) >= maxFrames {
					i++
					continue
				}
				funcName := strings.TrimSpace(fm[2])
				conf := confExact
				if fm[1] != "" { // created by ...
					conf = confLoose
				}
				pkg, class, method := splitGoFunc(funcName)
				sa.Frames = append(sa.Frames, domain.StackFrame{
					Raw:        strings.TrimSpace(line + "\n" + lines[i+1]),
					Package:    pkg,
					Class:      class,
					Method:     method,
					File:       normalizePath(fileM[1]),
					Line:       atoiSafe(fileM[2]),
					Language:   "go",
					Confidence: conf,
				})
				i++
				continue
			}
		}
		// 仅有文件行的兜底（goroutine 段被截断时）
		if fileM := goFileLineRe.FindStringSubmatch(line); fileM != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			sa.Frames = append(sa.Frames, domain.StackFrame{
				Raw:        trimmed,
				File:       normalizePath(fileM[1]),
				Line:       atoiSafe(fileM[2]),
				Language:   "go",
				Confidence: confLoose,
			})
		}
	}
}

// splitGoFunc 拆分 Go 函数全名（main.foo / pkg.(*T).Method / pkg.func.func1）。
func splitGoFunc(full string) (pkg, class, method string) {
	if full == "" {
		return "", "", ""
	}
	parts := strings.Split(full, ".")
	method = parts[len(parts)-1]
	rest := parts[:len(parts)-1]
	if len(rest) > 0 {
		last := rest[len(rest)-1]
		if strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
			class = strings.Trim(last, "()*")
			rest = rest[:len(rest)-1]
		}
	}
	if len(rest) > 0 {
		pkg = strings.Join(rest, ".")
	}
	return pkg, class, method
}

// ---------------------------------------------------------------------------
// Python
// ---------------------------------------------------------------------------

func parsePythonStack(sa *domain.StackAnalysis, lines []string) {
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := pyFrameRe.FindStringSubmatch(line); m != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			file := normalizePath(m[1])
			funcName := strings.TrimSpace(m[3])
			class, method := splitPyFunc(file, funcName)
			sa.Frames = append(sa.Frames, domain.StackFrame{
				Raw:        trimmed,
				Package:    pyPackage(file),
				Class:      class,
				Method:     method,
				File:       file,
				Line:       atoiSafe(m[2]),
				Language:   "python",
				Confidence: confExact,
			})
			continue
		}
		if m := pyExcRe.FindStringSubmatch(line); m != nil {
			sa.CausedBy = appendLimited(sa.CausedBy, trimmed, maxCausedBy)
			sa.ExceptionType = strings.TrimSpace(m[1])
			sa.Message = strings.TrimSpace(m[2])
			continue
		}
	}
	// Python 的 CausedBy 语义为「异常链」，首个异常头不作为 cause 重复记录。
	if len(sa.CausedBy) > 0 && sa.ExceptionType != "" {
		head := sa.ExceptionType
		if sa.Message != "" {
			head += ": " + sa.Message
		}
		sa.CausedBy = removeFirstEqual(sa.CausedBy, head)
	}
}

// splitPyFunc 解析 Python 帧的类/方法（`OrderService.create` → OrderService / create）。
func splitPyFunc(file, funcName string) (class, method string) {
	funcName = strings.TrimSpace(funcName)
	if funcName == "" {
		return pyFileNameClass(file), ""
	}
	if i := strings.LastIndex(funcName, "."); i > 0 {
		return funcName[:i], funcName[i+1:]
	}
	return pyFileNameClass(file), funcName
}

// pyFileNameClass 用文件名推导类名（service.py → service）。
func pyFileNameClass(file string) string {
	base := baseName(file)
	return strings.TrimSuffix(base, ".py")
}

// pyPackage 用文件路径推导 Python 包路径（/app/order/service.py → app.order）。
func pyPackage(file string) string {
	p := strings.TrimSuffix(normalizePath(file), ".py")
	segs := strings.Split(p, "/")
	if len(segs) <= 1 {
		return ""
	}
	segs = segs[:len(segs)-1]
	if len(segs) > 3 {
		segs = segs[len(segs)-3:]
	}
	return strings.Join(segs, ".")
}

// ---------------------------------------------------------------------------
// Node / TypeScript
// ---------------------------------------------------------------------------

func parseNodeStack(sa *domain.StackAnalysis, lines []string) {
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := nodeFuncFrameRe.FindStringSubmatch(line); m != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			full := strings.TrimSpace(m[1])
			class, method := splitJSFunc(full)
			sa.Frames = append(sa.Frames, domain.StackFrame{
				Raw:        trimmed,
				Class:      class,
				Method:     method,
				File:       normalizePath(m[2]),
				Line:       atoiSafe(m[3]),
				Language:   "node",
				Confidence: confExact,
			})
			continue
		}
		if m := nodeBareFrameRe.FindStringSubmatch(line); m != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			sa.Frames = append(sa.Frames, domain.StackFrame{
				Raw:        trimmed,
				Class:      jsFileClass(m[1]),
				File:       normalizePath(m[1]),
				Line:       atoiSafe(m[2]),
				Language:   "node",
				Confidence: confExact,
			})
			continue
		}
		if m := nodeExcRe.FindStringSubmatch(line); m != nil {
			sa.ExceptionType = strings.TrimSpace(m[1])
			sa.Message = strings.TrimSpace(m[2])
			continue
		}
		if m := looseErrRe.FindStringSubmatch(line); m != nil {
			sa.ExceptionType = strings.TrimSpace(m[1])
			sa.Message = strings.TrimSpace(m[2])
			continue
		}
	}
}

// splitJSFunc 解析 Node 帧函数名（`OrderService.create` → OrderService / create）。
func splitJSFunc(full string) (class, method string) {
	full = strings.TrimSpace(full)
	full = strings.TrimPrefix(full, "async ")
	full = strings.TrimPrefix(full, "new ")
	if full == "" {
		return "", ""
	}
	if i := strings.LastIndex(full, "."); i > 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

// jsFileClass 从文件名推导类名（order.service.ts → order.service）。
func jsFileClass(file string) string {
	base := baseName(file)
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// ---------------------------------------------------------------------------
// PHP
// ---------------------------------------------------------------------------

func parsePHPStack(sa *domain.StackAnalysis, lines []string) {
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := phpFrameRe.FindStringSubmatch(line); m != nil {
			if len(sa.Frames) >= maxFrames {
				continue
			}
			file := normalizePath(m[2])
			call := strings.TrimSpace(m[4])
			class, method := splitPHPFunc(call)
			if class == "" {
				class = strings.TrimSuffix(baseName(file), ".php")
			}
			sa.Frames = append(sa.Frames, domain.StackFrame{
				Raw:        trimmed,
				Class:      class,
				Method:     method,
				File:       file,
				Line:       atoiSafe(m[3]),
				Language:   "php",
				Confidence: confExact,
			})
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(strings.ToLower(trimmed), "stack trace:") {
			continue
		}
		if m := phpUncaughtRe.FindStringSubmatch(trimmed); m != nil {
			if sa.ExceptionType == "" {
				sa.ExceptionType = strings.TrimSpace(m[1])
				sa.Message = strings.TrimSpace(m[2])
			}
			continue
		}
		if m := phpExcRe.FindStringSubmatch(trimmed); m != nil {
			if sa.ExceptionType == "" {
				sa.ExceptionType = strings.TrimSpace(m[1])
				sa.Message = strings.TrimSpace(m[2])
			}
		}
	}
}

// splitPHPFunc 解析 `Order->create()` / `Order::create()` / `create()`。
func splitPHPFunc(call string) (class, method string) {
	call = strings.TrimSpace(call)
	if call == "" {
		return "", ""
	}
	if i := strings.Index(call, "->"); i > 0 {
		return strings.TrimSpace(call[:i]), trimCall(call[i+2:])
	}
	if i := strings.Index(call, "::"); i > 0 {
		return strings.TrimSpace(call[:i]), trimCall(call[i+2:])
	}
	return "", trimCall(call)
}

// trimCall 去掉调用表达式的括号与参数。
func trimCall(s string) string {
	if i := strings.Index(s, "("); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// 兜底与工具
// ---------------------------------------------------------------------------

// parseRawFrames 把非空行作为 Raw 帧（置信度最低，仅保证不丢线索）。
func parseRawFrames(sa *domain.StackAnalysis, lines []string, conf float64) {
	for _, raw := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if trimmed == "" || len(sa.Frames) >= maxFrames {
			continue
		}
		// 空行与分隔线不产生帧。
		if isNoiseLine(trimmed) {
			continue
		}
		// 尝试抽取行内文件位置，提升兜底帧的可用性。
		frame := domain.StackFrame{Raw: trimmed, Confidence: conf}
		if m := inlineLocRe.FindStringSubmatch(trimmed); m != nil {
			frame.File = normalizePath(m[1])
			frame.Line = atoiSafe(m[2])
			frame.Confidence = confLoose
		}
		sa.Frames = append(sa.Frames, frame)
	}
}

// isNoiseLine 判断一行是否为噪音（空行、纯分隔线、带说明文字的分隔线）。
//
// 例：""、"   "、"-----"、"=========="、"===== separator =====" 均为噪音。
func isNoiseLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return true
	}
	seps := 0
	for _, r := range t {
		switch {
		case r == '-' || r == '=' || r == '_' || r == '*' || r == '#' || r == '~' || r == '+' || r == '|' || r == '.':
			seps++
		case r == ' ' || r == '\t':
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		default:
			return false // 含数字/其他符号：视为有效内容行
		}
	}
	return seps >= 4
}

// buildFrame 组装一帧（Java 风格：(file:line) 或 (Unknown Source)）。
func buildFrame(raw, fullClass, method, loc, lang string, conf float64) domain.StackFrame {
	pkg, class := splitQualified(fullClass)
	frame := domain.StackFrame{
		Raw:        raw,
		Package:    pkg,
		Class:      class,
		Method:     strings.TrimSpace(method),
		Language:   lang,
		Confidence: conf,
	}
	loc = strings.TrimSpace(loc)
	if loc == "" || strings.Contains(loc, "Unknown Source") || strings.Contains(loc, "Native Method") {
		return frame
	}
	idx := strings.LastIndex(loc, ":")
	if idx > 0 {
		if n := atoiSafe(loc[idx+1:]); n > 0 {
			frame.Line = n
			frame.File = normalizePath(loc[:idx])
			return frame
		}
	}
	frame.File = normalizePath(loc)
	return frame
}

// splitQualified 拆分全限定类名（com.acme.order.OrderService → pkg + class）。
func splitQualified(full string) (pkg, class string) {
	full = strings.TrimSpace(full)
	if full == "" {
		return "", ""
	}
	if i := strings.LastIndex(full, "."); i > 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

// firstErrorLike 从行集合中挑出最像异常头的行。
func firstErrorLike(lines []string) (typ, msg string) {
	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if m := goPanicRe.FindStringSubmatch(trimmed); m != nil {
			return "panic", strings.TrimSpace(m[1])
		}
		if m := javaThreadExcRe.FindStringSubmatch(trimmed); m != nil {
			return strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
		}
		if m := javaExcRe.FindStringSubmatch(trimmed); m != nil {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
		if m := camelErrRe.FindStringSubmatch(trimmed); m != nil {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
		if m := pyExcRe.FindStringSubmatch(trimmed); m != nil {
			return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
	}
	return "", ""
}

// splitTypeMessage 拆分 "java.lang.NullPointerException: msg" 形态。
func splitTypeMessage(s string) (typ, msg string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	// 异常头以 `<类型>: <消息>` 呈现，类型部分不含空格。
	if i := strings.Index(s, ": "); i > 0 && !strings.ContainsAny(s[:i], " \t") {
		return s[:i], strings.TrimSpace(s[i+2:])
	}
	if i := strings.Index(s, ":"); i > 0 && !strings.ContainsAny(s[:i], " \t") {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// ---------------------------------------------------------------------------
// 后处理：分类 / 线索 / 符号 / 端点 / 服务 / TraceID / 日志清洗
// ---------------------------------------------------------------------------

// finalizeStack 汇总分析结果的全部派生字段。
func finalizeStack(sa *domain.StackAnalysis, logText string) {
	if sa.Language == "" {
		sa.Language = unknownLanguage
	}
	sa.Category = categorizeStack(sa, logText)
	sa.Symbols = extractStackSymbols(sa)
	sa.FileHints = extractFileHints(sa.Frames)
	hay := clueHaystack(sa, logText)
	sa.Endpoints = extractEndpoints(hay)
	sa.Services = extractServices(hay)
	sa.TraceID = extractTraceID(hay)
	sa.CleanedLog = CleanLogs(logText, cleanedLogLimit)
}

// clueHaystackLimit 线索提取（端点/服务/TraceID）的文本上限。
//
// 正则扫描按字节计费，对 1MB 级堆栈必须先裁剪再匹配，否则会拖慢主流程。
const clueHaystackLimit = 64 << 10

// clueHaystack 组装用于二次线索提取的文本（受 clueHaystackLimit 限制）。
func clueHaystack(sa *domain.StackAnalysis, logText string) string {
	head := truncateText(stackText(sa), clueHaystackLimit)
	if strings.TrimSpace(logText) == "" {
		return head
	}
	rest := clueHaystackLimit - len(head)
	if rest <= 0 {
		return head
	}
	return head + "\n" + truncateText(logText, rest)
}

// stackCategoryRule 分类规则：命中任一 typePatterns 或 messagePatterns 即归类。
type stackCategoryRule struct {
	category        string
	typePatterns    []string
	messagePatterns []string
}

// stackCategoryRules 异常分类规则表（顺序即优先级）。
var stackCategoryRules = []stackCategoryRule{
	{"null_pointer", []string{"NullPointerException", "nil pointer", "nil dereference", "NullReferenceException", "Cannot read properties of undefined", "Cannot read properties of null", "Cannot read property", "undefined is not a function", "is not a function", "Call to a member function"}, []string{"null pointer", "nil pointer dereference", "is null", "was null", "null reference"}},
	{"index_out_of_bounds", []string{"IndexOutOfBounds", "ArrayIndexOutOfBounds", "StringIndexOutOfBounds", "IndexError", "out of range", "slice bounds out of range", "SubscriptOutOfRange"}, []string{"index out of bounds", "index out of range", "list index out of range", "out of bounds", "out of range"}},
	{"type_error", []string{"ClassCastException", "TypeError", "InvalidCastException", "NumberFormatException", "ValueError", "UnsupportedOperationException", "type assertion"}, []string{"cannot convert", "type assertion", "invalid literal for", "could not convert", "unsupported type"}},
	{"class_not_found", []string{"NoClassDefFoundError", "ClassNotFoundException", "NoSuchMethodError", "NoSuchFieldError", "ClassNotFoundError"}, []string{"class not found", "cannot resolve symbol", "cannot find symbol", "unknown class"}},
	{"dependency_missing", []string{"ModuleNotFoundError", "ImportError", "UnsatisfiedLinkError", "Cannot find module", "MODULE_NOT_FOUND"}, []string{"no module named", "cannot find module", "module not found", "unable to load", "could not resolve", "package does not exist"}},
	{"connection_refused", []string{"ECONNREFUSED", "ConnectionRefused", "ConnectException", "ConnectionResetException"}, []string{"connection refused", "connect: connection refused", "connection reset by peer", "failed to connect", "no route to host"}},
	{"timeout", []string{"SocketTimeoutException", "TimeoutException", "TimeoutError", "ETIMEDOUT", "context deadline exceeded", "DeadlineExceeded"}, []string{"timed out", "timeout", "deadline exceeded", "etimedout"}},
	{"divide_by_zero", []string{"ArithmeticException"}, []string{"divide by zero", "division by zero", "/ by zero", "zero division"}},
	{"concurrent_modification", []string{"ConcurrentModificationException", "OptimisticLockException", "DeadlockLoserDataAccessException"}, []string{"concurrent modification", "concurrently modified", "deadlock", "lock wait timeout", "optimistic lock"}},
	{"serialization_error", []string{"JsonParseException", "JSONDecodeError", "JsonMappingException", "InvalidDefinitionException", "SerializationException", "UnmarshalError"}, []string{"cannot deserialize", "failed to parse json", "not valid json", "unexpected character"}},
	{"oom", []string{"OutOfMemoryError", "MemoryError", "Allocation failed"}, []string{"out of memory", "cannot allocate memory", "java heap space", "heap out of memory"}},
	{"syntax_error", []string{"SyntaxError", "ParseError", "CompileError"}, []string{"syntax error", "unexpected end of input", "unexpected token", "parse error", "compilation error", "invalid syntax"}},
}

// categorizeStack 依据异常类型、消息与堆栈帧内容做异常分类。
//
// 匹配顺序：先全表匹配异常类型（类型证据最强），再全表匹配消息/帧文本，
// 保证 NoClassDefFoundError → class_not_found、JsonParseException → serialization_error
// 这类由类型主导的判定不会被消息关键词抢占。
func categorizeStack(sa *domain.StackAnalysis, logText string) string {
	var b strings.Builder
	b.WriteString(sa.ExceptionType)
	b.WriteString("\n")
	b.WriteString(sa.Message)
	for _, c := range sa.CausedBy {
		b.WriteString("\n")
		b.WriteString(c)
	}
	for i, f := range sa.Frames {
		if i >= 5 {
			break
		}
		b.WriteString("\n")
		b.WriteString(f.Raw)
	}
	b.WriteString("\n")
	b.WriteString(truncateText(logText, 16384))
	hay := strings.ToLower(b.String())

	// 第一轮：异常类型匹配。
	for _, rule := range stackCategoryRules {
		for _, p := range rule.typePatterns {
			if strings.Contains(hay, strings.ToLower(p)) {
				return rule.category
			}
		}
	}
	// 第二轮：消息 / 帧文本匹配。
	for _, rule := range stackCategoryRules {
		for _, p := range rule.messagePatterns {
			if strings.Contains(hay, strings.ToLower(p)) {
				return rule.category
			}
		}
	}
	return "unknown"
}

// extractStackSymbols 提取类名 / 方法名 / Bean 名（去重保序）。
func extractStackSymbols(sa *domain.StackAnalysis) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || len(out) >= maxSymbolsOut {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, f := range sa.Frames {
		add(f.Class)
		add(f.Method)
		// Bean 名：类名首字母小写。
		if f.Class != "" {
			add(lowerFirst(f.Class))
		}
		add(f.Package)
	}
	if sa.ExceptionType != "" {
		if _, cls := splitQualified(sa.ExceptionType); cls != "" {
			add(cls)
		}
	}
	return out
}

// extractFileHints 从帧中提取候选文件路径/类名（多形态产出）。
func extractFileHints(frames []domain.StackFrame) []string {
	out := make([]string, 0, 16)
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(strings.TrimPrefix(normalizeRepoPath(p), "/"))
		if p == "" || seen[p] || len(out) >= maxFileHints {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, f := range frames {
		if f.Class != "" {
			// Java / PHP 风格：类名 + .java / .php
			switch {
			case f.Language == "java":
				add(f.Class + ".java")
				if f.Package != "" {
					add(strings.ReplaceAll(f.Package, ".", "/") + "/" + f.Class + ".java")
				}
			case f.Language == "php":
				add(f.Class + ".php")
			}
		}
		if f.File != "" {
			add(f.File)
			add(baseName(f.File))
			for _, suffix := range pathSuffixes(f.File) {
				add(suffix)
			}
		}
		if f.Language == "node" && f.Class != "" && strings.Contains(f.Class, ".") && f.File == "" {
			add(strings.ReplaceAll(f.Class, ".", "/"))
		}
	}
	return out
}

// pathSuffixes 返回路径的尾部 2..4 段组合（Go/Python/Node 定位用）。
func pathSuffixes(path string) []string {
	segs := strings.Split(normalizeRepoPath(path), "/")
	out := make([]string, 0, 3)
	for n := 2; n <= 4; n++ {
		if len(segs) < n {
			break
		}
		out = append(out, strings.Join(segs[len(segs)-n:], "/"))
	}
	return out
}

// extractEndpoints 从堆栈与日志中提取 HTTP 端点。
func extractEndpoints(hay string) []string {
	out := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(strings.TrimRight(v, ".,;)\"'"))
		if v == "" || seen[v] || len(out) >= maxEndpoints {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, m := range endpointMethodRe.FindAllStringSubmatch(hay, 64) {
		add(strings.ToUpper(m[1]) + " " + trimURLToPath(m[2]))
	}
	for _, m := range endpointPathRe.FindAllStringSubmatch(hay, 64) {
		add(m[1])
	}
	return out
}

// trimURLToPath 把完整 URL 裁剪为 path（丢弃 query 与 fragment）。
func trimURLToPath(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			u = rest[j:]
		} else {
			u = "/"
		}
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// extractServices 提取服务名（order-service / order_svc / order.internal）。
func extractServices(hay string) []string {
	out := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] || len(out) >= maxServices {
			return
		}
		switch v {
		case "java-service", "test-service", "my-service", "the-service", "mock-service":
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, m := range serviceRe.FindAllStringSubmatch(hay, 96) {
		add(m[1])
	}
	for _, m := range serviceFQDNRe.FindAllStringSubmatch(hay, 96) {
		add(m[1])
	}
	return out
}

// extractTraceID 提取链路追踪 ID。
func extractTraceID(hay string) string {
	for _, re := range traceIDRes {
		if m := re.FindStringSubmatch(hay); m != nil {
			if v := strings.TrimSpace(m[1]); v != "" {
				return v
			}
		}
	}
	return ""
}

// stackText 拼接堆栈分析中的全部文本（用于二次线索提取）。
func stackText(sa *domain.StackAnalysis) string {
	var b strings.Builder
	b.WriteString(sa.ExceptionType)
	b.WriteString("\n")
	b.WriteString(sa.Message)
	for _, c := range sa.CausedBy {
		b.WriteString("\n")
		b.WriteString(c)
	}
	for _, f := range sa.Frames {
		b.WriteString("\n")
		b.WriteString(f.Raw)
	}
	return b.String()
}

// CleanLogs 清洗日志：去 ANSI 与分隔线、抑制重复行、限制 DEBUG 噪音、按字符数截断。
//
// 脱敏由调用方负责（本函数只做清洗与截断）。
func CleanLogs(logs string, limit int) string {
	if limit <= 0 {
		limit = cleanedLogLimit
	}
	logs = truncateText(logs, maxStackBytes)
	if strings.TrimSpace(logs) == "" {
		return ""
	}
	logs = ansiRe.ReplaceAllString(logs, "")
	raw := splitLines(logs)

	out := make([]string, 0, len(raw))
	seen := map[string]int{}
	debugKept := 0
	for _, l := range raw {
		l = strings.TrimRight(l, " \t\r")
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || isNoiseLine(trimmed) {
			continue
		}
		if debugLineRe.MatchString(trimmed) {
			if debugKept >= debugLineLimit {
				continue
			}
			debugKept++
		}
		// 重复行抑制：同一行最多保留 3 次，超出折叠为一条提示。
		seen[trimmed]++
		switch {
		case seen[trimmed] == 4:
			out = append(out, fmt.Sprintf("[重复日志已折叠] %s", trimmed))
			continue
		case seen[trimmed] > 4:
			continue
		}
		out = append(out, trimmed)
	}
	joined := strings.Join(out, "\n")
	return truncateText(joined, limit)
}

// ---------------------------------------------------------------------------
// 基础工具
// ---------------------------------------------------------------------------

// truncateText 按字节安全截断文本（不切断多字节字符）。
func truncateText(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return TruncateUTF8(s, maxBytes)
}

// headLines 取前 n 行。
func headLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[:n]
}

// normalizePath 归一化堆栈中的文件路径（统一 /、去掉 URI 前缀与冗余段）。
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "file://")
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.Index(p, "!/"); i >= 0 {
		p = p[i+2:]
	}
	if i := strings.Index(p, "jar:"); i == 0 {
		p = p[len("jar:"):]
	}
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	if len(p) > 1 {
		p = strings.TrimPrefix(p, "./")
	}
	return p
}

// baseName 返回路径最后一段。
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// atoiSafe 安全解析非负整数（失败返回 0）。
func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}

// appendLimited 追加且限制容量（元素为空时忽略）。
func appendLimited(list []string, v string, limit int) []string {
	if strings.TrimSpace(v) == "" || len(list) >= limit {
		return list
	}
	return append(list, strings.TrimSpace(v))
}

// removeFirstEqual 删除首个等于 v 的元素。
func removeFirstEqual(list []string, v string) []string {
	for i, s := range list {
		if s == v {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

// lowerFirst 首字母小写（Bean 名推导）。
func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if !unicode.IsUpper(r) {
		return s
	}
	return string(unicode.ToLower(r)) + s[size:]
}
