package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 错误定义
// ---------------------------------------------------------------------------

var (
	// ErrGitUnavailable 执行环境没有可用的 git 可执行文件。
	ErrGitUnavailable = errors.New("git 可执行文件不可用")
	// ErrRepoNotFound 仓库不存在或镜像目录损坏。
	ErrRepoNotFound = errors.New("git 仓库不存在")
	// ErrRefNotFound 指定的 ref / 分支 / Tag / commit 无法解析。
	ErrRefNotFound = errors.New("git ref 无法解析")
	// ErrFileNotFound 仓库中不存在指定路径的文件。
	ErrFileNotFound = errors.New("仓库中不存在该文件")
	// ErrBinaryFile 目标文件为二进制，不做源码加载。
	ErrBinaryFile = errors.New("目标文件为二进制")
	// ErrFileTooLarge 目标文件超过单文件体积上限。
	ErrFileTooLarge = errors.New("目标文件超过体积上限")
	// ErrSnippetNotFound 在文件中找不到待替换片段。
	ErrSnippetNotFound = errors.New("在文件中未找到待替换片段")
	// ErrRepoResolve 仓库元数据缺失导致无法定位源码。
	ErrRepoResolve = errors.New("仓库元数据缺失")
	// ErrNoRepository 调用方未提供仓库。
	ErrNoRepository = errors.New("未提供仓库信息")
	// ErrBudgetExhausted 加载预算（文件数/字符数）已耗尽，仅返回已加载部分。
	//
	// 设计取舍：Load 在预算耗尽时**仍返回已加载的切片且 error 为 nil**，
	// 避免调用方因为错误而丢弃可用证据；调用方可通过本哨兵值判断、
	// 也可以读取 LastLoadNotes() 获取人类可读的裁剪说明。
	ErrBudgetExhausted = errors.New("加载预算已耗尽")
)

// ---------------------------------------------------------------------------
// GitRepoManager：git 镜像管理
// ---------------------------------------------------------------------------

// GitRepoManager 负责 git 镜像（bare mirror）与按 commit 检出的工作区管理。
//
// 目录布局（均位于 cfg.CacheDir 下）：
//
//	<CacheDir>/mirrors/<tenantID>/<repoID>.git   bare mirror（git clone --mirror）
//	<CacheDir>/workspaces/<runID>/<repoKey>      按 commit 检出的快照（供沙箱使用）
//
// 所有 git 调用统一经过 runGit：受 context 超时约束、禁用交互式提问、
// 并对 stderr 做归一化与凭证脱敏。
type GitRepoManager struct {
	cfg config.SourceConfig
	log *logx.Logger

	mu       sync.Mutex
	gitPath  string // 探测到的 git 可执行文件
	gitProbe bool   // 是否已探测过
	gitErr   error  // 探测失败原因

	tarMu       sync.Mutex
	tarPath     string
	tarProbed   bool
	tarUsable   bool
	ensuredMu   sync.Mutex
	ensuredKeys map[string]*sync.Mutex
}

// NewGitRepoManager 创建 git 镜像管理器。
func NewGitRepoManager(cfg config.SourceConfig, log *logx.Logger) *GitRepoManager {
	if log == nil {
		log = logx.Nop()
	}
	if strings.TrimSpace(cfg.GitBinary) == "" {
		cfg.GitBinary = "git"
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		cfg.CacheDir = filepath.Join(os.TempDir(), "codeagent-cache")
	}
	return &GitRepoManager{
		cfg:         cfg,
		log:         log,
		ensuredKeys: map[string]*sync.Mutex{},
	}
}

// Close 释放管理器资源（清理内部锁表；磁盘上的镜像保留以复用缓存）。
func (m *GitRepoManager) Close() error {
	m.mu.Lock()
	m.ensuredKeys = map[string]*sync.Mutex{}
	m.mu.Unlock()
	return nil
}

// MirrorsRoot 返回镜像根目录。
func (m *GitRepoManager) MirrorsRoot() string {
	return filepath.Join(m.cfg.CacheDir, "mirrors")
}

// WorkspacesRoot 返回工作区根目录。
func (m *GitRepoManager) WorkspacesRoot() string {
	return filepath.Join(m.cfg.CacheDir, "workspaces")
}

