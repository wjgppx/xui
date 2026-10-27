package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
)

func TestProductionVerificationProtectsPurchasesAndExistingNodeGrants(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	node, enrollment, admin := adminNode(t, f)
	u, cookie := register(t, f)
	cfg := map[string]any{"revision": 1, "bindings": []agentwire.BindingConfig{{ID: "verified-only", Protocol: "vless", Listen: "127.0.0.1", Port: 18443, ServerName: "node.example.test", Certificate: "node", PublicHost: "node.example.test", PublicPort: 443, Users: []string{u.Email}}}}
	body, _ := json.Marshal(cfg)
	if code, b, _ := request(t, f, "POST", "/api/v1/admin/nodes/"+node+"/configuration", string(body), admin); code != 200 {
		t.Fatalf("configure: %d %s", code, b)
	}
	order, err := f.db.CreateOrder(ctx, u.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(ctx, "test", store.ID(), order.ID, u.ID, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	token := issueSubscription(t, f, cookie)
	agent := store.ID() + store.ID()
	if code, b := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: agent}); code != 200 {
		t.Fatalf("enroll %d %s", code, b)
	}
	read := func() agentwire.DesiredState {
		t.Helper()
		code, b := machine(t, f, "GET", "/agent/v1/desired-state", agent, nil)
		if code != 200 {
			t.Fatalf("desired %d %s", code, b)
		}
		var d agentwire.DesiredState
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	ready := func(d agentwire.DesiredState) {
		t.Helper()
		if code, _ := machine(t, f, "POST", "/agent/v1/heartbeat", agent, agentwire.Heartbeat{AgentVersion: "test", ObservedRevision: d.Revision}); code != 200 {
			t.Fatal(code)
		}
		if code, _ := machine(t, f, "POST", "/agent/v1/applied-state", agent, agentwire.AppliedState{Revision: d.Revision, Status: "applied", InstanceID: store.ID()}); code != 200 {
			t.Fatal(code)
		}
	}
	legacy := read()
	if len(legacy.Bindings[0].Clients) != 1 {
		t.Fatal("development compatibility lost")
	}
	ready(legacy)
	if code, b := subscriptionRead(t, f, token.Token); code != 200 || b == "" {
		t.Fatal("development export missing")
	}
	// An installation upgraded from development may already have live grants and
	// subscription tokens. Production must filter these before a fresh agent pull.
	production := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, RequireVerified: true, PasswordCost: 4}).Handler())
	t.Cleanup(production.Close)
	f.server = production
	if code, b := subscriptionRead(t, f, token.Token); code != 200 || b != "" {
		t.Fatal("production exported unverified legacy credentials")
	}
	for _, path := range []string{"/api/v1/orders", "/api/v1/me/subscription/rotate", "/api/v1/me/proxy-credentials/rotate"} {
		data := "{}"
		if path == "/api/v1/orders" {
			data = `{"plan_id":"explore"}`
		}
		if code, _, _ := request(t, f, "POST", path, data, cookie); code != 403 {
			t.Fatalf("unverified %s: %d", path, code)
		}
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/admin/users", "", admin); code != 403 {
		t.Fatal("unverified admin accepted", code)
	}
	blocked := read()
	if len(blocked.Bindings[0].Clients) != 0 || blocked.Revision <= legacy.Revision {
		t.Fatal("unverified applied grant retained")
	}
	if code, _, _ := request(t, f, "GET", "/api/v1/me", "", cookie); code != 200 {
		t.Fatal("unverified user cannot recover account")
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/me/email-verification", "{}", cookie); code != 202 {
		t.Fatal("verification unavailable", code)
	}
	verify := latestAccountToken(t, f, u.ID, "verify")
	if code, b, _ := request(t, f, "POST", "/api/v1/auth/email/confirm", accountBody(map[string]string{"token": verify}), nil); code != 200 {
		t.Fatalf("confirm %d %s", code, b)
	}
	if code, b, _ := request(t, f, "POST", "/api/v1/orders", `{"plan_id":"explore"}`, cookie); code != 201 {
		t.Fatalf("verified purchase %d %s", code, b)
	}
	verified := read()
	if len(verified.Bindings[0].Clients) != 1 || verified.Revision <= blocked.Revision {
		t.Fatal("verified entitlement did not resume")
	}
	ready(verified)
	if code, b := subscriptionRead(t, f, token.Token); code != 200 || !strings.Contains(b, "vless://") {
		t.Fatal("verified export missing")
	}
}

func TestAuthenticationRateLimitIsSharedAndIgnoresForwardedHeaders(t *testing.T) {
	f := setup(t, true)
	other := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, Development: true, PasswordCost: 4}).Handler())
	t.Cleanup(other.Close)
	for i := 0; i < 21; i++ {
		base := f.server.URL
		if i%2 == 1 {
			base = other.URL
		}
		r, err := http.NewRequest("POST", base+"/api/v1/auth/login", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", store.ID())
		r.Header.Set("X-Real-IP", store.ID())
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := 401
		if i == 20 {
			want = 429
		}
		if resp.StatusCode != want {
			t.Fatalf("attempt %d across instances got %d want %d", i+1, resp.StatusCode, want)
		}
	}
	if _, err := f.db.DB.Exec(context.Background(), `DROP TABLE rate_limit_buckets`); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, f, "POST", "/api/v1/auth/login", "{}", nil); code != 429 {
		t.Fatal("limiter database failure allowed request", code)
	}
}
