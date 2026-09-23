// Package domain 定义全系统共享的领域模型与稳定契约。
//
// 本包不依赖任何其他内部包，是七层架构（接入层 / 租户权限层 / 仓库分组索引层 /
// Agent 调度核心层 / Skill 插件层 / MCP 模型管控层 / 源码沙箱执行层）之间唯一的
// 数据交换语言。任何跨层调用都必须经由本包定义的类型与接口。
package domain

import "strings"

// ---------------------------------------------------------------------------
// 租户 / 仓库
// ---------------------------------------------------------------------------

// TenantStatus 租户状态。
type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
)

// VCSProvider 代码托管平台类型。
type VCSProvider string

const (
	VCSGit    VCSProvider = "git"
	VCSGitHub VCSProvider = "github"
	VCSGitLab VCSProvider = "gitlab"
	VCSGitee  VCSProvider = "gitee"
)

// CredentialType Git 凭证类型。
type CredentialType string

const (
	CredSSHKey CredentialType = "ssh_key"
	CredToken  CredentialType = "token"
	CredBasic  CredentialType = "basic"
	CredNone   CredentialType = "none"
)

// RepoStatus 仓库状态。
type RepoStatus string

const (
	RepoActive   RepoStatus = "active"
	RepoDisabled RepoStatus = "disabled"
)

// RepoLayer 仓库在业务链路中的分层，用于跨仓库链路分析时的拓扑推断。
type RepoLayer string

const (
	LayerFrontend   RepoLayer = "frontend"
	LayerGateway    RepoLayer = "gateway"
	LayerService    RepoLayer = "service"
	LayerMiddleware RepoLayer = "middleware"
	LayerLibrary    RepoLayer = "library"
	LayerUnknown    RepoLayer = "unknown"
)

// GroupStatus 分组状态。
type GroupStatus string

const (
	GroupActive   GroupStatus = "active"
	GroupDisabled GroupStatus = "disabled"
)

// ---------------------------------------------------------------------------
// 任务状态机
// ---------------------------------------------------------------------------

// TaskState 任务（TaskRun）状态机。
//
//	queued → analyzing → repairing → verifying → succeeded
//	                                        ↘ needs_review
//	任意态 → failed / cancelled / degraded
type TaskState string

const (
	StateQueued      TaskState = "queued"
	StateAnalyzing   TaskState = "analyzing"
	StateRepairing   TaskState = "repairing"
	StateVerifying   TaskState = "verifying"
	StateSucceeded   TaskState = "succeeded"
	StateNeedsReview TaskState = "needs_review"
	StateFailed      TaskState = "failed"
	StateCancelled   TaskState = "cancelled"
	StateDegraded    TaskState = "degraded"
)

// TaskMode 任务模式：单仓库精准修复 / 多仓库分组联合排查。
type TaskMode string

const (
	ModeSingleRepo TaskMode = "single_repo"
	ModeGroup      TaskMode = "group"
)

// TriggerSource 任务触发来源。
type TriggerSource string

const (
	TriggerAlert   TriggerSource = "alert"
	TriggerIDE     TriggerSource = "ide"
	TriggerCICD    TriggerSource = "cicd"
	TriggerManual  TriggerSource = "manual"
	TriggerOpenAPI TriggerSource = "openapi"
)

// Severity 问题定级。
type Severity string

const (
	SeverityBlocker  Severity = "blocker"
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityInfo     Severity = "info"
)

// ---------------------------------------------------------------------------
// Patch / 修复
// ---------------------------------------------------------------------------

// PatchAction 增量补丁动作。
type PatchAction string

const (
	ActionModify PatchAction = "modify"
	ActionCreate PatchAction = "create"
	ActionDelete PatchAction = "delete"
)

// PatchRisk 修复风险等级，决定是否需要人工复核。
type PatchRisk string

const (
	RiskLow    PatchRisk = "low"
	RiskMedium PatchRisk = "medium"
	RiskHigh   PatchRisk = "high"
)

// PatchStatus 补丁验证状态。
type PatchStatus string

const (
	PatchProposed PatchStatus = "proposed"
	PatchApplied  PatchStatus = "applied"
	PatchRejected PatchStatus = "rejected"
	PatchFailed   PatchStatus = "failed"
)

// ---------------------------------------------------------------------------
// Skill
// ---------------------------------------------------------------------------

// SkillCategory 技能分类。
type SkillCategory string

const (
	CatParse     SkillCategory = "parse"      // 代码解析
	CatDiagnose  SkillCategory = "diagnose"   // 报错溯源
	CatRepair    SkillCategory = "repair"     // 修复
	CatVerify    SkillCategory = "verify"     // 验证
	CatCrossRepo SkillCategory = "cross_repo" // 多仓库扩展
)

// SkillStatus 技能生命周期状态。
type SkillStatus string

const (
	SkillEnabled    SkillStatus = "enabled"
	SkillDisabled   SkillStatus = "disabled"
	SkillCanary     SkillStatus = "canary" // 灰度
	SkillDeprecated SkillStatus = "deprecated"
)

// ---------------------------------------------------------------------------
// MCP
// ---------------------------------------------------------------------------

// ModelTier 模型分层，用于多模型智能调度。
type ModelTier string

const (
	TierLight    ModelTier = "light"    // 轻量：日志清洗、分类、路由
	TierStrong   ModelTier = "strong"   // 强力：根因推理、补丁生成
	TierFallback ModelTier = "fallback" // 兜底：格式修复、降级重试
)

// CallStatus 通用调用状态（Skill 调用、模型调用）。
type CallStatus string