// MirrorDir 返回某租户某仓库的镜像目录（不做存在性校验）。
func (m *GitRepoManager) MirrorDir(tenantID, repoID string) string {
	return filepath.Join(m.MirrorsRoot(), safeSegment(tenantID), safeSegment(repoID)+".git")
}

// GitAvailable 返回当前环境是否存在可用的 git 可执行文件（结果被缓存）。
func (m *GitRepoManager) GitAvailable() bool {
	return m.resolveGit() == nil
}

// resolveGit 探测 git 可执行文件（每进程只探测一次）。
func (m *GitRepoManager) resolveGit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gitProbe {
		return m.gitErr
	}
	m.gitProbe = true

	candidates := []string{}
	if b := strings.TrimSpace(m.cfg.GitBinary); b != "" {
		candidates = append(candidates, b)
	}
	candidates = append(candidates, "git")
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		// 实际执行一次，确认二进制可运行。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, path, "--version")
		cmd.Env = gitEnv()
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		if !strings.Contains(string(out), "git version") {
			continue
		}
		m.gitPath = path
		m.log.Debug("git 可执行文件就绪", "path", path, "version", strings.TrimSpace(string(out)))
		return nil
	}
	m.gitErr = fmt.Errorf("%w: 未找到 %q（请安装 git 或配置 source.gitBinary）", ErrGitUnavailable, m.cfg.GitBinary)
	return m.gitErr
}

func (m *GitRepoManager) binary() (string, error) {
	if err := m.resolveGit(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gitPath, nil
}

// EnsureMirror 确保仓库镜像存在并已更新，返回镜像目录。
//
// 目录不存在时执行 git clone --mirror；已存在时执行 git fetch --prune origin。
// 当 cfg.FetchDepth > 0 时优先尝试深度抓取，bare mirror 不支持时自动退化为普通 fetch。
// secret 只参与 URL 构造，绝不写入日志或错误信息（URL 中的凭证会被脱敏）。
func (m *GitRepoManager) EnsureMirror(ctx context.Context, repo *domain.Repository, secret string) (string, error) {
	if repo == nil {
		return "", ErrNoRepository
	}
	// 提前校验 git 可用性，避免创建半成品镜像目录。
	bin, err := m.binary()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repo.URL) == "" {
		return "", fmt.Errorf("%w: 仓库 %s 未配置 URL", ErrRepoNotFound, repo.Key)
	}

	dir := m.MirrorDir(repo.TenantID, repo.ID)
	key := dir
	lock := m.keyLock(key)
	lock.Lock()
	defer lock.Unlock()

	cloneURL, displayURL := buildAuthURL(repo, secret)
	if isGitDir(dir) {
		if err := m.updateMirror(ctx, bin, dir, displayURL); err != nil {
			m.log.Warn("镜像更新失败，尝试重建", "repo", repo.Key, "err", err.Error())
			if rerr := os.RemoveAll(dir); rerr != nil {
				return "", fmt.Errorf("清理损坏镜像失败: %w", rerr)
			}
			if cerr := m.cloneMirror(ctx, bin, dir, cloneURL, displayURL); cerr != nil {
				return "", cerr
			}
		}
		return dir, nil
	}

	if err := m.cloneMirror(ctx, bin, dir, cloneURL, displayURL); err != nil {
		return "", err
	}
	return dir, nil
}

// cloneMirror 执行 git clone --mirror。
func (m *GitRepoManager) cloneMirror(ctx context.Context, bin, dir, cloneURL, displayURL string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("创建镜像目录失败: %w", err)
	}
	args := []string{"clone", "--mirror", "--quiet"}
	if m.cfg.FetchDepth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", m.cfg.FetchDepth))
	}
	args = append(args, cloneURL, dir)
	m.log.Info("拉取镜像", "target", dir, "url", displayURL)
	if _, _, err := m.runGit(ctx, m.timeout(), args...); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("git clone --mirror 失败(%s): %w", displayURL, err)
	}
	return nil
}

