/** 认证状态（控制台 JWT）与租户上下文。 */
import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api, clearAuth, getAuth, setAuth } from '@/api/client'
import type { Subject, Tenant } from '@/types'

export const useAuthStore = defineStore('auth', () => {
  const token = ref(getAuth().token)
  const apiKey = ref(getAuth().apiKey)
  const subject = ref<Subject | null>(getAuth().subject)
  const tenant = ref<Tenant | null>(null)
  const loading = ref(false)
  const error = ref('')

  /** 控制台登录。 */
  async function login(tenantKey: string, username: string, password: string): Promise<boolean> {
    loading.value = true
    error.value = ''
    try {
      const res = await api.login(tenantKey, username, password)
      token.value = res.token
      apiKey.value = ''
      subject.value = res.subject
      setAuth({ token: res.token, apiKey: '', subject: res.subject })
      return true
    } catch (e) {
      error.value = e instanceof Error ? e.message : '登录失败'
      return false
    } finally {
      loading.value = false
    }
  }

  /**
   * 直接使用接入层 API Key（免登录调试）。
   *
   * 主体一律从 GET /auth/profile 拉取：本地伪造 tenantId / admin / scopes 会让
   * "租户与权限"页显示假数据（前端以为在 t-demo 且拥有 admin:all，服务端却按 Key
   * 的真实租户与 scopes 鉴权），表现为"页面显示有权限、请求却被 403"。
   * 返回 false 表示 Key 无效或无权访问接入层。
   */
  async function useApiKey(key: string): Promise<boolean> {
    loading.value = true
    error.value = ''
    apiKey.value = key
    token.value = ''
    try {
      const real = await api.profile()
      subject.value = real
      tenant.value = null
      setAuth({ apiKey: key, token: '', subject: real })
      return true
    } catch (e) {
      apiKey.value = ''
      subject.value = null
      setAuth({ apiKey: '', token: '', subject: null })
      error.value = e instanceof Error ? e.message : 'API Key 校验失败'
      return false
    } finally {
      loading.value = false
    }
  }

  /** 退出登录。 */
  function logout(): void {
    token.value = ''
    apiKey.value = ''
    subject.value = null
    tenant.value = null
    clearAuth()
  }

  /** 拉取当前主体。 */
  async function refreshProfile(): Promise<void> {
    if (!token.value && !apiKey.value) return
    try {
      subject.value = await api.profile()
      setAuth({ subject: subject.value })
    } catch {
      /* 静默失败：由请求拦截提示 */
    }
  }

  /** 拉取当前租户信息。 */
  async function loadTenant(): Promise<void> {
    try {
      tenant.value = await api.currentTenant()
    } catch {
      tenant.value = null
    }
  }

  /** 是否已认证。 */
  function isAuthenticated(): boolean {
    return Boolean(token.value || apiKey.value)
  }

  return {
    token,
    apiKey,
    subject,
    tenant,
    loading,
    error,
    login,
    logout,
    useApiKey,
    refreshProfile,
    loadTenant,
    isAuthenticated,
  }
})
