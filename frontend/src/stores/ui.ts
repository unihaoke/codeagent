/** 全局 UI 偏好（主题、侧边栏、刷新频率）。 */
import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

export type ThemeMode = 'dark' | 'light'

const THEME_KEY = 'codeagent.theme'
const SIDEBAR_KEY = 'codeagent.sidebar'

export const useUiStore = defineStore('ui', () => {
  const theme = ref<ThemeMode>((localStorage.getItem(THEME_KEY) as ThemeMode) || 'dark')
  const sidebarCollapsed = ref(localStorage.getItem(SIDEBAR_KEY) === '1')
  const autoRefreshSec = ref(10)

  function applyTheme(): void {
    document.documentElement.dataset.theme = theme.value
  }

  function toggleTheme(): void {
    theme.value = theme.value === 'dark' ? 'light' : 'dark'
  }

  function toggleSidebar(): void {
    sidebarCollapsed.value = !sidebarCollapsed.value
  }

  watch(
    theme,
    () => {
      localStorage.setItem(THEME_KEY, theme.value)
      applyTheme()
    },
    { immediate: true },
  )

  watch(sidebarCollapsed, (v) => localStorage.setItem(SIDEBAR_KEY, v ? '1' : '0'))

  return { theme, sidebarCollapsed, autoRefreshSec, toggleTheme, toggleSidebar, applyTheme }
})
