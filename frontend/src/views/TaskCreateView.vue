<script setup lang="ts">
/**
 * 发起排查（TaskCreateView）
 *
 * 组件职责：
 * 1. 提供两种排查场景的表单：单仓库精准修复（single_repo，仓库 + ref/commit）
 *    与多仓库分组联合排查（group，按 order 展示成员仓库链路与入口仓库）。
 * 2. 仓库选择后展示 matchRules 匹配依据（包前缀/路径前缀/主机模式…），体现「堆栈驱动懒加载」的解析依据；
 *    分组选择后调用 api.getGroup 拉取成员链路。
 * 3. 提交异常堆栈（必填，> 20 字符）、附加日志、嫌疑文件、环境/来源/优先级/沙箱验证/回调地址/幂等键，
 *    提交前做字段级校验，提交后跳转任务详情；按 ApiError.code 给出针对性提示。
 * 4. 右侧说明本次提交触发的完整链路与能力边界。
 *
 * 降级原则：仓库/分组等基础数据接口失败不阻塞表单，字段内以 .alert / .field-error 提示。
 */
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type {
  CreateTaskRequest,
  GroupMember,
  Repository,
  RepositoryGroup,
  TaskMode,
  TriggerSource,
} from '@/types'
import { useAsync } from '@/composables/useAsync'
import { toastError, toastOk, toastWarn } from '@/composables/useToast'
import { layerLabel, modeLabel, shortCommit } from '@/utils/format'

const router = useRouter()

/* ------------------------------ 内置示例堆栈 ------------------------------ */

const SAMPLE_JAVA = `java.lang.NullPointerException: Cannot invoke "com.acme.order.OrderService.calcTotal()" because "orderService" is null
	at com.acme.checkout.CheckoutController.submit(CheckoutController.java:87)
	at java.base/jdk.internal.reflect.DirectMethodHandleAccessor.invoke(DirectMethodHandleAccessor.java:103)
	at org.springframework.web.servlet.FrameworkServlet.service(FrameworkServlet.java:897)
	at javax.servlet.http.HttpServlet.service(HttpServlet.java:764)
Caused by: java.lang.IllegalStateException: order context not initialized
	at com.acme.order.OrderContext.require(OrderContext.java:42)
	at com.acme.checkout.CheckoutController.submit(CheckoutController.java:81)
	... 23 more`

const SAMPLE_GO = `panic: runtime error: invalid memory address or nil pointer dereference
	[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x8f2a11]

goroutine 42 [running]:
github.com/acme/gateway/internal/proxy.(*Router).forward(0xc0001a2000, 0xc0002b4000)
	/src/internal/proxy/router.go:128 +0x1f1
github.com/acme/gateway/internal/handler.HTTPHandler.ServeHTTP(0xc0000d8000, {0x1b2c4a0, 0xc0002b4000}, 0xc00033a000)
	/src/internal/handler/http.go:64 +0x9c
net/http.serverHandler.ServeHTTP(0xc0001c4000, {0x1b2c4a0, 0xc0002b4000}, 0xc00033a000)
	/usr/local/go/src/net/http/server.go:2938 +0x8e
created by net/http.(*Server).Serve in goroutine 1
	/usr/local/go/src/net/http/server.go:3090 +0x1cd`

const SAMPLE_NODE = `TypeError: Cannot read properties of undefined (reading 'total')
    at OrderService.calcTotal (/srv/app/src/services/order.service.ts:57:21)
    at CheckoutController.submit (/srv/app/src/controllers/checkout.controller.ts:112:30)
    at Layer.handle [as handle_request] (/srv/app/node_modules/express/lib/router/layer.js:95:5)
    at next (/srv/app/node_modules/express/lib/router/route.js:143:13)
    at processTicksAndRejections (node:internal/process/task_queues:95:5)
Caused by: Error: order context is undefined
    at OrderContext.require (/srv/app/src/domain/order-context.ts:31:15)
    at OrderService.calcTotal (/srv/app/src/services/order.service.ts:52:9)`

