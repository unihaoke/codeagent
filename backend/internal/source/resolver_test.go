package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 测试用本地 git 仓库
// ---------------------------------------------------------------------------

type gitFixture struct {
	originDir   string
	featureSHA  string
	mainSHA     string
	bogusBranch string
}

// runGitCmd 在指定目录执行 git 命令（测试辅助）。
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s 失败: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFixtureFile 写入测试文件（自动创建父目录，固定 LF 行尾）。
func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
}

// setupGitFixture 创建一个含两个 commit、两个分支的本地仓库。
func setupGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境缺少 git，跳过 resolver 集成测试")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	// 兼容 git 2.27 等旧版本：不使用 `git init -b main`。
	runGitCmd(t, origin, "init")
	runGitCmd(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, origin, "config", "user.email", "test@example.com")
	runGitCmd(t, origin, "config", "user.name", "Test")
	runGitCmd(t, origin, "config", "core.autocrlf", "false")
	runGitCmd(t, origin, "config", "commit.gpgsign", "false")

	writeFixtureFile(t, origin, "go.mod", "module example.com/order\n\ngo 1.23\n")
	writeFixtureFile(t, origin, "service.go", strings.Join([]string{
		"package main",
		"",
		"import (",
		"\t\"context\"",
		"\t\"errors\"",
		")",
		"",
		"var ErrEmpty = errors.New(\"empty order\")",
		"",
		"// OrderService 订单服务。",
		"type OrderService struct {",
		"\trepo string",
		"}",
		"",
		"// CreateOrder 创建订单。",
		"func (s *OrderService) CreateOrder(ctx context.Context, id string) error {",
		"\tif id == \"\" {",
		"\t\treturn ErrEmpty",
		"\t}",
		"\treturn nil",
		"}",
	}, "\n")+"\n")
	writeFixtureFile(t, origin, "service_test.go", "package main\n\nfunc TestCreateOrder(t *testing.T) {}\n")
	writeFixtureFile(t, origin, "config.yaml", "server:\n  port: 8080\n")
	writeFixtureFile(t, origin, "node_modules/dep/index.js", "module.exports = {};\n")
	writeFixtureFile(t, origin, "dist/bundle.js", "console.log('built');\n")
	writeFixtureFile(t, origin, ".gitignore", "dist/\n*.log\n")
	writeFixtureFile(t, origin, "debug.log", "noise\n")
	runGitCmd(t, origin, "add", "-A")
	runGitCmd(t, origin, "commit", "-m", "init")

	runGitCmd(t, origin, "checkout", "-b", "feature")
	writeFixtureFile(t, origin, "feature.go", "package main\n\nfunc FeatureFlag() bool { return true }\n")
	runGitCmd(t, origin, "add", "-A")
	runGitCmd(t, origin, "commit", "-m", "feat: add feature flag")
	featureSHA := runGitCmd(t, origin, "rev-parse", "HEAD")

	runGitCmd(t, origin, "checkout", "main")
	mainSHA := runGitCmd(t, origin, "rev-parse", "HEAD")
	return &gitFixture{originDir: origin, featureSHA: featureSHA, mainSHA: mainSHA}
}

// newTestResolver 构造指向本地仓库的 Resolver。
func newTestResolver(t *testing.T, fixture *gitFixture) (*Resolver, *domain.Repository) {
	t.Helper()
	cfg := config.SourceConfig{
		CacheDir:         filepath.Join(t.TempDir(), "cache"),
		WorkspaceDir:     filepath.Join(t.TempDir(), "ws"),
		CacheCapacity:    64,
		CacheTTLSec:      300,
		FetchDepth:       0,
		GitBinary:        "git",
		CloneTimeoutSec:  120,
		RespectGitignore: true,
		IgnorePatterns:   []string{".git", "node_modules", "dist", "build", "target", "vendor"},
	}
	st := store.New()
	r := NewResolver(cfg, st, nil, logx.Nop())
	t.Cleanup(func() { _ = r.Close() })

	repo := &domain.Repository{
		ID: "repo-1", TenantID: "tenant-1", Name: "订单服务", Key: "order-service",
		URL: fixture.originDir, DefaultBranch: "main", Provider: domain.VCSGit,
		Layer: domain.LayerService, Language: "go", Status: domain.RepoActive,
		MatchRules: domain.RepoMatchRules{PackagePrefixes: []string{"example.com/order"}},
	}
	if err := st.CreateRepo(repo); err != nil {
		t.Fatalf("写入仓库索引失败: %v", err)
	}
	return r, repo
}

