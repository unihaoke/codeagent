package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 内置 Mock 推理器
// ---------------------------------------------------------------------------

// defaultMockLatency 内置 Mock 的默认推理耗时。
const defaultMockLatency = 5 * time.Millisecond

// mockProvider 内置 Mock 推理器。
//
// 设计目标：**完全离线、完全确定**地跑通"日志清洗 → 分类 → 根因 → 补丁"全链路。
// 同样的输入必然得到同样的输出（随机量全部由内容 sha256 派生），
// 便于演示、压测与回归测试；输出形态与真实模型严格一致（JSONMode 下返回合法 JSON）。
type mockProvider struct {
	name    string
	model   string
	tier    domain.ModelTier
	latency time.Duration
	log     *logx.Logger
}

// NewMockProvider 构造内置 Mock 提供方。
//
// latencyMS <= 0 时使用默认 5ms；睡眠过程可被 ctx 取消。
func NewMockProvider(cfg config.ProviderConfig, latencyMS int, log *logx.Logger) Provider {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "mock"
	}
	model := ""
	if len(cfg.Models) > 0 {
		model = strings.TrimSpace(cfg.Models[0])
	}
	if model == "" {
		model = "mock-reasoner-v1"
	}
	latency := time.Duration(latencyMS) * time.Millisecond
	if latencyMS <= 0 {
		latency = defaultMockLatency
	}
	return &mockProvider{
		name:    name,
		model:   model,
		tier:    normalizeTier(cfg.Tier),
		latency: latency,
		log:     loggerOr(log),
	}
}

// Name 返回提供方名称。
func (m *mockProvider) Name() string { return m.name }

// Target 返回默认模型目标。
func (m *mockProvider) Target() domain.ModelTarget {
	return domain.ModelTarget{Provider: m.name, Model: m.model, Tier: m.tier}
}

// Healthy 内置 Mock 永远可用。
func (m *mockProvider) Healthy() bool { return true }

// Chat 依据阶段确定性地生成结构化结果。
//
// 阶段识别优先级：[STAGE:xxx] 标记 → req.Stage → req.Metadata["stage"]；
// 未识别阶段不会编造内容，只回显输入摘要的哈希。
func (m *mockProvider) Chat(ctx context.Context, req ProviderRequest) (*ProviderResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.latency > 0 {
		timer := time.NewTimer(m.latency)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	stage := resolveMockStage(req)
	userText := lastRoleContent(req.Messages, "user")

	var payload map[string]any
	switch stage {
	case string(StageRootCause):
		payload = mockRootCause(userText)
	case string(StagePatchSynthesize):
		payload = mockPatchSynthesize(userText)
	case string(StageClassify):
		payload = mockClassify(userText)
	case string(StageLogClean):
		payload = mockLogClean(userText)
	case string(StageSummary):
		payload = mockSummary(userText)
	case string(StageVerifyExplain):
		payload = mockVerifyExplain(userText)
	default:
		sum := sha256.Sum256([]byte(userText))
		payload = map[string]any{
			"note":     "mock provider: 未识别的阶段",
			"echoHash": hex.EncodeToString(sum[:])[:12],
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mock 结果序列化失败: %w", err)
	}
	text := string(raw)

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = m.model
	}
	// 需求约定：Mock 的 Usage 用字符数估算（prompt 字符数/2、completion 字符数/2）。
	promptTokens := len(mockPromptChars(req.Messages)) / 2
	completionTokens := len(text) / 2
	usage := domain.ModelUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
	m.log.Debug("mock 推理完成", "stage", stage, "model", model, "promptTokens", promptTokens, "completionTokens", completionTokens)
	return &ProviderResponse{
		Text:  text,
		Usage: usage,
		Model: model,
		Raw:   map[string]any{"mock": true, "stage": stage},
	}, nil
}

// mockPromptChars 拼接全部消息内容，用于估算 prompt 字符数。
func mockPromptChars(msgs []domain.ModelMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
	}
	return b.String()
}

// resolveMockStage 识别当前阶段。
func resolveMockStage(req ProviderRequest) string {
	for _, m := range req.Messages {
		if match := reStageMarker.FindStringSubmatch(m.Content); len(match) == 2 {
			return strings.ToLower(match[1])
		}
	}
	if s := strings.ToLower(strings.TrimSpace(req.Stage)); s != "" {
		return s
	}
	if req.Metadata != nil {
		if s, ok := req.Metadata["stage"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.ToLower(strings.TrimSpace(s))
		}
	}
	return ""
}

