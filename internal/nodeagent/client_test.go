package nodeagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wjgppx/xui/internal/nodeagent"
)

func TestRejectsUnsafeControllerAndPublicIdentityFile(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, controller := range []string{"http://public.example", "https://user:password@example.com", "https://example.com/path", "https://example.com?token=secret"} {
		if _, err := nodeagent.New(filepath.Join(t.TempDir(), "id.json"), controller, token); err == nil {
			t.Fatalf("unsafe controller accepted: %s", controller)
		}
	}
	path := filepath.Join(t.TempDir(), "id.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeagent.New(path, "", ""); err == nil {
		t.Fatal("public credentials accepted")
	}
}
func TestEnrollmentResponseLossCanRecover(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	attempts := 0
	credential := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/enroll":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if credential != "" && credential != body["agent_token"] {
				t.Error("credential changed after lost response")
			}
			credential = body["agent_token"]
			attempts++
			if attempts == 1 {
				w.WriteHeader(503)
				return
			}
			fmt.Fprint(w, `{"node_id":"node-1"}`)
		case "/agent/v1/desired-state":
			fmt.Fprint(w, `{"schema_version":1,"node_id":"node-1","revision":1,"bindings":[]}`)
		case "/agent/v1/heartbeat":
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "identity.json")
	c, err := nodeagent.New(path, server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Sync(context.Background()); err == nil {
		t.Fatal("lost response should fail")
	}
	c, err = nodeagent.New(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatal("enrollment did not retry")
	}
}
func TestUnsupportedBindingsAreNotReportedAsApplied(t *testing.T) {
	heartbeats := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/enroll":
			fmt.Fprint(w, `{"node_id":"node-1"}`)
		case "/agent/v1/desired-state":
			fmt.Fprint(w, `{"schema_version":1,"node_id":"node-1","revision":1,"bindings":[{"protocol":"vmess"}]}`)
		case "/agent/v1/heartbeat":
			heartbeats++
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	c, err := nodeagent.New(filepath.Join(t.TempDir(), "identity.json"), server.URL, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Sync(context.Background()); err == nil || heartbeats != 0 || c.State.ObservedRevision != 0 {
		t.Fatal("unsupported state reported as successful")
	}
}
