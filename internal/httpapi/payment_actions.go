package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wjgppx/xui/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func paymentActionReason(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func (s *Server) paymentActionUser(w http.ResponseWriter, r *http.Request, purpose string) (store.User, bool) {
	u, ok := s.current(w, r, true)
	if !ok {
		return u, false
	}
	if !s.allowedKey(r, "payment-action:"+purpose+":", 5) {
		fail(w, 429, "支付操作过于频繁，请稍后重试")
		return u, false
	}
	allowed, err := s.Store.AllowRate(r.Context(), "payment-action-user:"+purpose+":"+u.ID, 5, time.Minute)
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

// Queueing a money action requires a fresh password check. The store checks the
// same security generation again while locking the actor, closing the gap with
// a concurrent password or role change after the bcrypt comparison.
func (s *Server) paymentActionReauthenticate(w http.ResponseWriter, r *http.Request, u store.User, password string) bool {
	if !validAccountPassword(password) {
		fail(w, 401, "当前密码不正确或登录状态已变化")
		return false
	}
	var passwordHash string
	var version int64
	err := s.Store.DB.QueryRow(r.Context(), `SELECT password_hash,security_version FROM users WHERE id=$1`, u.ID).Scan(&passwordHash, &version)
	if err != nil {
		fail(w, 503, "暂时无法验证身份，请稍后重试")
		return false
	}
	if version != u.SecurityVersion || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		fail(w, 401, "当前密码不正确或登录状态已变化")
		return false
	}
	return true
}

func (s *Server) paymentActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrRoleForbidden):
		fail(w, 403, "身份或后台权限已变化，请重新登录")
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "支付或退款记录不存在")
	case errors.Is(err, store.ErrPaymentConflict), errors.Is(err, store.ErrPaymentProviderConflict):
		fail(w, 409, "此记录当前不能执行该操作；仅支持支付宝和微信的原订单全额退款及渠道关单")
	default:
		fail(w, 500, "支付操作暂时不可用，请稍后重试")
	}
}

func (s *Server) requestRefund(w http.ResponseWriter, r *http.Request) {
	u, ok := s.paymentActionUser(w, r, "refund-request")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	reason, valid := paymentActionReason(body.Reason)
	if !valid {
		fail(w, 400, "请填写退款原因，长度需为 1–256 字节")
		return
	}
	refund, err := s.Store.RequestRefund(r.Context(), u.ID, r.PathValue("id"), reason)
	if err != nil {
		s.paymentActionError(w, err)
		return
	}
	// A request is only an approval draft, never proof of a completed refund.
	reply(w, 200, refund)
}

func (s *Server) listRefunds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.current(w, r, true); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, 400, "退款列表暂不支持这些筛选条件")
		return
	}
	items, err := s.Store.ListRefunds(r.Context())
	if err != nil {
		s.paymentActionError(w, err)
		return
	}
	reply(w, 200, map[string]any{"items": items})
}

func (s *Server) approveRefund(w http.ResponseWriter, r *http.Request) {
	u, ok := s.paymentActionUser(w, r, "refund-approve")
	if !ok {
		return
	}
	if !s.Config.PaymentActionsEnabled {
		fail(w, 503, "资金操作尚未启用，退款申请仍可保留等待审核")
		return
	}
	var body struct {
		Password string `json:"current_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.paymentActionReauthenticate(w, r, u, body.Password) {
		return
	}
	if err := s.Store.ApproveRefund(r.Context(), u.ID, r.PathValue("id"), u.SecurityVersion); err != nil {
		s.paymentActionError(w, err)
		return
	}
	reply(w, 202, map[string]bool{"ok": true})
}

func (s *Server) requestGatewayClose(w http.ResponseWriter, r *http.Request) {
	u, ok := s.paymentActionUser(w, r, "gateway-close")
	if !ok {
		return
	}
	if !s.Config.PaymentActionsEnabled {
		fail(w, 503, "渠道关单尚未启用")
		return
	}
	var body struct {
		Reason   string `json:"reason"`
		Password string `json:"current_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	reason, valid := paymentActionReason(body.Reason)
	if !valid {
		fail(w, 400, "请填写关单原因，长度需为 1–256 字节")
		return
	}
	if !s.paymentActionReauthenticate(w, r, u, body.Password) {
		return
	}
	if err := s.Store.RequestPaymentClose(r.Context(), u.ID, r.PathValue("id"), reason, u.SecurityVersion); err != nil {
		s.paymentActionError(w, err)
		return
	}
	reply(w, 202, map[string]bool{"ok": true})
}
