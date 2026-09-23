// Package mcp 实现 MCP 模型管控层（第六层）。
//
// 本层是系统与"模型"之间唯一的通道，职责四件事：
//
//	① 输入标准化：prompt.go 把证据上下文渲染成统一的消息与结构化输出约束；
//	② 输出强约束：schema.go 解析/修复/校验模型输出，失败可纠错重试；
//	③ 智能调度：gateway.go 按阶段分层选模型（加权轮询 + 降级链），
//	   并统一负责重试、熔断、兜底与配额；
//	④ 全链路留痕：每次真实调用写入 domain.ModelCall（含 Token、耗时、是否兜底/修复）。
//
// 离线可用：仅当 cfg.Mock.Enabled 显式开启时装配内置 Mock 推理器（mock.go），
// 便于离线联调；生产环境默认不开启，避免无网络时悄悄拿到假推理结果。
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeagent/backend/internal/config"
	"github.com/codeagent/backend/internal/domain"
	"github.com/codeagent/backend/internal/platform/logx"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// 简版熔断器
//
// 本包内自实现（不依赖 internal/skill，避免第六层与第五层产生并行依赖）：
// 连续失败达到 FaiureThreshold 次 → open；冷却 CooldownSec 秒后进入 half_open，
// 只放行 HalfOpenProbes 个探测请求；探测成功 → closed，失败 → 立刻重新 open。
// 全部状态由 mutex 保护，可并发调用。
// ---------------------------------------------------------------------------

// 熔断状态取值（与 domain.ModelHealth.CircuitState 约定一致）。
const (
	circuitClosed   = "closed"
	circuitOpen     = "open"
	circuitHalfOpen = "half_open"
)

// 熔断配置缺省值。
const (
	defaultFailureThreshold = 3
	defaultCooldownSec      = 30
	defaultHalfOpenProbes   = 1
)

// circuitBreaker 简版熔断器（并发安全）。
type circuitBreaker struct {
	threshold      int
	cooldown       time.Duration
	halfOpenProbes int

	mu                  sync.Mutex
	state               string
	consecutiveFailures int
	totalFailures       int64
	probesInFlight      int
	openUntil           time.Time
	lastError           string
}

// newCircuitBreaker 依据配置构造熔断器（非法值回落到缺省值）。
func newCircuitBreaker(cfg config.CircuitConfig) *circuitBreaker {
	threshold := cfg.FailureThreshold
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}
	cooldownSec := cfg.CooldownSec
	if cooldownSec <= 0 {
		cooldownSec = defaultCooldownSec
	}
	probes := cfg.HalfOpenProbes
	if probes <= 0 {
		probes = defaultHalfOpenProbes
	}
	return &circuitBreaker{
		threshold:      threshold,
		cooldown:       time.Duration(cooldownSec) * time.Second,
		halfOpenProbes: probes,
		state:          circuitClosed,
	}
}

// transitionLocked 惰性把过期的 open 转为 half_open。
func (c *circuitBreaker) transitionLocked(now time.Time) {
	if c.state == circuitOpen && !now.Before(c.openUntil) {
		c.state = circuitHalfOpen
		c.probesInFlight = 0
	}
}

// State 返回当前状态（含惰性状态迁移）。
func (c *circuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transitionLocked(time.Now())
	return c.state
}

// CanAttempt 非消费式判断当前是否允许尝试（用于候选排序，不占用半开探测额度）。
func (c *circuitBreaker) CanAttempt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transitionLocked(time.Now())
	switch c.state {
	case circuitOpen:
		return false
	case circuitHalfOpen:
		return c.probesInFlight < c.halfOpenProbes
	default:
		return true
	}
}

// Allow 申请一次调用额度（会占用半开探测名额）。
func (c *circuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transitionLocked(time.Now())
	switch c.state {
	case circuitOpen:
		return false
	case circuitHalfOpen:
		if c.probesInFlight >= c.halfOpenProbes {
			return false
		}
	}
	c.probesInFlight++
	return true
}

// Success 记录一次成功调用：熔断器回到 closed。
func (c *circuitBreaker) Success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = circuitClosed
	c.consecutiveFailures = 0
	c.probesInFlight = 0
	c.openUntil = time.Time{}
	c.lastError = ""
}

// Failure 记录一次失败调用：半开探测失败立即 open，闭态累计到阈值后 open。
func (c *circuitBreaker) Failure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probesInFlight > 0 {
		c.probesInFlight--
	}
	c.totalFailures++
	if err != nil {
		c.lastError = err.Error()
	}
	if c.state == circuitHalfOpen {
		c.state = circuitOpen
		c.openUntil = time.Now().Add(c.cooldown)
		return
	}
	c.consecutiveFailures++
	if c.consecutiveFailures >= c.threshold {
		c.state = circuitOpen
		c.openUntil = time.Now().Add(c.cooldown)
	}
}

// ConsecutiveFailures 返回当前连续失败次数。
func (c *circuitBreaker) ConsecutiveFailures() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveFailures
}

// OpenUntil 返回熔断恢复时间（closed 时为零值）。
func (c *circuitBreaker) OpenUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openUntil
}

// LastError 返回最近一次失败原因。
func (c *circuitBreaker) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

// ---------------------------------------------------------------------------
// 提供方绑定与统计
// ---------------------------------------------------------------------------

