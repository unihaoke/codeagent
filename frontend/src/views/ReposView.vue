<script setup lang="ts">
/**
 * 代码源仓库（第三层：仓库分组索引层）。
 *
 * 仅维护仓库元数据与「堆栈驱动懒加载」的匹配索引：不存储源码、不预加载代码。
 * 所有接口失败（404/403/500）均降级为内联提示，不阻塞页面其它区域。
 */
import { computed, reactive, ref, watch } from 'vue'
import { ApiError, api } from '@/api/client'
import type { Credential, RepoLayer, RepoMatchRules, Repository, RepoStatus, VCSProvider } from '@/types'
import {
  formatDuration,
  formatNumber,
  formatRelative,
  formatTime,
  layerLabel,
  shortCommit,
  truncate,
} from '@/utils/format'
import { toastError, toastOk } from '@/composables/useToast'
import { useAsync } from '@/composables/useAsync'
import AppModal from '@/components/AppModal.vue'
import BarList from '@/components/BarList.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import Pagination from '@/components/Pagination.vue'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'

/* ------------------------------ 通用辅助 ------------------------------ */

function errText(e: unknown): string {
  if (e instanceof ApiError) return `${e.message}（code=${e.code}）`
  if (e instanceof Error) return e.message
  return String(e)
}

function actionError(action: string, e: unknown): string {
  return `${action}失败：${errText(e)}`
}

const PROVIDERS: { value: VCSProvider; label: string }[] = [
  { value: 'git', label: 'git（通用）' },
  { value: 'github', label: 'GitHub' },
  { value: 'gitlab', label: 'GitLab' },
  { value: 'gitee', label: 'Gitee' },
]

const LAYERS: { value: RepoLayer }[] = [
  { value: 'frontend' },
  { value: 'gateway' },
  { value: 'service' },
  { value: 'middleware' },
  { value: 'library' },
  { value: 'unknown' },
]

const STATUS_OPTIONS: { value: RepoStatus; label: string }[] = [
  { value: 'active', label: '启用' },
  { value: 'disabled', label: '停用' },
]

const LANGUAGES = ['java', 'go', 'typescript', 'javascript', 'python', 'php', 'csharp', 'rust', 'other']

const RULE_FIELDS: {
  key: keyof RepoMatchRules
  label: string
  hint: string
  placeholder: string
}[] = [
  {
    key: 'packagePrefixes',
    label: '包名前缀',
    hint: '堆栈中的包/命名空间匹配依据，命中即锁定该仓库。如 com.acme.order',
    placeholder: 'com.acme.order',
  },
  {
    key: 'pathPrefixes',
    label: '路径前缀',
    hint: '堆栈中的文件路径前缀，用于包名缺失时的兜底定位。如 src/main/java/com/acme/order',
    placeholder: 'src/main/java/com/acme/order',
  },
  {
    key: 'artifactNames',
    label: '产物名',
    hint: '构建产物 / 依赖坐标名称匹配。如 order-service.jar',
    placeholder: 'order-service.jar',
  },
  {
    key: 'hostPatterns',
    label: '主机 / 服务名',
    hint: '告警中的主机名、Pod 名或服务名匹配（支持前缀）。如 order-svc',
    placeholder: 'order-svc',
  },
  {
    key: 'endpointPatterns',
    label: '接口路径',
    hint: 'HTTP 接口路径匹配，支持 ** 通配，用于网关到微服务的链路推断。如 /api/order/**',
    placeholder: '/api/order/**',
  },
  {
    key: 'keywords',
    label: '兜底关键词',
    hint: '以上均未命中时使用的弱匹配关键词，仅作最后兜底。如 order',
    placeholder: 'order',
  },
]

function parseList(raw: string): string[] {
  return raw
    .split(/[\n,，;；]+/)
    .map((v) => v.trim())
    .filter((v) => v.length > 0)
}

function joinList(values?: string[]): string {
  return (values ?? []).join('\n')
}

function rulePreview(rules?: RepoMatchRules): string[] {
  const out: string[] = []
  for (const f of RULE_FIELDS) {
    for (const v of rules?.[f.key] ?? []) out.push(`${f.label}:${v}`)
  }
  return out
}

