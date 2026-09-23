// 本文件实现 domain.SourceResolver：版本锁定 + 懒加载 + 文件级 LRU/TTL 缓存。
//
// 缓存维度：
//   - 文件缓存：repoID|commit|path  → *domain.CodeSlice（内容 + 符号 + import）
//   - 树缓存：  repoID|commit       → []domain.FileNode（供线索解析与Tree复用）
//   - 元数据缓存：repoID|ref        → commit（减少重复 git 调用）
//
// 全部缓存并发安全，容量与 TTL 由 config.SourceConfig 控制（0 表示不过期）。
package source

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// SecretProvider 提供仓库解密后的访问凭证（由 env 层实现）。
//
// 定义在 source 包内以避免与 env 层形成依赖环；env 层只需实现本接口即可注入。
type SecretProvider interface {
	// RepoSecret 返回仓库访问凭证；无凭证时返回空串与 nil 错误。
	RepoSecret(ctx context.Context, tenantID, repoID string) (username, secret string, err error)
}

// NameKey 返回仓库的稳定缓存键（兼容 domain.Repository.ID / Key 混用的调用方）。
func NameKey(repoID, key string) string {
	key = strings.TrimSpace(key)
	if key != "" {
		return key
	}
	return strings.TrimSpace(repoID)
}

// ---------------------------------------------------------------------------
// 内部错误
// ---------------------------------------------------------------------------

var (
	// errCacheMiss 缓存未命中（内部使用）。
	errCacheMiss = errors.New("cache miss")
	// ErrUnknownRepository 仓库在索引中不存在或不属于该租户。
	ErrUnknownRepository = errors.New("仓库不存在或不属于当前租户")
	// ErrRepoDisabled 仓库已被停用，禁止加载源码。
	ErrRepoDisabled = errors.New("仓库已停用")
	// ErrBudgetExhaustedWrap 预留包装位（保证 errors.Is(err, ErrBudgetExhausted) 可用）。
	ErrBudgetExhaustedWrap = fmt.Errorf("源码加载: %w", ErrBudgetExhausted)
)

// ---------------------------------------------------------------------------
// LRU + TTL 缓存
// ---------------------------------------------------------------------------

// fileCache 带容量上限与 TTL 的并发安全 LRU 缓存。
//
// 命中/未命中通过回调外抛，便于 Resolver 聚合真实统计；返回内容始终为深拷贝，
// 避免调用方修改缓存内的切片导致数据污染。
type fileCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	entries  map[string]*list.Element
	order    *list.List // 队首 = 最近使用
	bytes    int64
	onHit    func()
	onMiss   func()
	onEvict  func()
}

// cacheEntry 缓存条目。
type cacheEntry struct {
	key      string
	value    *domain.CodeSlice
	bytes    int64
	expireAt time.Time
}

// newFileCache 创建缓存；capacity <= 0 时使用 512，ttl <= 0 表示永不过期。
func newFileCache(capacity int, ttl time.Duration) *fileCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &fileCache{
		capacity: capacity,
		ttl:      ttl,
		entries:  make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

// cloneSlice 深拷贝代码切片（切片字段独立分配）。
func cloneSlice(in *domain.CodeSlice) *domain.CodeSlice {
	if in == nil {
		return nil
	}
	out := *in
	out.FocusLines = append([]int{}, in.FocusLines...)
	out.Symbols = append([]string{}, in.Symbols...)
	out.Imports = append([]string{}, in.Imports...)
	return &out
}

// estimateBytes 估算条目内存占用（按内容字符数 + 元数据开销）。
func estimateBytes(s *domain.CodeSlice) int64 {
	if s == nil {
		return 0
	}
	n := int64(len(s.Content) + 256)
	for _, v := range s.Symbols {
		n += int64(len(v) + 8)
	}
	for _, v := range s.Imports {
		n += int64(len(v) + 8)
	}
	return n
}

// Get 读取缓存，命中返回深拷贝。
func (c *fileCache) Get(key string) (*domain.CodeSlice, error) {
	if c == nil {
		return nil, errCacheMiss
	}
	c.mu.Lock()
	el, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		if c.onMiss != nil {
			c.onMiss()
		}
		return nil, errCacheMiss
	}
	e := el.Value.(*cacheEntry)
	if !e.expireAt.IsZero() && time.Now().After(e.expireAt) {
		c.removeLocked(el)
		c.mu.Unlock()
		if c.onEvict != nil {
			c.onEvict()
		}
		if c.onMiss != nil {
			c.onMiss()
		}
		return nil, errCacheMiss
	}
	c.order.MoveToFront(el)
	val := cloneSlice(e.value)
	c.mu.Unlock()
	if c.onHit != nil {
		c.onHit()
	}
	return val, nil
}

// Put 写入缓存（超容量时按 LRU 淘汰）。
func (c *fileCache) Put(key string, val *domain.CodeSlice) {
	if c == nil || val == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		e := el.Value.(*cacheEntry)
		c.bytes -= e.bytes
		e.value = cloneSlice(val)
		e.bytes = estimateBytes(val)
		c.bytes += e.bytes
		e.expireAt = c.expiry()
		c.order.MoveToFront(el)
		return
	}
	e := &cacheEntry{key: key, value: cloneSlice(val), bytes: estimateBytes(val), expireAt: c.expiry()}
	c.entries[key] = c.order.PushFront(e)
	c.bytes += e.bytes
	for len(c.entries) > c.capacity {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.removeLocked(back)
		if c.onEvict != nil {
			c.onEvict()
		}
	}
}

// expiry 计算过期时间（ttl <= 0 表示不过期，返回零值）。
func (c *fileCache) expiry() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

// removeLocked 移除元素并维护字节计数。
func (c *fileCache) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	e, ok := el.Value.(*cacheEntry)
	if ok {
		c.bytes -= e.bytes
		if c.bytes < 0 {
			c.bytes = 0
		}
		delete(c.entries, e.key)
	}
	c.order.Remove(el)
}