/* ------------------------------ 表单状态 ------------------------------ */

const mode = ref<TaskMode>('single_repo')
const repoId = ref('')
const groupId = ref('')
const refValue = ref('')
const stacktrace = ref('')
const logs = ref('')
const entryFilesInput = ref('')
const environment = ref('prod')
const source = ref<TriggerSource>('manual')
const priority = ref(5)
const autoVerify = ref(true)
const callbackUrl = ref('')
const idempotencyKey = ref('')
const advancedOpen = ref(false)
const submitting = ref(false)
const errors = ref<Record<string, string>>({})
const submitError = ref('')

const ENV_OPTIONS = ['prod', 'staging', 'dev']
const SOURCE_OPTIONS: { value: TriggerSource; label: string }[] = [
  { value: 'manual', label: '手动提交' },
  { value: 'alert', label: '告警平台' },
  { value: 'ide', label: 'IDE 插件' },
  { value: 'cicd', label: 'CI/CD 流水线' },
  { value: 'openapi', label: 'OpenAPI 对接' },
]

/* ------------------------------ 基础数据（可降级） ------------------------------ */

const { data: repoPage, loading: repoLoading, error: repoError, reload: reloadRepos } = useAsync(
  () => api.listRepos({ pageSize: 200 }),
  { silent: true },
)
const { data: groupPage, loading: groupLoading, error: groupError, reload: reloadGroups } = useAsync(
  () => api.listGroups({ pageSize: 100 }),
  { silent: true },
)

interface GroupDetail {
  group: RepositoryGroup
  members: GroupMember[]
}

const {
  data: groupData,
  loading: groupDetailLoading,
  error: groupDetailError,
  reload: reloadGroupDetail,
} = useAsync<GroupDetail | null>(() => (groupId.value ? api.getGroup(groupId.value) : Promise.resolve(null)), {
  immediate: false,
  silent: true,
})

const repos = computed<Repository[]>(() => repoPage.value?.items ?? [])
const groups = computed<RepositoryGroup[]>(() => groupPage.value?.items ?? [])
const repoIndex = computed(() => new Map(repos.value.map((r) => [r.id, r])))

const selectedRepo = computed<Repository | null>(() => repos.value.find((r) => r.id === repoId.value) ?? null)
const selectedGroup = computed<RepositoryGroup | null>(
  () => groups.value.find((g) => g.id === groupId.value) ?? groupData.value?.group ?? null,
)

/** 按 order 排序的分组链路。 */
const members = computed<GroupMember[]>(() =>
  [...(groupData.value?.members ?? [])].sort((a, b) => a.order - b.order),
)
const entryIds = computed(() => new Set(groupData.value?.group.entryRepositoryIds ?? []))

function memberRepo(m: GroupMember): Repository | undefined {
  return m.repo ?? repoIndex.value.get(m.repositoryId)
}

/** matchRules 摘要：拆成徽标展示「堆栈驱动懒加载」的匹配依据。 */
const matchChips = computed<{ key: string; label: string; value: string }[]>(() => {
  const repo = selectedRepo.value
  if (!repo) return []
  const rules = repo.matchRules ?? {}
  const pairs: [string, string[] | undefined][] = [
    ['包前缀', rules.packagePrefixes],
    ['路径前缀', rules.pathPrefixes],
    ['构件名', rules.artifactNames],
    ['主机模式', rules.hostPatterns],
    ['端点模式', rules.endpointPatterns],
    ['关键字', rules.keywords],
  ]
  const chips: { key: string; label: string; value: string }[] = []
  for (const [label, values] of pairs) {
    for (const v of values ?? []) chips.push({ key: `${label}:${v}`, label, value: v })
  }
  return chips
})

/* ------------------------------ 校验 ------------------------------ */

const COMMIT_HEX = /^[0-9a-f]{40}$/i
const isCommitRef = computed(() => COMMIT_HEX.test(refValue.value.trim()))
const stackLength = computed(() => stacktrace.value.trim().length)
const entryFiles = computed(() =>
  entryFilesInput.value
    .split(/[,，\n]/)
    .map((s) => s.trim())
    .filter((s) => s.length > 0),
)

