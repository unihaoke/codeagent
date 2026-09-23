package skill

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Validator JSON Schema 子集校验器。
//
// 本校验器完全基于标准库实现（不引入任何 JSON Schema 第三方库），覆盖内置技能
// 契约所需的全部关键字：type（含多类型数组）/ required / properties /
// additionalProperties(bool) / items / enum / const / oneOf / anyOf /
// minimum / maximum / minLength / maxLength / minItems / maxItems / pattern / default。
//
// 设计约定：
//   - schema 为 nil 时直接通过（不产生任何错误）；
//   - 未知关键字一律忽略，保证 schema 向前兼容；
//   - 错误信息使用中文，便于直接透出到 HTTP 422 响应与前端。
type Validator struct{}

// SchemaError 单条 Schema 校验错误。
type SchemaError struct {
	// Path 出错字段路径，嵌套字段使用 a.b 形式，数组元素使用 a[0] 形式；根文档为空串。
	Path string `json:"path"`
	// Message 中文错误信息，例如「字段 stacktrace 必填」。
	Message string `json:"message"`
}

// Error 实现 error 接口，返回可读的中文错误信息。
func (e SchemaError) Error() string { return e.Message }

// Validate 依据 schema 校验 doc；返回全部校验错误（空切片表示通过）。
func (v *Validator) Validate(schema map[string]any, doc any) []SchemaError {
	if schema == nil {
		return nil
	}
	var errs []SchemaError
	validateNode(schema, doc, "", &errs)
	return errs
}

