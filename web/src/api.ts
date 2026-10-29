export interface User { id: string; email: string; role: 'user' | 'admin' | 'support' | 'finance' | 'operator'; email_verified_at: string | null; permissions: string[]; mfa_enabled?: boolean; mfa_required?: boolean; mfa_verified_until?: string | null }
export interface StaffMFAStatus { available: boolean; enabled: boolean; required: boolean; verified_until: string | null; setup_pending: boolean; recovery_codes_remaining: number }
export class APIError extends Error {
 constructor(message: string, public status: number, public code?: string) { super(message); this.name = 'APIError' }
}
export interface Plan { id: string; name: string; description: string; price_minor: number; currency: string; duration_days: number; quota_bytes: number }
export interface Order extends Omit<Plan, 'name' | 'description'> { user_id: string; plan_id: string; plan_name: string; status: 'pending' | 'expired' | 'closing' | 'closed' | 'paid' | 'refunded'; created_at: string; paid_at: string | null; expires_at?: string | null; closed_at?: string | null }
export interface Entitlement { revoked_at?: string | null; id: string; order_id: string; plan_name: string; starts_at: string; ends_at: string; quota_bytes: number; used_bytes: number }
export interface Overview { users: number; orders: number; paid_minor: number; pending_events: number }
export interface PaymentMethod { id: 'alipay' | 'wechat' | 'epay_alipay' | 'epay_wechat'; name: string }
export interface Payment { id: string; order_id: string; provider: PaymentMethod['id']; status: 'pending' | 'ready' | 'paid'; checkout_kind: 'redirect' | 'qr' | ''; checkout_url: string; transaction_id?: string | null; created_at: string; updated_at: string; paid_at: string | null }
export async function api<T>(path: string, body?: unknown, method: 'POST' | 'PUT' = 'POST'): Promise<T> {
 const response = await fetch('/api/v1' + path, { credentials: 'same-origin', method: body === undefined ? 'GET' : method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) })
 const data = await response.json()
 if (!response.ok) {
  const code = typeof data.code === 'string' ? data.code : undefined
  if (response.status === 403 && (code === 'mfa_required' || code === 'mfa_enrollment_required')) window.dispatchEvent(new CustomEvent('xui:mfa-required', { detail: code }))
  throw new APIError(data.error || '请求失败，请稍后再试', response.status, code)
 }
 return data as T
}

export interface PaymentOperation { id: string; order_id: string; user_email: string; provider: PaymentMethod['id']; status: Payment['status']; amount_minor: number; currency: string; transaction_id?: string | null; created_at: string; paid_at?: string | null; order_status: Order['status']; expires_at?: string | null; reconcile_status?: 'queued' | 'processing' | 'waiting' | 'paid' | 'manual_required' | 'waiting_callback' | 'closed' | null; reconcile_attempts?: number; last_outcome?: string; next_check_at?: string | null }
export interface PaymentActivity { id: string | number; payment_id?: string | null; order_id?: string | null; provider?: string; source: string; outcome: string; created_at: string }

export interface RefundOperation { id: string; payment_id: string; order_id: string; provider: PaymentMethod['id']; transaction_id: string; amount_minor: number; currency: string; reason: string; status: 'requested' | 'queued' | 'processing' | 'succeeded' | 'failed' | 'manual_required'; requested_by: string; approved_by: string | null; created_at: string; updated_at: string; completed_at: string | null; provider_refund_id: string | null; user_email: string }

