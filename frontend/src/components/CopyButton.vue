<script setup lang="ts">
/** 通用小组件集合：复制按钮、空状态、加载态、键值列表、条形图。 */
import { ref } from 'vue'
import { copyText } from '@/utils/format'
import { toastError, toastOk } from '@/composables/useToast'

defineProps<{
  text: string
  label?: string
  title?: string
}>()

const copied = ref(false)

async function doCopy(text: string) {
  const ok = await copyText(text)
  copied.value = ok
  if (ok) {
    toastOk('已复制到剪贴板')
    setTimeout(() => (copied.value = false), 1500)
    return
  }
  toastError('复制失败', '浏览器未授予剪贴板权限，请手动选中后复制')
}
</script>

<template>
  <button class="btn btn-sm btn-ghost" :title="title ?? '复制'" @click="doCopy(text)">
    {{ copied ? '已复制' : (label ?? '复制') }}
  </button>
</template>
