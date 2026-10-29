<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { NButton, NTag } from 'naive-ui'
import { api, type FinancialReport, type FinancialSnapshot, type StatementSource } from './api'

const props = defineProps<{ statementId: string; sources: StatementSource[]; canReconcile: boolean }>()
const open = ref(false), sourceID = ref(''), snapshot = ref<FinancialSnapshot | null>(null), report = ref<FinancialReport | null>(null)
const loading = ref(false), saving = ref(false), reading = ref(false), error = ref(''), fileError = ref(''), notice = ref('')
const fileInput = ref<HTMLInputElement | null>(null), fileName = ref(''), csv = ref(''), selectedRun = ref(''), filter = ref('all'), offset = ref(0)
let alive = true, generation = 0, fileGeneration = 0
const time = (value: string) => new Date(value).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })
const sourceFormat = (value: string) => ({ normalized_csv_v1: '标准 CSV', wechat_success_v1: '微信成功收款', wechat_refund_v1: '微信成功退款', alipay_trade_v1: '支付宝经典明细' }[value] || value)
const basis = computed(() => ({ wechat_settlement_order: '应结订单金额', wechat_refund: '退款金额（渠道口径）', alipay_merchant_received: '商家实收' }[snapshot.value?.basis || ''] || '渠道金额'))
const code = (value: string) => ({ missing_reference: '未提供预期记录', missing_observation: '来源无对应记录', identity_mismatch: '退款对应的支付编号不一致', fee_unavailable: '来源未提供手续费', channel_amount_unavailable: '来源未提供渠道金额', fee_mismatch: '手续费不一致', channel_amount_mismatch: '渠道金额不一致', fee_total_mismatch: '手续费明细合计与文件汇总不一致', channel_amount_total_mismatch: '渠道金额明细合计与文件汇总不一致' }[value] || '待核查项目')
const status = (value: string) => ({ matched: '预期字段已匹配', difference: '存在差异', unassessed: '尚未核对' }[value] || value)
function money(value: string | null | undefined) {
 if (value == null) return '未提供'
 const parts = /^(-?)(\d+)\.(\d{5})$/.exec(value)
 if (!parts) return '金额格式不可用'
 return `${parts[1]}¥${parts[2]!.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}.${parts[3]}`
}
const codes = computed(() => report.value?.run.summary_codes || snapshot.value?.summary_codes || [])
const filteredRows = computed(() => report.value ? report.value.rows.filter(row => filter.value === 'all' || row.status === filter.value) : [])
const totalRows = computed(() => report.value ? filteredRows.value.length : snapshot.value?.rows.length || 0)
const reportPage = computed(() => filteredRows.value.slice(offset.value, offset.value + 100))
const observationPage = computed(() => snapshot.value?.rows.slice(offset.value, offset.value + 100) || [])
const start = computed(() => totalRows.value ? offset.value + 1 : 0)
const end = computed(() => Math.min(offset.value + 100, totalRows.value))
const endpoint = () => `/admin/statements/${encodeURIComponent(props.statementId)}/sources/${encodeURIComponent(sourceID.value)}/financials`
const current = (token: number) => alive && open.value && token === generation
function clearFile() {
 fileGeneration++; fileName.value = ''; csv.value = ''; reading.value = false; fileError.value = ''
 if (fileInput.value) fileInput.value.value = ''
}
function reset() {
 generation++; loading.value = false; saving.value = false; snapshot.value = null; report.value = null
 error.value = ''; notice.value = ''; selectedRun.value = ''; filter.value = 'all'; offset.value = 0; clearFile()
}
function close() { open.value = false; reset(); sourceID.value = '' }
function show() {
 if (!props.sources.length) return
 open.value = true
 const first = props.sources[0]!.id
 if (sourceID.value === first) void load()
 else sourceID.value = first
}
async function load() {
 reset()
 if (!sourceID.value || !open.value) return
 const token = generation
 loading.value = true
 try { const data = await api<FinancialSnapshot>(endpoint()); if (current(token)) snapshot.value = data }
 catch (e) { if (current(token)) error.value = e instanceof Error ? e.message : '费用观察暂时无法读取。' }
 finally { if (current(token)) loading.value = false }
}
watch(sourceID, () => { if (open.value) void load() })
watch(() => props.statementId, close)
watch(() => props.sources, items => { if (sourceID.value && !items.some(item => item.id === sourceID.value)) close() })
watch(filter, () => { offset.value = 0 })
async function readFile(event: Event) {
 const file = (event.target as HTMLInputElement).files?.[0]
 clearFile()
 if (!file) return
 if (file.size > 1024 * 1024) { fileError.value = '预期金额 CSV 不能超过 1 MiB，请按来源拆分。'; return }
 const token = generation, fileToken = fileGeneration
 reading.value = true
 try {
  const content = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
  if (!current(token) || fileToken !== fileGeneration) return
  csv.value = content; fileName.value = file.name
 } catch { if (current(token) && fileToken === fileGeneration) fileError.value = '无法读取预期金额文件，请使用 UTF-8 CSV。' }
 finally { if (current(token) && fileToken === fileGeneration) reading.value = false }
}
function template() {
 const data = 'payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan\r\n'
 const url = URL.createObjectURL(new Blob([data], { type: 'text/csv;charset=utf-8' }))
 const link = document.createElement('a'); link.href = url; link.download = 'xui-financial-reference-v1.csv'; link.click(); URL.revokeObjectURL(url)
}
async function reconcile() {
 if (saving.value || loading.value || reading.value || !props.canReconcile || !snapshot.value?.available) return
 fileError.value = ''; error.value = ''; notice.value = ''
 if (!csv.value || !fileName.value) { fileError.value = '请选择预期金额 CSV。'; return }
 const token = generation
 saving.value = true
 try {
  const result = await api<FinancialReport>(`${endpoint()}/reconcile`, { csv: csv.value })
  if (!current(token)) return
  report.value = result; selectedRun.value = result.run.id; offset.value = 0; filter.value = 'all'
  snapshot.value!.runs = [result.run, ...snapshot.value!.runs.filter(item => item.id !== result.run.id)].sort((a, b) => b.revision - a.revision).slice(0, 20)
  clearFile()
  notice.value = '已生成或读取相同预期金额的核对报告。来源和历史报告保留，收款、退款及权益状态未改变。'
 } catch (e) { if (current(token)) fileError.value = e instanceof Error ? e.message : '预期金额暂时无法核对，请稍后重试。' }
 finally { if (current(token)) saving.value = false }
}
async function changeRun(event: Event) {
 const id = (event.target as HTMLSelectElement).value
 ;(event.target as HTMLSelectElement).value = selectedRun.value
 if (!id || loading.value || saving.value) return
 const token = generation
 loading.value = true; error.value = ''; notice.value = ''
 try {
  const result = await api<FinancialReport>(`${endpoint()}/runs/${encodeURIComponent(id)}`)
  if (current(token)) { report.value = result; selectedRun.value = id; offset.value = 0; filter.value = 'all' }
 } catch (e) { if (current(token)) error.value = e instanceof Error ? e.message : '历史核对报告暂时无法读取。' }
 finally { if (current(token)) loading.value = false }
}
function observations() { report.value = null; selectedRun.value = ''; filter.value = 'all'; offset.value = 0; notice.value = '' }
onBeforeUnmount(() => { alive = false; reset() })
</script>

