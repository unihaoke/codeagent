package builtin

// 本文件实现**自包含**的多语言堆栈解析器（Java / Go / Python / Node(TS/JS) / PHP）。
//
// 为什么自包含而不复用 internal/source.ParseStack：
//  1. 第五层（Skill）与第三层（Source）由不同开发者并行开发，若本包在编译期依赖
//     internal/source，任何一方签名调整都会互相阻塞联调，甚至导致整仓编译失败；
//  2. 堆栈解析结果 domain.StackAnalysis 本身是技能契约的一部分，自包含实现可离线单测、
//     可确定性复现；
//  3. 后续如需更精细的解析（例如读取 Git 中的真实源码行），可通过 Deps 注入
//     source.ParseStack 做增强，而不会破坏当前契约。
//
// 该实现约 200 行，覆盖常见框架的堆栈格式，产出 domain.StackAnalysis 所需的全部字段。

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 通用 JSON 辅助（内置技能之间复用：入参可能来自 JSON、也可能来自进程内结构体）
// ---------------------------------------------------------------------------

// toMap 把任意值转换为 map[string]any（失败返回 nil）。
func toMap(v any) map[string]any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return t
	case string:
		// 兼容调用方把结构体序列化成字符串传递的情况。
		if strings.TrimSpace(t) == "" {
			return nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(t), &out); err != nil {
			return nil
		}
		return out
	default:
		return jsonRoundTrip[map[string]any](v)
	}
}

// jsonRoundTrip 通过 JSON 序列化做宽容反序列化：字段缺失不报错，多余字段忽略。
func jsonRoundTrip[T any](v any) T {
	var zero T
	if v == nil {
		return zero
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return zero
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero
	}
	return out
}

// decodeSlices 宽容解析代码切片数组（支持 []domain.CodeSlice、[]map[string]any、JSON 字符串）。
func decodeSlices(v any) []domain.CodeSlice {
	if v == nil {
		return nil
	}
	if list, ok := v.([]domain.CodeSlice); ok {
		return list
	}
	out := jsonRoundTrip[[]domain.CodeSlice](v)
	if out == nil {
		// 单对象也容忍。
		if one := jsonRoundTrip[domain.CodeSlice](v); one.Path != "" || one.Content != "" {
			return []domain.CodeSlice{one}
		}
	}
	return out
}

// decodeCandidates 宽容解析候选仓库数组。
func decodeCandidates(v any) []domain.RepoCandidate {
	if v == nil {
		return nil
	}
	if list, ok := v.([]domain.RepoCandidate); ok {
		return list
	}
	out := jsonRoundTrip[[]domain.RepoCandidate](v)
	if out == nil {
		if one := jsonRoundTrip[domain.RepoCandidate](v); one.RepositoryID != "" || one.RepoKey != "" {
			return []domain.RepoCandidate{one}
		}
	}
	return out
}

// decodeStack 宽容解析堆栈分析结果。
func decodeStack(v any) *domain.StackAnalysis {
	if v == nil {
		return nil
	}
	if s, ok := v.(*domain.StackAnalysis); ok {
		return s
	}
	if s, ok := v.(domain.StackAnalysis); ok {
		return &s
	}
	out := jsonRoundTrip[domain.StackAnalysis](v)
	if out.Language == "" && len(out.Frames) == 0 && out.ExceptionType == "" && out.Category == "" {
		return nil
	}
	return &out
}

// decodePatches 宽容解析补丁数组。
func decodePatches(v any) []domain.Patch {
	if v == nil {
		return nil
	}
	if list, ok := v.([]domain.Patch); ok {
		return list
	}
	return jsonRoundTrip[[]domain.Patch](v)
}

// decodeInts 宽容解析整数数组。
func decodeInts(v any) []int {
	if v == nil {
		return nil
	}
	if list, ok := v.([]int); ok {
		return list
	}
	var raw []any
	raw = jsonRoundTrip[[]any](v)
	if raw == nil {
		return nil
	}
	out := make([]int, 0, len(raw))
	for _, item := range raw {
		if f, ok := toFloatValue(item); ok {
			out = append(out, int(f))
		}
	}
	return out
}

// toFloatValue 把 JSON 数值转为 float64。
func toFloatValue(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// atoiSafe 解析正则捕获到的十进制数字。
func atoiSafe(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0, false
		}
	}
	return n, true
}

