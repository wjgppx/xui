<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NInput, NTag } from 'naive-ui'
import { api, APIError, type StaffMFAStatus } from './api'

const emit = defineEmits<{ verified: []; disabled: [] }>()
const status = ref<StaffMFAStatus | null>(null), busy = ref(false), error = ref(''), notice = ref('')
const password = ref(''), confirmCode = ref(''), verificationCode = ref(''), disablePassword = ref(''), disableCode = ref('')
const enrollment = ref<{ secret: string; otpauth_url: string; expires_at: string } | null>(null), recoveryCodes = ref<string[]>([])
const now = ref(Date.now())
let alive = true, generation = 0
const clock = window.setInterval(() => { now.value = Date.now() }, 1000)
const verified = computed(() => !!status.value?.verified_until && new Date(status.value.verified_until).getTime() > now.value)
const setupExpired = computed(() => !!enrollment.value && new Date(enrollment.value.expires_at).getTime() <= now.value)
const time = (value: string) => new Date(value).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })
const validCode = (value: string) => /^\d{6}$/.test(value) || /^[0-9a-f]{32}$/.test(value)
function clearCredentials() { password.value = ''; confirmCode.value = ''; verificationCode.value = ''; disablePassword.value = ''; disableCode.value = '' }
function clearSensitive() { clearCredentials(); enrollment.value = null; recoveryCodes.value = [] }
async function readStatus() {
 const token = generation
 try {
  const next = await api<StaffMFAStatus>('/me/mfa')
  if (alive && token === generation) {
   status.value = next
   if (!next.enabled) recoveryCodes.value = []
   if (next.enabled || !next.setup_pending) { enrollment.value = null; confirmCode.value = '' }
  }
 }
 catch (e) {
  if (!alive || token !== generation) return
  if (e instanceof APIError && e.status === 404) status.value = { available: false, enabled: false, required: false, verified_until: null, setup_pending: false, recovery_codes_remaining: 0 }
  else throw e
 }
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() }
 catch (e) { if (alive) error.value = e instanceof Error ? e.message : '二次验证暂时无法处理，请稍后重试。' }
 finally { clearCredentials(); if (alive) busy.value = false }
}
function refresh() { return action(readStatus) }
function beginSetup() { return action(async () => {
 enrollment.value = null; recoveryCodes.value = []
 const next = await api<{ secret: string; otpauth_url: string; expires_at: string }>('/me/mfa/setup', { current_password: password.value })
 if (!alive) return
 enrollment.value = next
 notice.value = '请将绑定密钥添加到验证器，再输入当前动态验证码完成启用。'
}) }
function confirmSetup() { return action(async () => {
 if (!/^\d{6}$/.test(confirmCode.value)) throw new Error('请输入验证器显示的 6 位动态验证码。')
 const result = await api<{ recovery_codes: string[] }>('/me/mfa/confirm', { code: confirmCode.value })
 if (!alive) return
 enrollment.value = null; recoveryCodes.value = result.recovery_codes
 status.value = { available: true, enabled: true, required: true, verified_until: null, setup_pending: false, recovery_codes_remaining: result.recovery_codes.length }
 emit('verified')
 await readStatus()
 if (alive) notice.value = '后台二次验证已启用，其他设备已退出。请离线安全保存下方恢复码。'
}) }
function verify() { return action(async () => {
 if (!validCode(verificationCode.value)) throw new Error('请输入 6 位动态验证码或 32 位恢复码。')
 await api('/me/mfa/verify', { code: verificationCode.value })
 if (!alive) return
 await readStatus(); emit('verified')
 notice.value = '后台二次验证已完成，可以继续管理操作。'
}) }
function disable() { return action(async () => {
 if (!validCode(disableCode.value)) throw new Error('请输入 6 位动态验证码或 32 位恢复码。')
 await api('/me/mfa/disable', { current_password: disablePassword.value, code: disableCode.value })
 if (!alive) return
 clearSensitive(); emit('disabled')
}) }
function cancelSetup() { enrollment.value = null; confirmCode.value = ''; notice.value = '' }
onMounted(refresh)
onBeforeUnmount(() => { alive = false; generation++; window.clearInterval(clock); clearSensitive(); status.value = null })
</script>