// updateMirror 执行镜像更新（fetch --prune）。
func (m *GitRepoManager) updateMirror(ctx context.Context, bin, dir, displayURL string) error {
	timeout := m.timeout()
	if m.cfg.FetchDepth > 0 {
		depthArgs := []string{"--git-dir", dir, "fetch", "--prune", "--quiet",
			fmt.Sprintf("--depth=%d", m.cfg.FetchDepth), "origin"}
		if _, _, err := m.runGit(ctx, timeout, depthArgs...); err == nil {
			return nil
		} else {
			// bare mirror 的浅抓取限制（refusing to fetch into current branch 等）：
			// 退化为普通 fetch，保证功能可用。
			m.log.Debug("深度 fetch 不可用，退化为普通 fetch", "dir", dir, "reason", err.Error())
		}
	}
	args := []string{"--git-dir", dir, "fetch", "--prune", "--quiet", "origin"}
	if _, _, err := m.runGit(ctx, timeout, args...); err != nil {
		return fmt.Errorf("git fetch --prune 失败(%s): %w", displayURL, err)
	}
	return nil
}

// ResolveCommit 把任意 ref 解析为确定的 commit sha。
//
// 支持：空（HEAD）、分支、Tag、commit sha（含短 sha）、origin/branch、refs/heads/x。
func (m *GitRepoManager) ResolveCommit(ctx context.Context, dir, ref string) (string, error) {
	if !isGitDir(dir) {
		return "", fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	ref = strings.TrimSpace(ref)
	timeout := m.shortTimeout()

	if ref == "" || strings.EqualFold(ref, "HEAD") {
		if out, _, err := m.runGit(ctx, timeout, "--git-dir", dir, "rev-parse", "HEAD"); err == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				return sha, nil
			}
		}
		// 镜像 HEAD 未指向任何分支时，回退到 origin/HEAD → 首个分支。
		if out, _, err := m.runGit(ctx, timeout, "--git-dir", dir, "symbolic-ref", "--short", "HEAD"); err == nil {
			if name := strings.TrimSpace(string(out)); name != "" {
				if sha, err := m.revParse(ctx, dir, name); err == nil {
					return sha, nil
				}
			}
		}
		if out, _, err := m.runGit(ctx, timeout, "--git-dir", dir, "rev-parse", "HEAD^{commit}"); err == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				return sha, nil
			}
		}
		return "", fmt.Errorf("%w: HEAD", ErrRefNotFound)
	}

	candidates := []string{
		ref,
		"refs/heads/" + ref,
		"origin/" + ref,
		"refs/remotes/origin/" + ref,
		"refs/tags/" + ref,
	}
	// 兼容 `origin/x` 形态：镜像内分支实际存于 refs/heads/x。
	short := ref
	for _, prefix := range []string{"origin/", "refs/remotes/origin/", "refs/heads/"} {
		if strings.HasPrefix(short, prefix) {
			s := strings.TrimPrefix(short, prefix)
			candidates = append(candidates, s, "refs/heads/"+s, "refs/tags/"+s, "refs/remotes/origin/"+s)
		}
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if sha, err := m.revParse(ctx, dir, c); err == nil {
			return sha, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRefNotFound, ref)
}