function clearError(key: string): void {
  if (!errors.value[key]) return
  const next = { ...errors.value }
  delete next[key]
  errors.value = next
}

function fillSample(kind: 'java' | 'go' | 'node'): void {
  stacktrace.value = kind === 'java' ? SAMPLE_JAVA : kind === 'go' ? SAMPLE_GO : SAMPLE_NODE
  clearError('stacktrace')
}

function validate(): boolean {
  const next: Record<string, string> = {}
  const stack = stacktrace.value.trim()
  if (!stack) next.stacktrace = '异常堆栈为必填项'
  else if (stack.length <= 20) next.stacktrace = `堆栈长度 ${stack.length} 字符，需大于 20 字符，请粘贴完整报错堆栈`
  if (mode.value === 'single_repo' && !repoId.value) next.repoId = '请选择需要排查的仓库'
  if (mode.value === 'group' && !groupId.value) next.groupId = '请选择需要联合排查的业务分组'
  const cb = callbackUrl.value.trim()
  if (cb && !/^https?:\/\/[^\s]+$/i.test(cb)) next.callbackUrl = '回调地址需为 http(s):// 开头的合法 URL'
  const p = Number(priority.value)
  if (!Number.isInteger(p) || p < 0 || p > 9) next.priority = '优先级需为 0-9 的整数'
  errors.value = next
  return Object.keys(next).length === 0
}

/* ------------------------------ 提交 ------------------------------ */

async function submit(): Promise<void> {
  submitError.value = ''
  if (!validate()) {
    toastWarn('表单校验未通过', '请检查标红的字段后重新提交')
    return
  }
  submitting.value = true
  try {
    const payload: CreateTaskRequest = {
      mode: mode.value,
      stacktrace: stacktrace.value,
      source: source.value,
      environment: environment.value,
      priority: Number(priority.value),
      autoVerify: autoVerify.value,
    }
    if (mode.value === 'single_repo') payload.repoId = repoId.value
    else payload.groupId = groupId.value

    const ref = refValue.value.trim()
    if (ref) payload.ref = ref
    const lg = logs.value.trim()
    if (lg) payload.logs = lg
    if (entryFiles.value.length) payload.entryFiles = entryFiles.value
    const cb = callbackUrl.value.trim()
    if (cb) payload.callbackUrl = cb
    const ik = idempotencyKey.value.trim()
    if (ik) payload.idempotencyKey = ik

    const run = await api.createTask(payload)
    toastOk('任务已受理', run?.id ?? '')
    if (run?.id) {
      await router.push({ name: 'task-detail', params: { id: run.id } })
    } else {
      await router.push({ name: 'tasks' })
    }
  } catch (e) {
    if (e instanceof ApiError) {
      switch (e.code) {
        case 401:
          submitError.value = '认证失效，请重新登录'
          break
        case 403:
          submitError.value = '无权访问该仓库/分组'
          break
        case 429:
          submitError.value = '配额或限流：请稍后重试'
          break
        case 400:
        case 422:
          submitError.value = e.message || '请求参数不合法'
          break
        case 404:
          submitError.value = '任务接口未开放（404），请确认后端版本'
          break
        default:
          submitError.value = `${e.message}（code=${e.code}${e.requestId ? `, requestId=${e.requestId}` : ''}）`
      }
      toastError('提交失败', submitError.value)
    } else {
      submitError.value = e instanceof Error ? e.message : '提交失败，请稍后重试'
      toastError('提交失败', submitError.value)
    }
  } finally {
    submitting.value = false
  }
}

function resetForm(): void {
  repoId.value = ''
  groupId.value = ''
  refValue.value = ''
  stacktrace.value = ''
  logs.value = ''
  entryFilesInput.value = ''
  environment.value = 'prod'
  source.value = 'manual'
  priority.value = 5
  autoVerify.value = true
  callbackUrl.value = ''
  idempotencyKey.value = ''
  errors.value = {}
  submitError.value = ''
  groupData.value = null
}

