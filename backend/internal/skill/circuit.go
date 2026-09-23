package skill

import (
	"sync"
	"time"
)

// 熔断器状态常量。
const (
	// CircuitClosed 闭合：正常放行。
	CircuitClosed = "closed"
	// CircuitOpen 断开：拒绝放行，直到冷却结束。
	CircuitOpen = "open"
	// CircuitHalfOpen 半开：仅放行少量探测请求。
	CircuitHalfOpen = "half_open"
)

// 熔断器默认参数（构造参数非法或使用零值时生效）。
const (
	defaultFailureThreshold = 3
	defaultCooldown         = 30 * time.Second
	defaultHalfOpenProbes   = 1
)

// CircuitSnapshot 熔断器状态快照。
type CircuitSnapshot struct {
	// State 当前状态：closed | open | half_open。
	State string `json:"state"`
	// Failures 连续失败次数。
	Failures int `json:"failures"`
	// Successes 累计成功次数。
	Successes int `json:"successes"`
	// TotalCalls 累计放行次数。
	TotalCalls int64 `json:"totalCalls"`
	// OpenUntil 熔断断开截止时间（closed 状态为零值）。
	OpenUntil time.Time `json:"openUntil,omitempty"`
	// LastError 最近一次失败原因。
	LastError string `json:"lastError,omitempty"`
	// Probes 半开状态下已放行的探测数。
	Probes int `json:"probes"`
}

// Circuit 三态熔断器（closed/open/half_open）。
//
// 语义：
//   - closed：全部放行；连续失败达到阈值后转入 open；
//   - open：拒绝放行，直到冷却时间结束自动转入 half_open；
//   - half_open：只放行 halfOpenProbes 个探测；探测成功立即 closed 并清零失败计数，
//     探测失败立即重新 open 并重置冷却。
//
// 所有方法并发安全；零值可用（使用默认阈值 3 / 冷却 30s / 探测 1）。
type Circuit struct {
	mu sync.Mutex

	failureThreshold int
	cooldown         time.Duration
	halfOpenProbes   int

	state      string
	failures   int
	successes  int
	totalCalls int64
	probes     int
	openUntil  time.Time
	lastError  string
}

// NewCircuit 创建熔断器；参数非法（<=0）时使用默认值 3 / 30s / 1。
func NewCircuit(failureThreshold int, cooldown time.Duration, halfOpenProbes int) *Circuit {
	c := &Circuit{}
	c.failureThreshold = failureThreshold
	c.cooldown = cooldown
	c.halfOpenProbes = halfOpenProbes
	c.state = CircuitClosed
	c.mu.Lock()
	c.ensureDefaultsLocked()
	c.mu.Unlock()
	return c
}

// ensureDefaultsLocked 补齐非法参数（调用方需持有锁）。
func (c *Circuit) ensureDefaultsLocked() {
	if c.failureThreshold <= 0 {
		c.failureThreshold = defaultFailureThreshold
	}
	if c.cooldown <= 0 {
		c.cooldown = defaultCooldown
	}
	if c.halfOpenProbes <= 0 {
		c.halfOpenProbes = defaultHalfOpenProbes
	}
	if c.state == "" {
		c.state = CircuitClosed
	}
}

// refreshLocked 依据冷却时间推进状态机（调用方需持有锁）。
func (c *Circuit) refreshLocked() {
	c.ensureDefaultsLocked()
	if c.state == CircuitOpen && !c.openUntil.IsZero() && !time.Now().Before(c.openUntil) {
		c.state = CircuitHalfOpen
		c.probes = 0
	}
}

// Allow 判断本次调用是否放行。
func (c *Circuit) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	switch c.state {
	case CircuitOpen:
		return false
	case CircuitHalfOpen:
		if c.probes >= c.halfOpenProbes {
			return false
		}
		c.probes++
		c.totalCalls++
		return true
	default:
		c.totalCalls++
		return true
	}
}

// Success 记录一次成功；half_open 探测成功则闭合并清零失败计数。
func (c *Circuit) Success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureDefaultsLocked()
	c.successes++
	c.failures = 0
	c.state = CircuitClosed
	c.probes = 0
	c.openUntil = time.Time{}
}

// Failure 记录一次失败；连续失败达到阈值则断开，half_open 失败立即重新断开。
func (c *Circuit) Failure() { c.FailureWith(nil) }

// FailureWith 记录一次失败并保留失败原因（用于健康度展示）。
func (c *Circuit) FailureWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureDefaultsLocked()
	c.failures++
	if err != nil {
		c.lastError = err.Error()
	}
	if c.state == CircuitHalfOpen || c.failures >= c.failureThreshold {
		c.state = CircuitOpen
		c.openUntil = time.Now().Add(c.cooldown)
		c.probes = 0
	}
}

// RecordError 仅记录失败原因（不计入失败次数），用于外部统计回填健康度。
func (c *Circuit) RecordError(msg string) {
	if msg == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = msg
}

// State 返回当前状态：closed | open | half_open。
func (c *Circuit) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	return c.state
}

// Snapshot 返回熔断器状态快照。
func (c *Circuit) Snapshot() CircuitSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	return CircuitSnapshot{
		State:      c.state,
		Failures:   c.failures,
		Successes:  c.successes,
		TotalCalls: c.totalCalls,
		OpenUntil:  c.openUntil,
		LastError:  c.lastError,
		Probes:     c.probes,
	}
}

// Reset 重置为闭合状态并清零计数（用于人工恢复）。
func (c *Circuit) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureDefaultsLocked()
	c.state = CircuitClosed
	c.failures = 0
	c.probes = 0
	c.openUntil = time.Time{}
}