function ruleCount(rules?: RepoMatchRules): number {
  return RULE_FIELDS.reduce((n, f) => n + (rules?.[f.key]?.length ?? 0), 0)
}

function layerTone(layer?: string): string {
  switch (layer) {
    case 'frontend':
      return 'info'
    case 'gateway':
      return 'degraded'
    case 'service':
      return 'ok'
    case 'middleware':
      return 'warn'
    case 'library':
      return 'info'
    default:
      return 'muted'
  }
}

function statusLabel(status?: string): string {
  return status === 'disabled' ? '已停用' : '启用中'
}

/* ------------------------------ 数据加载 ------------------------------ */

const {
  data: repoPage,
  loading: reposLoading,
  error: reposError,
  reload: reloadRepos,
} = useAsync(() => api.listRepos({ page: 1, pageSize: 200 }), { silent: true })

const { data: credentialData, error: credentialError } = useAsync(() => api.listCredentials(), { silent: true })

/** 分组索引：listGroups + 逐分组详情成员，构造 repoId → 分组名 反查表。 */
const { data: groupIndex, error: groupIndexError, reload: reloadGroupIndex } = useAsync(async () => {
  const page = await api.listGroups({ page: 1, pageSize: 200 })
  const list = page?.items ?? []
  const settled = await Promise.allSettled(list.map((g) => api.getGroup(g.id)))
  const map: Record<string, string[]> = {}
  settled.forEach((res, i) => {
    const group = list[i]
    if (!group || res.status !== 'fulfilled') return
    for (const member of res.value?.members ?? []) {
      const repoId = member?.repositoryId
      if (!repoId) continue
      const names = map[repoId] ?? (map[repoId] = [])
      if (!names.includes(group.name)) names.push(group.name)
    }
  })
  return map
}, { silent: true })

const repos = computed<Repository[]>(() => repoPage.value?.items ?? [])
const credentials = computed<Credential[]>(() => credentialData.value ?? [])
const repoGroupMap = computed<Record<string, string[]>>(() => groupIndex.value ?? {})

function groupsOf(repoId: string): string[] {
  return repoGroupMap.value[repoId] ?? []
}

/* ------------------------------- 统计条 ------------------------------- */

const layerDist = computed<Record<string, number>>(() => {
  const acc: Record<string, number> = {}
  for (const r of repos.value) {
    const key = r?.layer ?? 'unknown'
    acc[key] = (acc[key] ?? 0) + 1
  }
  return acc
})

const layerNameMap = computed<Record<string, string>>(() => {
  const acc: Record<string, string> = {}
  for (const key of Object.keys(layerDist.value)) acc[key] = layerLabel(key)
  return acc
})

const languageDist = computed<Record<string, number>>(() => {
  const acc: Record<string, number> = {}
  for (const r of repos.value) {
    const key = (r?.language || 'unknown').toLowerCase()
    acc[key] = (acc[key] ?? 0) + 1
  }
  return acc
})

const credentialUsage = computed(() => {
  const bound = repos.value.filter((r) => Boolean(r?.credentialId)).length
  const distinct = new Set(repos.value.map((r) => r?.credentialId).filter(Boolean) as string[]).size
  return { bound, distinct, free: repos.value.length - bound }
})

/* ------------------------------- 筛选分页 ------------------------------- */

const keyword = ref('')
const layerFilter = ref('all')
const statusFilter = ref('all')

const filtered = computed<Repository[]>(() => {
  const kw = keyword.value.trim().toLowerCase()
  return repos.value.filter((r) => {
    if (!r) return false
    if (layerFilter.value !== 'all' && r.layer !== layerFilter.value) return false
    if (statusFilter.value !== 'all' && r.status !== statusFilter.value) return false
    if (!kw) return true
    return [r.name, r.key, r.url, r.language, r.defaultBranch].some((v) =>
      (v ?? '').toLowerCase().includes(kw),
    )
  })
})

const page = ref(1)
const pageSize = 10
const pagedRepos = computed<Repository[]>(() =>
  filtered.value.slice((page.value - 1) * pageSize, page.value * pageSize),
)

watch([keyword, layerFilter, statusFilter], () => {
  page.value = 1
})

function onPageChange(p: number) {
  page.value = p
}

/* ------------------------------- 探测 ------------------------------- */

