<script setup lang="ts">
/**
 * 租户与权限（第二层）。
 *
 * 多租户资源隔离 · Git 凭证加密管控 · 源码访问鉴权 · 敏感代码脱敏。
 * 四个数据源（当前租户 / 租户列表 / 接入密钥 / Git 凭证）各自独立降级。
 */
import { onMounted, reactive, ref } from 'vue'
import { ApiError, api } from '@/api/client'
import type { APIKey, ApiKeyCreateBody, ApiKeyCreateResult, Credential, CredentialType, Tenant, TenantQuota, TenantStatus } from '@/types'
import { downloadText, formatCompact, formatNumber, formatRelative, formatTime } from '@/utils/format'
import { toastOk } from '@/composables/useToast'
import { useAuthStore } from '@/stores/auth'
import StatusBadge from '@/components/StatusBadge.vue'
import AppModal from '@/components/AppModal.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'

const auth = useAuthStore()

/* ------------------------------ 通用 ------------------------------ */

const forbidden = ref(false)

/** 统一错误文案；403 时点亮「需要 admin:all 权限」提示。 */
function handleError(e: unknown, fallback: string): string {
  if (e instanceof ApiError && e.code === 403) {
    forbidden.value = true
    return '需要 admin:all 权限'
  }
  if (e instanceof ApiError) return `业务码 ${e.code}：${e.message}`
  return e instanceof Error ? e.message : fallback
}

/* ------------------------------ 当前租户 ------------------------------ */

const tenant = ref<Tenant | null>(null)
const tenantError = ref('')
const tenantLoading = ref(false)

async function loadTenant(): Promise<void> {
  tenantLoading.value = true
  try {
    tenant.value = await api.currentTenant()
    tenantError.value = ''
  } catch (e) {
    tenantError.value = handleError(e, '当前租户加载失败')
  } finally {
    tenantLoading.value = false
  }
}

function tenantTone(status?: TenantStatus | string): string {
  if (status === 'active') return 'ok'
  if (status === 'suspended') return 'warn'
  return 'muted'
}

function tenantStatusLabel(status?: TenantStatus | string): string {
  if (status === 'active') return '正常'
  if (status === 'suspended') return '已暂停'
  return status || '未知'
}

/* ------------------------------ 配额编辑 ------------------------------ */

const quotaOpen = ref(false)
const quotaSaving = ref(false)
const quotaError = ref('')
const quotaForm = reactive<TenantQuota>({
  maxConcurrentTasks: 4,
  maxFilesPerTask: 200,
  maxCodeChars: 600000,
  maxTokensPerTask: 200000,
  maxTaskSeconds: 600,
})

const QUOTA_FIELDS: { key: keyof TenantQuota; label: string; hint: string; unit?: string }[] = [
  {
    key: 'maxConcurrentTasks',
    label: '并发任务上限 maxConcurrentTasks',
    hint: '同一租户同时执行的任务数上限，超出后新任务排队而非失败。',
  },
  {
    key: 'maxFilesPerTask',
    label: '单任务最大解析文件数 maxFilesPerTask',
    hint: '单次任务最多拉取解析的文件数量，超限时按相关性优先级截断。',
  },
  {
    key: 'maxCodeChars',
    label: '单任务代码字符上限 maxCodeChars',
    hint: '送模型的代码总字符预算，超限时自动裁剪并只保留命中调用链的片段。',
  },
  {
    key: 'maxTokensPerTask',
    label: '单任务 Token 上限 maxTokensPerTask',
    hint: '单次任务的模型 Token 预算，接近上限时降级到轻量模型。',
  },
  {
    key: 'maxTaskSeconds',
    label: '单任务超时秒数 maxTaskSeconds',
    hint: '单次任务的总执行时长上限，超时转为已降级结果而非直接失败。',
  },
]

function openQuota(): void {
  const q = tenant.value?.quota
  if (q) {
    quotaForm.maxConcurrentTasks = q.maxConcurrentTasks ?? quotaForm.maxConcurrentTasks
    quotaForm.maxFilesPerTask = q.maxFilesPerTask ?? quotaForm.maxFilesPerTask
    quotaForm.maxCodeChars = q.maxCodeChars ?? quotaForm.maxCodeChars
    quotaForm.maxTokensPerTask = q.maxTokensPerTask ?? quotaForm.maxTokensPerTask
    quotaForm.maxTaskSeconds = q.maxTaskSeconds ?? quotaForm.maxTaskSeconds
  }
  quotaError.value = ''
  quotaOpen.value = true
}

