package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 依赖注入端口
// ---------------------------------------------------------------------------

// Materializer 由源码层提供：把某仓库某 commit 检出到目标目录。
//
// 定义在本包而不是直接依赖 internal/source，是为了让第七层的执行侧与源码管理层
// 解耦：并行开发时两者互不 import，装配阶段由 cmd/server 注入实现，避免循环依赖。
type Materializer interface {
	Materialize(ctx context.Context, repo *domain.Repository, commit, dest string) error
}

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

// defaultWorkspaceTTL 工作区默认保留时长（Reap 清理超过该时长的残留目录）。
const defaultWorkspaceTTL = 24 * time.Hour

// defaultMaterializeTimeout 未指定超时时的仓库物化超时。
const defaultMaterializeTimeout = 180 * time.Second

// defaultValidateTimeout 未指定超时时的整体验证超时。
const defaultValidateTimeout = 300 * time.Second

// maxScanFileBytes 语言统计/静态校验单文件读取上限（超限跳过，避免大文件拖垮验证）。
const maxScanFileBytes = 2 << 20

// maxScanFiles 语言统计的最大遍历文件数。
const maxScanFiles = 4000

// 校验 check 名称常量（写入 domain.VerificationCheck.Name，前端与报告依赖这些值）。
const (
	// checkPatchApply 补丁应用结果（每个补丁一条）。
	checkPatchApply = "patch_apply"
	// checkStaticSyntax 括号/引号配平（忽略字符串与注释）。
	checkStaticSyntax = "static_syntax"
	// checkGoParse go/parser 真实语法检查（零依赖的编译级校验）。
	checkGoParse = "go_parse"
	// checkJSONValid JSON 语法校验。
	checkJSONValid = "json_valid"
	// checkYAMLLint 简易 YAML 缩进/结构检查。
	checkYAMLLint = "yaml_lint"
	// checkDangerScan 危险操作扫描（check 名称常量）。
	checkDangerScan = "danger_scan"
)

// 验证结果 ApplyResult 取值。
const (
	applyApplied  = "applied"
	applyConflict = "conflict"
	applySkipped  = "skipped"
)

// ignoredDirs 语言统计与扫描时跳过的目录（与 config.SourceConfig.IgnorePatterns 默认值对齐）。
var ignoredDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true, "target": true,
	"vendor": true, ".idea": true, "__pycache__": true, ".venv": true, "venv": true,
	".next": true, "coverage": true, ".gradle": true, "out": true, "bin": true, "obj": true,
}

// extToLanguage 扩展名 → 语言名（与 config.SandboxConfig.Checks 的键保持一致）。
var extToLanguage = map[string]string{
	".go":    "go",
	".java":  "java",
	".kt":    "java",
	".ts":    "typescript",
	".tsx":   "typescript",
	".js":    "javascript",
	".jsx":   "javascript",
	".mjs":   "javascript",
	".cjs":   "javascript",
	".py":    "python",
	".php":   "php",
	".rs":    "rust",
	".cs":    "dotnet",
	".rb":    "ruby",
	".c":     "c",
	".h":     "c",
	".cc":    "cpp",
	".cpp":   "cpp",
	".hpp":   "cpp",
	".scala": "scala",
	".swift": "swift",
	".sh":    "shell",
}

// sourceLikeExt 视为"源码"的扩展名，参与主语言统计。
var sourceLikeExt = map[string]bool{
	".go": true, ".java": true, ".kt": true, ".ts": true, ".tsx": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".py": true,
	".php": true, ".rs": true, ".cs": true, ".rb": true, ".c": true,
	".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".scala": true,
	".swift": true,
}

// dangerPattern 危险操作模式。
type dangerPattern struct {
	pattern string
	note    string
}

// dangerPatterns 危险操作扫描规则（大小写不敏感）。
var dangerPatterns = []dangerPattern{
	{"rm -rf /", "递归删除根目录"},
	{"rm -rf /*", "递归删除根目录内容"},
	{"rm -rf ~", "递归删除用户目录"},
	{"rm -rf .", "递归删除当前目录"},
	{"drop table", "删除数据库表"},
	{"drop database", "删除数据库"},
	{"truncate table", "清空数据库表"},
	{"system.exit", "强制终止宿主进程"},
	{"os.exit", "强制终止宿主进程"},
	{"process.exit", "强制终止宿主进程"},
	{"runtime.halt", "强制终止宿主进程"},
	{"shutdown -h", "关闭主机"},
	{"shutdown -r", "重启主机"},
	{"mkfs.", "格式化文件系统"},
	{":(){:|:&};:", "fork 炸弹"},
	{"chmod 777 /", "开放根目录权限"},
	{"eval(base64_decode", "混淆代码执行"},
}

// ---------------------------------------------------------------------------
// Manager / Handle
// ---------------------------------------------------------------------------

// Manager 实现 domain.SandboxManager：工作区隔离物化 + 补丁验证。
//
// 并发安全：工作区表由 mu 保护；同一 runID 重复 Open 幂等返回同一工作区。
type Manager struct {
	cfg    config.SandboxConfig
	srcCfg config.SourceConfig
	mat    Materializer
	run    *Runner
	log    *logx.Logger

	mu      sync.Mutex
	spaces  map[string]*Handle
	metrics managerMetrics
}

// managerMetrics 管理器累计指标（Stats 使用）。
type managerMetrics struct {
	opens      int64
	reuse      int64
	openFail   int64
	closed     int64
	reaped     int64
	validates  int64
	validateOK int64
	degraded   int64
	conflicts  int64
	skipped    int64
}

// NewManager 创建沙箱管理器。mat 为 nil 时工作区仍可创建（空工作区，走静态校验降级）。
func NewManager(cfg config.SandboxConfig, srcCfg config.SourceConfig, mat Materializer, log *logx.Logger) *Manager {
	if log == nil {
		log = logx.Nop()
	}
	return &Manager{
		cfg:    cfg,
		srcCfg: srcCfg,
		mat:    mat,
		run:    NewRunner(cfg).WithLogger(log),
		log:    log,
		spaces: make(map[string]*Handle),
	}
}

// Runner 返回内部受限命令执行器（供上层单独复用）。
func (m *Manager) Runner() *Runner { return m.run }

// Handle 一个隔离工作区句柄，实现 domain.SandboxHandle。
type Handle struct {
	id        string
	root      string
	repoDirs  map[string]string
	dirs      []string
	warnings  []string
	createdAt time.Time
	repoCount int
	// owner 指回创建它的管理器，使 Handle.Close() 直接满足 domain.SandboxHandle
	// 契约（调用方可以 defer handle.Close()），清理策略仍然集中在 Manager 里。
	owner *Manager

	mu      sync.RWMutex
	deleted bool
}

// Close 释放工作区，实现 domain.SandboxHandle。
// 是否真正删除目录由 config.SandboxConfig.KeepWorkspace 决定。
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	owner := h.owner
	h.mu.Unlock()
	if owner == nil {
		return nil
	}
	return owner.Close(h)
}

// ID 工作区标识（文件名安全化的 runID）。
func (h *Handle) ID() string { return h.id }

// Root 工作区根目录（绝对路径）。
func (h *Handle) Root() string { return h.root }