const probeOpen = ref(false)
const probeTarget = ref<Repository | null>(null)
const probeRef = ref('')
const probing = ref(false)
const probeError = ref('')
const probeResult = ref<{ ok: boolean; commit: string; ref: string; durationMs: number; message?: string } | null>(null)

async function runProbe() {
  const target = probeTarget.value
  if (!target) return
  probing.value = true
  probeError.value = ''
  probeResult.value = null
  try {
    const res = await api.probeRepo(target.id, probeRef.value.trim() || undefined)
    probeResult.value = res ?? null
    if (res?.ok) {
      toastOk('探测成功', `${target.name} @ ${shortCommit(res.commit)}`)
    } else {
      toastError('探测未通过', res?.message ?? '远程仓库返回异常')
    }
  } catch (e) {
    probeError.value = errText(e)
    toastError('探测失败', probeError.value)
  } finally {
    probing.value = false
  }
}

function openProbe(repo: Repository) {
  probeTarget.value = repo
  probeRef.value = repo.defaultBranch || 'main'
  probeResult.value = null
  probeError.value = ''
  probeOpen.value = true
  void runProbe()
}

function closeProbe() {
  probeOpen.value = false
  probeTarget.value = null
  probeResult.value = null
  probeError.value = ''
}

/* ------------------------------ 注册 / 编辑 ------------------------------ */

interface RepoForm {
  name: string
  key: string
  url: string
  defaultBranch: string
  provider: string
  layer: string
  language: string
  credentialId: string
  status: string
  packagePrefixes: string
  pathPrefixes: string
  artifactNames: string
  hostPatterns: string
  endpointPatterns: string
  keywords: string
}

function emptyForm(): RepoForm {
  return {
    name: '',
    key: '',
    url: '',
    defaultBranch: 'main',
    provider: 'git',
    layer: 'service',
    language: 'java',
    credentialId: '',
    status: 'active',
    packagePrefixes: '',
    pathPrefixes: '',
    artifactNames: '',
    hostPatterns: '',
    endpointPatterns: '',
    keywords: '',
  }
}

const formOpen = ref(false)
const editingId = ref('')
const saving = ref(false)
const formError = ref('')
const form = reactive<RepoForm>(emptyForm())
const formErrors = reactive<Record<string, string>>({})

function clearFormErrors() {
  for (const k of Object.keys(formErrors)) delete formErrors[k]
}

function openCreate() {
  Object.assign(form, emptyForm())
  clearFormErrors()
  formError.value = ''
  editingId.value = ''
  formOpen.value = true
}

function openEdit(repo: Repository) {
  const rules = repo.matchRules ?? {}
  Object.assign(form, {
    name: repo.name ?? '',
    key: repo.key ?? '',
    url: repo.url ?? '',
    defaultBranch: repo.defaultBranch || 'main',
    provider: repo.provider ?? 'git',
    layer: repo.layer ?? 'unknown',
    language: repo.language ?? 'other',
    credentialId: repo.credentialId ?? '',
    status: repo.status ?? 'active',
    packagePrefixes: joinList(rules.packagePrefixes),
    pathPrefixes: joinList(rules.pathPrefixes),
    artifactNames: joinList(rules.artifactNames),
    hostPatterns: joinList(rules.hostPatterns),
    endpointPatterns: joinList(rules.endpointPatterns),
    keywords: joinList(rules.keywords),
  } satisfies RepoForm)
  clearFormErrors()
  formError.value = ''
  editingId.value = repo.id
  formOpen.value = true
}

function validate(): boolean {
  clearFormErrors()
  if (!form.name.trim()) formErrors.name = '请输入仓库名称'
  if (!form.key.trim()) formErrors.key = '请输入业务唯一键 Key'
  else if (!/^[A-Za-z0-9._-]+$/.test(form.key.trim())) formErrors.key = 'Key 仅允许字母、数字、点、下划线、短横线'
  if (!form.url.trim()) formErrors.url = '请输入仓库地址'
  else if (!/^(https?:\/\/|git@|ssh:\/\/)/i.test(form.url.trim())) formErrors.url = '地址需以 http(s)://、git@ 或 ssh:// 开头'
  if (!form.defaultBranch.trim()) formErrors.defaultBranch = '请输入默认分支'
  if (!form.language.trim()) formErrors.language = '请输入或选择主要语言'
  return Object.keys(formErrors).length === 0
}

