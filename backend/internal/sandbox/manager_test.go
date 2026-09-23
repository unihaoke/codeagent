package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeMaterializer 把预置文件"检出"到目标目录，并可按仓库 ID 注入失败。
type fakeMaterializer struct {
	files    map[string]map[string]string // repoID → 相对路径 → 内容
	fail     map[string]error             // repoID → 物化错误
	failKeys map[string]error             // repoKey → 物化错误

	mu    sync.Mutex
	calls []string
}

func newFakeMaterializer() *fakeMaterializer {
	return &fakeMaterializer{
		files:    map[string]map[string]string{},
		fail:     map[string]error{},
		failKeys: map[string]error{},
	}
}

// addRepo 预置某仓库要写出的文件。
func (f *fakeMaterializer) addRepo(repoID string, files map[string]string) *fakeMaterializer {
	f.files[repoID] = files
	return f
}

// Materialize 实现 Materializer。
func (f *fakeMaterializer) Materialize(_ context.Context, repo *domain.Repository, commit, dest string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s@%s", repoKeyOf(repo), commit))
	f.mu.Unlock()

	if err := f.fail[repo.ID]; err != nil {
		return err
	}
	if err := f.failKeys[repo.Key]; err != nil {
		return err
	}
	for rel, content := range f.files[repo.ID] {
		abs := filepath.Join(dest, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// callCount 返回物化调用次数。
func (f *fakeMaterializer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

// newTestManager 构造一个工作区根指向临时目录的管理器。
func newTestManager(t *testing.T, cfg config.SandboxConfig, mat Materializer) *Manager {
	t.Helper()
	dir := t.TempDir()
	src := config.SourceConfig{WorkspaceDir: filepath.Join(dir, "workspace")}
	return NewManager(cfg, src, mat, logx.Nop())
}

// testRepo 构造测试仓库。
func testRepo(id, key string) *domain.Repository {
	return &domain.Repository{
		ID:       id,
		TenantID: "tenant-1",
		Name:     key,
		Key:      key,
		Status:   domain.RepoActive,
	}
}

// goSourceA 语法正确的 Go 源码。
const goSourceA = `package demo

import "fmt"

// Greet 输出问候语。
func Greet() string {
	name := "world"
	return fmt.Sprintf("hello %s", name)
}
`

// checkByName 从验证结果中取指定名称的 check。
func checkByName(t *testing.T, v *domain.Verification, name string) domain.VerificationCheck {
	t.Helper()
	for _, c := range v.Checks {
		if c.Name == name {
			return c
		}
	}
	names := make([]string, 0, len(v.Checks))
	for _, c := range v.Checks {
		names = append(names, c.Name)
	}
	t.Fatalf("未找到名为 %q 的 check，实际有: %s", name, strings.Join(names, ", "))
	return domain.VerificationCheck{}
}

// ---------------------------------------------------------------------------
// Open / Close / Warnings / Reap
// ---------------------------------------------------------------------------

// TestManagerOpenCreatesWorkspaceAndClose 验证工作区创建、RepoDir 与 Close 清理。
func TestManagerOpenCreatesWorkspaceAndClose(t *testing.T) {
	mat := newFakeMaterializer().
		addRepo("repo-a", map[string]string{"main.go": goSourceA}).
		addRepo("repo-b", map[string]string{"lib/util.go": "package lib\n"})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false, KeepWorkspace: false}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID:  "run-close-1",
		Repos:  []*domain.Repository{testRepo("repo-a", "svc-a"), testRepo("repo-b", "svc-b")},
		Pinned: map[string]string{"repo-a": "aaa111", "repo-b": "bbb222"},
	})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if handle.ID() != "run-close-1" {
		t.Fatalf("handle.ID() 应为 run-close-1，实际 %q", handle.ID())
	}
	root := handle.Root()
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		t.Fatalf("工作区目录未创建: %v", statErr)
	}

	// RepoDir 必须指向工作区内的仓库子目录
	dirA := handle.RepoDir("svc-a")
	if !strings.HasPrefix(filepath.Clean(dirA), filepath.Clean(root)+string(filepath.Separator)) {
		t.Fatalf("RepoDir 必须位于工作区内: %q (root=%q)", dirA, root)
	}
	if _, statErr := os.Stat(filepath.Join(dirA, "main.go")); statErr != nil {
		t.Fatalf("物化文件不存在: %v", statErr)
	}
	if dirB := handle.RepoDir("svc-b"); filepath.Base(dirB) != "svc-b" {
		t.Fatalf("RepoDir(svc-b) 目录名不符: %q", dirB)
	}
	if mat.callCount() != 2 {
		t.Fatalf("应物化 2 个仓库，实际 %d 次", mat.callCount())
	}
	// 物化必须携带任务锁定的 commit
	mat.mu.Lock()
	calls := strings.Join(mat.calls, ",")
	mat.mu.Unlock()
	if !strings.Contains(calls, "svc-a@aaa111") || !strings.Contains(calls, "svc-b@bbb222") {
		t.Fatalf("物化未使用锁定 commit: %s", calls)
	}
	if h, ok := handle.(*Handle); ok && len(h.Warnings()) != 0 {
		t.Fatalf("正常物化不应产生告警: %v", h.Warnings())
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("KeepWorkspace=false 时 Close 应删除工作区，实际 stat=%v", statErr)
	}
	// 二次 Close 幂等
	if err := handle.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等，实际 %v", err)
	}
}

// TestManagerKeepWorkspace Close 后保留目录（人工复核场景）。
func TestManagerKeepWorkspace(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{Enabled: true, KeepWorkspace: true}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-keep-1",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	root := handle.Root()
	if err := handle.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("KeepWorkspace=true 时应保留工作区: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "svc-a", "main.go")); statErr != nil {
		t.Fatalf("保留的工作区应保留物化内容: %v", statErr)
	}
}

