<script setup lang="ts">
/**
 * 修复报告详情页。
 *
 * 覆盖：元信息、结论摘要、根因、跨仓库调用链路（chainFlow）、补丁、验证结果、
 * 时间线、建议与降级告警、完整 Markdown 报告。
 *
 * 报告接口并行开发中，字段可能缺失/接口可能 404/403/500：全部走可选链 + 默认值 + 空态兜底。
 */
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { CallEdge, Patch, Report, TimelineItem, VerificationCheck } from '@/types'
import AppModal from '@/components/AppModal.vue'
import CopyButton from '@/components/CopyButton.vue'
import DiffView from '@/components/DiffView.vue'
import EmptyState from '@/components/EmptyState.vue'
import MarkdownView from '@/components/MarkdownView.vue'
import StatusBadge from '@/components/StatusBadge.vue'
import { useAsync } from '@/composables/useAsync'
import { toastError, toastOk } from '@/composables/useToast'
import {
  categoryLabel,
  copyText,
  downloadText,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatTime,
  modeLabel,
  riskLabel,
  truncate,
} from '@/utils/format'

const route = useRoute()
const router = useRouter()

const reportId = computed(() => String(route.params.id ?? ''))

/* ------------------------------ 常量映射 ------------------------------ */

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

/* ------------------------------ 兜底助手 ------------------------------ */

/** 兼容 0-1 与 0-100 两种比例口径。 */
function ratio(v?: number): number {
  const x = v ?? 0
  if (!Number.isFinite(x)) return 0
  return x > 1 ? x / 100 : x
}

function pctWidth(v?: number): string {
  return `${Math.min(100, Math.max(0, ratio(v) * 100)).toFixed(1)}%`
}

function checkName(name?: string): string {
  if (!name) return '校验项'
  return CHECK_LABELS[name] ?? name
}

function applyResultLabel(v?: string): string {
  if (!v) return '-'
  return APPLY_RESULT[v] ?? v
}

function patchActionLabel(v?: string): string {
  if (!v) return '-'
  return PATCH_ACTION[v] ?? v
}

function timelineDot(level?: string): string {
  const l = (level ?? '').toLowerCase()
  if (l === 'error' || l === 'fatal') return 'error'
  if (l === 'warn' || l === 'warning') return 'warn'
  if (l === 'info' || l === 'ok' || l === 'success') return 'ok'
  return ''
}

function levelTone(level?: string): string {
  const l = (level ?? '').toLowerCase()
  if (l === 'error' || l === 'fatal') return 'error'
  if (l === 'warn' || l === 'warning') return 'warn'
  if (l === 'info') return 'info'
  if (l === 'ok' || l === 'success') return 'ok'
  return 'muted'
}

/* ------------------------------ 数据 ------------------------------ */

const errorCode = ref(0)
const reportAsync = useAsync<Report>(
  async () => {
    try {
      const data = await api.getReport(reportId.value)
      errorCode.value = 0
      return data
    } catch (e) {
      errorCode.value = e instanceof ApiError ? e.code : 0
      throw e
    }
  },
  { immediate: false, silent: true },
)

const report = reportAsync.data
const loading = reportAsync.loading
const loadError = reportAsync.error

/** 404 / 403：报告不存在或无权访问。 */
const inaccessible = computed(
  () => !report.value && !loading.value && !!loadError.value && [403, 404].includes(errorCode.value),
)

const checks = computed<VerificationCheck[]>(() => report.value?.verification?.checks ?? [])
const patches = computed<Patch[]>(() => report.value?.patches ?? [])
const chainEdges = computed<CallEdge[]>(() => report.value?.chainFlow ?? [])
const timeline = computed<TimelineItem[]>(() => (report.value?.timeline ?? []).filter((t) => !!t))
const suggestions = computed<string[]>(() => (report.value?.suggestions ?? []).filter((s) => !!s))
const warnings = computed<string[]>(() => (report.value?.warnings ?? []).filter((w) => !!w))

