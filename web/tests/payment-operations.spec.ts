import { test, expect, type Page } from '@playwright/test'

const createdAt = '2026-10-28T00:00:00Z'
const baseOrder = { user_id: 'buyer', plan_id: 'explore', plan_name: '订单生命周期套餐', price_minor: 1900, currency: 'CNY', duration_days: 30, quota_bytes: 107374182400, created_at: createdAt, paid_at: null, closed_at: null }
const rows = [
 { id: 'payment-alipay', order_id: 'order-alipay', user_email: 'buyer@example.test', provider: 'alipay', status: 'ready', amount_minor: 1900, currency: 'CNY', transaction_id: null, created_at: createdAt, paid_at: null, order_status: 'expired', expires_at: '2000-01-01T00:00:00Z', reconcile_status: 'manual_required', reconcile_attempts: 8, last_outcome: 'verification_error', next_check_at: null },
 { id: 'payment-epay', order_id: 'order-epay', user_email: 'another@example.test', provider: 'epay_alipay', status: 'ready', amount_minor: 3900, currency: 'CNY', transaction_id: null, created_at: createdAt, paid_at: null, order_status: 'pending', expires_at: '2030-01-01T00:00:00Z', reconcile_status: null, reconcile_attempts: 0, last_outcome: '', next_check_at: null }
]

async function operationsFixture(page: Page, role: 'user' | 'finance' | 'support' = 'finance') {
 const state = {
  orders: [
   { ...baseOrder, id: 'order-expired', status: 'expired', expires_at: '2000-01-01T00:00:00Z' },
   { ...baseOrder, id: 'order-closed', status: 'closed', expires_at: '2030-01-01T00:00:00Z', closed_at: createdAt },
   { ...baseOrder, id: 'order-open', status: 'pending', expires_at: '2030-01-01T00:00:00Z' }
  ] as Record<string, unknown>[],
  filters: [] as URLSearchParams[], retries: [] as string[], closes: [] as string[], financialRequests: 0,
  payments: rows.map(p => ({ ...p })), closeConflict: false
 }
 const permissions = role === 'finance' ? ['payments.read', 'payments.retry'] : role === 'support' ? ['orders.read', 'users.read'] : []
 await page.route('**/api/v1/**', async route => {
  const request = route.request(), url = new URL(request.url()), path = url.pathname, method = request.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: true, development_mail: false, email_verification_required: true })
  if (path === '/api/v1/me') return reply({ id: 'buyer', email: 'buyer@example.test', role, email_verified_at: createdAt, permissions })
  if (path === '/api/v1/plans') return reply([])
  if (path === '/api/v1/payment-methods') return reply([{ id: 'alipay', name: '支付宝' }])
  if (path === '/api/v1/orders') return reply(state.orders)
  if (path === '/api/v1/me/entitlements') return reply([])
  if (path === '/api/v1/admin/orders') return reply([])
  if (path === '/api/v1/admin/users') return reply([])
  if (path === '/api/v1/admin/payment-activity') { state.financialRequests++; return reply([{ id: 2, payment_id: null, order_id: null, provider: 'wechat', source: 'notify', outcome: 'verification_error', created_at: createdAt }]) }
  if (path === '/api/v1/admin/payments') {
   state.financialRequests++; state.filters.push(new URLSearchParams(url.search))
   return reply({ items: state.payments.filter(p => (!url.searchParams.get('provider') || p.provider === url.searchParams.get('provider')) && (!url.searchParams.get('status') || (url.searchParams.get('status') === 'manual_required' ? p.reconcile_status === 'manual_required' : p.status === url.searchParams.get('status'))) && (!url.searchParams.get('order_id') || p.order_id === url.searchParams.get('order_id'))) })
  }
  if (path.endsWith('/activity')) return reply([{ id: 1, source: 'auto_query', outcome: 'verification_error', created_at: createdAt }])
  if (path.endsWith('/retry') && method === 'POST') {
   const id = path.split('/').at(-2)!
   state.retries.push(id)
   const item = state.payments.find(p => p.id === id)
   if (item) { item.reconcile_status = 'queued'; item.reconcile_attempts = 0; item.last_outcome = 'queued' }
   return reply({ ok: true })
  }
  if (path.endsWith('/close') && method === 'POST') {
   const id = path.split('/').at(-2)!
   state.closes.push(id)
   if (state.closeConflict) return reply({ error: '该订单已发起外部付款，不能直接关闭' }, 409)
   const order = state.orders.find(o => o.id === id)
   if (order) { order.status = 'closed'; order.closed_at = createdAt }
   return reply(order)
  }
  return reply({ error: 'fixture endpoint unavailable' }, 404)
 })
 return state
}

