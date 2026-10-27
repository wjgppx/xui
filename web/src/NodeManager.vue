<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { NButton, NInput, NModal, NTag } from 'naive-ui'
import { api } from './api'
import NodeConfiguration from './NodeConfiguration.vue'
interface Node { id: string; name: string; enrolled: boolean; revoked: boolean; desired_revision: number; observed_revision: number; agent_version: string; last_seen_at: string | null; applied_revision: number; apply_status: string; apply_error: string }
interface Enrollment { node_id: string; enrollment_token: string; expires_at: string }
const nodes = ref<Node[]>([]), busy = ref(false), error = ref(''), notice = ref(''), createOpen = ref(false), tokenOpen = ref(false), name = ref('')
const enrollment = ref<Enrollment | null>(null)
const editing = ref<Node | null>(null)
const failureNames: Record<string, string> = { meter_unavailable: '流量采集或持久化失败', config_rejected: '配置或证书校验失败', start_failed: '代理进程启动失败', lease_invalid: '授权租约已失效', runtime_unavailable: '代理运行环境不可用' }
function stateLabel(n: Node) { if (n.revoked) return '身份已撤销'; if (n.last_seen_at && Date.now() - Date.parse(n.last_seen_at) > 60000) return '状态已过期'; return n.apply_status === 'applied' ? '已应用' : n.apply_status === 'failed' ? '应用失败' : '等待应用' }
const refresh = async () => { nodes.value = await api<Node[]>('/admin/nodes') }
async function action(task: () => Promise<void>) { if (busy.value) return; busy.value = true; error.value = ''; notice.value = ''; try { await task() } catch(e) { error.value = e instanceof Error ? e.message : '操作失败' } finally { busy.value = false } }
function create() { return action(async () => { enrollment.value = await api<Enrollment>('/admin/nodes', { name: name.value }); createOpen.value = false; tokenOpen.value = true; name.value = ''; await refresh() }) }
function rotate(n: Node) { if (!window.confirm('重新注册会使当前节点凭据失效，是否继续？')) return; return action(async () => { enrollment.value = await api<Enrollment>(`/admin/nodes/${n.id}/enrollment`, {}); tokenOpen.value = true; await refresh() }) }
function revoke(n: Node) { if (!window.confirm('撤销后，该节点将无法访问控制端，是否继续？')) return; return action(async () => { await api(`/admin/nodes/${n.id}/revoke`, {}); notice.value = '节点身份已撤销。'; await refresh() }) }
function copy() { return action(async () => { if (enrollment.value) { await navigator.clipboard.writeText(enrollment.value.enrollment_token); notice.value = '注册令牌已复制。' } }) }
onMounted(() => action(refresh))
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">NODE OPERATIONS</p><h1>节点管理</h1><p>管理节点、TLS 入站、账号授权和配置应用结果。</p></div><NButton type="primary" @click="createOpen = true">＋ 注册节点</NButton></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div><div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <section class="panel"><div class="section-title"><h2>已登记节点</h2><NButton size="small" :loading="busy" @click="action(refresh)">刷新状态</NButton></div>
  <div v-if="!nodes.length" class="empty-state"><span class="empty-symbol">◎</span><h3>连接你的第一台服务器</h3><p>创建节点，使用限时注册令牌启动 Agent。</p></div>
  <div v-else class="table-wrap"><table><thead><tr><th>节点</th><th>身份状态</th><th>最近心跳</th><th>已读取 / 期望版本</th><th>代理配置</th><th>操作</th></tr></thead><tbody><tr v-for="n in nodes" :key="n.id"><td><strong>{{ n.name }}</strong><small class="order-id">{{ n.id.slice(0, 12) }}</small></td><td><NTag round size="small" :type="n.revoked ? 'error' : n.enrolled ? 'success' : 'warning'">{{ n.revoked ? '已撤销' : n.enrolled ? '已注册' : '等待注册' }}</NTag></td><td>{{ n.last_seen_at ? new Date(n.last_seen_at).toLocaleString('zh-CN') : '尚无心跳' }}<small class="order-id">{{ n.agent_version ? `Agent ${n.agent_version}` : '' }}</small></td><td>{{ n.observed_revision }} / {{ n.desired_revision }}</td><td><NTag size="small" :type="n.apply_status === 'failed' ? 'error' : 'default'">{{ stateLabel(n) }}</NTag><small class="order-id">应用版本 {{ n.applied_revision }} / {{ n.desired_revision }}</small><small v-if="n.apply_error" class="apply-error">{{ failureNames[n.apply_error] || '应用失败' }}</small></td><td><div class="node-actions"><NButton size="small" :disabled="busy || n.revoked" @click="editing = n">代理配置</NButton><NButton size="small" :disabled="busy" @click="rotate(n)">重新注册</NButton><NButton v-if="!n.revoked" size="small" :disabled="busy" @click="revoke(n)">撤销身份</NButton></div></td></tr></tbody></table></div>
 </section><p class="footnote">已支持 VLESS / Trojan 的 TCP + TLS 配置。授权租约到期会停止代理；配置更新会中断现有连接。已接入流量批次入账与额度过滤，采样和上报存在延迟；已支持节点组授权与客户端订阅。</p>
 <NodeConfiguration :node-id="editing?.id || ''" :node-name="editing?.name || ''" @close="editing = null" @saved="action(async () => { await refresh(); notice = '配置已保存，等待节点应用。' })" />
 <NModal v-model:show="createOpen" preset="card" title="注册节点" style="width: 440px; max-width: calc(100vw - 32px)"><form @submit.prevent="create"><div v-if="error" class="alert error">{{ error }}</div><label>节点名称<NInput v-model:value="name" placeholder="例如：测试节点 A" :input-props="{ required: true, maxlength: 50, 'aria-label': '节点名称' }" /></label><p class="muted">创建后生成 15 分钟有效的注册令牌。</p><NButton block attr-type="submit" type="primary" :loading="busy">生成注册令牌</NButton></form></NModal>
 <NModal v-model:show="tokenOpen" preset="card" title="连接节点 Agent" style="width: 540px; max-width: calc(100vw - 32px)" @after-leave="enrollment = null"><template v-if="enrollment"><p class="muted">令牌仅在此处显示。请在 {{ new Date(enrollment.expires_at).toLocaleTimeString('zh-CN') }} 前完成注册。</p><label>注册令牌<NInput :value="enrollment.enrollment_token" readonly :input-props="{ 'aria-label': '注册令牌' }" /></label><NButton :disabled="busy" @click="copy">复制令牌</NButton><p class="muted">在服务器设置 XUI_ENROLL_TOKEN 环境变量，再启动 xui-agent。详细步骤见项目的 docs/node-agent.md。</p><p class="muted">令牌关闭后不再回显；丢失或过期时可选择“重新注册”。</p></template></NModal>
</template>

<style scoped>.node-actions{display:flex;gap:8px;flex-wrap:wrap;max-width:240px}.apply-error{display:block;white-space:normal;color:#a9513e;font-size:11px;margin-top:6px}</style>
