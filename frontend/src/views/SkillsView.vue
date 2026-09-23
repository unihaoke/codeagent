<script setup lang="ts">
/**
 * Skill 插件治理（第五层）。
 *
 * 入参 Schema 强校验 · 沙箱隔离运行 · 版本管理与灰度 · 熔断降级与备用兜底。
 * 注册中心未就绪（404 / 501）时降级为告警提示，不阻塞控制台其它页面。
 */
import { computed, onMounted, ref } from 'vue'
import { ApiError, api } from '@/api/client'
import type { SkillHealth, SkillManifest, SkillView } from '@/types'
import {
  formatDuration,
  formatNumber,
  formatPercent,
  formatTime,
  skillCategoryLabel,
  truncate,
} from '@/utils/format'
import { toastError, toastOk, toastWarn } from '@/composables/useToast'
import AppModal from '@/components/AppModal.vue'
import CopyButton from '@/components/CopyButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatTile from '@/components/StatTile.vue'
import StatusBadge from '@/components/StatusBadge.vue'

function errText(e: unknown): string {
  if (e instanceof ApiError) return `${e.message}（code=${e.code}）`
  if (e instanceof Error) return e.message
  return String(e)
}

const CATEGORIES: { value: string; label: string }[] = [
  { value: '', label: '全部' },
  { value: 'parse', label: '代码解析(parse)' },
  { value: 'diagnose', label: '报错溯源(diagnose)' },
  { value: 'repair', label: '自动修复(repair)' },
  { value: 'verify', label: '修复验证(verify)' },
  { value: 'cross_repo', label: '跨仓库分析(cross_repo)' },
]

function skillStatusMeta(status?: string): { label: string; tone: string } {
  switch (status) {
    case 'enabled':
      return { label: '已启用', tone: 'ok' }
    case 'canary':
      return { label: '灰度中', tone: 'warn' }
    case 'disabled':
      return { label: '已停用', tone: 'muted' }
    case 'deprecated':
      return { label: '已废弃', tone: 'error' }
    default:
      return { label: status || '未知', tone: 'muted' }
  }
}

function circuitMeta(state?: string): { label: string; tone: string } {
  switch (state) {
    case 'closed':
      return { label: '熔断器正常', tone: 'ok' }
    case 'half_open':
      return { label: '半开试探', tone: 'warn' }
    case 'open':
      return { label: '熔断中', tone: 'error' }
    default:
      return { label: '状态未知', tone: 'muted' }
  }
}

/* ------------------------------ 数据加载 ------------------------------ */

const category = ref('')
const skills = ref<SkillView[]>([])
const loading = ref(false)
const errorText = ref('')
const notReady = ref(false)

async function loadSkills() {
  loading.value = true
  errorText.value = ''
  notReady.value = false
  try {
    const list = await api.listSkills(category.value || undefined)
    skills.value = Array.isArray(list) ? list.filter(Boolean) : []
  } catch (e) {
    skills.value = []
    errorText.value = errText(e)
    notReady.value = e instanceof ApiError && (e.code === 404 || e.code === 501)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadSkills()
})

function pickCategory(value: string) {
  if (category.value === value) return
  category.value = value
  void loadSkills()
}

/* ------------------------------- 统计 ------------------------------- */

const total = computed(() => skills.value.length)

const healthyCount = computed(() => skills.value.filter((s) => Boolean(s?.health?.healthy)).length)

const openCount = computed(
  () => skills.value.filter((s) => s?.health?.circuitState === 'open').length,
)

const canaryCount = computed(
  () => skills.value.filter((s) => (s?.manifest?.status ?? '') === 'canary').length,
)

const avgLatency = computed(() => {
  const rows = skills.value.map((s) => s?.health?.avgLatencyMs ?? 0).filter((v) => v > 0)
  if (!rows.length) return 0
  return Math.round(rows.reduce((a, b) => a + b, 0) / rows.length)
})

function manifestOf(skill: SkillView): SkillManifest | undefined {
  return skill?.manifest
}

