<script setup lang="ts">
/**
 * 修复报告列表页。
 *
 * 顶部统计条（总报告数 / 成功率 / 平均耗时）+ 关键字与状态筛选 + 分页表格。
 * 报告接口并行开发中，字段可能缺失：全部走可选链 + 默认值 + 空态兜底。
 */
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { Report } from '@/types'
import EmptyState from '@/components/EmptyState.vue'
import Pagination from '@/components/Pagination.vue'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import { usePaged } from '@/composables/useAsync'
import {
  categoryLabel,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatRelative,
  formatTime,
  modeLabel,
  stateMeta,
  truncate,
} from '@/utils/format'

const router = useRouter()

const STATE_OPTIONS = [
  'succeeded',
  'needs_review',
  'failed',
  'degraded',
  'cancelled',
  'queued',
  'analyzing',
  'repairing',
  'verifying',
]

const PAGE_SIZE = 20
const STATS_SAMPLE = 200

const searchText = ref('')
const stateFilter = ref('')
const listError = ref('')

const paged = usePaged<Report>(async (page, pageSize, keyword) => {
  try {
    const res = await api.listReports({
      page,
      pageSize,
      keyword,
      state: stateFilter.value || undefined,
    })
    listError.value = ''
    return { items: res?.items ?? [], total: res?.total ?? 0 }
  } catch (e) {
    listError.value = e instanceof ApiError ? e.message : e instanceof Error ? e.message : '报告列表加载失败'
    throw e
  }
}, PAGE_SIZE)

const { items, total, page, pageSize, loading, load, go, search } = paged

const filtered = computed(() => Boolean(searchText.value.trim() || stateFilter.value))

/* ------------------------------ 统计 ------------------------------ */

const sample = ref<Report[]>([])
const statsTotal = ref(0)
const statsLoading = ref(false)

async function loadStats(): Promise<void> {
  statsLoading.value = true
  try {
    const res = await api.listReports({ page: 1, pageSize: STATS_SAMPLE })
    sample.value = res?.items ?? []
    statsTotal.value = res?.total ?? 0
  } catch {
    sample.value = []
    statsTotal.value = 0
  } finally {
    statsLoading.value = false
  }
}

const stats = computed(() => {
  const list = sample.value
  const n = list.length
  const succeeded = list.filter((r) => r?.state === 'succeeded').length
  const avgElapsed = n ? list.reduce((sum, r) => sum + (r?.elapsedMs ?? 0), 0) / n : 0
  const tokenSum = list.reduce((sum, r) => sum + (r?.usage?.totalTokens ?? 0), 0)
  return {
    total: statsTotal.value,
    sample: n,
    succeeded,
    rate: n ? succeeded / n : 0,
    avgElapsed,
    tokenSum,
  }
})

const rateTone = computed<'' | 'ok' | 'warn' | 'error'>(() => {
  const r = stats.value.rate
  if (r >= 0.6) return 'ok'
  if (r >= 0.3) return 'warn'
  return 'error'
})

onMounted(() => {
  void loadStats()
})

async function refresh(): Promise<void> {
  await Promise.all([load(), loadStats()])
}

/* ------------------------------ 交互 ------------------------------ */

function applySearch(): void {
  search(searchText.value.trim())
}

function onStateChange(): void {
  page.value = 1
  void load()
}

function resetFilters(): void {
  searchText.value = ''
  stateFilter.value = ''
  search('')
}

async function openReport(r: Report): Promise<void> {
  if (!r?.id) return
  await router.push({ name: 'report-detail', params: { id: r.id } })
}

function rootCauseText(r: Report): string {
  if (!r?.rootCause) return '-'
  return categoryLabel(r.rootCause.category)
}

