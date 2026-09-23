import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { getAuth } from '@/api/client'

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { public: true, title: '登录' },
  },
  {
    path: '/',
    component: () => import('@/layouts/AppLayout.vue'),
    children: [
      {
        path: '',
        name: 'dashboard',
        component: () => import('@/views/DashboardView.vue'),
        meta: { title: '运行监控台', icon: 'dashboard' },
      },
      {
        path: 'tasks',
        name: 'tasks',
        component: () => import('@/views/TasksView.vue'),
        meta: { title: '任务中心', icon: 'tasks' },
      },
      {
        path: 'tasks/new',
        name: 'task-new',
        component: () => import('@/views/TaskCreateView.vue'),
        meta: { title: '发起排查', icon: 'plus' },
      },
      {
        path: 'tasks/:id',
        name: 'task-detail',
        component: () => import('@/views/TaskDetailView.vue'),
        meta: { title: '任务详情', hidden: true },
      },
      {
        path: 'reports',
        name: 'reports',
        component: () => import('@/views/ReportsView.vue'),
        meta: { title: '修复报告', icon: 'report' },
      },
      {
        path: 'reports/:id',
        name: 'report-detail',
        component: () => import('@/views/ReportDetailView.vue'),
        meta: { title: '报告详情', hidden: true },
      },
      {
        path: 'repos',
        name: 'repos',
        component: () => import('@/views/ReposView.vue'),
        meta: { title: '代码源仓库', icon: 'repo' },
      },
      {
        path: 'groups',
        name: 'groups',
        component: () => import('@/views/GroupsView.vue'),
        meta: { title: '业务分组', icon: 'group' },
      },
      {
        path: 'groups/:id',
        name: 'group-detail',
        component: () => import('@/views/GroupDetailView.vue'),
        meta: { title: '分组详情', hidden: true },
      },
      {
        path: 'skills',
        name: 'skills',
        component: () => import('@/views/SkillsView.vue'),
        meta: { title: 'Skill 插件', icon: 'skill' },
      },
      {
        path: 'models',
        name: 'models',
        component: () => import('@/views/ModelsView.vue'),
        meta: { title: 'MCP 模型管控', icon: 'model' },
      },
      {
        path: 'ai-settings',
        name: 'ai-settings',
        component: () => import('@/views/AISettingsView.vue'),
        meta: { title: 'AI 设置', icon: 'setting' },
      },
      {
        path: 'tenants',
        name: 'tenants',
        component: () => import('@/views/TenantsView.vue'),
        meta: { title: '租户与权限', icon: 'tenant' },
      },
      {
        path: 'audits',
        name: 'audits',
        component: () => import('@/views/AuditView.vue'),
        meta: { title: '可观测审计', icon: 'audit' },
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { public: true, title: '页面不存在' },
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

router.beforeEach((to) => {
  const auth = getAuth()
  const authenticated = Boolean(auth.token || auth.apiKey)
  if (!to.meta.public && !authenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && authenticated) {
    return { name: 'dashboard' }
  }
  return true
})

router.afterEach((to) => {
  const title = (to.meta.title as string | undefined) ?? ''
  document.title = title ? `${title} · CodeAgent 控制台` : 'CodeAgent 控制台'
})

export default router
