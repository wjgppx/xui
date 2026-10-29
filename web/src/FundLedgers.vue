<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NModal, NTag } from 'naive-ui'
import { api, type FundLedger, type FundLedgerSummary, type FundReconciliation, type FundReconciliationSummary } from './api'

const props = defineProps<{ canImport: boolean; canReconcile: boolean }>()
const ledgers = ref<FundLedgerSummary[]>([]), reports = ref<FundReconciliationSummary[]>([]), listBusy = ref(false), error = ref(''), notice = ref('')
const ledger = ref<FundLedger | null>(null), ledgerBusy = ref(false), ledgerError = ref(''), ledgerOffset = ref(0)
const report = ref<FundReconciliation | null>(null), reportBusy = ref(false), reportError = ref(''), reportOffset = ref(0), reportFilter = ref('all')
const merchantID = ref(''), bankID = ref(''), importOpen = ref(false), saving = ref(false), reading = ref(false), importError = ref('')
const provider = ref<'alipay' | 'wechat' | 'bank'>('alipay'), accountKey = ref(''), day = ref(shanghaiDay()), coverage = ref<'partial' | 'full'>('partial'), opening = ref('0'), closing = ref('0'), fullConfirmed = ref(false)
const fileInput = ref<HTMLInputElement | null>(null), fileName = ref(''), csv = ref('')
let alive = true, ledgerGeneration = 0, reportGeneration = 0, fileGeneration = 0, importGeneration = 0
function shanghaiDay() { return new Date(Date.now() + 8 * 3600 * 1000).toISOString().slice(0, 10) }
const providerLabel = (value: string) => ({ alipay: '支付宝', wechat: '微信支付', bank: '银行' }[value] || value)
const coverageLabel = (value: string) => value === 'full' ? '声明完整日范围' : '部分范围'
const time = (value: string) => new Date(value).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })
const kindLabel = (value: string) => ({ payment: '收款', refund: '退款', fee: '费用', withdrawal: '提现 / 支出', deposit: '存入', settlement: '银行入账', adjustment: '调整', other: '其他' }[value] || value)
const code = (value: string) => ({ running_balance_mismatch: '累计推算余额与记录不一致', step_balance_mismatch: '上笔记录余额加本笔变动后不一致', closing_balance_mismatch: '期末声明与期初加净变动不一致', closing_entry_mismatch: '末笔余额与期末声明不一致', bank_record_missing: '本次银行范围未列出（需核对跨日）', merchant_record_missing: '本次商户范围未列出', duplicate_transfer_id: '同一引用存在多条记录，未自动配对', amount_mismatch: '金额不一致，未自动扣除手续费', merchant_partial: '商户批次声明为部分范围', bank_partial: '银行批次声明为部分范围', merchant_balance_issues: '商户批次存在余额差异', bank_balance_issues: '银行批次存在余额差异' }[value] || '待核查项目')
const resultLabel = (value: string) => ({ matched: '观察记录相符', difference: '金额有差异', unmatched: '范围内缺少对应记录', ambiguous: '引用重复需核查' }[value] || value)
function money(value: string) {
 const parts = /^(-?)(\d+)\.(\d{5})$/.exec(value)
 return parts ? `${parts[1]}¥${parts[2]!.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}.${parts[3]}` : '金额格式不可用'
}
const merchantBatches = computed(() => ledgers.value.filter(item => item.provider !== 'bank'))
const bankBatches = computed(() => ledgers.value.filter(item => item.provider === 'bank'))
const ledgerPage = computed(() => ledger.value?.rows.slice(ledgerOffset.value, ledgerOffset.value + 100) || [])
const filteredReport = computed(() => report.value?.rows.filter(row => reportFilter.value === 'all' || row.status === reportFilter.value) || [])
const reportPage = computed(() => filteredReport.value.slice(reportOffset.value, reportOffset.value + 100))
const batchLabel = (item: FundLedgerSummary) => `${item.account_key} · ${providerLabel(item.provider)} · ${item.date} · ${coverageLabel(item.coverage)} · ${item.id.slice(0, 12)}`
async function refresh() {
 if (listBusy.value) return
 listBusy.value = true; error.value = ''
 const results = await Promise.allSettled([api<{ items: FundLedgerSummary[] }>('/admin/fund-ledgers'), api<{ items: FundReconciliationSummary[] }>('/admin/fund-reconciliations')])
 if (!alive) return
 if (results[0].status === 'fulfilled') { ledgers.value = results[0].value.items; if (!merchantID.value) merchantID.value = merchantBatches.value[0]?.id || ''; if (!bankID.value) bankID.value = bankBatches.value[0]?.id || '' }
 if (results[1].status === 'fulfilled') reports.value = results[1].value.items
 error.value = results.filter(result => result.status === 'rejected').map(result => (result as PromiseRejectedResult).reason instanceof Error ? (result as PromiseRejectedResult).reason.message : '资金流水列表暂时无法读取。').join('；')
 listBusy.value = false
}
async function showLedger(id: string) {
 const token = ++ledgerGeneration
 ledgerBusy.value = true; ledgerError.value = ''; ledger.value = null; ledgerOffset.value = 0
 try { const item = await api<FundLedger>(`/admin/fund-ledgers/${encodeURIComponent(id)}`); if (alive && token === ledgerGeneration) ledger.value = item }
 catch (e) { if (alive && token === ledgerGeneration) ledgerError.value = e instanceof Error ? e.message : '流水批次暂时无法读取。' }
 finally { if (alive && token === ledgerGeneration) ledgerBusy.value = false }
}
function closeLedger() { ledgerGeneration++; ledger.value = null; ledgerError.value = ''; ledgerBusy.value = false; ledgerOffset.value = 0 }
async function showReport(id: string) {
 const token = ++reportGeneration
 reportBusy.value = true; reportError.value = ''; report.value = null; reportOffset.value = 0; reportFilter.value = 'all'
 try { const item = await api<FundReconciliation>(`/admin/fund-reconciliations/${encodeURIComponent(id)}`); if (alive && token === reportGeneration) report.value = item }
 catch (e) { if (alive && token === reportGeneration) reportError.value = e instanceof Error ? e.message : '核对报告暂时无法读取。' }
 finally { if (alive && token === reportGeneration) reportBusy.value = false }
}
function closeReport() { reportGeneration++; report.value = null; reportError.value = ''; reportBusy.value = false; reportOffset.value = 0 }
async function reconcile() {
 if (reportBusy.value || !props.canReconcile || !merchantID.value || !bankID.value) return
 const token = ++reportGeneration
 reportBusy.value = true; reportError.value = ''; notice.value = ''
 try {
  const item = await api<FundReconciliation>('/admin/fund-reconciliations', { merchant_batch_id: merchantID.value, bank_batch_id: bankID.value })
  if (!alive || token !== reportGeneration) return
  report.value = item; reportOffset.value = 0; reportFilter.value = 'all'
  const { rows: _rows, ...metadata } = item
  reports.value = [metadata, ...reports.value.filter(existing => existing.id !== item.id)].slice(0, 100)
  notice.value = '已保存或读取相同双方批次的观察核对报告。记录相符不代表银行已确认到账。'
 } catch (e) { if (alive && token === reportGeneration) reportError.value = e instanceof Error ? e.message : '资金记录暂时无法核对。' }
 finally { if (alive && token === reportGeneration) reportBusy.value = false }
}
function clearFile() { fileGeneration++; fileName.value = ''; csv.value = ''; reading.value = false; if (fileInput.value) fileInput.value.value = '' }
function clearImport() { importGeneration++; clearFile(); importError.value = ''; fullConfirmed.value = false }
function openImport() { if (!props.canImport) return; clearImport(); provider.value = 'alipay'; accountKey.value = ''; day.value = shanghaiDay(); coverage.value = 'partial'; opening.value = '0'; closing.value = '0'; importOpen.value = true }
watch(importOpen, value => { if (!value) clearImport() })
watch([provider, accountKey, day, coverage, opening, closing], () => { fullConfirmed.value = false })
watch(reportFilter, () => { reportOffset.value = 0 })
watch([merchantID, bankID], closeReport)
async function readFile(event: Event) {
 const file = (event.target as HTMLInputElement).files?.[0]
 clearFile(); importError.value = ''; fullConfirmed.value = false
 if (!file) return
 if (file.size > 1024 * 1024) { importError.value = '流水 CSV 不能超过 1 MiB，请按账户和日期拆分。'; return }
 const token = fileGeneration, formToken = importGeneration
 reading.value = true
 try {
  const content = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer())
  if (!alive || !importOpen.value || token !== fileGeneration || formToken !== importGeneration) return
  csv.value = content; fileName.value = file.name
 } catch { if (alive && token === fileGeneration) importError.value = '无法读取流水文件，请使用 UTF-8 CSV。' }
 finally { if (alive && token === fileGeneration) reading.value = false }
}
function template() {
 const data = 'sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\r\n'
 const url = URL.createObjectURL(new Blob([data], { type: 'text/csv;charset=utf-8' }))
 const link = document.createElement('a'); link.href = url; link.download = 'xui-fund-ledger-v1.csv'; link.click(); URL.revokeObjectURL(url)
}
async function importFile() {
 if (saving.value || reading.value || !props.canImport) return
 importError.value = ''
 if (!/^[a-z][a-z0-9_-]{0,63}$/.test(accountKey.value)) { importError.value = '请填写以小写字母开头的账户别名，最多 64 个小写字母、数字、下划线或短横线。'; return }
 if (!fileName.value || !csv.value) { importError.value = '请选择非空的标准流水 CSV。'; return }
 if (coverage.value === 'full' && !fullConfirmed.value) { importError.value = '请确认文件范围完整，或选择部分范围。'; return }
 if (![opening.value, closing.value].every(value => /^-?(0|[1-9]\d{0,24})(\.\d{1,5})?$/.test(value))) { importError.value = '期初和期末金额须明确填写，最多 25 位整数和 5 位小数，不使用千分位或科学计数法。'; return }
 const token = importGeneration
 saving.value = true; notice.value = ''
 try {
  const item = await api<FundLedger>('/admin/fund-ledgers', { provider: provider.value, account_key: accountKey.value, date: day.value, coverage: coverage.value, opening_balance_yuan: opening.value, closing_balance_yuan: closing.value, csv: csv.value })
  if (!alive || token !== importGeneration) return
  const { rows: _rows, ...metadata } = item
  ledgers.value = [metadata, ...ledgers.value.filter(existing => existing.id !== item.id)].slice(0, 100)
  ledgerGeneration++; ledger.value = item; ledgerOffset.value = 0; ledgerBusy.value = false; ledgerError.value = ''
  if (item.provider === 'bank') bankID.value = item.id; else merchantID.value = item.id
  importOpen.value = false
  notice.value = '已保存或读取相同流水批次。余额差异按原记录保留；期末观察不代表当前账户余额。'
 } catch (e) { if (alive && token === importGeneration) importError.value = e instanceof Error ? e.message : '流水导入失败，请核对文件。' }
 finally { if (alive) saving.value = false }
}
onMounted(refresh)
onBeforeUnmount(() => { alive = false; clearImport(); ledgerGeneration++; reportGeneration++ })
</script>

