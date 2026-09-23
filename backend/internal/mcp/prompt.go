package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/codeagent/backend/internal/domain"
)

// Stage 流水线阶段标识（与 CONTRACT.md §3 的阶段命名对齐）。
type Stage string

const (
	// StageLogClean 日志清洗：去噪、脱敏、提取有效行。
	StageLogClean Stage = "log_clean"
	// StageClassify 故障分类：判定 category 与 severity。
	StageClassify Stage = "classify"
	// StageRootCause 根因分析：结合堆栈与代码切片给出结构化根因。
	StageRootCause Stage = "root_cause"
	// StagePatchSynthesize 补丁生成：产出最小增量补丁。
	StagePatchSynthesize Stage = "patch_synthesize"
	// StageSummary 结论汇总：面向人的摘要与后续动作。
	StageSummary Stage = "summary"
	// StageVerifyExplain 验证解释：解释沙箱验证结果与下一步。
	StageVerifyExplain Stage = "verify_explain"
)

// AllStages 返回全部标准阶段（用于健康检查、指标初始化和测试遍历）。
func AllStages() []Stage {
	return []Stage{StageLogClean, StageClassify, StageRootCause, StagePatchSynthesize, StageSummary, StageVerifyExplain}
}

// stageTask 各阶段的专属任务描述，注入 system 模板。
var stageTask = map[Stage]string{
	StageLogClean: "清洗给定日志：剔除调试/心跳/健康检查等噪声行，保留异常与关键业务行，输出 JSON 对象 {cleaned, noiseRatio}。" +
		"noiseRatio 为被剔除行数占比（0~1，保留三位小数）。",
	StageClassify: "判定故障分类与定级，输出 JSON 对象 {category, severity, confidence}。" +
		"category 必须从 Schema 枚举中选择：空指针用 null_pointer、越界用 index_out_of_bounds、类型错误用 type_error、" +
		"依赖缺失用 dependency_missing、超时用 timeout。",
	StageRootCause: "结合异常堆栈、命中代码切片与跨仓库链路给出根因结论，输出 JSON 对象 " +
		"{summary, category, detail, confidence, evidence, blastRadius, severity}。" +
		"summary 为一句话中文根因并引用真实的文件与行号；evidence 每项为「文件:行」或调用链；" +
		"blastRadius 为受影响的仓库/接口；confidence 为 0~1 的数值。",
	StagePatchSynthesize: "针对根因产出最小增量补丁，输出 JSON 对象 {patches, rationale, risk}。" +
		"每个 patch 必须包含 {repositoryId, repoKey, filePath, action, line, oldSnippet, newSnippet, rationale, risk, confidence}：" +
		"oldSnippet 必须是上下文中真实存在的连续代码（保持原始缩进，不得改写）；" +
		"newSnippet 必须保留 oldSnippet 的全部非空关键行，仅按分类补充空值守卫/边界检查/默认值兜底。",
	StageSummary: "汇总结论与后续动作，输出 JSON 对象 {summary, actions}，actions 为可执行的中文动作列表。",
	StageVerifyExplain: "解释验证结果并给出后续动作，输出 JSON 对象 {explanation, nextActions}。" +
		"若验证失败，需说明最可能的冲突点与收敛建议。",
}

// systemTemplate 统一的 system 提示词模板（含角色设定、输出强约束与阶段标记）。
const systemTemplate = `你是 CodeAgent 的企业级代码故障排查专家，长期负责多仓库、多语言（Java/Go/Python/JS-TS/PHP）线上故障的根因定位与最小增量修复。
[STAGE:%s]

【输出强约束】
1. 只输出符合给定 JSON Schema 的 JSON，不要输出任何解释性文字，不要使用 Markdown 代码块，不要输出代码围栏。
2. JSON 字段名必须与 Schema 完全一致，类型必须匹配；缺失信息用空数组或空字符串表达。
3. 禁止暴力重写代码，仅做最小增量修改；禁止删除或改写与故障无关的既有逻辑。
4. 面向人的文本字段必须使用中文，并引用上下文中真实存在的文件路径与行号，禁止编造。
5. 证据不足时必须降低 confidence 并在 detail 中说明缺失的证据，不得臆测。

【阶段任务】
%s

【上下文使用规则】
- 上下文中的每个文件块都以 "--- 文件: <repoKey>/<path> @ <commit> (行 a-b) ---" 开头，引用时必须使用该路径与行号。
- 标记 "... (已裁剪 N 行) ..." 表示该处内容被预算裁剪，禁止假设未展示的代码内容。
- 补丁的 oldSnippet 必须逐行取自上下文原文（保持原始缩进），否则补丁会被校验拒绝。`

