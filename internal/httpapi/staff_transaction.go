package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/store"
)

// authorizeStaffTx runs after business locks, immediately before committing a
// staff write. current() authorizes request admission, but a queued transaction
// must also observe logout, account changes and expired step-up. Failed checks
// leave rollback to the caller; generated credentials must only be returned
// after Commit. Locks remain held until commit/rollback so authorization cannot
// be invalidated between this final check and the business commit.
func (s *Server) authorizeStaffTx(w http.ResponseWriter, r *http.Request, tx pgx.Tx, actor store.User) bool {
	var role string
	var version int64
	var emailVerified *time.Time
	err := tx.QueryRow(r.Context(), `SELECT role,security_version,email_verified_at FROM users WHERE id=$1 FOR SHARE`, actor.ID).Scan(&role, &version, &emailVerified)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version != actor.SecurityVersion) {
		fail(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return false
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法确认后台会话，请稍后重试")
		return false
	}
	if !permitted(role, requiredPermission(r)) {
		fail(w, http.StatusForbidden, "当前角色没有此操作权限")
		return false
	}
	if s.Config.RequireVerified && emailVerified == nil {
		fail(w, http.StatusForbidden, "请先完成邮箱验证")
		return false
	}
	var expires time.Time
	var stepUp *time.Time
	err = tx.QueryRow(r.Context(), `SELECT expires_at,mfa_verified_at FROM sessions WHERE token_hash=$1 AND user_id=$2 AND security_version=$3 FOR UPDATE`, mfaSessionHash(r), actor.ID, version).Scan(&expires, &stepUp)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return false
	}
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法确认后台会话，请稍后重试")
		return false
	}
	// Sample database time after acquiring both locks, not before any row wait.
	var now time.Time
	var enabled bool
	if err = tx.QueryRow(r.Context(), `SELECT clock_timestamp(),EXISTS(SELECT 1 FROM staff_mfa WHERE user_id=$1 AND enabled_at IS NOT NULL)`, actor.ID).Scan(&now, &enabled); err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法确认后台会话，请稍后重试")
		return false
	}
	if !expires.After(now) {
		fail(w, http.StatusUnauthorized, "登录已失效，请重新登录")
		return false
	}
	if enabled && len(s.Config.MFAKey) != 32 {
		fail(w, http.StatusServiceUnavailable, "二次验证暂时不可用，请联系主机管理员")
		return false
	}
	if !enabled && s.Config.RequireStaffMFA {
		reply(w, http.StatusForbidden, map[string]string{"error": "请先在账号安全中启用二次验证", "code": "mfa_enrollment_required"})
		return false
	}
	if enabled && (stepUp == nil || !stepUp.Add(store.MFASessionLifetime).After(now)) {
		reply(w, http.StatusForbidden, map[string]string{"error": "请先完成二次验证后再访问后台", "code": "mfa_required"})
		return false
	}
	return true
}