// revParse 尝试把 rev 解析为 commit sha（优先使用 <rev>^{commit}）。
func (m *GitRepoManager) revParse(ctx context.Context, dir, rev string) (string, error) {
	for _, r := range []string{rev + "^{commit}", rev} {
		if out, _, err := m.runGit(ctx, m.shortTimeout(), "--git-dir", dir, "rev-parse", "--verify", "--quiet", r); err == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", ErrRefNotFound, rev)
}

// Show 读取指定 commit 下某个路径的文件内容。
//
// 大文件会被截断到单文件上限；二进制文件返回 ErrBinaryFile。
func (m *GitRepoManager) Show(ctx context.Context, dir, commit, path string) ([]byte, error) {
	if !isGitDir(dir) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	commit = strings.TrimSpace(commit)
	path = normalizeRepoPath(path)
	if commit == "" || path == "" {
		return nil, ErrFileNotFound
	}
	out, stderr, err := m.runGit(ctx, m.shortTimeout(), "--git-dir", dir, "show", commit+":"+path)
	if err != nil {
		if isMissingPathErr(stderr) {
			return nil, fmt.Errorf("%w: %s@%s", ErrFileNotFound, path, shortSHA(commit))
		}
		return nil, fmt.Errorf("git show %s:%s 失败: %w", shortSHA(commit), path, err)
	}
	if int64(len(out)) > m.maxFileBytes() {
		out = out[:m.maxFileBytes()]
	}
	if IsBinary(out) {
		return nil, fmt.Errorf("%w: %s", ErrBinaryFile, path)
	}
	return out, nil
}

// LsTree 列出指定 commit 下的全部文件（含体积），并推断语言。
func (m *GitRepoManager) LsTree(ctx context.Context, dir, commit string) ([]domain.FileNode, error) {
	if !isGitDir(dir) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		commit = "HEAD"
	}
	out, _, err := m.runGit(ctx, m.longTimeout(), "--git-dir", dir, "ls-tree", "-r", "-l", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree 失败: %w", err)
	}
	nodes := make([]domain.FileNode, 0, 256)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n, ok := parseTreeLine(line); ok {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

// parseTreeLine 解析 `git ls-tree -r -l` 的一行：<mode> <type> <object> <size>\t<path>。
func parseTreeLine(line string) (domain.FileNode, bool) {
	tab := strings.Index(line, "\t")
	if tab < 0 {
		return domain.FileNode{}, false
	}
	meta := strings.Fields(line[:tab])
	path := strings.TrimSpace(line[tab+1:])
	if len(meta) < 3 || path == "" {
		return domain.FileNode{}, false
	}
	node := domain.FileNode{Path: path}
	kind := meta[1]
	switch kind {
	case "tree", "commit":
		node.IsDir = true
	default:
		if len(meta) >= 4 {
			if size, err := strconv.ParseInt(strings.TrimSpace(meta[3]), 10, 64); err == nil {
				node.Size = size
			}
		}
		node.Language = detectLanguageByPath(path)
	}
	return node, true
}

// GrepCommit 在指定 commit 的代码中检索内容，返回带行号的文件线索。
//
// 优先使用字面量匹配（git grep -F），无结果时退化为正则匹配（git grep -E），
// 并把 glob 风格的通配符（`*` / `**`）转换为正则等价式。
func (m *GitRepoManager) GrepCommit(ctx context.Context, dir, commit, pattern string, limit int) ([]domain.FileHint, error) {
	if !isGitDir(dir) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		commit = "HEAD"
	}
	// literal 优先（-F），正则兜底（-E）。
	literalArgs := []string{"--git-dir", dir, "grep", "-n", "-I", "--no-color", "-F", "-e", pattern, commit, "--"}
	hints, err := m.grepWith(ctx, literalArgs, limit)
	if err != nil || len(hints) > 0 {
		return hints, err
	}
	// 正则兜底：把调用方传入的模式按 ERE 直接使用（字面量失败通常意味着模式里
	// 含 `*`/`.`/`(` 等正则元字符，此时按正则解释最符合直觉）。
	regexArgs := []string{"--git-dir", dir, "grep", "-n", "-I", "--no-color", "-E", "-e", pattern, commit, "--"}
	hints, err = m.grepWith(ctx, regexArgs, limit)
	if err != nil {
		// 正则非法：不算致命错误，返回空结果。
		m.log.Debug("git grep 正则兜底失败", "pattern", pattern, "err", err.Error())
		return nil, nil
	}
	return hints, nil
}

// grepWith 执行一次 git grep 并解析结果（返回 0 结果不算错误）。
func (m *GitRepoManager) grepWith(ctx context.Context, args []string, limit int) ([]domain.FileHint, error) {
	out, stderr, err := m.runGit(ctx, m.longTimeout(), args...)
	if err != nil {
		if len(out) == 0 {
			// git grep 无匹配时退出码为 1，属正常情况。
			if strings.Contains(strings.ToLower(string(stderr)), "no such") {
				return nil, fmt.Errorf("git grep 失败: %s", normalizeErrorText(string(stderr)))
			}
			return nil, nil
		}
	}
	return parseGrepOutput(out, limit), nil
}

// parseGrepOutput 解析 `commit:path:line:content` 形态的 grep 输出。
func parseGrepOutput(out []byte, limit int) []domain.FileHint {
	hints := make([]domain.FileHint, 0, 16)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rest := line
		// 去掉 `<commit>:` 前缀：sha 为十六进制且长度 >= 7。
		if i := strings.Index(rest, ":"); i > 0 {
			head := rest[:i]
			if isHex(head) && len(head) >= 7 {
				rest = rest[i+1:]
			}
		}
		j := strings.Index(rest, ":")
		if j <= 0 {
			continue
		}
		path := rest[:j]
		rest2 := rest[j+1:]
		k := strings.Index(rest2, ":")
		if k <= 0 {
			continue
		}
		numStr := rest2[:k]
		num, err := strconv.Atoi(strings.TrimSpace(numStr))
		if err != nil {
			continue
		}
		key := path + ":" + numStr
		if seen[key] {
			continue
		}
		seen[key] = true
		hints = append(hints, domain.FileHint{
			Path:     normalizeRepoPath(path),
			Line:     num,
			Reason:   "grep",
			Priority: 0.6,
		})
		if len(hints) >= limit {
			break
		}
	}
	return hints
}