function healthOf(skill: SkillView): SkillHealth | undefined {
  return skill?.health
}

function fallbackChain(manifest?: SkillManifest): string {
  const list = manifest?.fallbackSkills ?? []
  return list.length ? list.join(' → ') : '无兜底链'
}

/* ------------------------------- 操作 ------------------------------- */

const acting = ref('')

async function applyStatus(skill: SkillView, status: string, canaryPercent?: number) {
  const name = skill?.manifest?.name
  if (!name) {
    toastError('技能名缺失，无法更新状态')
    return
  }
  acting.value = name
  try {
    await api.setSkillStatus(name, {
      status,
      version: skill?.manifest?.version,
      canaryPercent,
    })
    toastOk('技能状态已更新', `${skill?.manifest?.title || name} → ${skillStatusMeta(status).label}`)
    await loadSkills()
  } catch (e) {
    toastError('技能状态更新失败', errText(e))
  } finally {
    acting.value = ''
  }
}

function toggleStatus(skill: SkillView) {
  const current = skill?.manifest?.status ?? 'enabled'
  const next = current === 'enabled' ? 'disabled' : 'enabled'
  void applyStatus(skill, next)
}

/* 灰度 */
const canaryOpen = ref(false)
const canaryTarget = ref<SkillView | null>(null)
const canaryValue = ref('30')
const canaryError = ref('')

function openCanary(skill: SkillView) {
  canaryTarget.value = skill
  canaryValue.value = String(skill?.manifest?.canaryPercent ?? 30)
  canaryError.value = ''
  canaryOpen.value = true
}

async function submitCanary() {
  const target = canaryTarget.value
  if (!target) return
  const raw = canaryValue.value.trim()
  const value = Number(raw)
  if (raw === '' || !Number.isFinite(value) || value < 0 || value > 100) {
    canaryError.value = '灰度比例需为 0 - 100 之间的数字'
    return
  }
  canaryError.value = ''
  canaryOpen.value = false
  await applyStatus(target, 'canary', Math.trunc(value))
}

/* 回滚 */
const rollbackOpen = ref(false)
const rollbackTarget = ref<SkillView | null>(null)
const rollbackBusy = ref(false)
const rollbackError = ref('')

function openRollback(skill: SkillView) {
  rollbackTarget.value = skill
  rollbackError.value = ''
  rollbackOpen.value = true
}

async function confirmRollback() {
  const target = rollbackTarget.value
  const name = target?.manifest?.name
  if (!name) return
  rollbackBusy.value = true
  rollbackError.value = ''
  try {
    const res = await api.rollbackSkill(name)
    toastOk('已回滚到上一版本', `${target?.manifest?.title || name} → ${res?.version ?? '历史版本'}`)
    rollbackOpen.value = false
    await loadSkills()
  } catch (e) {
    if (e instanceof ApiError && e.code === 409) {
      toastWarn('没有可回滚的历史版本')
      rollbackOpen.value = false
    } else {
      rollbackError.value = errText(e)
      toastError('回滚失败', rollbackError.value)
    }
  } finally {
    rollbackBusy.value = false
  }
}

/* Schema */
const schemaOpen = ref(false)
const schemaTarget = ref<SkillView | null>(null)

function openSchema(skill: SkillView) {
  schemaTarget.value = skill
  schemaOpen.value = true
}

