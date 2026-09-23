<script setup lang="ts">
/**
 * AI 设置（大模型提供方管控）。
 *
 * 控制台持久化配置模型提供方（OpenAI 兼容协议），保存后立即热更新到 MCP 模型层；
 * 密钥经加密后落盘，列表接口只暴露 hasKey 与掩码 hint，不回显明文。
 * 权限要求：model:admin（admin:all 通配可过）。
 *
 * 契约对齐后端：以 name 为唯一标识，请求体为 items 数组，测试响应含 durationMs。
 */
import { onMounted, ref } from 'vue'
import { ApiError, api } from '@/api/client'
import type { ModelProviderSetting, ModelTestResult } from '@/types'
import { toastError, toastOk } from '@/composables/useToast'

/* ------------------------------ 状态 ------------------------------ */

const KIND_OPTIONS = ['openai', 'deepseek', 'azure', 'ollama', 'openai_compatible']
const TIER_OPTIONS = ['light', 'strong', 'fallback']

/** 控制台持久化的模型提供方配置。 */
const configProviders = ref<ModelProviderSetting[]>([])
const configError = ref('')
const configLoading = ref(false)

type EditingProvider = ModelProviderSetting & { apiKey?: string }
const editing = ref<EditingProvider | null>(null)
const isNew = ref(false)
const apiKeyInput = ref('')
const modelsText = ref('')
const saving = ref(false)
const testingName = ref('')
const testResult = ref<ModelTestResult | null>(null)
const saveMsg = ref<{ tone: string; text: string } | null>(null)

function errMsg(e: unknown, fallback: string): string {
  if (e instanceof ApiError) {
    if (e.code === 403) return '权限不足：需要 admin:all 或 model:admin 权限'
    return `HTTP/业务码 ${e.code}：${e.message}`
  }
  return e instanceof Error ? e.message : fallback
}

function openEdit(p: ModelProviderSetting | null): void {
  isNew.value = p === null
  editing.value = p
    ? { ...p, models: [...p.models], apiKey: '' }
    : {
        name: '',
        kind: 'openai_compatible',
        baseUrl: '',
        models: [],
        tier: 'strong',
        enabled: true,
        hasKey: false,
        apiKey: '',
      }
  apiKeyInput.value = ''
  modelsText.value = (p?.models ?? []).join(', ')
  testResult.value = null
  saveMsg.value = null
}

function cancelEdit(): void {
  editing.value = null
}

/** 从编辑态对象取出可持久化字段（剔除回显字段与临时 apiKey 之外的冗余）。 */
function stripMeta(p: EditingProvider): Record<string, unknown> {
  const { hasKey, keyHint, source, updatedAt, apiKey, ...rest } = p
  const clean: Record<string, unknown> = { ...rest }
  if (apiKey) clean.apiKey = apiKey
  return clean
}

async function persist(list: EditingProvider[]): Promise<void> {
  saving.value = true
  saveMsg.value = null
  try {
    const res = await api.updateModelConfig({
      items: list.map((p) => stripMeta(p) as never),
    })
    configProviders.value = res.items
    toastOk('AI 设置已保存，模型层已热更新')
    editing.value = null
  } catch (e) {
    saveMsg.value = { tone: 'alert-warn', text: errMsg(e, '保存失败') }
  } finally {
    saving.value = false
  }
}

async function saveProvider(): Promise<void> {
  const e = editing.value
  if (!e) return
  if (!e.name.trim()) {
    saveMsg.value = { tone: 'alert-warn', text: '名称（标识）不能为空' }
    return
  }
  if (!e.baseUrl.trim()) {
    saveMsg.value = { tone: 'alert-warn', text: 'Base URL 不能为空' }
    return
  }
  e.models = modelsText.value
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  e.apiKey = apiKeyInput.value
  const rest = configProviders.value.filter((p) => p.name !== e.name)
  await persist([...rest.map((p) => ({ ...p })), e] as EditingProvider[])
}

