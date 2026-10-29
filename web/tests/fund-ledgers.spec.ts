import { test, expect, type Page } from '@playwright/test'
import type { FundLedger, FundLedgerEntry, FundReconciliation } from '../src/api'

const merchantID = 'a'.repeat(32), bankID = 'b'.repeat(32), importedID = 'c'.repeat(32), reportID = 'd'.repeat(32)
const created = '2026-10-29T04:00:00Z'
const header = 'sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\n'
const importedCSV = header + '1,large-credit,2026-10-29T12:00:00+08:00,other,credit,92233720368547758.07000,92233720368547758.07000,,\n2,tiny-debit,2026-10-29T12:01:00+08:00,fee,debit,0.00001,92233720368547758.07000,,\n'
type InputRow = { id: string; kind: string; direction: 'credit' | 'debit'; amount: string; transfer?: string; other?: string }
function yuan(units: bigint) {
 const absolute = units < 0n ? -units : units
 return `${units < 0n ? '-' : ''}${absolute / 100000n}.${(absolute % 100000n).toString().padStart(5, '0')}`
}
function ledger(id: string, provider: 'alipay' | 'bank', account: string, date: string, input: InputRow[]): FundLedger {
 let balance = provider === 'bank' ? 0n : 1000000000n, credit = 0n, debit = 0n
 const opening = yuan(balance)
 const rows: FundLedgerEntry[] = input.map((row, index) => {
  const amount = BigInt(row.amount.replace('.', ''))
  if (row.direction === 'credit') { balance += amount; credit += amount } else { balance -= amount; debit += amount }
  return { sequence: index + 1, entry_id: row.id, occurred_at: new Date(new Date(`${date}T04:00:00Z`).getTime() + index * 1000).toISOString(), kind: row.kind, direction: row.direction, amount_yuan: row.amount, balance_after_yuan: yuan(balance), expected_balance_yuan: yuan(balance), step_expected_balance_yuan: yuan(balance), transfer_id: row.transfer || '', counterparty_account_key: row.other || '', codes: [] }
 })
 return { id, version: 'fund-ledger-v1', provider, account_key: account, date, coverage: 'partial', currency: 'CNY', digest: 'f'.repeat(64), opening_balance_yuan: opening, closing_balance_yuan: yuan(balance), expected_closing_balance_yuan: yuan(balance), total_credit_yuan: yuan(credit), total_debit_yuan: yuan(debit), row_count: rows.length, balance_difference_count: 0, summary_codes: [], imported_by: 'finance', created_at: created, rows }
}
function fixtures(many: boolean) {
 const m = ledger(merchantID, 'alipay', 'merchant_main', '2026-10-28', [
  { id: 'm-match', kind: 'withdrawal', direction: 'debit', amount: '20.12345', transfer: 'match', other: 'bank_main' },
  { id: 'm-difference', kind: 'withdrawal', direction: 'debit', amount: '20.00000', transfer: 'different', other: 'bank_main' },
  { id: 'm-waiting', kind: 'withdrawal', direction: 'debit', amount: '5.00000', transfer: 'waiting', other: 'bank_main' },
  { id: 'm-dup-a', kind: 'withdrawal', direction: 'debit', amount: '1.00000', transfer: 'duplicate', other: 'bank_main' },
  { id: 'm-dup-b', kind: 'withdrawal', direction: 'debit', amount: '2.00000', transfer: 'duplicate', other: 'bank_main' },
  { id: 'm-outside', kind: 'withdrawal', direction: 'debit', amount: '7.00000', transfer: 'match', other: 'other_bank' },
  { id: 'm-other', kind: 'payment', direction: 'credit', amount: '9.00000' },
  ...(many ? Array.from({ length: 100 }, (_, i) => ({ id: `ordinary-${i}`, kind: 'other', direction: 'credit' as const, amount: '1.00000' })) : [])
 ])
 const b = ledger(bankID, 'bank', 'bank_main', '2026-10-29', [
  { id: 'b-match', kind: 'settlement', direction: 'credit', amount: '20.12345', transfer: 'match', other: 'merchant_main' },
  { id: 'b-difference', kind: 'settlement', direction: 'credit', amount: '19.90000', transfer: 'different', other: 'merchant_main' },
  { id: 'b-dup', kind: 'settlement', direction: 'credit', amount: '3.00000', transfer: 'duplicate', other: 'merchant_main' },
  { id: 'b-outside', kind: 'settlement', direction: 'credit', amount: '9.00000', transfer: 'match', other: 'other_merchant' },
  { id: 'b-fee', kind: 'fee', direction: 'debit', amount: '0.00001' }
 ])
 const report: FundReconciliation = { id: reportID, version: 'fund-transfer-comparison-v1', merchant_batch_id: merchantID, bank_batch_id: bankID, merchant_account_key: 'merchant_main', bank_account_key: 'bank_main', matched_count: 1, difference_count: 1, unmatched_count: 1, ambiguous_count: 1, merchant_candidate_count: 5, bank_candidate_count: 3, merchant_out_of_scope_count: 1, bank_out_of_scope_count: 1, merchant_other_count: many ? 101 : 1, bank_other_count: 1, source_warnings: ['merchant_partial', 'bank_partial'], created_by: 'finance', created_at: created, rows: [
  { transfer_id: 'different', merchant_entries: [m.rows[1]!], bank_entries: [b.rows[1]!], status: 'difference', codes: ['amount_mismatch'] },
  { transfer_id: 'duplicate', merchant_entries: [m.rows[3]!, m.rows[4]!], bank_entries: [b.rows[2]!], status: 'ambiguous', codes: ['duplicate_transfer_id'] },
  { transfer_id: 'match', merchant_entries: [m.rows[0]!], bank_entries: [b.rows[0]!], status: 'matched', codes: [] },
  { transfer_id: 'waiting', merchant_entries: [m.rows[2]!], bank_entries: [], status: 'unmatched', codes: ['bank_record_missing'] }
 ] }
 const imported = ledger(importedID, 'bank', 'bank_extra', '2026-10-29', [{ id: 'large-credit', kind: 'other', direction: 'credit', amount: '92233720368547758.07000' }, { id: 'tiny-debit', kind: 'fee', direction: 'debit', amount: '0.00001' }])
 imported.rows[1]!.balance_after_yuan = imported.rows[0]!.balance_after_yuan
 imported.rows[1]!.codes = ['running_balance_mismatch', 'step_balance_mismatch']
 imported.closing_balance_yuan = imported.rows[0]!.balance_after_yuan
 imported.balance_difference_count = 1; imported.summary_codes = ['closing_balance_mismatch']
 return { m, b, imported, report }
}
async function fixture(page: Page, options: { readonly?: boolean; denied?: boolean; many?: boolean } = {}) {
 const data = fixtures(!!options.many)
 const state = { ...data, ledgers: [data.m, data.b], reports: [data.report], imports: [] as Record<string, unknown>[], pairs: [] as Record<string, unknown>[], gets: [] as string[], failImport: false }
 await page.route('**/api/v1/**', async route => {
  const req = route.request(), path = new URL(req.url()).pathname, method = req.method()
  const reply = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) })
  if (path === '/api/v1/config') return reply({ development_payments: false, development_mail: false, email_verification_required: false, payment_actions_enabled: false, bill_downloads_enabled: false })
  if (path === '/api/v1/me') return reply({ id: 'finance', email: 'finance@example.test', role: options.denied ? 'support' : 'finance', permissions: options.denied ? [] : ['funds.read', ...(options.readonly ? [] : ['funds.import', 'funds.reconcile'])], email_verified_at: created })
  if (['/api/v1/plans', '/api/v1/payment-methods', '/api/v1/orders', '/api/v1/me/entitlements'].includes(path)) return reply([])
  if (path.startsWith('/api/v1/admin/fund-')) state.gets.push(`${method} ${path}`)
  if (path === '/api/v1/admin/fund-ledgers' && method === 'GET') return reply({ items: state.ledgers.map(({ rows, ...meta }) => meta) })
  if (path === '/api/v1/admin/fund-ledgers' && method === 'POST') {
   state.imports.push(req.postDataJSON())
   if (state.failImport) return reply({ error: '流水文件暂时未通过校验，请核对后重试' }, 400)
   if (!state.ledgers.some(item => item.id === importedID)) state.ledgers.push(data.imported)
   return reply(data.imported)
  }
  if (path.startsWith('/api/v1/admin/fund-ledgers/')) return reply(state.ledgers.find(item => item.id === path.split('/').at(-1)))
  if (path === '/api/v1/admin/fund-reconciliations' && method === 'GET') return reply({ items: state.reports.map(({ rows, ...meta }) => meta) })
  if (path === '/api/v1/admin/fund-reconciliations' && method === 'POST') { state.pairs.push(req.postDataJSON()); return reply(data.report) }
  if (path === `/api/v1/admin/fund-reconciliations/${reportID}`) return reply(data.report)
  return reply({ error: 'unexpected fixture endpoint' }, 404)
 })
 return state
}
async function navigate(page: Page) {
 await page.goto('/')
 await page.getByRole('navigation').getByRole('button', { name: /资金流水/ }).click()
 await expect(page.getByRole('heading', { name: '资金流水账本', exact: true })).toBeVisible()
 await expect(page.locator(`[data-ledger-id="${merchantID}"]`)).toBeVisible()
}
async function openImport(page: Page) {
 await page.getByRole('button', { name: '导入资金流水', exact: true }).click()
 await page.getByLabel('流水来源', { exact: true }).selectOption('bank')
 await page.getByLabel('账户别名', { exact: true }).fill('bank_extra')
 await page.getByLabel('资金流水日期', { exact: true }).fill('2026-10-29')
 await page.getByLabel('期末声明', { exact: true }).fill('92233720368547758.07000')
 await page.getByLabel('标准资金流水 CSV', { exact: true }).setInputFiles({ name: 'funds.csv', mimeType: 'text/csv', buffer: Buffer.from(importedCSV) })
 await expect(page.getByText('已选择流水文件 funds.csv', { exact: true })).toBeVisible()
}

