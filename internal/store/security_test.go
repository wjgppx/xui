package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

func accountToken(t *testing.T, s *store.Store, userID, purpose string) string {
	t.Helper()
	if err := s.IssueAccountToken(context.Background(), userID, purpose, "https://portal.example.test"); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := s.DB.QueryRow(context.Background(), `SELECT body FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1 AND t.purpose=$2 AND m.cancelled_at IS NULL ORDER BY m.created_at DESC LIMIT 1`, userID, purpose).Scan(&body); err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "https://") {
			u, err := url.Parse(field)
			if err != nil {
				t.Fatal(err)
			}
			if u.RawQuery != "" {
				t.Fatal("account token leaked into query")
			}
			fragment, err := url.ParseQuery(u.Fragment)
			if err != nil {
				t.Fatal(err)
			}
			if fragment.Get("account-action") != purpose {
				t.Fatal("wrong action link")
			}
			token := fragment.Get("token")
			if len(token) != 64 {
				t.Fatal("invalid token")
			}
			return token
		}
	}
	t.Fatal("account mail lacks action link")
	return ""
}
func hashSecurity(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestSecurityVerificationHashPurposeResendAndConsume(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	first := accountToken(t, s, user, "verify")
	var stored string
	var seconds float64
	if err := s.DB.QueryRow(ctx, `SELECT token_hash,EXTRACT(EPOCH FROM expires_at-created_at) FROM account_tokens WHERE user_id=$1`, user).Scan(&stored, &seconds); err != nil {
		t.Fatal(err)
	}
	if stored == first || stored != hashSecurity(first) || seconds < 86399 || seconds > 86401 {
		t.Fatal("token not hashed or incorrect TTL")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte("new-safe-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(ctx, first, string(newHash)); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("verification token reset password", err)
	}
	second := accountToken(t, s, user, "verify")
	if second == first {
		t.Fatal("resend token reused")
	}
	if err = s.ConfirmEmail(ctx, first); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("replaced verification accepted", err)
	}
	var oldBody string
	var cancelled *time.Time
	if err = s.DB.QueryRow(ctx, `SELECT body,cancelled_at FROM mail_outbox WHERE token_hash=$1`, hashSecurity(first)).Scan(&oldBody, &cancelled); err != nil {
		t.Fatal(err)
	}
	if oldBody != "" || cancelled == nil {
		t.Fatal("superseded unsent mail not scrubbed")
	}
	if err = s.ConfirmEmail(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmEmail(ctx, second); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("verification token reused")
	}
	var verified *time.Time
	var consumed, mail int
	if err := s.DB.QueryRow(ctx, `SELECT email_verified_at FROM users WHERE id=$1`, user).Scan(&verified); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM account_tokens WHERE user_id=$1 AND consumed_at IS NULL`, user).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1 AND m.body<>''`, user).Scan(&mail); err != nil {
		t.Fatal(err)
	}
	if verified == nil || consumed != 0 || mail != 0 {
		t.Fatal("verification state incomplete")
	}
	// Already verified accounts do not receive a redundant usable link.
	if err = s.IssueAccountToken(ctx, user, "verify", "https://portal.example.test"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM account_tokens WHERE user_id=$1`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("verified account received new token")
	}
}

func TestSecurityResetConcurrentSingleUseInvalidatesAllSessions(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	token := accountToken(t, s, user, "reset")
	if err := s.ConfirmEmail(ctx, token); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("reset token verified email")
	}
	for i := 0; i < 2; i++ {
		if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, store.ID()+store.ID(), user); err != nil {
			t.Fatal(err)
		}
	}
	p, err := bcrypt.GenerateFromPassword([]byte("new-safe-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.ResetPassword(ctx, token, string(p)) }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, store.ErrInvalidAccountToken) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("reset single-use failure", success)
	}
	var sessions, version int
	var hash string
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id=$1`, user).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT password_hash,security_version FROM users WHERE id=$1`, user).Scan(&hash, &version); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || version != 1 || bcrypt.CompareHashAndPassword([]byte(hash), []byte("new-safe-password")) != nil {
		t.Fatal("reset did not invalidate sessions or change password")
	}
	var mail int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1 AND m.body<>''`, user).Scan(&mail); err != nil {
		t.Fatal(err)
	}
	if mail != 0 {
		t.Fatal("reset left plaintext action link")
	}
}

