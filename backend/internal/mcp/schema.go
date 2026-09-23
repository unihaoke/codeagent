package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
)

// ErrUnstructuredOutput 表示模型输出无法被解析为满足约束的结构化 JSON。
//
// 调用方应使用 errors.Is(err, ErrUnstructuredOutput) 判断该错误，
// 并据此决定是否触发纠错重试或降级为纯文本处理。
var ErrUnstructuredOutput = errors.New("模型输出无法解析为结构化 JSON")

// ---------------------------------------------------------------------------
// 枚举常量（Schema 与 Mock 共用，保证"模型产出"始终落在合法取值范围内）
// ---------------------------------------------------------------------------

// categoryEnum 异常/根因分类枚举，覆盖堆栈文本可直接判定的常见故障类型。
var categoryEnum = []any{
	"null_pointer",        // 空指针 / nil 解引用
	"index_out_of_bounds", // 数组/列表越界
	"type_error",          // 类型错误（undefined/null 属性访问、非函数调用）
	"dependency_missing",  // 依赖缺失（类/模块找不到）
	"timeout",             // 超时
	"syntax_error",        // 语法/解析错误
	"runtime_error",       // 其它运行时异常
	"logic_error",         // 业务逻辑错误
	"config_error",        // 配置/权限/认证错误
	"network_error",       // 网络不可达
	"resource_exhausted",  // 资源耗尽（内存/句柄/磁盘）
	"concurrency_error",   // 并发问题（死锁、竞态）
	"serialization_error", // 序列化/反序列化错误
	"unknown",             // 无法判定
}

// severityEnum 问题定级枚举，与 domain.Severity 保持一致。
var severityEnum = []any{"blocker", "critical", "major", "minor", "info"}

// riskEnum 补丁风险枚举，与 domain.PatchRisk 保持一致。
var riskEnum = []any{"low", "medium", "high"}

// actionEnum 补丁动作枚举，与 domain.PatchAction 保持一致。
var actionEnum = []any{"modify", "create", "delete"}

// ---------------------------------------------------------------------------
// JSON Schema 定义
//
// 说明：Schema 按"严格"编写（required 覆盖关键字段、enum 约束分类/风险/定级、
// additionalProperties=false），但 ValidateAgainst 在**校验时忽略多余字段**，
// 以免模型多输出一个字段就整条链路失败（宽松校验 + 严格提示词）。
// ---------------------------------------------------------------------------

// RootCauseSchema 根因分析（root_cause）阶段的结构化输出约束。
var RootCauseSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"summary", "category", "detail", "confidence", "evidence", "blastRadius", "severity"},
	"properties": map[string]any{
		"summary":     map[string]any{"type": "string"},
		"category":    map[string]any{"type": "string", "enum": categoryEnum},
		"detail":      map[string]any{"type": "string"},
		"confidence":  map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
		"evidence":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"blastRadius": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"severity":    map[string]any{"type": "string", "enum": severityEnum},
	},
}

// PatchSchema 补丁生成（patch_synthesize）阶段的结构化输出约束。
//
// patches 中每一项都必须给出可直接定位到源码的 oldSnippet / newSnippet，
// 供沙箱执行层做增量校验（禁止整文件重写）。
var PatchSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"patches", "rationale", "risk"},
	"properties": map[string]any{
		"patches": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required": []any{
					"repositoryId", "repoKey", "filePath", "action",
					"line", "oldSnippet", "newSnippet", "rationale", "risk", "confidence",
				},
				"properties": map[string]any{
					"repositoryId": map[string]any{"type": "string"},
					"repoKey":      map[string]any{"type": "string"},
					"filePath":     map[string]any{"type": "string"},
					"action":       map[string]any{"type": "string", "enum": actionEnum},
					"line":         map[string]any{"type": "integer", "minimum": 1.0},
					"oldSnippet":   map[string]any{"type": "string"},
					"newSnippet":   map[string]any{"type": "string"},
					"rationale":    map[string]any{"type": "string"},
					"risk":         map[string]any{"type": "string", "enum": riskEnum},
					"confidence":   map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
				},
			},
		},
		"rationale": map[string]any{"type": "string"},
		"risk":      map[string]any{"type": "string", "enum": riskEnum},
	},
}

// ClassifySchema 故障分类（classify）阶段的结构化输出约束。
var ClassifySchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"category", "severity", "confidence"},
	"properties": map[string]any{
		"category":   map[string]any{"type": "string", "enum": categoryEnum},
		"severity":   map[string]any{"type": "string", "enum": severityEnum},
		"confidence": map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
		"reason":     map[string]any{"type": "string"},
	},
}

