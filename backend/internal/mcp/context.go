package mcp

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 上下文智能治理
// ---------------------------------------------------------------------------

// 上下文分节名称（固定输出顺序）。
const (
	sectionStack  = "异常堆栈"
	sectionSlices = "命中代码切片"
	sectionEdges  = "跨仓库链路"
	sectionDeps   = "依赖清单"
	sectionEnv    = "环境信息"
)

// 预算缺省值：配置与入参都未给出时使用，保证裁剪逻辑始终"真实生效"。
const (
	defaultMaxChars         = 120_000
	defaultMaxSlices        = 24
	defaultMaxSliceChars    = 12_000
	defaultMaxLogChars      = 20_000
	defaultReserveForOutput = 8_000
)

// focusWindowRadius 单个切片超预算时保留的焦点行前后行数。
const focusWindowRadius = 40

// ContextPacker 上下文智能治理实现。
//
// 预算控制是**真实生效**的：先按优先级裁剪切片、再裁剪堆栈/日志，
// 最后按章节优先级丢弃整章，必要时硬截断，保证输出字符数不超过
// MaxChars - ReserveForOutput，且每一次裁剪都会写入 Notes（中文）供报告与降级说明使用。
type ContextPacker struct {
	cfg config.ContextBudgetConfig
	log *logx.Logger
}

// NewContextPacker 构造上下文治理器。
func NewContextPacker(cfg config.ContextBudgetConfig, log *logx.Logger) *ContextPacker {
	return &ContextPacker{cfg: cfg, log: loggerOr(log)}
}

// BudgetFromConfig 把配置形态的预算转换为领域预算，便于调度层直接组装调用。
func BudgetFromConfig(cfg config.ContextBudgetConfig) domain.ContextBudget {
	return domain.ContextBudget{
		MaxChars:         cfg.MaxChars,
		MaxSlices:        cfg.MaxSlices,
		MaxSliceChars:    cfg.MaxSliceChars,
		MaxLogChars:      cfg.MaxLogChars,
		ReserveForOutput: cfg.ReserveForOutput,
	}
}

// packedSection 内部使用的分节结构。
//
// 章节分两类：固定章节直接给出 body；可裁剪章节（代码切片/依赖清单）给出按优先级
// 排序的 blocks，总预算不足时可以从尾部逐个丢弃，而不必整章丢弃。
type packedSection struct {
	name     string
	order    int // 输出顺序（越小越靠前）
	dropRank int // 丢弃优先级（越大越先被丢弃）
	header   string
	body     string
	blocks   []string
	dropped  bool
}

// text 渲染章节文本。
func (s *packedSection) text() string {
	if s.blocks == nil {
		return s.body
	}
	if len(s.blocks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(s.header)
	for i, blk := range s.blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(blk)
	}
	return b.String()
}