function switchMode(next: TaskMode): void {
  mode.value = next
  clearError('repoId')
  clearError('groupId')
}

/* ------------------------------ 链路说明 ------------------------------ */

const PIPELINE: { title: string; detail: string }[] = [
  { title: '权限校验', detail: '校验租户配额、访问策略与 API Key 权限范围' },
  { title: '锁定仓库与 Commit', detail: '单仓库按 ref 精准锁定；分组按成员 order 逐个解析' },
  { title: '堆栈解析定位文件', detail: '解析 Java / Go / Node 堆栈帧，按 matchRules 匹配仓库与文件' },
  { title: '拉取精准版本源码', detail: '按锁定 Commit 拉取最小必要上下文，避免版本漂移' },
  { title: 'Skill 分析根因', detail: '按 parse → diagnose → repair → verify 顺序调用 Skill 插件' },
  { title: 'MCP 生成结构化修复方案', detail: '模型输出经 Schema 校验，形成带风险等级与理由的补丁方案' },
  { title: '沙箱验证', detail: '在源码沙箱内应用增量 Patch 并执行构建 / 测试校验' },
  { title: '输出报告与增量 Patch', detail: '生成根因报告、调用链与 unified diff 增量补丁' },
  { title: '任务归档', detail: '写入运行记录与审计事件，必要时回调 callbackUrl' },
]

const BOUNDARIES: string[] = [
  '仅覆盖代码 / 逻辑层 BUG；环境、配置、网络与数据问题不在自动修复范围内',
  '仅输出增量 Patch（unified diff），不会直接改写远端分支或自动合并',
  '分组分析按需懒加载成员仓库源码，不会一次性拉取整个分组',
  '修复结果必须人工复核后合并，needs_review 表示模型置信度不足',
]

/* ------------------------------ 监听 ------------------------------ */

watch(groupId, (id) => {
  if (!id) {
    groupData.value = null
    return
  }
  void reloadGroupDetail()
})

watch(mode, () => {
  submitError.value = ''
})
</script>