// userTemplates 各阶段的 user 提示词模板，变量缺失时由 renderTemplate 兜底填充。
var userTemplates = map[Stage]string{
	StageLogClean: `【任务】清洗以下日志并给出噪声占比。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,

	StageClassify: `【任务】判定该故障的分类与定级。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,

	StageRootCause: `【任务】定位故障根因。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,

	StagePatchSynthesize: `【任务】产出最小增量补丁（禁止整文件重写）。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,

	StageSummary: `【任务】汇总结论与后续动作。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,

	StageVerifyExplain: `【任务】解释沙箱验证结果并给出后续动作。
【上下文】
{{context}}
【关注点】
{{question}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`,
}

// simpleUserTemplate 未识别阶段使用的通用模板。
const simpleUserTemplate = `【任务】{{question}}
【上下文】
{{context}}
【约束】
{{constraints}}
【人工反馈】
{{feedback}}`

// varPattern 匹配模板变量 {{name}}。
var varPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// missingValue 变量缺失时的兜底文案（绝不返回空串，避免模型收到残缺提示词）。
var missingValue = map[string]string{
	"context":     "(上下文缺失)",
	"question":    "(未提供问题描述)",
	"constraints": "(无额外约束)",
	"feedback":    "(无人工反馈)",
}

// RenderSystem 渲染指定阶段的 system 提示词。
//
// 便于测试断言 [STAGE:<stage>] 标记存在，也便于控制台展示当前生效的提示词。
func RenderSystem(stage Stage) string {
	task := stageTask[stage]
	if task == "" {
		task = "按用户消息中的任务说明处理，并严格输出符合给定 JSON Schema 的 JSON。"
	}
	return fmt.Sprintf(systemTemplate, string(stage), task)
}

// SchemaFor 返回指定阶段的结构化输出 Schema；未识别阶段返回 nil。
func SchemaFor(stage Stage) map[string]any {
	switch stage {
	case StageLogClean:
		return LogCleanSchema
	case StageClassify:
		return ClassifySchema
	case StageRootCause:
		return RootCauseSchema
	case StagePatchSynthesize:
		return PatchSchema
	case StageSummary:
		return SummarySchema
	case StageVerifyExplain:
		return VerifyExplainSchema
	}
	return nil
}

// BuildMessages 组装一次推理的标准消息序列。
//
// 返回值为 (消息列表, 该阶段的结构化输出 Schema)：
//   - system 消息包含角色设定、输出强约束、[STAGE:<stage>] 标记与"禁止暴力重写代码"约束；
//   - user 消息注入 context（ContextPacked.Text）、question、constraints、feedback（人工反馈，用于重跑收敛）；
//   - 模板变量缺失时使用中文兜底文案（如 "(上下文缺失)"），不会 panic、不会产生空提示词。
func BuildMessages(stage Stage, vars map[string]any) ([]domain.ModelMessage, map[string]any) {
	tmpl := userTemplates[stage]
	if tmpl == "" {
		tmpl = simpleUserTemplate
	}
	user := renderTemplate(tmpl, vars)
	return []domain.ModelMessage{
		{Role: "system", Content: RenderSystem(stage)},
		{Role: "user", Content: user},
	}, SchemaFor(stage)
}

// renderTemplate 渲染模板并做变量缺失保护。
func renderTemplate(tmpl string, vars map[string]any) string {
	out := varPattern.ReplaceAllStringFunc(tmpl, func(token string) string {
		m := varPattern.FindStringSubmatch(token)
		if len(m) != 2 {
			return token
		}
		name := m[1]
		if vars != nil {
			if v, ok := vars[name]; ok {
				if s := stringifyVar(v); strings.TrimSpace(s) != "" {
					return s
				}
			}
		}
		if fallback, ok := missingValue[name]; ok {
			return fallback
		}
		return "(未提供)"
	})
	return strings.TrimSpace(out)
}

// stringifyVar 把模板变量转成文本。
func stringifyVar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []string:
		return strings.Join(t, "\n")
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// BuildRepairMessages 在原有消息后追加一条纠错指令，用于结构化输出失败后的一次纠错重试。
//
// 纠错消息会回灌具体的校验错误与"只输出合法 JSON"的要求，
// 不复用原始 system 之外的任何上下文，避免提示词膨胀。
func BuildRepairMessages(msgs []domain.ModelMessage, problems []string, raw string) []domain.ModelMessage {
	out := make([]domain.ModelMessage, 0, len(msgs)+1)
	out = append(out, msgs...)
	var b strings.Builder
	b.WriteString("你上一次的输出不是合法的、符合 Schema 的 JSON，无法被系统解析。\n")
	if len(problems) > 0 {
		b.WriteString("校验错误：\n")
		for i, p := range problems {
			if i >= 5 {
				break
			}
			b.WriteString("- ")
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	b.WriteString("请只输出合法 JSON，不要输出任何解释文字、不要使用 Markdown 代码块，字段名与类型必须与 Schema 完全一致。\n")
	if strings.TrimSpace(raw) != "" {
		b.WriteString("上一次输出片段（仅供定位问题，不要原样重复错误）：")
		b.WriteString(truncateRunes(raw, 200))
	}
	out = append(out, domain.ModelMessage{Role: "user", Content: b.String()})
	return out
}
