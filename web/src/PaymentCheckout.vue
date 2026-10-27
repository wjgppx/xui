<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NModal, NQrCode, NTag } from 'naive-ui'
import { api, type Order, type Payment, type PaymentMethod } from './api'

const props = defineProps<{ order: Order; methods: PaymentMethod[] }>()
const emit = defineEmits<{ close: []; settled: [] }>()
const payment = ref<Payment | null>(null), selected = ref<PaymentMethod['id'] | ''>('')
const busy = ref(false), loading = ref(true), error = ref(''), notice = ref('')
let mounted = true, announcedPaid = false
const providerName = computed(() => props.methods.find(m => m.id === payment.value?.provider)?.name || payment.value?.provider || '')
const now = ref(Date.now())
const clock = window.setInterval(() => { now.value = Date.now() }, 1000)
const cannotPay = computed(() => props.order.status === 'expired' || props.order.status === 'closed' || (!!props.order.expires_at && new Date(props.order.expires_at).getTime() <= now.value))
const canCheck = computed(() => payment.value?.provider === 'alipay' || payment.value?.provider === 'wechat')
const checkoutURL = computed(() => {
 try {
  const p = payment.value
  if (cannotPay.value) return ''
  if (!p || !p.checkout_url || p.status === 'paid') return ''
  const url = new URL(p.checkout_url)
  if (url.username || url.password) return ''
  return url.protocol === 'https:' || (p.checkout_kind === 'qr' && url.protocol === 'weixin:') ? url.href : ''
 } catch { return '' }
})
function accept(p: Payment | null) {
 if (!mounted) return
 payment.value = p
 if (p) selected.value = p.provider
 if (p?.status === 'paid' && !announcedPaid) { announcedPaid = true; emit('settled') }
}
async function readPayment() {
 const response = await fetch(`/api/v1/orders/${encodeURIComponent(props.order.id)}/payment`, { credentials: 'same-origin' })
 if (response.status === 404) { accept(null); return }
 const data = await response.json()
 if (!response.ok) throw new Error(data.error || '暂时无法读取支付状态，请稍后重试。')
 accept(data)
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch (e) { if (mounted) error.value = e instanceof Error ? e.message : '操作失败，请稍后重试。' }
 finally { if (mounted) busy.value = false }
}
function create() { return action(async () => {
 if (cannotPay.value) throw new Error('此订单已停止发起付款，请刷新查看实际支付状态。')
 const provider = payment.value?.provider || selected.value
 if (!provider) throw new Error('请选择支付方式。')
 try {
  accept(await api<Payment>(`/orders/${encodeURIComponent(props.order.id)}/payment`, { provider }))
 } catch (e) {
  // A gateway failure can follow the committed channel selection. Read it back
  // so retries keep the original method and merchant request identity.
  try { await readPayment() } catch { /* Keep the original checkout error. */ }
  throw e
 }
}) }
function refresh() { return action(async () => {
 await readPayment()
 notice.value = payment.value?.status === 'paid' ? '支付已确认，套餐权益已开通。' : '暂未确认支付，请完成付款后稍候再刷新。'
}) }
function check() { return action(async () => {
 accept(await api<Payment>(`/orders/${encodeURIComponent(props.order.id)}/payment/sync`, {}))
 notice.value = payment.value?.status === 'paid' ? '支付已确认，套餐权益已开通。' : '暂未确认支付，请稍候再核对。'
}) }
onMounted(async () => {
 await action(readPayment)
 if (mounted) loading.value = false
})
onBeforeUnmount(() => { mounted = false; window.clearInterval(clock) })
</script>

