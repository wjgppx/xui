import { test, expect, type Page } from '@playwright/test'
import type { FinancialComparison, FinancialReport, FinancialRun, FinancialSnapshot, StatementDetail, StatementSource } from '../src/api'

const statementID = 'a'.repeat(32), sourceA = 'b'.repeat(32), sourceB = 'c'.repeat(32), sourceLegacy = 'd'.repeat(32)
const created = '2026-10-29T03:00:00Z'
const referenceCSV = 'payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan\r\npayment-0,refund-0,0.10000,10.00000\r\n'
function source(id: string, index: number): StatementSource {
 return { id, statement_id: statementID, format: 'wechat_refund_v1', kind: 'upload', digest: String(index).repeat(64), integrity: 'unverified_upload', merchant_fingerprint: '', download_job_id: null, imported_by: 'finance', created_at: created }
}
function financialRun(id: string, revision: number): FinancialRun {
 return { id, source_id: sourceA, revision, reference_digest: 'e'.repeat(64), matched_count: 0, difference_count: 1, unassessed_count: 100, observed_row_count: 101, reference_row_count: 1, observed_fee_total_yuan: '-0.00001', observed_channel_amount_total_yuan: '92233720368548258.07000', expected_fee_total_yuan: '0.10000', expected_channel_amount_total_yuan: '10.00000', summary_codes: ['fee_total_mismatch'], created_by: 'finance', created_at: created }
}
function financialReport(id = 'report-1', revision = 1): FinancialReport {
 const rows: FinancialComparison[] = Array.from({ length: 101 }, (_, index) => ({ payment_id: index === 0 ? 'observed-payment' : `payment-${index}`, expected_payment_id: index === 0 ? 'payment-0' : '', refund_id: `refund-${index}`, observed_fee_yuan: index === 0 ? '-0.00001' : '0.00000', expected_fee_yuan: index === 0 ? '0.10000' : null, observed_channel_amount_yuan: index === 0 ? '92233720368547758.07000' : '5.00000', expected_channel_amount_yuan: index === 0 ? '10.00000' : null, status: index === 0 ? 'difference' : 'unassessed', codes: index === 0 ? ['identity_mismatch', 'fee_mismatch', 'channel_amount_mismatch'] : ['missing_reference'] }))
 return { run: financialRun(id, revision), rows }
}
function snapshot(id: string): FinancialSnapshot {
 if (id === sourceLegacy) return { available: false, source_id: id, version: '', basis: '', rows: [], fee_total_yuan: null, channel_amount_total_yuan: null, declared_fee_total_yuan: null, declared_channel_amount_total_yuan: null, summary_codes: [], runs: [] }
 const report = financialReport()
 return { available: true, source_id: id, version: 'native-financials-v1', basis: 'wechat_refund', rows: id === sourceA ? report.rows.map(row => ({ payment_id: row.payment_id, refund_id: row.refund_id, fee_yuan: row.observed_fee_yuan, channel_amount_yuan: row.observed_channel_amount_yuan })) : [{ payment_id: 'payment-0', refund_id: 'refund-0', fee_yuan: '0.00000', channel_amount_yuan: null }], fee_total_yuan: id === sourceA ? '-0.00001' : '0.00000', channel_amount_total_yuan: id === sourceA ? '92233720368548258.07000' : null, declared_fee_total_yuan: id === sourceA ? '0.00000' : null, declared_channel_amount_total_yuan: null, summary_codes: id === sourceA ? ['fee_total_mismatch'] : [], runs: id === sourceA ? [report.run] : [] }
}
async function fixture(page: Page, options: { readonly?: boolean } = {}) {
 const grossRun = { id: 'gross-run', statement_id: statementID, revision: 1, matched_count: 1, difference_count: 0, local_candidate_count: 0, as_of: created, created_by: 'finance' }
 const detail: StatementDetail = { statement: { id: statementID, provider: 'wechat', kind: 'refund', date: '2026-10-29', coverage: 'partial', digest: 'f'.repeat(64), imported_by: 'finance', row_count: 101, total_minor: '1900', created_at: created }, latest_run: grossRun, runs: [grossRun], sources: [source(sourceA, 1), source(sourceB, 2), source(sourceLegacy, 3)] }
 const state = { gets: [] as string[], posts: [] as { csv: string }[], failReference: false, failLoad: false, reportGets: [] as string[], snapshots: { [sourceA]: snapshot(sourceA), [sourceB]: snapshot(sourceB), [sourceLegacy]: snapshot(sourceLegacy) }, reports: { 'report-1': financialReport(), 'report-2': financialReport('report-2', 2) } as Record<string, FinancialReport> }
 await page.route('**/api/v1/**', async route => {
  const request = route.request(), path = new URL(request.url()).pathname, method = request.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: false })
  if (path === '/api/v1/me') return reply({ id: 'finance', email: 'finance@example.test', role: 'finance', permissions: ['statements.read', ...(options.readonly ? [] : ['statements.reconcile'])], email_verified_at: created })
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements'].includes(path)) return reply([])
  if (path === '/api/v1/admin/statements') return reply({ items: [{ ...detail.statement, latest_run: grossRun }] })
  if (path === '/api/v1/admin/bill-downloads' || path === '/api/v1/admin/bill-schedules') return reply({ items: [] })
  if (path === `/api/v1/admin/statements/${statementID}`) return reply(detail)
  if (path === `/api/v1/admin/statements/${statementID}/runs/gross-run/findings`) return reply({ items: [], total: 0, offset: 0, limit: 100 })
  const financial = path.match(/^\/api\/v1\/admin\/statements\/[^/]+\/sources\/([^/]+)\/financials(.*)$/)
  if (financial) {
   const id = financial[1]!, suffix = financial[2]
   if (method === 'GET' && !suffix) { state.gets.push(id); return state.failLoad ? reply({ error: '费用观察暂时不可用' }, 503) : reply(state.snapshots[id]) }
   if (method === 'POST' && suffix === '/reconcile') {
    state.posts.push(request.postDataJSON())
    if (state.failReference) return reply({ error: '预期金额第 2 行手续费不正确' }, 400)
    if (id !== sourceA) return reply({ error: 'unexpected source' }, 409)
    const result = state.reports['report-2']!
    state.snapshots[id]!.runs = [result.run, state.reports['report-1']!.run]
    return reply(result)
   }
   if (method === 'GET' && suffix?.startsWith('/runs/')) { const id = suffix.slice('/runs/'.length); state.reportGets.push(id); return reply(state.reports[id]) }
  }
  return reply({ error: 'unexpected fixture endpoint' }, 404)
 })
 return state
}
async function navigate(page: Page) {
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /商户账单/ }).click()
 await page.getByRole('button', { name: '查看对账结果', exact: true }).click()
 await expect(page.getByTestId('statement-report')).toBeVisible()
}
async function open(page: Page) {
 await page.getByRole('button', { name: '手续费与渠道金额核对', exact: true }).click()
 await expect(page.getByLabel('费用观察来源', { exact: true })).toBeVisible()
}
async function upload(page: Page) {
 await page.getByLabel('预期金额 CSV', { exact: true }).setInputFiles({ name: 'expected.csv', mimeType: 'text/csv', buffer: Buffer.from(referenceCSV) })
 await expect(page.getByText('已选择预期文件 expected.csv', { exact: true })).toBeVisible()
}

