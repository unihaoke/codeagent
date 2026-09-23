package builtin

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// 符号提取正则（覆盖 Go / Java / Python / JS-TS）。
var (
	reSymGoFunc   = regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(`)
	reSymGoType   = regexp.MustCompile(`^\s*type\s+([A-Za-z_]\w*)\s+(?:struct|interface)\b`)
	reSymJavaType = regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+)?(?:final\s+|abstract\s+|static\s+)*(?:class|interface|enum|record)\s+([A-Za-z_]\w*)`)
	reSymJavaFunc = regexp.MustCompile(`^\s*(?:public|private|protected|static|final|synchronized|abstract|native|default|\s)+[\w<>\[\],\.\?\s]+\s+([A-Za-z_]\w*)\s*\([^;{]*\)\s*(?:throws\s+[\w\s,\.]+)?\{?\s*$`)
	reSymPyDef    = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	reSymPyClass  = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	reSymJSFunc   = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(`)
	reSymJSArrow  = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`)
	reSymJSType   = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?(?:class|interface|enum)\s+([A-Za-z_$][\w$]*)`)
)

// 依赖/导入提取正则。
var (
	reImpJava   = regexp.MustCompile(`^\s*import\s+(?:static\s+)?([\w\.\*]+)\s*;`)
	reImpGo     = regexp.MustCompile(`^\s*(?:[\w\.]+\s+)?"([^"]+)"\s*$`)
	reImpPyFrom = regexp.MustCompile(`^\s*from\s+([\w\.]+)\s+import\s+`)
	reImpPy     = regexp.MustCompile(`^\s*import\s+([\w\.]+)`)
	reImpJS     = regexp.MustCompile(`(?:from\s*['"]([^'"]+)['"]|require\(\s*['"]([^'"]+)['"]\s*\))`)
	reImpC      = regexp.MustCompile(`^\s*#include\s*[<"]([^>"]+)[>"]`)
)

// 代码风险规则正则。
var (
	reAssignNullable = regexp.MustCompile(`(?:^|[^\w])([A-Za-z_]\w*)\s*(?::=|=)\s*[^;\n]*\.(?:get|find|findBy\w*|load|loadBy\w*|query|select|fetch|first|last|Read|Get|Do|Fetch|Find|Load|Query|Lookup)\w*\s*\(`)
	reNilGuard       = regexp.MustCompile(`(?i)\b(nil|null|None)\b`)
	reDeref          = regexp.MustCompile(`([A-Za-z_]\w*)\s*(?:\.|->)\s*[A-Za-z_]\w*`)
	reIndexAccess    = regexp.MustCompile(`([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*\[\s*([A-Za-z_]\w*)\s*\]`)
	reBoundsCheck    = regexp.MustCompile(`(?:len\s*\(|\.size\(\)|\.length\b|\.Length\b|\.size\b|<=\s*0|\bSize\(\)|<\s*0\s*\|\||>=\s*len)`)
	reTodo           = regexp.MustCompile(`(?i)\b(TODO|FIXME|XXX|HACK)\b`)
	reGoPanic        = regexp.MustCompile(`^\s*panic\s*\(`)
	reThrow          = regexp.MustCompile(`\bthrow\s+new\s+(?:RuntimeException|Error|IllegalStateException|IllegalArgumentException)\b`)
	reAcquire        = regexp.MustCompile(`(?:new\s+(?:FileInputStream|FileOutputStream|FileReader|FileWriter|BufferedReader|BufferedWriter|Socket|Scanner)|os\.Open\w*\(|http\.(?:Get|Post|Do)\(|open\s*\(|net\.Dial\w*\(|\.getConnection\(|urlopen\s*\()`)
	reRelease        = regexp.MustCompile(`(?:defer\s+\w+\.Close\(\)|\.Close\(\)|\.close\(\)|try\s*\(|with\s+open|finally|using\s*\()`)
	reDebugResidue   = regexp.MustCompile(`(?:fmt\.Print(?:ln|f)?\(|console\.log\(|System\.out\.print(?:ln)?\(|print\(|var_dump\(|dd\()`)
	reEmptyCatch     = regexp.MustCompile(`catch\s*\([^)]*\)\s*\{\s*\}|except[^:]*:\s*pass\b|catch\s*\{\s*\}`)
	reComment        = regexp.MustCompile(`^\s*(?://|#|\*|/\*)`)
)

