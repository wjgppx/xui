package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wjgppx/xui/internal/clientip"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	TrustedProxy          *clientip.Policy
	ReleaseID             string
	Origin                string
	Development           bool
	SecureCookies         bool
	RequireVerified       bool
	Payments              *payments.Registry
	PaymentActionsEnabled bool
	BillDownloadsEnabled  bool
	MFAKey                []byte
	RequireStaffMFA       bool
	// PasswordCost defaults to bcrypt.DefaultCost. Tests can use bcrypt.MinCost
	// to exercise authentication without race-instrumentation CPU amplification.
	PasswordCost int
}
type Server struct {
	Store  *store.Store
	Config Config
}

func New(s *store.Store, c Config) *Server {
	if len(c.MFAKey) != 0 && len(c.MFAKey) != 32 {
		panic("invalid MFA encryption key")
	}
	c.MFAKey = append([]byte(nil), c.MFAKey...)
	if c.PasswordCost == 0 {
		c.PasswordCost = bcrypt.DefaultCost
	}
	if c.PasswordCost < bcrypt.MinCost || c.PasswordCost > bcrypt.MaxCost {
		panic("invalid bcrypt cost")
	}
	// Keep the handler's authorization policy immutable and consistent across
	// sessions, public exports and agent grants without mutating a shared Store.
	policyStore := *s
	c.RequireVerified = c.RequireVerified || s.RequireVerified
	policyStore.RequireVerified = c.RequireVerified
	return &Server{Store: &policyStore, Config: c}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", s.health)
	m.HandleFunc("GET /readyz", s.readiness)
	m.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]bool{"development_payments": s.Config.Development, "development_mail": s.Config.Development, "email_verification_required": s.Config.RequireVerified, "payment_actions_enabled": s.Config.PaymentActionsEnabled, "bill_downloads_enabled": s.Config.BillDownloadsEnabled})
	})
	m.HandleFunc("POST /api/v1/auth/register", s.register)
	m.HandleFunc("POST /api/v1/auth/login", s.login)
	m.HandleFunc("POST /api/v1/auth/logout", s.logout)
	m.HandleFunc("GET /api/v1/me", s.me)
	m.HandleFunc("GET /api/v1/me/mfa", s.mfaStatus)
	m.HandleFunc("POST /api/v1/me/mfa/setup", s.mfaSetup)
	m.HandleFunc("POST /api/v1/me/mfa/confirm", s.mfaConfirm)
	m.HandleFunc("POST /api/v1/me/mfa/verify", s.mfaVerify)
	m.HandleFunc("POST /api/v1/me/mfa/disable", s.mfaDisable)
	m.HandleFunc("POST /api/v1/me/email-verification", s.requestVerification)
	m.HandleFunc("POST /api/v1/auth/email/confirm", s.confirmEmail)
	m.HandleFunc("POST /api/v1/auth/password/forgot", s.forgotPassword)
	m.HandleFunc("POST /api/v1/auth/password/reset", s.resetPassword)
	m.HandleFunc("POST /api/v1/me/password", s.changePassword)
	m.HandleFunc("GET /api/v1/me/development-mail", s.developmentMail)
	m.HandleFunc("GET /api/v1/admin/users", s.staffUsers)
	m.HandleFunc("POST /api/v1/admin/users/{id}/role", s.setStaffRole)
	m.HandleFunc("GET /api/v1/me/subscription", s.subscriptionStatus)
	m.HandleFunc("POST /api/v1/me/subscription/rotate", s.rotateSubscription)
	m.HandleFunc("POST /api/v1/me/subscription/revoke", s.revokeSubscription)
	m.HandleFunc("POST /api/v1/me/proxy-credentials/rotate", s.rotateProxyCredentials)
	m.HandleFunc("GET /api/v1/subscriptions/{token}", s.publicSubscription)
	m.HandleFunc("GET /api/v1/plans", s.plans)
	m.HandleFunc("GET /api/v1/payment-methods", s.paymentMethods)
	m.HandleFunc("POST /api/v1/orders/{id}/payment", s.createPayment)
	m.HandleFunc("GET /api/v1/orders/{id}/payment", s.orderPayment)
	m.HandleFunc("POST /api/v1/orders/{id}/payment/sync", s.syncPayment)
	m.HandleFunc("GET /api/v1/payments/notify/{provider}", s.paymentNotify)
	m.HandleFunc("POST /api/v1/payments/notify/{provider}", s.paymentNotify)
	m.HandleFunc("GET /api/v1/orders", s.orders)
	m.HandleFunc("POST /api/v1/orders", s.createOrder)
	m.HandleFunc("POST /api/v1/orders/{id}/close", s.closeOrder)
	m.HandleFunc("GET /api/v1/admin/payments", s.adminPayments)
	m.HandleFunc("GET /api/v1/admin/payment-activity", s.adminRecentPaymentActivity)
	m.HandleFunc("GET /api/v1/admin/payments/{id}/activity", s.adminPaymentActivity)
	m.HandleFunc("POST /api/v1/admin/payments/{id}/retry", s.retryPaymentReconciliation)
	m.HandleFunc("POST /api/v1/admin/payments/{id}/refund", s.requestRefund)
	m.HandleFunc("POST /api/v1/admin/payments/{id}/close", s.requestGatewayClose)
	m.HandleFunc("GET /api/v1/admin/refunds", s.listRefunds)
	m.HandleFunc("POST /api/v1/admin/refunds/{id}/approve", s.approveRefund)
	m.HandleFunc("GET /api/v1/admin/statements", s.adminStatements)
	m.HandleFunc("POST /api/v1/admin/statements", s.importStatement)
	m.HandleFunc("POST /api/v1/admin/statements/native", s.importNativeStatement)
	m.HandleFunc("GET /api/v1/admin/bill-downloads", s.adminBillDownloads)
	m.HandleFunc("POST /api/v1/admin/bill-downloads", s.requestBillDownload)
	m.HandleFunc("GET /api/v1/admin/bill-schedules", s.adminBillSchedules)
	m.HandleFunc("PUT /api/v1/admin/bill-schedules", s.updateBillSchedule)
	m.HandleFunc("GET /api/v1/admin/statements/{id}", s.adminStatement)
	m.HandleFunc("POST /api/v1/admin/statements/{id}/reconcile", s.reconcileStatement)
	m.HandleFunc("GET /api/v1/admin/statements/{id}/sources/{sourceID}/financials", s.sourceFinancials)
	m.HandleFunc("POST /api/v1/admin/statements/{id}/sources/{sourceID}/financials/reconcile", s.reconcileSourceFinancials)
	m.HandleFunc("GET /api/v1/admin/statements/{id}/sources/{sourceID}/financials/runs/{runID}", s.sourceFinancialReport)
	m.HandleFunc("GET /api/v1/admin/statements/{id}/runs/{runID}/findings", s.statementFindings)
	m.HandleFunc("GET /api/v1/admin/statement-findings/{id}/reviews", s.statementFindingReviews)
	m.HandleFunc("POST /api/v1/admin/statement-findings/{id}/reviews", s.reviewStatementFinding)
	m.HandleFunc("GET /api/v1/admin/fund-ledgers", s.adminFundLedgers)
	m.HandleFunc("POST /api/v1/admin/fund-ledgers", s.importFundLedger)
	m.HandleFunc("GET /api/v1/admin/fund-ledgers/{id}", s.adminFundLedger)
	m.HandleFunc("GET /api/v1/admin/fund-reconciliations", s.adminFundReconciliations)
	m.HandleFunc("POST /api/v1/admin/fund-reconciliations", s.reconcileFundLedgers)
	m.HandleFunc("GET /api/v1/admin/fund-reconciliations/{id}", s.adminFundReconciliation)
	m.HandleFunc("GET /api/v1/me/entitlements", s.entitlements)
	m.HandleFunc("POST /api/v1/dev/orders/{id}/settle", s.devSettle)
	m.HandleFunc("GET /api/v1/admin/overview", s.overview)
	m.HandleFunc("GET /api/v1/admin/access", s.accessOverview)
	m.HandleFunc("POST /api/v1/admin/node-groups", s.createNodeGroup)
	m.HandleFunc("POST /api/v1/admin/node-groups/{id}", s.updateNodeGroup)
	m.HandleFunc("POST /api/v1/admin/plans/{id}/node-groups", s.setPlanNodeGroups)
	m.HandleFunc("GET /api/v1/admin/events", s.listEvents)
	m.HandleFunc("POST /api/v1/admin/events/{id}/retry", s.retryEvent)
	m.HandleFunc("POST /api/v1/admin/entitlements/{id}/revoke", s.revokeEntitlement)
	m.HandleFunc("GET /api/v1/admin/orders", s.adminOrders)
	m.HandleFunc("POST /api/v1/admin/plans", s.createPlan)
	m.HandleFunc("GET /api/v1/admin/nodes", s.nodes)
	m.HandleFunc("POST /api/v1/admin/nodes", s.createNode)
	m.HandleFunc("POST /api/v1/admin/nodes/{id}/enrollment", s.rotateNode)
	m.HandleFunc("POST /api/v1/admin/nodes/{id}/revoke", s.revokeNode)
	m.HandleFunc("GET /api/v1/admin/nodes/{id}/configuration", s.configuration)
	m.HandleFunc("POST /api/v1/admin/nodes/{id}/configuration", s.configure)
	m.HandleFunc("POST /agent/v1/applied-state", s.applied)
	m.HandleFunc("POST /agent/v1/usage-batches", s.usage)
	m.HandleFunc("POST /agent/v1/enroll", s.enroll)
	m.HandleFunc("GET /agent/v1/desired-state", s.desiredState)
	m.HandleFunc("POST /agent/v1/heartbeat", s.heartbeat)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		var addressOK bool
		r, addressOK = s.addressRequest(w, r)
		if !addressOK {
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			machineWrite := r.URL.Path == "/agent/v1/enroll" || r.URL.Path == "/agent/v1/heartbeat" || r.URL.Path == "/agent/v1/applied-state" || r.URL.Path == "/agent/v1/usage-batches"
			callback := paymentNotificationPath(r.URL.Path)
			if !machineWrite && !callback && r.Header.Get("Origin") != s.Config.Origin {
				fail(w, 403, "请求来源不受信任")
				return
			}
			if !callback && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				fail(w, 415, "请使用 JSON 请求")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		m.ServeHTTP(w, r.WithContext(ctx))
	})
}
func paymentNotificationPath(path string) bool {
	switch path {
	case "/api/v1/payments/notify/alipay", "/api/v1/payments/notify/wechat", "/api/v1/payments/notify/epay_alipay", "/api/v1/payments/notify/epay_wechat":
		return true
	default:
		return false
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "请求内容格式不正确")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func serverError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	fail(w, 500, "服务暂时不可用，请稍后重试")
}
func hash(s string) string                     { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func (s *Server) allowed(r *http.Request) bool { return s.allowedKey(r, "", 20) }
func (s *Server) allowedKey(r *http.Request, prefix string, limit int) bool {
	ip := clientAddress(r)
	allowed, err := s.Store.AllowRate(r.Context(), prefix+ip, limit, time.Minute)
	if err != nil {
		slog.Warn("request limiter unavailable")
		return false
	}
	return allowed
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DB.Ping(r.Context()); err != nil {
		fail(w, 503, "数据库不可用")
		return
	}
	reply(w, 200, map[string]string{"status": "ok"})
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func validCredentials(c *credentials) bool {
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	addr, err := mail.ParseAddress(c.Email)
	return err == nil && addr.Address == c.Email && len(c.Email) <= 254 && len(c.Password) >= 12 && len(c.Password) <= 72
}
func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u store.User) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		serverError(w, err)
		return
	}
	token := hex.EncodeToString(b)
	// A user lock serializes session creation with password/role changes. A
	// password checked before a reset cannot create a valid session after it.
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var version int64
	if err = tx.QueryRow(r.Context(), `SELECT security_version FROM users WHERE id=$1 FOR SHARE`, u.ID).Scan(&version); err != nil {
		serverError(w, err)
		return
	}
	if version != u.SecurityVersion {
		fail(w, 401, "账号状态已变化，请重新登录")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO sessions(token_hash,user_id,expires_at,security_version) VALUES($1,$2,$3,$4)`, hash(token), u.ID, time.Now().Add(24*time.Hour), version)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	u.Permissions = permissions(u.Role)
	if _, err = s.decorateUserMFA(r.Context(), &u, hash(token)); err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法读取账号安全状态")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "xui_session", Value: token, Path: "/", HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	reply(w, 200, u)
}
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.allowed(r) {
		fail(w, 429, "尝试过于频繁，请稍后再试")
		return
	}
	var c credentials
	if !decode(w, r, &c) {
		return
	}
	if !validCredentials(&c) {
		fail(w, 400, "请输入有效邮箱，密码需为 12–72 字节")
		return
	}
	p, err := bcrypt.GenerateFromPassword([]byte(c.Password), s.Config.PasswordCost)
	if err != nil {
		serverError(w, err)
		return
	}
	u := store.User{ID: store.ID(), Email: c.Email, Role: "user"}
	_, err = s.Store.DB.Exec(r.Context(), `INSERT INTO users(id,email,password_hash) VALUES($1,$2,$3)`, u.ID, u.Email, string(p))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		fail(w, 409, "该邮箱已注册")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	s.newSession(w, r, u)
}

// A valid dummy hash keeps unknown-user login work comparable to a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-user-password"), bcrypt.DefaultCost)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.allowed(r) {
		fail(w, 429, "尝试过于频繁，请稍后再试")
		return
	}
	var c credentials
	if !decode(w, r, &c) {
		return
	}
	if !validCredentials(&c) {
		fail(w, 401, "邮箱或密码不正确")
		return
	}
	var u store.User
	var p string
	err := s.Store.DB.QueryRow(r.Context(), `SELECT id,email,role,password_hash,email_verified_at,security_version FROM users WHERE email=$1`, c.Email).Scan(&u.ID, &u.Email, &u.Role, &p, &u.EmailVerifiedAt, &u.SecurityVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, err)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(c.Password))
		fail(w, 401, "邮箱或密码不正确")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(p), []byte(c.Password)) != nil {
		fail(w, 401, "邮箱或密码不正确")
		return
	}
	s.newSession(w, r, u)
}
func (s *Server) current(w http.ResponseWriter, r *http.Request, admin bool) (store.User, bool) {
	var u store.User
	c, err := r.Cookie("xui_session")
	if err != nil || len(c.Value) != 64 {
		fail(w, 401, "请先登录")
		return u, false
	}
	err = s.Store.DB.QueryRow(r.Context(), `SELECT u.id,u.email,u.role,u.email_verified_at,u.security_version FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now() AND s.security_version=u.security_version`, hash(c.Value)).Scan(&u.ID, &u.Email, &u.Role, &u.EmailVerifiedAt, &u.SecurityVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 401, "登录已失效，请重新登录")
		return u, false
	}
	if err != nil {
		serverError(w, err)
		return u, false
	}
	if admin && !permitted(u.Role, requiredPermission(r)) {
		fail(w, 403, "当前角色没有此操作权限")
		return u, false
	}
	if s.Config.RequireVerified && u.EmailVerifiedAt == nil && verificationProtected(r) {
		fail(w, 403, "请先完成邮箱验证")
		return u, false
	}
	status, err := s.decorateUserMFA(r.Context(), &u, hash(c.Value))
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法读取账号安全状态")
		return u, false
	}
	if admin && u.MFARequired {
		if status.Enabled && len(s.Config.MFAKey) != 32 {
			fail(w, http.StatusServiceUnavailable, "二次验证暂时不可用，请联系主机管理员")
			return u, false
		}
		if !status.Enabled {
			reply(w, http.StatusForbidden, map[string]string{"error": "请先在账号安全中启用二次验证", "code": "mfa_enrollment_required"})
			return u, false
		}
		if status.VerifiedUntil == nil {
			reply(w, http.StatusForbidden, map[string]string{"error": "请先完成二次验证后再访问后台", "code": "mfa_required"})
			return u, false
		}
	}
	u.Permissions = permissions(u.Role)
	return u, true
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("xui_session"); err == nil {
		if _, err = s.Store.DB.Exec(r.Context(), `DELETE FROM sessions WHERE token_hash=$1`, hash(c.Value)); err != nil {
			serverError(w, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "xui_session", Value: "", Path: "/", HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if ok {
		reply(w, 200, u)
	}
}
func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.Plans(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, v)
}
func (s *Server) orders(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	v, err := s.Store.Orders(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, v)
}
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	var body struct {
		PlanID string `json:"plan_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	v, err := s.Store.CreateOrder(r.Context(), u.ID, body.PlanID)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, 404, "套餐不存在或已下架")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 201, v)
}
func (s *Server) entitlements(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	v, err := s.Store.Entitlements(r.Context(), u.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, v)
}
func (s *Server) devSettle(w http.ResponseWriter, r *http.Request) {
	if !s.Config.Development {
		fail(w, 404, "接口不存在")
		return
	}
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	var price int64
	var currency, status string
	var expires time.Time
	id := r.PathValue("id")
	err := s.Store.DB.QueryRow(r.Context(), `SELECT price_minor,currency,status,expires_at FROM orders WHERE id=$1 AND user_id=$2`, id, u.ID).Scan(&price, &currency, &status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "订单不存在")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if (status != "pending" && status != "paid") || (status == "pending" && !expires.After(time.Now())) {
		fail(w, 409, "订单已超时或已关闭")
		return
	}
	err = s.Store.Settle(r.Context(), "development", "dev:"+id, id, u.ID, price, currency)
	if errors.Is(err, store.ErrPaymentConflict) {
		fail(w, 409, "订单支付状态不允许此操作")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	var users, orders, paid, pending int64
	err := s.Store.DB.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM orders),(SELECT COALESCE(sum(price_minor),0) FROM orders WHERE status='paid'),(SELECT count(*) FROM outbox_events WHERE processed_at IS NULL)`).Scan(&users, &orders, &paid, &pending)
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, map[string]int64{"users": users, "orders": orders, "paid_minor": paid, "pending_events": pending})
}
func (s *Server) adminOrders(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	v, err := s.Store.Orders(r.Context(), "")
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 200, v)
}
func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	var p store.Plan
	if !decode(w, r, &p) {
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 100 || len(p.Description) > 1000 || p.Price <= 0 || p.Price > 100000000 || p.Currency != "CNY" || p.Days < 1 || p.Days > 366 || p.Quota <= 0 || p.Quota > 10995116277760 {
		fail(w, 400, "套餐字段不符合要求")
		return
	}
	p.ID = store.ID()
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	_, err = tx.Exec(r.Context(), `INSERT INTO plans(id,name,description,price_minor,currency,duration_days,quota_bytes) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.ID, p.Name, p.Description, p.Price, p.Currency, p.Days, p.Quota)
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'plan.created',$2)`, u.ID, p.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, err)
		return
	}
	reply(w, 201, p)
}