<template>
 <div class="page-heading funds-heading"><div><p class="eyebrow">FUND FLOW OBSERVATIONS</p><h1>资金流水账本</h1><p>独立保存商户和银行流水范围，核对批内余额及明确转账引用。</p></div><div class="funds-actions"><NButton :loading="listBusy" @click="refresh">刷新资金流水</NButton><NButton v-if="canImport" type="primary" @click="openImport">导入资金流水</NButton></div></div>
 <p class="alert funds-warning">手工导入仅为资金观察，不是银行或渠道确认。重叠批次分别保留，不累加为当前余额，也不会发起提现、确认收款或修改权益。</p>
 <div v-if="error" class="alert error" role="alert">{{ error }}</div><div v-if="notice" class="alert success" role="status">{{ notice }}</div>
 <section class="panel funds-panel" data-testid="fund-ledger-list"><div class="section-title"><h2>已导入流水批次</h2><span class="muted">最近 100 批</span></div>
  <div v-if="ledgers.length" class="table-wrap"><table><thead><tr><th>账户 / 日期</th><th>观察范围</th><th>期末声明 / 推算</th><th>余额检查</th><th>查看</th></tr></thead><tbody><tr v-for="item in ledgers" :key="item.id" :data-ledger-id="item.id"><td><strong>{{ item.account_key }}</strong><small class="funds-detail">{{ providerLabel(item.provider) }} · {{ item.date }}</small></td><td>{{ coverageLabel(item.coverage) }}<small class="funds-detail">{{ item.row_count }} 条 · {{ time(item.created_at) }}</small></td><td><span class="funds-detail">声明 {{ money(item.closing_balance_yuan) }}</span><span class="funds-detail">批内推算 {{ money(item.expected_closing_balance_yuan) }}</span></td><td><NTag size="small" :type="item.balance_difference_count || item.summary_codes.length ? 'warning' : 'info'">{{ item.balance_difference_count || item.summary_codes.length ? '存在余额差异' : '批内算术相符' }}</NTag></td><td><NButton size="small" @click="showLedger(item.id)">查看流水批次</NButton></td></tr></tbody></table></div><p v-else class="muted">暂无流水批次。请先整理为按账户、日期拆分的标准 CSV。</p>
 </section>
 <section v-if="ledgerBusy || ledgerError || ledger" class="panel funds-panel" data-testid="fund-ledger-detail"><div class="section-title"><h2>批内余额观察</h2><NButton size="small" @click="closeLedger">收起流水批次</NButton></div><p v-if="ledgerBusy" role="status" class="muted">正在读取流水批次…</p><div v-if="ledgerError" class="alert error" role="alert">{{ ledgerError }}</div><template v-if="ledger">
  <p><strong>{{ ledger.account_key }}</strong> · {{ providerLabel(ledger.provider) }} · {{ ledger.date }} · {{ coverageLabel(ledger.coverage) }}</p><p class="muted">以下期初、期末与逐行余额来自所选文件。算术自洽不能证明文件完整或当前账户余额。</p>
  <div class="funds-totals"><div><span>期初声明</span><strong>{{ money(ledger.opening_balance_yuan) }}</strong></div><div><span>期末声明</span><strong>{{ money(ledger.closing_balance_yuan) }}</strong></div><div><span>期初加净变动推算</span><strong>{{ money(ledger.expected_closing_balance_yuan) }}</strong></div></div>
  <p class="muted">贷记合计 {{ money(ledger.total_credit_yuan) }} · 借记合计 {{ money(ledger.total_debit_yuan) }} · 有差异明细 {{ ledger.balance_difference_count }} 条</p><div v-if="ledger.summary_codes.length" class="alert funds-warning" role="alert"><p v-for="item in ledger.summary_codes" :key="item">{{ code(item) }}</p></div>
  <div v-if="ledgerPage.length" class="table-wrap"><table class="funds-table"><thead><tr><th>顺序 / 标识 / 时间</th><th>方向 / 金额</th><th>报告余额 / 推算</th><th>检查 / 转账引用</th></tr></thead><tbody><tr v-for="item in ledgerPage" :key="item.entry_id" :data-entry-id="item.entry_id"><td><strong>{{ item.sequence }} · {{ item.entry_id }}</strong><small class="funds-detail">{{ time(item.occurred_at) }}（北京时间）</small><small class="funds-detail">{{ kindLabel(item.kind) }}</small></td><td>{{ item.direction === 'credit' ? '贷记 +' : '借记 −' }} {{ money(item.amount_yuan) }}</td><td><span class="funds-detail">记录 {{ money(item.balance_after_yuan) }}</span><span class="funds-detail">累计推算 {{ money(item.expected_balance_yuan) }}</span><span class="funds-detail">上笔加变动 {{ money(item.step_expected_balance_yuan) }}</span></td><td><small v-for="reason in item.codes" :key="reason" class="funds-detail">{{ code(reason) }}</small><small v-if="!item.codes.length" class="funds-detail">本行算术相符</small><span v-if="item.transfer_id" class="funds-detail funds-reference">引用 {{ item.transfer_id }} · 对方 {{ item.counterparty_account_key }}</span></td></tr></tbody></table></div><p v-else class="muted">此批次明确声明零条流水，仍不能据此证明完整。</p>
  <div class="funds-pagination"><span class="muted">流水第 {{ ledger.rows.length ? ledgerOffset + 1 : 0 }}–{{ Math.min(ledgerOffset + 100, ledger.rows.length) }} 项，共 {{ ledger.rows.length }} 项</span><div class="funds-actions"><NButton size="small" :disabled="!ledgerOffset" @click="ledgerOffset = Math.max(0, ledgerOffset - 100)">上一页流水</NButton><NButton size="small" :disabled="ledgerOffset + 100 >= ledger.rows.length" @click="ledgerOffset += 100">下一页流水</NButton></div></div><details class="funds-source"><summary>批次标识与摘要</summary><p class="funds-reference">{{ ledger.id }}</p><p class="funds-reference">{{ ledger.digest }}</p></details>
 </template></section>
 <section class="panel funds-panel" data-testid="fund-pair-form"><div class="section-title"><h2>商户提现与银行记录核对</h2></div><p class="muted">明确选择双方账户和批次，可以选择不同日期。仅比较双方声明指向对方账户的提现 / 入账，按转账引用及精确金额核对，不按相似时间或金额猜测，不自动扣费。</p>
  <form v-if="canReconcile" @submit.prevent="reconcile"><div class="funds-form-grid"><label>商户提现批次<select v-model="merchantID" aria-label="商户提现批次" :disabled="reportBusy"><option value="">请选择商户批次</option><option v-for="item in merchantBatches" :key="item.id" :value="item.id">{{ batchLabel(item) }}</option></select></label><label>银行入账批次<select v-model="bankID" aria-label="银行入账批次" :disabled="reportBusy"><option value="">请选择银行批次</option><option v-for="item in bankBatches" :key="item.id" :value="item.id">{{ batchLabel(item) }}</option></select></label></div><NButton type="primary" attr-type="submit" :loading="reportBusy" :disabled="!merchantID || !bankID">核对所选资金记录</NButton></form><p v-else class="muted">当前权限仅可读取流水观察与历史报告。</p>
  <div v-if="reportError" class="alert error" role="alert">{{ reportError }}</div>
 </section>
 <section class="panel funds-panel" data-testid="fund-report-list"><div class="section-title"><h2>历史资金核对报告</h2><span class="muted">最近 100 份</span></div><div v-if="reports.length" class="table-wrap"><table><thead><tr><th>选定账户方向</th><th>观察比较</th><th>保存时间</th><th>查看</th></tr></thead><tbody><tr v-for="item in reports" :key="item.id" :data-fund-report-id="item.id"><td><strong>{{ item.merchant_account_key }} → {{ item.bank_account_key }}</strong></td><td>相符 {{ item.matched_count }} · 差异 {{ item.difference_count }} · 缺记录 {{ item.unmatched_count }} · 重复引用 {{ item.ambiguous_count }}</td><td>{{ time(item.created_at) }}</td><td><NButton size="small" @click="showReport(item.id)">查看资金核对报告</NButton></td></tr></tbody></table></div><p v-else class="muted">暂无资金核对报告。保存相同双方批次会复用原报告，不叠加为已结算资金。</p></section>
 <section v-if="report" class="panel funds-panel" data-testid="fund-report"><div class="section-title"><h2>资金观察核对结果</h2><NButton size="small" @click="closeReport">收起资金核对报告</NButton></div><p><strong>{{ report.merchant_account_key }} → {{ report.bank_account_key }}</strong></p><p class="muted funds-reference">商户批次 {{ report.merchant_batch_id }} · 银行批次 {{ report.bank_batch_id }}</p><p class="alert funds-warning">观察记录相符不代表银行已确认到账。重复或修正批次可用于不同历史报告，报告不消耗资金，也不建立唯一结算关系。</p><div v-if="report.source_warnings.length" class="alert funds-warning" role="alert"><p v-for="item in report.source_warnings" :key="item">{{ code(item) }}</p></div>
  <div class="funds-counts"><span>观察相符 {{ report.matched_count }}</span><span>金额差异 {{ report.difference_count }}</span><span>范围缺记录 {{ report.unmatched_count }}</span><span>引用重复 {{ report.ambiguous_count }}</span></div><p class="muted">范围内候选：商户 {{ report.merchant_candidate_count }} 条、银行 {{ report.bank_candidate_count }} 条。其他账户方向未核对：商户 {{ report.merchant_out_of_scope_count }} 条、银行 {{ report.bank_out_of_scope_count }} 条；普通流水：商户 {{ report.merchant_other_count }} 条、银行 {{ report.bank_other_count }} 条。</p>
  <label class="funds-filter">资金核对筛选<select v-model="reportFilter" aria-label="资金核对筛选"><option value="all">全部引用</option><option value="matched">观察记录相符</option><option value="difference">金额有差异</option><option value="unmatched">范围内缺少对应记录</option><option value="ambiguous">引用重复需核查</option></select></label>
  <div v-if="reportPage.length" class="table-wrap"><table class="funds-table"><thead><tr><th>转账引用 / 结果</th><th>商户提现观察</th><th>银行入账观察</th></tr></thead><tbody><tr v-for="group in reportPage" :key="group.transfer_id" :data-transfer-id="group.transfer_id"><td><strong class="funds-reference">{{ group.transfer_id }}</strong><small class="funds-detail"><NTag size="small" :type="group.status === 'matched' ? 'info' : 'warning'">{{ resultLabel(group.status) }}</NTag></small><small v-for="reason in group.codes" :key="reason" class="funds-detail">{{ code(reason) }}</small></td><td><p v-for="entry in group.merchant_entries" :key="entry.entry_id" class="funds-entry"><strong class="funds-reference">{{ entry.entry_id }}</strong><span>{{ money(entry.amount_yuan) }}</span><small>{{ time(entry.occurred_at) }}（北京时间）</small></p><span v-if="!group.merchant_entries.length" class="muted">本次范围未列出</span></td><td><p v-for="entry in group.bank_entries" :key="entry.entry_id" class="funds-entry"><strong class="funds-reference">{{ entry.entry_id }}</strong><span>{{ money(entry.amount_yuan) }}</span><small>{{ time(entry.occurred_at) }}（北京时间）</small></p><span v-if="!group.bank_entries.length" class="muted">本次范围未列出</span></td></tr></tbody></table></div><p v-else-if="!report.rows.length" class="alert funds-warning" role="status">选定账户方向没有可核对的提现 / 入账观察，不能判定银行已到账。</p><p v-else class="muted">暂无符合筛选条件的引用。</p>
  <div class="funds-pagination"><span class="muted">核对引用第 {{ filteredReport.length ? reportOffset + 1 : 0 }}–{{ Math.min(reportOffset + 100, filteredReport.length) }} 项，共 {{ filteredReport.length }} 项</span><div class="funds-actions"><NButton size="small" :disabled="!reportOffset" @click="reportOffset = Math.max(0, reportOffset - 100)">上一页核对引用</NButton><NButton size="small" :disabled="reportOffset + 100 >= filteredReport.length" @click="reportOffset += 100">下一页核对引用</NButton></div></div>
 </section>
 <NModal v-model:show="importOpen" preset="card" title="导入资金流水观察" :mask-closable="!saving" :close-on-esc="!saving" style="width: 760px; max-width: calc(100vw - 32px)"><form @submit.prevent="importFile"><div v-if="importError" class="alert error" role="alert">{{ importError }}</div><p class="muted">请整理标准 CSV，最多 1 MiB、2000 条。账户别名为本平台范围标识，不填写银行卡号、商户密钥或凭据；同一别名首次导入后固定渠道。</p><div class="funds-form-grid"><label>流水来源<select v-model="provider" aria-label="流水来源" :disabled="saving"><option value="alipay">支付宝商户</option><option value="wechat">微信商户</option><option value="bank">银行账户</option></select></label><label>账户别名<input v-model="accountKey" aria-label="账户别名" placeholder="例如 alipay_main / bank_ops" required maxlength="64" :disabled="saving" /></label><label>流水日期（北京时间）<input v-model="day" type="date" aria-label="资金流水日期" min="2000-01-01" max="2099-12-31" required :disabled="saving" /></label><label>流水范围<select v-model="coverage" aria-label="资金流水范围" :disabled="saving"><option value="partial">部分范围</option><option value="full">声明完整日范围</option></select></label><label>期初声明（元）<input v-model="opening" aria-label="期初声明" required :disabled="saving" inputmode="decimal" /></label><label>期末声明（元）<input v-model="closing" aria-label="期末声明" required :disabled="saving" inputmode="decimal" /></label></div><label v-if="coverage === 'full'" class="funds-check"><input v-model="fullConfirmed" type="checkbox" aria-label="确认完整流水范围" :disabled="saving" />我确认已按该账户和日期提供完整日范围；此声明不等于渠道验证。</label><NButton size="small" :disabled="saving" @click="template">下载资金流水模板</NButton><label>标准资金流水 CSV<input ref="fileInput" type="file" accept=".csv,text/csv" aria-label="标准资金流水 CSV" :disabled="saving || reading" @change="readFile" /></label><p v-if="fileName" class="muted funds-reference">已选择流水文件 {{ fileName }}</p><p class="muted">sequence 按原流水顺序连续编号；金额为正数并明确 credit / debit，余额及金额最多 5 位小数。提现 / 银行入账须填写相同的明确转账引用和对方账户别名，其他记录对应字段留空。</p><div class="funds-actions"><NButton type="primary" attr-type="submit" :loading="saving || reading" :disabled="!csv || (coverage === 'full' && !fullConfirmed)">保存流水观察</NButton><NButton :disabled="saving" @click="importOpen = false">取消</NButton></div></form></NModal>
