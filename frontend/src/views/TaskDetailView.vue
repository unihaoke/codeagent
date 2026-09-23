<script setup lang="ts">
/**
 * 任务执行详情（实时执行视图）。
 *
 * 覆盖：实时事件流（历史回放 + SSE 增量）、阶段进度、任务级版本锁定、
 * 根因分析、增量补丁、沙箱验证、资源消耗与上下文、调用轨迹。
 *
 * 后端并行开发中，所有字段均按「可能缺失」处理：一律可选链 + 默认值 + 空态兜底。
 */
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api, subscribeRunEvents } from '@/api/client'
import type { AgentEvent, LineRange, ModelCall, Patch, SkillCall, TaskRun, TaskState, VerificationCheck } from '@/types'
import AppModal from '@/components/AppModal.vue'
import CopyButton from '@/components/CopyButton.vue'
import DiffView from '@/components/DiffView.vue'
import EmptyState from '@/components/EmptyState.vue'
import Pagination from '@/components/Pagination.vue'
import StageBar from '@/components/StageBar.vue'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import { useAsync } from '@/composables/useAsync'
import { toastError, toastOk, toastWarn } from '@/composables/useToast'
import { useRealtimeStore } from '@/stores/realtime'
import {
  categoryLabel,
  copyText,
  downloadText,
  formatBytes,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatRelative,
  formatTime,
  layerLabel,
  modeLabel,
  riskLabel,
  shortCommit,
  truncate,
} from '@/utils/format'

const route = useRoute()
const router = useRouter()
const realtime = useRealtimeStore()

/* ------------------------------ 常量映射 ------------------------------ */

const TERMINAL_STATES = ['succeeded', 'needs_review', 'failed', 'cancelled', 'degraded']
const FEEDBACK_MAX = 4000
const CALL_PAGE_SIZE = 100
const MAX_EVENTS = 800
/** 终态后等待历史补发结束的静默期（每条新事件都会重置）。 */
const TERMINAL_GRACE_MS = 1500
/** 一条事件都没收到时的兜底判定窗口。 */
const STREAM_WATCHDOG_MS = 15000

const RESOLVED_FROM: Record<string, string> = {
  request: '调用方指定',
  group_entry: '分组入口',
  stack_hint: '堆栈线索',
  repo_default: '仓库默认分支',
}

const APPLY_RESULT: Record<string, string> = {
  applied: '已应用',
  conflict: '冲突',
  skipped: '跳过',
}

const PATCH_ACTION: Record<string, string> = {
  modify: '修改',
  create: '新增',
  delete: '删除',
}

const CHECK_LABELS: Record<string, string> = {
  compile: '编译校验',
  unit_test: '单元测试',
  lint: '静态检查',
  ast_guard: 'AST 守卫',
  dependency_resolve: '依赖解析',
  static_syntax: '静态语法',
  go_parse: 'Go 语法解析',
  json_valid: 'JSON 校验',
  danger_scan: '危险操作扫描',
  balance: '括号配平',
  scale: '变更规模',
  snippet_integrity: '片段完整性',
}

const EVENT_TONES: Record<string, string> = {
  'task.created': 'info',
  'task.state': 'info',
  'task.terminal': 'ok',
  stage: 'info',
  'skill.call': 'ok',
  'model.call': 'degraded',
  'source.fetch': 'muted',
  'repo.switch': 'warn',
  'patch.proposed': 'ok',
  verification: 'info',
  'report.ready': 'ok',
  log: 'muted',
}

const TIER_LABELS: Record<string, string> = {
  light: '轻量模型',
  strong: '强模型',
  fallback: '兜底模型',
}

/* ------------------------------ 通用兜底助手 ------------------------------ */

function errMsg(e: unknown): string {
  if (e instanceof ApiError) return e.message
  if (e instanceof Error) return e.message
  return String(e)
}

/** 兼容 0-1 与 0-100 两种比例口径。 */
function ratio(v?: number): number {
  const x = v ?? 0
  if (!Number.isFinite(x)) return 0
  return x > 1 ? x / 100 : x
}

function pctWidth(v?: number): string {
  return `${Math.min(100, Math.max(0, ratio(v) * 100)).toFixed(1)}%`
}

function asString(v: unknown): string | undefined {
  return typeof v === 'string' && v.length > 0 ? v : undefined
}

function asNumber(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined
}

function asRecord(v: unknown): Record<string, unknown> | null {
  return v && typeof v === 'object' ? (v as Record<string, unknown>) : null
}

function resolvedFromLabel(v?: string): string {
  if (!v) return '-'
  return RESOLVED_FROM[v] ?? v
}

function applyResultLabel(v?: string): string {
  if (!v) return '-'
  return APPLY_RESULT[v] ?? v
}

function patchActionLabel(v?: string): string {
  if (!v) return '-'
  return PATCH_ACTION[v] ?? v
}

function checkName(name?: string): string {
  if (!name) return '校验项'
  return CHECK_LABELS[name] ?? name
}

function tierLabel(t?: string): string {
  if (!t) return '-'
  return TIER_LABELS[t] ?? t
}

function callStatusMeta(s?: string): { label: string; tone: string } {
  switch (s) {
    case 'ok':
      return { label: '成功', tone: 'ok' }
    case 'failed':
      return { label: '失败', tone: 'error' }
    case 'timeout':
      return { label: '超时', tone: 'warn' }
    case 'blocked':
      return { label: '熔断', tone: 'error' }
    case 'skipped':
      return { label: '跳过', tone: 'muted' }
    case 'fallback':
      return { label: '兜底', tone: 'degraded' }
    default:
      return { label: s || '未知', tone: 'muted' }
  }
}

