<script setup lang="ts">
/** 全局 Toast 通知宿主。 */
import { toasts, dismiss } from '@/composables/useToast'
</script>

<template>
  <div class="toast-wrap">
    <div v-for="t in toasts" :key="t.id" class="toast" :class="t.type" @click="dismiss(t.id)">
      <div class="row-between">
        <span class="strong">{{ t.message }}</span>
        <button class="btn-ghost btn btn-sm" title="关闭" @click.stop="dismiss(t.id)">×</button>
      </div>
      <div v-if="t.detail" class="toast-detail">{{ t.detail }}</div>
    </div>
  </div>
</template>
