<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NButton, NInput, NModal, NSelect, NTag } from 'naive-ui'
import { api, type PaymentActivity, type PaymentOperation } from './api'

const props = defineProps<{ canRetry: boolean }>()
const recentActivity = ref<PaymentActivity[]>([])
const items = ref<PaymentOperation[]>([]), activities = ref<PaymentActivity[]>([]), activityOpen = ref(false), activePayment = ref('')
const provider = ref<string | null>(null), status = ref<string | null>(null), orderID = ref('')
const busy = ref(false), error = ref(''), notice = ref('')
const providers = [
 { label: '支付宝', value: 'alipay' }, { label: '微信支付', value: 'wechat' },
 { label: '支付宝（易支付）', value: 'epay_alipay' }, { label: '微信支付（易支付）', value: 'epay_wechat' }
]
const statuses = [{ label: '等待收银台', value: 'pending' }, { label: '等待付款确认', value: 'ready' }, { label: '已确认付款', value: 'paid' }, { label: '需要处理', value: 'manual_required' }]
const providerLabel = (value: string) => providers.find(p => p.value === value)?.label || value
const statusLabel = (value: string) => statuses.find(s => s.value === value)?.label || value
const orderLabel = (value: string) => ({ pending: '待支付', expired: '已超时', closed: '已关闭', paid: '已开通' }[value] || value)
const reconcileLabel = (value?: string | null) => ({ queued: '待核实', processing: '核实中', waiting: '等待下一次核实', paid: '已确认', manual_required: '需要处理', waiting_callback: '等待渠道通知' }[value || ''] || '等待渠道通知')
const sourceLabel = (value: string) => ({ create: '创建收银台', notify: '渠道通知', query: '主动核对', auto_query: '自动核实', close: '关闭订单', retry: '重新排队' }[value] || value)
const outcomeLabel = (value?: string) => ({ pending: '已核实，尚未付款', ready: '收银台已创建', not_paid: '已核实，尚未付款', retry_scheduled: '已安排重试', manual_required: '需要处理', paid: '付款已确认', gateway_error: '渠道暂时不可用', verification_error: '验签未通过', conflict: '交易信息不一致', provider_unavailable: '渠道未启用', storage_error: '存储暂时不可用', created: '收银台已创建', accepted: '已受理', success: '处理成功', rejected: '请求被拒绝', closed: '订单已关闭', queued: '已重新排队', invalid_signature: '验签未通过', retry_limit: '达到重试上限', observation_expired: '超出自动观察期限' }[value || ''] || value || '—')
const time = (value?: string | null) => value ? new Date(value).toLocaleString('zh-CN') : '—'
const canQueue = (item: PaymentOperation) => props.canRetry && (item.provider === 'alipay' || item.provider === 'wechat') && item.status !== 'paid' && item.reconcile_status !== 'processing' && item.reconcile_status !== 'paid'
async function refresh() {
 const query = new URLSearchParams()
 if (provider.value) query.set('provider', provider.value)
 if (status.value) query.set('status', status.value)
 if (orderID.value.trim()) query.set('order_id', orderID.value.trim())
 const [data, recent] = await Promise.all([api<{ items: PaymentOperation[] }>(`/admin/payments${query.size ? `?${query}` : ''}`), api<PaymentActivity[]>('/admin/payment-activity')])
 items.value = data.items; recentActivity.value = recent
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch (e) { error.value = e instanceof Error ? e.message : '暂时无法读取支付记录' } finally { busy.value = false }
}
function activity(item: PaymentOperation) { return action(async () => {
 activities.value = await api<PaymentActivity[]>(`/admin/payments/${encodeURIComponent(item.id)}/activity`)
 activePayment.value = item.id; activityOpen.value = true
}) }
function retry(item: PaymentOperation) {
 if (!canQueue(item) || !window.confirm('重新排队后，系统将向渠道核实付款结果。此操作不会直接将订单标记为已支付，是否继续？')) return
 return action(async () => {
  await api(`/admin/payments/${encodeURIComponent(item.id)}/retry`, {})
  await refresh(); notice.value = '已重新排队，等待渠道核实付款结果。'
 })
}
onMounted(() => action(refresh))
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">PAYMENT OPERATIONS</p><h1>支付流水</h1><p>查看付款核实进度、渠道交易和异常处理记录。</p></div><NButton :loading="busy" @click="action(refresh)">刷新支付流水</NButton></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <section class="panel payment-filter-panel">
  <form class="payment-filters" @submit.prevent="action(refresh)">
   <label>支付渠道<NSelect v-model:value="provider" :options="providers" clearable placeholder="全部渠道" :disabled="busy" :input-props="{ 'aria-label': '筛选支付渠道' }" aria-label="筛选支付渠道" data-testid="payment-provider-filter" /></label>
   <label>付款状态<NSelect v-model:value="status" :options="statuses" clearable placeholder="全部状态" :disabled="busy" :input-props="{ 'aria-label': '筛选付款状态' }" aria-label="筛选付款状态" data-testid="payment-status-filter" /></label>
   <label>订单编号<NInput v-model:value="orderID" clearable placeholder="输入完整订单编号" :disabled="busy" :input-props="{ 'aria-label': '筛选订单编号', maxlength: 128 }" /></label>
   <NButton type="primary" attr-type="submit" :loading="busy">筛选流水</NButton>
  </form>
 </section>
 <section class="panel">
  <div class="section-title"><h2>渠道支付记录</h2><span class="subtle">最近 100 条</span></div>
  <p class="muted">自动核实适用于支付宝和微信。易支付等待签名通知；重新排队只会核实渠道付款，不会直接开通权益。</p>
  <div v-if="items.length" class="table-wrap"><table><thead><tr><th>订单 / 账号</th><th>渠道 / 交易号</th><th>金额</th><th>付款 / 订单</th><th>核实进度</th><th>操作</th></tr></thead><tbody>
   <tr v-for="item in items" :key="item.id" :data-payment-id="item.id">
    <td><strong>{{ item.order_id.slice(0, 12) }}</strong><small class="payment-detail">{{ item.user_email }}</small><small class="payment-detail">创建 {{ time(item.created_at) }}</small></td>
    <td><strong>{{ providerLabel(item.provider) }}</strong><small class="payment-detail payment-reference">{{ item.transaction_id || '尚无渠道交易号' }}</small><small class="payment-detail">支付记录 {{ item.id.slice(0, 12) }}</small></td>
    <td>{{ item.currency === 'CNY' ? '¥' : item.currency }}{{ (item.amount_minor / 100).toFixed(2) }}</td>
    <td><NTag size="small" round :type="item.status === 'paid' ? 'success' : 'warning'">{{ statusLabel(item.status) }}</NTag><small class="payment-detail">订单{{ orderLabel(item.order_status) }}</small><small v-if="item.paid_at" class="payment-detail">确认 {{ time(item.paid_at) }}</small></td>
    <td><NTag size="small" :type="item.reconcile_status === 'manual_required' ? 'error' : item.reconcile_status === 'paid' ? 'success' : 'default'">{{ reconcileLabel(item.reconcile_status) }}</NTag><small v-if="item.last_outcome" class="payment-detail">{{ outcomeLabel(item.last_outcome) }}</small><small v-if="item.reconcile_status" class="payment-detail">连续失败 {{ item.reconcile_attempts || 0 }} 次</small><small v-if="item.next_check_at && item.reconcile_status !== 'manual_required' && item.reconcile_status !== 'paid'" class="payment-detail">下次 {{ time(item.next_check_at) }}</small></td>
    <td><div class="payment-row-actions"><NButton size="small" :disabled="busy" @click="activity(item)">处理记录</NButton><NButton v-if="canQueue(item)" size="small" :disabled="busy" @click="retry(item)">重新排队核实</NButton></div></td>
   </tr>
  </tbody></table></div>
  <p v-else class="muted">暂无符合条件的支付记录。</p>
 </section>
 <section class="panel recent-payment-activity">
  <div class="section-title"><h2>最近处理记录</h2><span class="subtle">最近 100 条</span></div>
  <p class="muted">记录通知和核实结果。验签失败等未能关联订单的请求也显示在这里，便于排查。</p>
  <div v-if="recentActivity.length" class="table-wrap"><table><thead><tr><th>支付记录 / 订单</th><th>渠道</th><th>来源</th><th>结果</th><th>时间</th></tr></thead><tbody><tr v-for="item in recentActivity" :key="item.id"><td><strong>{{ item.payment_id ? item.payment_id.slice(0, 12) : '未绑定支付记录' }}</strong><small v-if="item.order_id" class="payment-detail">订单 {{ item.order_id.slice(0, 12) }}</small></td><td>{{ item.provider ? providerLabel(item.provider) : '—' }}</td><td>{{ sourceLabel(item.source) }}</td><td>{{ outcomeLabel(item.outcome) }}</td><td>{{ time(item.created_at) }}</td></tr></tbody></table></div>
  <p v-else class="muted">暂无处理记录。</p>
 </section>
 <NModal data-testid="payment-activity-dialog" v-model:show="activityOpen" preset="card" title="支付处理记录" style="width: 680px; max-width: calc(100vw - 32px)">
  <p class="muted payment-reference">支付记录 {{ activePayment }}</p>
  <div v-if="activities.length" class="table-wrap"><table><thead><tr><th>来源</th><th>结果</th><th>时间</th></tr></thead><tbody><tr v-for="item in activities" :key="item.id"><td>{{ sourceLabel(item.source) }}</td><td>{{ outcomeLabel(item.outcome) }}</td><td>{{ time(item.created_at) }}</td></tr></tbody></table></div>
  <p v-else class="muted">暂无处理记录。</p>
  <p class="muted">这里只显示处理来源和结果，不展示商户密钥、通知正文或支付链接。</p>
 </NModal>
</template>

<style scoped>
.recent-payment-activity{margin-top:24px}.payment-filter-panel{margin-bottom:24px}.payment-filters{display:grid;grid-template-columns:1fr 1fr 1.4fr auto;gap:16px;align-items:end}.payment-filters label{margin:0;min-width:0}.payment-filters button{margin-bottom:1px}.payment-detail{display:block;color:#75836f;font-size:11px;line-height:1.7;margin-top:5px;white-space:normal;max-width:240px;overflow-wrap:anywhere}.payment-reference{overflow-wrap:anywhere}.payment-row-actions{display:flex;gap:8px;flex-wrap:wrap;min-width:110px}table td{vertical-align:top}.payment-filters .n-select{margin-top:8px}@media(max-width:1000px){.payment-filters{grid-template-columns:1fr 1fr}}@media(max-width:600px){.payment-filters{grid-template-columns:1fr}.payment-filters button{justify-self:start}}
</style>
