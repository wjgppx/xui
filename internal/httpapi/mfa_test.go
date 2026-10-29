package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/mfa"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func mfaHTTPHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func mfaHTTPSetup(t *testing.T, required bool, key []byte) fixture {
	t.Helper()
	s := testutil.Database(t)
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: origin, Development: true, PasswordCost: 4, MFAKey: key, RequireStaffMFA: required}).Handler())
	t.Cleanup(server.Close)
	return fixture{s, server}
}
func mfaHTTPEnroll(t *testing.T, f fixture, cookie *http.Cookie) (store.MFASetup, []string, *http.Cookie) {
	t.Helper()
	code, body, _ := request(t, f, "POST", "/api/v1/me/mfa/setup", accountBody(map[string]string{"current_password": "sufficiently-long-password"}), cookie)
	var setup store.MFASetup
	if code != 200 || json.Unmarshal(body, &setup) != nil || len(setup.Secret) != 32 || setup.OTPAuthURL == "" {
		t.Fatal("setup failed", code, string(body))
	}
	otp, err := mfa.Code(setup.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, body, cookies := request(t, f, "POST", "/api/v1/me/mfa/confirm", accountBody(map[string]string{"code": otp}), cookie)
	var result struct {
		Codes []string `json:"recovery_codes"`
	}
	if code != 200 || json.Unmarshal(body, &result) != nil || len(result.Codes) != 10 || len(cookies) != 1 {
		t.Fatal("confirmation failed", code, string(body))
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Value == cookie.Value {
		t.Fatal("unsafe upgraded session")
	}
	return setup, result.Codes, cookies[0]
}
func TestMFAHTTPEnrollmentSessionStepUpExpiryRecoveryAndDisable(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	f := mfaHTTPSetup(t, true, key)
	user, old := staffAccount(t, f, "admin")
	code, body, _ := request(t, f, "GET", "/api/v1/admin/users", "", old)
	if code != 403 || !bytes.Contains(body, []byte(`"code":"mfa_enrollment_required"`)) {
		t.Fatal("unenrolled staff bypassed", code, string(body))
	}
	code, body, _ = request(t, f, "GET", "/api/v1/me", "", old)
	var profile store.User
	if code != 200 || json.Unmarshal(body, &profile) != nil || !profile.MFARequired || profile.MFAEnabled || profile.MFAVerifiedUntil != nil {
		t.Fatal("missing profile policy", code, string(body))
	}
	setup, recovery, cookie := mfaHTTPEnroll(t, f, old)
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", old); code != 401 {
		t.Fatal("old login survived activation", code)
	}
	if code, body, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookie); code != 200 {
		t.Fatal("upgraded login rejected", code, string(body))
	}
	code, body, _ = request(t, f, "GET", "/api/v1/me/mfa", "", cookie)
	var status struct {
		Available, Enabled, Required, SetupPending bool
		VerifiedUntil                              *time.Time `json:"verified_until"`
		Remaining                                  int        `json:"recovery_codes_remaining"`
	}
	if code != 200 || json.Unmarshal(body, &status) != nil || !status.Available || !status.Enabled || !status.Required || status.VerifiedUntil == nil || status.Remaining != 10 || bytes.Contains(body, []byte(setup.Secret)) || bytes.Contains(body, []byte(recovery[0])) {
		t.Fatal("unsafe/incomplete status", code, string(body))
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE sessions SET mfa_verified_at=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	code, body, _ = request(t, f, "GET", "/api/v1/admin/users", "", cookie)
	if code != 403 || !bytes.Contains(body, []byte(`"code":"mfa_required"`)) {
		t.Fatal("expired stepup accepted", code, string(body))
	}
	code, body, _ = request(t, f, "POST", "/api/v1/me/mfa/verify", accountBody(map[string]string{"code": recovery[0]}), cookie)
	if code != 200 || !bytes.Contains(body, []byte("verified_until")) {
		t.Fatal("recovery verification failed", code, string(body))
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/mfa/verify", accountBody(map[string]string{"code": recovery[0]}), cookie); code != 401 {
		t.Fatal("recovery replay accepted", code)
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookie); code != 200 {
		t.Fatal("recovered session rejected", code)
	}
	code, body, cookies := request(t, f, "POST", "/api/v1/me/mfa/disable", accountBody(map[string]string{"current_password": "sufficiently-long-password", "code": recovery[1]}), cookie)
	if code != 200 || len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatal("disable failed", code, string(body))
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 401 {
		t.Fatal("disabled session retained authority", code)
	}
	code, body, cookies = request(t, f, "POST", "/api/v1/auth/login", accountBody(map[string]string{"email": user.Email, "password": "sufficiently-long-password"}), nil)
	if code != 200 || len(cookies) != 1 {
		t.Fatal("fresh login failed", code, string(body))
	}
	if code, body, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookies[0]); code != 403 || !bytes.Contains(body, []byte("mfa_enrollment_required")) {
		t.Fatal("disabling bypassed mandatory enrollment", code, string(body))
	}
}
func TestMFAHTTPEnrolledStaffCannotBypassWithPolicyOffOrMissingKey(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	f := mfaHTTPSetup(t, false, key)
	_, cookie := staffAccount(t, f, "admin")
	if code, _, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookie); code != 200 {
		t.Fatal("optional unenrolled compatibility lost", code)
	}
	_, _, cookie = mfaHTTPEnroll(t, f, cookie)
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE sessions SET mfa_verified_at=NULL`); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookie); code != 403 || !bytes.Contains(body, []byte("mfa_required")) {
		t.Fatal("policy off bypassed enrolled factor", code, string(body))
	}
	unavailable := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4}).Handler())
	t.Cleanup(unavailable.Close)
	f.server = unavailable
	if code, body, _ := request(t, f, "GET", "/api/v1/admin/users", "", cookie); code != 503 || bytes.Contains(body, []byte("cipher")) {
		t.Fatal("missing key opened admin access", code, string(body))
	}
}
func TestMFAHTTPStaffProductionVerificationOriginAndStrictBodies(t *testing.T) {
	f := mfaHTTPSetup(t, false, bytes.Repeat([]byte{5}, 32))
	_, ordinary := register(t, f)
	if code, _, _ := request(t, f, "GET", "/api/v1/me/mfa", "", ordinary); code != 403 {
		t.Fatal("ordinary customer exposed staff MFA", code)
	}
	user, cookie := staffAccount(t, f, "finance")
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", "/api/v1/me/mfa?", ""},
		{"POST", "/api/v1/me/mfa/setup", `{"current_password":"sufficiently-long-password","secret":"untrusted"}`},
		{"POST", "/api/v1/me/mfa/confirm", `{"code":"123456"}{}`},
		{"POST", "/api/v1/me/mfa/verify", `{"code":123456}`},
		{"POST", "/api/v1/me/mfa/disable", `{"current_password":"` + strings.Repeat("x", 66000) + `","code":"000000"}`},
	} {
		if code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie); code != 400 {
			t.Fatal("invalid MFA request accepted", endpoint.path, code, string(body))
		}
	}
	req, _ := http.NewRequest("POST", f.server.URL+"/api/v1/me/mfa/setup", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("untrusted origin allowed", response.StatusCode)
	}
	production := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, RequireVerified: true, PasswordCost: 4, MFAKey: bytes.Repeat([]byte{5}, 32)}).Handler())
	t.Cleanup(production.Close)
	f.server = production
	if code, _, _ := request(t, f, "GET", "/api/v1/me/mfa", "", cookie); code != 403 {
		t.Fatal("unverified production staff reached factor", code)
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE users SET email_verified_at=clock_timestamp() WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me/mfa", "", cookie); code != 200 {
		t.Fatal("verified staff blocked", code)
	}
}
func TestMFAHTTPSharedAttemptLimitsAndSafeStorageFailures(t *testing.T) {
	key := bytes.Repeat([]byte{6}, 32)
	f := mfaHTTPSetup(t, false, key)
	_, cookie := staffAccount(t, f, "admin")
	_, _, cookie = mfaHTTPEnroll(t, f, cookie)
	for i := 0; i < 5; i++ {
		if code, _, _ := request(t, f, "POST", "/api/v1/me/mfa/verify", `{"code":"invalid"}`, cookie); code != 401 {
			t.Fatal("wrong factor result", code)
		}
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/mfa/verify", `{"code":"invalid"}`, cookie); code != 429 {
		t.Fatal("unbounded factor guesses", code)
	}
	handler := httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4, MFAKey: key}).Handler()
	req := httptest.NewRequest("POST", "/api/v1/me/mfa/verify", strings.NewReader(`{"code":"invalid"}`))
	req.RemoteAddr = "192.0.2.7:12345"
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, req)
	if record.Code != 429 {
		t.Fatal("new peer bypassed user MFA budget", record.Code)
	}
	if _, err := f.db.DB.Exec(context.Background(), `ALTER TABLE staff_mfa RENAME TO sensitive_factor_failure`); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := request(t, f, "GET", "/api/v1/me/mfa", "", cookie); code != 503 || bytes.Contains(body, []byte("sensitive")) || bytes.Contains(body, []byte("staff_mfa")) {
		t.Fatal("MFA storage error leaked", code, string(body))
	}
}