// providerBinding 提供方与其熔断器、健康统计的绑定关系。
type providerBinding struct {
	name        string
	kind        string
	models      []string
	tier        domain.ModelTier
	weight      int
	baseURL     string
	maxTokens   int
	temperature float64
	prov        Provider
	circuit     *circuitBreaker

	calls        atomic.Int64
	failures     atomic.Int64
	latencySumMS atomic.Int64
	lastError    atomic.Value // string
}

// newProviderBinding 构造绑定关系。
func newProviderBinding(prov Provider, cfg config.ProviderConfig) *providerBinding {
	models := normalizeModels(cfg.Models)
	if len(models) == 0 {
		if t := prov.Target(); strings.TrimSpace(t.Model) != "" {
			models = []string{t.Model}
		}
	}
	weight := cfg.Weight
	if weight <= 0 {
		weight = 1
	}
	if weight > 10 {
		weight = 10
	}
	b := &providerBinding{
		name:        prov.Name(),
		kind:        strings.ToLower(strings.TrimSpace(cfg.Kind)),
		models:      models,
		tier:        normalizeTier(cfg.Tier),
		weight:      weight,
		baseURL:     cfg.BaseURL,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		prov:        prov,
		circuit:     newCircuitBreaker(config.CircuitConfig{}),
	}
	b.lastError.Store("")
	return b
}

// primaryModel 返回主模型名。
func (b *providerBinding) primaryModel() string {
	if len(b.models) > 0 {
		return b.models[0]
	}
	return ""
}

// target 生成调用目标。
func (b *providerBinding) target(model string) domain.ModelTarget {
	if strings.TrimSpace(model) == "" {
		model = b.primaryModel()
	}
	return domain.ModelTarget{
		Provider:    b.name,
		Model:       model,
		Tier:        b.tier,
		Endpoint:    b.baseURL,
		MaxTokens:   b.maxTokens,
		Temperature: b.temperature,
	}
}

// recordCall 记录一次调用结果。
func (b *providerBinding) recordCall(durMS int64, ok bool, err error) {
	b.calls.Add(1)
	b.latencySumMS.Add(durMS)
	if ok {
		return
	}
	b.failures.Add(1)
	if err != nil {
		b.lastError.Store(err.Error())
	}
}

// avgLatencyMS 返回平均延迟。
func (b *providerBinding) avgLatencyMS() float64 {
	calls := b.calls.Load()
	if calls == 0 {
		return 0
	}
	return float64(b.latencySumMS.Load()) / float64(calls)
}

// lastErrorString 返回最近失败信息。
func (b *providerBinding) lastErrorString() string {
	if v, ok := b.lastError.Load().(string); ok {
		return v
	}
	return ""
}

// gatewayStats 模型层统计（并发安全）。
type gatewayStats struct {
	totalCalls       atomic.Int64
	failedCalls      atomic.Int64
	fallbackCalls    atomic.Int64
	repairedOutputs  atomic.Int64
	promptTokens     atomic.Int64
	completionTokens atomic.Int64
	totalTokens      atomic.Int64
	latencySumMS     atomic.Int64
	latencyCount     atomic.Int64

	mu      sync.Mutex
	byModel map[string]int64
	byTier  map[string]int64
}

// newGatewayStats 构造统计容器。
func newGatewayStats() *gatewayStats {
	return &gatewayStats{byModel: map[string]int64{}, byTier: map[string]int64{}}
}

// record 记录一次真实提供方调用。
func (s *gatewayStats) record(model string, tier domain.ModelTier, usage domain.ModelUsage, durMS int64, ok bool) {
	s.totalCalls.Add(1)
	if !ok {
		s.failedCalls.Add(1)
	}
	s.promptTokens.Add(int64(usage.PromptTokens))
	s.completionTokens.Add(int64(usage.CompletionTokens))
	s.totalTokens.Add(int64(usage.TotalTokens))
	s.latencySumMS.Add(durMS)
	s.latencyCount.Add(1)
	s.mu.Lock()
	if model != "" {
		s.byModel[model]++
	}
	s.byTier[string(tier)]++
	s.mu.Unlock()
}

// snapshot 生成统计快照。
func (s *gatewayStats) snapshot() domain.ModelStats {
	out := domain.ModelStats{
		TotalCalls:       s.totalCalls.Load(),
		FailedCalls:      s.failedCalls.Load(),
		FallbackCalls:    s.fallbackCalls.Load(),
		RepairedOutputs:  s.repairedOutputs.Load(),
		PromptTokens:     s.promptTokens.Load(),
		CompletionTokens: s.completionTokens.Load(),
		TotalTokens:      s.totalTokens.Load(),
		ByModel:          map[string]int64{},
		ByTier:           map[string]int64{},
	}
	if n := s.latencyCount.Load(); n > 0 {
		out.AvgLatencyMS = float64(s.latencySumMS.Load()) / float64(n)
	}
	s.mu.Lock()
	for k, v := range s.byModel {
		out.ByModel[k] = v
	}
	for k, v := range s.byTier {
		out.ByTier[k] = v
	}
	s.mu.Unlock()
	return out
}

// ---------------------------------------------------------------------------
// 错误类型
// ---------------------------------------------------------------------------

