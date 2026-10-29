package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/wjgppx/xui/internal/store"
)

func (s *Server) decorateUserMFA(ctx context.Context, user *store.User, sessionHash string) (store.MFAStatus, error) {
	user.MFAEnabled, user.MFARequired, user.MFAVerifiedUntil = false, false, nil
	if !store.IsStaffRole(user.Role) {
		return store.MFAStatus{}, nil
	}
	status, err := s.Store.MFAStatus(ctx, user.ID, sessionHash)
	if err != nil {
		return status, err
	}
	user.MFAEnabled = status.Enabled
	user.MFARequired = status.Enabled || s.Config.RequireStaffMFA
	user.MFAVerifiedUntil = status.VerifiedUntil
	return status, nil
}
func mfaSessionHash(r *http.Request) string {
	cookie, err := r.Cookie("xui_session")
	if err != nil {
		return ""
	}
	return hash(cookie.Value)
}
func (s *Server) mfaUser(w http.ResponseWriter, r *http.Request, purpose string) (store.User, bool) {
	user, ok := s.current(w, r, false)
	if !ok {
		return user, false
	}
	if !store.IsStaffRole(user.Role) {
		fail(w, http.StatusForbidden, "二次验证仅适用于后台账号")
		return user, false
	}
	if s.Config.RequireVerified && user.EmailVerifiedAt == nil {
		fail(w, http.StatusForbidden, "请先完成邮箱验证")
		return user, false
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		fail(w, http.StatusBadRequest, "二次验证接口不接受查询参数")
		return user, false
	}
	if purpose != "" {
		if !s.allowedKey(r, "mfa-peer:"+purpose+":", 5) {
			fail(w, http.StatusTooManyRequests, "二次验证操作过于频繁，请稍后重试")
			return user, false
		}
		allowed, err := s.Store.AllowRate(r.Context(), "mfa-user:"+purpose+":"+user.ID, 5, time.Minute)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "二次验证暂时不可用")
			return user, false
		}
		if !allowed {
			fail(w, http.StatusTooManyRequests, "二次验证操作过于频繁，请稍后重试")
			return user, false
		}
	}
	return user, true
}
func (s *Server) mfaStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mfaUser(w, r, "")
	if !ok {
		return
	}
	status, err := s.Store.MFAStatus(r.Context(), user.ID, mfaSessionHash(r))
	if err != nil {
		s.mfaError(w, err)
		return
	}
	reply(w, http.StatusOK, struct {
		store.MFAStatus
		Available bool `json:"available"`
		Required  bool `json:"required"`
	}{status, len(s.Config.MFAKey) == 32, status.Enabled || s.Config.RequireStaffMFA})
}
func (s *Server) mfaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrMFAInvalid):
		fail(w, http.StatusUnauthorized, "当前密码或二次验证码不正确、已使用，或状态已变化")
	case errors.Is(err, store.ErrMFAConflict):
		fail(w, http.StatusConflict, "二次验证设置已变化或过期，请重新读取状态")
	case errors.Is(err, store.ErrRoleForbidden):
		fail(w, http.StatusForbidden, "后台权限或登录状态已变化，请重新登录")
	case errors.Is(err, store.ErrEmailUnverified):
		fail(w, http.StatusForbidden, "请先完成邮箱验证")
	case errors.Is(err, store.ErrMFAUnavailable):
		fail(w, http.StatusServiceUnavailable, "二次验证密钥暂时不可用，请联系主机管理员")
	default:
		fail(w, http.StatusServiceUnavailable, "二次验证暂时不可用，请稍后重试")
	}
}
func (s *Server) mfaSetup(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mfaUser(w, r, "setup")
	if !ok {
		return
	}
	var body struct {
		Password string `json:"current_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	result, err := s.Store.BeginMFASetup(r.Context(), user.ID, mfaSessionHash(r), body.Password, user.SecurityVersion, s.Config.MFAKey)
	if err != nil {
		s.mfaError(w, err)
		return
	}
	reply(w, http.StatusOK, result)
}
func (s *Server) mfaConfirm(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mfaUser(w, r, "confirm")
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	result, err := s.Store.ConfirmMFA(r.Context(), user.ID, mfaSessionHash(r), body.Code, user.SecurityVersion, s.Config.MFAKey)
	if err != nil {
		s.mfaError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "xui_session", Value: result.SessionToken, Path: "/", HttpOnly: true, Secure: s.Config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	reply(w, http.StatusOK, struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}{result.RecoveryCodes})
}
func (s *Server) mfaVerify(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mfaUser(w, r, "verify")
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	until, err := s.Store.VerifyMFA(r.Context(), user.ID, mfaSessionHash(r), body.Code, user.SecurityVersion, s.Config.MFAKey)
	if err != nil {
		s.mfaError(w, err)
		return
	}
	reply(w, http.StatusOK, struct {
		VerifiedUntil time.Time `json:"verified_until"`
	}{until})
}
func (s *Server) mfaDisable(w http.ResponseWriter, r *http.Request) {
	user, ok := s.mfaUser(w, r, "disable")
	if !ok {
		return
	}
	var body struct {
		Password string `json:"current_password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.Store.DisableMFA(r.Context(), user.ID, mfaSessionHash(r), body.Password, body.Code, user.SecurityVersion, s.Config.MFAKey); err != nil {
		s.mfaError(w, err)
		return
	}
	clearAccountCookie(w, s.Config.SecureCookies)
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}
