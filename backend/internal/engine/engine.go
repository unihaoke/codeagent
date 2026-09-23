package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/codeagent/backend/internal/store"
)

// ---------------------------------------------------------------------------
// 包级错误：供 API 层通过 errors.Is 映射统一响应码（CONTRACT §2.1）
// ---------------------------------------------------------------------------

var (
	// ErrUnauthorized 未认证：缺少有效主体或 API Key（映射 HTTP 401）。
	ErrUnauthorized = errors.New("未认证：缺少有效的主体或凭证")
	// ErrForbidden 无权限 / 越权访问（映射 HTTP 403）。
	ErrForbidden = errors.New("无权限：主体不具备所需 scope 或租户已暂停")
	// ErrNotFound 资源不存在（映射 HTTP 404）。
	ErrNotFound = errors.New("资源不存在")
	// ErrQuotaExceeded 配额/队列超限（映射 HTTP 429）。
	ErrQuotaExceeded = errors.New("配额超限")
	// ErrConflict 状态冲突（映射 HTTP 409）。
	ErrConflict = errors.New("状态冲突")
	// ErrBadRequest 参数语义校验失败（映射 HTTP 400）。
	ErrBadRequest = errors.New("请求参数不合法")
)

// ValidationError 字段级语义校验错误，Unwrap 到 ErrBadRequest 以便 errors.Is 判定。
type ValidationError struct {
	// Field 出错的字段名。
	Field string
	// Msg 人类可读的中文说明。
	Msg string
}

// Error 实现 error 接口。
func (e *ValidationError) Error() string {
	return fmt.Sprintf("参数校验失败[%s]：%s", e.Field, e.Msg)
}

// Unwrap 返回 ErrBadRequest，使 errors.Is(err, ErrBadRequest) 成立。
func (e *ValidationError) Unwrap() error { return ErrBadRequest }

// ---------------------------------------------------------------------------
// 引擎实现
// ---------------------------------------------------------------------------

const (
	// stopGrace 优雅停止的最长等待时间：超时后仍返回，避免进程退出被阻塞。
	stopGrace = 15 * time.Second
	// callbackTimeout 终态回调的超时时间，保证 worker 不被外部系统拖垮。
	callbackTimeout = 5 * time.Second
	// defaultQueueSize 未配置队列长度时的兜底值。
	defaultQueueSize = 128
	// defaultWorkers 未配置 worker 数量时的兜底值。
	defaultWorkers = 4
)

// Engine Agent 调度核心实现（domain.TaskEngine）。
//
// 职责：任务受理（权限 → 校验 → 幂等 → 配额 → 入队）、worker pool 执行、
// 状态机迁移、取消、重跑与统计查询。所有 map/计数字段的访问均加锁。
type Engine struct {
	cfg  *config.Config
	st   *store.Store
	pipe domain.TaskPipeline
	rec  domain.Recorder
	bus  domain.EventBus
	auth domain.Authorizer
	log  *logx.Logger

	httpClient *http.Client
	// publicURL 对外可访问的基础地址，用于回调/响应生成报告跳转链接。
	publicURL string

	mu      sync.Mutex
	queue   chan string
	running map[string]*runHandle
	// pending 已受理但尚未出队就被取消的 run 集合（worker 出队时据此直接判取消）。
	pending map[string]struct{}
	started bool
	stopped bool
	stopCh  chan struct{}
	baseCtx context.Context
	wg      sync.WaitGroup
	cbWg    sync.WaitGroup
}

// runHandle 一个在执行的运行句柄：支持取消 + 记录引擎侧已校验的状态。
type runHandle struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	// state 引擎已校验通过的最新状态（流水线不直接改状态机，只由本层推进）。
	state domain.TaskState
}

// stageHookSetter 可选扩展：流水线支持阶段进度回调时由 Engine 注册（避免预迁移状态）。
type stageHookSetter interface {
	SetStageHook(StageHook)
}

// submitAuthorizer 可选扩展：租户权限层若实现了 CanSubmitTask 则优先调用。
//
// 说明：domain.Authorizer 为冻结契约，未定义 CanSubmitTask，故此处用可选接口探测；
// 未实现时退化为 scope 判定（task:write / admin:all / Admin 主体）。
type submitAuthorizer interface {
	CanSubmitTask(sub *domain.Subject) error
}