// SummarySchema 结论汇总（summary）阶段的结构化输出约束。
var SummarySchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"summary", "actions"},
	"properties": map[string]any{
		"summary":    map[string]any{"type": "string"},
		"actions":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"highlights": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}

// VerifyExplainSchema 验证解释（verify_explain）阶段的结构化输出约束。
var VerifyExplainSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"explanation", "nextActions"},
	"properties": map[string]any{
		"explanation": map[string]any{"type": "string"},
		"nextActions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"passed":      map[string]any{"type": "boolean"},
	},
}

// LogCleanSchema 日志清洗（log_clean）阶段的结构化输出约束。
//
// 需求约定的五个 Schema 之外补充本项：清洗阶段同样需要结构化输出，
// 且 prompt.BuildMessages 要求任一 stage 都返回非空 Schema。
var LogCleanSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []any{"cleaned", "noiseRatio"},
	"properties": map[string]any{
		"cleaned":    map[string]any{"type": "string"},
		"noiseRatio": map[string]any{"type": "number", "minimum": 0.0, "maximum": 1.0},
		"removed":    map[string]any{"type": "integer", "minimum": 0.0},
		"kept":       map[string]any{"type": "integer", "minimum": 0.0},
	},
}

// ---------------------------------------------------------------------------
// 结构化输出解析
// ---------------------------------------------------------------------------

// ParseStructured 把模型返回的文本解析为结构化 JSON 对象，并按 schema 做轻量校验。
//
// 解析按以下顺序逐层尝试（任一层成功即返回）：
//
//	① 直接 JSON 解析；
//	② 剥离 ```json ... ``` / ``` ... ``` 代码围栏后再解析；
//	③ 用括号配对（正确处理字符串与转义）抽取第一个平衡的 {...} 块；
//	④ 常见修复：单引号→双引号、尾随逗号、None/True/False→null/true/false、中文全角引号；
//	⑤ 以上全部失败返回 ErrUnstructuredOutput。
//
// schema 非空时校验失败同样返回 ErrUnstructuredOutput（错误信息包含中文校验明细），
// 校验会**忽略多余字段**，因此模型多输出字段不会导致失败。
func ParseStructured(text string, schema map[string]any) (map[string]any, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if raw == "" {
		return nil, fmt.Errorf("%w: 模型输出为空", ErrUnstructuredOutput)
	}

	var problems []string
	for _, cand := range extractCandidates(raw) {
		doc, err := decodeJSONObject(cand)
		if err != nil {
			problems = appendUnique(problems, err.Error())
			continue
		}
		if schema != nil {
			if verrs := ValidateAgainst(schema, doc); len(verrs) > 0 {
				problems = appendUnique(problems, strings.Join(verrs, "；"))
				continue
			}
		}
		return doc, nil
	}
	if len(problems) == 0 {
		problems = append(problems, "未找到可解析的 JSON 对象")
	}
	return nil, fmt.Errorf("%w: %s", ErrUnstructuredOutput, truncateRunes(strings.Join(problems, " | "), 300))
}

// extractCandidates 生成按"可信度从高到低"排序的候选文本。
func extractCandidates(raw string) []string {
	out := make([]string, 0, 8)
	seen := make(map[string]bool, 8)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	add(raw)
	stripped := stripCodeFences(raw)
	add(stripped)
	for _, base := range []string{raw, stripped} {
		if block, ok := firstBalancedObject(base); ok {
			add(block)
		}
	}
	// 修复变体：先修文本本身，再修"抽取出的平衡块"。
	for _, base := range append([]string(nil), out...) {
		repaired := repairJSON(base)
		add(repaired)
		if block, ok := firstBalancedObject(repaired); ok {
			add(block)
		}
	}
	return out
}

