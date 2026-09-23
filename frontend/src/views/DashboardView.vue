<script setup lang="ts">
/**
 * 运行监控台（DashboardView）
 *
 * 组件职责：
 * 1. 聚合 api.observability()（运行汇总）与 api.engineStats()（引擎实时统计）输出核心指标卡；
 *    engineStats 可能 404 / 403 / 500，此时降级为「由汇总数据推导」并在卡片 hint 中显式说明。
 * 2. 展示任务状态分布、根因分类分布、近 7 天任务趋势（本地聚合最近 200 条运行）、资源与缓存效率。
 * 3. 展示最近失败/降级运行（可跳转任务详情）与全局实时事件流（断线时告警、新事件自动滚到底部）。
 * 4. 支持手动刷新与 10s 自动轮询（开关控制，卸载时清理定时器）。
 *
 * 降级原则：任何接口失败都不阻塞渲染，以 .alert / .empty 占位替代。
 */
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import * as echarts from 'echarts'
import { ApiError, api } from '@/api/client'
import type { AgentEvent, EngineStats, ObservabilitySummary, RunBrief, TaskRun } from '@/types'
import { useAuthStore } from '@/stores/auth'
import { useRealtimeStore } from '@/stores/realtime'
import { toastError } from '@/composables/useToast'
import {
  categoryLabel,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatRelative,
  formatTime,
  stateMeta,
  truncate,
} from '@/utils/format'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import BarList from '@/components/BarList.vue'
import EmptyState from '@/components/EmptyState.vue'

const router = useRouter()
const auth = useAuthStore()
const realtime = useRealtimeStore()

/* ------------------------------ 状态 ------------------------------ */

/** 运行中状态集合（用于从汇总分布推导运行中任务数）。 */
const ACTIVE_STATES = ['queued', 'analyzing', 'repairing', 'verifying']

const summary = ref<ObservabilitySummary | null>(null)
const engine = ref<EngineStats | null>(null)
const summaryError = ref('')
/** engineStats 不可用时为 true，卡片 hint 提示「由汇总数据推导」。 */
const engineFallback = ref(false)
const runs = ref<TaskRun[]>([])
const runsError = ref('')
const loading = ref(false)
const autoRefresh = ref(true)
const lastLoadedAt = ref('')
let timer: number | null = null

function errText(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === 404) return '接口未实现或未开放（404）'
    if (e.code === 403) return '当前账号无权访问该指标（403）'
    return `${e.message}（code=${e.code}）`
  }
  return e instanceof Error ? e.message : '未知错误'
}

/* ------------------------------ 数据加载 ------------------------------ */

async function loadSummary(verbose = false): Promise<void> {
  try {
    summary.value = await api.observability()
    summaryError.value = ''
  } catch (e) {
    summary.value = null
    summaryError.value = errText(e)
    if (verbose) toastError('运行汇总加载失败', summaryError.value)
  }
  try {
    engine.value = await api.engineStats()
    engineFallback.value = false
  } catch {
    // engineStats 属于可选增强接口：失败即回退到 summary 推导，不打扰用户。
    engine.value = null
    engineFallback.value = true
  }
}

async function loadRuns(): Promise<void> {
  try {
    const res = await api.listRuns({ pageSize: 200 })
    runs.value = res.items ?? []
    runsError.value = ''
  } catch (e) {
    runs.value = []
    runsError.value = errText(e)
  }
}

async function loadAll(verbose = false): Promise<void> {
  loading.value = true
  try {
    await Promise.all([loadSummary(verbose), loadRuns()])
    lastLoadedAt.value = new Date().toISOString()
  } finally {
    loading.value = false
  }
}

function startTimer(): void {
  stopTimer()
  timer = window.setInterval(() => void loadAll(false), 10_000)
}

function stopTimer(): void {
  if (timer !== null) {
    window.clearInterval(timer)
    timer = null
  }
}

function manualRefresh(): void {
  void loadAll(true)
}

/* ------------------------------ 指标卡 ------------------------------ */

const tenantLabel = computed(() => auth.tenant?.name ?? auth.subject?.tenantId ?? '未知租户')

const todayCount = computed(() => {
  const today = dayKey(new Date())
  return runs.value.filter((r) => dayKey(new Date(r.createdAt)) === today).length
})