<template>
 <section class="panel staff-mfa" data-testid="staff-mfa">
  <div class="section-title"><h2>后台二次验证</h2><div class="mfa-actions"><NTag v-if="status" size="small" :type="status.enabled ? 'success' : 'warning'">{{ status.enabled ? '已启用验证器' : '尚未设置验证器' }}</NTag><NButton size="small" :loading="busy" @click="refresh">刷新二次验证状态</NButton></div></div>
  <p class="muted">后台操作使用验证器动态码或一次性恢复码进行二次验证，有效期为 15 分钟。密码重置不会移除验证器。</p>
  <div v-if="error" class="alert error" role="alert">{{ error }}</div>
  <div v-if="notice" class="alert success" role="status">{{ notice }}</div>
  <p v-if="!status && busy" class="muted" role="status">正在读取二次验证状态…</p>
  <template v-if="status">
   <p v-if="status.required && !status.enabled" class="alert mfa-warning" role="status">当前后台要求设置二次验证。完成绑定后才能使用管理功能。</p>
   <p v-if="!status.available" class="alert mfa-warning" role="status">二次验证服务尚未配置或当前不可用，请联系部署管理员检查加密密钥配置。已绑定的验证器不会因此被移除。</p>
   <template v-if="status.available">
    <template v-if="!status.enabled">
     <form v-if="!enrollment" @submit.prevent="beginSetup">
      <label>绑定验证器前确认密码<NInput v-model:value="password" type="password" :disabled="busy" :input-props="{ required: true, minlength: 12, maxlength: 72, autocomplete: 'current-password', 'aria-label': '绑定验证器密码' }" /></label>
      <NButton type="primary" attr-type="submit" :loading="busy">开始绑定验证器</NButton>
     </form>
     <div v-else class="mfa-enrollment">
      <p class="muted">在兼容 TOTP 的验证器中手动添加下面的绑定密钥。此密钥仅在本次绑定页面展示，不会写入浏览器存储。</p>
      <code class="mfa-secret" data-testid="mfa-enrollment-secret">{{ enrollment.secret }}</code>
      <details><summary>查看验证器绑定 URI</summary><code class="mfa-uri">{{ enrollment.otpauth_url }}</code></details>
      <p v-if="setupExpired" class="alert mfa-warning" role="alert">本次绑定已过期，请重新开始绑定。</p><p v-else class="muted">绑定有效期至 {{ time(enrollment.expires_at) }}（北京时间）。</p>
      <form @submit.prevent="confirmSetup"><label>验证器当前动态码<NInput v-model:value="confirmCode" type="password" :disabled="busy || setupExpired" :input-props="{ required: true, minlength: 6, maxlength: 6, inputmode: 'numeric', autocomplete: 'one-time-code', 'aria-label': '绑定动态验证码' }" /></label><div class="mfa-actions"><NButton type="primary" attr-type="submit" :loading="busy" :disabled="setupExpired">确认启用二次验证</NButton><NButton :disabled="busy" @click="cancelSetup">清除本次绑定信息</NButton></div></form>
     </div>
    </template>
    <template v-else>
     <p v-if="verified" class="alert mfa-success" role="status">本设备已完成二次验证，有效期至 {{ time(status.verified_until!) }}（北京时间）。</p>
     <p v-else class="alert mfa-warning" role="status">本设备尚未完成有效的后台二次验证，请输入动态码或未使用的恢复码。</p>
     <p class="muted">剩余可用恢复码 {{ status.recovery_codes_remaining }} 个。每个恢复码只能使用一次；动态验证码也不能重复提交。</p>
     <form @submit.prevent="verify"><label>验证码或恢复码<NInput v-model:value="verificationCode" type="password" show-password-on="click" :disabled="busy" :input-props="{ required: true, minlength: 6, maxlength: 32, autocomplete: 'one-time-code', 'aria-label': '后台验证码或恢复码' }" /></label><NButton type="primary" attr-type="submit" :loading="busy">验证后台操作</NButton></form>
     <details class="mfa-disable"><summary>停用后台二次验证</summary><p class="muted">停用需要当前密码和有效验证码，随后所有设备退出。如果部署要求二次验证，重新登录后仍需再次绑定。</p><form @submit.prevent="disable"><label>当前密码<NInput v-model:value="disablePassword" type="password" :disabled="busy" :input-props="{ required: true, minlength: 12, maxlength: 72, autocomplete: 'current-password', 'aria-label': '停用验证器密码' }" /></label><label>验证码或恢复码<NInput v-model:value="disableCode" type="password" :disabled="busy" :input-props="{ required: true, minlength: 6, maxlength: 32, autocomplete: 'one-time-code', 'aria-label': '停用验证码或恢复码' }" /></label><NButton attr-type="submit" :loading="busy">停用验证器并退出所有设备</NButton></form></details>
    </template>
   </template>
  </template>
  <div v-if="recoveryCodes.length" class="mfa-recovery" data-testid="mfa-recovery-codes">
   <h3>一次性恢复码</h3><p>请离线安全保存。离开本页或隐藏后不会再次展示，每个恢复码只能使用一次。</p>
   <ol><li v-for="item in recoveryCodes" :key="item"><code>{{ item }}</code></li></ol>
   <NButton @click="recoveryCodes = []">我已保存，隐藏恢复码</NButton>
  </div>
 </section>
</template>

<style scoped>
.staff-mfa{margin-top:24px}.staff-mfa h2{font-size:16px;font-weight:550}.staff-mfa form{max-width:560px}.mfa-actions{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.mfa-warning{background:#f7f2df;color:#817443}.mfa-success{background:#eef7ee;color:#336247}.mfa-secret,.mfa-uri{display:block;overflow-wrap:anywhere;white-space:normal;padding:14px;background:#f5f8f1;border-radius:8px;font:12px/1.9 monospace;color:#34584a}.mfa-enrollment details,.mfa-disable{margin:18px 0}.mfa-disable summary,.mfa-enrollment summary{cursor:pointer;font-size:12px}.mfa-recovery{margin-top:24px;padding:18px;border:1px solid #d9e1d4;border-radius:10px;background:#f7faf3}.mfa-recovery h3{margin:0;font-size:15px}.mfa-recovery p{font-size:12px;line-height:1.8}.mfa-recovery ol{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;padding-left:22px}.mfa-recovery li{overflow-wrap:anywhere;font-size:11px}.staff-mfa .section-title{gap:14px;flex-wrap:wrap}@media(max-width:760px){.mfa-recovery ol{grid-template-columns:1fr}.mfa-actions{align-items:flex-start}}
</style>
