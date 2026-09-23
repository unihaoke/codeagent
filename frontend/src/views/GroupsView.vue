<script setup lang="ts">
/**
 * 业务分组列表（第三层：仓库分组索引层）。
 *
 * 分组是纯逻辑业务索引：不改变仓库独立性，也不批量加载代码，
 * 仅用于多仓库链路推断与「按需加载」的范围界定。
 */
import { computed, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api/client'
import type { GroupMember, GroupStatus, Repository, RepositoryGroup } from '@/types'
import { formatNumber, formatRelative, formatTime, layerLabel, truncate } from '@/utils/format'
import { toastError, toastOk } from '@/composables/useToast'
import { useAsync } from '@/composables/useAsync'
import AppModal from '@/components/AppModal.vue'
import BarList from '@/components/BarList.vue'
import EmptyState from '@/components/EmptyState.vue'
import StatusBadge from '@/components/StatusBadge.vue'

const router = useRouter()

function errText(e: unknown): string {
  if (e instanceof ApiError) return `${e.message}（code=${e.code}）`
  if (e instanceof Error) return e.message
  return String(e)
}

/* ------------------------------ 数据加载 ------------------------------ */

const {
  data: groupPage,
  loading: groupsLoading,
  error: groupsError,
  reload: reloadGroups,
} = useAsync(() => api.listGroups({ page: 1, pageSize: 200 }), { silent: true })

const { data: repoPage, error: repoError } = useAsync(
  () => api.listRepos({ page: 1, pageSize: 200 }),
  { silent: true },
)

const groups = computed<RepositoryGroup[]>(() => groupPage.value?.items ?? [])
const allRepos = computed<Repository[]>(() => repoPage.value?.items ?? [])

/** 每个分组的成员：逐分组加载，单个失败不影响其它卡片。 */
const memberMap = ref<Record<string, GroupMember[]>>({})
const memberErrors = ref<Record<string, string>>({})

async function loadMembers(list: RepositoryGroup[]) {
  if (!list.length) {
    memberMap.value = {}
    memberErrors.value = {}
    return
  }
  const settled = await Promise.allSettled(list.map((g) => api.getGroup(g.id)))
  const okMap: Record<string, GroupMember[]> = {}
  const errMap: Record<string, string> = {}
  settled.forEach((res, i) => {
    const group = list[i]
    if (!group) return
    if (res.status === 'fulfilled') {
      okMap[group.id] = res.value?.members ?? []
    } else {
      errMap[group.id] = res.reason instanceof Error ? res.reason.message : '成员加载失败'
    }
  })
  memberMap.value = okMap
  memberErrors.value = errMap
}

watch(groups, (list) => {
  void loadMembers(list)
})

const layerNameMap = computed<Record<string, string>>(() => {
  const acc: Record<string, string> = {}
  for (const key of ['frontend', 'gateway', 'service', 'middleware', 'library', 'unknown']) {
    acc[key] = layerLabel(key)
  }
  return acc
})

function memberCount(groupId: string): string {
  const list = memberMap.value[groupId]
  return list ? formatNumber(list.length) : '—'
}

function memberLayerDist(groupId: string): Record<string, number> {
  const acc: Record<string, number> = {}
  for (const m of memberMap.value[groupId] ?? []) {
    const key = m?.repo?.layer ?? 'unknown'
    acc[key] = (acc[key] ?? 0) + 1
  }
  return acc
}

function repoOf(repositoryId: string): Repository | undefined {
  for (const list of Object.values(memberMap.value)) {
    const hit = list.find((m) => m.repositoryId === repositoryId)
    if (hit?.repo) return hit.repo
  }
  return allRepos.value.find((r) => r.id === repositoryId)
}

function entryNames(group: RepositoryGroup): { id: string; name: string }[] {
  return (group?.entryRepositoryIds ?? []).map((id) => ({
    id,
    name: repoOf(id)?.name ?? id,
  }))
}

function layerTone(layer?: string): string {
  switch (layer) {
    case 'frontend':
      return 'info'
    case 'gateway':
      return 'degraded'
    case 'service':
      return 'ok'
    case 'middleware':
      return 'warn'
    case 'library':
      return 'info'
    default:
      return 'muted'
  }
}

function openDetail(group: RepositoryGroup) {
  if (!group?.id) return
  void router.push({ name: 'group-detail', params: { id: group.id } })
}

/* --------------------------- 新建 / 编辑表单 --------------------------- */

const formOpen = ref(false)
const editingId = ref('')
const saving = ref(false)
const formError = ref('')
const formErrors = reactive<Record<string, string>>({})
const form = reactive({ name: '', key: '', description: '', status: 'active' })

/** 已选成员（数组顺序即链路 order）；entryIds 为链路入口集合。 */
const selectedIds = ref<string[]>([])
const entryIds = ref<string[]>([])
const repoKeyword = ref('')

const pickerRepos = computed<Repository[]>(() => {
  const kw = repoKeyword.value.trim().toLowerCase()
  if (!kw) return allRepos.value
  return allRepos.value.filter((r) =>
    [r?.name, r?.key, r?.language, r?.layer].some((v) => (v ?? '').toLowerCase().includes(kw)),
  )
})

const selectedRepos = computed<{ repo?: Repository; id: string }[]>(() =>
  selectedIds.value.map((id) => ({ id, repo: allRepos.value.find((r) => r.id === id) })),
)

function isSelected(id: string): boolean {
  return selectedIds.value.includes(id)
}

function isEntry(id: string): boolean {
  return entryIds.value.includes(id)
}

function toggleRepo(id: string, checked: boolean) {
  if (checked) {
    if (!selectedIds.value.includes(id)) selectedIds.value = [...selectedIds.value, id]
    return
  }
  selectedIds.value = selectedIds.value.filter((x) => x !== id)
  entryIds.value = entryIds.value.filter((x) => x !== id)
}

function onRepoCheck(id: string, e: Event) {
  const target = e.target as HTMLInputElement | null
  toggleRepo(id, Boolean(target?.checked))
}

function toggleEntry(id: string, checked: boolean) {
  if (!isSelected(id)) return
  if (checked) {
    if (!entryIds.value.includes(id)) entryIds.value = [...entryIds.value, id]
    return
  }
  entryIds.value = entryIds.value.filter((x) => x !== id)
}

function onEntryCheck(id: string, e: Event) {
  const target = e.target as HTMLInputElement | null
  toggleEntry(id, Boolean(target?.checked))
}

function moveRepo(id: string, dir: number) {
  const list = [...selectedIds.value]
  const i = list.indexOf(id)
  const j = i + dir
  if (i < 0 || j < 0 || j >= list.length) return
  const tmp = list[i]
  list[i] = list[j]
  list[j] = tmp
  selectedIds.value = list
}

function clearFormErrors() {
  for (const k of Object.keys(formErrors)) delete formErrors[k]
}

function resetForm(group?: RepositoryGroup, members?: GroupMember[]) {
  Object.assign(form, {
    name: group?.name ?? '',
    key: group?.key ?? '',
    description: group?.description ?? '',
    status: group?.status ?? 'active',
  })
  const sorted = [...(members ?? [])].sort((a, b) => (a?.order ?? 0) - (b?.order ?? 0))
  selectedIds.value = sorted.map((m) => m.repositoryId).filter(Boolean)
  entryIds.value = [...(group?.entryRepositoryIds ?? [])].filter((id) => selectedIds.value.includes(id))
  repoKeyword.value = ''
  formError.value = ''
  clearFormErrors()
}

function openCreate() {
  editingId.value = ''
  resetForm()
  formOpen.value = true
}

async function openEdit(group: RepositoryGroup) {
  editingId.value = group.id
  let members = memberMap.value[group.id]
  if (!members) {
    try {
      const detail = await api.getGroup(group.id)
      members = detail?.members ?? []
      memberMap.value = { ...memberMap.value, [group.id]: members }
    } catch (e) {
      members = []
      toastError('成员加载失败', errText(e))
    }
  }
  resetForm(group, members)
  formOpen.value = true
}

function validate(): boolean {
  clearFormErrors()
  if (!form.name.trim()) formErrors.name = '请输入分组名称'
  if (!form.key.trim()) formErrors.key = '请输入分组 Key'
  else if (!/^[A-Za-z0-9._-]+$/.test(form.key.trim())) {
    formErrors.key = 'Key 仅允许字母、数字、点、下划线、短横线'
  }
  if (!selectedIds.value.length) formErrors.members = '请至少选择一个成员仓库'
  return Object.keys(formErrors).length === 0
}

async function submitForm() {
  if (!validate()) return
  saving.value = true
  formError.value = ''
  try {
    const members = selectedIds.value.map((repositoryId, i) => ({ repositoryId, order: i }))
    const group: Partial<RepositoryGroup> = {
      name: form.name.trim(),
      key: form.key.trim(),
      description: form.description.trim(),
      status: form.status as GroupStatus,
      entryRepositoryIds: entryIds.value.filter((id) => selectedIds.value.includes(id)),
    }
    if (editingId.value) {
      await api.updateGroup(editingId.value, { group, members })
      toastOk('分组已更新', String(group.name))
    } else {
      await api.createGroup({ group, members })
      toastOk('分组已创建', `${group.name}（${members.length} 个成员）`)
    }
    formOpen.value = false
    await reloadGroups()
  } catch (e) {
    formError.value = `${editingId.value ? '更新分组' : '创建分组'}失败：${errText(e)}`
    toastError(editingId.value ? '更新分组失败' : '创建分组失败', errText(e))
  } finally {
    saving.value = false
  }
}

/* ------------------------------- 删除 ------------------------------- */

const deleteTarget = ref<RepositoryGroup | null>(null)
const deleting = ref(false)
const deleteError = ref('')

async function confirmDelete() {
  const target = deleteTarget.value
  if (!target) return
  deleting.value = true
  deleteError.value = ''
  try {
    await api.deleteGroup(target.id)
    toastOk('分组已删除', target.name)
    deleteTarget.value = null
    await reloadGroups()
  } catch (e) {
    deleteError.value = errText(e)
    toastError('删除分组失败', deleteError.value)
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h1 class="page-title">业务分组</h1>
        <p class="page-subtitle">分组为纯逻辑业务索引，不改变仓库独立性，不批量加载代码</p>
      </div>
      <div class="row">
        <button class="btn" :disabled="groupsLoading" @click="reloadGroups()">刷新</button>
        <button class="btn btn-primary" @click="openCreate">新建分组</button>
      </div>
    </div>

    <div v-if="groupsError" class="alert alert-error">
      <span class="strong nowrap">分组列表加载失败</span>
      <span>{{ groupsError }}</span>
      <span class="spacer" />
      <button class="btn btn-sm" @click="reloadGroups()">重试</button>
    </div>

    <div v-if="groupsLoading && !groups.length" class="grid grid-3">
      <div v-for="i in 3" :key="i" class="card">
        <div class="card-body">
          <div class="skeleton" style="height: 16px; width: 40%; margin-bottom: 12px" />
          <div class="skeleton" style="height: 12px; margin-bottom: 8px" />
          <div class="skeleton" style="height: 12px; width: 70%" />
        </div>
      </div>
    </div>

    <div v-else-if="!groups.length" class="card">
      <EmptyState
        :title="groupsError ? '分组数据不可用' : '暂无业务分组'"
        :hint="groupsError ? '接口暂不可用，可点击刷新重试' : '把同一条调用链上的仓库编成一个分组，便于多仓库联合排查'"
        icon="🗂"
      >
        <button class="btn btn-primary btn-sm" @click="openCreate">新建分组</button>
      </EmptyState>
    </div>

    <div v-else class="grid grid-3">
      <div v-for="g in groups" :key="g.id" class="card group-card" @click="openDetail(g)">
        <div class="card-head">
          <div style="min-width: 0">
            <div class="strong ellipsis" :title="g.name">{{ g.name || '未命名分组' }}</div>
            <div class="mono small muted ellipsis">{{ g.key || '-' }}</div>
          </div>
          <StatusBadge
            :label="g.status === 'disabled' ? '已停用' : '启用中'"
            :tone="g.status === 'disabled' ? 'muted' : 'ok'"
          />
        </div>
        <div class="card-body">
          <div class="small muted" style="min-height: 21px">
            {{ g.description ? truncate(g.description, 88) : '暂无描述' }}
          </div>

          <div class="row wrap" style="margin-top: 10px; gap: 6px">
            <span class="badge badge-muted">成员 {{ memberCount(g.id) }}</span>
            <span v-for="e in entryNames(g)" :key="e.id" class="badge badge-info">入口 · {{ e.name }}</span>
            <span v-if="!entryNames(g).length" class="badge badge-warn">未指定链路入口</span>
          </div>

          <div v-if="memberErrors[g.id]" class="alert alert-error small" style="margin-top: 10px">
            成员加载失败：{{ memberErrors[g.id] }}
          </div>
          <div v-else style="margin-top: 12px">
            <div class="small faint" style="margin-bottom: 6px">成员分层分布</div>
            <BarList :data="memberLayerDist(g.id)" :label-map="layerNameMap" />
          </div>

          <div class="row" style="margin-top: 12px">
            <button class="btn btn-sm btn-primary" @click.stop="openDetail(g)">查看链路与影响面</button>
            <button class="btn btn-sm" @click.stop="openEdit(g)">编辑</button>
            <button class="btn btn-sm btn-danger" @click.stop="deleteTarget = g">删除</button>
            <span class="spacer" />
            <span class="small faint nowrap" :title="formatTime(g.updatedAt)">{{ formatRelative(g.updatedAt) }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- 新建 / 编辑分组 -->
    <AppModal
      v-if="formOpen"
      :title="editingId ? `编辑分组 · ${form.name}` : '新建业务分组'"
      wide
      @close="formOpen = false"
    >
      <div v-if="formError" class="alert alert-error" style="margin-bottom: 12px">{{ formError }}</div>

      <div class="grid grid-2">
        <div class="field">
          <label class="field-label">名称<span class="req">*</span></label>
          <input v-model="form.name" class="input" placeholder="交易链路" />
          <span v-if="formErrors.name" class="field-error">{{ formErrors.name }}</span>
        </div>
        <div class="field">
          <label class="field-label">Key<span class="req">*</span></label>
          <input v-model="form.key" class="input mono" placeholder="trade-chain" />
          <span class="field-hint">业务唯一键，用于任务与报告中的分组标识</span>
          <span v-if="formErrors.key" class="field-error">{{ formErrors.key }}</span>
        </div>
      </div>

      <div class="grid grid-2" style="margin-top: 12px">
        <div class="field">
          <label class="field-label">描述</label>
          <textarea v-model="form.description" class="textarea" style="min-height: 72px" placeholder="从网关到核心服务再到公共库的完整交易链路" />
        </div>
        <div class="field">
          <label class="field-label">状态</label>
          <select v-model="form.status" class="select">
            <option value="active">启用</option>
            <option value="disabled">停用</option>
          </select>
          <span class="field-hint">停用后分组仍保留成员关系，但不会出现在任务创建的可选链路中。</span>
        </div>
      </div>

      <div class="card" style="margin-top: 16px">
        <div class="card-head">
          <h3 class="card-title">成员仓库与链路顺序</h3>
          <span class="small muted">越靠前越接近调用入口</span>
        </div>
        <div class="card-body">
          <div v-if="repoError" class="alert alert-error small" style="margin-bottom: 10px">
            仓库列表加载失败：{{ repoError }}
          </div>
          <div v-else-if="!allRepos.length" class="alert alert-warn small" style="margin-bottom: 10px">
            暂无可选仓库，请先在「代码源仓库」注册。
          </div>

          <div class="grid grid-2">
            <div class="field">
              <label class="field-label">可选仓库（{{ formatNumber(pickerRepos.length) }}）</label>
              <input v-model="repoKeyword" class="input" placeholder="搜索仓库名 / Key / 语言" />
              <div class="picker">
                <label v-for="r in pickerRepos" :key="r.id" class="picker-row">
                  <input type="checkbox" :checked="isSelected(r.id)" @change="onRepoCheck(r.id, $event)" />
                  <span class="ellipsis" style="flex: 1">{{ r.name || r.key }}</span>
                  <span class="badge" :class="`badge-${layerTone(r.layer)}`">{{ layerLabel(r.layer) }}</span>
                  <span class="small muted nowrap">{{ r.language || '-' }}</span>
                </label>
                <div v-if="!pickerRepos.length" class="small muted" style="padding: 10px">没有匹配的仓库</div>
              </div>
            </div>

            <div class="field">
              <label class="field-label">已选成员（{{ selectedIds.length }}）· 顺序即链路顺序</label>
              <div class="picker">
                <div v-for="(item, idx) in selectedRepos" :key="item.id" class="picker-row">
                  <span class="badge badge-muted mono nowrap">{{ idx + 1 }}</span>
                  <span class="ellipsis" style="flex: 1" :title="item.repo?.name ?? item.id">
                    {{ item.repo?.name ?? item.id }}
                  </span>
                  <span v-if="item.repo" class="badge" :class="`badge-${layerTone(item.repo.layer)}`">
                    {{ layerLabel(item.repo.layer) }}
                  </span>
                  <label class="checkbox small nowrap" title="设为链路入口">
                    <input
                      type="checkbox"
                      :checked="isEntry(item.id)"
                      @change="onEntryCheck(item.id, $event)"
                    />
                    入口
                  </label>
                  <button class="btn btn-sm btn-ghost" title="上移" :disabled="idx === 0" @click="moveRepo(item.id, -1)">↑</button>
                  <button
                    class="btn btn-sm btn-ghost"
                    title="下移"
                    :disabled="idx === selectedIds.length - 1"
                    @click="moveRepo(item.id, 1)"
                  >
                    ↓
                  </button>
                  <button class="btn btn-sm btn-ghost" title="移出分组" @click="toggleRepo(item.id, false)">×</button>
                </div>
                <div v-if="!selectedIds.length" class="small muted" style="padding: 10px">
                  从左侧勾选仓库加入分组
                </div>
              </div>
              <span v-if="formErrors.members" class="field-error">{{ formErrors.members }}</span>
              <span v-else-if="selectedIds.length && !entryIds.length" class="field-hint" style="color: var(--warn)">
                建议勾选「入口」，便于从调用入口开始解析堆栈链路。
              </span>
            </div>
          </div>

          <div class="alert alert-info small" style="margin-top: 12px">
            分组仅描述业务链路关系；故障分析时先解析堆栈链路，再按需加载故障关联的少量文件，不会全量拉取分组代码。
          </div>
        </div>
      </div>

      <template #footer>
        <button class="btn" :disabled="saving" @click="formOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="saving" @click="submitForm">
          {{ saving ? '提交中…' : editingId ? '保存修改' : '创建分组' }}
        </button>
      </template>
    </AppModal>

    <!-- 删除确认 -->
    <AppModal v-if="deleteTarget" title="删除分组" @close="deleteTarget = null">
      <div class="alert alert-error" style="margin-bottom: 12px">
        删除分组不会影响仓库本身，仅移除这层逻辑业务索引。
      </div>
      <div v-if="deleteError" class="alert alert-error" style="margin-bottom: 12px">{{ deleteError }}</div>
      <dl class="kv">
        <dt>名称</dt>
        <dd class="strong">{{ deleteTarget?.name }}</dd>
        <dt>Key</dt>
        <dd class="mono">{{ deleteTarget?.key }}</dd>
        <dt>成员数</dt>
        <dd>{{ deleteTarget ? memberCount(deleteTarget.id) : '—' }}</dd>
      </dl>
      <template #footer>
        <button class="btn" :disabled="deleting" @click="deleteTarget = null">取消</button>
        <button class="btn btn-danger" :disabled="deleting" @click="confirmDelete">
          {{ deleting ? '删除中…' : '确认删除' }}
        </button>
      </template>
    </AppModal>
  </div>
</template>

<style scoped>
.group-card {
  cursor: pointer;
  transition: border-color 160ms cubic-bezier(0.4, 0, 0.2, 1);
}

.group-card:hover {
  border-color: var(--brand-500);
}

.picker {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg-elev-2);
  max-height: 260px;
  overflow: auto;
  padding: 4px;
}

.picker-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 8px;
  border-radius: var(--radius-sm);
  font-size: 13px;
  cursor: pointer;
}

.picker-row:hover {
  background: var(--bg-hover);
}
</style>