// ---------------------------------------------------------------------------
// 堆栈解析
// ---------------------------------------------------------------------------

// 各语言堆栈行的正则（包级预编译，保证确定性且无重复开销）。
var (
	reJavaFrame    = regexp.MustCompile(`^\s*at\s+([\w$.<>]+)\(([^)]*)\)`)
	reJavaThread   = regexp.MustCompile(`^Exception in thread "([^"]*)"\s+([\w$.]+)(?::\s*(.*))?`)
	reJavaCausedBy = regexp.MustCompile(`^\s*Caused by:\s*([\w$.]+)(?::\s*(.*))?`)
	reJavaTypeLine = regexp.MustCompile(`^([\w$.]*(?:Exception|Error|Throwable))(?::\s*(.*))?$`)
	reFileLine     = regexp.MustCompile(`^([^:]+):(\d+)`)

	reGoGoroutine = regexp.MustCompile(`^goroutine\s+\d+\s+\[([^\]]*)\]:`)
	reGoFunc      = regexp.MustCompile(`^([\w\.\(\)\*\[\]/<>@$-]+)\(.*\)$`)
	reGoFile      = regexp.MustCompile(`^\s+([^\s:]+\.go):(\d+)(?:\s+\+0x[0-9a-fA-F]+)?\s*$`)

	rePyFile      = regexp.MustCompile(`^\s*File "([^"]+)", line (\d+)(?:, in (.+))?`)
	rePyException = regexp.MustCompile(`^([A-Za-z_][\w\.]*(?:Error|Exception|Warning|Exit|Interrupt|Fault|Iteration))(?::\s*(.*))?$`)

	reNodeFrame = regexp.MustCompile(`^\s*at\s+(?:(.*?)\s+\()?([^()\s]+):(\d+):(\d+)\)?\s*$`)
	reNodeError = regexp.MustCompile(`^([A-Za-z_][\w]*(?:Error|Exception))(?::\s*(.*))?$`)

	rePHPFrame = regexp.MustCompile(`^#\d+\s+([^\(]+)\((\d+)\):\s*(.*)$`)
	rePHPError = regexp.MustCompile(`PHP (?:Fatal error|Parse error|Warning|Notice|Recoverable fatal error):\s*(?:Uncaught\s+)?([\w\\]+)?:?\s*(.*)$`)

	reEndpoint  = regexp.MustCompile(`(?i)\b(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(/[A-Za-z0-9_\-/{}.]*)`)
	rePathLike  = regexp.MustCompile(`"(/(?:api|v\d|rest)[A-Za-z0-9_\-/{}.]*)"`)
	reHostPort  = regexp.MustCompile(`(?i)\b([a-z][a-z0-9\-]*(?:\.[a-z0-9\-]+)*):(\d{2,5})\b`)
	reTraceID   = regexp.MustCompile(`(?i)trace[_\-]?id["'\s:=]+([0-9a-fA-F\-]{8,})`)
	reSensitive = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|authorization|access[_-]?key)\b\s*[:=]\s*["']?([^\s"',;]+)`)
	reLogNoise  = regexp.MustCompile(`^\s*(at\s|goroutine\s+\d+\s+\[|\.{3}\s+\d+\s+more|File "|#\d+\s|\t)`)
)

// stackFrameDraft 解析中的中间帧结构。
type stackFrameDraft struct {
	raw    string
	pkg    string
	class  string
	method string
	file   string
	line   int
}

