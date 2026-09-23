/** 全局实时事件流：WebSocket 连接、事件缓冲、任务实时状态。 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { connectEventStream } from '@/api/client'
import type { AgentEvent, TaskState } from '@/types'

const MAX_BUFFER = 300

export const useRealtimeStore = defineStore('realtime', () => {
  const events = ref<AgentEvent[]>([])
  const connected = ref(false)
  const runStates = ref<Record<string, TaskState>>({})
  const lastError = ref('')
  let close: (() => void) | null = null
  let retryTimer: number | null = null

  /** 建立连接（幂等）。 */
  function connect(): void {
    if (close) return
    const shutdown = connectEventStream((ev) => {
      connected.value = true
      push(ev)
    })
    close = () => {
      shutdown()
      close = null
      connected.value = false
    }
    connected.value = true
    // 断线重连（WebSocket 自身不保证重连语义）
    const watchdog = window.setInterval(() => {
      if (!connected.value) {
        shutdown()
        close = null
        if (retryTimer) window.clearTimeout(retryTimer)
        retryTimer = window.setTimeout(() => connect(), 3000)
      }
    }, 15000)
    const originalClose = close
    close = () => {
      window.clearInterval(watchdog)
      if (retryTimer) window.clearTimeout(retryTimer)
      originalClose()
    }
  }

  /** 主动断开。 */
  function disconnect(): void {
    close?.()
    close = null
    connected.value = false
  }

  function push(ev: AgentEvent): void {
    events.value.push(ev)
    if (events.value.length > MAX_BUFFER) {
      events.value.splice(0, events.value.length - MAX_BUFFER)
    }
    if (ev.type === 'task.state') {
      const payload = ev.payload as { to?: TaskState } | undefined
      if (ev.runId && payload?.to) runStates.value[ev.runId] = payload.to
    }
    if (ev.type === 'task.created' && ev.runId) {
      runStates.value[ev.runId] = 'queued'
    }
  }

  /** 订阅某个运行的事件。 */
  function eventsOf(runId: string): AgentEvent[] {
    return events.value.filter((e) => e.runId === runId)
  }

  /** 最近 N 条事件（倒序）。 */
  function recent(n = 50): AgentEvent[] {
    return [...events.value].reverse().slice(0, n)
  }

  const runningCount = computed(
    () => Object.values(runStates.value).filter((s) => ['queued', 'analyzing', 'repairing', 'verifying'].includes(s)).length,
  )

  return { events, connected, runStates, lastError, connect, disconnect, push, eventsOf, recent, runningCount }
})