// TokenBudgetError 表示一次运行的模型 Token 消耗已超过调用方设定的预算。
//
// 调用方应使用 errors.As(err, &tokenErr) 识别该错误并做降级处理
// （例如收窄上下文预算、改用轻量模型或直接产出降级报告）。
type TokenBudgetError struct {
	RunID string
	Used  int64
	Limit int64
}

// Error 实现 error 接口。
func (e *TokenBudgetError) Error() string {
	return fmt.Sprintf("运行 %s 的模型 Token 消耗已达 %d，超过预算 %d，已停止推理", e.RunID, e.Used, e.Limit)
}

// ModelUnavailableError 表示当前没有任何可用模型（未配置 / 全部熔断 / 全部不健康）。
type ModelUnavailableError struct {
	Stage  string
	Reason string
}

// Error 实现 error 接口。
func (e *ModelUnavailableError) Error() string {
	return fmt.Sprintf("阶段 %s 没有可用的模型提供方：%s", e.Stage, e.Reason)
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

// Gateway 模型管控层入口，实现 domain.MCPGateway。
type Gateway struct {
	cfg config.MCPConfig
	eng config.EngineConfig
	rec domain.Recorder
	log *logx.Logger

	// mu 保护 bindings / schedule / cursors 的**整体替换**。
	// 控制台修改 AI 设置后会全量重建这三个结构，必须保证并发推理读到一致快照。
	// 单个 binding 内部的熔断器与计数器自带锁，不在本锁保护范围内。
	mu       sync.RWMutex
	bindings []*providerBinding
	schedule map[domain.ModelTier][]int
	cursors  map[domain.ModelTier]*atomic.Uint64

	stats *gatewayStats
	// runTokens 运行级 Token 累计（仅在调用方通过 SetTokenBudget 设定预算后才参与拦截）。
	runTokens   sync.Map // runID -> *atomic.Int64
	tokenBudget atomic.Int64
}

// 编译期断言：Gateway 必须满足 domain.MCPGateway 契约。
var _ domain.MCPGateway = (*Gateway)(nil)

// NewGateway 构造模型管控层。
//
// 构造过程"失败不阻断启动"：
//   - 单个 provider 构造失败（例如未配置 API Key）只记 warn 并跳过；
//   - 未配置任何 provider 时**不再偷偷注入 Mock**：Mock 只在 cfg.Mock.Enabled
//     显式开启时装配，避免生产环境拿到假推理结果；
//   - 一个可用 provider 都没有时网关以"空网关"形式返回（Chat 返回
//     ModelUnavailableError），由控制台 AI 设置页引导管理员补齐配置。
func NewGateway(cfg config.MCPConfig, eng config.EngineConfig, rec domain.Recorder, log *logx.Logger) (*Gateway, error) {
	lg := loggerOr(log)
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	g := &Gateway{
		cfg:      cfg,
		eng:      eng,
		rec:      rec,
		log:      lg,
		schedule: map[domain.ModelTier][]int{},
		cursors:  map[domain.ModelTier]*atomic.Uint64{},
		stats:    newGatewayStats(),
	}

	for _, pc := range cfg.Providers {
		prov, err := NewProvider(pc, timeout, lg)
		if err != nil {
			lg.Warn("模型提供方不可用，已跳过", "provider", pc.Name, "kind", pc.Kind, "error", err.Error())
			continue
		}
		g.addBinding(prov, pc)
		lg.Info("模型提供方已装配", "provider", prov.Name(), "kind", pc.Kind, "tier", normalizeTier(pc.Tier), "models", normalizeModels(pc.Models))
	}

	if len(g.bindings) == 0 && cfg.Mock.Enabled {
		model := strings.TrimSpace(cfg.Mock.Model)
		if model == "" {
			model = "mock-reasoner-v1"
		}
		mockCfg := config.ProviderConfig{
			Name:        "mock",
			Kind:        "mock",
			Models:      []string{model},
			Tier:        string(domain.TierStrong),
			MaxTokens:   8192,
			Temperature: 0.1,
			Weight:      1,
		}
		g.addBinding(NewMockProvider(mockCfg, cfg.Mock.LatencyMS, lg), mockCfg)
		lg.Warn("已启用内置 Mock 推理（仅用于离线联调，生产环境请配置真实模型）", "model", model)
	}

	if len(g.bindings) == 0 {
		lg.Warn("未配置任何可用的模型提供方，推理能力不可用；请在控制台「AI 设置」中添加模型")
	}

	if !cfg.Enabled {
		lg.Warn("mcp.enabled=false：模型管控层仍完成装配，请确认调度层不会发起推理")
	}
	g.rebuildScheduleLocked()
	return g, nil
}

// ReloadProviders 热重载全部模型提供方（控制台「AI 设置」保存后立即生效）。
//
// 语义是"全量替换"而非增量合并：以入参 cfgs 为唯一真源重建绑定关系，
// 已有的调用统计与熔断状态随之重置（配置已变，旧状态不再有参考意义）。
//
// 全部 provider 都构造失败时不改动现有配置并返回错误，
// 避免一次误操作把可用的模型层清空。
func (g *Gateway) ReloadProviders(cfgs []config.ProviderConfig) (loaded int, skipped []string, err error) {
	lg := loggerOr(g.log)
	timeout := time.Duration(g.cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	effective := cfgs
	if len(effective) == 0 && g.cfg.Mock.Enabled {
		model := strings.TrimSpace(g.cfg.Mock.Model)
		if model == "" {
			model = "mock-reasoner-v1"
		}
		effective = []config.ProviderConfig{{
			Name: "mock", Kind: "mock", Models: []string{model},
			Tier: string(domain.TierStrong), MaxTokens: 8192, Temperature: 0.1, Weight: 1,
		}}
	}

	bindings := make([]*providerBinding, 0, len(effective))
	for _, pc := range effective {
		prov, perr := NewProvider(pc, timeout, lg)
		if perr != nil {
			skipped = append(skipped, pc.Name+": "+perr.Error())
			lg.Warn("模型提供方不可用，已跳过", "provider", pc.Name, "kind", pc.Kind, "error", perr.Error())
			continue
		}
		b := newProviderBinding(prov, pc)
		b.circuit = newCircuitBreaker(g.cfg.Circuit)
		bindings = append(bindings, b)
	}
	if len(bindings) == 0 && len(effective) > 0 {
		return 0, skipped, errors.New("没有任何可用的模型提供方，已保留原有配置")
	}

	g.mu.Lock()
	g.bindings = bindings
	g.rebuildScheduleLocked()
	g.mu.Unlock()

	for _, b := range bindings {
		lg.Info("模型提供方已重载", "provider", b.name, "kind", b.kind, "tier", b.tier, "models", b.models)
	}
	lg.Info("模型提供方配置已热重载", "loaded", len(bindings), "skipped", len(skipped))
	return len(bindings), skipped, nil
}

// TestProvider 用给定的临时配置做一次最小连通性探测（不落库、不参与调度）。
//
// 返回往返耗时、实际使用的模型与错误：HTTP 层错误会被包装为可读信息，
// API Key 等敏感内容在进入错误之前已被 provider 层脱敏。
func (g *Gateway) TestProvider(ctx context.Context, pc config.ProviderConfig) (durationMS int64, model string, err error) {
	lg := loggerOr(g.log)
	timeout := time.Duration(g.cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	prov, err := NewProvider(pc, timeout, lg)
	if err != nil {
		return 0, "", err
	}
	if models := normalizeModels(pc.Models); len(models) > 0 {
		model = models[0]
	}
	if model == "" {
		model = strings.TrimSpace(prov.Target().Model)
	}
	if model == "" {
		return 0, "", errors.New("未指定要测试的模型名称")
	}
	maxTokens := pc.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16
	}
	start := time.Now()
	_, err = prov.Chat(ctx, ProviderRequest{
		Model:       model,
		Messages:    []domain.ModelMessage{{Role: "user", Content: "ping"}},
		Temperature: 0,
		MaxTokens:   maxTokens,
		Stage:       "connectivity_probe",
	})
	durationMS = time.Since(start).Milliseconds()
	if err != nil {
		return durationMS, model, err
	}
	return durationMS, model, nil
}

// addBinding 注册提供方绑定。
func (g *Gateway) addBinding(prov Provider, cfg config.ProviderConfig) {
	b := newProviderBinding(prov, cfg)
	b.circuit = newCircuitBreaker(g.cfg.Circuit)
	g.bindings = append(g.bindings, b)
}

// rebuildScheduleLocked 构造分层加权轮询表（调用方必须持有写锁）。
//
// 调度策略说明（加权轮询 WRR）：
// 同层每个提供方按 cfg.Providers[].Weight 展开成 schedule 槽位（权重 3 即占 3 个槽位），
// 每次调度把游标前移一格，从游标处沿 schedule 取"首个不重复"的提供方作为首选，
// 其余按顺序作为降级候选。这样在多次调用中，首选概率严格正比于权重，
// 同时每次调用都自带完整的降级链（同层其它 → fallback 层 → 任意健康模型）。
func (g *Gateway) rebuildScheduleLocked() {
	schedule := map[domain.ModelTier][]int{}
	for i, b := range g.bindings {
		for w := 0; w < b.weight; w++ {
			schedule[b.tier] = append(schedule[b.tier], i)
		}
	}
	g.schedule = schedule
	g.cursors = map[domain.ModelTier]*atomic.Uint64{}
	for tier := range schedule {
		g.cursors[tier] = &atomic.Uint64{}
	}
}

// tierOrderLocked 返回某层按加权轮询展开的候选顺序（调用方必须持有读锁）。
func (g *Gateway) tierOrderLocked(tier domain.ModelTier) []*providerBinding {
	sched := g.schedule[tier]
	if len(sched) == 0 {
		return nil
	}
	cursor := g.cursors[tier]
	if cursor == nil {
		return nil
	}
	start := int((cursor.Add(1) - 1) % uint64(len(sched)))
	out := make([]*providerBinding, 0, len(sched))
	seen := make(map[int]bool, len(sched))
	for i := 0; i < len(sched); i++ {
		idx := sched[(start+i)%len(sched)]
		if seen[idx] || idx < 0 || idx >= len(g.bindings) {
			continue
		}
		seen[idx] = true
		out = append(out, g.bindings[idx])
	}
	return out
}

// candidate 一次调度候选（binding + 可选的模型覆盖）。
type candidate struct {
	binding *providerBinding
	model   string
}

// selectCandidates 依据阶段与请求选择候选模型序列。
//
// 优先级：PinnedModel（跳过智能调度）→ 阶段所在层（加权轮询）→ fallback 层 → 任意健康模型。
// 熔断 open 或已判定不健康的提供方会被跳过（因此"全部不可用"时返回空列表）。
func (g *Gateway) selectCandidates(stage string, req domain.ModelRequest) []candidate {
	// 整个候选构造在读锁内完成：热重载会整体替换 bindings/schedule/cursors，
	// 必须保证本次调度看到的是同一份配置快照。
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]candidate, 0, len(g.bindings))
	seen := make(map[*providerBinding]bool, len(g.bindings))
	add := func(b *providerBinding, model string) {
		if b == nil || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, candidate{binding: b, model: model})
	}

	if pin := req.PinnedModel; pin != nil && strings.TrimSpace(pin.Provider) != "" {
		for _, b := range g.bindings {
			if strings.EqualFold(b.name, pin.Provider) || strings.EqualFold(b.kind, pin.Provider) {
				add(b, strings.TrimSpace(pin.Model))
				break
			}
		}
	}
	tier := g.decideTier(stage, req.Tier)
	for _, b := range g.tierOrderLocked(tier) {
		add(b, "")
	}
	if tier != domain.TierFallback {
		for _, b := range g.tierOrderLocked(domain.TierFallback) {
			add(b, "")
		}
	}
	for _, b := range g.bindings {
		add(b, "")
	}

	avail := make([]candidate, 0, len(out))
	for _, c := range out {
		if !c.binding.prov.Healthy() {
			continue
		}
		if !c.binding.circuit.CanAttempt() {
			continue
		}
		avail = append(avail, c)
	}
	return avail
}