<template>
  <div class="page">
    <!-- 页头 -->
    <div class="page-header">
      <div>
        <h1 class="page-title">发起排查</h1>
        <p class="page-subtitle">
          提交线上报错堆栈，由 Agent 锁定代码版本、定位文件、分析根因并产出可复核的增量修复补丁。
        </p>
      </div>
      <div class="row wrap">
        <button class="btn btn-sm" :disabled="submitting" @click="resetForm">重置表单</button>
        <button class="btn btn-sm btn-ghost" @click="router.push({ name: 'tasks' })">返回任务中心</button>
      </div>
    </div>

    <div class="create-grid">
      <!-- 左：表单 -->
      <div style="display: flex; flex-direction: column; gap: 14px">
        <!-- 场景切换 -->
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">排查场景</h2>
            <span class="small muted">决定锁定代码版本的方式</span>
          </div>
          <div class="card-body">
            <div class="tabs">
              <button class="tab" :class="{ active: mode === 'single_repo' }" @click="switchMode('single_repo')">
                ① {{ modeLabel('single_repo') }}
              </button>
              <button class="tab" :class="{ active: mode === 'group' }" @click="switchMode('group')">
                ② {{ modeLabel('group') }}
              </button>
            </div>
            <p class="small muted" style="margin: 10px 0 0">
              {{
                mode === 'single_repo'
                  ? '已知报错归属单一仓库时使用：锁定一个仓库 + Commit，产出该仓库的精准增量补丁。'
                  : '报错跨越多个服务或需要沿调用链排查时使用：按分组内 order 链路依次加载成员仓库源码。'
              }}
            </p>
          </div>
        </div>

        <!-- 代码源 -->
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">代码源定位</h2>
            <span class="small muted">必填</span>
          </div>
          <div class="card-body" style="display: flex; flex-direction: column; gap: 14px">
            <!-- 单仓库 -->
            <template v-if="mode === 'single_repo'">
              <div class="field">
                <label class="field-label">目标仓库<span class="req">*</span></label>
                <div v-if="repoLoading" class="skeleton" style="height: 34px" />
                <select v-else v-model="repoId" class="select" @change="clearError('repoId')">
                  <option value="">请选择仓库…</option>
                  <option v-for="r in repos" :key="r.id" :value="r.id">
                    {{ r.name }} [{{ layerLabel(r.layer) }}] {{ r.language }} {{ r.defaultBranch }}
                  </option>
                </select>
                <div v-if="repoError" class="alert alert-warn" style="margin-top: 6px">
                  <span>仓库列表加载失败：{{ repoError }}</span>
                  <div class="spacer" />
                  <button class="btn btn-sm" @click="reloadRepos">重试</button>
                </div>
                <div v-else-if="!repoLoading && !repos.length" class="alert alert-warn" style="margin-top: 6px">
                  当前租户下没有可用仓库，请先在「代码源仓库」中接入 Git 仓库。
                </div>
                <span v-if="errors.repoId" class="field-error">{{ errors.repoId }}</span>
                <span v-else class="field-hint">仓库列表来自 api.listRepos，仅展示当前租户可见的仓库。</span>
              </div>

              <!-- matchRules 匹配依据 -->
              <div v-if="selectedRepo" class="match-box">
                <div class="row-between" style="margin-bottom: 8px">
                  <span class="small strong">匹配依据（matchRules）</span>
                  <span class="small faint">
                    {{ selectedRepo.provider }} · {{ selectedRepo.status }} ·
                    <span class="mono">{{ selectedRepo.key }}</span>
                  </span>
                </div>
                <div v-if="matchChips.length" class="row wrap">
                  <span v-for="chip in matchChips" :key="chip.key" class="badge badge-info">
                    <span class="faint">{{ chip.label }}</span>
                    <span class="mono">{{ chip.value }}</span>
                  </span>
                </div>
                <div v-else class="small muted">
                  该仓库未配置 matchRules：Agent 将退化为按堆栈中的包名 / 路径在全仓库范围内检索，定位精度会下降。
                </div>
                <div class="small faint" style="margin-top: 8px">
                  堆栈帧解析后按上述规则打分，命中即触发该仓库源码的懒加载，避免全量拉取。
                </div>
              </div>
            </template>

            <!-- 分组 -->
            <template v-else>
              <div class="field">
                <label class="field-label">业务分组<span class="req">*</span></label>
                <div v-if="groupLoading" class="skeleton" style="height: 34px" />
                <select v-else v-model="groupId" class="select">
                  <option value="">请选择业务分组…</option>
                  <option v-for="g in groups" :key="g.id" :value="g.id">
                    {{ g.name }} · {{ g.status }} · {{ g.key }}
                  </option>
                </select>
                <div v-if="groupError" class="alert alert-warn" style="margin-top: 6px">
                  <span>分组列表加载失败：{{ groupError }}</span>
                  <div class="spacer" />
                  <button class="btn btn-sm" @click="reloadGroups">重试</button>
                </div>
                <div v-else-if="!groupLoading && !groups.length" class="alert alert-warn" style="margin-top: 6px">
                  当前租户下没有业务分组，请先在「业务分组」中创建分组并编排成员仓库。
                </div>
                <span v-if="errors.groupId" class="field-error">{{ errors.groupId }}</span>
              </div>

              <!-- 分组成员链路 -->
              <div v-if="groupId" class="match-box">
                <div class="row-between" style="margin-bottom: 8px">
                  <span class="small strong">成员仓库链路（按 order）</span>
                  <span class="small faint">{{ selectedGroup?.name ?? groupId }}</span>
                </div>
                <div v-if="groupDetailLoading" style="display: flex; flex-direction: column; gap: 8px">
                  <div v-for="i in 3" :key="i" class="skeleton" style="height: 24px" />
                </div>
                <div v-else-if="groupDetailError" class="alert alert-error">
                  <span>分组详情加载失败：{{ groupDetailError }}</span>
                  <div class="spacer" />
                  <button class="btn btn-sm" @click="reloadGroupDetail">重试</button>
                </div>
                <template v-else-if="members.length">
                  <div class="row wrap small" style="margin-bottom: 10px">
                    <template v-for="(m, i) in members" :key="`chain-${m.repositoryId}`">
                      <span class="badge" :class="entryIds.has(m.repositoryId) ? 'badge-ok' : 'badge-muted'">
                        {{ memberRepo(m)?.name ?? m.repositoryId }}
                      </span>
                      <span v-if="i < members.length - 1" class="faint">→</span>
                    </template>
                  </div>
                  <div class="timeline">
                    <div v-for="m in members" :key="m.repositoryId" class="timeline-item">
                      <span class="timeline-dot" :class="{ ok: entryIds.has(m.repositoryId) }" />
                      <div class="timeline-body">
                        <div class="row wrap">
                          <span class="badge badge-muted">#{{ m.order }}</span>
                          <span class="strong">{{ memberRepo(m)?.name ?? m.repositoryId }}</span>
                          <span v-if="memberRepo(m)" class="badge badge-info">{{ layerLabel(memberRepo(m)?.layer) }}</span>
                          <span v-if="entryIds.has(m.repositoryId)" class="badge badge-ok">入口仓库</span>
                        </div>
                        <div class="small muted">
                          {{ memberRepo(m)?.language ?? '语言未知' }} ·
                          {{ memberRepo(m)?.defaultBranch ?? '默认分支未知' }} ·
                          <span class="mono">{{ memberRepo(m)?.key ?? m.repositoryId }}</span>
                        </div>
                        <div v-if="m.note" class="small faint">{{ m.note }}</div>
                      </div>
                    </div>
                  </div>
                  <div class="small faint" style="margin-top: 8px">
                    第 {{ members.length }} 个成员仓库按 order 依次参与分析，入口仓库优先加载。
                  </div>
                </template>
                <div v-else class="small muted">该分组暂无成员仓库，提交后可能无法完成跨仓库分析。</div>
              </div>
            </template>

            <!-- 版本 ref -->
            <div v-if="mode === 'single_repo'" class="field">
              <label class="field-label">版本 ref / Commit（可选）</label>
              <input
                v-model="refValue"
                class="input mono"
                placeholder="例如 3f8a1c9d2b7e4f5a6c8d9e0f1a2b3c4d5e6f7a8b 或 release/2024.11"
              />
              <span v-if="isCommitRef" class="field-hint" style="color: var(--ok)">
                ✓ 已识别为 Commit（{{ shortCommit(refValue.trim()) }}），将精准锁定该提交，不做分支漂移解析。
              </span>
              <span v-else class="field-hint">
                强烈建议填写线上报错对应的 Commit，保证版本精准锁定；留空使用默认分支最新提交
                （{{ selectedRepo?.defaultBranch ?? '默认分支' }}）。
              </span>
            </div>
          </div>
        </div>

        <!-- 报错信息 -->
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">报错信息</h2>
            <span class="small muted">异常堆栈为必填</span>
          </div>
          <div class="card-body" style="display: flex; flex-direction: column; gap: 14px">
            <div class="field">
              <div class="row-between">
                <label class="field-label">异常堆栈<span class="req">*</span></label>
                <span class="small" :class="stackLength > 20 ? 'muted' : 'faint'">
                  {{ stackLength }} 字符（需 &gt; 20）
                </span>
              </div>
              <textarea
                v-model="stacktrace"
                class="textarea"
                style="min-height: 220px"
                placeholder="java.lang.NullPointerException: Cannot invoke &quot;com.acme.order.OrderService.calcTotal()&quot; because &quot;orderService&quot; is null&#10;	at com.acme.checkout.CheckoutController.submit(CheckoutController.java:87)&#10;Caused by: java.lang.IllegalStateException: order context not initialized&#10;	at com.acme.order.OrderContext.require(OrderContext.java:42)&#10;	... 23 more"
                @input="clearError('stacktrace')"
              />
              <div class="row wrap">
                <span class="small faint">快捷填充：</span>
                <button class="btn btn-sm" @click="fillSample('java')">填充 Java 示例</button>
                <button class="btn btn-sm" @click="fillSample('go')">填充 Go 示例</button>
                <button class="btn btn-sm" @click="fillSample('node')">填充 Node 示例</button>
              </div>
              <span v-if="errors.stacktrace" class="field-error">{{ errors.stacktrace }}</span>
              <span v-else class="field-hint">
                请保留 <code>Caused by</code> / goroutine / <code>at</code> 等原始帧信息，Agent 依赖文件行号定位源码。
              </span>
            </div>

            <div class="field">
              <label class="field-label">附加日志（可选）</label>
              <textarea
                v-model="logs"
                class="textarea"
                style="min-height: 110px"
                placeholder="可粘贴同一时间窗口的应用日志、网关访问日志或监控指标片段，用于交叉验证根因。"
              />
            </div>

            <div class="field">
              <label class="field-label">嫌疑文件（可选，逗号分隔）</label>
              <input
                v-model="entryFilesInput"
                class="input mono"
                placeholder="src/main/java/com/acme/checkout/CheckoutController.java, internal/proxy/router.go"
              />
              <span class="field-hint">
                已识别 {{ entryFiles.length }} 个文件路径；填写后将作为优先检索入口，可显著缩短定位时间。
              </span>
            </div>
          </div>
        </div>

        <!-- 执行参数 -->
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">执行参数</h2>
            <button class="btn btn-sm btn-ghost" @click="advancedOpen = !advancedOpen">
              {{ advancedOpen ? '▾ 收起高级选项' : '▸ 高级选项' }}
            </button>
          </div>
          <div class="card-body" style="display: flex; flex-direction: column; gap: 14px">
            <div class="grid grid-2">
              <div class="field">
                <label class="field-label">环境</label>
                <select v-model="environment" class="select">
                  <option v-for="env in ENV_OPTIONS" :key="env" :value="env">{{ env }}</option>
                </select>
              </div>
              <div class="field">
                <label class="field-label">触发来源</label>
                <select v-model="source" class="select">
                  <option v-for="opt in SOURCE_OPTIONS" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
                </select>
              </div>
              <div class="field">
                <label class="field-label">优先级（0-9）</label>
                <input v-model.number="priority" type="number" min="0" max="9" class="input" />
                <span v-if="errors.priority" class="field-error">{{ errors.priority }}</span>
                <span v-else class="field-hint">数值越大越先调度，默认 5。</span>
              </div>
              <div class="field">
                <label class="field-label">回调地址（可选）</label>
                <input v-model="callbackUrl" class="input mono" placeholder="https://alert.acme.com/hooks/codeagent" />
                <span v-if="errors.callbackUrl" class="field-error">{{ errors.callbackUrl }}</span>
                <span v-else class="field-hint">任务终态时以 POST 回调该地址，便于回写告警平台。</span>
              </div>
            </div>

            <label class="checkbox">
              <input v-model="autoVerify" type="checkbox" />
              沙箱验证（在隔离工作区应用增量 Patch 并执行构建 / 测试校验）
            </label>

            <div v-show="advancedOpen" class="field">
              <label class="field-label">幂等键（可选）</label>
              <input v-model="idempotencyKey" class="input mono" placeholder="alert-INC-20241120-001" />
              <span class="field-hint">同租户同幂等键的未完成任务会被复用，避免重复执行。</span>
            </div>
          </div>
        </div>

        <!-- 提交 -->
        <div class="card">
          <div class="card-body" style="display: flex; flex-direction: column; gap: 10px">
            <div v-if="submitError" class="alert alert-error">
              <div>
                <div class="strong">提交失败</div>
                <div class="small">{{ submitError }}</div>
              </div>
            </div>
            <button class="btn btn-primary btn-block" :disabled="submitting" @click="submit">
              {{ submitting ? '提交中…' : '提交排查任务' }}
            </button>
            <div class="small faint">
              提交后可在任务详情页跟踪「分析 → 修复 → 验证 → 报告」全过程，并实时接收事件流推送。
            </div>
          </div>
        </div>
      </div>

      <!-- 右：提交说明 -->
      <div style="display: flex; flex-direction: column; gap: 14px">
        <div class="card">
          <div class="card-head">
            <h2 class="card-title">本次提交将触发</h2>
            <span class="badge badge-info">{{ modeLabel(mode) }}</span>
          </div>
          <div class="card-body" style="display: flex; flex-direction: column; gap: 12px">
            <div class="alert alert-info">
              <div>
                <div class="strong">完整链路（共 {{ PIPELINE.length }} 步）</div>
                <div class="small">
                  每一阶段都会写入运行事件与审计记录，可在任务详情页与可观测审计中回溯。
                </div>
              </div>
            </div>
            <div class="pipeline">
              <div v-for="(step, i) in PIPELINE" :key="step.title" class="pipeline-step">
                <span class="pipeline-index">{{ i + 1 }}</span>
                <div style="min-width: 0">
                  <div class="strong small">{{ step.title }}</div>
                  <div class="small muted">{{ step.detail }}</div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <h2 class="card-title">能力边界</h2>
          </div>
          <div class="card-body" style="display: flex; flex-direction: column; gap: 10px">
            <div class="alert alert-warn">
              <div class="small">以下边界决定了自动修复的适用范围，提交前请确认场景匹配。</div>
            </div>
            <ul class="boundary-list">
              <li v-for="b in BOUNDARIES" :key="b" class="small">{{ b }}</li>
            </ul>
          </div>
        </div>

        <div class="card">
          <div class="card-head">
            <h2 class="card-title">快速自检</h2>
          </div>
          <div class="card-body">
            <dl class="kv">
              <dt>排查场景</dt>
              <dd>{{ modeLabel(mode) }}</dd>
              <dt>{{ mode === 'single_repo' ? '目标仓库' : '业务分组' }}</dt>
              <dd>{{ mode === 'single_repo' ? selectedRepo?.name ?? '未选择' : selectedGroup?.name ?? '未选择' }}</dd>
              <dt>版本锁定</dt>
              <dd>{{ isCommitRef ? shortCommit(refValue.trim()) : refValue.trim() || '默认分支最新提交' }}</dd>
              <dt>分组成员</dt>
              <dd>{{ mode === 'group' ? `${members.length} 个仓库` : '不适用' }}</dd>
              <dt>嫌疑文件</dt>
              <dd>{{ entryFiles.length ? `${entryFiles.length} 个` : '未指定' }}</dd>
              <dt>堆栈长度</dt>
              <dd>{{ stackLength }} 字符</dd>
              <dt>沙箱验证</dt>
              <dd>{{ autoVerify ? '开启' : '关闭' }}</dd>
              <dt>环境 / 来源</dt>
              <dd>{{ environment }} / {{ source }}</dd>
              <dt>幂等键</dt>
              <dd class="mono">{{ idempotencyKey.trim() || '-' }}</dd>
            </dl>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.create-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.55fr) minmax(0, 1fr);
  gap: 14px;
  align-items: start;
}

@media (max-width: 1180px) {
  .create-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

.match-box {
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-sm);
  padding: 10px 12px;
  background: var(--bg-elev-2);
}

.pipeline {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.pipeline-step {
  display: flex;
  gap: 10px;
  align-items: flex-start;
  padding: 7px 9px;
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2);
  border: 1px solid var(--border);
}

.pipeline-index {
  width: 20px;
  height: 20px;
  flex: none;
  border-radius: 50%;
  background: var(--brand-soft);
  color: var(--brand-400);
  display: grid;
  place-items: center;
  font-size: 11px;
  font-weight: 650;
  margin-top: 1px;
}

.boundary-list {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 7px;
  color: var(--text-dim);
}
</style>
