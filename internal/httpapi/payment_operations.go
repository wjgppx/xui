package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"

	"github.com/wjgppx/xui/internal/store"
)

var paymentOperationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Server) closeOrder(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, false)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	if !s.allowedKey(r, "payment:close:", 10) {
		fail(w, 429, "关闭订单操作过于频繁，请稍后重试")
		return
	}
	o, err := s.Store.CloseOrder(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrPaymentConflict) {
		fail(w, 409, "订单已付款或已发起渠道支付，不能直接关闭；请先核实付款结果")
		return
	}
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	reply(w, 200, o)
}

func paymentProviderID(id string) bool {
	return id == "alipay" || id == "wechat" || id == "epay_alipay" || id == "epay_wechat"
}

func (s *Server) adminPayments(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, 400, "支付流水筛选条件不正确")
		return
	}
	for key, values := range query {
		if (key != "status" && key != "provider" && key != "order_id") || len(values) != 1 {
			fail(w, 400, "支付流水筛选条件不正确")
			return
		}
	}
	status, provider, orderID := query.Get("status"), query.Get("provider"), query.Get("order_id")
	if (status != "" && status != "pending" && status != "ready" && status != "paid" && status != "manual_required") || (provider != "" && !paymentProviderID(provider)) || (orderID != "" && !paymentOperationID.MatchString(orderID)) {
		fail(w, 400, "支付流水筛选条件不正确")
		return
	}
	items, err := s.Store.ListPayments(r.Context(), status, provider, orderID)
	if err != nil {
		fail(w, 500, "支付流水暂时不可用")
		return
	}
	reply(w, 200, map[string]any{"items": items})
}

func (s *Server) adminPaymentActivity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	items, err := s.Store.PaymentActivity(r.Context(), r.PathValue("id"))
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	reply(w, 200, items)
}

func (s *Server) adminRecentPaymentActivity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	items, err := s.Store.RecentPaymentActivity(r.Context())
	if err != nil {
		fail(w, 500, "支付处理记录暂时不可用")
		return
	}
	reply(w, 200, items)
}

func (s *Server) retryPaymentReconciliation(w http.ResponseWriter, r *http.Request) {
	u, ok := s.current(w, r, true)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	if !s.allowedKey(r, "payment:retry:", 10) {
		fail(w, 429, "支付核实重试过于频繁，请稍后重试")
		return
	}
	err := s.Store.RetryPaymentReconciliation(r.Context(), u.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrRoleForbidden) {
		fail(w, 403, "后台权限已变化，请重新登录")
		return
	}
	if errors.Is(err, store.ErrPaymentConflict) {
		fail(w, 409, "此支付记录不能重新排队；易支付需等待签名通知，已支付或正在核实的记录无需重试")
		return
	}
	if err != nil {
		s.paymentStoreError(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}

func (s *Server) paymentActivity(w http.ResponseWriter, r *http.Request, paymentID, provider, source, outcome string) bool {
	if err := s.Store.RecordPaymentActivity(r.Context(), paymentID, provider, source, outcome); err != nil {
		fail(w, 500, "支付处理记录暂时不可用，请稍后重试")
		return false
	}
	return true
}

func paymentStoreOutcome(err error) string {
	if errors.Is(err, store.ErrPaymentConflict) || errors.Is(err, store.ErrPaymentProviderConflict) || errors.Is(err, store.ErrNotFound) {
		return "conflict"
	}
	return "storage_error"
}
