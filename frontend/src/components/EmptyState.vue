<script setup lang="ts">
/** 空状态与加载骨架。 */
defineProps<{ title?: string; hint?: string; icon?: string }>()
</script>

<template>
  <div class="empty">
    <div style="font-size: 22px; margin-bottom: 6px">{{ icon ?? '∅' }}</div>
    <div class="strong">{{ title ?? '暂无数据' }}</div>
    <div v-if="hint" class="small" style="margin-top: 4px">{{ hint }}</div>
    <div v-if="$slots.default" style="margin-top: 12px">
      <slot />
    </div>
  </div>
</template>
