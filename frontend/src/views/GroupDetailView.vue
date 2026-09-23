<script setup lang="ts">
/**
 * 分组详情：成员链路视图 + 影响面分析 + 成员明细 + 历史任务。
 *
 * 链路顺序按成员 order 升序（越靠前越接近调用入口）；入口成员单独标记。
 * 影响面接口不可用时降级为前端本地估算，页面不白屏。
 */
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { GroupImpact, GroupMember, Repository, RepositoryGroup, Task } from '@/types'
import {
  formatNumber,
  formatPercent,
  formatRelative,
  formatTime,
  layerLabel,
  modeLabel,
  truncate,
} from '@/utils/format'
import { toastError, toastOk } from '@/composables/useToast'
import { useAsync } from '@/composables/useAsync'
import AppModal from '@/components/AppModal.vue'
import BarList from '@/components/BarList.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'

const route = useRoute()
const router = useRouter()

function errText(e: unknown): string {
  if (e instanceof ApiError) return `${e.message}（code=${e.code}）`
  if (e instanceof Error) return e.message
  return String(e)
}

const groupId = computed(() => String(route.params.id ?? ''))

/* ------------------------------ 数据加载 ------------------------------ */

const {
  data: detail,
  loading: detailLoading,
  error: detailError,
  reload: reloadDetail,
} = useAsync(async () => {
  if (!groupId.value) throw new ApiError(400, '缺少分组 ID')
  return api.getGroup(groupId.value)
}, { silent: true })

const {
  data: impactData,
  error: impactError,
  reload: reloadImpact,
} = useAsync(async () => {
  if (!groupId.value) throw new ApiError(400, '缺少分组 ID')
  return api.groupImpact(groupId.value)
}, { silent: true })

const { data: repoPage, error: repoError } = useAsync(
  () => api.listRepos({ page: 1, pageSize: 200 }),
  { silent: true },
)

const { data: taskPage, error: taskError } = useAsync(
  () => api.listTasks({ page: 1, pageSize: 100 }),
  { silent: true },
)

const group = computed<RepositoryGroup | null>(() => detail.value?.group ?? null)
const members = computed<GroupMember[]>(() =>
  [...(detail.value?.members ?? [])].sort((a, b) => (a?.order ?? 0) - (b?.order ?? 0)),
)
const allRepos = computed<Repository[]>(() => repoPage.value?.items ?? [])

function isEntry(repositoryId: string): boolean {
  return (group.value?.entryRepositoryIds ?? []).includes(repositoryId)
}

function memberName(m: GroupMember): string {
  return m?.repo?.name ?? m?.repositoryId ?? '-'
}

function repoById(id: string): Repository | undefined {
  const member = members.value.find((m) => m.repositoryId === id)
  return member?.repo ?? allRepos.value.find((r) => r.id === id)
}

/* ------------------------------ 影响面 ------------------------------ */

const localImpact = computed<GroupImpact>(() => {
  const byLayer: Record<string, number> = {}
  const byLanguage: Record<string, number> = {}
  const sharedLibs: string[] = []
  for (const m of members.value) {
    const layer = m?.repo?.layer ?? 'unknown'
    const language = (m?.repo?.language || 'unknown').toLowerCase()
    byLayer[layer] = (byLayer[layer] ?? 0) + 1
    byLanguage[language] = (byLanguage[language] ?? 0) + 1
    if (layer === 'library') sharedLibs.push(memberName(m))
  }
  const entryRepos = (group.value?.entryRepositoryIds ?? []).map((id) => repoById(id)?.name ?? id)
  const count = members.value.length
  const layerCount = Object.keys(byLayer).length
  const riskScore = Math.min(1, Math.round(((count / 12) * 0.6 + (layerCount / 6) * 0.4) * 100) / 100)
  return {
    repoCount: count,
    byLayer,
    byLanguage,
    entryRepos,
    sharedLibs,
    riskScore,
    riskNotes: [
      '影响面接口暂不可用，以下为前端基于成员数据本地估算，仅供参考。',
      `成员仓库 ${count} 个，覆盖 ${layerCount} 个分层。`,
      sharedLibs.length ? `公共库 ${sharedLibs.length} 个，改动可能被多处复用。` : '未识别到公共库成员。',
    ],
  }
})

const effectiveImpact = computed<GroupImpact>(() => impactData.value ?? localImpact.value)
const impactDegraded = computed(() => Boolean(impactError.value))