// riskItem 代码风险条目。
type riskItem struct {
	RepositoryID string  `json:"repositoryId,omitempty"`
	RepoKey      string  `json:"repoKey,omitempty"`
	File         string  `json:"file"`
	Line         int     `json:"line"`
	Type         string  `json:"type"`
	Severity     string  `json:"severity"`
	Desc         string  `json:"desc"`
	Score        float64 `json:"score,omitempty"`
}

// handleCodeASTAnalyze code_ast_analyze 技能处理器。
//
// 输入：slices（代码切片数组，宽容反序列化）、focusLines（重点行号）。
// 输出：symbols / imports / risks / complexity。
func handleCodeASTAnalyze(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices := decodeSlices(in["slices"])
	focusLines := decodeInts(in["focusLines"])
	focus := map[int]bool{}
	for _, l := range focusLines {
		focus[l] = true
	}

	symSeen := map[string]bool{}
	impSeen := map[string]bool{}
	symbols := []map[string]any{}
	imports := []string{}
	risks := []riskItem{}

	totalLines := 0
	functions := 0
	maxNesting := 0
	for _, slice := range slices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lang := sliceLanguage(slice)
		lines := splitLines(slice.Content)
		totalLines += len(lines)

		for _, ref := range extractSymbols(lang, lines) {
			key := slice.Path + "|" + ref.Name
			if symSeen[key] {
				continue
			}
			symSeen[key] = true
			line := ref.Line
			if slice.StartLine > 0 && line > 0 {
				line = slice.StartLine + line - 1
			}
			// 输出为对象形态（path/symbol/line），与 pipeline 的符号合并逻辑对齐。
			symbols = append(symbols, map[string]any{
				"path":         slice.Path,
				"file":         slice.Path,
				"symbol":       ref.Name,
				"name":         ref.Name,
				"line":         line,
				"kind":         ref.Kind,
				"repositoryId": slice.RepositoryID,
				"repoKey":      slice.RepoKey,
			})
		}
		functions += countFunctions(lang, lines)
		if depth := maxIndentDepth(lang, lines); depth > maxNesting {
			maxNesting = depth
		}
		for _, imp := range extractImports(lang, slice, lines) {
			if !impSeen[imp] {
				impSeen[imp] = true
				imports = append(imports, imp)
			}
		}
		found := scanRisks(lang, lines, focus)
		for i := range found {
			found[i].RepositoryID = slice.RepositoryID
			found[i].RepoKey = slice.RepoKey
			if found[i].File == "" {
				found[i].File = slice.Path
			}
			// 切片偏移：StartLine 为切片在文件中的起始行号。
			if slice.StartLine > 0 && found[i].Line > 0 {
				found[i].Line = slice.StartLine + found[i].Line - 1
			}
		}
		risks = append(risks, found...)
	}

	sort.SliceStable(symbols, func(i, j int) bool {
		pi, _ := symbols[i]["path"].(string)
		pj, _ := symbols[j]["path"].(string)
		if pi != pj {
			return pi < pj
		}
		li, _ := symbols[i]["line"].(int)
		lj, _ := symbols[j]["line"].(int)
		if li != lj {
			return li < lj
		}
		si, _ := symbols[i]["symbol"].(string)
		sj, _ := symbols[j]["symbol"].(string)
		return si < sj
	})
	sort.Strings(imports)
	sort.SliceStable(risks, func(i, j int) bool {
		if risks[i].File != risks[j].File {
			return risks[i].File < risks[j].File
		}
		return risks[i].Line < risks[j].Line
	})

	avg := 0.0
	if len(slices) > 0 {
		avg = float64(totalLines) / float64(len(slices))
	}
	complexity := map[string]any{
		"files":        len(slices),
		"functions":    functions,
		"maxNesting":   maxNesting,
		"avgFileLines": round2(avg),
	}
	return normalizeOutput(map[string]any{
		"symbols":    symbols,
		"imports":    imports,
		"risks":      risksToAny(risks),
		"complexity": complexity,
	}), nil
}