function patchStatusMeta(s?: string): { label: string; tone: string } {
  switch (s) {
    case 'proposed':
      return { label: '待应用', tone: 'info' }
    case 'applied':
      return { label: '已应用', tone: 'ok' }
    case 'rejected':
      return { label: '已拒绝', tone: 'warn' }
    case 'failed':
      return { label: '应用失败', tone: 'error' }
    default:
      return { label: s || '未知', tone: 'muted' }
  }
}

function rangeText(ranges?: LineRange[]): string {
  const list = ranges ?? []
  if (!list.length) return '-'
  return list.map((r) => `L${r?.start ?? 0}-${r?.end ?? 0}`).join(', ')
}

function eventTone(type?: string): string {
  return EVENT_TONES[type ?? ''] ?? 'muted'
}

function dotClass(ev: AgentEvent): string {
  const level = (ev.level ?? '').toLowerCase()
  if (level === 'error' || level === 'fatal') return 'error'
  if (level === 'warn' || level === 'warning') return 'warn'
  if (ev.type === 'report.ready' || ev.type === 'task.terminal') return 'ok'
  return ''
}

/** 技能 / 模型事件的单行摘要（payload 结构可能不完整，全部做兜底）。 */
function payloadSummary(ev: AgentEvent): string {
  const p = asRecord(ev.payload)
  if (!p) return ''
  if (ev.type === 'skill.call') {
    const skill = asString(p.skill) ?? asString(p.name) ?? '未知技能'
    const version = asString(p.version)
    const status = asString(p.status)
    const meta = status ? callStatusMeta(status) : null
    const dur = asNumber(p.durationMs)
    const attempts = asNumber(p.attempts)
    const parts = [version ? `${skill}@${version}` : skill]
    if (meta) parts.push(meta.label)
    if (attempts !== undefined && attempts > 1) parts.push(`第 ${attempts} 次尝试`)
    if (dur !== undefined) parts.push(formatDuration(dur))
    return parts.join(' · ')
  }
  if (ev.type === 'model.call') {
    const provider = asString(p.provider)
    const model = asString(p.model)
    const status = asString(p.status)
    const meta = status ? callStatusMeta(status) : null
    const tokens = asNumber(p.totalTokens)
    const dur = asNumber(p.durationMs)
    const parts = [[provider, model].filter(Boolean).join('/') || '未知模型']
    const tier = asString(p.tier)
    if (tier) parts.push(tierLabel(tier))
    if (meta) parts.push(meta.label)
    if (tokens !== undefined) parts.push(`${formatCompact(tokens)} tokens`)
    if (dur !== undefined) parts.push(formatDuration(dur))
    return parts.join(' · ')
  }
  return ''
}

/* ------------------------------ 运行详情 ------------------------------ */

const runId = computed(() => String(route.params.id ?? ''))

const errorCode = ref(0)
const runAsync = useAsync<TaskRun>(
  async () => {
    try {
      const data = await api.getRun(runId.value)
      errorCode.value = 0
      return data
    } catch (e) {
      errorCode.value = e instanceof ApiError ? e.code : 0
      throw e
    }
  },
  { immediate: false, silent: true },
)

const run = runAsync.data
const loading = runAsync.loading
const loadError = runAsync.error

const isTerminal = computed(() => !!run.value && TERMINAL_STATES.includes(run.value.state))
const canCancel = computed(() => !!run.value && !isTerminal.value)
const notFound = computed(() => !run.value && !loading.value && !!loadError.value && [403, 404].includes(errorCode.value))

const usageTiles = computed(() => {
  const u = run.value?.usage
  const tiles: { label: string; value: string; hint?: string }[] = [
    { label: '加载文件数', value: formatNumber(u?.filesLoaded ?? 0), hint: '懒加载区间内文件' },
    { label: '代码字符数', value: formatNumber(u?.codeChars ?? 0), hint: `≈ ${formatBytes(u?.codeChars ?? 0)}` },
    { label: '技能调用', value: formatNumber(u?.skillCalls ?? 0), hint: `失败 ${formatNumber(u?.skillFailures ?? 0)} 次` },
    { label: '模型调用', value: formatNumber(u?.modelCalls ?? 0), hint: `兜底 ${formatNumber(u?.modelFallbacks ?? 0)} 次` },
    { label: 'Prompt Token', value: formatCompact(u?.promptTokens ?? 0) },
    { label: 'Completion Token', value: formatCompact(u?.completionTokens ?? 0) },
    { label: 'Total Token', value: formatCompact(u?.totalTokens ?? 0) },
    { label: '缓存命中', value: formatNumber(u?.cacheHits ?? 0) },
    { label: '模型兜底', value: formatNumber(u?.modelFallbacks ?? 0) },
  ]
  return tiles
})

const patches = computed<Patch[]>(() => run.value?.patches ?? [])
const checks = computed<VerificationCheck[]>(() => run.value?.verification?.checks ?? [])
const warnings = computed<string[]>(() => (run.value?.warnings ?? []).filter((w) => !!w))
const blastRadius = computed<string[]>(() => (run.value?.rootCause?.blastRadius ?? []).filter((b) => !!b))
const evidence = computed<string[]>(() => (run.value?.rootCause?.evidence ?? []).filter((e) => !!e))

const pinnedEntries = computed<{ key: string; commit: string }[]>(() => {
  const map = run.value?.pinnedCommits ?? {}
  return Object.entries(map).map(([key, commit]) => ({ key, commit: String(commit ?? '') }))
})

/* ------------------------------ 实时事件流 ------------------------------ */

