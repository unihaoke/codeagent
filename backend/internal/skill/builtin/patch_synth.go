package builtin

// 本文件实现核心修复技能 code_patch_synthesize 的**规则库**。
//
// 设计要点：
//  1. 全部规则基于「行级正则 + 上下文窗口」匹配，保证确定性、无网络、无副作用；
//  2. 产出补丁的 oldSnippet 一律取自原始文件行的精确子串（含缩进），
//     并在提交前用 strings.Contains(Content, oldSnippet) 自校验，
//     校验不通过的候选补丁直接丢弃并计入 dropped（引擎层可安全地用 strings.Index 定位）；
//  3. newSnippet 严格保持原缩进风格（Go 用制表符、其余语言沿用原行缩进）。

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// 补丁规则标识。
const (
	ruleNullPointer  = "null_pointer"
	ruleIndexBounds  = "index_out_of_bounds"
	ruleTypeError    = "type_error"
	ruleDependency   = "dependency_missing"
	ruleDivideByZero = "divide_by_zero"
	ruleConcurrent   = "concurrent_modification"
	ruleResourceLeak = "resource_leak"
	ruleTimeout      = "timeout"
)

// 修复规则正则。
var (
	// Java：从查询类方法取值（可能返回 null）。
	reJavaLookupAssign = regexp.MustCompile(`^\s*(?:final\s+)?([A-Z][\w<>,\.\[\]]*)\s+([a-zA-Z_]\w*)\s*=\s*([\w\.]+)\.(find|get|load|query|select|fetch|read)\w*\s*\(`)
	// Java：链式调用 findById(x).setStatus(y) 形式。
	reJavaChainCall = regexp.MustCompile(`^(\s*)([\w\.]+\.(?:find|get|load|query|select|fetch)\w*\s*\([^()]*\))\s*\.\s*([a-zA-Z_]\w*)\s*\((.*)\)\s*;\s*$`)
	// Java：void 方法签名（用于决定守卫是 return 还是抛异常）。
	reJavaVoidMethod = regexp.MustCompile(`\bvoid\s+[a-zA-Z_]\w*\s*\(`)

	// Go：多返回值调用赋值。
	reGoCallAssign = regexp.MustCompile(`^(\s*)([a-zA-Z_]\w*)\s*,\s*(?:err|ok)\s*:=\s*([\w\.]+)\.([A-Z]\w*)\s*\(`)

	// 通用：变量下标访问。
	reVarIndex = regexp.MustCompile(`([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*\[\s*([A-Za-z_]\w*)\s*\]`)
	// 通用：变量除法。
	reDivide = regexp.MustCompile(`([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)\s*/\s*([A-Za-z_]\w*)\s*(?:[;,\)\}]|$)`)

	// JS/TS：多级属性链（可能 undefined）。
	reJSChain    = regexp.MustCompile(`^(\s*)(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*){2,})\s*;?\s*$`)
	reJSChainUse = regexp.MustCompile(`([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*){2,})`)

	// Java 并发容器。
	reJavaHashMap   = regexp.MustCompile(`\bHashMap\b`)
	reJavaArrayList = regexp.MustCompile(`\bArrayList\b`)
	// Go 结构体 map 字段。
	reGoMapField = regexp.MustCompile(`^\s*([A-Z]\w*)\s+map\[`)

	// 资源获取。
	reJavaResource = regexp.MustCompile(`^\s*(?:final\s+)?([A-Z]\w*)\s+([a-zA-Z_]\w*)\s*=\s*new\s+(FileInputStream|FileOutputStream|FileReader|FileWriter|BufferedReader|BufferedWriter|InputStreamReader|OutputStreamWriter|Socket|Scanner)\b`)
	reGoResource   = regexp.MustCompile(`^(\s*)([a-zA-Z_]\w*)\s*,\s*(?:err|ok)\s*:=\s*(http\.(?:Get|Post|Do)|os\.Open\w*|net\.Dial\w*)\s*\(`)
	rePyResource   = regexp.MustCompile(`^(\s*)([a-zA-Z_]\w*)\s*=\s*open\s*\(`)

	// 超时缺失。
	reAxiosCall    = regexp.MustCompile(`(axios\.(?:get|post|put|delete|patch)\s*\()([^,()]*)(\)\s*;?\s*)$`)
	reGoHTTPGet    = regexp.MustCompile(`\bhttp\.(Get|Post|Do)\s*\(`)
	reJavaTemplate = regexp.MustCompile(`^\s*(?:final\s+)?RestTemplate\s+([a-zA-Z_]\w*)\s*=\s*new\s+RestTemplate\s*\(\s*\)\s*;`)

	// 清单文件行锚点。
	rePomDeps        = regexp.MustCompile(`^\s*<dependencies>\s*$`)
	rePackageJSONDep = regexp.MustCompile(`^(\s*)"(dependencies|devDependencies)"\s*:\s*\{\s*$`)
	reGoModRequire   = regexp.MustCompile(`^require\s*\($`)
	reGoModRequireLn = regexp.MustCompile(`^require\s+[^\s]+\s+v[^\s]+$`)
	reGradleDeps     = regexp.MustCompile(`^(\s*)dependencies\s*\{\s*$`)
)

// patchDraft 补丁草稿（引擎层据此生成 unified diff）。
type patchDraft struct {
	RepositoryID string
	RepoKey      string
	FilePath     string
	Line         int
	OldSnippet   string
	NewSnippet   string
	Rationale    string
	Risk         string
	Confidence   float64
	Rule         string
}

// patchBuilder 单文件补丁收集器。
type patchBuilder struct {
	repoID  string
	repoKey string
	path    string
	content string
	lines   []string
	drafts  []patchDraft
	dropped int
	seen    map[string]bool
}

// newPatchBuilder 创建单文件补丁收集器。
func newPatchBuilder(slice domain.CodeSlice) *patchBuilder {
	return &patchBuilder{
		repoID:  slice.RepositoryID,
		repoKey: slice.RepoKey,
		path:    slice.Path,
		content: slice.Content,
		lines:   splitLines(slice.Content),
		seen:    map[string]bool{},
	}
}

// commit 提交候选补丁：自校验通过才保留，否则计入 dropped。
func (b *patchBuilder) commit(d patchDraft, oldSnippet, newSnippet string) bool {
	if strings.TrimSpace(oldSnippet) == "" || strings.TrimSpace(newSnippet) == "" {
		b.dropped++
		return false
	}
	if oldSnippet == newSnippet {
		b.dropped++
		return false
	}
	if !strings.Contains(b.content, oldSnippet) {
		// oldSnippet 必须是原文精确子串，否则引擎层无法定位，直接丢弃。
		b.dropped++
		return false
	}
	key := d.Rule + "|" + itoa(d.Line)
	if b.seen[key] {
		return false
	}
	b.seen[key] = true
	d.OldSnippet = oldSnippet
	d.NewSnippet = newSnippet
	b.drafts = append(b.drafts, d)
	return true
}

// insert 构造「在某行前插入若干行」的补丁。
func (b *patchBuilder) insert(d patchDraft, anchor, insertAt, before, after int, inserted []string) bool {
	start, end := expandWindow(len(b.lines), anchor, before, after)
	if insertAt < start {
		start = insertAt
	}
	if insertAt > end+1 {
		end = insertAt - 1
	}
	if end < start || start < 0 || end >= len(b.lines) {
		b.dropped++
		return false
	}
	d.Line = insertAt + 1
	return b.commit(d, windowText(b.lines, start, end), windowWithInsert(b.lines, start, end, insertAt, inserted))
}