function buildPayload(): Partial<Repository> {
  const matchRules: RepoMatchRules = {
    packagePrefixes: parseList(form.packagePrefixes),
    pathPrefixes: parseList(form.pathPrefixes),
    artifactNames: parseList(form.artifactNames),
    hostPatterns: parseList(form.hostPatterns),
    endpointPatterns: parseList(form.endpointPatterns),
    keywords: parseList(form.keywords),
  }
  return {
    name: form.name.trim(),
    key: form.key.trim(),
    url: form.url.trim(),
    defaultBranch: form.defaultBranch.trim() || 'main',
    provider: form.provider as VCSProvider,
    layer: form.layer as RepoLayer,
    language: form.language.trim().toLowerCase(),
    credentialId: form.credentialId ? form.credentialId : undefined,
    status: form.status as RepoStatus,
    matchRules,
  }
}

async function submitForm() {
  if (!validate()) return
  saving.value = true
  formError.value = ''
  try {
    const payload = buildPayload()
    if (editingId.value) {
      await api.updateRepo(editingId.value, payload)
      toastOk('仓库已更新', payload.name)
    } else {
      await api.createRepo(payload)
      toastOk('仓库已注册', `${payload.name}（${payload.key}）`)
    }
    formOpen.value = false
    await reloadRepos()
    void reloadGroupIndex()
  } catch (e) {
    formError.value = actionError(editingId.value ? '更新仓库' : '注册仓库', e)
    toastError(editingId.value ? '更新仓库失败' : '注册仓库失败', errText(e))
  } finally {
    saving.value = false
  }
}

/* ------------------------------- 删除 ------------------------------- */

const deleteTarget = ref<Repository | null>(null)
const deleting = ref(false)
const deleteError = ref('')