// lastRoleContent 返回最后一个指定角色的消息内容。
func lastRoleContent(msgs []domain.ModelMessage, role string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(msgs[i].Role), role) {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

// reStageMarker 匹配 system 模板中的阶段标记。
var reStageMarker = regexp.MustCompile(`\[STAGE:([A-Za-z_]+)\]`)

// ---------------------------------------------------------------------------
// 阶段一：根因分析
// ---------------------------------------------------------------------------

// exceptionInfo 从文本中提取的异常摘要。
type exceptionInfo struct {
	Type    string
	Message string
	Raw     string
}

var (
	// Java / Python / JS 常见异常类型（允许包名前缀）。
	reExceptionType = regexp.MustCompile(`((?:[A-Za-z_][\w$]*\.)*[A-Za-z_][\w$]*(?:Exception|Error|Throwable))\b\s*:?\s*(.*)$`)
	// 堆栈帧：(File.java:123) / File "app.py", line 12 / path/app.js:12:9
	reFrameJava = regexp.MustCompile(`\(([^()\s]+\.(?:java|kt|scala|groovy|js|jsx|mjs|cjs|ts|tsx|py|go|php|rb|cs|c|cc|cpp|rs)):(\d+)(?::\d+)?\)`)
	reFramePy   = regexp.MustCompile(`File "([^"]+)",\s*line\s+(\d+)`)
	reFramePath = regexp.MustCompile(`([\w./@\\-]+\.(?:java|kt|go|py|js|jsx|mjs|cjs|ts|tsx|php|rb|cs|c|cc|cpp|rs)):(\d+)(?::\d+)?`)
)

// firstException 提取首个异常类型与消息。
func firstException(text string) exceptionInfo {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		for _, prefix := range []string{"Caused by:", "caused by:", "Caused by :"} {
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
		// 去掉 Java 的线程前缀：Exception in thread "main" java.lang.NullPointerException: xxx
		if strings.HasPrefix(line, "Exception in thread") {
			if i := strings.Index(line, "\""); i >= 0 {
				if j := strings.Index(line[i+1:], "\""); j >= 0 {
					line = strings.TrimSpace(line[i+1+j+1:])
				}
			}
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "panic:") {
			return exceptionInfo{Type: "panic", Message: strings.TrimSpace(line[len("panic:"):]), Raw: line}
		}
		if strings.HasPrefix(lower, "fatal error:") {
			return exceptionInfo{Type: "fatal error", Message: strings.TrimSpace(line[len("fatal error:"):]), Raw: line}
		}
		if strings.HasPrefix(line, "at ") || strings.HasPrefix(line, "File \"") ||
			strings.HasPrefix(line, "Traceback") || strings.HasPrefix(lower, "goroutine ") {
			continue
		}
		if m := reExceptionType.FindStringSubmatch(line); m != nil && m[1] != "Exception" {
			return exceptionInfo{Type: m[1], Message: strings.TrimSpace(strings.TrimPrefix(m[2], ":")), Raw: line}
		}
	}
	return exceptionInfo{}
}

// firstFrame 提取首个堆栈帧（文件 + 行号 + 原始行）。
func firstFrame(text string) (string, int, string) {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{reFrameJava, reFramePy, reFramePath} {
			if m := re.FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[2]); err == nil {
					return m[1], n, line
				}
			}
		}
	}
	return "", 0, ""
}

// collectFrames 收集前 n 个堆栈帧，用于证据列表。
func collectFrames(text string, n int) []string {
	out := make([]string, 0, n)
	seen := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		if len(out) >= n {
			break
		}
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{reFrameJava, reFramePy, reFramePath} {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			key := m[1] + ":" + m[2]
			if seen[key] {
				break
			}
			seen[key] = true
			out = append(out, key)
			break
		}
	}
	return out
}

// mockRootCause 生成符合 RootCauseSchema 的中文根因结论。
func mockRootCause(userText string) map[string]any {
	exc := firstException(userText)
	file, line, frameRaw := firstFrame(userText)
	category, keywords := detectCategoryDetail(userText)
	confidence := stableConfidence(userText)
	severity := severityFor(category)

	excType := exc.Type
	if excType == "" {
		excType = "未识别异常"
	}
	file = cleanPath(file)

	var summary string
	if file != "" {
		summary = fmt.Sprintf("%s:%d 处抛出 %s，判定为%s，首个堆栈帧即故障触发点",
			file, line, excType, categoryCN(category))
	} else {
		summary = fmt.Sprintf("从日志判定为%s（%s），未解析到文件行号，需结合候选仓库进一步定位",
			categoryCN(category), excType)
	}

	msg := exc.Message
	if strings.TrimSpace(msg) == "" {
		msg = "未解析到异常消息"
	}
	frameText := "未解析到堆栈帧"
	if file != "" {
		frameText = fmt.Sprintf("%s:%d", file, line)
	}
	keywordText := "无明确关键词"
	if len(keywords) > 0 {
		keywordText = strings.Join(keywords, "、")
	}
	detail := fmt.Sprintf("异常类型：%s\n异常消息：%s\n首个堆栈帧：%s\n判定依据：命中关键词「%s」，归类为 %s\n修复方向：%s",
		excType, truncateRunes(msg, 200), frameText, keywordText, categoryCN(category), fixDirection(category))

	evidence := make([]any, 0, 4)
	if exc.Raw != "" {
		evidence = append(evidence, "异常: "+truncateRunes(exc.Raw, 180))
	}
	for _, f := range collectFrames(userText, 3) {
		evidence = append(evidence, "帧: "+f)
	}
	if frameRaw != "" && len(evidence) == 0 {
		evidence = append(evidence, "帧: "+truncateRunes(frameRaw, 180))
	}
	if len(evidence) == 0 {
		evidence = append(evidence, "日志: "+truncateRunes(firstNonEmptyLine(userText), 180))
	}

	return map[string]any{
		"summary":     summary,
		"category":    category,
		"detail":      detail,
		"confidence":  confidence,
		"evidence":    evidence,
		"blastRadius": blastRadius(userText, excType),
		"severity":    severity,
	}
}