const runningCount = computed(() => {
  if (engine.value) return engine.value.running
  const dist = summary.value?.stateDist ?? {}
  return ACTIVE_STATES.reduce((acc, s) => acc + Number(dist[s] ?? 0), 0)
})

const runningHint = computed(() => {
  if (engine.value) {
    return engine.value.queued > 0 ? `队列中 ${formatNumber(engine.value.queued)} 个` : '来自引擎实时统计'
  }
  return '由汇总数据推导'
})

const todayHint = computed(() => {
  if (runsError.value) return `明细不可用，累计 ${formatNumber(summary.value?.tasksTotal)} 个任务`
  return '按最近 200 条运行的创建时间统计'
})

const fixRate = computed(() => summary.value?.fixRate ?? engine.value?.successRate ?? 0)

const fixRateHint = computed(() => {
  const s = summary.value
  if (s) {
    return `成功 ${formatNumber(s.runsSucceeded)} / 失败 ${formatNumber(s.runsFailed)} / 待复核 ${formatNumber(s.runsNeedsReview)}`
  }
  return engineFallback.value ? '由汇总数据推导' : '汇总数据不可用'
})

const avgElapsed = computed(() => engine.value?.avgElapsedMs ?? summary.value?.avgElapsedMs)

const avgElapsedHint = computed(() => {
  if (!engine.value && engineFallback.value) return '由汇总数据推导'
  return '任务从接受到归档的平均墙钟耗时'
})

const modelTokens = computed(() => summary.value?.modelTokens ?? 0)

const tokenHint = computed(() => `模型调用 ${formatNumber(summary.value?.modelCalls)} 次 · 兜底 ${formatNumber(summary.value?.modelFallbacks)} 次`)

const hasAnyData = computed(() => {
  const s = summary.value
  if (!s) return runs.value.length > 0
  const distTotal = Object.values(s.stateDist ?? {}).reduce((acc, v) => acc + Number(v ?? 0), 0)
  return s.tasksTotal > 0 || s.runsTotal > 0 || distTotal > 0 || runs.value.length > 0
})

/* ------------------------------ 分布 ------------------------------ */

const stateLabelMap = computed<Record<string, string>>(() =>
  Object.fromEntries(Object.keys(summary.value?.stateDist ?? {}).map((k) => [k, stateMeta(k).label])),
)

const categoryLabelMap = computed<Record<string, string>>(() =>
  Object.fromEntries(Object.keys(summary.value?.categoryDist ?? {}).map((k) => [k, categoryLabel(k)])),
)

/* ------------------------------ 资源与缓存 ------------------------------ */

const cacheHits = computed(() => summary.value?.cacheHits ?? 0)
const repoSwitches = computed(() => engine.value?.repoSwitches ?? summary.value?.repoSwitches ?? 0)
const skillFailureRate = computed(() => summary.value?.skillFailureRate ?? 0)
const modelFallbacks = computed(() => summary.value?.modelFallbacks ?? 0)
const degradedRuns = computed(() => engine.value?.degradedRuns ?? summary.value?.runsDegraded ?? 0)

const skillCalls = computed(() => (summary.value?.topSkills ?? []).map((s) => ({ label: s.skill, value: s.calls })))
const skillFailures = computed(() =>
  (summary.value?.topSkills ?? []).filter((s) => s.failures > 0).map((s) => ({ label: s.skill, value: s.failures })),
)

const repoSwitchHint = computed(() => {
  if (engine.value) return '引擎统计的多仓库切换次数'
  return engineFallback.value ? '由汇总数据推导' : '汇总数据不可用'
})

/* ------------------------------ 趋势图（本地聚合） ------------------------------ */

interface TrendPoint {
  key: string
  label: string
  total: number
  succeeded: number
  failed: number
  degraded: number
}

const TREND_SERIES = [
  { key: 'total', name: '任务总数', color: '#4f7cff' },
  { key: 'succeeded', name: '修复成功', color: '#2fbf87' },
  { key: 'failed', name: '失败', color: '#e5555f' },
  { key: 'degraded', name: '已降级', color: '#a97ae0' },
] as const

function dayKey(d: Date): string {
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
}

