<script setup lang="ts">
/** 水平条形图（纯 CSS，用于状态分布 / 分类分布）。 */
import { computed } from 'vue'

const props = withDefaults(
  defineProps<{
    data: Record<string, number> | { label: string; value: number }[]
    labelMap?: Record<string, string>
    tone?: string
    max?: number
  }>(),
  { tone: 'var(--brand-500)' },
)

const rows = computed(() => {
  const list = Array.isArray(props.data)
    ? props.data
    : Object.entries(props.data ?? {}).map(([label, value]) => ({ label, value: Number(value) }))
  const sorted = [...list].sort((a, b) => b.value - a.value)
  const max = props.max ?? Math.max(1, ...sorted.map((r) => r.value))
  return sorted.map((r) => ({
    ...r,
    display: props.labelMap?.[r.label] ?? r.label,
    pct: Math.round((r.value / max) * 100),
  }))
})
</script>

<template>
  <div class="chart-map">
    <div v-if="!rows.length" class="muted small">暂无数据</div>
    <div v-for="r in rows" :key="r.label" class="chart-map-row">
      <span class="ellipsis" :title="r.display">{{ r.display }}</span>
      <span class="chart-map-bar">
        <span :style="{ width: `${r.pct}%`, background: tone }" />
      </span>
      <span class="nowrap" style="text-align: right">{{ r.value }}</span>
    </div>
  </div>
</template>