// decideTier 依据路由配置决定模型层级。
//
// StrongStages 强制 strong；LightStages + CostAware 走 light；
// 两者都未命中时使用请求自带的 Tier，最后兜底到 strong（质量优先）。
func (g *Gateway) decideTier(stage string, reqTier domain.ModelTier) domain.ModelTier {
	s := strings.ToLower(strings.TrimSpace(stage))
	if containsFold(g.cfg.Routing.StrongStages, s) {
		return domain.TierStrong
	}
	if g.cfg.Routing.CostAware && containsFold(g.cfg.Routing.LightStages, s) {
		return domain.TierLight
	}
	if strings.TrimSpace(string(reqTier)) != "" {
		return normalizeTier(string(reqTier))
	}
	if containsFold(g.cfg.Routing.LightStages, s) {
		return domain.TierLight
	}
	return domain.TierStrong
}

// Chat 执行一次受管控的模型推理，实现 domain.MCPGateway。
//
// 调度：PinnedModel 优先；否则按阶段分层 + 加权轮询选首选，失败沿
// "同层其它 → fallback 层 → 任意健康模型"链路降级。
//
// 容错：每个候选最多尝试 cfg.MaxRetries+1 次（超时/网络/5xx/429 可重试，
// 4xx 参数或鉴权错误不重试），退避 200ms / 600ms，全程受 ctx 约束。
//
// 输出强约束的降级约定（**调用方必须知晓**）：
//   - req.Schema 非空时，解析或 Schema 校验失败会做**一次纠错重试**（回灌校验错误）；
//   - 纠错后仍失败则切换到下一个模型；若所有模型都拿不到合法结构化结果，
//     返回 Structured=nil、Text=最后一次的原始文本、Repaired=false、err=nil 的降级响应
//     —— err 为 nil 表示"链路可用但结构化失败"，调用方应据此降级（例如走纯文本报告或人工复核），
//     不要把它当成系统故障；
//   - 只有当所有候选都不可用（全部熔断/不健康/未配置）或全部调用出错且**没有拿到任何文本**时，
//     才返回非 nil 的 error（同时尽量给出带诊断信息的降级响应：Attempts/Target/DurationMS）。
//
// 记录：每次真实提供方调用都会写入一条 domain.ModelCall；日志只打印长度与阶段，
// 绝不打印完整 prompt（debug 级别最多打印前 200 字符）。
func (g *Gateway) Chat(ctx context.Context, cc *domain.CallContext, req domain.ModelRequest) (*domain.ModelResponse, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	stage := strings.TrimSpace(req.Stage)
	if stage == "" && cc != nil {
		stage = strings.TrimSpace(cc.Stage)
	}
	lg := g.loggerFor(cc, stage)

	runID := ""
	if cc != nil {
		runID = cc.RunID
	}

	// 配额管控：EngineConfig 中没有可直接引用的 Token 上限（只有超时/轮次），
	// 因此默认不拦截；调度层可通过 SetTokenBudget 显式设定运行级 Token 预算。
	if err := g.checkTokenBudget(runID); err != nil {
		lg.Warn("模型 Token 预算已超限，跳过推理", "used", err.Used, "limit", err.Limit)
		g.recordModelCall(cc, stage, domain.ModelTarget{}, domain.CallBlocked, 0, "", domain.ModelUsage{}, 0, start, err.Error(), false)
		return &domain.ModelResponse{Truncated: true, DurationMS: time.Since(start).Milliseconds()}, err
	}

	candidates := g.selectCandidates(stage, req)
	if len(candidates) == 0 {
		err := &ModelUnavailableError{Stage: stage, Reason: "全部提供方熔断、不健康或未配置"}
		lg.Error("没有可用的模型提供方", "error", err.Error(), "providers", len(g.bindings))
		return nil, err
	}
	if pin := req.PinnedModel; pin != nil && strings.TrimSpace(pin.Provider) != "" && !g.hasProvider(pin.Provider) {
		lg.Warn("指定的 PinnedModel 不存在，已回退到智能调度", "pinned", pin.Provider)
	}

	var (
		totalUsage   domain.ModelUsage
		attempts     int
		lastText     string
		lastTarget   domain.ModelTarget
		lastErr      error
		fallbackFrom string
		firstTarget  domain.ModelTarget
	)

	for i, cand := range candidates {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		b := cand.binding
		if !b.circuit.Allow() {
			g.recordModelCall(cc, stage, b.target(cand.model), domain.CallBlocked, attempts, fallbackFrom, domain.ModelUsage{}, 0, time.Now(), "提供方处于熔断状态，已跳过", false)
			continue
		}
		model, temp, maxTokens := g.resolveParams(cand, req)
		target := b.target(model)
		if i == 0 {
			firstTarget = target
		} else if fallbackFrom == "" {
			// 记录首次兜底的来源模型，便于报告与统计定位。
			fallbackFrom = targetKey(firstTarget)
			g.stats.fallbackCalls.Add(1)
		}
		lastTarget = target

		maxAttempts := g.maxAttempts()
		var (
			text      string
			usage     domain.ModelUsage
			candErr   error
			callMS    int64
			callStart time.Time
		)
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			attempts++
			preq := ProviderRequest{
				Model:       model,
				Messages:    req.Messages,
				Temperature: temp,
				MaxTokens:   maxTokens,
				JSONMode:    req.Schema != nil,
				Schema:      req.Schema,
				Stage:       stage,
			}
			callStart = time.Now()
			resp, err := b.prov.Chat(ctx, preq)
			durMS := time.Since(callStart).Milliseconds()
			if err != nil {
				candErr = err
				b.circuit.Failure(err)
				b.recordCall(durMS, false, err)
				g.stats.record(model, b.tier, domain.ModelUsage{}, durMS, false)
				status := domain.CallFailed
				if isTimeoutError(err) {
					status = domain.CallTimeout
				}
				g.recordModelCall(cc, stage, target, status, attempts, fallbackFrom, domain.ModelUsage{}, durMS, callStart, err.Error(), false)
				lg.Warn("模型调用失败", "provider", b.name, "model", model, "attempt", attempt,
					"status", httpStatusOf(err), "retryable", isRetryableError(err), "durationMs", durMS,
					"promptChars", messageChars(req.Messages), "error", err.Error())
				if !isRetryableError(err) || attempt == maxAttempts {
					break
				}
				if !sleepBackoff(ctx, attempt) {
					candErr = ctx.Err()
					break
				}
				continue
			}

			b.circuit.Success()
			b.recordCall(durMS, true, nil)
			text = resp.Text
			usage = resp.Usage
			callMS = durMS
			totalUsage = addUsage(totalUsage, usage)
			lg.Debug("模型调用成功", "provider", b.name, "model", model, "attempt", attempt,
				"durationMs", durMS, "promptChars", messageChars(req.Messages),
				"completionChars", len(text), "preview", previewForLog(text, 200))
			break
		}

		if strings.TrimSpace(text) == "" {
			lastErr = candErr
			if lastErr == nil {
				lastErr = errors.New("模型返回内容为空")
			}
			continue
		}

		// 无结构化约束：直接返回文本。
		if req.Schema == nil {
			g.addRunTokens(runID, totalUsage.TotalTokens)
			g.stats.record(model, b.tier, usage, callMS, true)
			g.recordModelCall(cc, stage, target, domain.CallOK, attempts, fallbackFrom, usage, callMS, callStart, "", true)
			return &domain.ModelResponse{
				Text:         text,
				Target:       target,
				Usage:        totalUsage,
				Attempts:     attempts,
				DurationMS:   time.Since(start).Milliseconds(),
				FallbackFrom: fallbackFrom,
			}, nil
		}

		// 结构化输出：解析 + 校验，失败做一次纠错重试。
		doc, parseErr := ParseStructured(text, req.Schema)
		repaired := false
		if parseErr != nil {
			attempts++
			repairMsgs := BuildRepairMessages(req.Messages, splitProblems(parseErr), text)
			repairReq := ProviderRequest{
				Model:       model,
				Messages:    repairMsgs,
				Temperature: temp,
				MaxTokens:   maxTokens,
				JSONMode:    true,
				Schema:      req.Schema,
				Stage:       stage,
			}
			repairStart := time.Now()
			rresp, rerr := b.prov.Chat(ctx, repairReq)
			repairMS := time.Since(repairStart).Milliseconds()
			switch {
			case rerr != nil:
				b.recordCall(repairMS, false, rerr)
				g.stats.record(model, b.tier, domain.ModelUsage{}, repairMS, false)
				g.recordModelCall(cc, stage, target, domain.CallFailed, attempts, fallbackFrom, domain.ModelUsage{}, repairMS, repairStart, rerr.Error(), false)
				lg.Warn("结构化输出纠错重试失败", "provider", b.name, "model", model, "error", rerr.Error())
			default:
				b.recordCall(repairMS, true, nil)
				totalUsage = addUsage(totalUsage, rresp.Usage)
				if doc2, err2 := ParseStructured(rresp.Text, req.Schema); err2 == nil {
					doc, text, repaired = doc2, rresp.Text, true
					g.stats.repairedOutputs.Add(1)
					lg.Info("结构化输出经纠错重试后通过校验", "provider", b.name, "model", model, "stage", stage)
				} else {
					parseErr = err2
					text = rresp.Text
					g.stats.record(model, b.tier, rresp.Usage, repairMS, false)
					g.recordModelCall(cc, stage, target, domain.CallFallback, attempts, fallbackFrom, rresp.Usage, repairMS, repairStart, err2.Error(), false)
				}
			}
		}

		if doc != nil {
			g.addRunTokens(runID, totalUsage.TotalTokens)
			g.stats.record(model, b.tier, usage, callMS, true)
			g.recordModelCall(cc, stage, target, domain.CallOK, attempts, fallbackFrom, usage, callMS, callStart, "", true)
			return &domain.ModelResponse{
				Structured:   doc,
				Text:         text,
				Target:       target,
				Usage:        totalUsage,
				Attempts:     attempts,
				DurationMS:   time.Since(start).Milliseconds(),
				FallbackFrom: fallbackFrom,
				Repaired:     repaired,
			}, nil
		}

		// 该模型无法给出合法结构化输出：记录后换下一个候选。
		lastText = text
		lastErr = parseErr
		g.stats.record(model, b.tier, usage, callMS, false)
		g.recordModelCall(cc, stage, target, domain.CallFallback, attempts, fallbackFrom, usage, callMS, callStart, parseErr.Error(), false)
		lg.Warn("结构化输出校验失败，切换下一个模型", "provider", b.name, "model", model, "error", parseErr.Error())
	}

	// 走到这里说明所有候选都没能给出合法结果。
	g.addRunTokens(runID, totalUsage.TotalTokens)

	if strings.TrimSpace(lastText) != "" {
		// 降级不失败：拿到了文本但结构不合法，返回降级响应（err 为 nil，Structured/Repaired 标记）。
		lg.Warn("结构化输出全部失败，返回降级响应", "stage", stage, "attempts", attempts)
		return &domain.ModelResponse{
			Text:         lastText,
			Target:       lastTarget,
			Usage:        totalUsage,
			Attempts:     attempts,
			DurationMS:   time.Since(start).Milliseconds(),
			FallbackFrom: fallbackFrom,
			Repaired:     false,
		}, nil
	}

	err := lastErr
	if err == nil {
		err = errors.New("模型调用全部失败")
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("模型调用被取消: %w", ctx.Err())
	}
	wrapped := fmt.Errorf("阶段 %s 的模型调用全部失败（尝试 %d 次）: %w", stage, attempts, err)
	lg.Error("模型调用全部失败", "stage", stage, "attempts", attempts, "error", err.Error())
	if attempts > 0 {
		// 带上诊断信息返回，便于上层记录与降级。
		return &domain.ModelResponse{
			Text:         lastText,
			Target:       lastTarget,
			Usage:        totalUsage,
			Attempts:     attempts,
			DurationMS:   time.Since(start).Milliseconds(),
			FallbackFrom: fallbackFrom,
		}, wrapped
	}
	return nil, wrapped
}

