package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func (s *Server) paymentMethods(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.Config.Payments.Methods())
}

func (s *Server) paymentUser(w http.ResponseWriter, r *http.Request, purpose string) (store.User, bool) {
	u, ok := s.current(w, r, false)
	if !ok {
		return u, false
	}
	if s.Config.RequireVerified && u.EmailVerifiedAt == nil {
		fail(w, 403, "请先完成邮箱验证")
		return u, false
	}
	if !s.allowedKey(r, "payment:"+purpose+":", 10) {
		fail(w, 429, "支付操作过于频繁，请稍后重试")
		return u, false
	}
	allowed, err := s.Store.AllowRate(r.Context(), "payment-user:"+purpose+":"+u.ID, 5, time.Minute)
	if err != nil {
		fail(w, 503, "支付服务暂时不可用")
		return u, false
	}
	if !allowed {
		fail(w, 429, "支付操作过于频繁，请稍后重试")
		return u, false
	}
	return u, true
}

func (s *Server) paymentRequest(p store.Payment, o store.Order) payments.Request {
	return payments.Request{
		PaymentID: p.ID, OrderID: o.ID, Name: o.PlanName, Amount: o.Price, Currency: o.Currency,
		NotifyURL: s.Config.Origin + "/api/v1/payments/notify/" + p.Provider,
		ReturnURL: s.Config.Origin + "/?payment_return=1",
	}
}

func (s *Server) paymentStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "订单或支付记录不存在")
	case errors.Is(err, store.ErrEmailUnverified):
		fail(w, 403, "请先完成邮箱验证")
	case errors.Is(err, store.ErrPaymentProviderConflict):
		fail(w, 409, "此订单已选择其他支付渠道，请使用原渠道或创建新订单")
	case errors.Is(err, store.ErrPaymentConflict):
		fail(w, 409, "订单支付状态不允许此操作")
	default:
		fail(w, 500, "支付记录暂时不可用")
	}
}