// delete 按键删除单个条目。
func (c *fileCache) delete(key string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return false
	}
	c.removeLocked(el)
	return true
}

// purgePrefix 删除键前缀匹配的全部条目，返回删除数量。
func (c *fileCache) purgePrefix(prefix string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if prefix == "" {
		n := len(c.entries)
		c.entries = map[string]*list.Element{}
		c.order.Init()
		c.bytes = 0
		return n
	}
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if el, ok := c.entries[k]; ok {
			c.removeLocked(el)
		}
	}
	return len(keys)
}

// len 返回当前条目数。
func (c *fileCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// totalBytes 返回当前估算占用字节数。
func (c *fileCache) totalBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// ---------------------------------------------------------------------------
// Resolver
// ---------------------------------------------------------------------------

// Resolver domain.SourceResolver 的默认实现。
type Resolver struct {
	cfg     config.SourceConfig
	store   *store.Store
	secrets SecretProvider
	log     *logx.Logger
	git     *GitRepoManager

	files *fileCache
	trees *fileCache
	meta  *fileCache

	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64

	notesMu sync.Mutex
	notes   []string
}

// 编译期断言：Resolver 必须实现 domain.SourceResolver。
var _ domain.SourceResolver = (*Resolver)(nil)

// NewResolver 创建默认源码解析器。
//
// st 用于读取仓库元数据（可为 nil，此时调用方必须通过 domain.RepoRef.Repository 传入仓库）；
// secrets 可为 nil（无凭证，仅支持匿名/本地仓库）。
func NewResolver(cfg config.SourceConfig, st *store.Store, secrets SecretProvider, log *logx.Logger) *Resolver {
	if log == nil {
		log = logx.Nop()
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		cfg.CacheDir = filepath.Join(os.TempDir(), "codeagent-cache")
	}
	ttl := time.Duration(cfg.CacheTTLSec) * time.Second
	cap := cfg.CacheCapacity
	if cap <= 0 {
		cap = 1000
	}
	r := &Resolver{
		cfg:     cfg,
		store:   st,
		secrets: secrets,
		log:     log,
		git:     NewGitRepoManager(cfg, log),
		files:   newFileCache(cap, ttl),
		trees:   newFileCache(cap/2+16, ttl),
		meta:    newFileCache(1024, ttl),
	}
	hit := func() { r.hits.Add(1) }
	miss := func() { r.misses.Add(1) }
	evict := func() { r.evictions.Add(1) }
	r.files.onHit, r.files.onMiss, r.files.onEvict = hit, miss, evict
	r.trees.onHit, r.trees.onMiss, r.trees.onEvict = hit, miss, evict
	r.meta.onHit, r.meta.onMiss, r.meta.onEvict = hit, miss, evict
	return r
}

// Close 释放解析器资源（git 管理器内部状态 + 全部缓存）。
func (r *Resolver) Close() error {
	if r.files != nil {
		r.files.purgePrefix("")
	}
	if r.trees != nil {
		r.trees.purgePrefix("")
	}
	if r.meta != nil {
		r.meta.purgePrefix("")
	}
	return r.git.Close()
}

// LastLoadNotes 返回最近一次 Load 的裁剪/降级说明（人类可读，中文）。
func (r *Resolver) LastLoadNotes() []string {
	r.notesMu.Lock()
	defer r.notesMu.Unlock()
	return append([]string{}, r.notes...)
}

// setNotes 覆盖本次加载说明。
func (r *Resolver) setNotes(notes []string) {
	r.notesMu.Lock()
	r.notes = notes
	r.notesMu.Unlock()
}

// addNote 追加加载说明（并发安全）。
func (r *Resolver) addNote(note string) {
	if strings.TrimSpace(note) == "" {
		return
	}
	r.notesMu.Lock()
	if len(r.notes) < 64 {
		r.notes = append(r.notes, note)
	}
	r.notesMu.Unlock()
}

// Materialize 把指定 commit 的仓库检出到 destRoot/<repoKey>，供沙箱层使用。
func (r *Resolver) Materialize(ctx context.Context, ref domain.RepoRef, destRoot string) (string, error) {
	repo := ref.Repository
	if repo == nil {
		return "", ErrNoRepository
	}
	if strings.TrimSpace(destRoot) == "" {
		destRoot = r.cfg.WorkspaceDir
	}
	if strings.TrimSpace(destRoot) == "" {
		return "", errors.New("未配置工作区目录")
	}
	commit, dir, err := r.resolveLocked(ctx, repo, ref.Ref, ref.Commit)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(destRoot, safeSegment(repo.Key))
	if head, herr := r.git.Head(ctx, filepath.Join(dest, ".git")); herr == nil && head == commit {
		r.log.Debug("工作区已存在且 commit 一致，直接复用", "repo", repo.Key, "commit", shortSHA(commit))
		return dest, nil
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", fmt.Errorf("清理旧工作区失败: %w", err)
	}
	if err := r.git.Checkout(ctx, dir, commit, dest); err != nil {
		return "", err
	}
	r.log.Info("源码物化完成", "repo", repo.Key, "commit", shortSHA(commit), "dir", dest)
	return dest, nil
}

// ---------------------------------------------------------------------------
// Resolve：版本锁定
// ---------------------------------------------------------------------------

// Resolve 解析仓库版本并返回锁定的 commit（写入元数据缓存以减少重复 git 调用）。
func (r *Resolver) Resolve(ctx context.Context, ref domain.RepoRef) (string, error) {
	repo, err := r.repositoryFor(ctx, ref)
	if err != nil {
		return "", err
	}
	commit, _, err := r.resolveLocked(ctx, repo, ref.Ref, ref.Commit)
	return commit, err
}

// resolveLocked 锁定版本并返回 (commit, 镜像目录)。
//
// 顺序：元数据缓存 → 凭证 → EnsureMirror → ResolveCommit；refCommit 非空时只校验存在性。
func (r *Resolver) resolveLocked(ctx context.Context, repo *domain.Repository, refIn, refCommit string) (string, string, error) {
	if repo == nil {
		return "", "", ErrNoRepository
	}
	if err := r.assertRepoUsable(repo); err != nil {
		return "", "", err
	}
	cacheKey := metaCacheKey(repo.TenantID, repo.ID, firstNonEmpty(refCommit, refIn))
	if v, err := r.meta.Get(cacheKey); err == nil && v != nil {
		if dir, ok := r.mirrorIfPresent(repo); ok {
			return v.Commit, dir, nil
		}
	}

	secret := r.secretFor(ctx, repo)
	dir, err := r.git.EnsureMirror(ctx, repo, secret)
	if err != nil {
		return "", "", err
	}
	rev := strings.TrimSpace(refCommit)
	if rev == "" {
		rev = strings.TrimSpace(refIn)
	}
	commit, err := r.git.ResolveCommit(ctx, dir, rev)
	if err != nil {
		return "", "", err
	}
	r.meta.Put(cacheKey, &domain.CodeSlice{Commit: commit})
	r.log.Info("源码版本锁定", "repo", repo.Key, "ref", firstNonEmpty(rev, "HEAD"), "commit", shortSHA(commit))
	return commit, dir, nil
}

// mirrorIfPresent 返回已存在的镜像目录（用于避免缓存命中后重复 clone）。
func (r *Resolver) mirrorIfPresent(repo *domain.Repository) (string, bool) {
	dir := r.git.MirrorDir(repo.TenantID, repo.ID)
	if isGitDir(dir) {
		return dir, true
	}
	return "", false
}

// secretFor 通过 SecretProvider 取凭证（失败时降级为匿名访问并记录日志）。
func (r *Resolver) secretFor(ctx context.Context, repo *domain.Repository) string {
	if r.secrets == nil || repo == nil || repo.CredentialID == "" {
		return ""
	}
	_, secret, err := r.secrets.RepoSecret(ctx, repo.TenantID, repo.ID)
	if err != nil {
		r.log.Warn("获取仓库凭证失败，降级为匿名访问", "repo", repo.Key, "err", err.Error())
		return ""
	}
	return secret
}

// assertRepoUsable 校验仓库状态。
func (r *Resolver) assertRepoUsable(repo *domain.Repository) error {
	if repo.Status == domain.RepoDisabled {
		return fmt.Errorf("%w: %s", ErrRepoDisabled, repo.Key)
	}
	return nil
}

// repositoryFor 解析调用方给定的仓库（优先使用入参中的完整对象，其次查存储）。
func (r *Resolver) repositoryFor(ctx context.Context, ref domain.RepoRef) (*domain.Repository, error) {
	_ = ctx
	if ref.Repository != nil {
		return ref.Repository, nil
	}
	return nil, ErrNoRepository
}

// ---------------------------------------------------------------------------
// Load：懒加载
// ---------------------------------------------------------------------------

// repoMeta 一次加载所需的仓库定位信息（可来自 Store 或调用方入参）。
type repoMeta struct {
	repoID   string
	tenantID string
	repo     *domain.Repository
	commit   string
	dir      string
}

// repoByID 从存储层补齐仓库元数据（供需要按 ID 反查的场景使用）。
func (r *Resolver) repoByID(meta *repoMeta) (*domain.Repository, error) {
	if meta.repo != nil {
		return meta.repo, nil
	}
	if r.store == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRepository, meta.repoID)
	}
	repo, ok := r.store.GetRepo(meta.tenantID, meta.repoID)
	if !ok || repo == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRepository, meta.repoID)
	}
	if repo.Status == domain.RepoDisabled {
		return nil, fmt.Errorf("%w: %s", ErrRepoDisabled, repo.Key)
	}
	meta.repo = repo
	return repo, nil
}

