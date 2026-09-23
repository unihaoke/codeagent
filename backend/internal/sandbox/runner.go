package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 错误与默认值
// ---------------------------------------------------------------------------

var (
	// ErrCommandNotAllowed 命令不在白名单内。
	ErrCommandNotAllowed = errors.New("sandbox: 命令不在白名单内")
	// ErrNetworkIsolated 沙箱处于无网络模式，且该命令需要联网。
	ErrNetworkIsolated = errors.New("sandbox: 沙箱处于无网络模式（networkIsolated=true），该命令需要访问网络")
	// ErrCommandEmpty 命令为空。
	ErrCommandEmpty = errors.New("sandbox: 命令为空")
	// ErrCommandBadQuotes 命令引号不配对。
	ErrCommandBadQuotes = errors.New("sandbox: 命令引号不配对")
	// ErrGitWriteOp git 只允许只读子命令。
	ErrGitWriteOp = errors.New("sandbox: git 仅允许只读子命令")
)

const (
	// defaultCommandTimeoutSec 未配置时的默认命令超时。
	defaultCommandTimeoutSec = 120
	// defaultMaxOutputBytes 未配置时的默认输出上限（64 KiB）。
	defaultMaxOutputBytes = 64 << 10
	// truncateHeadBytes 输出截断时保留的头部字节数。
	truncateHeadBytes = 16 << 10
	// truncateTailBytes 输出截断时保留的尾部字节数。
	truncateTailBytes = 8 << 10
	// maxCommandsPerLine 一条命令串最多允许拆分的子命令数，防止构造超长命令链。
	maxCommandsPerLine = 16
)

// allowedExecutables 命令白名单（小写、不含路径前缀）。
//
// 白名单是"半可信代码"隔离的第一道闸门：只有构建/测试/只读检查类的可执行文件
// 允许被 Agent 触发，任何解释器（sh/cmd/powershell 等）都被拒绝，从根本上消除
// "一条命令执行任意代码"的放大面。
var allowedExecutables = map[string]bool{
	"go":      true,
	"mvn":     true,
	"mvnw":    true,
	"gradle":  true,
	"gradlew": true,
	"npm":     true,
	"npx":     true,
	"node":    true,
	"pnpm":    true,
	"yarn":    true,
	"python":  true,
	"python3": true,
	"pytest":  true,
	"php":     true,
	"tsc":     true,
	"cargo":   true,
	"dotnet":  true,
	"git":     true,
	"make":    true,
}

// gitReadOnlySubcommands git 的只读子命令白名单。
var gitReadOnlySubcommands = map[string]bool{
	"status":       true,
	"diff":         true,
	"log":          true,
	"show":         true,
	"rev-parse":    true,
	"ls-files":     true,
	"describe":     true,
	"cat-file":     true,
	"ls-tree":      true,
	"grep":         true,
	"blame":        true,
	"shortlog":     true,
	"symbolic-ref": true,
	"merge-base":   true,
}

// gitWriteSubcommands 显式拒绝的 git 写操作，避免 Agent 改动工作区或仓库状态。
var gitWriteSubcommands = map[string]bool{
	"fetch": true, "pull": true, "push": true, "clone": true, "remote": true,
	"checkout": true, "switch": true, "reset": true, "clean": true, "commit": true,
	"add": true, "rm": true, "mv": true, "merge": true, "rebase": true, "tag": true,
	"branch": true, "config": true, "init": true, "apply": true, "stash": true,
}

