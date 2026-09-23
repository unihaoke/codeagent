package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/google/uuid"
)

// 重试与降级相关常量。
const (
	// retryBaseBackoff 首次重试退避时长（后续按 3 倍递增：100ms/300ms/900ms...）。
	retryBaseBackoff = 100 * time.Millisecond
	// retryMaxBackoff 单次重试退避上限。
	retryMaxBackoff = 900 * time.Millisecond
	// timeoutRetryLimit 超时类错误最多重试次数（避免长时间占用 worker）。
	timeoutRetryLimit = 1
	// maxFallbackDepth 兜底链最大递归深度（防止技能互相兜底形成环）。
	maxFallbackDepth = 2
	// digestLength 输入输出摘要长度（sha256 前 16 位十六进制）。
	digestLength = 16
	// maxAuditStringLength 审计输出中单字符串最大长度。
	maxAuditStringLength = 4000
)

// SchemaValidationError 技能入参 Schema 校验失败错误。
//
// 该错误可被调用方用 errors.As 识别，用于把 HTTP 状态码映射为 422。
type SchemaValidationError struct {
	// Skill 技能名。
	Skill string `json:"skill"`
	// Errors 全部字段级校验错误。
	Errors []SchemaError `json:"errors"`
}

// Error 实现 error 接口，汇总前若干条字段错误。
func (e *SchemaValidationError) Error() string {
	if e == nil {
		return ""
	}
	const limit = 5
	parts := make([]string, 0, limit)
	for i, item := range e.Errors {
		if i >= limit {
			parts = append(parts, fmt.Sprintf("等 %d 项错误", len(e.Errors)))
			break
		}
		parts = append(parts, item.Message)
	}
	return fmt.Sprintf("技能 %s 入参校验失败: %s", e.Skill, strings.Join(parts, "; "))
}

// callStat 技能调用统计（供 Stats 使用）。
type callStat struct {
	calls        int64
	failures     int64
	latencySumMS int64
}

// Runner 技能执行治理器：入参强校验 + 沙箱隔离 + 重试 + 熔断 + 兜底 + 审计。
//
// 沙箱隔离在本层体现为：handler 在独立 goroutine 中执行并受 context 超时约束、
// panic 被 recover 并转为错误、入参以深拷贝副本传入（handler 无法篡改调用方数据）、
// 技能不可写文件系统（handler 仅通过入参出参交互，不注入任何文件句柄）。
type Runner struct {
	reg       *Registry
	cfg       config.SkillConfig
	rec       domain.Recorder
	log       *logx.Logger
	validator *Validator

	mu              sync.Mutex
	stats           map[string]*callStat
	timeoutOverride map[string]time.Duration
}

// NewRunner 创建技能执行治理器；rec 允许为 nil（内部判空，便于单测）。
func NewRunner(reg *Registry, cfg config.SkillConfig, rec domain.Recorder, log *logx.Logger) *Runner {
	if log == nil {
		log = logx.Nop()
	}
	if reg == nil {
		reg = NewRegistry(cfg, log)
	}
	return &Runner{
		reg:             reg,
		cfg:             cfg,
		rec:             rec,
		log:             log,
		validator:       &Validator{},
		stats:           make(map[string]*callStat),
		timeoutOverride: make(map[string]time.Duration),
	}
}

// Registry 返回底层注册中心（供 HTTP 层的 /skills 接口复用）。
func (r *Runner) Registry() *Registry { return r.reg }

// SetTimeoutOverride 注入技能级超时覆盖（测试或特殊场景使用，优先级高于清单 Timeout）。
func (r *Runner) SetTimeoutOverride(name string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d <= 0 {
		delete(r.timeoutOverride, name)
		return
	}
	r.timeoutOverride[name] = d
}

// Run 执行一次受治理的技能调用。
//
// 流程：解析版本 → 入参强校验（失败返回 CallSkipped 且不重试）→ 熔断检查
// （不放行则走兜底链，仍失败返回 Blocked）→ 沙箱隔离执行 + 重试 → 兜底链 →
// 熔断更新 → 审计留痕。
func (r *Runner) Run(ctx context.Context, cc *domain.CallContext, inv domain.SkillInvocation) (*domain.SkillResult, error) {
	started := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	e, err := r.reg.SelectHandler(inv.Skill, inv.Version)
	if err != nil {
		return nil, err
	}
	return r.RunWithManifest(ctx, cc, e, inv, started)
}

// RunWithManifest 使用已解析的技能条目执行一次受治理调用（runner 内部使用，便于单测）。
func (r *Runner) RunWithManifest(ctx context.Context, cc *domain.CallContext, e *entry, inv domain.SkillInvocation, started time.Time) (*domain.SkillResult, error) {
	if e == nil {
		return nil, fmt.Errorf("%w: 技能条目为空", ErrSkillNotFound)
	}
	return r.run(ctx, cc, e, inv, started, "", 0)
}

