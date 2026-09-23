package builtin

// 本文件实现 patch_verify 技能：**纯静态**校验（不执行任何命令、不写文件系统）。
//
// 校验维度：
//  1. snippet_integrity：oldSnippet 非空且看起来是完整语法单元；
//  2. balance：newSnippet 的括号/引号配平（忽略字符串与注释）；
//  3. static_syntax：语言语法哨兵（Go 未使用 import/变量、Java 分号与大括号、
//     Python 缩进一致性、TS/JS 花括号配平）；
//  4. danger_scan：危险操作扫描（rm -rf / DROP TABLE / System.exit / os.Exit /
//     process.exit / while(true) / --force）；
//  5. scale：变更规模（新增行数 > 60 或多文件 > 5 视为高风险）。

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// 危险操作模式（命中即判定补丁存在高风险副作用）。
var dangerPatterns = []struct {
	pattern string
	desc    string
}{
	{"rm -rf", "包含递归强制删除命令"},
	{"DROP TABLE", "包含删除数据表的高危 SQL"},
	{"System.exit", "包含 JVM 强制退出调用，会直接杀死服务进程"},
	{"os.Exit", "包含进程强制退出调用"},
	{"process.exit", "包含 Node 进程强制退出调用"},
	{"while(true)", "引入无出口死循环"},
	{"while (true)", "引入无出口死循环"},
	{"--force", "包含强制覆盖参数，可能破坏目标环境"},
}

// Go / Java 语法哨兵正则。
var (
	reGoImportLine  = regexp.MustCompile(`^\s*import\s+(?:[\w\.]+\s+)?"([^"]+)"`)
	reGoImportInBlk = regexp.MustCompile(`^\s*(?:[\w\.]+\s+)?"([^"]+)"\s*$`)
	reGoShortDecl   = regexp.MustCompile(`^\s*([a-zA-Z_]\w*)\s*:=\s*`)
)

// verifyPatch 校验用补丁视图。
//
// 同时兼容补丁描述结构（oldSnippet/newSnippet/filePath，由 code_patch_synthesize 产出）
// 与 domain.Patch（oldContent/newContent/filePath）两种形态。
type verifyPatch struct {
	RepositoryID string
	RepoKey      string
	FilePath     string
	Action       string
	OldSnippet   string
	NewSnippet   string
	Risk         string
}