// ValidateAndFill 校验并填充 default 值。
//
// 填充在 doc 上原地生效：对象缺失字段若 schema 声明了 default 则写入默认值，
// 并递归处理 properties 与 items。填充后再执行标准校验。
func (v *Validator) ValidateAndFill(schema map[string]any, doc map[string]any) []SchemaError {
	if schema == nil {
		return nil
	}
	if doc != nil {
		fillDefaults(schema, doc)
	}
	var errs []SchemaError
	validateNode(schema, doc, "", &errs)
	return errs
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// validateNode 递归校验单个节点。
func validateNode(schema map[string]any, doc any, path string, errs *[]SchemaError) {
	if schema == nil {
		return
	}

	// type：允许字符串或字符串数组两种写法。
	if raw, ok := schema["type"]; ok {
		if !matchType(raw, doc) {
			*errs = append(*errs, newErr(path, "类型必须是 %s，实际为 %s", describeTypes(raw), jsonType(doc)))
			return // 类型不符时后续关键字无意义，直接返回
		}
	}

	// const / enum
	if raw, ok := schema["const"]; ok && !deepEqualJSON(raw, doc) {
		*errs = append(*errs, newErr(path, "取值必须等于 %s", formatValue(raw)))
	}
	if raw, ok := schema["enum"]; ok {
		if list, ok2 := raw.([]any); ok2 && len(list) > 0 {
			hit := false
			for _, item := range list {
				if deepEqualJSON(item, doc) {
					hit = true
					break
				}
			}
			if !hit {
				*errs = append(*errs, newErr(path, "取值必须是 [%s] 之一", formatList(list)))
			}
		}
	}

	// oneOf / anyOf：子模式校验时使用临时错误缓冲，避免噪声错误外泄。
	if raw, ok := schema["oneOf"]; ok {
		if list, ok2 := raw.([]any); ok2 && len(list) > 0 {
			matched := 0
			for _, item := range list {
				sub, ok3 := item.(map[string]any)
				if !ok3 {
					continue
				}
				var tmp []SchemaError
				validateNode(sub, doc, path, &tmp)
				if len(tmp) == 0 {
					matched++
				}
			}
			if matched != 1 {
				*errs = append(*errs, newErr(path, "必须且只能匹配 oneOf 中的一种模式（当前匹配 %d 种）", matched))
			}
		}
	}
	if raw, ok := schema["anyOf"]; ok {
		if list, ok2 := raw.([]any); ok2 && len(list) > 0 {
			matched := 0
			for _, item := range list {
				sub, ok3 := item.(map[string]any)
				if !ok3 {
					continue
				}
				var tmp []SchemaError
				validateNode(sub, doc, path, &tmp)
				if len(tmp) == 0 {
					matched++
				}
			}
			if matched == 0 {
				*errs = append(*errs, newErr(path, "必须至少匹配 anyOf 中的一种模式"))
			}
		}
	}

	switch value := doc.(type) {
	case map[string]any:
		validateObject(schema, value, path, errs)
	case []any:
		validateArray(schema, value, path, errs)
	case string:
		validateString(schema, value, path, errs)
	}

	// 数字约束对所有数值类型生效（含 struct 字段经反射取值的情况不处理，保持轻量）。
	if f, ok := toFloat(doc); ok {
		if raw, ok2 := schema["minimum"]; ok2 {
			if min, ok3 := toFloat(raw); ok3 && f < min {
				*errs = append(*errs, newErr(path, "不得小于 %s（当前 %s）", formatValue(raw), formatFloat(f)))
			}
		}
		if raw, ok2 := schema["maximum"]; ok2 {
			if max, ok3 := toFloat(raw); ok3 && f > max {
				*errs = append(*errs, newErr(path, "不得大于 %s（当前 %s）", formatValue(raw), formatFloat(f)))
			}
		}
	}
}

// validateObject 校验对象节点的 required / properties / additionalProperties。
func validateObject(schema map[string]any, doc map[string]any, path string, errs *[]SchemaError) {
	if raw, ok := schema["required"]; ok {
		for _, name := range toStringList(raw) {
			if _, exists := doc[name]; !exists {
				*errs = append(*errs, newErr(joinPath(path, name), "必填"))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)
	for key, value := range doc {
		if props != nil {
			if sub, ok := props[key].(map[string]any); ok {
				validateNode(sub, value, joinPath(path, key), errs)
				continue
			}
		}
		if raw, ok := schema["additionalProperties"]; ok {
			if allow, isBool := raw.(bool); isBool && !allow {
				*errs = append(*errs, newErr(joinPath(path, key), "不允许出现未声明的字段"))
			}
		}
	}
}

// validateArray 校验数组节点的 items / minItems / maxItems。
func validateArray(schema map[string]any, doc []any, path string, errs *[]SchemaError) {
	if raw, ok := schema["minItems"]; ok {
		if n, ok2 := toInt(raw); ok2 && len(doc) < n {
			*errs = append(*errs, newErr(path, "元素个数不得少于 %d（当前 %d）", n, len(doc)))
		}
	}
	if raw, ok := schema["maxItems"]; ok {
		if n, ok2 := toInt(raw); ok2 && len(doc) > n {
			*errs = append(*errs, newErr(path, "元素个数不得多于 %d（当前 %d）", n, len(doc)))
		}
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		return
	}
	for i, item := range doc {
		validateNode(items, item, fmt.Sprintf("%s[%d]", path, i), errs)
	}
}

// validateString 校验字符串节点的 minLength / maxLength / pattern。
func validateString(schema map[string]any, doc string, path string, errs *[]SchemaError) {
	runes := []rune(doc)
	if raw, ok := schema["minLength"]; ok {
		if n, ok2 := toInt(raw); ok2 && len(runes) < n {
			*errs = append(*errs, newErr(path, "长度不得小于 %d（当前 %d）", n, len(runes)))
		}
	}
	if raw, ok := schema["maxLength"]; ok {
		if n, ok2 := toInt(raw); ok2 && len(runes) > n {
			*errs = append(*errs, newErr(path, "长度不得大于 %d（当前 %d）", n, len(runes)))
		}
	}
	if raw, ok := schema["pattern"]; ok {
		if expr, ok2 := raw.(string); ok2 && expr != "" {
			re, err := regexp.Compile(expr)
			if err != nil {
				*errs = append(*errs, newErr(path, "pattern 正则非法: %s", expr))
			} else if !re.MatchString(doc) {
				*errs = append(*errs, newErr(path, "必须匹配正则 %s", expr))
			}
		}
	}
}

// fillDefaults 递归填充 default 值（对象属性与数组元素）。
func fillDefaults(schema map[string]any, doc map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	for name, rawSchema := range props {
		sub, ok := rawSchema.(map[string]any)
		if !ok {
			continue
		}
		current, exists := doc[name]
		if !exists {
			if def, hasDef := sub["default"]; hasDef {
				doc[name] = cloneJSON(def)
				current = doc[name]
				exists = true
			}
		}
		if !exists {
			continue
		}
		switch value := current.(type) {
		case map[string]any:
			fillDefaults(sub, value)
		case []any:
			if items, ok := sub["items"].(map[string]any); ok {
				for _, elem := range value {
					if m, ok2 := elem.(map[string]any); ok2 {
						fillDefaults(items, m)
					}
				}
			}
		}
	}
}

// matchType 判断 doc 是否满足 type 关键字（支持多类型数组）。
func matchType(raw any, doc any) bool {
	switch t := raw.(type) {
	case string:
		return typeMatches(t, doc)
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && typeMatches(s, doc) {
				return true
			}
		}
		return false
	default:
		return true // 非法 type 声明忽略
	}
}

// typeMatches 判断单个类型名是否匹配。
func typeMatches(want string, doc any) bool {
	actual := jsonType(doc)
	switch strings.ToLower(strings.TrimSpace(want)) {
	case "integer":
		if actual == "integer" {
			return true
		}
		f, ok := toFloat(doc)
		return ok && f == float64(int64(f))
	case "number":
		return actual == "number" || actual == "integer"
	case "object":
		return actual == "object"
	case "array":
		return actual == "array"
	case "string":
		return actual == "string"
	case "boolean":
		return actual == "boolean"
	case "null":
		return actual == "null"
	case "":
		return true
	default:
		return true // 未知类型名忽略，保持宽松
	}
}

// jsonType 返回 JSON 语义下的类型名。
func jsonType(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case string, json.Number:
		if _, ok := v.(json.Number); ok {
			if _, err := strconv.ParseInt(v.(json.Number).String(), 10, 64); err == nil {
				return "integer"
			}
			return "number"
		}
		return "string"
	case float32, float64:
		return "number"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "integer"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return "null"
		}
		return jsonType(rv.Elem().Interface())
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	default:
		return "unknown"
	}
}