func TestSecurityExpiredTokensCannotChangeAccount(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	verify := accountToken(t, s, user, "verify")
	reset := accountToken(t, s, user, "reset")
	if _, err := s.DB.Exec(ctx, `UPDATE account_tokens SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmEmail(ctx, verify); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("expired verify accepted", err)
	}
	p, err := bcrypt.GenerateFromPassword([]byte("new-safe-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(ctx, reset, string(p)); !errors.Is(err, store.ErrInvalidAccountToken) {
		t.Fatal("expired reset accepted", err)
	}
	var verified *time.Time
	var version int
	if err := s.DB.QueryRow(ctx, `SELECT email_verified_at,security_version FROM users WHERE id=$1`, user).Scan(&verified, &version); err != nil {
		t.Fatal(err)
	}
	if verified != nil || version != 0 {
		t.Fatal("expired token changed account")
	}
}

func TestSecurityIssueMailFailurePreservesEarlierToken(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	first := accountToken(t, s, user, "verify")
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_security_mail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test reject mail'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `CREATE TRIGGER reject_security_mail BEFORE INSERT ON mail_outbox FOR EACH ROW EXECUTE FUNCTION reject_security_mail()`); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueAccountToken(ctx, user, "verify", "https://portal.example.test"); err == nil {
		t.Fatal("mail failure hidden")
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM account_tokens WHERE user_id=$1`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("mail failure left orphan token")
	}
	if err := s.ConfirmEmail(ctx, first); err != nil {
		t.Fatal("failed resend consumed earlier token", err)
	}
}

