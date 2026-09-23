<script setup lang="ts">
/** 登录页：控制台 JWT 登录 + 接入层 API Key 直连（本地轻量化部署）。 */
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { useRealtimeStore } from '@/stores/realtime'
import { toastOk } from '@/composables/useToast'

const auth = useAuthStore()
const realtime = useRealtimeStore()
const router = useRouter()
const route = useRoute()

const tenantKey = ref('demo')
const username = ref('admin')
const password = ref('admin123')
const apiKey = ref('')
const mode = ref<'jwt' | 'apikey'>('jwt')

async function submit() {
  if (mode.value === 'apikey') {
    if (!apiKey.value.trim()) return
    auth.useApiKey(apiKey.value.trim())
    realtime.connect()
    toastOk('已使用接入层 API Key 建立会话')
    await router.push((route.query.redirect as string) || { name: 'dashboard' })
    return
  }
  const ok = await auth.login(tenantKey.value.trim(), username.value.trim(), password.value)
  if (ok) {
    realtime.connect()
    toastOk('登录成功')
    await router.push((route.query.redirect as string) || { name: 'dashboard' })
  }
}
</script>

<template>
  <div class="login-wrap">
    <div class="login-card card">
      <div class="card-body">
        <div class="row" style="gap: 10px; margin-bottom: 6px">
          <span class="brand-mark">CA</span>
          <div>
            <div class="strong" style="font-size: 16px">CodeAgent 控制台</div>
            <div class="small faint">多仓库分组代码修复 AI Agent</div>
          </div>
        </div>

        <div class="tabs" style="margin: 14px 0 16px">
          <button class="tab" :class="{ active: mode === 'jwt' }" @click="mode = 'jwt'">账号登录</button>
          <button class="tab" :class="{ active: mode === 'apikey' }" @click="mode = 'apikey'">API Key 直连</button>
        </div>

        <form @submit.prevent="submit">
          <template v-if="mode === 'jwt'">
            <div class="field" style="margin-bottom: 12px">
              <label class="field-label">租户标识<span class="req">*</span></label>
              <input v-model="tenantKey" class="input" placeholder="demo" autocomplete="username" />
            </div>
            <div class="field" style="margin-bottom: 12px">
              <label class="field-label">用户名<span class="req">*</span></label>
              <input v-model="username" class="input" placeholder="admin" autocomplete="username" />
            </div>
            <div class="field" style="margin-bottom: 16px">
              <label class="field-label">密码<span class="req">*</span></label>
              <input v-model="password" type="password" class="input" placeholder="admin123" autocomplete="current-password" />
            </div>
          </template>
          <template v-else>
            <div class="field" style="margin-bottom: 16px">
              <label class="field-label">接入层 API Key<span class="req">*</span></label>
              <input v-model="apiKey" class="input mono" placeholder="ca_live_xxxxxxxx" />
              <span class="field-hint">用于 IDE / 告警平台 / 流水线对接；启动日志中会打印演示密钥。</span>
            </div>
          </template>

          <div v-if="auth.error" class="alert alert-error" style="margin-bottom: 12px">{{ auth.error }}</div>

          <button class="btn btn-primary btn-block" type="submit" :disabled="auth.loading">
            {{ auth.loading ? '登录中…' : mode === 'jwt' ? '登录控制台' : '建立会话' }}
          </button>
        </form>

        <div class="alert alert-info" style="margin-top: 16px; font-size: 12px">
          默认演示账号：租户 <code>demo</code> / 用户 <code>admin</code> / 密码 <code>admin123</code>。
          多租户隔离验证可使用租户 <code>acme</code>（不含仓库数据）。
        </div>
      </div>
    </div>

    <div class="login-meta">
      <div class="strong">基于 DeepSeek Harness 架构的七层工程化 Agent</div>
      <div class="small muted" style="margin-top: 6px">
        接入层 · 租户权限层 · 仓库分组索引层 · Agent 调度核心层 · Skill 插件层 · MCP 模型管控层 · 源码沙箱执行层
      </div>
    </div>
  </div>
</template>

<style scoped>
.login-wrap {
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 22px;
  padding: 24px;
  background:
    radial-gradient(900px 420px at 15% -10%, rgba(79, 124, 255, 0.16), transparent 60%),
    radial-gradient(760px 380px at 100% 110%, rgba(123, 92, 255, 0.14), transparent 60%), var(--bg);
}

.login-card {
  width: 100%;
  max-width: 420px;
}

.login-meta {
  text-align: center;
  max-width: 560px;
}

.brand-mark {
  width: 36px;
  height: 36px;
  border-radius: 9px;
  background: linear-gradient(135deg, var(--brand-500), #7b5cff);
  color: #fff;
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 13px;
  flex: none;
}
</style>
