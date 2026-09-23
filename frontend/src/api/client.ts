/**
 * 接入层 API 客户端。
 *
 * 统一处理：认证头（控制台 JWT / 接入层 API Key）、响应信封解包、
 * 错误码到 Error 的映射、查询参数序列化、SSE 与 WebSocket 连接。
 */
import type {
  AgentEvent,
  APIKey,
  CreateTaskRequest,
  Credential,
  EngineStats,
  Envelope,
  GroupImpact,
  GroupMember,
  HealthItem,
  ModelHealth,
  ModelStats,
  ObservabilitySummary,
  Page,
  PageQuery,
  Report,
  Repository,
  RepositoryGroup,
  SkillView,
  Subject,
  Task,
  TaskRun,
  Tenant,
  AuditEvent,
  SkillCall,
  ModelCall,
  ModelTarget,
  ModelProviderSetting,
  ModelConfigPayload,
  ModelTestResult,
} from '@/types'

const API_BASE = import.meta.env.VITE_API_BASE ?? '/api/v1'

const STORAGE_KEY = 'codeagent.auth'

export interface AuthState {
  token: string
  apiKey: string
  subject: Subject | null
}

/** API 错误（携带业务码）。 */
export class ApiError extends Error {
  code: number
  requestId?: string

  constructor(code: number, message: string, requestId?: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.requestId = requestId
  }

  /** 是否为认证类错误。 */
  get isAuth(): boolean {
    return this.code === 401
  }
}

let authState: AuthState = loadAuth()

function loadAuth(): AuthState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) {
      const parsed = JSON.parse(raw) as AuthState
      return { token: parsed.token ?? '', apiKey: parsed.apiKey ?? '', subject: parsed.subject ?? null }
    }
  } catch {
    /* 忽略解析失败 */
  }
  return { token: '', apiKey: '', subject: null }
}

/** 读取当前认证状态。 */
export function getAuth(): AuthState {
  return authState
}

/** 写入认证状态并持久化。 */
export function setAuth(next: Partial<AuthState>): void {
  authState = { ...authState, ...next }
  localStorage.setItem(STORAGE_KEY, JSON.stringify(authState))
}

/** 清空认证状态。 */
export function clearAuth(): void {
  authState = { token: '', apiKey: '', subject: null }
  localStorage.removeItem(STORAGE_KEY)
}

function buildHeaders(json = true): HeadersInit {
  const headers: Record<string, string> = {}
  if (json) headers['Content-Type'] = 'application/json'
  if (authState.apiKey) {
    headers['X-API-Key'] = authState.apiKey
  } else if (authState.token) {
    headers['Authorization'] = `Bearer ${authState.token}`
  }
  return headers
}

function buildQuery(params?: Record<string, unknown>): string {
  if (!params) return ''
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ''
}