func TestSecurityResetAuditFailureRollsBackAccountSessionsAndToken(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	token := accountToken(t, s, user, "reset")
	if _, err := s.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 day')`, store.ID()+store.ID(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_security_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='account.password_reset' THEN RAISE EXCEPTION 'test reject audit'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `CREATE TRIGGER reject_security_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_security_audit()`); err != nil {
		t.Fatal(err)
	}
	p, err := bcrypt.GenerateFromPassword([]byte("new-safe-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(ctx, token, string(p)); err == nil {
		t.Fatal("audit failure hidden")
	}
	var version, sessions int
	var consumed *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT security_version FROM users WHERE id=$1`, user).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id=$1`, user).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT consumed_at FROM account_tokens WHERE token_hash=$1`, hashSecurity(token)).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if version != 0 || sessions != 1 || consumed != nil {
		t.Fatal("failed reset partially committed")
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_security_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(ctx, token, string(p)); err != nil {
		t.Fatal("rollback token not reusable", err)
	}
}

func TestSecuritySharedRateLimiterConcurrentWindowsAndBoundedCleanup(t *testing.T) {
	s := testutil.Database(t)
	other := &store.Store{DB: s.DB}
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan bool, 32)
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := s
			if i%2 == 0 {
				target = other
			}
			allowed, err := target.AllowRate(ctx, "login:127.0.0.1", 7, time.Minute)
			if err != nil {
				errs <- err
			}
			results <- allowed
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	allowed := 0
	for yes := range results {
		if yes {
			allowed++
		}
	}
	if allowed != 7 {
		t.Fatal("instances did not share limit", allowed)
	}
	var count int
	var key string
	if err := s.DB.QueryRow(ctx, `SELECT key_hash,count FROM rate_limit_buckets`).Scan(&key, &count); err != nil {
		t.Fatal(err)
	}
	if key == "login:127.0.0.1" || count != 8 {
		t.Fatal("bucket privacy or saturation failed")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE rate_limit_buckets SET expires_at=now()-interval '1 second' WHERE key_hash=$1`, key); err != nil {
		t.Fatal(err)
	}
	if yes, err := other.AllowRate(ctx, "login:127.0.0.1", 7, time.Minute); err != nil || !yes {
		t.Fatal("expired bucket did not reset", err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO rate_limit_buckets(key_hash,count,expires_at) SELECT md5('old'||n::text),1,now()-interval '2 hours' FROM generate_series(1,102) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllowRate(ctx, "another-key", 3, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM rate_limit_buckets WHERE expires_at<now()-interval '1 hour'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("rate cleanup was not bounded", count)
	}
}

func TestSecurityRateLimiterFailsClosedAndPolicyValidation(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	if allowed, err := s.AllowRate(ctx, "", 2, time.Minute); err == nil || allowed {
		t.Fatal("invalid rate policy accepted")
	}
	s.DB.Close()
	if allowed, err := s.AllowRate(ctx, "closed-backend", 2, time.Minute); err == nil || allowed {
		t.Fatal("limiter backend failure allowed request")
	}
}

func TestSecurityConcurrentResendsRetainOnlyOneUsableLink(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	const requests = 8
	results := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- s.IssueAccountToken(ctx, user, "verify", "https://portal.example.test")
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total, active, pending, cancelled int
	if err := s.DB.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE consumed_at IS NULL) FROM account_tokens WHERE user_id=$1`, user).Scan(&total, &active); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FILTER(WHERE m.cancelled_at IS NULL AND m.body<>''),count(*) FILTER(WHERE m.cancelled_at IS NOT NULL AND m.body='') FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1`, user).Scan(&pending, &cancelled); err != nil {
		t.Fatal(err)
	}
	if total != requests || active != 1 || pending != 1 || cancelled != requests-1 {
		t.Fatalf("concurrent resends left inconsistent state: tokens=%d active=%d pending=%d cancelled=%d", total, active, pending, cancelled)
	}
	var body string
	if err := s.DB.QueryRow(ctx, `SELECT m.body FROM mail_outbox m JOIN account_tokens t ON t.token_hash=m.token_hash WHERE t.user_id=$1 AND t.consumed_at IS NULL AND m.cancelled_at IS NULL`, user).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var token string
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "https://") {
			link, err := url.Parse(field)
			if err != nil {
				t.Fatal(err)
			}
			fragment, err := url.ParseQuery(link.Fragment)
			if err != nil {
				t.Fatal(err)
			}
			token = fragment.Get("token")
		}
	}
	if token == "" {
		t.Fatal("active mail lost action link")
	}
	if err := s.ConfirmEmail(ctx, token); err != nil {
		t.Fatal("latest concurrent resend unusable", err)
	}
}

func TestSecurityInvalidInputDoesNotRetireUsableToken(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	user := testutil.User(t, s)
	token := accountToken(t, s, user, "reset")
	var seconds float64
	if err := s.DB.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM expires_at-created_at) FROM account_tokens WHERE token_hash=$1`, hashSecurity(token)).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	if seconds < 1799 || seconds > 1801 {
		t.Fatal("incorrect reset token lifetime", seconds)
	}
	for _, origin := range []string{"https://portal.example.test?", "https://portal.example.test#", "https://portal.example.test/#token", "https://user:password@portal.example.test", "javascript:alert(1)", "https://portal.example.test/path"} {
		if err := s.IssueAccountToken(ctx, user, "reset", origin); err == nil {
			t.Fatal("invalid origin accepted", origin)
		}
	}
	if err := s.IssueAccountToken(ctx, user, "unsupported", "https://portal.example.test"); err == nil {
		t.Fatal("unsupported purpose accepted")
	}
	if err := s.ResetPassword(ctx, token, "$2"+strings.Repeat("x", 58)); err == nil {
		t.Fatal("malformed bcrypt hash accepted")
	}
	var count, live int
	if err := s.DB.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE consumed_at IS NULL) FROM account_tokens WHERE user_id=$1`, user).Scan(&count, &live); err != nil {
		t.Fatal(err)
	}
	if count != 1 || live != 1 {
		t.Fatal("invalid operation changed token state")
	}
	p, err := bcrypt.GenerateFromPassword([]byte("new-safe-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ResetPassword(ctx, token, string(p)); err != nil {
		t.Fatal("invalid operation invalidated legitimate token", err)
	}
}