function schemaText(kind: 'input' | 'output', skill: SkillView | null): string {
  const schema = kind === 'input' ? skill?.manifest?.inputSchema : skill?.manifest?.outputSchema
  if (!schema) return '（未定义 Schema）'
  try {
    return JSON.stringify(schema, null, 2)
  } catch {
    return '（Schema 无法序列化）'
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">Skill 插件</h1>
        <p class="page-subtitle">
          入参 Schema 强校验 · 沙箱隔离运行 · 版本管理与灰度 · 熔断降级与备用兜底
        </p>
      </div>
      <div class="row">
        <button class="btn" :disabled="loading" @click="loadSkills">
          {{ loading ? '刷新中…' : '刷新' }}
        </button>
      </div>
    </div>

    <!-- 顶部统计 -->
    <div class="grid grid-4">
      <StatTile label="技能总数" :value="formatNumber(total)" :hint="`当前分类：${skillCategoryLabel(category || '全部')}`" />
      <StatTile
        label="健康技能"
        :value="formatNumber(healthyCount)"
        :hint="total ? `占比 ${formatPercent(total ? healthyCount / total : 0, 0)}` : '无数据'"
        tone="ok"
      />
      <StatTile
        label="熔断中技能"
        :value="formatNumber(openCount)"
        :hint="canaryCount ? `另有 ${canaryCount} 个灰度中` : '无灰度发布'"
        :tone="openCount ? 'error' : ''"
      />
      <StatTile label="平均延迟" :value="avgLatency ? formatDuration(avgLatency) : '-'" hint="按技能健康度均值" tone="info" />
    </div>

    <!-- 分类筛选 -->
    <div class="tabs">
      <button
        v-for="c in CATEGORIES"
        :key="c.value || 'all'"
        class="tab"
        :class="{ active: category === c.value }"
        @click="pickCategory(c.value)"
      >
        {{ c.label }}
      </button>
    </div>

    <div v-if="notReady" class="alert alert-warn">
      <div>
        <div class="strong">Skill 注册中心未就绪</div>
        <div class="small">
          接口返回 {{ errorText }}，请确认后端 Skill 注册中心已启动；控制台其余功能不受影响。
        </div>
      </div>
      <span class="spacer" />
      <button class="btn btn-sm" @click="loadSkills">重试</button>
    </div>
    <div v-else-if="errorText" class="alert alert-error">
      <div>
        <div class="strong">技能列表加载失败</div>
        <div class="small">{{ errorText }}</div>
      </div>
      <span class="spacer" />
      <button class="btn btn-sm" @click="loadSkills">重试</button>
    </div>

    <div v-if="loading && !skills.length" class="grid grid-2">
      <div v-for="i in 4" :key="i" class="card">
        <div class="card-body">
          <div class="skeleton" style="height: 16px; width: 45%; margin-bottom: 12px" />
          <div class="skeleton" style="height: 12px; margin-bottom: 8px" />
          <div class="skeleton" style="height: 12px; width: 65%" />
        </div>
      </div>
    </div>

    <div v-else-if="!skills.length" class="card">
      <EmptyState
        :title="errorText ? '技能数据不可用' : '暂无技能'"
        :hint="errorText ? '注册中心暂不可用，可稍后重试' : '当前分类下没有已注册的 Skill 插件'"
        icon="🧩"
      />
    </div>

    <div v-else class="grid grid-2">
      <div v-for="(s, idx) in skills" :key="manifestOf(s)?.name ?? `skill-${idx}`" class="card">
        <div class="card-head">
          <div style="min-width: 0">
            <div class="row wrap" style="gap: 6px">
              <span class="strong ellipsis" :title="manifestOf(s)?.title">
                {{ manifestOf(s)?.title || manifestOf(s)?.name || '未命名技能' }}
              </span>
              <span class="mono small muted">{{ manifestOf(s)?.name }}</span>
              <span class="badge badge-muted mono">v{{ manifestOf(s)?.version || '?' }}</span>
              <span v-if="manifestOf(s)?.builtin" class="badge badge-degraded">内置</span>
            </div>
            <div class="small muted" style="margin-top: 3px">
              {{ skillCategoryLabel(manifestOf(s)?.category) }}
            </div>
          </div>
          <div class="row" style="gap: 5px">
            <span
              class="badge"
              :class="`badge-${skillStatusMeta(manifestOf(s)?.status).tone}`"
            >
              {{ skillStatusMeta(manifestOf(s)?.status).label }}
            </span>
          </div>
        </div>

        <div class="card-body">
          <div class="small muted">{{ truncate(manifestOf(s)?.description, 160) || '暂无描述' }}</div>

          <div class="row wrap" style="gap: 5px; margin-top: 8px">
            <span v-for="t in manifestOf(s)?.tags ?? []" :key="t" class="badge badge-info">{{ t }}</span>
            <span class="badge badge-muted">优先级 {{ manifestOf(s)?.priority ?? '-' }}</span>
            <span v-if="manifestOf(s)?.compatibleWith" class="badge badge-muted">
              兼容 {{ manifestOf(s)?.compatibleWith }}
            </span>
          </div>

          <!-- 治理参数 -->
          <div class="row wrap small muted" style="gap: 4px 10px; margin-top: 10px">
            <span>超时 <span class="strong">{{ manifestOf(s)?.timeout ?? '-' }}</span> 秒</span>
            <span>重试 <span class="strong">{{ manifestOf(s)?.retries ?? 0 }}</span></span>
            <span>连续失败阈值 <span class="strong">{{ manifestOf(s)?.failureThreshold ?? '-' }}</span></span>
            <span>冷却 <span class="strong">{{ manifestOf(s)?.cooldownSeconds ?? '-' }}</span> 秒</span>
            <span>最大并发 <span class="strong">{{ manifestOf(s)?.maxConcurrency ?? '-' }}</span></span>
            <span>
              灰度
              <span class="strong">
                {{ manifestOf(s)?.canaryPercent !== undefined ? `${manifestOf(s)?.canaryPercent}%` : '未开启' }}
              </span>
            </span>
          </div>
          <div class="small muted" style="margin-top: 6px">
            兜底链：<span class="mono">{{ fallbackChain(manifestOf(s)) }}</span>
          </div>

          <!-- 健康度 -->
          <div class="skill-health">
            <div class="row wrap" style="gap: 6px">
              <span
                class="badge"
                :class="`badge-${circuitMeta(healthOf(s)?.circuitState).tone}`"
              >
                <i class="dot" :class="{ pulse: healthOf(s)?.circuitState === 'open' }" />
                {{ circuitMeta(healthOf(s)?.circuitState).label }}
              </span>
              <StatusBadge
                :label="healthOf(s)?.healthy ? '健康' : '异常'"
                :tone="healthOf(s)?.healthy ? 'ok' : 'error'"
              />
              <span class="badge badge-muted">
                调用 {{ formatNumber(healthOf(s)?.totalCalls ?? 0) }}
              </span>
              <span class="badge badge-muted">失败 {{ formatNumber(healthOf(s)?.failures ?? 0) }}</span>
              <span class="badge badge-muted">平均 {{ formatDuration(healthOf(s)?.avgLatencyMs) }}</span>
            </div>

            <div v-if="healthOf(s)?.lastError" class="small" style="margin-top: 6px; color: var(--error)">
              最近错误：<span :title="healthOf(s)?.lastError">{{ truncate(healthOf(s)?.lastError, 110) }}</span>
            </div>
            <div v-if="healthOf(s)?.openUntil" class="small muted" style="margin-top: 4px">
              熔断至 {{ formatTime(healthOf(s)?.openUntil) }}
            </div>
            <div v-if="!healthOf(s)" class="small faint" style="margin-top: 6px">
              暂无健康度数据（技能可能尚未被调用）
            </div>
          </div>

          <!-- 操作 -->
          <div class="row wrap" style="gap: 6px; margin-top: 12px">
            <button
              class="btn btn-sm"
              :disabled="acting === manifestOf(s)?.name"
              @click="toggleStatus(s)"
            >
              {{ manifestOf(s)?.status === 'enabled' ? '停用' : '启用' }}
            </button>
            <button class="btn btn-sm" :disabled="acting === manifestOf(s)?.name" @click="openCanary(s)">
              设为灰度
            </button>
            <button
              class="btn btn-sm"
              :disabled="acting === manifestOf(s)?.name"
              title="回滚到上一版本"
              @click="openRollback(s)"
            >
              回滚到上一版本
            </button>
            <button class="btn btn-sm btn-primary" @click="openSchema(s)">查看 Schema</button>
          </div>
        </div>
      </div>
    </div>

    <!-- 灰度设置 -->
    <AppModal v-if="canaryOpen" title="设置灰度比例" @close="canaryOpen = false">
      <dl class="kv" style="margin-bottom: 12px">
        <dt>技能</dt>
        <dd class="strong">{{ canaryTarget?.manifest?.title || canaryTarget?.manifest?.name }}</dd>
        <dt>当前版本</dt>
        <dd class="mono">v{{ canaryTarget?.manifest?.version || '?' }}</dd>
      </dl>
      <div class="field">
        <label class="field-label">灰度百分比（0 - 100）</label>
        <input v-model="canaryValue" class="input mono" type="number" min="0" max="100" placeholder="30" />
        <span class="field-hint">按任务维度按比例分流；设为 0 相当于停止灰度流量，设为 100 相当于全量。</span>
        <span v-if="canaryError" class="field-error">{{ canaryError }}</span>
      </div>
      <template #footer>
        <button class="btn" @click="canaryOpen = false">取消</button>
        <button class="btn btn-primary" @click="submitCanary">确认灰度</button>
      </template>
    </AppModal>

    <!-- 回滚确认 -->
    <AppModal v-if="rollbackOpen" title="回滚到上一版本" @close="rollbackOpen = false">
      <div class="alert alert-warn" style="margin-bottom: 12px">
        回滚会立即把该技能切换到历史版本，正在运行的灰度流量会被中断。
      </div>
      <div v-if="rollbackError" class="alert alert-error" style="margin-bottom: 12px">{{ rollbackError }}</div>
      <dl class="kv">
        <dt>技能</dt>
        <dd class="strong">{{ rollbackTarget?.manifest?.title || rollbackTarget?.manifest?.name }}</dd>
        <dt>当前版本</dt>
        <dd class="mono">v{{ rollbackTarget?.manifest?.version || '?' }}</dd>
        <dt>兜底链</dt>
        <dd class="mono small">{{ fallbackChain(rollbackTarget?.manifest) }}</dd>
      </dl>
      <template #footer>
        <button class="btn" :disabled="rollbackBusy" @click="rollbackOpen = false">取消</button>
        <button class="btn btn-danger" :disabled="rollbackBusy" @click="confirmRollback">
          {{ rollbackBusy ? '回滚中…' : '确认回滚' }}
        </button>
      </template>
    </AppModal>

    <!-- Schema -->
    <AppModal
      v-if="schemaOpen"
      :title="`入参 / 出参 Schema · ${schemaTarget?.manifest?.title || schemaTarget?.manifest?.name || ''}`"
      wide
      @close="schemaOpen = false"
    >
      <div class="row" style="margin-bottom: 8px">
        <span class="strong">输入 Schema</span>
        <span class="badge badge-muted mono">v{{ schemaTarget?.manifest?.version || '?' }}</span>
        <span class="spacer" />
        <CopyButton :text="schemaText('input', schemaTarget)" label="复制输入 Schema" title="复制输入 Schema" />
      </div>
      <pre class="code">{{ schemaText('input', schemaTarget) }}</pre>

      <div class="row" style="margin: 16px 0 8px">
        <span class="strong">输出 Schema</span>
        <span class="spacer" />
        <CopyButton
          :text="schemaText('output', schemaTarget)"
          label="复制输出 Schema"
          title="复制输出 Schema"
        />
      </div>
      <pre class="code">{{ schemaText('output', schemaTarget) }}</pre>

      <div class="alert alert-info small" style="margin-top: 14px">
        入参在调用前由 Schema 强校验；校验失败会直接熔断本次 Skill 调用并转入兜底链
        <span v-if="schemaTarget && (schemaTarget.manifest?.fallbackSkills ?? []).length">
          （{{ fallbackChain(schemaTarget.manifest) }}）
        </span>
        。
      </div>
    </AppModal>
  </div>
</template>

<style scoped>
.skill-health {
  margin-top: 12px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2);
}
</style>
