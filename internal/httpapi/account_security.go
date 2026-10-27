package httpapi

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func normalizeAccountEmail(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(value)
	return value, err == nil && address.Address == value && len(value) <= 254
}
func validAccountPassword(value string) bool { return len(value) >= 12 && len(value) <= 72 }
func (s *Server) accountEmailRate(w http.ResponseWriter, r *http.Request, email, purpose string) bool {
	if !s.allowedKey(r, "account:"+purpose+":", 10) {
		fail(w, 429, "请求过于频繁，请稍后再试")
		return false
	}
	allowed, err := s.Store.AllowRate(r.Context(), "email:"+purpose+":"+email, 3, time.Minute)
	if err != nil {
		serverError(w, errors.New("account request limiter unavailable"))
		return false
	}
	if !allowed {
		fail(w, 429, "请求过于频繁，请稍后再试")
		return false
	}
	return true
}
func (s *Server) requestVerification(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r, false)
	if !ok {
		return
	}
	if !decode(w, r, &struct{}{}) {
		return
	}
	if user.EmailVerifiedAt != nil {
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if !s.accountEmailRate(w, r, user.Email, "verify") {
		return
	}
	if err := s.Store.IssueAccountToken(r.Context(), user.ID, "verify", s.Config.Origin); err != nil {
		serverError(w, errors.New("verification request could not be stored"))
		return
	}
	reply(w, 202, map[string]bool{"ok": true})
}
func (s *Server) confirmEmail(w http.ResponseWriter, r *http.Request) {
	if !s.allowedKey(r, "account:confirm:", 10) {
		fail(w, 429, "请求过于频繁，请稍后再试")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validToken(body.Token) {
		fail(w, 400, "验证链接无效或已过期")
		return
	}
	err := s.Store.ConfirmEmail(r.Context(), body.Token)
	if errors.Is(err, store.ErrInvalidAccountToken) {
		fail(w, 400, "验证链接无效或已过期")
		return
	}
	if err != nil {
		serverError(w, errors.New("verification could not be completed"))
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	email, valid := normalizeAccountEmail(body.Email)
	if !valid {
		fail(w, 400, "请输入有效邮箱")
		return
	}
	if !s.accountEmailRate(w, r, email, "reset") {
		return
	}
	var userID string
	err := s.Store.DB.QueryRow(r.Context(), `SELECT id FROM users WHERE email=$1`, email).Scan(&userID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		serverError(w, errors.New("password request unavailable"))
		return
	}
	if err == nil {
		if err = s.Store.IssueAccountToken(r.Context(), userID, "reset", s.Config.Origin); err != nil {
			serverError(w, errors.New("password request could not be stored"))
			return
		}
	}
	// The same status and body apply to known and unknown addresses. Delivery is
	// asynchronous; no provider error or account metadata appears in this response.
	reply(w, 202, map[string]bool{"ok": true})
}
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.allowedKey(r, "account:reset-complete:", 10) {
		fail(w, 429, "请求过于频繁，请稍后再试")
		return
	}
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validToken(body.Token) || !validAccountPassword(body.Password) {
		fail(w, 400, "重置链接无效，或密码不符合 12–72 字节要求")
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(body.Password), s.Config.PasswordCost)
	if err != nil {
		serverError(w, errors.New("password hashing unavailable"))
		return
	}
	err = s.Store.ResetPassword(r.Context(), body.Token, string(passwordHash))
	if errors.Is(err, store.ErrInvalidAccountToken) {
		fail(w, 400, "重置链接无效或已过期")
		return
	}
	if err != nil {
		serverError(w, errors.New("password reset could not be completed"))
		return
	}
	clearAccountCookie(w, s.Config.SecureCookies)
	reply(w, 200, map[string]bool{"ok": true})
}
func clearAccountCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: "xui_session", Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r, false)
	if !ok {
		return
	}
	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validAccountPassword(body.Current) || !validAccountPassword(body.New) || body.Current == body.New {
		fail(w, 400, "新密码需为 12–72 字节，并与当前密码不同")
		return
	}
	if !s.accountEmailRate(w, r, user.Email, "password-change") {
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.New), s.Config.PasswordCost)
	if err != nil {
		serverError(w, errors.New("password hashing unavailable"))
		return
	}
	tx, err := s.Store.DB.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var oldHash string
	var version int64
	err = tx.QueryRow(r.Context(), `SELECT password_hash,security_version FROM users WHERE id=$1 FOR NO KEY UPDATE`, user.ID).Scan(&oldHash, &version)
	if err != nil {
		serverError(w, errors.New("password change unavailable"))
		return
	}
	if version != user.SecurityVersion || bcrypt.CompareHashAndPassword([]byte(oldHash), []byte(body.Current)) != nil {
		fail(w, 401, "当前密码不正确或登录状态已变化")
		return
	}
	_, err = tx.Exec(r.Context(), `UPDATE users SET password_hash=$1,security_version=security_version+1 WHERE id=$2`, string(newHash), user.ID)
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE account_tokens SET consumed_at=clock_timestamp() WHERE user_id=$1 AND purpose='reset' AND consumed_at IS NULL`, user.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `UPDATE mail_outbox m SET body='',cancelled_at=clock_timestamp() FROM account_tokens t WHERE m.token_hash=t.token_hash AND t.user_id=$1 AND t.purpose='reset' AND m.sent_at IS NULL`, user.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, user.ID)
	}
	if err == nil {
		_, err = tx.Exec(r.Context(), `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'account.password_changed',$1)`, user.ID)
	}
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		serverError(w, errors.New("password change could not be committed"))
		return
	}
	clearAccountCookie(w, s.Config.SecureCookies)
	reply(w, 200, map[string]bool{"ok": true})
}
func (s *Server) developmentMail(w http.ResponseWriter, r *http.Request) {
	if !s.Config.Development {
		http.NotFound(w, r)
		return
	}
	user, ok := s.current(w, r, false)
	if !ok {
		return
	}
	rows, err := s.Store.DB.Query(r.Context(), `SELECT m.id,m.subject,m.body,m.created_at FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE m.to_email=$1 AND m.sent_at IS NULL AND m.cancelled_at IS NULL AND m.body<>'' AND t.consumed_at IS NULL AND t.expires_at>clock_timestamp() ORDER BY m.created_at DESC,m.id DESC LIMIT 10`, user.Email)
	if err != nil {
		serverError(w, errors.New("development mailbox unavailable"))
		return
	}
	defer rows.Close()
	type message struct {
		ID        string    `json:"id"`
		Subject   string    `json:"subject"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	}
	messages := []message{}
	for rows.Next() {
		var m message
		if err = rows.Scan(&m.ID, &m.Subject, &m.Body, &m.CreatedAt); err != nil {
			serverError(w, errors.New("development mailbox unavailable"))
			return
		}
		messages = append(messages, m)
	}
	if err = rows.Err(); err != nil {
		serverError(w, errors.New("development mailbox unavailable"))
		return
	}
	reply(w, 200, messages)
}
