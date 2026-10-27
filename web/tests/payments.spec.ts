import { test, expect, type Page } from '@playwright/test'

const createdAt = '2026-10-27T00:00:00Z'
const order = { id: 'payment-browser-order', user_id: 'buyer', plan_id: 'explore', plan_name: '浏览器支付套餐', price_minor: 1900, currency: 'CNY', duration_days: 30, quota_bytes: 107374182400, status: 'pending', created_at: createdAt, paid_at: null }
const allMethods = [{ id: 'alipay', name: '支付宝' }, { id: 'wechat', name: '微信支付' }, { id: 'epay_alipay', name: '易支付 · 支付宝' }, { id: 'epay_wechat', name: '易支付 · 微信支付' }]

async function paymentFixture(page: Page, methods = allMethods) {
 const state = { paid: false, gatewayPaid: false, createFailuresRemaining: 0, createProviders: [] as string[], syncRequests: 0, developmentSettles: 0, payment: null as Record<string, unknown> | null }
 await page.route('**/api/v1/**', async route => {
  const request = route.request(), path = new URL(request.url()).pathname, method = request.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: true })
  if (path === '/api/v1/me') return reply({ id: 'buyer', email: 'buyer@example.test', role: 'user', email_verified_at: createdAt, permissions: [] })
  if (path === '/api/v1/plans') return reply([{ id: 'explore', name: '浏览器支付套餐', description: '测试套餐', price_minor: 1900, currency: 'CNY', duration_days: 30, quota_bytes: 107374182400 }])
  if (path === '/api/v1/payment-methods') return reply(methods)
  if (path === '/api/v1/orders' && method === 'GET') return reply([{ ...order, status: state.paid ? 'paid' : 'pending', paid_at: state.paid ? createdAt : null }])
  if (path === '/api/v1/me/entitlements') return reply(state.paid ? [{ id: 'entitlement', order_id: order.id, plan_name: order.plan_name, starts_at: createdAt, ends_at: '2030-10-27T00:00:00Z', quota_bytes: order.quota_bytes, used_bytes: 0, revoked_at: null }] : [])
  if (path.includes('/dev/orders/')) { state.developmentSettles++; return reply({ error: 'disabled' }, 404) }
  if (path === `/api/v1/orders/${order.id}/payment/sync` && method === 'POST') {
   state.syncRequests++
   if (state.gatewayPaid) state.paid = true
   if (state.payment && state.paid) state.payment.status = 'paid'
   return reply(state.payment)
  }
  if (path === `/api/v1/orders/${order.id}/payment`) {
   if (method === 'POST') {
    const provider = request.postDataJSON().provider as string
    state.createProviders.push(provider)
    if (state.payment && state.payment.provider !== provider) return reply({ error: '订单已选择其他渠道' }, 409)
    if (!state.payment) state.payment = { id: 'stable-payment-id', order_id: order.id, provider, status: 'pending', checkout_kind: '', checkout_url: '', created_at: createdAt, updated_at: createdAt, paid_at: null }
    if (state.createFailuresRemaining > 0) { state.createFailuresRemaining--; return reply({ error: '支付渠道暂时不可用，请使用同一订单重试' }, 502) }
    state.payment = { ...state.payment, status: 'ready', checkout_kind: provider === 'wechat' ? 'qr' : 'redirect', checkout_url: provider === 'wechat' ? 'weixin://wxpay/bizpayurl?pr=local-test' : 'https://pay.example.test/submit.php?out_trade_no=stable-payment-id' }
   }
   if (!state.payment) return reply({ error: '尚未创建支付' }, 404)
   if (state.paid) state.payment.status = 'paid'
   return reply(state.payment)
  }
  return reply({ error: 'fixture endpoint unavailable' }, 404)
 })
 return state
}

test('未配置渠道时不提供付款入口，浏览器返回参数不会改变支付结果', async ({ page }) => {
 const state = await paymentFixture(page, [])
 await page.goto('/?payment_return=1&other=retained')
 await expect(page.getByRole('heading', { name: '每一笔，都有记录。' })).toBeVisible()
 await expect(page.getByRole('status').filter({ hasText: '已返回订单页面' })).toBeVisible()
 await expect(page.getByText('支付渠道待配置', { exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '去支付', exact: true })).toHaveCount(0)
 await expect(page.getByText('待支付', { exact: true })).toBeVisible()
 expect(new URL(page.url()).searchParams.has('payment_return')).toBe(false)
 expect(new URL(page.url()).searchParams.get('other')).toBe('retained')
 expect(state.createProviders).toEqual([])
 expect(state.developmentSettles).toBe(0)
 expect(state.paid).toBe(false)
})