async function removeProvider(p: ModelProviderSetting): Promise<void> {
  if (!confirm(`确认删除模型提供方「${p.name}」？`)) return
  const rest = configProviders.value.filter((x) => x.name !== p.name)
  configLoading.value = true
  try {
    const res = await api.updateModelConfig({ items: rest.map((x) => stripMeta(x)) as never })
    configProviders.value = res.items
    toastOk('已删除模型提供方')
  } catch (e) {
    configError.value = errMsg(e, '删除失败')
  } finally {
    configLoading.value = false
  }
}

/** 测试连通性：接受已保存配置或编辑态对象（含临时 apiKey）。 */
async function testConn(p: ModelProviderSetting | EditingProvider): Promise<void> {
  testingName.value = p.name
  testResult.value = null
  try {
    const res = await api.testModelProvider({
      name: p.name,
      kind: p.kind,
      baseUrl: p.baseUrl,
      models: p.models,
      apiKey: (p as EditingProvider).apiKey || apiKeyInput.value || undefined,
      testModel: p.models?.[0],
    })
    testResult.value = res
    if (res.ok) toastOk(`连通成功（${res.durationMs ?? '?'}ms）`)
    else toastError(`连通失败：${res.message}`)
  } catch (e) {
    testResult.value = { ok: false, message: errMsg(e, '连通测试失败') }
    toastError(errMsg(e, '连通测试失败'))
  } finally {
    testingName.value = ''
  }
}

async function loadConfig(): Promise<void> {
  configLoading.value = true
  try {
    const res = await api.getModelConfig()
    configProviders.value = res.items ?? []
    configError.value = ''
  } catch (e) {
    configError.value = errMsg(e, 'AI 设置加载失败')
  } finally {
    configLoading.value = false
  }
}

onMounted(() => {
  void loadConfig()
})
</script>