// replace 构造「替换某一行」的补丁。
func (b *patchBuilder) replace(d patchDraft, anchor, replaceAt, before, after int, replaced []string) bool {
	start, end := expandWindow(len(b.lines), anchor, before, after)
	if replaceAt < start {
		start = replaceAt
	}
	if replaceAt > end {
		end = replaceAt
	}
	if end < start || start < 0 || end >= len(b.lines) {
		b.dropped++
		return false
	}
	d.Line = replaceAt + 1
	return b.commit(d, windowText(b.lines, start, end), windowWithReplace(b.lines, start, end, replaceAt, replaced))
}

// windowText 返回 [start,end] 闭区间窗口的原始文本（精确子串）。
func windowText(lines []string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	if end < start {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// windowWithInsert 在 insertAt 行之前插入若干行后的窗口文本；insertAt == end+1 表示追加。
func windowWithInsert(lines []string, start, end, insertAt int, inserted []string) string {
	out := make([]string, 0, end-start+1+len(inserted))
	for i := start; i <= end; i++ {
		if i == insertAt {
			out = append(out, inserted...)
		}
		out = append(out, lines[i])
	}
	if insertAt > end {
		out = append(out, inserted...)
	}
	return strings.Join(out, "\n")
}

// windowWithReplace 用 replaced 行替换 replaceAt 行后的窗口文本。
func windowWithReplace(lines []string, start, end, replaceAt int, replaced []string) string {
	out := make([]string, 0, end-start+1+len(replaced))
	for i := start; i <= end; i++ {
		if i == replaceAt {
			out = append(out, replaced...)
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

// expandWindow 计算补丁窗口：尽量落在 3-6 行之间，并保证 anchor 在窗口内。
func expandWindow(n, anchor, before, after int) (int, int) {
	if n <= 0 {
		return 0, -1
	}
	if anchor < 0 {
		anchor = 0
	}
	if anchor >= n {
		anchor = n - 1
	}
	start := anchor - before
	end := anchor + after
	if start < 0 {
		end += -start
		start = 0
	}
	if end > n-1 {
		start -= end - (n - 1)
		end = n - 1
	}
	if start < 0 {
		start = 0
	}
	for end-start+1 < 3 && (start > 0 || end < n-1) {
		if end < n-1 {
			end++
		} else if start > 0 {
			start--
		}
	}
	for end-start+1 > 6 {
		if anchor-start > end-anchor {
			start++
		} else {
			end--
		}
	}
	return start, end
}

// indentOf 返回行的缩进前缀。
func indentOf(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	return line[:len(line)-len(trimmed)]
}

// ruleSetForCategory 返回该分类下可用的修复规则集合。
func ruleSetForCategory(category string) []string {
	c := strings.ToLower(strings.TrimSpace(category))
	switch c {
	case "null_pointer", "nullpointer", "npe", "nil_pointer", "nil", "nullpointerexception":
		return []string{ruleNullPointer}
	case "index_out_of_bounds", "indexoutofbounds", "index_error", "indexerror":
		return []string{ruleIndexBounds}
	case "type_error", "typeerror", "undefined_property", "cannot_read_properties":
		return []string{ruleTypeError}
	case "dependency_missing", "class_not_found", "dependency", "noclassdeffound":
		return []string{ruleDependency}
	case "divide_by_zero", "dividebyzero", "division_by_zero", "arithmetic":
		return []string{ruleDivideByZero}
	case "concurrent_modification", "concurrency", "concurrentmodification":
		return []string{ruleConcurrent}
	case "resource_leak", "resourceleak", "leak":
		return []string{ruleResourceLeak}
	case "timeout", "timeout_missing", "no_timeout":
		return []string{ruleTimeout}
	case "":
		return []string{ruleNullPointer, ruleIndexBounds, ruleTypeError, ruleDependency,
			ruleDivideByZero, ruleConcurrent, ruleResourceLeak, ruleTimeout}
	default:
		// 未识别的分类（含 unknown / oom / syntax_error）按全规则尽力匹配，
		// 匹配不到时返回空补丁，绝不编造修改。
		return []string{ruleNullPointer, ruleIndexBounds, ruleTypeError, ruleDependency,
			ruleDivideByZero, ruleConcurrent, ruleResourceLeak, ruleTimeout}
	}
}

// appendRules 合并规则集合并去重（保持先后顺序）。
func appendRules(base []string, extra []string) []string {
	out := append([]string(nil), base...)
	for _, r := range extra {
		if !containsString(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// rulesFromFeedback 依据人工反馈/验证失败信息识别需要补充的修复规则。
func rulesFromFeedback(feedback string) []string {
	text := strings.ToLower(feedback)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	type keywordRule struct {
		rule     string
		keywords []string
	}
	table := []keywordRule{
		{ruleNullPointer, []string{"空指针", "null", "npe", "nil"}},
		{ruleIndexBounds, []string{"越界", "index", "下标"}},
		{ruleTypeError, []string{"undefined", "typeerror", "可选链", "option"}},
		{ruleDependency, []string{"依赖", "dependency", "classnotfound", "module"}},
		{ruleDivideByZero, []string{"除零", "divide", "division", "arithmetic"}},
		{ruleConcurrent, []string{"并发", "concurrent", "线程安全"}},
		{ruleResourceLeak, []string{"泄漏", "leak", "未关闭", "close", "资源"}},
		{ruleTimeout, []string{"超时", "timeout", "重试"}},
	}
	out := []string{}
	for _, item := range table {
		for _, kw := range item.keywords {
			if strings.Contains(text, kw) {
				out = append(out, item.rule)
				break
			}
		}
	}
	return out
}

// handleCodePatchSynthesize code_patch_synthesize 技能处理器。
//
// 输入：rootCause（对象）、slices（数组）、category（字符串）。
// 输出：patches、rationale、risk。
func handleCodePatchSynthesize(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	category := strings.ToLower(strings.TrimSpace(stringValue(in["category"])))
	rootCause := toMap(in["rootCause"])
	slices := decodeSlices(in["slices"])
	// pipeline 会在多轮修复时注入 feedback（人工反馈或上一轮验证失败信息），
	// 这里按关键字并集补充规则，使反馈能真正影响修复方向而不是被静默忽略。
	feedback := stringValue(in["feedback"])
	rules := appendRules(ruleSetForCategory(category), rulesFromFeedback(feedback))
	rootCauseText := flattenMap(rootCause)

	builder := &multiPatchBuilder{}
	for _, slice := range slices {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(slice.Content) == "" {
			continue
		}
		lang := sliceLanguage(slice)
		pb := newPatchBuilder(slice)
		for _, rule := range rules {
			switch rule {
			case ruleNullPointer:
				synthNullPointer(pb, lang)
			case ruleIndexBounds:
				synthIndexBounds(pb, lang)
			case ruleTypeError:
				synthTypeError(pb, lang)
			case ruleDependency:
				synthDependency(pb, lang, rootCauseText)
			case ruleDivideByZero:
				synthDivideByZero(pb, lang)
			case ruleConcurrent:
				synthConcurrent(pb, lang)
			case ruleResourceLeak:
				synthResourceLeak(pb, lang)
			case ruleTimeout:
				synthTimeout(pb, lang)
			}
		}
		builder.merge(pb)
	}

	drafts := builder.drafts
	sort.SliceStable(drafts, func(i, j int) bool {
		if drafts[i].FilePath != drafts[j].FilePath {
			return drafts[i].FilePath < drafts[j].FilePath
		}
		return drafts[i].Line < drafts[j].Line
	})

	patches := make([]map[string]any, 0, len(drafts))
	for _, d := range drafts {
		patches = append(patches, map[string]any{
			"repositoryId": d.RepositoryID,
			"repoKey":      d.RepoKey,
			"filePath":     d.FilePath,
			"action":       string(domain.ActionModify),
			"line":         d.Line,
			"oldSnippet":   d.OldSnippet,
			"newSnippet":   d.NewSnippet,
			"rationale":    d.Rationale,
			"risk":         d.Risk,
			"confidence":   d.Confidence,
		})
	}

	risk := riskLowest
	fileSet := map[string]bool{}
	for _, d := range drafts {
		risk = maxRisk(risk, d.Risk)
		fileSet[d.FilePath] = true
	}
	rationale := buildSynthRationale(category, len(slices), builder.totalLines, len(drafts), len(fileSet), builder.dropped, risk)
	if strings.TrimSpace(feedback) != "" {
		rationale += " 已结合人工反馈调整修复方向：" + truncateForMsg(feedback) + "。"
	}
	return normalizeOutput(map[string]any{
		"patches":   patches,
		"rationale": rationale,
		"risk":      risk,
		"dropped":   builder.dropped,
	}), nil
}

// multiPatchBuilder 汇总多文件补丁并统计。
type multiPatchBuilder struct {
	drafts     []patchDraft
	dropped    int
	totalLines int
}

// merge 合并单文件结果。
func (m *multiPatchBuilder) merge(pb *patchBuilder) {
	m.drafts = append(m.drafts, pb.drafts...)
	m.dropped += pb.dropped
	m.totalLines += len(pb.lines)
}

// riskLowest 风险等级常量（与 domain.PatchRisk 取值一致）。
const (
	riskLowest = string(domain.RiskLow)
	riskMid    = string(domain.RiskMedium)
	riskTop    = string(domain.RiskHigh)
)

// maxRisk 取更高风险等级。
func maxRisk(a, b string) string {
	rank := map[string]int{riskLowest: 0, riskMid: 1, riskTop: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// buildSynthRationale 生成补丁总述（含丢弃数量说明）。
func buildSynthRationale(category string, files, lines, patches, patchFiles, dropped int, risk string) string {
	var sb strings.Builder
	if patches == 0 {
		sb.WriteString("未匹配到可自动修复模式，建议人工介入：")
		sb.WriteString("已扫描 ")
		sb.WriteString(itoa(files))
		sb.WriteString(" 个代码切片、")
		sb.WriteString(itoa(lines))
		sb.WriteString(" 行代码，分类=")
		if category == "" {
			sb.WriteString("未提供")
		} else {
			sb.WriteString(category)
		}
		sb.WriteString("；为保证补丁可精确应用，另有 ")
		sb.WriteString(itoa(dropped))
		sb.WriteString(" 个候选补丁因无法在原文中精确匹配而被丢弃。")
		return sb.String()
	}
	sb.WriteString("基于分类「")
	if category == "" {
		sb.WriteString("未提供")
	} else {
		sb.WriteString(category)
	}
	sb.WriteString("」与代码语义匹配，产出 ")
	sb.WriteString(itoa(patches))
	sb.WriteString(" 个可应用补丁（覆盖 ")
	sb.WriteString(itoa(patchFiles))
	sb.WriteString(" 个文件），整体风险等级 ")
	sb.WriteString(risk)
	sb.WriteString("；所有 oldSnippet 均为原文精确子串，可直接生成 unified diff。为保证精确性丢弃了 ")
	sb.WriteString(itoa(dropped))
	sb.WriteString(" 个无法定位的候选补丁。")
	return sb.String()
}

// stringValue 安全取字符串。
func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// flattenMap 把根因对象压平成文本，便于提取包名等线索。
func flattenMap(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	var sb strings.Builder
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("=")
		switch v := m[k].(type) {
		case string:
			sb.WriteString(v)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					sb.WriteString(s)
					sb.WriteString(" ")
				}
			}
		default:
			if s, ok := v.(string); ok {
				sb.WriteString(s)
			}
		}
		sb.WriteString("; ")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 规则 1：空指针 / nil 守卫
// ---------------------------------------------------------------------------

// synthNullPointer 空指针守卫规则（Java 查询结果判空 + Go 多返回值判空）。
func synthNullPointer(pb *patchBuilder, lang string) {
	switch lang {
	case "java":
		// 规则 0：可空方法参数被直接解引用（方法入口补判空守卫）。
		// 这是 Java NPE 最常见形态，例如 toDetail(Order order) 中直接 order.getAmount()。
		synthJavaParamNull(pb)
		for i, line := range pb.lines {
			if m := reJavaLookupAssign.FindStringSubmatch(line); m != nil {
				varName := m[2]
				if hasNilGuardNear(pb.lines, i, varName) {
					continue
				}
				if !hasDerefWithin(pb.lines, i, varName, 4) {
					continue
				}
				indent := indentOf(line)
				guard := javaNullGuard(pb.lines, i, indent, varName, m[3]+"."+m[4])
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "变量 " + varName + " 来自可能返回空的查询方法 " + m[3] + "." + m[4] + "，其后立即被解引用；补充提前返回守卫，避免 NullPointerException。",
					Risk:       riskMid,
					Confidence: 0.75,
					Rule:       ruleNullPointer,
				}
				pb.insert(d, i+1, i+1, 2, 1, guard)
				continue
			}
			if m := reJavaChainCall.FindStringSubmatch(line); m != nil {
				indent, receiver, method, args := m[1], strings.TrimSpace(m[2]), m[3], m[4]
				replaced := indent + "Optional.ofNullable(" + receiver + ").ifPresent(o -> o." + method + "(" + args + "));"
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "链式调用 " + receiver + "." + method + " 未做空值保护，改用 Optional.ofNullable 后置执行，兼顾空安全与可读性（需 import java.util.Optional）。",
					Risk:       riskMid,
					Confidence: 0.6,
					Rule:       ruleNullPointer,
				}
				pb.replace(d, i, i, 1, 1, []string{replaced})
			}
		}
	case "go":
		for i, line := range pb.lines {
			m := reGoCallAssign.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			varName, receiver, method := m[2], m[3], m[4]
			if hasNilGuardNear(pb.lines, i, varName) || !hasDerefWithin(pb.lines, i, varName, 4) {
				continue
			}
			indent := indentOf(line)
			if indent == "" {
				indent = "\t"
			}
			guard := []string{
				indent + "if " + varName + " == nil {",
			}
			guard = append(guard, goReturnSnippet(pb.lines, i, "\""+varName+" 为 nil："+receiver+"."+method+" 未返回结果\"")...)
			guard = append(guard, indent+"}")
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "变量 " + varName + " 来自 " + receiver + "." + method + "，返回 nil 时后续解引用会 panic；补充 nil 判断并在异常分支提前返回（返回签名由所在函数推断，如为多返回值会使用 nil 占位）。",
				Risk:       riskMid,
				Confidence: 0.65,
				Rule:       ruleNullPointer,
			}
			pb.insert(d, i+1, i+1, 2, 1, guard)
		}
	}
}

// synthJavaParamNull 识别"可空方法参数被直接解引用"的 NPE 形态，并在方法入口插入判空守卫。
//
// 命中条件（全部满足才算命中，避免误报）：
//  1. 向上 40 行内存在 Java 方法签名，且该签名不是控制流语句（if/for/while/catch/return/...）；
//  2. 签名的形参列表中存在被解引用的对象类型参数（非基本类型、非 final 修饰）；
//  3. 该参数在签名之后的 12 行内出现 `参数.` 解引用，且附近没有判空保护。
//
// 生成的守卫：void 方法提前 return；有返回值的方法返回 null（对调用方等价于"查无此对象"，
// 不引入新的异常语义）；无法解析返回类型时不生成补丁（宁可不改，也不猜测）。
func synthJavaParamNull(pb *patchBuilder) {
	for i, line := range pb.lines {
		header, returns := javaMethodHeaderAt(pb.lines, i)
		if header == "" {
			continue
		}
		params := javaObjectParams(header)
		for _, param := range params {
			if !hasDerefWithin(pb.lines, i, param, 12) {
				continue
			}
			if hasNilGuardNear(pb.lines, i, param) {
				continue
			}
			indent := indentOf(line) + "    "
			guard := []string{indent + "if (" + param + " == null) {"}
			switch {
			case returns == "void":
				guard = append(guard, indent+"    return;")
			case returns != "":
				guard = append(guard, indent+"    return null;")
			default:
				continue
			}
			guard = append(guard, indent+"}")
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale: "方法入参 " + param + " 被直接解引用而未做空值校验，调用方传入 null 时必然抛 NullPointerException；" +
					"在方法入口补充判空守卫（" + guardAction(returns) + "），属于最小增量修改，不改变原有业务分支。",
				Risk:       riskMid,
				Confidence: 0.72,
				Rule:       ruleNullPointer,
			}
			pb.insert(d, i+1, i+1, 1, 1, guard)
			return // 一个方法只插入一次守卫，避免重复补丁
		}
	}
}

// guardAction 返回守卫动作的中文描述。
func guardAction(returns string) string {
	if returns == "void" {
		return "void 方法提前返回"
	}
	return "有返回值的方法返回 null，调用方按\"查无此对象\"处理"
}

// javaIdentRe 匹配合法 Java 标识符。
var javaIdentRe = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)

// javaMethodHeaderAt 判断第 idx 行是否为 Java 方法签名（方法声明的左花括号所在行）。
//
// 返回方法签名文本与返回类型；非方法签名返回空串。
func javaMethodHeaderAt(lines []string, idx int) (header, returns string) {
	if idx < 0 || idx >= len(lines) {
		return "", ""
	}
	trimmed := strings.TrimSpace(lines[idx])
	if !strings.HasSuffix(trimmed, "{") {
		return "", ""
	}
	sig := strings.TrimSpace(strings.TrimSuffix(trimmed, "{"))
	if !strings.HasSuffix(sig, ")") {
		return "", ""
	}
	open := strings.Index(sig, "(")
	if open <= 0 {
		return "", ""
	}
	// 排除控制流与其它非方法声明。
	switch {
	case strings.HasPrefix(sig, "if"), strings.HasPrefix(sig, "for"), strings.HasPrefix(sig, "while"),
		strings.HasPrefix(sig, "switch"), strings.HasPrefix(sig, "catch"), strings.HasPrefix(sig, "return"),
		strings.HasPrefix(sig, "else"), strings.HasPrefix(sig, "try"), strings.HasPrefix(sig, "do"),
		strings.HasPrefix(sig, "synchronized"):
		return "", ""
	}
	name := strings.TrimSpace(sig[:open])
	if strings.ContainsAny(name, "=;+*/%<>!&|?") {
		return "", ""
	}
	// 构造器（方法名与类名同名、无返回类型）必须排除：
	// 在构造器里 `return null;` 无法编译，`return;` 虽合法但会跳过字段赋值、
	// 留下未初始化状态，属于比原缺陷更危险的改动。
	lastSpace := strings.LastIndex(name, " ")
	if lastSpace < 0 {
		return "", ""
	}
	methodName := strings.TrimSpace(name[lastSpace+1:])
	if isConstructorName(methodName) {
		return "", ""
	}
	ret := strings.TrimSpace(name[:lastSpace])
	for _, mod := range []string{"public", "private", "protected", "static", "final", "abstract", "default", "synchronized", "native"} {
		ret = strings.TrimSpace(strings.TrimPrefix(ret, mod+" "))
		ret = strings.TrimSpace(strings.TrimPrefix(ret, mod+"\t"))
	}
	if ret == "" || strings.Contains(ret, "new ") {
		return "", ""
	}
	return sig, ret
}

// isConstructorName 判断方法名是否为构造器形态。
//
// 判定依据：JVM 构造器约定为「首字母大写且全大写下划线式命名不出现」的驼峰类名，
// 同时排除常见全大写常量式命名（例如 `HTTPServer` 之外的 `LOG`）——
// 实践中构造器名等于类名，因此用「首字符大写 + 不含下划线」近似即可，
// 宁可漏改（不生成补丁）也不生成无法编译的补丁。
func isConstructorName(name string) bool {
	if name == "" {
		return false
	}
	first := rune(name[0])
	if first < 'A' || first > 'Z' {
		return false
	}
	return !strings.Contains(name, "_")
}

// javaObjectParams 从方法签名中提取可能为 null 的对象类型形参。
//
// 过滤规则：跳过泛型展开后的嵌套逗号、跳过 final 参数（作者已显式声明不可变语义）、
// 跳过基本类型与其包装类的常见误报（int/long/boolean/double/float/char/byte/short/void）。
func javaObjectParams(sig string) []string {
	open := strings.Index(sig, "(")
	close := strings.LastIndex(sig, ")")
	if open < 0 || close <= open {
		return nil
	}
	raw := sig[open+1 : close]
	var out []string
	for _, part := range splitTopLevel(raw) {
		p := strings.TrimSpace(part)
		if p == "" || strings.HasPrefix(p, "@") || strings.HasPrefix(p, "final ") {
			continue
		}
		if strings.Contains(p, "...") {
			continue // 变长参数不做守卫（数组语义不同）
		}
		fields := strings.Fields(p)
		if len(fields) < 2 {
			continue
		}
		name := fields[len(fields)-1]
		if !javaIdentRe.MatchString(name) {
			continue
		}
		typ := strings.Join(fields[:len(fields)-1], "")
		if isJavaPrimitive(typ) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// splitTopLevel 按顶层逗号切分（忽略泛型尖括号内的逗号）。
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	out = append(out, s[start:])
	return out
}

// isJavaPrimitive 判断是否为 Java 基本类型。
func isJavaPrimitive(t string) bool {
	switch strings.TrimSpace(t) {
	case "int", "long", "short", "byte", "char", "boolean", "float", "double", "void":
		return true
	default:
		return false
	}
}

// javaNullGuard 生成 Java 判空守卫（void 方法提前返回，否则抛业务异常）。
func javaNullGuard(lines []string, idx int, indent, varName, source string) []string {
	if isVoidMethodAbove(lines, idx) {
		return []string{
			indent + "if (" + varName + " == null) {",
			indent + "    return;",
			indent + "}",
		}
	}
	return []string{
		indent + "if (" + varName + " == null) {",
		indent + "    throw new IllegalStateException(\"" + varName + " 为 null：\" + \"" + source + " 未查询到数据\");",
		indent + "}",
	}
}

// javaSizeExpr 生成 Java 容器的长度表达式：数组用 .length，集合用 .size()。
//
// 判断依据：向上查找该变量的声明，若声明中出现 "[]" 则按数组处理（避免数组误用 size()）。
func javaSizeExpr(lines []string, idx int, container string) string {
	for i := idx; i >= 0 && i > idx-30; i-- {
		line := lines[i]
		if !strings.Contains(line, container) {
			continue
		}
		if strings.Contains(line, "[]") || strings.Contains(line, container+"[") {
			return container + ".length"
		}
		if strings.Contains(line, "List<") || strings.Contains(line, "Set<") || strings.Contains(line, "Map<") ||
			strings.Contains(line, "Collection<") || strings.Contains(line, "new ArrayList") {
			return container + ".size()"
		}
	}
	if strings.Contains(container, ".") {
		// 方法链/字段访问无法判定类型时回退到 size()（集合更常见）。
		return container + ".size()"
	}
	return container + ".length"
}

// isVoidMethodAbove 判断命中行是否处于 void 方法内。
func isVoidMethodAbove(lines []string, idx int) bool {
	for i := idx; i >= 0 && i > idx-30; i-- {
		line := lines[i]
		if !strings.Contains(line, "(") {
			continue
		}
		if reJavaVoidMethod.MatchString(line) {
			return true
		}
		if strings.Contains(line, " class ") || strings.HasPrefix(strings.TrimSpace(line), "class ") {
			return false
		}
		// 命中到方法签名（非 void）即停止。
		if reSymJavaFunc.MatchString(line) {
			return false
		}
	}
	return false
}

// hasNilGuardNear 判断变量附近是否已有判空保护。
func hasNilGuardNear(lines []string, idx int, varName string) bool {
	start := idx - 6
	if start < 0 {
		start = 0
	}
	end := idx + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := start; i < end; i++ {
		line := lines[i]
		if !strings.Contains(line, varName) {
			continue
		}
		if strings.Contains(line, varName+" == null") || strings.Contains(line, varName+" != null") ||
			strings.Contains(line, varName+" == nil") || strings.Contains(line, varName+" != nil") ||
			strings.Contains(line, "Optional.ofNullable("+varName) || strings.Contains(line, "if err != nil") {
			return true
		}
	}
	return false
}

// goReturnSnippet 依据所在 Go 函数签名生成错误返回语句。
func goReturnSnippet(lines []string, idx int, messageExpr string) []string {
	results := ""
	for i := idx; i >= 0 && i > idx-40; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "func ") {
			results = parseGoFuncResults(trimmed)
			break
		}
	}
	switch {
	case results == "":
		return []string{"\treturn"}
	case strings.Contains(results, "error"):
		if strings.Contains(results, ",") {
			return []string{"\t\treturn nil, fmt.Errorf(" + messageExpr + ")"}
		}
		return []string{"\t\treturn fmt.Errorf(" + messageExpr + ")"}
	default:
		return []string{"\treturn"}
	}
}

// parseGoFuncResults 解析 Go 函数签名的返回值列表。
func parseGoFuncResults(header string) string {
	idx := strings.Index(header, "func")
	if idx < 0 {
		return ""
	}
	rest := header[idx:]
	start := strings.Index(rest, "(")
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				tail := rest[i+1:]
				if bi := strings.Index(tail, "{"); bi >= 0 {
					tail = tail[:bi]
				}
				return strings.TrimSpace(tail)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 规则 2：数组下标越界
// ---------------------------------------------------------------------------

// synthIndexBounds 下标边界校验规则。
func synthIndexBounds(pb *patchBuilder, lang string) {
	for i, line := range pb.lines {
		code := stripStringLiterals(line)
		trimmed := strings.TrimSpace(code)
		if trimmed == "" || isTypeDeclaration(code) || reComment.MatchString(trimmed) {
			continue
		}
		m := reVarIndex.FindStringSubmatch(code)
		if m == nil {
			continue
		}
		container, indexVar := m[1], m[2]
		if container == "" || indexVar == "" {
			continue
		}
		if reBoundsCheck.MatchString(code) || hasBoundsCheckAround(pb.lines, i) {
			continue
		}
		indent := indentOf(line)
		var guard []string
		switch lang {
		case "java":
			guard = []string{
				indent + "if (" + indexVar + " < 0 || " + indexVar + " >= " + javaSizeExpr(pb.lines, i, container) + ") {",
				indent + "    throw new IndexOutOfBoundsException(\"下标越界: " + indexVar + "=\" + " + indexVar + ");",
				indent + "}",
			}
		case "go":
			guard = []string{
				indent + "if " + indexVar + " < 0 || " + indexVar + " >= len(" + container + ") {",
			}
			guard = append(guard, goReturnSnippet(pb.lines, i,
				"\"下标越界: "+indexVar+"=%d, len("+container+")=%d\", "+indexVar+", len("+container+")")...)
			guard = append(guard, indent+"}")
		case "python":
			guard = []string{
				indent + "if " + indexVar + " < 0 or " + indexVar + " >= len(" + container + "):",
				indent + "    raise IndexError(f\"下标越界: " + indexVar + "={" + indexVar + "}\")",
			}
		case "node":
			guard = []string{
				indent + "if (" + indexVar + " < 0 || " + indexVar + " >= " + container + ".length) {",
				indent + "    throw new RangeError(`下标越界: " + indexVar + "=${" + indexVar + "}`);",
				indent + "}",
			}
		default:
			continue
		}
		d := patchDraft{
			RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
			Rationale:  "访问 " + container + "[" + indexVar + "] 前未见边界校验，补充下标范围判断，避免 IndexOutOfBounds/IndexError。",
			Risk:       riskLowest,
			Confidence: 0.7,
			Rule:       ruleIndexBounds,
		}
		pb.insert(d, i, i, 1, 1, guard)
	}
}

// ---------------------------------------------------------------------------
// 规则 3：JS/TS 可选链
// ---------------------------------------------------------------------------

// synthTypeError JS/TS 空安全规则：补可选链与默认值。
func synthTypeError(pb *patchBuilder, lang string) {
	if lang != "node" {
		return
	}
	for i, line := range pb.lines {
		if strings.Contains(line, "?.") || strings.Contains(line, "??") {
			continue
		}
		if m := reJSChain.FindStringSubmatch(line); m != nil {
			indent, varName, chain := m[1], m[2], m[3]
			if isSafeJSChain(chain) {
				continue
			}
			suffix := "?? {}"
			if usesAsArray(pb.lines, i) {
				suffix = "?? []"
			}
			replaced := indent + "const " + varName + " = " + optionalChain(chain) + " " + suffix + ";"
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "多级属性链 " + chain + " 在中间对象为 undefined 时会抛 TypeError（Cannot read properties of undefined），补充可选链与默认值。",
				Risk:       riskLowest,
				Confidence: 0.65,
				Rule:       ruleTypeError,
			}
			pb.replace(d, i, i, 1, 1, []string{replaced})
			continue
		}
		if strings.Contains(line, ".map(") || strings.Contains(line, ".filter(") ||
			strings.Contains(line, ".forEach(") || strings.Contains(line, ".length") {
			loc := reJSChainUse.FindStringSubmatchIndex(line)
			if loc == nil {
				continue
			}
			chain := line[loc[2]:loc[3]]
			if isSafeJSChain(chain) {
				continue
			}
			replaced := line[:loc[2]] + optionalChain(chain) + line[loc[3]:]
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "对可能为 undefined 的属性链 " + chain + " 直接取属性/调用方法，补充可选链，避免 Cannot read properties of undefined。",
				Risk:       riskLowest,
				Confidence: 0.6,
				Rule:       ruleTypeError,
			}
			pb.replace(d, i, i, 1, 1, []string{replaced})
		}
	}
}

// optionalChain 把 a.b.c 转成 a?.b?.c。
func optionalChain(chain string) string {
	parts := strings.Split(chain, ".")
	return strings.Join(parts, "?.")
}

// isSafeJSChain 排除全局安全对象与已经是可选链的表达式。
func isSafeJSChain(chain string) bool {
	head := chain
	if idx := strings.Index(chain, "."); idx > 0 {
		head = chain[:idx]
	}
	switch head {
	case "process", "console", "Math", "JSON", "Object", "Array", "Number", "String", "this", "window":
		return true
	}
	return strings.Contains(chain, "?.")
}

// usesAsArray 判断变量后续是否按数组使用（决定默认值取 [] 还是 {}）。
func usesAsArray(lines []string, idx int) bool {
	end := idx + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := idx + 1; i < end; i++ {
		line := lines[i]
		if strings.Contains(line, ".map(") || strings.Contains(line, ".filter(") ||
			strings.Contains(line, ".forEach(") || strings.Contains(line, ".length") ||
			strings.Contains(line, "for (") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 规则 4：依赖缺失 / 类找不到
// ---------------------------------------------------------------------------

// synthDependency 依赖清单补全规则。
func synthDependency(pb *patchBuilder, lang string, rootCauseText string) {
	if lang != "manifest" && !isManifestPath(pb.path) {
		return
	}
	pkg, artifact := extractMissingPackage(rootCauseText)
	if pkg == "" {
		return
	}
	groupID, artifactID := splitJavaCoordinate(pkg)
	if artifact != "" {
		artifactID = artifact
	}
	oldSnippet, newSnippet, line, rationale, ok := buildDependencyInsertion(
		pb.lines, pb.path, pkg, groupID, artifactID, "")
	if !ok {
		return
	}
	d := patchDraft{
		RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
		Rationale:  rationale,
		Risk:       riskMid,
		Confidence: 0.6,
		Rule:       ruleDependency,
	}
	d.Line = line
	pb.commit(d, oldSnippet, newSnippet)
}

// isManifestPath 判断是否为依赖清单文件。
func isManifestPath(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	switch baseName(p) {
	case "pom.xml", "build.gradle", "build.gradle.kts", "package.json",
		"requirements.txt", "go.mod", "setup.py", "pyproject.toml":
		return true
	}
	return false
}

// extractMissingPackage 从报错文本中提取缺失的依赖/类坐标。
func extractMissingPackage(text string) (pkg, artifact string) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`NoClassDefFoundError:\s*([\w/\.\$]+)`),
		regexp.MustCompile(`ClassNotFoundException:\s*([\w/\.\$]+)`),
		regexp.MustCompile(`Cannot find module '([^']+)'`),
		regexp.MustCompile(`Cannot find module "([^"]+)"`),
		regexp.MustCompile(`No module named '?([\w\.\-]+)'?`),
		regexp.MustCompile(`cannot find package "([^"]+)"`),
		regexp.MustCompile(`no required module provides package ([\w\./\-]+)`),
		regexp.MustCompile(`Could not resolve dependencies[^:]*:\s*([\w\.\-]+:[\w\.\-]+)`),
		regexp.MustCompile(`ArtifactNotFound\w*:\s*([\w\.\-]+)`),
		regexp.MustCompile(`Unable to find\s+([\w\.\-]+:[\w\.\-]+)`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(text); m != nil {
			raw := strings.TrimSpace(m[1])
			raw = strings.TrimSuffix(raw, ".jar")
			// com/foo/Bar → com.foo.Bar
			raw = strings.ReplaceAll(raw, "/", ".")
			raw = strings.TrimSuffix(raw, ".")
			// 去掉内部类后缀
			if idx := strings.Index(raw, "$"); idx > 0 {
				raw = raw[:idx]
			}
			if strings.Contains(raw, ":") {
				parts := strings.SplitN(raw, ":", 2)
				return parts[0], parts[1]
			}
			return raw, ""
		}
	}
	return "", ""
}

// splitJavaCoordinate 把包名拆成 Maven 坐标（启发式）。
func splitJavaCoordinate(pkg string) (groupID, artifactID string) {
	segments := strings.Split(pkg, ".")
	if len(segments) == 0 {
		return "", ""
	}
	// 末段通常是类名（首字母大写）。
	if len(segments) > 1 && isUpperFirst(segments[len(segments)-1]) {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return "", ""
	}
	artifactID = segments[len(segments)-1]
	groupID = strings.Join(segments, ".")
	return groupID, artifactID
}

// isUpperFirst 判断首字母是否大写。
func isUpperFirst(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z'
}

// buildDependencyInsertion 依据清单类型生成依赖插入补丁（供修复技能与依赖修复技能复用）。
func buildDependencyInsertion(lines []string, path, pkg, groupID, artifactID, version string) (oldSnippet, newSnippet string, line int, rationale string, ok bool) {
	if len(lines) == 0 {
		return "", "", 0, "", false
	}
	base := strings.ToLower(baseName(path))
	switch {
	case base == "pom.xml":
		for i, l := range lines {
			if !rePomDeps.MatchString(l) {
				continue
			}
			indent := indentOf(l) + "    "
			inner := indent + "    "
			inserted := []string{
				indent + "<dependency>",
				inner + "<groupId>" + orDefault(groupID, pkg) + "</groupId>",
				inner + "<artifactId>" + orDefault(artifactID, pkg) + "</artifactId>",
				inner + "<version>" + orDefault(version, "1.0.0") + "</version>",
				indent + "</dependency>",
			}
			start, end := expandWindow(len(lines), i+1, 1, 2)
			return windowText(lines, start, end), windowWithInsert(lines, start, end, i+1, inserted), i + 2,
				"在 pom.xml 的 <dependencies> 中补充缺失依赖 " + orDefault(groupID, pkg) + ":" + orDefault(artifactID, pkg) + "（版本号需按实际私服/GAV 确认后再合并）。", true
		}
	case base == "package.json":
		for i, l := range lines {
			m := rePackageJSONDep.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			indent := m[1] + "  "
			inserted := []string{indent + "\"" + npmName(pkg) + "\": \"" + npmRange(version) + "\","}
			start, end := expandWindow(len(lines), i+1, 1, 2)
			return windowText(lines, start, end), windowWithInsert(lines, start, end, i+1, inserted), i + 2,
				"在 package.json 的 " + m[2] + " 中补充缺失依赖 " + npmName(pkg) + "（建议随后执行 npm install 锁定实际版本）。", true
		}
	case base == "requirements.txt":
		insertAt := len(lines)
		start, end := expandWindow(len(lines), len(lines)-1, 2, 0)
		inserted := []string{pythonRequirement(pkg, version)}
		return windowText(lines, start, end), windowWithInsert(lines, start, end, insertAt, inserted), insertAt,
			"在 requirements.txt 末尾补充缺失依赖 " + pythonRequirement(pkg, version) + "（版本约束需按环境确认）。", true
	case base == "go.mod":
		for i, l := range lines {
			if reGoModRequire.MatchString(strings.TrimSpace(l)) {
				indent := indentOf(l) + "\t"
				inserted := []string{indent + goModulePath(pkg) + " " + orDefault(version, "v1.0.0")}
				start, end := expandWindow(len(lines), i+1, 1, 2)
				return windowText(lines, start, end), windowWithInsert(lines, start, end, i+1, inserted), i + 2,
					"在 go.mod 的 require 块中补充缺失模块 " + goModulePath(pkg) + "（版本建议用 go get 解析 latest 后回写）。", true
			}
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if reGoModRequireLn.MatchString(strings.TrimSpace(lines[i])) {
				inserted := []string{"require " + goModulePath(pkg) + " " + orDefault(version, "v1.0.0")}
				start, end := expandWindow(len(lines), i, 1, 1)
				return windowText(lines, start, end), windowWithInsert(lines, start, end, i+1, inserted), i + 2,
					"在 go.mod 中新增 require " + goModulePath(pkg) + "（版本建议用 go get 解析 latest 后回写）。", true
			}
		}
		insertAt := len(lines)
		start, end := expandWindow(len(lines), len(lines)-1, 2, 0)
		inserted := []string{"require " + goModulePath(pkg) + " " + orDefault(version, "v1.0.0")}
		return windowText(lines, start, end), windowWithInsert(lines, start, end, insertAt, inserted), insertAt,
			"在 go.mod 末尾新增 require " + goModulePath(pkg) + "（版本建议用 go get 解析 latest 后回写）。", true
	case base == "build.gradle" || base == "build.gradle.kts":
		for i, l := range lines {
			m := reGradleDeps.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			indent := m[1] + "    "
			inserted := []string{indent + "implementation '" + orDefault(groupID, pkg) + ":" + orDefault(artifactID, pkg) + ":" + orDefault(version, "1.0.0") + "'"}
			start, end := expandWindow(len(lines), i+1, 1, 2)
			return windowText(lines, start, end), windowWithInsert(lines, start, end, i+1, inserted), i + 2,
				"在 build.gradle 的 dependencies 中补充缺失依赖 " + orDefault(groupID, pkg) + ":" + orDefault(artifactID, pkg) + "（版本号需确认）。", true
		}
	}
	return "", "", 0, "", false
}

// orDefault 返回首选值，为空时用兜底值。
func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// npmName 归一化 npm 包名。
func npmName(pkg string) string {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return "unknown-package"
	}
	// Java 风格包名退化为最后一段。
	if strings.Contains(pkg, ".") && !strings.HasPrefix(pkg, "@") {
		parts := strings.Split(pkg, ".")
		return strings.ToLower(parts[len(parts)-1])
	}
	return pkg
}

// npmRange 生成 npm 版本范围。
func npmRange(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return "^1.0.0"
	}
	if strings.HasPrefix(v, "^") || strings.HasPrefix(v, "~") || v == "latest" || v == "*" {
		return v
	}
	return "^" + v
}