async function request<T>(method: string, path: string, body?: unknown, params?: Record<string, unknown>): Promise<T> {
  const res = await fetch(`${API_BASE}${path}${buildQuery(params)}`, {
    method,
    headers: buildHeaders(body !== undefined),
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  const text = await res.text()
  let env: Envelope<T> | null = null
  if (text) {
    try {
      env = JSON.parse(text) as Envelope<T>
    } catch {
      throw new ApiError(res.status, `响应不是合法 JSON（HTTP ${res.status}）`)
    }
  }

  if (!env) {
    if (!res.ok) throw new ApiError(res.status, `HTTP ${res.status}`)
    return undefined as T
  }
  if (env.code !== 0) {
    throw new ApiError(env.code, env.message || `请求失败（code=${env.code}）`, env.requestId)
  }
  return env.data
}

export const http = {
  get: <T>(path: string, params?: Record<string, unknown>) => request<T>('GET', path, undefined, params),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  del: <T>(path: string) => request<T>('DELETE', path),
}

/**
 * 解包后端 `{ items, total }` 形式的列表响应。
 *
 * 背景：服务端部分列表接口（/tenants、/api-keys、/credentials、/skills）在业务信封 data 里再包一层
 * `{ items: [...], total: n }`，而不是直接返回数组。若前端按数组消费，`Array.isArray` 判定为 false，
 * 页面会静默渲染空列表（且不报错），典型表现就是"创建成功但列表没数据"。
 * 这里统一对齐到数组，避免每个页面各写一份判断。
 */
type ItemsPayload<T> = { items?: T[] | null; total?: number }

function unwrapItems<T>(data: T[] | ItemsPayload<T> | null | undefined): T[] {
  if (Array.isArray(data)) return data
  const items = (data as ItemsPayload<T> | null | undefined)?.items
  return Array.isArray(items) ? items : []
}

/* ------------------------------ 各资源 API ------------------------------ */

export const api = {
  /* 系统 */
  healthz: () => http.get<{ status: string; version: string; uptime: number; time: string }>('/healthz'),
  readyz: () => http.get<HealthItem[]>('/readyz'),

  /* 认证与租户 */
  login: (tenantKey: string, username: string, password: string) =>
    http.post<{ token: string; subject: Subject }>('/auth/login', { tenantKey, username, password }),
  profile: () => http.get<Subject>('/auth/profile'),
  listTenants: async () => unwrapItems<Tenant>(await http.get<Tenant[] | ItemsPayload<Tenant>>('/tenants')),
  createTenant: (body: Partial<Tenant>) => http.post<Tenant>('/tenants', body),
  currentTenant: () => http.get<Tenant>('/tenants/current'),
  updateCurrentTenant: (body: Partial<Tenant>) => http.put<Tenant>('/tenants/current', body),
  listAPIKeys: async () => unwrapItems<APIKey>(await http.get<APIKey[] | ItemsPayload<APIKey>>('/api-keys')),
  createAPIKey: (body: { name: string; scopes: string[]; ttlHours?: number }) =>
    http.post<{ plainKey: string; apiKey: APIKey }>('/api-keys', body),
  revokeAPIKey: (id: string) => http.del<{ revoked: boolean }>(`/api-keys/${id}`),
  listCredentials: async () =>
    unwrapItems<Credential>(await http.get<Credential[] | ItemsPayload<Credential>>('/credentials')),
  createCredential: (body: { name: string; type: string; username?: string; secret: string }) =>
    http.post<Credential>('/credentials', body),
  deleteCredential: (id: string) => http.del<{ deleted: boolean }>(`/credentials/${id}`),

  /* 仓库 */
  listRepos: (q?: PageQuery) => http.get<Page<Repository>>('/repos', q),
  getRepo: (id: string) => http.get<Repository & { groups?: RepositoryGroup[] }>(`/repos/${id}`),
  createRepo: (body: Partial<Repository>) => http.post<Repository>('/repos', body),
  updateRepo: (id: string, body: Partial<Repository>) => http.put<Repository>(`/repos/${id}`, body),
  deleteRepo: (id: string) => http.del<{ deleted: boolean }>(`/repos/${id}`),
  probeRepo: (id: string, ref?: string) =>
    http.post<{ ok: boolean; commit: string; ref: string; durationMs: number; message?: string }>(
      `/repos/${id}/probe`,
      { ref },
    ),

  /* 分组 */
  listGroups: (q?: PageQuery) => http.get<Page<RepositoryGroup>>('/groups', q),
  getGroup: (id: string) =>
    http.get<{ group: RepositoryGroup; members: GroupMember[] }>(`/groups/${id}`),
  createGroup: (body: { group: Partial<RepositoryGroup>; members: { repositoryId: string; order: number }[] }) =>
    http.post<RepositoryGroup>('/groups', body),
  updateGroup: (
    id: string,
    body: { group: Partial<RepositoryGroup>; members: { repositoryId: string; order: number }[] },
  ) => http.put<RepositoryGroup>(`/groups/${id}`, body),
  deleteGroup: (id: string) => http.del<{ deleted: boolean }>(`/groups/${id}`),
  groupImpact: (id: string) => http.get<GroupImpact>(`/groups/${id}/impact`),

  /* 任务 */
  createTask: (body: CreateTaskRequest) => http.post<TaskRun>('/tasks', body),
  listTasks: (q?: PageQuery) => http.get<Page<Task>>('/tasks', q),
  getTask: (id: string) => http.get<Task & { latestRun?: TaskRun }>(`/tasks/${id}`),
  listRuns: (q?: PageQuery) => http.get<Page<TaskRun>>('/runs', q),
  getRun: (runId: string, includeCode = false) =>
    http.get<TaskRun>(`/runs/${runId}`, { includeCode: includeCode ? 1 : undefined }),
  cancelRun: (runId: string) => http.post<{ cancelled: boolean }>(`/runs/${runId}/cancel`),
  retryRun: (runId: string) => http.post<TaskRun>(`/runs/${runId}/retry`),
  feedbackRun: (runId: string, feedback: string) => http.post<TaskRun>(`/runs/${runId}/feedback`, { feedback }),
  runSkillCalls: (runId: string, q?: PageQuery) => http.get<Page<SkillCall>>(`/runs/${runId}/skill-calls`, q),
  runModelCalls: (runId: string, q?: PageQuery) => http.get<Page<ModelCall>>(`/runs/${runId}/model-calls`, q),
  runEvents: (runId: string) => http.get<AgentEvent[]>(`/runs/${runId}/events`),

  /* 报告 */
  listReports: (q?: PageQuery) => http.get<Page<Report>>('/reports', q),
  getReport: (id: string) => http.get<Report>(`/reports/${id}`),
  reportMarkdownURL: (id: string) => `${API_BASE}/reports/${id}/markdown`,

  /* 技能 */
  listSkills: async (category?: string) =>
    unwrapItems<SkillView>(await http.get<SkillView[] | ItemsPayload<SkillView>>('/skills', { category })),
  getSkill: (name: string) => http.get<SkillView>(`/skills/${name}`),
  setSkillStatus: (name: string, body: { version?: string; status: string; canaryPercent?: number }) =>
    http.post<{ updated: boolean }>(`/skills/${name}/status`, body),
  rollbackSkill: (name: string) => http.post<{ version: string }>(`/skills/${name}/rollback`),

  /* 模型 */
  listModels: () =>
    http.get<{ enabled: boolean; targets: ModelTarget[]; health: ModelHealth[] }>('/models'),
  modelStats: () => http.get<ModelStats>('/models/stats'),
  getModelConfig: () =>
    http.get<{ enabled: boolean; secretReady: boolean; hotReload: boolean; items: ModelProviderSetting[] }>(
      '/models/config',
    ),
  updateModelConfig: (body: ModelConfigPayload) =>
    http.put<{ items: ModelProviderSetting[]; loaded: number; skipped: number }>('/models/config', body),
  testModelProvider: (body: {
    name: string
    kind: string
    baseUrl: string
    apiKey?: string
    models: string[]
    testModel?: string
  }) => http.post<ModelTestResult>('/models/test', body),

  /* 可观测 */
  observability: () => http.get<ObservabilitySummary>('/observability/summary'),
  audits: (q?: PageQuery) => http.get<Page<AuditEvent>>('/observability/audits', q),
  engineStats: () => http.get<EngineStats>('/tasks/stats'),
}

/* ------------------------------ 实时连接 ------------------------------ */

/** 订阅指定运行的事件流（SSE）。返回取消函数。 */
export function subscribeRunEvents(runId: string, onEvent: (ev: AgentEvent) => void, onError?: (e: unknown) => void): () => void {
  const url = `${API_BASE}/runs/${runId}/events`
  // EventSource 无法自定义请求头，控制台使用 token 查询参数（后端同时支持）。
  const token = authState.token || authState.apiKey
  const src = new EventSource(`${url}?token=${encodeURIComponent(token)}`)
  src.onmessage = (e: MessageEvent<string>) => {
    try {
      onEvent(JSON.parse(e.data) as AgentEvent)
    } catch (err) {
      onError?.(err)
    }
  }
  src.onerror = (e) => onError?.(e)
  return () => src.close()
}

/** 建立全局事件 WebSocket。返回关闭函数。 */
export function connectEventStream(onEvent: (ev: AgentEvent) => void): () => void {
  const token = authState.token || authState.apiKey
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const ws = new WebSocket(`${proto}://${window.location.host}${API_BASE}/ws?token=${encodeURIComponent(token)}`)
  ws.onmessage = (e) => {
    try {
      const data = JSON.parse(e.data as string) as AgentEvent
      onEvent(data)
    } catch {
      /* 忽略非 JSON 消息 */
    }
  }
  ws.onerror = () => {
    /* 断线由浏览器自动重连策略外部处理 */
  }
  return () => ws.close()
}