</template>

<style scoped>
.funds-heading{align-items:flex-start}.funds-actions{display:flex;align-items:center;gap:10px;flex-wrap:wrap}.funds-panel{margin-bottom:20px}.funds-warning{background:#f7f2df;color:#817443}.funds-warning p{margin:5px 0}.funds-detail{display:block;font-size:11px;line-height:1.8;margin-top:6px;white-space:normal;overflow-wrap:anywhere;max-width:300px}.funds-reference{font-family:monospace;overflow-wrap:anywhere;white-space:normal}.funds-form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:0 18px}.funds-totals{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:10px;margin:20px 0}.funds-totals>div{background:#f5f7f1;border-radius:9px;padding:16px;min-width:0}.funds-totals span{font-size:11px;color:#81907e}.funds-totals strong{display:block;font-size:17px;font-weight:550;overflow-wrap:anywhere;margin-top:12px}.funds-pagination{display:flex;justify-content:space-between;align-items:center;gap:15px;flex-wrap:wrap;margin-top:18px}.funds-source{margin-top:20px;font-size:11px;color:#81907e}.funds-filter{max-width:320px}.funds-counts{display:flex;gap:18px;flex-wrap:wrap;font-size:12px;margin:20px 0}.funds-table td{vertical-align:top;min-width:160px;max-width:330px;white-space:normal;overflow-wrap:anywhere}.funds-entry{display:grid;gap:6px;padding:0 0 12px;margin:0 0 12px;border-bottom:1px solid #eef1e8;white-space:normal;overflow-wrap:anywhere}.funds-entry strong{font-size:11px}.funds-entry small{font-size:10px;color:#81907e}select,input:not([type=checkbox]){display:block;width:100%;min-width:0;margin-top:8px;border:1px solid #d9e1d4;border-radius:8px;background:#fff;padding:10px 12px;color:#3d5340;font:inherit;font-size:12px}select:focus,input:focus{outline:2px solid #7dab91;outline-offset:2px}.funds-check{display:flex;gap:10px;align-items:flex-start;font-size:12px;line-height:1.8}.funds-check input{flex-shrink:0;margin-top:5px}@media(max-width:760px){.funds-heading{flex-direction:column}.funds-form-grid,.funds-totals{grid-template-columns:1fr}.funds-pagination{align-items:flex-start}.funds-counts{gap:10px}.section-title{gap:12px}}
</style>