// RepoDir 返回某仓库在工作区中的目录。
//
// 依次按仓库 Key、仓库 ID 匹配；未命中时回退到 <root>/<repoKey>（仍然位于工作区
// 之内），保证调用方总能拿到一个可用于执行命令的工作区内部路径，不会越界。
func (h *Handle) RepoDir(repoKey string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if dir, ok := h.repoDirs[repoKey]; ok {
		return dir
	}
	key := sanitizeRunID(repoKey)
	if dir, ok := h.repoDirs[key]; ok {
		return dir
	}
	if repoKey == "" {
		return h.root
	}
	return filepath.Join(h.root, key)
}

// Warnings 返回工作区构造过程中的非致命告警（例如某仓库物化失败）。
func (h *Handle) Warnings() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string(nil), h.warnings...)
}

// WarningsString 返回换行拼接的告警，便于写入 Verification.Log 与 TaskRun.Warnings。
func (h *Handle) WarningsString() string {
	return strings.Join(h.Warnings(), "\n")
}

// RepoKeys 返回工作区内已成功物化的仓库 Key（稳定排序）。
func (h *Handle) RepoKeys() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.repoDirs))
	for k := range h.repoDirs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Closed 工作区是否已关闭。
func (h *Handle) Closed() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.deleted
}

// addWarning 追加告警（并发安全）。
func (h *Handle) addWarning(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.warnings = append(h.warnings, msg)
}

// ---------------------------------------------------------------------------
// Open / Close / Reap
// ---------------------------------------------------------------------------

// Open 为一次任务创建隔离工作区，并物化所有锁定版本的仓库快照。
//
// 降级语义（对应契约 §6.6"降级不失败"）：
//   - 单个仓库物化失败**不返回致命错误**，而是记入 Handle.Warnings() 后继续处理其余仓库；
//   - meta.Repos 为空（无网络 / 无 git 场景）时同样能建出一个空工作区，
//     后续 Validate 会走静态校验路径；
//   - 同一 runID 重复 Open 幂等返回同一工作区。
//
// 物化超时取 meta.Timeout（<=0 时回退到 defaultMaterializeTimeout）。
func (m *Manager) Open(ctx context.Context, meta domain.SandboxMeta) (domain.SandboxHandle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(meta.RunID) == "" {
		meta.RunID = "run-" + uuid.NewString()
	}
	id := sanitizeRunID(meta.RunID)

	m.mu.Lock()
	if h, ok := m.spaces[id]; ok && !h.Closed() {
		m.metrics.reuse++
		m.mu.Unlock()
		m.log.Debug("复用已存在的沙箱工作区", "workspace", id, "run", meta.RunID)
		return h, nil
	}
	base := m.workspaceRoot()
	root := filepath.Join(base, id)
	if err := os.MkdirAll(root, 0o755); err != nil {
		m.metrics.openFail++
		m.mu.Unlock()
		return nil, fmt.Errorf("创建沙箱工作区失败(%s): %w", root, err)
	}
	h := &Handle{
		id:        id,
		root:      root,
		repoDirs:  make(map[string]string, len(meta.Repos)),
		createdAt: time.Now(),
		owner:     m,
	}
	m.spaces[id] = h
	m.metrics.opens++
	m.mu.Unlock()

	timeout := meta.Timeout
	if timeout <= 0 {
		timeout = defaultMaterializeTimeout
	}
	m.log.Info("沙箱工作区已创建", "workspace", id, "run", meta.RunID, "repos", len(meta.Repos), "root", root)

	for _, repo := range meta.Repos {
		if repo == nil {
			continue
		}
		key := repoKeyOf(repo)
		if key == "" {
			h.addWarning(fmt.Sprintf("仓库 %s 缺少 Key/ID，已跳过物化", repo.Name))
			continue
		}
		dir := filepath.Join(root, sanitizeRunID(key))
		if !withinRoot(root, dir) {
			h.addWarning(fmt.Sprintf("仓库 %s 的目标目录越界，已跳过物化", key))
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.addWarning(fmt.Sprintf("仓库 %s 目录创建失败：%v", key, err))
			continue
		}
		h.repoDirs[key] = dir
		h.dirs = append(h.dirs, dir)
		if m.mat == nil {
			h.addWarning(fmt.Sprintf("仓库 %s 未物化：未注入 Materializer（降级为静态校验）", key))
			continue
		}
		commit := ""
		if meta.Pinned != nil {
			commit = meta.Pinned[repo.ID]
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		err := m.mat.Materialize(callCtx, repo, commit, dir)
		cancel()
		if err != nil {
			// 关键：单个仓库失败不致命，记录告警后继续处理其它仓库。
			msg := fmt.Sprintf("仓库 %s 物化失败（commit=%s）：%v", key, shortCommit(commit), err)
			h.addWarning(msg)
			m.log.Warn("仓库物化失败，已降级继续", "workspace", id, "repo", key, "commit", commit, "err", err.Error())
			continue
		}
		h.repoCount++
		m.log.Debug("仓库物化完成", "workspace", id, "repo", key, "commit", shortCommit(commit), "dir", dir)
	}
	return h, nil
}

// Close 释放工作区。cfg.KeepWorkspace 为 true 时保留目录以便人工复核。
func (m *Manager) Close(h *Handle) error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.deleted {
		h.mu.Unlock()
		return nil
	}
	h.deleted = true
	h.mu.Unlock()

	m.mu.Lock()
	delete(m.spaces, h.id)
	m.metrics.closed++
	m.mu.Unlock()

	if m.cfg.KeepWorkspace {
		m.log.Info("保留沙箱工作区（keepWorkspace=true）", "workspace", h.id, "root", h.root)
		return nil
	}
	if !withinRoot(m.workspaceRoot(), h.root) || filepath.Clean(h.root) == filepath.Clean(m.workspaceRoot()) {
		return fmt.Errorf("sandbox: 拒绝删除工作区根目录之外的路径 %s", h.root)
	}
	if err := os.RemoveAll(h.root); err != nil {
		m.log.Warn("清理沙箱工作区失败", "workspace", h.id, "root", h.root, "err", err.Error())
		return fmt.Errorf("清理沙箱工作区失败(%s): %w", h.root, err)
	}
	m.log.Debug("沙箱工作区已清理", "workspace", h.id, "root", h.root)
	return nil
}

// Reap 清理 srcCfg.WorkspaceDir 下超过 ttl 的残留工作区目录（启动时调用即可）。
// ttl <= 0 时使用 defaultWorkspaceTTL（24h）。返回清理数量。
func (m *Manager) Reap(ttl time.Duration) (int, error) {
	if ttl <= 0 {
		ttl = defaultWorkspaceTTL
	}
	base := m.workspaceRoot()
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取工作区根目录失败(%s): %w", base, err)
	}
	deadline := time.Now().Add(-ttl)
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		if !withinRoot(base, dir) {
			continue
		}
		m.mu.Lock()
		active := false
		if h, ok := m.spaces[sanitizeRunID(entry.Name())]; ok && !h.Closed() {
			active = true
		}
		m.mu.Unlock()
		if active {
			continue // 正在使用的工作区不清理
		}
		info, statErr := entry.Info()
		if statErr != nil || info.ModTime().After(deadline) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			m.log.Warn("清理残留工作区失败", "dir", dir, "err", err.Error())
			continue
		}
		m.mu.Lock()
		m.metrics.reaped++
		m.mu.Unlock()
		removed++
		m.log.Info("已清理超期沙箱工作区", "dir", dir, "ttlHours", ttl.Hours())
	}
	return removed, nil
}

