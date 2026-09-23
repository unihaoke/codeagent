<script setup lang="ts">
/**
 * 可观测审计（第十层可观测体系）。
 *
 * 覆盖任务生命周期、仓库切换、技能调用、模型推理、源码拉取、修复结果变更。
 * 汇总（/observability/summary）与事件流（/observability/audits）完全独立降级。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { AuditEvent, ObservabilitySummary } from '@/types'
import {
  categoryLabel,
  downloadText,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatRelative,
  formatTime,
  stateMeta,
  truncate,
} from '@/utils/format'
import { toastOk } from '@/composables/useToast'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import BarList from '@/components/BarList.vue'
import EmptyState from '@/components/EmptyState.vue'
import Pagination from '@/components/Pagination.vue'
import CopyButton from '@/components/CopyButton.vue'

const router = useRouter()

/** 自动刷新间隔（毫秒）。 */
const AUTO_REFRESH_MS = 10_000
/** 审计事件分页大小。 */
const PAGE_SIZE = 30

/* ------------------------------ 状态 ------------------------------ */

const summary = ref<ObservabilitySummary | null>(null)
const summaryError = ref('')
const items = ref<AuditEvent[]>([])
const total = ref(0)
const page = ref(1)
const keyword = ref('')
const keywordInput = ref('')
const category = ref('')
const auditsError = ref('')
const loading = ref(false)
const forbidden = ref(false)
const autoRefresh = ref(false)
const expanded = ref<string>('')
let timer: number | null = null

function errMsg(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    if (e.code === 403) {
      forbidden.value = true
      return '需要 admin:all 权限'
    }
    return `业务码 ${e.code}：${e.message}`
  }
  return e instanceof Error ? e.message : fallback
}

/* ------------------------------ 加载 ------------------------------ */

/** 可观测汇总（独立失败）。 */
async function loadSummary(): Promise<void> {
  try {
    summary.value = await api.observability()
    summaryError.value = ''
  } catch (e) {
    summaryError.value = errMsg(e, '可观测汇总加载失败')
  }
}

/** 审计事件分页（独立失败：失败时保留上一次数据，降级展示）。 */
async function loadAudits(): Promise<void> {
  try {
    const res = await api.audits({
      page: page.value,
      pageSize: PAGE_SIZE,
      keyword: keyword.value || undefined,
      state: category.value || undefined,
    })
    items.value = res?.items ?? []
    total.value = res?.total ?? 0
    auditsError.value = ''
  } catch (e) {
    auditsError.value = errMsg(e, '审计事件流加载失败')
  }
}

async function refreshAll(notify = true): Promise<void> {
  loading.value = true
  await Promise.all([loadSummary(), loadAudits()])
  loading.value = false
  if (notify && !summaryError.value && !auditsError.value) toastOk('可观测数据已刷新')
}

/* ------------------------------ 交互 ------------------------------ */

function applySearch(): void {
  keyword.value = keywordInput.value.trim()
  page.value = 1
  void loadAudits()
}

function resetSearch(): void {
  keywordInput.value = ''
  keyword.value = ''
  category.value = ''
  page.value = 1
  void loadAudits()
}

function onCategoryChange(): void {
  page.value = 1
  void loadAudits()
}

function goPage(p: number): void {
  page.value = Math.max(1, p)
  void loadAudits()
}

function exportAudits(): void {
  downloadText('audits.json', JSON.stringify(items.value ?? [], null, 2), 'application/json')
  toastOk('已导出当前页审计事件', `共 ${items.value.length} 条`)
}

/** data 字段 → 格式化 JSON。 */
function pretty(data: unknown): string {
  if (typeof data === 'string') return data
  try {
    return JSON.stringify(data, null, 2) ?? String(data)
  } catch {
    return String(data)
  }
}

function expandable(ev: AuditEvent): boolean {
  return ev?.data !== undefined && ev?.data !== null
}

function toggle(ev: AuditEvent): void {
  if (!expandable(ev)) return
  expanded.value = expanded.value === ev.id ? '' : ev.id
}

function openRun(runId?: string): void {
  if (!runId) return
  void router.push({ name: 'task-detail', params: { id: runId } })
}

/* ------------------------------ 展示映射 ------------------------------ */

const CATEGORY_LABELS: Record<string, string> = {
  task: '任务',
  repo: '仓库',
  group: '分组',
  skill: '技能',
  model: '模型',
  source: '源码拉取',
  patch: '补丁',
  auth: '鉴权',
  quota: '配额',
}