function quotaValid(): boolean {
  for (const f of QUOTA_FIELDS) {
    const v = Number(quotaForm[f.key])
    if (!Number.isFinite(v) || v <= 0) {
      quotaError.value = `「${f.label}」必须是大于 0 的数值`
      return false
    }
  }
  return true
}

async function submitQuota(): Promise<void> {
  if (!quotaValid()) return
  quotaSaving.value = true
  quotaError.value = ''
  try {
    const updated = await api.updateCurrentTenant({ quota: { ...quotaForm } })
    if (updated) tenant.value = updated
    await auth.loadTenant()
    quotaOpen.value = false
    toastOk('租户配额已更新')
  } catch (e) {
    quotaError.value = handleError(e, '配额更新失败')
  } finally {
    quotaSaving.value = false
  }
}

/* ------------------------------ 租户列表 ------------------------------ */

const tenants = ref<Tenant[]>([])
const tenantsError = ref('')
const tenantsLoading = ref(false)

async function loadTenants(): Promise<void> {
  tenantsLoading.value = true
  try {
    tenants.value = (await api.listTenants()) ?? []
    tenantsError.value = ''
  } catch (e) {
    tenantsError.value = handleError(e, '租户列表加载失败')
  } finally {
    tenantsLoading.value = false
  }
}

function quotaSummary(q?: TenantQuota): string {
  if (!q) return '-'
  return [
    `并发 ${formatNumber(q.maxConcurrentTasks)}`,
    `文件 ${formatNumber(q.maxFilesPerTask)}`,
    `字符 ${formatCompact(q.maxCodeChars)}`,
    `Token ${formatCompact(q.maxTokensPerTask)}`,
    `超时 ${formatNumber(q.maxTaskSeconds)}s`,
  ].join(' · ')
}

const tenantOpen = ref(false)
const tenantSaving = ref(false)
const tenantFormError = ref('')
const tenantForm = reactive({ name: '', id: '', description: '', status: 'active' as TenantStatus })

function openTenantModal(): void {
  tenantForm.name = ''
  tenantForm.id = ''
  tenantForm.description = ''
  tenantForm.status = 'active'
  tenantFormError.value = ''
  tenantOpen.value = true
}

async function submitTenant(): Promise<void> {
  if (!tenantForm.name.trim()) {
    tenantFormError.value = '租户名称为必填项'
    return
  }
  tenantSaving.value = true
  tenantFormError.value = ''
  try {
    const body: Partial<Tenant> = {
      name: tenantForm.name.trim(),
      description: tenantForm.description.trim() || undefined,
      status: tenantForm.status,
    }
    if (tenantForm.id.trim()) body.id = tenantForm.id.trim()
    await api.createTenant(body)
    tenantOpen.value = false
    toastOk('租户已创建')
    await loadTenants()
  } catch (e) {
    tenantFormError.value = handleError(e, '租户创建失败')
  } finally {
    tenantSaving.value = false
  }
}

/* ------------------------------ 接入密钥 ------------------------------ */

const keys = ref<APIKey[]>([])
const keysError = ref('')
const keysLoading = ref(false)

async function loadKeys(): Promise<void> {
  keysLoading.value = true
  try {
    keys.value = (await api.listAPIKeys()) ?? []
    keysError.value = ''
  } catch (e) {
    keysError.value = handleError(e, '接入密钥加载失败')
  } finally {
    keysLoading.value = false
  }
}

const SCOPE_OPTIONS: { value: string; label: string; hint: string }[] = [
  { value: 'task:write', label: 'task:write', hint: '提交排查任务、上报堆栈与日志' },
  { value: 'repo:read', label: 'repo:read', hint: '读取仓库元数据与源码' },
  { value: 'group:read', label: 'group:read', hint: '读取业务分组与依赖关系' },
  { value: 'admin:all', label: 'admin:all', hint: '租户管理、密钥与凭证等全部权限' },
]

const TTL_OPTIONS: { value: number; label: string }[] = [
  { value: 24, label: '24 小时' },
  { value: 168, label: '7 天（168 小时）' },
  { value: 720, label: '30 天（720 小时）' },
  { value: 0, label: '永久有效' },
]