// blastRadius 依据上下文中的文件头与跨仓库链路推断影响面。
func blastRadius(userText, excType string) []any {
	repos := reposInContext(userText)
	if len(repos) == 0 {
		return []any{fmt.Sprintf("%s 的影响面待确认：证据中未包含跨仓库链路，需人工补充", excType)}
	}
	out := make([]any, 0, len(repos))
	for _, r := range repos {
		out = append(out, r+" 及其上游调用方")
	}
	return out
}

// reposInContext 从上下文中提取出现过的仓库 key（文件头 + 跨仓库链路）。
func reposInContext(text string) []string {
	seen := map[string]bool{}
	out := []string{}
	push := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := reFileHeader.FindStringSubmatch(trimmed); m != nil {
			if i := strings.Index(m[1], "/"); i > 0 {
				push(m[1][:i])
			}
		}
		if m := reEdgeLine.FindStringSubmatch(trimmed); m != nil {
			push(m[1])
			push(m[2])
		}
	}
	sort.Strings(out)
	return out
}

// cleanPath 去掉盘符与多余前缀，保留可引用的相对路径。
func cleanPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if len(p) > 2 && p[1] == ':' {
		p = p[2:]
	}
	return strings.TrimPrefix(p, "/")
}

// ---------------------------------------------------------------------------
// 阶段二：补丁生成
// ---------------------------------------------------------------------------

// mockFileBlock 上下文中解析出的一个文件块。
//
// 约定（与 context.go 保持一致）：块内除"... (已裁剪 N 行) ..."标记行外的代码行
// 与文件行号 1:1 连续对应，起始行号取文件头中的 a。
type mockFileBlock struct {
	repoKey   string
	path      string
	commit    string
	startLine int
	lines     []string
}

var (
	// 文件头：--- 文件: <repoKey>/<path> @ <commit> (行 a-b) ---
	reFileHeader = regexp.MustCompile(`^---\s*文件[:：]\s*(.+?)\s*@\s*(\S*)\s*\(行\s*(\d+)\s*-\s*(\d+)\)\s*---\s*$`)
	// 裁剪标记：... (已裁剪 N 行) ...
	reTrimMarker = regexp.MustCompile(`^\.\.\.\s*\(已裁剪\s*\d+\s*行\)\s*\.\.\.$`)
	// 跨仓库链路边：- repoA → repoB (http /api/x)
	reEdgeLine = regexp.MustCompile(`^[-*]\s*([\w.-]+)\s*(?:→|->)\s*([\w.-]+)`)
	// Markdown 代码围栏
	reFence = regexp.MustCompile("^```")
)

// parseMockFileBlocks 解析上下文中的文件块（优先文件头块，其次裸代码围栏）。
func parseMockFileBlocks(text string) []mockFileBlock {
	var (
		blocks  []mockFileBlock
		current *mockFileBlock
		inFence bool
	)
	flush := func() {
		if current == nil {
			return
		}
		current.lines = trimBlankLines(current.lines)
		if len(current.lines) > 0 {
			blocks = append(blocks, *current)
		}
		current = nil
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if m := reFileHeader.FindStringSubmatch(trimmed); m != nil {
			flush()
			repo, path := "", m[1]
			if i := strings.Index(m[1], "/"); i > 0 {
				repo, path = m[1][:i], m[1][i+1:]
			}
			start, _ := strconv.Atoi(m[3])
			current = &mockFileBlock{repoKey: repo, path: path, commit: m[2], startLine: start}
			continue
		}
		if strings.HasPrefix(trimmed, "### ") {
			flush()
			continue
		}
		if reTrimMarker.MatchString(trimmed) {
			// 裁剪标记不占行号。
			continue
		}
		if reFence.MatchString(trimmed) {
			if current != nil {
				flush()
			}
			inFence = !inFence
			if inFence {
				current = &mockFileBlock{repoKey: "unknown", path: "unknown", startLine: 1}
			} else {
				flush()
			}
			continue
		}
		if current == nil {
			continue
		}
		if inFence || current.repoKey != "unknown" {
			// 剥离行尾 \r：补丁的上下文行不带 \r，这里统一按 LF 语义处理，
			// 保证生成的补丁在 CRLF 检出的工作区中同样可以应用。
			current.lines = append(current.lines, strings.TrimSuffix(line, "\r"))
			continue
		}
		// 裸代码块（无文件头）收集到围栏结束。
		if inFence {
			current.lines = append(current.lines, strings.TrimSuffix(line, "\r"))
		}
	}
	flush()
	return blocks
}

// trimCR 剥离每行行尾的 \r（统一补丁文本为 LF 语义）。
func trimCR(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSuffix(l, "\r")
	}
	return out
}

// trimBlankLines 去掉首尾空行。
func trimBlankLines(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[start:end]
}

