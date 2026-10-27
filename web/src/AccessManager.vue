<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { NButton, NInput, NModal, NSelect, NSwitch, NTag } from 'naive-ui'
import { api } from './api'

interface NodeGroup { id: string; name: string; enabled: boolean; revision: number; node_ids: string[] }
interface AccessPlan { id: string; name: string; group_ids: string[]; revision: number }
interface AccessOverview { groups: NodeGroup[]; nodes: { id: string; name: string }[]; plans: AccessPlan[] }
interface DeliveryEvent { id: string; kind: string; aggregate_id: string; attempt_count: number; last_error: string; available_at: string; processed_at: string | null; dead_lettered_at: string | null; created_at: string }
const access = ref<AccessOverview>({ groups: [], nodes: [], plans: [] }), events = ref<DeliveryEvent[]>([])
const busy = ref(false), error = ref(''), notice = ref(''), groupOpen = ref(false), editing = ref<NodeGroup | null>(null)
const groupName = ref(''), groupNodes = ref<string[]>([]), groupEnabled = ref(true), planGroups = ref<Record<string, string[]>>({})
const nodeOptions = computed(() => access.value.nodes.map(n => ({ label: `${n.name} · ${n.id.slice(0, 8)}`, value: n.id })))
const groupOptions = computed(() => access.value.groups.map(g => ({ label: `${g.name}${g.enabled ? '' : '（已停用）'}`, value: g.id, disabled: !g.enabled })))
const groupNames = (ids: string[]) => ids.map(id => access.value.groups.find(g => g.id === id)?.name || id.slice(0, 8)).join('、')
const nodeNames = (ids: string[]) => ids.map(id => access.value.nodes.find(n => n.id === id)?.name || id.slice(0, 8)).join('、')
const kinds: Record<string, string> = { 'node_group.changed': '节点组已变更', 'plan.access_changed': '套餐授权已调整', 'entitlement.granted': '购买权益已生成', 'entitlement.activated': '续费权益已生效', 'entitlement.expired': '权益已到期', 'entitlement.revoked': '权益已撤销' }
function eventStatus(event: DeliveryEvent) { return event.processed_at ? '已投递' : event.dead_lettered_at ? '需要处理' : event.attempt_count > 0 ? '等待重试' : '等待投递' }
function errorLabel(code: string) { return ({ unsupported_event: '不支持的事件类型', invalid_payload: '事件内容校验失败', delivery_failed: '投递失败' } as Record<string, string>)[code] || '投递失败，请检查服务端记录。' }
async function refresh() {
 const [value, queue] = await Promise.all([api<AccessOverview>('/admin/access'), api<DeliveryEvent[]>('/admin/events')])
 access.value = value; events.value = queue
 planGroups.value = Object.fromEntries(value.plans.map(plan => [plan.id, [...plan.group_ids]]))
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch(e) { error.value = e instanceof Error ? e.message : '操作失败，请重试' } finally { busy.value = false }
}
function edit(group: NodeGroup | null) {
 editing.value = group; groupName.value = group?.name || ''; groupNodes.value = [...(group?.node_ids || [])]; groupEnabled.value = group?.enabled ?? true; groupOpen.value = true
}
function saveGroup() {
 const current = editing.value
 const changesAccess = current && (current.enabled !== groupEnabled.value || [...current.node_ids].sort().join(',') !== [...groupNodes.value].sort().join(','))
 if (changesAccess && !window.confirm('节点组成员或启用状态的变化会影响已购买权益。节点同步后可能中断现有连接，是否保存？')) return
 return action(async () => {
  await api(current ? `/admin/node-groups/${current.id}` : '/admin/node-groups', { name: groupName.value, node_ids: groupNodes.value, enabled: groupEnabled.value, ...(current ? { revision: current.revision } : {}) })
  groupOpen.value = false; await refresh(); notice.value = '节点组已保存，授权变化已进入事件队列。'
 })
}
function toggle(group: NodeGroup) {
 if (!window.confirm(`${group.enabled ? '停用' : '启用'}节点组会影响已购买权益的可用节点，节点同步后可能中断连接，是否继续？`)) return
 return action(async () => {
  await api(`/admin/node-groups/${group.id}`, { name: group.name, node_ids: group.node_ids, enabled: !group.enabled, revision: group.revision })
  await refresh(); notice.value = group.enabled ? '节点组已停用。' : '节点组已启用。'
 })
}
function savePlan(plan: AccessPlan) { return action(async () => {
 await api(`/admin/plans/${plan.id}/node-groups`, { group_ids: planGroups.value[plan.id] || [], revision: plan.revision })
 await refresh(); notice.value = '套餐节点组已保存，仅用于之后创建的订单。'
}) }
function retry(event: DeliveryEvent) { return action(async () => {
 await api(`/admin/events/${event.id}/retry`, {})
 await refresh(); notice.value = '事件已重新排队，等待投递。'
}) }
onMounted(() => action(refresh))
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">ACCESS & DELIVERY</p><h1>权限管理</h1><p>用节点组组织资源，为套餐配置可用范围并跟踪授权投递。</p></div><NButton :loading="busy" @click="action(refresh)">刷新权限与事件</NButton></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div><div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <section class="panel access-section"><div class="section-title"><h2>节点组</h2><NButton type="primary" :disabled="busy" @click="edit(null)">＋ 新建节点组</NButton></div>
  <p class="muted">节点组成员与启用状态会影响持有该组权益的用户。旧的逐用户授权模式继续保留。</p>
  <div v-if="!access.groups.length" class="empty-state"><span class="empty-symbol">▧</span><h3>先组织你的节点</h3><p>创建节点组，再将它分配给套餐。</p></div>
  <div v-else class="table-wrap"><table><thead><tr><th>节点组</th><th>状态</th><th>成员节点</th><th>操作</th></tr></thead><tbody><tr v-for="group in access.groups" :key="group.id" :data-group-id="group.id"><td><strong>{{ group.name }}</strong><small class="order-id">{{ group.id.slice(0, 12) }} · 版本 {{ group.revision }}</small></td><td><NTag size="small" round :type="group.enabled ? 'success' : 'default'">{{ group.enabled ? '已启用' : '已停用' }}</NTag></td><td class="access-names">{{ nodeNames(group.node_ids) || '暂无成员节点' }}</td><td><div class="access-actions"><NButton size="small" :disabled="busy" @click="edit(group)">编辑节点组</NButton><NButton size="small" :disabled="busy" @click="toggle(group)">{{ group.enabled ? '停用节点组' : '启用节点组' }}</NButton></div></td></tr></tbody></table></div>
 </section>
 <section class="panel access-section"><div class="section-title"><h2>套餐可用节点组</h2><span class="subtle">新订单的购买快照</span></div>
  <p class="muted">这里的调整仅影响之后创建的订单。已创建订单和已购买权益保留当时的节点组；修改组成员或停用整个组仍会影响现有权益。</p>
  <div v-if="!access.plans.length" class="muted">请先在运营工作台创建套餐。</div>
  <div v-for="plan in access.plans" :key="plan.id" class="plan-access-row" :data-plan-id="plan.id"><div><strong>{{ plan.name }}</strong><p class="muted">当前：{{ groupNames(plan.group_ids) || '尚未分配节点组' }}</p></div><NSelect v-model:value="planGroups[plan.id]" multiple clearable filterable :options="groupOptions" :aria-label="`${plan.name} 的节点组`" :input-props="{ 'aria-label': `${plan.name} 的节点组` }" :data-testid="`plan-groups-${plan.id}`" placeholder="选择可用节点组" :disabled="busy" /><NButton :disabled="busy" @click="savePlan(plan)">保存套餐授权</NButton></div>
 </section>
 <section class="panel access-section"><div class="section-title"><h2>业务事件投递</h2><span class="subtle">最近事件</span></div>
  <p class="muted">“已投递”表示控制端已推进目标节点的配置版本，实际代理应用仍需在节点管理查看。失败事件会退避重试，达到上限后可人工重新排队。</p>
  <p v-if="!events.length" class="muted">暂无事件。</p>
  <div v-else class="table-wrap"><table><thead><tr><th>事件</th><th>状态</th><th>失败次数</th><th>时间</th><th>操作</th></tr></thead><tbody><tr v-for="event in events" :key="event.id" :data-event-id="event.id"><td><strong>{{ kinds[event.kind] || '其他业务事件' }}</strong><small class="order-id">{{ event.kind }} · {{ event.aggregate_id.slice(0, 12) }}</small><small v-if="event.last_error" class="delivery-error">{{ errorLabel(event.last_error) }}</small></td><td><NTag size="small" :type="event.processed_at ? 'success' : event.dead_lettered_at ? 'error' : 'warning'">{{ eventStatus(event) }}</NTag></td><td>{{ event.attempt_count }}</td><td>{{ new Date(event.created_at).toLocaleString('zh-CN') }}<small v-if="!event.processed_at && !event.dead_lettered_at && event.attempt_count" class="order-id">下次尝试 {{ new Date(event.available_at).toLocaleTimeString('zh-CN') }}</small></td><td><NButton v-if="!event.processed_at && (event.attempt_count > 0 || event.dead_lettered_at)" size="small" :disabled="busy" @click="retry(event)">重新排队</NButton><span v-else class="muted">{{ event.processed_at ? '已完成' : '等待投递' }}</span></td></tr></tbody></table></div>
 </section>
 <NModal v-model:show="groupOpen" preset="card" :title="editing ? '编辑节点组' : '新建节点组'" style="width: 560px; max-width: calc(100vw - 32px)">
  <form @submit.prevent="saveGroup"><div v-if="error" class="alert error">{{ error }}</div><label>节点组名称<NInput v-model:value="groupName" :input-props="{ required: true, maxlength: 80, 'aria-label': '节点组名称' }" placeholder="例如：基础连接组" /></label><label>成员节点<NSelect v-model:value="groupNodes" multiple clearable filterable :options="nodeOptions" aria-label="节点组成员" :input-props="{ 'aria-label': '节点组成员' }" data-testid="group-nodes" placeholder="选择组内节点" /></label><div class="group-enabled"><span>启用节点组</span><NSwitch v-model:value="groupEnabled" aria-label="启用节点组" /></div><p class="muted">组内节点的入站需选择“按套餐节点组”授权模式。没有成员的组不会提供可连接节点。</p><NButton block type="primary" attr-type="submit" :loading="busy">保存节点组</NButton></form>
 </NModal>
</template>

<style scoped>
.access-section{margin-bottom:24px}.access-actions{display:flex;flex-wrap:wrap;gap:8px}.access-names{white-space:normal;max-width:320px;line-height:1.8}.plan-access-row{display:grid;grid-template-columns:minmax(150px,1fr) minmax(220px,1.5fr) auto;gap:20px;align-items:center;padding:18px 0;border-bottom:1px solid #edf0e9}.plan-access-row:last-child{border-bottom:0}.plan-access-row strong{font-size:13px;font-weight:500}.plan-access-row p{margin:8px 0 0}.group-enabled{display:flex;align-items:center;justify-content:space-between;color:#60725c;font-size:12px;margin:22px 0}.delivery-error{display:block;color:#a9513e;white-space:normal;font-size:11px;margin-top:7px}@media(max-width:850px){.plan-access-row{grid-template-columns:1fr}.plan-access-row .n-button{justify-self:start}.section-title{gap:12px;flex-wrap:wrap}}
</style>