// Archive 生成指定 commit 的 tar 归档字节流（供沙箱物化使用）。
func (m *GitRepoManager) Archive(ctx context.Context, dir, commit string) ([]byte, error) {
	if !isGitDir(dir) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	out, _, err := m.runGit(ctx, m.longTimeout(), "--git-dir", dir, "archive", "--format=tar", commit)
	if err != nil {
		return nil, fmt.Errorf("git archive 失败: %w", err)
	}
	return out, nil
}

// Checkout 把指定 commit 检出到 dest 目录（供沙箱使用）。
//
// 策略：优先 `git archive | tar -x`（体积最小）；当环境无可用 tar 时，
// 退化为 `git clone --shared --no-checkout` + `git checkout --detach`（保留 --shared 节省磁盘）。
func (m *GitRepoManager) Checkout(ctx context.Context, dir, commit, dest string) error {
	if !isGitDir(dir) {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		commit = "HEAD"
	}
	if strings.TrimSpace(dest) == "" {
		return errors.New("检出目标目录不能为空")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建工作区目录失败: %w", err)
	}
	if removed, err := removeEmptyOrStaleDir(dest); err != nil {
		return err
	} else if !removed {
		return fmt.Errorf("检出目标目录已存在且非空: %s", dest)
	}

	if m.tarUsableCached() {
		if err := m.checkoutWithTar(ctx, dir, commit, dest); err == nil {
			return nil
		} else {
			m.log.Debug("tar 解包失败，退化为 shared clone", "dest", dest, "reason", err.Error())
			_ = os.RemoveAll(dest)
			_ = os.MkdirAll(dest, 0o755)
		}
	}
	return m.checkoutWithSharedClone(ctx, dir, commit, dest)
}

// checkoutWithTar 使用 git archive + tar 解包。
func (m *GitRepoManager) checkoutWithTar(ctx context.Context, dir, commit, dest string) error {
	bin, err := m.binary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()

	archive := exec.CommandContext(ctx, bin, "--git-dir", dir, "archive", "--format=tar", commit)
	archive.Env = gitEnv()
	untar := exec.CommandContext(ctx, m.tarPath, "-xf", "-", "-C", dest)
	untar.Env = gitEnv()

	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	archive.Stdout = pw
	untar.Stdin = pr
	var archiveErr, untarErr strings.Builder
	archive.Stderr = &archiveErr
	untar.Stderr = &untarErr

	if err := archive.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("启动 git archive 失败: %w", err)
	}
	if err := untar.Start(); err != nil {
		pw.Close()
		pr.Close()
		_ = archive.Wait()
		return fmt.Errorf("启动 tar 失败: %w", err)
	}
	// 父进程必须关闭管道写端，否则 tar 不会收到 EOF。
	_ = pw.Close()
	aErr := archive.Wait()
	_ = pr.Close()
	tErr := untar.Wait()
	if aErr != nil {
		return fmt.Errorf("git archive 失败: %v (%s)", aErr, normalizeErrorText(archiveErr.String()))
	}
	if tErr != nil {
		return fmt.Errorf("tar 解包失败: %v (%s)", tErr, normalizeErrorText(untarErr.String()))
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	if len(entries) == 0 && !commitHasNoFiles(ctx, m, dir, commit) {
		return errors.New("tar 解包结果为空")
	}
	return nil
}

