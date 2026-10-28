<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NInput, NModal, NTag } from 'naive-ui'
import { api, type PaymentMethod, type StatementDetail, type StatementFinding, type StatementFindingsPage, type StatementReview, type StatementRun, type StatementSummary } from './api'

const props = defineProps<{ canImport: boolean; canReconcile: boolean; canReview: boolean }>()
const statements = ref<StatementSummary[]>([]), detail = ref<StatementDetail | null>(null), run = ref<StatementRun | null>(null)
const findings = ref<StatementFindingsPage>({ items: [], total: 0, offset: 0, limit: 100 })
const reviewFilter = ref<'all' | 'unreviewed' | 'reviewed'>('all')
const busy = ref(false), error = ref(''), notice = ref('')
const importOpen = ref(false), importError = ref(''), importProvider = ref<PaymentMethod['id']>('alipay'), importKind = ref<'payment' | 'refund'>('payment')
const chinaDate = () => new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString().slice(0, 10)
const importDate = ref(chinaDate()), importCoverage = ref<'partial' | 'full'>('partial'), fullConfirmed = ref(false)
const fileInput = ref<HTMLInputElement | null>(null), fileName = ref(''), csvText = ref(''), readingFile = ref(false)
const reviewOpen = ref(false), selectedFinding = ref<StatementFinding | null>(null), reviews = ref<StatementReview[]>([]), reviewNote = ref(''), reviewError = ref('')
let alive = true, fileGeneration = 0
const providers = [
 { value: 'alipay', label: '支付宝' }, { value: 'wechat', label: '微信支付' },
 { value: 'epay_alipay', label: '支付宝（易支付）' }, { value: 'epay_wechat', label: '微信支付（易支付）' }
]
const providerLabel = (value: string) => providers.find(p => p.value === value)?.label || value || '—'
const kindLabel = (value: string) => value === 'payment' ? '收款' : '退款'
const coverageLabel = (value: string) => value === 'full' ? '完整日账单' : '部分账单'
const localStatus = (value: string) => ({ pending: '等待收银台', ready: '等待付款确认', paid: '已确认付款', requested: '待审批', queued: '等待渠道处理', processing: '渠道处理中', succeeded: '退款成功', failed: '退款失败', manual_required: '需要人工处理' }[value] || value || '无本地记录')
const differenceLabel = (value: string) => ({ missing_local: '本地无对应记录', provider_mismatch: '支付渠道不一致', identity_mismatch: '交易标识不一致', amount_mismatch: '金额不一致', not_confirmed: '本地尚未确认成功', local_only: '本地确认、本账单未列出（需核对跨日）' }[value] || value)
const time = (value?: string | null) => value ? new Date(value).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false }) : '—'
function money(value: string | null | undefined) {
 if (value == null) return '—'
 if (!/^-?\d+$/.test(value)) return '金额格式不可用'
 const cents = BigInt(value), absolute = cents < 0n ? -cents : cents
 return `${cents < 0n ? '-' : ''}¥${(absolute / 100n).toLocaleString('zh-CN')}.${(absolute % 100n).toString().padStart(2, '0')}`
}
const visibleStart = computed(() => findings.value.items.length ? findings.value.offset + 1 : 0)
const visibleEnd = computed(() => findings.value.items.length ? Math.min(findings.value.offset + findings.value.items.length, findings.value.total) : 0)
const refundSupported = computed(() => importProvider.value === 'alipay' || importProvider.value === 'wechat')

