// Package sandbox 实现第七层的执行侧：源码工作区隔离、增量补丁生成/校验、
// 受限命令执行与静态校验兜底。
//
// 本包只依赖 domain / config / platform/logx，刻意不 import internal/source，
// 以免与并行开发的源码层形成编译期循环依赖；源码物化能力通过本包定义的
// Materializer 接口由上层（cmd/server 或 engine）注入。
package sandbox

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 常量与错误
// ---------------------------------------------------------------------------

// defaultContextLines unified diff 默认上下文行数（与 git diff -U3 对齐）。
const defaultContextLines = 3

// maxEditCells 差异计算的规模护栏。
//
// LCS 动态规划的空间复杂度是 O(m*n)，对两个"完全不同"的大文件会退化为巨额内存与
// 耗时。公共前后缀裁剪（diffOperations 的 trim 阶段）之后剩余的中间块若仍超过该
// 规模，说明两边差异面积极大，此时直接退化为"整块替换"（块首若干上下文 + 全删 +
// 全插）——语义完全正确，只是 hunk 粒度更粗，可避免 OOM 与超时。
const maxEditCells = 2_000_000

// snippetPreviewLen 错误信息中附带的片段预览长度（字符数）。
const snippetPreviewLen = 80

// noNewlineMarker diff 中"文件末尾缺少换行"的标准标记。
const noNewlineMarker = `\ No newline at end of file`

var (
	// ErrSnippetNotFound oldSnippet 不是 oldContent 的精确子串。
	ErrSnippetNotFound = errors.New("sandbox: 待替换片段未在原文中找到")
	// ErrFileNotFound 补丁目标文件在内存文件集中不存在。
	ErrFileNotFound = errors.New("sandbox: 补丁目标文件不存在")
	// ErrNoChanges 新旧内容完全一致，无需生成补丁。
	ErrNoChanges = errors.New("sandbox: 新旧内容一致，未产生变更")
	// ErrUnsafePath 路径不安全（含 .. / 绝对路径 / Windows 盘符）。
	ErrUnsafePath = errors.New("sandbox: 不安全的文件路径")
	// ErrNoPatches ApplyPatch 收到空补丁切片。
	ErrNoPatches = errors.New("sandbox: 补丁列表为空")
)

// hunkHeaderRe 匹配 `@@ -1,3 +1,4 @@` 形式的区块头（兼容简写 `@@ -1 +1 @@`）。
var hunkHeaderRe = regexp.MustCompile(`^@@+ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@+(.*)$`)

// ---------------------------------------------------------------------------
// 对外类型
// ---------------------------------------------------------------------------

// ApplyError 补丁应用失败。
//
// Hunk 为失败 hunk 的序号（1-based，0 表示与具体 hunk 无关的失败，例如路径非法或
// 文件不存在）；Reason 为中文失败原因，便于直接回填到验证日志与前端。
type ApplyError struct {
	// FilePath 出错的文件路径。
	FilePath string
	// Hunk 失败 hunk 的序号（1-based，0 表示非 hunk 级失败）。
	Hunk int
	// Reason 中文失败原因。
	Reason string
	// Err 底层错误（可选）。用于保留错误链，使 errors.Is(err, ErrUnsafePath) 仍成立。
	Err error
}

// Error 实现 error 接口。
func (e *ApplyError) Error() string {
	if e == nil {
		return "sandbox: 补丁应用失败"
	}
	if e.Hunk > 0 {
		return fmt.Sprintf("补丁应用失败: 文件 %s 第 %d 个 hunk: %s", e.FilePath, e.Hunk, e.Reason)
	}
	return fmt.Sprintf("补丁应用失败: 文件 %s: %s", e.FilePath, e.Reason)
}

// Unwrap 保留错误链：优先返回显式传入的底层错误，否则按原因文本映射到已知错误。
func (e *ApplyError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Err != nil {
		return e.Err
	}
	switch {
	case strings.Contains(e.Reason, "不安全"), strings.Contains(e.Reason, "路径穿越"):
		return ErrUnsafePath
	case strings.Contains(e.Reason, "不存在"):
		return ErrFileNotFound
	}
	return nil
}

// DiffLine unified diff 中的一行。Kind 取值：' ' 上下文、'-' 删除、'+' 新增。
//
// 以 "\" 开头的元信息行（如 `\ No newline at end of file`）不进入 Lines，
// 而是折叠为 DiffHunk.NoNewlineOld / NoNewlineNew 两个标记。
type DiffLine struct {
	// Kind 行类型：' ' 上下文、'-' 删除、'+' 新增。
	Kind byte `json:"kind"`
	// Text 去掉前缀后的原始行内容（不含换行符）。
	Text string `json:"text"`
	// NoNewline 该行是其所在文件的最后一行且缺少末尾换行符。
	NoNewline bool `json:"noNewline,omitempty"`
}

// DiffHunk 一个 unified diff 区块。
type DiffHunk struct {
	// OldStart 旧文件起始行号（1-based；旧侧为空时为 0）。
	OldStart int `json:"oldStart"`
	// OldLines 旧文件在该块中的行数。
	OldLines int `json:"oldLines"`
	// NewStart 新文件起始行号（1-based；新侧为空时为 0）。
	NewStart int `json:"newStart"`
	// NewLines 新文件在该块中的行数。
	NewLines int `json:"newLines"`
	// Header 原始 @@ 头（便于前端原样展示）。
	Header string `json:"header"`
	// Lines 块内所有行（上下文 + 删除 + 新增，按文件顺序）。
	Lines []DiffLine `json:"lines"`
	// NoNewlineOld 旧文件在该块末尾缺少换行符。
	NoNewlineOld bool `json:"noNewlineOld,omitempty"`
	// NoNewlineNew 新文件在该块末尾缺少换行符。
	NoNewlineNew bool `json:"noNewlineNew,omitempty"`
}