const keyOpen = ref(false)
const keySaving = ref(false)
const keyError = ref('')
const keyForm = reactive({ name: '', scopes: ['task:write'] as string[], ttlHours: 720, callbackEnabled: false, callbackHosts: '' })
const createdKey = ref<ApiKeyCreateResult | null>(null)

function openKeyModal(): void {
  keyForm.name = ''
  keyForm.scopes = ['task:write']
  keyForm.ttlHours = 720
  keyForm.callbackEnabled = false
  keyForm.callbackHosts = ''
  keyError.value = ''
  createdKey.value = null
  keyOpen.value = true
}

async function submitKey(): Promise<void> {
  if (!keyForm.name.trim()) {
    keyError.value = '密钥名称为必填项'
    return
  }
  if (!keyForm.scopes.length) {
    keyError.value = '至少选择一个 scope'
    return
  }
  keySaving.value = true
  keyError.value = ''
  try {
    const body: ApiKeyCreateBody = {
      name: keyForm.name.trim(),
      scopes: [...keyForm.scopes],
      callbackEnabled: keyForm.callbackEnabled,
      callbackHosts: keyForm.callbackHosts
        ? keyForm.callbackHosts.split(',').map((s: string) => s.trim()).filter(Boolean)
        : [],
    }
    if (keyForm.ttlHours > 0) body.ttlHours = keyForm.ttlHours
    const res = await api.createAPIKey(body)
    createdKey.value = res ?? null
    toastOk('接入密钥已创建', '明文仅显示一次，请立即保存')
    await loadKeys()
  } catch (e) {
    keyError.value = handleError(e, '密钥创建失败')
  } finally {
    keySaving.value = false
  }
}

function downloadPlainKey(): void {
  const created = createdKey.value
  if (!created) return
  const k = created.apiKey
  const lines = [
    'CodeAgent 接入层 API Key（明文仅显示一次）',
    '------------------------------------------',
    `名称：${k?.name ?? keyForm.name}`,
    `Key：${created.plainKey}`,
    `Key 前缀：${k?.keyPrefix ?? '-'}`,
    `Scopes：${(k?.scopes ?? keyForm.scopes).join(', ')}`,
    `租户：${k?.tenantId ?? tenant.value?.id ?? '-'}`,
    `创建时间：${formatTime(k?.createdAt)}`,
    `过期时间：${k?.expiresAt ? formatTime(k.expiresAt) : '永久有效'}`,
    '',
    '安全提示：服务端只保存哈希，关闭弹窗后无法再次查看明文，请立即妥善保存。',
  ]
  downloadText(`codeagent-apikey-${(k?.name ?? keyForm.name) || 'key'}.txt`, lines.join('\n'))
}

function keyStatus(k: APIKey): { label: string; tone: string } {
  if (k?.revoked) return { label: '已吊销', tone: 'error' }
  if (k?.expiresAt && new Date(k.expiresAt).getTime() < Date.now()) return { label: '已过期', tone: 'warn' }
  return { label: '有效', tone: 'ok' }
}

const revokeTarget = ref<APIKey | null>(null)
const revoking = ref(false)
const revokeError = ref('')

function askRevoke(k: APIKey): void {
  revokeError.value = ''
  revokeTarget.value = k
}

async function confirmRevoke(): Promise<void> {
  const target = revokeTarget.value
  if (!target) return
  revoking.value = true
  revokeError.value = ''
  try {
    await api.revokeAPIKey(target.id)
    revokeTarget.value = null
    toastOk('密钥已吊销', target.keyPrefix)
    await loadKeys()
  } catch (e) {
    revokeError.value = handleError(e, '吊销失败')
  } finally {
    revoking.value = false
  }
}

/* ------------------------------ Git 凭证 ------------------------------ */

const creds = ref<Credential[]>([])
const credsError = ref('')
const credsLoading = ref(false)

async function loadCreds(): Promise<void> {
  credsLoading.value = true
  try {
    creds.value = (await api.listCredentials()) ?? []
    credsError.value = ''
  } catch (e) {
    credsError.value = handleError(e, 'Git 凭证加载失败')
  } finally {
    credsLoading.value = false
  }
}

const CRED_TYPES: { value: CredentialType; label: string; hint: string }[] = [
  { value: 'ssh_key', label: 'SSH 密钥', hint: '部署密钥 / 私钥，用于 git@ 形式的 SSH 克隆' },
  { value: 'token', label: '访问令牌', hint: 'GitHub / GitLab / Gitee 个人访问令牌或项目令牌' },
  { value: 'basic', label: '用户名密码', hint: 'HTTPS 基础认证，需同时填写用户名' },
  { value: 'none', label: '无（公开仓库）', hint: '公开仓库无需密钥，仅登记凭证名称即可' },
]