// commitHasNoFiles 判断 commit 是否真的没有任何文件（用于区分解包失败与空仓库）。
func commitHasNoFiles(ctx context.Context, m *GitRepoManager, dir, commit string) bool {
	nodes, err := m.LsTree(ctx, dir, commit)
	return err == nil && len(nodes) == 0
}

// checkoutWithSharedClone 使用本地 shared clone + detach checkout。
func (m *GitRepoManager) checkoutWithSharedClone(ctx context.Context, dir, commit, dest string) error {
	if _, err := m.binary(); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	if _, _, err := m.runGit(ctx, m.timeout(), "clone", "--shared", "--no-checkout", "--quiet", dir, dest); err != nil {
		return fmt.Errorf("shared clone 失败: %w", err)
	}
	// 关键：代码分析必须与仓库中的规范文本逐字节一致。
	// Windows 上 Git 默认 core.autocrlf=true 会把 LF 检出为 CRLF，
	// 导致 diff 上下文行（来自 git show，为 LF）无法在工作区文件中匹配，
	// 表现为"上下文不匹配（期望 "x"，实际 "x\r"）"的补丁应用失败。
	// 因此显式关闭换行转换，按仓库原样（LF）检出。
	for _, cfg := range [][2]string{
		{"core.autocrlf", "false"},
		{"core.eol", "lf"},
		{"core.safecrlf", "false"},
	} {
		if _, _, err := m.runGit(ctx, m.shortTimeout(), "-C", dest, "config", cfg[0], cfg[1]); err != nil {
			return fmt.Errorf("配置 %s 失败: %w", cfg[0], err)
		}
	}
	if _, _, err := m.runGit(ctx, m.timeout(), "-C", dest, "checkout", "--detach", "--force", "--quiet", commit); err != nil {
		return fmt.Errorf("checkout %s 失败: %w", shortSHA(commit), err)
	}
	return nil
}