const layerNameMap = computed<Record<string, string>>(() => {
  const acc: Record<string, string> = {}
  for (const key of Object.keys(effectiveImpact.value.byLayer ?? {})) acc[key] = layerLabel(key)
  return acc
})

type Tone = '' | 'ok' | 'warn' | 'error' | 'info' | 'degraded'

function riskMeta(score?: number): { label: string; tone: Tone; text: string } {
  const s = score ?? 0
  if (s < 0.3) return { label: '低', tone: 'ok', text: '链路较短、分层集中，可优先单仓库定位。' }
  if (s < 0.6) return { label: '中', tone: 'warn', text: '跨多层调用，修复时需关注接口与契约兼容性。' }
  return { label: '高', tone: 'error', text: '跨层跨语言影响面大，建议联合排查并人工复核补丁。' }
}

const risk = computed(() => riskMeta(effectiveImpact.value.riskScore))

const impactLayerCount = computed(() => Object.keys(effectiveImpact.value.byLayer ?? {}).length)
const impactLanguageCount = computed(() => Object.keys(effectiveImpact.value.byLanguage ?? {}).length)

/* ------------------------------ 历史任务 ------------------------------ */

const groupTasks = computed<Task[]>(() => {
  const id = groupId.value
  return (taskPage.value?.items ?? [])
    .filter((t) => Boolean(t) && t.groupId === id)
    .sort((a, b) => new Date(b.createdAt ?? 0).getTime() - new Date(a.createdAt ?? 0).getTime())
})

const recentTasks = computed<Task[]>(() => groupTasks.value.slice(0, 8))

const taskStats = computed(() => {
  const list = groupTasks.value
  const success = list.filter((t) => t.latestRunId).length
  return { total: list.length, withRun: success }
})

/** Task 本身不带状态，最近任务的运行状态按需拉取（最多 8 条，失败静默降级）。 */
const runStates = ref<Record<string, string>>({})

async function loadRunStates(list: Task[]) {
  const ids = list.map((t) => t.latestRunId).filter(Boolean).slice(0, 8)
  if (!ids.length) {
    runStates.value = {}
    return
  }
  const settled = await Promise.allSettled(ids.map((id) => api.getRun(id)))
  const map: Record<string, string> = {}
  settled.forEach((res, i) => {
    const id = ids[i]
    if (!id || res.status !== 'fulfilled') return
    const state = res.value?.state
    if (state) map[id] = state
  })
  runStates.value = map
}

watch(recentTasks, (list) => {
  void loadRunStates(list)
})

function stateOf(task: Task): string {
  return task?.latestRunId ? (runStates.value[task.latestRunId] ?? '') : ''
}

function openTask(id?: string) {
  if (!id) return
  void router.push({ name: 'task-detail', params: { id } })
}

/* ------------------------------ 成员编辑 ------------------------------ */

interface DraftMember {
  repositoryId: string
  order: number
  entry: boolean
}

const editOpen = ref(false)
const saving = ref(false)
const editError = ref('')
const editMemberError = ref('')
const drafts = ref<DraftMember[]>([])
const pickKeyword = ref('')

function normalizeDrafts(): DraftMember[] {
  return [...drafts.value].sort((a, b) => a.order - b.order).map((d, i) => ({ ...d, order: i }))
}

const availableRepos = computed<Repository[]>(() => {
  const chosen = new Set(drafts.value.map((d) => d.repositoryId))
  const kw = pickKeyword.value.trim().toLowerCase()
  return allRepos.value.filter((r) => {
    if (!r || chosen.has(r.id)) return false
    if (!kw) return true
    return [r.name, r.key, r.language, r.layer].some((v) => (v ?? '').toLowerCase().includes(kw))
  })
})

function openEdit() {
  const g = group.value
  drafts.value = members.value.map((m, i) => ({
    repositoryId: m.repositoryId,
    order: Number.isFinite(m.order) ? m.order : i,
    entry: (g?.entryRepositoryIds ?? []).includes(m.repositoryId),
  }))
  drafts.value = normalizeDrafts()
  pickKeyword.value = ''
  editError.value = ''
  editMemberError.value = ''
  editOpen.value = true
}

function addDraft(id: string) {
  if (drafts.value.some((d) => d.repositoryId === id)) return
  drafts.value = [...normalizeDrafts(), { repositoryId: id, order: drafts.value.length, entry: false }]
}