<style scoped>
.provider-row {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 10px 12px;
  margin-bottom: 10px;
  background: var(--bg-elev-2);
}
.modal-overlay {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.5);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 50;
  padding: 16px;
}
.modal {
  width: min(540px, 100%);
  max-height: 90vh;
  overflow: auto;
  background: var(--bg-elev);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  box-shadow: 0 16px 48px rgba(0, 0, 0, 0.4);
}
.modal-head,
.modal-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 16px;
}
.modal-head {
  border-bottom: 1px solid var(--border);
}
.modal-foot {
  border-top: 1px solid var(--border);
  gap: 8px;
  justify-content: flex-end;
}
.modal-body {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
}
.field > span {
  color: var(--text-muted);
}
.field input,
.field select {
  width: 100%;
  padding: 8px 10px;
  background: var(--bg-input, var(--bg-elev-2));
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: 13px;
}
.field input:focus,
.field select:focus {
  outline: none;
  border-color: var(--brand-500);
}
.btn-primary {
  background: var(--brand-500);
  color: #fff;
  border-color: var(--brand-500);
}
.btn-sm {
  padding: 4px 10px;
  font-size: 12px;
}
.btn-danger {
  color: var(--error);
}
</style>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">AI 设置</h1>
        <p class="page-subtitle">
          配置大模型提供方（OpenAI 兼容协议）。保存后立即热更新到模型层；密钥加密落盘，不回显明文。
        </p>
      </div>
      <div class="row wrap">
        <button class="btn btn-primary" :disabled="configLoading" @click="openEdit(null)">
          + 新增提供方
        </button>
      </div>
    </div>

    <div class="card">
      <div class="card-head">
        <div style="min-width: 0">
          <h3 class="card-title">模型提供方</h3>
          <div class="small muted" style="margin-top: 2px">
            未配置任何提供方时系统无法完成推理（已移除内置 Mock 回退）。
          </div>
        </div>
      </div>
      <div class="card-body">
        <div v-if="configError" class="alert alert-warn">{{ configError }}</div>
        <div
          v-if="!configProviders.length && !configError"
          class="muted small"
          style="margin-bottom: 12px"
        >
          尚未配置任何模型提供方。
        </div>
        <div v-for="p in configProviders" :key="p.name" class="provider-row">
          <div class="row-between">
            <div style="min-width: 0">
              <span class="mono">{{ p.name }}</span>
              <span class="badge badge-muted" style="margin-left: 6px">{{ p.kind }}</span>
              <span v-if="p.enabled" class="badge badge-ok" style="margin-left: 6px">启用</span>
              <span v-else class="badge badge-warn" style="margin-left: 6px">停用</span>
              <span v-if="p.hasKey" class="badge badge-info" style="margin-left: 6px">密钥已设</span>
            </div>
            <div class="row nowrap" style="gap: 6px">
              <button class="btn btn-sm" :disabled="testingName === p.name" @click="testConn(p)">
                {{ testingName === p.name ? '测试中…' : '测试连通' }}
              </button>
              <button class="btn btn-sm" @click="openEdit(p)">编辑</button>
              <button class="btn btn-sm btn-danger" @click="removeProvider(p)">删除</button>
            </div>
          </div>
          <div class="small muted ellipsis" :title="p.baseUrl" style="margin-top: 6px">
            {{ p.baseUrl }}
          </div>
          <div v-if="p.models.length" class="row wrap" style="margin-top: 4px; gap: 6px">
            <span v-for="m in p.models" :key="m" class="badge badge-muted">{{ m }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- 编辑 / 新增弹窗 -->
    <div v-if="editing" class="modal-overlay" @click.self="cancelEdit">
      <div class="modal">
        <div class="modal-head">
          <h3 class="card-title">{{ isNew ? '新增模型提供方' : '编辑模型提供方' }}</h3>
          <button class="btn btn-sm" @click="cancelEdit">关闭</button>
        </div>
        <div class="modal-body">
          <div v-if="saveMsg" class="alert" :class="saveMsg.tone">{{ saveMsg.text }}</div>
          <div
            v-if="testResult"
            class="alert"
            :class="testResult.ok ? 'alert-info' : 'alert-warn'"
          >
            连通{{ testResult.ok ? '成功' : '失败' }}：{{ testResult.message }}
            <span v-if="testResult.durationMs !== undefined">（{{ testResult.durationMs }}ms）</span>
          </div>
          <label class="field">
            <span>名称（标识{{ isNew ? '' : '，不可修改' }}）</span>
            <input v-model="editing.name" :disabled="!isNew" placeholder="openai / deepseek / 自定义" />
          </label>
          <label class="field">
            <span>类型</span>
            <select v-model="editing.kind">
              <option v-for="k in KIND_OPTIONS" :key="k" :value="k">{{ k }}</option>
            </select>
          </label>
          <label class="field">
            <span>层级 tier</span>
            <select v-model="editing.tier">
              <option v-for="t in TIER_OPTIONS" :key="t" :value="t">{{ t }}</option>
            </select>
          </label>
          <label class="field">
            <span>Base URL</span>
            <input v-model="editing.baseUrl" placeholder="https://api.deepseek.com/v1" />
          </label>
          <label class="field">
            <span>模型名（逗号分隔）</span>
            <input v-model="modelsText" placeholder="deepseek-chat, deepseek-reasoner" />
          </label>
          <label class="field">
            <span>API Key{{ editing.hasKey ? '（已设置，留空则保持不变）' : '' }}</span>
            <input
              v-model="apiKeyInput"
              type="password"
              autocomplete="new-password"
              placeholder="sk-..."
            />
          </label>
          <label class="checkbox">
            <input v-model="editing.enabled" type="checkbox" />
            启用该提供方
          </label>
        </div>
        <div class="modal-foot">
          <button
            class="btn"
            :disabled="testingName === editing?.name"
            @click="editing && testConn(editing)"
          >
            {{ testingName === editing?.name ? '测试中…' : '测试连通' }}
          </button>
          <button class="btn btn-primary" :disabled="saving" @click="saveProvider">
            {{ saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
