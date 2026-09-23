/** 数据加载组合式函数：加载态、错误态、自动刷新、分页。 */
import { onMounted, onUnmounted, ref, type Ref } from 'vue'
import { ApiError } from '@/api/client'
import { toastError } from '@/composables/useToast'

export interface AsyncOptions {
  /** 自动刷新间隔（毫秒），0 表示不刷新。 */
  interval?: number
  /** 首次挂载即加载。 */
  immediate?: boolean
  /** 失败时是否弹出提示。 */
  silent?: boolean
}

export interface AsyncResult<T> {
  data: Ref<T | null>
  loading: Ref<boolean>
  error: Ref<string>
  reload: () => Promise<void>
}

/** 加载远程数据并管理加载/错误状态。 */
export function useAsync<T>(loader: () => Promise<T>, options: AsyncOptions = {}): AsyncResult<T> {
  const data = ref<T | null>(null) as Ref<T | null>
  const loading = ref(false)
  const error = ref('')
  let timer: number | null = null

  async function reload() {
    loading.value = true
    error.value = ''
    try {
      data.value = await loader()
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : e instanceof Error ? e.message : '加载失败'
      error.value = msg
      if (!options.silent) toastError('数据加载失败', msg)
    } finally {
      loading.value = false
    }
  }

  onMounted(() => {
    if (options.immediate !== false) void reload()
    if (options.interval && options.interval > 0) {
      timer = window.setInterval(() => void reload(), options.interval)
    }
  })

  onUnmounted(() => {
    if (timer) window.clearInterval(timer)
  })

  return { data, loading, error, reload }
}

/** 分页查询状态管理。 */
export function usePaged<T>(
  loader: (page: number, pageSize: number, keyword: string) => Promise<{ items: T[]; total: number }>,
  pageSize = 20,
) {
  const items = ref<T[]>([]) as Ref<T[]>
  const total = ref(0)
  const page = ref(1)
  const keyword = ref('')
  const loading = ref(false)

  async function load() {
    loading.value = true
    try {
      const res = await loader(page.value, pageSize, keyword.value)
      items.value = res.items ?? []
      total.value = res.total ?? 0
    } catch (e) {
      toastError('列表加载失败', e instanceof Error ? e.message : String(e))
      items.value = []
      total.value = 0
    } finally {
      loading.value = false
    }
  }

  function go(p: number) {
    page.value = Math.max(1, p)
    void load()
  }

  function search(k: string) {
    keyword.value = k
    page.value = 1
    void load()
  }

  onMounted(() => void load())

  return { items, total, page, pageSize, keyword, loading, load, go, search }
}
