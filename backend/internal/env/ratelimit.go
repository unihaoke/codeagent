package env

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/domain"
)

// 限流默认值。
const (
	// DefaultRateLimitPerMin 单租户默认每分钟请求上限。
	DefaultRateLimitPerMin = 600
	// defaultBucketIdleTTL 空闲令牌桶默认保留时长。
	defaultBucketIdleTTL = 10 * time.Minute
	// gcInterval 空闲桶清理周期。
	gcInterval = time.Minute
)

// tokenBucket 整型令牌桶状态。
type tokenBucket struct {
	tokens   int64
	last     time.Time
	lastSeen time.Time
}

// RateLimiter 按 key（通常为租户 ID）分桶的整型令牌桶限流器。
//
// 采用惰性补充：仅在访问桶时按经过时间折算补充令牌，避免后台定时器规模随租户数膨胀；
// 另有一个低频 goroutine 回收长期空闲的桶，防止内存随时间无界增长。
type RateLimiter struct {
	perMinute int64
	burst     int64
	idleTTL   time.Duration

	mu      sync.Mutex
	buckets map[string]*tokenBucket

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewRateLimiter 创建令牌桶限流器。
//
// perMinute <= 0 时使用 DefaultRateLimitPerMin；burst <= 0 时取 perMinute（至少 1）。
func NewRateLimiter(perMinute int, burst int) *RateLimiter {
	if perMinute <= 0 {
		perMinute = DefaultRateLimitPerMin
	}
	if burst <= 0 {
		burst = perMinute
	}
	if burst < 1 {
		burst = 1
	}
	r := &RateLimiter{
		perMinute: int64(perMinute),
		burst:     int64(burst),
		idleTTL:   defaultBucketIdleTTL,
		buckets:   map[string]*tokenBucket{},
		stopCh:    make(chan struct{}),
	}
	r.wg.Add(1)
	go r.gcLoop()
	return r
}

// PerMinute 返回每分钟补充速率。
func (r *RateLimiter) PerMinute() int {
	if r == nil {
		return 0
	}
	return int(r.perMinute)
}

// Burst 返回桶容量（瞬时突发上限）。
func (r *RateLimiter) Burst() int {
	if r == nil {
		return 0
	}
	return int(r.burst)
}

// Size 返回当前活跃桶数量（观测/测试用）。
func (r *RateLimiter) Size() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// Allow 尝试取走一个令牌。
//
// 返回 false 时 retryAfter 为建议的重试等待时长（至少 1ms）。
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	if r == nil {
		return true, 0
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.buckets[key]
	if !ok {
		// 新桶初始为满，保证首屏请求不被无谓拦截。
		b = &tokenBucket{tokens: r.burst, last: now, lastSeen: now}
		r.buckets[key] = b
	}
	r.refill(b, now)
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration(1-b.tokens) * time.Minute / time.Duration(r.perMinute)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return false, wait
}

// refill 按经过时间补充令牌，并按补充量精确推进基准时间以保留小数余量。
func (r *RateLimiter) refill(b *tokenBucket, now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	add := int64(elapsed) * r.perMinute / int64(time.Minute)
	if add <= 0 {
		return
	}
	b.tokens += add
	if b.tokens > r.burst {
		b.tokens = r.burst
	}
	b.last = b.last.Add(time.Duration(add) * time.Minute / time.Duration(r.perMinute))
	if b.last.After(now) {
		b.last = now
	}
}

// CleanupIdle 回收空闲时长达到 idle 的桶，返回回收数量。
func (r *RateLimiter) CleanupIdle(idle time.Duration) int {
	if r == nil {
		return 0
	}
	deadline := time.Now().Add(-idle)
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := 0
	for k, b := range r.buckets {
		if !b.lastSeen.After(deadline) {
			delete(r.buckets, k)
			removed++
		}
	}
	return removed
}

// gcLoop 周期性回收空闲桶。
func (r *RateLimiter) gcLoop() {
	defer r.wg.Done()
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			r.CleanupIdle(r.idleTTL)
		}
	}
}

// Close 停止后台回收 goroutine（可重复调用）。
func (r *RateLimiter) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
	r.wg.Wait()
}

// ---------------------------------------------------------------------------
// 租户配额守卫
// ---------------------------------------------------------------------------

// 配额类型标识。
const (
	// QuotaKindConcurrency 并发任务数。
	QuotaKindConcurrency = "concurrency"
	// QuotaKindFiles 单任务文件数。
	QuotaKindFiles = "files"
	// QuotaKindChars 单任务代码字符数。
	QuotaKindChars = "chars"
	// QuotaKindTokens 单任务模型 Token 数。
	QuotaKindTokens = "tokens"
)

