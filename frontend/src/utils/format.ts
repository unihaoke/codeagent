/** 通用格式化与展示辅助。 */
import type { RepoLayer, Severity, TaskMode, TaskState } from '@/types'

/** 毫秒耗时人类可读化。 */
export function formatDuration(ms?: number): string {
  if (!ms || ms <= 0) return '-'
  if (ms < 1000) return `${ms} ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(1)} s`
  const m = Math.floor(s / 60)
  const rest = Math.round(s % 60)
  if (m < 60) return `${m}m ${rest}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

/** 时间戳格式化（本地时区，秒级）。 */
export function formatTime(iso?: string): string {
  if (!iso) return '-'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '-'
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(
    d.getSeconds(),
  )}`
}

/** 相对时间（x 分钟前）。 */
export function formatRelative(iso?: string): string {
  if (!iso) return '-'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return '-'
  const diff = Date.now() - t
  if (diff < 0) return '刚刚'
  const min = Math.floor(diff / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return formatTime(iso)
}

/** 千分位数字。 */
export function formatNumber(n?: number): string {
  if (n === undefined || n === null) return '0'
  return n.toLocaleString('zh-CN')
}

/** 紧凑数字（1.2k / 3.4M）。 */
export function formatCompact(n?: number): string {
  const v = n ?? 0
  if (v < 1000) return String(v)
  if (v < 1_000_000) return `${(v / 1000).toFixed(1)}k`
  return `${(v / 1_000_000).toFixed(2)}M`
}

/** 百分比。 */
export function formatPercent(v?: number, digits = 1): string {
  const x = (v ?? 0) * 100
  return `${x.toFixed(digits)}%`
}

/** 字节数。 */
export function formatBytes(n?: number): string {
  const v = n ?? 0
  if (v < 1024) return `${v} B`
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`
  return `${(v / 1024 / 1024).toFixed(2)} MB`
}

/** 截断字符串。 */
export function truncate(s: string | undefined, n = 120): string {
  if (!s) return ''
  return s.length > n ? `${s.slice(0, n)}…` : s
}

/** 任务状态 → 中文标签与样式类。 */
export function stateMeta(state: TaskState | string): { label: string; tone: string } {
  switch (state) {
    case 'queued':
      return { label: '排队中', tone: 'muted' }
    case 'analyzing':
      return { label: '分析中', tone: 'info' }
    case 'repairing':
      return { label: '修复中', tone: 'warn' }
    case 'verifying':
      return { label: '验证中', tone: 'warn' }
    case 'succeeded':
      return { label: '修复成功', tone: 'ok' }
    case 'needs_review':
      return { label: '待人工复核', tone: 'warn' }
    case 'failed':
      return { label: '失败', tone: 'error' }
    case 'cancelled':
      return { label: '已取消', tone: 'muted' }
    case 'degraded':
      return { label: '已降级', tone: 'degraded' }
    default:
      return { label: state || '未知', tone: 'muted' }
  }
}

/** 定级 → 标签与样式类。 */
export function severityMeta(sev?: Severity | string): { label: string; tone: string } {
  switch (sev) {
    case 'blocker':
      return { label: 'P0 阻断', tone: 'error' }
    case 'critical':
      return { label: 'P1 严重', tone: 'error' }
    case 'major':
      return { label: 'P2 主要', tone: 'warn' }
    case 'minor':
      return { label: 'P3 次要', tone: 'info' }
    case 'info':
      return { label: '提示', tone: 'muted' }
    default:
      return { label: '未定级', tone: 'muted' }
  }
}

/** 仓库分层 → 中文名。 */
export function layerLabel(layer?: RepoLayer | string): string {
  switch (layer) {
    case 'frontend':
      return '前端'
    case 'gateway':
      return '网关'
    case 'service':
      return '微服务'
    case 'middleware':
      return '中间件适配'
    case 'library':
      return '公共库'
    default:
      return '未分类'
  }
}

/** 任务模式 → 中文名。 */
export function modeLabel(mode?: TaskMode | string): string {
  if (mode === 'group') return '多仓库分组联合排查'
  if (mode === 'single_repo') return '单仓库精准修复'
  return mode || '-'
}

/** 技能分类 → 中文名。 */
export function skillCategoryLabel(c?: string): string {
  switch (c) {
    case 'parse':
      return '代码解析'
    case 'diagnose':
      return '报错溯源'
    case 'repair':
      return '自动修复'
    case 'verify':
      return '修复验证'
    case 'cross_repo':
      return '跨仓库分析'
    default:
      return c || '-'
  }
}

/** 根因分类 → 中文名。 */
export function categoryLabel(c?: string): string {
  const map: Record<string, string> = {
    null_pointer: '空指针 / Nil 解引用',
    index_out_of_bounds: '数组越界',
    type_error: '类型错误',
    class_not_found: '类/模块未找到',
    dependency_missing: '依赖缺失',
    version_conflict: '版本冲突',
    connection_refused: '连接被拒绝',
    timeout: '超时',
    divide_by_zero: '除零',
    concurrent_modification: '并发修改',
    serialization_error: '序列化异常',
    syntax_error: '语法错误',
    resource_leak: '资源泄漏',
    oom: '内存溢出',
    unknown: '未分类',
  }
  return map[c ?? ''] ?? (c || '未分类')
}

/** 风险等级 → 中文名。 */
export function riskLabel(r?: string): string {
  switch (r) {
    case 'low':
      return '低风险'
    case 'medium':
      return '中风险'
    case 'high':
      return '高风险'
    default:
      return r || '-'
  }
}

/** 复制到剪贴板。 */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

/** 触发浏览器下载。 */
export function downloadText(filename: string, text: string, mime = 'text/plain;charset=utf-8'): void {
  const blob = new Blob([text], { type: mime })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

/** 稳定的短哈希（用于示例数据展示）。 */
export function shortCommit(commit?: string): string {
  if (!commit) return '-'
  return commit.length > 10 ? commit.slice(0, 10) : commit
}