// parseStacktrace 解析堆栈文本，返回结构化结果与置信度。
//
// languageHint 为空时自动探测语言；解析不到任何帧时仍返回带 CleanedLog 的结果（降级不失败）。
func parseStacktrace(text, languageHint string) (domain.StackAnalysis, float64) {
	cleaned := normalizeStackText(text)
	lang := detectStackLanguage(cleaned, languageHint)
	out := domain.StackAnalysis{Language: lang, Category: "unknown"}

	var frames []stackFrameDraft
	var exceptionType, message, thread string
	var causedBy []string

	switch lang {
	case "java":
		frames, exceptionType, message, thread, causedBy = parseJavaStack(cleaned)
	case "go":
		frames, exceptionType, message, thread = parseGoStack(cleaned)
	case "python":
		frames, exceptionType, message = parsePythonStack(cleaned)
	case "node":
		frames, exceptionType, message = parseNodeStack(cleaned)
	case "php":
		frames, exceptionType, message = parsePHPStack(cleaned)
	default:
		// 未知语言：退化为“扫描所有已知帧格式”，尽可能提取线索。
		frames, exceptionType, message = parseMixedStack(cleaned)
	}

	out.ExceptionType = exceptionType
	out.Message = strings.TrimSpace(message)
	out.Thread = thread
	out.CausedBy = causedBy
	out.Category = classifyStack(exceptionType, out.Message, cleaned)
	out.CleanedLog = cleanStackLog(cleaned)
	out.TraceID = firstMatch(reTraceID, cleaned)
	out.Endpoints = extractEndpoints(cleaned)
	out.Services = extractServices(cleaned)

	seenFile := map[string]bool{}
	seenSymbol := map[string]bool{}
	for _, f := range frames {
		frame := domain.StackFrame{
			Raw:        f.raw,
			Package:    f.pkg,
			Class:      f.class,
			Method:     f.method,
			File:       f.file,
			Line:       f.line,
			Language:   lang,
			Confidence: frameConfidence(f),
		}
		out.Frames = append(out.Frames, frame)

		if f.file != "" && !seenFile[f.file] {
			seenFile[f.file] = true
			out.FileHints = append(out.FileHints, f.file)
		}
		// 包名 → 目录线索（com.acme.order → com/acme/order）
		if f.pkg != "" && strings.Contains(f.pkg, ".") {
			p := strings.ReplaceAll(f.pkg, ".", "/")
			if !seenFile[p] {
				seenFile[p] = true
				out.FileHints = append(out.FileHints, p)
			}
		}
		for _, sym := range []string{f.class, f.method} {
			if sym != "" && !seenSymbol[sym] {
				seenSymbol[sym] = true
				out.Symbols = append(out.Symbols, sym)
			}
		}
		if f.class != "" && f.method != "" {
			joined := f.class + "." + f.method
			if !seenSymbol[joined] {
				seenSymbol[joined] = true
				out.Symbols = append(out.Symbols, joined)
			}
		}
	}
	if out.Frames == nil {
		out.Frames = []domain.StackFrame{}
	}
	if out.FileHints == nil {
		out.FileHints = []string{}
	}
	if out.Symbols == nil {
		out.Symbols = []string{}
	}
	out.FileHints = capStrings(out.FileHints, 20)
	out.Symbols = capStrings(out.Symbols, 30)

	return out, stackConfidence(out)
}

// normalizeStackText 归一化换行与制表符，避免 CRLF 干扰行匹配。
func normalizeStackText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

// detectStackLanguage 依据特征行探测语言，languageHint 优先。
func detectStackLanguage(text, hint string) string {
	switch strings.ToLower(strings.TrimSpace(hint)) {
	case "java":
		return "java"
	case "go", "golang":
		return "go"
	case "python", "py":
		return "python"
	case "node", "nodejs", "js", "javascript", "ts", "typescript":
		return "node"
	case "php":
		return "php"
	}
	switch {
	case strings.Contains(text, "\tat ") || reJavaCausedBy.MatchString(text) ||
		strings.Contains(text, "Exception in thread"):
		return "java"
	case strings.Contains(text, "goroutine ") && strings.Contains(text, ".go:"):
		return "go"
	case strings.Contains(text, "Traceback (most recent call last)"):
		return "python"
	case rePHPFrame.MatchString(text) || strings.Contains(text, "PHP Fatal error"):
		return "php"
	case strings.Contains(text, "\n    at ") || strings.HasPrefix(text, "    at ") ||
		strings.Contains(text, "node_modules") || strings.Contains(text, ".js:") || strings.Contains(text, ".ts:"):
		return "node"
	default:
		return "unknown"
	}
}