test('资金流水导入失败保留文件，重复内容复用，五位大金额与余额差异保留', async ({ page }) => {
 const state = await fixture(page); state.failImport = true
 await navigate(page); await openImport(page)
 await page.getByRole('button', { name: '保存流水观察', exact: true }).click()
 await expect(page.getByRole('alert').filter({ hasText: '流水文件暂时未通过校验' })).toBeVisible()
 await expect(page.getByText('已选择流水文件 funds.csv', { exact: true })).toBeVisible()
 state.failImport = false
 await page.getByRole('button', { name: '保存流水观察', exact: true }).click()
 await expect(page.getByRole('status').filter({ hasText: '已保存或读取相同流水批次' })).toBeVisible()
 const detail = page.getByTestId('fund-ledger-detail')
 await expect(detail).toContainText('¥92,233,720,368,547,758.07000')
 await expect(detail).toContainText('¥92,233,720,368,547,758.06999')
 await expect(detail.getByText('累计推算余额与记录不一致', { exact: true })).toBeVisible()
 await expect(detail.getByText('期末声明与期初加净变动不一致', { exact: true })).toBeVisible()
 expect(state.imports[1]).toMatchObject({ provider: 'bank', account_key: 'bank_extra', opening_balance_yuan: '0', closing_balance_yuan: '92233720368547758.07000', coverage: 'partial', csv: importedCSV })
 await openImport(page); await page.getByRole('button', { name: '保存流水观察', exact: true }).click()
 await expect(page.getByRole('button', { name: '保存流水观察', exact: true })).toHaveCount(0)
 expect(state.ledgers).toHaveLength(3)
 await expect(page.locator('[data-ledger-id]')).toHaveCount(3)
 expect(await page.evaluate(() => localStorage.length)).toBe(0)
})