// Pack 依据预算组装上下文文本。
//
// 裁剪优先级（信息量从高到低）：异常堆栈 > 命中代码切片 > 跨仓库链路 > 依赖清单 > 环境信息。
// bundle 为空或证据缺失时返回可读的降级上下文而不是错误（降级不失败）。
func (p *ContextPacker) Pack(bundle *domain.EvidenceBundle, budget domain.ContextBudget) (domain.ContextPacked, error) {
	b := p.resolveBudget(budget)
	limit := b.MaxChars - b.ReserveForOutput
	if limit <= 0 {
		limit = b.MaxChars
	}
	if limit <= 0 {
		limit = defaultMaxChars
	}

	if bundle == nil {
		text := sectionHeader(sectionEnv) + "\n(证据缺失：EvidenceBundle 为空，无法组装上下文)\n"
		return domain.ContextPacked{
			Text:      text,
			Chars:     len(text),
			Sections:  []domain.ContextSection{{Name: sectionEnv, Chars: len(text)}},
			Truncated: true,
			Notes:     []string{"证据包为空，未生成有效上下文"},
		}, nil
	}

	notes := make([]string, 0, 8)
	for _, n := range bundle.Notes {
		if s := strings.TrimSpace(n); s != "" {
			notes = append(notes, s)
		}
	}
	truncated := false

	// 1) 异常堆栈 / 日志。
	stackBody, stackDropped := buildStackSection(bundle, b.MaxLogChars)
	if stackDropped > 0 {
		notes = append(notes, fmt.Sprintf("堆栈/日志超出 %d 字符预算，保留异常头部与首个 Caused by（已裁剪 %d 字符）", b.MaxLogChars, stackDropped))
		truncated = true
	}

	// 2) 命中代码切片。
	sliceBlocksList, sliceNotes, slicesDropped := buildSliceBlocks(sectionSlices, bundle.Slices, b.MaxSlices, b.MaxSliceChars, b.MaxChars)
	notes = append(notes, sliceNotes...)
	if slicesDropped {
		truncated = true
	}

	// 3) 跨仓库链路。
	edgesBody := buildEdgesSection(bundle.Edges)

	// 4) 依赖清单（与代码切片共用切片预算，但数量上限收敛到 8，避免挤占主证据）。
	depLimit := b.MaxSlices
	if depLimit > 8 {
		depLimit = 8
	}
	depBlocksList, depNotes, depsDropped := buildSliceBlocks(sectionDeps, bundle.DependencyFiles, depLimit, b.MaxSliceChars, b.MaxChars)
	notes = append(notes, depNotes...)
	if depsDropped {
		truncated = true
	}

	// 5) 环境信息。
	envBody := buildEnvSection(bundle)

	slicesSec := &packedSection{
		name: sectionSlices, order: 1, dropRank: 2,
		header: sectionHeader(sectionSlices) + "\n", blocks: sliceBlocksList,
		dropped: slicesDropped,
	}
	depsSec := &packedSection{
		name: sectionDeps, order: 3, dropRank: 4,
		header: sectionHeader(sectionDeps) + "\n", blocks: depBlocksList,
		dropped: depsDropped,
	}
	if len(sliceBlocksList) == 0 {
		slicesSec.blocks = nil
		slicesSec.body = sectionHeader(sectionSlices) + "\n(无内容)\n"
	}
	if len(depBlocksList) == 0 {
		depsSec.blocks = nil
		depsSec.body = sectionHeader(sectionDeps) + "\n(无内容)\n"
	}

	sections := []*packedSection{
		{name: sectionStack, order: 0, dropRank: 1, body: stackBody},
		slicesSec,
		{name: sectionEdges, order: 2, dropRank: 3, body: edgesBody},
		depsSec,
		{name: sectionEnv, order: 4, dropRank: 5, body: envBody},
	}

	// 切片/日志级裁剪已经把单节压到预算内，这里做总预算兜底。
	total := sectionsChars(sections)
	if total > limit {
		truncated = true
		// 阶段 A：按优先级从尾部继续裁剪可裁剪章节（切片 → 依赖），尽量保留章节结构。
		for _, s := range []*packedSection{slicesSec, depsSec} {
			removed := 0
			for total > limit && len(s.blocks) > 1 {
				s.blocks = s.blocks[:len(s.blocks)-1]
				removed++
				total = sectionsChars(sections)
			}
			if removed > 0 {
				s.dropped = true
				notes = append(notes, fmt.Sprintf("%s 按总预算再裁剪 %d 个（预算 %d 字符）", s.name, removed, limit))
			}
		}

		// 阶段 B：按丢弃优先级（dropRank 从大到小）整章丢弃，至少保留信息量最高的一章。
		order := make([]*packedSection, len(sections))
		copy(order, sections)
		sort.SliceStable(order, func(i, j int) bool { return order[i].dropRank > order[j].dropRank })
		for _, s := range order {
			if total <= limit {
				break
			}
			if s.dropped || strings.TrimSpace(s.text()) == "" {
				continue
			}
			if activeSections(sections) <= 1 {
				break
			}
			s.blocks = nil
			s.body = ""
			s.dropped = true
			total = sectionsChars(sections)
			notes = append(notes, fmt.Sprintf("总字符超出预算，丢弃「%s」章节（预算 %d 字符）", s.name, limit))
		}

		// 阶段 C：仍然超预算，对剩余章节按优先级从低到高做硬截断。
		for _, s := range order {
			if total <= limit {
				break
			}
			over := total - limit
			body := s.text()
			if strings.TrimSpace(body) == "" || len(body) <= over {
				continue
			}
			keep := len(body) - over
			cut := len(body) - keep
			s.blocks = nil
			s.body = safeSlice(body, keep) + fmt.Sprintf("\n... (已裁剪 %d 字符) ...\n", cut)
			s.dropped = true
			total = sectionsChars(sections)
			notes = append(notes, fmt.Sprintf("「%s」章节按总预算硬截断 %d 字符（预算 %d 字符）", s.name, cut, limit))
		}
	}

	sort.SliceStable(sections, func(i, j int) bool { return sections[i].order < sections[j].order })

	var sb strings.Builder
	outSections := make([]domain.ContextSection, 0, len(sections))
	for _, s := range sections {
		body := s.text()
		if strings.TrimSpace(body) != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(body)
		}
		outSections = append(outSections, domain.ContextSection{
			Name:    s.name,
			Chars:   len(body),
			Dropped: s.dropped,
		})
	}
	text := sb.String()

	// 最后一道保险：任何情况下都不超过预算。
	if len(text) > limit {
		cut := len(text) - limit
		text = safeSlice(text, limit-len("... (已裁剪) ...")) + "... (已裁剪) ..."
		truncated = true
		for i := range outSections {
			if outSections[i].Name == sectionStack {
				outSections[i].Dropped = true
			}
		}
		notes = append(notes, fmt.Sprintf("上下文总字符超出预算，已强制裁剪 %d 字符（预算 %d 字符）", cut, limit))
	}

	packed := domain.ContextPacked{
		Text:      text,
		Chars:     len(text),
		Sections:  outSections,
		Truncated: truncated,
		Notes:     notes,
	}
	p.log.Debug("上下文组装完成",
		"chars", packed.Chars,
		"limit", limit,
		"sections", len(outSections),
		"truncated", truncated,
		"notes", len(notes))
	return packed, nil
}

