package domain

import "time"

// ---------------------------------------------------------------------------
// 租户与安全权限层（第二层）
// ---------------------------------------------------------------------------

// Tenant 租户，全系统资源隔离的根。所有仓库、分组、任务均归属租户。
type Tenant struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      TenantStatus `json:"status"`
	Description string       `json:"description,omitempty"`
	// Quota 资源配额，防止单租户耗尽集群资源。
	Quota     TenantQuota `json:"quota"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// TenantQuota 租户级资源配额。
type TenantQuota struct {
	MaxConcurrentTasks int `json:"maxConcurrentTasks"` // 并发任务上限
	MaxFilesPerTask    int `json:"maxFilesPerTask"`    // 单任务最大解析文件数
	MaxCodeChars       int `json:"maxCodeChars"`       // 单任务送入模型的代码字符上限
	MaxTokensPerTask   int `json:"maxTokensPerTask"`   // 单任务模型 Token 上限
	MaxTaskSeconds     int `json:"maxTaskSeconds"`     // 单任务执行超时（秒）
}

// DefaultQuota 返回企业默认配额。
func DefaultQuota() TenantQuota {
	return TenantQuota{
		MaxConcurrentTasks: 8,
		MaxFilesPerTask:    200,
		MaxCodeChars:       400_000,
		MaxTokensPerTask:   400_000,
		MaxTaskSeconds:     900,
	}
}

// APIKey 租户接入密钥。KeyHash 只存哈希，明文仅在创建时返回一次。
type APIKey struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenantId"`
	Name       string    `json:"name"`
	KeyHash    string    `json:"keyHash"`
	KeyPrefix  string    `json:"keyPrefix"` // 例如 ca_live_ab12，用于日志中识别
	Scopes     []string  `json:"scopes"`    // task:write repo:read admin:all ...
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool      `json:"revoked"`
	CreatedAt  time.Time `json:"createdAt"`

	// 回调鉴权（方案2：密钥与接入密钥/服务绑定）。启用后，该密钥触发的任务终态回调
	// 将以 HMAC-SHA256 签名置于请求头，供对端验签，证明回调确实来自本服务。
	CallbackEnabled   bool     `json:"callbackEnabled,omitempty"` // 是否启用回调签名
	CallbackSecretEnc string   `json:"callbackSecretEnc,omitempty"` // 回调密钥密文（AES-GCM，仅持久化；API 响应经 apiKeyView 脱敏不回显）
	CallbackMode      string   `json:"callbackMode,omitempty"`    // "hmac"（当前唯一支持）
	CallbackHosts     []string `json:"callbackHosts,omitempty"`   // 允许回调的 host 白名单（空=仅做 SSRF 防护）
}

