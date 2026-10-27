import { test, expect } from '@playwright/test'
import { execFileSync } from 'node:child_process'

const origin = 'http://127.0.0.1:5173'
test('管理员创建节点组、保存新订单套餐授权、切换组状态并查看事件', async ({ page }) => {
 test.setTimeout(90_000)
 const unique = Date.now().toString(), email = `access-admin-${unique}@example.test`
 const groupName = `浏览器权限组 ${unique}`, nodeName = `权限验收节点 ${unique}`, planName = `权限验收套餐 ${unique}`
 let promoted = false, nodeID = '', groupID = ''
 try {
  await page.goto('/')
  await page.getByRole('button', { name: '创建账号 ↗' }).click()
  await page.getByRole('textbox', { name: '邮箱', exact: true }).fill(email)
  await page.getByRole('textbox', { name: '密码', exact: true }).fill('access-browser-test-password')
  await page.getByRole('button', { name: '创建账号', exact: true }).last().click()
  await expect(page.getByRole('heading', { name: '欢迎回来。' })).toBeVisible()
  execFileSync('bash', ['../scripts/api.sh', '--promote-admin', email], { stdio: 'pipe' }); promoted = true
  // Role changes revoke existing sessions; authenticate with the new role.
  const loginResponse = await page.request.post('/api/v1/auth/login', { data: { email, password: 'access-browser-test-password' }, headers: { Origin: 'http://127.0.0.1:5173' } })
  expect(loginResponse.status()).toBe(200)
  const nodeResponse = await page.request.post('/api/v1/admin/nodes', { data: { name: nodeName }, headers: { Origin: origin } })
  expect(nodeResponse.status()).toBe(201)
  nodeID = (await nodeResponse.json()).node_id
  const planResponse = await page.request.post('/api/v1/admin/plans', { data: { id: '', name: planName, description: '浏览器权限测试专用', price_minor: 990000, currency: 'CNY', duration_days: 30, quota_bytes: 1073741824 }, headers: { Origin: origin } })
  expect(planResponse.status()).toBe(201)
  const planID = (await planResponse.json()).id
  await page.reload()
  const navigation = page.getByRole('navigation', { name: '主导航' })
  await navigation.getByRole('button', { name: /节点管理/ }).click()
  const nodeRow = page.getByRole('row').filter({ hasText: nodeID.slice(0, 12) })
  await nodeRow.getByRole('button', { name: '代理配置', exact: true }).click()
  await page.getByRole('button', { name: '＋ 添加入站' }).click()
  await page.getByRole('textbox', { name: 'TLS 域名 1', exact: true }).fill('access.example.test')
  await page.getByRole('textbox', { name: '证书名称 1', exact: true }).fill('access-node')
  await page.getByTestId('binding-access-0').click()
  await page.locator('.n-base-select-option').filter({ hasText: '按套餐节点组' }).click()
  await expect(page.getByRole('textbox', { name: '授权账号 1', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: '保存并下发', exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: '配置已保存' })).toBeVisible()
  await nodeRow.getByRole('button', { name: '代理配置', exact: true }).click()
  await expect(page.getByTestId('binding-access-0')).toContainText('按套餐节点组')
  await expect(page.getByRole('textbox', { name: '授权账号 1', exact: true })).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(page.getByRole('button', { name: '保存并下发', exact: true })).toHaveCount(0)
  await navigation.getByRole('button', { name: /权限管理/ }).click()
  await expect(page.getByRole('heading', { name: '权限管理', exact: true })).toBeVisible()
  await page.getByRole('button', { name: '＋ 新建节点组' }).click()
  await page.getByRole('textbox', { name: '节点组名称', exact: true }).fill(groupName)
  await page.getByTestId('group-nodes').click()
  await page.getByRole('textbox', { name: '节点组成员', exact: true }).fill(nodeName)
  await page.locator('.n-base-select-option').filter({ hasText: nodeName }).click()
  // Close the multi-select before submitting the modal form.
  await page.getByRole('textbox', { name: '节点组名称', exact: true }).click()
  const created = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/node-groups') && r.request().method() === 'POST')
  await page.getByRole('button', { name: '保存节点组', exact: true }).click()
  const groupResponse = await created; expect(groupResponse.status()).toBe(201)
  const group = await groupResponse.json(); groupID = group.id
  const groupRow = page.locator(`[data-group-id="${groupID}"]`)
  await expect(groupRow.getByText('已启用', { exact: true })).toBeVisible()
  await expect(groupRow).toContainText(nodeName)
  const planRow = page.locator(`[data-plan-id="${planID}"]`)
  await page.getByTestId(`plan-groups-${planID}`).click()
  await page.getByRole('textbox', { name: `${planName} 的节点组`, exact: true }).fill(groupName)
  await page.locator('.n-base-select-option').filter({ hasText: groupName }).click()
  await planRow.locator('strong').click()
  await planRow.getByRole('button', { name: '保存套餐授权', exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: '仅用于之后创建的订单' })).toBeVisible()
  await expect(planRow).toContainText(`当前：${groupName}`)
  await page.getByRole('button', { name: '刷新权限与事件', exact: true }).click()
  await expect(page.getByRole('heading', { name: '业务事件投递', exact: true })).toBeVisible()
  await expect(page.getByRole('row').filter({ hasText: groupID.slice(0, 12) }).filter({ hasText: 'node_group.changed' }).first()).toBeVisible()
  await expect(async () => {
   await page.getByRole('button', { name: '刷新权限与事件', exact: true }).click()
   await expect(page.getByRole('row').filter({ hasText: groupID.slice(0, 12) }).filter({ hasText: 'node_group.changed' }).first().getByText('已投递', { exact: true })).toBeVisible()
  }).toPass({ timeout: 15000, intervals: [500, 1000, 1500] })
  page.once('dialog', dialog => dialog.accept())
  await groupRow.getByRole('button', { name: '停用节点组', exact: true }).click()
  await expect(groupRow.getByText('已停用', { exact: true })).toBeVisible()
  page.once('dialog', dialog => dialog.accept())
  await groupRow.getByRole('button', { name: '启用节点组', exact: true }).click()
  await expect(groupRow.getByText('已启用', { exact: true })).toBeVisible()
  await page.evaluate(() => window.scrollTo(0, 0))
  await page.screenshot({ path: 'test-results/access-manager.png', fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.getByRole('heading', { name: '权限管理', exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: 'test-results/access-mobile.png', fullPage: true })
 } finally {
  try {
   if (groupID) {
    const response = await page.request.get('/api/v1/admin/access')
    if (response.ok()) {
     const access = await response.json(), group = access.groups.find((g: { id: string }) => g.id === groupID)
     if (group) await page.request.post(`/api/v1/admin/node-groups/${groupID}`, { data: { name: group.name, enabled: false, node_ids: group.node_ids, revision: group.revision }, headers: { Origin: origin } })
    }
   }
   if (nodeID) await page.request.post(`/api/v1/admin/nodes/${nodeID}/revoke`, { data: {}, headers: { Origin: origin } })
  } finally {
   if (promoted) execFileSync('bash', ['../scripts/api.sh', '--demote-admin', email], { stdio: 'pipe' })
  }
 }
})
