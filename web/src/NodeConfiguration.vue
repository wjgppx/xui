<script setup lang="ts">
import { ref, watch } from 'vue'
import { NButton, NInput, NInputNumber, NModal, NSelect } from 'naive-ui'
import { api } from './api'
interface Binding { id: string; protocol: string; listen: string; port: number; server_name: string; certificate: string; users: string[]; public_host?: string; public_port?: number; access_mode?: 'manual' | 'plan' }
interface Configuration { revision: number; bindings: Binding[] }
const props = defineProps<{ nodeId: string; nodeName: string }>()
const emit = defineEmits<{ close: []; saved: [] }>()
const busy = ref(false), error = ref(''), config = ref<Configuration | null>(null)
const emails = ref<string[]>([])
const accessModes = [{ label: '逐用户授权（兼容）', value: 'manual' }, { label: '按套餐节点组', value: 'plan' }]
const protocols = [{ label: 'VLESS · TCP + TLS', value: 'vless' }, { label: 'Trojan · TCP + TLS', value: 'trojan' }]
watch(() => props.nodeId, async id => {
 config.value = null; error.value = ''; if (!id) return
 busy.value = true
 try { const value = await api<Configuration>(`/admin/nodes/${id}/configuration`); config.value = { ...value, bindings: value.bindings.map(b => ({ ...b, access_mode: b.access_mode || 'manual', public_host: b.public_host || '', public_port: b.public_port || 0 })) }; emails.value = value.bindings.map(b => b.users.join('\n')) }
 catch(e) { error.value = e instanceof Error ? e.message : '读取配置失败' }
 finally { busy.value = false }
}, { immediate: true })
function add() {
 if (!config.value || config.value.bindings.length >= 8) return
 let index = 1
 while(config.value.bindings.some(b => b.id === `inbound-${index}`)) index++
 config.value.bindings.push({ id: `inbound-${index}`, protocol: 'vless', listen: '0.0.0.0', port: 8443 + index - 1, server_name: '', certificate: '', users: [], access_mode: 'manual', public_host: '', public_port: 0 }); emails.value.push('')
}
function remove(i: number) { config.value?.bindings.splice(i, 1); emails.value.splice(i, 1) }
async function save() {
 if (busy.value || !config.value) return
 busy.value = true; error.value = ''
 try {
  const bindings = config.value.bindings.map((b, i) => ({ ...b, users: b.access_mode === 'plan' ? [] : (emails.value[i] || '').split(/[\n,]/).map(s => s.trim()).filter(Boolean) }))
  await api(`/admin/nodes/${props.nodeId}/configuration`, { revision: config.value.revision, bindings })
  emit('saved'); emit('close')
 } catch(e) { error.value = e instanceof Error ? e.message : '保存失败' }
 finally { busy.value = false }
}
</script>

<template>
 <NModal :show="!!nodeId" preset="card" :title="`${nodeName} · 代理配置`" style="width: 720px; max-width: calc(100vw - 32px); max-height: calc(100vh - 48px); overflow: auto; margin: 24px auto" :mask-closable="!busy" @update:show="value => { if (!value && !busy) emit('close') }">
  <div v-if="error" role="alert" class="alert error">{{ error }}</div>
  <form v-if="config" @submit.prevent="save">
   <p class="muted">保存后等待 Agent 应用。更新会重启该节点的代理进程并中断现有连接。当前配置版本 {{ config.revision }}。</p>
   <div v-if="!config.bindings.length" class="empty-config">尚无入站。保存空配置会停止该节点的代理服务。</div>
   <fieldset v-for="(b, i) in config.bindings" :key="i" class="binding-form">
    <legend>入站 {{ i + 1 }}</legend>
    <div class="form-row"><label>入站标识<NInput v-model:value="b.id" :input-props="{ required: true, 'aria-label': `入站标识 ${i + 1}` }" /></label><label>协议<NSelect v-model:value="b.protocol" :options="protocols" :aria-label="`协议 ${i + 1}`" /></label></div>
    <div class="form-row"><label>监听地址<NInput v-model:value="b.listen" :input-props="{ required: true, 'aria-label': `监听地址 ${i + 1}` }" /></label><label>端口<NInputNumber v-model:value="b.port" :min="1024" :max="65535" :precision="0" :input-props="{ required: true, 'aria-label': `端口 ${i + 1}` }" /></label></div>
    <div class="form-row"><label>TLS 域名<NInput v-model:value="b.server_name" placeholder="node.example.com" :input-props="{ required: true, 'aria-label': `TLS 域名 ${i + 1}` }" /></label><label>证书名称<NInput v-model:value="b.certificate" placeholder="node-a" :input-props="{ required: true, 'aria-label': `证书名称 ${i + 1}` }" /></label></div>
    <p class="muted">证书需预先安装到节点的证书目录，文件名为「证书名称.crt」和「证书名称.key」。私钥保留在节点本地。</p>
    <div class="publication-fields"><div class="form-row"><label>订阅连接地址<NInput v-model:value="b.public_host" placeholder="node.example.com" :input-props="{ 'aria-label': `订阅连接地址 ${i + 1}` }" /></label><label>订阅连接端口<NInputNumber v-model:value="b.public_port" :min="0" :max="65535" :precision="0" :input-props="{ 'aria-label': `订阅连接端口 ${i + 1}` }" /></label></div><p class="muted">填写客户端实际连接的地址和端口，可与节点监听端口不同。地址留空、端口为 0 时不发布到用户订阅。</p></div>
    <label>授权方式<NSelect v-model:value="b.access_mode" :options="accessModes" :aria-label="`授权方式 ${i + 1}`" :input-props="{ 'aria-label': `授权方式 ${i + 1}` }" :data-testid="`binding-access-${i}`" /></label>
    <p v-if="b.access_mode === 'plan'" class="muted">按当前已生效套餐购买时的节点组授权，自动下发有剩余额度且未撤销的用户。无需逐一填写账号；节点需先加入对应节点组。旧订单未记录节点组时不会自动补齐，切换前请核对既有授权。</p>
    <template v-else><label>授权账号<NInput v-model:value="emails[i]" type="textarea" placeholder="每行一个已注册用户的邮箱" :input-props="{ 'aria-label': `授权账号 ${i + 1}` }" /></label>
    <p class="muted">只向权益生效且尚有额度的账号开放；未来续费周期尚未生效时不授权。留空表示此入站没有可连接用户。</p></template>
    <NButton size="small" :disabled="busy" @click="remove(i)">移除此入站</NButton>
   </fieldset>
   <div class="config-actions"><NButton :disabled="busy || config.bindings.length >= 8" @click="add">＋ 添加入站</NButton><NButton type="primary" attr-type="submit" :loading="busy">保存并下发</NButton></div>
  </form>
  <p v-else-if="busy" class="muted">正在读取配置…</p>
 </NModal>
</template>
<style scoped>
.binding-form{border:1px solid #dfe8dc;border-radius:10px;padding:10px 20px 20px;margin:22px 0}.publication-fields{border-top:1px solid #e4ebdf;margin-top:20px;padding-top:4px}.binding-form legend{font-size:13px;color:#405d50;padding:0 8px}.config-actions{display:flex;justify-content:space-between;gap:12px;margin-top:24px}.empty-config{padding:24px;background:#f3f6ee;border-radius:8px;color:#71816b;font-size:13px}.n-select{margin-top:8px}@media(max-width:500px){.form-row{display:block}}
</style>