async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch (e) { if (alive) error.value = e instanceof Error ? e.message : '账单暂时无法处理，请稍后重试。' }
 finally { if (alive) busy.value = false }
}
async function list() { return (await api<{ items: StatementSummary[] }>('/admin/statements')).items }
async function page(statementID: string, runID: string, review: string, offset: number) {
 const query = new URLSearchParams({ review, offset: String(offset), limit: '100' })
 const endpoint = `/admin/statements/${encodeURIComponent(statementID)}/runs/${encodeURIComponent(runID)}/findings`
 const result = await api<StatementFindingsPage>(`${endpoint}?${query}`)
 if (offset > 0 && !result.items.length && offset >= result.total) {
  query.set('offset', String(Math.floor(Math.max(result.total - 1, 0) / 100) * 100))
  return api<StatementFindingsPage>(`${endpoint}?${query}`)
 }
 return result
}
async function report(statementID: string, runID?: string, review: 'all' | 'unreviewed' | 'reviewed' = 'all', offset = 0) {
 const nextDetail = await api<StatementDetail>(`/admin/statements/${encodeURIComponent(statementID)}`)
 const nextRun = runID ? nextDetail.runs.find(item => item.id === runID) : nextDetail.latest_run
 if (!nextRun) throw new Error('该对账版本不在最近展示范围内，请重新选择账单。')
 const nextFindings = await page(statementID, nextRun.id, review, offset)
 if (!alive) return
 detail.value = nextDetail; run.value = nextRun; findings.value = nextFindings; reviewFilter.value = review
}
function refresh() { return action(async () => {
 const nextList = await list()
 if (detail.value && run.value) await report(detail.value.statement.id, run.value.id, reviewFilter.value, findings.value.offset)
 if (alive) statements.value = nextList
}) }
function selectStatement(id: string) { return action(() => report(id)) }
function changeRun(event: Event) {
 const id = (event.target as HTMLSelectElement).value
 if (run.value) (event.target as HTMLSelectElement).value = run.value.id
 if (!detail.value) return
 return action(() => report(detail.value!.statement.id, id))
}
function changeFilter(event: Event) {
 const value = (event.target as HTMLSelectElement).value as 'all' | 'unreviewed' | 'reviewed'
 ;(event.target as HTMLSelectElement).value = reviewFilter.value
 if (!detail.value || !run.value) return
 return action(() => report(detail.value!.statement.id, run.value!.id, value))
}
function changePage(offset: number) {
 if (!detail.value || !run.value) return
 return action(() => report(detail.value!.statement.id, run.value!.id, reviewFilter.value, offset))
}
function reconcile() { return action(async () => {
 if (!props.canReconcile || !detail.value) return
 const id = detail.value.statement.id
 const nextRun = await api<StatementRun>(`/admin/statements/${encodeURIComponent(id)}/reconcile`, {})
 try {
  await report(id, nextRun.id)
  const nextList = await list(); if (alive) statements.value = nextList
  notice.value = `已生成第 ${nextRun.revision} 版对账结果。历史复核记录保留在旧版本，新版本需要重新复核。`
 } catch (e) {
  throw new Error(`第 ${nextRun.revision} 版已生成，但暂时无法刷新显示，请重新读取账单。${e instanceof Error ? e.message : ''}`)
 }
}) }

function clearFile() {
 fileGeneration++; csvText.value = ''; fileName.value = ''; readingFile.value = false
 if (fileInput.value) fileInput.value.value = ''
}
function clearImport() { clearFile(); importError.value = ''; fullConfirmed.value = false }
function openImport() {
 if (!props.canImport) return
 clearImport(); importProvider.value = 'alipay'; importKind.value = 'payment'; importDate.value = chinaDate(); importCoverage.value = 'partial'; importOpen.value = true
}
watch(importOpen, open => { if (!open) clearImport() })
watch(importProvider, () => { if (!refundSupported.value) importKind.value = 'payment' })
watch([importProvider, importKind, importDate, importCoverage], () => { fullConfirmed.value = false })
async function readFile(event: Event) {
 const file = (event.target as HTMLInputElement).files?.[0]
 clearFile(); importError.value = ''
 if (!file) return
 if (file.size > 1024 * 1024) { importError.value = '账单文件不能超过 1 MiB，请按渠道和日期拆分后再导入。'; return }
 const generation = fileGeneration; readingFile.value = true
 try {
  const content = await file.text()
  if (!alive || generation !== fileGeneration || !importOpen.value) return
  csvText.value = content; fileName.value = file.name
 } catch { if (alive && generation === fileGeneration) importError.value = '无法读取文件，请使用 UTF-8 标准 CSV 文件。' }
 finally { if (alive && generation === fileGeneration) readingFile.value = false }
}
function downloadTemplate() {
 const data = 'payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at\r\n'
 const url = URL.createObjectURL(new Blob([data], { type: 'text/csv;charset=utf-8' }))
 const link = document.createElement('a'); link.href = url; link.download = 'xui-statement-v1.csv'; link.click()
 URL.revokeObjectURL(url)
}
async function importFile() {
 if (busy.value || readingFile.value || !props.canImport) return
 importError.value = ''
 if (!fileName.value || !csvText.value) { importError.value = '请选择标准 CSV 账单文件。'; return }
 if (!/^\d{4}-\d{2}-\d{2}$/.test(importDate.value)) { importError.value = '请选择账单日期。'; return }
 if (importCoverage.value === 'full' && !fullConfirmed.value) { importError.value = '请确认文件包含该渠道当日全部成功记录，或选择部分账单。'; return }
 busy.value = true; error.value = ''; notice.value = ''
 try {
  const imported = await api<StatementDetail>('/admin/statements', { provider: importProvider.value, kind: importKind.value, date: importDate.value, coverage: importCoverage.value, csv: csvText.value })
  importOpen.value = false
  try {
   const nextFindings = await page(imported.statement.id, imported.latest_run.id, 'all', 0)
   const nextList = await list()
   if (!alive) return
   detail.value = imported; run.value = imported.latest_run; findings.value = nextFindings; reviewFilter.value = 'all'; statements.value = nextList
   notice.value = '账单已导入或读取已有相同账单。显示已保存的对账版本；本地状态变化后可重新对账。'
  } catch (e) {
   error.value = `账单已保存，但暂时无法显示结果，请刷新账单列表。${e instanceof Error ? e.message : ''}`
  }
 } catch (e) { if (alive) importError.value = e instanceof Error ? e.message : '账单导入失败，请检查格式后重试。' }
 finally { if (alive) busy.value = false }
}