const events = ref<AgentEvent[]>([])
const currentStage = ref('')
const streamNote = ref('')
const streamUnavailable = ref(false)
const converged = ref(false)
const autoScroll = ref(true)
const timelineEl = ref<HTMLDivElement | null>(null)
let unsubscribe: (() => void) | null = null
/** 详情快照加载完成前到达的 task.state 目标状态：暂存后补，避免历史事件被丢弃。 */
let pendingState: TaskState | null = null
let terminalTimer: number | null = null
let watchdogTimer: number | null = null
let terminalReloaded = false

interface DecoratedEvent {
  key: string
  at?: string
  type: string
  stage?: string
  seq?: number
  message: string
  summary: string
  dot: string
  tone: string
}

const decoratedEvents = computed<DecoratedEvent[]>(() =>
  events.value.map((ev, i) => ({
    key: `${ev?.seq ?? i}-${i}`,
    at: ev?.at,
    type: ev?.type ?? 'log',
    stage: ev?.stage,
    seq: ev?.seq,
    message: ev?.message || '—',
    summary: payloadSummary(ev),
    dot: dotClass(ev),
    tone: eventTone(ev?.type),
  })),
)

/** 事件是否为终态信号（task.terminal / task.state 的 to 落在终态集合）。 */
function isTerminalEvent(ev: AgentEvent): boolean {
  if (ev.type === 'task.terminal') return true
  if (ev.type !== 'task.state') return false
  const to = asString(asRecord(ev.payload)?.to)
  return !!to && TERMINAL_STATES.includes(to)
}

function stopStream(): void {
  clearTerminalStop()
  clearWatchdog()
  if (unsubscribe) {
    unsubscribe()
    unsubscribe = null
  }
}

/**
 * 启动订阅。
 *
 * 后端 `GET /runs/{id}/events` 默认是 `text/event-stream`：连接建立时会先补发历史事件，
 * 再持续推送实时事件。因此这里不额外调用 `api.runEvents()` 拉历史（会把 SSE 当 JSON 解析失败）。
 */
function startStream(): void {
  stopStream()
  unsubscribe = subscribeRunEvents(
    runId.value,
    (ev) => {
      if (!ev) return
      streamUnavailable.value = false
      streamNote.value = ''
      if (ev.type === 'stage' && ev.stage) currentStage.value = ev.stage
      // 实时状态事件直接同步权威状态，避免中间态过渡时 UI 停留在旧快照
      // （例如取消前显示排队中、实际后端已终态）。
      // 详情快照尚未到达时（bootstrap 先订阅再 reload）暂存，加载完成后补应用。
      if (ev.type === 'task.state') {
        const to = asString(asRecord(ev.payload)?.to) as TaskState
        if (to) {
          if (run.value) run.value = { ...run.value, state: to }
          else pendingState = to
        }
      }
      events.value.push(ev)
      if (events.value.length > MAX_EVENTS) events.value.splice(0, events.value.length - MAX_EVENTS)
      if (isTerminalEvent(ev)) void convergeTerminal()
      else if (isTerminal.value) scheduleTerminalStop()
    },
    () => {
      if (!streamUnavailable.value) streamNote.value = '实时连接中断，浏览器将自动重试'
    },
  )
  armWatchdog()
}

/**
 * 应用暂存的实时状态。
 *
 * bootstrap 先建立 SSE 订阅再拉取详情，历史补发的 task.state 会早于详情到达；
 * 若直接丢弃，页面会停留在旧快照（后端已 degraded、UI 仍显示 queued）。
 */
function applyPendingState(): void {
  if (!pendingState || !run.value) return
  if (run.value.state !== pendingState) run.value = { ...run.value, state: pendingState }
  pendingState = null
}

// 详情加载完成（含终态收敛时的 reload）后立刻补应用暂存的实时状态。
watch(run, () => applyPendingState())

/** 终态收敛：重新拉取运行详情（只做一次），并在历史补发结束后关闭订阅。 */
async function convergeTerminal(): Promise<void> {
  if (terminalReloaded) {
    scheduleTerminalStop()
    return
  }
  terminalReloaded = true
  await runAsync.reload()
  if (isTerminal.value) scheduleTerminalStop()
}

/** 终态运行的「静默期」收尾：每条事件都会重置计时，避免截断历史补发。 */
function scheduleTerminalStop(): void {
  clearTerminalStop()
  terminalTimer = window.setTimeout(() => {
    terminalTimer = null
    stopStream()
    converged.value = true
  }, TERMINAL_GRACE_MS)
}

function clearTerminalStop(): void {
  if (terminalTimer) {
    window.clearTimeout(terminalTimer)
    terminalTimer = null
  }
}

/**
 * 兜底判定：15 秒内一条事件都没收到且任务已是终态
 * → 认为事件流不可用，展示任务快照并停止重试。
 */
function armWatchdog(): void {
  clearWatchdog()
  watchdogTimer = window.setTimeout(() => {
    watchdogTimer = null
    if (events.value.length > 0) return
    if (!isTerminal.value) {
      // 仍在执行且暂无事件：保持订阅，等下一轮再判
      if (unsubscribe) armWatchdog()
      return
    }
    streamUnavailable.value = true
    clearTerminalStop()
    if (unsubscribe) {
      unsubscribe()
      unsubscribe = null
    }
  }, STREAM_WATCHDOG_MS)
}

function clearWatchdog(): void {
  if (watchdogTimer) {
    window.clearTimeout(watchdogTimer)
    watchdogTimer = null
  }
}

watch(
  () => events.value.length,
  async () => {
    if (!autoScroll.value) return
    await nextTick()
    const el = timelineEl.value
    if (el) el.scrollTop = el.scrollHeight
  },
)

/* ------------------------------ 调用轨迹 ------------------------------ */