// QuotaError 配额超限错误，可通过 errors.As 取出具体维度。
type QuotaError struct {
	// Kind 超限维度：concurrency / files / chars / tokens。
	Kind string
	// Limit 配额上限。
	Limit int
	// Actual 实际值。
	Actual int
}

// Error 实现 error。
func (e *QuotaError) Error() string {
	if e == nil {
		return "配额超限"
	}
	return fmt.Sprintf("超出配额[%s]: 实际 %d，上限 %d", e.Kind, e.Actual, e.Limit)
}

// IsQuotaError 判断错误是否为配额超限。
func IsQuotaError(err error) bool {
	var qe *QuotaError
	return errors.As(err, &qe)
}

// AsQuotaError 提取配额错误详情。
func AsQuotaError(err error) (*QuotaError, bool) {
	var qe *QuotaError
	if errors.As(err, &qe) {
		return qe, true
	}
	return nil, false
}

// QuotaGuard 租户配额守卫。
//
// 约定：配额字段 <= 0 表示该维度不限制。
// 并发数是"硬"限制（超限直接拒绝），文件数/字符数/Tokens 是"软"限制
// （超限时降级裁剪并在 EvidenceBundle.Notes / Warnings 中留痕，契约要求降级不失败）。
type QuotaGuard struct{}

// NewQuotaGuard 创建配额守卫。
func NewQuotaGuard() *QuotaGuard { return &QuotaGuard{} }

// CheckConcurrency 校验并发任务数是否超限。
func (g *QuotaGuard) CheckConcurrency(current int, q domain.TenantQuota) error {
	if q.MaxConcurrentTasks > 0 && current >= q.MaxConcurrentTasks {
		return &QuotaError{Kind: QuotaKindConcurrency, Limit: q.MaxConcurrentTasks, Actual: current}
	}
	return nil
}

// CheckLoad 校验文件数与代码字符数。
//
// 返回是否降级以及降级说明；该方法永不返回错误（契约：配额超限降级而非失败）。
func (g *QuotaGuard) CheckLoad(files, chars int, q domain.TenantQuota) (bool, []string) {
	return CheckLoad(files, chars, q)
}

// CheckTokens 校验模型 Token 消耗是否超限。
func (g *QuotaGuard) CheckTokens(tokens int, q domain.TenantQuota) error {
	return CheckTokens(tokens, q)
}

// CheckLoad 包级便捷函数：校验文件数与代码字符数，返回降级标记与说明。
func CheckLoad(files, chars int, q domain.TenantQuota) (bool, []string) {
	degraded := false
	notes := []string{}
	if q.MaxFilesPerTask > 0 && files > q.MaxFilesPerTask {
		degraded = true
		notes = append(notes, fmt.Sprintf("加载文件数 %d 超出配额 %d，已按关联度裁剪至配额上限",
			files, q.MaxFilesPerTask))
	}
	if q.MaxCodeChars > 0 && chars > q.MaxCodeChars {
		degraded = true
		notes = append(notes, fmt.Sprintf("代码字符数 %d 超出配额 %d，已按优先级截断上下文",
			chars, q.MaxCodeChars))
	}
	return degraded, notes
}

// CheckTokens 包级便捷函数：校验模型 Token 消耗。
func CheckTokens(tokens int, q domain.TenantQuota) error {
	if q.MaxTokensPerTask > 0 && tokens > q.MaxTokensPerTask {
		return &QuotaError{Kind: QuotaKindTokens, Limit: q.MaxTokensPerTask, Actual: tokens}
	}
	return nil
}

// ClampFiles 按配额裁剪文件数，返回裁剪后的值。
func ClampFiles(files int, q domain.TenantQuota) int {
	if q.MaxFilesPerTask > 0 && files > q.MaxFilesPerTask {
		return q.MaxFilesPerTask
	}
	return files
}

// ClampChars 按配额裁剪代码字符数，返回裁剪后的值。
func ClampChars(chars int, q domain.TenantQuota) int {
	if q.MaxCodeChars > 0 && chars > q.MaxCodeChars {
		return q.MaxCodeChars
	}
	return chars
}

// TaskTimeout 返回租户配额下的任务超时；未配置时返回 fallback。
func TaskTimeout(q domain.TenantQuota, fallback time.Duration) time.Duration {
	if q.MaxTaskSeconds > 0 {
		return time.Duration(q.MaxTaskSeconds) * time.Second
	}
	return fallback
}