function clearReview() { selectedFinding.value = null; reviewNote.value = ''; reviewError.value = ''; reviews.value = [] }
watch(reviewOpen, open => { if (!open) clearReview() })
function openReview(finding: StatementFinding) { return action(async () => {
 const history = await api<{ items: StatementReview[] }>(`/admin/statement-findings/${encodeURIComponent(finding.id)}/reviews`)
 if (!alive) return
 clearReview(); selectedFinding.value = finding; reviews.value = history.items; reviewOpen.value = true
}) }
async function saveReview() {
 if (busy.value || !selectedFinding.value || !props.canReview) return
 const note = reviewNote.value.trim()
 if (!note || new TextEncoder().encode(note).length > 1000 || /\p{Cc}/u.test(note)) { reviewError.value = '请填写 1–1000 字节的复核说明，不包含换行等控制字符。'; return }
 const finding = selectedFinding.value
 busy.value = true; reviewError.value = ''; notice.value = ''
 try {
  const saved = await api<StatementReview>(`/admin/statement-findings/${encodeURIComponent(finding.id)}/reviews`, { note })
  if (!alive) return
  reviewNote.value = ''; reviews.value = [saved, ...reviews.value.filter(item => item.id !== saved.id)].slice(0, 100)
  notice.value = '复核说明已保存，差异记录继续保留。复核不会修改收款、退款或权益。'
  try {
   if (detail.value && run.value) {
    const nextFindings = await page(detail.value.statement.id, run.value.id, reviewFilter.value, findings.value.offset)
    if (alive) findings.value = nextFindings
   }
  } catch (e) { reviewError.value = `说明已保存，但差异列表暂时无法刷新。${e instanceof Error ? e.message : ''}` }
 } catch (e) { if (alive) reviewError.value = e instanceof Error ? e.message : '复核说明暂时无法保存。' }
 finally { if (alive) busy.value = false }
}
onMounted(() => action(async () => { const next = await list(); if (alive) statements.value = next }))
onBeforeUnmount(() => { alive = false; clearImport(); clearReview() })
</script>

