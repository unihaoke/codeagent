package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/domain"
)

// BuildReport 依据运行上下文与证据包构建归档报告（含 Markdown 全文）。
//
// 报告是"可复盘"的唯一载体：根因、影响范围、跨仓库链路、补丁、验证结果、
// 证据切片、资源消耗、时间线、建议与降级警告全部落库，供人工复核与统计。
func BuildReport(run *domain.TaskRun, bundle *domain.EvidenceBundle, timeline []domain.TimelineItem) *domain.Report {
	if run == nil {
		run = &domain.TaskRun{}
	}
	if bundle == nil {
		bundle = &domain.EvidenceBundle{RunID: run.ID, TenantID: run.TenantID}
	}

	rep := &domain.Report{
		ID:           uuid.NewString(),
		RunID:        run.ID,
		TaskID:       run.TaskID,
		TenantID:     run.TenantID,
		Title:        orDefault(run.Title, "缺陷分析任务 "+shortID(run.ID)),
		Mode:         run.Mode,
		State:        run.State,
		Severity:     severityOrDefault(run.Severity),
		RootCause:    run.RootCause,
		Patches:      append([]domain.Patch{}, run.Patches...),
		Verification: run.Verification,
		Resolution:   append([]domain.RepoResolved{}, run.Resolution...),
		ChainFlow:    append([]domain.CallEdge{}, bundle.Edges...),
		Timeline:     append([]domain.TimelineItem{}, timeline...),
		Warnings:     append([]string{}, run.Warnings...),
		Degraded:     run.Degraded,
		Usage:        run.Usage,
		ElapsedMS:    run.ElapsedMS(),
		CreatedAt:    time.Now(),
	}
	rep.Summary = buildSummary(rep, bundle)
	rep.Suggestions = buildSuggestions(rep, bundle)
	attachEvidenceInventory(rep, bundle)
	rep.Markdown = RenderMarkdown(rep)
	return rep
}

// 证据条目前缀：domain.Report 为冻结契约（没有独立的证据字段），
// 因此命中代码切片清单以带前缀的条目写入 RootCause.Evidence，由 RenderMarkdown 渲染为清单表。
const (
	evidenceSlicePrefix = "命中切片："
	evidenceDepPrefix   = "依赖清单："
)

// attachEvidenceInventory 把已加载的代码切片与依赖清单写入报告证据链，
// 保证报告（含 Markdown）可以独立还原"用了哪些代码、命中哪些行"。
func attachEvidenceInventory(rep *domain.Report, bundle *domain.EvidenceBundle) {
	if rep == nil || rep.RootCause == nil || bundle == nil {
		return
	}
	for _, s := range bundle.Slices {
		loc := s.Path
		if len(s.FocusLines) > 0 {
			loc = fmt.Sprintf("%s:%d", s.Path, s.FocusLines[0])
		}
		rep.RootCause.Evidence = appendUnique(rep.RootCause.Evidence,
			fmt.Sprintf("%s%s %s（%s）", evidenceSlicePrefix, orDefault(s.RepoKey, s.RepositoryID), loc, orDefault(s.Reason, "match")))
	}
	for _, d := range bundle.DependencyFiles {
		rep.RootCause.Evidence = appendUnique(rep.RootCause.Evidence, evidenceDepPrefix+d.Path)
	}
}

func severityOrDefault(s domain.Severity) domain.Severity {
	if s == "" {
		return domain.SeverityInfo
	}
	return s
}