// pythonRequirement 生成 requirements.txt 行。
func pythonRequirement(pkg, version string) string {
	name := strings.ToLower(strings.Split(pkg, ".")[0])
	v := strings.TrimSpace(version)
	if v == "" {
		return name
	}
	return name + ">=" + v
}

// goModulePath 生成 go.mod 模块路径。
func goModulePath(pkg string) string {
	p := strings.TrimSpace(pkg)
	if strings.Contains(p, "/") {
		return p
	}
	return "github.com/" + strings.ToLower(p)
}

// ---------------------------------------------------------------------------
// 规则 5：除零
// ---------------------------------------------------------------------------

// synthDivideByZero 除零保护规则。
func synthDivideByZero(pb *patchBuilder, lang string) {
	for i, line := range pb.lines {
		code := stripStringLiterals(line)
		trimmed := strings.TrimSpace(code)
		if trimmed == "" || reComment.MatchString(trimmed) {
			continue
		}
		if strings.Contains(line, "://") || strings.Contains(line, "/=") || strings.Contains(line, "http") {
			continue
		}
		m := reDivide.FindStringSubmatch(code)
		if m == nil {
			continue
		}
		divisor := m[2]
		if isNumericLiteral(divisor) {
			continue
		}
		if hasZeroGuardNear(pb.lines, i, divisor) {
			continue
		}
		indent := indentOf(line)
		var guard []string
		switch lang {
		case "java":
			guard = []string{
				indent + "if (" + divisor + " == 0) {",
				indent + "    throw new ArithmeticException(\"除数不能为 0: " + divisor + "\");",
				indent + "}",
			}
		case "go":
			guard = []string{indent + "if " + divisor + " == 0 {"}
			guard = append(guard, goReturnSnippet(pb.lines, i, "\"除数不能为 0: "+divisor+"\"")...)
			guard = append(guard, indent+"}")
		case "python":
			guard = []string{
				indent + "if " + divisor + " == 0:",
				indent + "    raise ZeroDivisionError(\"除数不能为 0: " + divisor + "\")",
			}
		case "node":
			guard = []string{
				indent + "if (" + divisor + " === 0) {",
				indent + "    throw new Error(\"除数不能为 0: " + divisor + "\");",
				indent + "}",
			}
		default:
			continue
		}
		d := patchDraft{
			RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
			Rationale:  "除法运算的除数 " + divisor + " 未做零值校验，存在除零异常风险；补充校验并显式失败。",
			Risk:       riskLowest,
			Confidence: 0.6,
			Rule:       ruleDivideByZero,
		}
		pb.insert(d, i, i, 1, 1, guard)
	}
}