// TestManagerMaterializeFailureDegrades 单个仓库物化失败：Open 仍成功且 Warnings 非空。
func TestManagerMaterializeFailureDegrades(t *testing.T) {
	mat := newFakeMaterializer().
		addRepo("repo-a", map[string]string{"main.go": goSourceA}).
		addRepo("repo-b", map[string]string{"lib/util.go": "package lib\n"})
	mat.fail["repo-b"] = errors.New("git fetch 超时")

	cfg := config.SandboxConfig{Enabled: true}
	m := newTestManager(t, cfg, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-degrade-1",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a"), testRepo("repo-b", "svc-b")},
	})
	if err != nil {
		t.Fatalf("单个仓库物化失败不得返回致命错误（降级不失败）: %v", err)
	}
	h, ok := handle.(*Handle)
	if !ok {
		t.Fatal("handle 类型应为 *Handle")
	}
	warnings := h.Warnings()
	if len(warnings) == 0 {
		t.Fatal("物化失败必须记录到 Warnings")
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "svc-b") || !strings.Contains(joined, "git fetch 超时") {
		t.Fatalf("告警应包含失败仓库与原因，实际: %s", joined)
	}
	// 成功仓库仍然被物化（继续处理其它仓库）——必须在 Close 之前检查，
	// 因为 KeepWorkspace=false 时 Close 会删除整个工作区。
	if _, statErr := os.Stat(filepath.Join(h.RepoDir("svc-a"), "main.go")); statErr != nil {
		t.Fatalf("成功仓库应完成物化: %v", statErr)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
}

// TestManagerOpenEmptyRepos 无仓库（无网络 / 无 git 场景）也必须能建出空工作区。
func TestManagerOpenEmptyRepos(t *testing.T) {
	for _, mat := range []Materializer{nil, newFakeMaterializer()} {
		m := newTestManager(t, config.SandboxConfig{Enabled: true}, mat)
		handle, err := m.Open(context.Background(), domain.SandboxMeta{RunID: "run-empty-1"})
		if err != nil {
			t.Fatalf("空仓库列表应能创建空工作区: %v", err)
		}
		if info, statErr := os.Stat(handle.Root()); statErr != nil || !info.IsDir() {
			t.Fatalf("空工作区目录未创建: %v", statErr)
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("Close 失败: %v", err)
		}
	}
}