function categoryMeta(c?: string): { label: string; tone: string } {
  const key = c ?? ''
  const toneMap: Record<string, string> = {
    task: 'info',
    repo: 'info',
    group: 'info',
    skill: 'degraded',
    model: 'degraded',
    source: 'muted',
    patch: 'warn',
    auth: 'error',
    quota: 'warn',
  }
  return { label: CATEGORY_LABELS[key] ?? (key || '通用'), tone: toneMap[key] ?? 'muted' }
}

function levelMeta(level?: string): { label: string; tone: string } {
  switch ((level ?? '').toLowerCase()) {
    case 'warn':
    case 'warning':
      return { label: '警告', tone: 'warn' }
    case 'error':
    case 'fatal':
      return { label: '错误', tone: 'error' }
    case 'debug':
    case 'trace':
      return { label: '调试', tone: 'muted' }
    default:
      return { label: '信息', tone: 'muted' }
  }
}

const stateLabelMap = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const key of Object.keys(summary.value?.stateDist ?? {})) {
    map[key] = stateMeta(key).label
  }
  return map
})

const categoryLabelMap = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const key of Object.keys(summary.value?.categoryDist ?? {})) {
    map[key] = categoryLabel(key)
  }
  return map
})

const topSkills = computed(() => summary.value?.topSkills ?? [])
const topModels = computed(() => summary.value?.topModels ?? [])
const recentFailures = computed(() => summary.value?.recentFailures ?? [])

/* ------------------------------ 生命周期 ------------------------------ */

watch(autoRefresh, (on) => {
  if (timer !== null) {
    window.clearInterval(timer)
    timer = null
  }
  if (on) timer = window.setInterval(() => void refreshAll(false), AUTO_REFRESH_MS)
})

onMounted(() => {
  void refreshAll(false)
})