// resolveBudget 合并配置预算与调用方预算（调用方非零值优先）。
func (p *ContextPacker) resolveBudget(budget domain.ContextBudget) domain.ContextBudget {
	out := BudgetFromConfig(p.cfg)
	if budget.MaxChars > 0 {
		out.MaxChars = budget.MaxChars
	}
	if budget.MaxSlices > 0 {
		out.MaxSlices = budget.MaxSlices
	}
	if budget.MaxSliceChars > 0 {
		out.MaxSliceChars = budget.MaxSliceChars
	}
	if budget.MaxLogChars > 0 {
		out.MaxLogChars = budget.MaxLogChars
	}
	if budget.ReserveForOutput > 0 {
		out.ReserveForOutput = budget.ReserveForOutput
	}
	if out.MaxChars <= 0 {
		out.MaxChars = defaultMaxChars
	}
	if out.MaxSlices <= 0 {
		out.MaxSlices = defaultMaxSlices
	}
	if out.MaxSliceChars <= 0 {
		out.MaxSliceChars = defaultMaxSliceChars
	}
	if out.MaxLogChars <= 0 {
		out.MaxLogChars = defaultMaxLogChars
	}
	if out.ReserveForOutput <= 0 {
		out.ReserveForOutput = defaultReserveForOutput
	}
	if out.ReserveForOutput >= out.MaxChars {
		out.ReserveForOutput = out.MaxChars / 8
	}
	return out
}

// sectionHeader 生成章节标题。
func sectionHeader(name string) string { return "### " + name }

// sectionsChars 统计各章节字符总数。
func sectionsChars(sections []*packedSection) int {
	total := 0
	for _, s := range sections {
		total += len(s.text())
	}
	return total
}

