<script setup lang="ts">
/** 控制台主布局：侧边导航 + 顶栏 + 内容区。 */
import { computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useRealtimeStore } from '@/stores/realtime'
import { useUiStore } from '@/stores/ui'
import { formatNumber } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const realtime = useRealtimeStore()
const ui = useUiStore()

interface NavItem {
  name: string
  label: string
  icon: string
  group: string
}

const NAV: NavItem[] = [
  { name: 'dashboard', label: '运行监控台', icon: '◧', group: '运行' },
  { name: 'tasks', label: '任务中心', icon: '☰', group: '运行' },
  { name: 'task-new', label: '发起排查', icon: '＋', group: '运行' },
  { name: 'reports', label: '修复报告', icon: '⎙', group: '运行' },
  { name: 'repos', label: '代码源仓库', icon: '⌸', group: '代码源' },
  { name: 'groups', label: '业务分组', icon: '⧉', group: '代码源' },
  { name: 'skills', label: 'Skill 插件', icon: '⚙', group: '能力治理' },
  { name: 'models', label: 'MCP 模型管控', icon: '⚛', group: '能力治理' },
  { name: 'ai-settings', label: 'AI 设置', icon: '⚡', group: '能力治理' },
  { name: 'tenants', label: '租户与权限', icon: '⚿', group: '平台' },
  { name: 'audits', label: '可观测审计', icon: '◈', group: '平台' },
]

const groups = computed(() => {
  const map = new Map<string, NavItem[]>()
  for (const item of NAV) {
    const list = map.get(item.group) ?? []
    list.push(item)
    map.set(item.group, list)
  }
  return [...map.entries()].map(([group, items]) => ({ group, items }))
})

const activeName = computed(() => (route.name as string) ?? '')
const tenantLabel = computed(() => auth.tenant?.name ?? auth.subject?.tenantId ?? '未知租户')

onMounted(async () => {
  await auth.loadTenant()
})

function logout() {
  realtime.disconnect()
  auth.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="shell" :class="{ collapsed: ui.sidebarCollapsed }">
    <aside class="sidebar">
      <div class="brand">
        <span class="brand-mark">CA</span>
        <div v-if="!ui.sidebarCollapsed" class="brand-text">
          <div class="strong">CodeAgent</div>
          <div class="small faint">多仓库分组代码修复</div>
        </div>
      </div>

      <nav class="nav">
        <template v-for="g in groups" :key="g.group">
          <div v-if="!ui.sidebarCollapsed" class="nav-group">{{ g.group }}</div>
          <router-link
            v-for="item in g.items"
            :key="item.name"
            class="nav-item"
            :class="{ active: activeName === item.name }"
            :to="{ name: item.name }"
            :title="item.label"
          >
            <span class="nav-icon">{{ item.icon }}</span>
            <span v-if="!ui.sidebarCollapsed">{{ item.label }}</span>
          </router-link>
        </template>
      </nav>

      <div class="sidebar-foot">
        <button class="btn btn-sm btn-ghost btn-block" @click="ui.toggleSidebar()">
          {{ ui.sidebarCollapsed ? '»' : '« 收起' }}
        </button>
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <div class="row" style="min-width: 0">
          <span class="strong nowrap">{{ (route.meta.title as string) ?? '控制台' }}</span>
          <span class="badge badge-muted">{{ tenantLabel }}</span>
        </div>
        <div class="spacer" />
        <div class="row wrap">
          <span class="badge" :class="realtime.connected ? 'badge-ok' : 'badge-muted'">
            <i class="dot" :class="{ pulse: realtime.connected }" />
            {{ realtime.connected ? '事件流已连接' : '事件流断开' }}
          </span>
          <span v-if="realtime.runningCount > 0" class="badge badge-info">
            运行中 {{ formatNumber(realtime.runningCount) }}
          </span>
          <button class="btn btn-sm btn-ghost" :title="ui.theme === 'dark' ? '切换明亮主题' : '切换深色主题'" @click="ui.toggleTheme()">
            {{ ui.theme === 'dark' ? '☀' : '☾' }}
          </button>
          <button class="btn btn-sm" @click="logout">退出</button>
        </div>
      </header>

      <main class="content">
        <router-view v-slot="{ Component }">
          <component :is="Component" />
        </router-view>
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell {
  display: grid;
  grid-template-columns: 232px minmax(0, 1fr);
  height: 100vh;
  overflow: hidden;
}

.shell.collapsed {
  grid-template-columns: 64px minmax(0, 1fr);
}

.sidebar {
  background: var(--bg-elev);
  border-right: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 16px;
  border-bottom: 1px solid var(--border);
}

.brand-mark {
  width: 30px;
  height: 30px;
  border-radius: 8px;
  background: linear-gradient(135deg, var(--brand-500), #7b5cff);
  color: #fff;
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 12px;
  flex: none;
}

.nav {
  flex: 1;
  overflow-y: auto;
  padding: 10px 8px;
}

.nav-group {
  font-size: 11px;
  color: var(--text-faint);
  padding: 12px 10px 5px;
  letter-spacing: 0.6px;
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  color: var(--text-dim);
  font-size: 13px;
  transition: var(--transition);
  text-decoration: none;
}

.nav-item:hover {
  background: var(--bg-hover);
  color: var(--text);
  text-decoration: none;
}

.nav-item.active {
  background: var(--brand-soft);
  color: var(--brand-400);
  font-weight: 580;
}

.nav-icon {
  width: 18px;
  text-align: center;
  flex: none;
}

.sidebar-foot {
  padding: 10px;
  border-top: 1px solid var(--border);
}

.main {
  display: flex;
  flex-direction: column;
  min-width: 0;
  overflow: hidden;
}

.topbar {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 11px 20px;
  border-bottom: 1px solid var(--border);
  background: var(--bg-elev);
  flex-wrap: wrap;
}

.content {
  flex: 1;
  overflow-y: auto;
  padding: 20px;
}
</style>
