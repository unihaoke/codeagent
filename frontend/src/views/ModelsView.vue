<script setup lang="ts">
/**
 * MCP 模型管控（第六层）。
 *
 * 输入标准化 · 输出强约束（结构化 JSON）· 上下文智能治理 · 多模型分级调度 · 推理容错熔断。
 * 两个接口（/models 与 /models/stats）完全独立降级：任一失败不影响另一部分渲染。
 */
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import * as echarts from 'echarts'
import { ApiError, api } from '@/api/client'
import type {
  ModelHealth,
  ModelStats,
  ModelTarget,
  ModelTier,
} from '@/types'
import {
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatTime,
  truncate,
} from '@/utils/format'
import { toastOk } from '@/composables/useToast'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import EmptyState from '@/components/EmptyState.vue'
import BarList from '@/components/BarList.vue'

/** 自动刷新间隔（毫秒）。 */
const AUTO_REFRESH_MS = 10_000

interface TierMeta {
  key: ModelTier
  label: string
  usage: string
}

/** 三层分级调度定义。 */
const TIERS: TierMeta[] = [
  { key: 'light', label: '轻量层 light', usage: '日志清洗 / 分类 / 摘要等轻任务' },
  { key: 'strong', label: '强力层 strong', usage: '根因推理 / 补丁合成等关键任务' },
  { key: 'fallback', label: '兜底层 fallback', usage: '格式修复与降级兜底' },
]

/* ------------------------------ 状态 ------------------------------ */

interface ModelsPayload {
  enabled: boolean
  targets: ModelTarget[]
  health: ModelHealth[]
}

const models = ref<ModelsPayload | null>(null)
const modelsError = ref('')
const stats = ref<ModelStats | null>(null)
const statsError = ref('')
const loading = ref(false)
const autoRefresh = ref(false)
let timer: number | null = null

function errMsg(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    if (e.code === 403) return '权限不足：需要 admin:all 权限'
    return `HTTP/业务码 ${e.code}：${e.message}`
  }
  return e instanceof Error ? e.message : fallback
}

/* ------------------------------ 加载 ------------------------------ */

/** 模型清单与健康（独立失败）。 */
async function loadModels(): Promise<void> {
  try {
    models.value = await api.listModels()
    modelsError.value = ''
  } catch (e) {
    modelsError.value = errMsg(e, '模型清单加载失败')
  }
}

/** 模型调用统计（独立失败）。 */
async function loadStats(): Promise<void> {
  try {
    stats.value = await api.modelStats()
    statsError.value = ''
  } catch (e) {
    statsError.value = errMsg(e, '模型统计加载失败')
  }
}

async function refreshAll(notify = true): Promise<void> {
  loading.value = true
  // 两个请求各自 try/catch，互不影响。
  await Promise.all([loadModels(), loadStats()])
  loading.value = false
  if (notify) {
    if (!modelsError.value && !statsError.value) toastOk('模型管控数据已刷新')
  }
}

/* ------------------------------ 派生数据 ------------------------------ */

const targets = computed<ModelTarget[]>(() => models.value?.targets ?? [])
const modelEnabled = computed<boolean>(() => models.value?.enabled !== false)

const healthMap = computed<Map<string, ModelHealth>>(() => {
  const map = new Map<string, ModelHealth>()
  for (const h of models.value?.health ?? []) {
    map.set(`${h.provider}/${h.model}`, h)
  }
  return map
})

interface TierRow {
  target: ModelTarget
  health?: ModelHealth
}

const tierRows = computed<Record<string, TierRow[]>>(() => {
  const out: Record<string, TierRow[]> = { light: [], strong: [], fallback: [] }
  for (const t of targets.value) {
    const tier = (t.tier ?? 'strong') as string
    const bucket = out[tier] ?? out.strong
    bucket.push({ target: t, health: healthMap.value.get(`${t.provider}/${t.model}`) })
  }
  return out
})

const circuitCounts = computed(() => {
  let open = 0
  let halfOpen = 0
  let healthy = 0
  for (const h of models.value?.health ?? []) {
    if (h.circuitState === 'open') open += 1
    else if (h.circuitState === 'half_open') halfOpen += 1
    if (h.healthy) healthy += 1
  }
  return { open, halfOpen, healthy, total: (models.value?.health ?? []).length }
})