const (
	CallOK       CallStatus = "ok"
	CallFailed   CallStatus = "failed"
	CallTimeout  CallStatus = "timeout"
	CallBlocked  CallStatus = "blocked" // 被熔断/配额拦截
	CallSkipped  CallStatus = "skipped"
	CallFallback CallStatus = "fallback"
)

// ---------------------------------------------------------------------------
// 枚举辅助函数
// ---------------------------------------------------------------------------

// TerminalStates 返回所有终态。
func TerminalStates() []TaskState {
	return []TaskState{StateSucceeded, StateNeedsReview, StateFailed, StateCancelled, StateDegraded}
}

// IsTerminal 判断任务是否已进入终态。
func (s TaskState) IsTerminal() bool {
	for _, t := range TerminalStates() {
		if s == t {
			return true
		}
	}
	return false
}

// IsRunning 判断任务是否处于执行中。
func (s TaskState) IsRunning() bool {
	return s == StateAnalyzing || s == StateRepairing || s == StateVerifying
}

// CanTransitionTo 校验状态机迁移合法性，防止任务状态被乱序篡改。
func (s TaskState) CanTransitionTo(next TaskState) bool {
	switch s {
	case StateQueued:
		return next == StateAnalyzing || next == StateCancelled || next == StateFailed
	case StateAnalyzing:
		return next == StateRepairing || next == StateDegraded || next == StateFailed || next == StateCancelled
	case StateRepairing:
		return next == StateVerifying || next == StateDegraded || next == StateFailed || next == StateCancelled
	case StateVerifying:
		return next == StateSucceeded || next == StateNeedsReview || next == StateDegraded ||
			next == StateFailed || next == StateCancelled
	default:
		return false // 终态不可再迁移
	}
}

// NormalizeSeverity 归一化模型返回的定级文本。
func NormalizeSeverity(raw string) Severity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "blocker", "p0", "fatal":
		return SeverityBlocker
	case "critical", "p1", "error", "严重":
		return SeverityCritical
	case "major", "p2", "warn", "warning":
		return SeverityMajor
	case "minor", "p3", "info", "低":
		return SeverityMinor
	default:
		return SeverityInfo
	}
}

// AllSkillNames 内置技能标准名，Skill 层与 Pipeline 必须保持一致。
const (
	SkillStacktraceParse     = "stacktrace_parse"
	SkillCodeASTAnalyze      = "code_ast_analyze"
	SkillErrorLocalize       = "error_localize"
	SkillCrossRepoTrace      = "cross_repo_trace"
	SkillCodePatchSynthesize = "code_patch_synthesize"
	SkillDependencyRepair    = "dependency_repair"
	SkillPatchVerify         = "patch_verify"
)

// ---------------------------------------------------------------------------
// 根因分类口径（全系统唯一权威定义）
// ---------------------------------------------------------------------------
//
// RootCause.Category 采用**细粒度**口径，与堆栈解析、补丁规则库、Mock 推理器一致；
// 同时保留粗粒度分类作为兜底（语法/运行时/逻辑/依赖/兼容/配置），
// 两者都受 CategoryEnums() 约束（模型结构化输出的 enum 即取该集合）。

const (
	// CategoryNullPointer 空指针 / Nil 解引用。
	CategoryNullPointer = "null_pointer"
	// CategoryIndexOutOfBounds 数组或集合越界。
	CategoryIndexOutOfBounds = "index_out_of_bounds"
	// CategoryTypeError 类型错误（含 JS/TS 的 undefined 属性访问）。
	CategoryTypeError = "type_error"
	// CategoryClassNotFound 类或模块未找到。
	CategoryClassNotFound = "class_not_found"
	// CategoryDependencyMissing 依赖缺失。
	CategoryDependencyMissing = "dependency_missing"
	// CategoryVersionConflict 依赖版本冲突。
	CategoryVersionConflict = "version_conflict"
	// CategoryConnectionRefused 连接被拒绝（下游不可达）。
	CategoryConnectionRefused = "connection_refused"
	// CategoryTimeout 调用或执行超时。
	CategoryTimeout = "timeout"
	// CategoryDivideByZero 除零。
	CategoryDivideByZero = "divide_by_zero"
	// CategoryConcurrentModification 并发修改集合。
	CategoryConcurrentModification = "concurrent_modification"
	// CategorySerializationError 序列化 / 反序列化异常。
	CategorySerializationError = "serialization_error"
	// CategoryResourceLeak 资源未释放。
	CategoryResourceLeak = "resource_leak"
	// CategorySyntaxError 语法错误。
	CategorySyntaxError = "syntax_error"
	// CategoryOOM 内存溢出。
	CategoryOOM = "oom"

	// 以下为粗粒度兜底分类。
	CategorySyntax     = "syntax"
	CategoryRuntime    = "runtime"
	CategoryLogic      = "logic"
	CategoryDependency = "dependency"
	CategoryCompat     = "compat"
	CategoryConfig     = "config"
	CategoryUnknown    = "unknown"
)

// CategoryEnums 返回根因分类的全部合法取值（用于模型输出 Schema 的 enum 约束）。
func CategoryEnums() []string {
	return []string{
		CategoryNullPointer, CategoryIndexOutOfBounds, CategoryTypeError, CategoryClassNotFound,
		CategoryDependencyMissing, CategoryVersionConflict, CategoryConnectionRefused, CategoryTimeout,
		CategoryDivideByZero, CategoryConcurrentModification, CategorySerializationError,
		CategoryResourceLeak, CategorySyntaxError, CategoryOOM,
		CategorySyntax, CategoryRuntime, CategoryLogic, CategoryDependency,
		CategoryCompat, CategoryConfig, CategoryUnknown,
	}
}
