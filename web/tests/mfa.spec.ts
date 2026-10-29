import { test, expect, type Page } from '@playwright/test'

const password = 'mfa-fixture-password', secret = 'JBSWY3DPEHPK3PXP'
const recoveryCodes = Array.from({ length: 10 }, (_, index) => (index + 1).toString(16).padStart(32, '0'))
const future = () => new Date(Date.now() + 15 * 60 * 1000).toISOString()
async function fixture(page: Page, options: { enabled?: boolean; required?: boolean; verified?: boolean; available?: boolean; regular?: boolean } = {}) {
 const state = { enabled: options.enabled ?? false, required: options.required ?? true, verifiedUntil: options.verified ? future() : null as string | null, available: options.available ?? true, pending: false, loggedOut: false, failSetup: false, failConfirm: false, failVerify: false, failDisable: false, expireOnAdmin: false, adminGets: [] as string[], statusGets: 0, setupCalls: [] as Record<string, unknown>[], confirms: [] as Record<string, unknown>[], verifies: [] as Record<string, unknown>[], disables: [] as Record<string, unknown>[], used: new Set<string>() }
 const user = () => ({ id: 'mfa-admin', email: 'mfa-admin@example.test', role: options.regular ? 'user' : 'admin', permissions: options.regular ? [] : ['overview.read', 'orders.read', 'plans.write'], email_verified_at: '2026-10-29T00:00:00Z', mfa_enabled: state.enabled, mfa_required: !options.regular && (state.required || state.enabled), mfa_verified_until: state.verifiedUntil })
 const status = () => ({ available: state.available, enabled: state.enabled, required: state.required || state.enabled, verified_until: state.verifiedUntil, setup_pending: state.pending, recovery_codes_remaining: state.enabled ? recoveryCodes.length - state.used.size : 0 })
 await page.route('**/api/v1/**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname, method = req.method()
  const reply = (value: unknown, code = 200) => route.fulfill({ status: code, contentType: 'application/json', body: JSON.stringify(value) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: false })
  if (path === '/api/v1/me') return state.loggedOut ? reply({ error: '请先登录' }, 401) : reply(user())
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements'].includes(path)) return reply([])
  if (path.startsWith('/api/v1/admin/')) {
   state.adminGets.push(path)
   if (state.expireOnAdmin) { state.verifiedUntil = null; return reply({ error: '请先完成后台二次验证', code: 'mfa_required' }, 403) }
   if (path === '/api/v1/admin/overview') return reply({ users: 1, orders: 0, paid_minor: 0, pending_events: 0 })
   if (path === '/api/v1/admin/orders') return reply([])
  }
  if (path === '/api/v1/me/mfa' && method === 'GET') { state.statusGets++; return reply(status()) }
  if (path === '/api/v1/me/mfa/setup') {
   const body = req.postDataJSON(); state.setupCalls.push(body)
   if (state.failSetup) return reply({ error: '当前密码不正确或登录状态已变化' }, 401)
   state.pending = true
   return reply({ secret, otpauth_url: `otpauth://totp/XUI:fixture?secret=${secret}&issuer=XUI`, expires_at: new Date(Date.now() + 10 * 60 * 1000).toISOString() })
  }
  if (path === '/api/v1/me/mfa/confirm') {
   const body = req.postDataJSON(); state.confirms.push(body)
   if (state.failConfirm) return reply({ error: '验证码不正确或已过期' }, 400)
   state.enabled = true; state.pending = false; state.verifiedUntil = future()
   return reply({ recovery_codes: recoveryCodes })
  }
  if (path === '/api/v1/me/mfa/verify') {
   const body = req.postDataJSON(); state.verifies.push(body)
   if (state.failVerify || state.used.has(body.code)) return reply({ error: '验证码不正确、已过期或已经使用' }, 400)
   if (recoveryCodes.includes(body.code)) state.used.add(body.code)
   state.verifiedUntil = future()
   return reply({ verified_until: state.verifiedUntil })
  }
  if (path === '/api/v1/me/mfa/disable') {
   const body = req.postDataJSON(); state.disables.push(body)
   if (state.failDisable) return reply({ error: '当前密码或验证码不正确' }, 401)
   state.loggedOut = true; state.enabled = false; state.verifiedUntil = null
   return reply({ ok: true })
  }
  return reply({ error: 'unexpected MFA fixture endpoint' }, 404)
 })
 return state
}
async function openSecurity(page: Page) {
 await page.getByRole('navigation').getByRole('button', { name: /账号安全/ }).click()
 await expect(page.getByRole('heading', { name: '后台二次验证', exact: true })).toBeVisible()
}
async function setup(page: Page) {
 await page.getByRole('textbox', { name: '绑定验证器密码', exact: true }).fill(password)
 await page.getByRole('button', { name: '开始绑定验证器', exact: true }).click()
 await expect(page.getByTestId('mfa-enrollment-secret')).toHaveText(secret)
}

