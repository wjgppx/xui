package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/store"
)

func adminNode(t *testing.T, f fixture) (string, string, *http.Cookie) {
	t.Helper()
	u, c := register(t, f)
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	status, b, _ := request(t, f, "POST", "/api/v1/admin/nodes", `{"name":"测试节点"}`, c)
	if status != 201 {
		t.Fatalf("create node %d %s", status, b)
	}
	var result struct {
		NodeID string `json:"node_id"`
		Token  string `json:"enrollment_token"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return result.NodeID, result.Token, c
}
func machine(t *testing.T, f fixture, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	result, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, result
}
func TestNodeEnrollmentRetryIdentityScopeAndRevocation(t *testing.T) {
	f := setup(t, true)
	nodeID, enrollment, c := adminNode(t, f)
	credential := store.ID() + store.ID()
	body := agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: credential}
	for i := 0; i < 2; i++ {
		status, b := machine(t, f, "POST", "/agent/v1/enroll", "", body)
		if status != 200 {
			t.Fatalf("enroll %d %s", status, b)
		}
		var response agentwire.EnrollmentResponse
		_ = json.Unmarshal(b, &response)
		if response.NodeID != nodeID {
			t.Fatal("wrong identity")
		}
	}
	body.AgentToken = store.ID() + store.ID()
	status, _ := machine(t, f, "POST", "/agent/v1/enroll", "", body)
	if status != 409 {
		t.Fatal("enrollment reused by a second agent")
	}
	status, b := machine(t, f, "GET", "/agent/v1/desired-state", credential, nil)
	if status != 200 || !strings.Contains(string(b), nodeID) {
		t.Fatal("agent cannot read own desired state")
	}
	status, _ = machine(t, f, "GET", "/api/v1/admin/nodes", credential, nil)
	if status != 401 {
		t.Fatal("agent credential grants administrator access")
	}
	status, _, _ = request(t, f, "GET", "/agent/v1/desired-state", "", c)
	if status != 401 {
		t.Fatal("web cookie grants node access")
	}
	status, _, _ = request(t, f, "POST", "/api/v1/admin/nodes/"+nodeID+"/revoke", "{}", c)
	if status != 200 {
		t.Fatal("revocation failed")
	}
	status, _ = machine(t, f, "GET", "/agent/v1/desired-state", credential, nil)
	if status != 401 {
		t.Fatal("revoked credential accepted")
	}
}
func TestExpiredEnrollmentAndHeartbeatRevision(t *testing.T) {
	f := setup(t, true)
	id, enrollment, _ := adminNode(t, f)
	credential := store.ID() + store.ID()
	status, _ := machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: credential})
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = machine(t, f, "POST", "/agent/v1/heartbeat", credential, agentwire.Heartbeat{AgentVersion: "test", ObservedRevision: 1})
	if status != 200 {
		t.Fatal("valid heartbeat failed")
	}
	for _, revision := range []int64{0, 2} {
		status, _ = machine(t, f, "POST", "/agent/v1/heartbeat", credential, agentwire.Heartbeat{AgentVersion: "test", ObservedRevision: revision})
		if status != 409 {
			t.Fatalf("invalid revision %d accepted: %d", revision, status)
		}
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE nodes SET enrollment_expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status, _ = machine(t, f, "POST", "/agent/v1/enroll", "", agentwire.EnrollmentRequest{EnrollmentToken: enrollment, AgentToken: credential})
	if status != 401 {
		t.Fatal("expired bootstrap accepted")
	}
}
func TestAgentRestartAndRotation(t *testing.T) {
	f := setup(t, true)
	id, enrollment, c := adminNode(t, f)
	path := filepath.Join(t.TempDir(), "identity.json")
	client, err := nodeagent.New(path, f.server.URL, enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := nodeagent.New(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if restored.State.NodeID != id {
		t.Fatal("node identity not durable")
	}
	if restored.State.EnrollmentToken != "" {
		t.Fatal("bootstrap retained after success")
	}
	if err = restored.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, b, _ := request(t, f, "GET", "/api/v1/admin/nodes", "", c)
	if status != 200 || strings.Contains(string(b), enrollment) || strings.Contains(string(b), client.State.AgentToken) || !strings.Contains(string(b), `"observed_revision":1`) {
		t.Fatal("node list leaked secrets or missed heartbeat")
	}
	status, _, _ = request(t, f, "POST", "/api/v1/admin/nodes/"+id+"/enrollment", "{}", c)
	if status != 200 {
		t.Fatal("rotation failed")
	}
	if err = restored.Sync(context.Background()); err != nodeagent.ErrIdentity {
		t.Fatalf("old identity survived rotation: %v", err)
	}
}
func TestOrdinaryUsersCannotManageNodes(t *testing.T) {
	f := setup(t, true)
	_, c := register(t, f)
	for _, method := range []string{"GET", "POST"} {
		status, _, _ := request(t, f, method, "/api/v1/admin/nodes", `{"name":"forbidden"}`, c)
		if status != 403 {
			t.Fatal("unprivileged node management")
		}
	}
}