function credTypeLabel(t?: CredentialType | string): string {
  const hit = CRED_TYPES.find((c) => c.value === t)
  return hit?.label ?? t ?? '-'
}

function credTypeTone(t?: CredentialType | string): string {
  if (t === 'ssh_key') return 'info'
  if (t === 'token') return 'ok'
  if (t === 'basic') return 'warn'
  return 'muted'
}

function credHint(t: CredentialType): string {
  return CRED_TYPES.find((c) => c.value === t)?.hint ?? ''
}

const credOpen = ref(false)
const credSaving = ref(false)
const credError = ref('')
const credForm = reactive({ name: '', type: 'token' as CredentialType, username: '', secret: '' })

function openCredModal(): void {
  credForm.name = ''
  credForm.type = 'token'
  credForm.username = ''
  credForm.secret = ''
  credError.value = ''
  credOpen.value = true
}

async function submitCred(): Promise<void> {
  if (!credForm.name.trim()) {
    credError.value = '凭证名称为必填项'
    return
  }
  if (credForm.type !== 'none' && !credForm.secret.trim()) {
    credError.value = '密钥明文为必填项'
    return
  }
  credSaving.value = true
  credError.value = ''
  try {
    await api.createCredential({
      name: credForm.name.trim(),
      type: credForm.type,
      username: credForm.username.trim() || undefined,
      secret: credForm.secret,
    })
    credOpen.value = false
    credForm.secret = ''
    toastOk('凭证已加密保存')
    await loadCreds()
  } catch (e) {
    credError.value = handleError(e, '凭证创建失败')
  } finally {
    credSaving.value = false
  }
}

const deleteTarget = ref<Credential | null>(null)
const deleting = ref(false)
const deleteError = ref('')

function askDelete(c: Credential): void {
  deleteError.value = ''
  deleteTarget.value = c
}

async function confirmDelete(): Promise<void> {
  const target = deleteTarget.value
  if (!target) return
  deleting.value = true
  deleteError.value = ''
  try {
    await api.deleteCredential(target.id)
    deleteTarget.value = null
    toastOk('凭证已删除', target.name)
    await loadCreds()
  } catch (e) {
    deleteError.value = handleError(e, '删除失败')
  } finally {
    deleting.value = false
  }
}

/* ------------------------------ 生命周期 ------------------------------ */