// mockPatchSynthesize 生成符合 PatchSchema 的最小增量补丁。
//
// 关键约束：
//   - oldSnippet 取自上下文中的**真实代码**（含可疑行的 3-5 行连续片段，保留原始缩进）；
//   - newSnippet = 按 category 选择的守卫语句 + oldSnippet 原文（原文逐行保留，
//     保证引擎可以校验"关键行未被删除"，属于最小增量修改而非重写）；
//   - 上下文中没有可用代码时返回空补丁数组与明确说明。
func mockPatchSynthesize(userText string) map[string]any {
	blocks := parseMockFileBlocks(userText)
	category, _ := detectCategoryDetail(userText)
	confidence := stableConfidence(userText)

	patches := []any{}
	// 同一文件只产出**一个**补丁：多个补丁同时改写一个文件时，
	// 后续补丁的上下文基于原始文件生成，无法与前一个补丁叠加（会判定为冲突），
	// 因此这里按文件去重，保证产出的补丁集合自洽、可全部应用。
	seenFiles := map[string]bool{}
	for _, b := range blocks {
		if len(patches) >= 3 {
			break
		}
		if b.path != "" && seenFiles[b.path] {
			continue
		}
		idx := findSuspiciousLine(b.lines)
		if idx < 0 {
			continue
		}
		start, end := snippetWindow(b.lines, idx)
		if end <= start {
			continue
		}
		// 逐个剥离行尾 \r：diff 的上下文行不带 \r，CRLF 检出的工作区会因此不匹配。
		// 文本上下文统一使用 LF 语义，保证补丁在 LF / CRLF 工作区都能应用。
		oldSnippet := strings.Join(trimCR(b.lines[start:end]), "\n")
		if strings.TrimSpace(oldSnippet) == "" {
			continue
		}
		line := b.startLine + start
		lang := detectLanguage(b.path, b.lines[idx])
		newSnippet := buildPatchedSnippet(lang, category, trimCR(b.lines), idx, start, end)
		repoKey := b.repoKey
		filePath := b.path
		if strings.TrimSpace(repoKey) == "" {
			repoKey = "unknown"
		}
		if strings.TrimSpace(filePath) == "" {
			filePath = "unknown"
		}
		if b.path != "" {
			seenFiles[b.path] = true
		}
		patches = append(patches, map[string]any{
			"repositoryId": repoKey, // 上下文中无仓库 ID 时以 repoKey 占位，引擎可据 key 反查
			"repoKey":      repoKey,
			"filePath":     filePath,
			"action":       string(domain.ActionModify),
			"line":         line,
			"oldSnippet":   oldSnippet,
			"newSnippet":   newSnippet,
			"rationale": fmt.Sprintf("在 %s:%d 的 %s 之前补充%s，原有代码逐行保留，仅做最小增量修改",
				filePath, line, guardTargetDesc(b.lines[idx]), strategyCN(guardStrategy(category))),
			"risk":       patchRisk(category, confidence),
			"confidence": confidence,
		})
	}

	if len(patches) == 0 {
		return map[string]any{
			"patches":   []any{},
			"rationale": "上下文缺少可修改代码片段",
			"risk":      string(domain.RiskLow),
		}
	}
	topRisk := string(domain.RiskLow)
	for _, p := range patches {
		pm, _ := p.(map[string]any)
		switch pm["risk"] {
		case string(domain.RiskHigh):
			topRisk = string(domain.RiskHigh)
		case string(domain.RiskMedium):
			if topRisk != string(domain.RiskHigh) {
				topRisk = string(domain.RiskMedium)
			}
		}
	}
	return map[string]any{
		"patches": patches,
		"rationale": fmt.Sprintf("针对%s，在 %d 个文件块中产出 %d 个最小增量补丁（仅插入守卫/兜底语句，不重写原有逻辑）",
			categoryCN(category), len(blocks), len(patches)),
		"risk": topRisk,
	}
}

// patchRisk 依据分类与置信度给出补丁风险。
func patchRisk(category string, confidence float64) string {
	switch category {
	case "concurrency_error", "resource_exhausted", "timeout":
		return string(domain.RiskHigh)
	case "null_pointer", "type_error":
		if confidence >= 0.8 {
			return string(domain.RiskLow)
		}
		return string(domain.RiskMedium)
	default:
		return string(domain.RiskMedium)
	}
}

