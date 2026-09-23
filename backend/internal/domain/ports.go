package domain

import (
	"context"
	"time"

	"github.com/codeagent/backend/internal/platform/logx"
)

// ---------------------------------------------------------------------------
// 调用上下文：贯穿七层的最小上下文载体
// ---------------------------------------------------------------------------

// CallContext 是一次技能/模型/源码调用的统一上下文。
// 它承载租户、任务、运行、仓库版本等隔离信息，保证并发任务互不污染。
type CallContext struct {
	TenantID  string
	TaskID    string
	RunID     string
	Stage     string
	RequestID string
	// PinnedCommits 仓库 → Commit 的任务级版本锁定快照。
	PinnedCommits map[string]string
	// Logger 带 trace 字段的结构化日志器。
	Logger *logx.Logger
	// Sandbox 沙箱句柄（仅沙箱阶段可用，其他阶段为 nil）。
	Sandbox SandboxHandle
}

// Child 派生一个子阶段上下文。
func (c *CallContext) Child(stage string) *CallContext {
	cc := *c
	cc.Stage = stage
	if c.Logger != nil {
		cc.Logger = c.Logger.With("stage", stage)
	}
	return &cc
}

// LoggerOr 返回日志器，nil 时返回 no-op 日志器。
func (c *CallContext) LoggerOr() *logx.Logger {
	if c == nil || c.Logger == nil {
		return logx.Nop()
	}
	return c.Logger
}

// CommitFor 返回指定仓库锁定的 commit。
func (c *CallContext) CommitFor(repoID string) string {
	if c == nil || c.PinnedCommits == nil {
		return ""
	}
	return c.PinnedCommits[repoID]
}

// ---------------------------------------------------------------------------
// 租户权限层（第二层）
// ---------------------------------------------------------------------------

