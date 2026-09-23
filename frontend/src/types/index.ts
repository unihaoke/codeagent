/**
 * 与后端 `internal/domain` 完全对齐的类型定义（JSON tag 小驼峰）。
 * 后端为唯一事实来源，本文件只做镜像，不得自行增删字段语义。
 */

/* ------------------------------ 枚举 ------------------------------ */

export type TenantStatus = 'active' | 'suspended'

export type VCSProvider = 'git' | 'github' | 'gitlab' | 'gitee'

export type CredentialType = 'ssh_key' | 'token' | 'basic' | 'none'

export type RepoStatus = 'active' | 'disabled'

export type RepoLayer = 'frontend' | 'gateway' | 'service' | 'middleware' | 'library' | 'unknown'

export type GroupStatus = 'active' | 'disabled'

export type TaskState =
  | 'queued'
  | 'analyzing'
  | 'repairing'
  | 'verifying'
  | 'succeeded'
  | 'needs_review'
  | 'failed'
  | 'cancelled'
  | 'degraded'

export type TaskMode = 'single_repo' | 'group'

export type TriggerSource = 'alert' | 'ide' | 'cicd' | 'manual' | 'openapi'

export type Severity = 'blocker' | 'critical' | 'major' | 'minor' | 'info'

export type PatchAction = 'modify' | 'create' | 'delete'

export type PatchRisk = 'low' | 'medium' | 'high'

export type PatchStatus = 'proposed' | 'applied' | 'rejected' | 'failed'

export type SkillCategory = 'parse' | 'diagnose' | 'repair' | 'verify' | 'cross_repo'

export type SkillStatus = 'enabled' | 'disabled' | 'canary' | 'deprecated'

export type ModelTier = 'light' | 'strong' | 'fallback'

export type CallStatus = 'ok' | 'failed' | 'timeout' | 'blocked' | 'skipped' | 'fallback'

/* --------------------------- 租户 / 权限 --------------------------- */

export interface TenantQuota {
  maxConcurrentTasks: number
  maxFilesPerTask: number
  maxCodeChars: number
  maxTokensPerTask: number
  maxTaskSeconds: number
}

export interface Tenant {
  id: string
  name: string
  status: TenantStatus
  description?: string
  quota: TenantQuota
  createdAt: string
  updatedAt: string
}

export interface APIKey {
  id: string
  tenantId: string
  name: string
  keyPrefix: string
  scopes: string[]
  expiresAt?: string
  lastUsedAt?: string
  revoked: boolean
  createdAt: string
}

export interface Credential {
  id: string
  tenantId: string
  name: string
  type: CredentialType
  username?: string
  hasSecret: boolean
  createdAt: string
  updatedAt: string
}

export interface Subject {
  tenantId: string
  tenantName?: string
  apiKeyId?: string
  keyPrefix?: string
  scopes?: string[]
  admin: boolean
  username?: string
  requestId?: string
}

/* --------------------------- 仓库 / 分组 --------------------------- */

export interface RepoMatchRules {
  packagePrefixes?: string[]
  pathPrefixes?: string[]
  artifactNames?: string[]
  hostPatterns?: string[]
  endpointPatterns?: string[]
  keywords?: string[]
}

export interface Repository {
  id: string
  tenantId: string
  name: string
  key: string
  url: string
  defaultBranch: string
  provider: VCSProvider
  layer: RepoLayer
  language: string
  credentialId?: string
  status: RepoStatus
  matchRules: RepoMatchRules
  accessPolicy?: string[]
  createdAt: string
  updatedAt: string
}

export interface RepositoryGroup {
  id: string
  tenantId: string
  name: string
  key: string
  description?: string
  status: GroupStatus
  entryRepositoryIds?: string[]
  createdAt: string
  updatedAt: string
}

export interface GroupMember {
  groupId: string
  repositoryId: string
  order: number
  note?: string
  createdAt: string
  repo?: Repository
}

export interface GroupImpact {
  repoCount: number
  byLayer: Record<string, number>
  byLanguage: Record<string, number>
  entryRepos: string[]
  sharedLibs: string[]
  riskScore: number
  riskNotes?: string[]
}

/* ------------------------------ 任务 ------------------------------ */

export interface RepoResolved {
  repositoryId: string
  repoKey: string
  name: string
  layer: RepoLayer
  commit: string
  requestedRef: string
  resolvedFrom: string
  matchScore: number
  matchedBy?: string[]
  resolvedAt: string
}

export interface RunUsage {
  filesLoaded: number
  codeChars: number
  skillCalls: number
  skillFailures: number
  modelCalls: number
  promptTokens: number
  completionTokens: number
  totalTokens: number
  cacheHits: number
  modelFallbacks: number
}