// 可疑行模式：按优先级从高到低排列（命中越靠前越可疑）。
var suspiciousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+\s*\(`), // xx.yy()
	regexp.MustCompile(`[A-Za-z_$][\w$]*\s*\[[^\]]+\]`),                // list[i]
	regexp.MustCompile(`\.\s*(?:length|size|count|len)\b`),             // arr.length
	regexp.MustCompile(`[A-Za-z_$)\d\]]\s+/\s+[A-Za-z_$(\d]`),          // a / b
	regexp.MustCompile(`\.\s*(?:body|data|value|result|payload|item)\b`),
}

// findSuspiciousLine 返回最可疑的代码行下标（无则为 -1）。
func findSuspiciousLine(lines []string) int {
	best, bestScore := -1, 0
	for i, line := range lines {
		score := suspiciousScore(line)
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// suspiciousScore 计算单行可疑度（0 表示不怀疑）。
func suspiciousScore(line string) int {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || reTrimMarker.MatchString(trimmed) {
		return 0
	}
	// 跳过注释行与纯括号行，避免把注释当故障点。
	for _, prefix := range []string{"//", "#", "*", "/*", "*/", "<!--"} {
		if strings.HasPrefix(trimmed, prefix) {
			return 0
		}
	}
	if trimmed == "}" || trimmed == "{" || trimmed == ")" || trimmed == "});" {
		return 0
	}
	score := 0
	for i, re := range suspiciousPatterns {
		if re.MatchString(trimmed) {
			score += len(suspiciousPatterns) - i
		}
	}
	if score == 0 {
		return 0
	}
	// 除法模式容易把 URL / 路径误判为可疑，命中 http(s):// 时降权。
	if strings.Contains(trimmed, "://") {
		score--
	}
	return score
}

// snippetWindow 返回包含焦点行的 3-5 行窗口。
func snippetWindow(lines []string, focus int) (int, int) {
	start := focus - 2
	end := focus + 3
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end-start < 3 {
		if start == 0 {
			end = min(len(lines), 3)
		} else {
			start = max(0, end-3)
		}
	}
	return start, end
}

// guardStrategy 依据故障分类选择补丁策略。
func guardStrategy(category string) string {
	switch category {
	case "index_out_of_bounds":
		return "bounds"
	case "timeout", "config_error", "network_error", "resource_exhausted":
		return "default"
	default:
		return "nil"
	}
}

// strategyCN 返回策略的中文名。
func strategyCN(strategy string) string {
	switch strategy {
	case "bounds":
		return "边界检查"
	case "default":
		return "默认值兜底"
	default:
		return "空值守卫"
	}
}

// detectLanguage 依据文件后缀与行内容判断语言。
func detectLanguage(path, line string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".go"):
		return "go"
	case strings.HasSuffix(lower, ".java"), strings.HasSuffix(lower, ".kt"), strings.HasSuffix(lower, ".scala"):
		return "java"
	case strings.HasSuffix(lower, ".py"):
		return "python"
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"):
		return "typescript"
	case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".jsx"),
		strings.HasSuffix(lower, ".mjs"), strings.HasSuffix(lower, ".cjs"):
		return "javascript"
	case strings.HasSuffix(lower, ".php"):
		return "php"
	}
	switch {
	case strings.Contains(line, ":="), strings.HasPrefix(strings.TrimSpace(line), "func "):
		return "go"
	case strings.HasPrefix(strings.TrimSpace(line), "def "), strings.Contains(line, "self."):
		return "python"
	case strings.Contains(line, "=>"), strings.Contains(line, "const "), strings.Contains(line, "let "):
		return "javascript"
	}
	return "javascript"
}

// buildPatchedSnippet 构造 newSnippet：守卫语句 + 原始代码（逐行保留）。
func buildPatchedSnippet(lang, category string, lines []string, focus, start, end int) string {
	indent := leadingWhitespace(lines[focus])
	guard := buildGuard(lang, guardStrategy(category), lines[focus], indent, categoryCN(category))
	out := make([]string, 0, len(guard)+end-start)
	out = append(out, guard...)
	out = append(out, lines[start:end]...)
	return strings.Join(out, "\n")
}

// buildGuard 生成语言相关的守卫语句。
func buildGuard(lang, strategy, line, indent, categoryText string) []string {
	shape := analyzeLine(line)
	target := shape.recv
	if target == "" {
		target = shape.index
	}
	if target == "" {
		target = shape.divisor
	}
	if target == "" {
		target = shape.timeout
	}
	if target == "" {
		target = "result"
	}
	unit := indentUnit(lang)
	body := indent + unit

	switch strategy {
	case "bounds":
		idx := shape.index
		if idx == "" {
			idx = "index"
		}
		container := shape.recv
		if container == "" {
			container = "list"
		}
		switch lang {
		case "go":
			return []string{
				fmt.Sprintf("%sif %s < 0 || %s >= len(%s) {", indent, idx, idx, container),
				fmt.Sprintf("%s// 边界检查：%s", body, categoryText),
				fmt.Sprintf("%sreturn", body),
				indent + "}",
			}
		case "python":
			return []string{
				fmt.Sprintf("%sif %s < 0 or %s >= len(%s):", indent, idx, idx, container),
				fmt.Sprintf("%sraise IndexError(\"索引越界：%s = %%s\" %% %s)", body, idx, idx),
			}
		case "java":
			return []string{
				fmt.Sprintf("%sif (%s < 0 || %s >= %s.size()) {", indent, idx, idx, container),
				fmt.Sprintf("%sthrow new IndexOutOfBoundsException(\"索引越界：\" + %s);", body, idx),
				indent + "}",
			}
		default:
			return []string{
				fmt.Sprintf("%sif (%s < 0 || %s >= %s.length) {", indent, idx, idx, container),
				fmt.Sprintf("%sthrow new RangeError(\"索引越界：\" + %s);", body, idx),
				indent + "}",
			}
		}
	case "default":
		defTarget := shape.timeout
		if defTarget == "" {
			defTarget = target
		}
		switch lang {
		case "go":
			return []string{
				fmt.Sprintf("%s// 默认值兜底：%s", indent, categoryText),
				fmt.Sprintf("%sif %s == 0 {", indent, defTarget),
				fmt.Sprintf("%s%s = 30000", body, defTarget),
				indent + "}",
			}
		case "python":
			return []string{
				fmt.Sprintf("%s# 默认值兜底：%s", indent, categoryText),
				fmt.Sprintf("%sif not %s:", indent, defTarget),
				fmt.Sprintf("%s%s = 30000", body, defTarget),
			}
		case "java":
			return []string{
				fmt.Sprintf("%s// 默认值兜底：%s", indent, categoryText),
				fmt.Sprintf("%sif (%s == 0) {", indent, defTarget),
				fmt.Sprintf("%s%s = 30000;", body, defTarget),
				indent + "}",
			}
		default:
			return []string{
				fmt.Sprintf("%s// 默认值兜底：%s", indent, categoryText),
				fmt.Sprintf("%sif (!%s) {", indent, defTarget),
				fmt.Sprintf("%s%s = 30000;", body, defTarget),
				indent + "}",
			}
		}
	default: // nil 守卫
		switch lang {
		case "go":
			return []string{
				fmt.Sprintf("%sif %s == nil {", indent, target),
				fmt.Sprintf("%s// 空值守卫：%s", body, categoryText),
				fmt.Sprintf("%sreturn", body),
				indent + "}",
			}
		case "python":
			return []string{
				fmt.Sprintf("%sif %s is None:", indent, target),
				fmt.Sprintf("%sraise ValueError(\"%s 为空，无法继续执行\")", body, target),
			}
		case "java":
			return []string{
				fmt.Sprintf("%sif (%s == null) {", indent, target),
				fmt.Sprintf("%sthrow new IllegalStateException(\"%s 为空，无法继续执行\");", body, target),
				indent + "}",
			}
		default:
			return []string{
				fmt.Sprintf("%sif (%s == null) {", indent, target),
				fmt.Sprintf("%sthrow new Error(\"%s 为空，无法继续执行\");", body, target),
				indent + "}",
			}
		}
	}
}

// indentUnit 返回语言的缩进单位。
func indentUnit(lang string) string {
	switch lang {
	case "go":
		return "\t"
	case "python":
		return "    "
	case "java":
		return "    "
	default:
		return "  "
	}
}

// leadingWhitespace 返回行首空白。
func leadingWhitespace(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return line[:i]
		}
	}
	return ""
}

// lineShape 从代码行中抽取守卫所需表达式。
type lineShape struct {
	recv    string // 调用/索引接收者，如 req.body
	index   string // 索引表达式，如 i
	divisor string // 除数，如 b
	timeout string // 超时/重试等可兜底标识符
}

var (
	reMemberChain = regexp.MustCompile(`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*`)
	reIndexExpr   = regexp.MustCompile(`([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*\[\s*([^\[\]]+?)\s*\]`)
	reDivision    = regexp.MustCompile(`([A-Za-z_$][\w$.\[\]]*)\s+/\s+([A-Za-z_$][\w$.\[\]]*)`)
	goKeywords    = map[string]bool{
		"if": true, "for": true, "return": true, "func": true, "var": true, "const": true,
		"type": true, "range": true, "nil": true, "err": true, "int": true, "string": true,
		"true": true, "false": true, "len": true, "make": true, "new": true, "defer": true,
		"go": true, "switch": true, "case": true, "default": true, "break": true, "continue": true,
		"else": true, "struct": true, "map": true, "interface": true, "package": true, "import": true,
	}
)

// analyzeLine 抽取行内的接收者、索引、除数与可兜底标识符。
func analyzeLine(line string) lineShape {
	var shape lineShape
	if m := reIndexExpr.FindStringSubmatch(line); m != nil {
		shape.recv = m[1]
		shape.index = strings.TrimSpace(m[2])
	}
	if shape.recv == "" {
		if chains := reMemberChain.FindAllString(line, -1); len(chains) > 0 {
			chain := ""
			for _, c := range chains {
				if goKeywords[c] || isNumeric(c) {
					continue
				}
				chain = c
				break
			}
			// 形如 xx.yy() 的调用：守卫接收者而非方法本身。
			if idx := strings.Index(line, chain+"("); idx >= 0 {
				if dot := strings.LastIndex(chain, "."); dot > 0 {
					chain = chain[:dot]
				}
			}
			shape.recv = chain
		}
	}
	if m := reDivision.FindStringSubmatch(line); m != nil && !strings.Contains(line, "://") {
		shape.divisor = m[2]
	}
	shape.timeout = findDefaultTarget(line)
	return shape
}

// findDefaultTarget 找出可做默认值兜底的标识符（超时/重试/容量类变量）。
func findDefaultTarget(line string) string {
	keywords := []string{"timeout", "deadline", "expire", "retry", "interval", "limit", "capacity", "duration"}
	for _, chain := range reMemberChain.FindAllString(line, -1) {
		lower := strings.ToLower(chain)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				return chain
			}
		}
	}
	return ""
}

// isNumeric 判断字符串是否为纯数字。
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// guardTargetDesc 返回守卫目标的中文描述（用于 rationale）。
func guardTargetDesc(line string) string {
	shape := analyzeLine(line)
	switch {
	case shape.recv != "":
		return shape.recv + " 调用点"
	case shape.index != "":
		return shape.index + " 索引"
	case shape.divisor != "":
		return shape.divisor + " 除数"
	default:
		return "可疑语句"
	}
}

// ---------------------------------------------------------------------------
// 阶段三：分类 / 日志清洗 / 汇总 / 验证解释
// ---------------------------------------------------------------------------

// mockClassify 输出故障分类结果。
func mockClassify(userText string) map[string]any {
	category, keywords := detectCategoryDetail(userText)
	reason := fmt.Sprintf("依据异常文本关键词判定为 %s", categoryCN(category))
	if len(keywords) > 0 {
		reason = fmt.Sprintf("依据关键词「%s」判定为 %s", strings.Join(keywords, "、"), categoryCN(category))
	}
	return map[string]any{
		"category":   category,
		"severity":   severityFor(category),
		"confidence": stableConfidence(userText),
		"reason":     reason,
	}
}

// noiseLinePatterns 日志噪声行特征（健康检查、心跳、调试级别等）。
var noiseLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(DEBUG|TRACE)\b`),
	regexp.MustCompile(`(?i)/healthz|/readyz|/actuator/health|/metrics`),
	regexp.MustCompile(`(?i)\bheartbeat\b|心跳`),
	regexp.MustCompile(`(?i)\bGC\s*\(|\bFull GC\b`),
	regexp.MustCompile(`(?i)Connection pooled|pool stats`),
}

// mockLogClean 输出清洗后的日志与噪声占比。
func mockLogClean(userText string) map[string]any {
	rawLines := strings.Split(strings.ReplaceAll(userText, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(rawLines))
	removed := 0
	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			removed++
			continue
		}
		if isNoiseLine(trimmed) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		// 极端情况下不返回空内容，保留前 20 行供人工判断。
		kept = rawLines[:min(len(rawLines), 20)]
		removed = len(rawLines) - len(kept)
	}
	ratio := 0.0
	if len(rawLines) > 0 {
		ratio = float64(removed) / float64(len(rawLines))
	}
	return map[string]any{
		"cleaned":    strings.Join(kept, "\n"),
		"noiseRatio": math.Round(ratio*1000) / 1000,
		"removed":    removed,
		"kept":       len(kept),
	}
}