// risksToAny 把风险数组转为可直接 JSON 序列化的结构。
func risksToAny(items []riskItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m := map[string]any{
			"file":     item.File,
			"line":     item.Line,
			"type":     item.Type,
			"severity": item.Severity,
			"desc":     item.Desc,
			// message 与 desc 同义，兼容 pipeline 的 pickString(m, "message", "detail", "reason")。
			"message": item.Desc,
		}
		if item.RepositoryID != "" {
			m["repositoryId"] = item.RepositoryID
		}
		if item.RepoKey != "" {
			m["repoKey"] = item.RepoKey
		}
		out = append(out, m)
	}
	return out
}

// sliceLanguage 推断代码切片语言（优先使用切片自带的 Language 字段）。
func sliceLanguage(slice domain.CodeSlice) string {
	if strings.TrimSpace(slice.Language) != "" {
		return normalizeLanguage(slice.Language)
	}
	return normalizeLanguage(languageByPath(slice.Path))
}

// normalizeLanguage 归一化语言名。
func normalizeLanguage(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "go", "golang":
		return "go"
	case "java", "kotlin":
		return "java"
	case "python", "py":
		return "python"
	case "ts", "typescript", "js", "javascript", "jsx", "tsx", "node":
		return "node"
	case "php":
		return "php"
	case "xml", "json", "yaml", "yml", "gradle", "toml", "properties", "text":
		return "manifest"
	default:
		return "text"
	}
}

// languageByPath 依据文件扩展名推断语言。
func languageByPath(path string) string {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	switch {
	case strings.HasSuffix(p, ".go"):
		return "go"
	case strings.HasSuffix(p, ".java"):
		return "java"
	case strings.HasSuffix(p, ".py"):
		return "python"
	case strings.HasSuffix(p, ".ts"), strings.HasSuffix(p, ".tsx"),
		strings.HasSuffix(p, ".js"), strings.HasSuffix(p, ".jsx"), strings.HasSuffix(p, ".mjs"):
		return "node"
	case strings.HasSuffix(p, ".php"):
		return "php"
	case strings.HasSuffix(p, ".xml"), strings.HasSuffix(p, ".json"), strings.HasSuffix(p, ".gradle"),
		strings.HasSuffix(p, ".mod"), strings.HasSuffix(p, ".txt"), strings.HasSuffix(p, ".toml"):
		return "manifest"
	default:
		return "text"
	}
}

// splitLines 按行拆分，保留原始行内容（含缩进与 CR）。
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// symbolRef 文件内符号引用（名称 + 行号 + 类别）。
type symbolRef struct {
	Name string
	Line int
	Kind string
}

// extractSymbols 提取文件内符号。
func extractSymbols(lang string, lines []string) []symbolRef {
	out := []symbolRef{}
	for i, line := range lines {
		lineNo := i + 1
		switch lang {
		case "go":
			if m := reSymGoFunc.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "function"})
			}
			if m := reSymGoType.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "type"})
			}
		case "java":
			if m := reSymJavaType.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "type"})
			}
			if m := reSymJavaFunc.FindStringSubmatch(line); m != nil && !isJavaKeyword(m[1]) {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "method"})
			}
		case "python":
			if m := reSymPyDef.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "function"})
			}
			if m := reSymPyClass.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "type"})
			}
		case "node":
			if m := reSymJSFunc.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "function"})
			}
			if m := reSymJSArrow.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "function"})
			}
			if m := reSymJSType.FindStringSubmatch(line); m != nil {
				out = append(out, symbolRef{Name: m[1], Line: lineNo, Kind: "type"})
			}
		}
	}
	return out
}

// isJavaKeyword 过滤被误识别为方法名的控制关键字。
func isJavaKeyword(name string) bool {
	switch name {
	case "if", "for", "while", "switch", "catch", "synchronized", "return", "new", "try":
		return true
	default:
		return false
	}
}

