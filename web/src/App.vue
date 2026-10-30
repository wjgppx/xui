<script setup lang="ts">
import { computed, defineAsyncComponent, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { NButton, NConfigProvider, NInput, NInputNumber, NModal, NTag } from 'naive-ui'
import { api, APIError, type User, type Plan, type Order, type Entitlement, type Overview, type PaymentMethod } from './api'
import NodeManager from './NodeManager.vue'
import SubscriptionManager from './SubscriptionManager.vue'
import AccessManager from './AccessManager.vue'
import AccountSecurity from './AccountSecurity.vue'
import StaffManager from './StaffManager.vue'
import PaymentCheckout from './PaymentCheckout.vue'
const PaymentOperations = defineAsyncComponent(() => import('./PaymentOperations.vue'))
const RefundOperations = defineAsyncComponent(() => import('./RefundOperations.vue'))
const MerchantStatements = defineAsyncComponent(() => import('./MerchantStatements.vue'))
const FundLedgers = defineAsyncComponent(() => import('./FundLedgers.vue'))
const demoMode = import.meta.env.VITE_DEMO_MODE === 'true'
const DemoToolbar = import.meta.env.VITE_DEMO_MODE === 'true' ? defineAsyncComponent(() => import('./DemoToolbar.vue')) : null
const DemoSecurity = import.meta.env.VITE_DEMO_MODE === 'true' ? defineAsyncComponent(() => import('./DemoSecurity.vue')) : null

type View = 'overview' | 'plans' | 'orders' | 'subscription' | 'admin' | 'nodes' | 'access' | 'security' | 'staff' | 'payments' | 'refunds' | 'statements' | 'funds'
const view = ref<View>('overview'), user = ref<User | null>(null), plans = ref<Plan[]>([]), orders = ref<Order[]>([]), entitlements = ref<Entitlement[]>([])
const paymentMethods = ref<PaymentMethod[]>([]), paymentMethodsError = ref(''), paymentOrder = ref<Order | null>(null)
const developmentMail = ref(false), verificationRequired = ref(false), paymentActionsEnabled = ref(false), billDownloadsEnabled = ref(false)
const linkAction = ref<'verify' | 'reset' | null>(null), linkToken = ref('')
const now = ref(Date.now()), mfaForced = ref('')
const mfaLocked = computed(() => {
 if (!user.value || user.value.role === 'user') return false
 const until = user.value.mfa_verified_until ? new Date(user.value.mfa_verified_until).getTime() : NaN
 return !!mfaForced.value || (user.value.mfa_required === true && (!Number.isFinite(until) || until <= now.value))
})
const mfaEnrollmentRequired = computed(() => mfaForced.value === 'mfa_enrollment_required' || user.value?.mfa_enabled === false)
const can = (permission: string) => !!user.value && !mfaLocked.value && (!verificationRequired.value || !!user.value.email_verified_at) && user.value.permissions?.includes(permission)
const staff = computed(() => !!user.value && user.value.role !== 'user')
const allOrders = ref<Order[]>([]), overview = ref<Overview | null>(null), development = ref(false), loading = ref(true), busy = ref(false)
const adminViews: View[] = ['admin', 'nodes', 'access', 'staff', 'payments', 'refunds', 'statements', 'funds']
watch(mfaLocked, locked => {
 if (!locked) return
 overview.value = null; allOrders.value = []; planOpen.value = false
 if (adminViews.includes(view.value)) view.value = 'security'
})
const error = ref(''), notice = ref(''), authOpen = ref(false), registering = ref(false), email = ref(''), password = ref('')
const planOpen = ref(false), planName = ref(''), planDescription = ref(''), planPrice = ref(19), planDays = ref(30), planQuota = ref(100)
const active = computed(() => entitlements.value.find(e => !e.revoked_at && new Date(e.starts_at).getTime() <= Date.now() && new Date(e.ends_at).getTime() > Date.now()))
const future = computed(() => entitlements.value.filter(e => !e.revoked_at && new Date(e.starts_at).getTime() > Date.now()))
const usedPercent = computed(() => active.value ? Math.min(100, active.value.used_bytes / active.value.quota_bytes * 100) : 0)
const usage = (v: number) => { const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']; const i = v > 0 ? Math.min(units.length - 1, Math.floor(Math.log(v) / Math.log(1024))) : 0; return `${(v / 1024 ** i).toLocaleString('zh-CN', { maximumFractionDigits: 2 })} ${units[i]}` }
const money = (v: number) => (v / 100).toFixed(2)
const gib = (v: number) => (v / 1024 ** 3).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
const date = (v: string) => new Date(v).toLocaleDateString('zh-CN')
const orderClock = window.setInterval(() => { now.value = Date.now() }, 1000)
onBeforeUnmount(() => window.clearInterval(orderClock))
const orderStatus = (order: Order): Order['status'] => order.status === 'pending' && order.expires_at && new Date(order.expires_at).getTime() <= now.value ? 'expired' : order.status
const orderStatusLabel = (order: Order) => ({ pending: '待支付', expired: '已超时', closing: '渠道关单中', closed: '已关闭', paid: '已开通', refunded: '已退款' }[orderStatus(order)])
function closeOrder(order: Order) {
 if (!window.confirm('仅可关闭尚未发起外部付款的订单。关闭后无法继续使用此订单付款，是否继续？')) return
 return action(async () => { await api(`/orders/${encodeURIComponent(order.id)}/close`, {}); await personal(); notice.value = '订单已关闭。' })
}
const labels: Record<View, string> = { overview: '我的概览', plans: '探索套餐', orders: '我的订单', subscription: '我的订阅', admin: '运营工作台', nodes: '节点管理', access: '权限管理', security: '账号安全', staff: '后台账号', payments: '支付流水', refunds: '退款管理', statements: '商户账单', funds: '资金流水' }
const theme = { common: { primaryColor: '#21766b', primaryColorHover: '#185e55', primaryColorPressed: '#124d46', borderRadius: '10px', fontFamily: 'Inter, "PingFang SC", "Microsoft YaHei", sans-serif' } }

async function personal() {
 if (!user.value) return
 ;[orders.value, entitlements.value] = await Promise.all([api<Order[]>('/orders'), api<Entitlement[]>('/me/entitlements')])
 overview.value = null; allOrders.value = []
 const userID = user.value?.id
 await Promise.all([can('overview.read') ? api<Overview>('/admin/overview').then(v => { if (user.value?.id === userID && can('overview.read')) overview.value = v }) : Promise.resolve(), can('orders.read') ? api<Order[]>('/admin/orders').then(v => { if (user.value?.id === userID && can('orders.read')) allOrders.value = v }) : Promise.resolve()])
}
async function action(task: () => Promise<void>) {
 if (busy.value) return
 busy.value = true; error.value = ''; notice.value = ''
 try { await task() } catch (e) { error.value = e instanceof Error ? e.message : '操作失败' } finally { busy.value = false }
}
async function load() {
 loading.value = true; error.value = ''
 try {
  const [p, c] = await Promise.all([api<Plan[]>('/plans'), api<{ development_payments: boolean; development_mail: boolean; email_verification_required: boolean; payment_actions_enabled?: boolean; bill_downloads_enabled?: boolean }>('/config')]); plans.value = p; development.value = c.development_payments; developmentMail.value = c.development_mail; verificationRequired.value = c.email_verification_required; paymentActionsEnabled.value = c.payment_actions_enabled === true; billDownloadsEnabled.value = c.bill_downloads_enabled === true
  try { user.value = await api<User>('/me') } catch (e) { if (!(e instanceof APIError && e.status === 401)) throw new Error('账号服务暂时不可用'); user.value = null }
  await personal()
  await loadPaymentMethods()
 } catch (e) { error.value = e instanceof Error ? e.message : '加载失败' } finally { loading.value = false }
}
async function loadPaymentMethods() {
 paymentMethodsError.value = ''
 try { paymentMethods.value = await api<PaymentMethod[]>('/payment-methods') } catch { paymentMethods.value = []; paymentMethodsError.value = '暂时无法读取支付渠道，请刷新后重试。' }
}
function openAuth(register = false) { if (demoMode) { void switchDemo('user'); return }; registering.value = register; authOpen.value = true; error.value = '' }
async function switchDemo(role: 'user' | 'admin' | 'reset') {
 if (import.meta.env.VITE_DEMO_MODE !== 'true') return
 await action(async () => { clearSession(); authOpen.value = false; planOpen.value = false; email.value = ''; password.value = ''; const { setDemoRole } = await import('./demo'); setDemoRole(role); await load(); view.value = role === 'admin' ? 'admin' : 'overview' })
}
function authenticate() { return action(async () => { user.value = await api<User>(registering.value ? '/auth/register' : '/auth/login', { email: email.value, password: password.value }); password.value = ''; authOpen.value = false; await personal(); notice.value = '欢迎回来，你的空间已准备就绪。' }) }
function clearSession() { paymentOrder.value = null; user.value = null; orders.value = []; entitlements.value = []; overview.value = null; allOrders.value = []; linkAction.value = null; linkToken.value = ''; mfaForced.value = ''; view.value = 'overview' }
function logout() { return action(async () => { await api('/auth/logout', {}); clearSession() }) }
function signedOut() { clearSession(); notice.value = '密码已更新，请重新登录。' }
function mfaSignedOut() { clearSession(); notice.value = '后台二次验证已停用，所有设备已退出，请重新登录。' }
async function refreshProfile() {
 const userID = user.value?.id
 if (!userID) return
 try {
  const next = await api<User>('/me')
  if (user.value?.id !== userID) return
  user.value = next; mfaForced.value = ''; error.value = ''; await personal()
 } catch (e) { error.value = e instanceof Error ? e.message : '账号服务暂时不可用，请稍后刷新。' }
}
function requireMFA(event: Event) {
 if (!user.value || user.value.role === 'user') return
 const code = (event as CustomEvent<string>).detail
 if (code !== 'mfa_required' && code !== 'mfa_enrollment_required') return
 mfaForced.value = code; view.value = 'security'; overview.value = null; allOrders.value = []; planOpen.value = false; error.value = ''
}
window.addEventListener('xui:mfa-required', requireMFA)
onBeforeUnmount(() => window.removeEventListener('xui:mfa-required', requireMFA))
function consumeLink() { linkAction.value = null; linkToken.value = '' }
function useMailAction(action: 'verify' | 'reset', token: string) { notice.value = ''; error.value = ''; linkAction.value = action; linkToken.value = token; view.value = 'security' }
function openRecovery() { authOpen.value = false; view.value = 'security'; error.value = '' }
function readAccountFragment() {
 const values = new URLSearchParams(window.location.hash.slice(1)), action = values.get('account-action'), token = values.get('token') || ''
 if (!values.has('account-action') && !values.has('token')) return
 window.history.replaceState(null, '', window.location.pathname + window.location.search)
 if ((action === 'verify' || action === 'reset') && /^[0-9a-f]{64}$/.test(token)) useMailAction(action, token)
 else { consumeLink(); view.value = 'security'; notice.value = '邮件链接格式不正确，请重新申请。' }
}
readAccountFragment()
window.addEventListener('hashchange', readAccountFragment)
onBeforeUnmount(() => window.removeEventListener('hashchange', readAccountFragment))
function returnedFromPayment() {
 const url = new URL(window.location.href)
 if (!url.searchParams.has('payment_return')) return
 url.searchParams.delete('payment_return')
 window.history.replaceState(null, '', url.pathname + url.search + url.hash)
 view.value = 'orders'; notice.value = '已返回订单页面，请刷新订单查看实际支付结果。'
}
returnedFromPayment()
function openPayment(order: Order) {
 if (orderStatus(order) !== 'pending') { notice.value = '此订单已不能继续发起付款，请刷新查看实际状态。'; return }
 if (verificationRequired.value && !user.value?.email_verified_at) { view.value = 'security'; notice.value = '请先验证邮箱，再进行支付。'; return }
 paymentOrder.value = order
}
function paymentSettled() { return action(async () => { await personal(); notice.value = '支付已确认，套餐权益已开通。' }) }
function buy(plan: Plan) {
 if (!user.value) { openAuth(true); return }
 if (verificationRequired.value && !user.value.email_verified_at) { view.value = 'security'; notice.value = '请先验证邮箱，再购买套餐。'; return }
 return action(async () => { await api<Order>('/orders', { plan_id: plan.id }); await personal(); view.value = 'orders'; notice.value = '订单已创建，请确认订单后完成支付。' })
}
function settle(order: Order) { if (orderStatus(order) !== 'pending') return; return action(async () => { await api(`/dev/orders/${order.id}/settle`, {}); await personal(); notice.value = '测试支付已确认，套餐权益已开通。' }) }
function createPlan() { return action(async () => { await api('/admin/plans', { id: '', name: planName.value, description: planDescription.value, price_minor: Math.round(planPrice.value * 100), currency: 'CNY', duration_days: planDays.value, quota_bytes: planQuota.value * 1024 ** 3 }); plans.value = await api<Plan[]>('/plans'); planOpen.value = false; notice.value = '新套餐已上架。' }) }
onMounted(load)
</script>

<template>
 <NConfigProvider :theme-overrides="theme">
  <div class="shell">
   <aside class="sidebar" :class="{ 'admin-sidebar': staff }">
    <a class="brand" href="#" @click.prevent="view = 'overview'"><span class="brand-mark">x</span><span>xui<span class="brand-dot">.</span></span></a>
    <div class="workspace-label">YOUR WORKSPACE</div>
    <nav aria-label="主导航" :class="{ 'admin-navigation': !!user }">
     <button v-for="(item, index) in (['overview','plans','orders'] as View[])" :key="item" :class="{ selected: view === item }" @click="view = item"><span class="nav-icon">{{ ['◈','▦','≡'][index] }}</span>{{ labels[item] }}<span v-if="view === item" class="nav-dot"></span></button>
     <button v-if="user" :class="{ selected: view === 'subscription' }" @click="view = 'subscription'"><span class="nav-icon">↗</span>我的订阅<span v-if="view === 'subscription'" class="nav-dot"></span></button>
     <button v-if="user" :class="{ selected: view === 'security' }" @click="view = 'security'"><span class="nav-icon">◇</span>账号安全</button>
     <button v-if="can('orders.read') || can('overview.read') || can('plans.write')" :class="{ selected: view === 'admin' }" @click="view = 'admin'"><span class="nav-icon">⌘</span>运营工作台</button>
     <button v-if="can('nodes.read')" :class="{ selected: view === 'nodes' }" @click="view = 'nodes'"><span class="nav-icon">◎</span>节点管理</button>
     <button v-if="can('access.read')" :class="{ selected: view === 'access' }" @click="view = 'access'"><span class="nav-icon">▧</span>权限管理</button>
     <button v-if="can('payments.read')" :class="{ selected: view === 'payments' }" @click="view = 'payments'"><span class="nav-icon">¥</span>支付流水</button>
     <button v-if="can('refunds.read')" :class="{ selected: view === 'refunds' }" @click="view = 'refunds'"><span class="nav-icon">↶</span>退款管理</button>
     <button v-if="can('funds.read')" :class="{ selected: view === 'funds' }" @click="view = 'funds'"><span class="nav-icon">⇄</span>资金流水</button>
     <button v-if="can('statements.read')" :class="{ selected: view === 'statements' }" @click="view = 'statements'"><span class="nav-icon">▤</span>商户账单</button>
     <button v-if="can('users.read')" :class="{ selected: view === 'staff' }" @click="view = 'staff'"><span class="nav-icon">♧</span>后台账号</button>
    </nav>
    <div class="sidebar-note"><span class="mini-orbit">◎</span><h3>连接，每一种可能。</h3><p>让每一次连接，<br/>都有清晰可见的状态。</p></div>
    <div class="sidebar-footer"><span class="status-dot"></span> XUI · 开发版本 <span>0.16</span></div>
   </aside>
   <main>
    <header class="topbar"><span class="breadcrumb">个人空间 <span>/</span> <strong>{{ labels[view] }}</strong></span><div class="account"><template v-if="user"><span class="avatar">{{ user.email[0]?.toUpperCase() }}</span><span class="account-email">{{ user.email }}</span><NButton text @click="logout" :disabled="busy">退出</NButton></template><template v-else><NButton text @click="openAuth()">登录</NButton><NButton type="primary" @click="openAuth(true)">创建账号 ↗</NButton></template></div></header>
    <div class="content">
     <component :is="DemoToolbar" v-if="demoMode" :busy="busy || loading" @select="switchDemo" />
     <div v-if="development && !demoMode" class="dev-banner"><span>开发体验环境</span> 测试支付不会扣款；支付宝、微信等支付渠道以收银台为准。</div>
     <div v-if="error && !authOpen && !planOpen" role="alert" class="alert error">{{ error }} <button @click="load">重新加载</button></div>
     <div v-if="notice" role="status" class="alert success">{{ notice }}</div>
     <div v-if="mfaLocked" role="status" class="alert mfa-required-banner">{{ mfaEnrollmentRequired ? '后台需要先设置验证器，完成二次验证后才能使用管理功能。' : '后台二次验证尚未完成或已过期，请在账号安全中验证。' }} <NButton size="small" @click="view = 'security'">前往后台二次验证</NButton></div>
     <div v-if="loading" class="loading">正在准备你的空间…</div>
     <template v-else>
      <component :is="DemoSecurity" v-if="demoMode && (view === 'security' || view === 'subscription')" :subscription="view === 'subscription'" />
      <AccountSecurity v-if="!demoMode && view === 'security'" :user="user" :development-mail="developmentMail" :verification-required="verificationRequired" :link-action="linkAction" :link-token="linkToken" @verified="refreshProfile" @signed-out="signedOut" @mfa-disabled="mfaSignedOut" @consumed="consumeLink" @login="openAuth()" @mail-action="useMailAction" />
      <PaymentOperations v-if="view === 'payments' && can('payments.read')" :can-retry="can('payments.retry')" :can-request-refund="can('refunds.request')" :can-close="can('payments.close')" :actions-enabled="paymentActionsEnabled" />
      <RefundOperations v-if="view === 'refunds' && can('refunds.read')" :can-approve="can('refunds.approve')" :actions-enabled="paymentActionsEnabled" />
      <FundLedgers v-if="view === 'funds' && can('funds.read')" :can-import="can('funds.import')" :can-reconcile="can('funds.reconcile')" />
      <MerchantStatements v-if="view === 'statements' && can('statements.read')" :can-import="can('statements.import')" :can-reconcile="can('statements.reconcile')" :can-review="can('statements.review')" :can-download="can('statements.download')" :can-schedule="can('statements.schedule')" :bill-downloads-enabled="billDownloadsEnabled" />
      <StaffManager v-if="view === 'staff' && can('users.read')" :can-manage-roles="can('users.roles')" />
      <NodeManager v-if="view === 'nodes' && can('nodes.read')" />
      <AccessManager v-if="view === 'access' && can('access.read')" />
      <SubscriptionManager v-if="!demoMode && view === 'subscription' && user" :key="user.id" />
      <template v-if="view === 'overview'">
       <div class="page-heading"><div><p class="eyebrow">OVERVIEW</p><h1>{{ user ? '欢迎回来。' : '连接，从这里开始。' }}</h1><p>你的套餐、用量和订单，一目了然。</p></div><span class="date-pill">{{ new Date().toLocaleDateString('zh-CN', { month: 'long', day: 'numeric' }) }}</span></div>
       <section class="hero"><div><span class="hero-label">YOUR NEXT CONNECTION</span><h2>为工作与生活，<br/>留出更多可能。</h2><p>选择适合自己的套餐，<br class="mobile-break"/>在一个空间管理每次连接。</p><NButton color="#e5efb7" text-color="#183b35" size="large" @click="view = 'plans'">探索套餐 <span class="button-arrow">↗</span></NButton></div><div class="orbit-art" aria-hidden="true"><div class="orbit one"></div><div class="orbit two"></div><div class="orbit three"></div><span class="orbit-core">x</span><span class="satellite s1"></span><span class="satellite s2"></span><span class="orbit-caption">A LITTLE MORE POSSIBILITY.</span></div></section>
       <div class="stats-grid"><article class="stat"><p>当前套餐 <span>◈</span></p><h3>{{ active?.plan_name || '尚未开通' }}</h3><small>{{ active ? `有效期至 ${date(active.ends_at)}` : '找到适合你的连接方案' }}</small></article><article class="stat"><p>已入账用量 <span>↗</span></p><h3>{{ active ? usage(active.used_bytes) : '—' }}</h3><small>{{ active ? `总额度 ${gib(active.quota_bytes)} GiB` : '开通后查看当前周期用量' }}</small></article><article class="stat"><p>我的订单 <span>≡</span></p><h3>{{ orders.length }} <em>笔</em></h3><small>{{ orders.filter(o => orderStatus(o) === 'pending').length }} 笔等待支付</small></article></div>
       <div class="detail-grid"><section class="panel"><div class="section-title"><h2>套餐权益</h2><NButton v-if="user" size="small" :loading="busy" @click="action(personal)">刷新用量</NButton><span v-else class="subtle">SUBSCRIPTION</span></div><template v-if="active"><div class="quota-row"><strong>{{ active.plan_name }}</strong><span>{{ usage(active.used_bytes) }} / {{ gib(active.quota_bytes) }} GiB</span></div><div class="progress-track" role="progressbar" aria-label="当前权益已入账用量" :aria-valuenow="Math.round(usedPercent)" aria-valuemin="0" aria-valuemax="100"><div :style="{ width: usedPercent + '%' }"></div></div><p class="muted">已入账用量包含上行与下行，采样和上报存在延迟。</p><p v-if="active.used_bytes > active.quota_bytes" class="muted">超出已购额度 {{ usage(active.used_bytes - active.quota_bytes) }}；已发生用量按实记录。</p><p class="muted">{{ date(active.starts_at) }} — {{ date(active.ends_at) }}</p><p v-if="future.length" class="muted">还有 {{ future.length }} 个已购买的后续周期。</p></template><div v-else class="empty-state"><span class="empty-symbol">◇</span><h3>你的旅程，即将开始</h3><p>{{ user ? '选择一个套餐，开通属于你的连接空间。' : '创建账号，开始管理你的套餐和订单。' }}</p><NButton @click="user ? view = 'plans' : openAuth(true)">{{ user ? '查看套餐' : '创建账号' }}</NButton></div></section><section class="panel getting-started"><div class="section-title"><h2>开始使用</h2><span>↗</span></div><div class="step"><span>01</span><div><h4>找到适合你的套餐</h4><p>按所需额度和使用周期选择。</p></div></div><div class="step"><span>02</span><div><h4>确认订单与开通</h4><p>订单确认后，权益自动记录。</p></div></div><div class="step"><span>03</span><div><h4>接入你的设备</h4><p>在“我的订阅”获取客户端配置。</p></div></div></section></div>
      </template>
      <template v-if="view === 'plans'">
       <div class="page-heading"><div><p class="eyebrow">FIND YOUR FIT</p><h1>为你，恰到好处。</h1><p>清晰的额度与周期，选择适合自己的节奏。</p></div></div>
       <div class="plans-grid"><article v-for="(plan, i) in plans" :key="plan.id" class="plan" :class="{ featured: i === 1 }"><div class="plan-top"><span class="plan-number">0{{ i + 1 }}</span><NTag v-if="i === 1" round size="small" type="success">日常之选</NTag></div><h2>{{ plan.name }}</h2><p class="plan-description">{{ plan.description }}</p><div class="price"><sup>¥</sup>{{ money(plan.price_minor) }}<small>/ {{ plan.duration_days }} 天</small></div><div class="plan-divider"></div><div class="plan-feature"><span>↗</span><strong>{{ gib(plan.quota_bytes) }} GiB</strong> 周期流量</div><div class="plan-feature"><span>◷</span>{{ plan.duration_days }} 天有效期</div><div class="plan-feature"><span>✓</span>订单与权益记录</div><NButton block size="large" :type="i === 1 ? 'primary' : 'default'" :loading="busy" @click="buy(plan)">选择 {{ plan.name }} <span class="button-arrow">→</span></NButton></article></div>
       <p class="footnote">续费将在现有权益结束后开始。1 GiB = 1024³ 字节。有权使用的已发布节点可在“我的订阅”中获取。</p>
      </template>
      <template v-if="view === 'orders'">
       <div class="page-heading"><div><p class="eyebrow">YOUR ORDERS</p><h1>每一笔，都有记录。</h1><p>查看订单进度与已开通的套餐。</p></div><div class="payment-actions-inline"><NButton v-if="user" :loading="busy" @click="action(async () => { await personal(); await loadPaymentMethods() })">刷新订单</NButton><NButton @click="view = 'plans'">探索套餐 ↗</NButton></div></div>
       <p v-if="paymentMethodsError" role="alert" class="muted">{{ paymentMethodsError }}</p>
       <section class="panel"><div v-if="!user || !orders.length" class="empty-state"><span class="empty-symbol">≡</span><h3>{{ user ? '还没有订单' : '登录后查看订单' }}</h3><p>你的购买记录会保存在这里。</p><NButton @click="user ? view = 'plans' : openAuth()">{{ user ? '选择套餐' : '登录账号' }}</NButton></div><div v-else class="table-wrap"><table><thead><tr><th>套餐 / 订单编号</th><th>金额</th><th>创建日期</th><th>状态</th><th>操作</th></tr></thead><tbody><tr v-for="o in orders" :key="o.id" :data-order-id="o.id"><td><strong>{{ o.plan_name }}</strong><small class="order-id">{{ o.id.slice(0, 12) }}</small><small v-if="o.expires_at && orderStatus(o) === 'pending'" class="order-id">付款截止 {{ new Date(o.expires_at).toLocaleString('zh-CN') }}</small></td><td>¥{{ money(o.price_minor) }}</td><td>{{ date(o.created_at) }}</td><td><NTag size="small" round :type="orderStatus(o) === 'paid' ? 'success' : orderStatus(o) === 'pending' ? 'warning' : 'default'">{{ orderStatusLabel(o) }}</NTag></td><td><div class="payment-actions-inline"><template v-if="orderStatus(o) === 'pending'"><NButton v-if="paymentMethods.length" size="small" type="primary" :disabled="busy" @click="openPayment(o)">去支付</NButton><NButton v-if="development" size="small" :type="paymentMethods.length ? 'default' : 'primary'" :loading="busy" @click="settle(o)">确认测试支付</NButton><span v-if="!paymentMethods.length && !development" class="muted">{{ paymentMethodsError ? '支付渠道暂不可用' : '支付渠道待配置' }}</span></template><NButton v-if="orderStatus(o) === 'pending' || orderStatus(o) === 'expired'" size="small" :disabled="busy" title="仅关闭尚未发起外部付款的订单" @click="closeOrder(o)">关闭订单</NButton><span v-else class="muted">{{ o.status === 'paid' ? '已完成' : o.status === 'refunded' ? '已退款' : o.status === 'closing' ? '等待渠道确认关单' : '订单已关闭' }}</span></div><p v-if="orderStatus(o) === 'expired'" class="muted order-expiry-note">已停止发起付款；已付款的订单仍会核实结果。</p></td></tr></tbody></table></div></section>
      </template>
      <template v-if="view === 'admin' && (can('orders.read') || can('overview.read') || can('plans.write'))">
       <div class="page-heading"><div><p class="eyebrow">OPERATIONS</p><h1>运营工作台</h1><p>查看平台业务记录，管理当前在售套餐。</p></div><NButton v-if="can('plans.write')" type="primary" @click="planOpen = true">＋ 新建套餐</NButton></div>
       <div class="stats-grid" v-if="overview"><article class="stat"><p>注册用户</p><h3>{{ overview.users }}</h3></article><article class="stat"><p>订单数量</p><h3>{{ overview.orders }}</h3></article><article class="stat"><p>{{ development ? '已确认测试金额' : '已确认订单金额' }}</p><h3>¥{{ money(overview.paid_minor) }}</h3></article></div>
       <section v-if="can('orders.read')" class="panel"><div class="section-title"><h2>最近订单</h2><span class="subtle">最近 100 条</span></div><div class="table-wrap"><table><thead><tr><th>订单</th><th>用户编号</th><th>套餐</th><th>金额</th><th>状态</th></tr></thead><tbody><tr v-for="o in allOrders" :key="o.id"><td>{{ o.id.slice(0, 12) }}</td><td>{{ o.user_id.slice(0, 12) }}</td><td>{{ o.plan_name }}</td><td>¥{{ money(o.price_minor) }}</td><td>{{ orderStatusLabel(o) }}</td></tr></tbody></table><p v-if="!allOrders.length" class="muted">暂无订单。</p></div></section>
      </template>
     </template>
     <footer class="page-footer"><span>© {{ new Date().getFullYear() }} xui. 用心连接。</span><span>让每一步，都清晰。</span></footer>
    </div>
   </main>
  </div>
  <PaymentCheckout v-if="paymentOrder && user" :key="paymentOrder.id" :order="paymentOrder" :methods="paymentMethods" @close="paymentOrder = null" @settled="paymentSettled" />
  <NModal v-model:show="authOpen" preset="card" :title="registering ? '创建你的账号' : '欢迎回来'" class="auth-modal" style="width: 420px; max-width: calc(100vw - 32px)">
   <form @submit.prevent="authenticate"><p class="muted">{{ registering ? '从这里开启你的连接空间。' : '登录，继续你的连接旅程。' }}</p><div v-if="error" role="alert" class="alert error">{{ error }}</div><label>邮箱<NInput v-model:value="email" type="text" placeholder="you@example.com" :input-props="{ type: 'email', autocomplete: 'email', required: true, 'aria-label': '邮箱' }" /></label><label>密码<NInput v-model:value="password" type="password" show-password-on="click" placeholder="至少 12 位字符" :input-props="{ autocomplete: registering ? 'new-password' : 'current-password', required: true, minlength: 12, 'aria-label': '密码' }" /></label><NButton type="primary" attr-type="submit" block size="large" :loading="busy">{{ registering ? '创建账号' : '登录' }}</NButton><button v-if="!registering" type="button" class="switch-auth" @click="openRecovery">忘记密码？</button><button type="button" class="switch-auth" @click="registering = !registering; error = ''">{{ registering ? '已有账号？前往登录' : '还没有账号？立即创建' }}</button></form>
  </NModal>
  <NModal v-model:show="planOpen" preset="card" title="新建套餐" style="width: 480px; max-width: calc(100vw - 32px)"><form @submit.prevent="createPlan"><div v-if="error" role="alert" class="alert error">{{ error }}</div><label>套餐名称<NInput v-model:value="planName" :input-props="{ required: true, 'aria-label': '套餐名称' }" /></label><label>描述<NInput v-model:value="planDescription" /></label><div class="form-row"><label>价格（元）<NInputNumber v-model:value="planPrice" :min="0.01" :max="1000000" :precision="2" /></label><label>周期（天）<NInputNumber v-model:value="planDays" :min="1" :max="366" :precision="0" /></label></div><label>额度（GiB）<NInputNumber v-model:value="planQuota" :min="1" :max="10240" :precision="0" /></label><NButton block attr-type="submit" type="primary" :loading="busy">创建并上架</NButton></form></NModal>
 </NConfigProvider>
</template>

<style scoped>
.order-expiry-note{font-size:12px;line-height:1.6;max-width:260px}
.payment-actions-inline{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
@media(min-width:761px){.sidebar.admin-sidebar{overflow-y:auto}}
@media(min-width:761px) and (max-height:950px){.sidebar.admin-sidebar .sidebar-note{display:none}.sidebar.admin-sidebar .sidebar-footer{margin-top:auto}}
@media(max-width:760px){nav.admin-navigation{flex-wrap:wrap;margin-left:0;width:100%}nav.admin-navigation button{flex:1 0 auto}}
</style>
