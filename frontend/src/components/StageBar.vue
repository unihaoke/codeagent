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

const currentIndex = computed(() => STAGES.findIndex((s) => s.key === props.currentStage))
const terminal = computed(() =>
  ['succeeded', 'needs_review', 'failed', 'cancelled', 'degraded'].includes(props.state ?? ''),
)

/** 计算某个阶段的展示状态。 */
function statusOf(index: number): string {
  if (currentIndex.value < 0) return terminal.value ? 'done' : ''
  if (index < currentIndex.value) return 'done'
  if (index === currentIndex.value) return terminal.value ? 'done' : 'active'
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