// NewEngine 创建调度核心。
func NewEngine(cfg *config.Config, st *store.Store, pipe domain.TaskPipeline, rec domain.Recorder,
	bus domain.EventBus, auth domain.Authorizer, log *logx.Logger) (*Engine, error) {
	if cfg == nil {
		return nil, fmt.Errorf("配置不能为空")
	}
	if st == nil {
		return nil, fmt.Errorf("数据访问层不能为空")
	}
	if pipe == nil {
		return nil, fmt.Errorf("任务流水线不能为空")
	}
	if log == nil {
		log = logx.Default()
	}
	q := cfg.Engine.QueueSize
	if q <= 0 {
		q = defaultQueueSize
	}
	return &Engine{
		cfg:        cfg,
		st:         st,
		pipe:       pipe,
		rec:        rec,
		bus:        bus,
		auth:       auth,
		log:        log,
		httpClient: &http.Client{Timeout: callbackTimeout},
		queue:      make(chan string, q),
		running:    map[string]*runHandle{},
		pending:    map[string]struct{}{},
		baseCtx:    context.Background(),
	}, nil
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// SetPublicURL 设置对外可访问的基础地址（含协议与端口），用于回调报文与开放接口
// 响应中拼装报告跳转链接；建议在 Start 之前装配。
func (e *Engine) SetPublicURL(u string) { e.publicURL = strings.TrimRight(u, "/") }

// Start 启动 worker pool。重复调用幂等（已启动则直接返回）。
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return nil
	}
	if e.stopped {
		e.mu.Unlock()
		e.log.Warn("引擎已停止，忽略重复启动")
		return nil
	}
	e.started = true
	if ctx != nil {
		e.baseCtx = ctx
	}
	e.stopCh = make(chan struct{})
	if s, ok := e.pipe.(stageHookSetter); ok {
		// 阶段进度回调：让状态机随真实阶段推进（analyzing → repairing → verifying）。
		s.SetStageHook(e.onStage)
	}
	workers := e.cfg.Engine.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	for i := 0; i < workers; i++ {
		e.wg.Add(1)
		go e.worker()
	}
	queueSize := cap(e.queue)
	e.mu.Unlock()

	e.audit(domain.AuditEvent{Category: "task", Action: "engine.start", Level: "info",
		Message: fmt.Sprintf("调度引擎已启动：workers=%d，queueSize=%d", workers, queueSize)})
	return nil
}

// Stop 停止接收新任务并等待在途任务（最多 15s），随后把仍在排队的任务标记为 failed。
// 重复调用幂等。
func (e *Engine) Stop() error {
	e.mu.Lock()
	if !e.started || e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	close(e.stopCh)
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopGrace):
		e.log.Warn("引擎停止等待超时，仍有任务在途", "grace", stopGrace.String())
	}
	e.drainQueue()

	e.mu.Lock()
	inflight := len(e.running)
	e.mu.Unlock()
	e.audit(domain.AuditEvent{Category: "task", Action: "engine.stop", Level: "info",
		Message: fmt.Sprintf("调度引擎已停止，停止时在途任务 %d 个", inflight)})
	return nil
}

// drainQueue 把停止时仍排队的任务标记为失败（避免永久停留在 queued 的假状态）。
func (e *Engine) drainQueue() {
	for {
		select {
		case runID := <-e.queue:
			e.failQueued(runID, "调度引擎已停止，任务未执行")
		default:
			return
		}
	}
}

func (e *Engine) failQueued(runID, reason string) {
	run, ok := e.st.GetRunRaw(runID)
	if !ok || run.State.IsTerminal() {
		return
	}
	from := run.State
	run.State = domain.StateFailed
	run.Error = reason
	run.EndedAt = time.Now()
	run.UpdatedAt = run.EndedAt
	_ = e.st.UpdateRun(run)
	e.publishState(run, from, domain.StateFailed)
	e.publish(domain.Event{
		Type: "task.terminal", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Level: "error", Message: reason, Payload: map[string]any{"state": string(run.State)},
	})
}

func (e *Engine) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.stopCh:
			return
		case runID := <-e.queue:
			e.execute(runID)
		}
	}
}

// ---------------------------------------------------------------------------
// 任务受理
// ---------------------------------------------------------------------------

// Submit 受理任务：权限校验 → 模式与目标校验 → 幂等去重 → 配额管控 → 创建运行 → 入队。
func (e *Engine) Submit(ctx context.Context, sub *domain.Subject, req domain.CreateTaskRequest) (*domain.TaskRun, error) {
	tenantID, err := e.resolveTenant(sub, req)
	if err != nil {
		return nil, err
	}
	if err := e.checkSubmitScope(sub); err != nil {
		e.audit(domain.AuditEvent{TenantID: tenantID, Category: "auth", Action: "task.submit", Level: "error",
			Message: err.Error(), RequestID: req.RequestID})
		return nil, err
	}
	if sub != nil && strings.TrimSpace(req.TenantID) != "" && req.TenantID != sub.TenantID {
		msg := fmt.Sprintf("请求体租户（%s）与主体租户（%s）不一致，已以主体租户为准", req.TenantID, sub.TenantID)
		e.log.Warn(msg, "run", req.RequestID)
		e.audit(domain.AuditEvent{TenantID: tenantID, Category: "auth", Action: "tenant.mismatch", Level: "warn",
			Message: msg, RequestID: req.RequestID})
	}
	req.TenantID = tenantID
	return e.submit(ctx, tenantID, req, submitOpts{})
}

// submitOpts 内部受理参数（重跑等场景复用同一条受理路径）。
type submitOpts struct {
	// taskID 复用已有逻辑任务（重跑场景）。
	taskID string
	// attempt 本次运行的人工序号（重跑为上一轮 +1）。
	attempt int
	// base 重跑来源运行：用于复用已固化的任务上下文。
	base *domain.TaskRun
	// feedback 人工反馈（RerunWithFeedback）。
	feedback string
	// key 显式幂等键（重跑必须生成新键，避免自我去重）。
	key string
	// requestID 链路追踪 ID。
	requestID string
}

