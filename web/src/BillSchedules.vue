<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NModal, NTag } from 'naive-ui'
import { api, type BillSchedule } from './api'

const props = defineProps<{ canManage: boolean; downloadsEnabled: boolean }>()
const items = ref<BillSchedule[]>([]), loading = ref(false), saving = ref(false), error = ref(''), notice = ref('')
const open = ref(false), selected = ref<BillSchedule | null>(null), runAt = ref('10:00'), enabled = ref(false), formError = ref('')
let alive = true
const provider = (value: string) => value === 'alipay' ? '支付宝' : '微信支付'
const kind = (value: string) => value === 'refund' ? '退款' : '收款'
const status = (value: string) => ({ active: '已启用', disabled: '已停用', actor_changed: '授权已变化，请重新启用', merchant_changed: '商户已变化，请重新启用', provider_unavailable: '渠道尚未配置' }[value] || value)
function nextRun(item: BillSchedule) {
 if (!item.enabled || !item.next_bill_date) return '—'
 const date = new Date(`${item.next_bill_date}T00:00:00+08:00`)
 if (!Number.isFinite(date.getTime())) return '—'
 date.setUTCDate(date.getUTCDate() + 1)
 return `${new Date(date.getTime() + 8 * 60 * 60 * 1000).toISOString().slice(0, 10)} ${item.run_at}`
}
async function refresh() {
 if (loading.value) return
 loading.value = true; error.value = ''
 try { const next = await api<{ items: BillSchedule[] }>('/admin/bill-schedules'); if (alive) items.value = next.items }
 catch (e) { if (alive) error.value = e instanceof Error ? e.message : '自动拉取计划暂时无法读取。' }
 finally { if (alive) loading.value = false }
}
function clear() { selected.value = null; formError.value = '' }
function edit(item: BillSchedule) {
 if (!props.canManage || saving.value) return
 clear(); selected.value = item; runAt.value = item.run_at; enabled.value = item.enabled; open.value = true
}
watch(open, value => { if (!value) clear() })
async function save() {
 if (!selected.value || saving.value || !props.canManage) return
 formError.value = ''
 if (enabled.value && !props.downloadsEnabled) { formError.value = '官方下载未启用，当前只能停用计划。'; return }
 if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(runAt.value)) { formError.value = '请选择有效的北京时间，格式为 HH:MM。'; return }
 const item = selected.value
 saving.value = true; notice.value = ''
 try {
  const saved = await api<BillSchedule>('/admin/bill-schedules', { provider: item.provider, kind: item.kind, enabled: enabled.value, run_at: runAt.value, revision: item.revision }, 'PUT')
  if (!alive) return
  items.value = items.value.map(existing => existing.id === saved.id ? saved : existing)
  open.value = false
  notice.value = saved.enabled ? '自动拉取计划已保存。到达执行时间后创建下载任务，保存计划不表示账单已导入。' : '自动拉取计划已停用。已导入账单和历史任务继续保留。'
 } catch (e) { if (alive) formError.value = e instanceof Error ? e.message : '计划暂时无法保存，请刷新后重试。' }
 finally { if (alive) saving.value = false }
}
onMounted(refresh)
onBeforeUnmount(() => { alive = false; clear() })
</script>

<template>
 <section class="panel schedules-panel" data-testid="bill-schedules">
  <div class="section-title schedules-heading"><div><h2>每日自动拉取</h2><p class="muted">按北京时间获取前一日账单。收款与退款分别配置，下载完成后生成对账结果。</p></div><NButton :loading="loading" :disabled="saving" @click="refresh">刷新自动拉取计划</NButton></div>
  <div v-if="error" role="alert" class="alert error">{{ error }}</div>
  <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
  <p v-if="!downloadsEnabled" role="status" class="alert schedules-note">官方下载未启用，自动拉取暂停。可以查看或停用已有计划。</p>
  <div v-if="items.length" class="table-wrap"><table><thead><tr><th>渠道 / 类型</th><th>每日时间 / 状态</th><th>账单进度</th><th>操作</th></tr></thead><tbody><tr v-for="item in items" :key="item.id" :data-schedule-id="item.id"><td><strong>{{ provider(item.provider) }}</strong><small class="schedule-detail">成功{{ kind(item.kind) }}</small></td><td>{{ item.run_at }}（北京时间）<small class="schedule-detail"><NTag size="small" :type="item.enabled ? 'success' : 'warning'">{{ status(item.state) }}</NTag></small></td><td><span class="schedule-detail">下一账单 {{ item.next_bill_date || '—' }}</span><small class="schedule-detail">预计执行 {{ nextRun(item) }}（北京时间）</small><small class="schedule-detail">最近排队 {{ item.last_enqueued_date || '—' }}</small></td><td><NButton v-if="canManage" size="small" :disabled="saving" @click="edit(item)">配置自动拉取</NButton><span v-else class="muted">仅可查看</span></td></tr></tbody></table></div>
  <p v-else-if="!loading && !error" class="muted">暂无自动拉取计划。</p>
  <p class="muted">停机恢复时最多补排最近 7 个已结束账单日。计划只安排下载和差异核对，不改变收款、退款或套餐权益；需人工处理的任务不会自动重新提交。</p>
 </section>

 <NModal v-model:show="open" preset="card" title="配置每日自动拉取" style="width: 580px; max-width: calc(100vw - 32px)">
  <form @submit.prevent="save">
   <div v-if="formError" role="alert" class="alert error">{{ formError }}</div>
   <p v-if="selected">{{ provider(selected.provider) }} · 成功{{ kind(selected.kind) }}</p>
   <label class="schedule-check"><input v-model="enabled" type="checkbox" aria-label="启用每日自动拉取" :disabled="saving || (!downloadsEnabled && !enabled)" />启用每日自动拉取</label>
   <label>每日执行时间（北京时间）<input v-model="runAt" type="time" aria-label="自动拉取执行时间" required :disabled="saving" /></label>
   <p class="muted">首次启用、重新启用或改用新商户时，从昨天的账单开始，不补排更早历史。同商户修改时间会保留当前进度。</p>
   <p class="muted">保存后由当前账号授权后台执行。账号权限、密码或商户配置变化后，计划会暂停，需核对并重新启用。停用或更换授权账号后，尚未完成的自动任务需人工重新提交。</p>
   <div class="schedule-actions"><NButton type="primary" attr-type="submit" :loading="saving">保存自动拉取计划</NButton><NButton :disabled="saving" @click="open = false">取消</NButton></div>
  </form>
 </NModal>
</template>

<style scoped>
.schedules-panel{margin-bottom:20px}.schedules-heading{align-items:flex-start;gap:16px}.schedule-detail{display:block;margin-top:6px;font-size:11px;color:#81907e}.schedules-note{background:#f7f2df;color:#817443}.schedule-check{display:flex;align-items:center;gap:10px}.schedule-check input{flex-shrink:0}.schedule-actions{display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-top:20px}input[type=time]{display:block;width:100%;min-width:0;margin-top:8px;border:1px solid #d9e1d4;border-radius:8px;background:#fff;padding:10px 12px;color:#3d5340;font:inherit}input:focus{outline:2px solid #7dab91;outline-offset:2px}@media(max-width:760px){.schedules-heading{flex-direction:column}}
</style>