const rawJson = computed(() => JSON.stringify(report.value ?? {}, null, 2))

/** 链路去重后的仓库顺序，用于顶部概览。 */
const chainNodes = computed<string[]>(() => {
  const out: string[] = []
  for (const e of chainEdges.value) {
    const from = e?.fromRepo
    const to = e?.toRepo
    if (from && !out.includes(from)) out.push(from)
    if (to && !out.includes(to)) out.push(to)
  }
  return out
})

const expandedEdges = ref<Record<number, boolean>>({})
function toggleEdge(i: number): void {
  expandedEdges.value = { ...expandedEdges.value, [i]: !expandedEdges.value[i] }
}

const jsonOpen = ref(false)

/* ------------------------------ 操作 ------------------------------ */

async function copyMarkdown(): Promise<void> {
  const md = report.value?.markdown
  if (!md) {
    toastError('无可复制的 Markdown 内容')
    return
  }
  const ok = await copyText(md)
  if (ok) toastOk('已复制 Markdown 报告')
  else toastError('复制失败', '当前浏览器未授予剪贴板权限')
}

function downloadMarkdown(): void {
  const md = report.value?.markdown
  if (!md) {
    toastError('无可下载的 Markdown 内容')
    return
  }
  const id = (report.value?.id ?? reportId.value).slice(0, 16)
  downloadText(`report-${id}.md`, md, 'text/markdown;charset=utf-8')
}

async function openRun(): Promise<void> {
  const rid = report.value?.runId
  if (!rid) {
    toastError('该报告未关联运行 ID')
    return
  }
  await router.push({ name: 'task-detail', params: { id: rid } })
}

async function copyPatch(p: Patch): Promise<void> {
  const ok = await copyText(p?.unifiedDiff ?? '')
  if (ok) toastOk('已复制 Patch 内容')
  else toastError('复制失败', '当前浏览器未授予剪贴板权限')
}

function downloadPatch(p: Patch): void {
  const base = (p?.filePath ?? '').split('/').filter(Boolean).pop() ?? 'patch'
  downloadText(`${p?.repoKey || 'patch'}-${base}.patch`, p?.unifiedDiff ?? '')
}

/* ------------------------------ 生命周期 ------------------------------ */

onMounted(() => {
  void reportAsync.reload()
})

watch(reportId, () => {
  expandedEdges.value = {}
  jsonOpen.value = false
  void reportAsync.reload()
})
</script>