// Credential Git 访问凭证。Secret 必须以密文形式持久化，日志与 API 一律脱敏。
type Credential struct {
	ID        string         `json:"id"`
	TenantID  string         `json:"tenantId"`
	Name      string         `json:"name"`
	Type      CredentialType `json:"type"`
	Username  string         `json:"username,omitempty"`
	SecretEnc string         `json:"secretEnc,omitempty"` // AES-GCM 密文（base64）
	HasSecret bool           `json:"hasSecret"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// 仓库分组索引层（第三层）
// ---------------------------------------------------------------------------

// Repository Git 仓库元数据。第三层只维护索引，不存储源码。
type Repository struct {
	ID            string      `json:"id"`
	TenantID      string      `json:"tenantId"`
	Name          string      `json:"name"`
	Key           string      `json:"key"` // 业务唯一键，用于堆栈/接口名匹配，如 order-service
	URL           string      `json:"url"`
	DefaultBranch string      `json:"defaultBranch"`
	Provider      VCSProvider `json:"provider"`
	Layer         RepoLayer   `json:"layer"`
	Language      string      `json:"language"`
	CredentialID  string      `json:"credentialId,omitempty"`
	Status        RepoStatus  `json:"status"`
	// MatchRules 匹配规则：堆栈中命中任一规则即认为该仓库关联故障。
	MatchRules RepoMatchRules `json:"matchRules"`
	// AccessPolicy 允许访问该仓库的租户白名单；为空表示租户内全量可见。
	AccessPolicy []string  `json:"accessPolicy,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// RepoMatchRules 仓库与故障的关联规则（堆栈驱动懒加载的匹配依据）。
type RepoMatchRules struct {
	// PackagePrefixes 例如 ["com.acme.order", "order."]
	PackagePrefixes []string `json:"packagePrefixes,omitempty"`
	// PathPrefixes 例如 ["src/main/java/com/acme/order"]
	PathPrefixes []string `json:"pathPrefixes,omitempty"`
	// ArtifactNames 例如 ["order-service.jar"]
	ArtifactNames []string `json:"artifactNames,omitempty"`
	// HostPatterns 例如 ["order-svc", "order.internal"]
	HostPatterns []string `json:"hostPatterns,omitempty"`
	// EndpointPatterns 例如 ["/api/order/**"]
	EndpointPatterns []string `json:"endpointPatterns,omitempty"`
	// Keywords 兜底关键词
	Keywords []string `json:"keywords,omitempty"`
}

// RepositoryGroup 业务域分组：纯逻辑索引，不批量加载代码。
type RepositoryGroup struct {
	ID          string      `json:"id"`
	TenantID    string      `json:"tenantId"`
	Name        string      `json:"name"`
	Key         string      `json:"key"`
	Description string      `json:"description,omitempty"`
	Status      GroupStatus `json:"status"`
	// EntryRepositoryIDs 链路入口仓库（通常为前端或网关），跨仓库分析从此开始。
	EntryRepositoryIDs []string  `json:"entryRepositoryIds,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// GroupMember 分组与仓库的成员关系。
type GroupMember struct {
	GroupID      string `json:"groupId"`
	RepositoryID string `json:"repositoryId"`
	// Order 链路推断顺序，越小越靠近调用入口。
	Order     int       `json:"order"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// RepoResolved 任务级代码版本锁定的结果：仓库 + Commit。
type RepoResolved struct {
	RepositoryID string    `json:"repositoryId"`
	RepoKey      string    `json:"repoKey"`
	Name         string    `json:"name"`
	Layer        RepoLayer `json:"layer"`
	Commit       string    `json:"commit"`              // 实际锁定并解析出的 commit
	RequestedRef string    `json:"requestedRef"`        // 调用方传入的 ref（分支/Tag/Commit）
	ResolvedFrom string    `json:"resolvedFrom"`        // request | group_entry | stack_hint | repo_default
	MatchScore   float64   `json:"matchScore"`          // 关联度评分
	MatchedBy    []string  `json:"matchedBy,omitempty"` // 命中原因
	ResolvedAt   time.Time `json:"resolvedAt"`
}

// ---------------------------------------------------------------------------
// 任务接入层（第一层）请求模型
// ---------------------------------------------------------------------------

// CreateTaskRequest 外部调用方提交的分析任务。
type CreateTaskRequest struct {
	// Mode 为空时按 RepoID / GroupID 自动推断。
	Mode    TaskMode `json:"mode"`
	RepoID  string   `json:"repoId"`
	GroupID string   `json:"groupId"`
	// Ref 报错对应的分支/Tag/Commit，强烈建议传 Commit 以精准锁定线上版本。
	Ref string `json:"ref"`
	// Refs 分组模式下可为每个仓库单独指定版本。
	Refs map[string]string `json:"refs"`
	// Source 触发来源，用于统计与埋点。
	Source TriggerSource `json:"source"`
	// Title 便于人读的任务标题。
	Title string `json:"title"`
	// Stacktrace 原始异常堆栈文本（核心线索）。
	Stacktrace string `json:"stacktrace"`
	// Logs 附加日志（可选）。
	Logs string `json:"logs,omitempty"`
	// EntryFiles 调用方已知的嫌疑文件（可选）。
	EntryFiles []string `json:"entryFiles,omitempty"`
	// IdempotencyKey 幂等键：同租户同键的未完成任务会被复用，避免重复执行。
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Priority 0-9，越大越优先。
	Priority int `json:"priority,omitempty"`
	// Environment 环境标识（prod/staging/dev），仅用于报告与定级。
	Environment string `json:"environment,omitempty"`
	// AutoVerify 是否在沙箱中执行验证（默认 true）。
	AutoVerify *bool `json:"autoVerify,omitempty"`
	// CallbackURL 异步回调地址（可选）。
	CallbackURL string `json:"callbackUrl,omitempty"`
	// CallerAPIKeyID 发起任务的接入密钥 ID，由接入层从主体注入，用于回调节度对应签名密钥。
	CallerAPIKeyID string `json:"-"`
	// TenantID 由接入层从凭证注入，调用方无需传递。
	TenantID string `json:"-"`
	// RequestID 链路追踪 ID，由接入层注入。
	RequestID string `json:"-"`
}

// AutoVerifyEnabled 返回是否启用沙箱验证。
func (r *CreateTaskRequest) AutoVerifyEnabled() bool {
	return r.AutoVerify == nil || *r.AutoVerify
}

// ---------------------------------------------------------------------------
// 任务上下文（4.3 智能任务上下文设计）
// ---------------------------------------------------------------------------

// Task 由幂等键聚合的逻辑任务（一个缺陷一个任务）。
type Task struct {
	ID             string        `json:"id"`
	TenantID       string        `json:"tenantId"`
	Title          string        `json:"title"`
	Mode           TaskMode      `json:"mode"`
	GroupID        string        `json:"groupId,omitempty"`
	Source         TriggerSource `json:"source"`
	IdempotencyKey string        `json:"idempotencyKey,omitempty"`
	LatestRunID    string        `json:"latestRunId"`
	RunCount       int           `json:"runCount"`
	CreatedAt      time.Time     `json:"createdAt"`
	UpdatedAt      time.Time     `json:"updatedAt"`
}

// TaskRun 一次完整执行。所有上下文持久化固化，支撑多轮修复持续收敛。
type TaskRun struct {
	ID       string    `json:"id"`
	TaskID   string    `json:"taskId"`
	TenantID string    `json:"tenantId"`
	Attempt  int       `json:"attempt"`
	Mode     TaskMode  `json:"mode"`
	State    TaskState `json:"state"`
	Severity Severity  `json:"severity,omitempty"`

	Title       string `json:"title"`
	Environment string `json:"environment,omitempty"`
	Stacktrace  string `json:"stacktrace"`
	Logs        string `json:"logs,omitempty"`

	RequestedRef string   `json:"requestedRef,omitempty"`
	RepoIDs      []string `json:"repoIds,omitempty"`
	EntryFiles   []string `json:"entryFiles,omitempty"`

	// Resolution 任务级代码版本锁定结果。
	Resolution []RepoResolved `json:"resolution,omitempty"`
	// PinnedCommits 仓库 → Commit 的锁定快照，保证多轮修复使用同一份代码。
	PinnedCommits map[string]string `json:"pinnedCommits,omitempty"`

	// RootCause 根因结论（结构化）。
	RootCause *RootCause `json:"rootCause,omitempty"`
	// Patches 增量补丁。
	Patches []Patch `json:"patches,omitempty"`
	// Verification 沙箱验证结果。
	Verification *Verification `json:"verification,omitempty"`
	// ReportID 归档报告。
	ReportID string `json:"reportId,omitempty"`

	// Usage 资源消耗统计，用于配额与成本分析。
	Usage RunUsage `json:"usage"`

	Degraded bool     `json:"degraded,omitempty"`
	Warnings []string `json:"warnings,omitempty"`

	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	RequestID      string `json:"requestId,omitempty"`
	CallbackURL    string `json:"callbackUrl,omitempty"`
	// CallerAPIKeyID 发起本次运行的接入密钥，回调节度据此选取签名密钥。
	CallerAPIKeyID string `json:"callerApiKeyId,omitempty"`

	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ElapsedMS 返回执行耗时（毫秒）。
func (r *TaskRun) ElapsedMS() int64 {
	if r.StartedAt.IsZero() {
		return 0
	}
	end := r.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.StartedAt).Milliseconds()
}

// RunUsage 单次执行的资源消耗。
type RunUsage struct {
	FilesLoaded      int `json:"filesLoaded"`
	CodeChars        int `json:"codeChars"`
	SkillCalls       int `json:"skillCalls"`
	SkillFailures    int `json:"skillFailures"`
	ModelCalls       int `json:"modelCalls"`
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	CacheHits        int `json:"cacheHits"`
	ModelFallbacks   int `json:"modelFallbacks"`
}

// RootCause 结构化根因。
type RootCause struct {
	Summary     string   `json:"summary"`     // 一句话根因
	Category    string   `json:"category"`    // syntax/runtime/logic/dependency/compat/config
	Detail      string   `json:"detail"`      // 详细推理
	Confidence  float64  `json:"confidence"`  // 0-1
	Evidence    []string `json:"evidence"`    // 支撑证据（文件:行 或 调用链）
	BlastRadius []string `json:"blastRadius"` // 影响范围（仓库/接口）
	Severity    Severity `json:"severity"`
}

// Patch 增量补丁（禁止暴力重写，仅做最小改动）。
type Patch struct {
	ID           string      `json:"id"`
	RepositoryID string      `json:"repositoryId"`
	RepoKey      string      `json:"repoKey"`
	FilePath     string      `json:"filePath"`
	Action       PatchAction `json:"action"`
	// UnifiedDiff 标准 unified diff 文本，可直接 git apply。
	UnifiedDiff string `json:"unifiedDiff"`
	// OldContent/NewContent 用于前端可视化对比与回滚。
	OldContent string    `json:"oldContent,omitempty"`
	NewContent string    `json:"newContent,omitempty"`
	Rationale  string    `json:"rationale"`
	Risk       PatchRisk `json:"risk"`
	// TouchedLines 变更行号区间，用于与原有业务逻辑隔离性校验。
	TouchedLines []LineRange `json:"touchedLines,omitempty"`
	Status       PatchStatus `json:"status"`
	// ProducedBy 产出该补丁的技能名。
	ProducedBy string `json:"producedBy,omitempty"`
	// Verification 单补丁验证结果（若独立验证）。
	Verification *Verification `json:"verification,omitempty"`
}

// LineRange 行号区间（1-based，闭区间）。
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Verification 沙箱验证结果。
type Verification struct {
	Passed      bool                `json:"passed"`
	Workspace   string              `json:"workspace,omitempty"`
	ApplyResult string              `json:"applyResult,omitempty"` // applied | conflict | skipped
	Checks      []VerificationCheck `json:"checks"`
	DurationMS  int64               `json:"durationMs"`
	Log         string              `json:"log,omitempty"`
	Degraded    bool                `json:"degraded,omitempty"` // 无可用验证器时降级为静态校验
}

// VerificationCheck 单项校验结果。
type VerificationCheck struct {
	Name       string `json:"name"` // compile / unit_test / lint / ast_guard / dependency_resolve
	Passed     bool   `json:"passed"`
	Command    string `json:"command,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"durationMs"`
	Skipped    bool   `json:"skipped,omitempty"`
	SkipReason string `json:"skipReason,omitempty"`
}

// ---------------------------------------------------------------------------
// 证据与代码切片（任务上下文的核心绑定要素：命中代码切片）
// ---------------------------------------------------------------------------

// EvidenceBundle 一次分析所依赖的全部证据，全程持久化。
type EvidenceBundle struct {
	RunID     string    `json:"runId"`
	TenantID  string    `json:"tenantId"`
	CreatedAt time.Time `json:"createdAt"`

	Stack *StackAnalysis `json:"stack,omitempty"`
	// CandidateRepos 堆栈驱动匹配出的候选仓库（懒加载前）。
	CandidateRepos []RepoCandidate `json:"candidateRepos,omitempty"`
	// Slices 实际加载的代码切片（仅故障关联的少量文件）。
	Slices []CodeSlice `json:"slices,omitempty"`
	// CrossRepoEdges 跨仓库链路边。
	Edges []CallEdge `json:"crossRepoEdges,omitempty"`
	// Dependencies 依赖清单切片（pom.xml/go.mod/package.json）。
	DependencyFiles []CodeSlice `json:"dependencyFiles,omitempty"`

	FilesLoaded int  `json:"filesLoaded"`
	CodeChars   int  `json:"codeChars"`
	Truncated   bool `json:"truncated,omitempty"`
	// Notes 降级/裁剪说明，进入报告供人工判断。
	Notes []string `json:"notes,omitempty"`
}

// StackFrame 一帧堆栈。
type StackFrame struct {
	Raw        string  `json:"raw"`
	Package    string  `json:"package,omitempty"`
	Class      string  `json:"class,omitempty"`
	Method     string  `json:"method,omitempty"`
	File       string  `json:"file,omitempty"`
	Line       int     `json:"line,omitempty"`
	Language   string  `json:"language,omitempty"`
	Confidence float64 `json:"confidence"`
	// RepoKey/RepositoryID 由仓库匹配阶段回填。
	RepoKey      string `json:"repoKey,omitempty"`
	RepositoryID string `json:"repositoryId,omitempty"`
}

// StackAnalysis 堆栈解析技能输出。
type StackAnalysis struct {
	Language      string       `json:"language"`
	ExceptionType string       `json:"exceptionType"`
	Message       string       `json:"message"`
	Thread        string       `json:"thread,omitempty"`
	CausedBy      []string     `json:"causedBy,omitempty"`
	Frames        []StackFrame `json:"frames"`
	// FileHints 供源码定位使用的文件/包线索。
	FileHints []string `json:"fileHints"`
	// Symbols 关键符号（类名、方法名、Bean 名）。
	Symbols []string `json:"symbols"`
	// Category 异常分类：null_pointer / index_out_of_bounds / ...
	Category string `json:"category"`
	// Endpoints 堆栈/日志中出现的 HTTP 路径与服务名。
	Endpoints []string `json:"endpoints,omitempty"`
	Services  []string `json:"services,omitempty"`
	TraceID   string   `json:"traceId,omitempty"`
	// CleanedLog 清洗后的日志（已脱敏、去噪）。
	CleanedLog string `json:"cleanedLog,omitempty"`
}

// RepoCandidate 候选关联仓库及评分。
type RepoCandidate struct {
	RepositoryID string    `json:"repositoryId"`
	RepoKey      string    `json:"repoKey"`
	Name         string    `json:"name"`
	Layer        RepoLayer `json:"layer"`
	Score        float64   `json:"score"`
	MatchedBy    []string  `json:"matchedBy"`
	// Hints 该仓库下需要优先加载的文件线索。
	Hints []FileHint `json:"hints"`
}

// FileHint 文件定位线索。
type FileHint struct {
	Path     string  `json:"path"`
	Reason   string  `json:"reason"` // frame / package_prefix / entry / import / symbol
	Line     int     `json:"line,omitempty"`
	Priority float64 `json:"priority"`
}

// CodeSlice 命中代码切片。
type CodeSlice struct {
	RepositoryID string `json:"repositoryId"`
	RepoKey      string `json:"repoKey"`
	Commit       string `json:"commit"`
	Path         string `json:"path"`
	Language     string `json:"language"`
	Content      string `json:"content"`
	// StartLine/EndLine 切片在文件中的位置（全文切片为 1..N）。
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
	// FocusLines 故障相关行号。
	FocusLines []int  `json:"focusLines,omitempty"`
	Reason     string `json:"reason"`
	Chars      int    `json:"chars"`
	// Truncated 是否被裁剪。
	Truncated bool `json:"truncated,omitempty"`
	// Redacted 是否检测并脱敏过敏感信息。
	Redacted bool `json:"redacted,omitempty"`
	// FromCache 是否命中文件级缓存。
	FromCache bool `json:"fromCache,omitempty"`
	// Symbols AST 提取的符号摘要。
	Symbols []string `json:"symbols,omitempty"`
	// Imports 该文件 import/require 的包。
	Imports []string `json:"imports,omitempty"`
}

// CallEdge 跨仓库调用链边。
type CallEdge struct {
	FromRepo   string  `json:"fromRepo"`
	ToRepo     string  `json:"toRepo"`
	FromFile   string  `json:"fromFile,omitempty"`
	ToFile     string  `json:"toFile,omitempty"`
	Symbol     string  `json:"symbol,omitempty"`
	Protocol   string  `json:"protocol,omitempty"` // http / grpc / mq / rpc
	Endpoint   string  `json:"endpoint,omitempty"`
	Evidence   string  `json:"evidence,omitempty"`
	Confidence float64 `json:"confidence"`
	// Anomaly 该链路上的异常点。
	Anomaly string `json:"anomaly,omitempty"`
}

// ---------------------------------------------------------------------------
// Skill / MCP 调用记录（任务上下文绑定要素）
// ---------------------------------------------------------------------------

// SkillCall 技能调用记录，全链路留存。
type SkillCall struct {
	ID           string     `json:"id"`
	RunID        string     `json:"runId"`
	TenantID     string     `json:"tenantId"`
	Skill        string     `json:"skill"`
	Version      string     `json:"version"`
	Stage        string     `json:"stage"`
	Status       CallStatus `json:"status"`
	InputDigest  string     `json:"inputDigest,omitempty"`
	OutputDigest string     `json:"outputDigest,omitempty"`
	Error        string     `json:"error,omitempty"`
	Attempts     int        `json:"attempts"`
	DurationMS   int64      `json:"durationMs"`
	FromFallback bool       `json:"fromFallback,omitempty"`
	Output       any        `json:"output,omitempty"`
	StartedAt    time.Time  `json:"startedAt"`
}

// ModelCall 模型推理记录。
type ModelCall struct {
	ID               string     `json:"id"`
	RunID            string     `json:"runId"`
	TenantID         string     `json:"tenantId"`
	Stage            string     `json:"stage"`
	Provider         string     `json:"provider"`
	Model            string     `json:"model"`
	Tier             ModelTier  `json:"tier"`
	Status           CallStatus `json:"status"`
	Attempt          int        `json:"attempt"`
	FallbackFrom     string     `json:"fallbackFrom,omitempty"`
	PromptTokens     int        `json:"promptTokens"`
	CompletionTokens int        `json:"completionTokens"`
	TotalTokens      int        `json:"totalTokens"`
	DurationMS       int64      `json:"durationMs"`
	Error            string     `json:"error,omitempty"`
	// SchemaValid 结构化输出是否通过 Schema 校验。
	SchemaValid bool      `json:"schemaValid"`
	StartedAt   time.Time `json:"startedAt"`
}

// ModelProviderConfig 模型提供方配置（AI 设置的持久化实体）。
//
// 与 config.ProviderConfig 的差异：
//   - 本实体随 Store 快照落盘，支持控制台在线修改并重启后保留；
//   - APIKey 只以密文（AES-GCM）形式存储，明文仅在运行时由模型管控层持有；
//   - 不回显明文，对外只暴露 HasKey 与 KeyHint（末 4 位的掩码形式）。
type ModelProviderConfig struct {
	Name string `json:"name"`
	// Kind 提供方类型：openai | deepseek | azure | ollama | openai_compatible | mock。
	Kind string `json:"kind"`
	// BaseURL 接口基址。
	BaseURL string `json:"baseUrl"`
	// APIKeyEnc 加密后的 API Key（服务端 AES-GCM 密文）。
	APIKeyEnc string `json:"apiKeyEnc,omitempty"`
	// HasKey 是否配置了密钥（kind=mock/ollama 等本地推理可为空）。
	HasKey bool `json:"hasKey"`
	// KeyHint 密钥掩码提示（末 4 位），仅用于界面辨识，不含可还原信息。
	KeyHint string `json:"keyHint,omitempty"`
	// Models 模型列表，第一个为主模型。
	Models []string `json:"models"`
	// Tier 分层：light | strong | fallback。
	Tier string `json:"tier"`
	// MaxTokens 单次输出上限。
	MaxTokens int `json:"maxTokens"`
	// Temperature 采样温度。
	Temperature float64 `json:"temperature"`
	// Weight 同层多模型的加权轮询权重（1-10）。
	Weight int `json:"weight"`
	// Enabled 是否参与调度。
	Enabled bool `json:"enabled"`
	// UpdatedAt 最近一次修改时间。
	UpdatedAt time.Time `json:"updatedAt"`
}

// ---------------------------------------------------------------------------
// 报告 / 审计 / 事件
// ---------------------------------------------------------------------------

// Report 归档报告：任务终态时生成，支撑复盘与统计。
type Report struct {
	ID       string    `json:"id"`
	RunID    string    `json:"runId"`
	TaskID   string    `json:"taskId"`
	TenantID string    `json:"tenantId"`
	Title    string    `json:"title"`
	Mode     TaskMode  `json:"mode"`
	State    TaskState `json:"state"`
	Severity Severity  `json:"severity"`

	Summary      string         `json:"summary"`
	RootCause    *RootCause     `json:"rootCause,omitempty"`
	Patches      []Patch        `json:"patches,omitempty"`
	Verification *Verification  `json:"verification,omitempty"`
	Resolution   []RepoResolved `json:"resolution,omitempty"`
	// ChainFlow 跨仓库链路（分组模式）。
	ChainFlow []CallEdge `json:"chainFlow,omitempty"`
	// Timeline 任务时间线关键节点。
	Timeline []TimelineItem `json:"timeline,omitempty"`
	// Suggestions 人工复核建议与后续动作。
	Suggestions []string `json:"suggestions,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Degraded    bool     `json:"degraded,omitempty"`
	// Markdown 可直接贴到工单/PR 的完整报告。
	Markdown  string    `json:"markdown"`
	Usage     RunUsage  `json:"usage"`
	ElapsedMS int64     `json:"elapsedMs"`
	CreatedAt time.Time `json:"createdAt"`
}

// TimelineItem 时间线节点。
type TimelineItem struct {
	At      time.Time `json:"at"`
	Stage   string    `json:"stage"`
	Message string    `json:"message"`
	Level   string    `json:"level,omitempty"` // info | warn | error
	Data    any       `json:"data,omitempty"`
}

// AuditEvent 审计事件：覆盖任务生命周期、仓库切换、技能调用、模型推理、源码拉取、修复变更。
type AuditEvent struct {
	ID       string `json:"id"`
	TenantID string `json:"tenantId"`
	RunID    string `json:"runId,omitempty"`
	TaskID   string `json:"taskId,omitempty"`
	// Category 分类：task / repo / group / skill / model / source / patch / auth / quota
	Category  string    `json:"category"`
	Action    string    `json:"action"`
	Level     string    `json:"level"` // info | warn | error
	Message   string    `json:"message"`
	RepoID    string    `json:"repoId,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Skill     string    `json:"skill,omitempty"`
	Model     string    `json:"model,omitempty"`
	Data      any       `json:"data,omitempty"`
	RequestID string    `json:"requestId,omitempty"`
	At        time.Time `json:"at"`
}

// Event 实时事件（SSE / WebSocket 推送）。
type Event struct {
	Seq      int64     `json:"seq"`
	Type     string    `json:"type"` // task.created / task.state / stage / skill.call / model.call / log / report
	TenantID string    `json:"tenantId"`
	RunID    string    `json:"runId,omitempty"`
	TaskID   string    `json:"taskId,omitempty"`
	Level    string    `json:"level,omitempty"`
	Stage    string    `json:"stage,omitempty"`
	Message  string    `json:"message,omitempty"`
	Payload  any       `json:"payload,omitempty"`
	At       time.Time `json:"at"`
}

// ---------------------------------------------------------------------------
// 分页
// ---------------------------------------------------------------------------

// Page 通用分页结果。
type Page[T any] struct {
	Items    []T `json:"items"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
}

// PageQuery 通用分页查询。
type PageQuery struct {
	Page     int    `json:"page"`
	PageSize int    `json:"pageSize"`
	Keyword  string `json:"keyword,omitempty"`
	State    string `json:"state,omitempty"`
	Sort     string `json:"sort,omitempty"`
}

// Normalize 修正非法分页参数。
func (q *PageQuery) Normalize() {
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.PageSize > 200 {
		q.PageSize = 200
	}
}

// Offset 返回偏移量。
func (q PageQuery) Offset() int { return (q.Page - 1) * q.PageSize }
