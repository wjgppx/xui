import { test, expect, type Page } from '@playwright/test'
import type { MerchantStatement, StatementDetail, StatementFinding, StatementReview, StatementRun, StatementSummary } from '../src/api'

const statementID = 'a'.repeat(32), firstRunID = 'b'.repeat(32), secondRunID = 'c'.repeat(32)
const paymentID = 'd'.repeat(32), created = '2026-10-28T03:00:00Z'
const header = 'payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at\r\n'
const largeCSV = header + `${paymentID},merchant-transaction,,,9223372036854775807,CNY,2026-10-28T10:00:00+08:00\r\n`
function run(id = firstRunID, revision = 1): StatementRun { return { id, statement_id: statementID, revision, matched_count: 0, difference_count: 101, local_candidate_count: 1, as_of: created, created_by: 'finance' } }
function batch(): MerchantStatement { return { id: statementID, provider: 'alipay', kind: 'payment', date: '2026-10-28', coverage: 'full', digest: 'e'.repeat(64), imported_by: 'finance', row_count: 100, total_minor: '190000', created_at: created } }
function finding(index: number, runID = firstRunID): StatementFinding {
 const localOnly = index === 0
 return {
  id: `${runID[0]}${index.toString(16).padStart(31, '0')}`, run_id: runID, row_number: localOnly ? null : index,
  payment_id: `payment-${index}`, refund_id: '', codes: [localOnly ? 'local_only' : index === 1 ? 'amount_mismatch' : 'not_confirmed'],
  statement_transaction_id: localOnly ? '' : `statement-transaction-${index}`, local_transaction_id: `local-transaction-${index}`,
  statement_provider_refund_id: '', local_provider_refund_id: '', statement_amount_minor: localOnly ? null : '1900', local_amount_minor: index === 1 ? '2000' : '1900',
  local_provider: 'alipay', local_status: localOnly || index === 1 ? 'paid' : 'ready', local_confirmed_at: created, statement_occurred_at: localOnly ? null : created,
  review_count: 0, latest_review: null
 }
}