test('明确跨日账户批次核对，不猜测重复引用或把本次未列出认作未到账', async ({ page }) => {
 const state = await fixture(page)
 await navigate(page)
 await expect(page.getByLabel('商户提现批次', { exact: true })).toHaveValue(merchantID)
 await expect(page.getByLabel('银行入账批次', { exact: true })).toHaveValue(bankID)
 await page.getByRole('button', { name: '核对所选资金记录', exact: true }).click()
 const report = page.getByTestId('fund-report')
 await expect(report).toBeVisible()
 expect(state.pairs).toEqual([{ merchant_batch_id: merchantID, bank_batch_id: bankID }])
 await expect(report.getByText('观察相符 1', { exact: true })).toBeVisible()
 await expect(report.getByText('引用重复 1', { exact: true })).toBeVisible()
 await expect(report).toContainText('其他账户方向未核对：商户 1 条、银行 1 条')
 await expect(report.getByText('商户批次声明为部分范围', { exact: true })).toBeVisible()
 await expect(report.getByText('本次银行范围未列出（需核对跨日）', { exact: true })).toBeVisible()
 await expect(report.locator('[data-transfer-id="duplicate"]')).toContainText('m-dup-a')
 await expect(report.locator('[data-transfer-id="duplicate"]')).toContainText('m-dup-b')
 await expect(report.locator('[data-transfer-id="different"]')).toContainText('未自动扣除手续费')
 await page.getByLabel('资金核对筛选', { exact: true }).selectOption('ambiguous')
 await expect(report.locator('[data-transfer-id]')).toHaveCount(1)
 await page.getByRole('button', { name: '收起资金核对报告', exact: true }).click()
 await page.getByRole('button', { name: '查看资金核对报告', exact: true }).click()
 await expect(report.locator('[data-transfer-id]')).toHaveCount(4)
 await page.getByRole('button', { name: '核对所选资金记录', exact: true }).click()
 await expect(page.locator('[data-fund-report-id]')).toHaveCount(1)
})

