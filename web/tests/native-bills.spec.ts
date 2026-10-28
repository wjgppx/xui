import { test, expect, type Page } from '@playwright/test'
import type { BillDownloadJob, StatementDetail, StatementSource, StatementSummary } from '../src/api'

const statementID = '1'.repeat(32), runID = '2'.repeat(32), jobID = '3'.repeat(32), created = '2026-10-28T03:00:00Z'
const shanghaiYesterday = () => new Date(Date.now() + 8 * 60 * 60 * 1000 - 24 * 60 * 60 * 1000).toISOString().slice(0, 10)
const nativeBytes = Buffer.from('交易时间,公众账号ID,商户号,原始账单内容\r\n本地界面文件传输用例\r\n', 'utf8')
function source(integrity: StatementSource['integrity'] = 'unverified_upload', format = 'wechat_refund_v1'): StatementSource {
 return { id: '4'.repeat(32), statement_id: statementID, format, kind: integrity === 'unverified_upload' ? 'upload' : 'download', digest: '5'.repeat(64), integrity, merchant_fingerprint: integrity === 'unverified_upload' ? '' : '6'.repeat(64), download_job_id: integrity === 'unverified_upload' ? null : jobID, imported_by: 'finance', created_at: created }
}
function detail(): StatementDetail {
 return {
  statement: { id: statementID, provider: 'wechat', kind: 'refund', date: '2026-10-28', coverage: 'partial', digest: '7'.repeat(64), imported_by: 'finance', row_count: 1, total_minor: '1900', created_at: created },
  latest_run: { id: runID, statement_id: statementID, revision: 1, matched_count: 0, difference_count: 1, local_candidate_count: 0, as_of: created, created_by: 'finance' },
  runs: [{ id: runID, statement_id: statementID, revision: 1, matched_count: 0, difference_count: 1, local_candidate_count: 0, as_of: created, created_by: 'finance' }], sources: [source()]
 }
}
function queuedJob(): BillDownloadJob {
 return { id: jobID, provider: 'wechat', kind: 'payment', date: shanghaiYesterday(), status: 'queued', attempts: 0, last_outcome: 'requested', statement_id: null, requested_by: 'finance', created_at: created, updated_at: created, next_attempt_at: created, completed_at: null }
}
async function fixture(page: Page, enabled = false) {
 const state = { imports: [] as Record<string, unknown>[], jobPosts: [] as Record<string, unknown>[], jobs: [] as BillDownloadJob[], summaries: [] as StatementSummary[], report: detail(), failNative: false }
 await page.route('**/api/v1/**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname, method = req.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: enabled })
  if (path === '/api/v1/me') return reply({ id: 'finance', email: 'finance@example.test', role: 'finance', permissions: ['statements.read', 'statements.import', 'statements.reconcile', 'statements.review', 'statements.download'], email_verified_at: created })
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements'].includes(path)) return reply([])
  if (path === '/api/v1/admin/bill-downloads' && method === 'GET') return reply({ items: state.jobs })
  if (path === '/api/v1/admin/bill-schedules' && method === 'GET') return reply({ items: [] })
  if (path === '/api/v1/admin/bill-downloads' && method === 'POST') { const body = req.postDataJSON(); state.jobPosts.push(body); const job = { ...queuedJob(), ...body }; state.jobs = [job]; return reply(job) }
  if (path === '/api/v1/admin/statements') return reply({ items: state.summaries })
  if (path === '/api/v1/admin/statements/native') {
   const body = req.postDataJSON(); state.imports.push(body)
   if (state.failNative) return reply({ error: '原始账单汇总与明细不一致，请核对所选格式' }, 400)
   state.report.statement = { ...state.report.statement, provider: body.provider, kind: body.kind, date: body.date, coverage: body.coverage }
   state.report.sources = [source('unverified_upload', String(body.format))]
   state.summaries = [{ ...state.report.statement, latest_run: state.report.latest_run }]
   return reply(state.report)
  }
  if (path === `/api/v1/admin/statements/${statementID}`) return reply(state.report)
  if (path === `/api/v1/admin/statements/${statementID}/runs/${runID}/findings`) return reply({ items: [{ id: '8'.repeat(32), run_id: runID, row_number: 1, payment_id: 'merchant-payment', refund_id: state.report.statement.kind === 'refund' ? 'merchant-refund' : '', codes: ['amount_mismatch'], statement_transaction_id: 'original-transaction', local_transaction_id: 'original-transaction', statement_provider_refund_id: 'gateway-refund', local_provider_refund_id: 'gateway-refund', statement_amount_minor: '1900', local_amount_minor: '1800', local_provider: state.report.statement.provider, local_status: 'processing', local_confirmed_at: null, statement_occurred_at: created, review_count: 0, latest_review: null }], total: 1, offset: 0, limit: 100 })
  return reply({ error: 'unexpected fixture endpoint' }, 404)
 })
 return state
}
async function navigate(page: Page) { await page.goto('/'); await page.getByRole('navigation').getByRole('button', { name: /商户账单/ }).click(); await expect(page.getByRole('heading', { name: '商户账单对账', exact: true })).toBeVisible() }
async function upload(page: Page, format = 'wechat_refund_v1', bytes = nativeBytes) {
 await page.getByRole('button', { name: '导入原始账单', exact: true }).click()
 await page.getByLabel('原始账单格式', { exact: true }).selectOption(format)
 await page.getByLabel('原始账单日期', { exact: true }).fill('2026-10-28')
 await page.getByLabel('原始账单文件', { exact: true }).setInputFiles({ name: format.startsWith('alipay') ? 'merchant-original.zip' : 'merchant-original.csv', mimeType: 'application/octet-stream', buffer: bytes })
 await expect(page.getByText(/已选择原始文件 merchant-original/)).toBeVisible()
}