const callTab = ref<'skills' | 'models'>('skills')
const skillCalls = ref<SkillCall[]>([])
const modelCalls = ref<ModelCall[]>([])
const skillTotal = ref(0)
const modelTotal = ref(0)
const skillPage = ref(1)
const modelPage = ref(1)
const callsLoading = ref(false)
const callsError = ref('')

async function loadSkillCalls(): Promise<void> {
  callsLoading.value = true
  try {
    const res = await api.runSkillCalls(runId.value, { page: skillPage.value, pageSize: CALL_PAGE_SIZE })
    skillCalls.value = res?.items ?? []
    skillTotal.value = res?.total ?? 0
    callsError.value = ''
  } catch (e) {
    skillCalls.value = []
    skillTotal.value = 0
    callsError.value = `技能调用记录加载失败：${errMsg(e)}`
  } finally {
    callsLoading.value = false
  }
}

async function loadModelCalls(): Promise<void> {
  callsLoading.value = true
  try {
    const res = await api.runModelCalls(runId.value, { page: modelPage.value, pageSize: CALL_PAGE_SIZE })
    modelCalls.value = res?.items ?? []
    modelTotal.value = res?.total ?? 0
    callsError.value = ''
  } catch (e) {
    modelCalls.value = []
    modelTotal.value = 0
    callsError.value = `模型推理记录加载失败：${errMsg(e)}`
  } finally {
    callsLoading.value = false
  }
}

function goSkillPage(p: number): void {
  skillPage.value = Math.max(1, p)
  void loadSkillCalls()
}

function goModelPage(p: number): void {
  modelPage.value = Math.max(1, p)
  void loadModelCalls()
}

/* ------------------------------ 操作 ------------------------------ */

const acting = ref(false)
const feedbackOpen = ref(false)
const feedbackText = ref('')
const feedbackError = ref('')

async function bootstrap(): Promise<void> {
  stopStream()
  events.value = []
  currentStage.value = ''
  streamNote.value = ''
  streamUnavailable.value = false
  converged.value = false
  terminalReloaded = false
  skillPage.value = 1
  modelPage.value = 1
  callsError.value = ''
  pendingState = null
  // 先订阅：SSE 建连时会自动补发历史事件（含终态任务），无需再单独拉历史
  startStream()
  await runAsync.reload()
  // 补应用订阅期间先行到达的状态迁移，避免 UI 停留在旧快照。
  applyPendingState()
  // 终态任务若历史事件未通过 SSE 补发，兜底点亮阶段进度条到末端阶段。
  if (isTerminal.value && !currentStage.value) currentStage.value = 'report'
  // 运行不存在 / 无权限时不再让 EventSource 反复重连
  if (notFound.value) {
    stopStream()
    return
  }
  // 终态任务的历史补发由「静默期」收尾；仍在运行的任务保持长连接
  if (isTerminal.value && events.value.length > 0) scheduleTerminalStop()
  await Promise.all([loadSkillCalls(), loadModelCalls()])
}

onMounted(() => {
  void bootstrap()
})

onUnmounted(() => {
  stopStream()
})

watch(runId, () => {
  // 同一路由组件复用（重跑后跳转新 run）时必须整体重置
  void bootstrap()
})

async function doCancel(): Promise<void> {
  acting.value = true
  try {
    const res = await api.cancelRun(runId.value)
    if (res?.cancelled) {
      toastOk('取消请求已提交')
    } else {
      toastWarn('取消请求已发送，但任务可能已进入终态')
    }
  } catch (e) {
    // 409：任务已进入终态，后端拒绝取消。此时 UI 多半还停留在排队中（旧快照），
    // 必须明确告知真实状态而不是笼统报"取消失败"，否则用户会反复点击。
    if (e instanceof ApiError && e.code === 409) {
      toastWarn(`任务已结束，无法取消：${e.message || '状态冲突'}`)
    } else {
      toastError('取消失败', errMsg(e))
    }
  } finally {
    acting.value = false
    // 无论取消成功与否都重新拉取权威状态，避免 UI 停留在旧快照
    // （例如后端已终态而前端仍显示排队中，导致重复点击取消报冲突）。
    // 终态回读后 canCancel 自动为 false，取消按钮随即置灰。
    await runAsync.reload()
  }
}

async function doRetry(): Promise<void> {
  acting.value = true
  try {
    const next = await api.retryRun(runId.value)
    toastOk('已基于相同 Commit 版本重跑', next?.id ? `新运行：${next.id}` : undefined)
    if (next?.id && next.id !== runId.value) {
      await router.push({ name: 'task-detail', params: { id: next.id } })
    } else {
      await bootstrap()
    }
  } catch (e) {
    toastError('重跑失败', errMsg(e))
  } finally {
    acting.value = false
  }
}

function openFeedback(): void {
  feedbackText.value = ''
  feedbackError.value = ''
  feedbackOpen.value = true
}

async function submitFeedback(): Promise<void> {
  const text = feedbackText.value.trim()
  if (!text) {
    feedbackError.value = '人工反馈为必填项'
    return
  }
  if (text.length > FEEDBACK_MAX) {
    feedbackError.value = `反馈内容不能超过 ${FEEDBACK_MAX} 字（当前 ${text.length} 字）`
    return
  }
  acting.value = true
  try {
    const next = await api.feedbackRun(runId.value, text)
    toastOk('已携带人工反馈重跑', next?.id ? `新运行：${next.id}` : undefined)
    feedbackOpen.value = false
    if (next?.id && next.id !== runId.value) {
      await router.push({ name: 'task-detail', params: { id: next.id } })
    } else {
      await bootstrap()
    }
  } catch (e) {
    feedbackError.value = errMsg(e)
  } finally {
    acting.value = false
  }
}