<template>
 <NModal :show="true" preset="card" title="订单支付" class="payment-modal" style="width: 520px; max-width: calc(100vw - 32px)" @update:show="value => { if (!value) emit('close') }">
  <div class="payment-heading"><div><strong>{{ order.plan_name }}</strong><small class="order-id">订单 {{ order.id.slice(0, 12) }}</small></div><strong class="payment-price">¥{{ (order.price_minor / 100).toFixed(2) }}</strong></div>
  <div v-if="error" role="alert" class="alert error">{{ error }}</div>
  <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
  <p v-if="loading" class="muted">正在读取支付状态…</p>
  <template v-else-if="payment?.status === 'paid'">
   <NTag type="success" round>支付已确认</NTag>
   <p>套餐权益已开通，可以关闭窗口查看订单和权益。</p>
  </template>
  <template v-else-if="cannotPay">
   <NTag round>{{ order.status === 'closed' ? '订单已关闭' : '订单已超时' }}</NTag>
   <p class="muted">此订单已停止发起付款。若你已完成付款，可以刷新或核对结果，系统仍会核实有效的付款通知。</p>
  </template>
  <template v-else>
   <template v-if="!payment">
    <h3 class="payment-subtitle">选择支付方式</h3>
    <div v-if="methods.length" class="payment-methods" role="group" aria-label="支付方式">
     <NButton v-for="method in methods" :key="method.id" :type="selected === method.id ? 'primary' : 'default'" :disabled="busy" :aria-pressed="selected === method.id" @click="selected = method.id">{{ method.name }}</NButton>
    </div>
    <p v-else class="muted">支付渠道待配置，请稍后重试或联系服务方。</p>
    <p class="muted">确认后此订单将使用所选支付方式。需要更换方式时，请创建新订单。</p>
    <NButton v-if="methods.length" type="primary" block :disabled="!selected" :loading="busy" @click="create">确认支付方式</NButton>
   </template>
   <template v-else>
    <p class="payment-provider">支付方式：<strong>{{ providerName }}</strong></p>
    <p class="muted">此订单已选择支付方式。如需更换，请创建新订单。</p>
    <div v-if="checkoutURL && payment.checkout_kind === 'qr'" class="payment-qr" role="img" aria-label="付款二维码"><NQrCode :value="checkoutURL" :size="220" :padding="12" /><p>请使用{{ providerName }}扫描二维码付款。</p></div>
    <a v-else-if="checkoutURL && payment.checkout_kind === 'redirect'" class="payment-link" :href="checkoutURL" target="_blank" rel="noopener noreferrer">打开支付收银台 ↗</a>
    <template v-else><p class="muted">支付入口尚未生成，可以使用同一支付方式重试。</p><NButton type="primary" :loading="busy" @click="create">重新获取支付入口</NButton></template>
    <p class="muted">请在收银台确认金额。完成付款后，点击下方按钮查看结果；订单显示“支付已确认”后权益才会开通。</p>
   </template>
  </template>
  <div class="payment-actions"><NButton v-if="payment && payment.status !== 'paid' && canCheck" :loading="busy" @click="check">核对支付结果</NButton><NButton v-if="payment?.status !== 'paid'" :loading="busy" @click="refresh">刷新支付结果</NButton><NButton :disabled="busy" @click="emit('close')">关闭</NButton></div>
 </NModal>
</template>

<style scoped>
.payment-heading{display:flex;gap:20px;align-items:center;justify-content:space-between;margin-bottom:24px}.payment-heading strong{font-weight:550}.payment-price{font-size:28px;white-space:nowrap}.payment-subtitle{font-size:14px;font-weight:550;margin-top:20px}.payment-methods{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}.payment-actions{display:flex;justify-content:flex-end;gap:10px;flex-wrap:wrap;margin-top:24px}.payment-provider{margin-top:20px}.payment-qr{display:flex;flex-direction:column;align-items:center;margin:24px 0 12px}.payment-qr p{font-size:13px;line-height:1.7}.payment-link{display:block;text-align:center;border:1px solid #21766b;border-radius:10px;padding:12px;color:#21766b;text-decoration:none;margin:24px 0}.payment-link:hover{background:#f0f6ee}@media(max-width:440px){.payment-methods{grid-template-columns:1fr}.payment-actions button{flex:1}}
</style>