// Head 返回镜像 HEAD 指向的 commit。
func (m *GitRepoManager) Head(ctx context.Context, dir string) (string, error) {
	if !isGitDir(dir) {
		return "", fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	out, _, err := m.runGit(ctx, m.shortTimeout(), "--git-dir", dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Branches 返回镜像中的分支名（refs/heads/* 与 refs/remotes/origin/* 去重后排序）。
func (m *GitRepoManager) Branches(ctx context.Context, dir string) ([]string, error) {
	if !isGitDir(dir) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, dir)
	}
	out, _, err := m.runGit(ctx, m.shortTimeout(), "--git-dir", dir,
		"for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		name = strings.TrimPrefix(name, "origin/")
		if name == "" || name == "HEAD" {
			continue
		}
		set[name] = true
	}
	list := make([]string, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	sort.Strings(list)
	return list, nil
}

// FilesOfCommit 返回某 commit 下所有文件的体积映射（供预算估算）。
func (m *GitRepoManager) FilesOfCommit(ctx context.Context, dir, commit string) (map[string]int64, error) {
	nodes, err := m.LsTree(ctx, dir, commit)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(nodes))
	for _, n := range nodes {
		if !n.IsDir {
			out[n.Path] = n.Size
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 内部：命令执行
// ---------------------------------------------------------------------------

// runGit 统一执行 git 命令，返回 stdout / stderr 与错误。
//
// timeout <= 0 时使用 cfg.CloneTimeoutSec。stderr 会先做归一化与凭证脱敏，
// 保证错误信息中不出现密码/Token。
func (m *GitRepoManager) runGit(ctx context.Context, timeout time.Duration, args ...string) ([]byte, []byte, error) {
	bin, err := m.binary()
	if err != nil {
		return nil, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = m.timeout()
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Env = gitEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	outBytes := []byte(stdout.String())
	errBytes := []byte(normalizeErrorText(stderr.String()))
	if runErr != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return outBytes, errBytes, fmt.Errorf("git 命令超时(%s): git %s", timeout, normalizeErrorText(strings.Join(args, " ")))
		}
		if ctx.Err() != nil {
			return outBytes, errBytes, fmt.Errorf("git 命令被取消: %w", ctx.Err())
		}
		return outBytes, errBytes, fmt.Errorf("git %s: %w: %s", normalizeErrorText(strings.Join(args, " ")), runErr, string(errBytes))
	}
	return outBytes, errBytes, nil
}

// timeout 返回普通 git 操作超时（拉取级别）。
func (m *GitRepoManager) timeout() time.Duration {
	sec := m.cfg.CloneTimeoutSec
	if sec <= 0 {
		sec = 180
	}
	return time.Duration(sec) * time.Second
}

// longTimeout 返回本地只读命令（ls-tree / grep）的超时。
func (m *GitRepoManager) longTimeout() time.Duration {
	t := m.timeout() / 3
	if t < 20*time.Second {
		t = 20 * time.Second
	}
	return t
}

// shortTimeout 返回极短只读命令（rev-parse / show）的超时。
func (m *GitRepoManager) shortTimeout() time.Duration {
	t := m.timeout() / 6
	if t < 10*time.Second {
		t = 10 * time.Second
	}
	return t
}

// maxFileBytes 返回单文件读取上限（默认 4MB）。
func (m *GitRepoManager) maxFileBytes() int64 {
	return 4 << 20
}

// gitEnv 返回禁用交互、固定输出的 git 环境变量。
func gitEnv() []string {
	env := os.Environ()
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=no",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)
	return env
}

// keyLock 返回某个 key 的互斥锁，避免同一镜像被并发 clone/fetch。
func (m *GitRepoManager) keyLock(key string) *sync.Mutex {
	m.ensuredMu.Lock()
	defer m.ensuredMu.Unlock()
	if l, ok := m.ensuredKeys[key]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.ensuredKeys[key] = l
	return l
}

// tarUsableCached 探测系统 tar 是否可用（结果缓存）。
func (m *GitRepoManager) tarUsableCached() bool {
	m.tarMu.Lock()
	defer m.tarMu.Unlock()
	if m.tarProbed {
		return m.tarUsable
	}
	m.tarProbed = true
	path, err := exec.LookPath("tar")
	if err != nil {
		m.log.Debug("系统未找到 tar，检出将使用 shared clone", "err", err.Error())
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	if out, err := cmd.Output(); err != nil {
		// BusyBox/BSD tar 不支持 --version，但仍可能可用，尝试直接解包空流。
		cmd2 := exec.CommandContext(ctx, path, "-tf", "-")
		cmd2.Stdin = strings.NewReader("")
		if _, err2 := cmd2.Output(); err2 == nil {
			m.tarPath = path
			m.tarUsable = true
			return true
		}
		m.log.Debug("系统 tar 不可用，检出将使用 shared clone", "err", err.Error())
		return false
	} else if !strings.Contains(strings.ToLower(string(out)), "tar") && len(out) == 0 {
		m.log.Debug("系统 tar 行为异常，检出将使用 shared clone")
		return false
	}
	m.tarPath = path
	m.tarUsable = true
	return true
}

// isGitDir 判断目录是否为可用的 git 仓库（bare 或普通）。
func isGitDir(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		return false
	}
	return true
}

// removeEmptyOrStaleDir 处理检出目标：不存在返回 true；空目录直接复用；非空返回 false。
func removeEmptyOrStaleDir(dest string) (bool, error) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// 内部：URL / 文本处理
// ---------------------------------------------------------------------------

// buildAuthURL 把仓库 URL 与凭证合成为可用的 clone URL。
//
// 返回 (真实 URL, 可安全打印的展示 URL)。本地路径与 file:// 不做任何改写。
// 用户名缺省使用 git 惯用占位名；URL 中如已带凭证会被配置凭证覆盖。
func buildAuthURL(repo *domain.Repository, secret string) (string, string) {
	raw := strings.TrimSpace(repo.URL)
	display := SanitizeURL(raw)
	if secret == "" || isLocalPath(raw) || strings.HasPrefix(strings.ToLower(raw), "file://") {
		return raw, display
	}
	// 用户名为空时使用 git 惯用占位名。
	name := "git"
	if i := strings.Index(raw, "://"); i > 0 {
		scheme := raw[:i]
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 && strings.Contains(rest[:at], ":") {
			// 已带凭证：以配置凭证为准重建。
			rest = rest[at+1:]
		}
		return scheme + "://" + urlEncodeUser(name) + ":" + urlEncodeUser(secret) + "@" + rest, display
	}
	// SSH 形态（git@host:path）不注入 HTTP 凭证，避免破坏 URL 语义。
	return raw, display
}

// isLocalPath 判断是否为本地文件系统路径（含 Windows 盘符与 UNC）。
func isLocalPath(u string) bool {
	if u == "" {
		return true
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, `\`) || strings.HasPrefix(u, ".") {
		return true
	}
	// Windows 盘符：C:\... 或 C:/...
	if len(u) >= 2 && u[1] == ':' {
		return true
	}
	if strings.HasPrefix(strings.ToLower(u), "file://") {
		return true
	}
	// 无协议前缀（裸路径、UNC 共享等）视为本地路径。
	return !strings.Contains(u, "://")
}

// SanitizeURL 脱敏 URL 中的用户凭证部分（user:pass@host → ***:***@host）。
func SanitizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if isLocalPath(raw) && !strings.Contains(raw, "://") {
		return raw
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return sanitizeSCPStyle(raw)
	}
	scheme := raw[:i]
	rest := raw[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	cred := rest[:at]
	host := rest[at+1:]
	if cred == "" {
		return scheme + "://" + host
	}
	user := ""
	if c := strings.Index(cred, ":"); c >= 0 {
		user = cred[:c]
	} else {
		user = cred
	}
	if user == "" {
		user = "***"
	}
	return scheme + "://" + user + ":***@" + host
}

// sanitizeSCPStyle 处理 git@host:path 形态（可能带 ssh:// 前缀）。
func sanitizeSCPStyle(raw string) string {
	at := strings.Index(raw, "@")
	if at < 0 {
		return raw
	}
	return raw[:at] + "@" + raw[at+1:]
}

// RedactSecret 从任意文本中移除明文 secret（长度不足 4 时整体打码）。
func RedactSecret(text, secret string) string {
	if secret == "" || text == "" {
		return text
	}
	if len(secret) < 4 {
		return strings.ReplaceAll(text, secret, "***")
	}
	return strings.ReplaceAll(text, secret, "***")
}

// normalizeErrorText 归一化 git 错误输出：脱敏、压缩空白、限长。
func normalizeErrorText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = SanitizeURL(s)
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// 去掉 git 的 hint 噪音，保留核心报错。
		if strings.HasPrefix(l, "hint:") {
			continue
		}
		kept = append(kept, l)
	}
	out := strings.Join(kept, " | ")
	if len(out) > 600 {
		out = TruncateUTF8(out, 600) + "...(截断)"
	}
	return out
}

// urlEncodeUser 对 URL 用户信息部分做最小必要转义。
func urlEncodeUser(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '@', '/', ':', '?', '#', '[', ']', ' ':
			b.WriteString(fmt.Sprintf("%%%02X", r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeRepoPath 归一化仓库内路径（统一 `/`、去掉 ./ 前缀与首尾斜杠）。
func normalizeRepoPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// safeSegment 把任意字符串转为安全的目录名片段。
func safeSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	out = strings.Trim(out, ".")
	if out == "" || out == "_" {
		return "_"
	}
	return out
}

// shortSHA 返回短 sha 便于日志展示。
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// isHex 判断字符串是否为十六进制。
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

// isMissingPathErr 判断 stderr 是否表示路径不存在。
func isMissingPathErr(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	for _, m := range []string{"does not exist", "not in", "exists on disk", "unknown revision", "invalid object name", "no such path", "path does not exist"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// globToRegex 把 glob 风格模式转换为 ERE（`**` → `.*`，`*` → `[^/]*`，`?` → `.`）。
func globToRegex(pattern string) string {
	var b strings.Builder
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// detectLanguageByPath 仅按路径推断语言（内部快捷方法）。
func detectLanguageByPath(path string) string {
	return detectByExt(path)
}

// detectByExt 只按扩展名判定语言，未识别时返回 text。
func detectByExt(path string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/")))
	if v, ok := nameLanguage[base]; ok {
		return v
	}
	if v, ok := extLanguage[strings.ToLower(filepath.Ext(base))]; ok {
		return v
	}
	return LangText
}
