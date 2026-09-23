<script setup lang="ts">
/**
 * 任务中心（TasksView）
 *
 * 组件职责：
 * 1. 分页浏览运行（api.listRuns），支持关键字搜索、9 种状态的筛选、以及「只看待复核」快捷筛选。
 * 2. 表格展示标题/模式/环境、状态、定级、锁定仓库数、补丁数、Token 消耗、耗时与创建时间。
 * 3. 用 realtime.runStates 覆盖列表中的状态徽标，无需刷新即可反映任务状态变化。
 * 4. 顶部给出当前页的本地统计（总数/成功/待复核/失败/降级），加载中显示骨架，失败显示告警，空结果显示空态。
 *
 * 说明：状态筛选需要参与请求，这里复用 usePaged 管理分页/关键字，状态作为闭包变量注入 loader，
 * 变更状态时重置到第一页后重新加载。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { TaskRun, TaskState } from '@/types'
import { usePaged } from '@/composables/useAsync'
import { useRealtimeStore } from '@/stores/realtime'
import {
  formatCompact,
  formatDuration,
  formatNumber,
  formatRelative,
  formatTime,
  modeLabel,
  stateMeta,
} from '@/utils/format'
import StatusBadge from '@/components/StatusBadge.vue'
import Pagination from '@/components/Pagination.vue'
import EmptyState from '@/components/EmptyState.vue'

const router = useRouter()
const realtime = useRealtimeStore()

/** 状态筛选项（标签复用 format.ts 的 stateMeta）。 */
const STATE_ORDER: TaskState[] = [
  'queued',
  'analyzing',
  'repairing',
  'verifying',
  'succeeded',
  'needs_review',
  'failed',
  'cancelled',
  'degraded',
]
const STATE_OPTIONS = STATE_ORDER.map((s) => ({ value: s, label: stateMeta(s).label }))

const stateFilter = ref('')
const keywordInput = ref('')
const listError = ref('')

function errText(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === 401) return '认证失效，请重新登录（401）'
    if (e.code === 403) return '无权访问该租户的任务列表（403）'
    if (e.code === 404) return '任务列表接口未开放（404）'
    return `${e.message}（code=${e.code}）`
  }
  return e instanceof Error ? e.message : '列表加载失败'
}

const { items: runItems, total, page, pageSize, loading, load, go, search } = usePaged<TaskRun>(
  async (p, ps, kw) => {
    try {
      const res = await api.listRuns({
        page: p,
        pageSize: ps,
        keyword: kw || undefined,
        state: stateFilter.value || undefined,
      })
      listError.value = ''
      return { items: res.items ?? [], total: res.total ?? 0 }
    } catch (e) {
      // 失败降级：清空列表并保留错误文案，由模板渲染 .alert-error。
      listError.value = errText(e)
      return { items: [], total: 0 }
    }
  },
  20,
)

/* ------------------------------ 交互 ------------------------------ */

function onSearch(): void {
  search(keywordInput.value.trim())
}

function applyState(next: string): void {
  stateFilter.value = next
  page.value = 1
  void load()
}

function toggleNeedsReview(): void {
  applyState(stateFilter.value === 'needs_review' ? '' : 'needs_review')
}

function clearFilters(): void {
  keywordInput.value = ''
  applyState('')
}

function refresh(): void {
  void load()
}

function goCreate(): void {
  void router.push({ name: 'task-new' })
}

function openRun(run: TaskRun): void {
  void router.push({ name: 'task-detail', params: { id: run.id } })
}

/* ------------------------------ 展示计算 ------------------------------ */

/** 实时状态优先：事件流推送的 task.state 覆盖列表中的快照状态。 */
function runState(run: TaskRun): TaskState {
  return realtime.runStates[run.id] ?? run.state
}

function repoCount(run: TaskRun): number {
  return run.resolution?.length ?? run.repoIds?.length ?? 0
}

function patchCount(run: TaskRun): number {
  return run.patches?.length ?? 0
}

function tokens(run: TaskRun): string {
  return formatCompact(run.usage?.totalTokens ?? 0)
}

function elapsed(run: TaskRun): string {
  if (run.elapsedMs && run.elapsedMs > 0) return formatDuration(run.elapsedMs)
  return ['queued', 'analyzing', 'repairing', 'verifying'].includes(runState(run)) ? '运行中' : '-'
}