// Subject 已认证的调用主体。
type Subject struct {
	TenantID   string   `json:"tenantId"`
	TenantName string   `json:"tenantName,omitempty"`
	APIKeyID   string   `json:"apiKeyId,omitempty"`
	KeyPrefix  string   `json:"keyPrefix,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	// Admin 是否控制台管理主体（JWT 登录）。
	Admin     bool   `json:"admin"`
	Username  string `json:"username,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Has 判断主体是否具备某个 scope。admin:all 通配。
func (s *Subject) Has(scope string) bool {
	if s == nil {
		return false
	}
	for _, sc := range s.Scopes {
		if sc == "admin:all" || sc == scope {
			return true
		}
	}
	return false
}

// AuthResult 鉴权结果。
type AuthResult struct {
	Subject *Subject
	Err     error
}

// Authorizer 认证 + 授权。
type Authorizer interface {
	// Authenticate 校验 API Key（接入层调用）。
	Authenticate(ctx context.Context, apiKey string) (*Subject, error)
	// Login 控制台登录，返回 JWT 与主体。
	Login(ctx context.Context, tenantKey, username, password string) (token string, sub *Subject, err error)
	// VerifyToken 校验控制台 JWT。
	VerifyToken(ctx context.Context, token string) (*Subject, error)
	// CanAccessRepo 校验主体是否有权访问某仓库（访问鉴权，防越权读取私有代码）。
	CanAccessRepo(ctx context.Context, sub *Subject, repoID string) error
	// CanAccessGroup 校验主体是否有权访问某分组。
	CanAccessGroup(ctx context.Context, sub *Subject, groupID string) error
}

// CredentialBox 凭证加密箱：Git 密钥加密存取，无明文落库。
type CredentialBox interface {
	// Seal 加密明文密钥。
	Seal(plain string) (string, error)
	// Open 解密。
	Open(sealed string) (string, error)
	// Mask 对任意文本做脱敏（清除 AK/SK、Token、密码、私钥）。
	Mask(text string) string
	// MaskCode 对代码内容做脱敏，返回脱敏结果与是否发生脱敏。
	MaskCode(code string) (string, bool)
}

// ---------------------------------------------------------------------------
// 仓库分组索引层（第三层）
// ---------------------------------------------------------------------------

// RepoIndex 仓库与分组的索引查询能力。
type RepoIndex interface {
	ListRepos(ctx context.Context, tenantID string, q PageQuery) (Page[Repository], error)
	GetRepo(ctx context.Context, tenantID, repoID string) (*Repository, error)
	GetRepoByKey(ctx context.Context, tenantID, key string) (*Repository, error)
	CreateRepo(ctx context.Context, repo *Repository) error
	UpdateRepo(ctx context.Context, repo *Repository) error
	DeleteRepo(ctx context.Context, tenantID, repoID string) error

	ListGroups(ctx context.Context, tenantID string, q PageQuery) (Page[RepositoryGroup], error)
	GetGroup(ctx context.Context, tenantID, groupID string) (*RepositoryGroup, error)
	CreateGroup(ctx context.Context, g *RepositoryGroup, members []GroupMember) error
	UpdateGroup(ctx context.Context, g *RepositoryGroup, members []GroupMember) error
	DeleteGroup(ctx context.Context, tenantID, groupID string) error
	// GroupMembers 返回分组成员（含仓库详情，按 Order 升序）。
	GroupMembers(ctx context.Context, tenantID, groupID string) ([]GroupMemberView, error)
	// GroupsOfRepo 反查仓库所属分组。
	GroupsOfRepo(ctx context.Context, tenantID, repoID string) ([]RepositoryGroup, error)
}

// GroupMemberView 分组成员的聚合视图。
type GroupMemberView struct {
	GroupMember
	Repo *Repository `json:"repo,omitempty"`
}

// ---------------------------------------------------------------------------
// 源码 & 沙箱执行层（第七层）
// ---------------------------------------------------------------------------

// RepoRef 定位一个仓库的某个版本。
type RepoRef struct {
	Repository *Repository
	Commit     string // 为空表示使用默认分支 HEAD
	Ref        string // 原始请求的 ref
	Budget     LoadBudget
}

// LoadBudget 加载预算：防止大仓库、大文件引发资源耗尽。
type LoadBudget struct {
	MaxFiles    int
	MaxChars    int
	MaxFileSize int // 单文件最大字节数
	MaxDepth    int
}

// FileNode 文件树节点。
type FileNode struct {
	Path     string `json:"path"`
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	Language string `json:"language,omitempty"`
}

// SourceResolver 精准版本源码解析与文件级缓存。
type SourceResolver interface {
	// Resolve 解析仓库版本，返回锁定的 commit（任务级代码版本锁定）。
	Resolve(ctx context.Context, ref RepoRef) (commit string, err error)
	// Load 按线索懒加载故障关联的少量文件。
	Load(ctx context.Context, ref RepoRef, hints []FileHint) ([]CodeSlice, error)
	// ReadFile 读取单文件（仓库+Commit+文件 维度缓存）。
	ReadFile(ctx context.Context, ref RepoRef, path string) (*CodeSlice, error)
	// Tree 列出文件树（带预算与忽略规则）。
	Tree(ctx context.Context, ref RepoRef) ([]FileNode, error)
	// Grep 在指定版本的代码中做符号搜索（堆栈驱动定位）。
	Grep(ctx context.Context, ref RepoRef, pattern string, limit int) ([]FileHint, error)
	// Invalidate 清理指定仓库缓存。
	Invalidate(ctx context.Context, tenantID, repoID string) error
	// Stats 返回缓存统计。
	Stats() CacheStats
}

// CacheStats 文件级缓存统计。
type CacheStats struct {
	Entries   int     `json:"entries"`
	Hits      int64   `json:"hits"`
	Misses    int64   `json:"misses"`
	Evictions int64   `json:"evictions"`
	Bytes     int64   `json:"bytes"`
	HitRate   float64 `json:"hitRate"`
	Capacity  int     `json:"capacity"`
}

// SandboxHandle 一个隔离的代码工作区句柄。
type SandboxHandle interface {
	// ID 工作区标识。
	ID() string
	// Root 工作区根目录。
	Root() string
	// RepoDir 返回某仓库在工作区中的目录。
	RepoDir(repoKey string) string
	// Close 释放工作区（清理临时目录）。
	Close() error
}

// SandboxManager 源码沙箱执行层入口：工作区隔离 + 补丁验证。
type SandboxManager interface {
	// Open 为一次任务创建隔离工作区，并物化所有锁定版本的仓库快照。
	Open(ctx context.Context, meta SandboxMeta) (SandboxHandle, error)
	// Validate 在隔离工作区中应用补丁并执行编译/静态校验/测试。
	Validate(ctx context.Context, handle SandboxHandle, req ValidateRequest) (*Verification, error)
}

// SandboxMeta 工作区元信息。
type SandboxMeta struct {
	RunID     string
	TenantID  string
	TaskID    string
	Pinned    map[string]string // repoID -> commit
	Repos     []*Repository
	Timeout   time.Duration
	AllowExec bool // 是否允许执行命令（编译/测试），流水线形态可关闭
}

// ValidateRequest 补丁验证请求。
type ValidateRequest struct {
	Patches []Patch
	// Checks 期望执行的校验项；为空时按语言自动推断。
	Checks  []string
	Timeout time.Duration
	// DryRun 只做 apply 校验，不执行命令。
	DryRun bool
}

// ---------------------------------------------------------------------------
// Skill 可插拔插件层（第五层）
// ---------------------------------------------------------------------------

// SkillManifest 技能清单：声明式定义，支持参数强校验与版本约束。
type SkillManifest struct {
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Category    SkillCategory `json:"category"`
	Status      SkillStatus   `json:"status"`
	// InputSchema JSON Schema，入参强校验，非法参数直接拦截。
	InputSchema map[string]any `json:"inputSchema"`
	// OutputSchema 输出结构约束。
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	// Timeout 单次执行超时（秒）。
	Timeout int `json:"timeout"`
	// Retries 失败重试次数。
	Retries int `json:"retries"`
	// MaxConcurrency 并发上限。
	MaxConcurrency int `json:"maxConcurrency"`
	// FailureThreshold 熔断阈值：连续失败次数。
	FailureThreshold int `json:"failureThreshold"`
	// CooldownSeconds 熔断冷却时间。
	CooldownSeconds int `json:"cooldownSeconds"`
	// FallbackSkills 备用技能链，主技能熔断/失败时兜底。
	FallbackSkills []string `json:"fallbackSkills,omitempty"`
	// CompatibleWith 兼容的 Harness/系统版本约束，例如 ">=1.0.0 <2.0.0"。
	CompatibleWith string `json:"compatibleWith,omitempty"`
	// Builtin 是否内置技能。
	Builtin bool `json:"builtin"`
	// Priority 排序权重。
	Priority int `json:"priority"`
	// CanaryPercent 灰度百分比（0-100），用于灰度更新。
	CanaryPercent int `json:"canaryPercent,omitempty"`
	// Tags 标签。
	Tags []string `json:"tags,omitempty"`
}

// SkillInvocation 技能调用入参。
type SkillInvocation struct {
	Skill   string
	Version string // 为空使用最新启用版本
	Input   map[string]any
}

// SkillResult 技能调用出参。
type SkillResult struct {
	Skill        string         `json:"skill"`
	Version      string         `json:"version"`
	Status       CallStatus     `json:"status"`
	Output       map[string]any `json:"output,omitempty"`
	Error        string         `json:"error,omitempty"`
	Attempts     int            `json:"attempts"`
	DurationMS   int64          `json:"durationMs"`
	FromFallback bool           `json:"fromFallback,omitempty"`
	FallbackFrom string         `json:"fallbackFrom,omitempty"`
	// Blocked 是否被熔断/配额拦截。
	Blocked bool `json:"blocked,omitempty"`
}

// SkillHealth 技能健康度。
type SkillHealth struct {
	Skill        string      `json:"skill"`
	Version      string      `json:"version"`
	Status       SkillStatus `json:"status"`
	Healthy      bool        `json:"healthy"`
	CircuitState string      `json:"circuitState"` // closed | open | half_open
	Failures     int         `json:"failures"`
	Successes    int         `json:"successes"`
	TotalCalls   int64       `json:"totalCalls"`
	AvgLatencyMS float64     `json:"avgLatencyMs"`
	LastError    string      `json:"lastError,omitempty"`
	OpenUntil    time.Time   `json:"openUntil,omitempty"`
}

// SkillRegistry 技能注册中心：热更新、版本管理、灰度。
type SkillRegistry interface {
	List(ctx context.Context, category string) []SkillManifest
	Get(ctx context.Context, name string) (*SkillManifest, error)
	// Register 注册/热更新技能（同 Name+Version 覆盖）。
	Register(ctx context.Context, m SkillManifest, handler SkillHandler) error
	// SetStatus 启停/灰度技能。
	SetStatus(ctx context.Context, name, version string, status SkillStatus) error
	// Rollback 回滚到上一个可用版本。
	Rollback(ctx context.Context, name string) (string, error)
	Health(ctx context.Context) []SkillHealth
	// Names 返回已注册技能名。
	Names() []string
}

// SkillHandler 技能实现。技能在沙箱隔离环境中运行，仅通过入参出参交互。
type SkillHandler func(ctx context.Context, cc *CallContext, in map[string]any) (map[string]any, error)

// SkillRunner 技能执行器：入参校验 + 沙箱隔离 + 重试 + 熔断 + 兜底 + 审计。
type SkillRunner interface {
	Run(ctx context.Context, cc *CallContext, inv SkillInvocation) (*SkillResult, error)
}

// ---------------------------------------------------------------------------
// MCP 模型管控层（第六层）
// ---------------------------------------------------------------------------

// ModelTarget 模型目标（由路由策略决定）。
type ModelTarget struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Tier     ModelTier `json:"tier"`
	// Endpoint 覆盖默认地址。
	Endpoint    string  `json:"endpoint,omitempty"`
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// ModelMessage 标准化后的模型消息。
type ModelMessage struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// ModelRequest MCP 标准化推理请求。
type ModelRequest struct {
	Stage    string         `json:"stage"`
	Tier     ModelTier      `json:"tier"`
	Messages []ModelMessage `json:"messages"`
	// Schema 强约束结构化输出（JSON Schema）；为空则要求纯文本。
	Schema map[string]any `json:"schema,omitempty"`
	// MaxTokens / Temperature 覆盖默认采样参数。
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	// PinnedModel 手动指定模型（跳过智能调度）。
	PinnedModel *ModelTarget `json:"pinnedModel,omitempty"`
}

// ModelUsage Token 消耗。
type ModelUsage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// ModelResponse MCP 标准化推理结果。
type ModelResponse struct {
	// Structured 通过 Schema 校验的结构化结果（JSON 反序列化后）。
	Structured map[string]any `json:"structured,omitempty"`
	// Text 原始文本（Schema 为空或结构化失败时）。
	Text       string      `json:"text,omitempty"`
	Target     ModelTarget `json:"target"`
	Usage      ModelUsage  `json:"usage"`
	Attempts   int         `json:"attempts"`
	DurationMS int64       `json:"durationMs"`
	// FallbackFrom 发生兜底时的原模型。
	FallbackFrom string `json:"fallbackFrom,omitempty"`
	// Repaired 是否经过输出修复。
	Repaired bool `json:"repaired,omitempty"`
	// Truncated 上下文是否被裁剪。
	Truncated bool `json:"truncated,omitempty"`
}

// ContextBudget 上下文预算。
type ContextBudget struct {
	MaxChars         int
	MaxSlices        int
	MaxSliceChars    int
	MaxLogChars      int
	ReserveForOutput int
}

// ContextPacker 上下文智能治理：裁剪无效内容、超长代码分片、规避 Token 超限。
type ContextPacker interface {
	// Pack 依据预算组装上下文文本。
	Pack(bundle *EvidenceBundle, budget ContextBudget) (ContextPacked, error)
}

// ContextPacked 组装后的上下文。
type ContextPacked struct {
	Text      string           `json:"text"`
	Chars     int              `json:"chars"`
	Sections  []ContextSection `json:"sections"`
	Truncated bool             `json:"truncated"`
	Notes     []string         `json:"notes"`
}

// ContextSection 上下文片段。
type ContextSection struct {
	Name    string `json:"name"`
	Chars   int    `json:"chars"`
	Dropped bool   `json:"dropped,omitempty"`
}

// MCPGateway 模型管控层入口：输入标准化 + 输出强约束 + 智能调度 + 容错熔断。
type MCPGateway interface {
	// Chat 执行一次受管控的模型推理。
	Chat(ctx context.Context, cc *CallContext, req ModelRequest) (*ModelResponse, error)
	// Providers 返回已配置的模型提供方。
	Providers() []ModelTarget
	// Health 返回各模型健康度。
	Health(ctx context.Context) []ModelHealth
	// Stats 返回调用统计。
	Stats(ctx context.Context) ModelStats
}

// ModelHealth 模型健康度。
type ModelHealth struct {
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	Tier         ModelTier `json:"tier"`
	Healthy      bool      `json:"healthy"`
	CircuitState string    `json:"circuitState"`
	Failures     int       `json:"failures"`
	AvgLatencyMS float64   `json:"avgLatencyMs"`
	TotalCalls   int64     `json:"totalCalls"`
	LastError    string    `json:"lastError,omitempty"`
	OpenUntil    time.Time `json:"openUntil,omitempty"`
}

// ModelStats 模型层统计。
type ModelStats struct {
	TotalCalls       int64            `json:"totalCalls"`
	FailedCalls      int64            `json:"failedCalls"`
	FallbackCalls    int64            `json:"fallbackCalls"`
	RepairedOutputs  int64            `json:"repairedOutputs"`
	PromptTokens     int64            `json:"promptTokens"`
	CompletionTokens int64            `json:"completionTokens"`
	TotalTokens      int64            `json:"totalTokens"`
	AvgLatencyMS     float64          `json:"avgLatencyMs"`
	ByModel          map[string]int64 `json:"byModel"`
	ByTier           map[string]int64 `json:"byTier"`
}

// ---------------------------------------------------------------------------
// Agent 核心调度层（第四层）
// ---------------------------------------------------------------------------

// PipelineResult 一次流水线执行的产物。
type PipelineResult struct {
	Run      *TaskRun
	Report   *Report
	Evidence *EvidenceBundle
	State    TaskState
	Degraded bool
	Warnings []string
}

// TaskPipeline 任务流水线：证据准备 → 分析 → 修复 → 验证 → 报告。
type TaskPipeline interface {
	Execute(ctx context.Context, cc *CallContext, run *TaskRun) (*PipelineResult, error)
}

// TaskEngine Agent 核心调度层入口。
type TaskEngine interface {
	// Submit 受理任务：权限校验 → 幂等去重 → 配额管控 → 入队。
	Submit(ctx context.Context, sub *Subject, req CreateTaskRequest) (*TaskRun, error)
	// GetRun 查询执行详情。
	GetRun(ctx context.Context, tenantID, runID string) (*TaskRun, error)
	// ListTasks / ListRuns 列表查询。
	ListTasks(ctx context.Context, tenantID string, q PageQuery) (Page[Task], error)
	ListRuns(ctx context.Context, tenantID string, q PageQuery) (Page[TaskRun], error)
	// Cancel 取消任务。
	Cancel(ctx context.Context, tenantID, runID string) error
	// Retry 基于已固化上下文重跑（多轮修复收敛）。
	Retry(ctx context.Context, tenantID, runID string) (*TaskRun, error)
	// RerunWithFeedback 携带人工反馈重跑，用于修复不收敛时的定向迭代。
	RerunWithFeedback(ctx context.Context, tenantID, runID string, feedback string) (*TaskRun, error)
	// Stats 运行统计。
	Stats(ctx context.Context, tenantID string) (*EngineStats, error)
	// Start / Stop 生命周期。
	Start(ctx context.Context) error
	Stop() error
}

// EngineStats 调度层统计。
type EngineStats struct {
	Running      int               `json:"running"`
	Queued       int               `json:"queued"`
	Total        int               `json:"total"`
	ByState      map[TaskState]int `json:"byState"`
	BySeverity   map[Severity]int  `json:"bySeverity"`
	SuccessRate  float64           `json:"successRate"`
	AvgElapsedMS int64             `json:"avgElapsedMs"`
	RepoSwitches int64             `json:"repoSwitches"`
	DegradedRuns int64             `json:"degradedRuns"`
}

// ---------------------------------------------------------------------------
// 可观测（贯穿全层）
// ---------------------------------------------------------------------------

// EventBus 实时事件总线：任务生命周期、仓库切换、技能调用、模型推理、源码拉取、修复变更。
type EventBus interface {
	Publish(ev Event)
	// Subscribe 订阅某租户事件（tenantID 为空表示全部租户）。
	Subscribe(tenantID string) (<-chan Event, func())
}

// Recorder 审计 / 埋点记录器。
type Recorder interface {
	Audit(ev AuditEvent)
	SkillCall(call SkillCall)
	ModelCall(call ModelCall)
	ListAudits(ctx context.Context, tenantID string, q PageQuery) (Page[AuditEvent], error)
	ListSkillCalls(ctx context.Context, tenantID, runID string, q PageQuery) (Page[SkillCall], error)
	ListModelCalls(ctx context.Context, tenantID, runID string, q PageQuery) (Page[ModelCall], error)
	Observability(ctx context.Context, tenantID string) (*ObservabilitySummary, error)
}

// ObservabilitySummary 可观测汇总：支撑修复率分析、复盘、持续迭代。
type ObservabilitySummary struct {
	TasksTotal       int            `json:"tasksTotal"`
	RunsTotal        int            `json:"runsTotal"`
	RunsSucceeded    int            `json:"runsSucceeded"`
	RunsNeedsReview  int            `json:"runsNeedsReview"`
	RunsFailed       int            `json:"runsFailed"`
	RunsDegraded     int            `json:"runsDegraded"`
	FixRate          float64        `json:"fixRate"`
	AvgElapsedMS     int64          `json:"avgElapsedMs"`
	SkillCalls       int            `json:"skillCalls"`
	SkillFailures    int            `json:"skillFailures"`
	SkillFailureRate float64        `json:"skillFailureRate"`
	ModelCalls       int            `json:"modelCalls"`
	ModelTokens      int            `json:"modelTokens"`
	ModelFallbacks   int            `json:"modelFallbacks"`
	CacheHits        int            `json:"cacheHits"`
	RepoSwitches     int            `json:"repoSwitches"`
	TopSkills        []SkillMetric  `json:"topSkills"`
	TopModels        []ModelMetric  `json:"topModels"`
	StateDist        map[string]int `json:"stateDist"`
	CategoryDist     map[string]int `json:"categoryDist"`
	RecentFailures   []RunBrief     `json:"recentFailures"`
}

// SkillMetric 技能指标。
type SkillMetric struct {
	Skill        string  `json:"skill"`
	Calls        int     `json:"calls"`
	Failures     int     `json:"failures"`
	AvgLatencyMS float64 `json:"avgLatencyMs"`
}

// ModelMetric 模型指标。
type ModelMetric struct {
	Model        string  `json:"model"`
	Calls        int     `json:"calls"`
	AvgLatencyMS float64 `json:"avgLatencyMs"`
	TotalTokens  int     `json:"totalTokens"`
}

// RunBrief 运行摘要。
type RunBrief struct {
	RunID    string    `json:"runId"`
	Title    string    `json:"title"`
	State    TaskState `json:"state"`
	Severity Severity  `json:"severity"`
	Summary  string    `json:"summary,omitempty"`
	At       time.Time `json:"at"`
}