// DiffFile 一个文件的 unified diff。
type DiffFile struct {
	// OldPath 旧文件路径（a/<path>；新增文件为 /dev/null）。
	OldPath string `json:"oldPath"`
	// NewPath 新文件路径（b/<path>；删除文件为 /dev/null）。
	NewPath string `json:"newPath"`
	// Hunks 区块列表，按 OldStart 升序。
	Hunks []DiffHunk `json:"hunks"`
	// IsNew 新增文件（--- /dev/null）。
	IsNew bool `json:"isNew,omitempty"`
	// IsDelete 删除文件（+++ /dev/null）。
	IsDelete bool `json:"isDelete,omitempty"`
}

// ---------------------------------------------------------------------------
// 补丁构建
// ---------------------------------------------------------------------------

// BuildPatch 由"原文 + 修改片段"生成可直接 git apply 的标准 unified diff 补丁。
//
// oldSnippet 必须是 oldContent 的精确子串；不是则返回 ErrSnippetNotFound（错误信息
// 附带前 80 字符片段便于排障）。片段在原文中出现多次时，使用 strings.Index 的第一次
// 出现，并在返回的 Patch.Rationale 末尾追加“（同名片段出现 N 次，已定位第 1 处）”。
//
// 返回的 Patch 会填充 UnifiedDiff / OldContent / NewContent / TouchedLines / Action
// / ID / RepositoryID / RepoKey / Risk / Rationale / Status 等字段。
func BuildPatch(repo *domain.Repository, filePath, oldContent, oldSnippet, newSnippet, rationale string, risk domain.PatchRisk) (*domain.Patch, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, &ApplyError{FilePath: filePath, Reason: "文件路径为空"}
	}
	safe, err := CheckSafeRelPath(filePath)
	if err != nil {
		return nil, err
	}
	if oldSnippet == "" {
		return nil, &ApplyError{FilePath: filePath, Reason: "oldSnippet 不能为空（新增文件请使用 BuildCreatePatch）"}
	}

	index := strings.Index(oldContent, oldSnippet)
	if index < 0 {
		return nil, fmt.Errorf("%w: 文件 %s，片段前 %d 字符: %q",
			ErrSnippetNotFound, safe, snippetPreviewLen, preview(oldSnippet, snippetPreviewLen))
	}
	occurrences := strings.Count(oldContent, oldSnippet)

	newContent := oldContent[:index] + newSnippet + oldContent[index+len(oldSnippet):]
	if newContent == oldContent {
		return nil, fmt.Errorf("%w: 文件 %s", ErrNoChanges, safe)
	}

	note := ""
	if occurrences > 1 {
		note = fmt.Sprintf("（同名片段出现 %d 次，已定位第 1 处）", occurrences)
	}
	return buildFilePatch(repo, safe, oldContent, newContent, domain.ActionModify, rationale+note, risk)
}

// BuildCreatePatch 生成新增文件的补丁（oldContent 为空）。
func BuildCreatePatch(repo *domain.Repository, filePath, newContent, rationale string, risk domain.PatchRisk) (*domain.Patch, error) {
	if strings.TrimSpace(filePath) == "" {
		return nil, &ApplyError{FilePath: filePath, Reason: "文件路径为空"}
	}
	safe, err := CheckSafeRelPath(filePath)
	if err != nil {
		return nil, err
	}
	if newContent == "" {
		return nil, fmt.Errorf("%w: 新增文件 %s 内容为空", ErrNoChanges, safe)
	}
	return buildFilePatch(repo, safe, "", newContent, domain.ActionCreate, rationale, risk)
}

// buildFilePatch 组装完整的 domain.Patch。
//
// UnifiedDiff 与 TouchedLines 出自同一份 diff 文本（UnifiedDiff → ParseDiff →
// 行号汇总），保证两者永远一致，不会出现"补丁与行号区间对不上"的问题。
func buildFilePatch(repo *domain.Repository, filePath, oldContent, newContent string, action domain.PatchAction, rationale string, risk domain.PatchRisk) (*domain.Patch, error) {
	diffText := UnifiedDiff(oldContent, newContent, filePath, filePath, defaultContextLines)
	if strings.TrimSpace(diffText) == "" {
		return nil, fmt.Errorf("%w: 文件 %s", ErrNoChanges, filePath)
	}
	files, err := ParseDiff(diffText)
	if err != nil {
		return nil, fmt.Errorf("解析生成的补丁失败: %w", err)
	}
	touched := diffTouchedLines(files)

	p := &domain.Patch{
		ID:           "patch-" + uuid.NewString(),
		FilePath:     filePath,
		Action:       action,
		UnifiedDiff:  diffText,
		OldContent:   oldContent,
		NewContent:   newContent,
		Rationale:    rationale,
		Risk:         risk,
		TouchedLines: touched,
		Status:       domain.PatchProposed,
	}
	if repo != nil {
		p.RepositoryID = repo.ID
		p.RepoKey = repo.Key
	}
	if p.Risk == "" {
		p.Risk = domain.RiskLow
	}
	p.ProducedBy = "sandbox.patch"
	return p, nil
}

// diffTouchedLines 从解析结果汇总"新文件"中的变更行号区间。
func diffTouchedLines(files []DiffFile) []domain.LineRange {
	var lines []int
	for _, f := range files {
		for _, h := range f.Hunks {
			newLine := h.NewStart
			for _, l := range h.Lines {
				switch l.Kind {
				case '-':
					// 删除行在新文件中不存在，不占用新行号。
				case '+':
					lines = append(lines, newLine)
					newLine++
				default:
					newLine++
				}
			}
		}
	}
	return groupLineNumbers(lines)
}