// buildSummary 生成一句话报告摘要。
func buildSummary(rep *domain.Report, bundle *domain.EvidenceBundle) string {
	var sb strings.Builder
	sb.WriteString("[" + severityLabel(rep.Severity) + "] ")
	switch {
	case rep.RootCause != nil && rep.RootCause.Summary != "":
		sb.WriteString(rep.RootCause.Summary)
	case len(rep.Warnings) > 0:
		sb.WriteString("未能定位明确根因：" + rep.Warnings[0])
	default:
		sb.WriteString("未能定位明确根因，需人工补充报错上下文")
	}
	sb.WriteString(fmt.Sprintf("；共生成 %d 个增量补丁", len(rep.Patches)))
	if rep.Verification != nil {
		switch {
		case rep.Verification.Passed && !rep.Verification.Degraded:
			sb.WriteString("，沙箱验证通过")
		case rep.Verification.Passed && rep.Verification.Degraded:
			sb.WriteString("，仅通过静态降级校验")
		default:
			sb.WriteString("，沙箱验证未通过")
		}
	} else if len(rep.Patches) > 0 {
		sb.WriteString("，未执行沙箱验证")
	}
	sb.WriteString(fmt.Sprintf("；最终状态 %s。", stateLabel(rep.State)))
	if rep.Degraded {
		sb.WriteString("（本次执行发生降级，详见降级说明）")
	}
	if bundle != nil && bundle.Truncated {
		sb.WriteString("（代码上下文已按配额裁剪）")
	}
	return sb.String()
}

// buildSuggestions 依据终态与证据情况给出可执行的中文建议。
func buildSuggestions(rep *domain.Report, bundle *domain.EvidenceBundle) []string {
	out := []string{}
	add := func(s string) {
		if s == "" {
			return
		}
		for _, e := range out {
			if e == s {
				return
			}
		}
		out = append(out, s)
	}

	switch rep.State {
	case domain.StateSucceeded:
		add("补丁已通过沙箱校验，可直接生成 PR 并触发流水线复核")
	case domain.StateDegraded:
		add("本次分析缺少代码证据/模型不可用，建议人工补充报错堆栈对应的 Commit 后重跑")
	case domain.StateNeedsReview:
		add("补丁风险等级较高或未通过校验，请人工复核后再合并")
	case domain.StateFailed:
		add("任务执行失败，请检查报错信息与服务健康度后重跑")
	case domain.StateCancelled:
		add("任务已被取消，如需继续分析请重新提交或重跑")
	default:
		add("请人工确认根因结论后再决定修复动作")
	}

	if len(rep.Patches) == 0 && rep.State != domain.StateFailed && rep.State != domain.StateCancelled {
		add("未匹配到自动修复模式，建议人工排查根因")
	}
	if hasHighRisk(rep.Patches) {
		add("存在 high 风险补丁（改动行数较多或涉及多文件），建议由模块 Owner 复核后再合并")
	}
	if len(rep.Patches) > 0 && rep.Verification == nil {
		add("补丁尚未经过沙箱验证，合并前请手动执行编译与单元测试")
	}
	if rep.Verification != nil && rep.Verification.Degraded {
		add("验证在降级模式下完成（未执行真实编译/测试），请以本地流水线结果为准")
	}
	if bundle != nil && bundle.Truncated {
		add("本次代码加载触发配额上限被裁剪，建议提高租户配额或缩小报错范围后重跑")
	}
	if rep.Degraded {
		add("本次结论包含堆栈规则推断成分，建议人工补充报错上下文后重跑以提升置信度")
	}
	if rep.RootCause == nil {
		add("未产出结构化根因，建议检查堆栈解析技能与模型服务是否可用")
	} else {
		if rep.RootCause.Confidence > 0 && rep.RootCause.Confidence < 0.5 {
			add("根因置信度偏低，建议补充调用链日志或人工线索后重跑")
		}
		if isDependencyCategory(rep.RootCause.Category) {
			add("依赖类问题建议同步核对依赖清单与运行环境的版本一致性")
		}
	}
	if bundle != nil && len(bundle.Edges) > 0 && lowConfidenceEdges(bundle.Edges) {
		add("跨仓库链路包含低置信度推断边，建议结合链路追踪数据核对")
	}
	add("如需定向迭代，可通过反馈接口补充人工线索后重跑（RerunWithFeedback）")
	return out
}

