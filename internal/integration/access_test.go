//go:build xrayintegration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"github.com/wjgppx/xui/internal/xraycore"
)

func TestPurchasedGroupsEventsSubscriptionAndRealConnectionRevocation(t *testing.T) {
	ctx := context.Background()
	db := testutil.Database(t)
	bin := os.Getenv("XUI_XRAY_BIN")
	if bin == "" {
		t.Fatal("XUI_XRAY_BIN required")
	}
	admin, user := testutil.User(t, db), testutil.User(t, db)
	if _, err := db.DB.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	sessions := map[string]string{}
	for _, uid := range []string{admin, user} {
		token := store.ID() + store.ID()
		digest := sha256.Sum256([]byte(token))
		if _, err := db.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, hex.EncodeToString(digest[:]), uid); err != nil {
			t.Fatal(err)
		}
		sessions[uid] = token
	}
	server := httptest.NewServer(httpapi.New(db, httpapi.Config{Origin: "http://test.local", Development: true, PasswordCost: 4}).Handler())
	defer server.Close()
	call := func(method, path, actor string, body any) []byte {
		t.Helper()
		encoded, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(encoded))
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("Content-Type", "application/json")
		if actor != "" {
			r.AddCookie(&http.Cookie{Name: "xui_session", Value: sessions[actor]})
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			t.Fatalf("%s %s status %d", method, path, res.StatusCode)
		}
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	var enrollment struct {
		NodeID string `json:"node_id"`
		Token  string `json:"enrollment_token"`
	}
	json.Unmarshal(call("POST", "/api/v1/admin/nodes", admin, map[string]string{"name": "plan integration"}), &enrollment)
	var otherNode struct {
		NodeID string `json:"node_id"`
	}
	json.Unmarshal(call("POST", "/api/v1/admin/nodes", admin, map[string]string{"name": "other integration"}), &otherNode)
	type group struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	var a, b group
	json.Unmarshal(call("POST", "/api/v1/admin/node-groups", admin, map[string]any{"name": "Purchased group A", "enabled": true, "node_ids": []string{enrollment.NodeID}}), &a)
	json.Unmarshal(call("POST", "/api/v1/admin/node-groups", admin, map[string]any{"name": "Future group B", "enabled": true, "node_ids": []string{otherNode.NodeID}}), &b)
	call("POST", "/api/v1/admin/plans/explore/node-groups", admin, map[string]any{"revision": 1, "group_ids": []string{a.ID}})
	// Capture A now, then change catalog to B before payment. Captured A must win.
	order, err := db.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/v1/admin/plans/explore/node-groups", admin, map[string]any{"revision": 2, "group_ids": []string{b.ID}})
	port := freePort(t)
	definition := agentwire.BindingConfig{ID: "vless", Protocol: "vless", Listen: "127.0.0.1", Port: port, ServerName: "node.test", Certificate: "node", PublicHost: "127.0.0.1", PublicPort: port, AccessMode: "plan", Users: []string{}}
	call("POST", "/api/v1/admin/nodes/"+enrollment.NodeID+"/configuration", admin, map[string]any{"revision": 1, "bindings": []agentwire.BindingConfig{definition}})
	root := t.TempDir()
	certificate(t, root, "node")
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port
	runtime, err := xraycore.New(bin, filepath.Join(root, "runtime"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	runtime.AllowLoopbackEcho(echoPort)
	agent, err := nodeagent.New(filepath.Join(root, "identity.json"), server.URL, enrollment.Token)
	if err != nil {
		t.Fatal(err)
	}
	agent.Runtime = runtime
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var issued struct {
		Token string `json:"token"`
	}
	json.Unmarshal(call("POST", "/api/v1/me/subscription/rotate", user, map[string]any{}), &issued)
	subscription := func(format string) []byte {
		return call("GET", "/api/v1/subscriptions/"+issued.Token+"?format="+format, "", nil)
	}
	if len(subscription("uri")) != 0 {
		t.Fatal("unpaid group user authorized")
	}
	if err = db.Settle(ctx, "test", "group-payment", order.ID, user, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		t.Helper()
		if _, err := db.ProcessOutbox(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	drain()
	var deliveries int
	if err = db.DB.QueryRow(ctx, `SELECT count(*) FROM outbox_deliveries WHERE node_id=$1`, enrollment.NodeID).Scan(&deliveries); err != nil || deliveries == 0 {
		t.Fatal("business events did not target the node", err)
	}
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(subscription("uri")) == 0 {
		t.Fatal("captured purchased group A lost after catalog change")
	}
	config := subscription("xray")
	client := runGeneratedClient(t, "xray", bin, root, config, 0, filepath.Join(root, "node.crt"))
	connect := func() net.Conn {
		t.Helper()
		c, err := socksConnect(client, echoPort)
		if err != nil {
			t.Fatal(err)
		}
		if err = transfer(c); err != nil {
			c.Close()
			t.Fatal(err)
		}
		return c
	}
	active := connect()
	defer active.Close()
	changeGroup := func(enabled bool, members []string) {
		t.Helper()
		json.Unmarshal(call("POST", "/api/v1/admin/node-groups/"+a.ID, admin, map[string]any{"name": "Purchased group A", "enabled": enabled, "node_ids": members, "revision": a.Revision}), &a)
	}
	changeGroup(false, []string{enrollment.NodeID})
	// Authoritative subscription filtering does not wait for a background worker.
	if len(subscription("uri")) != 0 {
		t.Fatal("disabled group published while event pending")
	}
	drain()
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err = transfer(active); err == nil {
		t.Fatal("disabled group kept an existing connection alive")
	}
	changeGroup(true, []string{otherNode.NodeID})
	drain()
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(subscription("uri")) != 0 {
		t.Fatal("removed node still authorized through group")
	}
	changeGroup(true, []string{enrollment.NodeID})
	drain()
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	restored := connect()
	defer restored.Close()
	// The group's permissions are restored, but an individually revoked period
	// must still be excluded from both subscription and the real core.
	var period string
	if err = db.DB.QueryRow(ctx, `SELECT id FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&period); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/v1/admin/entitlements/"+period+"/revoke", admin, map[string]any{})
	if len(subscription("uri")) != 0 {
		t.Fatal("revoked entitlement published before event delivery")
	}
	drain()
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err = transfer(restored); err == nil {
		t.Fatal("revoked period retained a real connection")
	}
	var revision int64
	db.DB.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id=$1`, enrollment.NodeID).Scan(&revision)
	drain()
	var after int64
	db.DB.QueryRow(ctx, `SELECT desired_revision FROM nodes WHERE id=$1`, enrollment.NodeID).Scan(&after)
	if revision != after {
		t.Fatal("processed events were delivered twice")
	}
	t.Log("Purchased group snapshot, event delivery, subscription, group stop/move/restore and period revocation verified with real TLS traffic")
}