// groupLineNumbers 把升序行号列表合并为闭区间。
func groupLineNumbers(lines []int) []domain.LineRange {
	if len(lines) == 0 {
		return nil
	}
	sorted := append([]int(nil), lines...)
	sort.Ints(sorted)
	out := make([]domain.LineRange, 0, len(sorted))
	start, prev := sorted[0], sorted[0]
	for _, n := range sorted[1:] {
		if n == prev+1 {
			prev = n
			continue
		}
		out = append(out, domain.LineRange{Start: start, End: prev})
		start, prev = n, n
	}
	out = append(out, domain.LineRange{Start: start, End: prev})
	return out
}

// ---------------------------------------------------------------------------
// unified diff 生成
// ---------------------------------------------------------------------------

// UnifiedDiff 生成两点文本之间的 unified diff。
//
// 头部严格遵循 git 格式：
//
//	diff --git a/<path> b/<path>
//	--- a/<path>
//	+++ b/<path>
//	@@ -oldStart,oldLines +newStart,newLines @@
//
// 行首前缀为 ' '、'-'、'+'；新增文件使用 `--- /dev/null`，删除文件使用
// `+++ /dev/null`；文件末尾缺少换行时在对应行之后补
// `\ No newline at end of file`。oldText == newText 时返回空串。
func UnifiedDiff(oldText, newText, oldName, newName string, contextLines int) string {
	if oldText == newText {
		return ""
	}
	if contextLines < 0 {
		contextLines = 0
	}
	if contextLines > 100 {
		contextLines = 100
	}
	oldLines, oldTrailing := splitKeepEnds(oldText)
	newLines, newTrailing := splitKeepEnds(newText)

	ops := diffOperations(oldLines, newLines)
	// 把"末行缺少换行"的状态挂到具体的那一行上：标记必须紧跟其所属行输出，
	// 否则在 -old / +new 两侧都缺换行时标记会错位，git apply 会直接报
	// "patch does not apply"。
	markedOld, markedNew := false, false
	if !oldTrailing && len(oldLines) > 0 {
		last := oldLines[len(oldLines)-1]
		for i := len(ops) - 1; i >= 0; i-- {
			if ops[i].kind != opInsert && ops[i].line == last {
				ops[i].noNewline = true
				markedOld = true
				break
			}
		}
	}
	if !newTrailing && len(newLines) > 0 {
		last := newLines[len(newLines)-1]
		for i := len(ops) - 1; i >= 0; i-- {
			if ops[i].kind != opDelete && ops[i].line == last {
				ops[i].noNewline = true
				markedNew = true
				break
			}
		}
	}
	// 边界场景：新旧内容逐行完全相同，差异仅在末尾换行符（a\nb / a\nb\n）。
	// 此时操作序列全是上下文行，必须把"变化的那一侧标记"上提到最后一行，
	// 否则会退化成空 diff 而丢失这次变更。
	if len(ops) > 0 {
		if !oldTrailing && !markedOld && oldTrailing != newTrailing {
			for i := len(ops) - 1; i >= 0; i-- {
				if ops[i].kind != opInsert {
					ops[i].noNewline = true
					break
				}
			}
		}
		if !newTrailing && !markedNew && oldTrailing != newTrailing {
			for i := len(ops) - 1; i >= 0; i-- {
				if ops[i].kind != opDelete {
					ops[i].noNewline = true
					break
				}
			}
		}
	}

	hunks := buildHunks(ops, oldLines, newLines, contextLines, oldTrailing, newTrailing)
	if len(hunks) == 0 {
		return ""
	}

	oldPath := normalizeDiffPath(oldName)
	newPath := normalizeDiffPath(newName)
	isNew := oldText == "" && newText != ""
	isDelete := newText == "" && oldText != ""
	oldHeader := "a/" + oldPath
	newHeader := "b/" + newPath
	if isNew {
		oldHeader = "/dev/null"
	}
	if isDelete {
		newHeader = "/dev/null"
	}

	var b strings.Builder
	b.WriteString("diff --git a/" + oldPath + " b/" + newPath + "\n")
	// 必须输出 `new file mode` / `deleted file mode`：否则 git apply 会把
	// `--- /dev/null` 解析成仓库内名为 dev/null 的文件，报
	// "dev/null: No such file or directory"。
	switch {
	case isNew:
		b.WriteString("new file mode 100644\n")
	case isDelete:
		b.WriteString("deleted file mode 100644\n")
	}
	b.WriteString("--- " + oldHeader + "\n")
	b.WriteString("+++ " + newHeader + "\n")
	for _, h := range hunks {
		b.WriteString(renderHunk(h))
	}
	return b.String()
}

// renderHunk 渲染单个区块。行类型字节本身即行首前缀（' ' / '-' / '+'），
// 末尾换行标记紧跟其所属行输出。
func renderHunk(h opHunk) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(h.oldStart, h.oldCount), hunkRange(h.newStart, h.newCount))
	for _, ol := range h.ops {
		b.WriteString(string(ol.kind) + ol.line + "\n")
		if ol.noNewline {
			b.WriteString(noNewlineMarker + "\n")
		}
	}
	return b.String()
}

// hunkRange 渲染 `start,count`；单行块简写为 `start`（与 git 的紧凑写法一致）。
func hunkRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(count)
}

// normalizeDiffPath 归一化 diff 头部使用的相对路径。
func normalizeDiffPath(p string) string {
	s := strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimPrefix(s, "/")
	// 防御：调用方若误传工作区绝对路径，尽量只保留仓库内的相对部分。
	if i := strings.Index(s, ":/"); i >= 0 {
		s = strings.TrimPrefix(s[i+2:], "/")
	}
	if s == "" {
		s = "unknown"
	}
	return s
}

// ---------------------------------------------------------------------------
// unified diff 解析
// ---------------------------------------------------------------------------

