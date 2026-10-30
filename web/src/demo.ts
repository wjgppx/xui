// This adapter is imported only by an explicit VITE_DEMO_MODE=true build.
// State is per page, never persisted, and no credentials or network are used.
import { APIError, type Entitlement, type Order, type Plan, type User } from './api'

export type DemoRole = 'user' | 'admin'
const minute = 60_000, day = 24 * 60 * minute
const permissions = ['overview.read', 'orders.read', 'plans.write', 'nodes.read', 'access.read', 'payments.read', 'refunds.read', 'funds.read', 'statements.read', 'users.read']
const now = () => new Date().toISOString()
const copy = <T>(value: T): T => structuredClone(value)
const unavailable = () => new APIError('此操作需要真实后端。GitHub Pages 演示不会发送邮件、注册节点、处理真实资金或变更账号安全设置。', 501)
function initialPlans(): Plan[] {
 return [
  { id: 'demo-light', name: '轻量连接', description: '适合日常浏览与轻量办公的演示套餐。', price_minor: 1900, currency: 'CNY', duration_days: 30, quota_bytes: 100 * 1024 ** 3 },
  { id: 'demo-everyday', name: '日常连接', description: '为学习、工作和多设备准备的演示套餐。', price_minor: 3900, currency: 'CNY', duration_days: 30, quota_bytes: 300 * 1024 ** 3 },
  { id: 'demo-plus', name: '进阶连接', description: '用于体验更大周期额度的演示套餐。', price_minor: 6900, currency: 'CNY', duration_days: 30, quota_bytes: 600 * 1024 ** 3 },
 ]
}
let role: DemoRole | null = null, plans = initialPlans(), orders: Order[] = [], entitlements: (Entitlement & { user_id: string })[] = [], sequence = 0
function profile(): User {
 if (!role) throw new APIError('请选择上方的一键体验入口。', 401)
 return { id: `demo-${role}`, email: `${role}@demo.example.invalid`, role: role === 'admin' ? 'admin' : 'user', permissions: role === 'admin' ? [...permissions] : [], email_verified_at: now(), mfa_enabled: false, mfa_required: false, mfa_verified_until: null }
}
export function setDemoRole(value: DemoRole | 'reset') {
 if (value === 'reset') { role = null; plans = initialPlans(); orders = []; entitlements = []; sequence = 0 }
 else role = value
}
function requireAdmin() { if (profile().role !== 'admin') throw new APIError('请先选择体验后台。', 403) }

export async function demoRequest<T>(path: string, body?: unknown): Promise<T> {
 // Yield like the real API without making a request, retaining component flows.
 await Promise.resolve()
 const url = new URL(path, 'https://demo.example.invalid'), route = url.pathname
 const reading = body === undefined, data = (body ?? {}) as Record<string, unknown>
 let result: unknown
 if (route === '/config' && reading) result = { development_payments: true, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: false }
 else if (route === '/plans' && reading) result = plans
 else if (route === '/payment-methods' && reading) result = []
 else if (route === '/me' && reading) result = profile()
 else if (route === '/auth/logout' && !reading) { role = null; result = { ok: true } }
 else {
  const user = profile()
  if (route.startsWith('/admin/')) requireAdmin()
  if (route === '/orders' && reading) result = orders.filter(o => o.user_id === user.id)
  else if (route === '/me/entitlements' && reading) result = entitlements.filter(e => e.user_id === user.id)
  else if (route === '/orders' && !reading) {
   const plan = plans.find(p => p.id === data.plan_id)
   if (!plan) throw new APIError('请选择存在的演示套餐。', 400)
   const order: Order = { ...plan, id: `demo-order-${++sequence}`, plan_id: plan.id, plan_name: plan.name, user_id: user.id, status: 'pending', created_at: now(), paid_at: null, expires_at: new Date(Date.now() + 30 * minute).toISOString(), closed_at: null }
   orders.unshift(order); result = order
  } else if (/^\/(?:dev\/)?orders\/[^/]+\/(?:settle|close)$/.test(route) && !reading) {
   const id = route.split('/').at(-2), order = orders.find(o => o.id === id && o.user_id === user.id)
   if (!order) throw new APIError('演示订单不存在。', 404)
   if (order.status !== 'pending') throw new APIError('该演示订单已处理。', 409)
   if (route.endsWith('/close')) { order.status = 'closed'; order.closed_at = now() }
   else {
    order.status = 'paid'; order.paid_at = now()
    const lastEnd = Math.max(Date.now(), ...entitlements.filter(e => e.user_id === user.id).map(e => Date.parse(e.ends_at)))
    entitlements.push({ id: `demo-period-${++sequence}`, user_id: user.id, order_id: order.id, plan_name: order.plan_name, starts_at: new Date(lastEnd).toISOString(), ends_at: new Date(lastEnd + order.duration_days * day).toISOString(), quota_bytes: order.quota_bytes, used_bytes: 0, revoked_at: null })
   }
   result = { ok: true }
  } else if (route === '/admin/overview' && reading) result = { users: 2, orders: orders.length, paid_minor: orders.filter(o => o.status === 'paid').reduce((sum, o) => sum + o.price_minor, 0), pending_events: 0 }
  else if (route === '/admin/orders' && reading) result = orders
  else if (route === '/admin/plans' && !reading) {
   if (typeof data.name !== 'string' || !data.name.trim() || !Number.isInteger(data.price_minor) || Number(data.price_minor) <= 0 || !Number.isInteger(data.duration_days) || Number(data.duration_days) < 1 || !Number.isInteger(data.quota_bytes) || Number(data.quota_bytes) < 1) throw new APIError('请填写有效的演示套餐信息。', 400)
   const plan: Plan = { id: `demo-plan-${++sequence}`, name: data.name.trim(), description: String(data.description ?? ''), price_minor: Number(data.price_minor), currency: 'CNY', duration_days: Number(data.duration_days), quota_bytes: Number(data.quota_bytes) }
   plans.push(plan); result = plan
  } else if (route === '/admin/nodes' && reading) result = [{ id: 'demo-node-offline', name: '演示节点（未连接服务器）', enrolled: false, revoked: false, desired_revision: 0, observed_revision: 0, agent_version: '', last_seen_at: null, applied_revision: 0, apply_status: 'pending', apply_error: '' }]
  else if (route === '/admin/access' && reading) result = { groups: [], nodes: [{ id: 'demo-node-offline', name: '演示节点（未连接服务器）' }], plans: plans.map(p => ({ id: p.id, name: p.name, group_ids: [], revision: 0 })) }
  else if (route === '/admin/users' && reading) result = (['user', 'admin'] as const).map(r => ({ id: `demo-${r}`, email: `${r}@demo.example.invalid`, role: r, email_verified_at: now(), created_at: now() }))
  else if (reading && ['/admin/payments', '/admin/refunds', '/admin/statements', '/admin/fund-ledgers', '/admin/fund-reconciliations', '/admin/bill-downloads', '/admin/bill-schedules'].includes(route)) result = { items: [] }
  else if (reading && ['/admin/events', '/admin/payment-activity'].includes(route)) result = []
  else throw unavailable()
 }
 return copy(result) as T
}