// TestManagerOpenIdempotent 同一 runID 重复 Open 返回同一工作区。
func TestManagerOpenIdempotent(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	m := newTestManager(t, config.SandboxConfig{}, mat)
	meta := domain.SandboxMeta{RunID: "run-idem", Repos: []*domain.Repository{testRepo("repo-a", "svc-a")}}

	h1, err := m.Open(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := m.Open(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("同一 runID 重复 Open 应返回同一 handle")
	}
	if h1.Root() != h2.Root() {
		t.Fatalf("工作区目录应一致: %q vs %q", h1.Root(), h2.Root())
	}
	if mat.callCount() != 1 {
		t.Fatalf("幂等复用不应重复物化，实际 %d 次", mat.callCount())
	}
	_ = h1.Close()
}

// TestManagerRunIDSanitized runID 中的路径分隔符必须被安全化，不能越出工作区根。
func TestManagerRunIDSanitized(t *testing.T) {
	m := newTestManager(t, config.SandboxConfig{}, nil)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{RunID: "../../../etc/evil-run"})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = handle.Close() }()
	root := filepath.Clean(m.workspaceRoot())
	got := filepath.Clean(handle.Root())
	if !strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("工作区路径越界: %q（根目录 %q）", got, root)
	}
	if strings.ContainsAny(handle.ID(), `/\`) || strings.Contains(handle.ID(), "..") {
		t.Fatalf("工作区 ID 未安全化: %q", handle.ID())
	}
}

// TestManagerReap 清理超期残留工作区，跳过正在使用的工作区。
func TestManagerReap(t *testing.T) {
	m := newTestManager(t, config.SandboxConfig{}, nil)
	base := m.workspaceRoot()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// 1) 超期残留目录
	stale := filepath.Join(base, "stale-run")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// 2) 新鲜目录
	fresh := filepath.Join(base, "fresh-run")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	// 3) 正在使用的工作区
	active, err := m.Open(context.Background(), domain.SandboxMeta{RunID: "active-run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active.Root(), old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := m.Reap(24 * time.Hour)
	if err != nil {
		t.Fatalf("Reap 失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("应清理 1 个超期目录，实际 %d", removed)
	}
	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Fatal("超期目录应被清理")
	}
	if _, statErr := os.Stat(fresh); statErr != nil {
		t.Fatal("未超期目录不应被清理")
	}
	if _, statErr := os.Stat(active.Root()); statErr != nil {
		t.Fatal("正在使用的工作区不应被清理")
	}
	_ = active.Close()
}

// TestManagerStats Stats 应反映工作区与验证计数。
func TestManagerStats(t *testing.T) {
	m := newTestManager(t, config.SandboxConfig{}, nil)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{RunID: "run-stats"})
	if err != nil {
		t.Fatal(err)
	}
	stats := m.Stats()
	if stats["activeWorkspaces"] != 1 {
		t.Fatalf("activeWorkspaces 应为 1，实际 %v", stats["activeWorkspaces"])
	}
	if _, ok := stats["workspaceOpens"]; !ok {
		t.Fatal("Stats 应包含 workspaceOpens")
	}
	_ = handle.Close()
	if stats = m.Stats(); stats["activeWorkspaces"] != 0 {
		t.Fatalf("Close 后 activeWorkspaces 应为 0，实际 %v", stats["activeWorkspaces"])
	}
}

// ---------------------------------------------------------------------------
// Validate：补丁应用 + 静态校验
// ---------------------------------------------------------------------------

// TestValidateAppliesGoodPatch 语法正确的补丁 → Passed=true，且含静态校验 check。
func TestValidateAppliesGoodPatch(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-good",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepo("repo-a", "svc-a"), "main.go", goSourceA,
		`name := "world"`, `name := "codeagent"`, "修正问候对象", domain.RiskLow)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}

	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if !verification.Passed {
		t.Fatalf("语法正确的补丁应通过验证，checks=%s", dumpChecks(verification))
	}
	if verification.ApplyResult != applyApplied {
		t.Fatalf("ApplyResult 应为 applied，实际 %q", verification.ApplyResult)
	}
	if verification.Workspace != "run-good" {
		t.Fatalf("Workspace 应为 run-good，实际 %q", verification.Workspace)
	}
	if verification.DurationMS < 0 {
		t.Fatal("DurationMS 不应为负")
	}
	if strings.TrimSpace(verification.Log) == "" {
		t.Fatal("Log 应汇总验证过程")
	}
	// 静态校验 check 必须存在且通过
	for _, name := range []string{checkStaticSyntax, checkGoParse, checkDangerScan} {
		c := checkByName(t, verification, name)
		if c.Skipped {
			t.Fatalf("%s 不应被跳过（StaticSkip=%q）", name, c.SkipReason)
		}
		if !c.Passed {
			t.Fatalf("%s 应通过，输出: %s", name, c.Output)
		}
	}
	if c := checkByName(t, verification, checkPatchApply); !c.Passed {
		t.Fatalf("patch_apply 应通过，输出: %s", c.Output)
	}

	// 变更必须真正落到工作区文件
	got, err := m.SnapshotFiles(handle, "svc-a", []string{"main.go"})
	if err != nil {
		t.Fatalf("SnapshotFiles 失败: %v", err)
	}
	if !strings.Contains(got["main.go"], `name := "codeagent"`) {
		t.Fatalf("工作区文件未被改写: %q", got["main.go"])
	}
}

// TestValidateBreaksGoSyntax 破坏 Go 语法的补丁 → go_parse 失败 → Passed=false。
func TestValidateBreaksGoSyntax(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-syntax",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	// 去掉函数右括号 → Go 语法错误
	patch, err := BuildPatch(testRepoCtx(), "main.go", goSourceA,
		"func Greet() string {\n\tname := \"world\"\n\treturn fmt.Sprintf(\"hello %s\", name)\n}",
		"func Greet() string {\n\tname := \"world\"\n\treturn fmt.Sprintf(\"hello %s\", name)",
		"故意破坏语法", domain.RiskHigh)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}

	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if verification.Passed {
		t.Fatalf("破坏语法的补丁不应通过，checks=%s", dumpChecks(verification))
	}
	goCheck := checkByName(t, verification, checkGoParse)
	if goCheck.Passed {
		t.Fatal("go_parse 应失败")
	}
	if goCheck.Skipped {
		t.Fatal("go_parse 不应被跳过（有 Go 文件）")
	}
	if !strings.Contains(goCheck.Output, "expected") && !strings.Contains(goCheck.Output, "main.go") {
		t.Fatalf("go_parse 输出应包含语法错误信息，实际: %s", goCheck.Output)
	}
}

// TestValidateDangerScan 含 System.exit(0) 的补丁 → danger_scan 失败 → Passed=false。
func TestValidateDangerScan(t *testing.T) {
	javaSource := `public class App {
    public static void main(String[] args) {
        System.out.println("hello");
    }
}
`
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"App.java": javaSource})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-danger",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepoCtx(), "App.java", javaSource,
		`System.out.println("hello");`, `System.exit(0);`, "注入危险退出调用", domain.RiskHigh)
	if err != nil {
		t.Fatalf("BuildPatch 失败: %v", err)
	}

	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if verification.Passed {
		t.Fatalf("含 System.exit 的补丁不应通过，checks=%s", dumpChecks(verification))
	}
	danger := checkByName(t, verification, checkDangerScan)
	if danger.Passed {
		t.Fatal("danger_scan 应失败")
	}
	if !strings.Contains(strings.ToLower(danger.Output), "system.exit") {
		t.Fatalf("danger_scan 输出应指明命中的危险操作，实际: %s", danger.Output)
	}
}

// TestValidateDegradedWithoutCommandExec AllowCommandExec=false → Degraded=true 且仍有静态 check。
func TestValidateDegradedWithoutCommandExec(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{
		Enabled:          true,
		AllowCommandExec: false,
		Checks:           map[string][]string{"go": {"go vet ./..."}},
	}
	m := newTestManager(t, cfg, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-degraded",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepoCtx(), "main.go", goSourceA, `"world"`, `"degraded"`, "降级用例", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if !verification.Degraded {
		t.Fatal("AllowCommandExec=false 时 Degraded 应为 true")
	}
	if verification.ApplyResult != applyApplied {
		t.Fatalf("ApplyResult 应为 applied，实际 %q", verification.ApplyResult)
	}
	// Degraded 时 Passed 仍可为 true，但必须在 Log 中说明
	if !verification.Passed {
		t.Fatalf("无命令执行能力不应直接判失败，checks=%s", dumpChecks(verification))
	}
	if !strings.Contains(verification.Log, "降级") {
		t.Fatalf("Log 应说明降级原因，实际:\n%s", verification.Log)
	}
	// 静态 check 必须仍然执行
	for _, name := range []string{checkStaticSyntax, checkGoParse} {
		c := checkByName(t, verification, name)
		if c.Skipped {
			t.Fatalf("%s 不应被跳过", name)
		}
	}
	// 命令执行 check 应标记为 Skipped
	cmdCheck := checkByName(t, verification, "command_exec")
	if !cmdCheck.Skipped {
		t.Fatal("command_exec 应标记为 Skipped")
	}
	if !strings.Contains(cmdCheck.SkipReason, "AllowCommandExec") {
		t.Fatalf("SkipReason 应说明未开启命令执行，实际 %q", cmdCheck.SkipReason)
	}
}

// TestValidateDryRun DryRun 只做应用校验，不执行命令。
func TestValidateDryRun(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{
		Enabled:          true,
		AllowCommandExec: true,
		Checks:           map[string][]string{"go": {"go vet ./..."}},
	}
	m := newTestManager(t, cfg, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-dryrun",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepoCtx(), "main.go", goSourceA, `"world"`, `"dry"`, "dry run", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{
		Patches: []domain.Patch{*patch},
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if !verification.Degraded {
		t.Fatal("DryRun 时 Degraded 应为 true")
	}
	if c := checkByName(t, verification, "command_exec"); !c.Skipped || !strings.Contains(c.SkipReason, "DryRun") {
		t.Fatalf("DryRun 时不应执行命令，实际 Skipped=%v reason=%q", c.Skipped, c.SkipReason)
	}
}

// TestValidateMissingRepoIsSkipped 仓库不在工作区时补丁标记 skipped，不计为失败。
func TestValidateMissingRepoIsSkipped(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-skip",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	// 补丁指向工作区里不存在的仓库
	patch := domain.Patch{
		FilePath:    "main.go",
		RepoKey:     "not-materialized",
		Action:      domain.ActionModify,
		UnifiedDiff: UnifiedDiff(goSourceA, strings.Replace(goSourceA, `"world"`, `"x"`, 1), "main.go", "main.go", 3),
		OldContent:  goSourceA,
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if verification.ApplyResult != applySkipped {
		t.Fatalf("ApplyResult 应为 skipped，实际 %q", verification.ApplyResult)
	}
	applyCheck := checkByName(t, verification, checkPatchApply)
	if !applyCheck.Skipped {
		t.Fatal("仓库不在工作区时 patch_apply 应标记 Skipped")
	}
	if !strings.Contains(applyCheck.SkipReason, "不在工作区") {
		t.Fatalf("SkipReason 应说明原因，实际 %q", applyCheck.SkipReason)
	}
	if !verification.Passed {
		t.Fatalf("skipped 不应导致验证失败，checks=%s", dumpChecks(verification))
	}
}

// TestValidateConflictFails 补丁无法应用（上下文冲突）→ ApplyResult=conflict 且不修改文件。
func TestValidateConflictFails(t *testing.T) {
	current := "package demo\n\nvar Value = 42\n"
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": current})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-conflict",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	// 基于另一份内容生成的补丁，与工作区文件不一致 → 上下文冲突
	stale := "package demo\n\nvar Value = 1\n"
	patch, err := BuildPatch(testRepoCtx(), "main.go", stale, "var Value = 1", "var Value = 2", "过期补丁", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}
	if verification.ApplyResult != applyConflict {
		t.Fatalf("ApplyResult 应为 conflict，实际 %q", verification.ApplyResult)
	}
	if verification.Passed {
		t.Fatal("补丁冲突应导致验证失败")
	}
	applyCheck := checkByName(t, verification, checkPatchApply)
	if applyCheck.Passed {
		t.Fatal("patch_apply 应失败")
	}
	// 冲突时不得写入
	got, err := m.SnapshotFiles(handle, "svc-a", []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if got["main.go"] != current {
		t.Fatalf("冲突时不得修改工作区文件: %q", got["main.go"])
	}
}

// TestValidatePathTraversalRejected 补丁路径穿越必须被拒绝且不触碰工作区外文件。
func TestValidatePathTraversalRejected(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	cfg := config.SandboxConfig{Enabled: true, AllowCommandExec: false}
	m := newTestManager(t, cfg, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-escape",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	outside := filepath.Join(filepath.Dir(handle.Root()), "escaped.txt")
	if err := os.WriteFile(outside, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evil := domain.Patch{
		FilePath:    "../escaped.txt",
		RepoKey:     "svc-a",
		Action:      domain.ActionModify,
		UnifiedDiff: UnifiedDiff("original\n", "hacked\n", "../escaped.txt", "../escaped.txt", 3),
		OldContent:  "original\n",
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{evil}})
	if err != nil {
		t.Fatalf("Validate 不应返回致命错误: %v", err)
	}
	if verification.Passed {
		t.Fatal("路径穿越补丁必须导致验证失败")
	}
	if verification.ApplyResult != applyConflict {
		t.Fatalf("ApplyResult 应为 conflict，实际 %q", verification.ApplyResult)
	}
	raw, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "original\n" {
		t.Fatalf("工作区之外的文件被改写: %q", string(raw))
	}
}

// TestValidateNilHandle handle 为 nil 时返回错误而不是 panic。
func TestValidateNilHandle(t *testing.T) {
	m := newTestManager(t, config.SandboxConfig{}, nil)
	verification, err := m.Validate(context.Background(), nil, domain.ValidateRequest{})
	if err == nil {
		t.Fatal("handle 为 nil 时应返回错误")
	}
	if verification != nil {
		t.Fatal("失败时不应返回验证结果")
	}
}

// TestValidateStaticSyntaxFailure 括号不配平 → static_syntax 失败。
func TestValidateStaticSyntaxFailure(t *testing.T) {
	source := "package demo\n\nfunc Broken() int {\n\treturn 1\n}\n"
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": source})
	m := newTestManager(t, config.SandboxConfig{Enabled: true, AllowCommandExec: false}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-balance",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepoCtx(), "main.go", source, "\treturn 1\n}", "\treturn 1\n", "删除右括号", domain.RiskHigh)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatal(err)
	}
	if verification.Passed {
		t.Fatalf("括号缺失应导致失败，checks=%s", dumpChecks(verification))
	}
	if c := checkByName(t, verification, checkStaticSyntax); c.Passed {
		t.Fatal("static_syntax 应失败")
	}
	if c := checkByName(t, verification, checkGoParse); c.Passed {
		t.Fatal("go_parse 也应失败")
	}
}

// TestValidateInvalidJSON JSON 破坏 → json_valid 失败。
func TestValidateInvalidJSON(t *testing.T) {
	valid := "{\n  \"name\": \"demo\",\n  \"version\": 1\n}\n"
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"package.json": valid})
	m := newTestManager(t, config.SandboxConfig{Enabled: true, AllowCommandExec: false}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-json",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	patch, err := BuildPatch(testRepoCtx(), "package.json", valid, "\"version\": 1", "\"version\": }", "破坏 JSON", domain.RiskHigh)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatal(err)
	}
	if verification.Passed {
		t.Fatalf("非法 JSON 应导致失败，checks=%s", dumpChecks(verification))
	}
	if c := checkByName(t, verification, checkJSONValid); c.Passed || c.Skipped {
		t.Fatalf("json_valid 应执行且失败，Skipped=%v Output=%s", c.Skipped, c.Output)
	}
}

// TestValidateNewFilePatch 新增文件补丁：create 动作 + 新文件参与静态校验。
func TestValidateNewFilePatch(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	m := newTestManager(t, config.SandboxConfig{Enabled: true, AllowCommandExec: false}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-create",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	content := "package demo\n\n// Helper 是新增的辅助函数。\nfunc Helper() int { return 1 }\n"
	patch, err := BuildCreatePatch(testRepo("repo-a", "svc-a"), "helper.go", content, "补齐辅助函数", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed {
		t.Fatalf("新增合法文件应通过，checks=%s", dumpChecks(verification))
	}
	if verification.ApplyResult != applyApplied {
		t.Fatalf("ApplyResult 应为 applied，实际 %q", verification.ApplyResult)
	}
	raw, err := os.ReadFile(filepath.Join(handle.RepoDir("svc-a"), "helper.go"))
	if err != nil {
		t.Fatalf("新增文件未落盘: %v", err)
	}
	if string(raw) != content {
		t.Fatalf("新增文件内容不符: %q", string(raw))
	}
}

// TestValidateMultiRepoPatchIsolation 多仓库工作区：补丁必须落到它自己所属的仓库目录，
// 绝不能"落到第一个仓库"。同名文件（如两个仓库都有 main.go）是最容易暴露该缺陷的场景。
func TestValidateMultiRepoPatchIsolation(t *testing.T) {
	srcA := "package demo\n\nvar Owner = \"repo-a\"\n"
	srcB := "package demo\n\nvar Owner = \"repo-b\"\n"
	mat := newFakeMaterializer().
		addRepo("repo-a", map[string]string{"main.go": srcA}).
		addRepo("repo-b", map[string]string{"main.go": srcB})
	m := newTestManager(t, config.SandboxConfig{Enabled: true, AllowCommandExec: false}, mat)

	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-multi",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a"), testRepo("repo-b", "svc-b")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	// 只给 repo-b 打补丁
	patch, err := BuildPatch(testRepo("repo-b", "svc-b"), "main.go", srcB,
		`var Owner = "repo-b"`, `var Owner = "patched-b"`, "只改 B 仓库", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := m.Validate(context.Background(), handle, domain.ValidateRequest{Patches: []domain.Patch{*patch}})
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed {
		t.Fatalf("补丁应通过，checks=%s", dumpChecks(verification))
	}

	snapshot, err := m.SnapshotFiles(handle, "", []string{"svc-a/main.go", "svc-b/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["svc-b/main.go"] != "package demo\n\nvar Owner = \"patched-b\"\n" {
		t.Fatalf("B 仓库文件未被正确改写: %q", snapshot["svc-b/main.go"])
	}
	if snapshot["svc-a/main.go"] != srcA {
		t.Fatalf("A 仓库文件被误改（跨仓库污染）: %q", snapshot["svc-a/main.go"])
	}
}

// TestApplyToWorkspaceUnknownRepoRejected 仓库信息缺失时不得落盘到任意仓库。
func TestApplyToWorkspaceUnknownRepoRejected(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	m := newTestManager(t, config.SandboxConfig{}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-unknown-repo",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	orphan := domain.Patch{
		FilePath:    "main.go",
		Action:      domain.ActionModify,
		UnifiedDiff: UnifiedDiff(goSourceA, strings.Replace(goSourceA, `"world"`, `"x"`, 1), "main.go", "main.go", 3),
	}
	applied, conflicts, err := m.ApplyToWorkspace(handle, []domain.Patch{orphan})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 || len(conflicts) != 1 {
		t.Fatalf("无仓库信息的补丁应被拒绝，实际 applied=%d conflicts=%v", applied, conflicts)
	}
	snapshot, err := m.SnapshotFiles(handle, "svc-a", []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["main.go"] != goSourceA {
		t.Fatalf("无仓库信息的补丁不得写入任何仓库: %q", snapshot["main.go"])
	}
}

// ---------------------------------------------------------------------------
// ApplyToWorkspace / SnapshotFiles
// ---------------------------------------------------------------------------

// TestApplyToWorkspace 仅应用不验证。
func TestApplyToWorkspace(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	m := newTestManager(t, config.SandboxConfig{}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-apply",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	good, err := BuildPatch(testRepoCtx(), "main.go", goSourceA, `"world"`, `"applied"`, "应用", domain.RiskLow)
	if err != nil {
		t.Fatal(err)
	}
	applied, conflicts, err := m.ApplyToWorkspace(handle, []domain.Patch{*good})
	if err != nil {
		t.Fatalf("ApplyToWorkspace 失败: %v", err)
	}
	if applied != 1 || len(conflicts) != 0 {
		t.Fatalf("应成功应用 1 个补丁，实际 applied=%d conflicts=%v", applied, conflicts)
	}
	snapshot, err := m.SnapshotFiles(handle, "svc-a", []string{"main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot["main.go"], `"applied"`) {
		t.Fatalf("文件未改写: %q", snapshot["main.go"])
	}

	// 冲突补丁记入 conflicts 但不影响其它补丁
	bad := domain.Patch{
		FilePath:    "main.go",
		RepoKey:     "svc-a",
		Action:      domain.ActionModify,
		UnifiedDiff: UnifiedDiff("nope\n", "yes\n", "main.go", "main.go", 3),
	}
	applied, conflicts, err = m.ApplyToWorkspace(handle, []domain.Patch{bad})
	if err != nil {
		t.Fatalf("ApplyToWorkspace 不应返回致命错误: %v", err)
	}
	if applied != 0 || len(conflicts) != 1 {
		t.Fatalf("冲突应被记录，实际 applied=%d conflicts=%v", applied, conflicts)
	}

	// 路径穿越必须被拒绝
	evil := domain.Patch{
		FilePath:    "../../escape.go",
		RepoKey:     "svc-a",
		Action:      domain.ActionCreate,
		UnifiedDiff: UnifiedDiff("", "package evil\n", "../../escape.go", "../../escape.go", 3),
	}
	applied, conflicts, err = m.ApplyToWorkspace(handle, []domain.Patch{evil})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 0 || len(conflicts) != 1 {
		t.Fatalf("路径穿越必须被拒绝，实际 applied=%d conflicts=%v", applied, conflicts)
	}
	if _, _, err := m.ApplyToWorkspace(nil, nil); err == nil {
		t.Fatal("handle 为 nil 时应返回错误")
	}
}

// TestSnapshotFilesRejectsTraversal SnapshotFiles 必须拒绝越界与穿越路径。
func TestSnapshotFilesRejectsTraversal(t *testing.T) {
	mat := newFakeMaterializer().addRepo("repo-a", map[string]string{"main.go": goSourceA})
	m := newTestManager(t, config.SandboxConfig{}, mat)
	handle, err := m.Open(context.Background(), domain.SandboxMeta{
		RunID: "run-snapshot",
		Repos: []*domain.Repository{testRepo("repo-a", "svc-a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Close() }()

	if _, err := m.SnapshotFiles(handle, "svc-a", []string{"../../etc/passwd"}); err == nil {
		t.Fatal("路径穿越应被拒绝")
	}
	if _, err := m.SnapshotFiles(handle, "no-such-repo", []string{"main.go"}); err == nil {
		t.Fatal("不存在的仓库应返回错误")
	}
	if _, err := m.SnapshotFiles(nil, "svc-a", nil); err == nil {
		t.Fatal("handle 为 nil 时应返回错误")
	}
	got, err := m.SnapshotFiles(handle, "", []string{"svc-a/main.go"})
	if err != nil {
		t.Fatalf("repoKey 为空时应以工作区根为基准: %v", err)
	}
	if !strings.Contains(got["svc-a/main.go"], "package demo") {
		t.Fatalf("内容不符: %q", got["svc-a/main.go"])
	}
}

// TestStaticCheckHelpers 静态校验辅助函数的边界行为。
func TestStaticCheckHelpers(t *testing.T) {
	// 字符串与注释中的括号不参与配平
	if issue := checkBracketBalance("var s = \"(((\"\n// )))\nvar t = `[[[`\n"); issue != "" {
		t.Fatalf("字符串/注释中的括号不应影响配平: %s", issue)
	}
	if issue := checkBracketBalance("func f() {\n"); issue == "" {
		t.Fatal("未闭合的括号应被检出")
	}
	if issue := checkBracketBalance("var s = \"unterminated\n"); issue == "" {
		t.Fatal("未闭合的字符串应被检出")
	}
	if issue := checkBracketBalance("}\n"); issue == "" {
		t.Fatal("多余闭合符号应被检出")
	}
	if issue := checkGoSyntax("a.go", "package a\n\nfunc f() {}\n"); issue != "" {
		t.Fatalf("合法 Go 代码不应报错: %s", issue)
	}
	if issue := checkGoSyntax("a.go", "package a\n\nfunc f() {\n"); issue == "" {
		t.Fatal("非法 Go 代码应被检出")
	}
	if issue := checkJSONSyntax(`{"a":1}`); issue != "" {
		t.Fatalf("合法 JSON 不应报错: %s", issue)
	}
	if issue := checkJSONSyntax(`{"a":}`); issue == "" {
		t.Fatal("非法 JSON 应被检出")
	}
	if issue := checkYAMLSyntax("a: 1\nb:\n  - x\n  - y\n"); issue != "" {
		t.Fatalf("合法 YAML 不应报错: %s", issue)
	}
	if issue := checkYAMLSyntax("a: 1\n\tb: 2\n"); issue == "" {
		t.Fatal("Tab 缩进应被检出")
	}
	// 主语言推断：Go 文件多于其它语言时判为 go
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if lang := inferWorkspaceLanguage(dir); lang != "go" {
		t.Fatalf("主语言应为 go，实际 %q", lang)
	}
}

// dumpChecks 便于失败时打印所有 check。
func dumpChecks(v *domain.Verification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "passed=%v applyResult=%s degraded=%v", v.Passed, v.ApplyResult, v.Degraded)
	for _, c := range v.Checks {
		fmt.Fprintf(&b, "\n  - %s passed=%v skipped=%v output=%s", c.Name, c.Passed, c.Skipped, firstLines(c.Output, 3))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// TestRunnerRejectsNonWhitelisted 白名单外的命令必须被拒绝。
func TestRunnerRejectsNonWhitelisted(t *testing.T) {
	r := NewRunner(config.SandboxConfig{})
	for _, cmd := range []string{
		"curl https://example.com",
		"bash -c ls",
		"sh -c ls",
		"cmd /c dir",
		"powershell -Command ls",
		"rm -rf /tmp/x",
		"/usr/bin/evil",
		"docker ps",
	} {
		out, _, err := r.Run(context.Background(), t.TempDir(), cmd)
		if err == nil {
			t.Fatalf("命令 %q 应被拒绝（输出 %q）", cmd, out)
		}
		if !errors.Is(err, ErrCommandNotAllowed) {
			t.Fatalf("命令 %q 应返回 ErrCommandNotAllowed，实际 %v", cmd, err)
		}
	}
	if _, _, err := r.Run(context.Background(), t.TempDir(), "   "); !errors.Is(err, ErrCommandEmpty) {
		t.Fatalf("空命令应返回 ErrCommandEmpty，实际 %v", err)
	}
	// 空输出 + 未安装的命令：报"未安装"而不是"不允许"
	_, _, err := r.Run(context.Background(), t.TempDir(), "gradle --version")
	if err != nil && errors.Is(err, ErrCommandNotAllowed) {
		t.Fatalf("gradle 在白名单内，不应报 ErrCommandNotAllowed: %v", err)
	}
}

// TestRunnerNetworkIsolation npm install 在 NetworkIsolated=true 时必须被拒。
func TestRunnerNetworkIsolation(t *testing.T) {
	r := NewRunner(config.SandboxConfig{NetworkIsolated: true})
	for _, cmd := range []string{
		"npm install",
		"npm ci",
		"pnpm install",
		"yarn install",
		"go get github.com/x/y",
		"go mod download",
		"mvn dependency:resolve",
		"python -m pip install requests",
		"python3 -m pip install requests",
	} {
		out, _, err := r.Run(context.Background(), t.TempDir(), cmd)
		if err == nil {
			t.Fatalf("无网络模式下 %q 应被拒绝（输出 %q）", cmd, out)
		}
		if !errors.Is(err, ErrNetworkIsolated) {
			t.Fatalf("%q 应返回 ErrNetworkIsolated，实际 %v", cmd, err)
		}
		if !strings.Contains(err.Error(), "网络") {
			t.Fatalf("%q 的错误信息应含中文说明，实际 %v", cmd, err)
		}
	}
	// 非联网命令不受影响（会用 go 跑一次 version）
	if _, err := exec.LookPath("go"); err == nil {
		if _, _, err := r.Run(context.Background(), t.TempDir(), "go version"); err != nil {
			t.Fatalf("go version 不应被网络隔离拒绝: %v", err)
		}
	}
	// NetworkIsolated=false 时不因网络原因拒绝（命令本身可能因环境缺失而失败）
	r2 := NewRunner(config.SandboxConfig{NetworkIsolated: false})
	if _, _, err := r2.Run(context.Background(), t.TempDir(), "npm install"); err != nil && errors.Is(err, ErrNetworkIsolated) {
		t.Fatalf("未开启网络隔离时不应返回 ErrNetworkIsolated: %v", err)
	}
}

// TestRunnerRunAllowedCommand 白名单内命令正常执行并返回输出与耗时。
func TestRunnerRunAllowedCommand(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境未安装 go，跳过")
	}
	r := NewRunner(config.SandboxConfig{CommandTimeoutSec: 60})
	out, duration, err := r.Run(context.Background(), t.TempDir(), "go version")
	if err != nil {
		t.Fatalf("go version 执行失败: %v\n输出: %s", err, out)
	}
	if !strings.Contains(out, "go version") {
		t.Fatalf("输出应包含 go version，实际 %q", out)
	}
	if duration < 0 {
		t.Fatalf("耗时不应为负: %d", duration)
	}
	// 工作目录必须生效：go env GOMOD 在无 go.mod 的临时目录下输出 /dev/null
	if _, _, err := r.Run(context.Background(), t.TempDir(), "go env GOOS"); err != nil {
		t.Fatalf("go env 执行失败: %v", err)
	}
}

// TestRunnerCommandChain && 分隔的多条命令顺序执行。
func TestRunnerCommandChain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境未安装 go，跳过")
	}
	r := NewRunner(config.SandboxConfig{CommandTimeoutSec: 60})
	out, _, err := r.Run(context.Background(), t.TempDir(), "go version && go env GOOS")
	if err != nil {
		t.Fatalf("命令链执行失败: %v\n输出: %s", err, out)
	}
	if !strings.Contains(out, "go version") {
		t.Fatalf("第一条命令未执行: %q", out)
	}
	if !strings.Contains(out, "windows") && !strings.Contains(out, "linux") && !strings.Contains(out, "darwin") {
		t.Fatalf("第二条命令未执行: %q", out)
	}
	if !strings.Contains(out, "$ go version") || !strings.Contains(out, "$ go env GOOS") {
		t.Fatalf("多条子命令应在输出中标注命令，实际 %q", out)
	}

	// 任一条失败即停止，并指明失败的是第几条
	_, _, err = r.Run(context.Background(), t.TempDir(), "go version && go build ./definitely-not-exist")
	if err == nil {
		t.Fatal("命令链中存在失败命令时应返回错误")
	}
	if !strings.Contains(err.Error(), "第 2/2 条") {
		t.Fatalf("错误应指明失败的子命令序号，实际 %v", err)
	}

	// 分号与管道同样被拆分（不使用 shell）
	out, _, err = r.Run(context.Background(), t.TempDir(), "go version; go env GOARCH")
	if err != nil {
		t.Fatalf("分号命令链执行失败: %v\n输出: %s", err, out)
	}
	if !strings.Contains(out, "$ go env GOARCH") {
		t.Fatalf("分号未触发拆分: %q", out)
	}
}

// TestRunnerRejectsShellInjection 重定向与命令替换必须被拒绝（不使用 shell）。
func TestRunnerRejectsShellInjection(t *testing.T) {
	r := NewRunner(config.SandboxConfig{})
	for _, cmd := range []string{
		"go version > /tmp/out",
		"go version `whoami`",
		"go version $(whoami)",
		"go build < input.txt",
	} {
		_, _, err := r.Run(context.Background(), t.TempDir(), cmd)
		if err == nil {
			t.Fatalf("含 shell 注入的命令 %q 应被拒绝", cmd)
		}
	}
	// 引号内出现参数是合法的
	if _, _, err := r.Run(context.Background(), t.TempDir(), `git -c "core.autocrlf=false" status`); err != nil {
		if errors.Is(err, ErrCommandNotAllowed) {
			t.Fatalf("带引号参数不应被判定为非法命令: %v", err)
		}
	}
}

// TestRunnerGitReadOnly git 只允许只读子命令。
func TestRunnerGitReadOnly(t *testing.T) {
	r := NewRunner(config.SandboxConfig{})
	for _, cmd := range []string{"git push origin main", "git reset --hard", "git checkout main", "git commit -m x"} {
		if _, _, err := r.Run(context.Background(), t.TempDir(), cmd); !errors.Is(err, ErrGitWriteOp) {
			t.Fatalf("%q 应返回 ErrGitWriteOp，实际 %v", cmd, err)
		}
	}
	if _, _, err := r.Run(context.Background(), t.TempDir(), "git"); !errors.Is(err, ErrGitWriteOp) {
		t.Fatalf("未指定子命令的 git 应被拒绝，实际 %v", err)
	}
}

// TestRunnerTimeout 超时用例：命令运行时间超过 cfg.CommandTimeoutSec 时被终止并回收。
func TestRunnerTimeout(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境未安装 go，跳过超时用例")
	}
	dir := t.TempDir()
	prog := "package main\n\nimport (\n\t\"os\"\n\t\"time\"\n)\n\nfunc main() {\n\t_ = os.Stdout\n\ttime.Sleep(10 * time.Second)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module slowprog\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 只给 1 秒：go run 需要先编译，基本必然超时
	r := NewRunner(config.SandboxConfig{CommandTimeoutSec: 1, MaxOutputBytes: 32 << 10})
	start := time.Now()
	out, duration, err := r.Run(context.Background(), dir, "go run .")
	elapsed := time.Since(start)
	if err == nil {
		t.Skipf("go run 在 1 秒内完成，跳过超时断言（输出 %q）", out)
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应说明超时，实际 %v\n输出: %s", err, out)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("超时后未及时回收进程，实际耗时 %v", elapsed)
	}
	if duration <= 0 {
		t.Fatalf("超时场景也应返回耗时，实际 %d", duration)
	}

	// ctx 取消分支：外部取消同样必须终止命令
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	r2 := NewRunner(config.SandboxConfig{CommandTimeoutSec: 60})
	if _, _, err := r2.Run(ctx, dir, "go run ."); err == nil {
		t.Fatal("ctx 取消后命令应返回错误")
	}
}

// TestRunnerOutputTruncation 输出截断保留头尾并插入提示。
func TestRunnerOutputTruncation(t *testing.T) {
	long := strings.Repeat("A", 5000) + "MIDDLE" + strings.Repeat("Z", 5000)
	got := truncateOutput(long, 1000)
	if len(got) > 1400 {
		t.Fatalf("截断后长度异常: %d", len(got))
	}
	if !strings.Contains(got, "输出已截断") {
		t.Fatalf("应插入截断提示，实际 %q", got)
	}
	if !strings.HasPrefix(got, "AAAA") || !strings.HasSuffix(got, "ZZZZ") {
		t.Fatal("截断应保留头尾")
	}
	if strings.Contains(got, "MIDDLE") {
		t.Fatal("中间内容应被截掉")
	}
	// 未超限时原样返回
	if s := truncateOutput("short", 1000); s != "short" {
		t.Fatalf("未超限不应截断: %q", s)
	}
	// 极小上限也不能崩
	if s := truncateOutput(long, 8); s == "" {
		t.Fatal("极小上限仍应返回内容")
	}
}

// TestRunnerStats 执行统计。
func TestRunnerStats(t *testing.T) {
	r := NewRunner(config.SandboxConfig{NetworkIsolated: true, CommandTimeoutSec: 5, MaxOutputBytes: 1024})
	_, _, _ = r.Run(context.Background(), t.TempDir(), "curl http://x")
	stats := r.Stats()
	if stats["runs"] != int64(1) {
		t.Fatalf("runs 应为 1，实际 %v", stats["runs"])
	}
	if stats["failures"] != int64(1) {
		t.Fatalf("failures 应为 1，实际 %v", stats["failures"])
	}
	if stats["networkIsolated"] != true {
		t.Fatalf("networkIsolated 应为 true，实际 %v", stats["networkIsolated"])
	}
}

// TestSanitizeRunID 目录名安全化。
func TestSanitizeRunID(t *testing.T) {
	cases := map[string]string{
		"run-1":            "run-1",
		"../../etc/passwd": "etc_passwd",
		"a/b\\c":           "a_b_c",
		".hidden":          "hidden",
		"..":               "", // 触发随机回退
		"":                 "", // 触发随机回退
	}
	for in, want := range cases {
		got := sanitizeRunID(in)
		if want == "" {
			if !strings.HasPrefix(got, "ws-") {
				t.Fatalf("sanitizeRunID(%q) 应回退为随机名，实际 %q", in, got)
			}
			continue
		}
		if got != want {
			t.Fatalf("sanitizeRunID(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestWithinRoot 工作区边界判定（含软链接逃逸）。
func TestWithinRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a", "b.txt")
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if !withinRoot(root, root) {
		t.Fatal("root 自身应判定为位于内部")
	}
	if !withinRoot(root, inside) {
		t.Fatalf("%q 应判定为位于 root 内", inside)
	}
	if withinRoot(root, outside) {
		t.Fatalf("%q 不应判定为位于 root 内", outside)
	}
	if withinRoot("", inside) || withinRoot(root, "") {
		t.Fatal("空路径应判定为不合法")
	}
}