// parseJavaStack 解析 Java 堆栈。
func parseJavaStack(text string) (frames []stackFrameDraft, exceptionType, message, thread string, causedBy []string) {
	for _, line := range strings.Split(text, "\n") {
		if m := reJavaFrame.FindStringSubmatch(line); m != nil {
			f := stackFrameDraft{raw: strings.TrimSpace(line)}
			f.pkg, f.class, f.method = splitJavaSymbol(m[1])
			if fm := reFileLine.FindStringSubmatch(m[2]); fm != nil {
				f.file = strings.TrimSpace(fm[1])
				if n, ok := atoiSafe(fm[2]); ok {
					f.line = n
				}
			} else if loc := strings.TrimSpace(m[2]); loc != "" && loc != "Native Method" {
				f.file = loc
			}
			frames = append(frames, f)
			continue
		}
		if m := reJavaThread.FindStringSubmatch(line); m != nil {
			thread = m[1]
			if exceptionType == "" {
				exceptionType = m[2]
				message = m[3]
			}
			continue
		}
		if m := reJavaCausedBy.FindStringSubmatch(line); m != nil {
			causedBy = append(causedBy, strings.TrimSpace(strings.TrimPrefix(line, "Caused by:")))
			if exceptionType == "" {
				exceptionType = m[1]
				message = m[2]
			}
			continue
		}
		if exceptionType == "" {
			if m := reJavaTypeLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				exceptionType = m[1]
				message = m[2]
			}
		}
	}
	return frames, exceptionType, message, thread, causedBy
}

// splitJavaSymbol 拆分 com.acme.order.OrderService.setStatus 为包名/类名/方法名。
func splitJavaSymbol(symbol string) (pkg, class, method string) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", "", ""
	}
	parts := strings.Split(symbol, ".")
	method = parts[len(parts)-1]
	rest := parts[:len(parts)-1]
	if len(rest) == 0 {
		return "", "", method
	}
	class = rest[len(rest)-1]
	if idx := strings.Index(class, "$"); idx > 0 {
		// 内部类保留外层类名，便于与仓库键匹配。
		class = class[:idx]
	}
	if len(rest) > 1 {
		pkg = strings.Join(rest[:len(rest)-1], ".")
	}
	return pkg, class, method
}

// parseGoStack 解析 Go panic 堆栈。
func parseGoStack(text string) (frames []stackFrameDraft, exceptionType, message, thread string) {
	lines := strings.Split(text, "\n")
	thread = "goroutine"
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := reGoGoroutine.FindStringSubmatch(line); m != nil {
			thread = "goroutine[" + m[1] + "]"
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "panic:") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "panic:"))
			if exceptionType == "" {
				exceptionType, message = splitGoPanic(rest)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[signal ") && exceptionType == "" {
			exceptionType = "runtime.Signal"
			message = trimmed
			continue
		}
		if strings.HasPrefix(trimmed, "goroutine ") || strings.HasPrefix(trimmed, "created by ") ||
			strings.HasPrefix(trimmed, "exit status ") {
			continue
		}
		// 帧：函数行 + 紧随其后的 file.go:line 行
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			if m := reGoFunc.FindStringSubmatch(trimmed); m != nil {
				if i+1 < len(lines) {
					if fm := reGoFile.FindStringSubmatch(lines[i+1]); fm != nil {
						pkg, class, method := splitGoSymbol(m[1])
						f := stackFrameDraft{raw: trimmed + " " + strings.TrimSpace(lines[i+1]),
							pkg: pkg, class: class, method: method, file: strings.TrimSpace(fm[1])}
						if n, ok := atoiSafe(fm[2]); ok {
							f.line = n
						}
						frames = append(frames, f)
						i++
						continue
					}
				}
			}
		}
	}
	return frames, exceptionType, message, thread
}

// splitGoPanic 拆分 panic 文本为异常类型与消息。
func splitGoPanic(rest string) (string, string) {
	if idx := strings.Index(rest, ":"); idx > 0 {
		head := strings.TrimSpace(rest[:idx])
		if strings.Contains(head, "runtime error") || strings.Contains(head, "runtime.") {
			return head, strings.TrimSpace(rest[idx+1:])
		}
	}
	if strings.HasPrefix(rest, "runtime error") {
		return "runtime.Error", rest
	}
	parts := strings.SplitN(rest, " ", 2)
	return parts[0], rest
}