const modelNames = computed(() => Object.keys(stats.value?.byModel ?? {}))

const byTierRows = computed<{ label: string; value: number }[]>(() => {
  const raw = stats.value?.byTier ?? {}
  const labelOf: Record<string, string> = { light: '轻量层', strong: '强力层', fallback: '兜底层' }
  return Object.entries(raw).map(([label, value]) => ({
    label: labelOf[label] ?? label,
    value: Number(value),
  }))
})

const initialLoading = computed(
  () =>
    loading.value &&
    !models.value &&
    !stats.value &&
    !modelsError.value &&
    !statsError.value,
)

const showEmpty = computed(
  () => !modelsError.value && models.value !== null && targets.value.length === 0,
)

/** 失败率。 */
function failRate(h?: ModelHealth): number {
  const total = h?.totalCalls ?? 0
  if (!total || !h) return 0
  return (h.failures ?? 0) / total
}

/** 健康状态 + 熔断状态 → 徽标。 */
function circuitMeta(h?: ModelHealth): { label: string; tone: string } {
  if (!h) return { label: '无健康数据', tone: 'muted' }
  const state = h.circuitState ?? ''
  if (state === 'open') return { label: '熔断 open', tone: 'error' }
  if (state === 'half_open') return { label: '半开探测', tone: 'warn' }
  if (h.healthy === false) return { label: '不健康', tone: 'error' }
  if (state === 'closed') return { label: '正常 closed', tone: 'ok' }
  return { label: '健康', tone: 'ok' }
}

function tierLabel(tier: string): string {
  if (tier === 'light') return '轻量'
  if (tier === 'strong') return '强力'
  if (tier === 'fallback') return '兜底'
  return tier || '未分级'
}

function tierTone(tier: string): string {
  if (tier === 'light') return 'info'
  if (tier === 'strong') return 'ok'
  if (tier === 'fallback') return 'warn'
  return 'muted'
}

/* ------------------------------ ECharts 饼图 ------------------------------ */

const chartEl = ref<HTMLDivElement | null>(null)
let chart: ReturnType<typeof echarts.init> | null = null

function renderChart(): void {
  const entries = Object.entries(stats.value?.byModel ?? {})
  if (!entries.length) {
    chart?.clear()
    return
  }
  if (!chartEl.value) return
  if (!chart) chart = echarts.init(chartEl.value)
  chart.setOption(
    {
      backgroundColor: 'transparent',
      tooltip: { trigger: 'item', formatter: '{b}<br/>调用 {c} 次（{d}%）' },
      legend: {
        type: 'scroll',
        bottom: 0,
        textStyle: { color: '#9aa5b8', fontSize: 11 },
        itemWidth: 10,
        itemHeight: 10,
      },
      series: [
        {
          type: 'pie',
          radius: ['44%', '68%'],
          center: ['50%', '44%'],
          avoidLabelOverlap: true,
          itemStyle: { borderColor: 'rgba(0,0,0,0)', borderWidth: 2 },
          label: { color: '#9aa5b8', fontSize: 11, formatter: '{b}\n{d}%' },
          labelLine: { lineStyle: { color: '#3a4351' } },
          data: entries.map(([name, value]) => ({ name, value: Number(value) })),
        },
      ],
    },
    true,
  )
  chart.resize()
}

function onResize(): void {
  chart?.resize()
}

watch(
  () => stats.value?.byModel,
  () => {
    void nextTick(renderChart)
  },
  { flush: 'post', deep: true },
)

onMounted(() => {
  window.addEventListener('resize', onResize)
  void refreshAll(false)
  if (autoRefresh.value) {
    timer = window.setInterval(() => void refreshAll(false), AUTO_REFRESH_MS)
  }
})

onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  if (timer !== null) window.clearInterval(timer)
  timer = null
  chart?.dispose()
  chart = null
})