// Stats 返回工作区与验证统计（供可观测汇总使用）。
func (m *Manager) Stats() map[string]any {
	m.mu.Lock()
	metrics := m.metrics
	active := 0
	for _, h := range m.spaces {
		if !h.Closed() {
			active++
		}
	}
	m.mu.Unlock()

	validateRate := 0.0
	if metrics.validates > 0 {
		validateRate = float64(metrics.validateOK) / float64(metrics.validates)
	}
	return map[string]any{
		"activeWorkspaces":    active,
		"workspaceOpens":      metrics.opens,
		"workspaceReuse":      metrics.reuse,
		"workspaceOpenFail":   metrics.openFail,
		"workspaceClosed":     metrics.closed,
		"workspaceReaped":     metrics.reaped,
		"validations":         metrics.validates,
		"validationsPassed":   metrics.validateOK,
		"validationRate":      validateRate,
		"degradedValidations": metrics.degraded,
		"applyConflicts":      metrics.conflicts,
		"skippedPatches":      metrics.skipped,
		"workspaceRoot":       m.workspaceRoot(),
		"keepWorkspace":       m.cfg.KeepWorkspace,
		"allowCommandExec":    m.cfg.AllowCommandExec,
		"networkIsolated":     m.cfg.NetworkIsolated,
		"commandExec":         m.run.Stats(),
	}
}

// workspaceRoot 返回工作区根目录（相对路径按当前工作目录解析为绝对路径）。
func (m *Manager) workspaceRoot() string {
	dir := strings.TrimSpace(m.srcCfg.WorkspaceDir)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "codeagent-workspace")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// ---------------------------------------------------------------------------
// Validate
// ---------------------------------------------------------------------------