// isNumericLiteral 判断是否为数字字面量。
func isNumericLiteral(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// hasZeroGuardNear 判断附近是否已有除零保护。
func hasZeroGuardNear(lines []string, idx int, divisor string) bool {
	start := idx - 6
	if start < 0 {
		start = 0
	}
	for i := start; i < idx; i++ {
		line := lines[i]
		if strings.Contains(line, divisor) && (strings.Contains(line, "== 0") ||
			strings.Contains(line, "=== 0") || strings.Contains(line, "!= 0") || strings.Contains(line, "> 0")) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 规则 6：并发修改
// ---------------------------------------------------------------------------

// synthConcurrent 并发安全规则（Java 并发容器 / Go map 加锁）。
func synthConcurrent(pb *patchBuilder, lang string) {
	for i, line := range pb.lines {
		code := stripStringLiterals(line)
		switch lang {
		case "java":
			if reJavaHashMap.MatchString(code) && !strings.Contains(code, "ConcurrentHashMap") {
				replaced := reJavaHashMap.ReplaceAllString(line, "ConcurrentHashMap")
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "HashMap 在多线程读写场景会触发 ConcurrentModificationException 或数据丢失，替换为 ConcurrentHashMap（注意：不允许 null key/value）。",
					Risk:       riskMid,
					Confidence: 0.55,
					Rule:       ruleConcurrent,
				}
				pb.replace(d, i, i, 1, 1, []string{replaced})
				continue
			}
			if reJavaArrayList.MatchString(code) && !strings.Contains(code, "CopyOnWriteArrayList") &&
				(strings.Contains(code, "static") || strings.Contains(code, "private") || strings.Contains(code, "public")) {
				replaced := reJavaArrayList.ReplaceAllString(line, "CopyOnWriteArrayList")
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "ArrayList 作为共享集合在并发读写时会抛 ConcurrentModificationException，替换为 CopyOnWriteArrayList（写多读少场景请改用手动同步）。",
					Risk:       riskMid,
					Confidence: 0.5,
					Rule:       ruleConcurrent,
				}
				pb.replace(d, i, i, 1, 1, []string{replaced})
			}
		case "go":
			if m := reGoMapField.FindStringSubmatch(code); m != nil && insideStruct(pb.lines, i) &&
				!strings.Contains(strings.Join(pb.lines[maxInt(0, i-3):i], "\n"), "sync.Mutex") {
				indent := indentOf(line)
				inserted := []string{indent + "mu sync.Mutex // 并发保护：读写 " + m[1] + " 前请先加锁"}
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "map 字段 " + m[1] + " 在并发读写时会触发 fatal error: concurrent map writes，补充 sync.Mutex 保护（需在访问处加锁）。",
					Risk:       riskMid,
					Confidence: 0.5,
					Rule:       ruleConcurrent,
				}
				pb.insert(d, i, i, 1, 1, inserted)
			}
		}
	}
}