function patchCount(r: Report): number {
  return r?.patches?.length ?? 0
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">修复报告</h1>
        <p class="page-subtitle">任务完成后自动归档的修复结论、根因与验证证据</p>
      </div>
      <div class="row">
        <button class="btn" :disabled="loading || statsLoading" @click="refresh">
          {{ loading || statsLoading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </div>

    <!-- 统计条 -->
    <div class="grid grid-3">
      <StatTile label="总报告数" :value="formatNumber(stats.total)" hint="全量归档报告" />
      <StatTile
        label="成功率"
        :value="formatPercent(stats.rate)"
        :tone="rateTone"
        :hint="`最近 ${stats.sample} 份样本中成功 ${stats.succeeded} 份`"
      />
      <StatTile
        label="平均耗时"
        :value="formatDuration(stats.avgElapsed)"
        hint="最近样本平均 · Token 合计"
      />
    </div>

    <!-- 工具栏 -->
    <div class="card">
      <div class="card-body">
        <div class="row wrap">
          <div class="field" style="flex: 1; min-width: 220px">
            <label class="field-label">关键字</label>
            <input
              v-model="searchText"
              class="input"
              placeholder="按标题 / 摘要 / 任务 ID 搜索"
              @keyup.enter="applySearch"
            />
          </div>
          <div class="field" style="min-width: 180px">
            <label class="field-label">状态</label>
            <select v-model="stateFilter" class="select" @change="onStateChange">
              <option value="">全部状态</option>
              <option v-for="s in STATE_OPTIONS" :key="s" :value="s">{{ stateMeta(s).label }}</option>
            </select>
          </div>
          <div class="row" style="align-self: flex-end">
            <button class="btn btn-primary" @click="applySearch">搜索</button>
            <button class="btn btn-ghost" @click="resetFilters">重置</button>
          </div>
        </div>
      </div>
    </div>

    <div v-if="listError" class="alert alert-error">
      <span class="strong nowrap">报告列表加载失败</span>
      <span class="spacer" />
      <span>{{ listError }}</span>
      <button class="btn btn-sm" @click="refresh">重试</button>
    </div>

    <!-- 列表 -->
    <div class="card">
      <div class="card-head">
        <h3 class="card-title">报告清单</h3>
        <span class="small muted">共 {{ formatNumber(total) }} 条 · Token 合计 {{ formatCompact(stats.tokenSum) }}</span>
      </div>
      <div class="card-body" style="padding-top: 0">
        <div v-if="loading && !items.length" class="grid" style="gap: 8px">
          <div class="skeleton" />
          <div class="skeleton" />
          <div class="skeleton" style="width: 70%" />
        </div>

        <div v-else-if="!items.length">
          <EmptyState
            v-if="!filtered"
            title="暂无修复报告"
            hint="任务完成后会自动归档"
          />
          <EmptyState v-else title="没有匹配的报告" hint="请调整关键字或状态筛选条件">
            <button class="btn btn-sm" @click="resetFilters">重置筛选</button>
          </EmptyState>
        </div>

        <template v-else>
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th>标题</th>
                  <th>状态</th>
                  <th>定级</th>
                  <th>模式</th>
                  <th>根因分类</th>
                  <th>补丁</th>
                  <th>Token</th>
                  <th>耗时</th>
                  <th>创建时间</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(r, i) in items" :key="r?.id || i" class="clickable" @click="openReport(r)">
                  <td style="max-width: 320px">
                    <div class="strong ellipsis" :title="r?.title || ''">{{ r?.title || '未命名报告' }}</div>
                    <div v-if="r?.summary" class="small muted ellipsis" :title="truncate(r.summary, 300)">
                      {{ truncate(r.summary, 80) }}
                    </div>
                    <div class="small faint mono ellipsis">{{ r?.id || '-' }}</div>
                  </td>
                  <td class="nowrap"><StatusBadge :state="r?.state" /></td>
                  <td class="nowrap">
                    <StatusBadge v-if="r?.severity" :severity="r.severity" />
                    <span v-else class="muted">-</span>
                  </td>
                  <td class="nowrap">{{ modeLabel(r?.mode) }}</td>
                  <td class="nowrap">{{ rootCauseText(r) }}</td>
                  <td class="nowrap">{{ patchCount(r) }}</td>
                  <td class="nowrap">{{ formatCompact(r?.usage?.totalTokens ?? 0) }}</td>
                  <td class="nowrap">{{ formatDuration(r?.elapsedMs) }}</td>
                  <td class="nowrap">
                    <div>{{ formatTime(r?.createdAt) }}</div>
                    <div class="small faint">{{ formatRelative(r?.createdAt) }}</div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <Pagination
            :page="page"
            :page-size="pageSize"
            :total="total"
            @change="go"
          />
        </template>
      </div>
    </div>
  </div>
</template>