// Validate 在隔离工作区中应用补丁并执行编译/静态校验/测试。
//
// 流程：
//  1. 逐个补丁在**工作区文件**上应用；仓库不在工作区时标记 ApplyResult="skipped"
//     并在 checks 中说明（不视为失败，对应契约的降级要求）；
//  2. 变更只写回工作区内部（绝对不触碰工作区之外的路径，含路径穿越防护）；
//  3. AllowCommandExec 且 req.DryRun==false 时，按工作区主语言取 cfg.Checks 命令执行；
//     未配置该语言命令时追加一条 Skipped 的 check 并改为静态校验；
//  4. 静态校验**始终执行**（括号配平 / go/parser / JSON / YAML / 危险操作扫描）；
//  5. 聚合 Passed / ApplyResult / DurationMS / Log / Degraded。
//
// handle 为 nil 时返回错误（不 panic）。
func (m *Manager) Validate(ctx context.Context, handle domain.SandboxHandle, req domain.ValidateRequest) (*domain.Verification, error) {
	if handle == nil {
		return nil, errors.New("sandbox: handle 不能为 nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	root := handle.Root()
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("sandbox: handle.Root() 为空，拒绝执行验证")
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultValidateTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	m.mu.Lock()
	m.metrics.validates++
	m.mu.Unlock()

	verification := &domain.Verification{
		Workspace:   handle.ID(),
		ApplyResult: applySkipped,
		Checks:      []domain.VerificationCheck{},
	}
	var logLines []string
	dirCache := newRepoDirCache(handle, root)

	// --- 1. 补丁应用 -------------------------------------------------------
	repoKeys := handleRepoKeys(handle)
	appliedCount, skippedCount, conflictCount := 0, 0, 0
	// patchedFiles 记录本次真正落地的文件（相对工作区根），静态校验只扫这些文件。
	patchedFiles := map[string]string{} // 相对 root 的路径 → 新内容
	for i := range req.Patches {
		p := req.Patches[i]
		label := patchLabel(p)
		repoDir, repoKnown := dirCache.resolve(p.RepoKey, p.RepositoryID, p.FilePath)
		if !repoKnown {
			skippedCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name:       checkPatchApply,
				Passed:     true,
				Skipped:    true,
				SkipReason: fmt.Sprintf("仓库 %s 不在工作区内，补丁已跳过（降级：不影响整体结论）", displayRepo(p)),
			})
			logLines = append(logLines, fmt.Sprintf("[skip] %s：仓库 %s 未物化，跳过应用", label, displayRepo(p)))
			continue
		}
		rel, err := CheckSafeRelPath(p.FilePath)
		if err != nil {
			conflictCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name:    checkPatchApply,
				Passed:  false,
				Command: label,
				Output:  err.Error(),
			})
			logLines = append(logLines, fmt.Sprintf("[conflict] %s：%v", label, err))
			continue
		}
		absTarget := filepath.Join(repoDir, filepath.FromSlash(rel))
		if !withinRoot(root, absTarget) {
			conflictCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name:    checkPatchApply,
				Passed:  false,
				Command: label,
				Output:  "目标路径越出工作区，已拒绝",
			})
			logLines = append(logLines, fmt.Sprintf("[conflict] %s：路径穿越被拒绝（%s）", label, p.FilePath))
			continue
		}

		oldContent := ""
		if p.Action != domain.ActionCreate {
			raw, readErr := os.ReadFile(absTarget)
			if readErr != nil {
				if !os.IsNotExist(readErr) {
					conflictCount++
					verification.Checks = append(verification.Checks, domain.VerificationCheck{Name: checkPatchApply, Passed: false, Command: label, Output: readErr.Error()})
					logLines = append(logLines, fmt.Sprintf("[conflict] %s：读取失败 %v", label, readErr))
					continue
				}
				if p.OldContent == "" {
					conflictCount++
					verification.Checks = append(verification.Checks, domain.VerificationCheck{
						Name: checkPatchApply, Passed: false, Command: label,
						Output: fmt.Sprintf("工作区内不存在文件 %s", rel),
					})
					logLines = append(logLines, fmt.Sprintf("[conflict] %s：工作区缺少文件 %s", label, rel))
					continue
				}
				raw = []byte(p.OldContent)
			}
			oldContent = string(raw)
		}

		// 冲突判定：同一文件是否已被前序补丁改写。
		//
		// 多个补丁常常来自同一文件的**独立**修复轮次（例如同一方法的不同缺陷），
		// 它们都基于原始文件上下文生成，无法互相叠加：
		//   - 若第一个补丁已成功改写该文件，后续补丁视为"与已应用补丁冲突"，
		//     不再改写工作区内容（保证工作区是确定、可复现的结果），但在日志中给出中文说明；
		//   - 首补丁失败（例如上下文不匹配）时该文件不会进入 patchedFiles，后续补丁仍可尝试应用。
		if _, already := patchedFiles[relativeToRoot(root, absTarget)]; already {
			// 同文件多补丁：本补丁的上下文基于原始文件生成，无法与已应用补丁叠加。
			// 语义上是"跳过重复变更"而非"校验失败"，因此标记 Skipped（不计入 passed 判定），
			// 保证工作区内容是确定、可复现的单一结果；真正的冲突（上下文不匹配、
			// 路径越界等）仍按失败处理。
			skippedCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name: checkPatchApply, Passed: true, Skipped: true, Command: label,
				SkipReason: "同一文件存在多个补丁，本补丁与已应用补丁重叠，已跳过（同文件多缺陷建议合并为一个补丁后重跑）",
				Output:     "已保留先应用的补丁，工作区内容为该补丁的结果",
			})
			logLines = append(logLines, fmt.Sprintf("[skip] %s：同文件多补丁重叠，已保留先应用者", label))
			continue
		}

		newContent, applyErr := ApplyUnifiedDiff(oldContent, p.UnifiedDiff)
		if applyErr != nil {
			conflictCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name: checkPatchApply, Passed: false, Command: label, Output: applyErr.Error(),
			})
			logLines = append(logLines, fmt.Sprintf("[conflict] %s：%v", label, applyErr))
			continue
		}
		if p.Action == domain.ActionDelete {
			// 删除动作：仍然限定在工作区内。
			if err := os.Remove(absTarget); err != nil && !os.IsNotExist(err) {
				conflictCount++
				verification.Checks = append(verification.Checks, domain.VerificationCheck{Name: checkPatchApply, Passed: false, Command: label, Output: err.Error()})
				logLines = append(logLines, fmt.Sprintf("[conflict] %s：删除失败 %v", label, err))
				continue
			}
			delete(patchedFiles, relativeToRoot(root, absTarget))
			appliedCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{Name: checkPatchApply, Passed: true, Command: label, Output: "已删除文件"})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
			conflictCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{Name: checkPatchApply, Passed: false, Command: label, Output: err.Error()})
			logLines = append(logLines, fmt.Sprintf("[conflict] %s：创建目录失败 %v", label, err))
			continue
		}
		if err := os.WriteFile(absTarget, []byte(newContent), 0o644); err != nil {
			conflictCount++
			verification.Checks = append(verification.Checks, domain.VerificationCheck{Name: checkPatchApply, Passed: false, Command: label, Output: err.Error()})
			logLines = append(logLines, fmt.Sprintf("[conflict] %s：写入失败 %v", label, err))
			continue
		}
		patchedFiles[relativeToRoot(root, absTarget)] = newContent
		appliedCount++
		verification.Checks = append(verification.Checks, domain.VerificationCheck{
			Name: checkPatchApply, Passed: true, Command: label,
			Output: fmt.Sprintf("已应用（%d 行 → %d 行）", countLines(oldContent), countLines(newContent)),
		})
	}
	switch {
	case conflictCount > 0:
		verification.ApplyResult = applyConflict
	case appliedCount > 0:
		verification.ApplyResult = applyApplied
	default:
		verification.ApplyResult = applySkipped
	}
	logLines = append(logLines, fmt.Sprintf("补丁应用：成功 %d，冲突 %d，跳过 %d（ApplyResult=%s）",
		appliedCount, conflictCount, skippedCount, verification.ApplyResult))

	// --- 2. 命令校验 -------------------------------------------------------
	lang := inferWorkspaceLanguage(root)
	verification.Checks = append(verification.Checks, domain.VerificationCheck{
		Name: "language_detect", Passed: true, Skipped: true,
		SkipReason: fmt.Sprintf("推断主语言为 %s", orUnknown(lang)),
		Output:     fmt.Sprintf("工作区仓库: %s", strings.Join(repoKeys, ", ")),
	})

	commands := m.commandsFor(lang)
	executed := 0
	degraded := false
	degradeReasons := []string{}
	switch {
	case req.DryRun:
		degraded = true
		degradeReasons = append(degradeReasons, "req.DryRun=true：仅做补丁应用校验，不执行命令")
	case !m.cfg.AllowCommandExec:
		degraded = true
		degradeReasons = append(degradeReasons, "cfg.AllowCommandExec=false：沙箱未开启命令执行能力")
	case m.cfg.Enabled == false:
		degraded = true
		degradeReasons = append(degradeReasons, "cfg.Enabled=false：沙箱执行层未启用")
	}
	if !degraded {
		if len(commands) == 0 {
			verification.Checks = append(verification.Checks, domain.VerificationCheck{
				Name: "command_exec", Passed: true, Skipped: true,
				SkipReason: fmt.Sprintf("未配置 %s 的校验命令", orUnknown(lang)),
			})
			logLines = append(logLines, fmt.Sprintf("未配置 %s 的校验命令，改用静态校验", orUnknown(lang)))
		} else {
			for _, cmdStr := range commands {
				if runCtx.Err() != nil {
					verification.Checks = append(verification.Checks, domain.VerificationCheck{
						Name: "command_exec", Passed: false, Command: cmdStr,
						Output: "验证整体超时，剩余命令未执行",
					})
					logLines = append(logLines, fmt.Sprintf("[timeout] %s 未执行（整体超时）", cmdStr))
					break
				}
				workDir := resolveCommandDir(handle, root, repoKeys)
				// 能力预检：工具链不存在（未安装 / 项目未附带 wrapper）属于**环境能力缺失**，
				// 应按降级处理而不是判定修复失败，否则在无 Maven/无网络的机器上
				// 会把"补丁本身没问题"误报为验证不通过。
				if missing := safeProbeCommand(cmdStr, workDir); missing != "" {
					verification.Checks = append(verification.Checks, domain.VerificationCheck{
						Name: "command_exec", Passed: true, Skipped: true, Command: cmdStr,
						SkipReason: missing + "（环境缺少该校验能力，已降级为静态校验）",
					})
					degraded = true
					degradeReasons = append(degradeReasons, missing)
					logLines = append(logLines, fmt.Sprintf("[skip] %s：%s", cmdStr, missing))
					continue
				}
				out, duration, runErr := m.run.Run(runCtx, workDir, cmdStr)
				check := domain.VerificationCheck{
					Name:       "command_exec",
					Passed:     runErr == nil,
					Command:    cmdStr,
					Output:     out,
					DurationMS: duration,
				}
				if runErr != nil {
					check.SkipReason = ""
					check.Output = out + "\n" + runErr.Error()
					// 无网络导致的拒绝属于"能力缺失"，计入降级而非失败。
					if errors.Is(runErr, ErrNetworkIsolated) || errors.Is(runErr, ErrCommandNotAllowed) {
						check.Skipped = true
						check.Passed = true
						check.SkipReason = runErr.Error()
						degraded = true
						degradeReasons = append(degradeReasons, "命令因隔离策略被拒绝："+cmdStr)
					} else {
						logLines = append(logLines, fmt.Sprintf("[fail] %s：%v", cmdStr, runErr))
					}
				} else {
					executed++
					logLines = append(logLines, fmt.Sprintf("[ok] %s（%dms）", cmdStr, duration))
				}
				verification.Checks = append(verification.Checks, check)
			}
		}
	} else {
		verification.Checks = append(verification.Checks, domain.VerificationCheck{
			Name: "command_exec", Passed: true, Skipped: true,
			SkipReason: strings.Join(degradeReasons, "；"),
		})
	}

	// --- 3. 静态校验（始终执行，作为无命令环境下的兜底） --------------------
	verification.Checks = append(verification.Checks, staticChecks(root, repoKeys, patchedFiles)...)
	if executed == 0 && !degraded {
		degraded = true
		degradeReasons = append(degradeReasons, "没有任何校验命令被执行")
	}

	// --- 4. 聚合 -----------------------------------------------------------
	passed := true
	for _, c := range verification.Checks {
		if c.Skipped {
			continue
		}
		if !c.Passed {
			passed = false
			break
		}
	}
	verification.Passed = passed
	verification.Degraded = degraded
	verification.DurationMS = time.Since(start).Milliseconds()

	if degraded {
		logLines = append(logLines, "【降级】"+strings.Join(degradeReasons, "；")+"；已改为静态校验兜底，结论仍基于静态校验给出。")
	}
	if h, ok := handle.(*Handle); ok {
		if warns := h.Warnings(); len(warns) > 0 {
			logLines = append(logLines, "工作区告警：")
			for _, w := range warns {
				logLines = append(logLines, "  - "+w)
			}
		}
	}
	verification.Log = truncateOutput(strings.Join(logLines, "\n"), m.run.maxOutput())

	m.mu.Lock()
	if verification.Passed {
		m.metrics.validateOK++
	}
	if verification.Degraded {
		m.metrics.degraded++
	}
	m.metrics.conflicts += int64(conflictCount)
	m.metrics.skipped += int64(skippedCount)
	m.mu.Unlock()

	m.log.Info("沙箱验证完成",
		"workspace", verification.Workspace,
		"passed", verification.Passed,
		"applyResult", verification.ApplyResult,
		"degraded", verification.Degraded,
		"checks", len(verification.Checks),
		"durationMs", verification.DurationMS,
	)
	return verification, nil
}