func refFor(repo *domain.Repository, ref string, budget domain.LoadBudget) domain.RepoRef {
	return domain.RepoRef{Repository: repo, Ref: ref, Budget: budget}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestResolverResolveLockCommit(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()
	ref := refFor(repo, "", domain.LoadBudget{})

	// 空 ref → 默认分支 HEAD
	commit, err := r.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if commit != fx.mainSHA {
		t.Fatalf("Resolve(空 ref) = %q，期望 main HEAD %q", commit, fx.mainSHA)
	}

	// 分支名
	featCommit, err := r.Resolve(ctx, refFor(repo, "feature", domain.LoadBudget{}))
	if err != nil {
		t.Fatalf("Resolve(feature) 失败: %v", err)
	}
	if featCommit != fx.featureSHA {
		t.Fatalf("Resolve(feature) = %q，期望 %q", featCommit, fx.featureSHA)
	}

	// 显式 commit：直接校验存在性后返回
	same, err := r.Resolve(ctx, refFor(repo, fx.featureSHA, domain.LoadBudget{}))
	if err != nil {
		t.Fatalf("Resolve(sha) 失败: %v", err)
	}
	if same != fx.featureSHA {
		t.Fatalf("Resolve(sha) = %q，期望 %q", same, fx.featureSHA)
	}

	// ref.Commit 优先于 ref.Ref
	pinned, err := r.Resolve(ctx, domain.RepoRef{
		Repository: repo, Ref: "feature", Commit: fx.mainSHA, Budget: domain.LoadBudget{},
	})
	if err != nil {
		t.Fatalf("Resolve(commit 优先) 失败: %v", err)
	}
	if pinned != fx.mainSHA {
		t.Fatalf("ref.Commit 应优先，得到 %q，期望 %q", pinned, fx.mainSHA)
	}

	// 不存在的 ref
	if _, err := r.Resolve(ctx, refFor(repo, "no-such-branch", domain.LoadBudget{})); err == nil {
		t.Fatalf("不存在的 ref 应返回错误")
	}

	// 元数据缓存生效：再次解析命中缓存（Hits 增加）
	before := r.Stats().Hits
	if _, err := r.Resolve(ctx, ref); err != nil {
		t.Fatalf("重复 Resolve 失败: %v", err)
	}
	if r.Stats().Hits <= before {
		t.Fatalf("重复 Resolve 应命中元数据缓存: before=%d after=%d", before, r.Stats().Hits)
	}
}

func TestResolverReadFileAndCache(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()
	ref := refFor(repo, "", domain.LoadBudget{MaxFileSize: 1 << 20})

	sl, err := r.ReadFile(ctx, ref, "service.go")
	if err != nil {
		t.Fatalf("ReadFile 失败: %v", err)
	}
	if !strings.Contains(sl.Content, "func (s *OrderService) CreateOrder") {
		t.Fatalf("文件内容不正确: %q", sl.Content)
	}
	if sl.Path != "service.go" || sl.Language != LangGo {
		t.Fatalf("切片元信息错误: path=%q lang=%q", sl.Path, sl.Language)
	}
	if sl.RepositoryID != repo.ID || sl.RepoKey != repo.Key || sl.Commit != fx.mainSHA {
		t.Fatalf("切片未绑定仓库/commit: %+v", sl)
	}
	if sl.StartLine != 1 || sl.EndLine != LineCount(sl.Content) {
		t.Fatalf("行号区间错误: %d..%d", sl.StartLine, sl.EndLine)
	}
	if sl.Chars != len(sl.Content) {
		t.Fatalf("Chars = %d，期望 %d", sl.Chars, len(sl.Content))
	}
	if sl.Redacted || sl.FromCache {
		t.Fatalf("首次读取不应标记 FromCache/Redacted: %+v", sl)
	}
	if !contains(sl.Symbols, "OrderService") || !contains(sl.Symbols, "CreateOrder") {
		t.Fatalf("Symbols = %v", sl.Symbols)
	}
	if !contains(sl.Imports, "context") {
		t.Fatalf("Imports = %v", sl.Imports)
	}

	// 第二次读取应命中文件缓存
	before := r.Stats()
	sl2, err := r.ReadFile(ctx, ref, "service.go")
	if err != nil {
		t.Fatalf("二次 ReadFile 失败: %v", err)
	}
	after := r.Stats()
	if !sl2.FromCache {
		t.Fatalf("二次读取应命中缓存（FromCache=true）")
	}
	if after.Hits <= before.Hits {
		t.Fatalf("二次读取应累加命中计数: %d → %d", before.Hits, after.Hits)
	}
	if sl2.Content != sl.Content {
		t.Fatalf("缓存内容与首次不一致")
	}

	// 不存在的文件
	if _, err := r.ReadFile(ctx, ref, "nope/missing.go"); err == nil {
		t.Fatalf("读取不存在的文件应返回错误")
	}

	// 指定 commit 读取（feature 分支上的新文件）
	featRef := refFor(repo, fx.featureSHA, domain.LoadBudget{})
	featSlice, err := r.ReadFile(ctx, featRef, "feature.go")
	if err != nil {
		t.Fatalf("按 commit 读取失败: %v", err)
	}
	if !strings.Contains(featSlice.Content, "FeatureFlag") {
		t.Fatalf("按 commit 读取内容错误: %q", featSlice.Content)
	}
	// 该文件在 main 上不存在
	if _, err := r.ReadFile(ctx, ref, "feature.go"); err == nil {
		t.Fatalf("main 分支不应存在 feature.go")
	}
}

func TestResolverLoadBudgetTrim(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()

	hints := []domain.FileHint{
		{Path: "service.go", Reason: "frame", Line: 0, Priority: 1.0},
		{Path: "service_test.go", Reason: "symbol", Priority: 0.9},
		{Path: "config.yaml", Reason: "symbol", Priority: 0.8},
	}

	// MaxFiles = 1：只允许加载 1 个文件
	slices, err := r.Load(ctx, refFor(repo, "", domain.LoadBudget{MaxFiles: 1}), hints)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(slices) != 1 {
		t.Fatalf("MaxFiles=1 时应只返回 1 个切片，得到 %d", len(slices))
	}
	if slices[0].Path != "service.go" {
		t.Fatalf("应优先加载最高优先级的 service.go，得到 %q", slices[0].Path)
	}
	// 被加载的切片本身未被裁剪（文件完整），裁剪体现在说明与后续线索被丢弃上。
	notes := r.LastLoadNotes()
	if len(notes) == 0 {
		t.Fatalf("预算裁剪应产生说明（LastLoadNotes）")
	}
	if !strings.Contains(strings.Join(notes, " "), "MaxFiles") {
		t.Fatalf("裁剪说明应指出文件数预算: %v", notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "未加载") {
		t.Fatalf("裁剪说明应说明剩余线索未加载: %v", notes)
	}
	if !errors.Is(ErrBudgetExhausted, ErrBudgetExhausted) {
		t.Fatalf("ErrBudgetExhausted 哨兵值应可用")
	}

	// 字符预算：MaxChars 极小 → 值返回一个裁剪后的片段
	tiny, err := r.Load(ctx, refFor(repo, "", domain.LoadBudget{MaxFiles: 5, MaxChars: 80}), hints)
	if err != nil {
		t.Fatalf("Load(小字符预算) 失败: %v", err)
	}
	if len(tiny) == 0 {
		t.Fatalf("字符预算耗尽时仍应返回首个裁剪片段")
	}
	if tiny[0].Chars > 80 {
		t.Fatalf("裁剪后字符数 %d 超过预算 80", tiny[0].Chars)
	}
	if !tiny[0].Truncated {
		t.Fatalf("字符裁剪应标记 Truncated")
	}
	if !strings.Contains(strings.Join(r.LastLoadNotes(), " "), "MaxChars") {
		t.Fatalf("裁剪说明应指出字符预算: %v", r.LastLoadNotes())
	}

	// 路径深度预算：MaxDepth=2 时 4 层深的线索被跳过并给出说明
	deepHints := []domain.FileHint{{Path: "src/main/java/OrderService.java", Reason: "frame", Priority: 1.0}}
	if _, err := r.Load(ctx, refFor(repo, "", domain.LoadBudget{MaxFiles: 5, MaxDepth: 2}), deepHints); err != nil {
		t.Fatalf("Load(MaxDepth) 失败: %v", err)
	}
	deepNotes := strings.Join(r.LastLoadNotes(), " ")
	if !strings.Contains(deepNotes, "MaxDepth") {
		t.Fatalf("应说明深度预算跳过: %q", deepNotes)
	}
	// 放开深度后不再因深度被跳过（该文件本身不存在，会走"未找到"分支而不是深度分支）
	if _, err := r.Load(ctx, refFor(repo, "", domain.LoadBudget{MaxFiles: 5, MaxDepth: 32}), deepHints); err != nil {
		t.Fatalf("Load(放宽 MaxDepth) 失败: %v", err)
	}
	if strings.Contains(strings.Join(r.LastLoadNotes(), " "), "MaxDepth") {
		t.Fatalf("放宽 MaxDepth 后不应再因深度跳过: %v", r.LastLoadNotes())
	}

	// 目录 / 包路径线索
	dirRef := refFor(repo, "", domain.LoadBudget{MaxFiles: 5, MaxChars: 100000})
	got, err := r.Load(ctx, dirRef, []domain.FileHint{{Path: "service.go", Reason: "frame", Line: 16, Priority: 1.0}})
	if err != nil {
		t.Fatalf("Load(带行号线索) 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应加载 1 个切片，得到 %d", len(got))
	}
	if len(got[0].FocusLines) == 0 || got[0].FocusLines[0] != 16 {
		t.Fatalf("FocusLines 应包含线索行号 16: %v", got[0].FocusLines)
	}
	if got[0].Reason == "" {
		t.Fatalf("Reason 不应为空")
	}

	// 空线索：不加载任何文件，并给出说明
	empty, err := r.Load(ctx, dirRef, nil)
	if err != nil {
		t.Fatalf("Load(空线索) 失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空线索不应加载文件，得到 %d", len(empty))
	}
	if len(r.LastLoadNotes()) == 0 {
		t.Fatalf("空线索应给出说明")
	}
}

func TestResolverLoadFocusWindowForLargeFile(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()

	// 构造一个超过 2000 行的大文件并提交
	var b strings.Builder
	b.WriteString("package main\n")
	for i := 0; i < 3000; i++ {
		b.WriteString("// filler line ")
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	b.WriteString("func BigFunc() {}\n")
	writeFixtureFile(t, fx.originDir, "big.go", b.String())
	runGitCmd(t, fx.originDir, "add", "-A")
	runGitCmd(t, fx.originDir, "commit", "-m", "add big file")
	bigSHA := runGitCmd(t, fx.originDir, "rev-parse", "HEAD")

	ref := refFor(repo, bigSHA, domain.LoadBudget{MaxFiles: 2, MaxChars: 200000, MaxFileSize: 1 << 20})
	slices, err := r.Load(ctx, ref, []domain.FileHint{{Path: "big.go", Reason: "frame", Line: 1500, Priority: 1.0}})
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(slices) != 1 {
		t.Fatalf("应加载 1 个切片，得到 %d", len(slices))
	}
	sl := slices[0]
	if !sl.Truncated {
		t.Fatalf("大文件聚焦切片应标记 Truncated")
	}
	if sl.StartLine >= 1500 || sl.EndLine <= 1500 {
		t.Fatalf("聚焦窗口应覆盖第 1500 行: %d..%d", sl.StartLine, sl.EndLine)
	}
	if sl.EndLine-sl.StartLine+1 > 2*focusRadiusLarge+1 {
		t.Fatalf("聚焦窗口过大: %d..%d", sl.StartLine, sl.EndLine)
	}
	if !strings.Contains(sl.Reason, "聚焦") {
		t.Fatalf("Reason 应说明聚焦切片: %q", sl.Reason)
	}
	if !containsInt(sl.FocusLines, 1500) {
		t.Fatalf("FocusLines = %v", sl.FocusLines)
	}
}

func TestResolverTreeFiltering(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()

	nodes, err := r.Tree(ctx, refFor(repo, "", domain.LoadBudget{}))
	if err != nil {
		t.Fatalf("Tree 失败: %v", err)
	}
	paths := map[string]domain.FileNode{}
	for _, n := range nodes {
		paths[n.Path] = n
	}
	for _, want := range []string{"service.go", "service_test.go", "go.mod", "config.yaml", ".gitignore"} {
		if _, ok := paths[want]; !ok {
			t.Fatalf("Tree 缺少 %q: %v", want, nodePaths(nodes))
		}
	}
	for _, banned := range []string{".git/config", "node_modules/dep/index.js", "dist/bundle.js", "debug.log"} {
		if _, ok := paths[banned]; ok {
			t.Fatalf("Tree 应过滤 %q（忽略规则/.gitignore）: %v", banned, nodePaths(nodes))
		}
	}
	// 目录节点补齐且不含被忽略目录
	if n, ok := paths["node_modules"]; ok && n.IsDir {
		t.Fatalf("node_modules 目录不应出现在树中")
	}
	if !paths["service.go"].IsDir == false {
		t.Fatalf("service.go 应为文件节点")
	}
	if paths["service.go"].Language != LangGo {
		t.Fatalf("语言推断错误: %q", paths["service.go"].Language)
	}
	if paths["service.go"].Size <= 0 {
		t.Fatalf("文件体积应大于 0: %d", paths["service.go"].Size)
	}
}

func TestResolverGrepAndInvalidate(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()
	ref := refFor(repo, "", domain.LoadBudget{})

	// 预热缓存
	if _, err := r.ReadFile(ctx, ref, "service.go"); err != nil {
		t.Fatalf("ReadFile 失败: %v", err)
	}
	if _, err := r.Tree(ctx, ref); err != nil {
		t.Fatalf("Tree 失败: %v", err)
	}
	entriesBefore := r.Stats().Entries
	if entriesBefore == 0 {
		t.Fatalf("缓存条目数应大于 0")
	}

	hints, err := r.Grep(ctx, ref, "CreateOrder", 10)
	if err != nil {
		t.Fatalf("Grep 失败: %v", err)
	}
	if len(hints) == 0 {
		t.Fatalf("Grep 未命中 CreateOrder")
	}
	found := false
	for _, h := range hints {
		if h.Path == "service.go" {
			found = true
			if h.Line <= 0 {
				t.Fatalf("Grep 行号应大于 0: %+v", h)
			}
			// 与文件内容逐行核对，确保行号真实可信。
			content, rerr := r.ReadFile(ctx, ref, "service.go")
			if rerr != nil {
				t.Fatalf("ReadFile 失败: %v", rerr)
			}
			lineText, _, _ := SliceLines(content.Content, h.Line, h.Line)
			if !strings.Contains(lineText, "CreateOrder") {
				t.Fatalf("Grep 行号 %d 指向的内容不含 CreateOrder: %q", h.Line, lineText)
			}
			if h.Reason != "grep" || h.Priority <= 0 {
				t.Fatalf("Grep 线索字段不完整: %+v", h)
			}
		}
	}
	if !found {
		t.Fatalf("Grep 结果缺少 service.go: %+v", hints)
	}

	// 正则兜底：字面量无结果时按 ERE 重试（模式含 `(` 等元字符，只有正则路径能命中）
	reHints, err := r.Grep(ctx, ref, `CreateOrder\(ctx`, 10)
	if err != nil {
		t.Fatalf("Grep(正则) 失败: %v", err)
	}
	if len(reHints) == 0 {
		t.Fatalf("正则兜底未命中")
	}
	globHints, err := r.Grep(ctx, ref, "CreateOrder.*ctx", 10)
	if err != nil {
		t.Fatalf("Grep(通配) 失败: %v", err)
	}
	if len(globHints) == 0 {
		t.Fatalf("通配模式未命中")
	}

	// Invalidate 后条目数下降
	if err := r.Invalidate(ctx, repo.TenantID, repo.ID); err != nil {
		t.Fatalf("Invalidate 失败: %v", err)
	}
	if after := r.Stats().Entries; after >= entriesBefore {
		t.Fatalf("Invalidate 后条目数应下降: %d → %d", entriesBefore, after)
	}
	// 失效后仍可正常读取（重新回源）
	sl, err := r.ReadFile(ctx, ref, "service.go")
	if err != nil {
		t.Fatalf("失效后 ReadFile 失败: %v", err)
	}
	if sl.FromCache {
		t.Fatalf("失效后首次读取不应命中缓存")
	}
	// 全量清理
	if err := r.Invalidate(ctx, "", ""); err != nil {
		t.Fatalf("全量 Invalidate 失败: %v", err)
	}
	if got := r.Stats().Entries; got != 0 {
		t.Fatalf("全量失效后条目数应为 0，得到 %d", got)
	}
}

func TestResolverStats(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()
	ref := refFor(repo, "", domain.LoadBudget{MaxFileSize: 1 << 20})

	if _, err := r.ReadFile(ctx, ref, "service.go"); err != nil {
		t.Fatalf("ReadFile 失败: %v", err)
	}
	if _, err := r.ReadFile(ctx, ref, "service.go"); err != nil {
		t.Fatalf("二次 ReadFile 失败: %v", err)
	}
	if _, err := r.ReadFile(ctx, ref, "go.mod"); err != nil {
		t.Fatalf("ReadFile(go.mod) 失败: %v", err)
	}

	st := r.Stats()
	if st.Hits < 1 {
		t.Fatalf("Hits 应 >= 1，得到 %d", st.Hits)
	}
	if st.Misses < 1 {
		t.Fatalf("Misses 应 >= 1，得到 %d", st.Misses)
	}
	if st.Entries < 2 {
		t.Fatalf("Entries 应 >= 2，得到 %d", st.Entries)
	}
	if st.Bytes <= 0 {
		t.Fatalf("Bytes 应大于 0，得到 %d", st.Bytes)
	}
	if st.Capacity <= 0 {
		t.Fatalf("Capacity 应大于 0，得到 %d", st.Capacity)
	}
	want := float64(st.Hits) / float64(st.Hits+st.Misses)
	if diff := st.HitRate - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("HitRate = %v，期望 %v", st.HitRate, want)
	}
	if st.HitRate <= 0 || st.HitRate > 1 {
		t.Fatalf("HitRate 应在 (0,1]: %v", st.HitRate)
	}
}

func TestResolverMaterialize(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	ctx := context.Background()

	destRoot := filepath.Join(t.TempDir(), "workspaces")
	dir, err := r.Materialize(ctx, domain.RepoRef{Repository: repo, Commit: fx.featureSHA}, destRoot)
	if err != nil {
		t.Fatalf("Materialize 失败: %v", err)
	}
	if filepath.Base(dir) != repo.Key {
		t.Fatalf("物化目录应以 repoKey 结尾: %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "service.go")); err != nil {
		t.Fatalf("物化结果缺少 service.go: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "feature.go")); err != nil {
		t.Fatalf("物化结果缺少 feature.go（应按指定 commit 检出）: %v", err)
	}
	// 幂等：重复物化复用同一目录
	again, err := r.Materialize(ctx, domain.RepoRef{Repository: repo, Commit: fx.featureSHA}, destRoot)
	if err != nil {
		t.Fatalf("重复 Materialize 失败: %v", err)
	}
	if again != dir {
		t.Fatalf("重复物化目录应一致: %q vs %q", again, dir)
	}
	// 换 commit 重新物化
	mainDir, err := r.Materialize(ctx, domain.RepoRef{Repository: repo, Commit: fx.mainSHA}, destRoot)
	if err != nil {
		t.Fatalf("按 main 物化失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mainDir, "feature.go")); err == nil {
		t.Fatalf("main commit 物化结果不应包含 feature.go")
	}
}

func TestResolverLocalPathRepoWithoutGit(t *testing.T) {
	// 仓库 URL 指向不存在的路径：Resolve 必须快速失败且不 panic。
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)
	bad := *repo
	bad.ID = "repo-bad"
	bad.URL = filepath.Join(t.TempDir(), "does-not-exist")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := r.Resolve(ctx, domain.RepoRef{Repository: &bad}); err == nil {
		t.Fatalf("不存在的仓库应返回错误")
	}

	// 未提供仓库对象
	if _, err := r.Resolve(ctx, domain.RepoRef{}); err == nil {
		t.Fatalf("未提供仓库应返回错误")
	}
	// 停用仓库
	disabled := *repo
	disabled.ID = "repo-disabled"
	disabled.Status = domain.RepoDisabled
	if _, err := r.Resolve(ctx, domain.RepoRef{Repository: &disabled}); err == nil {
		t.Fatalf("停用仓库应返回错误")
	}
	// Load / Tree / Grep 的入参校验
	if _, err := r.Load(ctx, domain.RepoRef{}, nil); err == nil {
		t.Fatalf("Load 未提供仓库应返回错误")
	}
	if _, err := r.Tree(ctx, domain.RepoRef{}); err == nil {
		t.Fatalf("Tree 未提供仓库应返回错误")
	}
	if _, err := r.Grep(ctx, domain.RepoRef{}, "x", 1); err == nil {
		t.Fatalf("Grep 未提供仓库应返回错误")
	}
}

func TestResolverRepoByID(t *testing.T) {
	fx := setupGitFixture(t)
	r, repo := newTestResolver(t, fx)

	// 入参已带仓库对象：直接返回
	got, err := r.repoByID(&repoMeta{repoID: repo.ID, tenantID: repo.TenantID, repo: repo})
	if err != nil || got == nil || got.ID != repo.ID {
		t.Fatalf("repoByID(带对象) = %v, %v", got, err)
	}
	// 仅给 ID：从 store 补齐
	got, err = r.repoByID(&repoMeta{repoID: repo.ID, tenantID: repo.TenantID})
	if err != nil || got == nil || got.Key != repo.Key {
		t.Fatalf("repoByID(仅 ID) = %v, %v", got, err)
	}
	// 未知仓库
	if _, err := r.repoByID(&repoMeta{repoID: "missing", tenantID: repo.TenantID}); err == nil {
		t.Fatalf("未知仓库应返回错误")
	}
	// 跨租户访问不可见
	if _, err := r.repoByID(&repoMeta{repoID: repo.ID, tenantID: "other-tenant"}); err == nil {
		t.Fatalf("跨租户查询应返回错误")
	}
}

func TestGitRepoManagerHelpers(t *testing.T) {
	fx := setupGitFixture(t)
	m := NewGitRepoManager(config.SourceConfig{CacheDir: t.TempDir(), GitBinary: "git", CloneTimeoutSec: 60}, logx.Nop())
	defer func() { _ = m.Close() }()

	if !m.GitAvailable() {
		t.Skip("git 不可用，跳过")
	}
	ctx := context.Background()

	repo := &domain.Repository{ID: "r1", TenantID: "t1", Key: "order-service",
		URL: fx.originDir, DefaultBranch: "main", Provider: domain.VCSGit, Status: domain.RepoActive}
	dir, err := m.EnsureMirror(ctx, repo, "")
	if err != nil {
		t.Fatalf("EnsureMirror 失败: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join("mirrors", "t1", "r1.git")) {
		t.Fatalf("镜像目录布局不符: %q", dir)
	}
	// 二次调用走 fetch 分支
	if dir2, err := m.EnsureMirror(ctx, repo, ""); err != nil || dir2 != dir {
		t.Fatalf("二次 EnsureMirror 失败: dir=%q err=%v", dir2, err)
	}

	head, err := m.Head(ctx, dir)
	if err != nil {
		t.Fatalf("Head 失败: %v", err)
	}
	if head != fx.mainSHA {
		t.Fatalf("Head = %q，期望 %q", head, fx.mainSHA)
	}
	branches, err := m.Branches(ctx, dir)
	if err != nil {
		t.Fatalf("Branches 失败: %v", err)
	}
	if !contains(branches, "main") || !contains(branches, "feature") {
		t.Fatalf("Branches = %v", branches)
	}
	for _, ref := range []string{"main", "feature", fx.featureSHA, "origin/feature", ""} {
		if _, err := m.ResolveCommit(ctx, dir, ref); err != nil {
			t.Fatalf("ResolveCommit(%q) 失败: %v", ref, err)
		}
	}
	if _, err := m.ResolveCommit(ctx, dir, "nope"); err == nil {
		t.Fatalf("不存在的 ref 应返回错误")
	}

	content, err := m.Show(ctx, dir, fx.mainSHA, "service.go")
	if err != nil {
		t.Fatalf("Show 失败: %v", err)
	}
	if !strings.Contains(string(content), "CreateOrder") {
		t.Fatalf("Show 内容错误")
	}
	if _, err := m.Show(ctx, dir, fx.mainSHA, "missing.go"); err == nil {
		t.Fatalf("Show 不存在的文件应返回错误")
	}

	nodes, err := m.LsTree(ctx, dir, fx.mainSHA)
	if err != nil {
		t.Fatalf("LsTree 失败: %v", err)
	}
	if len(nodes) < 5 {
		t.Fatalf("LsTree 结果过少: %d", len(nodes))
	}
	grepHints, err := m.GrepCommit(ctx, dir, fx.mainSHA, "CreateOrder", 5)
	if err != nil {
		t.Fatalf("GrepCommit 失败: %v", err)
	}
	if len(grepHints) == 0 || grepHints[0].Line == 0 {
		t.Fatalf("GrepCommit 结果异常: %+v", grepHints)
	}

	// Checkout（tar 或 shared clone 任一路径均可）
	dest := filepath.Join(t.TempDir(), "work")
	if err := m.Checkout(ctx, dir, fx.mainSHA, dest); err != nil {
		t.Fatalf("Checkout 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "service.go")); err != nil {
		t.Fatalf("Checkout 结果缺少 service.go: %v", err)
	}

	// 凭证脱敏
	withCred := &domain.Repository{ID: "r2", TenantID: "t1", Key: "secret",
		URL: "https://git.example.com/acme/order.git", Provider: domain.VCSGitLab, Status: domain.RepoActive}
	_, display := buildAuthURL(withCred, "sup3r-s3cret-token")
	if strings.Contains(display, "sup3r-s3cret-token") {
		t.Fatalf("展示用 URL 泄露了凭证: %q", display)
	}
	if strings.Contains(SanitizeURL("https://u:p@git.example.com/a.git"), "p@") {
		t.Fatalf("SanitizeURL 未脱敏: %q", SanitizeURL("https://u:p@git.example.com/a.git"))
	}
	if got := RedactSecret("token=abc12345", "abc12345"); strings.Contains(got, "abc12345") {
		t.Fatalf("RedactSecret 未脱敏: %q", got)
	}
}

func TestShouldIgnorePath(t *testing.T) {
	patterns := []string{".git", "node_modules", "dist", "*.min.js", "src/generated", "**/tmp"}
	cases := map[string]bool{
		".git/config":                 true,
		"node_modules/react/index.js": true,
		"dist/bundle.js":              true,
		"a/b/dist/bundle.js":          true,
		"app.min.js":                  true,
		"static/app.min.js":           true,
		"src/generated/Api.java":      true,
		"x/tmp/file.go":               true,
		"service.go":                  false,
		"src/main/Service.java":       false,
		"distributed/a.go":            false,
	}
	for path, want := range cases {
		if got := shouldIgnorePath(path, patterns); got != want {
			t.Fatalf("shouldIgnorePath(%q) = %v，期望 %v", path, got, want)
		}
	}
}

func TestGlobToRegex(t *testing.T) {
	cases := map[string]string{
		"CreateOrder":   "CreateOrder",
		"a.b":           `a\.b`,
		"/api/order/**": "/api/order/.*",
		"/api/order/*":  "/api/order/[^/]*",
		"a?c":           "a.c",
		"a+b":           `a\+b`,
		"[x]":           `\[x\]`,
	}
	for in, want := range cases {
		if got := globToRegex(in); got != want {
			t.Fatalf("globToRegex(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func nodePaths(nodes []domain.FileNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	return out
}

// containsInt 判断整型切片是否包含某值。
func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