/** 当前页本地统计（实时状态参与，与表格展示保持一致）。 */
const pageStats = computed(() => {
  const acc = { succeeded: 0, needs_review: 0, failed: 0, degraded: 0 }
  for (const run of runItems.value) {
    const s = runState(run)
    if (s === 'succeeded') acc.succeeded += 1
    else if (s === 'needs_review') acc.needs_review += 1
    else if (s === 'failed') acc.failed += 1
    else if (s === 'degraded') acc.degraded += 1
  }
  return acc
})

const isEmpty = computed(() => !loading.value && runItems.value.length === 0)

/* ------------------------------ 列表自刷新 ------------------------------ */

/**
 * 全局 WebSocket 只推送连接建立之后的事件：页面打开前或断线期间发生的状态迁移
 * 永远不会回补，列表会长期停留在 queued（进而出现"点取消却报任务已终态"的矛盾）。
 * 这里对「本页存在未终态任务」的情况做轻量轮询兜底。
 */
const ACTIVE_STATES: TaskState[] = ['queued', 'analyzing', 'repairing', 'verifying']
const POLL_INTERVAL_MS = 8000

/** 本页是否存在未终态任务：据此开关轮询，避免任务全部结束后仍空转请求。 */
const hasActive = computed(() =>
  runItems.value.some((run) => ACTIVE_STATES.includes(runState(run))),
)

let pollTimer: number | null = null
/** 轮询触发的刷新标记为 true：避免"加载中"徽标每 8 秒闪烁一次。 */
const autoRefreshing = ref(false)

