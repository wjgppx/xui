<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NInput, NSelect, NTag } from 'naive-ui'
import { api } from './api'

type Format = 'uri' | 'base64' | 'clash' | 'xray'
interface SubscriptionStatus { enabled: boolean; created_at: string | null }
interface IssuedSubscription { token: string; urls: Record<Format, string> }
const status = ref<SubscriptionStatus | null>(null), issued = ref<IssuedSubscription | null>(null)
const busy = ref(false), error = ref(''), notice = ref(''), format = ref<Format>('clash')
const formats = [
 { label: 'Mihomo / Clash Meta 配置', value: 'clash' },
 { label: '通用 URI 列表', value: 'uri' },
 { label: 'Base64 URI 订阅', value: 'base64' },
 { label: 'Xray 客户端配置', value: 'xray' },
]
const currentURL = computed(() => issued.value?.urls[format.value] || '')
let mounted = true
async function refresh() { const value = await api<SubscriptionStatus>('/me/subscription'); if (mounted) status.value = value }
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch(e) { if (mounted) error.value = e instanceof Error ? e.message : '操作失败，请重试' }
 finally { if (mounted) busy.value = false }
}
function generate() {
 if (!status.value || (status.value.enabled && !window.confirm('重新生成后，现有订阅链接立即失效。需要在客户端替换链接，是否继续？'))) return
 return action(async () => {
  issued.value = null
  const value = await api<IssuedSubscription>('/me/subscription/rotate', {})
  if (!mounted) return
  issued.value = value
  await refresh()
  if (mounted) notice.value = '新订阅链接已生成。请在离开页面前保存到客户端。'
 })
}
function revoke() {
 if (!window.confirm('撤销后，现有订阅链接立即失效。已导入的代理凭据仍需单独重置，是否继续？')) return
 return action(async () => {
  await api('/me/subscription/revoke', {})
  if (!mounted) return
  issued.value = null
  await refresh()
  if (mounted) notice.value = '订阅链接已撤销。'
 })
}
function rotateCredentials() {
 if (!window.confirm('重置代理凭据后，需要在客户端更新订阅。旧凭据将在节点同步后失效，并中断连接，是否继续？')) return
 return action(async () => {
  await api('/me/proxy-credentials/rotate', {})
  if (mounted) notice.value = '代理凭据已重置。请更新客户端订阅；旧凭据将在节点同步应用后失效。'
 })
}
function copy() {
 return action(async () => {
  if (!currentURL.value) return
  if (!navigator.clipboard) throw new Error('当前浏览器不支持自动复制，请手动复制订阅链接。')
  await navigator.clipboard.writeText(currentURL.value)
  if (mounted) notice.value = '订阅链接已复制。'
 })
}
function download() {
 return action(async () => {
  if (!currentURL.value) return
  const response = await fetch(currentURL.value, { credentials: 'omit', cache: 'no-store', referrerPolicy: 'no-referrer' })
  if (!response.ok) throw new Error('配置下载失败，订阅链接可能已失效。请刷新状态或重新生成。')
  const blob = await response.blob()
  if (!mounted) return
  const objectURL = URL.createObjectURL(blob), anchor = document.createElement('a')
  anchor.href = objectURL; anchor.download = format.value === 'clash' ? 'xui-mihomo.yaml' : format.value === 'xray' ? 'xui-xray.json' : 'xui-subscription.txt'
  document.body.appendChild(anchor); anchor.click(); anchor.remove(); URL.revokeObjectURL(objectURL)
  notice.value = '配置文件已下载。仅包含当前有权使用且已发布的节点。'
 })
}
onMounted(() => action(refresh))
onBeforeUnmount(() => { mounted = false; issued.value = null })
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">YOUR CONNECTIONS</p><h1>我的订阅</h1><p>获取已授权节点，在客户端更新你的连接配置。</p></div></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <div class="subscription-grid">
  <section class="panel subscription-main">
   <div class="section-title"><h2>客户端订阅</h2><NTag v-if="status" size="small" round :type="status.enabled ? 'success' : 'default'">{{ status.enabled ? '订阅已启用' : '订阅未启用' }}</NTag></div>
   <template v-if="status">
    <p class="muted">订阅会按当前权益和节点授权生成。套餐尚未生效、到期或额度耗尽时，对应节点不会出现在配置中。</p>
    <p v-if="status.created_at" class="muted">最近生成：{{ new Date(status.created_at).toLocaleString('zh-CN') }}</p>
    <div v-if="issued" class="issued-subscription">
     <label>配置格式<NSelect v-model:value="format" :options="formats" aria-label="配置格式" /></label>
     <label>订阅链接<NInput :value="currentURL" readonly :input-props="{ 'aria-label': '订阅链接', autocomplete: 'off', spellcheck: false }" /></label>
     <div class="subscription-actions"><NButton :disabled="busy" @click="copy">复制链接</NButton><NButton type="primary" :disabled="busy" @click="download">下载配置</NButton></div>
     <p class="muted">链接包含访问凭据，请妥善保存。离开页面后不再回显；已有客户端可继续使用保存的链接。</p>
    </div>
    <div v-else class="subscription-empty"><span class="empty-symbol">↗</span><h3>{{ status.enabled ? '订阅链接已生成' : '准备接入你的设备' }}</h3><p>{{ status.enabled ? '链接仅在生成时显示。请使用已保存在客户端中的链接，或重新生成。' : '生成订阅链接，再复制到支持的客户端。' }}</p></div>
    <div class="subscription-actions"><NButton :type="issued ? 'default' : 'primary'" :loading="busy" @click="generate">{{ status.enabled ? '重新生成订阅链接' : '生成订阅链接' }}</NButton><NButton v-if="status.enabled" :disabled="busy" @click="revoke">撤销订阅链接</NButton></div>
   </template>
   <p v-else-if="busy" class="muted">正在读取订阅状态…</p>
   <NButton v-else @click="action(refresh)">重新读取状态</NButton>
  </section>
  <section class="panel subscription-security">
   <div class="section-title"><h2>管理连接凭据</h2><span class="subtle">ACCESS</span></div>
   <p class="muted">重新生成订阅链接会让旧链接立即失效，已导入客户端的代理凭据仍然有效。</p>
   <p class="muted">如果需要撤销已导入的连接，请重置代理凭据，并在客户端更新订阅。节点同步并应用后，旧凭据和现有连接会失效。</p>
   <NButton :disabled="busy || !status" @click="rotateCredentials">重置代理凭据</NButton>
   <p class="muted">节点正常每 10 秒同步；失联时由最长 60 秒授权租约限制继续运行。</p>
  </section>
 </div>
 <p class="footnote">只包含管理员已发布、且当前允许你使用的节点。没有可用节点时，订阅可能为空；生成链接不代表套餐已开通。</p>
</template>

<style scoped>
.subscription-grid{display:grid;grid-template-columns:minmax(0,1.6fr) minmax(260px,1fr);gap:24px;align-items:start}.subscription-main,.subscription-security{min-width:0}.subscription-actions{display:flex;flex-wrap:wrap;gap:10px;margin-top:24px}.issued-subscription{padding:4px 20px 16px;margin:22px 0;background:#f5f8f1;border-radius:10px}.subscription-empty{padding:20px 0;text-align:center;color:#89977f}.subscription-empty h3{font-size:16px;color:#304e42}.subscription-empty p{font-size:12px;line-height:1.8;max-width:360px;margin:12px auto}.n-select{margin-top:8px}.subscription-security .n-button{margin-top:12px}@media(max-width:1000px){.subscription-grid{grid-template-columns:1fr}.subscription-security{max-width:none}}@media(max-width:500px){.issued-subscription{padding:4px 12px 16px}.subscription-actions .n-button{flex:1}}
</style>