async function fixture(page: Page, options: { seed?: boolean; role?: 'finance' | 'support' } = {}) {
 const initialRun = run(), original = batch()
 const state = {
  summaries: (options.seed ? [{ ...original, latest_run: initialRun }] : []) as StatementSummary[],
  details: (options.seed ? { [statementID]: { statement: original, latest_run: initialRun, runs: [initialRun] } } : {}) as Record<string, StatementDetail>,
  findings: { [firstRunID]: Array.from({ length: 101 }, (_, i) => finding(i)) } as Record<string, StatementFinding[]>,
  reviews: {} as Record<string, StatementReview[]>, imports: [] as Record<string, unknown>[], reconcileCalls: 0,
  findingQueries: [] as URLSearchParams[], reviewPosts: [] as Record<string, unknown>[], statementRequests: 0,
  failImport: false, failReconcile: false
 }
 const role = options.role || 'finance'
 const permissions = role === 'finance' ? ['statements.read', 'statements.import', 'statements.reconcile', 'statements.review'] : ['orders.read', 'users.read']
 const reviewed = (item: StatementFinding): StatementFinding => ({ ...item, review_count: (state.reviews[item.id] || []).length, latest_review: state.reviews[item.id]?.[0] || null })
 await page.route('**/api/v1/**', async route => {
  const request = route.request(), url = new URL(request.url()), path = url.pathname, method = request.method()
  const reply = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false })
  if (path === '/api/v1/me') return reply({ id: role, email: `${role}@example.test`, role, permissions, email_verified_at: created })
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements', '/api/v1/admin/orders'].includes(path)) return reply([])
  if (path.includes('/admin/statements') || path.includes('/admin/statement-findings')) state.statementRequests++
  if (path === '/api/v1/admin/bill-downloads' && method === 'GET') return reply({ items: [] })
  if (path === '/api/v1/admin/bill-schedules' && method === 'GET') return reply({ items: [] })
  if (path === '/api/v1/admin/statements' && method === 'GET') return reply({ items: state.summaries })
  if (path === '/api/v1/admin/statements' && method === 'POST') {
   const body = request.postDataJSON(); state.imports.push(body)
   if (state.failImport) return reply({ error: '账单第 2 行金额格式不正确，请使用整数分' }, 400)
   if (!state.details[statementID]) {
    const statement = { ...batch(), provider: body.provider, kind: body.kind, date: body.date, coverage: body.coverage, row_count: 1, total_minor: '9223372036854775807' }
    const importedRun = { ...run(), difference_count: 1, local_candidate_count: 0 }
    state.details[statementID] = { statement, latest_run: importedRun, runs: [importedRun] }
    state.summaries.push({ ...statement, latest_run: importedRun })
    state.findings[firstRunID] = [{ ...finding(1), payment_id: paymentID, statement_amount_minor: statement.total_minor }]
   }
   return reply(state.details[statementID])
  }
  if (path === `/api/v1/admin/statements/${statementID}`) return reply(state.details[statementID])
  if (path === `/api/v1/admin/statements/${statementID}/reconcile`) {
   state.reconcileCalls++
   if (state.failReconcile) return reply({ error: '本次对账发生并发冲突，请稍后重试' }, 409)
   const secondRun = { ...run(secondRunID, 2), difference_count: 1, local_candidate_count: 0 }
   state.details[statementID].latest_run = secondRun; state.details[statementID].runs = [secondRun, initialRun]
   state.summaries[0].latest_run = secondRun
   state.findings[secondRunID] = [finding(1, secondRunID)]
   return reply(secondRun)
  }
  const result = path.match(/^\/api\/v1\/admin\/statements\/[^/]+\/runs\/([^/]+)\/findings$/)
  if (result) {
   state.findingQueries.push(new URLSearchParams(url.search))
   const offset = Number(url.searchParams.get('offset') || 0), limit = Number(url.searchParams.get('limit') || 100), filter = url.searchParams.get('review')
   const items = (state.findings[result[1]] || []).map(reviewed).filter(item => filter === 'unreviewed' ? item.review_count === 0 : filter === 'reviewed' ? item.review_count > 0 : true)
   return reply({ items: items.slice(offset, offset + limit), total: items.length, offset, limit })
  }
  const history = path.match(/^\/api\/v1\/admin\/statement-findings\/([^/]+)\/reviews$/)
  if (history) {
   const id = history[1]
   if (method === 'GET') return reply({ items: state.reviews[id] || [] })
   const body = request.postDataJSON(); state.reviewPosts.push(body)
   const review: StatementReview = { id: `review-${state.reviewPosts.length}`, finding_id: id, actor_id: role, note: body.note, created_at: created }
   state.reviews[id] = [review, ...(state.reviews[id] || [])]
   return reply(review)
  }
  return reply({ error: 'fixture endpoint unavailable' }, 404)
 })
 return state
}
async function navigate(page: Page) {
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /账单/ }).click()
 await expect(page.getByRole('heading', { name: '商户账单对账', exact: true })).toBeVisible()
}
async function setImport(page: Page) {
 await page.getByRole('button', { name: '导入标准账单', exact: true }).click()
 await page.getByLabel('账单渠道', { exact: true }).selectOption('wechat')
 await page.getByLabel('账单日期', { exact: true }).fill('2026-10-28')
 await page.getByLabel('账单范围', { exact: true }).selectOption('full')
 await page.getByRole('checkbox', { name: /我确认这是完整日账单/ }).check()
 await page.getByLabel('标准 CSV 文件', { exact: true }).setInputFiles({ name: 'merchant-day.csv', mimeType: 'text/csv', buffer: Buffer.from(largeCSV) })
 await expect(page.getByText('已选择 merchant-day.csv', { exact: true })).toBeVisible()
}

test('资金操作关闭时仍可导入标准账单，错误保留文件且相同内容复用账单', async ({ page }) => {
 const state = await fixture(page); state.failImport = true
 await navigate(page); await setImport(page)
 await page.getByRole('button', { name: '导入并生成对账结果' }).click()
 await expect(page.getByRole('alert').filter({ hasText: '账单第 2 行金额格式不正确' })).toBeVisible()
 await expect(page.getByText('已选择 merchant-day.csv', { exact: true })).toBeVisible()
 state.failImport = false
 await page.getByRole('button', { name: '导入并生成对账结果' }).click()
 await expect(page.getByTestId('statement-report')).toBeVisible()
 await expect(page.getByText('¥92,233,720,368,547,758.07', { exact: true }).first()).toBeVisible()
 expect(state.imports[1]).toEqual({ provider: 'wechat', kind: 'payment', date: '2026-10-28', coverage: 'full', csv: largeCSV })
 await page.getByRole('button', { name: '导入标准账单', exact: true }).click()
 await expect(page.getByText('已选择 merchant-day.csv', { exact: true })).toHaveCount(0)
 await expect(page.getByRole('button', { name: '导入并生成对账结果' })).toBeDisabled()
 await page.getByRole('button', { name: '取消', exact: true }).click()
 await setImport(page); await page.getByRole('button', { name: '导入并生成对账结果' }).click()
 await expect(page.getByRole('status').filter({ hasText: '读取已有相同账单' })).toBeVisible()
 expect(state.summaries).toHaveLength(1)
 expect(state.details[statementID].runs).toHaveLength(1)
})