// commandsFor 取某语言的校验命令列表（去空、去重、保序）。
func (m *Manager) commandsFor(lang string) []string {
	if lang == "" || len(m.cfg.Checks) == 0 {
		return nil
	}
	raw, ok := m.cfg.Checks[lang]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, c := range raw {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// 工作区文件读写（供引擎/报告使用）
// ---------------------------------------------------------------------------

// SnapshotFiles 读取工作区内指定文件的内容。
//
// repoKey 为空表示路径相对工作区根；paths 中的每一项都会经过路径穿越防护。
// 返回 map[相对工作区根的路径]内容；文件不存在会返回错误（含具体路径）。
func (m *Manager) SnapshotFiles(handle domain.SandboxHandle, repoKey string, paths []string) (map[string]string, error) {
	if handle == nil {
		return nil, errors.New("sandbox: handle 不能为 nil")
	}
	root := handle.Root()
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	base, ok := repoBaseDir(handle, root, repoKey)
	if !ok {
		return nil, fmt.Errorf("sandbox: 仓库 %s 不在工作区内", repoKey)
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		rel, err := CheckSafeRelPath(p)
		if err != nil {
			return nil, err
		}
		abs := filepath.Join(base, filepath.FromSlash(rel))
		if !withinRoot(root, abs) {
			return nil, fmt.Errorf("%w: %s 越出工作区", ErrUnsafePath, p)
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("读取工作区文件失败(%s): %w", rel, err)
		}
		out[p] = string(raw)
	}
	return out, nil
}

// ApplyToWorkspace 仅把补丁应用到工作区（不做验证），返回成功数量与冲突描述。
//
// 与 Validate 的区别：不执行任何命令、不做静态校验，供引擎在"预演补丁"时使用。
// 所有写入都被限制在工作区内部；任一补丁失败只记入 conflicts，不影响其它补丁。
func (m *Manager) ApplyToWorkspace(handle domain.SandboxHandle, patches []domain.Patch) (applied int, conflicts []string, err error) {
	if handle == nil {
		return 0, nil, errors.New("sandbox: handle 不能为 nil")
	}
	root := handle.Root()
	if abs, absErr := filepath.Abs(root); absErr == nil {
		root = abs
	}
	dirCache := newRepoDirCache(handle, root)
	for i := range patches {
		p := patches[i]
		label := patchLabel(p)
		repoDir, repoKnown := dirCache.resolve(p.RepoKey, p.RepositoryID, p.FilePath)
		if !repoKnown {
			conflicts = append(conflicts, fmt.Sprintf("%s：仓库 %s 不在工作区内（已跳过）", label, displayRepo(p)))
			continue
		}
		rel, pathErr := CheckSafeRelPath(p.FilePath)
		if pathErr != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s：%v", label, pathErr))
			continue
		}
		absTarget := filepath.Join(repoDir, filepath.FromSlash(rel))
		if !withinRoot(root, absTarget) {
			conflicts = append(conflicts, fmt.Sprintf("%s：目标路径越出工作区（已拒绝）", label))
			continue
		}
		oldContent := ""
		if p.Action != domain.ActionCreate {
			raw, readErr := os.ReadFile(absTarget)
			if readErr != nil {
				if p.OldContent == "" {
					conflicts = append(conflicts, fmt.Sprintf("%s：工作区内不存在文件 %s", label, rel))
					continue
				}
				raw = []byte(p.OldContent)
			}
			oldContent = string(raw)
		}
		newContent, applyErr := ApplyUnifiedDiff(oldContent, p.UnifiedDiff)
		if applyErr != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s：%v", label, applyErr))
			continue
		}
		if p.Action == domain.ActionDelete {
			if rmErr := os.Remove(absTarget); rmErr != nil && !os.IsNotExist(rmErr) {
				conflicts = append(conflicts, fmt.Sprintf("%s：删除失败 %v", label, rmErr))
				continue
			}
			applied++
			continue
		}
		if mkErr := os.MkdirAll(filepath.Dir(absTarget), 0o755); mkErr != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s：创建目录失败 %v", label, mkErr))
			continue
		}
		if wErr := os.WriteFile(absTarget, []byte(newContent), 0o644); wErr != nil {
			conflicts = append(conflicts, fmt.Sprintf("%s：写入失败 %v", label, wErr))
			continue
		}
		applied++
	}
	return applied, conflicts, nil
}

// ---------------------------------------------------------------------------
// 路径安全
// ---------------------------------------------------------------------------