async function openReport(): Promise<void> {
  const id = run.value?.reportId
  if (!id) return
  await router.push({ name: 'report-detail', params: { id } })
}

async function copyPatch(p: Patch): Promise<void> {
  const ok = await copyText(p?.unifiedDiff ?? '')
  if (ok) toastOk('已复制 Patch 内容')
  else toastError('复制失败', '当前浏览器未授予剪贴板权限')
}

function downloadPatch(p: Patch): void {
  const base = (p?.filePath ?? '').split('/').filter(Boolean).pop() ?? 'patch'
  const name = `${p?.repoKey || 'patch'}-${base}.patch`
  downloadText(name, p?.unifiedDiff ?? '')
}
</script>

<template>
  <div class="page">
    <!-- 页头 -->
    <div class="page-header">
      <div style="min-width: 0">
        <div class="row wrap" style="margin-bottom: 6px">
          <button class="btn btn-sm btn-ghost" @click="router.back()">← 返回</button>
          <h1 class="page-title ellipsis">{{ run?.title || '任务执行详情' }}</h1>
          <StatusBadge v-if="run" :state="run.state" />
          <StatusBadge v-if="run?.severity" :severity="run.severity" />
          <span v-if="run" class="badge badge-muted">{{ modeLabel(run.mode) }}</span>
          <span v-if="run?.environment" class="badge badge-info">{{ run.environment }}</span>
          <span v-if="run" class="badge badge-muted">第 {{ run.attempt ?? 1 }} 次尝试</span>
        </div>
        <p class="page-subtitle row wrap">
          <span>运行 ID</span>
          <span class="mono strong">{{ runId }}</span>
          <CopyButton :text="runId" label="复制 ID" title="复制运行 ID" />
          <span v-if="run?.createdAt" class="muted">创建于 {{ formatTime(run.createdAt) }} · {{ formatRelative(run.createdAt) }}</span>
        </p>
      </div>

      <div class="row wrap">
        <button class="btn btn-danger" :disabled="!canCancel || acting" @click="doCancel">取消任务</button>
        <button class="btn" :disabled="acting" @click="doRetry">基于固化上下文重跑</button>
        <button class="btn" :disabled="acting" @click="openFeedback">携带人工反馈重跑</button>
        <button v-if="run?.reportId" class="btn btn-primary" @click="openReport">查看报告</button>
      </div>
    </div>

    <!-- 顶层异常提示 -->
    <div v-if="run?.error" class="alert alert-error">
      <span class="strong nowrap">执行错误</span>
      <span style="word-break: break-word">{{ run.error }}</span>
    </div>
    <div v-if="run?.degraded" class="alert alert-warn">
      <span class="strong nowrap">已降级执行</span>
      <span>部分能力不可用（模型或沙箱），结果仅供参考，请结合报告人工复核。</span>
    </div>
    <div v-if="loadError && !notFound" class="alert alert-error">
      <span class="strong nowrap">运行详情加载失败</span>
      <span class="spacer" />
      <span>{{ loadError }}</span>
      <button class="btn btn-sm" @click="runAsync.reload()">重试</button>
    </div>

    <div v-if="loading && !run" class="card">
      <div class="card-body">
        <div class="skeleton" style="height: 18px; width: 40%" />
        <div class="skeleton" style="margin-top: 10px" />
        <div class="skeleton" style="margin-top: 8px; width: 80%" />
      </div>
    </div>

    <div v-else-if="!run" class="card">
      <div class="card-body">
        <div v-if="notFound" class="alert alert-error" style="margin-bottom: 12px">任务不存在或无权访问</div>
        <EmptyState title="暂无任务数据" hint="运行记录可能尚未创建，或后端接口暂不可用">
          <button class="btn btn-sm" @click="router.back()">返回上一页</button>
        </EmptyState>
      </div>
    </div>

    <template v-else>
      <!-- 阶段进度 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">执行阶段</h3>
          <div class="row small muted">
            <span v-if="currentStage" class="mono">{{ currentStage }}</span>
            <span v-else>等待阶段事件</span>
            <span>·</span>
            <span>已耗时 {{ formatDuration(run.elapsedMs) }}</span>
          </div>
        </div>
        <div class="card-body">
          <StageBar :current-stage="currentStage" :state="run.state" />
        </div>
      </div>

      <!-- 实时事件流 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">实时事件流</h3>
          <div class="row wrap">
            <StatusBadge v-if="streamUnavailable" tone="warn" label="事件流不可用" />
            <StatusBadge v-else-if="converged || (isTerminal && !streamNote)" tone="muted" label="任务已结束 · 实时流已关闭" />
            <StatusBadge v-else-if="realtime.connected" tone="ok" label="实时推送中" :pulse="true" />
            <StatusBadge v-else tone="warn" label="已订阅 · 等待推送" />
            <span class="small muted nowrap">{{ decoratedEvents.length }} 条</span>
            <label class="checkbox small"><input v-model="autoScroll" type="checkbox" />自动滚动</label>
          </div>
        </div>
        <div class="card-body">
          <div v-if="streamUnavailable" class="alert alert-warn" style="margin-bottom: 10px">
            实时事件流不可用，已展示任务当前快照
          </div>
          <div v-else-if="streamNote" class="alert alert-warn" style="margin-bottom: 10px">{{ streamNote }}</div>
          <EmptyState
            v-if="!decoratedEvents.length"
            :title="streamUnavailable ? '事件流不可用' : '暂无事件'"
            :hint="streamUnavailable ? '已回退为任务快照视图，可刷新页面重试' : '任务排队中或事件流尚未推送'"
          />
          <div v-else ref="timelineEl" style="max-height: 440px; overflow: auto">
            <div class="timeline">
              <div v-for="ev in decoratedEvents" :key="ev.key" class="timeline-item">
                <span class="timeline-dot" :class="ev.dot" />
                <div class="timeline-body">
                  <div class="row wrap">
                    <span class="mono small muted nowrap">{{ formatTime(ev.at) }}</span>
                    <span class="badge" :class="`badge-${ev.tone}`">{{ ev.type }}</span>
                    <span v-if="ev.stage" class="timeline-stage">{{ ev.stage }}</span>
                    <span class="spacer" />
                    <span v-if="ev.seq !== undefined" class="small faint nowrap">#{{ ev.seq }}</span>
                  </div>
                  <div class="small">{{ ev.message }}</div>
                  <div v-if="ev.summary" class="small mono muted ellipsis">{{ ev.summary }}</div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 版本锁定 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">版本锁定 · 任务级代码版本固化</h3>
          <span class="small muted">杜绝「最新代码与线上版本错位」，全链路基于锁定 Commit 执行</span>
        </div>
        <div class="card-body">
          <div v-if="run.resolution?.length" class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>仓库</th>
                  <th>分层</th>
                  <th>请求 ref</th>
                  <th>锁定 Commit</th>
                  <th>解析来源</th>
                  <th>匹配分</th>
                  <th>命中依据</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(r, i) in run.resolution" :key="r?.repositoryId || `${r?.repoKey}-${i}`">
                  <td>
                    <div class="strong">{{ r?.repoKey || '-' }}</div>
                    <div class="small muted ellipsis">{{ r?.name || '-' }}</div>
                  </td>
                  <td class="nowrap">{{ layerLabel(r?.layer) }}</td>
                  <td class="mono nowrap">{{ r?.requestedRef || '-' }}</td>
                  <td>
                    <div class="row">
                      <span class="mono" :title="r?.commit || ''">{{ shortCommit(r?.commit) }}</span>
                      <CopyButton v-if="r?.commit" :text="r.commit" label="复制" title="复制完整 Commit" />
                    </div>
                  </td>
                  <td class="nowrap">{{ resolvedFromLabel(r?.resolvedFrom) }}</td>
                  <td class="nowrap">{{ formatPercent(ratio(r?.matchScore)) }}</td>
                  <td>
                    <div v-if="r?.matchedBy?.length" class="row wrap">
                      <span v-for="m in r.matchedBy" :key="m" class="badge badge-info">{{ m }}</span>
                    </div>
                    <span v-else class="muted">-</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <EmptyState v-else title="暂无版本锁定信息" hint="任务可能仍在解析阶段，或该运行未启用仓库解析" />
        </div>
      </div>

      <!-- 根因分析 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">根因分析</h3>
          <div class="row wrap">
            <StatusBadge v-if="run.rootCause?.category" tone="info" :label="categoryLabel(run.rootCause.category)" />
            <StatusBadge v-if="run.rootCause?.severity" :severity="run.rootCause.severity" />
          </div>
        </div>
        <div class="card-body">
          <template v-if="run.rootCause">
            <div class="strong" style="font-size: 15px">{{ run.rootCause.summary || '未给出结论摘要' }}</div>

            <div class="row" style="margin: 12px 0 6px">
              <span class="small muted nowrap">置信度</span>
              <div class="progress" style="flex: 1">
                <span
                  :class="ratio(run.rootCause.confidence) >= 0.7 ? 'ok' : ratio(run.rootCause.confidence) >= 0.4 ? 'warn' : 'error'"
                  :style="{ width: pctWidth(run.rootCause.confidence) }"
                />
              </div>
              <span class="small strong nowrap">{{ formatPercent(ratio(run.rootCause.confidence)) }}</span>
            </div>

            <div v-if="run.rootCause.detail" class="code" style="white-space: pre-wrap">{{ run.rootCause.detail }}</div>

            <div v-if="evidence.length" style="margin-top: 12px">
              <div class="small muted" style="margin-bottom: 6px">证据链</div>
              <ul style="margin: 0; padding-left: 20px">
                <li v-for="(e, i) in evidence" :key="i" class="small">{{ e }}</li>
              </ul>
            </div>

            <div v-if="blastRadius.length" style="margin-top: 12px">
              <div class="small muted" style="margin-bottom: 6px">影响面（blastRadius）</div>
              <div class="row wrap">
                <span v-for="(b, i) in blastRadius" :key="i" class="badge badge-warn">{{ b }}</span>
              </div>
            </div>
          </template>
          <EmptyState v-else title="根因分析尚未产出" hint="任务可能仍在执行或已降级" />
        </div>
      </div>

      <!-- 增量补丁 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">增量补丁</h3>
          <span class="small muted">{{ patches.length }} 个候选补丁</span>
        </div>
        <div class="card-body">
          <EmptyState
            v-if="!patches.length"
            title="暂无补丁"
            hint="未匹配到可自动修复模式，请参考报告建议人工排查"
          />
          <div v-else class="grid" style="gap: 14px">
            <div v-for="(p, i) in patches" :key="p?.id || i" class="card">
              <div class="card-head">
                <div class="row wrap" style="min-width: 0">
                  <span class="badge badge-info">{{ p?.repoKey || '未知仓库' }}</span>
                  <span class="mono ellipsis" :title="p?.filePath || ''">{{ p?.filePath || '-' }}</span>
                </div>
                <div class="row wrap">
                  <StatusBadge tone="warn" :label="riskLabel(p?.risk)" />
                  <StatusBadge tone="muted" :label="patchActionLabel(p?.action)" />
                  <StatusBadge :tone="patchStatusMeta(p?.status).tone" :label="patchStatusMeta(p?.status).label" />
                </div>
              </div>
              <div class="card-body">
                <div v-if="p?.rationale" class="small" style="margin-bottom: 8px">{{ p.rationale }}</div>
                <div class="row wrap small muted" style="margin-bottom: 10px">
                  <span>产生者：{{ p?.producedBy || '-' }}</span>
                  <span>·</span>
                  <span>改动行：{{ rangeText(p?.touchedLines) }}</span>
                </div>
                <DiffView v-if="p?.unifiedDiff" :diff="p.unifiedDiff" />
                <div v-else class="empty">补丁内容为空</div>
                <div class="row wrap" style="margin-top: 10px">
                  <button class="btn btn-sm" :disabled="!p?.unifiedDiff" @click="copyPatch(p)">复制 Patch</button>
                  <button class="btn btn-sm" :disabled="!p?.unifiedDiff" @click="downloadPatch(p)">下载 .patch</button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 沙箱验证 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">沙箱验证</h3>
          <div class="row wrap">
            <StatusBadge
              v-if="run.verification"
              :tone="run.verification.passed ? 'ok' : 'error'"
              :label="run.verification.passed ? '验证通过' : '验证未通过'"
            />
            <span v-if="run.verification" class="small muted">耗时 {{ formatDuration(run.verification.durationMs) }}</span>
          </div>
        </div>
        <div class="card-body">
          <template v-if="run.verification">
            <div v-if="run.verification.degraded" class="alert alert-warn" style="margin-bottom: 12px">
              仅完成静态校验（沙箱命令执行被禁用或无可用校验器）
            </div>
            <div class="row wrap small" style="margin-bottom: 10px">
              <span class="muted">补丁应用结果：</span>
              <span class="strong">{{ applyResultLabel(run.verification.applyResult) }}</span>
              <span class="muted">· 工作区：</span>
              <span class="mono">{{ run.verification.workspace || '-' }}</span>
            </div>
            <div v-if="checks.length" class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th>校验项</th>
                    <th>结果</th>
                    <th>命令</th>
                    <th>耗时</th>
                    <th>说明</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(c, i) in checks" :key="`${c?.name}-${i}`">
                    <td class="nowrap">
                      <div class="strong">{{ checkName(c?.name) }}</div>
                      <div class="small faint mono">{{ c?.name || '-' }}</div>
                    </td>
                    <td class="nowrap">
                      <StatusBadge
                        :tone="c?.skipped ? 'muted' : c?.passed ? 'ok' : 'error'"
                        :label="c?.skipped ? '已跳过' : c?.passed ? '通过' : '未通过'"
                      />
                    </td>
                    <td class="mono small" style="max-width: 320px; word-break: break-all">{{ c?.command || '-' }}</td>
                    <td class="nowrap">{{ formatDuration(c?.durationMs) }}</td>
                    <td style="max-width: 380px">
                      <div v-if="c?.skipped" class="small muted">跳过原因：{{ c?.skipReason || '未说明' }}</div>
                      <details v-if="c?.output">
                        <summary class="small muted" style="cursor: pointer">查看输出</summary>
                        <pre class="code" style="margin-top: 6px; max-height: 220px">{{ c.output }}</pre>
                      </details>
                      <span v-else-if="!c?.skipped" class="muted small">-</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <EmptyState v-else title="暂无校验项" hint="沙箱未执行任何校验器" />
            <details v-if="run.verification.log" style="margin-top: 12px">
              <summary class="small muted" style="cursor: pointer">查看验证日志</summary>
              <pre class="code" style="margin-top: 6px">{{ run.verification.log }}</pre>
            </details>
          </template>
          <EmptyState v-else title="尚未执行沙箱验证" hint="任务可能仍在修复阶段，或沙箱能力不可用" />
        </div>
      </div>

      <!-- 资源消耗与上下文 -->
      <div class="grid grid-2">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">资源消耗</h3>
            <span class="small muted">本次运行</span>
          </div>
          <div class="card-body">
            <div class="grid grid-3" style="gap: 10px">
              <StatTile v-for="t in usageTiles" :key="t.label" :label="t.label" :value="t.value" :hint="t.hint" small />
            </div>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <h3 class="card-title">上下文与追踪</h3>
          </div>
          <div class="card-body">
            <div v-if="pinnedEntries.length" style="margin-bottom: 14px">
              <div class="small muted" style="margin-bottom: 6px">版本固定（pinnedCommits）</div>
              <div class="table-wrap">
                <table class="table">
                  <thead>
                    <tr>
                      <th>仓库</th>
                      <th>Commit</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="p in pinnedEntries" :key="p.key">
                      <td class="strong">{{ p.key }}</td>
                      <td class="mono" :title="p.commit">{{ shortCommit(p.commit) }}</td>
                      <td class="nowrap"><CopyButton :text="p.commit" label="复制" /></td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>

            <div v-if="warnings.length" style="margin-bottom: 14px">
              <div class="small muted" style="margin-bottom: 6px">告警</div>
              <div class="grid" style="gap: 8px">
                <div v-for="(w, i) in warnings" :key="i" class="alert alert-warn">{{ w }}</div>
              </div>
            </div>

            <dl class="kv">
              <dt>请求 ID</dt>
              <dd class="mono ellipsis">{{ run.requestId || '-' }}</dd>
              <dt>幂等键</dt>
              <dd class="mono ellipsis">{{ run.idempotencyKey || '-' }}</dd>
              <dt>回调地址</dt>
              <dd class="ellipsis" :title="run.callbackUrl || ''">{{ run.callbackUrl || '-' }}</dd>
              <dt>创建时间</dt>
              <dd>{{ formatTime(run.createdAt) }}</dd>
              <dt>开始时间</dt>
              <dd>{{ run.startedAt ? formatTime(run.startedAt) : '-' }}</dd>
              <dt>结束时间</dt>
              <dd>{{ run.endedAt ? formatTime(run.endedAt) : '-' }}</dd>
              <dt>总耗时</dt>
              <dd>{{ formatDuration(run.elapsedMs) }}</dd>
              <dt>任务 ID</dt>
              <dd class="mono ellipsis">{{ run.taskId || '-' }}</dd>
            </dl>
          </div>
        </div>
      </div>

      <!-- 调用轨迹 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">调用轨迹</h3>
          <div class="row small muted">
            <span v-if="callsLoading">加载中…</span>
            <button class="btn btn-sm" @click="callTab === 'skills' ? loadSkillCalls() : loadModelCalls()">刷新</button>
          </div>
        </div>
        <div class="card-body" style="padding-top: 0">
          <div class="tabs" style="margin-bottom: 12px">
            <button class="tab" :class="{ active: callTab === 'skills' }" @click="callTab = 'skills'">
              技能调用（{{ skillTotal }}）
            </button>
            <button class="tab" :class="{ active: callTab === 'models' }" @click="callTab = 'models'">
              模型推理（{{ modelTotal }}）
            </button>
          </div>

          <div v-if="callsError" class="alert alert-error" style="margin-bottom: 10px">{{ callsError }}</div>

          <template v-if="callTab === 'skills'">
            <div v-if="skillCalls.length" class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th>技能</th>
                    <th>版本</th>
                    <th>阶段</th>
                    <th>状态</th>
                    <th>尝试</th>
                    <th>耗时</th>
                    <th>兜底</th>
                    <th>错误</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(c, i) in skillCalls" :key="c?.id || i">
                    <td class="strong nowrap">{{ c?.skill || '-' }}</td>
                    <td class="mono nowrap">{{ c?.version || '-' }}</td>
                    <td class="mono nowrap">{{ c?.stage || '-' }}</td>
                    <td class="nowrap">
                      <StatusBadge :tone="callStatusMeta(c?.status).tone" :label="callStatusMeta(c?.status).label" />
                    </td>
                    <td class="nowrap">{{ c?.attempts ?? '-' }}</td>
                    <td class="nowrap">{{ formatDuration(c?.durationMs) }}</td>
                    <td class="nowrap">
                      <StatusBadge v-if="c?.fromFallback" tone="degraded" label="兜底" />
                      <span v-else class="muted">-</span>
                    </td>
                    <td class="small muted" style="max-width: 320px" :title="c?.error || ''">{{ truncate(c?.error, 80) || '-' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <EmptyState v-else title="暂无技能调用记录" hint="任务尚未进入技能调度阶段" />
            <Pagination
              v-if="skillTotal > CALL_PAGE_SIZE"
              :page="skillPage"
              :page-size="CALL_PAGE_SIZE"
              :total="skillTotal"
              @change="goSkillPage"
            />
          </template>

          <template v-else>
            <div v-if="modelCalls.length" class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th>模型</th>
                    <th>层级</th>
                    <th>状态</th>
                    <th>Tokens</th>
                    <th>耗时</th>
                    <th>Schema</th>
                    <th>兜底自</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(c, i) in modelCalls" :key="c?.id || i">
                    <td class="nowrap">{{ c?.provider || '-' }}</td>
                    <td class="mono nowrap">{{ c?.model || '-' }}</td>
                    <td class="nowrap">{{ tierLabel(c?.tier) }}</td>
                    <td class="nowrap">
                      <StatusBadge :tone="callStatusMeta(c?.status).tone" :label="callStatusMeta(c?.status).label" />
                    </td>
                    <td class="nowrap">
                      <span :title="`prompt ${formatNumber(c?.promptTokens ?? 0)} / completion ${formatNumber(c?.completionTokens ?? 0)}`">
                        {{ formatCompact(c?.totalTokens ?? 0) }}
                      </span>
                    </td>
                    <td class="nowrap">{{ formatDuration(c?.durationMs) }}</td>
                    <td class="nowrap">
                      <StatusBadge :tone="c?.schemaValid ? 'ok' : 'error'" :label="c?.schemaValid ? '合法' : '不合规'" />
                    </td>
                    <td class="mono nowrap" :title="c?.error || ''">{{ c?.fallbackFrom || '-' }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <EmptyState v-else title="暂无模型调用记录" hint="任务尚未触发模型推理" />
            <Pagination
              v-if="modelTotal > CALL_PAGE_SIZE"
              :page="modelPage"
              :page-size="CALL_PAGE_SIZE"
              :total="modelTotal"
              @change="goModelPage"
            />
          </template>
        </div>
      </div>
    </template>

    <!-- 携带人工反馈重跑 -->
    <AppModal v-if="feedbackOpen" title="携带人工反馈重跑" @close="feedbackOpen = false">
      <div class="field">
        <label class="field-label">人工反馈<span class="req">*</span></label>
        <textarea
          v-model="feedbackText"
          class="textarea"
          :maxlength="FEEDBACK_MAX"
          placeholder="例如：根因应为 Redis 连接池耗尽，而非空指针；请重点检查 gateway 层的超时配置。"
        />
        <span class="field-hint">
          反馈会注入根因推理与补丁合成阶段的模型提示，用于修复不收敛时的定向迭代。
          最多 {{ FEEDBACK_MAX }} 字，当前 {{ feedbackText.length }} 字。
        </span>
        <span v-if="feedbackError" class="field-error">{{ feedbackError }}</span>
      </div>
      <template #footer>
        <button class="btn" :disabled="acting" @click="feedbackOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="acting || !feedbackText.trim()" @click="submitFeedback">
          {{ acting ? '提交中…' : '提交并重跑' }}
        </button>
      </template>
    </AppModal>
  </div>
</template>
