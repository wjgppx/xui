package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var accountTokenPattern = regexp.MustCompile(`token=([0-9a-f]{64})`)

func latestAccountToken(t *testing.T, f fixture, userID, purpose string) string {
	t.Helper()
	var body string
	err := f.db.DB.QueryRow(context.Background(), `SELECT m.body FROM mail_outbox m JOIN account_tokens a ON a.token_hash=m.token_hash WHERE a.user_id=$1 AND a.purpose=$2 AND a.consumed_at IS NULL AND m.cancelled_at IS NULL ORDER BY m.created_at DESC,m.id DESC LIMIT 1`, userID, purpose).Scan(&body)
	if err != nil {
		t.Fatal("account message unavailable", err)
	}
	match := accountTokenPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatal("account action missing in fragment link")
	}
	return match[1]
}
func loginAccount(t *testing.T, f fixture, email, password string) (int, *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	code, _, cookies := request(t, f, "POST", "/api/v1/auth/login", string(body), nil)
	if len(cookies) == 0 {
		return code, nil
	}
	return code, cookies[0]
}
func accountBody(values map[string]string) string {
	body, _ := json.Marshal(values)
	return string(body)
}

func TestAccountVerificationPurposeSingleUseAndMailboxIsolation(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	_, other := register(t, f)
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", nil); code != 401 {
		t.Fatal("anonymous verification request", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", cookie); code != 202 {
		t.Fatal("verification request", code)
	}
	token := latestAccountToken(t, f, user.ID, "verify")
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", other); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("other account mailbox leaked", code)
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", nil); code != 401 {
		t.Fatal("anonymous mailbox accessible", code)
	}
	code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", cookie)
	if code != 200 || !strings.Contains(string(data), "#") || !strings.Contains(string(data), token) {
		t.Fatal("own development message missing", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/reset", accountBody(map[string]string{"token": token, "password": "a-different-valid-password"}), nil); code != 400 {
		t.Fatal("verification token reset a password", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", accountBody(map[string]string{"token": token}), nil); code != 200 {
		t.Fatal("confirm", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", accountBody(map[string]string{"token": token}), nil); code != 400 {
		t.Fatal("verification token reused", code)
	}
	var verified bool
	if err := f.db.DB.QueryRow(context.Background(), `SELECT email_verified_at IS NOT NULL FROM users WHERE id=$1`, user.ID).Scan(&verified); err != nil || !verified {
		t.Fatal("verification not committed", err)
	}
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", cookie); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("used verification link remained in mailbox", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", cookie); code != 200 {
		t.Fatal("already-verified request not idempotent", code)
	}
}
func TestForgotPasswordGenericResponsesAndPerEmailLimit(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	_, other := register(t, f)
	known := accountBody(map[string]string{"email": user.Email})
	unknown := accountBody(map[string]string{"email": "not-a-registered-user@example.test"})
	knownCode, knownBody, _ := request(t, f, "POST", "/api/v1/auth/password/forgot", known, nil)
	unknownCode, unknownBody, _ := request(t, f, "POST", "/api/v1/auth/password/forgot", unknown, nil)
	if knownCode != 202 || unknownCode != 202 || string(knownBody) != string(unknownBody) {
		t.Fatal("password request revealed account existence", knownCode, unknownCode)
	}
	if strings.Contains(string(knownBody), user.ID) || strings.Contains(string(knownBody), "token") {
		t.Fatal("password request returned account metadata")
	}
	latestAccountToken(t, f, user.ID, "reset")
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", other); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("reset link leaked to another user")
	}
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", cookie); code != 200 || !strings.Contains(string(data), "token=") {
		t.Fatal("own reset message missing")
	}
	for _, body := range []string{known, unknown} {
		for i := 0; i < 2; i++ {
			if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/forgot", body, nil); code != 202 {
				t.Fatal("limit before third request", code)
			}
		}
		if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/forgot", body, nil); code != 429 {
			t.Fatal("known and unknown email limits differ", code)
		}
	}
	var unknownMail int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM mail_outbox WHERE to_email='not-a-registered-user@example.test'`).Scan(&unknownMail); err != nil || unknownMail != 0 {
		t.Fatal("mail queued for nonexistent account", err)
	}
}
func TestPasswordResetExpiryRevokesEverySessionAndHasNoAutomaticLogin(t *testing.T) {
	f := setup(t, true)
	user, first := register(t, f)
	code, second := loginAccount(t, f, user.Email, "sufficiently-long-password")
	if code != 200 || second == nil {
		t.Fatal("second login failed")
	}
	forgot := accountBody(map[string]string{"email": user.Email})
	request(t, f, "POST", "/api/v1/auth/password/forgot", forgot, nil)
	expired := latestAccountToken(t, f, user.ID, "reset")
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", accountBody(map[string]string{"token": expired}), nil); code != 400 {
		t.Fatal("reset token verified email")
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE account_tokens SET created_at=now()-interval '1 hour',expires_at=now()-interval '1 second' WHERE user_id=$1 AND purpose='reset'`, user.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/reset", accountBody(map[string]string{"token": expired, "password": "new-password-from-reset"}), nil); code != 400 {
		t.Fatal("expired reset accepted", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/forgot", forgot, nil); code != 202 {
		t.Fatal(code)
	}
	token := latestAccountToken(t, f, user.ID, "reset")
	code, _, cookies := request(t, f, "POST", "/api/v1/auth/password/reset", accountBody(map[string]string{"token": token, "password": "new-password-from-reset"}), nil)
	if code != 200 {
		t.Fatal("reset failed", code)
	}
	for _, cookie := range cookies {
		if cookie.Name == "xui_session" && cookie.Value != "" {
			t.Fatal("reset automatically logged in")
		}
	}
	for _, cookie := range []*http.Cookie{first, second} {
		if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 401 {
			t.Fatal("reset left a session alive", code)
		}
	}
	if code, _ = loginAccount(t, f, user.Email, "sufficiently-long-password"); code != 401 {
		t.Fatal("old password remained valid")
	}
	code, fresh := loginAccount(t, f, user.Email, "new-password-from-reset")
	if code != 200 || fresh == nil {
		t.Fatal("new password cannot log in", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/reset", accountBody(map[string]string{"token": token, "password": "another-new-password"}), nil); code != 400 {
		t.Fatal("reset token reused", code)
	}
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", fresh); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("reset message retained after use")
	}
}
func TestChangePasswordInvalidatesOutstandingResetLinksAndSessions(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	request(t, f, "POST", "/api/v1/auth/password/forgot", accountBody(map[string]string{"email": user.Email}), nil)
	reset := latestAccountToken(t, f, user.ID, "reset")
	body := accountBody(map[string]string{"current_password": "wrong-but-valid-password", "new_password": "changed-password-for-account"})
	if code, _, _ := request(t, f, "POST", "/api/v1/me/password", body, cookie); code != 401 {
		t.Fatal("wrong current password accepted")
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 200 {
		t.Fatal("wrong password attempt revoked legitimate session")
	}
	body = accountBody(map[string]string{"current_password": "sufficiently-long-password", "new_password": "changed-password-for-account"})
	if code, _, _ := request(t, f, "POST", "/api/v1/me/password", body, cookie); code != 200 {
		t.Fatal("change failed", code)
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 401 {
		t.Fatal("change left session alive")
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/password/reset", accountBody(map[string]string{"token": reset, "password": "old-reset-must-not-work"}), nil); code != 400 {
		t.Fatal("old reset link survived password change")
	}
	if code, _ := loginAccount(t, f, user.Email, "changed-password-for-account"); code != 200 {
		t.Fatal("new password was not retained")
	}
	var count int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action='account.password_changed'`, user.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("password change audit missing", err)
	}
}
func TestProductionDevelopmentMailboxIsNotExposed(t *testing.T) {
	f := setup(t, false)
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", nil); code != 404 || strings.Contains(string(data), "token") {
		t.Fatal("production exposed development mailbox", code)
	}
	// Strict decoders must not accept an account identity supplied by the caller.
	f = setup(t, true)
	_, cookie := register(t, f)
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", fmt.Sprintf(`{"user_id":%q}`, "someone-else"), cookie); code != 400 {
		t.Fatal("verification accepted user override")
	}
}

func TestVerificationExpiryOriginAndExplicitConsumption(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", cookie); code != 202 {
		t.Fatal("verification request", code)
	}
	token := latestAccountToken(t, f, user.ID, "verify")
	body := accountBody(map[string]string{"token": token})
	// Mail-link scanners and browser navigation cannot consume a token with GET.
	if code, _, _ := request(t, f, "GET", "/api/v1/auth/email/confirm?token="+token, "", nil); code != 405 {
		t.Fatal("GET can consume a verification link", code)
	}
	req, err := http.NewRequest("POST", f.server.URL+"/api/v1/auth/email/confirm", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://untrusted.example")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("untrusted origin can consume a token", response.StatusCode)
	}
	var consumed bool
	if err = f.db.DB.QueryRow(context.Background(), `SELECT consumed_at IS NOT NULL FROM account_tokens WHERE user_id=$1`, user.ID).Scan(&consumed); err != nil || consumed {
		t.Fatal("navigation or untrusted request consumed the token", err)
	}
	if _, err = f.db.DB.Exec(context.Background(), `UPDATE account_tokens SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", body, nil); code != 400 {
		t.Fatal("expired verification token accepted", code)
	}
	if code, data, _ := request(t, f, "GET", "/api/v1/me/development-mail", "", cookie); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("expired link remained visible in development mailbox", code)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", cookie); code != 202 {
		t.Fatal("cannot replace expired verification link", code)
	}
	token = latestAccountToken(t, f, user.ID, "verify")
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", accountBody(map[string]string{"token": token}), nil); code != 200 {
		t.Fatal("fresh verification link rejected", code)
	}
}