onUnmounted(() => {
  if (timer !== null) window.clearInterval(timer)
  timer = null
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">可观测审计</h1>
        <p class="page-subtitle">
          覆盖任务生命周期、仓库切换、技能调用、模型推理、源码拉取、修复结果变更
        </p>
      </div>
      <div class="row wrap">
        <label class="checkbox" title="每 10 秒自动刷新汇总与审计事件">
          <input v-model="autoRefresh" type="checkbox" />
          自动刷新（10s）
        </label>
        <button class="btn" :disabled="loading" @click="refreshAll()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </div>

    <div v-if="forbidden" class="alert alert-error">需要 admin:all 权限</div>
    <div v-if="summaryError" class="alert alert-warn">
      可观测汇总不可用：{{ summaryError }}（指标卡、分布与 Top 榜单可能为空，审计事件流不受影响）
    </div>
    <div v-if="auditsError" class="alert alert-warn">
      审计事件流不可用：{{ auditsError }}（下方展示的是最近一次成功加载的数据）
    </div>

    <!-- 汇总指标 -->
    <div class="grid grid-5">
      <StatTile label="任务总数" :value="formatNumber(summary?.tasksTotal)" :hint="`运行总数 ${formatNumber(summary?.runsTotal)}`" />
      <StatTile
        label="运行总数"
        :value="formatNumber(summary?.runsTotal)"
        :hint="`成功 ${formatNumber(summary?.runsSucceeded)} · 失败 ${formatNumber(summary?.runsFailed)}`"
      />
      <StatTile
        label="修复成功率"
        :value="formatPercent(summary?.fixRate)"
        :tone="(summary?.fixRate ?? 0) >= 0.6 ? 'ok' : 'warn'"
        :hint="`待复核 ${formatNumber(summary?.runsNeedsReview)} · 降级 ${formatNumber(summary?.runsDegraded)}`"
      />
      <StatTile label="平均耗时" :value="formatDuration(summary?.avgElapsedMs)" hint="单次运行平均端到端耗时" />
      <StatTile
        label="模型 Token"
        :value="formatCompact(summary?.modelTokens)"
        tone="info"
        :hint="`模型调用 ${formatNumber(summary?.modelCalls)} 次`"
      />
    </div>

    <!-- 分布与 Top 技能 -->
    <div class="grid grid-3">
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">运行结果分布</h3>
        </div>
        <div class="card-body">
          <div v-if="summaryError" class="alert alert-warn small">{{ summaryError }}</div>
          <BarList v-else :data="summary?.stateDist ?? {}" :label-map="stateLabelMap" tone="var(--ok)" />
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h3 class="card-title">根因分类分布</h3>
        </div>
        <div class="card-body">
          <div v-if="summaryError" class="alert alert-warn small">{{ summaryError }}</div>
          <BarList v-else :data="summary?.categoryDist ?? {}" :label-map="categoryLabelMap" tone="var(--brand-500)" />
        </div>
      </div>

      <div class="card">
        <div class="card-head">
          <h3 class="card-title">Top 技能</h3>
          <span class="small muted">调用 / 失败 / 平均延迟</span>
        </div>
        <div class="card-body">
          <div v-if="summaryError" class="alert alert-warn small">{{ summaryError }}</div>
          <div v-else-if="!topSkills.length" class="muted small">暂无技能调用记录</div>
          <div v-else class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>技能</th>
                  <th>调用</th>
                  <th>失败</th>
                  <th>平均延迟</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="s in topSkills" :key="s.skill">
                  <td class="mono ellipsis" style="max-width: 180px" :title="s.skill">{{ s.skill }}</td>
                  <td>{{ formatNumber(s.calls) }}</td>
                  <td :style="(s.failures ?? 0) > 0 ? { color: 'var(--error)' } : undefined">
                    {{ formatNumber(s.failures) }}
                  </td>
                  <td class="nowrap">{{ formatDuration(s.avgLatencyMs) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>

    <!-- Top 模型 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">模型调用排行</h3>
        <span class="small muted">调用次数 · 平均延迟 · Token 消耗</span>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>模型</th>
              <th>调用数</th>
              <th>平均延迟</th>
              <th>Token</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in topModels" :key="m.model">
              <td class="mono ellipsis" style="max-width: 320px" :title="m.model">{{ m.model }}</td>
              <td>{{ formatNumber(m.calls) }}</td>
              <td class="nowrap">{{ formatDuration(m.avgLatencyMs) }}</td>
              <td class="nowrap">{{ formatCompact(m.totalTokens) }}</td>
            </tr>
          </tbody>
        </table>
        <EmptyState
          v-if="!topModels.length && !summaryError"
          title="暂无模型调用记录"
          hint="发起一次任务后即可看到模型维度的调用统计"
        />
      </div>
    </div>

    <!-- 资源效率 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">资源效率</h3>
        <span class="small muted">缓存与兜底机制的直接收益</span>
      </div>
      <div class="card-body">
        <div class="grid grid-4">
          <StatTile
            label="技能调用数"
            :value="formatNumber(summary?.skillCalls)"
            small
            :hint="`失败 ${formatNumber(summary?.skillFailures)} 次`"
          />
          <StatTile
            label="技能失败率"
            :value="formatPercent(summary?.skillFailureRate)"
            small
            :tone="(summary?.skillFailureRate ?? 0) > 0.1 ? 'warn' : 'ok'"
            hint="超过 10% 建议检查 Skill 熔断与降级链"
          />
          <StatTile
            label="模型调用 / 兜底"
            :value="`${formatNumber(summary?.modelCalls)} / ${formatNumber(summary?.modelFallbacks)}`"
            small
            tone="info"
            hint="主模型不可用时的自动切换次数"
          />
          <StatTile
            label="缓存命中"
            :value="formatNumber(summary?.cacheHits)"
            small
            tone="ok"
            hint="命中缓存可省去重复的模型推理"
          />
          <StatTile
            label="仓库切换次数"
            :value="formatNumber(summary?.repoSwitches)"
            small
            hint="分组联合排查中的跨仓库溯源跳转"
          />
          <StatTile label="待人工复核" :value="formatNumber(summary?.runsNeedsReview)" small tone="warn" />
          <StatTile label="降级运行" :value="formatNumber(summary?.runsDegraded)" small tone="degraded" />
          <StatTile label="失败运行" :value="formatNumber(summary?.runsFailed)" small tone="error" />
        </div>
      </div>
    </div>

    <!-- 最近失败运行 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">最近失败运行</h3>
        <span class="small muted">点击进入任务详情</span>
      </div>
      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>标题</th>
              <th>状态</th>
              <th>定级</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="b in recentFailures"
              :key="b.runId"
              class="clickable"
              @click="openRun(b.runId)"
            >
              <td style="max-width: 520px">
                <div class="ellipsis strong" :title="b.title">{{ b.title || '(未命名运行)' }}</div>
                <div v-if="b.summary" class="small muted ellipsis" :title="b.summary">{{ truncate(b.summary, 100) }}</div>
              </td>
              <td><StatusBadge :state="b.state" /></td>
              <td><StatusBadge :severity="b.severity" /></td>
              <td class="nowrap">
                {{ formatTime(b.at) }}
                <div class="small faint">{{ formatRelative(b.at) }}</div>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState
          v-if="!recentFailures.length && !summaryError"
          title="暂无失败运行"
          hint="失败与降级运行会自动汇总到这里，便于快速定位"
        />
      </div>
    </div>

    <!-- 审计事件流 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">审计事件流</h3>
        <div class="row wrap">
          <input
            v-model="keywordInput"
            class="input"
            style="width: 200px"
            placeholder="搜索动作 / 消息 / 请求 ID"
            @keyup.enter="applySearch()"
          />
          <select v-model="category" class="select" style="width: 150px" @change="onCategoryChange()">
            <option value="">全部分类</option>
            <option v-for="(label, key) in CATEGORY_LABELS" :key="key" :value="key">
              {{ label }}（{{ key }}）
            </option>
          </select>
          <button class="btn btn-sm" @click="applySearch()">搜索</button>
          <button class="btn btn-sm btn-ghost" @click="resetSearch()">重置</button>
          <button class="btn btn-sm" :disabled="!items.length" @click="exportAudits()">导出 JSON</button>
        </div>
      </div>

      <div class="table-wrap">
        <table class="table">
          <thead>
            <tr>
              <th>时间</th>
              <th>级别</th>
              <th>分类</th>
              <th>动作</th>
              <th>消息</th>
              <th>关联</th>
              <th>请求 ID</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="ev in items" :key="ev.id">
              <tr :class="{ clickable: expandable(ev) }" @click="toggle(ev)">
                <td class="nowrap" :title="formatRelative(ev.at)">{{ formatTime(ev.at) }}</td>
                <td>
                  <StatusBadge :label="levelMeta(ev.level).label" :tone="levelMeta(ev.level).tone" />
                </td>
                <td>
                  <StatusBadge
                    :label="categoryMeta(ev.category).label"
                    :tone="categoryMeta(ev.category).tone"
                  />
                </td>
                <td class="mono ellipsis" style="max-width: 220px" :title="ev.action">{{ ev.action }}</td>
                <td style="max-width: 420px">
                  <span class="ellipsis" :title="ev.message">{{ truncate(ev.message, 110) }}</span>
                </td>
                <td class="small" style="min-width: 190px">
                  <div class="row wrap" style="gap: 6px">
                    <button
                      v-if="ev.runId"
                      class="btn btn-sm btn-ghost mono"
                      title="打开任务详情"
                      @click.stop="openRun(ev.runId)"
                    >
                      run {{ truncate(ev.runId, 8) }}
                    </button>
                    <span v-if="ev.repoId" class="mono muted" :title="ev.repoId">
                      repo {{ truncate(ev.repoId, 8) }}
                    </span>
                    <span v-if="ev.commit" class="mono muted" :title="ev.commit">
                      @{{ truncate(ev.commit, 8) }}
                    </span>
                    <span v-if="ev.skill" class="badge badge-degraded">{{ ev.skill }}</span>
                    <span v-if="ev.model" class="badge badge-info">{{ ev.model }}</span>
                  </div>
                </td>
                <td>
                  <div class="row" style="gap: 4px">
                    <span
                      v-if="ev.requestId"
                      class="mono ellipsis small"
                      style="max-width: 120px"
                      :title="ev.requestId"
                    >
                      {{ truncate(ev.requestId, 16) }}
                    </span>
                    <span v-else class="muted small">-</span>
                    <CopyButton v-if="ev.requestId" :text="ev.requestId" label="⧉" title="复制请求 ID" />
                  </div>
                </td>
              </tr>
              <tr v-if="expanded === ev.id && expandable(ev)">
                <td colspan="7" style="background: var(--bg-elev-2)">
                  <pre class="code">{{ pretty(ev.data) }}</pre>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <EmptyState
          v-if="!items.length"
          title="暂无审计事件"
          hint="任务执行、仓库切换、技能与模型调用都会产生审计记录"
        />
      </div>

      <div style="padding: 0 14px 10px">
        <Pagination :page="page" :page-size="PAGE_SIZE" :total="total" @change="goPage" />
      </div>
    </div>
  </div>
</template>
