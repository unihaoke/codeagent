<script setup lang="ts">
/** 状态徽标：任务状态 / 定级 / 通用色调。 */
import { computed } from 'vue'
import { severityMeta, stateMeta } from '@/utils/format'

const props = defineProps<{
  /** 直接指定色调（muted|info|ok|warn|error|degraded）。 */
  tone?: string
  /** 文本内容。 */
  label?: string
  /** 按任务状态渲染。 */
  state?: string
  /** 按严重级别渲染。 */
  severity?: string
  /** 是否显示脉冲圆点。 */
  pulse?: boolean
}>()

const resolved = computed(() => {
  if (props.state) return stateMeta(props.state)
  if (props.severity) return severityMeta(props.severity)
  return { label: props.label ?? '', tone: props.tone ?? 'muted' }
})

const isRunning = computed(() =>
  ['queued', 'analyzing', 'repairing', 'verifying'].includes(props.state ?? ''),
)
</script>

<template>
  <span class="badge" :class="`badge-${resolved.tone}`">
    <i v-if="pulse || isRunning" class="dot pulse" />
    {{ label ?? resolved.label }}
  </span>
</template>
