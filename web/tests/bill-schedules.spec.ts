import { test, expect, type Page } from '@playwright/test'
import type { BillSchedule } from '../src/api'

const created = '2026-10-28T03:00:00Z'
const shanghaiYesterday = () => new Date(Date.now() + 8 * 60 * 60 * 1000 - 24 * 60 * 60 * 1000).toISOString().slice(0, 10)
function schedules(): BillSchedule[] {
 return (['alipay', 'wechat'] as const).flatMap(provider => (['payment', 'refund'] as const).map(kind => ({ id: `${provider}-${kind}`, provider, kind, enabled: false, run_at: '10:00', timezone: 'Asia/Shanghai', revision: 1, state: 'disabled', next_bill_date: null, last_enqueued_date: null, last_job_id: null, authorized_by: null, updated_at: created })))
}
async function fixture(page: Page, options: { enabled?: boolean; manage?: boolean } = {}) {
 const state = { items: schedules(), puts: [] as Record<string, unknown>[], conflict: false, failLoad: false, downloadPosts: 0 }
 const permissions = ['statements.read', ...(options.manage === false ? [] : ['statements.schedule'])]
 await page.route('**/api/v1/**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname, method = req.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: options.enabled ?? true })
  if (path === '/api/v1/me') return reply({ id: 'finance', email: 'finance@example.test', role: 'finance', permissions, email_verified_at: created })
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements'].includes(path)) return reply([])
  if (path === '/api/v1/admin/statements') return reply({ items: [] })
  if (path === '/api/v1/admin/bill-downloads') { if (method !== 'GET') state.downloadPosts++; return reply({ items: [] }) }
  if (path === '/api/v1/admin/bill-schedules' && method === 'GET') return state.failLoad ? reply({ error: '自动拉取计划暂时不可用' }, 503) : reply({ items: state.items })
  if (path === '/api/v1/admin/bill-schedules' && method === 'PUT') {
   const body = req.postDataJSON(); state.puts.push(body)
   if (state.conflict) return reply({ error: '计划已被其他操作者修改，请刷新后重试' }, 409)
   const item = state.items.find(item => item.provider === body.provider && item.kind === body.kind)!
   Object.assign(item, body, { revision: item.revision + 1, state: body.enabled ? 'active' : 'disabled', authorized_by: 'finance', next_bill_date: body.enabled ? shanghaiYesterday() : null })
   return reply(item)
  }
  return reply({ error: 'unexpected fixture endpoint' }, 404)
 })
 return state
}
async function navigate(page: Page) {
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /商户账单/ }).click()
 await expect(page.getByTestId('bill-schedules')).toBeVisible()
}
const row = (page: Page, id = 'wechat-refund') => page.locator(`[data-schedule-id="${id}"]`)

test('按北京时间保存收款退款各自的计划，保存不会创建浏览器下载任务或声称已导入', async ({ page }) => {
 const state = await fixture(page)
 await navigate(page)
 await expect(page.locator('[data-schedule-id]')).toHaveCount(4)
 await row(page).getByRole('button', { name: '配置自动拉取' }).click()
 await page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true }).check()
 await page.getByLabel('自动拉取执行时间', { exact: true }).fill('11:45')
 await page.getByRole('button', { name: '保存自动拉取计划', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '到达执行时间后创建下载任务' })).toBeVisible()
 expect(state.puts).toEqual([{ provider: 'wechat', kind: 'refund', enabled: true, run_at: '11:45', revision: 1 }])
 expect(state.downloadPosts).toBe(0)
 await expect(row(page).getByText('已启用', { exact: true })).toBeVisible()
 await expect(row(page).getByText(`下一账单 ${shanghaiYesterday()}`, { exact: true })).toBeVisible()
 const today = new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString().slice(0, 10)
 await expect(row(page).getByText(`预计执行 ${today} 11:45（北京时间）`, { exact: true })).toBeVisible()
 await expect(row(page, 'wechat-payment').getByText('已停用', { exact: true })).toBeVisible()
 expect(await page.evaluate(() => localStorage.length)).toBe(0)
})