// metaCacheKey 元数据缓存键：仓库 + 请求版本。
func metaCacheKey(tenantID, repoID, ref string) string {
	return fmt.Sprintf("%s|%s|%s", tenantID, repoID, strings.TrimSpace(ref))
}

// fileCacheKey 文件缓存键：仓库 + commit + 路径。
func fileCacheKey(repoID, commit, path string) string {
	return repoID + "|" + commit + "|" + normalizeRepoPath(path)
}

// treeCacheKey 文件树缓存键：仓库 + commit。
func treeCacheKey(repoID, commit string) string {
	return repoID + "|" + commit
}

// hintCandidate 一个待加载的候选文件。
type hintCandidate struct {
	path     string
	reason   string
	line     int
	priority float64
}

// Load 按线索懒加载故障关联的少量文件。
//
// 严格受 ref.Budget（domain.LoadBudget）约束：文件数、累计字符数、单文件大小与路径深度。
// 预算耗尽时**返回已加载部分且 error 为 nil**，裁剪原因写入 LastLoadNotes()，
// 并体现为切片的 Truncated 字段与 Reason 说明；调用方可用 errors.Is 判断
// ErrBudgetExhausted 语义（本实现不主动抛出该错误，以免丢弃可用证据）。
func (r *Resolver) Load(ctx context.Context, ref domain.RepoRef, hints []domain.FileHint) ([]domain.CodeSlice, error) {
	if ref.Repository == nil {
		return nil, ErrNoRepository
	}
	budget := effectiveBudget(ref.Budget)
	r.setNotes(nil)

	meta := &repoMeta{repoID: ref.Repository.ID, tenantID: ref.Repository.TenantID, repo: ref.Repository}
	commit, dir, err := r.resolveLocked(ctx, ref.Repository, ref.Ref, ref.Commit)
	if err != nil {
		return nil, err
	}
	meta.commit, meta.dir = commit, dir

	ordered := sortHintsByPriority(hints)
	tree, err := r.treeNodeList(ctx, meta)
	if err != nil {
		return nil, err
	}

	if len(ordered) == 0 {
		r.addNote("未提供文件线索（hints 为空），已跳过源码加载")
		return []domain.CodeSlice{}, nil
	}

	selected := make([]hintCandidate, 0, len(ordered))
	seen := map[string]bool{}
	for _, h := range ordered {
		// 路径深度预算：过深的线索直接跳过并解释原因。
		if pathDepth(h.Path) > budget.MaxDepth {
			r.addNote(fmt.Sprintf("线索 %s 路径深度超过 MaxDepth=%d，已跳过", h.Path, budget.MaxDepth))
			continue
		}
		// 单文件体积预算：明显超限的线索直接跳过并解释原因。
		if r.filesTooLarge(tree, h.Path, budget.MaxFileSize) {
			r.addNote(fmt.Sprintf("线索 %s 超出单文件预算 MaxFileSize=%d 字节，已跳过", h.Path, budget.MaxFileSize))
			continue
		}
		for _, c := range r.resolveHintPaths(ctx, meta, tree, h) {
			if seen[c.path] {
				continue
			}
			seen[c.path] = true
			selected = append(selected, c)
		}
	}

	out := make([]domain.CodeSlice, 0, minInt(len(selected), budget.MaxFiles))
	filesUsed, charsUsed := 0, 0
	truncatedAny := false
	stop := false

	for _, cand := range selected {
		if stop {
			break
		}
		sl, truncated, isNew, lerr := r.loadOne(ctx, meta, cand, budget)
		if lerr != nil {
			r.addNote(fmt.Sprintf("跳过 %s：%v", cand.path, lerr))
			continue
		}
		if sl == nil {
			continue
		}
		if filesUsed >= budget.MaxFiles {
			stop = true
			truncatedAny = true
			r.addNote(fmt.Sprintf("已达文件数预算 MaxFiles=%d，剩余 %d 个线索文件未加载", budget.MaxFiles, len(selected)-filesUsed))
			break
		}
		if charsUsed+sl.Chars > budget.MaxChars {
			if filesUsed == 0 {
				// 首个文件就超字符预算：至少返回一个裁剪后的片段，避免完全无证据。
				sl, truncated = r.clipToBudget(sl, budget.MaxChars)
				sl.Reason = joinReason(sl.Reason, fmt.Sprintf("字符预算 MaxChars=%d 裁剪", budget.MaxChars))
				charsUsed += sl.Chars
				filesUsed++
				truncatedAny = truncatedAny || truncated
				out = append(out, *sl)
				r.addNote(fmt.Sprintf("字符预算 MaxChars=%d 已用尽，仅返回首个片段的裁剪结果", budget.MaxChars))
			} else {
				r.addNote(fmt.Sprintf("字符预算 MaxChars=%d 已用尽，剩余 %d 个线索文件未加载", budget.MaxChars, len(selected)-filesUsed))
			}
			stop = true
			truncatedAny = true
			break
		}
		if truncated {
			truncatedAny = true
			sl.Reason = joinReason(sl.Reason, "已按预算裁剪")
		}
		if isNew {
			r.cachePutFile(meta, sl)
		}
		charsUsed += sl.Chars
		filesUsed++
		out = append(out, *sl)
	}

	if truncatedAny {
		r.addNote(fmt.Sprintf("本次加载已裁剪：文件 %d/%d，字符 %d/%d", filesUsed, budget.MaxFiles, charsUsed, budget.MaxChars))
	}
	r.log.Info("源码懒加载完成",
		"repo", ref.Repository.Key, "commit", shortSHA(meta.commit),
		"files", filesUsed, "chars", charsUsed, "hints", len(hints))
	return out, nil
}