test('强制后台二次验证阻止预取，绑定错误清空输入，恢复码仅当次展示且离开即清除', async ({ page }) => {
 const state = await fixture(page)
 await page.goto('/')
 await expect(page.getByRole('status').filter({ hasText: '后台需要先设置验证器' })).toBeVisible()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toHaveCount(0)
 expect(state.adminGets).toHaveLength(0)
 await page.getByRole('button', { name: '前往后台二次验证', exact: true }).click()
 state.failSetup = true
 await page.getByRole('textbox', { name: '绑定验证器密码', exact: true }).fill(password)
 await page.getByRole('button', { name: '开始绑定验证器', exact: true }).click()
 await expect(page.getByTestId('staff-mfa').getByRole('alert')).toContainText('当前密码不正确')
 await expect(page.getByRole('textbox', { name: '绑定验证器密码', exact: true })).toHaveValue('')
 state.failSetup = false; await setup(page)
 state.failConfirm = true
 await page.getByRole('textbox', { name: '绑定动态验证码', exact: true }).fill('000000')
 await page.getByRole('button', { name: '确认启用二次验证', exact: true }).click()
 await expect(page.getByTestId('staff-mfa').getByRole('alert')).toContainText('验证码不正确')
 await expect(page.getByRole('textbox', { name: '绑定动态验证码', exact: true })).toHaveValue('')
 state.failConfirm = false
 await page.getByRole('textbox', { name: '绑定动态验证码', exact: true }).fill('123456')
 await page.getByRole('button', { name: '确认启用二次验证', exact: true }).click()
 await expect(page.getByTestId('mfa-recovery-codes').locator('li')).toHaveCount(10)
 await expect(page.getByTestId('mfa-enrollment-secret')).toHaveCount(0)
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toBeVisible()
 expect(state.setupCalls).toEqual([{ current_password: password }, { current_password: password }])
 expect(state.confirms).toEqual([{ code: '000000' }, { code: '123456' }])
 await page.getByRole('navigation').getByRole('button', { name: /我的概览/ }).click()
 await openSecurity(page)
 await expect(page.getByTestId('mfa-recovery-codes')).toHaveCount(0)
 await expect(page.getByTestId('mfa-enrollment-secret')).toHaveCount(0)
 expect(await page.evaluate(() => ({ local: localStorage.length, session: sessionStorage.length }))).toEqual({ local: 0, session: 0 })
})

test('恢复码解锁后台，服务端过期响应回到验证界面，已用恢复码拒绝且不会丢失入口', async ({ page }) => {
 const state = await fixture(page, { enabled: true })
 await page.goto('/'); await openSecurity(page)
 state.failVerify = true
 await page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true }).fill('111111')
 await page.getByRole('button', { name: '验证后台操作', exact: true }).click()
 await expect(page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true })).toHaveValue('')
 await expect(page.getByTestId('staff-mfa').getByRole('alert')).toContainText('已经使用')
 state.failVerify = false
 await page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true }).fill(recoveryCodes[0]!)
 await page.getByRole('button', { name: '验证后台操作', exact: true }).click()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toBeVisible()
 await expect(page.getByText('剩余可用恢复码 9 个。每个恢复码只能使用一次；动态验证码也不能重复提交。', { exact: true })).toBeVisible()
 state.expireOnAdmin = true
 await page.getByRole('navigation').getByRole('button', { name: /我的概览/ }).click()
 await page.getByRole('button', { name: '刷新用量', exact: true }).click()
 await expect(page.getByRole('heading', { name: '账号安全', exact: true })).toBeVisible()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toHaveCount(0)
 await page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true }).fill(recoveryCodes[0]!)
 await page.getByRole('button', { name: '验证后台操作', exact: true }).click()
 await expect(page.getByTestId('staff-mfa').getByRole('alert')).toContainText('已经使用')
 expect(state.used.size).toBe(1)
 state.expireOnAdmin = false
 await page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true }).fill(recoveryCodes[1]!)
 await page.getByRole('button', { name: '验证后台操作', exact: true }).click()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toBeVisible()
})