export interface LineRange {
  start: number
  end: number
}

export interface VerificationCheck {
  name: string
  passed: boolean
  command?: string
  output?: string
  durationMs: number
  skipped?: boolean
  skipReason?: string
}

export interface Verification {
  passed: boolean
  workspace?: string
  applyResult?: string
  checks: VerificationCheck[]
  durationMs: number
  log?: string
  degraded?: boolean
}

export interface Patch {
  id: string
  repositoryId: string
  repoKey: string
  filePath: string
  action: PatchAction
  unifiedDiff: string
  oldContent?: string
  newContent?: string
  rationale: string
  risk: PatchRisk
  touchedLines?: LineRange[]
  status: PatchStatus
  producedBy?: string
  verification?: Verification
}

export interface RootCause {
  summary: string
  category: string
  detail: string
  confidence: number
  evidence: string[]
  blastRadius: string[]
  severity: Severity
}

export interface Task {
  id: string
  tenantId: string
  title: string
  mode: TaskMode
  groupId?: string
  source: TriggerSource
  idempotencyKey?: string
  latestRunId: string
  runCount: number
  createdAt: string
  updatedAt: string
}

export interface TaskRun {
  id: string
  taskId: string
  tenantId: string
  attempt: number
  mode: TaskMode
  state: TaskState
  severity?: Severity
  title: string
  environment?: string
  stacktrace: string
  logs?: string
  requestedRef?: string
  repoIds?: string[]
  entryFiles?: string[]
  resolution?: RepoResolved[]
  pinnedCommits?: Record<string, string>
  rootCause?: RootCause
  patches?: Patch[]
  verification?: Verification
  reportId?: string
  usage: RunUsage
  degraded?: boolean
  warnings?: string[]
  idempotencyKey?: string
  requestId?: string
  callbackUrl?: string
  error?: string
  createdAt: string
  startedAt?: string
  endedAt?: string
  updatedAt: string
  elapsedMs?: number
}

export interface CreateTaskRequest {
  mode?: TaskMode
  repoId?: string
  groupId?: string
  ref?: string
  refs?: Record<string, string>
  source?: TriggerSource
  title?: string
  stacktrace: string
  logs?: string
  entryFiles?: string[]
  idempotencyKey?: string
  priority?: number
  environment?: string
  autoVerify?: boolean
  callbackUrl?: string
}

export interface SkillCall {
  id: string
  runId: string
  tenantId: string
  skill: string
  version: string
  stage: string
  status: CallStatus
  inputDigest?: string
  outputDigest?: string
  error?: string
  attempts: number
  durationMs: number
  fromFallback?: boolean
  startedAt: string
}

export interface ModelCall {
  id: string
  runId: string
  tenantId: string
  stage: string
  provider: string
  model: string
  tier: ModelTier
  status: CallStatus
  attempt: number
  fallbackFrom?: string
  promptTokens: number
  completionTokens: number
  totalTokens: number
  durationMs: number
  error?: string
  schemaValid: boolean
  startedAt: string
}

/* ------------------------------ 报告 ------------------------------ */

export interface CallEdge {
  fromRepo: string
  toRepo: string
  fromFile?: string
  toFile?: string
  symbol?: string
  protocol?: string
  endpoint?: string
  evidence?: string
  confidence: number
  anomaly?: string
}

export interface TimelineItem {
  at: string
  stage: string
  message: string
  level?: string
  data?: unknown
}

export interface Report {
  id: string
  runId: string
  taskId: string
  tenantId: string
  title: string
  mode: TaskMode
  state: TaskState
  severity: Severity
  summary: string
  rootCause?: RootCause
  patches?: Patch[]
  verification?: Verification
  resolution?: RepoResolved[]
  chainFlow?: CallEdge[]
  timeline?: TimelineItem[]
  suggestions?: string[]
  warnings?: string[]
  degraded?: boolean
  markdown: string
  usage: RunUsage
  elapsedMs: number
  createdAt: string
}

/* ---------------------------- 技能 / 模型 ---------------------------- */

export interface SkillManifest {
  name: string
  version: string
  title: string
  description: string
  category: SkillCategory
  status: SkillStatus
  inputSchema: Record<string, unknown>
  outputSchema?: Record<string, unknown>
  timeout: number
  retries: number
  maxConcurrency: number
  failureThreshold: number
  cooldownSeconds: number
  fallbackSkills?: string[]
  compatibleWith?: string
  builtin: boolean
  priority: number
  canaryPercent?: number
  tags?: string[]
}

export interface SkillHealth {
  skill: string
  version: string
  status: SkillStatus
  healthy: boolean
  circuitState: 'closed' | 'open' | 'half_open'
  failures: number
  successes: number
  totalCalls: number
  avgLatencyMs: number
  lastError?: string
  openUntil?: string
}