// ParseDiff 解析 unified diff 为结构化 hunk 列表（供前端展示与校验）。
//
// 注意：**只**剥离结构行（diff --git / --- / +++ / @@）行尾的 '\r'，正文行的 '\r'
// 原样保留——CRLF 文件的上下文行内容本身就带 '\r'，剥离会导致上下文匹配失败。
func ParseDiff(diff string) ([]DiffFile, error) {
	if strings.TrimSpace(diff) == "" {
		return nil, nil
	}
	raw := strings.Split(diff, "\n")
	// 末尾通常是空串（diff 以 \n 结尾），仅剥掉最后一个空串。
	if n := len(raw); n > 0 && raw[n-1] == "" {
		raw = raw[:n-1]
	}

	var (
		files   []DiffFile
		cur     *DiffFile
		curHunk *DiffHunk
	)
	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.Hunks = append(cur.Hunks, *curHunk)
			curHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}
	ensureFile := func() *DiffFile {
		if cur == nil {
			cur = &DiffFile{}
		}
		return cur
	}

	for i, rawLine := range raw {
		// 结构行（diff --git / --- / +++ / @@）才允许剥离行尾 '\r'；
		// 正文行保留 '\r'，否则 CRLF 文件的上下文行永远匹配不上。
		line := rawLine
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "@@") {
			line = strings.TrimSuffix(line, "\r")
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			f := ensureFile()
			f.OldPath, f.NewPath = parseGitHeader(line)
		case strings.HasPrefix(line, "--- "):
			flushHunk()
			f := ensureFile()
			f.OldPath = parseFileHeader(line[4:])
			if f.OldPath == "/dev/null" {
				f.IsNew = true
			}
		case strings.HasPrefix(line, "+++ "):
			f := ensureFile()
			f.NewPath = parseFileHeader(line[4:])
			if f.NewPath == "/dev/null" {
				f.IsDelete = true
			}
		case strings.HasPrefix(line, "@@"):
			flushHunk()
			ensureFile()
			h, ok := parseHunkHeader(line)
			if !ok {
				return nil, fmt.Errorf("sandbox: 无法解析 hunk 头（第 %d 行）: %q", i+1, truncateRunes(line, snippetPreviewLen))
			}
			curHunk = h
		case strings.HasPrefix(line, `\`):
			// `\ No newline at end of file`
			if curHunk != nil && len(curHunk.Lines) > 0 {
				last := len(curHunk.Lines) - 1
				curHunk.Lines[last].NoNewline = true
				if curHunk.Lines[last].Kind == '+' {
					curHunk.NoNewlineNew = true
				} else {
					curHunk.NoNewlineOld = true
				}
			}
		case curHunk != nil:
			var kind byte
			var text string
			switch line[0] {
			case ' ', '-', '+':
				kind = line[0]
				text = line[1:]
			case '\t':
				// 容错：部分工具用 Tab 代替上下文行的空格前缀。
				kind = ' '
				text = line
			default:
				return nil, fmt.Errorf("sandbox: 无法解析 diff 行（第 %d 行）: %q", i+1, truncateRunes(line, snippetPreviewLen))
			}
			curHunk.Lines = append(curHunk.Lines, DiffLine{Kind: kind, Text: text})
		default:
			if cur == nil && strings.HasPrefix(line, "Index:") {
				continue // 兼容 svn 风格头部，忽略
			}
			if cur == nil && strings.TrimSpace(line) != "" {
				return nil, fmt.Errorf("sandbox: 非法的 unified diff（第 %d 行）: %q", i+1, truncateRunes(line, snippetPreviewLen))
			}
		}
	}
	flushFile()
	if len(files) == 0 {
		return nil, fmt.Errorf("sandbox: unified diff 中未找到任何文件段")
	}
	return files, nil
}

// parseGitHeader 解析 `diff --git a/x b/y`。
func parseGitHeader(line string) (string, string) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "diff --git"))
	fields := strings.Fields(rest)
	if len(fields) >= 2 {
		return stripPathPrefix(fields[0]), stripPathPrefix(fields[1])
	}
	if len(fields) == 1 {
		p := stripPathPrefix(fields[0])
		return p, p
	}
	return "", ""
}

// parseFileHeader 解析 `--- a/x` / `+++ b/y`（丢弃行尾的 Tab 时间戳）。
func parseFileHeader(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	return stripPathPrefix(strings.TrimSpace(s))
}

// stripPathPrefix 去掉 a/ b/ 前缀并统一分隔符。
func stripPathPrefix(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\\", "/"))
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		s = s[2:]
	}
	return s
}

// parseHunkHeader 解析 `@@ -1,3 +1,4 @@ context`。
func parseHunkHeader(line string) (*DiffHunk, bool) {
	m := hunkHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	pick := func(idx, def int) int {
		if m[idx] == "" {
			return def
		}
		n, err := strconv.Atoi(m[idx])
		if err != nil {
			return def
		}
		return n
	}
	h := &DiffHunk{
		OldStart: pick(1, 0),
		OldLines: pick(2, 1),
		NewStart: pick(3, 0),
		NewLines: pick(4, 1),
		Header:   strings.TrimSpace(line),
	}
	if m[5] != "" {
		h.Header = strings.TrimSpace(m[5])
	}
	return h, true
}

// ---------------------------------------------------------------------------
// unified diff 应用
// ---------------------------------------------------------------------------

// ApplyUnifiedDiff 将 unified diff 应用到内容上。
//
// 严格校验上下文行：任一上下文行不匹配即返回 *ApplyError（含失败 hunk 序号与原因）。
// 支持 hunk 偏移校正：前面 hunk 改变行数后，后续 hunk 按已应用偏移重新定位。
// 空 diff 返回原内容。
func ApplyUnifiedDiff(content, diff string) (string, error) {
	if strings.TrimSpace(diff) == "" {
		return content, nil
	}
	files, err := ParseDiff(diff)
	if err != nil {
		return "", &ApplyError{Reason: err.Error()}
	}
	if len(files) == 0 {
		return content, nil
	}
	target := fileTargetPath(files[0])
	out := content
	for _, f := range files {
		if len(files) > 1 && fileTargetPath(f) != target {
			return "", &ApplyError{FilePath: f.OldPath, Reason: "一个 diff 中包含多个文件，请使用 ApplyPatch 按文件应用"}
		}
		out, err = applyDiffFile(out, f)
		if err != nil {
			var ae *ApplyError
			if errors.As(err, &ae) && ae.FilePath == "" {
				ae.FilePath = target
			}
			return "", err
		}
	}
	return out, nil
}

// applyDiffFile 把单个文件的 hunks 应用到内容上（按行重建，不改动入参）。
//
// 换行符容错：diff 的上下文/删除行**不带** `\r`，而工作区文件可能是 CRLF
// （例如 Windows 上 Git 默认 core.autocrlf=true 的检出结果）。此时逐字节比较会
// 因行尾 `\r` 而误判"上下文不匹配"。因此本函数在**比较**时忽略行尾 `\r`，
// 但保留原文行（含 `\r`）原样输出，从而兼容 LF / CRLF 两种工作区。
func applyDiffFile(content string, f DiffFile) (string, error) {
	if f.IsDelete {
		return "", nil
	}
	lines, trailing := splitKeepEnds(content)
	out := make([]string, 0, len(lines))
	// outNoNewline / outAdded 与 out 一一对应：
	//   outNoNewline 该行是否带有"缺少末尾换行"标记（仅对原文末行或新增行有意义）；
	//   outAdded 该行是否为 + 新增行（决定末尾换行状态是继承原文还是取标记）。
	outNoNewline := make([]bool, 0, len(lines))
	outAdded := make([]bool, 0, len(lines))
	cursor := 0 // 已消费到 lines 的位置（0-based）

	// sameLine 比较内容是否等价（忽略行尾 \r 差异）。
	sameLine := func(a, b string) bool {
		if a == b {
			return true
		}
		return strings.TrimSuffix(a, "\r") == strings.TrimSuffix(b, "\r")
	}

	appendLine := func(text string, noNewline, added bool) {
		out = append(out, text)
		outNoNewline = append(outNoNewline, noNewline)
		outAdded = append(outAdded, added)
	}

	for idx, h := range f.Hunks {
		hunkNo := idx + 1
		// 关键：hunk 头里的 oldStart 是**未修改源文件**中的绝对行号，而 cursor 始终
		// 指向未修改源文件里的位置，所以定位不需要任何偏移修正——前面 hunk 造成的行数
		// 变化只影响"输出的新内容"，不影响后续 hunk 在源文件中的锚点（这正是 unified
		// diff 能容忍行数变化的原因）。
		want := h.OldStart - 1
		if want < 0 {
			want = 0
		}
		if want < cursor || want > len(lines) {
			return "", &ApplyError{
				Hunk:   hunkNo,
				Reason: fmt.Sprintf("hunk 定位越界：期望从第 %d 行开始，可用范围 %d-%d", want+1, cursor+1, len(lines)),
			}
		}
		for _, l := range lines[cursor:want] {
			appendLine(l, false, false)
		}
		cursor = want

		for j, l := range h.Lines {
			switch l.Kind {
			case ' ':
				if cursor >= len(lines) {
					return "", &ApplyError{Hunk: hunkNo, Reason: fmt.Sprintf("上下文行超出文件末尾（hunk 内第 %d 行）: %q", j+1, truncateRunes(l.Text, snippetPreviewLen))}
				}
				if !sameLine(lines[cursor], l.Text) {
					return "", &ApplyError{
						Hunk: hunkNo,
						Reason: fmt.Sprintf("上下文不匹配（hunk 内第 %d 行，文件第 %d 行）：期望 %q，实际 %q",
							j+1, cursor+1, truncateRunes(l.Text, snippetPreviewLen), truncateRunes(lines[cursor], snippetPreviewLen)),
					}
				}
				appendLine(lines[cursor], l.NoNewline, false)
				cursor++
			case '-':
				if cursor >= len(lines) {
					return "", &ApplyError{Hunk: hunkNo, Reason: fmt.Sprintf("待删除行超出文件末尾（hunk 内第 %d 行）: %q", j+1, truncateRunes(l.Text, snippetPreviewLen))}
				}
				if !sameLine(lines[cursor], l.Text) {
					return "", &ApplyError{
						Hunk: hunkNo,
						Reason: fmt.Sprintf("待删除行不匹配（hunk 内第 %d 行，文件第 %d 行）：期望 %q，实际 %q",
							j+1, cursor+1, truncateRunes(l.Text, snippetPreviewLen), truncateRunes(lines[cursor], snippetPreviewLen)),
					}
				}
				cursor++
			case '+':
				appendLine(l.Text, l.NoNewline, true)
			default:
				return "", &ApplyError{Hunk: hunkNo, Reason: fmt.Sprintf("非法的行类型 %q", string(l.Kind))}
			}
		}
	}

	// 尾部未被任何 hunk 覆盖的行原样保留。
	for _, l := range lines[cursor:] {
		appendLine(l, false, false)
	}
	if len(out) == 0 {
		return "", nil
	}
	// 结果文件末行是否有换行：
	//   - 末行来自 + 新增行（或纯插入块）→ 取该行的 NoNewline 标记；
	//   - 末行来自原文 → 只有它仍是原文的最后一个"行"时才继承原文的换行状态，
	//     否则它后面必然跟着换行（例如 `-c` / `+c\n` 这种仅改换行符的补丁）。
	outTrailing := trailing
	if outAdded[len(out)-1] {
		outTrailing = !outNoNewline[len(out)-1]
	} else if len(lines) == 0 || lines[len(lines)-1] != out[len(out)-1] {
		outTrailing = true
	}
	if outNoNewline[len(out)-1] {
		outTrailing = false
	}
	return joinWithTrailing(out, outTrailing), nil
}

// fileTargetPath 返回补丁实际作用的相对路径（优先新路径）。
func fileTargetPath(f DiffFile) string {
	if f.NewPath != "" && f.NewPath != "/dev/null" {
		return f.NewPath
	}
	if f.OldPath != "" && f.OldPath != "/dev/null" {
		return f.OldPath
	}
	return ""
}

// ---------------------------------------------------------------------------
// 内存文件集应用
// ---------------------------------------------------------------------------

// ApplyPatch 把补丁应用到"路径 → 内容"的内存文件集上。
//
// files 的 key 为相对路径；返回全新的文件集（不修改入参）。任何补丁失败都会整体
// 返回错误，保证"全成功或全不改"的原子语义。
func ApplyPatch(files map[string]string, p domain.Patch) (map[string]string, error) {
	return ApplyPatches(files, []domain.Patch{p})
}

// ApplyPatches 批量应用补丁，语义与 ApplyPatch 一致（全成功或全不改）。
func ApplyPatches(files map[string]string, patches []domain.Patch) (map[string]string, error) {
	if len(patches) == 0 {
		return nil, ErrNoPatches
	}
	next := make(map[string]string, len(files)+len(patches))
	for k, v := range files {
		next[k] = v
	}
	for i := range patches {
		if err := applyOnePatch(next, patches[i]); err != nil {
			return nil, err
		}
	}
	return next, nil
}

// applyOnePatch 把单个补丁写入文件集。
func applyOnePatch(files map[string]string, p domain.Patch) error {
	target, err := patchRelativePath(p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(p.UnifiedDiff) == "" {
		// 无 diff 文本时只能依赖 NewContent（兼容外部直接构造的 Patch）。
		if p.Action == domain.ActionCreate || p.Action == domain.ActionModify {
			files[target] = p.NewContent
			return nil
		}
		return &ApplyError{FilePath: target, Reason: "补丁缺少 UnifiedDiff"}
	}
	content, ok := lookupFile(files, target)
	if !ok && p.OldContent != "" {
		content, ok = p.OldContent, true
	}
	if !ok && p.Action != domain.ActionCreate {
		return &ApplyError{FilePath: target, Reason: "文件不存在于工作区文件集"}
	}

	updated, err := ApplyUnifiedDiff(content, p.UnifiedDiff)
	if err != nil {
		var ae *ApplyError
		if errors.As(err, &ae) {
			ae.FilePath = target
			return ae
		}
		return &ApplyError{FilePath: target, Reason: err.Error()}
	}
	if p.Action == domain.ActionDelete {
		delete(files, target)
		delete(files, p.FilePath)
		return nil
	}
	files[target] = updated
	if p.FilePath != "" && p.FilePath != target {
		delete(files, p.FilePath)
	}
	return nil
}

// patchRelativePath 求补丁的相对目标路径，并做路径穿越防护。
func patchRelativePath(p domain.Patch) (string, error) {
	candidates := []string{p.FilePath}
	if files, err := ParseDiff(p.UnifiedDiff); err == nil && len(files) > 0 {
		candidates = append(candidates, fileTargetPath(files[0]))
	}
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		rel, err := CheckSafeRelPath(c)
		if err != nil {
			// 统一包装为 *ApplyError，便于调用方用 errors.As 归类；
			// Err 保留原始错误链，使 errors.Is(err, ErrUnsafePath) 同时成立。
			return "", &ApplyError{FilePath: c, Reason: err.Error(), Err: err}
		}
		return rel, nil
	}
	return "", &ApplyError{FilePath: p.FilePath, Reason: "补丁缺少文件路径"}
}

// lookupFile 兼容绝对/带前缀 key 的宽松查找。
func lookupFile(files map[string]string, target string) (string, bool) {
	if v, ok := files[target]; ok {
		return v, true
	}
	for k, v := range files {
		if strings.HasSuffix(strings.ReplaceAll(k, "\\", "/"), "/"+target) {
			return v, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 路径安全
// ---------------------------------------------------------------------------

// CheckSafeRelPath 校验并归一化一个相对路径，拒绝路径穿越。
//
// 拒绝规则：空路径、绝对路径（/ 或 \）、Windows 盘符（C:）、任何 ".." 段、
// "~" 开头的家目录引用、URL 形式路径、含 NUL 字符的路径。
// 返回值统一使用 "/" 分隔符并去掉 "./" 前缀。
func CheckSafeRelPath(p string) (string, error) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrUnsafePath)
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: 路径含 NUL 字符", ErrUnsafePath)
	}
	s := strings.ReplaceAll(raw, "\\", "/")
	if strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("%w: 不允许绝对路径 %q", ErrUnsafePath, raw)
	}
	if strings.HasPrefix(s, "~") {
		return "", fmt.Errorf("%w: 不允许家目录引用 %q", ErrUnsafePath, raw)
	}
	if len(s) >= 2 && isDriveLetter(s[0]) && s[1] == ':' {
		return "", fmt.Errorf("%w: 不允许 Windows 盘符路径 %q", ErrUnsafePath, raw)
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("%w: 不允许 URL 形式路径 %q", ErrUnsafePath, raw)
	}
	cleaned := path.Clean(s)
	if cleaned == "." || cleaned == "/" || cleaned == "" {
		return "", fmt.Errorf("%w: 路径为空 %q", ErrUnsafePath, raw)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: 路径穿越 %q", ErrUnsafePath, raw)
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: 路径穿越 %q", ErrUnsafePath, raw)
		}
	}
	return cleaned, nil
}

// ContainsTraversal 判断路径是否包含穿越意图（供上层快速前置拦截）。
func ContainsTraversal(p string) bool {
	s := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return true
	}
	if len(s) >= 2 && isDriveLetter(s[0]) && s[1] == ':' {
		return true
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ---------------------------------------------------------------------------
// 差异算法（LCS 动态规划 + 公共前后缀裁剪）
// ---------------------------------------------------------------------------

type opKind byte

const (
	opEqual  opKind = ' '
	opDelete opKind = '-'
	opInsert opKind = '+'
)

type opLine struct {
	kind opKind
	line string
	// noNewline 该行是对应文件的最后一行且缺少末尾换行符。
	noNewline bool
}

// opHunk 是尚未渲染的一个区块。
type opHunk struct {
	ops      []opLine
	oldStart int // 1-based；旧侧为空时为 0
	newStart int // 1-based；新侧为空时为 0
	oldCount int
	newCount int
}

// diffOperations 返回覆盖两边的编辑操作序列。
//
// 实现要点：
//  1. 先裁掉公共前/后缀，这一步对"整体相同、局部改动"的真实场景能把问题规模压到很小
//     （12000 行文件改 1 行 → DP 规模 1×1）；
//  2. 中间块用经典 LCS 动态规划求最长公共子序列，再回溯出编辑操作；
//  3. 中间块规模超过 maxEditCells 时退化为"整块替换"，避免 OOM / 超时。
func diffOperations(a, b []string) []opLine {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	midA := a[prefix : len(a)-suffix]
	midB := b[prefix : len(b)-suffix]

	ops := make([]opLine, 0, len(a)+len(b))
	for i := 0; i < prefix; i++ {
		ops = append(ops, opLine{kind: opEqual, line: a[i]})
	}
	ops = append(ops, midOps(midA, midB)...)
	for i := len(a) - suffix; i < len(a); i++ {
		ops = append(ops, opLine{kind: opEqual, line: a[i]})
	}
	return ops
}

// midOps 处理中间块：任一侧为空则直接拼接，超规模则整块替换，否则走 LCS。
//
// 说明：调用方已裁掉公共前后缀，因此这里只需处理"中间必然存在差异"的情况；
// 整块替换时前后缀仍由调用方原样保留，不会丢失上下文。
func midOps(a, b []string) []opLine {
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0:
		ops := make([]opLine, 0, len(b))
		for _, s := range b {
			ops = append(ops, opLine{kind: opInsert, line: s})
		}
		return ops
	case len(b) == 0:
		ops := make([]opLine, 0, len(a))
		for _, s := range a {
			ops = append(ops, opLine{kind: opDelete, line: s})
		}
		return ops
	case len(a)*len(b) > maxEditCells:
		// 差异面积极大：整块替换（a 中段全删 + b 中段全插），不保留内部公共行。
		// 语义完全正确——hunk 生成时会自动为变更点补上前后各 context 行上下文，
		// 所以这里不需要（也不应该）自己保留公共行；同时只按 |a|+|b| 追加操作，
		// 内存与耗时都是线性的，彻底规避 O(n*m) 的 DP 爆炸。
		ops := make([]opLine, 0, len(a)+len(b))
		for _, s := range a {
			ops = append(ops, opLine{kind: opDelete, line: s})
		}
		for _, s := range b {
			ops = append(ops, opLine{kind: opInsert, line: s})
		}
		return ops
	default:
		return lcsOps(a, b)
	}
}

// lcsOps 经典 LCS 动态规划 + 回溯。
func lcsOps(a, b []string) []opLine {
	n, m := len(a), len(b)
	table := make([]int32, (n+1)*(m+1))
	width := m + 1
	for i := n - 1; i >= 0; i-- {
		row := i * width
		next := (i + 1) * width
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[row+j] = table[next+j+1] + 1
				continue
			}
			if table[next+j] >= table[row+j+1] {
				table[row+j] = table[next+j]
			} else {
				table[row+j] = table[row+j+1]
			}
		}
	}
	ops := make([]opLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, opLine{kind: opEqual, line: a[i]})
			i++
			j++
		case table[(i+1)*width+j] >= table[i*width+j+1]:
			ops = append(ops, opLine{kind: opDelete, line: a[i]})
			i++
		default:
			ops = append(ops, opLine{kind: opInsert, line: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, opLine{kind: opDelete, line: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, opLine{kind: opInsert, line: b[j]})
	}
	return ops
}

// buildHunks 把操作序列切成若干 hunk。
//
// 行号语义：遍历时 oldLine/newLine 是"下一个待消费的 1-based 行号"，因此每个 op
// 在旧/新文件中占据的行号都是确定的（opOldLine / opNewLine）。切块时记录窗口首尾
// 两个 op 的行号即可得到 @@ -oldStart,oldCount +newStart,newCount @@，多 hunk 场景
// 下各自的行号天然正确（这正是 git apply 偏移校正所依赖的）。
//
// 拆块规则：两个变更点之间累计的公共行数达到 2*context 时收尾当前块；窗口尾部的
// 公共行在 finalizeHunk 中裁剪到 context 行以内。
func buildHunks(ops []opLine, oldLines, newLines []string, context int, oldTrailing, newTrailing bool) []opHunk {
	var (
		hunks    []opHunk
		cur      []hunkSlot
		oldLine  = 1
		newLine  = 1
		pending  int
		lastKind opKind
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		hunks = append(hunks, finalizeHunk(cur, context, oldTrailing, newTrailing))
		cur = nil
		pending = 0
	}

	for _, ol := range ops {
		hasOld := ol.kind == opEqual || ol.kind == opDelete
		hasNew := ol.kind == opEqual || ol.kind == opInsert
		s := hunkSlot{op: ol, oldLine: oldLine, newLine: newLine, hasOld: hasOld, hasNew: hasNew}

		if ol.kind == opEqual {
			if lastKind != opEqual {
				pending = 0
			}
			if len(cur) > 0 {
				if pending >= 2*context {
					// 距离下一个变更点足够远：当前块就此收尾。
					flush()
				} else {
					// pending 落在 [context, 2*context) 区间时，该行既可能是上一个块的
					// 尾部上下文，也可能是下一个块的首部上下文，先保留，flush 时再裁剪。
					cur = append(cur, s)
					pending++
				}
			} else if ol.noNewline && len(hunks) == 0 {
				// 边界场景：全篇内容逐行相同，差异只在末尾换行符。此时没有任何
				// 增删行，必须靠"该行缺少末尾换行"本身构成一个 hunk，否则 diff 为空、
				// 变更被静默丢弃。仅当此前尚无任何 hunk 时才这样兜底。
				cur = append(cur, s)
			}
			lastKind = opEqual
			oldLine++
			newLine++
			continue
		}

		if len(cur) == 0 {
			// 变更点之前补首部上下文（上限为该变更点之前的可用行数）。
			startOld := oldLine - context
			if startOld < 1 {
				startOld = 1
			}
			for i := startOld; i < oldLine; i++ {
				cur = append(cur, hunkSlot{
					op:      opLine{kind: opEqual, line: oldLines[i-1]},
					oldLine: i,
					newLine: newLine - (oldLine - i),
					hasOld:  true,
					hasNew:  true,
				})
			}
		}
		cur = append(cur, s)
		pending = 0
		lastKind = ol.kind
		if hasOld {
			oldLine++
		}
		if hasNew {
			newLine++
		}
	}
	flush()
	return hunks
}

// hunkSlot 是 hunk 窗口内的一个操作及其在旧/新文件中的确定行号。
type hunkSlot struct {
	op      opLine
	oldLine int // 在旧文件中的 1-based 行号；旧侧不存在时为 0
	newLine int // 在新文件中的 1-based 行号；新侧不存在时为 0
	hasOld  bool
	hasNew  bool
}

// finalizeHunk 裁掉尾部多余上下文并计算 hunk 的起止行号。
func finalizeHunk(cur []hunkSlot, context int, oldTrailing, newTrailing bool) opHunk {
	// 1. 裁尾：最后一个变更点之后最多保留 context 行上下文。
	lastChange := len(cur) - 1
	for lastChange >= 0 && cur[lastChange].op.kind == opEqual {
		lastChange--
	}
	if lastChange >= 0 {
		if cut := lastChange + 1 + context; cut < len(cur) {
			cur = cur[:cut]
		}
	}
	if len(cur) == 0 {
		return opHunk{}
	}

	// 行号取窗口内首个"存在行"的确定行号，计数为该侧实际出现的行数。
	oldStart, newStart := 0, 0
	oldCount, newCount := 0, 0
	ops := make([]opLine, 0, len(cur))
	// trailingOnly 表示窗口内没有任何增删行；此时候选行只可能是被标记了
	// "缺少末尾换行"的上下文行。
	trailingOnly := true
	markedIdx := -1
	for i, s := range cur {
		if s.hasOld {
			if oldStart == 0 {
				oldStart = s.oldLine
			}
			oldCount++
		}
		if s.hasNew {
			if newStart == 0 {
				newStart = s.newLine
			}
			newCount++
		}
		if s.op.kind != opEqual {
			trailingOnly = false
		}
		if s.op.noNewline {
			markedIdx = i
		}
		ops = append(ops, s.op)
	}
	lastChangeIdx := markedIdx
	if !trailingOnly {
		lastChangeIdx = -1
	}

	// 边界场景：窗口内没有任何增删行，差异只在末尾换行符。git 要求 hunk 至少包含
	// 一个变更行，纯上下文行会被判为 "corrupt patch"，因此把被标记的那一行拆成
	// 一删一增（内容相同、换行状态不同），这是唯一既合法又能表达该变更的形式。
	// 计数按实际渲染出的行重算，避免 @@ 头与正文行数不一致。
	if lastChangeIdx >= 0 {
		target := ops[lastChangeIdx]
		del, ins := target, target
		switch {
		case !oldTrailing && newTrailing:
			// 旧侧缺换行 → 新侧有换行：标记落在被删除的旧行上。
			del.noNewline, ins.noNewline = true, false
		case oldTrailing && !newTrailing:
			// 旧侧有换行 → 新侧缺换行：标记落在新增行上。
			del.noNewline, ins.noNewline = false, true
		default:
			del.noNewline, ins.noNewline = false, false
		}
		del.kind, ins.kind = opDelete, opInsert
		ops = append(ops[:lastChangeIdx], append([]opLine{del, ins}, ops[lastChangeIdx+1:]...)...)

		oldStart, newStart = 0, 0
		oldCount, newCount = 0, 0
		for _, s := range cur {
			oldStart = s.oldLine
			newStart = s.newLine
			break
		}
		for _, o := range ops {
			if o.kind == opEqual || o.kind == opDelete {
				oldCount++
			}
			if o.kind == opEqual || o.kind == opInsert {
				newCount++
			}
		}
	}
	// 保底：空窗口（理论不可达）直接返回空 hunk，由上层跳过。
	if len(ops) == 0 {
		return opHunk{}
	}
	// 末行换行标记已在 UnifiedDiff 中按行挂好（opLine.noNewline），此处无需推断：
	// 即使该行因合并/裁剪落入别的 hunk，标记也始终跟随正确的行。
	return opHunk{
		ops:      ops,
		oldStart: oldStart,
		newStart: newStart,
		oldCount: oldCount,
		newCount: newCount,
	}
}

// ---------------------------------------------------------------------------
// 文本工具
// ---------------------------------------------------------------------------

// splitKeepEnds 按 '\n' 拆行，返回行列表与"是否以换行结束"。
// 仅按 '\n' 切分（保留 '\r'）以保证逐字节严格匹配，兼容 CRLF 文件。
func splitKeepEnds(s string) ([]string, bool) {
	if s == "" {
		return nil, true
	}
	lines := strings.Split(s, "\n")
	if last := len(lines) - 1; lines[last] == "" {
		return lines[:last], true
	}
	return lines, false
}

// joinWithTrailing 还原文本，trailing 表示末尾是否补换行。
func joinWithTrailing(lines []string, trailing bool) string {
	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, "\n")
	if trailing {
		out += "\n"
	}
	return out
}

// preview 截取前 n 个字符（按 rune 计数，避免截断多字节字符）。
func preview(s string, n int) string {
	return truncateRunes(s, n)
}

// truncateRunes 按 rune 截断字符串，超长时追加省略号。
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