// splitGoSymbol 拆分 Go 函数名（含方法接收者）。
func splitGoSymbol(symbol string) (pkg, class, method string) {
	symbol = strings.TrimSpace(symbol)
	if idx := strings.Index(symbol, ".("); idx > 0 {
		// github.com/acme/order.(*Service).SetStatus
		pkg = symbol[:idx]
		rest := symbol[idx+2:]
		if end := strings.Index(rest, ")"); end > 0 {
			class = strings.TrimPrefix(strings.TrimSpace(rest[:end]), "*")
			if len(rest) > end+1 {
				method = strings.TrimPrefix(rest[end+1:], ".")
			}
			return pkg, class, method
		}
	}
	if idx := strings.LastIndex(symbol, "."); idx > 0 {
		return symbol[:idx], "", symbol[idx+1:]
	}
	return "", "", symbol
}

// parsePythonStack 解析 Python traceback。
func parsePythonStack(text string) (frames []stackFrameDraft, exceptionType, message string) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if m := rePyFile.FindStringSubmatch(line); m != nil {
			f := stackFrameDraft{raw: strings.TrimSpace(line), file: m[1]}
			if n, ok := atoiSafe(m[2]); ok {
				f.line = n
			}
			if len(m) > 3 {
				f.method = strings.TrimSpace(m[3])
			}
			f.pkg, f.class = pythonModuleOf(m[1])
			frames = append(frames, f)
		}
	}
	// 异常类型取最后一行匹配（traceback 最末行才是真正的异常）。
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if m := rePyException.FindStringSubmatch(trimmed); m != nil {
			exceptionType = m[1]
			message = m[2]
		}
	}
	return frames, exceptionType, message
}

// pythonModuleOf 由文件路径推导包名与模块名。
func pythonModuleOf(path string) (pkg, class string) {
	p := strings.ReplaceAll(path, "\\", "/")
	dir := ""
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		dir = p[:idx]
		class = strings.TrimSuffix(p[idx+1:], ".py")
	} else {
		class = strings.TrimSuffix(p, ".py")
	}
	dir = strings.TrimPrefix(dir, "/")
	pkg = strings.ReplaceAll(strings.TrimSuffix(dir, "/"), "/", ".")
	return pkg, class
}

// parseNodeStack 解析 Node/TS/JS 堆栈。
func parseNodeStack(text string) (frames []stackFrameDraft, exceptionType, message string) {
	for _, line := range strings.Split(text, "\n") {
		if m := reNodeFrame.FindStringSubmatch(line); m != nil {
			f := stackFrameDraft{raw: strings.TrimSpace(line), file: m[2]}
			if n, ok := atoiSafe(m[3]); ok {
				f.line = n
			}
			f.class, f.method = splitNodeSymbol(m[1])
			if idx := strings.LastIndex(strings.ReplaceAll(m[2], "\\", "/"), "/"); idx > 0 {
				f.pkg = strings.ReplaceAll(strings.ReplaceAll(m[2][:idx], "\\", "/"), "/", ".")
				f.pkg = strings.TrimPrefix(f.pkg, ".")
			}
			frames = append(frames, f)
			continue
		}
		if exceptionType == "" {
			if m := reNodeError.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				exceptionType = m[1]
				message = m[2]
			}
		}
	}
	return frames, exceptionType, message
}

// splitNodeSymbol 拆分 Node 帧函数名，例如 OrderService.update。
func splitNodeSymbol(symbol string) (class, method string) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", ""
	}
	// 去掉 async / new 前缀
	symbol = strings.TrimPrefix(symbol, "async ")
	symbol = strings.TrimPrefix(symbol, "new ")
	if idx := strings.LastIndex(symbol, "."); idx > 0 {
		return symbol[:idx], symbol[idx+1:]
	}
	return "", symbol
}

