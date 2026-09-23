<script setup lang="ts">
/** 极简 Markdown 渲染（标题/表格/列表/代码块/引用/加粗/行内代码），无第三方依赖。 */
import { computed } from 'vue'

const props = defineProps<{ source: string }>()

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function inline(s: string): string {
  return escapeHtml(s)
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|\s)\*([^*]+)\*/g, '$1<em>$2</em>')
    .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>')
}

const html = computed(() => {
  const lines = (props.source ?? '').split('\n')
  const out: string[] = []
  let inCode = false
  let inTable = false
  let inList: 'ul' | 'ol' | null = null

  const closeList = () => {
    if (inList) {
      out.push(`</${inList}>`)
      inList = null
    }
  }
  const closeTable = () => {
    if (inTable) {
      out.push('</tbody></table>')
      inTable = false
    }
  }

  for (const raw of lines) {
    const line = raw.replace(/\r$/, '')

    if (line.trim().startsWith('```')) {
      closeList()
      closeTable()
      if (inCode) {
        out.push('</code></pre>')
        inCode = false
      } else {
        out.push('<pre><code>')
        inCode = true
      }
      continue
    }
    if (inCode) {
      out.push(escapeHtml(line))
      continue
    }

    // 表格
    if (/^\s*\|.*\|\s*$/.test(line)) {
      const cells = line.trim().slice(1, -1).split('|').map((c) => c.trim())
      if (/^[\s|:-]+$/.test(line) && cells.every((c) => /^:?-{2,}:?$/.test(c) || c === '')) {
        continue // 分隔行
      }
      if (!inTable) {
        out.push('<table><thead><tr>')
        out.push(cells.map((c) => `<th>${inline(c)}</th>`).join(''))
        out.push('</tr></thead><tbody>')
        inTable = true
      } else {
        out.push('<tr>')
        out.push(cells.map((c) => `<td>${inline(c)}</td>`).join(''))
        out.push('</tr>')
      }
      continue
    }
    closeTable()

    if (/^\s*$/.test(line)) {
      closeList()
      continue
    }
    const h = /^(#{1,4})\s+(.*)$/.exec(line)
    if (h) {
      closeList()
      const level = h[1].length
      out.push(`<h${level}>${inline(h[2])}</h${level}>`)
      continue
    }
    if (/^>\s?/.test(line)) {
      closeList()
      out.push(`<blockquote>${inline(line.replace(/^>\s?/, ''))}</blockquote>`)
      continue
    }
    const ul = /^\s*[-*+]\s+(.*)$/.exec(line)
    if (ul) {
      if (inList !== 'ul') {
        closeList()
        out.push('<ul>')
        inList = 'ul'
      }
      out.push(`<li>${inline(ul[1])}</li>`)
      continue
    }
    const ol = /^\s*\d+[.)]\s+(.*)$/.exec(line)
    if (ol) {
      if (inList !== 'ol') {
        closeList()
        out.push('<ol>')
        inList = 'ol'
      }
      out.push(`<li>${inline(ol[1])}</li>`)
      continue
    }
    if (/^\s*(---|\*\*\*|___)\s*$/.test(line)) {
      closeList()
      out.push('<hr />')
      continue
    }
    closeList()
    out.push(`<p>${inline(line)}</p>`)
  }
  closeList()
  closeTable()
  if (inCode) out.push('</code></pre>')
  return out.join('\n')
})
</script>

<template>
  <div class="markdown" v-html="html" />
</template>