// networkInstallerCommands 依赖包管理器键：归一化后的命令前缀 → 中文说明。
// 这些命令即使在白名单内，也会在 networkIsolated 时被直接拒绝。
var networkInstallerCommands = []struct {
	prefix []string
	note   string
}{
	{[]string{"npm", "install"}, "npm 安装依赖"},
	{[]string{"npm", "i"}, "npm 安装依赖"},
	{[]string{"npm", "ci"}, "npm 安装依赖"},
	{[]string{"npm", "update"}, "npm 更新依赖"},
	{[]string{"pnpm", "install"}, "pnpm 安装依赖"},
	{[]string{"pnpm", "i"}, "pnpm 安装依赖"},
	{[]string{"pnpm", "add"}, "pnpm 添加依赖"},
	{[]string{"yarn", "install"}, "yarn 安装依赖"},
	{[]string{"yarn", "add"}, "yarn 安装依赖"},
	{[]string{"mvn", "dependency:go-offline"}, "Maven 拉取依赖"},
	{[]string{"mvn", "dependency:resolve"}, "Maven 解析依赖"},
	{[]string{"mvn", "deploy"}, "Maven 发布"},
	{[]string{"gradle", "dependencies"}, "Gradle 解析依赖"},
	{[]string{"go", "get"}, "go get 拉取依赖"},
	{[]string{"go", "mod", "download"}, "go mod download 拉取依赖"},
	{[]string{"go", "mod", "tidy"}, "go mod tidy 拉取依赖"},
	// 说明：pip 未列入可执行白名单，因此这里只登记 python 解释器形式的 pip 调用。
	{[]string{"python", "-m", "pip", "install"}, "pip 安装依赖"},
	{[]string{"python3", "-m", "pip", "install"}, "pip 安装依赖"},
	{[]string{"cargo", "fetch"}, "cargo 拉取依赖"},
	{[]string{"cargo", "update"}, "cargo 更新依赖"},
	{[]string{"dotnet", "restore"}, "dotnet 还原依赖"},
	{[]string{"php", "composer", "install"}, "composer 安装依赖"},
	{[]string{"composer", "install"}, "composer 安装依赖"},
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner 受限命令执行器：白名单 + 超时 + 输出截断 + 工作区路径约束。
//
// 设计取舍：本执行器**不经过 shell**（不使用 sh -c / cmd /c）。shell 会让
// `;`、`&&`、重定向等元字符获得完整解释权，等于把任意命令执行能力交给模型
// 生成的文本。这里改为自行按元字符拆分后用 exec 直接启动进程，等价于只允许多条
// 白名单命令顺序执行：任何一条失败立即停止并返回该条错误，由上层决定降级。
type Runner struct {
	cfg config.SandboxConfig
	log *logx.Logger

	mu    sync.Mutex
	runs  int64
	fails int64
}

// NewRunner 创建受限命令执行器。
func NewRunner(cfg config.SandboxConfig) *Runner {
	return &Runner{cfg: cfg, log: logx.Nop()}
}

// WithLogger 注入结构化日志器（返回自身，便于链式调用）。
func (r *Runner) WithLogger(l *logx.Logger) *Runner {
	if l != nil {
		r.log = l
	}
	return r
}

// Stats 返回执行统计（供可观测使用）。
func (r *Runner) Stats() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return map[string]any{
		"runs":              r.runs,
		"failures":          r.fails,
		"allowedCommand":    len(allowedExecutables),
		"networkIsolated":   r.cfg.NetworkIsolated,
		"commandTimeoutSec": r.commandTimeout(),
		"maxOutputBytes":    r.maxOutput(),
	}
}

func (r *Runner) commandTimeout() int {
	if r.cfg.CommandTimeoutSec <= 0 {
		return defaultCommandTimeoutSec
	}
	return r.cfg.CommandTimeoutSec
}

func (r *Runner) maxOutput() int {
	if r.cfg.MaxOutputBytes <= 0 {
		return defaultMaxOutputBytes
	}
	return r.cfg.MaxOutputBytes
}