<template>
  <div class="page">
    <!-- 页头 -->
    <div class="page-header">
      <div style="min-width: 0">
        <div class="row wrap" style="margin-bottom: 6px">
          <button class="btn btn-sm btn-ghost" @click="router.back()">← 返回</button>
          <h1 class="page-title ellipsis">{{ report?.title || '修复报告详情' }}</h1>
          <StatusBadge v-if="report" :state="report.state" />
          <StatusBadge v-if="report?.severity" :severity="report.severity" />
          <span v-if="report" class="badge badge-muted">{{ modeLabel(report.mode) }}</span>
          <StatusBadge v-if="report?.degraded" tone="degraded" label="降级产出" />
        </div>
        <p class="page-subtitle row wrap">
          <span>报告 ID</span>
          <span class="mono strong">{{ reportId }}</span>
          <CopyButton :text="reportId" label="复制 ID" title="复制报告 ID" />
          <span v-if="report?.createdAt" class="muted">生成于 {{ formatTime(report.createdAt) }}</span>
          <span v-if="report" class="muted">· 耗时 {{ formatDuration(report.elapsedMs) }}</span>
        </p>
      </div>

      <div class="row wrap">
        <button class="btn" :disabled="!report?.markdown" @click="copyMarkdown">复制 Markdown</button>
        <button class="btn" :disabled="!report?.markdown" @click="downloadMarkdown">下载 .md</button>
        <button class="btn" :disabled="!report" @click="jsonOpen = true">查看原始 JSON</button>
        <button class="btn btn-primary" :disabled="!report?.runId" @click="openRun">重新打开任务</button>
      </div>
    </div>

    <!-- 异常态 -->
    <div v-if="loadError" class="alert alert-error">
      <span class="strong nowrap">{{ inaccessible ? '报告不存在或无权访问' : '报告加载失败' }}</span>
      <span class="spacer" />
      <span>{{ inaccessible ? `HTTP ${errorCode}` : loadError }}</span>
      <button class="btn btn-sm" @click="reportAsync.reload()">重试</button>
      <button class="btn btn-sm" @click="router.back()">返回</button>
    </div>

    <div v-if="loading && !report" class="card">
      <div class="card-body">
        <div class="skeleton" style="height: 18px; width: 40%" />
        <div class="skeleton" style="margin-top: 10px" />
        <div class="skeleton" style="margin-top: 8px; width: 80%" />
      </div>
    </div>

    <div v-else-if="!report" class="card">
      <div class="card-body">
        <EmptyState title="暂无报告数据" hint="报告可能尚未生成，或后端接口暂不可用">
          <button class="btn btn-sm" @click="router.back()">返回上一页</button>
        </EmptyState>
      </div>
    </div>

    <template v-else>
      <!-- 元信息 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">报告元信息</h3>
          <span class="small muted">由 Agent 自动归档，仅作修复参考</span>
        </div>
        <div class="card-body">
          <dl class="kv">
            <dt>报告 ID</dt>
            <dd class="mono ellipsis">{{ report.id || '-' }}</dd>
            <dt>运行 ID</dt>
            <dd class="row">
              <span class="mono ellipsis">{{ report.runId || '-' }}</span>
              <CopyButton v-if="report.runId" :text="report.runId" label="复制" />
            </dd>
            <dt>任务 ID</dt>
            <dd class="mono ellipsis">{{ report.taskId || '-' }}</dd>
            <dt>模式</dt>
            <dd>{{ modeLabel(report.mode) }}</dd>
            <dt>状态</dt>
            <dd><StatusBadge :state="report.state" /></dd>
            <dt>定级</dt>
            <dd>
              <StatusBadge v-if="report.severity" :severity="report.severity" />
              <span v-else class="muted">未定级</span>
            </dd>
            <dt>耗时</dt>
            <dd>{{ formatDuration(report.elapsedMs) }}</dd>
            <dt>Token</dt>
            <dd>
              <span :title="`prompt ${formatNumber(report.usage?.promptTokens ?? 0)} / completion ${formatNumber(report.usage?.completionTokens ?? 0)}`">
                {{ formatCompact(report.usage?.totalTokens ?? 0) }}
              </span>
              <span class="small muted" style="margin-left: 8px">
                模型调用 {{ formatNumber(report.usage?.modelCalls ?? 0) }} 次 · 技能调用 {{ formatNumber(report.usage?.skillCalls ?? 0) }} 次
              </span>
            </dd>
            <dt>是否降级</dt>
            <dd>
              <StatusBadge v-if="report.degraded" tone="degraded" label="是（部分能力不可用）" />
              <span v-else class="muted">否</span>
            </dd>
          </dl>
        </div>
      </div>

      <!-- 结论摘要 -->
      <div v-if="report.summary" class="alert alert-info">
        <span class="strong nowrap">结论摘要</span>
        <span style="word-break: break-word">{{ report.summary }}</span>
      </div>

      <!-- 根因 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">根因定位</h3>
          <div class="row wrap">
            <StatusBadge v-if="report.rootCause?.category" tone="info" :label="categoryLabel(report.rootCause.category)" />
            <StatusBadge v-if="report.rootCause?.severity" :severity="report.rootCause.severity" />
          </div>
        </div>
        <div class="card-body">
          <template v-if="report.rootCause">
            <div class="strong" style="font-size: 15px">{{ report.rootCause.summary || '未给出结论摘要' }}</div>

            <div class="row" style="margin: 12px 0 8px">
              <span class="small muted nowrap">置信度</span>
              <div class="progress" style="flex: 1">
                <span
                  :class="ratio(report.rootCause.confidence) >= 0.7 ? 'ok' : ratio(report.rootCause.confidence) >= 0.4 ? 'warn' : 'error'"
                  :style="{ width: pctWidth(report.rootCause.confidence) }"
                />
              </div>
              <span class="small strong nowrap">{{ formatPercent(ratio(report.rootCause.confidence)) }}</span>
            </div>

            <div
              v-if="report.rootCause.detail"
              class="code"
              style="white-space: pre-wrap; max-height: 260px"
            >{{ report.rootCause.detail }}</div>

            <div v-if="report.rootCause.evidence?.length" style="margin-top: 12px">
              <div class="small muted" style="margin-bottom: 6px">证据链</div>
              <ul style="margin: 0; padding-left: 20px">
                <li v-for="(e, i) in report.rootCause.evidence" :key="i" class="small" :title="truncate(e, 400)">{{ e }}</li>
              </ul>
            </div>

            <div v-if="report.rootCause.blastRadius?.length" style="margin-top: 12px">
              <div class="small muted" style="margin-bottom: 6px">影响面</div>
              <div class="row wrap">
                <span v-for="(b, i) in report.rootCause.blastRadius" :key="i" class="badge badge-warn">{{ b }}</span>
              </div>
            </div>
          </template>
          <EmptyState v-else title="报告未包含根因结论" hint="任务可能降级执行或根因推理未收敛" />
        </div>
      </div>

      <!-- 跨仓库链路 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">跨仓库调用链路</h3>
          <span class="small muted">{{ chainNodes.length }} 个仓库 · {{ chainEdges.length }} 条调用边</span>
        </div>
        <div class="card-body">
          <template v-if="chainEdges.length">
            <div class="row wrap small muted" style="margin-bottom: 12px">
              <template v-for="(n, i) in chainNodes" :key="n">
                <span v-if="i > 0" class="stage-arrow">→</span>
                <span class="badge badge-info">{{ n }}</span>
              </template>
            </div>

            <div style="overflow-x: auto; padding-bottom: 4px">
              <div class="row" style="align-items: flex-start; gap: 8px; min-width: max-content">
                <template v-for="(e, i) in chainEdges" :key="i">
                  <span v-if="i > 0" class="stage-arrow" style="margin-top: 14px">→</span>
                  <div style="max-width: 340px">
                    <div
                      class="card"
                      style="cursor: pointer"
                      :title="e.evidence ? truncate(e.evidence, 200) : '点击展开证据'"
                      @click="toggleEdge(i)"
                    >
                      <div class="card-body" style="padding: 10px 12px">
                        <div class="row wrap">
                          <span class="badge badge-info">{{ e.fromRepo || '未知源' }}</span>
                          <span class="muted">→</span>
                          <span class="badge badge-muted">{{ e.toRepo || '未知目标' }}</span>
                        </div>
                        <div class="row wrap" style="margin-top: 6px">
                          <span v-if="e.protocol" class="badge badge-info">{{ e.protocol }}</span>
                          <span class="mono small ellipsis" :title="e.endpoint || e.symbol || ''">
                            {{ e.endpoint || e.symbol || '未提供端点/符号' }}
                          </span>
                        </div>
                        <div class="row small" style="margin-top: 6px">
                          <span class="muted">置信度 {{ formatPercent(ratio(e.confidence)) }}</span>
                          <span class="spacer" />
                          <span v-if="e.anomaly" class="badge badge-warn">异常点</span>
                          <span class="faint">{{ expandedEdges[i] ? '收起 ▲' : '展开 ▼' }}</span>
                        </div>
                      </div>
                    </div>

                    <div v-if="expandedEdges[i]" class="alert alert-warn" style="margin-top: 8px; display: block">
                      <div v-if="e.anomaly" style="word-break: break-word">
                        <span class="strong">异常点：</span>{{ e.anomaly }}
                      </div>
                      <div v-if="e.evidence" style="margin-top: 4px; word-break: break-word">
                        <span class="strong">证据：</span>{{ e.evidence }}
                      </div>
                      <div v-if="e.fromFile || e.toFile || e.symbol" class="small muted mono" style="margin-top: 6px; word-break: break-all">
                        {{ e.fromFile || '-' }} → {{ e.toFile || '-' }}<span v-if="e.symbol"> · {{ e.symbol }}</span>
                      </div>
                      <div v-if="!e.anomaly && !e.evidence" class="small muted">该调用边未提供证据或异常描述</div>
                    </div>
                  </div>
                </template>
              </div>
            </div>

            <div class="table-wrap" style="margin-top: 14px">
              <table class="table">
                <thead>
                  <tr>
                    <th>源仓库</th>
                    <th>目标仓库</th>
                    <th>协议</th>
                    <th>端点 / 符号</th>
                    <th>置信度</th>
                    <th>异常点</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(e, i) in chainEdges" :key="`row-${i}`" class="clickable" @click="toggleEdge(i)">
                    <td class="strong nowrap">{{ e.fromRepo || '-' }}</td>
                    <td class="strong nowrap">{{ e.toRepo || '-' }}</td>
                    <td class="nowrap">{{ e.protocol || '-' }}</td>
                    <td class="mono small" style="max-width: 340px; word-break: break-all">
                      {{ e.endpoint || '-' }}
                      <span v-if="e.symbol" class="muted"> · {{ e.symbol }}</span>
                    </td>
                    <td class="nowrap">{{ formatPercent(ratio(e.confidence)) }}</td>
                    <td class="small" style="max-width: 320px">
                      <span v-if="e.anomaly" class="badge badge-warn">{{ truncate(e.anomaly, 60) }}</span>
                      <span v-else class="muted">-</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>
          <EmptyState
            v-else
            title="该报告不包含跨仓库调用链路"
            hint="单仓库任务或链路分析未命中跨仓库调用"
          />
        </div>
      </div>

      <!-- 补丁 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">增量补丁</h3>
          <span class="small muted">{{ patches.length }} 个候选补丁</span>
        </div>
        <div class="card-body">
          <div v-if="patches.length" class="alert alert-warn" style="margin-bottom: 12px">
            ⚠ 修复结果需人工或流水线复核后方可上线
          </div>
          <EmptyState
            v-if="!patches.length"
            title="暂无补丁"
            hint="未匹配到可自动修复模式，请参考下方建议人工排查"
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
                </div>
              </div>
              <div class="card-body">
                <div v-if="p?.rationale" class="small" style="margin-bottom: 10px">{{ p.rationale }}</div>
                <div v-if="p?.producedBy" class="small muted" style="margin-bottom: 8px">产生者：{{ p.producedBy }}</div>
                <DiffView v-if="p?.unifiedDiff" :diff="p.unifiedDiff" />
                <div v-else class="empty">补丁内容为空</div>
                <div class="row" style="margin-top: 10px">
                  <button class="btn btn-sm" :disabled="!p?.unifiedDiff" @click="copyPatch(p)">复制 Patch</button>
                  <button class="btn btn-sm" :disabled="!p?.unifiedDiff" @click="downloadPatch(p)">下载 .patch</button>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- 验证结果 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">验证结果</h3>
          <div class="row wrap">
            <StatusBadge
              v-if="report.verification"
              :tone="report.verification.passed ? 'ok' : 'error'"
              :label="report.verification.passed ? '验证通过' : '验证未通过'"
            />
            <span v-if="report.verification" class="small muted">耗时 {{ formatDuration(report.verification.durationMs) }}</span>
          </div>
        </div>
        <div class="card-body">
          <template v-if="report.verification">
            <div v-if="report.verification.degraded" class="alert alert-warn" style="margin-bottom: 12px">
              仅完成静态校验（沙箱命令执行被禁用或无可用校验器）
            </div>
            <div class="row wrap small" style="margin-bottom: 10px">
              <span class="muted">补丁应用结果：</span>
              <span class="strong">{{ applyResultLabel(report.verification.applyResult) }}</span>
              <span class="muted">· 工作区：</span>
              <span class="mono">{{ report.verification.workspace || '-' }}</span>
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
          </template>
          <EmptyState v-else title="报告未包含验证结果" hint="任务可能未开启自动验证或沙箱不可用" />
        </div>
      </div>

      <!-- 时间线 -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">执行时间线</h3>
          <span class="small muted">{{ timeline.length }} 个节点</span>
        </div>
        <div class="card-body">
          <div v-if="timeline.length" class="timeline">
            <div v-for="(t, i) in timeline" :key="`${t?.at}-${i}`" class="timeline-item">
              <span class="timeline-dot" :class="timelineDot(t?.level)" />
              <div class="timeline-body">
                <div class="row wrap">
                  <span class="mono small muted nowrap">{{ formatTime(t?.at) }}</span>
                  <span v-if="t?.stage" class="timeline-stage">{{ t.stage }}</span>
                  <span v-if="t?.level" class="badge" :class="`badge-${levelTone(t.level)}`">{{ t.level }}</span>
                </div>
                <div class="small">{{ t?.message || '—' }}</div>
              </div>
            </div>
          </div>
          <EmptyState v-else title="暂无时间线" hint="报告未记录执行阶段节点" />
        </div>
      </div>

      <!-- 建议与降级告警 -->
      <div class="grid grid-2">
        <div class="card">
          <div class="card-head">
            <h3 class="card-title">后续建议</h3>
          </div>
          <div class="card-body">
            <ul v-if="suggestions.length" style="margin: 0; padding-left: 20px">
              <li v-for="(s, i) in suggestions" :key="i" style="margin-bottom: 6px">{{ s }}</li>
            </ul>
            <EmptyState v-else title="暂无建议" hint="报告未给出后续处理建议" />
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <h3 class="card-title">降级与告警</h3>
          </div>
          <div class="card-body">
            <div v-if="warnings.length" class="grid" style="gap: 8px">
              <div v-for="(w, i) in warnings" :key="i" class="alert alert-warn">{{ w }}</div>
            </div>
            <EmptyState v-else title="无告警" hint="本次执行未记录降级或异常告警" />
          </div>
        </div>
      </div>

      <!-- 完整 Markdown -->
      <div class="card">
        <div class="card-head">
          <h3 class="card-title">完整 Markdown 报告</h3>
          <div class="row">
            <button class="btn btn-sm" :disabled="!report.markdown" @click="copyMarkdown">复制</button>
            <button class="btn btn-sm" :disabled="!report.markdown" @click="downloadMarkdown">下载</button>
          </div>
        </div>
        <div class="card-body">
          <details open>
            <summary class="small muted" style="cursor: pointer">展开 / 折叠原始报告内容</summary>
            <div style="margin-top: 10px">
              <MarkdownView v-if="report.markdown" :source="report.markdown" />
              <EmptyState v-else title="报告 Markdown 为空" hint="后端可能尚未生成归档正文" />
            </div>
          </details>
        </div>
      </div>
    </template>

    <!-- 原始 JSON -->
    <AppModal v-if="jsonOpen" title="原始 JSON" wide @close="jsonOpen = false">
      <pre class="code" style="max-height: 60vh">{{ rawJson }}</pre>
      <template #footer>
        <CopyButton :text="rawJson" label="复制 JSON" />
        <button class="btn" @click="jsonOpen = false">关闭</button>
      </template>
    </AppModal>
  </div>
</template>