// run 技能执行核心流程（fallbackFrom 非空表示本次为兜底调用）。
func (r *Runner) run(ctx context.Context, cc *domain.CallContext, e *entry, inv domain.SkillInvocation, started time.Time, fallbackFrom string, depth int) (*domain.SkillResult, error) {
	if started.IsZero() {
		started = time.Now()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if inv.Input == nil {
		inv.Input = map[string]any{}
	}

	// 1. 入参强校验：非法参数不重试，直接拦截，避免无效调用与熔断误伤。
	if verrs := r.validator.Validate(e.manifest.InputSchema, inv.Input); len(verrs) > 0 {
		verr := &SchemaValidationError{Skill: e.manifest.Name, Errors: verrs}
		res := &domain.SkillResult{
			Skill:      e.manifest.Name,
			Version:    e.manifest.Version,
			Status:     domain.CallSkipped,
			Error:      verr.Error(),
			DurationMS: time.Since(started).Milliseconds(),
		}
		r.finish(cc, e, res, inv.Input, nil, started, "")
		r.log.Warn("技能入参校验失败", "skill", e.manifest.Name, "version", e.manifest.Version, "errors", len(verrs))
		return res, verr
	}

	// 2. 熔断检查：不放行则直接尝试兜底链。
	if !e.circuit.Allow() {
		r.log.Warn("技能熔断中，尝试兜底链", "skill", e.manifest.Name, "version", e.manifest.Version)
		if fb := r.runFallbackChain(ctx, cc, e, inv, started, depth); fb != nil {
			return fb, nil
		}
		res := &domain.SkillResult{
			Skill:      e.manifest.Name,
			Version:    e.manifest.Version,
			Status:     domain.CallBlocked,
			Blocked:    true,
			Error:      fmt.Sprintf("%s: %s@%s", ErrCircuitOpen.Error(), e.manifest.Name, e.manifest.Version),
			DurationMS: time.Since(started).Milliseconds(),
		}
		r.finish(cc, e, res, inv.Input, nil, started, fallbackFrom)
		return res, fmt.Errorf("%w: %s@%s", ErrCircuitOpen, e.manifest.Name, e.manifest.Version)
	}

	// 3-4. 沙箱隔离执行 + 重试。
	out, attempts, execErr, timedOut := r.executeWithRetry(ctx, cc, e, inv.Input)
	if execErr == nil {
		e.circuit.Success()
		status := domain.CallOK
		if fallbackFrom != "" {
			status = domain.CallFallback
		}
		res := &domain.SkillResult{
			Skill:        e.manifest.Name,
			Version:      e.manifest.Version,
			Status:       status,
			Output:       out,
			Attempts:     attempts,
			DurationMS:   time.Since(started).Milliseconds(),
			FromFallback: fallbackFrom != "",
			FallbackFrom: fallbackFrom,
		}
		r.finish(cc, e, res, inv.Input, out, started, fallbackFrom)
		r.log.Debug("技能执行成功", "skill", e.manifest.Name, "version", e.manifest.Version,
			"attempts", attempts, "durationMs", res.DurationMS, "fallbackFrom", fallbackFrom)
		return res, nil
	}

	// 5. 失败定级与熔断记账：主技能确实执行并最终失败，先落熔断与审计，
	//    再尝试兜底链，保证审计顺序为「主技能失败 → 兜底调用」且兜底不会掩盖故障技能。
	status := domain.CallFailed
	if timedOut {
		status = domain.CallTimeout
	}
	e.circuit.FailureWith(execErr)
	res := &domain.SkillResult{
		Skill:        e.manifest.Name,
		Version:      e.manifest.Version,
		Status:       status,
		Error:        execErr.Error(),
		Attempts:     attempts,
		DurationMS:   time.Since(started).Milliseconds(),
		FromFallback: fallbackFrom != "",
		FallbackFrom: fallbackFrom,
	}
	r.finish(cc, e, res, inv.Input, nil, started, fallbackFrom)
	r.log.Warn("技能执行失败", "skill", e.manifest.Name, "version", e.manifest.Version,
		"attempts", attempts, "timedOut", timedOut, "error", execErr.Error())

	// 6. 兜底：重试仍失败则走兜底技能链，成功时返回兜底结果。
	if fb := r.runFallbackChain(ctx, cc, e, inv, started, depth); fb != nil {
		return fb, nil
	}
	return res, execErr
}

// runFallbackChain 依次尝试兜底技能链，全部失败返回 nil。
func (r *Runner) runFallbackChain(ctx context.Context, cc *domain.CallContext, e *entry, inv domain.SkillInvocation, started time.Time, depth int) *domain.SkillResult {
	if depth >= maxFallbackDepth {
		return nil
	}
	for _, name := range r.reg.resolveFallbacks(e.manifest.Name) {
		fe, err := r.reg.SelectHandler(name, "")
		if err != nil {
			continue
		}
		// 兜底技能自身的熔断判定交由下面的 run 统一处理，避免重复消费半开探测名额。
		fbInv := domain.SkillInvocation{Skill: name, Version: fe.manifest.Version, Input: inv.Input}
		res, ferr := r.run(ctx, cc, fe, fbInv, started, e.manifest.Name, depth+1)
		if ferr == nil && res != nil && (res.Status == domain.CallOK || res.Status == domain.CallFallback) {
			res.FromFallback = true
			res.FallbackFrom = e.manifest.Name
			res.Status = domain.CallFallback
			r.log.Info("技能兜底成功", "skill", e.manifest.Name, "fallback", name,
				"fallbackVersion", res.Version, "durationMs", res.DurationMS)
			return res
		}
	}
	return nil
}

// executeWithRetry 执行技能并按清单配置重试。
//
// 重试退避为 100ms/300ms/900ms（受 ctx 取消约束）；超时类错误最多重试 1 次。
func (r *Runner) executeWithRetry(ctx context.Context, cc *domain.CallContext, e *entry, input map[string]any) (out map[string]any, attempts int, err error, timedOut bool) {
	retries := e.manifest.Retries
	if retries < 0 {
		retries = 0
	}
	maxAttempts := retries + 1
	timeout := r.timeoutFor(e)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && timedOut && attempts >= 1+timeoutRetryLimit {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, attempts, err, timedOut
		}
		attempts++
		out, err, timedOut = r.invokeOnce(ctx, cc, e, input, timeout)
		if err == nil {
			return out, attempts, nil, false
		}
		if ctx.Err() != nil {
			// 调用方上下文已取消/超时，不再重试。
			return nil, attempts, err, timedOut
		}
		if attempt < maxAttempts-1 {
			if timedOut && attempts >= 1+timeoutRetryLimit {
				break
			}
			if !sleepWithContext(ctx, retryBackoff(attempt)) {
				return nil, attempts, err, timedOut
			}
		}
	}
	return nil, attempts, err, timedOut
}

