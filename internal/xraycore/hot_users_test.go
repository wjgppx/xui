package xraycore

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
)

func hotFixture() []agentwire.Binding {
	return []agentwire.Binding{{ID: "vless", Protocol: "vless", Listen: "127.0.0.1", Port: 12345, ServerName: "node.test", Certificate: "node", Clients: []agentwire.ProxyUser{{UserID: "user", EntitlementID: "period", Credential: "12345678-1234-1234-1234-123456789abc", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}}}}
}

func TestHotAdditionsRejectEveryExistingAuthorizationChange(t *testing.T) {
	old := hotFixture()
	next := copyBindings(old)
	next[0].Clients = append(next[0].Clients, agentwire.ProxyUser{UserID: "newuser", EntitlementID: "newperiod", Credential: "12345678-1234-1234-1234-123456789abd", ExpiresAt: old[0].Clients[0].ExpiresAt})
	added, ok := hotAdditions(old, next)
	if !ok || len(added) != 1 || len(added[0].Clients) != 1 || added[0].Clients[0].UserID != "newuser" {
		t.Fatal("add-only update rejected")
	}
	mutations := map[string]func([]agentwire.Binding){
		"credential":  func(b []agentwire.Binding) { b[0].Clients[0].Credential = "replacement" },
		"period":      func(b []agentwire.Binding) { b[0].Clients[0].EntitlementID = "replacement" },
		"expiry":      func(b []agentwire.Binding) { b[0].Clients[0].ExpiresAt = b[0].Clients[0].ExpiresAt.Add(time.Second) },
		"remove":      func(b []agentwire.Binding) { b[0].Clients = b[0].Clients[1:] },
		"protocol":    func(b []agentwire.Binding) { b[0].Protocol = "trojan" },
		"listen":      func(b []agentwire.Binding) { b[0].Listen = "0.0.0.0" },
		"port":        func(b []agentwire.Binding) { b[0].Port++ },
		"server name": func(b []agentwire.Binding) { b[0].ServerName = "other.test" },
		"certificate": func(b []agentwire.Binding) { b[0].Certificate = "other" },
		"duplicate":   func(b []agentwire.Binding) { b[0].Clients = append(b[0].Clients, b[0].Clients[1]) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := copyBindings(next)
			mutate(changed)
			if _, ok := hotAdditions(old, changed); ok {
				t.Fatal("unsafe hot change accepted")
			}
		})
	}
	// The stored snapshot must not alias a controller-owned client slice.
	copy := copyBindings(old)
	copy[0].Clients[0].Credential = "changed"
	if old[0].Clients[0].Credential == "changed" {
		t.Fatal("bindings alias caller data")
	}
}

func TestHotManagementCountStrictness(t *testing.T) {
	for _, data := range []string{`{}`, `{"count":"0"}`, `{"count":0}`, `{"count":"100"}`} {
		if _, err := parseUserCount([]byte(data)); err != nil {
			t.Fatal(data, err)
		}
	}
	for _, data := range []string{`null`, `{"count":null}`, `{"count":-1}`, `{"count":"01"}`, `{"count":"101"}`, `{"count":1.5}`, `{"count":"1","unexpected":true}`, `{} {}`} {
		if _, err := parseUserCount([]byte(data)); err == nil {
			t.Fatal("invalid count accepted", data)
		}
	}
}

func TestHotShapeIncludesTLSMaterialAndEveryNonUserSetting(t *testing.T) {
	config := map[string]any{"inbounds": []any{map[string]any{"tag": "vless", "port": 12345, "settings": map[string]any{"clients": []any{map[string]any{"id": "old"}}, "decryption": "none"}}}, "api": map[string]any{"services": []string{"StatsService", "HandlerService"}}}
	before, err := renderShape(config, []byte("original-certificate-and-private-key"))
	if err != nil {
		t.Fatal(err)
	}
	inbound := config["inbounds"].([]any)[0].(map[string]any)
	inbound["settings"].(map[string]any)["clients"] = []any{map[string]any{"id": "old"}, map[string]any{"id": "new"}}
	added, err := renderShape(config, []byte("original-certificate-and-private-key"))
	if err != nil || added != before {
		t.Fatal("adding a client changed structural shape", err)
	}
	changedKey, err := renderShape(config, []byte("rotated-certificate-or-private-key"))
	if err != nil || changedKey == before {
		t.Fatal("TLS material change missed", err)
	}
	inbound["port"] = 12346
	changedPort, err := renderShape(config, []byte("original-certificate-and-private-key"))
	if err != nil || changedPort == before {
		t.Fatal("listener change missed", err)
	}
}