// Run 在 dir 下执行一条命令。返回输出与耗时（毫秒）。命令必须通过白名单校验。
//
// 支持带参数的命令（按空白切分并去掉引号）。命令串中含 shell 元字符
// （&&、||、;、|、>、`、$(、&）时会被**拆分**为多条顺序执行，全程不经过 shell。
// 超时取 cfg.CommandTimeoutSec，输出按 cfg.MaxOutputBytes 截断（保留头尾）。
func (r *Runner) Run(ctx context.Context, dir, command string) (out string, durationMS int64, err error) {
	start := time.Now()
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	defer func() {
		durationMS = time.Since(start).Milliseconds()
		if err != nil {
			r.mu.Lock()
			r.fails++
			r.mu.Unlock()
		}
	}()

	if strings.TrimSpace(command) == "" {
		return "", 0, ErrCommandEmpty
	}
	if ctx == nil {
		ctx = context.Background()
	}
	segments, err := splitCommandChain(command)
	if err != nil {
		return "", 0, err
	}
	if len(segments) == 0 {
		return "", 0, ErrCommandEmpty
	}

	timeout := time.Duration(r.commandTimeout()) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}

	var buf strings.Builder
	// 预检：所有子命令都必须通过校验，避免"前半段已执行、后半段才被拒"的副作用。
	for _, seg := range segments {
		if err := r.check(seg); err != nil {
			r.log.Warn("沙箱命令被拒绝", "command", seg.raw, "原因", err.Error())
			return "", 0, err
		}
	}
	for i, seg := range segments {
		if len(segments) > 1 {
			buf.WriteString(fmt.Sprintf("$ %s\n", seg.raw))
		}
		segOut, runErr := r.runOne(ctx, dir, seg, timeout)
		buf.WriteString(segOut)
		if runErr != nil {
			reason := runErr.Error()
			if errors.Is(runErr, context.DeadlineExceeded) {
				reason = fmt.Sprintf("命令执行超时（>%s）：%s", timeout, seg.raw)
			}
			if len(segments) > 1 {
				reason = fmt.Sprintf("%s（第 %d/%d 条子命令，后续子命令已终止）", reason, i+1, len(segments))
			}
			buf.WriteString("错误: " + reason + "\n")
			return truncateOutput(buf.String(), r.maxOutput()), time.Since(start).Milliseconds(), fmt.Errorf("命令执行失败: %s", reason)
		}
	}
	return truncateOutput(buf.String(), r.maxOutput()), time.Since(start).Milliseconds(), nil
}