test('只读查看历史及分页，手机布局无溢出，无权限时不显示也不预取资金数据', async ({ page, context }) => {
 const state = await fixture(page, { readonly: true, many: true })
 await navigate(page)
 await expect(page.getByRole('button', { name: '导入资金流水', exact: true })).toHaveCount(0)
 await expect(page.getByRole('button', { name: '核对所选资金记录', exact: true })).toHaveCount(0)
 await page.locator(`[data-ledger-id="${merchantID}"]`).getByRole('button', { name: '查看流水批次' }).click()
 await expect(page.getByTestId('fund-ledger-detail').locator('[data-entry-id]')).toHaveCount(100)
 await page.getByRole('button', { name: '下一页流水', exact: true }).click()
 await expect(page.getByText('流水第 101–107 项，共 107 项', { exact: true })).toBeVisible()
 await page.getByRole('button', { name: '查看资金核对报告', exact: true }).click()
 await expect(page.getByTestId('fund-report')).toBeVisible()
 await page.setViewportSize({ width: 390, height: 844 })
 expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
 expect(state.imports).toHaveLength(0); expect(state.pairs).toHaveLength(0)
 const denied = await context.newPage(), deniedState = await fixture(denied, { denied: true })
 await denied.goto('/')
 await expect(denied.getByRole('navigation').getByRole('button', { name: /资金流水/ })).toHaveCount(0)
 expect(deniedState.gets).toHaveLength(0)
 await denied.close()
})

test('完整范围须确认，文件超限拒绝，收起后清空文件且晚到批次不会覆盖当前选择', async ({ page }) => {
 const state = await fixture(page)
 await navigate(page); await openImport(page)
 await page.getByLabel('资金流水范围', { exact: true }).selectOption('full')
 await expect(page.getByRole('button', { name: '保存流水观察', exact: true })).toBeDisabled()
 await page.getByRole('checkbox', { name: '确认完整流水范围', exact: true }).check()
 await page.getByLabel('标准资金流水 CSV', { exact: true }).setInputFiles({ name: 'large.csv', mimeType: 'text/csv', buffer: Buffer.alloc(1024 * 1024 + 1, 65) })
 await expect(page.getByRole('alert').filter({ hasText: '流水 CSV 不能超过 1 MiB' })).toBeVisible()
 await expect(page.getByRole('checkbox', { name: '确认完整流水范围', exact: true })).not.toBeChecked()
 await page.getByRole('button', { name: '取消', exact: true }).click()
 await page.getByRole('button', { name: '导入资金流水', exact: true }).click()
 await expect(page.getByText('已选择流水文件 funds.csv', { exact: true })).toHaveCount(0)
 await page.getByRole('button', { name: '取消', exact: true }).click()
 let release!: () => void, started!: () => void
 const held = new Promise<void>(resolve => { release = resolve }), requested = new Promise<void>(resolve => { started = resolve })
 const response = page.waitForResponse(result => result.url().endsWith(`/fund-ledgers/${merchantID}`))
 await page.route(`**/api/v1/admin/fund-ledgers/${merchantID}`, async route => { started(); await held; await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(state.m) }) })
 await page.locator(`[data-ledger-id="${merchantID}"]`).getByRole('button', { name: '查看流水批次' }).click()
 await requested
 await page.getByRole('button', { name: '收起流水批次', exact: true }).click()
 await page.locator(`[data-ledger-id="${bankID}"]`).getByRole('button', { name: '查看流水批次' }).click()
 await expect(page.getByTestId('fund-ledger-detail')).toContainText('bank_main')
 release(); await (await response).finished()
 await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
 await expect(page.getByTestId('fund-ledger-detail').locator('p > strong').first()).toHaveText('bank_main')
 expect(state.imports).toHaveLength(0)
})