func TestAccountActionsRejectMalformedInputWithoutMutation(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/v1/auth/password/forgot", `{"email":"not-an-email"}`},
		{"/api/v1/auth/password/forgot", `{"email":"valid@example.test","user_id":"someone-else"}`},
		{"/api/v1/auth/email/confirm", `{"token":"short"}`},
		{"/api/v1/auth/password/reset", accountBody(map[string]string{"token": strings.Repeat("g", 64), "password": "valid-long-password"})},
		{"/api/v1/auth/password/reset", accountBody(map[string]string{"token": strings.Repeat("a", 64), "password": "short"})},
		{"/api/v1/me/password", `{"current_password":"sufficiently-long-password","new_password":"sufficiently-long-password"}`},
		{"/api/v1/me/password", `{"current_password":"sufficiently-long-password","new_password":"short"}`},
	} {
		if code, _, _ := request(t, f, "POST", tc.path, tc.body, cookie); code != 400 {
			t.Fatalf("accepted invalid account action %s: %d", tc.path, code)
		}
	}
	var version int64
	var tokens int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT security_version FROM users WHERE id=$1`, user.ID).Scan(&version); err != nil || version != 0 {
		t.Fatal("invalid account action changed account version", err)
	}
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM account_tokens WHERE user_id=$1`, user.ID).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatal("invalid account action issued a token", err)
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 200 {
		t.Fatal("invalid account action revoked legitimate session", code)
	}
}