// stripCodeFences 剥离 Markdown 代码围栏，返回围栏内部内容。
// 没有围栏时原样返回；只有起始围栏时返回其后全部内容。
func stripCodeFences(s string) string {
	idx := strings.Index(s, "```")
	if idx < 0 {
		return s
	}
	rest := s[idx+3:]
	// 跳过语言标记（json / JSON / jsonc 等）：到第一个换行为止。
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		lang := strings.TrimSpace(rest[:nl])
		if lang == "" || !strings.ContainsAny(lang, "{}[]\"'") {
			rest = rest[nl+1:]
		}
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// firstBalancedObject 抽取文本中第一个括号平衡的 {...} 块。
// 自行实现括号配对，正确处理字符串字面量与反斜杠转义（含字符串内的 }）。
func firstBalancedObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

var (
	// 单引号字符串（不含双引号，避免破坏已经合法的 JSON）。
	reSingleQuoted = regexp.MustCompile(`'([^'\n]*)'`)
	// 尾随逗号：{"a":1,} / [1,2,]
	reTrailingComma = regexp.MustCompile(`,\s*([}\]])`)
)

// repairJSON 对常见非法 JSON 做保守修复。
func repairJSON(s string) string {
	out := s
	// 中文全角引号 → 半角引号。
	out = strings.NewReplacer("“", "\"", "”", "\"", "‘", "'", "’", "'").Replace(out)
	// 单引号 → 双引号（仅在内容中不含双引号时替换，避免破坏合法 JSON）。
	out = reSingleQuoted.ReplaceAllStringFunc(out, func(m string) string {
		inner := m[1 : len(m)-1]
		if strings.Contains(inner, `"`) || strings.Contains(inner, `\`) {
			return m
		}
		return `"` + inner + `"`
	})
	// 尾随逗号（最多迭代 3 轮，处理 },, / ],} 等组合）。
	for i := 0; i < 3; i++ {
		fixed := reTrailingComma.ReplaceAllString(out, "$1")
		if fixed == out {
			break
		}
		out = fixed
	}
	// Python 风格字面量（仅在字符串外替换，避免污染中文/英文正文）。
	out = replaceBareLiterals(out)
	return out
}

// replaceBareLiterals 把字符串外的 None/True/False 替换为 JSON 字面量。
func replaceBareLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for i := 0; i < len(s); {
		c := s[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			i++
			continue
		}
		if repl, n, ok := matchLiteralAt(s, i); ok {
			b.WriteString(repl)
			i += n
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

// matchLiteralAt 判断位置 i 是否为一个独立的 Python 字面量。
func matchLiteralAt(s string, i int) (string, int, bool) {
	for _, pair := range [][2]string{{"None", "null"}, {"True", "true"}, {"False", "false"}} {
		word, repl := pair[0], pair[1]
		if !strings.HasPrefix(s[i:], word) {
			continue
		}
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		end := i + len(word)
		if end < len(s) && isWordByte(s[end]) {
			continue
		}
		return repl, len(word), true
	}
	return "", 0, false
}

func isWordByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// decodeJSONObject 把候选文本解码为 JSON 对象。
func decodeJSONObject(s string) (map[string]any, error) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %v", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("JSON 顶层不是对象（实际为 %s）", describeType(v))
	}
	return obj, nil
}

// ---------------------------------------------------------------------------
// 轻量 Schema 校验
// ---------------------------------------------------------------------------

// ValidateAgainst 对结构化结果做轻量 Schema 校验（type / required / enum / items / properties）。
//
// 返回中文错误列表，空列表表示通过。
// 与严格校验器的差异（有意为之）：
//   - additionalProperties=false 仅作为提示词约束，校验时**忽略多余字段**；
//   - minimum / maximum / minLength 等数值与长度约束不做校验，避免模型给出的
//     近似合法结果被直接判死，交由上层业务做兜底归一化。
func ValidateAgainst(schema map[string]any, doc map[string]any) []string {
	if schema == nil || doc == nil {
		return nil
	}
	return validateValue("", schema, doc)
}

// validateValue 递归校验单个值；path 为人类可读字段路径（根为空）。
func validateValue(path string, schema map[string]any, val any) []string {
	if len(schema) == 0 {
		return nil
	}
	var errs []string

	// oneOf / anyOf：任一分支通过即视为通过。
	for _, key := range []string{"oneOf", "anyOf"} {
		branches := schemaObjectList(schema[key])
		if len(branches) == 0 {
			continue
		}
		passed := false
		for _, br := range branches {
			if len(validateValue(path, br, val)) == 0 {
				passed = true
				break
			}
		}
		if !passed {
			errs = append(errs, fmt.Sprintf("字段 %q 不满足 %s 中的任一约束", displayPath(path), key))
		}
	}

	if want, ok := schema["type"]; ok {
		names := typeNames(want)
		if len(names) > 0 {
			matched := false
			for _, t := range names {
				if typeMatches(t, val) {
					matched = true
					break
				}
			}
			if !matched {
				errs = append(errs, fmt.Sprintf("字段 %q 类型应为 %s，实际为 %s",
					displayPath(path), strings.Join(names, "|"), describeType(val)))
			}
		}
	}

	if enum := schemaValueList(schema["enum"]); len(enum) > 0 {
		if !enumContains(enum, val) {
			errs = append(errs, fmt.Sprintf("字段 %q 取值 %s 不在允许的枚举 %s 中",
				displayPath(path), describeValue(val), describeEnum(enum)))
		}
	}

	if obj, ok := asMap(val); ok {
		for _, r := range schemaStringList(schema["required"]) {
			v, exists := obj[r]
			if !exists || v == nil {
				errs = append(errs, fmt.Sprintf("缺少必填字段 %q", displayPath(childPath(path, r))))
			}
		}
		if props, ok := asMap(schema["properties"]); ok {
			for k, sub := range props {
				v, exists := obj[k]
				if !exists || v == nil {
					continue
				}
				subSchema, _ := asMap(sub)
				errs = append(errs, validateValue(childPath(path, k), subSchema, v)...)
			}
		}
	}

	if arr, ok := asSlice(val); ok {
		if items, ok := asMap(schema["items"]); ok {
			for i, el := range arr {
				errs = append(errs, validateValue(fmt.Sprintf("%s[%d]", path, i), items, el)...)
			}
		}
	}

	return errs
}

// childPath 拼接字段路径。
func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// displayPath 把空路径（文档根）显示为 "$"。
func displayPath(path string) string {
	if path == "" {
		return "$"
	}
	return path
}

// typeNames 归一化 type 约束（字符串或字符串数组）。
func typeNames(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// typeMatches 判断值是否符合 JSON Schema 的基础类型。
func typeMatches(want string, val any) bool {
	switch want {
	case "object":
		_, ok := asMap(val)
		return ok
	case "array":
		_, ok := asSlice(val)
		return ok
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number":
		_, ok := toFloat(val)
		return ok
	case "integer":
		f, ok := toFloat(val)
		return ok && math.Trunc(f) == f
	case "null":
		return val == nil
	}
	return true // 未知类型关键字不做限制
}

// asMap 归一化为 map[string]any。
func asMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = val
		}
		return out, true
	}
	return nil, false
}

// asSlice 归一化为 []any（支持任意切片/数组类型）。
func asSlice(v any) ([]any, bool) {
	if v == nil {
		return nil, false
	}
	if s, ok := v.([]any); ok {
		return s, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// toFloat 把数值类型归一化为 float64。
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// schemaValueList 归一化 enum 列表。
func schemaValueList(v any) []any {
	if v == nil {
		return nil
	}
	list, ok := asSlice(v)
	if !ok {
		return nil
	}
	return list
}

// schemaStringList 归一化字符串数组约束（如 required）。
func schemaStringList(v any) []string {
	list, ok := asSlice(v)
	if !ok {
		if s, ok := v.(string); ok {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// schemaObjectList 归一化子 Schema 列表（oneOf / anyOf）。
func schemaObjectList(v any) []map[string]any {
	list, ok := asSlice(v)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := asMap(e); ok {
			out = append(out, m)
		}
	}
	return out
}

// enumContains 判断值是否命中枚举（字符串与数值分别归一化比较）。
func enumContains(enum []any, val any) bool {
	for _, e := range enum {
		if es, ok := e.(string); ok {
			if vs, ok := val.(string); ok && es == vs {
				return true
			}
			continue
		}
		if ef, ok := toFloat(e); ok {
			if vf, ok := toFloat(val); ok && ef == vf {
				return true
			}
			continue
		}
		if reflect.DeepEqual(e, val) {
			return true
		}
	}
	return false
}

// describeType 返回中文类型描述。
func describeType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if _, ok := asMap(v); ok {
		return "object"
	}
	if _, ok := asSlice(v); ok {
		return "array"
	}
	if f, ok := toFloat(v); ok {
		if math.Trunc(f) == f {
			return "number(整数)"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

// describeValue 返回简短的值描述，用于错误信息。
func describeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", truncateRunes(t, 32))
	}
	return fmt.Sprintf("%v", v)
}

// describeEnum 返回枚举的简短展示。
func describeEnum(enum []any) string {
	parts := make([]string, 0, len(enum))
	for i, e := range enum {
		if i >= 8 {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, describeValue(e))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// appendUnique 去重追加，避免错误信息被同类问题刷屏。
func appendUnique(list []string, item string) []string {
	for _, s := range list {
		if s == item {
			return list
		}
	}
	if len(list) >= 6 {
		return list
	}
	return append(list, item)
}

// truncateRunes 按字符（rune）安全截断文本。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