onMounted(() => {
  void loadTenant()
  void loadTenants()
  void loadKeys()
  void loadCreds()
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">租户与权限</h1>
        <p class="page-subtitle">多租户资源隔离 · Git 凭证加密管控 · 源码访问鉴权 · 敏感代码脱敏</p>
      </div>
      <div class="row wrap">
        <button class="btn" :disabled="tenantLoading" @click="loadTenant()">
          {{ tenantLoading ? '刷新中…' : '刷新租户' }}
        </button>
      </div>
    </div>

    <div v-if="forbidden" class="alert alert-error">需要 admin:all 权限</div>

    <!-- 当前租户 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">当前租户</h3>
        <div class="row">
          <StatusBadge
            :label="tenantStatusLabel(tenant?.status)"
            :tone="tenantTone(tenant?.status)"
            :pulse="tenant?.status === 'active'"
          />
          <button class="btn btn-sm" :disabled="!tenant" @click="openQuota()">编辑配额</button>
        </div>
      </div>
      <div class="card-body">
        <div v-if="tenantError" class="alert alert-warn">{{ tenantError }}</div>
        <div v-else-if="tenantLoading && !tenant" class="skeleton" style="height: 92px" />
        <EmptyState v-else-if="!tenant" title="暂无当前租户信息" hint="请检查登录态或接入层 /tenants/current 接口" />
        <dl v-else class="kv">
          <dt>租户名称</dt>
          <dd class="strong">{{ tenant.name }}</dd>
          <dt>租户 ID</dt>
          <dd class="row" style="gap: 8px">
            <span class="mono ellipsis">{{ tenant.id }}</span>
            <CopyButton :text="tenant.id" label="复制 ID" title="复制租户 ID" />
          </dd>
          <dt>状态</dt>
          <dd>{{ tenantStatusLabel(tenant.status) }}</dd>
          <dt>描述</dt>
          <dd>{{ tenant.description || '-' }}</dd>
          <dt>创建时间</dt>
          <dd>{{ formatTime(tenant.createdAt) }} <span class="small faint">（{{ formatRelative(tenant.createdAt) }}）</span></dd>
          <dt>配额</dt>
          <dd>{{ quotaSummary(tenant.quota) }}</dd>
        </dl>
      </div>
    </div>

    <!-- 租户列表 -->
    <div class="card">
      <div class="card-head">
        <div>
          <h3 class="card-title">租户列表</h3>
          <div class="small muted" style="margin-top: 2px">跨租户完全隔离：仓库、分组、任务、密钥、凭证均归属单一租户</div>
        </div>
        <button class="btn btn-sm btn-primary" @click="openTenantModal()">新建租户</button>
      </div>
      <div v-if="tenantsError" class="card-body tight">
        <div class="alert alert-warn">{{ tenantsError }}</div>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>ID</th>
              <th>状态</th>
              <th>配额摘要</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in tenants" :key="t.id">
              <td class="strong">{{ t.name }}</td>
              <td class="mono ellipsis" style="max-width: 220px">{{ t.id }}</td>
              <td>
                <StatusBadge :label="tenantStatusLabel(t.status)" :tone="tenantTone(t.status)" />
              </td>
              <td class="small muted">{{ quotaSummary(t.quota) }}</td>
              <td class="nowrap">{{ formatTime(t.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
        <EmptyState
          v-if="!tenants.length && !tenantsLoading"
          title="暂无租户"
          hint="可点击右上角「新建租户」创建，用于验证跨租户隔离"
        />
      </div>
    </div>

    <!-- 接入密钥 -->
    <div class="card">
      <div class="card-head">
        <div>
          <h3 class="card-title">接入密钥</h3>
          <div class="small muted" style="margin-top: 2px">供 IDE / 告警平台 / CI 流水线调用接入层使用</div>
        </div>
        <button class="btn btn-sm btn-primary" @click="openKeyModal()">创建密钥</button>
      </div>
      <div v-if="keysError" class="card-body tight">
        <div class="alert alert-warn">{{ keysError }}</div>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>Key 前缀</th>
              <th>Scopes</th>
              <th>最近使用</th>
              <th>过期时间</th>
              <th>状态</th>
              <th style="text-align: right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in keys" :key="k.id">
              <td class="strong">{{ k.name }}</td>
              <td class="mono">{{ k.keyPrefix || '-' }}</td>
              <td>
                <span class="row wrap" style="gap: 4px">
                  <span v-for="s in k.scopes ?? []" :key="s" class="badge badge-info">{{ s }}</span>
                  <span v-if="!(k.scopes ?? []).length" class="muted small">-</span>
                </span>
              </td>
              <td class="small">{{ k.lastUsedAt ? formatRelative(k.lastUsedAt) : '从未使用' }}</td>
              <td class="small nowrap">{{ k.expiresAt ? formatTime(k.expiresAt) : '永久有效' }}</td>
              <td>
                <StatusBadge :label="keyStatus(k).label" :tone="keyStatus(k).tone" />
                <span v-if="k.callbackEnabled" class="badge bg-info-soft text-info me-1">回调·HMAC</span>
              </td>
              <td style="text-align: right">
                <button class="btn btn-sm btn-danger" :disabled="k.revoked" @click="askRevoke(k)">
                  {{ k.revoked ? '已吊销' : '吊销' }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState
          v-if="!keys.length && !keysLoading"
          title="暂无接入密钥"
          hint="创建后请立即保存一次性明文 Key"
        />
      </div>
    </div>

    <!-- Git 凭证 -->
    <div class="card">
      <div class="card-head">
        <div>
          <h3 class="card-title">Git 凭证</h3>
          <div class="small muted" style="margin-top: 2px">密钥以 AES-GCM 加密存储，接口与日志中永不回显明文</div>
        </div>
        <button class="btn btn-sm btn-primary" @click="openCredModal()">新增凭证</button>
      </div>
      <div v-if="credsError" class="card-body tight">
        <div class="alert alert-warn">{{ credsError }}</div>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>类型</th>
              <th>用户名</th>
              <th>密钥</th>
              <th>创建时间</th>
              <th style="text-align: right">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in creds" :key="c.id">
              <td class="strong">{{ c.name }}</td>
              <td>
                <StatusBadge :label="credTypeLabel(c.type)" :tone="credTypeTone(c.type)" />
              </td>
              <td class="mono small">{{ c.username || '-' }}</td>
              <td>
                <StatusBadge
                  :label="c.hasSecret ? '已配置密钥' : '无密钥'"
                  :tone="c.hasSecret ? 'ok' : 'muted'"
                />
                <span class="small faint" style="margin-left: 6px">密文不可查看</span>
              </td>
              <td class="nowrap">{{ formatTime(c.createdAt) }}</td>
              <td style="text-align: right">
                <button class="btn btn-sm btn-danger" @click="askDelete(c)">删除</button>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState
          v-if="!creds.length && !credsLoading"
          title="暂无 Git 凭证"
          hint="私有仓库需要先登记 SSH 密钥或访问令牌"
        />
      </div>
    </div>

    <!-- 安全能力说明 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">安全能力基线（验收项）</h3>
        <StatusBadge label="四项基线" tone="ok" />
      </div>
      <div class="card-body">
        <dl class="kv">
          <dt>租户隔离</dt>
          <dd>所有仓库、分组、任务、密钥与凭证均归属唯一租户；跨租户读写一律拒绝，列表查询自动按租户过滤。</dd>
          <dt>凭证安全</dt>
          <dd>Git 密钥以 AES-GCM 加密存储，列表与详情接口只返回 hasSecret 标记，明文既不出现在响应中，也不写入任何日志。</dd>
          <dt>访问鉴权</dt>
          <dd>每次任务执行前校验仓库访问权限（scopes + 租户归属），防止越权读取私有代码。</dd>
          <dt>源码脱敏</dt>
          <dd>源码送模型前自动清除密钥、AK/SK、密码等高危字面量，命中即替换为占位符。</dd>
        </dl>
        <div class="alert alert-ok" style="margin-top: 14px">
          上述四项为安全验收项：可通过创建第二个租户、吊销密钥、删除凭证等操作现场验证隔离与鉴权效果。
        </div>
      </div>
    </div>

    <!-- 配额编辑弹窗 -->
    <AppModal v-if="quotaOpen" title="编辑租户配额" @close="quotaOpen = false">
      <div class="field-hint" style="margin-bottom: 12px">
        资源配额管控，超限时自动降级而非失败。
      </div>
      <div v-for="f in QUOTA_FIELDS" :key="f.key" class="field" style="margin-bottom: 12px">
        <label class="field-label">{{ f.label }}<span class="req">*</span></label>
        <input v-model.number="quotaForm[f.key]" type="number" min="1" class="input" />
        <span class="field-hint">{{ f.hint }}</span>
      </div>
      <div v-if="quotaError" class="alert alert-error">{{ quotaError }}</div>
      <template #footer>
        <button class="btn" @click="quotaOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="quotaSaving" @click="submitQuota()">
          {{ quotaSaving ? '保存中…' : '保存配额' }}
        </button>
      </template>
    </AppModal>

    <!-- 新建租户弹窗 -->
    <AppModal v-if="tenantOpen" title="新建租户" @close="tenantOpen = false">
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">租户名称<span class="req">*</span></label>
        <input v-model="tenantForm.name" class="input" placeholder="例如：ACME 事业部" />
      </div>
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">租户 ID（可选）</label>
        <input v-model="tenantForm.id" class="input mono" placeholder="留空则由服务端生成，例如 acme" />
        <span class="field-hint">自定义 ID 便于跨租户隔离验证与脚本化调用。</span>
      </div>
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">描述</label>
        <input v-model="tenantForm.description" class="input" placeholder="用途说明（可选）" />
      </div>
      <div class="field">
        <label class="field-label">状态</label>
        <select v-model="tenantForm.status" class="select">
          <option value="active">正常 active</option>
          <option value="suspended">已暂停 suspended</option>
        </select>
      </div>
      <div v-if="tenantFormError" class="alert alert-error" style="margin-top: 12px">{{ tenantFormError }}</div>
      <template #footer>
        <button class="btn" @click="tenantOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="tenantSaving" @click="submitTenant()">
          {{ tenantSaving ? '创建中…' : '创建租户' }}
        </button>
      </template>
    </AppModal>

    <!-- 创建密钥弹窗 -->
    <AppModal
      v-if="keyOpen"
      title="创建接入密钥"
      :close-on-mask="!createdKey"
      @close="keyOpen = false"
    >
      <template v-if="!createdKey">
        <div class="field" style="margin-bottom: 12px">
          <label class="field-label">密钥名称<span class="req">*</span></label>
          <input v-model="keyForm.name" class="input" placeholder="例如：CI 流水线 / 告警平台" />
        </div>
        <div class="field" style="margin-bottom: 12px">
          <label class="field-label">Scopes<span class="req">*</span></label>
          <div style="display: flex; flex-direction: column; gap: 6px">
            <label v-for="s in SCOPE_OPTIONS" :key="s.value" class="checkbox">
              <input v-model="keyForm.scopes" type="checkbox" :value="s.value" />
              <span class="mono">{{ s.label }}</span>
              <span class="small faint">— {{ s.hint }}</span>
            </label>
          </div>
        </div>
        <div class="field">
          <label class="field-label">有效期</label>
          <select v-model.number="keyForm.ttlHours" class="select">
            <option v-for="t in TTL_OPTIONS" :key="t.value" :value="t.value">{{ t.label }}</option>
          </select>
          <span class="field-hint">到期后密钥自动失效，永久有效仅建议用于本地联调。</span>
        </div>

        <div class="field" style="margin-top: 12px">
          <label class="field-label">回调签名鉴权</label>
          <label class="scope-item">
            <input type="checkbox" v-model="keyForm.callbackEnabled" />
            <span>启用后，该密钥触发的任务终态回调将以 HMAC-SHA256 签名发送（请求头 <code>X-Callback-Signature</code>）</span>
          </label>
          <input
            v-if="keyForm.callbackEnabled"
            v-model="keyForm.callbackHosts"
            class="input mono"
            style="margin-top: 8px"
            placeholder="允许回调 Host 白名单，逗号分隔，如 hook.svc.com,*.inner.net"
          />
          <span v-if="keyForm.callbackEnabled" class="field-hint">留空表示不限制 Host（仅做内网/保留地址段 SSRF 防护）；建议显式填写对端域名。</span>
        </div>

        <div v-if="keyError" class="alert alert-error" style="margin-top: 12px">{{ keyError }}</div>
      </template>

      <template v-else>
        <div class="alert alert-warn" style="margin-bottom: 12px">
          明文仅显示一次，请立即保存，关闭后无法再次查看。
        </div>
        <div class="field" style="margin-bottom: 12px">
          <label class="field-label">明文 Key</label>
          <div class="row" style="gap: 8px; align-items: stretch">
            <code class="code mono" style="flex: 1; font-size: 15px; max-height: none; padding: 12px">{{ createdKey.plainKey }}</code>
            <CopyButton :text="createdKey.plainKey" label="复制" title="复制明文 Key" />
          </div>
        </div>

        <div v-if="createdKey.callbackSecret" class="field" style="margin-bottom: 12px">
          <label class="field-label text-warn">回调签名密钥（仅此一次）</label>
          <p class="text-warn small" style="margin: 0 0 6px">
            请将此密钥配置到对端服务的回调令牌校验处；本服务后续回调用 HMAC-SHA256 以请求头
            <code>X-Callback-Signature</code> 签名，服务端只保存密文、不保存明文。
          </p>
          <div class="row" style="gap: 8px; align-items: stretch">
            <code class="code mono" style="flex: 1; padding: 12px">{{ createdKey.callbackSecret }}</code>
            <CopyButton :text="createdKey.callbackSecret" label="复制" title="复制回调签名密钥" />
          </div>
        </div>

        <dl class="kv">
          <dt>名称</dt>
          <dd>{{ createdKey.apiKey?.name ?? keyForm.name }}</dd>
          <dt>Key 前缀</dt>
          <dd class="mono">{{ createdKey.apiKey?.keyPrefix ?? '-' }}</dd>
          <dt>Scopes</dt>
          <dd>{{ (createdKey.apiKey?.scopes ?? keyForm.scopes).join(', ') }}</dd>
          <dt>过期时间</dt>
          <dd>{{ createdKey.apiKey?.expiresAt ? formatTime(createdKey.apiKey.expiresAt) : '永久有效' }}</dd>
          <dt>回调鉴权</dt>
          <dd>{{ createdKey.apiKey?.callbackEnabled ? '已启用 · HMAC-SHA256' : '未启用' }}</dd>
          <template v-if="createdKey.apiKey?.callbackEnabled && createdKey.apiKey?.callbackHosts?.length">
            <dt>允许回调 Host</dt>
            <dd><code v-for="h in createdKey.apiKey.callbackHosts" :key="h" class="me-1">{{ h }}</code></dd>
          </template>
        </dl>
      </template>

      <template #footer>
        <template v-if="!createdKey">
          <button class="btn" @click="keyOpen = false">取消</button>
          <button class="btn btn-primary" :disabled="keySaving" @click="submitKey()">
            {{ keySaving ? '创建中…' : '创建密钥' }}
          </button>
        </template>
        <template v-else>
          <button class="btn" @click="downloadPlainKey()">下载 .txt</button>
          <button class="btn btn-primary" @click="keyOpen = false">我已保存，关闭</button>
        </template>
      </template>
    </AppModal>

    <!-- 吊销确认 -->
    <AppModal v-if="revokeTarget" title="确认吊销密钥" @close="revokeTarget = null">
      <div class="alert alert-warn">
        吊销后该密钥立即失效，使用它的 IDE / 流水线 / 告警平台将无法再调用接入层。此操作不可撤销。
      </div>
      <dl class="kv" style="margin-top: 12px">
        <dt>名称</dt>
        <dd class="strong">{{ revokeTarget.name }}</dd>
        <dt>Key 前缀</dt>
        <dd class="mono">{{ revokeTarget.keyPrefix || '-' }}</dd>
      </dl>
      <div v-if="revokeError" class="alert alert-error" style="margin-top: 12px">{{ revokeError }}</div>
      <template #footer>
        <button class="btn" @click="revokeTarget = null">取消</button>
        <button class="btn btn-danger" :disabled="revoking" @click="confirmRevoke()">
          {{ revoking ? '吊销中…' : '确认吊销' }}
        </button>
      </template>
    </AppModal>

    <!-- 新增凭证弹窗 -->
    <AppModal v-if="credOpen" title="新增 Git 凭证" @close="credOpen = false">
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">凭证名称<span class="req">*</span></label>
        <input v-model="credForm.name" class="input" placeholder="例如：GitLab 只读令牌" />
      </div>
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">类型</label>
        <select v-model="credForm.type" class="select">
          <option v-for="t in CRED_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
        </select>
        <span class="field-hint">{{ credHint(credForm.type) }}</span>
      </div>
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">用户名</label>
        <input v-model="credForm.username" class="input mono" placeholder="basic / token 认证时填写" />
      </div>
      <div v-if="credForm.type !== 'none'" class="field">
        <label class="field-label">密钥明文<span class="req">*</span></label>
        <textarea
          v-model="credForm.secret"
          class="textarea"
          placeholder="粘贴私钥或访问令牌，提交后不会再回显"
        />
        <span class="field-hint">密钥将以 AES-GCM 加密存储，接口与日志中永不回显明文。</span>
      </div>
      <div v-else class="field-hint">公开仓库无需密钥，仅登记凭证名称用于权限归属说明。</div>
      <div v-if="credError" class="alert alert-error" style="margin-top: 12px">{{ credError }}</div>
      <template #footer>
        <button class="btn" @click="credOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="credSaving" @click="submitCred()">
          {{ credSaving ? '保存中…' : '加密保存' }}
        </button>
      </template>
    </AppModal>

    <!-- 删除凭证确认 -->
    <AppModal v-if="deleteTarget" title="确认删除凭证" @close="deleteTarget = null">
      <div class="alert alert-warn">
        删除后引用该凭证的仓库将无法克隆私有代码，请先确认没有仓库正在使用。
      </div>
      <dl class="kv" style="margin-top: 12px">
        <dt>名称</dt>
        <dd class="strong">{{ deleteTarget.name }}</dd>
        <dt>类型</dt>
        <dd>{{ credTypeLabel(deleteTarget.type) }}</dd>
        <dt>密钥</dt>
        <dd>{{ deleteTarget.hasSecret ? '已配置密钥' : '无密钥' }}</dd>
      </dl>
      <div v-if="deleteError" class="alert alert-error" style="margin-top: 12px">{{ deleteError }}</div>
      <template #footer>
        <button class="btn" @click="deleteTarget = null">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="confirmDelete()">
          {{ deleting ? '删除中…' : '确认删除' }}
        </button>
      </template>
    </AppModal>
  </div>
</template>
