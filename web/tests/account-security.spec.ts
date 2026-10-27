import { test, expect } from '@playwright/test'

const origin = 'http://127.0.0.1:5173'
interface DevelopmentMail { subject: string; body: string }
function actionLink(messages: DevelopmentMail[], action: 'verify' | 'reset') {
 for (const mail of messages) {
  const match = mail.body.match(new RegExp(`#account-action=${action}&token=([0-9a-f]{64})\\b`))
  if (match) return `/#account-action=${action}&token=${match[1]}`
 }
 throw new Error(`Development ${action} mail missing`)
}

test('用户验证邮箱、通过邮件链接重置密码并使旧登录失效', async ({ page }) => {
 test.setTimeout(90_000)
 const email = `security-${Date.now()}@example.test`, initialPassword = 'security-initial-password', replacementPassword = 'security-replacement-password'
 await page.goto('/')
 await page.getByRole('button', { name: '创建账号 ↗' }).click()
 await page.getByRole('textbox', { name: '邮箱', exact: true }).fill(email)
 await page.getByRole('textbox', { name: '密码', exact: true }).fill(initialPassword)
 await page.getByRole('button', { name: '创建账号', exact: true }).last().click()
 await expect(page.getByRole('heading', { name: '欢迎回来。' })).toBeVisible()
 const navigation = page.getByRole('navigation', { name: '主导航' })
 await navigation.getByRole('button', { name: /账号安全/ }).click()
 await expect(page.getByText('邮箱未验证', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '发送验证邮件', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '验证邮件请求已提交' })).toBeVisible()
 let response = await page.request.get('/api/v1/me/development-mail')
 expect(response.status()).toBe(200)
 const verifyLink = actionLink(await response.json(), 'verify')
 await page.goto(verifyLink)
 await expect(page.getByRole('heading', { name: '确认邮箱验证', exact: true })).toBeVisible()
 expect(new URL(page.url()).hash).toBe('')
 // Opening a mail link must not consume it before the explicit confirmation.
 expect((await (await page.request.get('/api/v1/me')).json()).email_verified_at).toBeNull()
 // A malformed fragment must discard any previous in-memory action, without
 // consuming that valid link or leaving its confirmation button active.
 await page.goto('/#account-action=verify&token=invalid')
 await expect(page.getByRole('status').filter({ hasText: '邮件链接格式不正确' })).toBeVisible()
 await expect(page.getByRole('button', { name: '确认验证邮箱', exact: true })).toHaveCount(0)
 expect(new URL(page.url()).hash).toBe('')
 await page.goto(verifyLink)
 await expect(page.getByRole('heading', { name: '确认邮箱验证', exact: true })).toBeVisible()
 await expect(page.getByRole('status').filter({ hasText: '邮件链接格式不正确' })).toHaveCount(0)
 await page.getByRole('button', { name: '确认验证邮箱', exact: true }).click()
 await expect(page.getByText('邮箱已验证', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '读取我的开发邮件', exact: true }).click()
 await expect(page.getByRole('heading', { name: '开发邮件收件箱', exact: true })).toBeVisible()
 // Request recovery using the same public endpoint the login dialog exposes.
 response = await page.request.post('/api/v1/auth/password/forgot', { data: { email }, headers: { Origin: origin } })
 expect(response.status()).toBe(202)
 response = await page.request.get('/api/v1/me/development-mail')
 const resetLink = actionLink(await response.json(), 'reset')
 // A second browser context holds a separate existing session that must also end.
 const oldSession = await page.context().browser()!.newContext()
 try {
  const login = await oldSession.request.post(`${origin}/api/v1/auth/login`, { data: { email, password: initialPassword }, headers: { Origin: origin } })
  expect(login.status()).toBe(200)
  await page.goto(resetLink)
  await expect(page.getByRole('heading', { name: '设置新密码', exact: true })).toBeVisible()
  expect(new URL(page.url()).hash).toBe('')
  expect((await oldSession.request.get(`${origin}/api/v1/me`)).status()).toBe(200)
  await page.getByRole('textbox', { name: '重置新密码', exact: true }).fill(replacementPassword)
  await page.getByRole('textbox', { name: '确认重置新密码', exact: true }).fill(replacementPassword)
  await page.getByRole('button', { name: '重置密码并退出所有设备', exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: '密码已更新，请重新登录' })).toBeVisible()
  expect((await page.request.get('/api/v1/me')).status()).toBe(401)
  expect((await oldSession.request.get(`${origin}/api/v1/me`)).status()).toBe(401)
  expect((await oldSession.request.post(`${origin}/api/v1/auth/login`, { data: { email, password: initialPassword }, headers: { Origin: origin } })).status()).toBe(401)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.getByRole('textbox', { name: '邮箱', exact: true }).fill(email)
  await page.getByRole('textbox', { name: '密码', exact: true }).fill(replacementPassword)
  await page.getByRole('button', { name: '登录', exact: true }).last().click()
  await expect(page.getByRole('heading', { name: '欢迎回来。' })).toBeVisible()
  await navigation.getByRole('button', { name: /账号安全/ }).click()
  await expect(page.getByText('邮箱已验证', { exact: true })).toBeVisible()
  await page.evaluate(() => window.scrollTo(0, 0))
  await page.screenshot({ path: 'test-results/account-security.png', fullPage: true })
  await page.getByRole('button', { name: '退出', exact: true }).click()
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await page.getByRole('button', { name: '忘记密码？', exact: true }).click()
  await expect(page.getByRole('heading', { name: '找回密码', exact: true })).toBeVisible()
  await page.getByRole('textbox', { name: '找回密码邮箱', exact: true }).fill(`unknown-${Date.now()}@example.test`)
  await page.getByRole('button', { name: '发送重置邮件', exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: '如果该邮箱已注册' })).toBeVisible()
 } finally { await oldSession.close() }
})
