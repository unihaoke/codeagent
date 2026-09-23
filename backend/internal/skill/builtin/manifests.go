// Package builtin 提供第五层（Skill 可插拔插件层）的 7 个内置技能实现。
//
// 所有技能均满足契约要求：确定性（同入参同出参）、无网络访问、无文件系统副作用、
// 受 context 超时约束（在循环中检查 ctx.Err()）。技能通过 domain.SkillRegistry.Register
// 反向注册（宿主在 main 中调用 Register），skill 包不反向依赖本包。
package builtin

import (
	"context"
	"fmt"

	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/skill"
)

// Deps 内置技能依赖（当前为空结构体，预留后续注入源码解析器 / 沙箱 / 脱敏箱等能力）。
type Deps struct{}

// binder 技能清单与处理器的绑定。
type binder struct {
	manifest domain.SkillManifest
	handler  domain.SkillHandler
}

// Register 把 7 个内置技能注册进技能注册中心；任一注册失败立即返回错误。
func Register(reg domain.SkillRegistry, deps Deps) error {
	if reg == nil {
		return fmt.Errorf("技能注册中心为空")
	}
	for _, b := range builtins() {
		if err := reg.Register(context.Background(), b.manifest, b.handler); err != nil {
			return fmt.Errorf("注册内置技能 %s@%s 失败: %w", b.manifest.Name, b.manifest.Version, err)
		}
	}
	return nil
}

// Manifests 返回全部内置技能清单，便于宿主做自检、配置覆盖或前端展示。
func Manifests() []domain.SkillManifest {
	all := builtins()
	out := make([]domain.SkillManifest, 0, len(all))
	for _, b := range all {
		out = append(out, b.manifest)
	}
	return out
}

// builtins 组装全部内置技能（清单 + 处理器）。
func builtins() []binder {
	return []binder{
		{manifest: stacktraceParseManifest(), handler: handleStacktraceParse},
		{manifest: codeASTAnalyzeManifest(), handler: handleCodeASTAnalyze},
		{manifest: errorLocalizeManifest(), handler: handleErrorLocalize},
		{manifest: crossRepoTraceManifest(), handler: handleCrossRepoTrace},
		{manifest: codePatchSynthesizeManifest(), handler: handleCodePatchSynthesize},
		{manifest: dependencyRepairManifest(), handler: handleDependencyRepair},
		{manifest: patchVerifyManifest(), handler: handlePatchVerify},
	}
}

// baseManifest 生成内置技能公共字段，避免 7 份清单重复声明。
func baseManifest(name string, category domain.SkillCategory, title, desc string,
	timeout int, priority int, fallbacks []string, schema map[string]any) domain.SkillManifest {
	return domain.SkillManifest{
		Name:             name,
		Version:          "1.0.0",
		Title:            title,
		Description:      desc,
		Category:         category,
		Status:           domain.SkillEnabled,
		InputSchema:      schema,
		Timeout:          timeout,
		Retries:          1,
		FailureThreshold: 3,
		CooldownSeconds:  30,
		FallbackSkills:   fallbacks,
		Builtin:          true,
		Priority:         priority,
		Tags:             []string{"builtin", string(category)},
	}
}

// objectSchema 构造宽松的入参 Schema：只约束已知字段类型，允许调用方附加字段。
func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": true,
	}
	if len(required) > 0 {
		list := make([]any, 0, len(required))
		for _, r := range required {
			list = append(list, r)
		}
		schema["required"] = list
	}
	return schema
}

// arrayOfObjects 生成对象数组字段的 Schema。
func arrayOfObjects() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
}

// stringField 生成字符串字段的 Schema。
func stringField() map[string]any { return map[string]any{"type": "string"} }

// stacktraceParseManifest stacktrace_parse 技能清单。
func stacktraceParseManifest() domain.SkillManifest {
	schema := objectSchema([]string{"stacktrace"}, map[string]any{
		"stacktrace": map[string]any{"type": "string", "minLength": 1},
		"logs":       stringField(),
		"language":   stringField(),
	})
	// 基础解析技能，无同类兜底对象。
	return baseManifest(domain.SkillStacktraceParse, domain.CatParse,
		"堆栈解析", "解析 Java/Go/Python/Node/PHP 异常堆栈，输出结构化 StackAnalysis 与异常分类",
		20, 100, nil, schema)
}

