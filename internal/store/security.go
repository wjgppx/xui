package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidAccountToken = errors.New("invalid, expired or already consumed account token")

func securityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func validAccountToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// AllowRate shares one fixed-window counter between every control process. The
// key is hashed before persistence; caller-supplied forwarded IP headers are not
// interpreted here. Backend failures are errors, never an in-memory fallback.
func (s *Store) AllowRate(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if key == "" || len(key) > 4096 || limit < 1 || limit > 1000000 || window < time.Second || window > 24*time.Hour {
		return false, errors.New("invalid rate-limit policy")
	}
	var allowed bool
	// Use one timestamp for both CASE expressions. Reading the clock separately
	// could cross a window boundary between incrementing count and renewing expiry.
	err := s.DB.QueryRow(ctx, `WITH operation AS MATERIALIZED (SELECT clock_timestamp() AS at)
 INSERT INTO rate_limit_buckets(key_hash,count,expires_at)
 SELECT $1,1,at+make_interval(secs=>$3) FROM operation
 ON CONFLICT(key_hash) DO UPDATE SET
 count=CASE WHEN rate_limit_buckets.expires_at<=(SELECT at FROM operation) THEN 1 ELSE LEAST(rate_limit_buckets.count+1,$2+1) END,
 expires_at=CASE WHEN rate_limit_buckets.expires_at<=(SELECT at FROM operation) THEN EXCLUDED.expires_at ELSE rate_limit_buckets.expires_at END
 RETURNING count<=$2`, securityHash(key), limit, window.Seconds()).Scan(&allowed)
	if err != nil {
		return false, err
	}
	// Remove a bounded number of long-expired buckets without waiting on writers.
	// Rechecking expiry avoids deleting a bucket concurrently renewed by a caller.
	_, err = s.DB.Exec(ctx, `DELETE FROM rate_limit_buckets WHERE key_hash IN (
 SELECT key_hash FROM rate_limit_buckets WHERE expires_at<clock_timestamp()-interval '1 hour'
 ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED
 ) AND expires_at<clock_timestamp()-interval '1 hour'`)
	return allowed, err
}

func (s *Store) IssueAccountToken(ctx context.Context, userID, purpose, origin string) error {
	if purpose != "verify" && purpose != "reset" {
		return errors.New("invalid account token purpose")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(origin, "#") || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid account action origin")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var email string
	var verified *time.Time
	err = tx.QueryRow(ctx, `SELECT email,email_verified_at FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&email, &verified)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if purpose == "verify" && verified != nil {
		return tx.Commit(ctx)
	}
	if strings.ContainsAny(email, "\r\n") {
		return errors.New("invalid account email")
	}
	if _, err = tx.Exec(ctx, `UPDATE account_tokens SET consumed_at=clock_timestamp() WHERE user_id=$1 AND purpose=$2 AND consumed_at IS NULL`, userID, purpose); err != nil {
		return err
	}
	if err = cancelAccountMail(ctx, tx, userID, purpose); err != nil {
		return err
	}
	token := ID() + ID()
	hash := securityHash(token)
	ttl := 24 * time.Hour
	subject := "验证你的 XUI 邮箱"
	intro := "请打开链接验证邮箱，链接将在 24 小时后失效。"
	if purpose == "reset" {
		ttl = 30 * time.Minute
		subject = "重置你的 XUI 登录密码"
		intro = "请打开链接设置新密码，链接将在 30 分钟后失效。如果不是你发起，请忽略此邮件。"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO account_tokens(token_hash,user_id,purpose,expires_at) VALUES($1,$2,$3,clock_timestamp()+make_interval(secs=>$4))`, hash, userID, purpose, ttl.Seconds()); err != nil {
		return err
	}
	action := url.Values{"account-action": []string{purpose}, "token": []string{token}}
	link := origin + "/#" + action.Encode()
	body := intro + "\n\n" + link + "\n\n请勿向他人提供此链接。"
	if _, err = tx.Exec(ctx, `INSERT INTO mail_outbox(id,to_email,subject,body,token_hash) VALUES($1,$2,$3,$4,$5)`, ID(), email, subject, body, hash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func cancelAccountMail(ctx context.Context, tx pgx.Tx, userID, purpose string) error {
	_, err := tx.Exec(ctx, `UPDATE mail_outbox m SET body='',cancelled_at=clock_timestamp()
 FROM account_tokens t WHERE m.token_hash=t.token_hash AND t.user_id=$1 AND t.purpose=$2
 AND m.sent_at IS NULL AND m.cancelled_at IS NULL`, userID, purpose)
	return err
}

// Lock the account before its token, matching issue/change-password lock order.
// A nonlocking lookup only discovers ownership; validity is checked again after
// obtaining the account lock against the actual wall clock.
func lockAccountToken(ctx context.Context, tx pgx.Tx, token, purpose string) (string, error) {
	if !validAccountToken(token) {
		return "", ErrInvalidAccountToken
	}
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id FROM account_tokens WHERE token_hash=$1 AND purpose=$2`, securityHash(token), purpose).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidAccountToken
	}
	if err != nil {
		return "", err
	}
	var locked string
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR NO KEY UPDATE`, userID).Scan(&locked); err != nil {
		return "", err
	}
	err = tx.QueryRow(ctx, `SELECT user_id FROM account_tokens WHERE token_hash=$1 AND user_id=$2 AND purpose=$3 AND consumed_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, securityHash(token), userID, purpose).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidAccountToken
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

func (s *Store) ConfirmEmail(ctx context.Context, token string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	userID, err := lockAccountToken(ctx, tx, token, "verify")
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET email_verified_at=COALESCE(email_verified_at,clock_timestamp()) WHERE id=$1`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE account_tokens SET consumed_at=clock_timestamp() WHERE user_id=$1 AND purpose='verify' AND consumed_at IS NULL`, userID); err != nil {
		return err
	}
	if err = cancelAccountMail(ctx, tx, userID, "verify"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'account.email_verified',$1)`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ResetPassword(ctx context.Context, token, newBcryptHash string) error {
	if _, err := bcrypt.Cost([]byte(newBcryptHash)); err != nil || len(newBcryptHash) != 60 {
		return errors.New("invalid password hash")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	userID, err := lockAccountToken(ctx, tx, token, "reset")
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET password_hash=$1,security_version=security_version+1 WHERE id=$2`, newBcryptHash, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE account_tokens SET consumed_at=clock_timestamp() WHERE user_id=$1 AND purpose='reset' AND consumed_at IS NULL`, userID); err != nil {
		return err
	}
	if err = cancelAccountMail(ctx, tx, userID, "reset"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(actor_id,action,resource_id) VALUES($1,'account.password_reset',$1)`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