// loadOne 加载单个候选文件（复用文件缓存）。
//
// 返回 (切片, 是否被裁剪, 是否为缓存未命中, 错误)。
func (r *Resolver) loadOne(ctx context.Context, meta *repoMeta, cand hintCandidate, budget domain.LoadBudget) (*domain.CodeSlice, bool, bool, error) {
	key := fileCacheKey(meta.repoID, meta.commit, cand.path)
	if cached, err := r.files.Get(key); err == nil && cached != nil {
		sl := cached
		sl.FromCache = true
		sl.RepositoryID = meta.repoID
		sl.RepoKey = meta.repo.Key
		sl.Commit = meta.commit
		sl.Reason = cand.reason
		if cand.line > 0 {
			sl.FocusLines = []int{cand.line}
		}
		return sl, sl.Truncated, false, nil
	}

	content, err := r.git.Show(ctx, meta.dir, meta.commit, cand.path)
	if err != nil {
		return nil, false, false, err
	}
	text := string(content)
	total := LineCount(text)

	if cand.line > 0 && r.needsFocusWindow(text, total, budget) {
		radius := focusRadiusSmall
		if len(text) > budget.MaxFileSize || total > largeFileLines {
			radius = focusRadiusLarge
		}
		snippet, start, end := FocusWindow(text, cand.line, radius)
		if snippet != "" {
			sl := r.newSlice(meta, cand.path, snippet, start, end)
			sl.FocusLines = appendFocus(sl.FocusLines, cand.line, start, end)
			sl.Truncated = start > 1 || end < total
			sl.Reason = joinReason(cand.reason, fmt.Sprintf("聚焦切片 %d-%d 行（原始共 %d 行）", start, end, total))
			return sl, sl.Truncated, true, nil
		}
	}

	if len(text) > budget.MaxFileSize {
		clipped := TruncateUTF8(text, budget.MaxFileSize)
		lines := LineCount(clipped)
		sl := r.newSlice(meta, cand.path, clipped, 1, lines)
		sl.Truncated = true
		sl.Reason = joinReason(cand.reason, fmt.Sprintf("单文件超限（%d 字节 > MaxFileSize=%d），仅加载前 %d 行", len(text), budget.MaxFileSize, lines))
		return sl, true, true, nil
	}

	sl := r.newSlice(meta, cand.path, text, 1, total)
	if cand.line > 0 {
		sl.FocusLines = []int{cand.line}
	}
	return sl, false, true, nil
}