test('按需读取来源费用，五位负数和大金额准确显示，旧来源及空字段不当作零', async ({ page }) => {
 const state = await fixture(page)
 await navigate(page)
 expect(state.gets).toEqual([])
 await open(page)
 const panel = page.getByTestId('statement-financials')
 await expect(panel.getByText('-¥0.00001', { exact: true }).first()).toBeVisible()
 await expect(panel.getByText('¥92,233,720,368,547,758.07000', { exact: true })).toBeVisible()
 await expect(panel.getByText('手续费明细合计与文件汇总不一致', { exact: true })).toBeVisible()
 await expect(panel.getByText('以下仅展示来源金额，尚未与预期金额核对。未提供的字段显示为“未提供”。', { exact: true })).toBeVisible()
 await page.getByLabel('费用观察来源', { exact: true }).selectOption(sourceB)
 await expect(panel.getByText('¥0.00000', { exact: true }).first()).toBeVisible()
 await expect(panel.locator('[data-financial-row="refund-0"]')).toContainText('未提供')
 await expect(panel.getByText('-¥0.00001', { exact: true })).toHaveCount(0)
 await page.getByLabel('费用观察来源', { exact: true }).selectOption(sourceLegacy)
 await expect(panel.getByRole('status').filter({ hasText: '不能按零处理' })).toBeVisible()
 await expect(page.getByRole('button', { name: '核对预期金额', exact: true })).toHaveCount(0)
 expect(state.gets).toEqual([sourceA, sourceB, sourceLegacy])
})