// parsePHPStack 解析 PHP 堆栈。
func parsePHPStack(text string) (frames []stackFrameDraft, exceptionType, message string) {
	for _, line := range strings.Split(text, "\n") {
		if m := rePHPFrame.FindStringSubmatch(line); m != nil {
			f := stackFrameDraft{raw: strings.TrimSpace(line), file: strings.TrimSpace(m[1])}
			if n, ok := atoiSafe(m[2]); ok {
				f.line = n
			}
			call := strings.TrimSpace(m[3])
			if idx := strings.Index(call, "->"); idx > 0 {
				f.class = strings.TrimSpace(call[:idx])
				rest := call[idx+2:]
				if p := strings.Index(rest, "("); p > 0 {
					f.method = strings.TrimSpace(rest[:p])
				} else {
					f.method = strings.TrimSpace(rest)
				}
			} else if idx := strings.Index(call, "::"); idx > 0 {
				f.class = strings.TrimSpace(call[:idx])
				f.method = strings.TrimSpace(strings.SplitN(call[idx+2:], "(", 2)[0])
			}
			frames = append(frames, f)
			continue
		}
		if m := rePHPError.FindStringSubmatch(line); m != nil {
			exceptionType = m[1]
			message = m[2]
			if idx := strings.Index(message, " in /"); idx > 0 {
				tail := message[idx+4:]
				if fm := reFileLine.FindStringSubmatch(tail); fm != nil && len(frames) == 0 {
					ln := 0
					if n, ok := atoiSafe(fm[2]); ok {
						ln = n
					}
					frames = append(frames, stackFrameDraft{raw: strings.TrimSpace(line), file: strings.TrimSpace(fm[1]), line: ln})
				}
				message = strings.TrimSpace(message[:idx])
			}
		}
	}
	return frames, exceptionType, message
}

// parseMixedStack 语言未知时扫描所有已知帧格式。
func parseMixedStack(text string) (frames []stackFrameDraft, exceptionType, message string) {
	if f, et, msg, _, _ := parseJavaStack(text); len(f) > 0 {
		return f, et, msg
	}
	if f, et, msg, _ := parseGoStack(text); len(f) > 0 {
		return f, et, msg
	}
	if f, et, msg := parsePythonStack(text); len(f) > 0 {
		return f, et, msg
	}
	if f, et, msg := parseNodeStack(text); len(f) > 0 {
		return f, et, msg
	}
	if f, et, msg := parsePHPStack(text); len(f) > 0 {
		return f, et, msg
	}
	return nil, "", ""
}

// frameConfidence 单帧置信度：信息越完整越高。
func frameConfidence(f stackFrameDraft) float64 {
	c := 0.4
	if f.class != "" || f.method != "" {
		c += 0.2
	}
	if f.file != "" {
		c += 0.2
	}
	if f.line > 0 {
		c += 0.15
	}
	if f.pkg != "" {
		c += 0.05
	}
	if c > 1 {
		c = 1
	}
	return c
}

// stackConfidence 整体解析置信度。
func stackConfidence(s domain.StackAnalysis) float64 {
	c := 0.2
	if len(s.Frames) > 0 {
		c += 0.35
		if len(s.Frames) > 3 {
			c += 0.1
		}
	}
	if s.ExceptionType != "" {
		c += 0.15
	}
	if s.Category != "" && s.Category != "unknown" {
		c += 0.15
	}
	if len(s.FileHints) > 0 {
		c += 0.05
	}
	if c > 0.95 {
		c = 0.95
	}
	return round2(c)
}

// classifyStack 依据异常类型/消息/原文判定异常分类。
//
// 覆盖契约要求的 10 类：null_pointer / index_out_of_bounds / type_error /
// class_not_found / dependency_missing / connection_refused / timeout / syntax_error /
// oom / unknown（另补充 concurrent_modification 供修复技能识别）。
func classifyStack(exceptionType, message, raw string) string {
	primary := strings.ToLower(exceptionType + " " + message)
	if c := classifyByKeywords(primary); c != "" {
		return c
	}
	if c := classifyByKeywords(strings.ToLower(raw)); c != "" {
		return c
	}
	return "unknown"
}