// newSlice 组装完整字段的代码切片（Symbols / Imports 由 astutil 提取）。
func (r *Resolver) newSlice(meta *repoMeta, path, content string, start, end int) *domain.CodeSlice {
	lang := DetectLanguage(path, []byte(content))
	sl := &domain.CodeSlice{
		RepositoryID: meta.repoID,
		RepoKey:      meta.repo.Key,
		Commit:       meta.commit,
		Path:         path,
		Language:     lang,
		Content:      content,
		StartLine:    start,
		EndLine:      end,
		Chars:        len(content),
		Redacted:     false, // 脱敏由 env 层负责，本层不做
		FromCache:    false,
	}
	sl.Symbols = ExtractSymbols(lang, content)
	sl.Imports = ExtractImports(lang, content)
	return sl
}

// cachePutFile 写入文件缓存。
func (r *Resolver) cachePutFile(meta *repoMeta, sl *domain.CodeSlice) {
	if sl == nil || sl.Path == "" {
		return
	}
	r.files.Put(fileCacheKey(meta.repoID, meta.commit, sl.Path), sl)
}

// clipToBudget 把切片裁剪到字符预算内（从头部保留）。
func (r *Resolver) clipToBudget(sl *domain.CodeSlice, maxChars int) (*domain.CodeSlice, bool) {
	if sl == nil || maxChars <= 0 || sl.Chars <= maxChars {
		return sl, false
	}
	clipped := TruncateUTF8(sl.Content, maxChars)
	out := cloneSlice(sl)
	out.Content = clipped
	out.Chars = len(clipped)
	out.EndLine = out.StartLine + LineCount(clipped) - 1
	if out.EndLine < out.StartLine {
		out.EndLine = out.StartLine
	}
	out.Truncated = true
	return out, true
}

// filesTooLarge 判断线索对应的文件是否已超出单文件预算（无法精确匹配时返回 false）。
func (r *Resolver) filesTooLarge(tree []domain.FileNode, hintPath string, maxSize int) bool {
	if maxSize <= 0 {
		return false
	}
	p := normalizeRepoPath(hintPath)
	if p == "" {
		return false
	}
	for _, n := range tree {
		if n.IsDir {
			continue
		}
		if n.Path == p || strings.EqualFold(n.Path, p) {
			return n.Size > int64(maxSize)
		}
	}
	return false
}

// needsFocusWindow 判断是否需要对大文件做聚焦切片。
func (r *Resolver) needsFocusWindow(text string, total int, budget domain.LoadBudget) bool {
	return len(text) > budget.MaxFileSize || total > largeFileLines
}

const (
	// focusRadiusSmall 常规聚焦半径。
	focusRadiusSmall = 20
	// focusRadiusLarge 大文件聚焦半径。
	focusRadiusLarge = 60
	// largeFileLines 触发聚焦切片的行数阈值。
	largeFileLines = 2000
)

// ---------------------------------------------------------------------------
// 线索 → 仓库内真实路径
// ---------------------------------------------------------------------------

// resolveHintPaths 把一个 FileHint 解析为仓库内真实存在的候选文件（最多 3 个）。
func (r *Resolver) resolveHintPaths(ctx context.Context, meta *repoMeta, tree []domain.FileNode, h domain.FileHint) []hintCandidate {
	raw := strings.TrimSpace(h.Path)
	if raw == "" {
		return nil
	}
	reason := strings.TrimSpace(h.Reason)
	if reason == "" {
		reason = "hint"
	}
	prio := h.Priority
	if prio <= 0 {
		prio = 0.5
	}

	p := normalizeRepoPath(raw)
	lower := strings.ToLower(p)
	isDirHint := strings.HasSuffix(raw, "/")

	// 1) 精确文件
	if !isDirHint {
		if actual, ok := exactPathInTree(tree, p); ok {
			return []hintCandidate{{path: actual, reason: reason, line: h.Line, priority: prio}}
		}
	}

	// 2) 目录 / 包路径前缀
	prefix := strings.Trim(lower, "/")
	if prefix != "" {
		matches := filesUnderPrefix(tree, prefix)
		if len(matches) > 0 {
			ranked := rankTreeMatches(matches, p, prio, h.Line, reason)
			return capCandidates(ranked, maxFilesPerHint)
		}
	}

	// 3) 类名 / 文件名（后缀匹配）
	if !isDirHint && !strings.Contains(p, "/") {
		if matches := filesByBaseName(tree, p); len(matches) > 0 {
			ranked := rankTreeMatches(matches, p, prio, h.Line, reason)
			return capCandidates(ranked, maxFilesPerHint)
		}
	}

	// 4) 兜底：GrepCommit 全仓搜索
	if !isDirHint && len(p) >= 3 {
		if found, err := r.git.GrepCommit(ctx, meta.dir, meta.commit, p, maxFilesPerHint); err == nil {
			out := make([]hintCandidate, 0, len(found))
			for _, f := range found {
				c := hintCandidate{path: normalizeRepoPath(f.Path), reason: reason, line: f.Line, priority: prio * 0.9}
				if h.Line > 0 {
					c.line = h.Line
				}
				out = append(out, c)
			}
			if len(out) > 0 {
				return capCandidates(out, maxFilesPerHint)
			}
		}
	}
	return nil
}

// exactPathInTree 精确（含大小写回退）查找文件。
func exactPathInTree(tree []domain.FileNode, p string) (string, bool) {
	lower := strings.ToLower(p)
	for _, n := range tree {
		if n.IsDir {
			continue
		}
		if n.Path == p {
			return n.Path, true
		}
	}
	for _, n := range tree {
		if n.IsDir {
			continue
		}
		if strings.EqualFold(n.Path, lower) {
			return n.Path, true
		}
	}
	return "", false
}

// filesUnderPrefix 返回前缀目录下的全部文件。
func filesUnderPrefix(tree []domain.FileNode, prefix string) []domain.FileNode {
	out := make([]domain.FileNode, 0, 16)
	for _, n := range tree {
		if n.IsDir {
			continue
		}
		lp := strings.ToLower(n.Path)
		if strings.HasPrefix(lp, prefix+"/") {
			out = append(out, n)
		}
	}
	return out
}