func lowConfidenceEdges(edges []domain.CallEdge) bool {
	for _, e := range edges {
		if e.Confidence < 0.5 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Markdown 渲染
// ---------------------------------------------------------------------------

// RenderMarkdown 把报告渲染为结构完整的 Markdown，可直接贴到工单或 PR 描述。
//
// 为控制篇幅，代码证据仅列出文件与行号，并对每个文件最多摘录 5 行关键代码。
func RenderMarkdown(rep *domain.Report) string {
	if rep == nil {
		return ""
	}
	var sb strings.Builder

	sb.WriteString("# " + rep.Title + " —— 故障分析报告\n\n")
	renderMeta(&sb, rep)
	renderRootCause(&sb, rep)
	renderBlastRadius(&sb, rep)
	renderChainFlow(&sb, rep)
	renderPatches(&sb, rep)
	renderVerification(&sb, rep)
	renderEvidence(&sb, rep)
	renderUsage(&sb, rep)
	renderTimeline(&sb, rep)
	renderSuggestions(&sb, rep)
	renderWarnings(&sb, rep)

	return sb.String()
}

func renderMeta(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 一、任务元信息\n\n")
	sb.WriteString("| 项目 | 内容 |\n| --- | --- |\n")
	row(sb, "任务 ID", code(rep.TaskID))
	row(sb, "执行 ID", code(rep.RunID))
	row(sb, "租户", code(rep.TenantID))
	row(sb, "模式", modeLabel(rep.Mode))
	row(sb, "状态", stateLabel(rep.State))
	row(sb, "定级", severityLabel(rep.Severity))
	row(sb, "耗时", fmt.Sprintf("%d ms", rep.ElapsedMS))
	row(sb, "Token 消耗", fmt.Sprintf("%d（prompt %d / completion %d）", rep.Usage.TotalTokens, rep.Usage.PromptTokens, rep.Usage.CompletionTokens))
	row(sb, "报告生成时间", rep.CreatedAt.Format(time.RFC3339))
	if len(rep.Resolution) > 0 {
		parts := make([]string, 0, len(rep.Resolution))
		for _, r := range rep.Resolution {
			parts = append(parts, fmt.Sprintf("%s@%s(%s)", orDefault(r.RepoKey, r.RepositoryID), shortCommit(r.Commit), orDefault(r.ResolvedFrom, "unknown")))
		}
		row(sb, "锁定版本", strings.Join(parts, "；"))
	}
	sb.WriteString("\n**摘要**：" + rep.Summary + "\n\n")
}

func renderRootCause(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 二、根因结论\n\n")
	if rep.RootCause == nil {
		sb.WriteString("> 未产出结构化根因结论，请结合下方证据人工排查。\n\n")
		return
	}
	rc := rep.RootCause
	sb.WriteString(fmt.Sprintf("**%s**\n\n", orDefault(rc.Summary, "（无摘要）")))
	sb.WriteString("| 项目 | 内容 |\n| --- | --- |\n")
	row(sb, "分类", orDefault(rc.Category, "unknown"))
	row(sb, "置信度", fmt.Sprintf("%.2f", rc.Confidence))
	row(sb, "定级", severityLabel(rc.Severity))
	sb.WriteString("\n")
	if strings.TrimSpace(rc.Detail) != "" {
		sb.WriteString("**推理过程**\n\n" + rc.Detail + "\n\n")
	}
	if len(rc.Evidence) > 0 {
		rest := []string{}
		for _, e := range rc.Evidence {
			if strings.HasPrefix(e, evidenceSlicePrefix) || strings.HasPrefix(e, evidenceDepPrefix) {
				continue
			}
			rest = append(rest, e)
		}
		if len(rest) > 0 {
			sb.WriteString("**支撑证据**\n\n")
			for _, e := range rest {
				sb.WriteString("- " + e + "\n")
			}
			sb.WriteString("\n")
		}
	}
}

func renderBlastRadius(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 三、影响范围\n\n")
	if rep.RootCause == nil || len(rep.RootCause.BlastRadius) == 0 {
		sb.WriteString("> 未识别到明确的影响范围（可能为单点异常）。\n\n")
		return
	}
	for _, b := range rep.RootCause.BlastRadius {
		sb.WriteString("- " + b + "\n")
	}
	sb.WriteString("\n")
}

func renderChainFlow(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 四、跨仓库调用链路\n\n")
	if len(rep.ChainFlow) == 0 {
		sb.WriteString("> 本次未产出跨仓库链路（单仓库模式或候选仓库不足 2 个）。\n\n")
		return
	}
	sb.WriteString("| 起点仓库 | 终点仓库 | 协议 | 接口/符号 | 置信度 | 异常点 |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, e := range rep.ChainFlow {
		symbol := orDefault(e.Endpoint, e.Symbol)
		fmt.Fprintf(sb, "| %s | %s | %s | %s | %.2f | %s |\n",
			tableCell(e.FromRepo), tableCell(e.ToRepo), tableCell(orDefault(e.Protocol, "-")),
			tableCell(orDefault(symbol, "-")), e.Confidence, tableCell(orDefault(e.Anomaly, "-")))
	}
	sb.WriteString("\n")
	if detail := chainEvidence(rep.ChainFlow); detail != "" {
		sb.WriteString("> 链路边证据：" + detail + "\n\n")
	}
}

func chainEvidence(edges []domain.CallEdge) string {
	parts := []string{}
	for _, e := range edges {
		if strings.TrimSpace(e.Evidence) != "" {
			parts = append(parts, e.FromRepo+"→"+e.ToRepo+"："+e.Evidence)
		}
	}
	return strings.Join(parts, "；")
}

func renderPatches(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 五、修复补丁\n\n")
	if len(rep.Patches) == 0 {
		sb.WriteString("> 本次未产出自动修复补丁，建议人工排查根因。\n\n")
		return
	}
	for i, p := range rep.Patches {
		sb.WriteString(fmt.Sprintf("### 补丁 %d：%s\n\n", i+1, p.FilePath))
		sb.WriteString("| 项目 | 内容 |\n| --- | --- |\n")
		row(sb, "仓库", orDefault(p.RepoKey, p.RepositoryID))
		row(sb, "动作", string(orDefaultAction(p.Action)))
		row(sb, "风险", riskLabel(p.Risk))
		row(sb, "状态", patchStatusLabel(p.Status))
		row(sb, "产出技能", orDefault(p.ProducedBy, "-"))
		if len(p.TouchedLines) > 0 {
			row(sb, "变更行", touchedLines(p.TouchedLines))
		}
		sb.WriteString("\n")
		if strings.TrimSpace(p.Rationale) != "" {
			sb.WriteString("**修复理由**：" + p.Rationale + "\n\n")
		}
		if strings.TrimSpace(p.UnifiedDiff) != "" {
			sb.WriteString("```diff\n" + strings.TrimRight(p.UnifiedDiff, "\n") + "\n```\n\n")
		}
	}
}

func renderVerification(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 六、验证结果\n\n")
	v := rep.Verification
	if v == nil {
		if len(rep.Patches) > 0 {
			sb.WriteString("> 补丁未经过沙箱验证（调用方关闭验证或流水线降级跳过）。\n\n")
		} else {
			sb.WriteString("> 无补丁，未执行验证。\n\n")
		}
		return
	}
	result := "未通过"
	if v.Passed {
		result = "通过"
	}
	if v.Degraded {
		result += "（降级为静态校验）"
	}
	sb.WriteString(fmt.Sprintf("- 结论：**%s**\n- 工作区：%s\n- 补丁应用结果：%s\n- 耗时：%d ms\n\n",
		result, orDefault(v.Workspace, "-"), orDefault(v.ApplyResult, "-"), v.DurationMS))
	if len(v.Checks) > 0 {
		sb.WriteString("| 校验项 | 结果 | 命令 | 耗时 | 输出摘要 |\n| --- | --- | --- | --- | --- |\n")
		for _, c := range v.Checks {
			state := "✅ 通过"
			if !c.Passed {
				state = "❌ 失败"
			}
			if c.Skipped {
				state = "⏭ 跳过"
			}
			fmt.Fprintf(sb, "| %s | %s | %s | %d ms | %s |\n",
				tableCell(c.Name), state, tableCell(orDefault(c.Command, "-")), c.DurationMS,
				tableCell(truncate(strings.TrimSpace(c.Output), 300)))
		}
		sb.WriteString("\n")
	}
	if strings.TrimSpace(v.Log) != "" {
		sb.WriteString("**验证日志摘要**\n\n```text\n" + truncate(strings.TrimSpace(v.Log), 2000) + "\n```\n\n")
	}
}

func renderEvidence(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 七、证据与命中代码切片\n\n")

	rest := []string{}
	if rep.RootCause != nil {
		for _, e := range rep.RootCause.Evidence {
			if strings.HasPrefix(e, evidenceSlicePrefix) || strings.HasPrefix(e, evidenceDepPrefix) {
				continue
			}
			rest = append(rest, e)
		}
	}
	if len(rest) > 0 {
		sb.WriteString("**根因证据**\n\n")
		for _, e := range rest {
			sb.WriteString("- " + e + "\n")
		}
		sb.WriteString("\n")
	}

	slices, deps := sliceInventory(rep)
	if len(slices) == 0 {
		sb.WriteString("> 本次未加载到代码切片（堆栈线索不足或源码不可用）。\n\n")
	} else {
		sb.WriteString("| 仓库 | 文件 | 焦点行 | 命中原因 |\n| --- | --- | --- | --- |\n")
		for _, s := range slices {
			fmt.Fprintf(sb, "| %s | %s | %s | %s |\n",
				tableCell(s.RepoKey), tableCell(s.Path), focusLabel(s.FocusLines), tableCell(s.Reason))
		}
		sb.WriteString("\n")
		// 每个文件最多摘录 5 行关键代码，避免报告体积失控。
		for _, s := range slices {
			if excerpt := codeExcerpt(patchOldContent(rep, s.Path), s.FocusLines, 5); excerpt != "" {
				sb.WriteString(fmt.Sprintf("**%s** 关键代码摘要：\n\n```text\n%s\n```\n\n", s.Path, excerpt))
			}
		}
	}
	if len(deps) > 0 {
		sb.WriteString("**依赖清单**\n\n")
		for _, d := range deps {
			sb.WriteString("- `" + d + "`\n")
		}
		sb.WriteString("\n")
	}
}

// sliceInventory 从报告证据链中还原命中代码切片清单与依赖清单。
func sliceInventory(rep *domain.Report) ([]domain.CodeSlice, []string) {
	slices := []domain.CodeSlice{}
	deps := []string{}
	if rep == nil || rep.RootCause == nil {
		return slices, deps
	}
	for _, e := range rep.RootCause.Evidence {
		switch {
		case strings.HasPrefix(e, evidenceSlicePrefix):
			body := strings.TrimPrefix(e, evidenceSlicePrefix)
			repoKey := ""
			if idx := strings.Index(body, " "); idx > 0 {
				repoKey = body[:idx]
				body = body[idx+1:]
			}
			reason := ""
			if idx := strings.LastIndex(body, "（"); idx > 0 {
				reason = strings.TrimSuffix(body[idx+len("（"):], "）")
				body = body[:idx]
			}
			file, line := splitLoc(strings.TrimSpace(body))
			sl := domain.CodeSlice{RepoKey: repoKey, Path: file, Reason: reason}
			if line > 0 {
				sl.FocusLines = []int{line}
			}
			slices = append(slices, sl)
		case strings.HasPrefix(e, evidenceDepPrefix):
			deps = append(deps, strings.TrimPrefix(e, evidenceDepPrefix))
		}
	}
	sort.SliceStable(slices, func(i, j int) bool {
		if slices[i].RepoKey != slices[j].RepoKey {
			return slices[i].RepoKey < slices[j].RepoKey
		}
		return slices[i].Path < slices[j].Path
	})
	if len(slices) > 20 {
		slices = slices[:20]
	}
	return slices, deps
}

// patchOldContent 返回补丁携带的原文内容（用于生成 ≤5 行的代码摘要）。
func patchOldContent(rep *domain.Report, filePath string) string {
	for _, p := range rep.Patches {
		if p.FilePath == filePath && p.OldContent != "" {
			return p.OldContent
		}
	}
	return ""
}

func splitLoc(loc string) (string, int) {
	file := loc
	line := 0
	if idx := strings.LastIndex(loc, ":"); idx > 0 {
		file = loc[:idx]
		fmt.Sscanf(loc[idx+1:], "%d", &line)
	}
	return strings.TrimSpace(file), line
}

// codeExcerpt 摘录焦点行附近的少量代码（最多 maxLines 行），避免报告中贴大段源码。
func codeExcerpt(content string, focus []int, maxLines int) string {
	if strings.TrimSpace(content) == "" || maxLines <= 0 {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return ""
	}
	pick := []int{}
	for _, f := range focus {
		if f > 0 && f <= len(lines) {
			pick = append(pick, f)
		}
		if len(pick) >= maxLines {
			break
		}
	}
	if len(pick) == 0 {
		for i := 0; i < len(lines) && len(pick) < maxLines; i++ {
			if strings.TrimSpace(lines[i]) != "" {
				pick = append(pick, i+1)
			}
		}
	}
	sort.Ints(pick)
	var sb strings.Builder
	for _, n := range pick {
		sb.WriteString(fmt.Sprintf("%5d | %s\n", n, truncate(lines[n-1], 200)))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func renderUsage(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 八、资源消耗\n\n")
	sb.WriteString("| 指标 | 数值 |\n| --- | --- |\n")
	fmt.Fprintf(sb, "| 加载文件数 | %d |\n", rep.Usage.FilesLoaded)
	fmt.Fprintf(sb, "| 代码字符数 | %d |\n", rep.Usage.CodeChars)
	fmt.Fprintf(sb, "| 技能调用/失败 | %d / %d |\n", rep.Usage.SkillCalls, rep.Usage.SkillFailures)
	fmt.Fprintf(sb, "| 模型调用/兜底 | %d / %d |\n", rep.Usage.ModelCalls, rep.Usage.ModelFallbacks)
	fmt.Fprintf(sb, "| Token 合计 | %d |\n", rep.Usage.TotalTokens)
	fmt.Fprintf(sb, "| 文件缓存命中 | %d |\n", rep.Usage.CacheHits)
	fmt.Fprintf(sb, "| 执行耗时 | %d ms |\n", rep.ElapsedMS)
	sb.WriteString("\n")
}

func renderTimeline(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 九、执行时间线\n\n")
	if len(rep.Timeline) == 0 {
		sb.WriteString("> 无时间线记录。\n\n")
		return
	}
	sb.WriteString("| 时间 | 阶段 | 级别 | 说明 |\n| --- | --- | --- | --- |\n")
	for _, t := range rep.Timeline {
		level := orDefault(t.Level, "info")
		icon := "ℹ"
		switch level {
		case "warn":
			icon = "⚠"
		case "error":
			icon = "⛔"
		}
		fmt.Fprintf(sb, "| %s | %s | %s %s | %s |\n",
			t.At.Format("15:04:05"), tableCell(orDefault(t.Stage, "-")), icon, level, tableCell(t.Message))
	}
	sb.WriteString("\n")
}

func renderSuggestions(sb *strings.Builder, rep *domain.Report) {
	sb.WriteString("## 十、建议与后续动作\n\n")
	if len(rep.Suggestions) == 0 {
		sb.WriteString("> 暂无建议。\n\n")
		return
	}
	for i, s := range rep.Suggestions {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
	}
	sb.WriteString("\n")
}

func renderWarnings(sb *strings.Builder, rep *domain.Report) {
	if !rep.Degraded && len(rep.Warnings) == 0 {
		return
	}
	sb.WriteString("## 十一、降级与警告\n\n")
	if rep.Degraded {
		sb.WriteString("> ⚠ 本次执行发生降级：结论可信度下降，请结合人工判断使用。\n\n")
	}
	for _, w := range rep.Warnings {
		sb.WriteString("- " + w + "\n")
	}
	sb.WriteString("\n")
}

// ---------------------------------------------------------------------------
// 渲染工具
// ---------------------------------------------------------------------------

func row(sb *strings.Builder, k, v string) {
	fmt.Fprintf(sb, "| %s | %s |\n", tableCell(k), tableCell(v))
}

// tableCell 转义 Markdown 表格中的竖线与换行。
func tableCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", "<br>")
	s = strings.ReplaceAll(s, "\n", "<br>")
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func code(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return "`" + s + "`"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func modeLabel(m domain.TaskMode) string {
	switch m {
	case domain.ModeGroup:
		return "分组多仓库联合排查"
	case domain.ModeSingleRepo:
		return "单仓库精准修复"
	default:
		return orDefault(string(m), "unknown")
	}
}

func stateLabel(s domain.TaskState) string {
	switch s {
	case domain.StateQueued:
		return "排队中"
	case domain.StateAnalyzing:
		return "分析中"
	case domain.StateRepairing:
		return "修复中"
	case domain.StateVerifying:
		return "验证中"
	case domain.StateSucceeded:
		return "修复成功"
	case domain.StateNeedsReview:
		return "待人工复核"
	case domain.StateFailed:
		return "执行失败"
	case domain.StateCancelled:
		return "已取消"
	case domain.StateDegraded:
		return "降级完成"
	default:
		return orDefault(string(s), "unknown")
	}
}

func severityLabel(s domain.Severity) string {
	switch s {
	case domain.SeverityBlocker:
		return "blocker（阻断）"
	case domain.SeverityCritical:
		return "critical（严重）"
	case domain.SeverityMajor:
		return "major（主要）"
	case domain.SeverityMinor:
		return "minor（次要）"
	case domain.SeverityInfo:
		return "info（提示）"
	default:
		return orDefault(string(s), "info（提示）")
	}
}

func riskLabel(r domain.PatchRisk) string {
	switch r {
	case domain.RiskLow:
		return "low（低）"
	case domain.RiskHigh:
		return "high（高，需人工复核）"
	default:
		return "medium（中）"
	}
}

func patchStatusLabel(s domain.PatchStatus) string {
	switch s {
	case domain.PatchApplied:
		return "applied（已验证）"
	case domain.PatchRejected:
		return "rejected（已拒绝）"
	case domain.PatchFailed:
		return "failed（验证失败）"
	default:
		return "proposed（待验证）"
	}
}

func orDefaultAction(a domain.PatchAction) domain.PatchAction {
	if a == "" {
		return domain.ActionModify
	}
	return a
}

func touchedLines(rs []domain.LineRange) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%d-%d", r.Start, r.End))
	}
	return strings.Join(parts, ", ")
}

func focusLabel(lines []int) string {
	if len(lines) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(lines))
	for i, l := range lines {
		if i >= 8 {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%d", l))
	}
	return strings.Join(parts, ", ")
}