watch(autoRefresh, (on) => {
  if (timer !== null) {
    window.clearInterval(timer)
    timer = null
  }
  if (on) timer = window.setInterval(() => void refreshAll(false), AUTO_REFRESH_MS)
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">MCP 模型管控</h1>
        <p class="page-subtitle">
          输入标准化 · 输出强约束（结构化 JSON）· 上下文智能治理 · 多模型分级调度 · 推理容错熔断
        </p>
      </div>
      <div class="row wrap">
        <label class="checkbox" title="每 10 秒自动刷新模型清单与调用统计">
          <input v-model="autoRefresh" type="checkbox" />
          自动刷新（10s）
        </label>
        <button class="btn" :disabled="loading" @click="refreshAll()">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </div>

    <!-- 降级提示 -->
    <div v-if="modelsError" class="alert alert-warn">
      模型清单不可用：{{ modelsError }}（展示能力受限，其余区块仍可正常使用）
    </div>
    <div v-if="statsError" class="alert alert-warn">
      模型调用统计不可用：{{ statsError }}（指标卡与分布图可能为空）
    </div>
    <div v-if="!modelEnabled && !modelsError" class="alert alert-warn">
      模型管控层未启用，当前使用内置 Mock 推理器（离线可跑通全链路）
    </div>

    <!-- 骨架 -->
    <div v-if="initialLoading" class="grid grid-4">
      <div v-for="i in 8" :key="i" class="card">
        <div class="card-body">
          <div class="skeleton" style="width: 45%; margin-bottom: 10px" />
          <div class="skeleton" style="width: 70%; height: 22px" />
        </div>
      </div>
    </div>

    <!-- 指标卡 -->
    <template v-else>
      <div class="grid grid-4">
        <StatTile
          label="总调用次数"
          :value="formatNumber(stats?.totalCalls)"
          :hint="`按模型去重 ${modelNames.length} 个`"
        />
        <StatTile
          label="失败次数"
          :value="formatNumber(stats?.failedCalls)"
          :tone="(stats?.failedCalls ?? 0) > 0 ? 'error' : ''"
          :hint="stats?.totalCalls ? `失败率 ${formatPercent((stats?.failedCalls ?? 0) / (stats?.totalCalls || 1))}` : '暂无调用'"
        />
        <StatTile
          label="兜底切换次数"
          :value="formatNumber(stats?.fallbackCalls)"
          tone="warn"
          hint="主模型不可用时的自动切换"
        />
        <StatTile
          label="结构化输出修复次数"
          :value="formatNumber(stats?.repairedOutputs)"
          tone="info"
          hint="JSON 约束校验失败后自动修复"
        />
      </div>

      <div class="grid grid-4">
        <StatTile label="Prompt Token" :value="formatCompact(stats?.promptTokens)" small />
        <StatTile label="Completion Token" :value="formatCompact(stats?.completionTokens)" small />
        <StatTile label="总 Token" :value="formatCompact(stats?.totalTokens)" small />
        <StatTile
          label="平均延迟"
          :value="formatDuration(stats?.avgLatencyMs)"
          small
          tone="info"
        />
      </div>

      <!-- 分级调度视图 -->
      <div class="grid grid-3">
        <div v-for="tier in TIERS" :key="tier.key" class="card">
          <div class="card-head">
            <div style="min-width: 0">
              <h3 class="card-title">{{ tier.label }}</h3>
              <div class="small muted" style="margin-top: 2px">{{ tier.usage }}</div>
            </div>
            <span class="badge" :class="`badge-${tierTone(tier.key)}`">
              {{ (tierRows[tier.key] ?? []).length }}
            </span>
          </div>
          <div class="card-body">
            <div v-if="!(tierRows[tier.key] ?? []).length" class="muted small">
              该层暂无模型配置
            </div>
            <div
              v-for="row in tierRows[tier.key] ?? []"
              :key="`${row.target.provider}/${row.target.model}`"
              style="
                border: 1px solid var(--border);
                border-radius: var(--radius-sm);
                padding: 10px 12px;
                margin-bottom: 10px;
                background: var(--bg-elev-2);
              "
            >
              <div class="row-between" style="gap: 8px">
                <span class="mono ellipsis" :title="`${row.target.provider}/${row.target.model}`">
                  {{ row.target.provider }}/{{ row.target.model }}
                </span>
                <span class="row nowrap" style="gap: 6px">
                  <StatusBadge :label="tierLabel(row.target.tier)" :tone="tierTone(row.target.tier)" />
                  <StatusBadge
                    :label="circuitMeta(row.health).label"
                    :tone="circuitMeta(row.health).tone"
                    :pulse="row.health?.circuitState === 'half_open'"
                  />
                </span>
              </div>

              <div class="row wrap small muted" style="margin-top: 6px; gap: 10px">
                <span>调用 {{ formatNumber(row.health?.totalCalls) }}</span>
                <span :style="(row.health?.failures ?? 0) > 0 ? { color: 'var(--error)' } : undefined">
                  失败 {{ formatNumber(row.health?.failures) }}
                </span>
                <span>平均 {{ formatDuration(row.health?.avgLatencyMs) }}</span>
              </div>

              <div v-if="row.health" class="row wrap small faint" style="margin-top: 4px; gap: 10px">
                <span>成功率 {{ formatPercent(1 - failRate(row.health)) }}</span>
                <span v-if="row.target.maxTokens">maxTokens {{ formatNumber(row.target.maxTokens) }}</span>
                <span v-if="row.target.temperature !== undefined">temp {{ row.target.temperature }}</span>
              </div>

              <div
                v-if="row.target.endpoint"
                class="small faint ellipsis"
                style="margin-top: 4px"
                :title="row.target.endpoint"
              >
                {{ truncate(row.target.endpoint, 52) }}
              </div>

              <div
                v-if="row.health?.lastError"
                class="small"
                style="margin-top: 6px; color: var(--error)"
                :title="row.health.lastError"
              >
                最近错误：{{ truncate(row.health.lastError, 88) }}
              </div>

              <div v-if="row.health?.openUntil" class="small faint" style="margin-top: 4px">
                熔断恢复时间：{{ formatTime(row.health.openUntil) }}
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 调用分布 -->
      <div class="grid grid-2">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">模型调用分布</h3>
            <span class="small muted">provider/model 维度</span>
          </div>
          <div class="card-body">
            <div v-if="statsError" class="alert alert-warn small">{{ statsError }}</div>
            <template v-else>
              <div v-if="!modelNames.length" class="muted small">暂无调用数据</div>
              <div v-show="modelNames.length > 0" ref="chartEl" class="chart" />
            </template>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <h3 class="card-title">分级调用分布</h3>
            <span class="small muted">light / strong / fallback</span>
          </div>
          <div class="card-body">
            <div v-if="statsError" class="alert alert-warn small">{{ statsError }}</div>
            <BarList v-else :data="byTierRows" tone="var(--brand-500)" />
          </div>
        </div>
      </div>
    </template>

    <!-- 熔断与降级说明 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">熔断、重试与降级策略</h3>
        <StatusBadge
          :label="modelEnabled ? '管控层已启用' : '管控层未启用'"
          :tone="modelEnabled ? 'ok' : 'warn'"
        />
      </div>
      <div class="card-body">
        <div class="alert alert-info">
          连续失败达到阈值后熔断，冷却后半开探测，探测成功自动恢复；模型不可用时自动切换同层其它模型 →
          兜底模型 → 全部失败时降级为堆栈文本分析。
        </div>
        <dl class="kv" style="margin-top: 14px">
          <dt>输入标准化</dt>
          <dd>堆栈、日志与代码片段统一裁剪、脱敏、结构化后再送入模型。</dd>
          <dt>输出强约束</dt>
          <dd>要求返回结构化 JSON，经 Schema 校验；不合法时触发一次修复重试并计入「结构化输出修复次数」。</dd>
          <dt>熔断阈值</dt>
          <dd>连续失败触发 open；冷却期结束进入 half_open 探测，探测成功回到 closed。</dd>
          <dt>实时熔断状态</dt>
          <dd>
            <span class="row wrap" style="gap: 8px">
              <span class="badge badge-error">熔断 open {{ circuitCounts.open }}</span>
              <span class="badge badge-warn">半开探测 {{ circuitCounts.halfOpen }}</span>
              <span class="badge badge-ok">健康 {{ circuitCounts.healthy }}</span>
              <span class="badge badge-muted">健康上报总数 {{ circuitCounts.total }}</span>
            </span>
          </dd>
        </dl>
      </div>
    </div>

    <!-- 空态 -->
    <div v-if="showEmpty" class="card">
      <div class="card-body">
        <EmptyState
          title="尚未配置任何模型"
          hint="可在配置文件中添加 provider（OpenAI 兼容协议），或启用内置 Mock 推理器"
        />
      </div>
    </div>
  </div>
</template>