// countFunctions 近似统计函数数量。
func countFunctions(lang string, lines []string) int {
	n := 0
	for _, line := range lines {
		switch lang {
		case "go":
			if reSymGoFunc.MatchString(line) {
				n++
			}
		case "java":
			if reSymJavaFunc.MatchString(line) {
				n++
			}
		case "python":
			if reSymPyDef.MatchString(line) {
				n++
			}
		case "node":
			if reSymJSFunc.MatchString(line) || reSymJSArrow.MatchString(line) {
				n++
			}
		}
	}
	return n
}

// maxIndentDepth 用缩进/括号深度近似最大嵌套层级。
func maxIndentDepth(lang string, lines []string) int {
	max := 0
	if lang == "python" {
		for _, line := range lines {
			trimmed := strings.TrimRight(line, " \t")
			if strings.TrimSpace(trimmed) == "" {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			depth := indent / 4
			if depth > max {
				max = depth
			}
		}
		return max
	}
	depth := 0
	for _, line := range lines {
		code := stripStringLiterals(line)
		depth += strings.Count(code, "{")
		if depth > max {
			max = depth
		}
		depth -= strings.Count(code, "}")
		if depth < 0 {
			depth = 0
		}
	}
	return max
}

// stripStringLiterals 去掉字符串与注释内容，避免括号统计被干扰。
func stripStringLiterals(line string) string {
	var b strings.Builder
	inSingle, inDouble, inBacktick := false, false, false
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case ch == '\'' && !inDouble && !inBacktick:
			inSingle = !inSingle
		case ch == '"' && !inSingle && !inBacktick:
			inDouble = !inDouble
		case ch == '`' && !inSingle && !inDouble:
			inBacktick = !inBacktick
		case ch == '/' && !inSingle && !inDouble && !inBacktick && i+1 < len(runes) && runes[i+1] == '/':
			return b.String()
		case ch == '#' && !inSingle && !inDouble && !inBacktick && (i == 0 || runes[i-1] == ' ' || runes[i-1] == '\t'):
			return b.String()
		}
		if !inSingle && !inDouble && !inBacktick {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// extractImports 提取 import/require 依赖。
func extractImports(lang string, slice domain.CodeSlice, lines []string) []string {
	out := []string{}
	if len(slice.Imports) > 0 {
		out = append(out, slice.Imports...)
	}
	for _, line := range lines {
		switch lang {
		case "java":
			if m := reImpJava.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		case "go":
			if m := reImpGo.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		case "python":
			if m := reImpPyFrom.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			} else if m := reImpPy.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		case "node":
			for _, m := range reImpJS.FindAllStringSubmatch(line, -1) {
				if m[1] != "" {
					out = append(out, m[1])
				} else if m[2] != "" {
					out = append(out, m[2])
				}
			}
		default:
			if m := reImpC.FindStringSubmatch(line); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// scanRisks 按规则扫描代码风险（行号从 1 开始）。
func scanRisks(lang string, lines []string, focus map[int]bool) []riskItem {
	out := []riskItem{}
	for i, raw := range lines {
		line := raw
		lineNo := i + 1
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		code := stripStringLiterals(line)

		// TODO/FIXME 标记
		if m := reTodo.FindStringSubmatch(code); m != nil && !reComment.MatchString(trimmed) {
			out = append(out, riskItem{Line: lineNo, Type: "todo", Severity: string(domain.SeverityInfo),
				Desc: "存在 " + strings.ToUpper(m[1]) + " 未完成标记，可能遗留未处理逻辑"})
		}
		// 调试残留
		if reDebugResidue.MatchString(code) {
			out = append(out, riskItem{Line: lineNo, Type: "debug_residue", Severity: string(domain.SeverityMinor),
				Desc: "疑似调试输出残留（print/console.log），上线前应清理或改为结构化日志"})
		}
		// 空捕获
		if reEmptyCatch.MatchString(code) {
			out = append(out, riskItem{Line: lineNo, Type: "empty_catch", Severity: string(domain.SeverityMajor),
				Desc: "空异常捕获吞掉了错误，异常无法被发现，建议记录日志或重新抛出"})
		}
		// 裸 panic / throw
		if lang == "go" && reGoPanic.MatchString(code) {
			out = append(out, riskItem{Line: lineNo, Type: "panic", Severity: string(domain.SeverityMajor),
				Desc: "裸 panic 会中断整个请求链路，建议返回 error 并由上层统一处理"})
		}
		if (lang == "java" || lang == "node") && reThrow.MatchString(code) {
			out = append(out, riskItem{Line: lineNo, Type: "panic", Severity: string(domain.SeverityMinor),
				Desc: "直接抛出通用异常缺少上下文信息，建议携带业务参数与原始异常原因"})
		}
		// 空指针风险：来自查询类调用的变量在未判空的情况下被解引用
		if m := reAssignNullable.FindStringSubmatch(code); m != nil {
			varName := m[1]
			if !hasNilGuardAround(lines, i, varName) && hasDerefWithin(lines, i, varName, 6) {
				out = append(out, riskItem{Line: lineNo, Type: "nil_deref", Severity: string(domain.SeverityCritical),
					Desc: "变量 " + varName + " 来自可能返回空值的查询调用，后续解引用前缺少判空保护，存在空指针风险"})
			}
		}
		// 数组下标越界风险
		if m := reIndexAccess.FindStringSubmatch(code); m != nil && !isTypeDeclaration(code) {
			if !reBoundsCheck.MatchString(code) && !hasBoundsCheckAround(lines, i) {
				out = append(out, riskItem{Line: lineNo, Type: "index_unchecked", Severity: string(domain.SeverityMajor),
					Desc: "下标 " + m[2] + " 访问 " + m[1] + " 前未见边界校验，存在越界风险"})
			}
		}
		// 资源泄漏风险
		if reAcquire.MatchString(code) && !reRelease.MatchString(code) && !hasReleaseAround(lines, i) {
			out = append(out, riskItem{Line: lineNo, Type: "resource_leak", Severity: string(domain.SeverityMajor),
				Desc: "资源获取后未见关闭/释放动作（defer close / try-with-resources / with），存在句柄泄漏风险"})
		}
		if focus[lineNo] {
			// 故障重点行：提升已有风险的语义（此处仅用于标记，避免改变类型）。
			if len(out) > 0 && out[len(out)-1].Line == lineNo {
				out[len(out)-1].Desc += "（该行为任务重点行）"
			}
		}
	}
	return out
}

// hasNilGuardAround 判断变量在附近是否已有判空保护。
func hasNilGuardAround(lines []string, idx int, varName string) bool {
	start := idx - 8
	if start < 0 {
		start = 0
	}
	end := idx + 3
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		line := lines[i]
		if !strings.Contains(line, varName) {
			continue
		}
		if strings.Contains(line, "Optional") && idx != i {
			return true
		}
		if reNilGuard.MatchString(line) && (strings.Contains(line, "if") || strings.Contains(line, "assert") ||
			strings.Contains(line, "require") || strings.Contains(line, "Objects.")) {
			return true
		}
	}
	// 变量本身就在判空语句中定义（如 if (x != null) {）同样视为保护。
	return false
}

// hasDerefWithin 判断变量在随后若干行内是否被解引用。
func hasDerefWithin(lines []string, idx int, varName string, window int) bool {
	end := idx + window
	if end > len(lines) {
		end = len(lines)
	}
	for i := idx + 1; i < end; i++ {
		code := stripStringLiterals(lines[i])
		if strings.Contains(code, varName+".") || strings.Contains(code, varName+"->") {
			return true
		}
	}
	return false
}

// hasBoundsCheckAround 判断附近是否存在边界校验。
func hasBoundsCheckAround(lines []string, idx int) bool {
	start := idx - 6
	if start < 0 {
		start = 0
	}
	end := idx + 2
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		if reBoundsCheck.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// hasReleaseAround 判断附近是否存在资源释放动作。
func hasReleaseAround(lines []string, idx int) bool {
	start := idx - 2
	if start < 0 {
		start = 0
	}
	end := idx + 10
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		if reRelease.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// isTypeDeclaration 过滤数组类型声明等非访问语句。
func isTypeDeclaration(code string) bool {
	trimmed := strings.TrimSpace(code)
	if strings.Contains(trimmed, "[]") || strings.Contains(trimmed, "map[") {
		return true
	}
	if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
		return true
	}
	return false
}