// resolveParams 归一化一次调用的模型与采样参数。
func (g *Gateway) resolveParams(c candidate, req domain.ModelRequest) (string, float64, int) {
	b := c.binding
	model := strings.TrimSpace(c.model)
	if model == "" {
		model = b.primaryModel()
	}
	temp := req.Temperature
	if temp <= 0 && req.PinnedModel != nil {
		temp = req.PinnedModel.Temperature
	}
	if temp <= 0 {
		temp = b.temperature
	}
	if temp <= 0 {
		temp = 0.1
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 && req.PinnedModel != nil {
		maxTokens = req.PinnedModel.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = b.maxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return model, temp, maxTokens
}

// hasProvider 判断是否存在指定名称（或 kind）的提供方。
func (g *Gateway) hasProvider(name string) bool {
	name = strings.TrimSpace(name)
	for _, b := range g.bindings {
		if strings.EqualFold(b.name, name) || strings.EqualFold(b.kind, name) {
			return true
		}
	}
	return false
}

// maxAttempts 返回单候选最大尝试次数（含首次）。
func (g *Gateway) maxAttempts() int {
	n := g.cfg.MaxRetries + 1
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	return n
}

// loggerFor 返回带链路字段的日志器。
func (g *Gateway) loggerFor(cc *domain.CallContext, stage string) *logx.Logger {
	lg := g.log
	if cc != nil && cc.Logger != nil {
		lg = cc.Logger
	}
	return lg.With("stage", stage)
}

// recordModelCall 写入模型调用记录（rec 为 nil 时安全跳过）。
func (g *Gateway) recordModelCall(cc *domain.CallContext, stage string, target domain.ModelTarget,
	status domain.CallStatus, attempt int, fallbackFrom string,
	usage domain.ModelUsage, durMS int64, started time.Time, errText string, schemaValid bool) {
	if g.rec == nil {
		return
	}
	var runID, tenantID string
	if cc != nil {
		runID, tenantID = cc.RunID, cc.TenantID
	}
	g.rec.ModelCall(domain.ModelCall{
		ID:               uuid.NewString(),
		RunID:            runID,
		TenantID:         tenantID,
		Stage:            stage,
		Provider:         target.Provider,
		Model:            target.Model,
		Tier:             target.Tier,
		Status:           status,
		Attempt:          attempt,
		FallbackFrom:     fallbackFrom,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		DurationMS:       durMS,
		Error:            truncateRunes(errText, 500),
		SchemaValid:      schemaValid,
		StartedAt:        started,
	})
}

// Providers 返回已配置的提供方与模型（每个模型一项）。
func (g *Gateway) Providers() []domain.ModelTarget {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]domain.ModelTarget, 0, len(g.bindings))
	for _, b := range g.bindings {
		for _, m := range b.models {
			out = append(out, b.target(m))
		}
		if len(b.models) == 0 {
			out = append(out, b.target(""))
		}
	}
	return out
}