// isNoiseLine 判断日志行是否为噪声。
func isNoiseLine(line string) bool {
	for _, re := range noiseLinePatterns {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// mockSummary 输出结论汇总与后续动作。
func mockSummary(userText string) map[string]any {
	exc := firstException(userText)
	file, line, _ := firstFrame(userText)
	category, _ := detectCategoryDetail(userText)
	excType := exc.Type
	if excType == "" {
		excType = "未识别异常"
	}
	location := "日志未给出文件行号"
	if file != "" {
		location = fmt.Sprintf("%s:%d", cleanPath(file), line)
	}
	return map[string]any{
		"summary": fmt.Sprintf("%s 在 %s 触发，归类为%s；建议按最小增量补丁修复后回归验证",
			excType, location, categoryCN(category)),
		"actions": []any{
			fmt.Sprintf("复核 %s 的入参与空值/边界校验", location),
			"对可疑调用点补充守卫语句并生成 unified diff",
			"在沙箱中执行编译与用例验证，确认无回归后归档报告",
		},
		"highlights": []any{
			"异常类型：" + excType,
			"故障分类：" + categoryCN(category),
		},
	}
}

// mockVerifyExplain 输出验证解释与后续动作。
func mockVerifyExplain(userText string) map[string]any {
	passed := !strings.Contains(strings.ToLower(userText), "失败") &&
		!strings.Contains(strings.ToLower(userText), "failed")
	explanation := "补丁已按最小增量方式应用，静态检查通过；建议补充针对该故障分支的单元测试"
	if !passed {
		explanation = "检测到验证失败信息：补丁可能与其他调用点耦合，建议缩小改动范围后重新验证"
	}
	return map[string]any{
		"explanation": explanation,
		"nextActions": []any{
			"针对故障分支补充单元测试并纳入回归集",
			"复核同仓库其它相似调用点是否存在同类缺陷",
		},
		"passed": passed,
	}
}

// ---------------------------------------------------------------------------
// 故障分类与定级
// ---------------------------------------------------------------------------

// categoryRule 关键词 → 分类规则（顺序敏感：越靠前优先级越高）。
type categoryRule struct {
	category string
	keywords []string
}

var categoryRules = []categoryRule{
	{"null_pointer", []string{"nullpointer", "null pointer", "nil pointer", "invalid memory address or nil", "nil dereference",
		"is null", "cannot invoke", "nullreferenceexception", "nonetype", "nil map", "空指针"}},
	{"index_out_of_bounds", []string{"index out of bounds", "indexerror", "index out of range", "arrayindexoutofbound",
		"list index out of range", "outofbounds", "string index out of range", "数组越界", "索引越界"}},
	{"type_error", []string{"cannot read properties of undefined", "cannot read properties of null", "cannot read property",
		"typeerror", "is not a function", "undefined is not", "is not assignable", "classcastexception", "类型错误"}},
	{"dependency_missing", []string{"noclassdeffounderror", "modulenotfounderror", "cannot find module", "classnotfoundexception",
		"no module named", "importerror", "module not found", "could not resolve dependency", "nosuchmethoderror", "依赖缺失"}},
	{"timeout", []string{"timeout", "timed out", "etimedout", "deadline exceeded", "socket timeout", "read timeout", "超时"}},
	{"syntax_error", []string{"syntaxerror", "syntax error", "unexpected token", "parse error", "invalid syntax", "语法错误"}},
	{"concurrency_error", []string{"deadlock", "concurrent modification", "data race", "race detected", "concurrentmodificationexception", "死锁"}},
	{"serialization_error", []string{"json parse", "jsondecodeerror", "unmarshal", "deserialization", "invalid character", "decode error", "序列化"}},
	{"resource_exhausted", []string{"outofmemory", "out of memory", "too many open files", "resource exhausted", "no space left",
		"disk quota", "connection pool exhausted", "资源耗尽"}},
	{"network_error", []string{"connection refused", "econnrefused", "no such host", "network is unreachable", "broken pipe",
		"connection reset", "unreachable", "网络不可达"}},
	{"config_error", []string{"unauthorized", "forbidden", "permission denied", "invalid configuration", "missing property",
		"no such property", "environment variable", "401", "403", "配置错误"}},
	{"logic_error", []string{"assertionerror", "assertion failed", "illegalstateexception", "invalid state", "business rule", "业务逻辑"}},
}

// detectCategoryDetail 依据异常文本判定分类，并返回命中的关键词。
func detectCategoryDetail(text string) (string, []string) {
	lower := strings.ToLower(text)
	for _, rule := range categoryRules {
		var hits []string
		for _, kw := range rule.keywords {
			if strings.Contains(lower, kw) {
				hits = append(hits, kw)
			}
		}
		if len(hits) > 0 {
			return rule.category, hits
		}
	}
	if exc := firstException(text); exc.Type != "" {
		return "runtime_error", []string{exc.Type}
	}
	return "unknown", nil
}

// detectCategory 仅返回分类。
func detectCategory(text string) string {
	category, _ := detectCategoryDetail(text)
	return category
}

// categoryCN 返回分类的中文名。
func categoryCN(category string) string {
	switch category {
	case "null_pointer":
		return "空指针/空值异常"
	case "index_out_of_bounds":
		return "索引越界异常"
	case "type_error":
		return "类型错误"
	case "dependency_missing":
		return "依赖缺失"
	case "timeout":
		return "调用超时"
	case "syntax_error":
		return "语法错误"
	case "runtime_error":
		return "运行时异常"
	case "logic_error":
		return "业务逻辑错误"
	case "config_error":
		return "配置/权限错误"
	case "network_error":
		return "网络不可达"
	case "resource_exhausted":
		return "资源耗尽"
	case "concurrency_error":
		return "并发问题"
	case "serialization_error":
		return "序列化错误"
	default:
		return "未分类异常"
	}
}

// fixDirection 返回分类对应的修复方向。
func fixDirection(category string) string {
	switch category {
	case "null_pointer":
		return "在首个解引用点之前补充空值守卫（提前返回或抛出可读异常），并为上游返回值增加判空"
	case "index_out_of_bounds":
		return "在索引访问前补充边界检查（下界与上界），并为越界场景提供安全默认分支"
	case "type_error":
		return "校验对象/属性是否存在（可选链或显式判空），避免对 undefined 取值或调用"
	case "dependency_missing":
		return "核对依赖声明与运行时类路径/模块解析路径，补齐缺失依赖或修正导入"
	case "timeout":
		return "排查下游耗时，调整超时与重试策略，并对超时结果做降级兜底"
	case "syntax_error":
		return "修正语法/配置解析错误后重新构建，避免带病发布"
	case "concurrency_error":
		return "缩小临界区并按固定顺序获取锁，避免共享可变状态被并发修改"
	case "serialization_error":
		return "对齐序列化协议与字段类型，对解析失败做容错与日志留痕"
	case "resource_exhausted":
		return "定位资源泄漏点并限制并发/缓存上限，必要时扩容"
	case "network_error":
		return "核对目标地址与网络策略，增加连接失败的重试与熔断"
	case "config_error":
		return "核对配置项与凭证权限，缺失项给出显式默认值"
	default:
		return "结合首个堆栈帧复核入参，按最小增量补丁补充防御性校验"
	}
}

// severityFor 依据分类给出定级。
func severityFor(category string) string {
	switch category {
	case "null_pointer", "index_out_of_bounds", "type_error", "dependency_missing":
		return string(domain.SeverityCritical)
	case "timeout", "syntax_error", "runtime_error", "logic_error", "serialization_error",
		"concurrency_error", "resource_exhausted", "network_error":
		return string(domain.SeverityMajor)
	default:
		return string(domain.SeverityMinor)
	}
}

// ---------------------------------------------------------------------------
// 确定性工具
// ---------------------------------------------------------------------------

// stableConfidence 由内容 sha256 派生稳定置信度（0.55 ~ 0.85）。
func stableConfidence(text string) float64 {
	sum := sha256.Sum256([]byte(text))
	v := 0.55 + float64(int(sum[0])%31)/100.0
	return math.Round(v*100) / 100
}

// firstNonEmptyLine 返回首个非空行。
func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return "(空内容)"
}
