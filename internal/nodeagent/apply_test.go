package nodeagent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/xraycore"
)

type brokenRuntime struct{}

func (brokenRuntime) Apply(context.Context, agentwire.DesiredState) (string, error) {
	return "", &xraycore.Failure{Code: "config_rejected"}
}
func TestFailedApplicationReportsObservedButNeverApplied(t *testing.T) {
	var reports []agentwire.AppliedState
	heartbeat := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agent/v1/enroll":
			json.NewEncoder(w).Encode(agentwire.EnrollmentResponse{NodeID: "test-node"})
		case "/agent/v1/desired-state":
			json.NewEncoder(w).Encode(agentwire.DesiredState{SchemaVersion: 1, NodeID: "test-node", Revision: 2, Bindings: []agentwire.Binding{}, LeaseExpiresAt: time.Now().Add(time.Minute)})
		case "/agent/v1/heartbeat":
			heartbeat = true
			w.Write([]byte(`{"ok":true}`))
		case "/agent/v1/applied-state":
			var report agentwire.AppliedState
			json.NewDecoder(r.Body).Decode(&report)
			reports = append(reports, report)
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()
	c, err := nodeagent.New(filepath.Join(t.TempDir(), "identity.json"), server.URL, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	c.Runtime = brokenRuntime{}
	if err = c.Sync(context.Background()); err == nil {
		t.Fatal("apply failure hidden")
	}
	if !heartbeat || c.State.ObservedRevision != 2 || len(reports) != 1 || reports[0].Status != "failed" || reports[0].ErrorCode != "config_rejected" || reports[0].InstanceID != "" {
		t.Fatal("failed apply misreported", reports)
	}
}