// withinRoot 判断 target 是否位于 root 之内（含等于 root）。
//
// 双重保险：先做字面路径段比较，再用 filepath.EvalSymlinks 解析软链接后复核，
// 防止工作区内存在指向外部的符号链接导致越界写入。
func withinRoot(root, target string) bool {
	if root == "" || target == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err == nil {
		if rel == "." {
			return true
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	// 软链接复核：仅当 target 已存在时才有意义。
	realRoot, rootErr := filepath.EvalSymlinks(cleanRoot)
	realTarget, targetErr := filepath.EvalSymlinks(cleanTarget)
	if rootErr == nil && targetErr == nil {
		if realRel, relErr := filepath.Rel(realRoot, realTarget); relErr == nil {
			return realRel == "." || (realRel != ".." && !strings.HasPrefix(realRel, ".."+string(filepath.Separator)))
		}
	}
	return false
}

// relativeToRoot 返回相对工作区根的斜杠路径（用于日志与静态校验定位）。
func relativeToRoot(root, abs string) string {
	if rel, err := filepath.Rel(root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

// ---------------------------------------------------------------------------
// 仓库目录解析
// ---------------------------------------------------------------------------

// repoDirCache 缓存 handle → 仓库目录 的解析结果。
type repoDirCache struct {
	handle domain.SandboxHandle
	root   string
	cache  map[string]repoDirEntry
	known  []string
}

type repoDirEntry struct {
	dir   string
	known bool
}

func newRepoDirCache(handle domain.SandboxHandle, root string) *repoDirCache {
	return &repoDirCache{handle: handle, root: root, cache: map[string]repoDirEntry{}}
}

// resolve 解析补丁所属仓库的工作区目录。
//
// 依次尝试：RepoDir(repoKey) → RepoDir(repositoryID) → 路径首段等于仓库名。
// 返回的 known=false 表示该仓库不在当前工作区（补丁应标记为 skipped）。
//
// 关键约束：兜底匹配**必须**依赖补丁路径的首段与仓库名一致，绝不能"落到第一个仓库"，
// 否则多仓库工作区里会把 A 仓库的补丁写进 B 仓库，属于静默的正确性事故。
func (c *repoDirCache) resolve(repoKey, repoID, filePath string) (string, bool) {
	key := repoKey + "|" + repoID + "|" + filePath
	if entry, ok := c.cache[key]; ok {
		return entry.dir, entry.known
	}
	entry := repoDirEntry{}
	for _, candidate := range []string{repoKey, repoID} {
		dir, ok := c.dirOf(candidate)
		if !ok {
			continue
		}
		entry = repoDirEntry{dir: dir, known: true}
		break
	}
	if !entry.known && filePath != "" {
		// 兜底：补丁路径首段即仓库目录名（常见于 patch 路径已含仓库前缀）。
		segments := strings.Split(filepath.ToSlash(filePath), "/")
		if len(segments) > 1 {
			dir, ok := c.dirOf(segments[0])
			if ok {
				entry = repoDirEntry{dir: dir, known: true}
			}
		}
	}
	c.cache[key] = entry
	return entry.dir, entry.known
}

// dirOf 解析某个仓库名对应的工作区子目录；不存在时返回 false。
func (c *repoDirCache) dirOf(name string) (string, bool) {
	if strings.TrimSpace(name) == "" {
		return "", false
	}
	dir := c.handle.RepoDir(name)
	if dir == "" {
		return "", false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if dir == c.root || !withinRoot(c.root, dir) {
		return "", false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

// repoBaseDir 解析 repoKey 对应的工作区目录（repoKey 为空时返回工作区根）。
func repoBaseDir(handle domain.SandboxHandle, root, repoKey string) (string, bool) {
	if strings.TrimSpace(repoKey) == "" {
		return root, true
	}
	dir := handle.RepoDir(repoKey)
	if dir == "" {
		return "", false
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if !withinRoot(root, dir) {
		return "", false
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

// handleRepoKeys 取工作区中已物化的仓库 Key 列表（优先 *Handle，否则回退目录扫描）。
func handleRepoKeys(handle domain.SandboxHandle) []string {
	if h, ok := handle.(*Handle); ok {
		return h.RepoKeys()
	}
	root := handle.Root()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// resolveCommandDir 选择执行校验命令的工作目录（优先第一个已物化仓库，否则工作区根）。
func resolveCommandDir(handle domain.SandboxHandle, root string, repoKeys []string) string {
	for _, key := range repoKeys {
		dir := handle.RepoDir(key)
		if dir == "" {
			continue
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if withinRoot(root, dir) {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				return dir
			}
		}
	}
	return root
}

// ---------------------------------------------------------------------------
// 语言推断
// ---------------------------------------------------------------------------

// inferWorkspaceLanguage 按文件扩展名统计工作区主语言（占比最高者）。
func inferWorkspaceLanguage(root string) string {
	counts := map[string]int{}
	scanned := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单项错误不影响整体统计
		}
		if scanned >= maxScanFiles {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p == root {
				return nil
			}
			if ignoredDirs[name] || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !sourceLikeExt[ext] {
			return nil
		}
		scanned++
		if lang, ok := extToLanguage[ext]; ok {
			counts[lang]++
		}
		return nil
	})
	if len(counts) == 0 {
		return ""
	}
	best, bestCount := "", -1
	for lang, n := range counts {
		if n > bestCount || (n == bestCount && lang < best) {
			best, bestCount = lang, n
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// 静态校验
// ---------------------------------------------------------------------------

// staticChecks 执行静态校验（始终执行，作为无命令环境下的兜底）。
//
// 扫描目标为"本次补丁实际落地的文件"；没有任何补丁落地时退化为扫描工作区内的
// 少量源码文件（有上限），保证无 git / 无命令环境也能给出结论。
func staticChecks(root string, repoKeys []string, patched map[string]string) []domain.VerificationCheck {
	targets := collectStaticTargets(root, patched)
	if len(targets) == 0 {
		return []domain.VerificationCheck{{
			Name: checkStaticSyntax, Passed: true, Skipped: true,
			SkipReason: "没有可校验的变更文件（补丁未落地或无源码文件）",
		}}
	}

	var checks []domain.VerificationCheck
	syntax := domain.VerificationCheck{Name: checkStaticSyntax, Passed: true}
	goParse := domain.VerificationCheck{Name: checkGoParse, Passed: true, Skipped: true}
	jsonValid := domain.VerificationCheck{Name: checkJSONValid, Passed: true, Skipped: true}
	yamlLint := domain.VerificationCheck{Name: checkYAMLLint, Passed: true, Skipped: true}

	var (
		syntaxIssues []string
		goIssues     []string
		jsonIssues   []string
		yamlIssues   []string
		goChecked    int
		jsonChecked  int
		yamlChecked  int
	)
	for _, t := range targets {
		content, err := os.ReadFile(t.abs)
		if err != nil {
			syntaxIssues = append(syntaxIssues, fmt.Sprintf("%s：读取失败 %v", t.rel, err))
			continue
		}
		text := string(content)
		if issue := checkBracketBalance(text); issue != "" {
			syntaxIssues = append(syntaxIssues, fmt.Sprintf("%s：%s", t.rel, issue))
		}
		switch strings.ToLower(filepath.Ext(t.rel)) {
		case ".go":
			goChecked++
			if issue := checkGoSyntax(t.rel, text); issue != "" {
				goIssues = append(goIssues, issue)
			}
		case ".json":
			jsonChecked++
			if issue := checkJSONSyntax(text); issue != "" {
				jsonIssues = append(jsonIssues, fmt.Sprintf("%s：%s", t.rel, issue))
			}
		case ".yml", ".yaml":
			yamlChecked++
			if issue := checkYAMLSyntax(text); issue != "" {
				yamlIssues = append(yamlIssues, fmt.Sprintf("%s：%s", t.rel, issue))
			}
		}
	}

	syntax.Output = fmt.Sprintf("已检查 %d 个文件（括号/引号配平，忽略字符串与注释）", len(targets))
	if len(syntaxIssues) > 0 {
		syntax.Passed = false
		syntax.Output = strings.Join(syntaxIssues, "\n")
	}
	checks = append(checks, syntax)

	if goChecked == 0 {
		goParse.SkipReason = "无 Go 文件"
	} else {
		goParse.Skipped = false
		goParse.Output = fmt.Sprintf("go/parser 已校验 %d 个 Go 文件", goChecked)
		if len(goIssues) > 0 {
			goParse.Passed = false
			goParse.Output = strings.Join(goIssues, "\n")
		}
	}
	checks = append(checks, goParse)

	if jsonChecked == 0 {
		jsonValid.SkipReason = "无 JSON 文件"
	} else {
		jsonValid.Skipped = false
		jsonValid.Output = fmt.Sprintf("encoding/json 已校验 %d 个 JSON 文件", jsonChecked)
		if len(jsonIssues) > 0 {
			jsonValid.Passed = false
			jsonValid.Output = strings.Join(jsonIssues, "\n")
		}
	}
	checks = append(checks, jsonValid)

	if yamlChecked == 0 {
		yamlLint.SkipReason = "无 YAML 文件"
	} else {
		yamlLint.Skipped = false
		yamlLint.Output = fmt.Sprintf("缩进/结构检查已覆盖 %d 个 YAML 文件", yamlChecked)
		if len(yamlIssues) > 0 {
			yamlLint.Passed = false
			yamlLint.Output = strings.Join(yamlIssues, "\n")
		}
	}
	checks = append(checks, yamlLint)

	checks = append(checks, checkDangerScanFiles(targets, patched))
	return checks
}

// staticTarget 一个待静态校验的文件。
type staticTarget struct {
	abs string
	rel string
}

// collectStaticTargets 汇总静态校验目标文件（补丁落地文件优先，且有数量上限）。
func collectStaticTargets(root string, patched map[string]string) []staticTarget {
	var out []staticTarget
	seen := map[string]bool{}
	// 1. 补丁落地的文件必须全部校验（含新建文件）。
	keys := make([]string, 0, len(patched))
	for rel := range patched {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if !withinRoot(root, abs) || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, staticTarget{abs: abs, rel: rel})
	}
	if len(out) > 0 {
		return out
	}
	// 2. 无补丁落地：扫描工作区内少量源码文件（跳过依赖目录）。
	scanned := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || scanned >= 200 {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p == root {
				return nil
			}
			if ignoredDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !sourceLikeExt[ext] && ext != ".json" && ext != ".yml" && ext != ".yaml" {
			return nil
		}
		if info, statErr := d.Info(); statErr == nil && info.Size() > maxScanFileBytes {
			return nil
		}
		scanned++
		out = append(out, staticTarget{abs: p, rel: relativeToRoot(root, p)})
		return nil
	})
	return out
}

// checkBracketBalance 括号/引号配平检查（忽略字符串与注释）。
//
// 这是一个跨语言的"廉价语法检查"：能抓住括号未闭合、字符串未终止这类最常见的
// 补丁破坏，且不依赖任何语言工具链。
func checkBracketBalance(content string) string {
	type open struct {
		ch   rune
		line int
	}
	var (
		stack   []open
		line    = 1
		inBlock = false // /* */ 块注释
	)
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{', '>': '<'}
	isBracket := func(r rune) bool {
		switch r {
		case '(', ')', '[', ']', '{', '}':
			return true
		}
		return false
	}
	runes := []rune(content)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\n' {
			line++
			continue
		}
		if inBlock {
			if r == '*' && i+1 < len(runes) && runes[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		// 行注释
		if r == '/' && i+1 < len(runes) {
			next := runes[i+1]
			if next == '/' {
				for i < len(runes) && runes[i] != '\n' {
					i++
				}
				line++
				continue
			}
			if next == '*' {
				inBlock = true
				i++
				continue
			}
		}
		// 字符串（单双引号、反引号），支持 \ 转义
		if r == '"' || r == '\'' || r == '`' {
			quote := r
			startLine := line
			i++
			closed := false
			for i < len(runes) {
				c := runes[i]
				if c == '\n' {
					line++
					if quote != '`' {
						break // 非模板字符串不允许跨行
					}
				}
				if c == '\\' && quote != '`' {
					i += 2
					continue
				}
				if c == quote {
					closed = true
					break
				}
				i++
			}
			if !closed {
				return fmt.Sprintf("第 %d 行存在未闭合的字符串（%c）", startLine, quote)
			}
			continue
		}
		if !isBracket(r) {
			continue
		}
		switch r {
		case '(', '[', '{':
			stack = append(stack, open{ch: r, line: line})
		default:
			if len(stack) == 0 {
				return fmt.Sprintf("第 %d 行出现多余的闭合符号 %c", line, r)
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.ch != pairs[r] {
				return fmt.Sprintf("第 %d 行的 %c 与第 %d 行的 %c 不匹配", line, r, top.line, top.ch)
			}
		}
	}
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		return fmt.Sprintf("第 %d 行的 %c 未闭合", top.line, top.ch)
	}
	return ""
}

// checkGoSyntax 用标准库 go/parser 做真实语法检查（零依赖的编译级校验）。
func checkGoSyntax(rel, content string) string {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, rel, content, parser.AllErrors); err != nil {
		return fmt.Sprintf("%s：%s", rel, err.Error())
	}
	return ""
}

// checkJSONSyntax 用 encoding/json 校验 JSON 文本。
func checkJSONSyntax(content string) string {
	var v any
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return err.Error()
	}
	return ""
}

// checkYAMLSyntax 简易 YAML 校验：Tab 缩进、括号配平、行首结构合法。
//
// 不引入第三方 YAML 库（依赖白名单限制），因此只做"结构性"检查：这已能覆盖
// 补丁破坏 YAML 的绝大多数情形（缩进错乱、未闭合列表/映射）。
func checkYAMLSyntax(content string) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	for i, raw := range lines {
		lineNo := i + 1
		if strings.ContainsRune(raw, '\t') {
			return fmt.Sprintf("第 %d 行包含 Tab 缩进（YAML 要求空格）", lineNo)
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "---") || strings.HasPrefix(trimmed, "...") {
			continue
		}
		if idx := strings.Index(trimmed, " #"); idx > 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			continue
		}
		if !strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "-") {
			return fmt.Sprintf("第 %d 行缺少键值分隔符 `:`：%q", lineNo, truncateRunes(trimmed, 40))
		}
	}
	return checkBracketBalance(content)
}

// checkDangerScanFiles 危险操作扫描（只扫补丁涉及的文件，避免把仓库既有代码误判为风险）。
func checkDangerScanFiles(targets []staticTarget, patched map[string]string) domain.VerificationCheck {
	check := domain.VerificationCheck{
		Name: checkDangerScan, Passed: true,
		Output: fmt.Sprintf("已扫描 %d 个变更文件", len(targets)),
	}
	var hits []string
	for _, t := range targets {
		text, ok := patched[t.rel]
		if !ok {
			raw, err := os.ReadFile(t.abs)
			if err != nil {
				continue
			}
			text = string(raw)
		}
		lower := strings.ToLower(text)
		for _, dp := range dangerPatterns {
			if strings.Contains(lower, dp.pattern) {
				line := lineOfPattern(text, dp.pattern)
				hits = append(hits, fmt.Sprintf("%s:%d 命中危险操作 %q（%s）", t.rel, line, dp.pattern, dp.note))
			}
		}
	}
	if len(hits) > 0 {
		check.Passed = false
		check.Output = strings.Join(hits, "\n")
	}
	return check
}

// lineOfPattern 返回模式首次出现的行号（大小写不敏感，找不到返回 1）。
func lineOfPattern(text, pattern string) int {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, strings.ToLower(pattern))
	if idx < 0 {
		return 1
	}
	return strings.Count(text[:idx], "\n") + 1
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// sanitizeRunID 把任意字符串安全化为单层目录名，防止 runId 携带路径分隔符造成越界。
func sanitizeRunID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "ws-" + uuid.NewString()
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			if r == '.' && b.Len() == 0 {
				continue // 不允许以 . 开头（隐藏目录 / . / ..）
			}
			if r == '.' && strings.HasSuffix(b.String(), ".") {
				continue
			}
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" || out == "." || out == ".." {
		return "ws-" + uuid.NewString()
	}
	if utf8.RuneCountInString(out) > 96 {
		out = string([]rune(out)[:96])
	}
	return out
}

// repoKeyOf 取仓库的业务唯一键（Key 优先，回退 ID）。
func repoKeyOf(repo *domain.Repository) string {
	if repo == nil {
		return ""
	}
	if strings.TrimSpace(repo.Key) != "" {
		return repo.Key
	}
	return repo.ID
}

// displayRepo 返回补丁所属仓库的展示名。
func displayRepo(p domain.Patch) string {
	switch {
	case p.RepoKey != "":
		return p.RepoKey
	case p.RepositoryID != "":
		return p.RepositoryID
	default:
		return "unknown"
	}
}

// patchLabel 生成补丁的可读标签（用于 check.Command 与日志）。
func patchLabel(p domain.Patch) string {
	action := string(p.Action)
	if action == "" {
		action = string(domain.ActionModify)
	}
	return fmt.Sprintf("%s %s@%s", action, p.FilePath, displayRepo(p))
}

// shortCommit 截断 commit 便于日志展示。
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	if commit == "" {
		return "HEAD"
	}
	return commit
}

// countLines 统计文本行数（空文本为 0）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// orUnknown 空字符串返回"未知"。
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知"
	}
	return s
}

// safeProbeCommand 是 probeCommand 的容错包装。
//
// 兼容性预检本身是"尽力而为"的优化：即便探测逻辑因环境差异出错（例如解析异常、
// 路径不可读、正则不支持），也绝不能让整个验证流程 panic 崩溃——那会把一次
// 可降级的校验失败升级为任务失败。因此这里 recover 并把异常转为"预检不可用"。
func safeProbeCommand(command, workDir string) (result string) {
	defer func() {
		if r := recover(); r != nil {
			result = ""
		}
	}()
	return probeCommand(command, workDir)
}

// probeCommand 校验一条校验命令的工具链是否可用。
//
// 返回空串表示可以尝试执行；否则返回中文说明（用于 SkipReason）。
// 判定规则：
//   - `./xxx`（相对项目路径）：要求工作区中存在该可执行文件，Windows 上允许 `xxx.cmd`；
//   - 其它命令：要求能在 PATH 中解析到可执行文件。
func probeCommand(command, workDir string) string {
	segments := splitCommandSegments(command)
	if len(segments) == 0 {
		return "命令为空"
	}
	bin := segments[0][0]
	if strings.HasPrefix(bin, "./") || strings.HasPrefix(bin, ".\\") {
		rel := strings.TrimPrefix(strings.TrimPrefix(bin, "./"), ".\\")
		candidates := []string{filepath.Join(workDir, filepath.FromSlash(rel))}
		if runtime.GOOS == "windows" {
			candidates = append(candidates,
				filepath.Join(workDir, filepath.FromSlash(rel))+".cmd",
				filepath.Join(workDir, filepath.FromSlash(rel))+".bat")
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return ""
			}
		}
		return fmt.Sprintf("未找到项目内可执行文件 %s", bin)
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Sprintf("未安装命令 %s", bin)
	}
	// 离线 Maven：若项目声明的依赖在本地仓库中缺失，命令必然失败，
	// 这属于"环境能力缺失"而非"补丁有问题"，应降级为静态校验。
	if strings.Contains(bin, "mvn") {
		if missing := offlineMissingMavenDeps(workDir); len(missing) > 0 {
			return fmt.Sprintf("离线模式缺少 Maven 依赖 %s 等 %d 个，无法执行编译校验",
				strings.Join(missing[:minInt(len(missing), 3)], "、"), len(missing))
		}
	}
	return ""
}

