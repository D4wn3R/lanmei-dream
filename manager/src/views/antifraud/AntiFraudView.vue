<script setup lang="ts">
// 反诈面板：概览统计 + 待审批/全部记录 + 详情与审批处置（撤回+禁言）
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import type { PrimaryTableCol } from 'tdesign-vue-next'
import { ShieldCheck, ShieldX, Trash2 } from 'lucide-vue-next'
import { antifraudApi } from '@/api'
import { ApiError } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import StepUpDialog from '@/components/StepUpDialog.vue'
import type { AntiFraudRecord, AntiFraudStats } from '@/types/api'

const columns: PrimaryTableCol[] = [
  { colKey: 'created_at', title: '判定时间', width: 170 },
  { colKey: 'target', title: '群 / 用户', width: 200 },
  { colKey: 'label', title: '类别', width: 150 },
  { colKey: 'score', title: '违规分 / 置信度', width: 150 },
  { colKey: 'disposition', title: '处置', width: 170 },
  { colKey: 'status', title: '状态', width: 100 },
  { colKey: 'ops', title: '操作', width: 210, fixed: 'right' },
]

const auth = useAuthStore()
const route = useRoute()

const tab = ref<'pending' | 'all'>('pending')
const items = ref<AntiFraudRecord[]>([])
const total = ref(0)
const loading = ref(false)
const page = ref(1)
const pageSize = 20
const keyword = ref('')
const groupFilter = ref('')
const userFilter = ref('')
const stats = ref<AntiFraudStats | null>(null)
const disabled = ref(false)

const detailVisible = ref(false)
const detail = ref<AntiFraudRecord | null>(null)

const approveVisible = ref(false)
const approveTarget = ref<AntiFraudRecord | null>(null)
const muteSeconds = ref<number | null>(null)
const approveNote = ref('')

const rejectVisible = ref(false)
const rejectTarget = ref<AntiFraudRecord | null>(null)
const rejectNote = ref('')

const stepUpVisible = ref(false)
const pendingAction = ref<((token: string) => Promise<void>) | null>(null)

const scoreBuckets = computed(() => {
  const buckets = stats.value?.score_buckets ?? []
  const max = Math.max(1, ...buckets.map((b) => b.count))
  return buckets.map((b) => ({ ...b, percent: Math.round((b.count / max) * 100) }))
})

async function load() {
  loading.value = true
  try {
    const [pageRes, statsRes] = await Promise.all([
      antifraudApi.records({
        status: tab.value === 'pending' ? 'pending' : '',
        group_id: groupFilter.value.trim(),
        user_id: userFilter.value.trim(),
        keyword: keyword.value.trim(),
        page: page.value,
        pageSize,
      }),
      antifraudApi.stats(),
    ])
    items.value = pageRes.items ?? []
    total.value = pageRes.total
    stats.value = statsRes
    disabled.value = false
  } catch (err) {
    if (err instanceof ApiError && err.status === 503) {
      disabled.value = true
      items.value = []
      stats.value = null
    } else {
      MessagePlugin.error(err instanceof Error ? err.message : '加载失败')
    }
  } finally {
    loading.value = false
  }
}

function switchTab(value: unknown) {
  tab.value = value as 'pending' | 'all'
  page.value = 1
  void load()
}

function search() {
  page.value = 1
  void load()
}

function pageChange(p: { current: number }) {
  page.value = p.current
  void load()
}

function openDetail(rec: AntiFraudRecord) {
  detail.value = rec
  detailVisible.value = true
}

function openApprove(rec: AntiFraudRecord) {
  approveTarget.value = rec
  muteSeconds.value = null
  approveNote.value = ''
  approveVisible.value = true
}

async function submitApprove() {
  if (!approveTarget.value) return
  const target = approveTarget.value
  const body = {
    mute_seconds: muteSeconds.value && muteSeconds.value > 0 ? muteSeconds.value : 0,
    note: approveNote.value.trim(),
  }
  approveVisible.value = false
  try {
    const res = await antifraudApi.approve(target.id, body)
    MessagePlugin.success(describeOutcome(res.record))
    await load()
  } catch (err) {
    MessagePlugin.error(err instanceof Error ? err.message : '处置执行失败')
  }
}