/** 用列表接口兜底：按本地时区把最近 200 条运行聚合成 7 天 × 4 条序列。 */
const trendDays = computed<TrendPoint[]>(() => {
  const now = new Date()
  const days: TrendPoint[] = []
  for (let i = 6; i >= 0; i--) {
    const d = new Date(now.getFullYear(), now.getMonth(), now.getDate() - i)
    days.push({
      key: dayKey(d),
      label: `${d.getMonth() + 1}/${d.getDate()}`,
      total: 0,
      succeeded: 0,
      failed: 0,
      degraded: 0,
    })
  }
  const index = new Map(days.map((d) => [d.key, d]))
  for (const run of runs.value) {
    const bucket = index.get(dayKey(new Date(run.createdAt)))
    if (!bucket) continue
    bucket.total += 1
    const state = realtime.runStates[run.id] ?? run.state
    if (state === 'succeeded') bucket.succeeded += 1
    else if (state === 'failed') bucket.failed += 1
    else if (state === 'degraded') bucket.degraded += 1
  }
  return days
})

const trendUnavailable = computed(() => runs.value.length === 0)

const trendRef = ref<HTMLDivElement | null>(null)
let chart: ReturnType<typeof echarts.init> | null = null

/** 读取主题色令牌，保证图表跟随明暗主题。 */
function cssVar(name: string, fallback: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback
}

function renderChart(): void {
  if (!chart) return
  const days = trendDays.value
  const axisColor = cssVar('--border', '#252c38')
  const labelColor = cssVar('--text-dim', '#9aa5b8')
  chart.setOption({
    grid: { left: 6, right: 12, top: 30, bottom: 2, containLabel: true },
    tooltip: { trigger: 'axis' },
    legend: {
      top: 0,
      itemWidth: 12,
      itemHeight: 8,
      textStyle: { color: labelColor, fontSize: 11 },
    },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      data: days.map((d) => d.label),
      axisLine: { lineStyle: { color: axisColor } },
      axisLabel: { color: labelColor, fontSize: 11 },
    },
    yAxis: {
      type: 'value',
      minInterval: 1,
      splitLine: { lineStyle: { color: axisColor } },
      axisLabel: { color: labelColor, fontSize: 11 },
    },
    series: TREND_SERIES.map((s) => ({
      name: s.name,
      type: 'line' as const,
      smooth: true,
      showSymbol: false,
      lineStyle: { width: 2, color: s.color },
      itemStyle: { color: s.color },
      data: days.map((d) => d[s.key]),
    })),
  })
}

function handleResize(): void {
  chart?.resize()
}

/* ------------------------------ 实时事件流 ------------------------------ */

const eventBoxRef = ref<HTMLDivElement | null>(null)

/** 事件流按时间正序展示（最新在底部），便于「自动滚到底部」看到最新事件。 */
const liveEvents = computed<AgentEvent[]>(() => [...realtime.recent(30)].reverse())

function eventTone(type: string): string {
  if (type === 'task.terminal') return 'ok'
  if (type.includes('fail') || type === 'error') return 'error'
  if (type === 'verification' || type === 'patch.proposed') return 'warn'
  if (type.startsWith('skill') || type.startsWith('model')) return 'info'
  if (type === 'repo.switch') return 'degraded'
  return 'muted'
}

/* ------------------------------ 最近异常运行 ------------------------------ */

const recentFailures = computed<RunBrief[]>(() => summary.value?.recentFailures ?? [])

function openRun(runId: string): void {
  if (!runId) return
  void router.push({ name: 'task-detail', params: { id: runId } })
}

function goCreate(): void {
  void router.push({ name: 'task-new' })
}

/* ------------------------------ 生命周期 ------------------------------ */

watch(autoRefresh, (on) => {
  if (on) startTimer()
  else stopTimer()
})

watch(trendDays, () => renderChart())

watch(
  () => realtime.events.length,
  async () => {
    await nextTick()
    const el = eventBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  },
)

onMounted(async () => {
  if (trendRef.value) chart = echarts.init(trendRef.value)
  window.addEventListener('resize', handleResize)
  await loadAll(false)
  // 等 DOM 更新后再渲染，避免图表在 v-show 隐藏状态下以 0 宽初始化。
  await nextTick()
  renderChart()
  chart?.resize()
  if (autoRefresh.value) startTimer()
})

