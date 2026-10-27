<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { NButton, NInput, NTag } from 'naive-ui'
import { api, type User } from './api'
const props = defineProps<{ user: User | null; developmentMail: boolean; verificationRequired: boolean; linkAction: 'verify' | 'reset' | null; linkToken: string }>()
const emit = defineEmits<{ verified: []; signedOut: []; consumed: []; login: []; mailAction: [action: 'verify' | 'reset', token: string] }>()
interface DevelopmentMail { id: string; subject: string; body: string; created_at: string }
const busy = ref(false), error = ref(''), notice = ref(''), inbox = ref<DevelopmentMail[]>([]), inboxOpen = ref(false)
const recoveryEmail = ref(''), currentPassword = ref(''), newPassword = ref(''), repeatPassword = ref('')
const verified = computed(() => !!props.user?.email_verified_at)
// A second mail link may arrive through same-document fragment navigation.
// Clear fields from the previous action before presenting the new one.
watch(() => props.linkToken, (token, previous) => {
 if (!token || token === previous) return
 error.value = ''; notice.value = ''; inbox.value = []; inboxOpen.value = false
 currentPassword.value = ''; newPassword.value = ''; repeatPassword.value = ''
})
let mounted = true
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch(e) { if (mounted) error.value = e instanceof Error ? e.message : '操作失败，请稍后重试' }
 finally { if (mounted) busy.value = false }
}
function requestVerification() { return action(async () => { await api('/me/email-verification', {}); notice.value = '验证邮件请求已提交，请查看收件箱。' }) }
function confirmEmail() { return action(async () => {
 await api('/auth/email/confirm', { token: props.linkToken })
 notice.value = '邮箱已验证。'; emit('verified'); emit('consumed')
}) }
function requestRecovery() { return action(async () => {
 await api('/auth/password/forgot', { email: recoveryEmail.value })
 notice.value = '如果该邮箱已注册，你将收到密码重置邮件。请检查收件箱。'
}) }
function passwordMatches() { if (newPassword.value !== repeatPassword.value) throw new Error('两次输入的新密码不一致。') }
function resetPassword() { return action(async () => {
 passwordMatches()
 await api('/auth/password/reset', { token: props.linkToken, password: newPassword.value })
 newPassword.value = ''; repeatPassword.value = ''; emit('consumed'); emit('signedOut')
}) }
function changePassword() { return action(async () => {
 passwordMatches()
 await api('/me/password', { current_password: currentPassword.value, new_password: newPassword.value })
 currentPassword.value = ''; newPassword.value = ''; repeatPassword.value = ''; emit('signedOut')
}) }
function readMail() { return action(async () => { inbox.value = await api<DevelopmentMail[]>('/me/development-mail'); inboxOpen.value = true }) }
function mailLink(body: string) {
 const match = body.match(/#account-action=(verify|reset)&token=([0-9a-f]{64})\b/)
 return match ? { action: match[1] as 'verify' | 'reset', token: match[2]! } : null
}
function openMail(mail: DevelopmentMail) { const link = mailLink(mail.body); if (link) { inbox.value = []; inboxOpen.value = false; emit('mailAction', link.action, link.token) } }
onBeforeUnmount(() => { mounted = false; inbox.value = []; currentPassword.value = ''; newPassword.value = ''; repeatPassword.value = '' })
</script>

<template>
 <div class="page-heading"><div><p class="eyebrow">ACCOUNT SECURITY</p><h1>账号安全</h1><p>验证邮箱，管理密码和账号恢复。</p></div></div>
 <div v-if="error" role="alert" class="alert error">{{ error }}</div>
 <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
 <section v-if="linkAction" class="panel account-action">
  <h2>{{ linkAction === 'verify' ? '确认邮箱验证' : '设置新密码' }}</h2>
  <p class="muted">{{ linkAction === 'verify' ? '确认后将验证这封邮件所对应的邮箱。链接过期或已使用时，请重新申请。' : '设置新密码后，所有已登录设备会退出，需要重新登录。' }}</p>
  <NButton v-if="linkAction === 'verify'" type="primary" :loading="busy" @click="confirmEmail">确认验证邮箱</NButton>
  <form v-else @submit.prevent="resetPassword">
   <label>新密码<NInput v-model:value="newPassword" type="password" show-password-on="click" :input-props="{ required: true, minlength: 12, autocomplete: 'new-password', 'aria-label': '重置新密码' }" placeholder="12–72 字节" /></label>
   <label>再次输入新密码<NInput v-model:value="repeatPassword" type="password" :input-props="{ required: true, minlength: 12, autocomplete: 'new-password', 'aria-label': '确认重置新密码' }" /></label>
   <NButton type="primary" attr-type="submit" :loading="busy">重置密码并退出所有设备</NButton>
  </form>
 </section>
 <div v-if="user && !linkAction" class="security-grid">
  <section class="panel">
   <div class="section-title"><h2>邮箱验证</h2><NTag :type="verified ? 'success' : 'warning'" size="small" round>{{ verified ? '邮箱已验证' : '邮箱未验证' }}</NTag></div>
   <p class="account-email-value">{{ user.email }}</p>
   <p class="muted">{{ verified ? '此邮箱可用于账号恢复。' : verificationRequired ? '验证邮箱后才能购买套餐和获取订阅，请先完成验证。' : '完成验证可确认邮箱归属，并用于账号恢复。' }}</p>
   <NButton v-if="!verified" type="primary" :loading="busy" @click="requestVerification">发送验证邮件</NButton>
  </section>
  <section class="panel">
   <div class="section-title"><h2>修改密码</h2><span class="subtle">PASSWORD</span></div>
   <form @submit.prevent="changePassword">
    <label>当前密码<NInput v-model:value="currentPassword" type="password" :input-props="{ required: true, autocomplete: 'current-password', 'aria-label': '当前密码' }" /></label>
    <label>新密码<NInput v-model:value="newPassword" type="password" show-password-on="click" :input-props="{ required: true, minlength: 12, autocomplete: 'new-password', 'aria-label': '修改新密码' }" placeholder="12–72 字节" /></label>
    <label>再次输入新密码<NInput v-model:value="repeatPassword" type="password" :input-props="{ required: true, minlength: 12, autocomplete: 'new-password', 'aria-label': '确认修改新密码' }" /></label>
    <p class="muted">修改后所有设备会退出登录。已导入的代理连接凭据可在“我的订阅”中单独重置。</p>
    <NButton attr-type="submit" type="primary" :loading="busy">修改密码并重新登录</NButton>
   </form>
  </section>
 </div>
 <section v-else-if="!linkAction" class="panel recovery-panel">
  <h2>找回密码</h2><p class="muted">输入注册邮箱，我们会向符合条件的账号发送重置链接。</p>
  <form @submit.prevent="requestRecovery"><label>注册邮箱<NInput v-model:value="recoveryEmail" :input-props="{ type: 'email', required: true, autocomplete: 'email', 'aria-label': '找回密码邮箱' }" /></label><div class="security-actions"><NButton attr-type="submit" type="primary" :loading="busy">发送重置邮件</NButton><NButton :disabled="busy" @click="emit('login')">返回登录</NButton></div></form>
 </section>
 <section v-if="user && developmentMail && !linkAction" class="panel development-inbox">
  <div class="section-title"><h2>开发邮件收件箱</h2><NButton size="small" :loading="busy" @click="readMail">读取我的开发邮件</NButton></div>
  <p class="muted">仅开发环境提供，只显示当前账号的邮件。正式环境通过配置的邮件服务投递。</p>
  <template v-if="inboxOpen"><p v-if="!inbox.length" class="muted">暂无邮件。</p><article v-for="mail in inbox" :key="mail.id" class="mail-item"><h3>{{ mail.subject }}</h3><p class="muted">{{ new Date(mail.created_at).toLocaleString('zh-CN') }}</p><pre>{{ mail.body }}</pre><NButton v-if="mailLink(mail.body)" size="small" :disabled="busy" @click="openMail(mail)">打开邮件操作</NButton></article></template>
 </section>
</template>
<style scoped>
.security-grid{display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1.2fr);gap:24px;align-items:start}.account-action{margin-bottom:24px;max-width:700px}.panel h2{font-size:16px;font-weight:550}.account-email-value{overflow-wrap:anywhere;font-size:14px}.development-inbox{margin-top:24px}.mail-item{padding:18px 0;border-top:1px solid #e5ebe4}.mail-item h3{font-size:13px;font-weight:550}.mail-item pre{white-space:pre-wrap;overflow-wrap:anywhere;font:12px/1.8 monospace;color:#526a5d;background:#f5f8f1;padding:14px;border-radius:8px}.recovery-panel{max-width:650px}.security-actions{display:flex;gap:12px;flex-wrap:wrap}@media(max-width:800px){.security-grid{grid-template-columns:1fr}}
</style>