function openReject(rec: AntiFraudRecord) {
  rejectTarget.value = rec
  rejectNote.value = ''
  rejectVisible.value = true
}

async function submitReject() {
  if (!rejectTarget.value) return
  const target = rejectTarget.value
  const note = rejectNote.value.trim()
  rejectVisible.value = false
  try {
    await antifraudApi.reject(target.id, note)
    MessagePlugin.success('已驳回，该记录不再处置')
    await load()
  } catch (err) {
    MessagePlugin.error(err instanceof Error ? err.message : '驳回失败')
  }
}

function remove(rec: AntiFraudRecord) {
  stepUpVisible.value = true
  pendingAction.value = async (token) => {
    await antifraudApi.remove(rec.id, token)
    MessagePlugin.success('记录已删除')
    await load()
  }
}

function onStepUpSuccess(token: string) {
  void pendingAction.value?.(token)
  pendingAction.value = null
}

// describeOutcome 汇总一次处置的动作结果，失败原因原样展示（来自平台响应）。
function describeOutcome(rec: AntiFraudRecord): string {
  const parts: string[] = []
  parts.push(rec.recall_status === 'ok' ? '已撤回' : `撤回失败（${rec.recall_error || '未执行'}）`)
  if (rec.mute_status === 'ok') {
    parts.push(`已禁言 ${formatDuration(rec.mute_seconds)}`)
  } else if (rec.mute_status === 'failed') {
    parts.push(`禁言失败（${rec.mute_error}）`)
  }
  return parts.join('；')
}

function formatDuration(seconds: number): string {
  if (!seconds) return '0 秒'
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
}

function formatTime(value: string | null): string {
  return value ? new Date(value).toLocaleString() : '—'
}

function statusTheme(status: string): 'primary' | 'success' | 'warning' | 'danger' | 'default' {
  switch (status) {
    case 'pending':
      return 'warning'
    case 'auto_handled':
      return 'danger'
    case 'approved':
      return 'success'
    case 'rejected':
      return 'default'
    default:
      return 'primary'
  }
}

function statusText(status: string): string {
  switch (status) {
    case 'pending':
      return '待审批'
    case 'auto_handled':
      return '已自动处置'
    case 'approved':
      return '审批通过'
    case 'rejected':
      return '已驳回'
    default:
      return status
  }
}

// topLabels 取概率最高的若干类别（详情里做分布条）。
function topLabels(rec: AntiFraudRecord, limit = 5) {
  const probs = rec.probabilities ?? []
  return probs
    .map((probability, id) => ({ id, probability }))
    .sort((a, b) => b.probability - a.probability)
    .slice(0, limit)
}

function confirmable(rec: AntiFraudRecord): boolean {
  return rec.status === 'pending' || rec.status === 'auto_handled'
}

onMounted(async () => {
  await load()
  const id = Number(route.query.id)
  if (Number.isFinite(id) && id > 0) {
    try {
      const res = await antifraudApi.record(id)
      openDetail(res.record)
    } catch {
      // 链接失效时保持列表页，不打断使用
    }
  }
})
</script>

