package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
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

// Handler 技能处理器别名，等价于 domain.SkillHandler，便于内置技能包引用。
type Handler = domain.SkillHandler

// 注册中心与执行器对外暴露的错误。
var (
	// ErrSkillNotFound 技能或指定版本不存在。
	ErrSkillNotFound = errors.New("技能不存在")
	// ErrSkillDisabled 技能已被禁用。
	ErrSkillDisabled = errors.New("技能已禁用")
	// ErrNoRollback 没有可回滚的历史版本。
	ErrNoRollback = errors.New("没有可回滚的历史版本")
	// ErrInvalidManifest 技能清单非法。
	ErrInvalidManifest = errors.New("技能清单非法")
	// ErrCanaryRejected 灰度未命中且不存在可用稳定版本。
	ErrCanaryRejected = errors.New("灰度版本未生效且无稳定版本可回退")
	// ErrCircuitOpen 技能熔断中。
	ErrCircuitOpen = errors.New("技能熔断中")
	// ErrSkillTimeout 技能执行超时。
	ErrSkillTimeout = errors.New("技能执行超时")
	// ErrHandlerMissing 技能未绑定处理器。
	ErrHandlerMissing = errors.New("技能未绑定处理器")
)

// builtinSkillNames 已知内置技能名集合，用于清单注册校验。
var builtinSkillNames = map[string]struct{}{
	domain.SkillStacktraceParse:     {},
	domain.SkillCodeASTAnalyze:      {},
	domain.SkillErrorLocalize:       {},
	domain.SkillCrossRepoTrace:      {},
	domain.SkillCodePatchSynthesize: {},
	domain.SkillDependencyRepair:    {},
	domain.SkillPatchVerify:         {},
}

// IsBuiltinSkill 判断技能名是否为已知内置技能。
func IsBuiltinSkill(name string) bool {
	_, ok := builtinSkillNames[strings.TrimSpace(name)]
	return ok
}

// entry 单个技能版本的注册条目。
type entry struct {
	manifest domain.SkillManifest
	handler  domain.SkillHandler
	circuit  *Circuit

	registeredAt time.Time
	totalCalls   int64
	successes    int64
	failures     int64
	latencySumMS int64
	lastError    string
}

// Registry 技能注册中心：注册 / 热更新 / 版本管理 / 灰度 / 回滚。
//
// 内部结构为 map[技能名]map[版本]*entry，并按技能名维护“当前生效版本”。
// 同一 Name+Version 重复注册即热更新（覆盖 handler 与清单，复用熔断器与统计）。
type Registry struct {
	mu  sync.RWMutex
	cfg config.SkillConfig
	log *logx.Logger

	skills  map[string]map[string]*entry
	current map[string]string
	// canaryDecision 按技能名缓存稳定的灰度抽样结果，保证同一进程内决策一致（可注入，便于测试）。
	canaryDecision map[string]bool
	rnd            *rand.Rand
}