<template>
 <div class="page-heading statement-heading"><div><p class="eyebrow">MERCHANT STATEMENTS</p><h1>商户账单对账</h1><p>导入渠道账单，核对成功收款和退款，保留每次对账与人工复核记录。</p></div><div class="statement-actions"><NButton :loading="busy" @click="refresh">刷新账单列表</NButton><NButton v-if="canImport" type="primary" :disabled="busy" @click="openImport">导入标准账单</NButton></div></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <p class="muted statement-boundary">本轮支持整理后的标准 CSV。导入文件不具有支付验签效力，对账和复核不会修改收款、退款或套餐权益。</p>
 <section class="panel statement-panel">
  <div class="section-title"><h2>最近导入账单</h2><span class="subtle">最近 100 份</span></div>
  <div v-if="statements.length" class="table-wrap"><table><thead><tr><th>日期 / 渠道</th><th>类型 / 范围</th><th>记录 / 总额</th><th>最新对账</th><th>操作</th></tr></thead><tbody><tr v-for="item in statements" :key="item.id" :data-statement-id="item.id" :class="{ 'statement-selected': detail?.statement.id === item.id }"><td><strong>{{ item.date }}</strong><small class="statement-detail">{{ providerLabel(item.provider) }}</small><small class="statement-detail statement-reference">{{ item.id.slice(0, 12) }}</small></td><td>{{ kindLabel(item.kind) }}<small class="statement-detail">{{ coverageLabel(item.coverage) }}</small></td><td>{{ item.row_count }} 条<small class="statement-detail">{{ money(item.total_minor) }}</small></td><td>第 {{ item.latest_run.revision }} 版<small class="statement-detail">{{ item.latest_run.difference_count }} 项差异</small></td><td><NButton size="small" :disabled="busy" @click="selectStatement(item.id)">查看对账结果</NButton></td></tr></tbody></table></div>
  <p v-else class="muted">暂无导入账单。可先下载标准模板，将渠道成功记录整理后导入。</p>
 </section>

 <section v-if="detail && run" class="panel statement-panel" data-testid="statement-report">
  <div class="section-title statement-report-heading"><div><h2>{{ detail.statement.date }} · {{ providerLabel(detail.statement.provider) }} · {{ kindLabel(detail.statement.kind) }}对账</h2><p class="muted">{{ coverageLabel(detail.statement.coverage) }} · 查询时间 {{ time(run.as_of) }}（北京时间）</p></div><NButton v-if="canReconcile" :loading="busy" @click="reconcile">重新对账</NButton></div>
  <div class="statement-stats"><div><span>已匹配记录</span><strong>{{ run.matched_count }}</strong></div><div><span>差异记录</span><strong>{{ run.difference_count }}</strong></div><div><span>其中跨日待核对</span><strong>{{ run.local_candidate_count }}</strong></div></div>
  <p v-if="detail.statement.coverage === 'partial'" class="muted">此文件为部分账单，仅核对已导入记录，不检查本地确认记录是否遗漏。</p>
  <p v-else class="alert statement-timing-note">本地确认时间可能晚于渠道交易时间。“本地确认、本账单未列出”仅表示需要核对跨日或延迟，不能直接认定资金缺失。</p>
  <div class="statement-filters"><label>对账版本<select :value="run.id" aria-label="对账版本" :disabled="busy" @change="changeRun"><option v-for="version in detail.runs" :key="version.id" :value="version.id">第 {{ version.revision }} 版 · {{ time(version.as_of) }}</option></select></label><label>复核状态<select :value="reviewFilter" aria-label="复核状态" :disabled="busy" @change="changeFilter"><option value="all">全部差异</option><option value="unreviewed">尚未复核</option><option value="reviewed">已有复核</option></select></label></div>
  <p class="muted">每版结果固定保存。重新对账生成新版本，历史复核说明不会自动套用到新差异。</p>
  <div v-if="findings.items.length" class="table-wrap"><table class="statement-findings"><thead><tr><th>支付 / 退款编号</th><th>差异</th><th>账单与本地金额</th><th>交易信息 / 时间</th><th>复核记录</th></tr></thead><tbody><tr v-for="finding in findings.items" :key="finding.id" :data-finding-id="finding.id"><td><strong class="statement-reference">{{ finding.payment_id }}</strong><small v-if="finding.refund_id" class="statement-detail statement-reference">退款 {{ finding.refund_id }}</small><small class="statement-detail">{{ finding.row_number == null ? '本地确认记录' : `账单记录 ${finding.row_number}` }}</small></td><td><span v-for="code in finding.codes" :key="code" class="statement-code">{{ differenceLabel(code) }}</span></td><td><span class="statement-detail">账单 {{ money(finding.statement_amount_minor) }}</span><span class="statement-detail">本地 {{ money(finding.local_amount_minor) }}</span><small class="statement-detail">{{ localStatus(finding.local_status) }}<template v-if="finding.local_provider"> · {{ providerLabel(finding.local_provider) }}</template></small></td><td><small class="statement-detail statement-reference">账单交易 {{ finding.statement_transaction_id || '—' }}</small><small class="statement-detail statement-reference">本地交易 {{ finding.local_transaction_id || '—' }}</small><template v-if="detail.statement.kind === 'refund'"><small class="statement-detail statement-reference">账单渠道退款 {{ finding.statement_provider_refund_id || '—' }}</small><small class="statement-detail statement-reference">本地渠道退款 {{ finding.local_provider_refund_id || '—' }}</small></template><small class="statement-detail">渠道发生 {{ time(finding.statement_occurred_at) }}</small><small class="statement-detail">本地确认 {{ time(finding.local_confirmed_at) }}</small></td><td><NTag size="small" :type="finding.review_count ? 'info' : 'warning'">{{ finding.review_count ? `已有 ${finding.review_count} 条复核` : '尚未复核' }}</NTag><p v-if="finding.latest_review" class="statement-note">{{ finding.latest_review.note }}</p><NButton size="small" :disabled="busy" @click="openReview(finding)">{{ canReview ? '查看与记录复核' : '查看复核' }}</NButton></td></tr></tbody></table></div>
  <p v-else class="muted">{{ findings.total ? '此页暂无差异，请返回上一页或更改筛选。' : reviewFilter === 'all' ? '本次导入记录未发现差异。此结果仅针对已保存的账单和查询快照。' : '暂无符合复核状态的差异。' }}</p>
  <div class="statement-pagination"><span class="muted">第 {{ visibleStart }}–{{ visibleEnd }} 项，共 {{ findings.total }} 项</span><div class="statement-actions"><NButton size="small" :disabled="busy || findings.offset === 0" @click="changePage(Math.max(0, findings.offset - findings.limit))">上一页差异</NButton><NButton size="small" :disabled="busy || findings.offset + findings.limit >= findings.total" @click="changePage(findings.offset + findings.limit)">下一页差异</NButton></div></div>
  <details class="statement-source"><summary>查看账单来源标识</summary><p class="statement-reference">账单 {{ detail.statement.id }}</p><p class="statement-reference">内容摘要 {{ detail.statement.digest }}</p><p class="muted">导入时间 {{ time(detail.statement.created_at) }} · 内容相同的账单重复导入会复用原记录。</p></details>
 </section>

 <NModal v-model:show="importOpen" preset="card" title="导入标准账单" style="width: 700px; max-width: calc(100vw - 32px)">
  <form @submit.prevent="importFile">
   <div v-if="importError" role="alert" class="alert error">{{ importError }}</div>
   <p class="muted">请从渠道账单整理为标准 CSV，最多 1 MiB、2000 条记录。金额为正整数分、币种为 CNY，使用原交易总额，不使用扣除手续费后的净额。</p>
   <div class="statement-form-grid"><label>账单渠道<select v-model="importProvider" aria-label="账单渠道" :disabled="busy"><option v-for="provider in providers" :key="provider.value" :value="provider.value">{{ provider.label }}</option></select></label><label>记录类型<select v-model="importKind" aria-label="账单记录类型" :disabled="busy"><option value="payment">成功收款</option><option value="refund" :disabled="!refundSupported">成功退款（仅支付宝 / 微信）</option></select></label><label>账单日期（北京时间）<input v-model="importDate" type="date" aria-label="账单日期" min="2000-01-01" max="2099-12-31" required :disabled="busy" /></label><label>账单范围<select v-model="importCoverage" aria-label="账单范围" :disabled="busy"><option value="partial">部分账单：仅核对导入记录</option><option value="full">完整日账单：同时检查本地记录</option></select></label></div>
   <label v-if="importCoverage === 'full'" class="statement-confirm"><input v-model="fullConfirmed" type="checkbox" :disabled="busy" />我确认这是完整日账单，包含所选渠道、日期和类型的全部成功记录。</label>
   <div class="statement-template"><NButton :disabled="busy" @click="downloadTemplate">下载标准 CSV 模板</NButton><p class="muted">使用商户支付单号 payment_id，而非业务订单号。occurred_at 需包含时区并落在账单日期内。退款还需原交易号、商户退款号及渠道退款号。</p></div>
   <label>标准 CSV 文件<input ref="fileInput" type="file" accept=".csv,text/csv" aria-label="标准 CSV 文件" :disabled="busy || readingFile" @change="readFile" /></label>
   <p v-if="fileName" class="muted statement-reference">已选择 {{ fileName }}</p>
   <p class="muted">未验签账单仅用于差异核对，不会自动补单、退款或撤销权益。</p>
   <div class="statement-actions"><NButton type="primary" attr-type="submit" :loading="busy || readingFile" :disabled="!fileName || (importCoverage === 'full' && !fullConfirmed)">导入并生成对账结果</NButton><NButton :disabled="busy" @click="importOpen = false">取消</NButton></div>
  </form>
 </NModal>

 <NModal v-model:show="reviewOpen" preset="card" title="差异复核记录" style="width: 650px; max-width: calc(100vw - 32px)">
  <div v-if="reviewError" role="alert" class="alert error">{{ reviewError }}</div>
  <template v-if="selectedFinding"><p class="statement-reference">支付 {{ selectedFinding.payment_id }}<template v-if="selectedFinding.refund_id"> · 退款 {{ selectedFinding.refund_id }}</template></p><p class="muted">{{ selectedFinding.codes.map(differenceLabel).join('；') }}</p></template>
  <p class="muted">复核只记录调查结论，不表示资金差异已经纠正，也不会修改收款、退款或权益。最近 100 条说明按时间倒序展示。</p>
  <ol v-if="reviews.length" class="statement-review-list"><li v-for="review in reviews" :key="review.id"><p>{{ review.note }}</p><small>{{ time(review.created_at) }} · 操作者 {{ review.actor_id }}</small></li></ol><p v-else class="muted">尚无复核说明。</p>
  <form v-if="canReview" @submit.prevent="saveReview"><label>复核说明<NInput v-model:value="reviewNote" :input-props="{ required: true, maxlength: 1000, 'aria-label': '复核说明' }" :disabled="busy" placeholder="记录已核查的交易和下一步处理" /></label><NButton type="primary" attr-type="submit" :loading="busy">保存复核说明</NButton></form>
 </NModal>