test('版本冲突保留编辑内容，刷新后重新读取版本，停用与重新启用使用当前版本', async ({ page }) => {
 const state = await fixture(page); state.conflict = true
 await navigate(page)
 await row(page).getByRole('button', { name: '配置自动拉取' }).click()
 await page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true }).check()
 await page.getByLabel('自动拉取执行时间', { exact: true }).fill('12:35')
 await page.getByRole('button', { name: '保存自动拉取计划', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '计划已被其他操作者修改' })).toBeVisible()
 await expect(page.getByLabel('自动拉取执行时间', { exact: true })).toHaveValue('12:35')
 await expect(page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true })).toBeChecked()
 await page.getByRole('button', { name: '取消', exact: true }).click()
 state.conflict = false; Object.assign(state.items.find(item => item.id === 'wechat-refund')!, { revision: 2, enabled: true, state: 'active', run_at: '13:20', next_bill_date: shanghaiYesterday() })
 await page.getByRole('button', { name: '刷新自动拉取计划', exact: true }).click()
 await expect(row(page)).toContainText('13:20（北京时间）')
 await row(page).getByRole('button', { name: '配置自动拉取' }).click()
 await page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true }).uncheck()
 await page.getByRole('button', { name: '保存自动拉取计划', exact: true }).click()
 await expect(row(page).getByText('已停用', { exact: true })).toBeVisible()
 expect(state.puts[1]).toMatchObject({ enabled: false, revision: 2, run_at: '13:20' })
 await row(page).getByRole('button', { name: '配置自动拉取' }).click()
 await page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true }).check()
 await page.getByRole('button', { name: '保存自动拉取计划', exact: true }).click()
 await expect(row(page).getByText('已启用', { exact: true })).toBeVisible()
 expect(state.puts[2]).toMatchObject({ enabled: true, revision: 3 })
})

test('官方下载总开关关闭时无法启用新计划，但仍可停用已经启用的计划', async ({ page }) => {
 const state = await fixture(page, { enabled: false })
 Object.assign(state.items.find(item => item.id === 'wechat-refund')!, { enabled: true, state: 'active', revision: 2, next_bill_date: shanghaiYesterday() })
 await navigate(page)
 await expect(page.getByRole('status').filter({ hasText: '官方下载未启用，自动拉取暂停' })).toBeVisible()
 await row(page, 'alipay-payment').getByRole('button', { name: '配置自动拉取' }).click()
 await expect(page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true })).toBeDisabled()
 await page.getByRole('button', { name: '取消', exact: true }).click()
 await row(page).getByRole('button', { name: '配置自动拉取' }).click()
 await page.getByRole('checkbox', { name: '启用每日自动拉取', exact: true }).uncheck()
 await page.getByRole('button', { name: '保存自动拉取计划', exact: true }).click()
 await expect(row(page).getByText('已停用', { exact: true })).toBeVisible()
 expect(state.puts).toHaveLength(1); expect(state.puts[0]).toMatchObject({ enabled: false, revision: 2 })
})

test('只读账号可查看暂停原因，读取故障独立恢复，移动端无横向溢出', async ({ page }) => {
 const state = await fixture(page, { manage: false }); state.failLoad = true
 Object.assign(state.items[0], { state: 'actor_changed', authorized_by: 'former-finance' })
 Object.assign(state.items[1], { state: 'merchant_changed' })
 await navigate(page)
 await expect(page.getByTestId('bill-schedules').getByRole('alert').filter({ hasText: '自动拉取计划暂时不可用' })).toBeVisible()
 await expect(page.getByRole('heading', { name: '最近导入账单', exact: true })).toBeVisible()
 state.failLoad = false
 await page.getByRole('button', { name: '刷新自动拉取计划', exact: true }).click()
 await expect(page.getByText('授权已变化，请重新启用', { exact: true })).toBeVisible()
 await expect(page.getByText('商户已变化，请重新启用', { exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '配置自动拉取', exact: true })).toHaveCount(0)
 await expect(page.getByText('仅可查看', { exact: true })).toHaveCount(4)
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 expect(state.puts).toHaveLength(0)
})