export interface MerchantStatement { id: string; provider: PaymentMethod['id']; kind: 'payment' | 'refund'; date: string; coverage: 'partial' | 'full'; digest: string; imported_by: string; row_count: number; total_minor: string; created_at: string }
export interface StatementRun { id: string; statement_id: string; revision: number; matched_count: number; difference_count: number; local_candidate_count: number; as_of: string; created_by: string }
export interface StatementSummary extends MerchantStatement { latest_run: StatementRun }
export interface StatementDetail { statement: MerchantStatement; latest_run: StatementRun; runs: StatementRun[]; sources?: StatementSource[] }
export interface StatementSource { id: string; statement_id: string; format: string; kind: 'upload' | 'download'; digest: string; integrity: 'unverified_upload' | 'wechat_signed_sha1' | 'alipay_signed_url'; merchant_fingerprint: string; download_job_id: string | null; imported_by: string; created_at: string }
export interface BillDownloadJob { id: string; provider: 'alipay' | 'wechat'; kind: 'payment' | 'refund'; date: string; status: 'queued' | 'processing' | 'waiting' | 'succeeded' | 'manual_required'; attempts: number; last_outcome: string; statement_id: string | null; schedule_id?: string | null; requested_by: string; created_at: string; updated_at: string; next_attempt_at: string | null; completed_at: string | null }
export interface BillSchedule { id: string; provider: 'alipay' | 'wechat'; kind: 'payment' | 'refund'; enabled: boolean; run_at: string; timezone: 'Asia/Shanghai'; revision: number; state: 'disabled' | 'active' | 'actor_changed' | 'merchant_changed' | 'provider_unavailable'; next_bill_date: string | null; last_enqueued_date: string | null; last_job_id: string | null; authorized_by: string | null; updated_at: string }
export interface StatementReview { id: string; finding_id: string; actor_id: string; note: string; created_at: string }
export interface StatementFinding { id: string; run_id: string; row_number: number | null; payment_id: string; refund_id: string; codes: string[]; statement_transaction_id: string; local_transaction_id: string; statement_provider_refund_id: string; local_provider_refund_id: string; statement_amount_minor: string | null; local_amount_minor: string | null; local_provider: string; local_status: string; local_confirmed_at: string | null; statement_occurred_at: string | null; review_count: number; latest_review: StatementReview | null }
export interface StatementFindingsPage { items: StatementFinding[]; total: number; offset: number; limit: number }

export interface FinancialRow { payment_id: string; refund_id: string; fee_yuan: string | null; channel_amount_yuan: string | null }
export interface FinancialRun { id: string; source_id: string; revision: number; reference_digest: string; matched_count: number; difference_count: number; unassessed_count: number; observed_row_count: number; reference_row_count: number; observed_fee_total_yuan: string | null; observed_channel_amount_total_yuan: string | null; expected_fee_total_yuan: string; expected_channel_amount_total_yuan: string; summary_codes: string[]; created_by: string; created_at: string }
export interface FinancialSnapshot { available: boolean; source_id: string; version: string; basis: string; rows: FinancialRow[]; fee_total_yuan: string | null; channel_amount_total_yuan: string | null; declared_fee_total_yuan: string | null; declared_channel_amount_total_yuan: string | null; summary_codes: string[]; runs: FinancialRun[] }
export interface FinancialComparison { payment_id: string; expected_payment_id: string; refund_id: string; observed_fee_yuan: string | null; expected_fee_yuan: string | null; observed_channel_amount_yuan: string | null; expected_channel_amount_yuan: string | null; status: 'matched' | 'difference' | 'unassessed'; codes: string[] }
export interface FinancialReport { run: FinancialRun; rows: FinancialComparison[] }

export interface FundLedgerEntry { sequence: number; entry_id: string; occurred_at: string; kind: string; direction: 'credit' | 'debit'; amount_yuan: string; balance_after_yuan: string; expected_balance_yuan: string; step_expected_balance_yuan: string; transfer_id: string; counterparty_account_key: string; codes: string[] }
export interface FundLedgerMetadata { version: string; provider: 'alipay' | 'wechat' | 'bank'; account_key: string; date: string; coverage: 'partial' | 'full'; currency: 'CNY'; digest: string; opening_balance_yuan: string; closing_balance_yuan: string; expected_closing_balance_yuan: string; total_credit_yuan: string; total_debit_yuan: string; row_count: number; balance_difference_count: number; summary_codes: string[] }
export interface FundLedgerSummary extends FundLedgerMetadata { id: string; imported_by: string; created_at: string }
export interface FundLedger extends FundLedgerSummary { rows: FundLedgerEntry[] }
export interface FundTransferComparison { transfer_id: string; merchant_entries: FundLedgerEntry[]; bank_entries: FundLedgerEntry[]; status: 'matched' | 'difference' | 'unmatched' | 'ambiguous'; codes: string[] }
export interface FundReconciliationSummary { id: string; version: string; merchant_batch_id: string; bank_batch_id: string; merchant_account_key: string; bank_account_key: string; matched_count: number; difference_count: number; unmatched_count: number; ambiguous_count: number; merchant_candidate_count: number; bank_candidate_count: number; merchant_out_of_scope_count: number; bank_out_of_scope_count: number; merchant_other_count: number; bank_other_count: number; source_warnings: string[]; created_by: string; created_at: string }
export interface FundReconciliation extends FundReconciliationSummary { rows: FundTransferComparison[] }
