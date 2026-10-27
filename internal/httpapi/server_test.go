package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

const origin = "http://127.0.0.1:5173"

type fixture struct {
	db     *store.Store
	server *httptest.Server
}

func setup(t *testing.T, development bool) fixture {
	t.Helper()
	s := testutil.Database(t)
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: origin, Development: development, PasswordCost: 4}).Handler())
	t.Cleanup(server.Close)
	return fixture{s, server}
}
func request(t *testing.T, f fixture, method, path, body string, cookie *http.Cookie) (int, []byte, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, data, r.Cookies()
}
func register(t *testing.T, f fixture) (store.User, *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": store.ID() + "@example.test", "password": "sufficiently-long-password"})
	status, b, c := request(t, f, "POST", "/api/v1/auth/register", string(body), nil)
	if status != 200 || len(c) != 1 {
		t.Fatalf("register %d %s", status, b)
	}
	var u store.User
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatal(err)
	}
	return u, c[0]
}
func TestPurchaseAndLogout(t *testing.T) {
	f := setup(t, true)
	_, c := register(t, f)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe session cookie")
	}
	status, b, _ := request(t, f, "POST", "/api/v1/orders", `{"plan_id":"explore"}`, c)
	if status != 201 {
		t.Fatalf("create %d %s", status, b)
	}
	var o store.Order
	_ = json.Unmarshal(b, &o)
	for i := 0; i < 2; i++ {
		status, b, _ = request(t, f, "POST", "/api/v1/dev/orders/"+o.ID+"/settle", "{}", c)
		if status != 200 {
			t.Fatalf("settle %d %s", status, b)
		}
	}
	status, b, _ = request(t, f, "GET", "/api/v1/me/entitlements", "", c)
	var e []store.Entitlement
	_ = json.Unmarshal(b, &e)
	if status != 200 || len(e) != 1 {
		t.Fatalf("entitlements %d %s", status, b)
	}
	status, _, _ = request(t, f, "POST", "/api/v1/auth/logout", "{}", c)
	if status != 200 {
		t.Fatal(status)
	}
	status, _, _ = request(t, f, "GET", "/api/v1/me", "", c)
	if status != 401 {
		t.Fatal("logout did not revoke session")
	}
}
func TestUsersCannotPayOthersOrdersOrAccessAdmin(t *testing.T) {
	f := setup(t, true)
	_, c1 := register(t, f)
	_, c2 := register(t, f)
	status, b, _ := request(t, f, "POST", "/api/v1/orders", `{"plan_id":"explore"}`, c1)
	if status != 201 {
		t.Fatal(string(b))
	}
	var o store.Order
	_ = json.Unmarshal(b, &o)
	status, _, _ = request(t, f, "POST", "/api/v1/dev/orders/"+o.ID+"/settle", "{}", c2)
	if status != 404 {
		t.Fatalf("cross-user order access: %d", status)
	}
	status, b, _ = request(t, f, "GET", "/api/v1/orders", "", c2)
	if status != 200 || string(bytes.TrimSpace(b)) != "[]" {
		t.Fatal("other user's orders leaked")
	}
	for _, path := range []string{"/api/v1/admin/orders", "/api/v1/admin/overview"} {
		status, _, _ = request(t, f, "GET", path, "", c2)
		if status != 403 {
			t.Fatal("admin resource exposed")
		}
	}
	status, _, _ = request(t, f, "POST", "/api/v1/admin/plans", "{}", c2)
	if status != 403 {
		t.Fatal("user can create plans")
	}
}
func TestDevelopmentSettlementUnavailableInProduction(t *testing.T) {
	f := setup(t, false)
	_, c := register(t, f)
	status, _, _ := request(t, f, "POST", "/api/v1/dev/orders/any/settle", "{}", c)
	if status != 404 {
		t.Fatalf("dev settlement available in production: %d", status)
	}
}
func TestOriginAndStrictJSON(t *testing.T) {
	f := setup(t, true)
	req, _ := http.NewRequest("POST", f.server.URL+"/api/v1/auth/register", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://untrusted.example")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatal("untrusted origin accepted")
	}
	_, c := register(t, f)
	for _, body := range []string{`{"plan_id":"explore","price_minor":1}`, `{"plan_id":"explore"} {}`, `null {}`} {
		status, _, _ := request(t, f, "POST", "/api/v1/orders", body, c)
		if status != 400 {
			t.Fatalf("accepted invalid body %s: %d", body, status)
		}
	}
}
func TestAdminPlanAuditAndExpiredSession(t *testing.T) {
	f := setup(t, true)
	u, c := register(t, f)
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	status, b, _ := request(t, f, "POST", "/api/v1/admin/plans", `{"name":"测试套餐","description":"描述","price_minor":2000,"currency":"CNY","duration_days":30,"quota_bytes":1073741824}`, c)
	if status != 201 {
		t.Fatalf("plan create %d %s", status, b)
	}
	var count int
	if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action='plan.created'`, u.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("missing audit record")
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE sessions SET expires_at=now()-interval '1 second' WHERE user_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	status, _, _ = request(t, f, "GET", "/api/v1/admin/overview", "", c)
	if status != 401 {
		t.Fatal("expired session accepted")
	}
}
func TestLoginEmailNormalizationAndWrongPassword(t *testing.T) {
	f := setup(t, true)
	status, b, _ := request(t, f, "POST", "/api/v1/auth/register", `{"email":" PERSON@EXAMPLE.TEST ","password":"sufficiently-long-password"}`, nil)
	if status != 200 {
		t.Fatal(string(b))
	}
	status, _, _ = request(t, f, "POST", "/api/v1/auth/register", `{"email":"person@example.test","password":"sufficiently-long-password"}`, nil)
	if status != 409 {
		t.Fatal("duplicate email accepted")
	}
	status, _, _ = request(t, f, "POST", "/api/v1/auth/login", `{"email":"person@example.test","password":"wrong-long-password"}`, nil)
	if status != 401 {
		t.Fatal("wrong password accepted")
	}
	status, _, _ = request(t, f, "POST", "/api/v1/auth/login", `{"email":"person@example.test","password":"sufficiently-long-password"}`, nil)
	if status != 200 {
		t.Fatal("valid login failed")
	}
}
