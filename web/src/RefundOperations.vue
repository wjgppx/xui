<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NInput, NModal, NTag } from 'naive-ui'
import { api, type RefundOperation } from './api'
const props = defineProps<{ canApprove: boolean; actionsEnabled: boolean }>()
const items = ref<RefundOperation[]>([]), busy = ref(false), error = ref(''), notice = ref('')
const open = ref(false), selected = ref<RefundOperation | null>(null), password = ref(''), approvalError = ref('')
const status = (value: string) => ({ requested: '待管理员审批', queued: '等待渠道处理', processing: '渠道处理中', succeeded: '退款成功', failed: '退款失败', manual_required: '需要人工处理' }[value] || value)
const channel = (value: string) => value === 'alipay' ? '支付宝' : '微信支付'
const time = (value?: string | null) => value ? new Date(value).toLocaleString('zh-CN') : '—'
async function refresh() { items.value = (await api<{ items: RefundOperation[] }>('/admin/refunds')).items }
async function load() {
 if (busy.value) return
 busy.value = true; error.value = ''
 try { await refresh() } catch (e) { error.value = e instanceof Error ? e.message : '退款记录暂时无法加载。' } finally { busy.value = false }
}
function clear() { password.value = ''; approvalError.value = ''; selected.value = null }
watch(open, value => { if (!value) clear() })
onBeforeUnmount(clear)
function review(item: RefundOperation) { if (!props.canApprove || !props.actionsEnabled) return; clear(); selected.value = item; open.value = true }
async function approve() {
 if (busy.value || !selected.value || !props.canApprove || !props.actionsEnabled) return
 if (!password.value) { approvalError.value = '请输入当前密码确认身份。'; return }
 const id = selected.value.id, currentPassword = password.value
 busy.value = true; approvalError.value = ''; notice.value = ''
 try {
  await api(`/admin/refunds/${encodeURIComponent(id)}/approve`, { current_password: currentPassword })
  open.value = false; await refresh(); notice.value = '退款已排队，只有渠道确认成功后才撤销原订单权益。'
 } catch (e) { approvalError.value = e instanceof Error ? e.message : '审批暂时无法完成。' }
 finally { password.value = ''; busy.value = false }
}
onMounted(load)
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">REFUNDS</p><h1>退款管理</h1><p>核对原付款全额退款申请、审批和渠道确认进度。</p></div><NButton :loading="busy" @click="load">刷新退款记录</NButton></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <p v-if="!actionsEnabled" role="status" class="alert warning">资金操作未启用。申请会保留，当前不能提交退款审批。</p>
 <section class="panel">
  <p class="muted">仅支持支付宝和微信原订单全额退款。申请和审批不会立即撤销权益；以渠道确认的退款结果为准。</p>
  <div v-if="items.length" class="table-wrap"><table><thead><tr><th>退款 / 订单</th><th>用户 / 渠道</th><th>金额 / 原因</th><th>处理进度</th><th>操作</th></tr></thead><tbody>
   <tr v-for="item in items" :key="item.id" :data-refund-id="item.id">
    <td><strong>{{ item.id.slice(0, 12) }}</strong><small class="payment-detail">订单 {{ item.order_id.slice(0, 12) }}</small><small class="payment-detail">申请 {{ time(item.created_at) }}</small></td>
    <td>{{ item.user_email }}<small class="payment-detail">{{ channel(item.provider) }}</small></td>
    <td><strong>¥{{ (item.amount_minor / 100).toFixed(2) }}</strong><small class="payment-detail refund-reason">{{ item.reason }}</small></td>
    <td><NTag size="small" :type="item.status === 'succeeded' ? 'success' : item.status === 'failed' || item.status === 'manual_required' ? 'error' : 'warning'">{{ status(item.status) }}</NTag><small v-if="item.completed_at" class="payment-detail">确认 {{ time(item.completed_at) }}</small></td>
    <td><NButton v-if="canApprove && (item.status === 'requested' || item.status === 'manual_required')" size="small" :disabled="busy || !actionsEnabled" @click="review(item)">{{ item.status === 'manual_required' ? '重新核实退款' : '审批全额退款' }}</NButton><span v-else class="muted">{{ item.status === 'requested' ? '等待管理员审批' : '—' }}</span></td>
   </tr>
  </tbody></table></div>
  <p v-else class="muted">暂无退款申请。可从支付流水中申请原付款全额退款。</p>
 </section>
 <NModal v-model:show="open" preset="card" title="审批全额退款" style="width: 520px; max-width: calc(100vw - 32px)">
  <form @submit.prevent="approve">
   <div v-if="approvalError" role="alert" class="alert error">{{ approvalError }}</div>
   <template v-if="selected"><p class="payment-reference">订单 {{ selected.order_id }}</p><p>{{ selected.user_email }} · {{ channel(selected.provider) }}</p><p>退款金额 <strong>¥{{ (selected.amount_minor / 100).toFixed(2) }}</strong></p><p class="refund-reason">原因：{{ selected.reason }}</p></template>
   <p class="muted">此操作会向支付渠道提交原订单全额退款。结果不明时将使用同一退款编号继续核实，请先核对交易。</p>
   <label>当前密码<NInput v-model:value="password" type="password" :input-props="{ required: true, autocomplete: 'current-password', 'aria-label': '退款审批当前密码' }" /></label>
   <div class="payment-row-actions"><NButton attr-type="submit" type="primary" :loading="busy">验证身份并批准退款</NButton><NButton :disabled="busy" @click="open = false">取消</NButton></div>
  </form>
 </NModal>
</template>

<style scoped>.refund-reason { white-space: pre-wrap; overflow-wrap: anywhere; max-width: 28rem; }</style>