test('微信付款二维码固定订单渠道，只有服务端确认后显示权益已开通', async ({ page }) => {
 const state = await paymentFixture(page)
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 await page.getByRole('button', { name: '去支付', exact: true }).click()
 await page.getByRole('group', { name: '支付方式' }).getByRole('button', { name: '微信支付', exact: true }).click()
 await page.getByRole('button', { name: '确认支付方式', exact: true }).click()
 await expect(page.getByRole('img', { name: '付款二维码' })).toBeVisible()
 await expect(page.getByRole('group', { name: '支付方式' })).toHaveCount(0)
 expect(state.createProviders).toEqual(['wechat'])
 await page.getByRole('button', { name: '核对支付结果', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '暂未确认支付' })).toBeVisible()
 await expect(page.getByText('待支付', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '关闭', exact: true }).click()
 await page.getByRole('button', { name: '去支付', exact: true }).click()
 await expect(page.getByRole('img', { name: '付款二维码' })).toBeVisible()
 expect(state.createProviders).toEqual(['wechat'])
 state.gatewayPaid = true
 await page.getByRole('button', { name: '核对支付结果', exact: true }).click()
 await expect(page.getByText('支付已确认', { exact: true })).toBeVisible()
 await expect(page.getByText('已开通', { exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '去支付', exact: true })).toHaveCount(0)
 expect(state.syncRequests).toBe(2)
 expect(state.developmentSettles).toBe(0)
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 await page.screenshot({ path: 'test-results/payment-confirmed-mobile.png', fullPage: true })
})

test('易支付提供独立安全跳转入口，刷新只读取已确认的订单状态', async ({ page }) => {
 const state = await paymentFixture(page)
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 await page.getByRole('button', { name: '去支付', exact: true }).click()
 await page.getByRole('group', { name: '支付方式' }).getByRole('button', { name: '易支付 · 支付宝', exact: true }).click()
 await page.getByRole('button', { name: '确认支付方式', exact: true }).click()
 const link = page.getByRole('link', { name: '打开支付收银台 ↗' })
 await expect(link).toHaveAttribute('href', 'https://pay.example.test/submit.php?out_trade_no=stable-payment-id')
 await expect(link).toHaveAttribute('target', '_blank')
 await expect(link).toHaveAttribute('rel', 'noopener noreferrer')
 await expect(page.getByRole('button', { name: '核对支付结果', exact: true })).toHaveCount(0)
 await page.getByRole('button', { name: '刷新支付结果', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '暂未确认支付' })).toBeVisible()
 expect(state.syncRequests).toBe(0)
 expect(state.paid).toBe(false)
 await page.screenshot({ path: 'test-results/payment-redirect.png', fullPage: true })
 state.paid = true
 await page.getByRole('button', { name: '刷新支付结果', exact: true }).click()
 await expect(page.getByText('支付已确认', { exact: true })).toBeVisible()
 await expect(page.getByText('已开通', { exact: true })).toBeVisible()
 expect(state.syncRequests).toBe(0)
})


test('网关暂时失败后读取已固定的渠道，重试沿用同一支付记录', async ({ page }) => {
 const state = await paymentFixture(page)
 state.createFailuresRemaining = 1
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 await page.getByRole('button', { name: '去支付', exact: true }).click()
 await page.getByRole('group', { name: '支付方式' }).getByRole('button', { name: '支付宝', exact: true }).click()
 await page.getByRole('button', { name: '确认支付方式', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '支付渠道暂时不可用' })).toBeVisible()
 await expect(page.getByRole('group', { name: '支付方式' })).toHaveCount(0)
 await expect(page.getByText('支付方式：支付宝', { exact: true })).toBeVisible()
 const paymentID = state.payment?.id
 expect(paymentID).toBe('stable-payment-id')
 await page.getByRole('button', { name: '重新获取支付入口', exact: true }).click()
 await expect(page.getByRole('link', { name: '打开支付收银台 ↗' })).toBeVisible()
 expect(state.payment?.id).toBe(paymentID)
 expect(state.createProviders).toEqual(['alipay', 'alipay'])
 expect(state.paid).toBe(false)
})