// classifyByKeywords 关键字规则表（顺序即优先级）。
func classifyByKeywords(text string) string {
	rules := []struct {
		category string
		keywords []string
	}{
		{"oom", []string{"outofmemoryerror", "out of memory", "oomkilled", "cannot allocate memory", "memoryerror", "bad_alloc", "heap out of memory"}},
		{"connection_refused", []string{"connection refused", "econnrefused", "connect: connection refused", "connection reset by peer"}},
		{"timeout", []string{"timeout", "timed out", "etimedout", "deadline exceeded", "sockettimeout", "read timed out", "request timeout"}},
		{"syntax_error", []string{"syntaxerror", "parseexception", "invalid syntax", "unexpected token", "unexpected end of", "compilation failed", "compile error", "cannot parse"}},
		{"null_pointer", []string{"nullpointerexception", "nil pointer", "null pointer", "cannot invoke", "is null", " on null", "nonetype", "null object reference", "attempt to invoke virtual method", "call to a member function", "nullreferenceexception", "undefined is not an object"}},
		{"index_out_of_bounds", []string{"arrayindexoutofbounds", "indexerror", "index out of range", "stringindexoutofbounds", "out of bounds", "list index out of range", "indexoutofboundsexception"}},
		{"dependency_missing", []string{"could not resolve dependencies", "unable to resolve dependency", "cannot find package", "no required module provides package", "module not found", "npm err! 404", "no module named", "cannot find module", "artifactnotfound", "unresolved dependency", "dependency resolution"}},
		{"class_not_found", []string{"noclassdeffound", "classnotfound", "class not found", "cannot find symbol", "nosuchmethoderror", "nosuchbeandefinition", "unresolved reference"}},
		{"concurrent_modification", []string{"concurrentmodificationexception", "concurrent modification", "fatal error: concurrent map"}},
		{"type_error", []string{"typeerror", "cannot read properties of undefined", "cannot read property", "is not a function", "classcastexception", "cannot be cast", "type assertion", "unsupported operand type", "cannot convert", "invalid type", "attributeerror"}},
	}
	for _, rule := range rules {
		for _, kw := range rule.keywords {
			if strings.Contains(text, kw) {
				return rule.category
			}
		}
	}
	return ""
}

// cleanStackLog 生成清洗后的日志：去噪、脱敏、限长。
func cleanStackLog(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if reLogNoise.MatchString(line) {
			continue
		}
		kept = append(kept, reSensitive.ReplaceAllString(line, "$1=***"))
	}
	if len(kept) > 40 {
		kept = kept[:40]
	}
	out := strings.Join(kept, "\n")
	if len(out) > 4000 {
		out = out[:4000]
	}
	return out
}

// extractEndpoints 提取日志/堆栈中出现的 HTTP 路径。
func extractEndpoints(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range reEndpoint.FindAllStringSubmatch(text, -1) {
		ep := m[1]
		if !seen[ep] {
			seen[ep] = true
			out = append(out, ep)
		}
	}
	for _, m := range rePathLike.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return capStrings(out, 10)
}

// extractServices 提取 host:port 形式的服务名与域名。
func extractServices(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range reHostPort.FindAllStringSubmatch(text, -1) {
		host := m[1]
		if len(host) < 3 || seen[host] {
			continue
		}
		if strings.Contains(host, ".jar") || strings.Contains(host, "0.0.0") {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	sort.Strings(out)
	return capStrings(out, 10)
}

// firstMatch 返回正则首个捕获组。
func firstMatch(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); m != nil && len(m) > 1 {
		return m[1]
	}
	return ""
}

// capStrings 截断字符串数组。
func capStrings(in []string, limit int) []string {
	if len(in) <= limit {
		return in
	}
	return in[:limit]
}

// round2 保留两位小数，保证输出确定性。
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// handleStacktraceParse stacktrace_parse 技能处理器。
//
// 输出：stack（domain.StackAnalysis 的 map 形态）、language、category、confidence。
func handleStacktraceParse(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stacktrace, _ := in["stacktrace"].(string)
	logs, _ := in["logs"].(string)
	langHint, _ := in["language"].(string)
	if strings.TrimSpace(stacktrace) == "" {
		stacktrace = logs
	}
	// 语言探测需要结合日志上下文，避免只有 logs 时判不出语言。
	text := strings.TrimSpace(stacktrace)
	if logs != "" {
		text = text + "\n" + logs
	}
	if text == "" {
		return nil, fmt.Errorf("stacktrace 与 logs 均为空，无法解析")
	}

	analysis, confidence := parseStacktrace(text, langHint)
	output := structToMap(analysis)
	return normalizeOutput(map[string]any{
		"stack":      output,
		"language":   analysis.Language,
		"category":   analysis.Category,
		"confidence": confidence,
	}), nil
}

// structToMap 把结构体转成 map[string]any（走 JSON 编解码，保证字段名与 domain 标签一致）。
func structToMap(v any) map[string]any {
	out := jsonRoundTrip[map[string]any](v)
	if out == nil {
		return map[string]any{}
	}
	return out
}