// Health 返回各模型健康度。
//
// 熔断器按提供方维度生效：同一提供方的多个模型共享熔断状态与调用计数。
func (g *Gateway) Health(ctx context.Context) []domain.ModelHealth {
	_ = ctx
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]domain.ModelHealth, 0, len(g.bindings))
	for _, b := range g.bindings {
		state := b.circuit.State()
		healthy := b.prov.Healthy() && state != circuitOpen
		models := b.models
		if len(models) == 0 {
			models = []string{""}
		}
		for _, m := range models {
			out = append(out, domain.ModelHealth{
				Provider:     b.name,
				Model:        m,
				Tier:         b.tier,
				Healthy:      healthy,
				CircuitState: state,
				Failures:     b.circuit.ConsecutiveFailures(),
				AvgLatencyMS: b.avgLatencyMS(),
				TotalCalls:   b.calls.Load(),
				LastError:    truncateRunes(b.lastErrorString(), 300),
				OpenUntil:    b.circuit.OpenUntil(),
			})
		}
	}
	return out
}

// Stats 返回模型层统计（调用数、失败数、兜底数、修复数、Token 累计、平均延迟、按模型/层级计数）。
func (g *Gateway) Stats(ctx context.Context) domain.ModelStats {
	_ = ctx
	return g.stats.snapshot()
}

