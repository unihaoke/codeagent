<script setup lang="ts">
/** 通用模态框。 */
import { onMounted, onUnmounted } from 'vue'

const props = withDefaults(defineProps<{ title: string; wide?: boolean; closeOnMask?: boolean }>(), {
  wide: false,
  closeOnMask: true,
})

const emit = defineEmits<{ (e: 'close'): void }>()

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}

onMounted(() => window.addEventListener('keydown', onKey))
onUnmounted(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <div class="modal-mask" @click.self="props.closeOnMask && emit('close')">
    <div class="modal" :class="{ wide: props.wide }">
      <div class="modal-head">
        <span>{{ title }}</span>
        <button class="btn btn-sm btn-ghost" @click="emit('close')">×</button>
      </div>
      <div class="modal-body">
        <slot />
      </div>
      <div v-if="$slots.footer" class="modal-foot">
        <slot name="footer" />
      </div>
    </div>
  </div>
</template>