// submit 受理核心：调用方必须已完成权限校验。
func (e *Engine) submit(ctx context.Context, tenantID string, req domain.CreateTaskRequest, opts submitOpts) (*domain.TaskRun, error) {
	mode, repoID, groupID, repoIDs, err := e.validateTarget(tenantID, &req)
	if err != nil {
		return nil, err
	}

	quota := domain.DefaultQuota()
	if tenant, ok := e.st.GetTenant(tenantID); ok {
		if tenant.Status == domain.TenantSuspended {
			return nil, fmt.Errorf("%w: 租户已被暂停，禁止提交任务", ErrForbidden)
		}
		quota = tenant.Quota
	}

	key := opts.key
	if strings.TrimSpace(key) == "" {
		key = buildIdempotencyKey(tenantID, mode, repoID, groupID, req.Ref, req.Stacktrace, "")
	}

	// 幂等判定 → 配额判定 → 创建运行 → 入队：必须原子完成，
	// 否则并发提交会重复入队或突破租户并发上限。
	e.mu.Lock()
	defer e.mu.Unlock()

	if existing, ok := e.st.FindRunByIdempotencyKey(tenantID, key); ok {
		if !existing.State.IsTerminal() {
			e.publishIdempotentHit(existing, "命中幂等：任务已在执行中，复用已有运行")
			return existing, nil
		}
		if e.withinIdempotencyTTL(existing) {
			e.publishIdempotentHit(existing, "命中幂等：复用 TTL 内已完成的运行")
			return existing, nil
		}
	}

	if quota.MaxConcurrentTasks > 0 {
		if active := e.countActive(tenantID); active >= quota.MaxConcurrentTasks {
			msg := fmt.Sprintf("当前运行中任务数（%d）已达租户并发上限（%d），请稍后重试", active, quota.MaxConcurrentTasks)
			e.audit(domain.AuditEvent{TenantID: tenantID, Category: "quota", Action: "task.submit.rejected",
				Level: "warn", Message: msg, RequestID: req.RequestID})
			return nil, fmt.Errorf("%w: %s", ErrQuotaExceeded, msg)
		}
	}

	now := time.Now()
	attempt := opts.attempt
	if attempt <= 0 {
		attempt = 1
	}

	// 逻辑任务：同一 IdempotencyKey 复用，RunCount 累加。
	var task *domain.Task
	if opts.taskID != "" {
		if t, ok := e.st.GetTask(tenantID, opts.taskID); ok {
			task = t
		}
	}
	newTask := task == nil
	if newTask {
		task = &domain.Task{
			ID: uuid.NewString(), TenantID: tenantID, Title: req.Title, Mode: mode,
			GroupID: groupID, Source: req.Source, IdempotencyKey: key,
			CreatedAt: now, UpdatedAt: now, RunCount: 1,
		}
	} else {
		task.RunCount++
		task.UpdatedAt = now
		if task.Title == "" {
			task.Title = req.Title
		}
		if task.Mode == "" {
			task.Mode = mode
		}
		if task.GroupID == "" {
			task.GroupID = groupID
		}
	}

	run := &domain.TaskRun{
		ID: uuid.NewString(), TaskID: task.ID, TenantID: tenantID,
		Attempt: attempt, Mode: mode, State: domain.StateQueued,
		Title: req.Title, Environment: req.Environment,
		Stacktrace: req.Stacktrace, Logs: req.Logs,
		RequestedRef: req.Ref, RepoIDs: append([]string{}, repoIDs...),
		EntryFiles:     append([]string{}, req.EntryFiles...),
		IdempotencyKey: key, RequestID: firstNonEmpty(opts.requestID, req.RequestID),
		CallbackURL: req.CallbackURL,
		CreatedAt:   now, UpdatedAt: now,
	}
	if opts.base != nil {
		// 多轮修复：复用固化上下文，保证"同一份代码、同一份堆栈"（CONTRACT §6.8）。
		run.PinnedCommits = cloneStrMap(opts.base.PinnedCommits)
		run.Resolution = append([]domain.RepoResolved{}, opts.base.Resolution...)
		run.Warnings = carryOverWarnings(opts.base.Warnings)
		run.Severity = opts.base.Severity
		if run.Mode == "" {
			run.Mode = opts.base.Mode
		}
		if len(run.RepoIDs) == 0 {
			run.RepoIDs = append([]string{}, opts.base.RepoIDs...)
		}
		if run.Stacktrace == "" {
			run.Stacktrace = opts.base.Stacktrace
		}
		if run.Logs == "" {
			run.Logs = opts.base.Logs
		}
		if run.RequestedRef == "" {
			run.RequestedRef = opts.base.RequestedRef
		}
		if run.Title == "" {
			run.Title = opts.base.Title
		}
		if run.Environment == "" {
			run.Environment = opts.base.Environment
		}
		if len(run.EntryFiles) == 0 {
			run.EntryFiles = append([]string{}, opts.base.EntryFiles...)
		}
	}
	if opts.feedback != "" {
		run.Warnings = append(run.Warnings, warnFeedbackPrefix+strings.TrimSpace(opts.feedback))
	}
	if !req.AutoVerifyEnabled() {
		run.Warnings = append(run.Warnings, warnAutoVerifyOffPrefix+"：调用方在提交任务时关闭了沙箱验证，补丁仅作建议")
	}

	task.LatestRunID = run.ID
	if newTask {
		if err := e.st.CreateTask(task); err != nil {
			return nil, fmt.Errorf("创建逻辑任务失败：%w", err)
		}
	} else {
		if err := e.st.UpdateTask(task); err != nil {
			return nil, fmt.Errorf("更新逻辑任务失败：%w", err)
		}
	}
	if err := e.st.CreateRun(run); err != nil {
		return nil, fmt.Errorf("创建执行记录失败：%w", err)
	}

	// 入队：队列满时按"快速失败"策略拒绝，而不是在 HTTP 处理线程里同步执行
	// （CONTRACT §6.9 要求 worker pool 无阻塞，同步执行会让接入层被长任务拖垮）。
	select {
	case e.queue <- run.ID:
	default:
		msg := "任务队列已满，请稍后重试"
		from := run.State
		run.State = domain.StateFailed
		run.Error = msg
		run.EndedAt = now
		run.UpdatedAt = now
		_ = e.st.UpdateRun(run)
		e.publishState(run, from, domain.StateFailed)
		e.publish(domain.Event{
			Type: "task.terminal", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Level: "error", Message: msg, Payload: map[string]any{"state": string(run.State)},
		})
		e.audit(domain.AuditEvent{TenantID: tenantID, RunID: run.ID, TaskID: run.TaskID,
			Category: "quota", Action: "task.enqueue.rejected", Level: "warn", Message: msg})
		return nil, fmt.Errorf("%w: %s", ErrQuotaExceeded, msg)
	}

	e.publish(domain.Event{
		Type: "task.created", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Message: "任务已受理", Payload: run,
	})
	e.audit(domain.AuditEvent{TenantID: tenantID, RunID: run.ID, TaskID: run.TaskID,
		Category: "task", Action: "task.created", Level: "info",
		Message: fmt.Sprintf("任务已受理：mode=%s attempt=%d", run.Mode, run.Attempt), RequestID: run.RequestID})
	e.log.Info("任务已受理", "run", run.ID, "mode", run.Mode, "attempt", run.Attempt)
	return run, nil
}

