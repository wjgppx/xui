import { test, expect, type Page } from '@playwright/test'

async function fixture(page: Page, role: 'admin' | 'finance' = 'admin', enabled = true) {
 const created = '2026-10-28T00:00:00Z'
 const payment = { id: 'paid-payment', order_id: 'paid-order', user_email: 'buyer@example.test', provider: 'alipay', status: 'paid', amount_minor: 1900, currency: 'CNY', transaction_id: 'original-transaction', created_at: created, paid_at: created, order_status: 'paid', reconcile_status: 'paid', reconcile_attempts: 0 }
 const open = { ...payment, id: 'unpaid-payment', order_id: 'unpaid-order', status: 'ready', order_status: 'pending', paid_at: null, reconcile_status: 'waiting' }
 const refund = { id: 'stable-refund', payment_id: payment.id, order_id: payment.order_id, user_email: payment.user_email, provider: 'alipay', transaction_id: payment.transaction_id, amount_minor: 1900, currency: 'CNY', reason: '原订单全额退款', status: 'requested', requested_by: 'finance', approved_by: null, provider_refund_id: null, created_at: created, updated_at: created, completed_at: null as string | null }
 const state = { payments: [payment, open], refunds: [] as typeof refund[], drafts: [] as Record<string, unknown>[], approvals: [] as Record<string, unknown>[], closes: [] as Record<string, unknown>[] }
 const permissions = ['payments.read', 'payments.retry', 'payments.close', 'refunds.request', 'refunds.read', ...(role === 'admin' ? ['refunds.approve'] : [])]
 await page.route('**/api/v1/**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: enabled })
  if (path === '/api/v1/me') return reply({ id: role, email: `${role}@example.test`, role, permissions, email_verified_at: created })
  if (path === '/api/v1/plans' || path === '/api/v1/payment-methods' || path === '/api/v1/orders' || path === '/api/v1/me/entitlements' || path === '/api/v1/admin/payment-activity') return reply([])
  if (path === '/api/v1/admin/payments') return reply({ items: state.payments })
  if (path === '/api/v1/admin/refunds') return reply({ items: state.refunds })
  if (path.endsWith('/refund')) {
   const body = req.postDataJSON(); state.drafts.push(body); state.refunds = [{ ...refund, reason: body.reason }]; return reply(state.refunds[0])
  }
  if (path.endsWith('/approve')) {
   const body = req.postDataJSON(); state.approvals.push(body)
   if (!enabled) return reply({ error: '资金操作尚未启用' }, 503)
   if (body.current_password !== 'correct-password') return reply({ error: '当前密码不正确或登录状态已变化' }, 401)
   state.refunds[0].status = 'queued'; return reply({ ok: true }, 202)
  }
  if (path.endsWith('/close')) {
   const body = req.postDataJSON(); state.closes.push(body)
   if (body.current_password !== 'correct-password') return reply({ error: '当前密码不正确或登录状态已变化' }, 401)
   open.order_status = 'closing'; return reply({ ok: true }, 202)
  }
  return reply({ error: 'unexpected fixture endpoint' }, 404)
 })
 return { state, refund }
}
async function nav(page: Page, name: string) { await page.getByRole('navigation').getByRole('button', { name: new RegExp(name.replace(/^[¥↶]/, '')) }).click() }

test('财务申请全额退款并查看审批进度，不提供审批权限', async ({ page }) => {
 const { state } = await fixture(page, 'finance')
 await page.goto('/'); await nav(page, '¥支付流水')
 await page.getByRole('button', { name: '申请全额退款' }).click()
 await page.getByRole('textbox', { name: '资金操作原因' }).fill('套餐不再需要')
 await page.getByRole('button', { name: '提交退款申请', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '退款申请已记录' })).toBeVisible()
 expect(state.drafts).toEqual([{ reason: '套餐不再需要' }])
 await nav(page, '↶退款管理')
 await expect(page.getByText('待管理员审批', { exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '审批全额退款', exact: true })).toHaveCount(0)
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})

test('管理员退款审批校验密码并等待渠道成功确认', async ({ page }) => {
 const { state, refund } = await fixture(page); state.refunds.push(refund)
 await page.goto('/'); await nav(page, '↶退款管理')
 await page.getByRole('button', { name: '审批全额退款', exact: true }).click()
 const password = page.getByLabel('退款审批当前密码')
 await password.fill('incorrect-password'); await page.getByRole('button', { name: '验证身份并批准退款' }).click()
 await expect(page.getByRole('alert').filter({ hasText: '当前密码不正确' })).toBeVisible(); await expect(password).toHaveValue('')
 await password.fill('correct-password'); await page.getByRole('button', { name: '验证身份并批准退款' }).click()
 await expect(page.getByText('等待渠道处理', { exact: true })).toBeVisible()
 await expect(page.getByText('退款成功', { exact: true })).toHaveCount(0)
 expect(state.approvals).toEqual([{ current_password: 'incorrect-password' }, { current_password: 'correct-password' }])
 state.refunds[0].status = 'succeeded'; state.refunds[0].completed_at = '2026-10-28T01:00:00Z'
 await page.getByRole('button', { name: '刷新退款记录' }).click(); await expect(page.getByText('退款成功', { exact: true })).toBeVisible()
 expect(await page.evaluate(() => localStorage.length)).toBe(0)
})

test('默认资金开关关闭时保留申请入口且禁止关单和审批', async ({ page }) => {
 const { state, refund } = await fixture(page, 'admin', false); state.refunds.push(refund)
 await page.goto('/'); await nav(page, '¥支付流水')
 await expect(page.getByRole('button', { name: '申请全额退款' })).toBeEnabled()
 await expect(page.getByRole('button', { name: '渠道关单', exact: true })).toBeDisabled()
 await nav(page, '↶退款管理')
 await expect(page.getByRole('button', { name: '审批全额退款', exact: true })).toBeDisabled()
 expect(state.approvals).toHaveLength(0); expect(state.closes).toHaveLength(0)
})

test('渠道关单校验密码且排队后显示关单中', async ({ page }) => {
 const { state } = await fixture(page)
 await page.goto('/'); await nav(page, '¥支付流水')
 await page.getByRole('button', { name: '渠道关单', exact: true }).click()
 await page.getByRole('textbox', { name: '资金操作原因' }).fill('停止此订单付款')
 const password = page.getByLabel('关单当前密码')
 await password.fill('incorrect-password'); await page.getByRole('button', { name: '验证身份并提交关单' }).click()
 await expect(page.getByRole('alert').filter({ hasText: '当前密码不正确' })).toBeVisible(); await expect(password).toHaveValue('')
 await password.fill('correct-password'); await page.getByRole('button', { name: '验证身份并提交关单' }).click()
 await expect(page.getByText('订单渠道关单中', { exact: true })).toBeVisible()
 expect(state.closes[1]).toEqual({ reason: '停止此订单付款', current_password: 'correct-password' })
})