// codeASTAnalyzeManifest code_ast_analyze 技能清单。
func codeASTAnalyzeManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"slices":     arrayOfObjects(),
		"focusLines": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	})
	return baseManifest(domain.SkillCodeASTAnalyze, domain.CatParse,
		"代码结构与风险分析", "提取符号/依赖并按规则识别空指针、越界、资源泄漏等代码风险",
		30, 90, nil, schema)
}

// errorLocalizeManifest error_localize 技能清单。
func errorLocalizeManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"stack":      map[string]any{"type": "object"},
		"candidates": arrayOfObjects(),
	})
	// 语义兜底：定位失败时回退到堆栈解析，保证仍能给出来源线索。
	return baseManifest(domain.SkillErrorLocalize, domain.CatDiagnose,
		"报错定位", "把堆栈帧与候选仓库/文件线索做归一化匹配，输出可解释的命中位置",
		20, 80, []string{domain.SkillStacktraceParse}, schema)
}

// crossRepoTraceManifest cross_repo_trace 技能清单。
func crossRepoTraceManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"stack":      map[string]any{"type": "object"},
		"candidates": arrayOfObjects(),
		"slices":     arrayOfObjects(),
	})
	// 语义兜底链：链路推断依赖定位结果，定位不可用时依次回退。
	return baseManifest(domain.SkillCrossRepoTrace, domain.CatCrossRepo,
		"跨仓库链路推断", "依据候选分层与代码中的 HTTP/gRPC 调用证据构造调用链、异常点与断点",
		40, 70, []string{domain.SkillErrorLocalize, domain.SkillStacktraceParse}, schema)
}

// codePatchSynthesizeManifest code_patch_synthesize 技能清单。
func codePatchSynthesizeManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"rootCause": map[string]any{"type": "object"},
		"slices":    arrayOfObjects(),
		"category":  stringField(),
	})
	// 修复技能不做跨语义兜底：由其他技能产出的补丁语义不同，误兜底会产出误导性结果，
	// 失败时交由 pipeline 降级为人工复核。
	return baseManifest(domain.SkillCodePatchSynthesize, domain.CatRepair,
		"补丁合成", "按根因分类与代码语义产出可应用的最小补丁描述（oldSnippet 保证是原文精确子串）",
		40, 60, nil, schema)
}

// dependencyRepairManifest dependency_repair 技能清单。
func dependencyRepairManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"dependencyFiles": arrayOfObjects(),
		"errorMessage":    stringField(),
	})
	return baseManifest(domain.SkillDependencyRepair, domain.CatRepair,
		"依赖修复", "解析依赖清单与报错信息，定位缺失/冲突依赖并产出清单级补丁建议",
		30, 50, nil, schema)
}

// patchVerifyManifest patch_verify 技能清单。
func patchVerifyManifest() domain.SkillManifest {
	schema := objectSchema(nil, map[string]any{
		"patches":  arrayOfObjects(),
		"language": stringField(),
	})
	return baseManifest(domain.SkillPatchVerify, domain.CatVerify,
		"补丁静态校验", "对补丁做片段完整性、括号配平、语言语法哨兵、危险操作与变更规模校验",
		30, 40, nil, schema)
}

// compile-time 断言：确保内置技能使用的是第五层统一别名。
var _ skill.Handler = handlePatchVerify

// normalizeOutput 把技能输出做一次 JSON 往返，保证进程内调用（pipeline 直接调用）与
// HTTP/JSON 调用拿到的数据结构完全一致：切片统一为 []any、对象统一为 map[string]any、
// 数字统一为 float64。技能输出本身只包含可 JSON 序列化的值。
func normalizeOutput(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := jsonRoundTrip[map[string]any](m)
	if out == nil {
		return m
	}
	return out
}