function removeDraft(id: string) {
  drafts.value = normalizeDrafts().filter((d) => d.repositoryId !== id)
}

function moveDraft(id: string, dir: number) {
  const list = normalizeDrafts()
  const i = list.findIndex((d) => d.repositoryId === id)
  const j = i + dir
  if (i < 0 || j < 0 || j >= list.length) return
  const a = list[i].order
  list[i].order = list[j].order
  list[j].order = a
  drafts.value = list.sort((x, y) => x.order - y.order)
}

function setOrder(id: string, e: Event) {
  const target = e.target as HTMLInputElement | null
  const raw = target?.value ?? ''
  if (raw === '') return
  const value = Number(raw)
  if (!Number.isFinite(value)) return
  const next = Math.max(0, Math.trunc(value))
  drafts.value = drafts.value.map((d) => (d.repositoryId === id ? { ...d, order: next } : d))
}

function toggleDraftEntry(id: string, e: Event) {
  const target = e.target as HTMLInputElement | null
  const checked = Boolean(target?.checked)
  drafts.value = drafts.value.map((d) => (d.repositoryId === id ? { ...d, entry: checked } : d))
}

async function submitEdit() {
  const g = group.value
  if (!g) return
  const list = normalizeDrafts()
  if (!list.length) {
    editMemberError.value = '分组至少保留一个成员仓库'
    return
  }
  editMemberError.value = ''
  saving.value = true
  editError.value = ''
  try {
    await api.updateGroup(g.id, {
      group: {
        name: g.name,
        key: g.key,
        description: g.description,
        status: g.status,
        entryRepositoryIds: list.filter((d) => d.entry).map((d) => d.repositoryId),
      },
      members: list.map((d, i) => ({ repositoryId: d.repositoryId, order: i })),
    })
    toastOk('分组成员已更新', `${list.length} 个成员`)
    editOpen.value = false
    await reloadDetail()
    void reloadImpact()
  } catch (e) {
    editError.value = `更新成员失败：${errText(e)}`
    toastError('更新成员失败', errText(e))
  } finally {
    saving.value = false
  }
}

/* ------------------------------- 跳转 ------------------------------- */

function goBack() {
  void router.push({ name: 'groups' })
}

function startGroupTask() {
  void router.push({ name: 'task-new', query: { groupId: groupId.value } })
}

