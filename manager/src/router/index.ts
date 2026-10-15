// 路由：hash 模式（后端静态服务无需额外 fallback 配置）
// 未登录访问受保护页 → 重定向 /login；已登录访问 /login → 重定向 /
import { createRouter, createWebHashHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const routes = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/login/LoginView.vue'),
    meta: { title: '登录' },
  },
  {
    path: '/',
    component: () => import('@/layouts/AdminLayout.vue'),
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/views/dashboard/DashboardView.vue'),
        meta: { title: '仪表盘' },
      },
      {
        path: 'admins',
        name: 'admins',
        component: () => import('@/views/admins/AdminsView.vue'),
        meta: { title: '管理员' },
      },
      {
        path: 'llm/providers',
        name: 'llm-providers',
        component: () => import('@/views/llm/ProvidersView.vue'),
        meta: { title: 'LLM Provider' },
      },
      {
        path: 'llm/usage',
        name: 'llm-usage',
        component: () => import('@/views/llm/UsageView.vue'),
        meta: { title: 'Token 用量' },
      },
      {
        path: 'conduit/behavior-tree',
        name: 'conduit-bt',
        component: () => import('@/views/conduit/BehaviorTreeView.vue'),
        meta: { title: '行为树' },
      },
      {
        path: 'conduit/pipelines',
        name: 'conduit-pipelines',
        component: () => import('@/views/conduit/PipelinesView.vue'),
        meta: { title: '管线' },
      },
      {
        path: 'conduit/traces',
        name: 'conduit-traces',
        component: () => import('@/views/conduit/TracesView.vue'),
        meta: { title: 'Trace 审计' },
      },
      {
        path: 'conduit/traffic',
        name: 'conduit-traffic',
        component: () => import('@/views/conduit/TrafficView.vue'),
        meta: { title: '节点流量' },
      },
      {
        path: 'audit',
        name: 'audit',
        component: () => import('@/views/audit/AuditView.vue'),
        meta: { title: '操作审计' },
      },
      {
        path: 'groups',
        name: 'groups',
        component: () => import('@/views/groups/GroupsView.vue'),
        meta: { title: '群组' },
      },
      {
        path: 'users',
        name: 'users',
        component: () => import('@/views/users/UsersView.vue'),
        meta: { title: '用户' },
      },
      {
        path: 'knowledge',
        name: 'knowledge',
        component: () => import('@/views/knowledge/KnowledgeView.vue'),
        meta: { title: '知识库' },
      },
      {
        path: 'memories',
        name: 'memories',
        component: () => import('@/views/memory/MemoryView.vue'),
        meta: { title: '记忆' },
      },
      {
        path: 'plugins',
        name: 'plugins',
        component: () => import('@/views/plugins/PluginsView.vue'),
        meta: { title: '插件' },
      },
      {
        path: 'skills',
        name: 'skills',
        component: () => import('@/views/skills/SkillsView.vue'),
        meta: { title: 'Skills' },
      },
      {
        path: 'prompts',
        name: 'prompts',
        component: () => import('@/views/prompts/PromptsView.vue'),
        meta: { title: 'Prompt 模板' },
      },
      {
        path: 'stickers',
        name: 'stickers',
        component: () => import('@/views/stickers/StickersView.vue'),
        meta: { title: '表情包库' },
      },
      {
        path: 'commands',
        name: 'commands',
        component: () => import('@/views/commands/CommandsView.vue'),
        meta: { title: '命令' },
      },
      {
        path: 'antifraud',
        name: 'antifraud',
        component: () => import('@/views/antifraud/AntiFraudView.vue'),
        meta: { title: '反诈' },
      },
      {
        path: 'settings',
        name: 'settings',
        component: () => import('@/views/settings/SettingsView.vue'),
        meta: { title: '设置' },
      },
    ],
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
]

export const router = createRouter({
  history: createWebHashHistory(),
  routes,
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.ready) await auth.init()
  if (to.name === 'login') {
    if (auth.isLoggedIn) return { name: 'dashboard' }
    return true
  }
  if (!auth.isLoggedIn) return { name: 'login', query: { redirect: to.fullPath } }
  return true
})

router.afterEach((to) => {
  document.title = to.meta.title ? `${String(to.meta.title)} · 蓝妹管理面板` : '蓝妹管理面板'
})