// decodeVerifyPatches 宽容解析待校验补丁。
func decodeVerifyPatches(v any) []verifyPatch {
	raw := jsonRoundTrip[[]map[string]any](v)
	if raw == nil {
		if one := jsonRoundTrip[map[string]any](v); one != nil {
			raw = []map[string]any{one}
		}
	}
	out := make([]verifyPatch, 0, len(raw))
	for _, m := range raw {
		p := verifyPatch{
			RepositoryID: stringValue(m["repositoryId"]),
			RepoKey:      stringValue(m["repoKey"]),
			FilePath:     firstNonEmpty(stringValue(m["filePath"]), stringValue(m["path"])),
			Action:       stringValue(m["action"]),
			OldSnippet:   firstNonEmpty(stringValue(m["oldSnippet"]), stringValue(m["oldContent"])),
			NewSnippet:   firstNonEmpty(stringValue(m["newSnippet"]), stringValue(m["newContent"])),
			Risk:         stringValue(m["risk"]),
		}
		if p.FilePath == "" && p.OldSnippet == "" && p.NewSnippet == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// firstNonEmpty 返回首个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// verifyIssue 校验问题收集器。
type verifyIssue struct {
	msgs []string
}

// add 追加问题（最多保留 20 条，避免输出过长）。
func (v *verifyIssue) add(format string, args ...any) {
	if len(v.msgs) >= 20 {
		return
	}
	v.msgs = append(v.msgs, fmt.Sprintf(format, args...))
}

// ok 是否无问题。
func (v *verifyIssue) ok() bool { return len(v.msgs) == 0 }

// text 生成校验输出文本。
func (v *verifyIssue) text(okText string) string {
	if v.ok() {
		return okText
	}
	return strings.Join(v.msgs, "；")
}

// handlePatchVerify patch_verify 技能处理器。
//
// 输入：patches（补丁数组）、language（语言）。
// 输出：checks（[]domain.VerificationCheck）、passed、score。
func handlePatchVerify(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	patches := decodeVerifyPatches(in["patches"])
	lang := normalizeLanguage(stringValue(in["language"]))

	checks := make([]domain.VerificationCheck, 0, 5)
	if len(patches) == 0 {
		checks = append(checks,
			domain.VerificationCheck{Name: "snippet_integrity", Passed: true, Skipped: true,
				SkipReason: "补丁列表为空，无需校验片段完整性", Output: "跳过"},
			domain.VerificationCheck{Name: "balance", Passed: true, Skipped: true,
				SkipReason: "补丁列表为空，无需校验配平", Output: "跳过"},
			domain.VerificationCheck{Name: "static_syntax", Passed: true, Skipped: true,
				SkipReason: "补丁列表为空，无需校验语法", Output: "跳过"},
			domain.VerificationCheck{Name: "danger_scan", Passed: true, Skipped: true,
				SkipReason: "补丁列表为空，无危险操作", Output: "跳过"},
			domain.VerificationCheck{Name: "scale", Passed: true, Output: "无变更，规模校验通过"},
		)
		return normalizeOutput(map[string]any{
			"checks": toCheckMaps(checks),
			"passed": true,
			"score":  1.0,
			"notes":  "补丁列表为空，未执行校验（静态校验不执行任何命令）",
		}), nil
	}

	start := time.Now()
	integrity := checkSnippetIntegrity(patches)
	integrityMS := time.Since(start).Milliseconds()
	start = time.Now()
	balanceIssues := checkBalance(patches)
	balanceMS := time.Since(start).Milliseconds()
	start = time.Now()
	syntax := checkStaticSyntax(patches, lang)
	syntaxMS := time.Since(start).Milliseconds()
	danger := checkDangerScan(patches)
	scale, scaleInfo := checkScale(patches)

	checks = append(checks,
		domain.VerificationCheck{
			Name: "snippet_integrity", Passed: integrity.ok(), DurationMS: integrityMS,
			Output: integrity.text("全部补丁 oldSnippet 非空且括号配平，可作为完整语法单元定位"),
		},
		domain.VerificationCheck{
			Name: "balance", Passed: balanceIssues.ok(), DurationMS: balanceMS,
			Output: balanceIssues.text("全部补丁 newSnippet 括号与引号配平"),
		},
		domain.VerificationCheck{
			Name: "static_syntax", Passed: syntax.ok(), DurationMS: syntaxMS,
			Output: syntax.text("语言语法哨兵检查通过"),
		},
		domain.VerificationCheck{
			Name: "danger_scan", Passed: danger.ok(),
			Output: danger.text("未发现危险操作"),
		},
		domain.VerificationCheck{
			Name: "scale", Passed: scale.ok(),
			Output: scale.text(scaleInfo),
		},
	)

	passedCount := 0
	for _, c := range checks {
		if c.Passed {
			passedCount++
		}
	}
	passed := passedCount == len(checks)
	score := round2(float64(passedCount) / float64(len(checks)))
	if !passed && score > 0.5 {
		score = 0.5
	}
	return normalizeOutput(map[string]any{
		"checks": toCheckMaps(checks),
		"passed": passed,
		"score":  score,
		"notes":  "静态校验：不执行命令、不修改文件，仅对补丁文本做语法与安全扫描",
	}), nil
}

// toCheckMaps 把校验结果转为可 JSON 序列化的结构。
func toCheckMaps(checks []domain.VerificationCheck) []map[string]any {
	out := make([]map[string]any, 0, len(checks))
	for _, c := range checks {
		out = append(out, structToMap(c))
	}
	return out
}

// checkSnippetIntegrity 校验 oldSnippet 是否为可定位的完整语法单元。
func checkSnippetIntegrity(patches []verifyPatch) *verifyIssue {
	issues := &verifyIssue{}
	for _, p := range patches {
		file := orDefault(p.FilePath, "<未命名文件>")
		old := p.OldSnippet
		if strings.TrimSpace(old) == "" {
			issues.add("补丁 %s 的 oldSnippet 为空，引擎层无法用 strings.Index 定位原文", file)
			continue
		}
		if res := analyzeBalance(old); !res.OK {
			issues.add("补丁 %s 的 oldSnippet 不是完整语法单元（%s）", file, res.Detail)
		}
		if strings.TrimSpace(p.NewSnippet) == "" {
			issues.add("补丁 %s 的 newSnippet 为空，无法生成 unified diff", file)
		} else if p.NewSnippet == old {
			issues.add("补丁 %s 的 newSnippet 与 oldSnippet 完全相同，属于空补丁", file)
		}
	}
	return issues
}

// checkBalance 校验 newSnippet 的括号与引号配平。
//
// 由于补丁片段是文件窗口（可能只截取了代码块的一部分），这里以「不引入新的配平偏差」
// 为准：newSnippet 的括号净变化必须与 oldSnippet 一致，且不得出现错配/未闭合引号。
func checkBalance(patches []verifyPatch) *verifyIssue {
	issues := &verifyIssue{}
	for _, p := range patches {
		file := orDefault(p.FilePath, "<未命名文件>")
		if strings.TrimSpace(p.NewSnippet) == "" {
			issues.add("补丁 %s 的 newSnippet 为空", file)
			continue
		}
		newRes := analyzeBalance(p.NewSnippet)
		if !newRes.OK {
			issues.add("补丁 %s 的 newSnippet 不配平：%s", file, newRes.Detail)
			continue
		}
		oldRes := analyzeBalance(p.OldSnippet)
		if oldRes.OK && newRes.Delta != oldRes.Delta {
			issues.add("补丁 %s 的括号净变化与原文不一致（old=%d, new=%d），可能破坏代码块结构",
				file, oldRes.Delta, newRes.Delta)
		}
	}
	return issues
}

// balanceResult 配平分析结果。
type balanceResult struct {
	// OK 是否存在结构性错误（错配闭合符、未闭合字符串/块注释）。
	OK bool
	// Delta 括号净深度（开括号数 - 闭括号数），窗口片段通常不为 0。
	Delta int
	// Detail 错误说明。
	Detail string
}

// analyzeBalance 括号/引号配平分析（忽略字符串与注释内容）。
func analyzeBalance(text string) balanceResult {
	type opener struct {
		ch  rune
		pos int
	}
	stack := []opener{}
	runes := []rune(text)
	line := 1
	depth := 0
	inLineComment := false
	inBlockComment := false
	var quote rune
	escaped := false
	matching := map[rune]rune{')': '(', ']': '[', '}': '{'}

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch == '\n' {
			line++
			inLineComment = false
			continue
		}
		if inLineComment {
			continue
		}
		if inBlockComment {
			if ch == '*' && i+1 < len(runes) && runes[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"', '`':
			quote = ch
			continue
		case '#':
			inLineComment = true
			continue
		case '/':
			if i+1 < len(runes) {
				switch runes[i+1] {
				case '/':
					inLineComment = true
					i++
					continue
				case '*':
					inBlockComment = true
					i++
					continue
				}
			}
		}
		switch ch {
		case '(', '[', '{':
			depth++
			stack = append(stack, opener{ch: ch, pos: line})
		case ')', ']', '}':
			depth--
			if len(stack) == 0 {
				return balanceResult{Detail: fmt.Sprintf("第 %d 行出现多余闭合符号 %q", line, string(ch))}
			}
			top := stack[len(stack)-1]
			if top.ch != matching[ch] {
				return balanceResult{Detail: fmt.Sprintf("第 %d 行闭合符号 %q 与第 %d 行的 %q 不匹配", line, string(ch), top.pos, string(top.ch))}
			}
			stack = stack[:len(stack)-1]
		}
	}
	if quote != 0 {
		return balanceResult{Detail: "存在未闭合的字符串引号"}
	}
	if inBlockComment {
		return balanceResult{Detail: "存在未闭合的块注释"}
	}
	return balanceResult{OK: true, Delta: depth}
}

// checkStaticSyntax 语言语法哨兵检查。
func checkStaticSyntax(patches []verifyPatch, lang string) *verifyIssue {
	issues := &verifyIssue{}
	for _, p := range patches {
		file := orDefault(p.FilePath, "<未命名文件>")
		target := lang
		if target == "" || target == "text" || target == "manifest" {
			target = normalizeLanguage(languageByPath(p.FilePath))
		}
		added := addedLines(p.OldSnippet, p.NewSnippet)
		switch target {
		case "go":
			checkGoSyntax(issues, file, p.NewSnippet, added)
		case "java":
			checkJavaSyntax(issues, file, added)
		case "python":
			checkPythonSyntax(issues, file, added)
		case "node":
			if !strings.Contains(p.NewSnippet, "{") && strings.Contains(p.NewSnippet, "=>") {
				issues.add("补丁 %s 的箭头函数缺少函数体，请确认 TS/JS 语法", file)
			}
		default:
			// 其他语言不做语法哨兵。
		}
	}
	return issues
}

// addedLines 返回 newSnippet 中相对 oldSnippet 新增的行。
func addedLines(oldSnippet, newSnippet string) []string {
	oldSet := map[string]int{}
	for _, l := range strings.Split(oldSnippet, "\n") {
		oldSet[l]++
	}
	out := []string{}
	for _, l := range strings.Split(newSnippet, "\n") {
		if oldSet[l] > 0 {
			oldSet[l]--
			continue
		}
		out = append(out, l)
	}
	return out
}

// checkGoSyntax Go 哨兵：未使用的 import / 未使用的变量。
func checkGoSyntax(issues *verifyIssue, file, newSnippet string, added []string) {
	imports := []string{}
	for _, line := range added {
		if m := reGoImportLine.FindStringSubmatch(line); m != nil {
			imports = append(imports, m[1])
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "\"") {
			if m := reGoImportInBlk.FindStringSubmatch(line); m != nil {
				imports = append(imports, m[1])
			}
		}
	}
	for _, imp := range imports {
		ident := baseName(imp)
		if ident == "" || strings.Contains(ident, ".") {
			continue
		}
		if !strings.Contains(newSnippet, ident+".") {
			issues.add("补丁 %s 新增 import %q 但未在补丁范围内使用，Go 编译器会报 imported and not used", file, imp)
		}
	}
	for _, line := range added {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "import") {
			continue
		}
		m := reGoShortDecl.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ident := m[1]
		if ident == "_" || strings.Contains(newSnippet, ident+" ") || strings.Contains(newSnippet, ident+".") ||
			strings.Contains(newSnippet, ident+")") || strings.Contains(newSnippet, ident+",") {
			continue
		}
		issues.add("补丁 %s 新增变量 %q 未在补丁范围内使用，Go 编译器会报 declared and not used", file, ident)
	}
}

// checkJavaSyntax Java 哨兵：语句分号与块大括号。
func checkJavaSyntax(issues *verifyIssue, file string, added []string) {
	for _, line := range added {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "@") ||
			strings.HasPrefix(trimmed, "<") {
			continue
		}
		last := trimmed[len(trimmed)-1]
		switch last {
		case ';', '{', '}', ',', '+', '(', '.', ':':
			continue
		}
		issues.add("补丁 %s 新增行 %q 缺少语句结束符（Java 需要分号或大括号）", file, truncateForMsg(trimmed))
	}
}