test('原始文件按字节提交，格式错误保留文件，成功后显示毛额与上传来源', async ({ page }) => {
 const state = await fixture(page); state.failNative = true
 await navigate(page); await upload(page)
 await page.getByRole('button', { name: '解析并导入原始账单', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '原始账单汇总与明细不一致' })).toBeVisible()
 await expect(page.getByText('已选择原始文件 merchant-original.csv', { exact: true })).toBeVisible()
 state.failNative = false
 await page.getByRole('button', { name: '解析并导入原始账单', exact: true }).click()
 await expect(page.getByTestId('statement-report')).toBeVisible()
 expect(state.imports[1]).toEqual({ provider: 'wechat', kind: 'refund', date: '2026-10-28', coverage: 'partial', format: 'wechat_refund_v1', content_base64: nativeBytes.toString('base64') })
 await expect(page.getByText('账单 ¥19.00', { exact: true })).toBeVisible()
 await page.getByText('查看账单来源标识', { exact: true }).click()
 await expect(page.getByText('手动上传 · 未验证渠道来源', { exact: true })).toBeVisible()
 await expect(page.getByText('来源记录只保存格式、摘要与处理方式，不保存原始文件、签名下载地址或令牌；来源校验不表示收款或退款已确认。', { exact: true })).toBeVisible()
})

test('官方下载关闭时禁止排队，支付宝原始上传仍可部分核对且保留二进制', async ({ page }) => {
 const state = await fixture(page), binary = Buffer.from([0x50, 0x4b, 0x03, 0x04, 0xd6, 0xd0, 0xce, 0xc4])
 await navigate(page)
 await expect(page.getByRole('button', { name: '从渠道获取账单', exact: true })).toBeDisabled()
 await expect(page.getByRole('status').filter({ hasText: '官方下载未启用，仍可手动导入' })).toBeVisible()
 await upload(page, 'alipay_trade_v1', binary)
 await page.getByLabel('原始记录类型', { exact: true }).selectOption('refund')
 await expect(page.getByText(/支付宝当前格式仅按部分账单核对/)).toBeVisible()
 await expect(page.getByLabel('原始账单范围', { exact: true })).toHaveCount(0)
 await page.getByRole('button', { name: '解析并导入原始账单', exact: true }).click()
 await expect(page.getByTestId('statement-report')).toBeVisible()
 expect(state.imports[0]).toEqual({ provider: 'alipay', kind: 'refund', date: '2026-10-28', coverage: 'partial', format: 'alipay_trade_v1', content_base64: binary.toString('base64') })
 expect(state.jobPosts).toHaveLength(0)
})