// invokeOnce 在受限 goroutine 中执行一次 handler 调用。
func (r *Runner) invokeOnce(ctx context.Context, cc *domain.CallContext, e *entry, input map[string]any, timeout time.Duration) (map[string]any, error, bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 沙箱隔离：传入深拷贝入参与 CallContext 副本，handler 无法篡改调用方数据。
	in := deepCopyMap(input)
	ccCopy := cloneCallContext(cc)

	type outcome struct {
		out   map[string]any
		err   error
		panic any
		stack string
	}
	ch := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- outcome{panic: p, stack: string(debug.Stack())}
			}
		}()
		result, err := e.handler(attemptCtx, ccCopy, in)
		ch <- outcome{out: result, err: err}
	}()

	select {
	case o := <-ch:
		if o.panic != nil {
			// panic 细节只落日志，不返回给调用方（避免泄露内部实现与堆栈）。
			r.log.Error("技能执行 panic 已捕获", "skill", e.manifest.Name, "version", e.manifest.Version,
				"panic", fmt.Sprintf("%v", o.panic), "stack", o.stack)
			return nil, fmt.Errorf("技能 %s 执行 panic: %v", e.manifest.Name, o.panic), false
		}
		if o.err != nil {
			to := errors.Is(o.err, context.DeadlineExceeded)
			if attemptCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				to = true
			}
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				to = false
			}
			return nil, o.err, to
		}
		if o.out == nil {
			o.out = map[string]any{}
		}
		return o.out, nil, false
	case <-attemptCtx.Done():
		// 超时中断：handler 若未响应取消会继续在后台运行至结束（无进程级隔离时的已知限制）。
		return nil, fmt.Errorf("%w: 技能 %s 执行超过 %s", ErrSkillTimeout, e.manifest.Name, timeout), true
	}
}

// timeoutFor 返回技能单次执行超时。
func (r *Runner) timeoutFor(e *entry) time.Duration {
	r.mu.Lock()
	override, ok := r.timeoutOverride[e.manifest.Name]
	r.mu.Unlock()
	if ok && override > 0 {
		return override
	}
	sec := e.manifest.Timeout
	if sec <= 0 {
		sec = r.cfg.DefaultTimeoutSec
	}
	if sec <= 0 {
		sec = 60
	}
	return time.Duration(sec) * time.Second
}