test('已超时和已关闭订单不再提供付款入口，未发起付款的订单可以关闭', async ({ page }) => {
 const state = await operationsFixture(page, 'user')
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 const expired = page.locator('[data-order-id="order-expired"]'), closed = page.locator('[data-order-id="order-closed"]'), open = page.locator('[data-order-id="order-open"]')
 await expect(expired.getByText('已超时', { exact: true })).toBeVisible()
 for (const row of [expired, closed]) {
  await expect(row.getByRole('button', { name: '去支付', exact: true })).toHaveCount(0)
  await expect(row.getByRole('button', { name: '确认测试支付', exact: true })).toHaveCount(0)
 }
 await expect(closed.getByRole('button', { name: '关闭订单', exact: true })).toHaveCount(0)
 await expect(open.getByRole('button', { name: '去支付', exact: true })).toBeVisible()
 page.once('dialog', dialog => dialog.accept())
 await open.getByRole('button', { name: '关闭订单', exact: true }).click()
 await expect(open.getByText('已关闭', { exact: true })).toBeVisible()
 expect(state.closes).toEqual(['order-open'])
 await expect(open.getByRole('button', { name: '去支付', exact: true })).toHaveCount(0)
})

test('已发起外部付款的订单关闭被拒绝后仍保留原状态', async ({ page }) => {
 const state = await operationsFixture(page, 'user'); state.closeConflict = true
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /我的订单/ }).click()
 const row = page.locator('[data-order-id="order-open"]')
 page.once('dialog', dialog => dialog.accept())
 await row.getByRole('button', { name: '关闭订单', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '已发起外部付款' })).toBeVisible()
 await expect(row.getByText('待支付', { exact: true })).toBeVisible()
 expect(state.orders.find(o => o.id === 'order-open')?.status).toBe('pending')
})

test('财务筛选支付流水、查看处理记录并重新排队可信查询', async ({ page }) => {
 const state = await operationsFixture(page)
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /支付流水/ }).click()
 await expect(page.getByRole('heading', { name: '支付流水', exact: true })).toBeVisible()
 const row = page.locator('[data-payment-id="payment-alipay"]')
 await expect(row.getByText('需要处理', { exact: true })).toBeVisible()
 await expect(page.getByText('未绑定支付记录', { exact: true })).toBeVisible()
 await page.getByTestId('payment-status-filter').click()
 await page.locator('.n-base-select-option').filter({ hasText: '需要处理' }).click()
 await page.getByRole('button', { name: '筛选流水', exact: true }).click()
 await expect(row).toBeVisible()
 await expect(page.locator('[data-payment-id="payment-epay"]')).toHaveCount(0)
 expect(state.filters.at(-1)?.get('status')).toBe('manual_required')
 await page.getByTestId('payment-status-filter').click()
 await page.locator('.n-base-select-option').filter({ hasText: '等待付款确认' }).click()
 await page.getByTestId('payment-provider-filter').click()
 await page.locator('.n-base-select-option').filter({ hasText: '支付宝' }).filter({ hasNotText: '易支付' }).click()
 await page.getByRole('textbox', { name: '筛选订单编号', exact: true }).fill('order-alipay')
 await page.getByRole('button', { name: '筛选流水', exact: true }).click()
 await expect(page.locator('[data-payment-id="payment-epay"]')).toHaveCount(0)
 expect(state.filters.at(-1)?.get('status')).toBe('ready')
 expect(state.filters.at(-1)?.get('provider')).toBe('alipay')
 expect(state.filters.at(-1)?.get('order_id')).toBe('order-alipay')
 await row.getByRole('button', { name: '处理记录', exact: true }).click()
 await expect(page.getByRole('heading', { name: '支付处理记录', exact: true })).toBeVisible()
 await expect(page.getByTestId('payment-activity-dialog').getByRole('cell', { name: '自动核实', exact: true })).toBeVisible()
 await expect(page.getByTestId('payment-activity-dialog').getByRole('cell', { name: '验签未通过', exact: true })).toBeVisible()
 await page.keyboard.press('Escape')
 page.once('dialog', dialog => dialog.accept())
 await row.getByRole('button', { name: '重新排队核实', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '已重新排队' })).toBeVisible()
 expect(state.retries).toEqual(['payment-alipay'])
 await expect(row.getByText('待核实', { exact: true })).toBeVisible()
 expect(state.payments[0]?.status).toBe('ready')
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 await page.screenshot({ path: 'test-results/payment-operations-mobile.png', fullPage: true })
})

test('客服没有支付流水入口，也不会预先请求财务数据', async ({ page }) => {
 const state = await operationsFixture(page, 'support')
 await page.goto('/')
 await expect(page.getByRole('heading', { name: '欢迎回来。', exact: true })).toBeVisible()
 await expect(page.getByRole('navigation').getByRole('button', { name: /支付流水/ })).toHaveCount(0)
 expect(state.financialRequests).toBe(0)
})
