<script setup lang="ts">
// 管理面板主布局：响应式侧栏导航 + 顶栏（主题切换 / 用户菜单）
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import {
  BarChart3,
  BookOpen,
  Bot,
  BrainCircuit,
  Coins,
  Command,
  FileText,
  KeyRound,
  Library,
  Menu,
  Moon,
  PanelLeft,
  Puzzle,
  Settings,
  ShieldAlert,
  ShieldCheck,
  Sparkles,
  Sticker,
  Sun,
  Users,
  Workflow,
} from 'lucide-vue-next'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

const app = useAppStore()
const auth = useAuthStore()
const route = useRoute()
const router = useRouter()

const sidebarVisible = ref(!app.isMobile)

// 侧栏菜单项（icon 用渲染函数渲染，避免模板大量判断）
interface MenuItem {
  path: string
  title: string
  icon: string
  children?: { path: string; title: string }[]
}

const menuItems: MenuItem[] = [
  { path: '/dashboard', title: '仪表盘', icon: 'dashboard' },
  {
    path: '/conduit',
    title: 'Conduit 控制台',
    icon: 'workflow',
    children: [
      { path: '/conduit/behavior-tree', title: '行为树' },
      { path: '/conduit/pipelines', title: '管线' },
      { path: '/conduit/traces', title: 'Trace 审计' },
      { path: '/conduit/traffic', title: '节点流量' },
    ],
  },
  {
    path: '/llm',
    title: 'LLM 与计费',
    icon: 'coins',
    children: [
      { path: '/llm/providers', title: 'Provider 管理' },
      { path: '/llm/usage', title: 'Token 用量' },
    ],
  },
  { path: '/admins', title: '管理员', icon: 'users' },
  {
    path: '/content',
    title: '内容管理',
    icon: 'library',
    children: [
      { path: '/groups', title: '群组' },
      { path: '/users', title: '用户' },
      { path: '/knowledge', title: '知识库' },
      { path: '/memories', title: '记忆' },
      { path: '/plugins', title: '插件' },
      { path: '/skills', title: 'Skills' },
      { path: '/prompts', title: 'Prompt 模板' },
      { path: '/stickers', title: '表情包库' },
      { path: '/commands', title: '命令' },
    ],
  },
  { path: '/antifraud', title: '反诈', icon: 'antifraud' },
  { path: '/audit', title: '操作审计', icon: 'shield' },
  { path: '/settings', title: '设置', icon: 'settings' },
]

function menuIcon(name: string) {
  const map: Record<string, unknown> = {
    dashboard: BarChart3,
    workflow: Workflow,
    coins: Coins,
    users: Users,
    shield: ShieldCheck,
    antifraud: ShieldAlert,
    settings: Settings,
    library: Library,
    book: BookOpen,
    brain: BrainCircuit,
    puzzle: Puzzle,
    sparkles: Sparkles,
    file: FileText,
    sticker: Sticker,
    command: Command,
  }
  return map[name]
}

const activePath = computed(() => route.path)

function navigate(path: string) {
  router.push(path)
  if (app.isMobile) sidebarVisible.value = false
}

function toggleSidebar() {
  if (app.isMobile) {
    sidebarVisible.value = !sidebarVisible.value
  } else {
    app.toggleSidebar()
  }
}

async function handleLogout() {
  await auth.logout()
  MessagePlugin.success('已退出登录')
  router.push({ name: 'login' })
}

function handleResize() {
  app.setMobile(window.innerWidth < 768)
}

onMounted(() => {
  handleResize()
  window.addEventListener('resize', handleResize)
})
onBeforeUnmount(() => window.removeEventListener('resize', handleResize))
</script>

<template>
  <t-layout class="admin-layout">
    <!-- 移动端遮罩 -->
    <div v-if="app.isMobile && sidebarVisible" class="layout-mask" @click="sidebarVisible = false" />

    <t-aside :width="app.isMobile ? '260px' : app.collapsed ? '64px' : '220px'" class="layout-aside" :class="{ collapsed: app.collapsed }">
      <div class="brand">
        <Bot :size="26" />
        <span v-show="!app.collapsed" class="brand-text">蓝妹管理面板</span>
      </div>

      <t-menu v-model="activePath" :theme="app.theme === 'dark' ? 'dark' : 'light'" class="side-menu">
        <template v-for="item in menuItems" :key="item.path">
          <t-submenu v-if="item.children" :value="item.path" :title="item.title">
            <template #icon>
              <component :is="menuIcon(item.icon)" :size="18" />
            </template>
            <t-menu-item v-for="child in item.children" :key="child.path" :value="child.path" @click="navigate(child.path)">
              {{ child.title }}
            </t-menu-item>
          </t-submenu>
          <t-menu-item v-else :value="item.path" @click="navigate(item.path)">
            <template #icon>
              <component :is="menuIcon(item.icon)" :size="18" />
            </template>
            {{ item.title }}
          </t-menu-item>
        </template>
      </t-menu>
    </t-aside>

    <t-layout class="layout-main">
      <t-header class="layout-header">
        <div class="header-left">
          <t-button variant="text" shape="square" @click="toggleSidebar">
            <Menu v-if="app.isMobile" :size="20" />
            <PanelLeft v-else :size="20" />
          </t-button>
          <span class="header-title">{{ String(route.meta.title ?? '') }}</span>
        </div>
        <div class="header-right">
          <t-button variant="text" shape="square" :title="app.theme === 'dark' ? '切换到亮色' : '切换到暗色'" @click="app.toggleTheme()">
            <Sun v-if="app.theme === 'dark'" :size="18" />
            <Moon v-else :size="18" />
          </t-button>
          <t-dropdown>
            <span class="user-chip">
              <t-avatar size="small">{{ (auth.me?.display_name || auth.me?.username || 'A').slice(0, 1) }}</t-avatar>
              <span class="user-name">{{ auth.me?.display_name || auth.me?.username }}</span>
            </span>
            <template #dropdown>
              <t-dropdown-menu>
                <t-dropdown-item @click="router.push('/settings')">
                  <template #prefix><KeyRound :size="16" /></template>
                  账号与安全
                </t-dropdown-item>
                <t-dropdown-item @click="handleLogout">退出登录</t-dropdown-item>
              </t-dropdown-menu>
            </template>
          </t-dropdown>
        </div>
      </t-header>

      <t-content class="layout-content">
        <router-view />
      </t-content>
    </t-layout>
  </t-layout>
</template>

<style scoped>
.admin-layout {
  height: 100%;
}

.layout-mask {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.45);
  z-index: 999;
}

.layout-aside {
  transition: width 0.2s ease;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  background: var(--mgr-sidebar-bg);
  border-right: 1px solid var(--mgr-border);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 18px 16px;
  color: var(--mgr-sidebar-text-active);
  font-weight: 600;
  white-space: nowrap;
}

.side-menu {
  flex: 1;
  border: none;
  overflow-y: auto;
  background: transparent;
}

.layout-main {
  min-width: 0;
}

.layout-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 16px;
  background: var(--mgr-bg-card);
  border-bottom: 1px solid var(--mgr-border);
  height: 56px;
}

.header-left,
.header-right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.header-title {
  font-weight: 600;
}

.user-chip {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  padding: 4px 8px;
  border-radius: 6px;
}

.user-chip:hover {
  background: var(--mgr-border);
}

.layout-content {
  height: calc(100vh - 56px);
  overflow-y: auto;
}
</style>
