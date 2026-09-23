/** 全局轻量 Toast 通知（无第三方依赖）。 */
import { reactive } from 'vue'

export interface Toast {
  id: number
  type: 'info' | 'ok' | 'warn' | 'error'
  message: string
  detail?: string
}

let seq = 0

export const toasts = reactive<Toast[]>([])

/** 推送一条通知。 */
export function notify(type: Toast['type'], message: string, detail?: string, ttl = 4000): void {
  const id = ++seq
  toasts.push({ id, type, message, detail })
  if (ttl > 0) {
    setTimeout(() => dismiss(id), ttl)
  }
}

/** 关闭指定通知。 */
export function dismiss(id: number): void {
  const idx = toasts.findIndex((t) => t.id === id)
  if (idx >= 0) toasts.splice(idx, 1)
}

/** 成功通知。 */
export const toastOk = (m: string, d?: string) => notify('ok', m, d)
/** 错误通知（默认不自动关闭）。 */
export const toastError = (m: string, d?: string) => notify('error', m, d, 8000)
/** 警告通知。 */
export const toastWarn = (m: string, d?: string) => notify('warn', m, d, 6000)
/** 信息通知。 */
export const toastInfo = (m: string, d?: string) => notify('info', m, d)