// runOne 执行拆分后的单条命令。
func (r *Runner) runOne(ctx context.Context, dir string, seg commandSegment, timeout time.Duration) (string, error) {
	execPath, args, err := r.resolveExecutable(seg)
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, execPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// 非交互 + 无色输出：避免命令等待输入、避免 ANSI 转义污染日志。
	cmd.Env = append(os.Environ(),
		"CI=true",
		"GIT_TERMINAL_PROMPT=0",
		"NO_COLOR=1",
		"FORCE_COLOR=0",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return buf.String(), fmt.Errorf("启动命令失败(%s): %w", seg.raw, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-runCtx.Done():
		// 超时/取消：杀掉进程树并等待 Wait 返回，确保不泄漏进程与管道。
		killProcessTree(cmd)
		err = <-done
	}
	if err == nil {
		return buf.String(), nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return buf.String(), context.DeadlineExceeded
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return buf.String(), context.Canceled
	}
	return buf.String(), fmt.Errorf("%s: %w", seg.raw, err)
}

// resolveExecutable 把逻辑命令名解析为真实可执行文件路径（含 Windows 兼容处理）。
func (r *Runner) resolveExecutable(seg commandSegment) (string, []string, error) {
	name := seg.name
	args := append([]string(nil), seg.args...)

	if isGitCommand(name) {
		if err := checkGitSubcommand(args); err != nil {
			return "", nil, err
		}
	}

	// 目录包装器：./mvnw、./gradlew（Windows 上退化为 mvnw.cmd / gradlew.cmd）。
	if trimmed := strings.TrimPrefix(name, "./"); trimmed != name || strings.ContainsAny(name, `/\`) {
		base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		base = strings.TrimSuffix(base, ".cmd")
		base = strings.TrimSuffix(base, ".bat")
		if !allowedExecutables[strings.ToLower(base)] {
			return "", nil, fmt.Errorf("%w: 不允许执行 %q", ErrCommandNotAllowed, seg.raw)
		}
		path := name
		if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(path), ".cmd") && !strings.HasSuffix(strings.ToLower(path), ".bat") && !strings.HasSuffix(strings.ToLower(path), ".exe") {
			if _, statErr := os.Stat(path + ".cmd"); statErr == nil {
				path += ".cmd"
			}
		}
		return path, args, nil
	}

	lower := strings.ToLower(name)
	// python3 不可用时退化为 python（只在确实探测不到 python3 时才退化）。
	if lower == "python3" {
		if p, lookErr := exec.LookPath("python3"); lookErr == nil {
			return p, args, nil
		}
		if p, lookErr := exec.LookPath("python"); lookErr == nil {
			return p, args, nil
		}
		return "", nil, fmt.Errorf("环境未安装 python3/python，无法执行: %s", seg.raw)
	}
	path, lookErr := exec.LookPath(name)
	if lookErr != nil {
		// Windows 下 .cmd/.bat 包装器需要显式扩展名探测。
		if runtime.GOOS == "windows" {
			for _, ext := range []string{".cmd", ".bat", ".exe"} {
				if p, e := exec.LookPath(name + ext); e == nil {
					return p, args, nil
				}
			}
		}
		return "", nil, fmt.Errorf("环境未安装 %s，无法执行: %s", name, seg.raw)
	}
	return path, args, nil
}

// check 校验单条子命令：白名单 → git 只读 → 网络隔离。
func (r *Runner) check(seg commandSegment) error {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(seg.name, "\\", "/")))
	base = strings.TrimSuffix(base, ".cmd")
	base = strings.TrimSuffix(base, ".bat")
	base = strings.TrimSuffix(base, ".exe")
	if strings.HasPrefix(seg.name, "./") || strings.HasPrefix(seg.name, ".\\") {
		base = strings.TrimSuffix(base, ".cmd")
	}
	base = strings.TrimPrefix(base, ".")
	if !allowedExecutables[base] {
		return fmt.Errorf("%w: %q（允许: %s）", ErrCommandNotAllowed, seg.name, strings.Join(allowedList(), ", "))
	}
	if base == "git" {
		if err := checkGitSubcommand(seg.args); err != nil {
			return err
		}
	}
	if r.cfg.NetworkIsolated {
		if note, needNet := requiresNetwork(seg); needNet {
			return fmt.Errorf("%w：%s。设计说明：无网络沙箱中依赖拉取必然失败，"+
				"直接拒绝可避免长时间无谓等待，交由上层降级为静态校验（Passed 仍可判定，Degraded=true 并在 Log 中说明）",
				ErrNetworkIsolated, note)
		}
	}
	return nil
}

// allowedList 返回排序后的白名单，用于错误提示。
func allowedList() []string {
	out := make([]string, 0, len(allowedExecutables))
	for k := range allowedExecutables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 命令解析
// ---------------------------------------------------------------------------

// commandSegment 拆分后的一条子命令。
type commandSegment struct {
	raw  string
	name string
	args []string
}

// splitCommandChain 按 shell 元字符把命令串拆成多条子命令（不使用 shell 执行）。
func splitCommandChain(command string) ([]commandSegment, error) {
	normalized := strings.ReplaceAll(command, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\\\n", " ")
	normalized = strings.ReplaceAll(normalized, "\n", " && ")
	tokens, err := tokenizeCommand(normalized)
	if err != nil {
		return nil, err
	}
	var (
		segs []commandSegment
		cur  []string
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		segs = append(segs, commandSegment{
			raw:  strings.Join(cur, " "),
			name: cur[0],
			args: append([]string(nil), cur[1:]...),
		})
		cur = nil
	}
	for _, tok := range tokens {
		if tok == "&&" || tok == "||" || tok == ";" || tok == "|" {
			flush()
			// `||` 的语义是"失败才执行右边"，本执行器不支持条件回退，
			// 按顺序执行以便上层拿到确定的失败信息。
			continue
		}
		cur = append(cur, tok)
	}
	flush()
	if len(segs) > maxCommandsPerLine {
		return nil, fmt.Errorf("sandbox: 单条命令串拆分子命令过多（%d > %d），已拒绝", len(segs), maxCommandsPerLine)
	}
	return segs, nil
}

// tokenizeCommand 按空白切分并去掉引号，同时识别 shell 元字符。
func tokenizeCommand(s string) ([]string, error) {
	var (
		tokens  []string
		cur     strings.Builder
		quote   byte
		started bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, cur.String())
			cur.Reset()
			started = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			started = true
		case ' ', '\t':
			flush()
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				flush()
				tokens = append(tokens, "&&")
				i++
				continue
			}
			flush()
			tokens = append(tokens, "&")
		case '|':
			flush()
			if i+1 < len(s) && s[i+1] == '|' {
				tokens = append(tokens, "||")
				i++
			} else {
				tokens = append(tokens, "|")
			}
		case ';':
			flush()
			tokens = append(tokens, ";")
		case '>':
			flush()
			// 重定向一律拒绝：写文件属于补丁变更的职责，不应由校验命令完成。
			return nil, fmt.Errorf("sandbox: 命令中包含重定向 %q，已拒绝", s[i:min(i+2, len(s))])
		case '<':
			flush()
			return nil, fmt.Errorf("sandbox: 命令中包含输入重定向 %q，已拒绝", s[i:min(i+2, len(s))])
		case '`':
			return nil, fmt.Errorf("sandbox: 命令中包含命令替换 %q，已拒绝", "`")
		case '$':
			if i+1 < len(s) && s[i+1] == '(' {
				return nil, fmt.Errorf("sandbox: 命令中包含命令替换 %q，已拒绝", "$(")
			}
			cur.WriteByte(c)
			started = true
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("%w: %q", ErrCommandBadQuotes, s)
	}
	flush()
	return tokens, nil
}

// requiresNetwork 判断子命令是否需要联网，返回中文说明。
func requiresNetwork(seg commandSegment) (string, bool) {
	normalized := make([]string, 0, len(seg.args)+1)
	normalized = append(normalized, strings.ToLower(filepath.Base(strings.ReplaceAll(seg.name, "\\", "/"))))
	for _, a := range seg.args {
		normalized = append(normalized, strings.ToLower(a))
	}
	for _, item := range networkInstallerCommands {
		if hasPrefixFold(normalized, item.prefix) {
			return item.note, true
		}
	}
	if isGitCommand(seg.name) {
		for _, a := range seg.args {
			if gitWriteSubcommands[strings.ToLower(a)] {
				return "git " + strings.ToLower(a) + " 需要访问远端仓库", true
			}
		}
	}
	return "", false
}

func hasPrefixFold(haystack, prefix []string) bool {
	if len(prefix) > len(haystack) {
		return false
	}
	for i, p := range prefix {
		if haystack[i] != p {
			return false
		}
	}
	return true
}

func isGitCommand(name string) bool {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	base = strings.TrimSuffix(base, ".exe")
	return base == "git" || base == "git.exe"
}

// checkGitSubcommand 校验 git 子命令属于只读白名单。
func checkGitSubcommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: 未指定 git 子命令", ErrGitWriteOp)
	}
	for _, a := range args {
		// 跳过全局选项，如 -c core.autocrlf=false
		if strings.HasPrefix(a, "-") {
			continue
		}
		sub := strings.ToLower(a)
		if gitReadOnlySubcommands[sub] {
			return nil
		}
		if gitWriteSubcommands[sub] {
			return fmt.Errorf("%w: git %s", ErrGitWriteOp, sub)
		}
		return fmt.Errorf("%w: git %s 不在只读白名单内", ErrGitWriteOp, sub)
	}
	return fmt.Errorf("%w: 未指定 git 子命令", ErrGitWriteOp)
}

// ---------------------------------------------------------------------------
// 输出截断
// ---------------------------------------------------------------------------

// truncateOutput 把输出截断到 limit 字节：保留头尾，中间插入截断提示。
func truncateOutput(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	notice := "\n... (输出已截断 %d 字节) ...\n"
	head := truncateHeadBytes
	tail := truncateTailBytes
	if head+tail+len(notice) >= limit {
		head = limit / 2
		tail = limit - head
		notice = "... (输出已截断 %d 字节) ..."
	}
	if head > len(s) {
		head = len(s)
	}
	if tail > len(s)-head {
		tail = len(s) - head
	}
	dropped := len(s) - head - tail
	if dropped <= 0 {
		return s
	}
	return s[:head] + fmt.Sprintf(notice, dropped) + s[len(s)-tail:]
}

// killProcessTree 尽力回收进程（Windows 上连子进程一起回收，避免 go/mvn 派生进程泄漏）。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		// taskkill 失败时（例如进程已退出）退回普通 Kill，绝不阻塞。
		kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
		kill.Stdout = io.Discard
		kill.Stderr = io.Discard
		if err := kill.Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}