// filesByBaseName 按 basename 精确/后缀匹配文件。
func filesByBaseName(tree []domain.FileNode, base string) []domain.FileNode {
	lb := strings.ToLower(base)
	out := make([]domain.FileNode, 0, 8)
	for _, n := range tree {
		if n.IsDir {
			continue
		}
		if strings.EqualFold(baseName(n.Path), lb) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		// 去掉扩展名后再匹配（如 OrderService → OrderService.java）
		stem := stripKnownExt(lb)
		for _, n := range tree {
			if n.IsDir {
				continue
			}
			if stripKnownExt(strings.ToLower(baseName(n.Path))) == stem {
				out = append(out, n)
			}
		}
	}
	return out
}

// rankTreeMatches 对候选文件按相关度排序（文件名相似度 + 入口偏好）。
func rankTreeMatches(nodes []domain.FileNode, query string, prio float64, line int, reason string) []hintCandidate {
	type scored struct {
		node domain.FileNode
		s    float64
	}
	list := make([]scored, 0, len(nodes))
	qBase := strings.ToLower(stripKnownExt(baseName(query)))
	qNorm := normKey(query)
	for _, n := range nodes {
		b := strings.ToLower(baseName(n.Path))
		stem := stripKnownExt(b)
		s := 0.0
		switch {
		case stem == qBase:
			s += 0.5
		case strings.Contains(stem, qBase) || strings.Contains(qBase, stem):
			s += 0.25
		}
		np := strings.ToLower(n.Path)
		if strings.Contains(np, strings.ToLower(query)) {
			s += 0.2
		}
		if qNorm != "" && normKey(n.Path) == qNorm {
			s += 0.3
		}
		if isEntryLike(b) {
			s += 0.08
		}
		list = append(list, scored{node: n, s: s})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].s != list[j].s {
			return list[i].s > list[j].s
		}
		return list[i].node.Path < list[j].node.Path
	})
	out := make([]hintCandidate, 0, len(list))
	for _, it := range list {
		out = append(out, hintCandidate{
			path:     it.node.Path,
			reason:   reason,
			line:     line,
			priority: prio + it.s*0.1,
		})
	}
	return out
}

// isEntryLike 判断是否为常见入口文件。
func isEntryLike(base string) bool {
	switch stripKnownExt(base) {
	case "index", "main", "app", "server", "mod", "route", "controller", "handler", "service":
		return true
	}
	return false
}

// stripKnownExt 去掉常见源码扩展名。
func stripKnownExt(s string) string {
	for _, ext := range []string{".java", ".php", ".py", ".ts", ".tsx", ".js", ".jsx", ".go", ".cs", ".rs", ".kt", ".rb", ".vue"} {
		if strings.HasSuffix(s, ext) {
			return s[:len(s)-len(ext)]
		}
	}
	return s
}

// capCandidates 截断候选数量。
func capCandidates(in []hintCandidate, n int) []hintCandidate {
	if n > 0 && len(in) > n {
		return in[:n]
	}
	return in
}

const maxFilesPerHint = 3

