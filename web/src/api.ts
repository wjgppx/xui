export interface User { id: string; email: string; role: 'user' | 'admin' | 'support' | 'finance' | 'operator'; email_verified_at: string | null; permissions: string[] }
export interface Plan { id: string; name: string; description: string; price_minor: number; currency: string; duration_days: number; quota_bytes: number }
export interface Order extends Omit<Plan, 'name' | 'description'> { user_id: string; plan_id: string; plan_name: string; status: 'pending' | 'paid'; created_at: string; paid_at: string | null }
export interface Entitlement { revoked_at?: string | null; id: string; order_id: string; plan_name: string; starts_at: string; ends_at: string; quota_bytes: number; used_bytes: number }
export interface Overview { users: number; orders: number; paid_minor: number; pending_events: number }
export interface PaymentMethod { id: 'alipay' | 'wechat' | 'epay_alipay' | 'epay_wechat'; name: string }
export interface Payment { id: string; order_id: string; provider: PaymentMethod['id']; status: 'pending' | 'ready' | 'paid'; checkout_kind: 'redirect' | 'qr' | ''; checkout_url: string; transaction_id?: string | null; created_at: string; updated_at: string; paid_at: string | null }
export async function api<T>(path: string, body?: unknown): Promise<T> {
 const response = await fetch('/api/v1' + path, { credentials: 'same-origin', method: body === undefined ? 'GET' : 'POST', headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) })
 const data = await response.json()
 if (!response.ok) throw new Error(data.error || '请求失败，请稍后再试')
 return data as T
}