// toFloat 把任意 JSON 数值转为 float64。
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
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// toInt 把任意 JSON 整数转为 int。
func toInt(v any) (int, bool) {
	if f, ok := toFloat(v); ok {
		return int(f), true
	}
	return 0, false
}

// toStringList 归一化字符串数组（required 等关键字）。
func toStringList(raw any) []string {
	out := make([]string, 0, 4)
	switch t := raw.(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, t...)
	case string:
		out = append(out, t)
	}
	return out
}

// describeTypes 生成类型描述文本，多类型时用方括号包裹。
func describeTypes(raw any) string {
	switch t := raw.(type) {
	case string:
		return t
	case []any:
		names := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
		return "[" + strings.Join(names, " ") + "]"
	default:
		return "unknown"
	}
}

// formatList 生成枚举取值文本，例如 [java go]。
func formatList(list []any) string {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		parts = append(parts, formatValue(item))
	}
	return strings.Join(parts, " ")
}

// formatValue 生成 JSON 值的展示文本（字符串不加引号）。
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return formatFloat(t)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// formatFloat 去除浮点数无意义的小数尾巴。
func formatFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// deepEqualJSON 按 JSON 语义比较两个值。
func deepEqualJSON(a, b any) bool {
	if af, ok := toFloat(a); ok {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
	}
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(ab) == string(bb)
}

// cloneJSON 深拷贝 JSON 值，避免 default 值被多个文档共享引用。
func cloneJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// joinPath 拼接字段路径。
func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// newErr 构造带字段路径的中文错误。
func newErr(path, format string, args ...any) SchemaError {
	prefix := "根文档"
	if path != "" {
		prefix = "字段 " + path
	}
	return SchemaError{Path: path, Message: prefix + " " + fmt.Sprintf(format, args...)}
}