// activeSections 统计未被丢弃的章节数。
func activeSections(sections []*packedSection) int {
	n := 0
	for _, s := range sections {
		if !s.dropped && strings.TrimSpace(s.text()) != "" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 各章节组装
// ---------------------------------------------------------------------------

// buildStackSection 组装「异常堆栈」章节；返回章节文本与被裁剪的字符数。
func buildStackSection(bundle *domain.EvidenceBundle, maxLogChars int) (string, int) {
	var b strings.Builder
	b.WriteString(sectionHeader(sectionStack))
	b.WriteString("\n")
	stack := bundle.Stack
	if stack == nil {
		b.WriteString("(无堆栈信息)\n")
		return b.String(), 0
	}

	if v := strings.TrimSpace(stack.Language); v != "" {
		fmt.Fprintf(&b, "语言: %s\n", v)
	}
	if v := strings.TrimSpace(stack.ExceptionType); v != "" {
		fmt.Fprintf(&b, "异常类型: %s\n", v)
	}
	if v := strings.TrimSpace(stack.Message); v != "" {
		fmt.Fprintf(&b, "异常消息: %s\n", truncateRunes(v, 600))
	}
	if v := strings.TrimSpace(stack.Thread); v != "" {
		fmt.Fprintf(&b, "线程: %s\n", v)
	}
	if v := strings.TrimSpace(stack.TraceID); v != "" {
		fmt.Fprintf(&b, "TraceID: %s\n", v)
	}
	if v := strings.TrimSpace(stack.Category); v != "" {
		fmt.Fprintf(&b, "初始分类: %s\n", v)
	}
	if len(stack.Services) > 0 {
		fmt.Fprintf(&b, "相关服务: %s\n", truncateRunes(strings.Join(stack.Services, ", "), 400))
	}
	if len(stack.Endpoints) > 0 {
		fmt.Fprintf(&b, "相关接口: %s\n", truncateRunes(strings.Join(stack.Endpoints, ", "), 400))
	}
	if len(stack.CausedBy) > 0 {
		b.WriteString("Caused by:\n")
		for i, c := range stack.CausedBy {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "  - %s\n", truncateRunes(strings.TrimSpace(c), 240))
		}
	}
	if len(stack.FileHints) > 0 {
		fmt.Fprintf(&b, "文件线索: %s\n", truncateRunes(strings.Join(stack.FileHints, ", "), 400))
	}
	if len(stack.Symbols) > 0 {
		fmt.Fprintf(&b, "关键符号: %s\n", truncateRunes(strings.Join(stack.Symbols, ", "), 400))
	}
	if len(stack.Frames) > 0 {
		b.WriteString("堆栈帧:\n")
		for i, f := range stack.Frames {
			if i >= 60 {
				fmt.Fprintf(&b, "  ... 其余 %d 帧已省略 ...\n", len(stack.Frames)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", frameLine(f))
		}
	}
	if v := strings.TrimSpace(stack.CleanedLog); v != "" {
		b.WriteString("清洗日志:\n")
		b.WriteString(v)
		b.WriteString("\n")
	}

	text := b.String()
	if maxLogChars > 0 && len(text) > maxLogChars {
		trimmed, dropped := trimLogText(text, maxLogChars)
		return trimmed, dropped
	}
	return text, 0
}

// frameLine 渲染单帧。
func frameLine(f domain.StackFrame) string {
	if raw := strings.TrimSpace(f.Raw); raw != "" {
		return truncateRunes(raw, 400)
	}
	parts := make([]string, 0, 3)
	if f.Package != "" {
		parts = append(parts, f.Package)
	}
	if f.Class != "" {
		parts = append(parts, f.Class)
	}
	if f.Method != "" {
		parts = append(parts, f.Method)
	}
	symbol := strings.Join(parts, ".")
	if symbol == "" {
		symbol = "未知符号"
	}
	if f.File != "" {
		return fmt.Sprintf("at %s(%s:%d)", symbol, f.File, f.Line)
	}
	return "at " + symbol
}

// trimLogText 按"保留异常头部 + 首个 Caused by"的策略裁剪超长堆栈/日志。
//
// 不是简单取前 N 字符：头部保留异常类型与最内层帧，中段丢弃，
// 尾部保留首个 Caused by 块（根因通常在那里），并在中间插入裁剪标记。
func trimLogText(text string, limit int) (string, int) {
	if limit <= 0 || len(text) <= limit {
		return text, 0
	}
	markerTmpl := "\n... (已裁剪 %d 字符，保留异常头部与首个 Caused by) ...\n"
	// 预留标记长度（按 12 位数字上界估算）。
	reserve := len(fmt.Sprintf(markerTmpl, 999999999999))
	avail := limit - reserve
	if avail < 128 {
		// 预算过小，退化为按字符边界硬截断，仍保证不超预算。
		out := safeSlice(text, maxInt(limit-len("... (已裁剪) ..."), 0)) + "... (已裁剪) ..."
		return out, len(text) - len(out)
	}
	headBudget := avail * 3 / 5
	tailBudget := avail - headBudget

	head := safeSlice(text, headBudget)
	rest := text[len(head):]
	if idx := strings.Index(rest, "Caused by"); idx >= 0 {
		cause := safeSlice(rest[idx:], tailBudget)
		dropped := len(text) - len(head) - len(cause)
		out := head + fmt.Sprintf(markerTmpl, dropped) + cause
		if len(out) > limit {
			out = safeSlice(out, limit)
			dropped = len(text) - len(out)
		}
		return out, dropped
	}
	// 无 Caused by：保留头部 + 尾部（尾部通常是最深层的根因帧）。
	tail := text[len(text)-tailBudget:]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx < len(tail)-1 {
		tail = tail[idx+1:]
	}
	dropped := len(text) - len(head) - len(tail)
	out := head + fmt.Sprintf(markerTmpl, dropped) + tail
	if len(out) > limit {
		out = safeSlice(out, limit)
		dropped = len(text) - len(out)
	}
	return out, dropped
}

// buildSliceBlocks 组装代码切片的文件块列表（排序 + 数量裁剪 + 单切片窗口裁剪）。
//
// 返回的块按优先级从高到低排列，因此总预算不足时可以直接从尾部丢弃。
func buildSliceBlocks(name string, slices []domain.CodeSlice, maxSlices, maxSliceChars, budgetChars int) ([]string, []string, bool) {
	var notes []string
	if len(slices) == 0 {
		return nil, nil, false
	}
	ordered := rankSlices(slices)
	dropped := false

	kept := ordered
	if maxSlices > 0 && len(ordered) > maxSlices {
		notes = append(notes, fmt.Sprintf("%s裁剪 %d/%d 个（预算 %d 字符）",
			name, len(ordered)-maxSlices, len(ordered), budgetChars))
		kept = ordered[:maxSlices]
		dropped = true
	}

	blocks := make([]string, 0, len(kept))
	for _, s := range kept {
		block, trimmed, cutLines := buildFileBlock(s, maxSliceChars)
		if trimmed {
			notes = append(notes, fmt.Sprintf("%s: %s 超出单切片预算 %d 字符，保留焦点行前后各 %d 行窗口（已裁剪 %d 行）",
				name, displayPathOf(s), maxSliceChars, focusWindowRadius, cutLines))
			dropped = true
		}
		if !strings.HasSuffix(block, "\n") {
			block += "\n"
		}
		blocks = append(blocks, block)
	}
	return blocks, notes, dropped
}

// rankSlices 按"焦点行数 / 是否命中堆栈 / Reason 优先级"排序（稳定排序，保持同分时的原始顺序）。
func rankSlices(slices []domain.CodeSlice) []domain.CodeSlice {
	out := make([]domain.CodeSlice, len(slices))
	copy(out, slices)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := rankScore(out[i]), rankScore(out[j])
		if si != sj {
			return si > sj
		}
		return false
	})
	return out
}

// rankScore 计算切片优先级分数。
func rankScore(s domain.CodeSlice) int {
	score := 0
	// 焦点行越多越重要（封顶 8 分，避免单一切片刷分）。
	score += min(len(s.FocusLines), 8)
	// 命中堆栈的切片优先。
	if strings.Contains(strings.ToLower(s.Reason), "frame") || strings.Contains(s.Reason, "堆栈") {
		score += 10
	}
	score += reasonRank(s.Reason)
	if !s.Truncated {
		score++ // 完整内容优先于已裁剪内容
	}
	return score
}

// reasonRank 依据命中原因给出优先级。
func reasonRank(reason string) int {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(r, "frame"):
		return 8
	case strings.Contains(r, "entry"):
		return 6
	case strings.Contains(r, "symbol"):
		return 5
	case strings.Contains(r, "import"):
		return 4
	case strings.Contains(r, "package"):
		return 3
	case strings.Contains(r, "grep"):
		return 2
	case r == "":
		return 0
	default:
		return 1
	}
}