// insideStruct 判断命中行是否位于 Go 结构体定义内。
func insideStruct(lines []string, idx int) bool {
	for i := idx; i >= 0 && i > idx-40; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "func ") {
			return false
		}
		if strings.Contains(trimmed, "struct {") {
			return true
		}
	}
	return false
}

// maxInt 返回较大值。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// 规则 7：资源泄漏
// ---------------------------------------------------------------------------

// synthResourceLeak 资源释放规则（try-with-resources / defer close / with open）。
func synthResourceLeak(pb *patchBuilder, lang string) {
	for i, line := range pb.lines {
		if hasReleaseAround(pb.lines, i) {
			continue
		}
		switch lang {
		case "java":
			if m := reJavaResource.FindStringSubmatch(line); m != nil {
				indent := indentOf(line)
				decl := strings.TrimSpace(line)
				// 收集紧随其后的 1-2 行，纳入 try-with-resources 代码块。
				body := []string{indent + "    " + decl}
				last := i
				for j := i + 1; j < len(pb.lines) && j <= i+2; j++ {
					next := pb.lines[j]
					trimmedNext := strings.TrimSpace(next)
					if trimmedNext == "" || trimmedNext == "}" || strings.HasPrefix(trimmedNext, "//") {
						break
					}
					body = append(body, indent+"    "+trimmedNext)
					last = j
				}
				replaced := []string{indent + "try (" + decl + ") {"}
				replaced = append(replaced, body[1:]...)
				replaced = append(replaced, indent+"} catch (java.io.IOException e) {",
					indent+"    throw new IllegalStateException(\"资源读取失败\", e);", indent+"}")
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "资源 " + m[2] + "（" + m[3] + "）创建后未见关闭动作，改为 try-with-resources 自动释放，避免句柄泄漏。",
					Risk:       riskMid,
					Confidence: 0.55,
					Rule:       ruleResourceLeak,
				}
				pb.replace(d, i, i, 0, last-i, replaced)
				continue
			}
		case "go":
			if m := reGoResource.FindStringSubmatch(line); m != nil {
				varName := m[2]
				insertAt := i + 1
				// 若下一行是 err 判断，则把 defer 放到 err 判断之后，避免对 nil 响应做 Close。
				for j := i + 1; j < len(pb.lines) && j <= i+3; j++ {
					trimmed := strings.TrimSpace(pb.lines[j])
					if strings.HasPrefix(trimmed, "if err != nil") {
						insertAt = j + 1
						if trimmed == "if err != nil {" {
							// 跳过代码块闭合行
							for k := j + 1; k < len(pb.lines); k++ {
								if strings.TrimSpace(pb.lines[k]) == "}" {
									insertAt = k + 1
									break
								}
							}
						}
						break
					}
				}
				indent := indentOf(line)
				call := "defer " + varName + ".Close()"
				if strings.Contains(m[3], "http.") {
					call = "defer " + varName + ".Body.Close()"
				}
				inserted := []string{indent + call + " // 确保资源释放，避免句柄泄漏"}
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "通过 " + m[3] + " 获取的资源 " + varName + " 未见释放，补充 " + call + "（放在错误处理之后，避免对空响应调用 Close）。",
					Risk:       riskLowest,
					Confidence: 0.65,
					Rule:       ruleResourceLeak,
				}
				pb.insert(d, insertAt, insertAt, 1, 1, inserted)
			}
		case "python":
			if m := rePyResource.FindStringSubmatch(line); m != nil {
				indent, varName := m[1], m[2]
				body := []string{}
				last := i
				for j := i + 1; j < len(pb.lines) && j <= i+2; j++ {
					trimmed := strings.TrimSpace(pb.lines[j])
					if trimmed == "" {
						break
					}
					body = append(body, indent+"    "+trimmed)
					last = j
				}
				if len(body) == 0 {
					continue
				}
				replaced := []string{indent + "with open(" + pythonOpenArgs(strings.TrimSpace(line)) + ") as " + varName + ":"}
				replaced = append(replaced, body...)
				d := patchDraft{
					RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
					Rationale:  "文件对象 " + varName + " 未使用 with 语句管理，异常路径下不会关闭；改为 with open(...) 上下文管理。",
					Risk:       riskMid,
					Confidence: 0.55,
					Rule:       ruleResourceLeak,
				}
				pb.replace(d, i, i, 0, last-i, replaced)
			}
		}
	}
}

