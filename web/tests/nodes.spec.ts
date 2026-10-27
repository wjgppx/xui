import { test, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

test('管理员登记节点，真实 Agent 注册上报，撤销后身份失效', async ({ page }) => {
 test.setTimeout(90_000)
 const email = `node-admin-${Date.now()}@example.test`
 const directory = mkdtempSync(join(tmpdir(), 'xui-browser-agent-'))
 let promoted = false, nodeID = ''
 try {
  await page.goto('/')
  await page.getByRole('button', { name: '创建账号 ↗' }).click()
  await page.getByRole('textbox', { name: '邮箱', exact: true }).fill(email)
  await page.getByRole('textbox', { name: '密码', exact: true }).fill('node-browser-test-password')
  await page.getByRole('button', { name: '创建账号', exact: true }).last().click()
  await expect(page.getByRole('heading', { name: '欢迎回来。' })).toBeVisible()
  execFileSync('bash', ['../scripts/api.sh', '--promote-admin', email], { stdio: 'pipe' }); promoted = true
  // Role changes revoke existing sessions; authenticate with the new role.
  const loginResponse = await page.request.post('/api/v1/auth/login', { data: { email, password: 'node-browser-test-password' }, headers: { Origin: 'http://127.0.0.1:5173' } })
  expect(loginResponse.status()).toBe(200)
  await page.reload()
  await page.getByRole('navigation').getByRole('button', { name: /节点管理/ }).click()
  await page.getByRole('button', { name: '＋ 注册节点' }).click()
  await page.getByRole('textbox', { name: '节点名称' }).fill('浏览器验收节点')
  const response = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/nodes') && r.request().method() === 'POST')
  await page.getByRole('button', { name: '生成注册令牌' }).click()
  const enrollment = await (await response).json(); nodeID = enrollment.node_id
  await expect(page.getByRole('textbox', { name: '注册令牌' })).toHaveValue(enrollment.enrollment_token)
  const args = ['--controller', 'http://127.0.0.1:8080', '--state', join(directory, 'identity.json'), '--once']
  execFileSync('../bin/xui-agent', args, { env: { ...process.env, XUI_ENROLL_TOKEN: enrollment.enrollment_token }, stdio: 'pipe' })
  // Reload closes the one-time token display and fetches authoritative node state.
  await page.reload()
  await page.getByRole('navigation').getByRole('button', { name: /节点管理/ }).click()
  const row = page.getByRole('row').filter({ hasText: nodeID.slice(0, 12) })
  await expect(row.getByText('已注册', { exact: true })).toBeVisible()
  await expect(row.getByText('1 / 1', { exact: true })).toBeVisible()
  await row.getByRole('button', { name: '代理配置', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加入站' }).click()
  await page.getByRole('textbox', { name: 'TLS 域名 1', exact: true }).fill('node.example.test')
  await page.getByRole('textbox', { name: '证书名称 1', exact: true }).fill('browser-node')
  await page.getByRole('textbox', { name: '授权账号 1', exact: true }).fill(email)
  await page.getByRole('button', { name: '保存并下发', exact: true }).click()
  await expect(row.getByText('1 / 2', { exact: true })).toBeVisible()
  await row.getByRole('button', { name: '代理配置', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'TLS 域名 1', exact: true })).toHaveValue('node.example.test')
  await expect(page.getByRole('textbox', { name: '授权账号 1', exact: true })).toHaveValue(email)
  await page.screenshot({ path: 'test-results/node-configuration.png', fullPage: true })
  await page.getByRole('button', { name: '移除此入站', exact: true }).click()
  await page.getByRole('button', { name: '保存并下发', exact: true }).click()
  await expect(row.getByText('1 / 3', { exact: true })).toBeVisible()
  execFileSync('../bin/xui-agent', args, { stdio: 'pipe' })
  await page.getByRole('button', { name: '刷新状态', exact: true }).click()
  await expect(row.getByText('3 / 3', { exact: true })).toBeVisible()
  await page.screenshot({ path: 'test-results/node-manager.png', fullPage: true })
  page.once('dialog', dialog => dialog.accept())
  await row.getByRole('button', { name: '撤销身份' }).click()
  await expect(row.getByText('已撤销', { exact: true })).toBeVisible()
  expect(() => execFileSync('../bin/xui-agent', args, { stdio: 'pipe' })).toThrow()
 } finally {
  try {
   if (nodeID) await page.request.post(`/api/v1/admin/nodes/${nodeID}/revoke`, { data: {}, headers: { Origin: 'http://127.0.0.1:5173' } })
  } finally {
   if (promoted) execFileSync('bash', ['../scripts/api.sh', '--demote-admin', email], { stdio: 'pipe' })
   rmSync(directory, { recursive: true, force: true })
  }
 }
})