function reloadAll() {
  void reloadDetail()
  void reloadImpact()
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div style="min-width: 0">
        <div class="row" style="gap: 8px">
          <button class="btn btn-sm btn-ghost" @click="goBack">← 返回</button>
          <h1 class="page-title ellipsis">{{ group?.name ?? '分组详情' }}</h1>
          <span v-if="group?.key" class="mono muted">{{ group.key }}</span>
          <StatusBadge
            v-if="group"
            :label="group.status === 'disabled' ? '已停用' : '启用中'"
            :tone="group.status === 'disabled' ? 'muted' : 'ok'"
          />
        </div>
        <p class="page-subtitle">
          {{ group?.description || '纯逻辑业务索引：不改变仓库独立性，不批量加载代码' }}
        </p>
      </div>
      <div class="row">
        <button class="btn" :disabled="detailLoading" @click="reloadAll">刷新</button>
        <button class="btn btn-primary" :disabled="!group" @click="startGroupTask">发起分组联合排查</button>
      </div>
    </div>

    <div v-if="detailError" class="alert alert-error">
      <div>
        <div class="strong">分组详情加载失败</div>
        <div class="small">{{ detailError }}</div>
      </div>
      <span class="spacer" />
      <button class="btn btn-sm" @click="reloadDetail()">重试</button>
    </div>

    <div v-if="detailLoading && !group" class="card">
      <div class="card-body">
        <div class="skeleton" style="height: 16px; width: 30%; margin-bottom: 12px" />
        <div class="skeleton" style="height: 12px; margin-bottom: 8px" />
        <div class="skeleton" style="height: 12px; width: 60%" />
      </div>
    </div>

    <div v-else-if="!group" class="card">
      <EmptyState title="分组不存在或不可访问" hint="请确认分组 ID，或返回业务分组列表重新选择" icon="🗂">
        <button class="btn btn-sm" @click="goBack">返回分组列表</button>
      </EmptyState>
    </div>

    <template v-else>
      <!-- 概览统计 -->
      <div class="grid grid-4">
        <StatTile label="成员仓库" :value="formatNumber(members.length)" hint="按链路顺序排列" />
        <StatTile label="覆盖分层" :value="formatNumber(impactLayerCount)" hint="影响面分层数" />
        <StatTile
          label="链路入口"
          :value="formatNumber((group.entryRepositoryIds ?? []).length)"
          :hint="(group.entryRepositoryIds ?? []).length ? '已指定解析起点' : '未指定，按 order 推断'"
          tone="info"
        />
        <StatTile label="影响面风险" :value="risk.label" :hint="`风险分 ${formatPercent(effectiveImpact.riskScore, 0)}`" :tone="risk.tone" />
      </div>

      <!-- 成员链路视图 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">成员链路视图</h3>
          <span class="small muted">按 order 升序 · 越靠前越接近调用入口</span>
        </div>
        <div class="card-body">
          <EmptyState v-if="!members.length" title="分组暂无成员" hint="点击「编辑成员」为分组添加仓库" icon="🔗" />
          <div v-else class="chain">
            <template v-for="(m, idx) in members" :key="m.repositoryId">
              <div v-if="idx > 0" class="chain-arrow">→</div>
              <div class="chain-node">
                <div class="row" style="gap: 6px">
                  <span class="badge badge-muted mono nowrap">{{ idx + 1 }}</span>
                  <span class="strong ellipsis" :title="memberName(m)">{{ memberName(m) }}</span>
                  <span v-if="isEntry(m.repositoryId)" class="badge badge-info nowrap">链路入口</span>
                </div>
                <div class="mono small muted ellipsis" :title="m.repo?.key">{{ m.repo?.key ?? m.repositoryId }}</div>
                <div class="row wrap" style="gap: 5px; margin-top: 6px">
                  <span class="badge badge-muted">{{ layerLabel(m.repo?.layer) }}</span>
                  <span class="badge badge-muted">{{ m.repo?.language || '未知语言' }}</span>
                  <StatusBadge
                    :label="m.repo?.status === 'disabled' ? '已停用' : '可用'"
                    :tone="m.repo?.status === 'disabled' ? 'muted' : 'ok'"
                  />
                </div>
                <div v-if="m.note" class="small faint" style="margin-top: 6px">{{ truncate(m.note, 60) }}</div>
              </div>
            </template>
          </div>

          <div class="alert alert-info" style="margin-top: 14px">
            故障分析时先解析堆栈链路，再按需加载故障关联的少量文件，不会全量拉取分组代码。
          </div>

          <div class="row" style="margin-top: 12px">
            <button class="btn btn-sm" @click="openEdit">编辑成员</button>
            <span v-if="repoError" class="small muted">仓库列表加载失败：{{ repoError }}</span>
          </div>
        </div>
      </div>

      <!-- 影响面分析 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">影响面分析</h3>
          <span class="small muted">用于评估补丁的波及范围与复核强度</span>
        </div>
        <div class="card-body">
          <div v-if="impactDegraded" class="alert alert-warn" style="margin-bottom: 12px">
            影响面接口不可用（{{ impactError }}），已降级为前端基于成员数据的本地估算。
          </div>

          <dl class="kv" style="margin-bottom: 14px">
            <dt>仓库数量</dt>
            <dd class="strong">{{ formatNumber(effectiveImpact.repoCount) }}</dd>
            <dt>链路入口</dt>
            <dd>
              <template v-if="(effectiveImpact.entryRepos ?? []).length">
                <span
                  v-for="e in effectiveImpact.entryRepos"
                  :key="e"
                  class="badge badge-info"
                  style="margin-right: 4px"
                >
                  {{ e }}
                </span>
              </template>
              <span v-else class="muted small">未指定</span>
            </dd>
            <dt>共享公共库</dt>
            <dd>
              <template v-if="(effectiveImpact.sharedLibs ?? []).length">
                <span
                  v-for="l in effectiveImpact.sharedLibs"
                  :key="l"
                  class="badge badge-warn"
                  style="margin-right: 4px"
                >
                  {{ l }}
                </span>
              </template>
              <span v-else class="muted small">无</span>
            </dd>
          </dl>

          <div class="field" style="margin-bottom: 14px">
            <label class="field-label">
              风险评分 {{ formatPercent(effectiveImpact.riskScore, 0) }} · {{ risk.label }}风险
            </label>
            <div class="progress">
              <span :class="risk.tone" :style="{ width: `${Math.min(100, Math.round((effectiveImpact.riskScore ?? 0) * 100))}%` }" />
            </div>
            <span class="field-hint">{{ risk.text }}</span>
          </div>

          <div class="grid grid-2">
            <div>
              <div class="small faint" style="margin-bottom: 6px">按分层（{{ impactLayerCount }} 类）</div>
              <BarList :data="effectiveImpact.byLayer ?? {}" :label-map="layerNameMap" />
            </div>
            <div>
              <div class="small faint" style="margin-bottom: 6px">按语言（{{ impactLanguageCount }} 类）</div>
              <BarList :data="effectiveImpact.byLanguage ?? {}" tone="var(--info)" />
            </div>
          </div>

          <div v-if="(effectiveImpact.riskNotes ?? []).length" style="margin-top: 14px">
            <div class="small faint" style="margin-bottom: 6px">风险提示</div>
            <ul style="margin: 0; padding-left: 20px">
              <li v-for="(n, i) in effectiveImpact.riskNotes ?? []" :key="i" class="small">{{ n }}</li>
            </ul>
          </div>
        </div>
      </div>

      <!-- 成员明细表 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">成员明细</h3>
          <span class="small muted">共 {{ formatNumber(members.length) }} 个成员</span>
        </div>
        <EmptyState v-if="!members.length" title="暂无成员明细" hint="先为分组添加成员仓库" />
        <div v-else class="table-wrap">
          <table class="table">
            <thead>
              <tr>
                <th>顺序</th>
                <th>仓库名</th>
                <th>Key</th>
                <th>分层</th>
                <th>语言</th>
                <th>默认分支</th>
                <th>状态</th>
                <th>备注</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(m, idx) in members" :key="m.repositoryId">
                <td class="mono">{{ idx + 1 }}</td>
                <td>
                  <div class="row" style="gap: 6px">
                    <span class="strong">{{ memberName(m) }}</span>
                    <span v-if="isEntry(m.repositoryId)" class="badge badge-info">链路入口</span>
                  </div>
                </td>
                <td class="mono">{{ m.repo?.key ?? '-' }}</td>
                <td><span class="badge badge-muted">{{ layerLabel(m.repo?.layer) }}</span></td>
                <td>{{ m.repo?.language || '-' }}</td>
                <td class="mono">{{ m.repo?.defaultBranch || '-' }}</td>
                <td>
                  <StatusBadge
                    :label="m.repo?.status === 'disabled' ? '已停用' : '可用'"
                    :tone="m.repo?.status === 'disabled' ? 'muted' : 'ok'"
                  />
                </td>
                <td class="small muted">{{ m.note ? truncate(m.note, 40) : '—' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- 历史任务 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">分组历史任务</h3>
          <span class="small muted">本地统计（最多取最近 100 条任务）</span>
        </div>
        <div class="card-body">
          <div v-if="taskError" class="empty">暂无统计数据（{{ taskError }}）</div>
          <template v-else>
            <div class="grid grid-3" style="margin-bottom: 14px">
              <StatTile label="历史任务数" :value="formatNumber(taskStats.total)" small />
              <StatTile label="已产生运行" :value="formatNumber(taskStats.withRun)" small tone="info" />
              <StatTile
                label="最近任务时间"
                :value="recentTasks.length ? formatRelative(recentTasks[0].createdAt) : '—'"
                small
              />
            </div>

            <EmptyState v-if="!recentTasks.length" title="暂无关联任务" hint="该分组尚未发起过联合排查任务" icon="🧾" />
            <div v-else class="timeline">
              <div
                v-for="t in recentTasks"
                :key="t.id"
                class="timeline-item clickable"
                style="cursor: pointer"
                @click="openTask(t.id)"
              >
                <div class="timeline-dot" />
                <div class="timeline-body">
                  <div class="row" style="gap: 8px">
                    <StatusBadge v-if="stateOf(t)" :state="stateOf(t)" />
                    <span v-else class="badge badge-muted">状态未知</span>
                    <span class="strong ellipsis" :title="t.title">{{ t.title || '未命名任务' }}</span>
                    <span class="badge badge-muted">{{ modeLabel(t.mode) }}</span>
                  </div>
                  <div class="row small muted" style="gap: 10px; margin-top: 3px">
                    <span :title="formatTime(t.createdAt)">{{ formatRelative(t.createdAt) }}</span>
                    <span>运行 {{ formatNumber(t.runCount) }} 次</span>
                    <span class="mono">{{ t.id }}</span>
                    <CopyButton :text="t.id" label="复制 ID" title="复制任务 ID" />
                  </div>
                </div>
              </div>
            </div>
          </template>
        </div>
      </div>
    </template>

    <!-- 编辑成员 -->
    <AppModal v-if="editOpen" title="编辑分组成员与链路顺序" wide @close="editOpen = false">
      <div v-if="editError" class="alert alert-error" style="margin-bottom: 12px">{{ editError }}</div>
      <div v-if="repoError" class="alert alert-error small" style="margin-bottom: 12px">
        仓库列表加载失败：{{ repoError }}，无法新增成员。
      </div>

      <div class="grid grid-2">
        <div class="field">
          <label class="field-label">可添加仓库（{{ formatNumber(availableRepos.length) }}）</label>
          <input v-model="pickKeyword" class="input" placeholder="搜索仓库名 / Key / 语言" />
          <div class="picker">
            <div v-for="r in availableRepos" :key="r.id" class="picker-row">
              <span class="ellipsis" style="flex: 1" :title="r.name">{{ r.name || r.key }}</span>
              <span class="badge badge-muted">{{ layerLabel(r.layer) }}</span>
              <span class="small muted nowrap">{{ r.language || '-' }}</span>
              <button class="btn btn-sm" @click="addDraft(r.id)">加入</button>
            </div>
            <div v-if="!availableRepos.length" class="small muted" style="padding: 10px">
              没有可添加的仓库（可能已全部加入或未注册仓库）
            </div>
          </div>
        </div>

        <div class="field">
          <label class="field-label">分组成员（{{ drafts.length }}）· 数字为链路顺序</label>
          <div class="picker">
            <div v-for="d in drafts" :key="d.repositoryId" class="picker-row">
              <input
                class="input order-input"
                type="number"
                min="0"
                :value="d.order"
                title="链路顺序，越小越靠前"
                @change="setOrder(d.repositoryId, $event)"
              />
              <span class="ellipsis" style="flex: 1" :title="repoById(d.repositoryId)?.name ?? d.repositoryId">
                {{ repoById(d.repositoryId)?.name ?? d.repositoryId }}
              </span>
              <span class="badge badge-muted nowrap">{{ layerLabel(repoById(d.repositoryId)?.layer) }}</span>
              <label class="checkbox small nowrap" title="设为链路入口">
                <input type="checkbox" :checked="d.entry" @change="toggleDraftEntry(d.repositoryId, $event)" />
                入口
              </label>
              <button class="btn btn-sm btn-ghost" title="上移" @click="moveDraft(d.repositoryId, -1)">↑</button>
              <button class="btn btn-sm btn-ghost" title="下移" @click="moveDraft(d.repositoryId, 1)">↓</button>
              <button class="btn btn-sm btn-ghost" title="移除" @click="removeDraft(d.repositoryId)">×</button>
            </div>
            <div v-if="!drafts.length" class="small muted" style="padding: 10px">分组暂无成员，请从左侧添加</div>
          </div>
          <span v-if="editMemberError" class="field-error">{{ editMemberError }}</span>
          <span class="field-hint">提交后顺序会按 0..N-1 归一化写入。</span>
        </div>
      </div>

      <template #footer>
        <button class="btn" :disabled="saving" @click="editOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="saving" @click="submitEdit">
          {{ saving ? '保存中…' : '保存成员' }}
        </button>
      </template>
    </AppModal>
  </div>
</template>

<style scoped>
.chain {
  display: flex;
  align-items: stretch;
  gap: 10px;
  flex-wrap: wrap;
}

.chain-node {
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg-elev-2);
  padding: 10px 12px;
  min-width: 200px;
  max-width: 280px;
  flex: 1 1 200px;
}

.chain-arrow {
  display: flex;
  align-items: center;
  color: var(--brand-400);
  font-size: 18px;
  font-weight: 600;
}

.picker {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2);
  max-height: 280px;
  overflow: auto;
  padding: 4px;
}

.picker-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius-sm);
  font-size: 13px;
}

.picker-row:hover {
  background: var(--bg-hover);
}

.order-input {
  width: 62px;
  padding: 3px 6px;
  font-family: var(--font-mono);
  font-size: 12px;
}
</style>
