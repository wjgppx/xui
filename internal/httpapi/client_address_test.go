package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/clientip"
	"github.com/wjgppx/xui/internal/httpapi"
)

func TestTrustedProxyFinancialLimitsSeparateClientsButStillShareUserLimits(t *testing.T) {
	f := setup(t, false)
	_, alice := staffAccount(t, f, "finance")
	_, bob := staffAccount(t, f, "finance")
	policy, _ := clientip.Parse("127.0.0.1/32")
	handler := httpapi.New(f.db, httpapi.Config{Origin: origin, TrustedProxy: policy, PasswordCost: 4}).Handler()
	body := fundsHTTPJSON(t, fundsHTTPInput("bank", "proxy_bank", "2026-10-29", "0", "0", ""))
	send := func(cookie *http.Cookie, client string) int {
		r := httptest.NewRequest("POST", "/api/v1/admin/fund-ledgers", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Forwarded-For", client)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if r.RemoteAddr != "127.0.0.1:12345" {
			t.Fatal("transport peer was overwritten")
		}
		return w.Code
	}
	for i := 0; i < 5; i++ {
		if code := send(alice, "192.0.2.10"); code != 200 {
			t.Fatal("valid trusted client rejected", code)
		}
	}
	if code := send(bob, "192.0.2.20"); code != 200 {
		t.Fatal("unrelated proxy client shared the peer bucket", code)
	}
	if code := send(bob, "192.0.2.10"); code != 429 {
		t.Fatal("another account bypassed client limit", code)
	}
	if code := send(alice, "192.0.2.30"); code != 429 {
		t.Fatal("another client address bypassed user limit", code)
	}
}

func TestTrustedProxyBadChainsFailClosedWithoutChangingOriginPolicy(t *testing.T) {
	f := setup(t, false)
	policy, _ := clientip.Parse("127.0.0.1/32")
	handler := httpapi.New(f.db, httpapi.Config{Origin: origin, TrustedProxy: policy, PasswordCost: 4}).Handler()
	for _, tc := range []struct {
		peer string
		want int
	}{{"127.0.0.1:1234", 400}, {"192.0.2.1:1234", 200}} {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", "malformed-chain")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal("forwarded trust boundary", w.Code, tc.want)
		}
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.5")
	r.Header.Set("Origin", "https://untrusted.example")
	r.Header.Set("X-Forwarded-Host", strings.TrimPrefix(origin, "http://"))
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("forwarding headers changed trusted origin", w.Code)
	}
}

func TestReadinessIdentifiesReleaseAndSchemaWithoutLeakingDatabaseFailures(t *testing.T) {
	f := setup(t, false)
	handler := httpapi.New(f.db, httpapi.Config{Origin: origin, ReleaseID: "candidate-security-v1", PasswordCost: 4}).Handler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	var ready struct {
		Status        string `json:"status"`
		Release       string `json:"release"`
		SchemaVersion int    `json:"schema_version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ready); err != nil || w.Code != 200 || ready.Status != "ok" || ready.Release != "candidate-security-v1" || ready.SchemaVersion < 16 {
		t.Fatal("release identity is not reviewable", w.Code, err)
	}
	if _, err := f.db.DB.Exec(context.Background(), `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 || bytes.Contains(w.Body.Bytes(), []byte("schema_migrations")) {
		t.Fatal("database failure did not fail closed", w.Code)
	}
}
