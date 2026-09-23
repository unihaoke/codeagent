package sandbox

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// roundTrip 生成 diff 后立刻应用回去，校验能精确还原目标文本。
func roundTrip(t *testing.T, name, oldText, newText string) string {
	t.Helper()
	diff := UnifiedDiff(oldText, newText, "a.txt", "a.txt", 3)
	if oldText == newText {
		if diff != "" {
			t.Fatalf("%s: 内容相同时应返回空 diff，实际:\n%s", name, diff)
		}
		return diff
	}
	if strings.TrimSpace(diff) == "" {
		t.Fatalf("%s: 内容有变化但 diff 为空", name)
	}
	got, err := ApplyUnifiedDiff(oldText, diff)
	if err != nil {
		t.Fatalf("%s: ApplyUnifiedDiff 失败: %v\ndiff:\n%s", name, err, diff)
	}
	if got != newText {
		t.Fatalf("%s: 还原结果不符\nwant: %q\ngot:  %q\ndiff:\n%s", name, newText, got, diff)
	}
	return diff
}

// kindSeq 返回 hunk 内所有行的 Kind 序列。
func kindSeq(h DiffHunk) string {
	var b strings.Builder
	for _, l := range h.Lines {
		b.WriteByte(l.Kind)
	}
	return b.String()
}

// assertHeader 校验 diff 头部严格符合 git 格式。
func assertHeader(t *testing.T, name, diff, path string, isNew, isDelete bool) {
	t.Helper()
	lines := strings.Split(diff, "\n")
	if len(lines) < 4 {
		t.Fatalf("%s: diff 行数不足:\n%s", name, diff)
	}
	wantGit := fmt.Sprintf("diff --git a/%s b/%s", path, path)
	if lines[0] != wantGit {
		t.Fatalf("%s: 首行不符\nwant: %s\ngot:  %s", name, wantGit, lines[0])
	}
	idx := 1
	if isNew {
		if lines[idx] != "new file mode 100644" {
			t.Fatalf("%s: 新增文件缺少 new file mode 行，实际 %q", name, lines[idx])
		}
		idx++
	}
	if isDelete {
		if lines[idx] != "deleted file mode 100644" {
			t.Fatalf("%s: 删除文件缺少 deleted file mode 行，实际 %q", name, lines[idx])
		}
		idx++
	}
	wantOld := "--- a/" + path
	if isNew {
		wantOld = "--- /dev/null"
	}
	if lines[idx] != wantOld {
		t.Fatalf("%s: --- 行不符\nwant: %s\ngot:  %s", name, wantOld, lines[idx])
	}
	wantNew := "+++ b/" + path
	if isDelete {
		wantNew = "+++ /dev/null"
	}
	if lines[idx+1] != wantNew {
		t.Fatalf("%s: +++ 行不符\nwant: %s\ngot:  %s", name, wantNew, lines[idx+1])
	}
	// 每个 hunk 头必须是 @@ -x,y +a,b @@ 形式
	for _, l := range lines[idx+2:] {
		if !strings.HasPrefix(l, "@@ ") {
			continue
		}
		if !strings.HasSuffix(l, " @@") && !strings.Contains(l, " @@") {
			t.Fatalf("%s: hunk 头格式非法: %q", name, l)
		}
		if !strings.HasPrefix(l, "@@ -") || !strings.Contains(l, " +") {
			t.Fatalf("%s: hunk 头格式非法: %q", name, l)
		}
	}
}

// testRepoCtx 用于 patch_test.go 中的 BuildPatch 调用：补丁必须携带 RepoKey/
// RepositoryID，Manager.Validate 才能定位到工作区内的目标仓库（引擎侧同样如此）。
// testRepo 定义在 manager_test.go，同包内共享。
func testRepoCtx() *domain.Repository {
	return testRepo("repo-1", "svc-a")
}

// ---------------------------------------------------------------------------
// 需求 1：UnifiedDiff + ApplyUnifiedDiff 八类场景
// ---------------------------------------------------------------------------