<template>
  <div class="page">
    <div class="page-header">
      <h2 class="page-title">反诈</h2>
      <div class="header-search">
        <t-button theme="primary" :loading="loading" @click="load">刷新</t-button>
      </div>
    </div>

    <t-alert
      v-if="disabled"
      theme="info"
      message="反诈插件未启用"
      description="请开启 [plugin.builtins].antifraud 并配置 [plugin.antifraud].endpoint（详见 docs/anti-fraud.md）。"
    />

    <template v-else>
      <t-row :gutter="[16, 16]" class="stat-row">
        <t-col :span="3">
          <t-card class="stat-card">
            <div class="stat-label">待审批</div>
            <div class="stat-value warn">{{ stats?.pending ?? '—' }}</div>
          </t-card>
        </t-col>
        <t-col :span="3">
          <t-card class="stat-card">
            <div class="stat-label">今日新增</div>
            <div class="stat-value">{{ stats?.today_total ?? '—' }}</div>
          </t-card>
        </t-col>
        <t-col :span="3">
          <t-card class="stat-card">
            <div class="stat-label">今日处置</div>
            <div class="stat-value ok">{{ stats?.today_auto ?? '—' }}</div>
          </t-card>
        </t-col>
        <t-col :span="3">
          <t-card class="stat-card">
            <div class="stat-label">动作失败</div>
            <div class="stat-value err">{{ stats?.action_failed ?? '—' }}</div>
          </t-card>
        </t-col>
      </t-row>

      <t-card class="dist-card" title="近 7 天违规分分布（用于校准阈值）">
        <div v-if="scoreBuckets.length" class="buckets">
          <div v-for="b in scoreBuckets" :key="`${b.min}-${b.max}`" class="bucket">
            <span class="bucket-label">{{ b.min.toFixed(1) }}~{{ b.max.toFixed(1) }}</span>
            <div class="bucket-bar"><i :style="{ width: b.percent + '%' }" /></div>
            <span class="bucket-count">{{ b.count }}</span>
          </div>
        </div>
        <div v-else class="muted">暂无数据</div>
        <div v-if="stats?.label_counts?.length" class="labels">
          <t-tag v-for="l in stats.label_counts" :key="l.label_key" theme="danger" variant="light" size="small">
            {{ l.label_name || l.label_key }} {{ l.count }}
          </t-tag>
        </div>
      </t-card>

      <t-card>
        <t-tabs :value="tab" @change="switchTab">
          <t-tab-panel value="pending" label="待审批" />
          <t-tab-panel value="all" label="全部记录" />
        </t-tabs>

        <div class="filters">
          <t-input v-model="groupFilter" placeholder="群 ID" clearable :style="{ width: 160 }" @enter="search" @clear="search" />
          <t-input v-model="userFilter" placeholder="用户 ID" clearable :style="{ width: 160 }" @enter="search" @clear="search" />
          <t-input
            v-model="keyword"
            placeholder="搜索内容 / 昵称…"
            clearable
            :style="{ width: 220 }"
            @enter="search"
            @clear="search"
          />
          <t-button theme="primary" :loading="loading" @click="search">查询</t-button>
        </div>

        <t-table
          :data="items"
          :loading="loading"
          row-key="id"
          :pagination="{ current: page, pageSize, total, onChange: pageChange }"
          :columns="columns"
        >
          <template #created_at="{ row }">{{ formatTime(row.created_at) }}</template>
          <template #target="{ row }">
            <div class="target">
              <span class="mono">{{ row.group_id || '私聊' }}</span>
              <span class="muted">{{ row.nickname || row.user_id }}（{{ row.user_id }}）</span>
            </div>
          </template>
          <template #label="{ row }">
            <t-tag theme="danger" variant="light" size="small">{{ row.label_name || row.label_key }}</t-tag>
          </template>
          <template #score="{ row }">
            <span class="score">{{ row.violation_score.toFixed(2) }}</span>
            <span class="muted"> / {{ row.confidence.toFixed(2) }}</span>
          </template>
          <template #disposition="{ row }">
            <span :class="{ err: row.recall_status === 'failed' }">
              撤回：{{ row.recall_status === 'ok' ? '成功' : row.recall_status === 'failed' ? '失败' : '未执行' }}
            </span>
            <span class="muted">
              ｜禁言：{{ row.mute_status === 'ok' ? formatDuration(row.mute_seconds) : row.mute_status === 'failed' ? '失败' : '未执行' }}
            </span>
            <div v-if="row.recall_error" class="err small">{{ row.recall_error }}</div>
            <div v-if="row.mute_error" class="err small">{{ row.mute_error }}</div>
          </template>
          <template #status="{ row }">
            <t-tag :theme="statusTheme(row.status)" variant="light" size="small">{{ statusText(row.status) }}</t-tag>
            <t-tag v-if="row.repeat_count > 1" theme="warning" variant="light" size="small">×{{ row.repeat_count }}</t-tag>
          </template>
          <template #ops="{ row }">
            <t-space size="4">
              <t-button size="small" variant="text" @click="openDetail(row)">详情</t-button>
              <t-button v-if="confirmable(row)" size="small" theme="primary" @click="openApprove(row)">
                <template #icon><ShieldCheck :size="14" /></template>通过
              </t-button>
              <t-button v-if="row.status === 'pending'" size="small" variant="base" @click="openReject(row)">
                <template #icon><ShieldX :size="14" /></template>驳回
              </t-button>
              <t-button v-if="auth.isSuper" size="small" variant="text" theme="danger" @click="remove(row)">
                <template #icon><Trash2 :size="14" /></template>
              </t-button>
            </t-space>
          </template>
          <template #empty>
            <div class="muted">暂无违规记录</div>
          </template>
        </t-table>
      </t-card>
    </template>

    <t-drawer v-model:visible="detailVisible" header="违规详情" size="560px" :footer="false">
      <template v-if="detail">
        <div class="detail-row"><span class="detail-label">判定时间</span>{{ formatTime(detail.created_at) }}</div>
        <div class="detail-row"><span class="detail-label">平台 / 群</span>{{ detail.platform }} / {{ detail.group_id || '私聊' }}</div>
        <div class="detail-row">
          <span class="detail-label">用户</span>{{ detail.nickname || '—' }}（{{ detail.user_id }}）
        </div>
        <div class="detail-row">
          <span class="detail-label">类别</span>{{ detail.label_name || detail.label_key }}（置信度 {{ detail.confidence.toFixed(3) }}）
        </div>
        <div class="detail-row">
          <span class="detail-label">违规分</span><span class="score">{{ detail.violation_score.toFixed(3) }}</span>
        </div>
        <div class="detail-row"><span class="detail-label">消息 ID</span><span class="mono">{{ detail.message_id || '—' }}</span></div>
        <div class="detail-block">
          <div class="detail-label">内容</div>
          <pre class="content">{{ detail.content }}</pre>
        </div>
        <div class="detail-block">
          <div class="detail-label">类别概率（top 5）</div>
          <div v-for="l in topLabels(detail)" :key="l.id" class="bucket">
            <span class="bucket-label">#{{ l.id }}</span>
            <div class="bucket-bar"><i :style="{ width: Math.round(l.probability * 100) + '%' }" /></div>
            <span class="bucket-count">{{ (l.probability * 100).toFixed(1) }}%</span>
          </div>
        </div>
        <div v-if="detail.expert_weights?.length" class="detail-block">
          <div class="detail-label">专家门控权重</div>
          <t-space size="4" wrap>
            <t-tag v-for="e in detail.expert_weights" :key="e.key" size="small" variant="light">
              {{ e.key }} {{ e.weight.toFixed(3) }}
            </t-tag>
          </t-space>
        </div>
        <div class="detail-block">
          <div class="detail-label">处置结果</div>
          <div>撤回：{{ detail.recall_status === 'ok' ? '成功' : detail.recall_status === 'failed' ? `失败（${detail.recall_error}）` : '未执行' }}</div>
          <div>禁言：{{ detail.mute_status === 'ok' ? `已禁言 ${formatDuration(detail.mute_seconds)}` : detail.mute_status === 'failed' ? `失败（${detail.mute_error}）` : '未执行' }}</div>
          <div v-if="detail.repeat_count > 1" class="muted">同一用户同文本重复 {{ detail.repeat_count }} 次</div>
        </div>
        <div v-if="detail.reviewed_by" class="detail-block">
          <div class="detail-label">审批</div>
          <div>{{ detail.reviewed_by }} · {{ formatTime(detail.reviewed_at) }}</div>
          <div v-if="detail.review_note" class="muted">{{ detail.review_note }}</div>
        </div>
        <div class="drawer-ops">
          <t-button v-if="confirmable(detail)" theme="primary" @click="openApprove(detail)">
            <template #icon><ShieldCheck :size="14" /></template>通过并处置（撤回 + 禁言）
          </t-button>
          <t-button v-if="detail.status === 'pending'" variant="base" @click="openReject(detail)">
            <template #icon><ShieldX :size="14" /></template>驳回
          </t-button>
        </div>
      </template>
    </t-drawer>

    <t-dialog
      v-model:visible="approveVisible"
      header="审批通过并执行处置"
      :confirm-btn="{ content: '确认处置', theme: 'primary' }"
      cancel-btn="取消"
      @confirm="submitApprove"
    >
      <p class="muted">将对该消息执行撤回，并禁言发送者；失败原因由平台返回并记录在案。</p>
      <div class="field">
        <span class="field-label">禁言时长（秒）</span>
        <t-input-number v-model="muteSeconds" :min="0" :step="60" placeholder="留空使用插件默认时长" />
      </div>
      <t-textarea v-model="approveNote" :autosize="{ minRows: 2, maxRows: 4 }" placeholder="审批备注（可选）" />
    </t-dialog>

    <t-dialog
      v-model:visible="rejectVisible"
      header="驳回（判定为误报）"
      :confirm-btn="{ content: '确认驳回', theme: 'danger' }"
      cancel-btn="取消"
      @confirm="submitReject"
    >
      <p class="muted">驳回只标记结论，不会撤回或禁言；若此前已自动处置，动作无法撤销。</p>
      <t-textarea v-model="rejectNote" :autosize="{ minRows: 2, maxRows: 4 }" placeholder="驳回原因（可选）" />
    </t-dialog>

    <StepUpDialog v-model="stepUpVisible" @success="onStepUpSuccess" />
  </div>