export interface SkillView {
  manifest: SkillManifest
  health?: SkillHealth
}

export interface ModelTarget {
  provider: string
  model: string
  tier: ModelTier
  endpoint?: string
  maxTokens?: number
  temperature?: number
}

export interface ModelHealth {
  provider: string
  model: string
  tier: ModelTier
  healthy: boolean
  circuitState: string
  failures: number
  avgLatencyMs: number
  totalCalls: number
  lastError?: string
  openUntil?: string
}

export interface ModelStats {
  totalCalls: number
  failedCalls: number
  fallbackCalls: number
  repairedOutputs: number
  promptTokens: number
  completionTokens: number
  totalTokens: number
  avgLatencyMs: number
  byModel: Record<string, number>
  byTier: Record<string, number>
}

/* ---------------------------- AI 设置（模型管控） ---------------------------- */

// 模型提供方接入设置（控制台「AI 设置」持久化）。
// 契约对齐后端 providerSetting：以 name 为唯一标识，对外不暴露密钥明文。
export interface ModelProviderSetting {
  name: string // 唯一标识，如 openai / deepseek / 自定义
  kind: string // openai | deepseek | azure | ollama | openai_compatible
  baseUrl: string
  models: string[] // 该 provider 下可用模型名
  tier?: string // light | strong | fallback
  maxTokens?: number
  temperature?: number
  weight?: number
  enabled: boolean
  hasKey: boolean // 是否已设置密钥（前端不回显明文）
  keyHint?: string // 密钥掩码提示（仅末 4 位）
  source?: string // store（控制台）| config（配置文件）
  updatedAt?: string
}

export interface ModelConfigPayload {
  items: Array<
    Omit<ModelProviderSetting, 'hasKey' | 'keyHint' | 'source' | 'updatedAt'> & {
      apiKey?: string
    }
  >
}

export interface ModelTestResult {
  ok: boolean
  durationMs?: number
  model?: string
  message: string
}

/* ---------------------------- 可观测 ---------------------------- */

export interface AuditEvent {
  id: string
  tenantId: string
  runId?: string
  taskId?: string
  category: string
  action: string
  level: string
  message: string
  repoId?: string
  commit?: string
  skill?: string
  model?: string
  data?: unknown
  requestId?: string
  at: string
}

export interface SkillMetric {
  skill: string
  calls: number
  failures: number
  avgLatencyMs: number
}

export interface ModelMetric {
  model: string
  calls: number
  avgLatencyMs: number
  totalTokens: number
}

export interface RunBrief {
  runId: string
  title: string
  state: TaskState
  severity: Severity
  summary?: string
  at: string
}

export interface ObservabilitySummary {
  tasksTotal: number
  runsTotal: number
  runsSucceeded: number
  runsNeedsReview: number
  runsFailed: number
  runsDegraded: number
  fixRate: number
  avgElapsedMs: number
  skillCalls: number
  skillFailures: number
  skillFailureRate: number
  modelCalls: number
  modelTokens: number
  modelFallbacks: number
  cacheHits: number
  repoSwitches: number
  topSkills: SkillMetric[]
  topModels: ModelMetric[]
  stateDist: Record<string, number>
  categoryDist: Record<string, number>
  recentFailures: RunBrief[]
}

export interface EngineStats {
  running: number
  queued: number
  total: number
  byState: Record<string, number>
  bySeverity: Record<string, number>
  successRate: number
  avgElapsedMs: number
  repoSwitches: number
  degradedRuns: number
}

/* ---------------------------- 实时事件 ---------------------------- */

export interface AgentEvent {
  seq: number
  type:
    | 'task.created'
    | 'task.state'
    | 'task.terminal'
    | 'stage'
    | 'skill.call'
    | 'model.call'
    | 'source.fetch'
    | 'repo.switch'
    | 'patch.proposed'
    | 'verification'
    | 'report.ready'
    | 'log'
    | string
  tenantId: string
  runId?: string
  taskId?: string
  level?: string
  stage?: string
  message?: string
  payload?: unknown
  at: string
}

/* ------------------------------ 通用 ------------------------------ */

export interface Page<T> {
  items: T[]
  total: number
  page: number
  pageSize: number
}

export interface PageQuery {
  page?: number
  pageSize?: number
  keyword?: string
  state?: string
  /** 允许透传其它查询参数（后端按需读取）。 */
  [key: string]: unknown
}

export interface Envelope<T> {
  code: number
  message: string
  requestId?: string
  data: T
}

export interface HealthItem {
  name: string
  ok: boolean
  detail?: string
}