// validateTarget 推断并校验任务模式与目标，返回模式、单仓库 ID、分组 ID 与候选仓库范围。
func (e *Engine) validateTarget(tenantID string, req *domain.CreateTaskRequest) (domain.TaskMode, string, string, []string, error) {
	mode := req.Mode
	if mode == "" {
		if strings.TrimSpace(req.GroupID) != "" {
			mode = domain.ModeGroup
		} else {
			mode = domain.ModeSingleRepo
		}
	}
	switch mode {
	case domain.ModeSingleRepo:
		if strings.TrimSpace(req.RepoID) == "" {
			return "", "", "", nil, &ValidationError{Field: "repoId", Msg: "单仓库模式必须提供 repoId"}
		}
		repo, ok := e.st.GetRepo(tenantID, req.RepoID)
		if !ok {
			return "", "", "", nil, fmt.Errorf("%w: 仓库不存在或不属于当前租户", ErrNotFound)
		}
		if repo.Status == domain.RepoDisabled {
			return "", "", "", nil, fmt.Errorf("%w: 仓库 %s 已禁用", ErrForbidden, repo.Key)
		}
		return mode, repo.ID, "", []string{repo.ID}, nil
	case domain.ModeGroup:
		if strings.TrimSpace(req.GroupID) == "" {
			return "", "", "", nil, &ValidationError{Field: "groupId", Msg: "分组模式必须提供 groupId"}
		}
		group, ok := e.st.GetGroup(tenantID, req.GroupID)
		if !ok {
			return "", "", "", nil, fmt.Errorf("%w: 分组不存在或不属于当前租户", ErrNotFound)
		}
		if group.Status == domain.GroupDisabled {
			return "", "", "", nil, fmt.Errorf("%w: 分组 %s 已禁用", ErrForbidden, group.Key)
		}
		views := e.st.GroupMemberViews(tenantID, group.ID)
		if len(views) == 0 {
			return "", "", "", nil, &ValidationError{Field: "groupId", Msg: "分组下没有成员仓库，无法进行多仓库联合排查"}
		}
		ids := make([]string, 0, len(views))
		for _, v := range views {
			ids = append(ids, v.RepositoryID)
		}
		return mode, "", group.ID, ids, nil
	default:
		return "", "", "", nil, &ValidationError{Field: "mode", Msg: "不支持的任务模式：" + string(mode)}
	}
}

// resolveTenant 解析受理租户：主体租户优先；匿名模式按配置开关放行。
func (e *Engine) resolveTenant(sub *domain.Subject, req domain.CreateTaskRequest) (string, error) {
	if sub == nil {
		if !e.cfg.Auth.AllowAnonymous {
			return "", fmt.Errorf("%w: 匿名提交未开启，请携带有效 API Key", ErrUnauthorized)
		}
		if strings.TrimSpace(req.TenantID) == "" {
			return "", fmt.Errorf("%w: 匿名提交必须携带 tenantId", ErrUnauthorized)
		}
		return req.TenantID, nil
	}
	if strings.TrimSpace(sub.TenantID) == "" {
		return "", fmt.Errorf("%w: 主体缺少 tenantId", ErrUnauthorized)
	}
	return sub.TenantID, nil
}

// checkSubmitScope 校验主体是否具备提交任务的权限。
func (e *Engine) checkSubmitScope(sub *domain.Subject) error {
	if sub == nil {
		return nil // 匿名模式已由 Cfg.Auth.AllowAnonymous 放行
	}
	if e.auth != nil {
		if sa, ok := e.auth.(submitAuthorizer); ok {
			if err := sa.CanSubmitTask(sub); err != nil {
				return fmt.Errorf("%w: %v", ErrForbidden, err)
			}
			return nil
		}
	}
	if sub.Admin || sub.Has("task:write") {
		return nil
	}
	return fmt.Errorf("%w: 主体缺少 task:write scope", ErrForbidden)
}