</template>

<style scoped>
.page-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.page-title {
  margin: 0;
  font-size: 20px;
}
.header-search {
  display: flex;
  gap: 8px;
}
.stat-row {
  margin-bottom: 16px;
}
.stat-card {
  text-align: center;
}
.stat-label {
  color: var(--mgr-text-secondary);
  font-size: 13px;
  margin-bottom: 6px;
}
.stat-value {
  font-size: 26px;
  font-weight: 600;
}
.stat-value.warn {
  color: var(--td-warning-color);
}
.stat-value.ok {
  color: var(--td-success-color);
}
.stat-value.err {
  color: var(--td-error-color);
}
.dist-card {
  margin-bottom: 16px;
}
.buckets {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: 8px 24px;
}
.bucket {
  display: flex;
  align-items: center;
  gap: 8px;
}
.bucket-label {
  width: 72px;
  color: var(--mgr-text-secondary);
  font-size: 13px;
}
.bucket-bar {
  flex: 1;
  height: 8px;
  background: var(--td-bg-color-component);
  border-radius: 4px;
  overflow: hidden;
}
.bucket-bar i {
  display: block;
  height: 100%;
  background: var(--td-brand-color);
}
.bucket-count {
  width: 48px;
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.labels {
  margin-top: 12px;
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
.filters {
  display: flex;
  gap: 8px;
  margin: 12px 0;
  flex-wrap: wrap;
}
.target {
  display: flex;
  flex-direction: column;
}
.mono {
  font-family: var(--td-font-family-mono, monospace);
}
.muted {
  color: var(--mgr-text-secondary);
}
.small {
  font-size: 12px;
}
.score {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.err {
  color: var(--td-error-color);
}
.detail-row {
  display: flex;
  gap: 8px;
  padding: 4px 0;
}
.detail-label {
  width: 96px;
  color: var(--mgr-text-secondary);
  flex: none;
}
.detail-block {
  margin-top: 14px;
}
.content {
  margin: 6px 0 0;
  padding: 10px;
  background: var(--td-bg-color-container-hover);
  border-radius: 6px;
  white-space: pre-wrap;
  word-break: break-all;
  font-family: inherit;
}
.drawer-ops {
  margin-top: 20px;
  display: flex;
  gap: 8px;
}
.field {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}
.field-label {
  color: var(--mgr-text-secondary);
  flex: none;
}
</style>