onUnmounted(() => {
  stopTimer()
  window.removeEventListener('resize', handleResize)
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="page">
    <!-- 页头 -->
    <div class="page-header">
      <div>
        <h1 class="page-title">运行监控台</h1>
        <div class="page-subtitle row wrap" style="gap: 8px">
          <span>租户 <span class="strong">{{ tenantLabel }}</span></span>
          <span class="muted">·</span>
          <span class="badge" :class="realtime.connected ? 'badge-ok' : 'badge-muted'">
            <i class="dot" :class="{ pulse: realtime.connected }" />
            {{ realtime.connected ? '事件流已连接' : '事件流断开' }}
          </span>
          <span class="muted">·</span>
          <span class="muted">{{ lastLoadedAt ? `最近更新 ${formatTime(lastLoadedAt)}` : '加载中…' }}</span>
        </div>
      </div>
      <div class="row wrap">
        <label class="checkbox" title="每 10 秒自动拉取一次运行汇总">
          <input v-model="autoRefresh" type="checkbox" />
          10s 自动刷新
        </label>
        <button class="btn btn-sm" :disabled="loading" @click="manualRefresh">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
        <button class="btn btn-sm btn-primary" @click="goCreate">发起排查</button>
      </div>
    </div>

    <!-- 汇总接口不可用 -->
    <div v-if="summaryError" class="alert alert-error">
      <div>
        <div class="strong">运行汇总接口不可用</div>
        <div class="small">{{ summaryError }}；页面已降级为「仅本地运行明细」模式，趋势与异常列表可能为空。</div>
      </div>
      <div class="spacer" />
      <button class="btn btn-sm" @click="manualRefresh">重试</button>
    </div>

    <!-- 指标卡 -->
    <div class="grid grid-5">
      <StatTile label="运行中任务数" :value="formatNumber(runningCount)" :hint="runningHint" tone="info" />
      <StatTile label="今日任务总数" :value="formatNumber(todayCount)" :hint="todayHint" />
      <StatTile
        label="修复成功率"
        :value="formatPercent(fixRate)"
        :hint="fixRateHint"
        :tone="fixRate >= 0.8 ? 'ok' : fixRate >= 0.5 ? 'warn' : 'error'"
      />
      <StatTile label="平均耗时" :value="formatDuration(avgElapsed)" :hint="avgElapsedHint" />
      <StatTile label="模型 Token 消耗" :value="formatCompact(modelTokens)" :hint="tokenHint" tone="degraded" />
    </div>

    <!-- 全局空态 -->
    <div v-if="!loading && !hasAnyData" class="card">
      <div class="card-body">
        <EmptyState title="尚无任务数据" hint="点击「发起排查」提交第一个报错分析任务">
          <button class="btn btn-primary" @click="goCreate">发起排查</button>
        </EmptyState>
      </div>
    </div>

    <!-- 分布 -->
    <div class="grid grid-2">
      <div class="card">
        <div class="card-head">
          <h2 class="card-title">任务状态分布</h2>
          <span class="small muted">共 {{ formatNumber(Object.values(summary?.stateDist ?? {}).reduce((a, b) => a + Number(b ?? 0), 0)) }} 次运行</span>
        </div>
        <div class="card-body">
          <BarList v-if="summary && Object.keys(summary.stateDist ?? {}).length" :data="summary.stateDist" :label-map="stateLabelMap" />
          <div v-else class="empty">暂无状态分布数据</div>
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2 class="card-title">根因分类分布</h2>
          <span class="small muted">来自已定位根因的运行</span>
        </div>
        <div class="card-body">
          <BarList
            v-if="summary && Object.keys(summary.categoryDist ?? {}).length"
            :data="summary.categoryDist"
            :label-map="categoryLabelMap"
            tone="var(--info)"
          />
          <div v-else class="empty">暂无根因分类数据</div>
        </div>
      </div>
    </div>

    <!-- 趋势 + 资源 -->
    <div class="grid grid-2">
      <div class="card">
        <div class="card-head">
          <h2 class="card-title">近 7 天任务趋势</h2>
          <span class="small muted">基于最近 200 条运行本地聚合</span>
        </div>
        <div class="card-body">
          <div v-if="trendUnavailable" class="empty">
            趋势数据不可用{{ runsError ? `：${runsError}` : '（暂无运行明细）' }}
          </div>
          <div v-show="!trendUnavailable" ref="trendRef" class="chart" />
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2 class="card-title">资源与缓存效率</h2>
          <span v-if="engineFallback" class="badge badge-warn">引擎统计不可用</span>
        </div>
        <div class="card-body" style="display: flex; flex-direction: column; gap: 14px">
          <div class="grid grid-2">
            <StatTile label="缓存命中次数" :value="formatNumber(cacheHits)" hint="源码/解析结果复用" small />
            <StatTile label="仓库切换次数" :value="formatNumber(repoSwitches)" :hint="repoSwitchHint" small />
            <StatTile
              label="技能失败率"
              :value="formatPercent(skillFailureRate)"
              :hint="`Skill 调用 ${formatNumber(summary?.skillCalls)} / 失败 ${formatNumber(summary?.skillFailures)}`"
              :tone="skillFailureRate > 0.1 ? 'error' : 'ok'"
              small
            />
            <StatTile
              label="模型兜底次数"
              :value="formatNumber(modelFallbacks)"
              :hint="`降级运行 ${formatNumber(degradedRuns)} 次`"
              tone="degraded"
              small
            />
          </div>

          <div class="grid grid-2">
            <div>
              <div class="row-between" style="margin-bottom: 8px">
                <span class="small strong">Skill 调用次数</span>
                <span class="small faint">Top {{ skillCalls.length }}</span>
              </div>
              <BarList v-if="skillCalls.length" :data="skillCalls" />
              <div v-else class="muted small">暂无 Skill 调用数据</div>
            </div>
            <div>
              <div class="row-between" style="margin-bottom: 8px">
                <span class="small strong">Skill 失败次数</span>
                <span class="small faint">仅统计失败 &gt; 0</span>
              </div>
              <BarList v-if="skillFailures.length" :data="skillFailures" tone="var(--error)" />
              <div v-else class="muted small">无 Skill 失败记录</div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 异常运行 + 实时事件流 -->
    <div class="grid grid-2">
      <div class="card">
        <div class="card-head">
          <h2 class="card-title">最近失败 / 降级运行</h2>
          <span class="small muted">{{ recentFailures.length }} 条</span>
        </div>
        <div class="table-wrap">
          <table v-if="recentFailures.length" class="table">
            <thead>
              <tr>
                <th>标题</th>
                <th>状态</th>
                <th>定级</th>
                <th>时间</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="f in recentFailures" :key="f.runId" class="clickable" @click="openRun(f.runId)">
                <td style="max-width: 280px">
                  <div class="ellipsis" :title="f.summary || f.title">{{ f.title || '(无标题)' }}</div>
                  <div v-if="f.summary" class="small faint ellipsis" :title="f.summary">{{ truncate(f.summary, 60) }}</div>
                </td>
                <td><StatusBadge :state="f.state" /></td>
                <td><StatusBadge :severity="f.severity" /></td>
                <td class="nowrap small muted" :title="formatTime(f.at)">{{ formatRelative(f.at) }}</td>
              </tr>
            </tbody>
          </table>
          <div v-else class="empty">暂无失败或降级的运行记录</div>
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h2 class="card-title">实时事件流</h2>
          <span class="small muted">最近 {{ liveEvents.length }} 条 · 最新在底部</span>
        </div>
        <div class="card-body" style="display: flex; flex-direction: column; gap: 10px">
          <div v-if="!realtime.connected" class="alert alert-warn">
            <span class="dot pulse" />
            <span>事件流断开，正在重连…（页面指标仍按自动刷新周期更新）</span>
          </div>
          <div ref="eventBoxRef" class="event-box">
            <div v-if="!liveEvents.length" class="empty">暂无实时事件，等待任务提交或状态变更</div>
            <div v-for="ev in liveEvents" :key="`${ev.seq}-${ev.at}-${ev.type}`" class="event-row">
              <span class="small faint nowrap" :title="formatTime(ev.at)">{{ formatTime(ev.at).slice(11) }}</span>
              <StatusBadge :label="ev.type" :tone="eventTone(ev.type)" />
              <span v-if="ev.stage" class="badge badge-muted">{{ ev.stage }}</span>
              <span class="small ellipsis" :title="ev.message">{{ ev.message || ev.runId || '-' }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.event-box {
  max-height: 300px;
  min-height: 180px;
  overflow-y: auto;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--code-bg);
  padding: 8px 10px;
}

.event-row {
  display: grid;
  grid-template-columns: 62px auto auto minmax(0, 1fr);
  gap: 8px;
  align-items: center;
  padding: 4px 0;
  border-bottom: 1px dashed var(--border);
}

.event-row:last-child {
  border-bottom: none;
}
</style>