// buildFileBlock 渲染单个文件块；超预算时保留焦点窗口。
func buildFileBlock(s domain.CodeSlice, maxSliceChars int) (string, bool, int) {
	content := strings.ReplaceAll(s.Content, "\r\n", "\n")
	base := s.StartLine
	if base <= 0 {
		base = 1
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	startLine, endLine := base, base+maxInt(len(lines)-1, 0)
	trimmed := false
	cutLines := 0

	if maxSliceChars > 0 && len(content) > maxSliceChars && len(lines) > 1 {
		lo, hi := focusWindow(s, lines, base, maxSliceChars)
		head, tail := lo, len(lines)-1-hi
		cutLines = head + tail
		var sb strings.Builder
		if head > 0 {
			fmt.Fprintf(&sb, "... (已裁剪 %d 行) ...\n", head)
		}
		sb.WriteString(strings.Join(lines[lo:hi+1], "\n"))
		if tail > 0 {
			fmt.Fprintf(&sb, "\n... (已裁剪 %d 行) ...", tail)
		}
		content = sb.String()
		startLine, endLine = base+lo, base+hi
		trimmed = true
		if len(content) > maxSliceChars {
			content = safeSlice(content, maxSliceChars) + "\n... (已裁剪) ..."
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- 文件: %s/%s @ %s (行 %d-%d) ---\n",
		displayRepoKey(s), displayPathOf(s), shortCommit(s.Commit), startLine, endLine)
	b.WriteString(strings.TrimRight(content, "\n"))
	b.WriteString("\n")
	return b.String(), trimmed, cutLines
}

// focusWindow 计算"焦点行前后各 focusWindowRadius 行"的窗口，并保证窗口内容不超预算。
func focusWindow(s domain.CodeSlice, lines []string, base, maxChars int) (int, int) {
	focus := -1
	for _, fl := range s.FocusLines {
		idx := fl - base
		if idx >= 0 && idx < len(lines) {
			focus = idx
			break
		}
	}
	if focus < 0 {
		focus = len(lines) / 2
	}
	lo := maxInt(focus-focusWindowRadius, 0)
	hi := minInt(focus+focusWindowRadius, len(lines)-1)
	// 窗口仍然超预算时对称收缩，但至少保留焦点行本身。
	for hi > lo && len(strings.Join(lines[lo:hi+1], "\n")) > maxChars {
		if hi-lo <= 2 {
			break
		}
		lo++
		hi--
	}
	// 收缩后仍超预算：从中点向外重新取一个更小的窗口。
	if len(strings.Join(lines[lo:hi+1], "\n")) > maxChars {
		lo, hi = focus, focus
		avg := 0
		for _, l := range lines {
			avg += len(l) + 1
		}
		avg = maxInt(avg/len(lines), 1)
		span := maxInt(maxChars/avg/2, 1)
		lo = maxInt(focus-span, 0)
		hi = minInt(focus+span, len(lines)-1)
	}
	return lo, hi
}

// displayRepoKey 返回仓库 key，缺失时用仓库 ID 兜底。
func displayRepoKey(s domain.CodeSlice) string {
	if v := strings.TrimSpace(s.RepoKey); v != "" {
		return v
	}
	if v := strings.TrimSpace(s.RepositoryID); v != "" {
		return v
	}
	return "unknown-repo"
}

// displayPathOf 返回文件路径。
func displayPathOf(s domain.CodeSlice) string {
	if v := strings.TrimSpace(s.Path); v != "" {
		return v
	}
	return "unknown-path"
}

// shortCommit 返回短 commit（前 8 位）。
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "unknown"
	}
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// buildEdgesSection 组装跨仓库链路章节。
func buildEdgesSection(edges []domain.CallEdge) string {
	var b strings.Builder
	b.WriteString(sectionHeader(sectionEdges))
	b.WriteString("\n")
	if len(edges) == 0 {
		b.WriteString("(无跨仓库链路)\n")
		return b.String()
	}
	for _, e := range edges {
		from, to := strings.TrimSpace(e.FromRepo), strings.TrimSpace(e.ToRepo)
		if from == "" {
			from = "unknown"
		}
		if to == "" {
			to = "unknown"
		}
		detail := make([]string, 0, 4)
		if e.Protocol != "" {
			detail = append(detail, e.Protocol)
		}
		if e.Endpoint != "" {
			detail = append(detail, e.Endpoint)
		}
		if e.Symbol != "" {
			detail = append(detail, e.Symbol)
		}
		if e.FromFile != "" {
			detail = append(detail, "from="+e.FromFile)
		}
		if e.ToFile != "" {
			detail = append(detail, "to="+e.ToFile)
		}
		fmt.Fprintf(&b, "- %s → %s (%s) 置信度 %.2f", from, to, strings.Join(detail, " "), e.Confidence)
		if e.Anomaly != "" {
			fmt.Fprintf(&b, " 异常点: %s", truncateRunes(e.Anomaly, 200))
		}
		if e.Evidence != "" {
			fmt.Fprintf(&b, " 证据: %s", truncateRunes(e.Evidence, 200))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildEnvSection 组装环境信息章节。
func buildEnvSection(bundle *domain.EvidenceBundle) string {
	var b strings.Builder
	b.WriteString(sectionHeader(sectionEnv))
	b.WriteString("\n")
	fmt.Fprintf(&b, "RunID: %s\n", orDash(bundle.RunID))
	fmt.Fprintf(&b, "TenantID: %s\n", orDash(bundle.TenantID))
	if !bundle.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "证据生成时间: %s\n", bundle.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(&b, "已加载文件数: %d\n", bundle.FilesLoaded)
	fmt.Fprintf(&b, "代码字符数: %d\n", bundle.CodeChars)
	fmt.Fprintf(&b, "候选仓库数: %d\n", len(bundle.CandidateRepos))
	for _, c := range bundle.CandidateRepos {
		key := c.RepoKey
		if key == "" {
			key = c.RepositoryID
		}
		fmt.Fprintf(&b, "  - %s (layer=%s score=%.2f 命中=%s)\n",
			key, c.Layer, c.Score, truncateRunes(strings.Join(c.MatchedBy, ","), 120))
	}
	fmt.Fprintf(&b, "证据是否已裁剪: %t\n", bundle.Truncated)
	return b.String()
}

// orDash 空串显示为 "-"。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// Token 估算与拟合
// ---------------------------------------------------------------------------

// EstimateTokens 估算文本 Token 数。
//
// 混合口径：中文（CJK）按 1.6 字符/token，其余（英文/数字/符号）按 4 字符/token，
// 两者分别计算后向上取整，空文本返回 0。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range text {
		if isCJKRune(r) {
			cjk++
		} else {
			other++
		}
	}
	tokens := float64(cjk)/1.6 + float64(other)/4.0
	if tokens < 1 {
		return 1
	}
	return int(math.Ceil(tokens))
}

// FitToTokens 把文本裁剪到指定 Token 预算内。
//
// 返回 (裁剪后的文本, 是否发生裁剪)。裁剪按 rune 边界进行并附加中文标记，
// 保证返回值重新估算后仍不超过预算。
func FitToTokens(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 {
		return "", text != ""
	}
	if EstimateTokens(text) <= maxTokens {
		return text, false
	}
	const marker = "\n... (已按 Token 预算裁剪) ..."
	budget := maxTokens - EstimateTokens(marker)
	if budget < 1 {
		return "", true
	}
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if EstimateTokens(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(runes[:lo]), "\n") + marker, true
}

// isCJKRune 判断是否为中日韩字符或全角标点。
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一表意文字
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容表意文字
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK 标点
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角字符
		return true
	}
	return unicode.Is(unicode.Han, r)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// safeSlice 按字节上限截断字符串，并回退到合法的 UTF-8 边界。
func safeSlice(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// maxInt / minInt 返回较大/较小值。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