// SetTokenBudget 设置单次运行的 Token 预算（<=0 表示不限制）。
//
// 说明：EngineConfig 与 domain.CallContext 中都没有可直接引用的"已消耗 Token"字段，
// 因此本实现由 Gateway 自行按 RunID 累计消耗，仅在调度层显式调用 SetTokenBudget
// 后才做拦截；未设置时该逻辑整体跳过（不影响正常推理）。
func (g *Gateway) SetTokenBudget(perRun int) {
	g.tokenBudget.Store(int64(perRun))
}

// ResetRunTokens 清理某次运行的 Token 累计（运行结束时调用，避免长时间运行的内存增长）。
func (g *Gateway) ResetRunTokens(runID string) {
	if runID == "" {
		return
	}
	g.runTokens.Delete(runID)
}

// addRunTokens 累计运行级 Token 消耗。
func (g *Gateway) addRunTokens(runID string, tokens int) {
	if runID == "" || tokens <= 0 {
		return
	}
	v, _ := g.runTokens.LoadOrStore(runID, &atomic.Int64{})
	if counter, ok := v.(*atomic.Int64); ok {
		counter.Add(int64(tokens))
	}
}

// checkTokenBudget 校验运行级 Token 预算。
func (g *Gateway) checkTokenBudget(runID string) *TokenBudgetError {
	limit := g.tokenBudget.Load()
	if limit <= 0 || runID == "" {
		return nil
	}
	v, ok := g.runTokens.Load(runID)
	if !ok {
		return nil
	}
	counter, ok := v.(*atomic.Int64)
	if !ok {
		return nil
	}
	used := counter.Load()
	if used >= limit {
		return &TokenBudgetError{RunID: runID, Used: used, Limit: limit}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// sleepBackoff 指数退避（200ms / 600ms），受 ctx 约束；返回 false 表示被取消。
func sleepBackoff(ctx context.Context, attempt int) bool {
	d := 600 * time.Millisecond
	if attempt <= 1 {
		d = 200 * time.Millisecond
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

// addUsage 累加 Token 消耗。
func addUsage(a, b domain.ModelUsage) domain.ModelUsage {
	return domain.ModelUsage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
}

// splitProblems 把解析错误拆成逐条校验问题，用于纠错提示词。
func splitProblems(err error) []string {
	if err == nil {
		return nil
	}
	msg := strings.TrimPrefix(err.Error(), ErrUnstructuredOutput.Error()+": ")
	parts := strings.FieldsFunc(msg, func(r rune) bool { return r == '；' || r == '|' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		out = append(out, msg)
	}
	return out
}

// targetKey 返回"提供方/模型"形式的标识，用于回填 FallbackFrom。
func targetKey(t domain.ModelTarget) string {
	if strings.TrimSpace(t.Provider) == "" {
		return strings.TrimSpace(t.Model)
	}
	if strings.TrimSpace(t.Model) == "" {
		return t.Provider
	}
	return t.Provider + "/" + t.Model
}

// containsFold 忽略大小写判断切片是否包含目标值。
func containsFold(list []string, target string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	for _, v := range list {
		if strings.ToLower(strings.TrimSpace(v)) == target {
			return true
		}
	}
	return false
}
