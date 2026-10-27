import { test, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'

const origin = 'http://127.0.0.1:5173'

test('管理员分配客服角色，旧登录退出且客服仅显示获准的后台操作', async ({ page, browser }) => {
 test.setTimeout(90_000)
 const unique = Date.now().toString(), adminEmail = `staff-admin-${unique}@example.test`, supportEmail = `staff-support-${unique}@example.test`
 const password = 'staff-browser-test-password'
 const support = await browser.newContext({ baseURL: origin })
 let promoted = false, changedRole = false
 try {
  await page.goto('/')
  const adminRegistration = await page.request.post('/api/v1/auth/register', { data: { email: adminEmail, password }, headers: { Origin: origin } })
  expect(adminRegistration.status()).toBe(200)
  const supportRegistration = await support.request.post('/api/v1/auth/register', { data: { email: supportEmail, password }, headers: { Origin: origin } })
  expect(supportRegistration.status()).toBe(200)
  const target = await supportRegistration.json()
  execFileSync('bash', ['../scripts/api.sh', '--promote-admin', adminEmail], { stdio: 'pipe' }); promoted = true
  const login = await page.request.post('/api/v1/auth/login', { data: { email: adminEmail, password }, headers: { Origin: origin } })
  expect(login.status()).toBe(200)
  await page.reload()
  await page.getByRole('navigation', { name: '主导航' }).getByRole('button', { name: /后台账号/ }).click()
  await expect(page.getByRole('heading', { name: '后台账号', exact: true })).toBeVisible()
  await page.getByRole('textbox', { name: '按邮箱筛选账号', exact: true }).fill(supportEmail)
  const row = page.locator(`[data-user-id="${target.id}"]`)
  await expect(row).toContainText(supportEmail)
  await row.locator('.staff-role').click()
  await page.locator('.n-base-select-option').filter({ hasText: '客服' }).click()
  page.once('dialog', dialog => dialog.accept())
  const update = page.waitForResponse(response => response.url().endsWith(`/api/v1/admin/users/${target.id}/role`) && response.request().method() === 'POST')
  await row.getByRole('button', { name: '保存角色', exact: true }).click()
  expect((await update).status()).toBe(200); changedRole = true
  await expect(page.getByRole('status').filter({ hasText: '角色已更新' })).toBeVisible()
  await expect(row.locator('.staff-role')).toContainText('客服')
  expect((await support.request.get('/api/v1/me')).status()).toBe(401)
  await page.screenshot({ path: 'test-results/staff-manager.png', fullPage: true })

  const supportLogin = await support.request.post('/api/v1/auth/login', { data: { email: supportEmail, password }, headers: { Origin: origin } })
  expect(supportLogin.status()).toBe(200)
  const supportPage = await support.newPage()
  await supportPage.goto('/')
  const navigation = supportPage.getByRole('navigation', { name: '主导航' })
  await expect(navigation.getByRole('button', { name: /后台账号/ })).toBeVisible()
  await expect(navigation.getByRole('button', { name: /节点管理/ })).toHaveCount(0)
  await expect(navigation.getByRole('button', { name: /权限管理/ })).toHaveCount(0)
  await navigation.getByRole('button', { name: /运营工作台/ }).click()
  await expect(supportPage.getByRole('heading', { name: '最近订单', exact: true })).toBeVisible()
  await expect(supportPage.getByRole('button', { name: '＋ 新建套餐', exact: true })).toHaveCount(0)
  await expect(supportPage.getByText('注册用户', { exact: true })).toHaveCount(0)
  await navigation.getByRole('button', { name: /后台账号/ }).click()
  await expect(supportPage.getByText('当前角色可查看账号，角色调整需由管理员操作。', { exact: true })).toBeVisible()
  await expect(supportPage.locator('.staff-role')).toHaveCount(0)
  await expect(supportPage.getByRole('button', { name: '保存角色', exact: true })).toHaveCount(0)
  await supportPage.screenshot({ path: 'test-results/staff-support.png', fullPage: true })
 } finally {
  try {
   if (changedRole) execFileSync('bash', ['../scripts/api.sh', '--set-role', supportEmail, '--role', 'user'], { stdio: 'pipe' })
  } finally {
   try { if (promoted) execFileSync('bash', ['../scripts/api.sh', '--demote-admin', adminEmail], { stdio: 'pipe' }) }
   finally { await support.close() }
  }
 }
})