<template>
 <section class="financial-panel" data-testid="statement-financials">
  <div class="financial-heading"><div><h3>手续费与渠道金额</h3><p class="muted">按原始来源核对 5 位小数金额。各渠道字段保持原口径，账单金额不代表银行已到账。</p></div><NButton v-if="!open" :disabled="!sources.length" @click="show">手续费与渠道金额核对</NButton><NButton v-else @click="close">收起费用核对</NButton></div>
  <p v-if="!sources.length" class="muted">暂无来源记录，可重新导入原始账单补充费用观察。</p>
  <template v-if="open">
   <label class="financial-source">费用观察来源<select v-model="sourceID" aria-label="费用观察来源"><option v-for="(source, index) in sources" :key="source.id" :value="source.id">{{ index + 1 }} · {{ sourceFormat(source.format) }} · {{ source.kind === 'download' ? '下载' : '上传' }} · {{ time(source.created_at) }} · {{ source.id.slice(0, 12) }}</option></select></label>
   <p class="muted financial-reference">当前来源 {{ sourceID }}</p>
   <div v-if="error" class="alert error" role="alert">{{ error }} <NButton size="small" :disabled="saving" @click="load">重读费用观察</NButton></div>
   <div v-if="notice" class="alert success" role="status">{{ notice }}</div>
   <p v-if="loading" role="status" class="muted">正在读取费用观察或历史报告…</p>
   <p v-if="snapshot && !snapshot.available" role="status" class="alert financial-warning">此来源尚无手续费和渠道金额观察，不能按零处理。标准 CSV 不包含这些字段；历史原始来源可重新导入同一文件补充观察。</p>
   <template v-if="snapshot?.available">
    <p class="alert financial-warning">本来源的渠道金额指“{{ basis }}”，与毛额、银行结算到账金额分别核对。手续费保留文件原符号，不自动套用毛额减手续费公式。</p>
    <div v-if="codes.length" class="alert financial-warning" role="alert"><strong>来源文件汇总待核查</strong><p v-for="item in codes" :key="item">{{ code(item) }}</p><p>即使逐条预期字段匹配，文件汇总差异仍需单独调查。</p></div>
    <div class="financial-totals"><div><span>手续费明细合计</span><strong>{{ money(snapshot.fee_total_yuan) }}</strong><small>文件汇总 {{ money(snapshot.declared_fee_total_yuan) }}</small></div><div><span>{{ basis }}明细合计</span><strong>{{ money(snapshot.channel_amount_total_yuan) }}</strong><small>文件汇总 {{ money(snapshot.declared_channel_amount_total_yuan) }}</small></div></div>
    <form v-if="canReconcile" class="financial-import" @submit.prevent="reconcile">
     <h4>上传明确预期金额</h4><p class="muted">请按商户合同或独立核算结果填写预期值。两项金额都必须明确填写，未知不能留空当作零；最多 5 位小数、1 MiB、2000 条记录。预期值是人工参考，不是渠道资金证明。</p>
     <NButton size="small" :disabled="saving" @click="template">下载预期金额模板</NButton>
     <label>预期金额 CSV<input ref="fileInput" type="file" accept=".csv,text/csv" aria-label="预期金额 CSV" :disabled="saving || loading || reading" @change="readFile" /></label>
     <p v-if="fileName" class="muted financial-reference">已选择预期文件 {{ fileName }}</p>
     <div v-if="fileError" class="alert error" role="alert">{{ fileError }}</div>
     <NButton type="primary" attr-type="submit" :loading="saving || reading" :disabled="!csv || loading">核对预期金额</NButton>
    </form>
    <p v-else class="muted">当前权限仅可查看来源观察与已保存报告。</p>
    <div class="financial-tools"><label v-if="snapshot.runs.length">费用核对版本<select :value="selectedRun" aria-label="费用核对版本" :disabled="loading || saving" @change="changeRun"><option value="">选择已保存的核对报告</option><option v-for="version in snapshot.runs" :key="version.id" :value="version.id">第 {{ version.revision }} 版 · {{ time(version.created_at) }}</option></select></label><NButton v-if="report" size="small" :disabled="loading || saving" @click="observations">查看来源明细</NButton><label v-if="report">费用核对筛选<select v-model="filter" aria-label="费用核对筛选"><option value="all">全部记录</option><option value="difference">存在差异</option><option value="unassessed">尚未核对</option><option value="matched">预期字段已匹配</option></select></label></div>
    <template v-if="report">
     <div class="financial-counts"><span>预期字段已匹配 {{ report.run.matched_count }}</span><span>存在差异 {{ report.run.difference_count }}</span><span>尚未核对 {{ report.run.unassessed_count }}</span></div>
     <p class="muted">第 {{ report.run.revision }} 版报告固定保存，来源 {{ report.run.observed_row_count }} 条，预期 {{ report.run.reference_row_count }} 条。缺少预期的来源记录属于尚未核对。</p>
     <p class="muted financial-reference">预期手续费合计 {{ money(report.run.expected_fee_total_yuan) }} · 预期{{ basis }}合计 {{ money(report.run.expected_channel_amount_total_yuan) }}</p>
     <div v-if="reportPage.length" class="table-wrap"><table class="financial-table"><thead><tr><th>支付 / 退款编号</th><th>结果</th><th>手续费</th><th>{{ basis }}</th></tr></thead><tbody><tr v-for="item in reportPage" :key="`${item.payment_id}/${item.refund_id}`" :data-financial-row="item.refund_id || item.payment_id"><td><strong class="financial-reference">{{ item.payment_id }}</strong><small v-if="item.expected_payment_id && item.expected_payment_id !== item.payment_id" class="financial-detail financial-reference">预期支付 {{ item.expected_payment_id }}</small><small v-if="item.refund_id" class="financial-detail financial-reference">退款 {{ item.refund_id }}</small></td><td><NTag size="small" :type="item.status === 'matched' ? 'info' : 'warning'">{{ status(item.status) }}</NTag><small v-for="reason in item.codes" :key="reason" class="financial-detail">{{ code(reason) }}</small></td><td><span class="financial-detail">来源 {{ money(item.observed_fee_yuan) }}</span><span class="financial-detail">预期 {{ money(item.expected_fee_yuan) }}</span></td><td><span class="financial-detail">来源 {{ money(item.observed_channel_amount_yuan) }}</span><span class="financial-detail">预期 {{ money(item.expected_channel_amount_yuan) }}</span></td></tr></tbody></table></div>
     <p v-else class="muted">暂无符合筛选条件的核对记录。</p>
    </template>
    <template v-else>
     <p class="muted">以下仅展示来源金额，尚未与预期金额核对。未提供的字段显示为“未提供”。</p>
     <div v-if="observationPage.length" class="table-wrap"><table class="financial-table"><thead><tr><th>支付 / 退款编号</th><th>手续费</th><th>{{ basis }}</th></tr></thead><tbody><tr v-for="item in observationPage" :key="`${item.payment_id}/${item.refund_id}`" :data-financial-row="item.refund_id || item.payment_id"><td><strong class="financial-reference">{{ item.payment_id }}</strong><small v-if="item.refund_id" class="financial-detail financial-reference">退款 {{ item.refund_id }}</small></td><td>{{ money(item.fee_yuan) }}</td><td>{{ money(item.channel_amount_yuan) }}</td></tr></tbody></table></div>
     <p v-else class="muted">此来源明确记录零条所选类型明细。</p>
    </template>
    <div class="financial-pagination"><span class="muted">费用记录第 {{ start }}–{{ end }} 项，共 {{ totalRows }} 项</span><div class="financial-actions"><NButton size="small" :disabled="offset === 0 || loading || saving" @click="offset = Math.max(0, offset - 100)">上一页费用记录</NButton><NButton size="small" :disabled="offset + 100 >= totalRows || loading || saving" @click="offset += 100">下一页费用记录</NButton></div></div>
   </template>
  </template>
 </section>