// TestUnifiedDiffRoundTrip 覆盖需求列出的 8 类场景，逐项校验能精确还原。
func TestUnifiedDiffRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		old, nw  string
		wantKind string // 期望第一个 hunk 的 Kind 序列（空表示不校验）
	}{
		{
			name:     "①单行修改",
			old:      "line1\nline2\nline3\nline4\nline5\n",
			nw:       "line1\nline2\nCHANGED\nline4\nline5\n",
			wantKind: "  -+  ",
		},
		{
			name:     "②多行插入",
			old:      "alpha\nbeta\ngamma\n",
			nw:       "alpha\nbeta\nnew-1\nnew-2\nnew-3\ngamma\n",
			wantKind: "  +++ ",
		},
		{
			name:     "③多行删除",
			old:      "alpha\nbeta\ngamma\ndelta\nepsilon\n",
			nw:       "alpha\nepsilon\n",
			wantKind: " --- ",
		},
		{
			name:     "④a文件首部修改",
			old:      "first\nsecond\nthird\n",
			nw:       "FIRST\nsecond\nthird\n",
			wantKind: "-+  ",
		},
		{
			name:     "④b文件尾部修改",
			old:      "first\nsecond\nthird\n",
			nw:       "first\nsecond\nTHIRD\n",
			wantKind: "  -+",
		},
		{
			name: "⑤相距很远的两个 hunk（含行数变化，验证偏移校正）",
			old:  numbered(1, 30),
			nw:   farApartChange(),
		},
		{
			name:     "⑥中文内容",
			old:      "中文第一行\n中文第二行\n中文第三行\n中文第四行\n",
			nw:       "中文第一行\n中文第二行已修改\n中文第三行\n新增的中文行\n中文第四行\n",
			wantKind: " -+ + ",
		},
		{
			name:     "⑦无末尾换行（修改末行且仍无换行）",
			old:      "aaa\nbbb\nccc",
			nw:       "aaa\nbbb\nCCC",
			wantKind: "  -+",
		},
		{
			name:     "⑦b从有换行变为无末尾换行",
			old:      "aaa\nbbb\nccc\n",
			nw:       "aaa\nbbb\nccc",
			wantKind: "-+",
		},
		{
			name:     "⑦c从无末尾换行变为有换行",
			old:      "aaa\nbbb\nccc",
			nw:       "aaa\nbbb\nccc\n",
			wantKind: "-+",
		},
		{
			name: "⑧空→非空（新增）",
			old:  "",
			nw:   "brand\nnew\nfile\n",
		},
		{
			name: "⑧b非空→空（删除）",
			old:  "to\nbe\nremoved\n",
			nw:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := roundTrip(t, tc.name, tc.old, tc.nw)
			parsed, err := ParseDiff(diff)
			if err != nil {
				t.Fatalf("ParseDiff 失败: %v\ndiff:\n%s", err, diff)
			}
			if len(parsed) != 1 {
				t.Fatalf("应解析出 1 个文件段，实际 %d", len(parsed))
			}
			if len(parsed[0].Hunks) == 0 {
				t.Fatalf("应至少解析出 1 个 hunk")
			}
			if tc.wantKind != "" {
				if got := kindSeq(parsed[0].Hunks[0]); got != tc.wantKind {
					t.Fatalf("Kind 序列不符\nwant: %q\ngot:  %q\ndiff:\n%s", tc.wantKind, got, diff)
				}
			}
		})
	}
}

