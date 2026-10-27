package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
)

type subscriptionToken struct {
	Token string            `json:"token"`
	URLs  map[string]string `json:"urls"`
}

func issueSubscription(t *testing.T, f fixture, cookie *http.Cookie) subscriptionToken {
	t.Helper()
	code, data, _ := request(t, f, "POST", "/api/v1/me/subscription/rotate", "{}", cookie)
	if code != 200 {
		t.Fatalf("issue subscription %d", code)
	}
	var value subscriptionToken
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func subscriptionRead(t *testing.T, f fixture, token string) (int, string) {
	t.Helper()
	code, data, _ := request(t, f, "GET", "/api/v1/subscriptions/"+token+"?format=uri", "", nil)
	return code, string(data)
}
func TestSubscriptionTokenHashRotationRevocationAndSessionIsolation(t *testing.T) {
	f := setup(t, true)
	user, cookie := register(t, f)
	_, otherCookie := register(t, f)
	code, _, _ := request(t, f, "POST", "/api/v1/me/subscription/rotate", "{}", nil)
	if code != 401 {
		t.Fatal("anonymous token creation")
	}
	first := issueSubscription(t, f, cookie)
	var stored string
	if err := f.db.DB.QueryRow(context.Background(), `SELECT token_hash FROM subscriptions WHERE user_id=$1`, user.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(first.Token))
	if stored == first.Token || stored != hex.EncodeToString(digest[:]) {
		t.Fatal("subscription token not hashed")
	}
	for _, link := range first.URLs {
		if !strings.HasPrefix(link, origin+"/api/v1/subscriptions/") {
			t.Fatal("untrusted subscription origin")
		}
	}
	if code, body := subscriptionRead(t, f, first.Token); code != 200 || body != "" {
		t.Fatal("unpaid subscription not empty")
	}
	code, data, _ := request(t, f, "GET", "/api/v1/me/subscription", "", cookie)
	if code != 200 || strings.Contains(string(data), first.Token) || strings.Contains(string(data), stored) {
		t.Fatal("status leaks secret")
	}
	request(t, f, "POST", "/api/v1/me/subscription/revoke", "{}", otherCookie)
	if code, _ = subscriptionRead(t, f, first.Token); code != 200 {
		t.Fatal("other user revoked subscription")
	}
	second := issueSubscription(t, f, cookie)
	if first.Token == second.Token {
		t.Fatal("token not changed")
	}
	if code, _ = subscriptionRead(t, f, first.Token); code != 401 {
		t.Fatal("old token accepted")
	}
	request(t, f, "POST", "/api/v1/me/subscription/revoke", "{}", cookie)
	if code, _ = subscriptionRead(t, f, second.Token); code != 401 {
		t.Fatal("revoked token accepted")
	}
}
func TestPublishedSubscriptionAuthorizationAndProxyCredentialRotation(t *testing.T) {
	f := setup(t, true)
	nodeID, enrollment, admin := adminNode(t, f)
	user, cookie := register(t, f)
	other, otherCookie := register(t, f)
	cfg := map[string]any{"revision": 1, "bindings": []agentwire.BindingConfig{
		{ID: "a-vless", Protocol: "vless", Listen: "0.0.0.0", Port: 18443, ServerName: "node.example.test", Certificate: "node", PublicHost: "127.0.0.1", PublicPort: 443, Users: []string{user.Email}},
		{ID: "b-trojan", Protocol: "trojan", Listen: "0.0.0.0", Port: 18444, ServerName: "node.example.test", Certificate: "node", PublicHost: "node.example.test", PublicPort: 444, Users: []string{user.Email}},
		{ID: "c-unpublished", Protocol: "vless", Listen: "0.0.0.0", Port: 18445, ServerName: "node.example.test", Certificate: "node", Users: []string{user.Email, other.Email}},
	}}
	data, _ := json.Marshal(cfg)
	if code, b, _ := request(t, f, "POST", "/api/v1/admin/nodes/"+nodeID+"/configuration", string(data), admin); code != 200 {
		t.Fatalf("configure %d %s", code, b)
	}
	credential := store.ID() + store.ID()
	if code, _ := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: credential}); code != 200 {
		t.Fatal(code)
	}
	ready := func() {
		t.Helper()
		code, b := machine(t, f, "GET", "/agent/v1/desired-state", credential, nil)
		if code != 200 {
			t.Fatal(code)
		}
		var d agentwire.DesiredState
		json.Unmarshal(b, &d)
		if code, _ = machine(t, f, "POST", "/agent/v1/heartbeat", credential, agentwire.Heartbeat{AgentVersion: "test", ObservedRevision: d.Revision}); code != 200 {
			t.Fatal(code)
		}
		if code, _ = machine(t, f, "POST", "/agent/v1/applied-state", credential, agentwire.AppliedState{Revision: d.Revision, Status: "applied", InstanceID: store.ID()}); code != 200 {
			t.Fatal(code)
		}
	}
	first := issueSubscription(t, f, cookie)
	ready()
	if code, body := subscriptionRead(t, f, first.Token); code != 200 || body != "" {
		t.Fatal("unpaid user subscribed")
	}
	for _, uid := range []string{user.ID, other.ID} {
		order, err := f.db.CreateOrder(context.Background(), uid, "explore")
		if err != nil {
			t.Fatal(err)
		}
		if err = f.db.Settle(context.Background(), "test", store.ID(), order.ID, uid, order.Price, order.Currency); err != nil {
			t.Fatal(err)
		}
	}
	ready()
	var oldUUID, oldSecret, otherUUID string
	f.db.DB.QueryRow(context.Background(), `SELECT credential_id,secret FROM node_grants WHERE node_id=$1 AND user_id=$2`, nodeID, user.ID).Scan(&oldUUID, &oldSecret)
	f.db.DB.QueryRow(context.Background(), `SELECT credential_id FROM node_grants WHERE node_id=$1 AND user_id=$2`, nodeID, other.ID).Scan(&otherUUID)
	code, body := subscriptionRead(t, f, first.Token)
	if code != 200 || strings.Count(strings.TrimSpace(body), "\n") != 1 || !strings.Contains(body, oldUUID) || !strings.Contains(body, oldSecret) || strings.Contains(body, "c-unpublished") {
		t.Fatal("published subscription content incorrect")
	}
	secondUser := issueSubscription(t, f, otherCookie)
	if _, value := subscriptionRead(t, f, secondUser.Token); value != "" {
		t.Fatal("other user got published credentials")
	}
	code, _, _ = request(t, f, "POST", "/api/v1/me/proxy-credentials/rotate", "{}", cookie)
	if code != 200 {
		t.Fatal(code)
	}
	if _, value := subscriptionRead(t, f, first.Token); value != "" {
		t.Fatal("pending credentials published before application")
	}
	ready()
	_, value := subscriptionRead(t, f, first.Token)
	if value == "" || strings.Contains(value, oldUUID) || strings.Contains(value, oldSecret) {
		t.Fatal("proxy credentials not rotated")
	}
	var unchanged string
	f.db.DB.QueryRow(context.Background(), `SELECT credential_id FROM node_grants WHERE node_id=$1 AND user_id=$2`, nodeID, other.ID).Scan(&unchanged)
	if unchanged != otherUUID {
		t.Fatal("rotated another user's credential")
	}
	// Stale node status must not publish an apparently live configuration.
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE nodes SET last_seen_at=$2 WHERE id=$1`, nodeID, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, value = subscriptionRead(t, f, first.Token); value != "" {
		t.Fatal("stale node published")
	}
	ready()
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET used_bytes=quota_bytes WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, value = subscriptionRead(t, f, first.Token); value != "" {
		t.Fatal("exhausted subscription published")
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE entitlement_periods SET used_bytes=0,starts_at=now()-interval '2 seconds',ends_at=now()-interval '1 second' WHERE user_id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, value = subscriptionRead(t, f, first.Token); value != "" {
		t.Fatal("expired subscription published")
	}
}

func TestConcurrentSubscriptionRotationLeavesOnlyOneUsableToken(t *testing.T) {
	f := setup(t, true)
	_, cookie := register(t, f)
	results := make(chan subscriptionToken, 6)
	for i := 0; i < 6; i++ {
		go func() {
			var result subscriptionToken
			defer func() { results <- result }()
			result = issueSubscription(t, f, cookie)
		}()
	}
	tokens := make([]subscriptionToken, 0, 6)
	for i := 0; i < 6; i++ {
		tokens = append(tokens, <-results)
	}
	active := 0
	for _, token := range tokens {
		code, _ := subscriptionRead(t, f, token.Token)
		if code == 200 {
			active++
		} else if code != 401 {
			t.Fatal("unexpected subscription status", code)
		}
	}
	if active != 1 {
		t.Fatal("concurrent rotation left multiple or no valid tokens", active)
	}
}