// offlineMissingMavenDeps 解析 pom.xml 的 compile/runtime 依赖，返回本地仓库中缺失的坐标。
//
// 只做保守判定：无法确定本地仓库位置或解析 pom 失败时返回空（交由命令真实执行）。
func offlineMissingMavenDeps(workDir string) []string {
	pom := ""
	for _, candidate := range []string{"pom.xml", filepath.Join("..", "pom.xml")} {
		p := filepath.Join(workDir, candidate)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			pom = p
			break
		}
	}
	if pom == "" {
		return nil
	}
	raw, err := os.ReadFile(pom)
	if err != nil {
		return nil
	}
	repo := mavenLocalRepo()
	if repo == "" {
		return nil
	}
	missing := []string{}
	for _, d := range parseMavenDependencies(string(raw)) {
		if d.Scope == "test" || d.Scope == "provided" || d.Optional || d.ArtifactID == "" {
			continue
		}
		dir := filepath.Join(repo, filepath.FromSlash(strings.ReplaceAll(d.GroupID, ".", "/")), d.ArtifactID)
		if !dirExistsWithVersion(dir, d.Version) {
			missing = append(missing, d.GroupID+":"+d.ArtifactID+":"+d.Version)
		}
	}
	return missing
}

// mavenDependency pom.xml 中的一条依赖（仅保留判定所需字段）。
type mavenDependency struct {
	GroupID    string
	ArtifactID string
	Version    string
	Scope      string
	Optional   bool
}