// NewRegistry 创建技能注册中心。
func NewRegistry(cfg config.SkillConfig, log *logx.Logger) *Registry {
	if log == nil {
		log = logx.Nop()
	}
	return &Registry{
		cfg:            cfg,
		log:            log,
		skills:         make(map[string]map[string]*entry),
		current:        make(map[string]string),
		canaryDecision: make(map[string]bool),
		rnd:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// List 返回技能清单：仅包含 Status != disabled 的技能，每个技能名只返回当前生效版本，
// 并按 Priority 递增排序。
func (r *Registry) List(ctx context.Context, category string) []domain.SkillManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]domain.SkillManifest, 0, len(r.skills))
	for name := range r.skills {
		e := r.currentEntryLocked(name)
		if e == nil || e.manifest.Status == domain.SkillDisabled {
			continue
		}
		if category != "" && string(e.manifest.Category) != category {
			continue
		}
		out = append(out, e.manifest)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get 返回技能当前生效版本的清单。
//
// 说明：与 List 不同，Get 对 disabled/deprecated 版本仍然返回清单（并带 status 字段），
// 否则控制台「启停」接口（先 Get 解析版本再 SetStatus）将无法把已禁用技能重新启用。
// 执行路径的禁用拦截由 SelectHandler 负责，调用方不会误用被禁用的技能。
func (r *Registry) Get(ctx context.Context, name string) (*domain.SkillManifest, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := r.currentEntryLocked(strings.TrimSpace(name))
	if e == nil {
		return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	m := e.manifest
	return &m, nil
}

// Register 注册或热更新技能。
//
// 校验：Name/Version 非空；名称属于内置技能名集合，或显式声明 Builtin=false 以注册自定义技能；
// handler 非空。Status 为空时默认 enabled。注册时套用 cfg.Overrides 与 cfg 默认值补齐
// Timeout/Retries/FailureThreshold/CooldownSeconds，并构造或复用熔断器。
func (r *Registry) Register(ctx context.Context, m domain.SkillManifest, handler domain.SkillHandler) error {
	name := strings.TrimSpace(m.Name)
	version := strings.TrimSpace(m.Version)
	if name == "" || version == "" {
		return fmt.Errorf("%w: Name/Version 不能为空", ErrInvalidManifest)
	}
	if !IsBuiltinSkill(name) && m.Builtin {
		return fmt.Errorf("%w: %s 不是已知内置技能名（自定义技能需设置 Builtin=false）", ErrInvalidManifest, name)
	}
	if handler == nil {
		return fmt.Errorf("%w: %s@%s %s", ErrInvalidManifest, name, version, ErrHandlerMissing)
	}
	statusProvided := m.Status != ""
	r.applyOverride(&m)
	r.applyDefaults(&m, statusProvided)
	if err := validateSkillStatus(m.Status); err != nil {
		return err
	}
	m.Name, m.Version = name, version

	r.mu.Lock()
	defer r.mu.Unlock()

	versions := r.skills[name]
	if versions == nil {
		versions = make(map[string]*entry)
		r.skills[name] = versions
	}
	e := &entry{manifest: m, handler: handler, registeredAt: time.Now()}
	if old := versions[version]; old != nil {
		// 热更新：复用统计与熔断器（阈值变化时重建）。
		e.registeredAt = old.registeredAt
		e.totalCalls = old.totalCalls
		e.successes = old.successes
		e.failures = old.failures
		e.latencySumMS = old.latencySumMS
		e.lastError = old.lastError
		e.circuit = old.circuit
		if e.circuit != nil && (e.circuit.failureThreshold != m.FailureThreshold ||
			e.circuit.cooldown != time.Duration(m.CooldownSeconds)*time.Second) {
			e.circuit = nil
		}
	}
	if e.circuit == nil {
		e.circuit = NewCircuit(m.FailureThreshold, time.Duration(m.CooldownSeconds)*time.Second, 1)
	}
	versions[version] = e

	// 当前生效版本决策：注册即生效（enabled/canary）；disabled/deprecated 版本不抢占当前版本，
	// 但首次注册仍需记录，避免技能名无可用版本。
	cur := r.current[name]
	if m.Status == domain.SkillEnabled || m.Status == domain.SkillCanary {
		r.current[name] = version
	} else if cur == "" {
		r.current[name] = version
	}
	// 配置里显式指定版本时以其为准。
	if ov := r.overrideFor(name); ov != nil && strings.TrimSpace(ov.Version) != "" {
		if _, ok := versions[strings.TrimSpace(ov.Version)]; ok {
			r.current[name] = strings.TrimSpace(ov.Version)
		}
	}
	r.log.Debug("技能注册成功", "skill", name, "version", version, "status", string(m.Status), "current", r.current[name])
	return nil
}

// SetStatus 启停/灰度技能。
//
// 支持 enabled / disabled / canary / deprecated；version 为空时作用于当前生效版本。
// status 为 enabled/canary 时该版本同时成为当前生效版本；禁用当前版本时会自动切换到
// 最高的其他可用版本（若存在）。canary 生效与否按 CanaryPercent 概率抽样，并按技能名
// 缓存稳定决策（可通过 SetCanaryDecision 注入）。
func (r *Registry) SetStatus(ctx context.Context, name, version string, status domain.SkillStatus) error {
	if err := validateSkillStatus(status); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()

	versions := r.skills[name]
	if len(versions) == 0 {
		return fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	ver := strings.TrimSpace(version)
	if ver == "" {
		ver = r.current[name]
	}
	e := versions[ver]
	if e == nil {
		return fmt.Errorf("%w: %s@%s", ErrSkillNotFound, name, ver)
	}
	e.manifest.Status = status
	switch status {
	case domain.SkillEnabled, domain.SkillCanary:
		r.current[name] = ver
		delete(r.canaryDecision, name)
	case domain.SkillDisabled, domain.SkillDeprecated:
		if r.current[name] == ver {
			if next := r.highestEnabledLocked(versions, ver); next != nil {
				r.current[name] = next.manifest.Version
			}
		}
	}
	r.log.Info("技能状态变更", "skill", name, "version", ver, "status", string(status), "current", r.current[name])
	return nil
}

// Rollback 把当前生效版本切回上一个可用版本。
//
// 按语义化版本降序查找第一个状态为 enabled/canary 且不同于当前版本的历史版本；
// 无可回滚版本时返回 ErrNoRollback。
func (r *Registry) Rollback(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()

	versions := r.skills[name]
	if len(versions) == 0 {
		return "", fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	cur := r.current[name]
	ordered := sortVersionsDesc(versions)
	for _, ver := range ordered {
		if ver == cur {
			continue
		}
		e := versions[ver]
		if e == nil {
			continue
		}
		if e.manifest.Status == domain.SkillEnabled || e.manifest.Status == domain.SkillCanary {
			r.current[name] = ver
			delete(r.canaryDecision, name)
			r.log.Info("技能回滚", "skill", name, "from", cur, "to", ver)
			return ver, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoRollback, name)
}

// Health 返回全部技能当前生效版本的健康度。
func (r *Registry) Health(ctx context.Context) []domain.SkillHealth {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]domain.SkillHealth, 0, len(r.skills))
	for name := range r.skills {
		e := r.currentEntryLocked(name)
		if e == nil {
			continue
		}
		snap := e.circuit.Snapshot()
		avg := 0.0
		if e.totalCalls > 0 {
			avg = float64(e.latencySumMS) / float64(e.totalCalls)
		}
		out = append(out, domain.SkillHealth{
			Skill:        e.manifest.Name,
			Version:      e.manifest.Version,
			Status:       e.manifest.Status,
			Healthy:      snap.State != CircuitOpen,
			CircuitState: snap.State,
			Failures:     snap.Failures,
			Successes:    snap.Successes,
			TotalCalls:   e.totalCalls,
			AvgLatencyMS: avg,
			LastError:    snap.LastError,
			OpenUntil:    snap.OpenUntil,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Skill < out[j].Skill })
	return out
}

// Names 返回全部已注册技能名（含 disabled），按字典序排序。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.skills))
	for name := range r.skills {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SelectHandler 依据技能名与版本解析出可执行条目（供 runner 使用）。
//
// version 为空时使用当前生效版本；若命中灰度版本且抽样未通过，则回退到最高的
// enabled 版本；若不存在稳定版本则返回 ErrCanaryRejected。
func (r *Registry) SelectHandler(name, version string) (*entry, error) {
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()

	versions := r.skills[name]
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	ver := strings.TrimSpace(version)
	if ver == "" {
		ver = r.current[name]
		if ver == "" {
			if e := r.highestEnabledLocked(versions, ""); e != nil {
				return e, nil
			}
			return nil, fmt.Errorf("%w: %s 无可用版本", ErrSkillNotFound, name)
		}
	}
	e := versions[ver]
	if e == nil {
		return nil, fmt.Errorf("%w: %s@%s", ErrSkillNotFound, name, ver)
	}
	if e.handler == nil {
		return nil, fmt.Errorf("%w: %s@%s", ErrHandlerMissing, name, ver)
	}
	switch e.manifest.Status {
	case domain.SkillDisabled, domain.SkillDeprecated:
		return nil, fmt.Errorf("%w: %s@%s", ErrSkillDisabled, name, ver)
	case domain.SkillCanary:
		if !r.canaryHitLocked(name, e.manifest.CanaryPercent) {
			if stable := r.highestEnabledLocked(versions, ver); stable != nil {
				r.log.Debug("灰度未命中，回退稳定版本", "skill", name, "canary", ver, "stable", stable.manifest.Version)
				return stable, nil
			}
			return nil, fmt.Errorf("%w: %s@%s", ErrCanaryRejected, name, ver)
		}
	}
	return e, nil
}

// SetCanaryDecision 注入技能级灰度抽样结果（测试可注入，也便于运维强制生效/失效）。
func (r *Registry) SetCanaryDecision(name string, use bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canaryDecision[strings.TrimSpace(name)] = use
}

// resolveFallbacks 返回主技能配置的兜底技能链（去重、排除自身与不可用技能）。
func (r *Registry) resolveFallbacks(name string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e := r.currentEntryLocked(name)
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.manifest.FallbackSkills))
	seen := map[string]bool{name: true}
	for _, fb := range e.manifest.FallbackSkills {
		fb = strings.TrimSpace(fb)
		if fb == "" || seen[fb] {
			continue
		}
		versions := r.skills[fb]
		if len(versions) == 0 {
			continue
		}
		if r.highestEnabledLocked(versions, "") == nil && r.highestAvailableLocked(versions) == nil {
			continue
		}
		seen[fb] = true
		out = append(out, fb)
	}
	return out
}

// LoadManifests 从目录热加载外部技能清单（JSON 文件数组或单个对象）。
//
// 容错策略：单个文件解析或注册失败只记录错误，不影响内置技能与其他文件；
// 同名技能的清单覆盖会复用已注册的 handler，未知技能名则绑定占位 handler
// （调用时返回明确错误），避免清单声明被执行层误认为已实现。
func (r *Registry) LoadManifests(dir string) (loaded int, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return 0, nil
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取技能清单目录失败: %w", rerr)
	}
	var errs []error
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(strings.ToLower(de.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		raw, e := os.ReadFile(path)
		if e != nil {
			errs = append(errs, fmt.Errorf("%s: 读取失败: %w", de.Name(), e))
			continue
		}
		manifests, e := decodeManifests(raw)
		if e != nil {
			errs = append(errs, fmt.Errorf("%s: %w", de.Name(), e))
			continue
		}
		for _, m := range manifests {
			if err := r.registerExternal(m); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", de.Name(), err))
				continue
			}
			loaded++
		}
	}
	if len(errs) > 0 {
		return loaded, errors.Join(errs...)
	}
	return loaded, nil
}

// registerExternal 注册一条外部清单（复用已有 handler 或绑定占位 handler）。
func (r *Registry) registerExternal(m domain.SkillManifest) error {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return fmt.Errorf("%w: 清单缺少 name", ErrInvalidManifest)
	}
	r.mu.RLock()
	var handler domain.SkillHandler
	if e := r.currentEntryLocked(name); e != nil {
		handler = e.handler
	}
	r.mu.RUnlock()
	if handler == nil {
		if IsBuiltinSkill(name) {
			return fmt.Errorf("内置技能 %s 尚未注册实现，跳过清单覆盖", name)
		}
		if m.Builtin {
			return fmt.Errorf("%w: %s 不是已知内置技能名", ErrInvalidManifest, name)
		}
		handler = placeholderHandler(name)
	}
	return r.Register(context.Background(), m, handler)
}

// placeholderHandler 外部声明型技能的占位处理器：调用时返回明确错误。
func placeholderHandler(name string) domain.SkillHandler {
	return func(ctx context.Context, cc *domain.CallContext, in map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("外部技能 %s 仅在清单中声明，宿主进程未绑定处理器", name)
	}
}

// decodeManifests 解析清单文件内容，兼容数组、单对象与 {"skills":[...]} 三种形态。
func decodeManifests(raw []byte) ([]domain.SkillManifest, error) {
	var arr []domain.SkillManifest
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}
	var single domain.SkillManifest
	if err := json.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single.Name) != "" {
		return []domain.SkillManifest{single}, nil
	}
	var wrapper struct {
		Skills []domain.SkillManifest `json:"skills"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Skills) > 0 {
		return wrapper.Skills, nil
	}
	return nil, fmt.Errorf("%w: 无法解析技能清单 JSON", ErrInvalidManifest)
}

// applyOverride 套用配置中的技能级覆盖。
func (r *Registry) applyOverride(m *domain.SkillManifest) {
	ov := r.overrideFor(m.Name)
	if ov == nil {
		return
	}
	if strings.TrimSpace(ov.Version) != "" && strings.TrimSpace(ov.Version) != m.Version {
		// 覆盖配置指定了另一个版本时，仅补齐通用参数，不改写本清单的版本号。
		return
	}
	statusSet := false
	if s := strings.TrimSpace(ov.Status); s != "" {
		m.Status = domain.SkillStatus(s)
		statusSet = true
	}
	if ov.TimeoutSec > 0 {
		m.Timeout = ov.TimeoutSec
	}
	if ov.Retries > 0 {
		m.Retries = ov.Retries
	}
	if ov.FailureThreshold > 0 {
		m.FailureThreshold = ov.FailureThreshold
	}
	if ov.CooldownSec > 0 {
		m.CooldownSeconds = ov.CooldownSec
	}
	if len(ov.FallbackSkills) > 0 {
		m.FallbackSkills = append([]string(nil), ov.FallbackSkills...)
	}
	if ov.CanaryPercent > 0 {
		m.CanaryPercent = ov.CanaryPercent
		// 声明灰度百分比但未显式指定状态时，语义上即为灰度发布。
		if !statusSet && m.Status != domain.SkillDisabled && m.Status != domain.SkillDeprecated {
			m.Status = domain.SkillCanary
		}
	}
}

// overrideFor 返回技能名对应的覆盖配置。
func (r *Registry) overrideFor(name string) *config.SkillOverride {
	for i := range r.cfg.Overrides {
		if strings.TrimSpace(r.cfg.Overrides[i].Name) == name {
			return &r.cfg.Overrides[i]
		}
	}
	return nil
}

// applyDefaults 用配置默认值与安全下限补齐清单字段。
func (r *Registry) applyDefaults(m *domain.SkillManifest, statusProvided bool) {
	if m.Timeout <= 0 {
		m.Timeout = r.cfg.DefaultTimeoutSec
	}
	if m.Timeout <= 0 {
		m.Timeout = 60
	}
	if m.Retries < 0 {
		m.Retries = 0
	}
	if m.Retries == 0 && r.cfg.DefaultRetries > 0 {
		m.Retries = r.cfg.DefaultRetries
	}
	if m.FailureThreshold <= 0 {
		m.FailureThreshold = r.cfg.FailureThreshold
	}
	if m.FailureThreshold <= 0 {
		m.FailureThreshold = defaultFailureThreshold
	}
	if m.CooldownSeconds <= 0 {
		m.CooldownSeconds = r.cfg.CooldownSec
	}
	if m.CooldownSeconds <= 0 {
		m.CooldownSeconds = int(defaultCooldown / time.Second)
	}
	if m.Status == "" {
		if m.CanaryPercent > 0 && !statusProvided {
			m.Status = domain.SkillCanary
		} else {
			m.Status = domain.SkillEnabled
		}
	}
	// 兜底链去重并剔除自身。
	if len(m.FallbackSkills) > 0 {
		seen := map[string]bool{m.Name: true}
		cleaned := make([]string, 0, len(m.FallbackSkills))
		for _, fb := range m.FallbackSkills {
			fb = strings.TrimSpace(fb)
			if fb == "" || seen[fb] {
				continue
			}
			seen[fb] = true
			cleaned = append(cleaned, fb)
		}
		m.FallbackSkills = cleaned
	}
}

// validateSkillStatus 校验技能状态合法性。
func validateSkillStatus(status domain.SkillStatus) error {
	switch status {
	case domain.SkillEnabled, domain.SkillDisabled, domain.SkillCanary, domain.SkillDeprecated:
		return nil
	default:
		return fmt.Errorf("%w: 非法技能状态 %q", ErrInvalidManifest, string(status))
	}
}

// currentEntryLocked 返回技能当前生效版本的条目（调用方需持有读锁）。
func (r *Registry) currentEntryLocked(name string) *entry {
	versions := r.skills[name]
	if len(versions) == 0 {
		return nil
	}
	if ver := r.current[name]; ver != "" {
		if e := versions[ver]; e != nil {
			return e
		}
	}
	return r.highestAvailableLocked(versions)
}

// highestEnabledLocked 返回状态为 enabled 的最高版本条目（exclude 用于排除指定版本）。
func (r *Registry) highestEnabledLocked(versions map[string]*entry, exclude string) *entry {
	for _, ver := range sortVersionsDesc(versions) {
		if ver == exclude {
			continue
		}
		if e := versions[ver]; e != nil && e.manifest.Status == domain.SkillEnabled {
			return e
		}
	}
	return nil
}

// highestAvailableLocked 返回任意非 disabled 的最高版本条目。
func (r *Registry) highestAvailableLocked(versions map[string]*entry) *entry {
	for _, ver := range sortVersionsDesc(versions) {
		if e := versions[ver]; e != nil && e.manifest.Status != domain.SkillDisabled {
			return e
		}
	}
	return nil
}

// canaryHitLocked 依据灰度百分比做稳定抽样（调用方需持有写锁）。
func (r *Registry) canaryHitLocked(name string, percent int) bool {
	if percent >= 100 {
		return true
	}
	if percent <= 0 {
		return false
	}
	if v, ok := r.canaryDecision[name]; ok {
		return v
	}
	hit := r.rnd.Intn(100) < percent
	r.canaryDecision[name] = hit
	return hit
}

// recordCall 记录一次调用统计（供 runner 调用）。
func (r *Registry) recordCall(name, version string, latencyMS int64, failed bool, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.skills[name][version]
	if e == nil {
		return
	}
	e.totalCalls++
	e.latencySumMS += latencyMS
	if failed {
		e.failures++
		if lastErr != "" {
			e.lastError = lastErr
			e.circuit.RecordError(lastErr)
		}
	} else {
		e.successes++
	}
}

// sortVersionsDesc 按语义化版本降序返回版本号。
func sortVersionsDesc(versions map[string]*entry) []string {
	out := make([]string, 0, len(versions))
	for ver := range versions {
		out = append(out, ver)
	}
	sort.SliceStable(out, func(i, j int) bool { return compareVersions(out[i], out[j]) > 0 })
	return out
}

// compareVersions 比较语义化版本：a>b 返回 1，a<b 返回 -1，相等返回 0。
func compareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

// splitVersion 抽取版本号中的数字段，忽略预发布后缀。
func splitVersion(v string) []int {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}