func (e *Engine) publishIdempotentHit(run *domain.TaskRun, msg string) {
	e.publish(domain.Event{
		Type: "task.created", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Level: "info", Message: msg + "（runId=" + run.ID + "）", Payload: run,
	})
	e.audit(domain.AuditEvent{TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Category: "task", Action: "task.idempotent_hit", Level: "info", Message: msg})
}

// withinIdempotencyTTL 判断已完成运行是否仍在幂等 TTL 内（TTL<=0 表示不去重）。
func (e *Engine) withinIdempotencyTTL(run *domain.TaskRun) bool {
	ttl := e.cfg.Engine.IdempotencyTTLSec
	if ttl <= 0 {
		return false
	}
	ref := run.UpdatedAt
	if ref.IsZero() {
		ref = run.CreatedAt
	}
	if ref.IsZero() {
		return false
	}
	return time.Since(ref) <= time.Duration(ttl)*time.Second
}

// countActive 统计租户当前占用并发额度的运行数（queued + 执行中）。
func (e *Engine) countActive(tenantID string) int {
	byState := e.st.CountRunsByState(tenantID)
	return byState[domain.StateQueued] + byState[domain.StateAnalyzing] +
		byState[domain.StateRepairing] + byState[domain.StateVerifying]
}

// buildIdempotencyKey 生成幂等键：sha256(tenantID|mode|repoID|groupID|ref|stacktrace|feedback) 前 32 位。
func buildIdempotencyKey(tenantID string, mode domain.TaskMode, repoID, groupID, ref, stacktrace, feedback string) string {
	h := sha256.New()
	_, _ = io.WriteString(h, strings.Join([]string{tenantID, string(mode), repoID, groupID, ref, stacktrace, feedback}, "|"))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// carryOverWarnings 复制上一轮运行的警告（剔除上一轮的修复轮次噪声）。
func carryOverWarnings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, w := range in {
		if strings.Contains(w, "轮修复验证失败") || strings.Contains(w, "已停止加载并裁剪上下文") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func cloneStrMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// worker 执行
// ---------------------------------------------------------------------------

func (e *Engine) execute(runID string) {
	e.mu.Lock()
	if _, cancelled := e.pending[runID]; cancelled {
		delete(e.pending, runID)
		e.mu.Unlock()
		e.finalizeCancelled(runID)
		return
	}
	run, ok := e.st.GetRunRaw(runID)
	if !ok {
		e.mu.Unlock()
		e.log.Warn("任务出队时运行记录已丢失", "run", runID)
		return
	}
	if run.State.IsTerminal() {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(e.baseCtx, e.cfg.TaskTimeout())
	ctx, cancel2 := context.WithCancel(ctx)
	h := &runHandle{cancel: func() { cancel2(); cancel() }, state: run.State}
	e.running[runID] = h
	e.mu.Unlock()

	defer func() {
		cancel2()
		cancel()
		e.mu.Lock()
		delete(e.running, runID)
		e.mu.Unlock()
	}()

	run.StartedAt = time.Now()
	run.UpdatedAt = run.StartedAt
	if run.CreatedAt.IsZero() {
		run.CreatedAt = run.StartedAt
	}
	e.walk(run, h, domain.StateAnalyzing)

	cc := &domain.CallContext{
		TenantID: run.TenantID, TaskID: run.TaskID, RunID: run.ID,
		RequestID: run.RequestID, PinnedCommits: run.PinnedCommits,
		Logger: e.log.WithRun(run.TenantID, run.TaskID, run.ID, run.RequestID),
	}

	res, err := e.pipe.Execute(ctx, cc, run)

	var report *domain.Report
	var target domain.TaskState
	switch {
	case e.isCancelled(runID) || errors.Is(ctx.Err(), context.Canceled):
		run.Error = "任务已取消"
		target = domain.StateCancelled
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			run.Error = fmt.Sprintf("任务执行超时（%s）", e.cfg.TaskTimeout())
		} else {
			run.Error = err.Error()
		}
		target = domain.StateFailed
		e.log.Error("任务执行失败", "run", run.ID, "err", err)
	default:
		if res != nil {
			if res.Run != nil {
				run = res.Run
			}
			report = res.Report
			target = res.State
		}
		if target == "" {
			target = DecideState(run)
		}
	}

	// 存储侧可能已被 Cancel 写入终态（工作副本是陈旧快照）：必须以存储为准，
	// 否则会出现"取消后又被成功结果覆盖"的状态回退。
	if latest, ok := e.st.GetRunRaw(runID); ok && latest.State.IsTerminal() {
		target = latest.State
	}

	run.UpdatedAt = time.Now()
	if run.EndedAt.IsZero() {
		run.EndedAt = run.UpdatedAt
	}
	// 把工作副本对齐到最终状态后再走状态机（路径为空时直接以终态落库）。
	run.State = target
	e.walk(run, h, target)
	if uerr := e.st.UpdateRun(run); uerr != nil {
		e.log.Warn("运行上下文落库失败", "run", run.ID, "err", uerr)
	}

	// Pipeline 走到 archive 阶段时会自行发布 task.terminal；此处只在异常/取消路径补发。
	if run.ReportID == "" {
		e.publish(domain.Event{
			Type: "task.terminal", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Level: terminalLevel(run.State), Message: "任务进入终态：" + string(run.State),
			Payload: map[string]any{"state": string(run.State), "reportId": run.ReportID},
		})
	}
	if run.CallbackURL != "" && run.State != domain.StateCancelled {
		e.fireCallback(run, report)
	}
}

// finalizeCancelled 处理"未出队即被取消"的任务：状态已由 Cancel 迁移，这里补发终态事件。
func (e *Engine) finalizeCancelled(runID string) {
	run, ok := e.st.GetRunRaw(runID)
	if !ok {
		return
	}
	if run.State != domain.StateCancelled {
		from := run.State
		if from.CanTransitionTo(domain.StateCancelled) {
			run.State = domain.StateCancelled
			run.EndedAt = time.Now()
			run.UpdatedAt = run.EndedAt
			_ = e.st.UpdateRun(run)
			e.publishState(run, from, domain.StateCancelled)
		}
	}
	e.publish(domain.Event{
		Type: "task.terminal", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Level: "warn", Message: "任务已取消", Payload: map[string]any{"state": string(domain.StateCancelled)},
	})
}

// onStage 阶段进度回调：状态机随流水线真实阶段推进。
func (e *Engine) onStage(runID, stage string) {
	switch stage {
	case StagePatchSynthesize:
		e.transitionByID(runID, domain.StateRepairing)
	case StageSandboxVerify:
		e.transitionByID(runID, domain.StateVerifying)
	}
}

func terminalLevel(state domain.TaskState) string {
	switch state {
	case domain.StateFailed:
		return "error"
	case domain.StateCancelled, domain.StateDegraded, domain.StateNeedsReview:
		return "warn"
	default:
		return "info"
	}
}

// ---------------------------------------------------------------------------
// 状态机迁移
// ---------------------------------------------------------------------------

// transitionByID 按 runID 推进状态机。
func (e *Engine) transitionByID(runID string, target domain.TaskState) bool {
	e.mu.Lock()
	h := e.running[runID]
	e.mu.Unlock()

	run, ok := e.st.GetRunRaw(runID)
	if !ok {
		return false
	}
	if h == nil {
		// 尚未出队的任务：按存储状态做单跳校验（queued → cancelled 等）。
		if run.State == target {
			return true
		}
		if run.State.IsTerminal() {
			return false
		}
		if !run.State.CanTransitionTo(target) {
			e.log.Error("非法状态迁移被拒绝", "run", runID, "from", run.State, "to", target)
			e.audit(domain.AuditEvent{TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
				Category: "task", Action: "task.state.rejected", Level: "error",
				Message: fmt.Sprintf("非法状态迁移被拒绝：%s → %s", run.State, target)})
			return false
		}
		from := run.State
		run.State = target
		run.UpdatedAt = time.Now()
		_ = e.st.UpdateRun(run)
		e.publishState(run, from, target)
		return true
	}
	return e.walk(run, h, target)
}

// walk 沿合法迁移路径把运行推进到目标状态（BFS 求最短合法路径），每跳都校验 CanTransitionTo。
func (e *Engine) walk(run *domain.TaskRun, h *runHandle, target domain.TaskState) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	path, ok := statePath(h.state, target)
	if !ok {
		e.log.Error("非法状态迁移被拒绝", "run", run.ID, "from", h.state, "to", target)
		e.audit(domain.AuditEvent{TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Category: "task", Action: "task.state.rejected", Level: "error",
			Message: fmt.Sprintf("非法状态迁移被拒绝：%s → %s", h.state, target)})
		return false
	}
	for _, next := range path {
		from := h.state
		if !from.CanTransitionTo(next) {
			e.log.Error("非法状态迁移被拒绝", "run", run.ID, "from", from, "to", next)
			return false
		}
		h.state = next
		run.State = next
		run.UpdatedAt = time.Now()
		if err := e.st.UpdateRun(run); err != nil {
			e.log.Warn("状态迁移落库失败", "run", run.ID, "err", err)
		}
		e.publishState(run, from, next)
	}
	return true
}

// statePath 求 from → to 的最短合法迁移路径（终态不可作为中间节点）。
func statePath(from, to domain.TaskState) ([]domain.TaskState, bool) {
	if from == to {
		return []domain.TaskState{}, true
	}
	all := []domain.TaskState{
		domain.StateQueued, domain.StateAnalyzing, domain.StateRepairing, domain.StateVerifying,
		domain.StateSucceeded, domain.StateNeedsReview, domain.StateFailed, domain.StateCancelled, domain.StateDegraded,
	}
	type node struct {
		state domain.TaskState
		path  []domain.TaskState
	}
	queue := []node{{state: from}}
	visited := map[domain.TaskState]bool{from: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range all {
			if !cur.state.CanTransitionTo(next) || visited[next] {
				continue
			}
			path := append(append([]domain.TaskState{}, cur.path...), next)
			if next == to {
				return path, true
			}
			if next.IsTerminal() {
				continue // 终态不可作为中间节点
			}
			visited[next] = true
			queue = append(queue, node{state: next, path: path})
		}
	}
	return nil, false
}

func (e *Engine) publishState(run *domain.TaskRun, from, to domain.TaskState) {
	e.publish(domain.Event{
		Type: "task.state", TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
		Level: terminalLevel(to), Message: fmt.Sprintf("状态迁移：%s → %s", from, to),
		Payload: map[string]any{"from": from, "to": to},
	})
}

func (e *Engine) isCancelled(runID string) bool {
	run, ok := e.st.GetRunRaw(runID)
	return ok && run.State == domain.StateCancelled
}

// ---------------------------------------------------------------------------
// 取消 / 重跑
// ---------------------------------------------------------------------------

// Cancel 取消任务：执行中的任务取消其上下文，未出队的任务直接标记跳过；已终态返回 ErrConflict。
func (e *Engine) Cancel(ctx context.Context, tenantID, runID string) error {
	run, ok := e.st.GetRun(tenantID, runID)
	if !ok {
		return fmt.Errorf("%w: 运行记录不存在", ErrNotFound)
	}
	if run.State.IsTerminal() {
		return fmt.Errorf("%w: 任务已处于终态（%s），无法取消", ErrConflict, run.State)
	}

	e.mu.Lock()
	if h, ok := e.running[runID]; ok {
		h.cancel()
	} else {
		// 未出队：登记跳过标记，worker 出队时立即判取消。
		e.pending[runID] = struct{}{}
	}
	e.mu.Unlock()

	if !e.transitionByID(runID, domain.StateCancelled) {
		return fmt.Errorf("%w: 运行状态（%s）不允许取消", ErrConflict, run.State)
	}
	e.audit(domain.AuditEvent{TenantID: tenantID, RunID: run.ID, TaskID: run.TaskID,
		Category: "task", Action: "task.cancelled", Level: "warn", Message: "任务已取消"})
	_ = ctx
	return nil
}

// Retry 基于原运行已固化的上下文（PinnedCommits/Resolution/RepoIDs/模式与堆栈）重跑。
func (e *Engine) Retry(ctx context.Context, tenantID, runID string) (*domain.TaskRun, error) {
	return e.rerun(ctx, tenantID, runID, "")
}

// RerunWithFeedback 携带人工反馈重跑：反馈写入新运行的 Warnings，
// 并由流水线在根因/补丁阶段的模型提示中注入。
func (e *Engine) RerunWithFeedback(ctx context.Context, tenantID, runID, feedback string) (*domain.TaskRun, error) {
	if strings.TrimSpace(feedback) == "" {
		return nil, &ValidationError{Field: "feedback", Msg: "人工反馈不能为空"}
	}
	return e.rerun(ctx, tenantID, runID, feedback)
}

func (e *Engine) rerun(ctx context.Context, tenantID, runID, feedback string) (*domain.TaskRun, error) {
	src, ok := e.st.GetRun(tenantID, runID)
	if !ok {
		return nil, fmt.Errorf("%w: 运行记录不存在", ErrNotFound)
	}
	if !src.State.IsTerminal() {
		return nil, fmt.Errorf("%w: 任务仍在执行中（%s），无法重跑", ErrConflict, src.State)
	}

	var task *domain.Task
	if src.TaskID != "" {
		if t, ok := e.st.GetTask(tenantID, src.TaskID); ok {
			task = t
		}
	}
	req := domain.CreateTaskRequest{
		Mode: src.Mode, Ref: src.RequestedRef, Title: src.Title,
		Stacktrace: src.Stacktrace, Logs: src.Logs,
		EntryFiles:  append([]string{}, src.EntryFiles...),
		Environment: src.Environment, CallbackURL: src.CallbackURL,
		TenantID: tenantID, RequestID: src.RequestID,
	}
	if task != nil {
		req.Source = task.Source
	}
	switch src.Mode {
	case domain.ModeGroup:
		if task != nil {
			req.GroupID = task.GroupID
		}
	case domain.ModeSingleRepo:
		if len(src.RepoIDs) > 0 {
			req.RepoID = src.RepoIDs[0]
		}
	}

	attempt := src.Attempt + 1
	base := strings.TrimSpace(src.IdempotencyKey)
	if base == "" {
		base = buildIdempotencyKey(tenantID, src.Mode, req.RepoID, req.GroupID, src.RequestedRef, src.Stacktrace, "")
	}
	// 多轮修复必须生成新 key（追加尝试序号 / 反馈摘要），避免被自身幂等键去重。
	key := base + "#attempt-" + strconv.Itoa(attempt)
	if feedback != "" {
		key = base + "#fb-" + shortHash(feedback) + "-" + strconv.Itoa(attempt)
	}

	run, err := e.submit(ctx, tenantID, req, submitOpts{
		taskID: src.TaskID, attempt: attempt, base: src,
		feedback: feedback, key: key, requestID: src.RequestID,
	})
	if err != nil {
		return nil, err
	}
	e.audit(domain.AuditEvent{TenantID: tenantID, RunID: run.ID, TaskID: run.TaskID,
		Category: "task", Action: "task.rerun", Level: "info",
		Message: fmt.Sprintf("基于运行 %s 重跑（attempt=%d，feedback=%v，复用锁定版本 %d 个）",
			src.ID, attempt, feedback != "", len(run.PinnedCommits))})
	return run, nil
}

// ---------------------------------------------------------------------------
// 查询与统计
// ---------------------------------------------------------------------------

// GetRun 查询执行详情（含租户隔离校验）。
func (e *Engine) GetRun(ctx context.Context, tenantID, runID string) (*domain.TaskRun, error) {
	run, ok := e.st.GetRun(tenantID, runID)
	if !ok {
		return nil, fmt.Errorf("%w: 运行记录不存在", ErrNotFound)
	}
	_ = ctx
	return run, nil
}

// ListTasks 查询逻辑任务列表。
func (e *Engine) ListTasks(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.Task], error) {
	_ = ctx
	return e.st.ListTasks(tenantID, q), nil
}