// finish 汇总一次调用的审计留痕与统计。
func (r *Runner) finish(cc *domain.CallContext, e *entry, res *domain.SkillResult, in map[string]any, out map[string]any, started time.Time, fallbackFrom string) {
	duration := time.Since(started).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	res.DurationMS = duration
	if fallbackFrom != "" {
		res.FromFallback = true
		res.FallbackFrom = fallbackFrom
	}

	call := domain.SkillCall{
		ID:           uuid.NewString(),
		Skill:        res.Skill,
		Version:      res.Version,
		Status:       res.Status,
		InputDigest:  digestOf(in),
		OutputDigest: digestOf(out),
		Error:        res.Error,
		Attempts:     res.Attempts,
		DurationMS:   duration,
		FromFallback: fallbackFrom != "",
		Output:       sanitizeValue(out),
		StartedAt:    started,
	}
	if cc != nil {
		call.RunID = cc.RunID
		call.TenantID = cc.TenantID
		call.Stage = cc.Stage
	}
	if r.rec != nil {
		r.rec.SkillCall(call)
	}

	r.reg.recordCall(res.Skill, res.Version, duration, failedStatus(res.Status), res.Error)

	r.mu.Lock()
	st := r.stats[res.Skill]
	if st == nil {
		st = &callStat{}
		r.stats[res.Skill] = st
	}
	st.calls++
	st.latencySumMS += duration
	if failedStatus(res.Status) {
		st.failures++
	}
	r.mu.Unlock()
}

// Stats 返回各技能调用/失败/平均延迟（供 /skills 接口使用）。
func (r *Runner) Stats() []domain.SkillMetric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.SkillMetric, 0, len(r.stats))
	for name, st := range r.stats {
		avg := 0.0
		if st.calls > 0 {
			avg = float64(st.latencySumMS) / float64(st.calls)
		}
		out = append(out, domain.SkillMetric{
			Skill:        name,
			Calls:        int(st.calls),
			Failures:     int(st.failures),
			AvgLatencyMS: avg,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Skill < out[j].Skill })
	return out
}

// failedStatus 判断调用状态是否计入失败。
func failedStatus(s domain.CallStatus) bool {
	switch s {
	case domain.CallFailed, domain.CallTimeout, domain.CallBlocked:
		return true
	default:
		return false
	}
}

// retryBackoff 计算第 attempt 次重试的退避时长：100ms / 300ms / 900ms（上限 900ms）。
func retryBackoff(attempt int) time.Duration {
	d := retryBaseBackoff
	for i := 0; i < attempt; i++ {
		d *= 3
		if d >= retryMaxBackoff {
			return retryMaxBackoff
		}
	}
	if d > retryMaxBackoff {
		d = retryMaxBackoff
	}
	return d
}

// sleepWithContext 可被 ctx 中断的退避等待。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// cloneCallContext 复制调用上下文，保证 handler 无法篡改调用方字段；
// PinnedCommits 做浅拷贝，Sandbox 句柄按原值透传（仅沙箱阶段有值，可能为 nil）。
func cloneCallContext(cc *domain.CallContext) *domain.CallContext {
	if cc == nil {
		return &domain.CallContext{}
	}
	cp := *cc
	if cc.PinnedCommits != nil {
		pinned := make(map[string]string, len(cc.PinnedCommits))
		for k, v := range cc.PinnedCommits {
			pinned[k] = v
		}
		cp.PinnedCommits = pinned
	}
	return &cp
}

// deepCopyMap 深拷贝入参（map/slice 递归复制），保证 handler 无法篡改调用方数据。
func deepCopyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue 深拷贝单个 JSON 风格值；其余类型按值传递。
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = deepCopyValue(item)
		}
		return out
	case []string:
		return append([]string(nil), t...)
	case []int:
		return append([]int(nil), t...)
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, item := range t {
			out[i] = deepCopyMap(item)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(t))
		for k, val := range t {
			out[k] = val
		}
		return out
	default:
		return v
	}
}

// digestOf 计算 JSON 摘要（sha256 前 16 位十六进制），用于审计留痕。
func digestOf(v any) string {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		raw = []byte(fmt.Sprintf("%v", v))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:digestLength]
}

// sensitiveKeys 审计输出脱敏的敏感字段关键字。
var sensitiveKeys = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "accesskey",
	"authorization", "private_key", "privatekey", "credential", "cookie", "session",
}

// sanitizeValue 递归脱敏并截断超长文本，保证审计输出安全且可序列化。
func sanitizeValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if isSensitiveKey(k) {
				out[k] = "***"
				continue
			}
			out[k] = sanitizeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = sanitizeValue(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = sanitizeValue(item)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = truncateString(item)
		}
		return out
	case string:
		return truncateString(t)
	default:
		return v
	}
}

// isSensitiveKey 判断字段名是否为敏感字段。
func isSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// truncateString 截断超长字符串。
func truncateString(s string) string {
	if len(s) <= maxAuditStringLength {
		return s
	}
	return s[:maxAuditStringLength] + "...(已截断)"
}