function stopPolling(): void {
  if (pollTimer !== null) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPolling(): void {
  if (pollTimer !== null) return
  pollTimer = window.setInterval(() => {
    // 页面不可见时不打扰后端；切回前台由 visibilitychange 立即补一次。
    if (document.hidden) return
    autoRefreshing.value = true
    void load().finally(() => {
      autoRefreshing.value = false
    })
  }, POLL_INTERVAL_MS)
}

// 出现进行中任务则开启轮询，全部结束后自动停止。
watch(hasActive, (active) => (active ? startPolling() : stopPolling()))
// 翻页 / 筛选 / 搜索后若仍有进行中任务，确保轮询处于开启状态。
watch(runItems, () => {
  if (hasActive.value) startPolling()
})

function onVisibility(): void {
  if (!document.hidden && hasActive.value) void load()
}

onMounted(() => {
  if (hasActive.value) startPolling()
  document.addEventListener('visibilitychange', onVisibility)
})

onUnmounted(() => {
  stopPolling()
  document.removeEventListener('visibilitychange', onVisibility)
})
</script>

<template>
  <div class="page">
    <!-- 页头 -->
    <div class="page-header">
      <div>
        <h1 class="page-title">任务中心</h1>
        <div class="page-subtitle">
          共 {{ formatNumber(total) }} 个运行
          <template v-if="stateFilter"> · 已按「{{ stateMeta(stateFilter).label }}」筛选</template>
          <template v-if="keywordInput"> · 关键字「{{ keywordInput }}」</template>
        </div>
      </div>
      <div class="row wrap">
        <span v-if="loading && !autoRefreshing" class="badge badge-info"><i class="dot pulse" />加载中</span>
        <button class="btn btn-sm" :disabled="loading" @click="refresh">刷新</button>
        <button class="btn btn-sm btn-primary" @click="goCreate">发起排查</button>
      </div>
    </div>

    <!-- 当前页本地统计 -->
    <div class="grid grid-5">
      <div class="stat">
        <div class="stat-label">当前筛选结果</div>
        <div class="stat-value sm">{{ formatNumber(total) }}</div>
        <div class="stat-hint">本页 {{ runItems.length }} 条</div>
      </div>
      <div class="stat">
        <div class="stat-label">修复成功</div>
        <div class="stat-value sm" style="color: var(--ok)">{{ formatNumber(pageStats.succeeded) }}</div>
        <div class="stat-hint">本页统计</div>
      </div>
      <div class="stat">
        <div class="stat-label">待复核</div>
        <div class="stat-value sm" style="color: var(--warn)">{{ formatNumber(pageStats.needs_review) }}</div>
        <div class="stat-hint">本页统计</div>
      </div>
      <div class="stat">
        <div class="stat-label">失败</div>
        <div class="stat-value sm" style="color: var(--error)">{{ formatNumber(pageStats.failed) }}</div>
        <div class="stat-hint">本页统计</div>
      </div>
      <div class="stat">
        <div class="stat-label">已降级</div>
        <div class="stat-value sm" style="color: var(--degraded)">{{ formatNumber(pageStats.degraded) }}</div>
        <div class="stat-hint">本页统计</div>
      </div>
    </div>

    <!-- 工具栏 -->
    <div class="card">
      <div class="card-body tight">
        <div class="row wrap" style="padding: 4px 0">
          <input
            v-model="keywordInput"
            class="input"
            style="max-width: 300px"
            placeholder="搜索标题 / 运行 ID（回车触发）"
            @keyup.enter="onSearch"
          />
          <button class="btn btn-sm" @click="onSearch">搜索</button>
          <select class="select" style="max-width: 170px" :value="stateFilter" @change="applyState(($event.target as HTMLSelectElement).value)">
            <option value="">全部状态</option>
            <option v-for="opt in STATE_OPTIONS" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
          </select>
          <button
            class="btn btn-sm"
            :class="{ 'btn-primary': stateFilter === 'needs_review' }"
            @click="toggleNeedsReview"
          >
            只看待复核
          </button>
          <div class="spacer" />
          <button v-if="stateFilter || keywordInput" class="btn btn-sm btn-ghost" @click="clearFilters">清空筛选</button>
        </div>
      </div>
    </div>

    <!-- 列表失败 -->
    <div v-if="listError && !loading" class="alert alert-error">
      <div>
        <div class="strong">任务列表加载失败</div>
        <div class="small">{{ listError }}</div>
      </div>
      <div class="spacer" />
      <button class="btn btn-sm" @click="refresh">重试</button>
    </div>

    <!-- 加载骨架 -->
    <div v-if="loading && !runItems.length" class="card">
      <div class="card-body" style="display: flex; flex-direction: column; gap: 10px">
        <div v-for="i in 7" :key="i" class="skeleton" style="height: 30px" />
      </div>
    </div>

    <!-- 空态 -->
    <div v-else-if="isEmpty && !listError" class="card">
      <div class="card-body">
        <EmptyState
          title="没有匹配的运行记录"
          :hint="stateFilter || keywordInput ? '试着放宽筛选条件或清空关键字' : '点击「发起排查」提交第一个报错分析任务'"
        >
          <div class="row" style="justify-content: center">
            <button v-if="stateFilter || keywordInput" class="btn" @click="clearFilters">清空筛选</button>
            <button class="btn btn-primary" @click="goCreate">发起排查</button>
          </div>
        </EmptyState>
      </div>
    </div>

    <!-- 列表 -->
    <div v-else-if="runItems.length" class="card">
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>标题</th>
              <th>状态</th>
              <th>定级</th>
              <th>模式</th>
              <th>锁定仓库</th>
              <th>补丁</th>
              <th>Token</th>
              <th>耗时</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="run in runItems" :key="run.id" class="clickable" @click="openRun(run)">
              <td style="max-width: 340px">
                <div class="row" style="gap: 6px">
                  <span class="badge badge-muted">{{ modeLabel(run.mode) }}</span>
                  <span v-if="run.environment" class="badge badge-info">{{ run.environment }}</span>
                </div>
                <div class="ellipsis" style="margin-top: 3px" :title="run.title">{{ run.title || '(无标题)' }}</div>
                <div class="small faint mono ellipsis" :title="run.id">{{ run.id }}</div>
              </td>
              <td>
                <StatusBadge :state="runState(run)" />
                <div v-if="run.degraded" class="small faint" style="margin-top: 3px">降级执行</div>
              </td>
              <td><StatusBadge :severity="run.severity" /></td>
              <td class="nowrap small muted">{{ modeLabel(run.mode) }}</td>
              <td>
                <span class="badge" :class="repoCount(run) > 0 ? 'badge-info' : 'badge-muted'">{{ formatNumber(repoCount(run)) }}</span>
              </td>
              <td>
                <span class="badge" :class="patchCount(run) > 0 ? 'badge-ok' : 'badge-muted'">{{ formatNumber(patchCount(run)) }}</span>
              </td>
              <td class="nowrap small">{{ tokens(run) }}</td>
              <td class="nowrap small">{{ elapsed(run) }}</td>
              <td class="nowrap small muted" :title="formatTime(run.createdAt)">{{ formatRelative(run.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="card-body tight">
        <Pagination :page="page" :page-size="pageSize" :total="total" @change="go" />
      </div>
    </div>
  </div>
</template>
