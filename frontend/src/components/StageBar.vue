<script setup lang="ts">
/** 任务阶段进度条：resolve → stack_parse → ... → archive。 */
import { computed } from 'vue'

const props = defineProps<{ currentStage?: string; state?: string }>()

const STAGES = [
  { key: 'resolve', label: '版本锁定' },
  { key: 'stack_parse', label: '堆栈解析' },
  { key: 'candidates', label: '仓库匹配' },
  { key: 'code_load', label: '代码懒加载' },
  { key: 'cross_repo', label: '跨仓库链路' },
  { key: 'root_cause', label: '根因推理' },
  { key: 'patch_synthesize', label: '补丁合成' },
  { key: 'sandbox_verify', label: '沙箱验证' },
  { key: 'report', label: '报告归档' },
]

// 运行态 → 当前阶段兜底：SSE 的 stage 事件缺失/中断时，依据权威状态推断高亮点。
// queued 必须兜底到首个阶段，否则「已受理未开始」时进度条会整体不亮。
const STAGE_BY_STATE: Record<string, string> = {
  queued: 'resolve',
  analyzing: 'stack_parse',
  repairing: 'patch_synthesize',
  verifying: 'sandbox_verify',
}

/** 流水线已走完的终态：整体点亮（failed/cancelled 可能中途中断，只亮到实际到达的阶段）。 */
const FULL_LIT_STATES = ['succeeded', 'needs_review', 'degraded']

const effectiveStage = computed(
  () => props.currentStage || STAGE_BY_STATE[props.state ?? ''] || '',
)
const currentIndex = computed(() => STAGES.findIndex((s) => s.key === effectiveStage.value))
const terminal = computed(() =>
  ['succeeded', 'needs_review', 'failed', 'cancelled', 'degraded'].includes(props.state ?? ''),
)

/** 计算某个阶段的展示状态。 */
function statusOf(index: number): string {
  if (terminal.value) {
    if (FULL_LIT_STATES.includes(props.state ?? '')) return 'done'
    if (currentIndex.value < 0) return ''
    return index <= currentIndex.value ? 'done' : ''
  }
  if (currentIndex.value < 0) return ''
  if (index < currentIndex.value) return 'done'
  if (index === currentIndex.value) return 'active'
  return ''
}
</script>

<template>
  <div class="stages">
    <template v-for="(s, i) in STAGES" :key="s.key">
      <span class="stage-chip" :class="statusOf(i)">
        <i class="dot" />
        {{ s.label }}
      </span>
      <span v-if="i < STAGES.length - 1" class="stage-arrow">→</span>
    </template>
  </div>
</template>