</template>

<style scoped>
.financial-panel{margin-top:24px;border-top:1px solid #edf0e7;padding-top:22px}.financial-heading{display:flex;justify-content:space-between;align-items:flex-start;gap:16px}.financial-heading h3{margin:0;font-size:17px;font-weight:550}.financial-source{display:block;max-width:900px}.financial-reference{overflow-wrap:anywhere;white-space:normal;font-family:monospace}.financial-warning{background:#f7f2df;color:#817443;font-size:12px;line-height:1.8}.financial-warning p{margin:6px 0}.financial-totals{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin:16px 0}.financial-totals>div{background:#f5f7f1;border-radius:9px;padding:16px;min-width:0}.financial-totals span,.financial-totals small{display:block;font-size:11px;color:#81907e}.financial-totals strong{display:block;overflow-wrap:anywhere;margin:10px 0;font-size:17px;font-weight:550}.financial-import{border:1px solid #edf0e7;border-radius:9px;padding:16px;margin:20px 0}.financial-import h4{margin:0;font-size:14px}.financial-import label{display:block;margin-top:14px}.financial-tools{display:flex;gap:14px;align-items:flex-end;flex-wrap:wrap}.financial-tools label{min-width:180px;max-width:100%}.financial-counts{display:flex;gap:18px;flex-wrap:wrap;margin:18px 0;font-size:12px}.financial-detail{display:block;line-height:1.8;margin-top:6px;font-size:11px}.financial-table td{vertical-align:top;min-width:140px}.financial-table td:first-child{max-width:220px}.financial-pagination{display:flex;align-items:center;justify-content:space-between;gap:14px;flex-wrap:wrap;margin-top:16px}.financial-actions{display:flex;gap:8px;flex-wrap:wrap}select,input[type=file]{display:block;width:100%;min-width:0;margin-top:8px;border:1px solid #d9e1d4;border-radius:8px;background:#fff;padding:10px 12px;color:#3d5340;font:inherit;font-size:12px}select:focus,input:focus{outline:2px solid #7dab91;outline-offset:2px}@media(max-width:760px){.financial-heading{flex-direction:column}.financial-totals{grid-template-columns:1fr}.financial-tools{align-items:flex-start;flex-direction:column;width:100%}.financial-tools label{width:100%;min-width:0}.financial-table td{min-width:130px}.financial-pagination{align-items:flex-start}}
</style>
