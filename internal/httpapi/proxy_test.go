package httpapi_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
)

func TestConfigurationAuthorizationRevisionAndEntitlementTransitions(t *testing.T) {
	f := setup(t, true)
	id, enrollment, admin := adminNode(t, f)
	user, cookie := register(t, f)
	credential := store.ID() + store.ID()
	if code, _ := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: credential}); code != 200 {
		t.Fatal(code)
	}
	path := "/api/v1/admin/nodes/" + id + "/configuration"
	cfg := map[string]any{"revision": 1, "bindings": []agentwire.BindingConfig{{ID: "vless-main", Protocol: "vless", Listen: "127.0.0.1", Port: 18443, ServerName: "node.example.test", Certificate: "node", Users: []string{user.Email}}}}
	data, _ := json.Marshal(cfg)
	if code, _, _ := request(t, f, "POST", path, string(data), cookie); code != 403 {
		t.Fatal("ordinary user configured node", code)
	}
	if code, b, _ := request(t, f, "POST", path, string(data), admin); code != 200 {
		t.Fatalf("configure %d %s", code, b)
	}
	if code, _, _ := request(t, f, "POST", path, string(data), admin); code != 409 {
		t.Fatal("stale write accepted", code)
	}
	read := func() agentwire.DesiredState {
		t.Helper()
		code, b := machine(t, f, "GET", "/agent/v1/desired-state", credential, nil)
		if code != 200 {
			t.Fatalf("snapshot %d %s", code, b)
		}
		var d agentwire.DesiredState
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	pending := read()
	if pending.Revision != 2 || len(pending.Bindings) != 1 || len(pending.Bindings[0].Clients) != 0 {
		t.Fatal("unpaid user authorized", pending.Revision)
	}
	order, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(context.Background(), "test", "event-1", order.ID, user.ID, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	paid := read()
	if paid.Revision != 3 || len(paid.Bindings[0].Clients) != 1 {
		t.Fatal("paid user missing")
	}
	if read().Revision != paid.Revision {
		t.Fatal("lease renewal advanced configuration revision")
	}
	report := agentwire.AppliedState{Revision: paid.Revision, Status: "applied", InstanceID: "test-instance"}
	if code, _ := machine(t, f, "POST", "/agent/v1/applied-state", credential, report); code != 409 {
		t.Fatal("unobserved result accepted")
	}
	if code, _ := machine(t, f, "POST", "/agent/v1/heartbeat", credential, agentwire.Heartbeat{AgentVersion: "test", ObservedRevision: paid.Revision}); code != 200 {
		t.Fatal(code)
	}
	if code, _ := machine(t, f, "POST", "/agent/v1/applied-state", credential, report); code != 200 {
		t.Fatal(code)
	}
	// A renewal queued in the future must not authorize the current exhausted period.
	renewal, err := f.db.CreateOrder(context.Background(), user.ID, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.db.Settle(context.Background(), "test", "event-2", renewal.ID, user.ID, renewal.Price, renewal.Currency); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET used_bytes=quota_bytes WHERE order_id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	exhausted := read()
	if exhausted.Revision != 4 || len(exhausted.Bindings[0].Clients) != 0 {
		t.Fatal("exhausted/future period authorized")
	}
	if code, _ := machine(t, f, "POST", "/agent/v1/applied-state", credential, report); code != 409 {
		t.Fatal("stale applied result accepted")
	}
	// Model the next period becoming current and expiring before the normal lease.
	if _, err = f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET starts_at=now()-interval '1 second',ends_at=now()+interval '20 seconds' WHERE order_id=$1`, renewal.ID); err != nil {
		t.Fatal(err)
	}
	renewed := read()
	if len(renewed.Bindings[0].Clients) != 1 || renewed.Bindings[0].Clients[0].EntitlementID == paid.Bindings[0].Clients[0].EntitlementID {
		t.Fatal("wrong renewal ownership")
	}
	if time.Until(renewed.LeaseExpiresAt) > 21*time.Second {
		t.Fatal("lease extends beyond entitlement")
	}
	if _, err = f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET starts_at=now()-interval '2 seconds',ends_at=now()-interval '1 second' WHERE order_id=$1`, renewal.ID); err != nil {
		t.Fatal(err)
	}
	if len(read().Bindings[0].Clients) != 0 {
		t.Fatal("expired user authorized")
	}
	// An invalid replacement must preserve the previous stored configuration.
	cfg["revision"] = read().Revision
	cfg["bindings"] = []agentwire.BindingConfig{{ID: "bad", Protocol: "vless", Listen: "127.0.0.1", Port: 18443, ServerName: "node.example.test", Certificate: "../private", Users: []string{user.Email}}}
	data, _ = json.Marshal(cfg)
	if code, _, _ := request(t, f, "POST", path, string(data), admin); code != 400 {
		t.Fatal("path traversal accepted")
	}
	if read().Bindings[0].ID != "vless-main" {
		t.Fatal("invalid write changed active configuration")
	}
}