test('预期文件失败后保留，相同参考值复用报告，历史可查看且缺失预期保持未核对', async ({ page }) => {
 const state = await fixture(page); state.failReference = true
 await navigate(page); await open(page); await upload(page)
 await page.getByRole('button', { name: '核对预期金额', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '预期金额第 2 行手续费不正确' })).toBeVisible()
 await expect(page.getByText('已选择预期文件 expected.csv', { exact: true })).toBeVisible()
 state.failReference = false
 await page.getByRole('button', { name: '核对预期金额', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '已生成或读取相同预期金额' })).toBeVisible()
 expect(state.posts).toEqual([{ csv: referenceCSV }, { csv: referenceCSV }])
 const panel = page.getByTestId('statement-financials')
 await expect(panel.getByText('尚未核对 100', { exact: true })).toBeVisible()
 await expect(panel.getByText('存在差异 1', { exact: true })).toBeVisible()
 await expect(panel.getByText('预期支付 payment-0', { exact: true })).toBeVisible()
 await expect(panel.getByText('退款对应的支付编号不一致', { exact: true })).toBeVisible()
 await page.getByLabel('费用核对筛选', { exact: true }).selectOption('unassessed')
 await expect(panel.locator('[data-financial-row]')).toHaveCount(100)
 await expect(panel.getByText('未提供预期记录', { exact: true }).first()).toBeVisible()
 await expect(panel.getByText('手续费明细合计与文件汇总不一致', { exact: true })).toBeVisible()
 await page.getByLabel('费用核对版本', { exact: true }).selectOption('report-1')
 await expect(page.getByLabel('费用核对版本', { exact: true })).toHaveValue('report-1')
 expect(state.reportGets).toEqual(['report-1'])
 await upload(page); await page.getByRole('button', { name: '核对预期金额', exact: true }).click()
 await expect(page.getByLabel('费用核对版本', { exact: true })).toHaveValue('report-2')
 expect(state.snapshots[sourceA]!.runs).toHaveLength(2)
 await page.getByRole('button', { name: '收起费用核对', exact: true }).click()
 await open(page)
 await expect(page.getByText('已选择预期文件 expected.csv', { exact: true })).toHaveCount(0)
 expect(await page.evaluate(() => localStorage.length)).toBe(0)
})

test('费用读取故障可独立恢复，只读权限保留历史，分页和手机布局可用', async ({ page }) => {
 const state = await fixture(page, { readonly: true }); state.failLoad = true
 await navigate(page); await open(page)
 await expect(page.getByRole('alert').filter({ hasText: '费用观察暂时不可用' })).toBeVisible()
 await expect(page.getByRole('heading', { name: '商户账单对账', exact: true })).toBeVisible()
 state.failLoad = false
 await page.getByRole('button', { name: '重读费用观察', exact: true }).click()
 await expect(page.getByText('当前权限仅可查看来源观察与已保存报告。', { exact: true })).toBeVisible()
 await expect(page.getByLabel('预期金额 CSV', { exact: true })).toHaveCount(0)
 await page.getByRole('button', { name: '下一页费用记录', exact: true }).click()
 await expect(page.getByText('费用记录第 101–101 项，共 101 项', { exact: true })).toBeVisible()
 await page.getByLabel('费用核对版本', { exact: true }).selectOption('report-1')
 await expect(page.getByText('费用记录第 1–100 项，共 101 项', { exact: true })).toBeVisible()
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 expect(state.posts).toHaveLength(0)
})

test('切换来源或收起后清除预期文件，超限文件拒绝且晚到响应不会覆盖新来源', async ({ page }) => {
 const state = await fixture(page)
 await navigate(page); await open(page); await upload(page)
 await page.getByLabel('费用观察来源', { exact: true }).selectOption(sourceB)
 await expect(page.getByText('已选择预期文件 expected.csv', { exact: true })).toHaveCount(0)
 await page.getByLabel('预期金额 CSV', { exact: true }).setInputFiles({ name: 'too-large.csv', mimeType: 'text/csv', buffer: Buffer.alloc(1024 * 1024 + 1, 65) })
 await expect(page.getByRole('alert').filter({ hasText: '预期金额 CSV 不能超过 1 MiB' })).toBeVisible()
 await expect(page.getByRole('button', { name: '核对预期金额', exact: true })).toBeDisabled()
 let release!: () => void, started!: () => void
 const held = new Promise<void>(resolve => { release = resolve })
 const requested = new Promise<void>(resolve => { started = resolve })
 const response = page.waitForResponse(result => result.url().endsWith(`/sources/${sourceA}/financials`))
 await page.route(`**/api/v1/admin/statements/${statementID}/sources/${sourceA}/financials`, async route => { started(); await held; await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(state.snapshots[sourceA]) }) })
 await page.getByLabel('费用观察来源', { exact: true }).selectOption(sourceA)
 await requested
 await page.getByLabel('费用观察来源', { exact: true }).selectOption(sourceLegacy)
 await expect(page.getByRole('status').filter({ hasText: '不能按零处理' })).toBeVisible()
 release()
 await (await response).finished()
 await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
 await expect(page.getByLabel('费用观察来源', { exact: true })).toHaveValue(sourceLegacy)
 await expect(page.getByTestId('statement-financials').getByText('-¥0.00001', { exact: true })).toHaveCount(0)
 await expect(page.getByRole('button', { name: '核对预期金额', exact: true })).toHaveCount(0)
})