// ListRuns 查询执行记录列表（支持 q.State 状态过滤）。
func (e *Engine) ListRuns(ctx context.Context, tenantID string, q domain.PageQuery) (domain.Page[domain.TaskRun], error) {
	_ = ctx
	return e.st.ListRuns(tenantID, q), nil
}

// Stats 返回租户维度的调度层统计。
func (e *Engine) Stats(ctx context.Context, tenantID string) (*domain.EngineStats, error) {
	_ = ctx
	return BuildEngineStats(e.st, tenantID)
}

// QueueLen 返回当前排队中的任务数（并发安全）。
func (e *Engine) QueueLen() int { return len(e.queue) }

// RunningCount 返回当前正在执行的任务数（并发安全）。
func (e *Engine) RunningCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.running)
}

// Workers 返回配置的 worker 数量。
func (e *Engine) Workers() int {
	if e.cfg.Engine.Workers > 0 {
		return e.cfg.Engine.Workers
	}
	return defaultWorkers
}

// ---------------------------------------------------------------------------
// 终态回调
// ---------------------------------------------------------------------------

// callbackPayload 终态回调报文（标准库 net/http POST JSON）。
//
// runId/state/reportId/severity/summary 为契约约定的必备字段，不加 omitempty，
// 保证外部系统始终能读到稳定的键集合；rootCause/patches/reportUrl 为开放接口
// 扩展字段，供第三方直接渲染（如飞书告警）而无需二次拉取。
type callbackPayload struct {
	RunID     string            `json:"runId"`
	State     string            `json:"state"`
	ReportID  string            `json:"reportId"`
	Severity  string            `json:"severity"`
	Summary   string            `json:"summary"`
	TenantID  string            `json:"tenantId"`
	TaskID    string            `json:"taskId"`
	Attempt   int               `json:"attempt"`
	Error     string            `json:"error,omitempty"`
	RootCause *domain.RootCause `json:"rootCause,omitempty"`
	Patches   []callbackPatch   `json:"patches,omitempty"`
	ReportURL string            `json:"reportUrl,omitempty"`
}