test('差异分页、跨日提示及复核说明保留，复核后自动回到有效页', async ({ page }) => {
 const state = await fixture(page, { seed: true })
 await navigate(page); await page.getByRole('button', { name: '查看对账结果' }).click()
 await expect(page.getByText('本地确认、本账单未列出（需核对跨日）', { exact: true })).toBeVisible()
 await page.getByLabel('复核状态', { exact: true }).selectOption('unreviewed')
 await page.getByRole('button', { name: '下一页差异' }).click()
 await expect(page.getByText('第 101–101 项，共 101 项', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '查看与记录复核' }).click()
 await page.getByRole('textbox', { name: '复核说明', exact: true }).fill('已核对渠道原交易，等待可信查询确认')
 await page.getByRole('button', { name: '保存复核说明', exact: true }).click()
 await expect(page.getByText('已核对渠道原交易，等待可信查询确认', { exact: true })).toBeVisible()
 await page.keyboard.press('Escape')
 await expect(page.getByText('第 1–100 项，共 100 项', { exact: true })).toBeVisible()
 await page.getByLabel('复核状态', { exact: true }).selectOption('reviewed')
 await expect(page.getByText('第 1–1 项，共 1 项', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '查看与记录复核' }).click()
 await expect(page.getByText('已核对渠道原交易，等待可信查询确认', { exact: true }).last()).toBeVisible()
 await page.getByRole('textbox', { name: '复核说明', exact: true }).fill('已补充商户记录，原差异继续保留')
 await page.getByRole('button', { name: '保存复核说明', exact: true }).click()
 await expect(page.getByText('已补充商户记录，原差异继续保留', { exact: true }).last()).toBeVisible()
 expect(state.reviewPosts).toEqual([{ note: '已核对渠道原交易，等待可信查询确认' }, { note: '已补充商户记录，原差异继续保留' }])
 expect(state.findings[firstRunID]).toHaveLength(101)
 await page.keyboard.press('Escape'); await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})

test('重新对账失败保留当前结果，新版本不继承旧复核且可查看历史', async ({ page }) => {
 const state = await fixture(page, { seed: true }), oldFinding = state.findings[firstRunID][1]
 state.reviews[oldFinding.id] = [{ id: 'historical-note', finding_id: oldFinding.id, actor_id: 'finance', note: '旧版本已核对，后续仍需观察', created_at: created }]
 await navigate(page); await page.getByRole('button', { name: '查看对账结果' }).click()
 state.failReconcile = true
 await page.getByRole('button', { name: '重新对账', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '对账发生并发冲突' })).toBeVisible()
 await expect(page.getByLabel('对账版本', { exact: true })).toHaveValue(firstRunID)
 await expect(page.locator(`[data-finding-id="${oldFinding.id}"]`).getByText('旧版本已核对，后续仍需观察')).toBeVisible()
 state.failReconcile = false
 await page.getByRole('button', { name: '重新对账', exact: true }).click()
 await expect(page.getByLabel('对账版本', { exact: true })).toHaveValue(secondRunID)
 await expect(page.getByText('旧版本已核对，后续仍需观察', { exact: true })).toHaveCount(0)
 await expect(page.getByText('尚未复核', { exact: true }).last()).toBeVisible()
 await page.getByLabel('对账版本', { exact: true }).selectOption(firstRunID)
 await expect(page.locator(`[data-finding-id="${oldFinding.id}"]`).getByText('旧版本已核对，后续仍需观察')).toBeVisible()
 expect(state.reconcileCalls).toBe(2)
})

test('客服没有商户账单入口且不提前请求账单数据', async ({ page }) => {
 const state = await fixture(page, { role: 'support' })
 await page.goto('/')
 await expect(page.getByRole('button', { name: '退出', exact: true })).toBeVisible()
 await expect(page.getByRole('navigation').getByRole('button', { name: /账单/ })).toHaveCount(0)
 expect(state.statementRequests).toBe(0)
})
