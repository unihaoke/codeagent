<script setup lang="ts">
/** 分页控件。 */
import { computed } from 'vue'

const props = defineProps<{ page: number; pageSize: number; total: number }>()
const emit = defineEmits<{ (e: 'change', page: number): void }>()

const pages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))
const from = computed(() => (props.total === 0 ? 0 : (props.page - 1) * props.pageSize + 1))
const to = computed(() => Math.min(props.total, props.page * props.pageSize))
</script>

<template>
  <div class="row-between" style="padding: 10px 4px">
    <span class="small muted">共 {{ total }} 条 · 当前 {{ from }}-{{ to }}</span>
    <div class="row">
      <button class="btn btn-sm" :disabled="page <= 1" @click="emit('change', page - 1)">上一页</button>
      <span class="small muted">{{ page }} / {{ pages }}</span>
      <button class="btn btn-sm" :disabled="page >= pages" @click="emit('change', page + 1)">下一页</button>
    </div>
  </div>
</template>