// parseMavenDependencies 用正则从 pom.xml 中提取依赖坐标（不引入 XML 解析依赖）。
//
// 注意：Go 的 regexp 是 RE2，**不支持反向引用**（`\1` 会 panic），
// 因此这里对每个标签分别编译匹配，而不是写成一个带反向引用的通用表达式。
func parseMavenDependencies(text string) []mavenDependency {
	blockRe := regexp.MustCompile(`(?s)<dependency>(.*?)</dependency>`)
	tagRe := regexp.MustCompile(`(?s)<(groupId|artifactId|version|scope|optional)>(.*?)</[a-zA-Z]+>`)
	out := []mavenDependency{}
	for _, block := range blockRe.FindAllStringSubmatch(text, -1) {
		d := mavenDependency{}
		for _, m := range tagRe.FindAllStringSubmatch(block[1], -1) {
			value := strings.TrimSpace(m[2])
			switch m[1] {
			case "groupId":
				d.GroupID = value
			case "artifactId":
				d.ArtifactID = value
			case "version":
				d.Version = value
			case "scope":
				d.Scope = value
			case "optional":
				d.Optional = strings.EqualFold(value, "true")
			}
		}
		// 含 ${...} 占位（父 pom 管理版本）时无法判定，跳过版本以免误报。
		if strings.Contains(d.Version, "${") {
			d.Version = ""
		}
		out = append(out, d)
	}
	return out
}

// mavenLocalRepo 推断本地 Maven 仓库路径（settings.xml 优先，其次默认 ~/.m2/repository）。
func mavenLocalRepo() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	settings := filepath.Join(home, ".m2", "settings.xml")
	if raw, err := os.ReadFile(settings); err == nil {
		re := regexp.MustCompile(`<localRepository>\s*([^<]+?)\s*</localRepository>`)
		if m := re.FindStringSubmatch(string(raw)); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	repo := filepath.Join(home, ".m2", "repository")
	if st, err := os.Stat(repo); err == nil && st.IsDir() {
		return repo
	}
	return ""
}

// dirExistsWithVersion 判断本地仓库中是否存在该依赖（指定版本按前缀匹配）。
func dirExistsWithVersion(dir, version string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	if version == "" {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), version) {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// splitCommandSegments 按 shell 元字符拆分命令（与 runner 保持一致的拆分口径）。
func splitCommandSegments(command string) [][]string {
	parts := strings.FieldsFunc(command, func(r rune) bool {
		switch r {
		case ';', '&', '|', '\n':
			return true
		default:
			return false
		}
	})
	out := make([][]string, 0, len(parts))
	for _, p := range parts {
		fields := strings.Fields(strings.TrimSpace(p))
		if len(fields) == 0 {
			continue
		}
		out = append(out, fields)
	}
	return out
}
