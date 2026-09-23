<script setup lang="ts">
/** 指标卡片。 */
defineProps<{
  label: string
  value: string | number
  hint?: string
  tone?: '' | 'ok' | 'warn' | 'error' | 'info' | 'degraded'
  small?: boolean
}>()
</script>

<template>
  <div class="stat">
    <div class="stat-label">{{ label }}</div>
    <div class="stat-value" :class="{ sm: small }" :style="tone ? { color: `var(--${tone})` } : undefined">
      {{ value }}
    </div>
    <div v-if="hint" class="stat-hint">{{ hint }}</div>
  </div>
</template>
