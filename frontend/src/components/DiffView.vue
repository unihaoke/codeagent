<script setup lang="ts">
/** 统一 diff 展示组件：解析 unified diff 并渲染为带行号与配色的视图。 */
import { computed } from 'vue'

const props = defineProps<{ diff: string; maxHeight?: string }>()

interface Row {
  kind: 'meta' | 'hunk' | 'add' | 'del' | 'ctx'
  text: string
  no: string
}

const rows = computed<Row[]>(() => {
  const out: Row[] = []
  let oldNo = 0
  let newNo = 0
  for (const raw of (props.diff ?? '').split('\n')) {
    if (raw.startsWith('@@')) {
      const m = /@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(raw)
      if (m) {
        oldNo = Number(m[1])
        newNo = Number(m[2])
      }
      out.push({ kind: 'hunk', text: raw, no: '' })
      continue
    }
    if (
      raw.startsWith('diff --git') ||
      raw.startsWith('index ') ||
      raw.startsWith('--- ') ||
      raw.startsWith('+++ ') ||
      raw.startsWith('\\ No newline')
    ) {
      out.push({ kind: 'meta', text: raw, no: '' })
      continue
    }
    if (raw.startsWith('+')) {
      out.push({ kind: 'add', text: raw.slice(1), no: String(newNo++) })
    } else if (raw.startsWith('-')) {
      out.push({ kind: 'del', text: raw.slice(1), no: String(oldNo++) })
    } else if (raw.startsWith(' ') || raw === '') {
      out.push({ kind: 'ctx', text: raw.slice(1), no: String(newNo) })
      oldNo++
      newNo++
    } else {
      out.push({ kind: 'ctx', text: raw, no: '' })
    }
  }
  return out
})

const stat = computed(() => {
  let add = 0
  let del = 0
  for (const r of rows.value) {
    if (r.kind === 'add') add++
    if (r.kind === 'del') del++
  }
  return { add, del }
})
</script>

<template>
  <div>
    <div class="row small muted" style="margin-bottom: 6px">
      <span class="badge badge-ok">+{{ stat.add }}</span>
      <span class="badge badge-error">-{{ stat.del }}</span>
      <span>{{ rows.filter((r) => r.kind === 'hunk').length }} 个变更块</span>
    </div>
    <div class="diff" :style="maxHeight ? { maxHeight } : undefined">
      <div v-for="(r, i) in rows" :key="i" class="diff-line" :class="r.kind">
        <span class="ln">{{ r.no }}</span>
        <span class="txt">{{ r.kind === 'add' ? '+' : r.kind === 'del' ? '-' : r.kind === 'ctx' ? ' ' : '' }}{{ r.text }}</span>
      </div>
    </div>
  </div>
</template>