// checkPythonSyntax Python 哨兵：缩进一致性。
func checkPythonSyntax(issues *verifyIssue, file string, added []string) {
	indents := map[int]bool{}
	for _, line := range added {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "\t") {
			issues.add("补丁 %s 新增行使用制表符缩进，与 Python 空格缩进规范冲突", file)
			continue
		}
		indents[len(indentOf(line))] = true
	}
	if len(indents) <= 1 {
		return
	}
	list := make([]int, 0, len(indents))
	for v := range indents {
		list = append(list, v)
	}
	sort.Ints(list)
	base := list[0]
	for _, v := range list[1:] {
		if (v-base)%4 != 0 {
			issues.add("补丁 %s 新增行缩进层级 %d 与基准 %d 的差值不是 4 的倍数，存在 IndentationError 风险", file, v, base)
			break
		}
	}
}

// checkDangerScan 危险操作扫描。
func checkDangerScan(patches []verifyPatch) *verifyIssue {
	issues := &verifyIssue{}
	for _, p := range patches {
		file := orDefault(p.FilePath, "<未命名文件>")
		for _, d := range dangerPatterns {
			if strings.Contains(p.NewSnippet, d.pattern) {
				issues.add("补丁 %s %s（命中 %q）", file, d.desc, d.pattern)
			}
		}
	}
	return issues
}

// checkScale 变更规模检查。
func checkScale(patches []verifyPatch) (*verifyIssue, string) {
	issues := &verifyIssue{}
	files := map[string]bool{}
	addedTotal := 0
	for _, p := range patches {
		files[orDefault(p.FilePath, "<未命名文件>")] = true
		added := len(strings.Split(p.NewSnippet, "\n")) - len(strings.Split(p.OldSnippet, "\n"))
		if added > 0 {
			addedTotal += added
		}
	}
	info := fmt.Sprintf("变更 %d 个文件、净增 %d 行", len(files), addedTotal)
	if addedTotal > 60 {
		issues.add("新增行数 %d 超过 60 行阈值，属于大规模变更，需人工复核", addedTotal)
	}
	if len(files) > 5 {
		issues.add("影响文件数 %d 超过 5 个阈值，跨文件变更风险较高", len(files))
	}
	return issues, info
}

// truncateForMsg 截断过长的代码行用于提示。
func truncateForMsg(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "..."
}