// sortHintsByPriority 按 Priority 降序稳定排序（同优先级保持输入顺序）。
func sortHintsByPriority(hints []domain.FileHint) []domain.FileHint {
	out := append([]domain.FileHint{}, hints...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// effectiveBudget 补全预算缺省值。
func effectiveBudget(b domain.LoadBudget) domain.LoadBudget {
	if b.MaxFiles <= 0 {
		b.MaxFiles = 50
	}
	if b.MaxChars <= 0 {
		b.MaxChars = 200_000
	}
	if b.MaxFileSize <= 0 {
		b.MaxFileSize = 1 << 20
	}
	if b.MaxDepth <= 0 {
		b.MaxDepth = 32
	}
	return b
}

// pathDepth 计算路径深度。
func pathDepth(p string) int {
	p = strings.Trim(normalizeRepoPath(p), "/")
	if p == "" {
		return 0
	}
	return len(strings.Split(p, "/"))
}

// ---------------------------------------------------------------------------
// ReadFile / Tree / Grep / Invalidate / Stats
// ---------------------------------------------------------------------------

// ReadFile 读取单文件全文（仓库 + Commit + 路径 维度缓存），受 MaxFileSize 约束。
func (r *Resolver) ReadFile(ctx context.Context, ref domain.RepoRef, path string) (*domain.CodeSlice, error) {
	if ref.Repository == nil {
		return nil, ErrNoRepository
	}
	p := normalizeRepoPath(path)
	if p == "" {
		return nil, ErrFileNotFound
	}
	meta := &repoMeta{repoID: ref.Repository.ID, tenantID: ref.Repository.TenantID, repo: ref.Repository}
	commit, dir, err := r.resolveLocked(ctx, ref.Repository, ref.Ref, ref.Commit)
	if err != nil {
		return nil, err
	}
	meta.commit, meta.dir = commit, dir

	budget := effectiveBudget(ref.Budget)
	key := fileCacheKey(meta.repoID, meta.commit, p)
	if cached, cerr := r.files.Get(key); cerr == nil && cached != nil {
		out := cached
		out.FromCache = true
		out.RepositoryID = meta.repoID
		out.RepoKey = meta.repo.Key
		out.Commit = meta.commit
		return out, nil
	}

	content, err := r.git.Show(ctx, meta.dir, meta.commit, p)
	if err != nil {
		if errors.Is(err, ErrBinaryFile) {
			return nil, fmt.Errorf("%w: %s", ErrBinaryFile, p)
		}
		return nil, err
	}
	text := string(content)
	total := LineCount(text)
	sl := r.newSlice(meta, p, text, 1, total)
	sl.Reason = "read_file"
	if len(text) > budget.MaxFileSize {
		clipped := TruncateUTF8(text, budget.MaxFileSize)
		lines := LineCount(clipped)
		sl = r.newSlice(meta, p, clipped, 1, lines)
		sl.Truncated = true
		sl.Reason = fmt.Sprintf("单文件超限（%d 字节 > MaxFileSize=%d），仅加载前 %d 行", len(text), budget.MaxFileSize, lines)
		r.addNote(sl.Reason)
	}
	r.cachePutFile(meta, sl)
	return sl, nil
}

// Tree 列出文件树（忽略规则 + 条目上限过滤）。
func (r *Resolver) Tree(ctx context.Context, ref domain.RepoRef) ([]domain.FileNode, error) {
	if ref.Repository == nil {
		return nil, ErrNoRepository
	}
	meta := &repoMeta{repoID: ref.Repository.ID, tenantID: ref.Repository.TenantID, repo: ref.Repository}
	commit, dir, err := r.resolveLocked(ctx, ref.Repository, ref.Ref, ref.Commit)
	if err != nil {
		return nil, err
	}
	meta.commit, meta.dir = commit, dir
	nodes, err := r.treeNodeList(ctx, meta)
	if err != nil {
		return nil, err
	}
	out := make([]domain.FileNode, len(nodes))
	copy(out, nodes)
	return out, nil
}

// treeNodeList 获取（并缓存）某 commit 下过滤后的文件树。
func (r *Resolver) treeNodeList(ctx context.Context, meta *repoMeta) ([]domain.FileNode, error) {
	if meta == nil || meta.commit == "" {
		return nil, errors.New("未锁定 commit，无法读取文件树")
	}
	if meta.dir == "" {
		dir, ok := r.mirrorIfPresent(meta.repo)
		if !ok {
			secret := r.secretFor(ctx, meta.repo)
			var err error
			dir, err = r.git.EnsureMirror(ctx, meta.repo, secret)
			if err != nil {
				return nil, err
			}
		}
		meta.dir = dir
	}
	key := treeCacheKey(meta.repoID, meta.commit)
	if cached, err := r.trees.Get(key); err == nil && cached != nil {
		return decodeTree(cached.Content), nil
	}
	raw, err := r.git.LsTree(ctx, meta.dir, meta.commit)
	if err != nil {
		return nil, err
	}
	patterns := r.ignorePatternsOf(meta.repo)
	maxEntries := r.maxTreeEntries()
	filtered := make([]domain.FileNode, 0, len(raw))
	truncated := false
	dirs := map[string]bool{}
	for _, n := range raw {
		if n.IsDir {
			continue
		}
		if shouldIgnorePath(n.Path, patterns) {
			continue
		}
		if len(filtered) >= maxEntries {
			truncated = true
			break
		}
		filtered = append(filtered, n)
		p := n.Path
		for {
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
			if p == "" || dirs[p] {
				break
			}
			dirs[p] = true
		}
	}
	if truncated {
		r.log.Warn("文件树条目超过上限，已截断", "repo", meta.repo.Key, "limit", maxEntries)
	}
	// 目录节点补齐（父目录可能因忽略规则而整体缺失）。
	for d := range dirs {
		if shouldIgnorePath(d, patterns) {
			continue
		}
		filtered = append(filtered, domain.FileNode{Path: d, IsDir: true})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Path < filtered[j].Path })
	r.trees.Put(key, &domain.CodeSlice{Content: encodeTree(filtered), Chars: len(filtered)})
	return filtered, nil
}

// ignorePatternsOf 返回仓库生效的忽略规则（配置 + 仓库根 .gitignore）。
func (r *Resolver) ignorePatternsOf(repo *domain.Repository) []string {
	patterns := append([]string{}, defaultIgnorePatterns...)
	for _, p := range r.cfg.IgnorePatterns {
		if s := strings.TrimSpace(p); s != "" {
			patterns = append(patterns, s)
		}
	}
	if r.cfg.RespectGitignore && repo != nil {
		patterns = append(patterns, r.gitignoreOf(repo)...)
	}
	return patterns
}

// gitignoreOf 解析仓库根 .gitignore 中的简单模式（带缓存，失败返回空）。
func (r *Resolver) gitignoreOf(repo *domain.Repository) []string {
	key := fileCacheKey(repo.ID, "gitignore", ".gitignore")
	if cached, err := r.files.Get(key); err == nil && cached != nil {
		return splitNonEmptyLines(cached.Content)
	}
	dir := r.git.MirrorDir(repo.TenantID, repo.ID)
	if !isGitDir(dir) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	head, err := r.git.Head(ctx, dir)
	if err != nil {
		return nil
	}
	raw, err := r.git.Show(ctx, dir, head, ".gitignore")
	if err != nil {
		return nil
	}
	text := string(raw)
	r.files.Put(key, &domain.CodeSlice{Path: ".gitignore", Content: text, Chars: len(text)})
	patterns := []string{}
	for _, line := range splitNonEmptyLines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// defaultIgnorePatterns 兜底忽略目录（保证 .git / node_modules 永不进入加载范围）。
var defaultIgnorePatterns = []string{".git", "node_modules", "dist", "build", "target", "vendor", "__pycache__", ".venv", ".idea"}

// maxTreeEntries 文件树条目上限。
func (r *Resolver) maxTreeEntries() int { return 20000 }

// encodeTree / decodeTree 用制表符分隔的文本承载文件树缓存条目（复用 LRU 与统计）。
func encodeTree(nodes []domain.FileNode) string {
	var b strings.Builder
	for i, n := range nodes {
		if i > 0 {
			b.WriteByte('\n')
		}
		flag := "f"
		if n.IsDir {
			flag = "d"
		}
		fmt.Fprintf(&b, "%s\t%d\t%s", flag, n.Size, n.Path)
	}
	return b.String()
}

func decodeTree(s string) []domain.FileNode {
	if strings.TrimSpace(s) == "" {
		return []domain.FileNode{}
	}
	lines := strings.Split(s, "\n")
	out := make([]domain.FileNode, 0, len(lines))
	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		n := domain.FileNode{Path: parts[2], IsDir: parts[0] == "d"}
		n.Size = int64(atoiSafe(parts[1]))
		if !n.IsDir {
			n.Language = detectLanguageByPath(n.Path)
		}
		out = append(out, n)
	}
	return out
}

// Grep 在指定版本的代码中检索符号，返回带行号的文件线索。
func (r *Resolver) Grep(ctx context.Context, ref domain.RepoRef, pattern string, limit int) ([]domain.FileHint, error) {
	if ref.Repository == nil {
		return nil, ErrNoRepository
	}
	if strings.TrimSpace(pattern) == "" {
		return []domain.FileHint{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	commit, dir, err := r.resolveLocked(ctx, ref.Repository, ref.Ref, ref.Commit)
	if err != nil {
		return nil, err
	}
	hints, err := r.git.GrepCommit(ctx, dir, commit, pattern, limit)
	if err != nil {
		return nil, err
	}
	meta := &repoMeta{repoID: ref.Repository.ID, tenantID: ref.Repository.TenantID, repo: ref.Repository, commit: commit, dir: dir}
	tree, terr := r.treeNodeList(ctx, meta)
	if terr == nil && len(tree) > 0 {
		filtered := make([]domain.FileHint, 0, len(hints))
		for _, h := range hints {
			if shouldIgnorePath(h.Path, r.ignorePatternsOf(ref.Repository)) {
				continue
			}
			filtered = append(filtered, h)
		}
		hints = filtered
	}
	if hints == nil {
		hints = []domain.FileHint{}
	}
	r.log.Debug("源码检索完成", "repo", ref.Repository.Key, "pattern", pattern, "hits", len(hints))
	return hints, nil
}

// Invalidate 清理指定仓库的文件缓存与元数据缓存；tenantID 为空时全量清理。
func (r *Resolver) Invalidate(ctx context.Context, tenantID, repoID string) error {
	repoID = strings.TrimSpace(repoID)
	tenantID = strings.TrimSpace(tenantID)
	removed := 0
	switch {
	case repoID != "":
		prefix := repoID + "|"
		removed += r.files.purgePrefix(prefix)
		removed += r.trees.purgePrefix(prefix)
		removed += r.meta.purgePrefix(prefix)
	case tenantID != "":
		// 仅清理该租户下的仓库键（仓库 ID 全局唯一，按仓库逐个清理最精确）。
		for _, repo := range r.reposOfTenant(tenantID) {
			prefix := repo.ID + "|"
			removed += r.files.purgePrefix(prefix)
			removed += r.trees.purgePrefix(prefix)
			removed += r.meta.purgePrefix(prefix)
		}
		// 元数据缓存键含租户前缀，一并清理。
		removed += r.meta.purgePrefix(tenantID + "|")
	default:
		removed += r.files.purgePrefix("")
		removed += r.trees.purgePrefix("")
		removed += r.meta.purgePrefix("")
	}
	r.log.Info("源码缓存已失效", "tenant", tenantID, "repo", repoID, "removed", removed)
	return nil
}

// reposOfTenant 返回某租户的全部仓库（store 不可用时返回空）。
func (r *Resolver) reposOfTenant(tenantID string) []domain.Repository {
	if r.store == nil {
		return nil
	}
	return r.store.ListRepos(tenantID)
}

// Stats 返回文件级缓存统计（全部为真实计数）。
func (r *Resolver) Stats() domain.CacheStats {
	hits := r.hits.Load()
	misses := r.misses.Load()
	rate := 0.0
	if total := hits + misses; total > 0 {
		rate = float64(hits) / float64(total)
	}
	total := r.files.len() + r.trees.len() + r.meta.len()
	return domain.CacheStats{
		Entries:   total + r.entryCacheLen(),
		Hits:      hits,
		Misses:    misses,
		Evictions: r.evictions.Load(),
		Bytes:     r.files.totalBytes() + r.trees.totalBytes() + r.meta.totalBytes(),
		HitRate:   rate,
		Capacity:  r.files.capacity + r.trees.capacity + r.meta.capacity,
	}
}

// entryCacheLen 返回额外统计的缓存条目数（当前无额外缓存，保留扩展位）。
func (r *Resolver) entryCacheLen() int { return 0 }

// ---------------------------------------------------------------------------
// 忽略规则
// ---------------------------------------------------------------------------

// shouldIgnorePath 判断路径是否命中忽略规则。
//
// 规则语义贴近 .gitignore 的常用子集：
//   - 不含 `/` 与通配符的模式匹配任意层级的同名文件/目录（如 node_modules）；
//   - 含 `/` 的模式按仓库根相对路径前缀匹配；
//   - 支持 `*` 与 `**` 通配（如 *.min.js、dist/**）。
func shouldIgnorePath(path string, patterns []string) bool {
	p := strings.ToLower(normalizeRepoPath(path))
	if p == "" {
		return false
	}
	segs := strings.Split(p, "/")
	for _, raw := range patterns {
		pat := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "./")))
		pat = strings.Trim(pat, "/")
		if pat == "" {
			continue
		}
		if strings.ContainsAny(pat, "*?") {
			if strings.Contains(pat, "/") {
				if globSimpleMatch(pat, p) || globSimpleMatch(pat+"/**", p) {
					return true
				}
				continue
			}
			for _, s := range segs {
				if globSimpleMatch(pat, s) {
					return true
				}
			}
			continue
		}
		if strings.Contains(pat, "/") {
			if p == pat || strings.HasPrefix(p, pat+"/") {
				return true
			}
			continue
		}
		for _, s := range segs {
			if s == pat {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// splitNonEmptyLines 按行拆分并去空。
func splitNonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range splitLines(s) {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// joinReason 拼接原因说明（去重）。
func joinReason(base, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	case strings.Contains(base, extra):
		return base
	default:
		return base + "；" + extra
	}
}

// appendFocus 追加聚焦行号（保持在文件有效范围内、去重保序）。
func appendFocus(focus []int, line, start, end int) []int {
	add := func(v int) {
		if v < start || v > end {
			return
		}
		for _, e := range focus {
			if e == v {
				return
			}
		}
		focus = append(focus, v)
	}
	add(line)
	add(start)
	add(end)
	sort.Ints(focus)
	return focus
}

// firstNonEmpty 返回首个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// minInt 返回较小值。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