func (s *Server) createPayment(w http.ResponseWriter, r *http.Request) {
	u, ok := s.paymentUser(w, r, "create")
	if !ok {
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if !decode(w, r, &body) {
		return
	}
	provider, ok := s.Config.Payments.Get(body.Provider)
	if !ok {
		fail(w, 404, "该支付渠道未启用")
		return
	}
	o, err := s.Store.PaymentOrder(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	p, err := s.Store.CreatePayment(r.Context(), u.ID, o.ID, body.Provider)
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	if p.CheckoutURL != "" || p.Status == "paid" {
		reply(w, 200, p)
		return
	}
	if !s.paymentActivity(w, r, p.ID, p.Provider, "create", "created") {
		return
	}
	checkout, err := provider.Create(r.Context(), s.paymentRequest(p, o))
	if err != nil {
		outcome := "gateway_error"
		if errors.Is(err, payments.ErrVerification) {
			outcome = "verification_error"
		}
		if !s.paymentActivity(w, r, p.ID, p.Provider, "create", outcome) {
			return
		}
		fail(w, 502, "支付渠道暂时不可用，请使用同一订单重试")
		return
	}
	paymentID := p.ID
	p, err = s.Store.SavePaymentCheckout(r.Context(), p.ID, checkout.Kind, checkout.URL)
	if err != nil {
		if !s.paymentActivity(w, r, paymentID, body.Provider, "create", paymentStoreOutcome(err)) {
			return
		}
		s.paymentStoreError(w, err)
		return
	}
	if !s.paymentActivity(w, r, p.ID, p.Provider, "create", p.Status) {
		return
	}
	reply(w, 200, p)
}

func (s *Server) orderPayment(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	p, err := s.Store.Payment(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	reply(w, 200, p)
}

func (s *Server) syncPayment(w http.ResponseWriter, r *http.Request) {
	u, ok := s.paymentUser(w, r, "sync")
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	p, err := s.Store.Payment(r.Context(), u.ID, r.PathValue("id"))
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	if p.Status == "paid" {
		reply(w, 200, p)
		return
	}
	provider, ok := s.Config.Payments.Get(p.Provider)
	if !ok {
		if !s.paymentActivity(w, r, p.ID, p.Provider, "query", "provider_unavailable") {
			return
		}
		fail(w, 404, "该支付渠道未启用")
		return
	}
	o, err := s.Store.PaymentOrder(r.Context(), u.ID, p.OrderID)
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	n, err := provider.Query(r.Context(), s.paymentRequest(p, o))
	if errors.Is(err, payments.ErrNotPaid) {
		if !s.paymentActivity(w, r, p.ID, p.Provider, "query", "not_paid") {
			return
		}
		// A callback may have committed while this request was querying the gateway.
		p, err = s.Store.Payment(r.Context(), u.ID, p.OrderID)
		if err != nil {
			s.paymentStoreError(w, err)
			return
		}
		reply(w, 200, p)
		return
	}
	if err != nil {
		outcome := "gateway_error"
		if errors.Is(err, payments.ErrVerification) {
			outcome = "verification_error"
		}
		if !s.paymentActivity(w, r, p.ID, p.Provider, "query", outcome) {
			return
		}
		fail(w, 502, "暂时无法验证渠道支付结果，请稍后刷新订单")
		return
	}
	// Queries are authoritative only after adapter verification and exact binding
	// to the stored merchant request identity and purchase snapshot.
	if n.PaymentID != p.ID || n.Amount != o.Price || n.Currency != o.Currency {
		if !s.paymentActivity(w, r, p.ID, p.Provider, "query", "conflict") {
			return
		}
		fail(w, 502, "渠道支付结果与订单不一致")
		return
	}
	if n.Paid {
		if err = s.Store.SettlePayment(r.Context(), p.ID, p.Provider, n.TransactionID, n.Amount, n.Currency); err != nil {
			if !s.paymentActivity(w, r, p.ID, p.Provider, "query", paymentStoreOutcome(err)) {
				return
			}
			s.paymentStoreError(w, err)
			return
		}
	}
	outcome := "pending"
	if n.Paid {
		outcome = "paid"
	}
	if !s.paymentActivity(w, r, p.ID, p.Provider, "query", outcome) {
		return
	}
	p, err = s.Store.Payment(r.Context(), u.ID, p.OrderID)
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	reply(w, 200, p)
}

func paymentAcknowledgement(w http.ResponseWriter, provider string, status int) {
	if provider == "wechat" {
		if status == 200 {
			reply(w, 200, map[string]string{"code": "SUCCESS", "message": "成功"})
		} else {
			reply(w, status, map[string]string{"code": "FAIL", "message": "通知验证或处理失败"})
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if status == 200 {
		_, _ = io.WriteString(w, "success")
	} else {
		_, _ = io.WriteString(w, "fail")
	}
}

func (s *Server) paymentNotify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider")
	provider, ok := s.Config.Payments.Get(id)
	if !ok {
		if paymentProviderID(id) {
			if !s.allowedKey(r, "invalid-notify:"+id+":", 10) {
				paymentAcknowledgement(w, id, 400)
				return
			}
			if err := s.Store.RecordPaymentActivity(r.Context(), "", id, "notify", "provider_unavailable"); err != nil {
				paymentAcknowledgement(w, id, 500)
				return
			}
		}
		paymentAcknowledgement(w, id, 404)
		return
	}
	record := func(paymentID, outcome string) bool {
		if err := s.Store.RecordPaymentActivity(r.Context(), paymentID, id, "notify", outcome); err != nil {
			paymentAcknowledgement(w, id, 500)
			return false
		}
		return true
	}
	invalid := func() bool {
		// Invalid notifications are rejected regardless of the logging budget. Keep
		// a bounded diagnostic sample rather than giving unauthenticated senders an
		// unbounded append-only storage sink. Valid provider proofs bypass this cap.
		if !s.allowedKey(r, "invalid-notify:"+id+":", 10) {
			paymentAcknowledgement(w, id, 400)
			return false
		}
		return record("", "verification_error")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	if len(r.URL.RawQuery) > 65536 {
		if !invalid() {
			return
		}
		paymentAcknowledgement(w, id, 400)
		return
	}
	n, err := provider.Verify(r)
	if err != nil {
		// Unverified request content never chooses the payment record to annotate.
		if !invalid() {
			return
		}
		paymentAcknowledgement(w, id, 400)
		return
	}
	if !n.Paid {
		if !record(n.PaymentID, "pending") {
			return
		}
		paymentAcknowledgement(w, id, 200)
		return
	}
	err = s.Store.SettlePayment(r.Context(), n.PaymentID, id, n.TransactionID, n.Amount, n.Currency)
	if err != nil {
		if !record(n.PaymentID, paymentStoreOutcome(err)) {
			return
		}
		status := 500
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrPaymentConflict) || errors.Is(err, store.ErrPaymentProviderConflict) {
			status = 400
		}
		paymentAcknowledgement(w, id, status)
		return
	}
	if !record(n.PaymentID, "paid") {
		return
	}
	paymentAcknowledgement(w, id, 200)
}