func hotProcess(t *testing.T, lease time.Time) *Runtime {
	t.Helper()
	cmd := exec.Command("sleep", "15")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{cmd: cmd, done: make(chan error, 1), directory: t.TempDir(), instance: "unchanged", started: time.Now().UTC(), bindings: hotFixture()}
	go func() { r.done <- cmd.Wait() }()
	r.leasedProcess = cmd.Process
	r.leaseLocked(lease)
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.stopLocked()
		r.leaseMu.Lock()
		if r.timer != nil {
			r.timer.Stop()
		}
		r.leaseMu.Unlock()
	})
	return r
}

func TestHotManagementFailureStopsProcessWithoutUnsafeRemoval(t *testing.T) {
	for _, mode := range []string{"false-success", "partial-error", "bad-count"} {
		t.Run(mode, func(t *testing.T) {
			r := hotProcess(t, time.Now().Add(time.Minute))
			next := copyBindings(r.bindings)
			next[0].Clients = append(next[0].Clients, agentwire.ProxyUser{UserID: "newuser", EntitlementID: "newperiod", Credential: "12345678-1234-1234-1234-123456789abd", ExpiresAt: next[0].Clients[0].ExpiresAt})
			calls := []string{}
			r.hotCommand = func(ctx context.Context, op string, args ...string) ([]byte, error) {
				calls = append(calls, op)
				if op == "adu" {
					if mode == "partial-error" {
						return nil, errors.New("partial")
					}
					if filepath.Dir(args[0]) != r.directory {
						t.Fatal("patch outside private directory")
					}
					return []byte("success"), nil
				}
				if mode == "bad-count" {
					return []byte(`{"count":"broken"}`), nil
				}
				return []byte(`{"count":"1"}`), nil
			}
			added, _ := hotAdditions(r.bindings, next)
			if err := r.hotUpdateLocked(context.Background(), agentwire.DesiredState{Bindings: next, LeaseExpiresAt: time.Now().Add(time.Minute)}, added); err == nil {
				t.Fatal("uncertain update accepted")
			}
			if r.aliveLocked() {
				t.Fatal("proxy remained alive after uncertain authorization")
			}
			for _, call := range calls {
				if call != "adu" && call != "inboundusercount" {
					t.Fatal("unsafe rollback operation", call)
				}
			}
			files, err := filepath.Glob(filepath.Join(r.directory, "hot-users-*.json"))
			if err != nil || len(files) != 0 {
				t.Fatal("credential patch not removed", err)
			}
		})
	}
}

func TestHotManagementLeaseExpiresDuringBlockedCall(t *testing.T) {
	lease := time.Now().Add(250 * time.Millisecond)
	r := hotProcess(t, lease)
	r.hotCommand = func(ctx context.Context, op string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	if err := r.hotUpdateLocked(context.Background(), agentwire.DesiredState{Bindings: r.bindings, LeaseExpiresAt: time.Now().Add(time.Minute)}, nil); err == nil {
		t.Fatal("expired update accepted")
	}
	if r.aliveLocked() || time.Since(started) > 2*time.Second {
		t.Fatal("slow management extended authorization lease")
	}
	if r.lease.After(lease) {
		t.Fatal("failed management extended lease")
	}
}

func TestHotManagementSuccessPreservesInstanceAndExtendsLease(t *testing.T) {
	r := hotProcess(t, time.Now().Add(10*time.Second))
	next := copyBindings(r.bindings)
	next[0].Clients = append(next[0].Clients, agentwire.ProxyUser{UserID: "newuser", EntitlementID: "newperiod", Credential: "12345678-1234-1234-1234-123456789abd", ExpiresAt: next[0].Clients[0].ExpiresAt})
	count := "1"
	r.hotCommand = func(ctx context.Context, op string, args ...string) ([]byte, error) {
		if op == "adu" {
			count = "2"
			return nil, nil
		}
		return []byte(`{"count":"` + count + `"}`), nil
	}
	pid, started := r.cmd.Process.Pid, r.started
	until := time.Now().Add(time.Minute)
	added, _ := hotAdditions(r.bindings, next)
	if err := r.hotUpdateLocked(context.Background(), agentwire.DesiredState{Bindings: next, LeaseExpiresAt: until}, added); err != nil {
		t.Fatal(err)
	}
	if !r.aliveLocked() || r.cmd.Process.Pid != pid || r.instance != "unchanged" || !r.started.Equal(started) || !r.lease.Equal(until) {
		t.Fatal("hot update replaced process or failed to commit lease")
	}
}