// TestUnifiedDiffHeaderFormat 校验生成的 diff 头部严格符合 git 格式。
func TestUnifiedDiffHeaderFormat(t *testing.T) {
	t.Run("修改", func(t *testing.T) {
		diff := UnifiedDiff("a\nb\nc\n", "a\nB\nc\n", "src/main.go", "src/main.go", 3)
		assertHeader(t, "修改", diff, "src/main.go", false, false)
	})
	t.Run("新增", func(t *testing.T) {
		diff := UnifiedDiff("", "x\ny\n", "pkg/new.go", "pkg/new.go", 3)
		assertHeader(t, "新增", diff, "pkg/new.go", true, false)
		for _, l := range strings.Split(diff, "\n") {
			if strings.HasPrefix(l, "@@ ") {
				if !strings.HasPrefix(l, "@@ -0,0 ") {
					t.Fatalf("新增文件的旧侧应为 -0,0，实际 %q", l)
				}
			}
		}
	})
	t.Run("删除", func(t *testing.T) {
		diff := UnifiedDiff("x\ny\n", "", "pkg/old.go", "pkg/old.go", 3)
		assertHeader(t, "删除", diff, "pkg/old.go", false, true)
	})
	t.Run("无末尾换行标记", func(t *testing.T) {
		diff := UnifiedDiff("a\nb\nc", "a\nb\nC", "f.txt", "f.txt", 3)
		if !strings.Contains(diff, noNewlineMarker) {
			t.Fatalf("缺少 %q 标记:\n%s", noNewlineMarker, diff)
		}
		// 标记必须紧跟在被标记的那一行之后
		lines := strings.Split(diff, "\n")
		for i, l := range lines {
			if strings.HasPrefix(l, `\`) {
				if i == 0 || len(lines[i-1]) == 0 {
					t.Fatalf("末尾换行标记位置非法（第 %d 行）:\n%s", i+1, diff)
				}
				switch lines[i-1][0] {
				case ' ', '-', '+':
				default:
					t.Fatalf("末尾换行标记前一行不是 diff 行: %q", lines[i-1])
				}
			}
		}
	})
}

// TestUnifiedDiffTwoHunks 验证相距很远的两个 hunk 会拆成两块，且行号正确。
func TestUnifiedDiffTwoHunks(t *testing.T) {
	oldText := numbered(1, 60)
	newText := numbered(1, 60)
	newText = strings.Replace(newText, "3\n", "THREE\n", 1)
	newText = strings.Replace(newText, "57\n", "FIFTY-SEVEN\n", 1)

	diff := roundTrip(t, "双 hunk", oldText, newText)
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("应拆出 2 个 hunk，实际 %d\ndiff:\n%s", len(files[0].Hunks), diff)
	}
	first, second := files[0].Hunks[0], files[0].Hunks[1]
	if first.OldStart != 1 {
		t.Fatalf("第一个 hunk 应从第 1 行开始，实际 %d", first.OldStart)
	}
	if second.OldStart != 54 {
		t.Fatalf("第二个 hunk 应从第 54 行开始（57-3），实际 %d\ndiff:\n%s", second.OldStart, diff)
	}
	if second.NewStart != 54 {
		t.Fatalf("第二个 hunk 新侧应从第 54 行开始，实际 %d", second.NewStart)
	}
	if first.OldLines != first.NewLines {
		t.Fatalf("等长替换时两侧行数应相等：old=%d new=%d", first.OldLines, first.NewLines)
	}
}

// TestApplyUnifiedDiffHunkOffset 专门验证"前一个 hunk 改变行数后，后续 hunk 需按偏移校正"。
func TestApplyUnifiedDiffHunkOffset(t *testing.T) {
	oldText := numbered(1, 40)
	// 头部插入 3 行（行数 +3）→ 后面的 hunk 必须整体下移 3 行才能定位
	newText := "x1\nx2\nx3\n" + numbered(1, 40)
	newText = strings.Replace(newText, "35\n", "CHANGED-35\n", 1)

	diff := roundTrip(t, "偏移校正", oldText, newText)
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("应拆出 2 个 hunk，实际 %d\ndiff:\n%s", len(files[0].Hunks), diff)
	}
	first, second := files[0].Hunks[0], files[0].Hunks[1]
	if got := first.NewLines - first.OldLines; got != 3 {
		t.Fatalf("第一个 hunk 应新增 3 行，实际 %d", got)
	}
	if second.OldStart != 32 || second.NewStart != 35 {
		t.Fatalf("第二个 hunk 行号应体现偏移：old=%d(期望32) new=%d(期望35)\ndiff:\n%s",
			second.OldStart, second.NewStart, diff)
	}
}

// TestApplyUnifiedDiffStrictContext 上下文不匹配必须失败，并给出失败 hunk 序号。
func TestApplyUnifiedDiffStrictContext(t *testing.T) {
	diff := UnifiedDiff("a\nb\nc\nd\ne\n", "a\nb\nC\nd\ne\n", "f.txt", "f.txt", 3)
	tampered := "a\nXXXX\nc\nd\ne\n"
	_, err := ApplyUnifiedDiff(tampered, diff)
	if err == nil {
		t.Fatal("上下文不匹配时应返回错误")
	}
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型应为 *ApplyError，实际 %T: %v", err, err)
	}
	if ae.Hunk != 1 {
		t.Fatalf("失败 hunk 序号应为 1，实际 %d", ae.Hunk)
	}
	if !strings.Contains(ae.Reason, "上下文不匹配") {
		t.Fatalf("失败原因应说明上下文不匹配，实际 %q", ae.Reason)
	}
	if !strings.Contains(err.Error(), "f.txt") {
		t.Fatalf("错误信息应包含文件路径，实际 %q", err.Error())
	}
}

// TestApplyUnifiedDiffSecondHunkFailure 第二个 hunk 定位失败时 Hunk 序号应为 2。
func TestApplyUnifiedDiffSecondHunkFailure(t *testing.T) {
	oldText := numbered(1, 60)
	newText := numbered(1, 60)
	newText = strings.Replace(newText, "3\n", "THREE\n", 1)
	newText = strings.Replace(newText, "57\n", "FIFTY-SEVEN\n", 1)
	diff := UnifiedDiff(oldText, newText, "f.txt", "f.txt", 3)

	// 只破坏第二个 hunk 附近的上下文
	broken := strings.Replace(oldText, "55\n56\n57\n", "55\n56\nXXX\n", 1)
	_, err := ApplyUnifiedDiff(broken, diff)
	if err == nil {
		t.Fatal("第二个 hunk 上下文被破坏时应失败")
	}
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型应为 *ApplyError，实际 %T: %v", err, err)
	}
	if ae.Hunk != 2 {
		t.Fatalf("失败 hunk 序号应为 2，实际 %d；错误: %v", ae.Hunk, err)
	}
}

// TestApplyUnifiedDiffEmpty 空 diff 返回原内容。
func TestApplyUnifiedDiffEmpty(t *testing.T) {
	content := "hello\nworld\n"
	for _, d := range []string{"", "   ", "\n"} {
		got, err := ApplyUnifiedDiff(content, d)
		if err != nil {
			t.Fatalf("空 diff 不应报错: %v", err)
		}
		if got != content {
			t.Fatalf("空 diff 应返回原内容，实际 %q", got)
		}
	}
}

// ---------------------------------------------------------------------------
// ParseDiff
// ---------------------------------------------------------------------------

// TestParseDiff 覆盖需求里的 8 类场景，校验 hunk 数与 Kind 序列。
func TestParseDiff(t *testing.T) {
	cases := []struct {
		name      string
		old, nw   string
		wantHunks int
		wantKinds []string
	}{
		{"单行修改", "l1\nl2\nl3\nl4\nl5\n", "l1\nl2\nX\nl4\nl5\n", 1, []string{"  -+  "}},
		{"多行插入", "a\nb\n", "a\nn1\nn2\nb\n", 1, []string{" ++ "}},
		{"多行删除", "a\nb\nc\nd\n", "a\nd\n", 1, []string{" -- "}},
		{"首部修改", "a\nb\nc\n", "A\nb\nc\n", 1, []string{"-+  "}},
		{"尾部修改", "a\nb\nc\n", "a\nb\nC\n", 1, []string{"  -+"}},
		{"中文修改", "甲\n乙\n丙\n", "甲\n乙改\n丙\n", 1, []string{" -+ "}}, {"空到非空", "", "x\ny\n", 1, []string{"++"}},
		{"非空到空", "x\ny\n", "", 1, []string{"--"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff := UnifiedDiff(tc.old, tc.nw, "f.txt", "f.txt", 3)
			files, err := ParseDiff(diff)
			if err != nil {
				t.Fatalf("ParseDiff 失败: %v\ndiff:\n%s", err, diff)
			}
			if len(files) != 1 {
				t.Fatalf("文件段数应为 1，实际 %d", len(files))
			}
			if len(files[0].Hunks) != tc.wantHunks {
				t.Fatalf("hunk 数应为 %d，实际 %d\ndiff:\n%s", tc.wantHunks, len(files[0].Hunks), diff)
			}
			for i, want := range tc.wantKinds {
				if got := kindSeq(files[0].Hunks[i]); got != want {
					t.Fatalf("第 %d 个 hunk 的 Kind 序列不符\nwant: %q\ngot:  %q", i+1, want, got)
				}
			}
		})
	}
}

// TestParseDiffMultiHunk 多 hunk 场景的解析。
func TestParseDiffMultiHunk(t *testing.T) {
	oldText := numbered(1, 60)
	newText := strings.Replace(numbered(1, 60), "3\n", "THREE\n", 1)
	newText = strings.Replace(newText, "57\n", "FIFTY-SEVEN\n", 1)
	diff := UnifiedDiff(oldText, newText, "f.txt", "f.txt", 3)

	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("hunk 数应为 2，实际 %d", len(files[0].Hunks))
	}
	for i, h := range files[0].Hunks {
		if !strings.HasPrefix(h.Header, "@@") {
			t.Fatalf("第 %d 个 hunk 缺少 Header", i+1)
		}
		if h.OldLines <= 0 || h.NewLines <= 0 {
			t.Fatalf("第 %d 个 hunk 行数非法: old=%d new=%d", i+1, h.OldLines, h.NewLines)
		}
	}
}

// TestParseDiffShape 校验解析出的行文本与路径。
func TestParseDiffShape(t *testing.T) {
	diff := UnifiedDiff("a\nb\nc\n", "a\nB\nc\n", "dir/file.go", "dir/file.go", 3)
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	f := files[0]
	if f.OldPath != "dir/file.go" || f.NewPath != "dir/file.go" {
		t.Fatalf("路径解析错误: old=%q new=%q", f.OldPath, f.NewPath)
	}
	if f.IsNew || f.IsDelete {
		t.Fatalf("修改类补丁不应标记 IsNew/IsDelete")
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 3 || h.NewStart != 1 || h.NewLines != 3 {
		t.Fatalf("hunk 行号解析错误: -%d,%d +%d,%d", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	}
	wantTexts := []string{"a", "b", "B", "c"}
	for i, want := range wantTexts {
		if h.Lines[i].Text != want {
			t.Fatalf("第 %d 行文本不符: want %q got %q", i, want, h.Lines[i].Text)
		}
	}
}

// TestParseDiffNewFile 新增文件段的解析：IsNew、/dev/null、无末尾换行标记。
func TestParseDiffNewFile(t *testing.T) {
	diff := UnifiedDiff("", "hello\nworld", "pkg/new.go", "pkg/new.go", 3)
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v\ndiff:\n%s", err, diff)
	}
	f := files[0]
	if !f.IsNew {
		t.Fatalf("应标记 IsNew=true\ndiff:\n%s", diff)
	}
	if f.OldPath != "/dev/null" {
		t.Fatalf("OldPath 应为 /dev/null，实际 %q", f.OldPath)
	}
	if f.NewPath != "pkg/new.go" {
		t.Fatalf("NewPath 应为 pkg/new.go，实际 %q", f.NewPath)
	}
	if !f.Hunks[0].NoNewlineNew {
		t.Fatalf("应识别出 NewNoNewline\ndiff:\n%s", diff)
	}
}

// TestParseDiffErrors 非法输入应报错而不是静默通过。
func TestParseDiffErrors(t *testing.T) {
	if _, err := ParseDiff(""); err != nil {
		t.Fatalf("空输入应返回空结果而不是错误: %v", err)
	}
	if files, err := ParseDiff("   \n"); err != nil || files != nil {
		t.Fatalf("空白输入应返回 nil, nil；实际 files=%v err=%v", files, err)
	}
	if _, err := ParseDiff("this is not a diff at all\n"); err == nil {
		t.Fatal("非法输入应返回错误")
	}
	if _, err := ParseDiff("@@ not-a-valid-hunk @@\n"); err == nil {
		t.Fatal("非法 hunk 头应返回错误")
	}
}

// ---------------------------------------------------------------------------
// BuildPatch / BuildCreatePatch
// ---------------------------------------------------------------------------

const buildPatchSource = `package demo

func Add(a, b int) int {
	return a + b
}

func Sub(a, b int) int {
	return a - b
}
`

// TestBuildPatchSnippetNotFound oldSnippet 不是子串时返回 ErrSnippetNotFound。
func TestBuildPatchSnippetNotFound(t *testing.T) {
	repo := &domain.Repository{ID: "repo-1", Key: "order-service"}
	_, err := BuildPatch(repo, "internal/demo/math.go", buildPatchSource,
		"func Missing() {", "func Missing() {}", "修复缺失函数", domain.RiskLow)
	if err == nil {
		t.Fatal("片段不存在时应返回错误")
	}
	if !errors.Is(err, ErrSnippetNotFound) {
		t.Fatalf("应为 ErrSnippetNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "func Missing() {") {
		t.Fatalf("错误信息应包含前 80 字符片段，实际 %q", err.Error())
	}
}

// TestBuildPatchNormal 正常场景：diff 非空、TouchedLines 非空、Action=modify。
func TestBuildPatchNormal(t *testing.T) {
	repo := &domain.Repository{ID: "repo-1", Key: "order-service"}
	oldSnippet := "\treturn a + b\n"
	newSnippet := "\tif a == 0 {\n\t\treturn b\n\t}\n\treturn a + b\n"

	p, err := BuildPatch(repo, "internal/demo/math.go", buildPatchSource, oldSnippet, newSnippet,
		"修复零值加法导致的空指针", domain.RiskMedium)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}
	if strings.TrimSpace(p.UnifiedDiff) == "" {
		t.Fatal("UnifiedDiff 不应为空")
	}
	if p.Action != domain.ActionModify {
		t.Fatalf("Action 应为 modify，实际 %q", p.Action)
	}
	if len(p.TouchedLines) == 0 {
		t.Fatal("TouchedLines 不应为空")
	}
	if p.ID == "" {
		t.Fatal("ID 不应为空")
	}
	if p.RepositoryID != "repo-1" || p.RepoKey != "order-service" {
		t.Fatalf("仓库信息未回填: %q / %q", p.RepositoryID, p.RepoKey)
	}
	if p.Risk != domain.RiskMedium {
		t.Fatalf("Risk 应为 medium，实际 %q", p.Risk)
	}
	if p.Status != domain.PatchProposed {
		t.Fatalf("Status 应为 proposed，实际 %q", p.Status)
	}
	if !strings.Contains(p.Rationale, "修复零值加法导致的空指针") {
		t.Fatalf("Rationale 应包含入参说明，实际 %q", p.Rationale)
	}
	if p.OldContent != buildPatchSource {
		t.Fatal("OldContent 应回填原始内容")
	}
	if p.NewContent == "" || p.NewContent == buildPatchSource {
		t.Fatal("NewContent 应回填修改后的内容")
	}
	// TouchedLines 必须落在新内容范围内
	total := countLines(p.NewContent)
	for _, lr := range p.TouchedLines {
		if lr.Start < 1 || lr.End < lr.Start || lr.End > total {
			t.Fatalf("TouchedLines 越界: %+v（新内容共 %d 行）", lr, total)
		}
	}
	// 生成的 diff 必须能应用到原文
	applied, err := ApplyUnifiedDiff(buildPatchSource, p.UnifiedDiff)
	if err != nil {
		t.Fatalf("生成的补丁无法应用: %v", err)
	}
	if applied != p.NewContent {
		t.Fatalf("补丁应用结果与 NewContent 不一致")
	}
}

// TestBuildPatchRepeatedSnippet 重复片段：用第一次出现并追加说明。
func TestBuildPatchRepeatedSnippet(t *testing.T) {
	repo := &domain.Repository{ID: "r", Key: "k"}
	src := "func A() {\n\treturn nil\n}\n\nfunc B() {\n\treturn nil\n}\n"
	oldSnippet := "\treturn nil\n"
	newSnippet := "\treturn nil // 已确认\n"

	p, err := BuildPatch(repo, "a.go", src, oldSnippet, newSnippet, "补充注释", domain.RiskLow)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}
	if !strings.Contains(p.Rationale, "出现 2 次") {
		t.Fatalf("Rationale 应提示片段出现 2 次，实际 %q", p.Rationale)
	}
	if !strings.Contains(p.Rationale, "第 1 处") {
		t.Fatalf("Rationale 应说明定位到第 1 处，实际 %q", p.Rationale)
	}
	// 只应修改第一次出现（第 2 行）
	if !strings.Contains(p.NewContent, "func A() {\n\treturn nil // 已确认\n}") {
		t.Fatalf("应替换第一次出现:\n%s", p.NewContent)
	}
	if strings.Count(p.NewContent, "已确认") != 1 {
		t.Fatalf("只应替换一处，实际替换了 %d 处", strings.Count(p.NewContent, "已确认"))
	}
}

// TestBuildPatchErrors 边界输入。
func TestBuildPatchErrors(t *testing.T) {
	repo := &domain.Repository{ID: "r", Key: "k"}
	if _, err := BuildPatch(repo, "a.go", "x\n", "", "y\n", "r", domain.RiskLow); err == nil {
		t.Fatal("oldSnippet 为空应报错")
	}
	if _, err := BuildPatch(repo, "a.go", "x\n", "x\n", "x\n", "r", domain.RiskLow); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("内容未变化应返回 ErrNoChanges，实际 %v", err)
	}
	if _, err := BuildPatch(repo, "  ", "x\n", "x\n", "y\n", "r", domain.RiskLow); err == nil {
		t.Fatal("文件路径为空应报错")
	}
	if _, err := BuildPatch(repo, "../evil.go", "x\n", "x\n", "y\n", "r", domain.RiskLow); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("路径穿越应返回 ErrUnsafePath，实际 %v", err)
	}
}

// TestBuildCreatePatch 新增文件补丁。
func TestBuildCreatePatch(t *testing.T) {
	repo := &domain.Repository{ID: "repo-2", Key: "gateway"}
	content := "package gateway\n\n// New 创建网关\nfunc New() {}\n"
	p, err := BuildCreatePatch(repo, "internal/gateway/new.go", content, "补齐缺失的构造函数", domain.RiskLow)
	if err != nil {
		t.Fatalf("BuildCreatePatch 失败: %v", err)
	}
	if p.Action != domain.ActionCreate {
		t.Fatalf("Action 应为 create，实际 %q", p.Action)
	}
	if p.OldContent != "" {
		t.Fatalf("新增文件的 OldContent 应为空，实际 %q", p.OldContent)
	}
	if p.NewContent != content {
		t.Fatal("NewContent 应等于传入内容")
	}
	if len(p.TouchedLines) == 0 {
		t.Fatal("新增文件也应填充 TouchedLines")
	}
	if p.TouchedLines[0].Start != 1 {
		t.Fatalf("新增文件的行区间应从第 1 行开始，实际 %+v", p.TouchedLines[0])
	}
	assertHeader(t, "新增补丁", p.UnifiedDiff, "internal/gateway/new.go", true, false)
	if _, err := ApplyUnifiedDiff("", p.UnifiedDiff); err != nil {
		t.Fatalf("新增补丁应能应用到空内容: %v", err)
	}
	if _, err := BuildCreatePatch(repo, "a.go", "", "空内容", domain.RiskLow); err == nil {
		t.Fatal("新增空文件应报错")
	}
	if _, err := BuildCreatePatch(repo, "../x.go", "x\n", "越界", domain.RiskLow); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("路径穿越应返回 ErrUnsafePath，实际 %v", err)
	}
}

// TestBuildPatchTouchedLinesSingleRange 只在文件末尾追加时，行区间必须准确。
func TestBuildPatchTouchedLinesSingleRange(t *testing.T) {
	repo := &domain.Repository{ID: "r", Key: "k"}
	src := "l1\nl2\nl3\nl4\nl5\n"
	p, err := BuildPatch(repo, "f.txt", src, "l5\n", "l5\nl6\n", "追加一行", domain.RiskLow)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}
	if len(p.TouchedLines) != 1 {
		t.Fatalf("应聚合为 1 个区间，实际 %+v", p.TouchedLines)
	}
	if p.TouchedLines[0].Start != 6 || p.TouchedLines[0].End != 6 {
		t.Fatalf("变更行区间应为 [6,6]，实际 %+v", p.TouchedLines[0])
	}
}

// ---------------------------------------------------------------------------
// ApplyPatch（内存文件集）
// ---------------------------------------------------------------------------

// TestApplyPatchBasic 正常应用补丁到内存文件集。
func TestApplyPatchBasic(t *testing.T) {
	repo := &domain.Repository{ID: "r", Key: "k"}
	src := "package demo\n\nfunc A() int { return 1 }\n"
	p, err := BuildPatch(repo, "demo.go", src, "return 1", "return 2", "修正返回值", domain.RiskLow)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}
	files := map[string]string{"demo.go": src}
	next, err := ApplyPatch(files, *p)
	if err != nil {
		t.Fatalf("ApplyPatch 失败: %v", err)
	}
	if !strings.Contains(next["demo.go"], "return 2") {
		t.Fatalf("补丁未生效: %q", next["demo.go"])
	}
	if files["demo.go"] != src {
		t.Fatal("ApplyPatch 不得修改入参文件集")
	}
}

// TestApplyPatchPathTraversal 路径穿越必须被拒绝。
func TestApplyPatchPathTraversal(t *testing.T) {
	malicious := []string{
		"../../etc/passwd",
		"..\\..\\windows\\system32\\drivers\\etc\\hosts",
		"/etc/passwd",
		"C:\\Windows\\System32\\config",
		"~/secret",
		"a/../../b",
	}
	for _, path := range malicious {
		t.Run(path, func(t *testing.T) {
			p := domain.Patch{
				FilePath:    path,
				Action:      domain.ActionModify,
				UnifiedDiff: "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n",
				OldContent:  "a\n",
				NewContent:  "b\n",
			}
			_, err := ApplyPatch(map[string]string{}, p)
			if err == nil {
				t.Fatalf("路径穿越 %q 应被拒绝", path)
			}
			// 契约要求两种判定同时成立：errors.As 归类为 *ApplyError，
			// errors.Is 判定为 ErrUnsafePath（错误链不能被包装截断）。
			var ae *ApplyError
			if !errors.As(err, &ae) {
				t.Fatalf("应返回 *ApplyError，实际 %T", err)
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("错误链应保留 ErrUnsafePath，实际 %v", err)
			}
			if ae.FilePath != path {
				t.Fatalf("ApplyError.FilePath 应为 %q，实际 %q", path, ae.FilePath)
			}
		})
	}
	// 直接校验防护函数
	if _, err := CheckSafeRelPath("../../etc/passwd"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("CheckSafeRelPath 应拒绝路径穿越，实际 %v", err)
	}
	if _, err := CheckSafeRelPath("C:/Windows/x"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("CheckSafeRelPath 应拒绝盘符路径，实际 %v", err)
	}
	if got, err := CheckSafeRelPath("./src/../src/main.go"); err != nil || got != "src/main.go" {
		t.Fatalf("合法路径应被归一化，实际 %q err=%v", got, err)
	}
	if !ContainsTraversal("a/../../b") || ContainsTraversal("a/b/c.go") {
		t.Fatal("ContainsTraversal 判定错误")
	}
}

// TestApplyPatchMissingFile 目标文件不存在时报错。
func TestApplyPatchMissingFile(t *testing.T) {
	p := domain.Patch{
		FilePath:    "missing.go",
		Action:      domain.ActionModify,
		UnifiedDiff: "--- a/missing.go\n+++ b/missing.go\n@@ -1 +1 @@\n-a\n+b\n",
	}
	_, err := ApplyPatch(map[string]string{"other.go": "x\n"}, p)
	if err == nil {
		t.Fatal("文件不存在应报错")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("应为 ErrFileNotFound，实际 %v", err)
	}
}

// TestApplyPatchesAtomic 批量应用失败时应整体回滚（不改动入参）。
func TestApplyPatchesAtomic(t *testing.T) {
	repo := &domain.Repository{ID: "r", Key: "k"}
	good, err := BuildPatch(repo, "a.go", "package a\n\nvar X = 1\n", "var X = 1", "var X = 2", "ok", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	bad := domain.Patch{
		FilePath:    "b.go",
		Action:      domain.ActionModify,
		UnifiedDiff: "--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-nope\n+yes\n",
	}
	files := map[string]string{"a.go": "package a\n\nvar X = 1\n", "b.go": "package b\n"}
	next, err := ApplyPatches(files, []domain.Patch{*good, bad})
	if err == nil {
		t.Fatal("含失败补丁时应整体返回错误")
	}
	if next != nil {
		t.Fatal("失败时不应返回部分结果")
	}
	if files["a.go"] != "package a\n\nvar X = 1\n" {
		t.Fatal("入参文件集被污染")
	}
	if _, err := ApplyPatches(files, nil); !errors.Is(err, ErrNoPatches) {
		t.Fatalf("空补丁列表应返回 ErrNoPatches，实际 %v", err)
	}
}

// TestApplyPatchDeleteAction 删除动作。
func TestApplyPatchDeleteAction(t *testing.T) {
	p := domain.Patch{
		FilePath:    "gone.go",
		Action:      domain.ActionDelete,
		UnifiedDiff: UnifiedDiff("x\ny\n", "", "gone.go", "gone.go", 3),
	}
	next, err := ApplyPatch(map[string]string{"gone.go": "x\ny\n", "keep.go": "z\n"}, p)
	if err != nil {
		t.Fatalf("ApplyPatch 失败: %v", err)
	}
	if _, ok := next["gone.go"]; ok {
		t.Fatal("删除动作后文件应消失")
	}
	if next["keep.go"] != "z\n" {
		t.Fatal("其它文件应保持不变")
	}
}

// ---------------------------------------------------------------------------
// 多 hunk 应用与偏移校正（构造两个 hunk 且第一个改变行数）
// ---------------------------------------------------------------------------

// TestApplyUnifiedDiffMultiHunkWithGrowth 第一个 hunk 增加行数，第二个 hunk 必须按偏移定位。
func TestApplyUnifiedDiffMultiHunkWithGrowth(t *testing.T) {
	base := numbered(1, 50)
	grown := strings.Replace(base, "2\n", "2\ninsert-a\ninsert-b\ninsert-c\ninsert-d\n", 1)
	grown = strings.Replace(grown, "45\n", "FORTY-FIVE\n", 1)

	diff := UnifiedDiff(base, grown, "big.txt", "big.txt", 3)
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("应拆出 2 个 hunk，实际 %d\ndiff:\n%s", len(files[0].Hunks), diff)
	}
	got, err := ApplyUnifiedDiff(base, diff)
	if err != nil {
		t.Fatalf("ApplyUnifiedDiff 失败（偏移校正异常）: %v\ndiff:\n%s", err, diff)
	}
	if got != grown {
		t.Fatalf("多 hunk 偏移校正结果不符\nwant:\n%s\ngot:\n%s", grown, got)
	}
}

// TestApplyUnifiedDiffCRLF CRLF 文件不得被静默改写换行符。
//
// 关键点：ParseDiff 只剥离结构行（diff/---/+++/@@）的 '\r'，正文行原样保留，
// 因此 CRLF 文件的上下文行天然带 '\r'，比较与重建都不需要额外规范化。
func TestApplyUnifiedDiffCRLF(t *testing.T) {
	oldText := "a\r\nb\r\nc\r\n"
	newText := "a\r\nB\r\nc\r\n"
	diff := UnifiedDiff(oldText, newText, "f.txt", "f.txt", 3)
	got, err := ApplyUnifiedDiff(oldText, diff)
	if err != nil {
		t.Fatalf("CRLF 应用失败: %v\ndiff:\n%s", err, diff)
	}
	if got != newText {
		t.Fatalf("CRLF 内容被破坏\nwant %q\ngot  %q", newText, got)
	}
	// 上下文行必须保持 "\r" 结尾，才可能逐字节匹配 CRLF 文件
	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("ParseDiff 失败: %v", err)
	}
	for _, l := range files[0].Hunks[0].Lines {
		if l.Kind == ' ' && !strings.HasSuffix(l.Text, "\r") {
			t.Fatalf("CRLF 上下文行丢掉了 \\r：%q", l.Text)
		}
	}
	// 大范围 CRLF 改动（含多 hunk）也必须可往返
	bigOld := strings.Repeat("x\r\ny\r\nz\r\n", 40)
	bigNew := strings.Replace(bigOld, "y\r\n", "Y\r\n", 1)
	bigNew = strings.Replace(bigNew, "z\r\n", "Z\r\n", 1)
	bigDiff := UnifiedDiff(bigOld, bigNew, "big.txt", "big.txt", 3)
	bigGot, err := ApplyUnifiedDiff(bigOld, bigDiff)
	if err != nil {
		t.Fatalf("CRLF 多 hunk 应用失败: %v", err)
	}
	if bigGot != bigNew {
		t.Fatal("CRLF 多 hunk 还原结果不符")
	}
}

// ---------------------------------------------------------------------------
// 性能
// ---------------------------------------------------------------------------

// TestUnifiedDiffPerformance 12000 行文本中部修改必须远快于 500ms
// （依赖公共前后缀裁剪把 DP 规模压到 1×1）。
func TestUnifiedDiffPerformance(t *testing.T) {
	const lines = 12000
	oldText := numbered(1, lines)
	newText := strings.Replace(oldText, "6000\n", "SIX-THOUSAND\n", 1)
	if oldText == newText {
		t.Fatal("基准数据构造失败")
	}

	start := time.Now()
	diff := UnifiedDiff(oldText, newText, "big.txt", "big.txt", 3)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("UnifiedDiff 耗时 %v，超过 500ms 阈值", elapsed)
	}
	t.Logf("12000 行单点修改：UnifiedDiff 耗时 %v，diff 大小 %d 字节", elapsed, len(diff))

	start = time.Now()
	got, err := ApplyUnifiedDiff(oldText, diff)
	applyElapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ApplyUnifiedDiff 失败: %v", err)
	}
	if got != newText {
		t.Fatal("大文件还原结果不符")
	}
	if applyElapsed > 500*time.Millisecond {
		t.Fatalf("ApplyUnifiedDiff 耗时 %v，超过 500ms 阈值", applyElapsed)
	}
	t.Logf("12000 行单点修改：ApplyUnifiedDiff 耗时 %v", applyElapsed)
}

// TestUnifiedDiffLargeScaleGuard 差异面积极大时应退化为整块替换而不是 OOM/超时，
// 且退化路径产出的 hunk 头与正文行数必须合法（@@ -1,N +1,M @@；删除行数 == 旧侧行数，
// 新增行数 == 新侧行数）。
func TestUnifiedDiffLargeScaleGuard(t *testing.T) {
	const lines = 4000
	oldText := numbered(1, lines)
	newText := numbered(1_000_000, 1_000_000+lines) // 每行都不同
	oldLineCount, newLineCount := countLines(oldText), countLines(newText)
	start := time.Now()
	diff := UnifiedDiff(oldText, newText, "a.txt", "b.txt", 3)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("全量改写退化路径耗时过长: %v", elapsed)
	}

	files, err := ParseDiff(diff)
	if err != nil {
		t.Fatalf("退化路径生成了非法 diff: %v\ndiff 头部:\n%s", err, firstLines(diff, 8))
	}
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		t.Fatalf("退化路径应为单文件单 hunk，实际 files=%d hunks=%d", len(files), len(files[0].Hunks))
	}
	h := files[0].Hunks[0]
	if h.OldStart != 1 || h.NewStart != 1 {
		t.Fatalf("退化路径 hunk 应从第 1 行开始：old=%d new=%d", h.OldStart, h.NewStart)
	}
	if h.OldLines != oldLineCount {
		t.Fatalf("hunk 旧侧行数应为 %d，实际 %d", oldLineCount, h.OldLines)
	}
	if h.NewLines != newLineCount {
		t.Fatalf("hunk 新侧行数应为 %d，实际 %d", newLineCount, h.NewLines)
	}
	var del, ins int
	for _, l := range h.Lines {
		switch l.Kind {
		case '-':
			del++
		case '+':
			ins++
		}
	}
	if del != oldLineCount {
		t.Fatalf("删除行数应等于旧侧行数 %d，实际 %d", oldLineCount, del)
	}
	if ins != newLineCount {
		t.Fatalf("新增行数应等于新侧行数 %d，实际 %d", newLineCount, ins)
	}

	got, err := ApplyUnifiedDiff(oldText, diff)
	if err != nil {
		t.Fatalf("退化路径生成的补丁无法应用: %v", err)
	}
	if got != newText {
		t.Fatal("退化路径还原结果不符")
	}
	t.Logf("全量改写（旧 %d 行 → 新 %d 行）耗时 %v，hunk 头 @@ -%d,%d +%d,%d @@",
		oldLineCount, newLineCount, elapsed, h.OldStart, h.OldLines, h.NewStart, h.NewLines)
}

// firstLines 返回文本的前 n 行，用于失败时打印 diff 头部。
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// numbered 生成 from..to 的文本，每行形如 "12"，便于定位。
func numbered(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

// farApartChange 构造相距很远的两个变更（含插入，改变行数）。
func farApartChange() string {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		switch i {
		case 3:
			b.WriteString("THREE\ninserted-1\ninserted-2\n")
		case 27:
			b.WriteString("TWENTY-SEVEN\n")
		default:
			fmt.Fprintf(&b, "%d\n", i)
		}
	}
	return b.String()
}