test('下载排队不显示已导入，完成刷新后可打开账单并说明微信内容校验范围', async ({ page }) => {
 const state = await fixture(page, true)
 await navigate(page)
 await page.getByRole('button', { name: '从渠道获取账单', exact: true }).click()
 await page.getByLabel('下载渠道', { exact: true }).selectOption('wechat')
 await page.getByLabel('下载记录类型', { exact: true }).selectOption('payment')
 await page.getByRole('button', { name: '提交下载任务', exact: true }).click()
 await expect(page.getByText('等待下载', { exact: true })).toBeVisible()
 await expect(page.getByText('尚未导入账单', { exact: true })).toBeVisible()
 await expect(page.getByRole('button', { name: '打开已导入账单', exact: true })).toHaveCount(0)
 expect(state.jobPosts).toEqual([{ provider: 'wechat', kind: 'payment', date: shanghaiYesterday() }])
 state.jobs[0] = { ...state.jobs[0], status: 'succeeded', last_outcome: 'succeeded', statement_id: statementID, completed_at: created, next_attempt_at: null }
 state.report.statement = { ...state.report.statement, kind: 'payment', date: shanghaiYesterday(), coverage: 'full' }
 state.report.sources = [source('wechat_signed_sha1', 'wechat_success_v1')]
 state.summaries = [{ ...state.report.statement, latest_run: state.report.latest_run }]
 await page.getByRole('button', { name: '刷新下载任务', exact: true }).click()
 await page.getByRole('button', { name: '打开已导入账单', exact: true }).click()
 await expect(page.getByTestId('statement-report')).toBeVisible()
 await page.getByText('查看账单来源标识', { exact: true }).click()
 await expect(page.getByText('渠道下载 · 已校验下载内容；仅用于对账', { exact: true })).toBeVisible()
 expect(await page.evaluate(() => localStorage.length)).toBe(0)
 await expect(page.locator('body')).not.toContainText('token=')
 await expect(page.locator('body')).not.toContainText('download_url')
})

test('支付宝来源不声称文件付款验签，超大原始文件拒绝且移动端无横向溢出', async ({ page }) => {
 const state = await fixture(page, true)
 state.report.statement.provider = 'alipay'; state.report.sources = [source('alipay_signed_url', 'alipay_trade_v1')]
 state.summaries = [{ ...state.report.statement, latest_run: state.report.latest_run }]
 await navigate(page)
 await page.getByRole('button', { name: '查看对账结果', exact: true }).click()
 await page.getByText('查看账单来源标识', { exact: true }).click()
 await expect(page.getByText('渠道下载 · 已验签下载地址，经 HTTPS 获取', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '导入原始账单', exact: true }).click()
 await page.getByLabel('原始账单文件', { exact: true }).setInputFiles({ name: 'too-large.zip', mimeType: 'application/zip', buffer: Buffer.alloc(8 * 1024 * 1024 + 1) })
 await expect(page.getByRole('alert').filter({ hasText: '原始账单文件不能超过 8 MiB' })).toBeVisible()
 await expect(page.getByRole('button', { name: '解析并导入原始账单', exact: true })).toBeDisabled()
 expect(state.imports).toHaveLength(0)
 await page.getByRole('button', { name: '取消', exact: true }).click()
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})