async function confirmDelete() {
  const target = deleteTarget.value
  if (!target) return
  deleting.value = true
  deleteError.value = ''
  try {
    await api.deleteRepo(target.id)
    toastOk('仓库已删除', target.name)
    deleteTarget.value = null
    await reloadRepos()
    void reloadGroupIndex()
  } catch (e) {
    deleteError.value = errText(e)
    toastError('删除仓库失败', deleteError.value)
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">代码源仓库</h1>
        <p class="page-subtitle">仅维护仓库元数据与匹配索引，不存储源码、不预加载代码</p>
      </div>
      <div class="row">
        <button class="btn" :disabled="reposLoading" @click="reloadRepos()">刷新</button>
        <button class="btn btn-primary" @click="openCreate">注册仓库</button>
      </div>
    </div>

    <div v-if="reposError" class="alert alert-error">
      <span class="strong nowrap">仓库列表加载失败</span>
      <span>{{ reposError }}</span>
      <span class="spacer" />
      <button class="btn btn-sm" @click="reloadRepos()">重试</button>
    </div>

    <!-- 统计条 -->
    <div class="grid grid-4">
      <StatTile
        label="仓库总数"
        :value="formatNumber(repos.length)"
        :hint="`当前页筛选命中 ${formatNumber(filtered.length)} 个`"
      />
      <div class="card">
        <div class="card-head"><h3 class="card-title">按分层统计</h3></div>
        <div class="card-body tight">
          <BarList :data="layerDist" :label-map="layerNameMap" />
        </div>
      </div>
      <div class="card">
        <div class="card-head"><h3 class="card-title">语言分布</h3></div>
        <div class="card-body tight">
          <BarList :data="languageDist" tone="var(--info)" />
        </div>
      </div>
      <StatTile
        label="凭证使用数"
        :value="formatNumber(credentialUsage.bound)"
        :hint="`已绑定 ${credentialUsage.distinct} 个凭证 · 未绑定 ${credentialUsage.free} 个仓库`"
        tone="info"
      />
    </div>

    <div v-if="credentialError" class="alert alert-warn small">
      凭证列表加载失败（{{ credentialError }}），注册仓库时可留空凭证。
    </div>
    <div v-if="groupIndexError" class="alert alert-warn small">
      分组索引加载失败（{{ groupIndexError }}），「所属分组」列暂不可用，其余功能不受影响。
    </div>

    <!-- 工具栏 -->
    <div class="card">
      <div class="card-body tight">
        <div class="row wrap">
          <input v-model="keyword" class="input" style="max-width: 280px" placeholder="搜索仓库名 / Key / URL" />
          <select v-model="layerFilter" class="select" style="max-width: 168px">
            <option value="all">全部分层</option>
            <option v-for="l in LAYERS" :key="l.value" :value="l.value">{{ layerLabel(l.value) }}</option>
          </select>
          <select v-model="statusFilter" class="select" style="max-width: 148px">
            <option value="all">全部状态</option>
            <option v-for="s in STATUS_OPTIONS" :key="s.value" :value="s.value">{{ s.label }}</option>
          </select>
          <span class="spacer" />
          <span class="small muted nowrap">
            共 {{ formatNumber(filtered.length) }} / {{ formatNumber(repos.length) }} 个仓库
          </span>
        </div>
      </div>
    </div>

    <!-- 列表 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">仓库索引</h3>
        <span class="small muted">匹配规则用于堆栈驱动的按需加载，越精确越少拉取代码</span>
      </div>

      <div v-if="reposLoading && !repos.length" class="card-body">
        <div class="skeleton" style="height: 16px; margin-bottom: 10px" />
        <div class="skeleton" style="height: 16px; margin-bottom: 10px" />
        <div class="skeleton" style="height: 16px" />
      </div>

      <EmptyState
        v-else-if="!filtered.length"
        :title="reposError ? '仓库数据不可用' : repos.length ? '没有匹配的仓库' : '暂无仓库'"
        :hint="
          reposError
            ? '接口暂不可用，可点击刷新重试'
            : repos.length
              ? '调整关键字或筛选条件后重试'
              : '点击右上角「注册仓库」维护第一个代码源'
        "
        icon="📦"
      />

      <div v-else class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>名称</th>
              <th>分层</th>
              <th>语言</th>
              <th>Provider</th>
              <th>默认分支</th>
              <th>URL</th>
              <th>匹配规则</th>
              <th>所属分组</th>
              <th>状态</th>
              <th>更新时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in pagedRepos" :key="r.id">
              <td>
                <div class="strong">{{ r.name || '未命名仓库' }}</div>
                <div class="mono muted small">{{ r.key || '-' }}</div>
              </td>
              <td>
                <span class="badge" :class="`badge-${layerTone(r.layer)}`">{{ layerLabel(r.layer) }}</span>
              </td>
              <td class="nowrap">{{ r.language || '-' }}</td>
              <td class="nowrap">{{ r.provider || '-' }}</td>
              <td class="mono nowrap">{{ r.defaultBranch || '-' }}</td>
              <td style="max-width: 240px">
                <div class="row" style="gap: 4px">
                  <span class="mono ellipsis" style="max-width: 160px" :title="r.url">{{ r.url || '-' }}</span>
                  <CopyButton v-if="r.url" :text="r.url" label="复制" title="复制仓库地址" />
                </div>
              </td>
              <td style="max-width: 260px">
                <div v-if="ruleCount(r.matchRules)" class="row wrap" style="gap: 4px">
                  <span
                    v-for="p in rulePreview(r.matchRules).slice(0, 3)"
                    :key="p"
                    class="badge badge-muted mono"
                    :title="p"
                  >
                    {{ truncate(p, 22) }}
                  </span>
                  <span v-if="ruleCount(r.matchRules) > 3" class="badge badge-info">
                    +{{ ruleCount(r.matchRules) - 3 }}
                  </span>
                </div>
                <div class="small muted">{{ ruleCount(r.matchRules) }} 条规则</div>
              </td>
              <td style="max-width: 180px">
                <div v-if="groupsOf(r.id).length" class="row wrap" style="gap: 4px">
                  <span v-for="g in groupsOf(r.id)" :key="g" class="badge badge-info">{{ g }}</span>
                </div>
                <span v-else class="muted small">—</span>
              </td>
              <td>
                <StatusBadge :label="statusLabel(r.status)" :tone="r.status === 'disabled' ? 'muted' : 'ok'" />
              </td>
              <td class="nowrap small muted" :title="formatTime(r.updatedAt)">
                {{ formatRelative(r.updatedAt) }}
              </td>
              <td>
                <div class="row" style="gap: 4px">
                  <button class="btn btn-sm" @click="openProbe(r)">探测</button>
                  <button class="btn btn-sm" @click="openEdit(r)">编辑</button>
                  <button class="btn btn-sm btn-danger" @click="deleteTarget = r">删除</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <Pagination
        v-if="filtered.length"
        :page="page"
        :page-size="pageSize"
        :total="filtered.length"
        @change="onPageChange"
      />
    </div>

    <!-- 探测结果 -->
    <AppModal v-if="probeOpen" :title="`仓库连通性探测 · ${probeTarget?.name ?? ''}`" @close="closeProbe">
      <div class="field" style="margin-bottom: 12px">
        <label class="field-label">解析引用（分支 / Tag / Commit）</label>
        <div class="row">
          <input v-model="probeRef" class="input mono" placeholder="main" @keyup.enter="runProbe" />
          <button class="btn btn-primary" :disabled="probing" @click="runProbe">
            {{ probing ? '探测中…' : '开始探测' }}
          </button>
        </div>
        <span class="field-hint">探测只读取远程引用信息，不下载源码。</span>
      </div>

      <div v-if="probing" class="muted small">正在解析远程仓库引用…</div>

      <div v-else-if="probeError" class="alert alert-error">
        <div>
          <div class="strong">探测失败</div>
          <div class="small" style="margin-top: 2px">{{ probeError }}</div>
          <div class="small muted" style="margin-top: 6px">
            远程仓库不可达时不影响索引维护，任务执行时才会真正解析。
          </div>
        </div>
      </div>

      <div v-else-if="probeResult">
        <div v-if="probeResult.ok" class="alert alert-ok" style="margin-bottom: 12px">
          解析成功，引用可用。
        </div>
        <div v-else class="alert alert-error" style="margin-bottom: 12px">
          {{ probeResult.message || '远程仓库返回异常状态' }}
        </div>
        <dl class="kv">
          <dt>解析引用</dt>
          <dd class="mono">{{ probeResult.ref || probeRef || '-' }}</dd>
          <dt>Commit</dt>
          <dd>
            <div class="row" style="gap: 6px">
              <span class="mono strong">{{ shortCommit(probeResult.commit) }}</span>
              <CopyButton v-if="probeResult.commit" :text="probeResult.commit" label="复制完整 SHA" />
            </div>
            <div class="mono small muted">{{ probeResult.commit || '-' }}</div>
          </dd>
          <dt>耗时</dt>
          <dd>{{ formatDuration(probeResult.durationMs) }}</dd>
        </dl>
        <div class="alert alert-info small" style="margin-top: 12px">
          远程仓库不可达时不影响索引维护，任务执行时才会真正解析。
        </div>
      </div>
    </AppModal>

    <!-- 注册 / 编辑 -->
    <AppModal
      v-if="formOpen"
      :title="editingId ? `编辑仓库 · ${form.name || editingId}` : '注册代码源仓库'"
      wide
      @close="formOpen = false"
    >
      <div v-if="formError" class="alert alert-error" style="margin-bottom: 12px">{{ formError }}</div>

      <div class="grid grid-3">
        <div class="field">
          <label class="field-label">名称<span class="req">*</span></label>
          <input v-model="form.name" class="input" placeholder="订单服务" />
          <span v-if="formErrors.name" class="field-error">{{ formErrors.name }}</span>
        </div>
        <div class="field">
          <label class="field-label">Key<span class="req">*</span></label>
          <input v-model="form.key" class="input mono" placeholder="order-service" />
          <span class="field-hint">业务唯一键，用于堆栈包名 / 主机名匹配，如 order-service</span>
          <span v-if="formErrors.key" class="field-error">{{ formErrors.key }}</span>
        </div>
        <div class="field">
          <label class="field-label">默认分支<span class="req">*</span></label>
          <input v-model="form.defaultBranch" class="input mono" placeholder="main" />
          <span v-if="formErrors.defaultBranch" class="field-error">{{ formErrors.defaultBranch }}</span>
        </div>
      </div>

      <div class="field" style="margin-top: 12px">
        <label class="field-label">仓库地址<span class="req">*</span></label>
        <input v-model="form.url" class="input mono" placeholder="https://git.example.com/acme/order-service.git" />
        <span v-if="formErrors.url" class="field-error">{{ formErrors.url }}</span>
      </div>

      <div class="grid grid-4" style="margin-top: 12px">
        <div class="field">
          <label class="field-label">Provider</label>
          <select v-model="form.provider" class="select">
            <option v-for="p in PROVIDERS" :key="p.value" :value="p.value">{{ p.label }}</option>
          </select>
        </div>
        <div class="field">
          <label class="field-label">分层</label>
          <select v-model="form.layer" class="select">
            <option v-for="l in LAYERS" :key="l.value" :value="l.value">{{ layerLabel(l.value) }}</option>
          </select>
        </div>
        <div class="field">
          <label class="field-label">语言</label>
          <input v-model="form.language" class="input" list="repo-language-options" placeholder="java" />
          <datalist id="repo-language-options">
            <option v-for="l in LANGUAGES" :key="l" :value="l" />
          </datalist>
          <span v-if="formErrors.language" class="field-error">{{ formErrors.language }}</span>
        </div>
        <div class="field">
          <label class="field-label">状态</label>
          <select v-model="form.status" class="select">
            <option v-for="s in STATUS_OPTIONS" :key="s.value" :value="s.value">{{ s.label }}</option>
          </select>
        </div>
      </div>

      <div class="field" style="margin-top: 12px">
        <label class="field-label">凭证</label>
        <select v-model="form.credentialId" class="select">
          <option value="">不使用凭证（公开仓库 / 匿名访问）</option>
          <option v-for="c in credentials" :key="c.id" :value="c.id">
            {{ c.name }}（{{ c.type }}{{ c.username ? ` · ${c.username}` : '' }}）
          </option>
        </select>
        <span class="field-hint">
          凭证在「租户与权限」统一维护，仓库只引用其 ID；编辑时留空表示不改变后端已有绑定。
        </span>
      </div>

      <div class="card" style="margin-top: 16px">
        <div class="card-head">
          <h3 class="card-title">匹配规则（堆栈驱动懒加载的匹配依据）</h3>
          <span class="small muted">多值字段支持换行或逗号分隔</span>
        </div>
        <div class="card-body">
          <div class="grid grid-3">
            <div v-for="f in RULE_FIELDS" :key="f.key" class="field">
              <label class="field-label">{{ f.label }}</label>
              <textarea
                v-model="form[f.key]"
                class="textarea"
                style="min-height: 84px"
                :placeholder="f.placeholder"
              />
              <span class="field-hint">{{ f.hint }}</span>
            </div>
          </div>
        </div>
      </div>

      <template #footer>
        <button class="btn" :disabled="saving" @click="formOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="saving" @click="submitForm">
          {{ saving ? '提交中…' : editingId ? '保存修改' : '注册仓库' }}
        </button>
      </template>
    </AppModal>

    <!-- 删除确认 -->
    <AppModal v-if="deleteTarget" title="删除仓库" @close="deleteTarget = null">
      <div class="alert alert-error" style="margin-bottom: 12px">
        删除后分组关联也会一并移除，历史任务中的仓库解析记录不受影响。
      </div>
      <div v-if="deleteError" class="alert alert-error" style="margin-bottom: 12px">{{ deleteError }}</div>
      <dl class="kv">
        <dt>名称</dt>
        <dd class="strong">{{ deleteTarget.name }}</dd>
        <dt>Key</dt>
        <dd class="mono">{{ deleteTarget.key }}</dd>
        <dt>地址</dt>
        <dd class="mono small">{{ deleteTarget.url }}</dd>
        <dt>所属分组</dt>
        <dd>
          <template v-if="groupsOf(deleteTarget.id).length">
            <span v-for="g in groupsOf(deleteTarget.id)" :key="g" class="badge badge-warn" style="margin-right: 4px">
              {{ g }}
            </span>
          </template>
          <span v-else class="muted small">未加入任何分组</span>
        </dd>
      </dl>
      <template #footer>
        <button class="btn" :disabled="deleting" @click="deleteTarget = null">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="confirmDelete">
          {{ deleting ? '删除中…' : '确认删除' }}
        </button>
      </template>
    </AppModal>
  </div>
</template>

<style scoped>
.textarea {
  min-height: 84px;
}
</style>