test('停用需要密码和验证码，失败清空凭据，成功后退出且不保留验证信息', async ({ page }) => {
 const state = await fixture(page, { enabled: true, verified: true })
 await page.goto('/'); await openSecurity(page)
 await page.getByText('停用后台二次验证', { exact: true }).click()
 state.failDisable = true
 await page.getByRole('textbox', { name: '停用验证器密码', exact: true }).fill(password)
 await page.getByRole('textbox', { name: '停用验证码或恢复码', exact: true }).fill('123456')
 await page.getByRole('button', { name: '停用验证器并退出所有设备', exact: true }).click()
 await expect(page.getByTestId('staff-mfa').getByRole('alert')).toContainText('密码或验证码不正确')
 await expect(page.getByRole('textbox', { name: '停用验证器密码', exact: true })).toHaveValue('')
 await expect(page.getByRole('textbox', { name: '停用验证码或恢复码', exact: true })).toHaveValue('')
 state.failDisable = false
 await page.getByRole('textbox', { name: '停用验证器密码', exact: true }).fill(password)
 await page.getByRole('textbox', { name: '停用验证码或恢复码', exact: true }).fill(recoveryCodes[0]!)
 await page.getByRole('button', { name: '停用验证器并退出所有设备', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '后台二次验证已停用，所有设备已退出' })).toBeVisible()
 await expect(page.getByTestId('staff-mfa')).toHaveCount(0)
 await expect(page.getByRole('button', { name: '登录', exact: true })).toBeVisible()
 expect(state.disables).toEqual([{ current_password: password, code: '123456' }, { current_password: password, code: recoveryCodes[0] }])
})

test('不可用服务不绕过强制验证，密钥只在绑定页显示，移动端可用且普通用户不读取后台验证', async ({ page, context }) => {
 const state = await fixture(page, { available: false })
 await page.goto('/'); await openSecurity(page)
 await expect(page.getByRole('status').filter({ hasText: '二次验证服务尚未配置' })).toBeVisible()
 await expect(page.getByRole('button', { name: '开始绑定验证器', exact: true })).toHaveCount(0)
 expect(state.adminGets).toHaveLength(0)
 state.available = true
 await page.getByRole('button', { name: '刷新二次验证状态', exact: true }).click()
 await setup(page)
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 await page.getByRole('button', { name: '清除本次绑定信息', exact: true }).click()
 await expect(page.getByTestId('mfa-enrollment-secret')).toHaveCount(0)
 await setup(page)
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 await openSecurity(page)
 await expect(page.getByTestId('mfa-enrollment-secret')).toHaveCount(0)
 const regular = await context.newPage(), regularState = await fixture(regular, { regular: true, required: false })
 await regular.goto('/'); await regular.getByRole('navigation').getByRole('button', { name: /账号安全/ }).click()
 await expect(regular.getByRole('heading', { name: '账号安全', exact: true })).toBeVisible()
 await expect(regular.getByTestId('staff-mfa')).toHaveCount(0)
 expect(regularState.statusGets).toBe(0)
 await regular.close()
})


test('本地验证到期自动离开后台，停止后台读取，重新验证后恢复入口', async ({ page }) => {
 const state = await fixture(page, { enabled: true, verified: true })
 state.verifiedUntil = new Date(Date.now() + 60_000).toISOString()
 await page.clock.install()
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /运营工作台/ }).click()
 await expect(page.getByRole('heading', { name: '运营工作台', exact: true })).toBeVisible()
 const beforeExpiry = state.adminGets.length
 await page.clock.fastForward(65_000)
 await expect(page.getByRole('heading', { name: '账号安全', exact: true })).toBeVisible()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toHaveCount(0)
 await expect(page.getByText('本设备尚未完成有效的后台二次验证，请输入动态码或未使用的恢复码。', { exact: true })).toBeVisible()
 expect(state.adminGets.length).toBe(beforeExpiry)
 await page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true }).fill(recoveryCodes[0]!)
 await page.getByRole('button', { name: '验证后台操作', exact: true }).click()
 await expect(page.getByRole('navigation').getByRole('button', { name: /运营工作台/ })).toBeVisible()
 await expect(page.getByRole('textbox', { name: '后台验证码或恢复码', exact: true })).toHaveValue('')
})