// pythonOpenArgs 提取 open(...) 的参数部分。
func pythonOpenArgs(decl string) string {
	idx := strings.Index(decl, "open(")
	if idx < 0 {
		return ""
	}
	rest := decl[idx+len("open("):]
	if end := strings.LastIndex(rest, ")"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// ---------------------------------------------------------------------------
// 规则 8：超时缺失
// ---------------------------------------------------------------------------

// synthTimeout 超时保护规则（axios / Go http / Java RestTemplate）。
func synthTimeout(pb *patchBuilder, lang string) {
	for i, line := range pb.lines {
		switch lang {
		case "node":
			m := reAxiosCall.FindStringSubmatch(line)
			if m == nil || strings.Contains(line, "timeout") {
				continue
			}
			replaced := line[:strings.Index(line, m[1])] + m[1] + m[2] + ", { timeout: 5000 }" + m[3]
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "axios 请求未设置超时，网络抖动时会长时间挂起；补充 timeout: 5000（可按接口 SLA 调整）。",
				Risk:       riskLowest,
				Confidence: 0.6,
				Rule:       ruleTimeout,
			}
			pb.replace(d, i, i, 1, 1, []string{replaced})
		case "go":
			if !reGoHTTPGet.MatchString(line) || strings.Contains(line, "client.") {
				continue
			}
			indent := indentOf(line)
			clientLine := indent + "client := &http.Client{Timeout: 5 * time.Second} // 避免无超时请求长期阻塞（需 import \"time\"）"
			replacedLine := reGoHTTPGet.ReplaceAllString(line, "client.$1(")
			start, end := expandWindow(len(pb.lines), i, 1, 1)
			if i < start {
				start = i
			}
			if i > end {
				end = i
			}
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "http.Get/Post/Do 使用默认客户端且无超时，下游不可用时会长期阻塞并耗尽连接；改为带 Timeout 的 http.Client。",
				Risk:       riskMid,
				Confidence: 0.55,
				Rule:       ruleTimeout,
				Line:       i + 1,
			}
			old := windowText(pb.lines, start, end)
			newText := windowWithReplace(pb.lines, start, end, i, []string{clientLine, replacedLine})
			pb.commit(d, old, newText)
			// 同一文件只补充一次 client 声明，避免重复定义变量。
			return
		case "java":
			m := reJavaTemplate.FindStringSubmatch(line)
			if m == nil || strings.Contains(line, "factory") {
				continue
			}
			indent, varName := indentOf(line), m[1]
			replaced := []string{
				indent + "SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();",
				indent + "factory.setConnectTimeout(3000);",
				indent + "factory.setReadTimeout(5000);",
				indent + "RestTemplate " + varName + " = new RestTemplate(factory);",
			}
			d := patchDraft{
				RepositoryID: pb.repoID, RepoKey: pb.repoKey, FilePath: pb.path,
				Rationale:  "RestTemplate 使用默认无超时配置，下游抖动会拖垮线程池；补充连接/读取超时（需 import org.springframework.http.client.SimpleClientHttpRequestFactory）。",
				Risk:       riskMid,
				Confidence: 0.55,
				Rule:       ruleTimeout,
			}
			pb.replace(d, i, i, 1, 1, replaced)
		}
	}
}