// callbackPatch 回调报文中的补丁摘要，供第三方直接展示差异。
type callbackPatch struct {
	RepositoryID string `json:"repositoryId"`
	RepoKey      string `json:"repoKey"`
	FilePath     string `json:"filePath"`
	Action       string `json:"action"`
	UnifiedDiff  string `json:"unifiedDiff"`
	Rationale    string `json:"rationale,omitempty"`
}

// fireCallback 异步回调外部系统；失败指数退避重试（最多 3 次），不阻塞 worker。
func (e *Engine) fireCallback(run *domain.TaskRun, report *domain.Report) {
	if run.CallbackURL == "" {
		return
	}
	payload := callbackPayload{
		RunID: run.ID, State: string(run.State), ReportID: run.ReportID,
		Severity: string(run.Severity), TenantID: run.TenantID, TaskID: run.TaskID,
		Attempt: run.Attempt, Error: run.Error,
	}
	if report != nil {
		payload.Summary = report.Summary
	} else if run.RootCause != nil {
		payload.Summary = run.RootCause.Summary
	}
	// 结构化详情：让第三方（如飞书告警）直接渲染，无需二次拉取。
	if run.RootCause != nil {
		payload.RootCause = run.RootCause
	}
	for _, p := range run.Patches {
	payload.Patches = append(payload.Patches, callbackPatch{
		RepositoryID: p.RepositoryID, RepoKey: p.RepoKey, FilePath: p.FilePath,
		Action: string(p.Action), UnifiedDiff: p.UnifiedDiff, Rationale: p.Rationale,
	})
	}
	if e.publicURL != "" && run.ReportID != "" {
		payload.ReportURL = e.publicURL + "/r/" + run.ReportID
	}

	url := run.CallbackURL
	e.cbWg.Add(1)
	go func() {
		defer e.cbWg.Done()
		body, err := json.Marshal(payload)
		if err != nil {
			e.log.Warn("终态回调报文序列化失败", "run", run.ID, "err", err)
			return
		}
		// 指数退避重试：最多 3 次（1s/2s），外部系统偶发抖动时仍尽量送达。
		const maxAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if e.postCallback(url, body) {
				return
			}
			lastErr = fmt.Errorf("回调第 %d 次失败", attempt)
			if attempt < maxAttempts {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
		}
		e.log.Warn("终态回调最终失败", "run", run.ID, "url", url, "err", lastErr)
		e.audit(domain.AuditEvent{TenantID: run.TenantID, RunID: run.ID, TaskID: run.TaskID,
			Category: "task", Action: "callback.failed", Level: "warn", Message: lastErr.Error()})
	}()
}

// postCallback 向外部系统 POST 回调报文，返回是否送达（2xx）。
func (e *Engine) postCallback(url string, body []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode < 300
}

// ---------------------------------------------------------------------------
// 事件与审计
// ---------------------------------------------------------------------------

func (e *Engine) publish(ev domain.Event) {
	if e.bus == nil {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	e.bus.Publish(ev)
}

func (e *Engine) audit(ev domain.AuditEvent) {
	if e.rec == nil {
		return
	}
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	e.rec.Audit(ev)
}