</template>

<style scoped>
.statement-heading{align-items:flex-start}.statement-actions{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.statement-boundary{margin-bottom:20px}.statement-panel{margin-bottom:20px}.statement-detail{display:block;margin-top:6px;font-size:11px;color:#81907e}.statement-reference{font-family:monospace;overflow-wrap:anywhere;white-space:normal;max-width:24rem}.statement-selected{background:#f5f8ef}.statement-report-heading{gap:16px;align-items:flex-start}.statement-stats{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px;margin:20px 0}.statement-stats>div{background:#f5f7f1;border-radius:9px;padding:16px}.statement-stats span{font-size:12px;color:#7d8d78;display:block}.statement-stats strong{display:block;margin-top:12px;font-size:24px;font-weight:550}.statement-timing-note{background:#f7f2df;color:#817443}.statement-filters,.statement-form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:0 18px}.statement-filters{max-width:700px}select,input[type=date],input[type=file]{display:block;width:100%;min-width:0;margin-top:8px;border:1px solid #d9e1d4;border-radius:8px;background:#fff;padding:10px 12px;color:#3d5340;font:inherit}select:focus,input:focus{outline:2px solid #7dab91;outline-offset:2px}input[type=file]{padding:10px;font-size:12px}select:disabled,input:disabled{opacity:.65}.statement-confirm{display:flex;align-items:flex-start;gap:9px;font-size:12px;line-height:1.8}.statement-confirm input{margin-top:5px;flex-shrink:0}.statement-template{padding:14px 0}.statement-template p{margin-bottom:0}.statement-findings td{vertical-align:top}.statement-findings td:first-child{min-width:150px}.statement-findings td:nth-child(2){min-width:170px;max-width:230px;white-space:normal}.statement-code{display:block;margin-bottom:8px;line-height:1.6;color:#8c6a39}.statement-note{max-width:230px;white-space:normal;overflow-wrap:anywhere;font-size:11px;line-height:1.8;color:#718369}.statement-pagination{display:flex;align-items:center;justify-content:space-between;gap:14px;flex-wrap:wrap;margin-top:20px}.statement-source{margin-top:24px;border-top:1px solid #edf0e7;padding-top:16px;font-size:12px;color:#7a8a73}.statement-source summary{cursor:pointer}.statement-review-list{padding-left:22px;max-height:320px;overflow:auto}.statement-review-list li{padding:0 0 16px 4px}.statement-review-list p{font-size:13px;line-height:1.8;overflow-wrap:anywhere;margin:0 0 6px}.statement-review-list small{font-size:10px;color:#8a9883;overflow-wrap:anywhere}@media(max-width:760px){.statement-heading{flex-direction:column;gap:16px}.statement-form-grid,.statement-filters{grid-template-columns:1fr}.statement-stats{gap:7px}.statement-stats>div{padding:13px 9px}.statement-stats span{font-size:10px;line-height:1.6}.statement-stats strong{font-size:20px}.statement-report-heading{flex-direction:column}.statement-pagination{align-items:flex-start}.statement-reference{max-width:18rem}}
</style>
