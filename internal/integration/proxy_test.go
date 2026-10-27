//go:build xrayintegration

package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"github.com/wjgppx/xui/internal/usageledger"
	"github.com/wjgppx/xui/internal/xraycore"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func certificate(t *testing.T, root, name string) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "node.test"}, DNSNames: []string{"node.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, e := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	data, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), 0600); e != nil {
		t.Fatal(e)
	}
}
func startClient(t *testing.T, bin, root, protocol string, port int, credential, trust string) int {
	t.Helper()
	socks := freePort(t)
	settings := map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": port, "users": []any{map[string]any{"id": credential, "encryption": "none"}}}}}
	if protocol == "trojan" {
		settings = map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": port, "password": credential}}}
	}
	cfg := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": socks, "protocol": "socks", "settings": map[string]any{"auth": "noauth", "udp": false}}}, "outbounds": []any{map[string]any{"protocol": protocol, "settings": settings, "streamSettings": map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"serverName": "node.test", "certificates": []any{map[string]any{"certificateFile": trust, "usage": "verify"}}}}}}}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(root, "client-"+store.ID()+".json")
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(bin, "run", "-config", path)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	for i := 0; i < 50; i++ {
		c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socks)), 100*time.Millisecond)
		if e == nil {
			c.Close()
			return socks
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("client did not start")
	return 0
}
func socksConnect(port, target int) (net.Conn, error) {
	c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if e != nil {
		return nil, e
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	_, e = c.Write([]byte{5, 1, 0})
	if e != nil {
		c.Close()
		return nil, e
	}
	b := make([]byte, 2)
	if _, e = io.ReadFull(c, b); e != nil {
		c.Close()
		return nil, e
	}
	if !bytes.Equal(b, []byte{5, 0}) {
		c.Close()
		return nil, io.ErrUnexpectedEOF
	}
	req := []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 0}
	binary.BigEndian.PutUint16(req[8:], uint16(target))
	c.Write(req)
	header := make([]byte, 4)
	if _, e = io.ReadFull(c, header); e != nil {
		c.Close()
		return nil, e
	}
	if header[1] != 0 {
		c.Close()
		return nil, io.ErrUnexpectedEOF
	}
	size := 4
	if header[3] == 4 {
		size = 16
	} else if header[3] == 3 {
		n := make([]byte, 1)
		io.ReadFull(c, n)
		size = int(n[0])
	}
	if _, e = io.ReadFull(c, make([]byte, size+2)); e != nil {
		c.Close()
		return nil, e
	}
	return c, nil
}
func transfer(c net.Conn) error {
	c.SetDeadline(time.Now().Add(2 * time.Second))
	payload := bytes.Repeat([]byte("independent-xui-tls-test"), 1024)
	if _, e := c.Write(payload); e != nil {
		return e
	}
	out := make([]byte, len(payload))
	if _, e := io.ReadFull(c, out); e != nil {
		return e
	}
	if !bytes.Equal(payload, out) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func TestPaidUserToVLESSTrojanTLSAndRuntimeFailureRecovery(t *testing.T) {
	bin := os.Getenv("XUI_XRAY_BIN")
	if bin == "" {
		t.Fatal("XUI_XRAY_BIN required for real-core tests")
	}
	ctx := context.Background()
	db := testutil.Database(t)
	adminID := testutil.User(t, db)
	userID := testutil.User(t, db)
	session := store.ID() + store.ID()
	digest := sha256.Sum256([]byte(session))
	if _, e := db.DB.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, adminID); e != nil {
		t.Fatal(e)
	}
	if _, e := db.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, hex.EncodeToString(digest[:]), adminID); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(httpapi.New(db, httpapi.Config{Origin: "http://test.local", Development: true, PasswordCost: 4}).Handler())
	defer server.Close()
	request := func(method, path string, body any, token string, out any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		req.Header.Set("Origin", "http://test.local")
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.AddCookie(&http.Cookie{Name: "xui_session", Value: session})
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			t.Fatalf("%s %s: %d", method, path, res.StatusCode)
		}
		if out != nil {
			if e = json.NewDecoder(res.Body).Decode(out); e != nil {
				t.Fatal(e)
			}
		}
	}
	var enrollment struct {
		NodeID string `json:"node_id"`
		Token  string `json:"enrollment_token"`
	}
	request("POST", "/api/v1/admin/nodes", map[string]string{"name": "TLS test"}, "", &enrollment)
	ports := []int{freePort(t), freePort(t)}
	for ports[0] == ports[1] {
		ports[1] = freePort(t)
	}
	definitions := []agentwire.BindingConfig{}
	for i, p := range []string{"vless", "trojan"} {
		definitions = append(definitions, agentwire.BindingConfig{ID: p, Protocol: p, Listen: "127.0.0.1", Port: ports[i], ServerName: "node.test", Certificate: "node", Users: []string{userID + "@example.test"}})
	}
	var revision int64 = 1
	configure := func(bindings []agentwire.BindingConfig) {
		t.Helper()
		var result struct {
			Revision int64 `json:"revision"`
		}
		request("POST", "/api/v1/admin/nodes/"+enrollment.NodeID+"/configuration", map[string]any{"revision": revision, "bindings": bindings}, "", &result)
		revision = result.Revision
	}
	configure(definitions)
	order, e := db.CreateOrder(ctx, userID, "explore")
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Settle(ctx, "test", "paid-tls", order.ID, userID, order.Price, order.Currency); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	ledger, e := usageledger.Open(filepath.Join(root, "usage", "ledger.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer ledger.Close()
	certificate(t, root, "node")
	certificate(t, root, "untrusted")
	echo, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	var connections sync.Map
	defer connections.Range(func(k, v any) bool { k.(net.Conn).Close(); return true })
	go func() {
		for {
			c, e := echo.Accept()
			if e != nil {
				return
			}
			connections.Store(c, true)
			go func() { defer c.Close(); defer connections.Delete(c); io.Copy(c, c) }()
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port
	runtime, e := xraycore.New(bin, filepath.Join(root, "runtime"), root)
	if e != nil {
		t.Fatal(e)
	}
	defer runtime.Close()
	runtime.AllowLoopbackEcho(echoPort)
	if duplicate, e := xraycore.New(bin, filepath.Join(root, "runtime"), root); e == nil {
		duplicate.Close()
		t.Fatal("duplicate process ownership accepted")
	}
	agent, e := nodeagent.New(filepath.Join(root, "identity.json"), server.URL, enrollment.Token)
	if e != nil {
		t.Fatal(e)
	}
	agent.Runtime = runtime
	agent.Outbox = ledger
	var meterBroken atomic.Bool
	runtime.SetRecorder(func(instance string, start, at time.Time, observations []xraycore.TrafficCounter) error {
		if meterBroken.Load() {
			return io.ErrClosedPipe
		}
		counters := make([]usageledger.Counter, 0, len(observations))
		for _, v := range observations {
			counters = append(counters, usageledger.Counter{BindingID: v.BindingID, UserID: v.UserID, EntitlementID: v.EntitlementID, Uplink: v.Uplink, Downlink: v.Downlink})
		}
		return ledger.Sample(instance, start, at, counters)
	})
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("initial application", e)
	}
	snapshot := func() agentwire.DesiredState {
		t.Helper()
		var d agentwire.DesiredState
		request("GET", "/agent/v1/desired-state", nil, agent.State.AgentToken, &d)
		revision = d.Revision
		return d
	}
	desired := snapshot()
	var instance string
	if e = db.DB.QueryRow(ctx, `SELECT core_instance_id FROM nodes WHERE id=$1 AND apply_status='applied' AND applied_revision=desired_revision`, enrollment.NodeID).Scan(&instance); e != nil {
		t.Fatal("application not confirmed", e)
	}
	clients := map[string]int{}
	active := map[string]net.Conn{}
	for _, b := range desired.Bindings {
		if len(b.Clients) != 1 {
			t.Fatal("entitled user not authorized")
		}
		port := startClient(t, bin, root, b.Protocol, b.Port, b.Clients[0].Credential, filepath.Join(root, "node.crt"))
		clients[b.Protocol] = port
		c, e := socksConnect(port, echoPort)
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		if e = transfer(c); e != nil {
			t.Fatal(b.Protocol, "TLS transfer failed", e)
		}
		active[b.Protocol] = c
		wrong := strings.Repeat("a", 64)
		if b.Protocol == "vless" {
			wrong = "00000000-0000-0000-0000-000000000001"
		}
		badPort := startClient(t, bin, root, b.Protocol, b.Port, wrong, filepath.Join(root, "node.crt"))
		if c, e := socksConnect(badPort, echoPort); e == nil {
			e = transfer(c)
			c.Close()
			if e == nil {
				t.Fatal("invalid proxy credential accepted")
			}
		}
		untrustedPort := startClient(t, bin, root, b.Protocol, b.Port, b.Clients[0].Credential, filepath.Join(root, "untrusted.crt"))
		if c, e := socksConnect(untrustedPort, echoPort); e == nil {
			e = transfer(c)
			c.Close()
			if e == nil {
				t.Fatal("TLS trust verification bypassed")
			}
		}
	}
	lost := &loseUsageAck{base: http.DefaultTransport}
	agent.HTTP.Transport = lost
	if e = agent.Sync(ctx); e == nil || !lost.lost {
		t.Fatal("lost usage ack was not simulated")
	}
	queued, e := ledger.Pending(100)
	if e != nil || len(queued) == 0 {
		t.Fatal("lost ack discarded durable evidence")
	}
	var chargedBeforeRetry int64
	if e = db.DB.QueryRow(ctx, `SELECT used_bytes FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&chargedBeforeRetry); e != nil {
		t.Fatal(e)
	}
	agent.HTTP.Transport = http.DefaultTransport
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("usage retry failed", e)
	}
	var chargedAfterRetry int64
	if e = db.DB.QueryRow(ctx, `SELECT used_bytes FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&chargedAfterRetry); e != nil {
		t.Fatal(e)
	}
	if chargedBeforeRetry <= 0 || chargedAfterRetry != chargedBeforeRetry {
		t.Fatal("lost ack retry duplicated charge", chargedBeforeRetry, chargedAfterRetry)
	}
	var renewedInstance string
	db.DB.QueryRow(ctx, `SELECT core_instance_id FROM nodes WHERE id=$1`, enrollment.NodeID).Scan(&renewedInstance)
	if instance != renewedInstance {
		t.Fatal("unchanged config restarted Xray")
	}
	// Invalid local certificate fails before stopping the existing service.
	definitions[0].Certificate = "missing"
	configure(definitions)
	if e = agent.Sync(ctx); e == nil {
		t.Fatal("missing certificate accepted")
	}
	var status string
	var applied int64
	db.DB.QueryRow(ctx, `SELECT apply_status,applied_revision FROM nodes WHERE id=$1`, enrollment.NodeID).Scan(&status, &applied)
	if status != "failed" || applied >= revision {
		t.Fatal("failed replacement marked applied")
	}
	if e = transfer(active["vless"]); e != nil {
		t.Fatal("validation failure interrupted valid old process", e)
	}
	definitions[0].Certificate = "node"
	configure(definitions)
	if e = agent.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	// A valid config with an occupied replacement port fails at start, then
	// restarts the last working config under its old lease with a new instance ID.
	collision, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer collision.Close()
	originalPort := definitions[0].Port
	definitions[0].Port = collision.Addr().(*net.TCPAddr).Port
	configure(definitions)
	if e = agent.Sync(ctx); e == nil {
		t.Fatal("occupied port reported as applied")
	}
	if c, e := socksConnect(clients["vless"], echoPort); e == nil {
		defer c.Close()
		if e = transfer(c); e != nil {
			t.Fatal("rollback failed", e)
		}
	} else {
		t.Fatal(e)
	}
	definitions[0].Port = originalPort
	configure(definitions)
	if e = agent.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	db.DB.QueryRow(ctx, `SELECT core_instance_id FROM nodes WHERE id=$1`, enrollment.NodeID).Scan(&renewedInstance)
	if renewedInstance == instance {
		t.Fatal("rollback reused the previous process instance identity")
	}
	// Reopen connections after the intentional process restart for removal checks.
	for protocol, port := range clients {
		c, e := socksConnect(port, echoPort)
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		if e = transfer(c); e != nil {
			t.Fatal(e)
		}
		active[protocol] = c
	}
	// Remove authorization; replacing the process also closes existing sessions.
	for i := range definitions {
		definitions[i].Users = []string{}
	}
	configure(definitions)
	if e = agent.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	for protocol, c := range active {
		if e = transfer(c); e == nil {
			t.Fatal(protocol, "old session survived managed replacement")
		}
		if c, e := socksConnect(clients[protocol], echoPort); e == nil {
			e = transfer(c)
			c.Close()
			if e == nil {
				t.Fatal(protocol, "removed user still authorized")
			}
		}
	}
	for i := range definitions {
		definitions[i].Users = []string{userID + "@example.test"}
	}
	configure(definitions)
	if e = agent.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	// A short test lease proves the autonomous timer, with no further controller calls.
	desired = snapshot()
	desired.LeaseExpiresAt = time.Now().Add(750 * time.Millisecond)
	if _, e = runtime.Apply(ctx, desired); e != nil {
		t.Fatal(e)
	}
	c, e := socksConnect(clients["vless"], echoPort)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = transfer(c); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Second)
	if e = transfer(c); e == nil {
		t.Fatal("lease expiry left a proxy connection alive")
	}
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("lease recovery failed", e)
	}
	if c, e := socksConnect(clients["trojan"], echoPort); e == nil {
		defer c.Close()
		if e = transfer(c); e != nil {
			t.Fatal("runtime did not restart after lease expiry", e)
		}
	} else {
		t.Fatal(e)
	}
	// Exact server balance equals the immutable ledger, grouped by both bindings.
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("meter delivery", e)
	}
	var used, total int64
	var bindings int
	if e = db.DB.QueryRow(ctx, `SELECT used_bytes FROM entitlement_periods WHERE order_id=$1`, order.ID).Scan(&used); e != nil {
		t.Fatal(e)
	}
	if e = db.DB.QueryRow(ctx, `SELECT COALESCE(sum(uplink_bytes+downlink_bytes),0),count(DISTINCT binding_id) FROM usage_items WHERE entitlement_period_id=(SELECT id FROM entitlement_periods WHERE order_id=$1)`, order.ID).Scan(&total, &bindings); e != nil {
		t.Fatal(e)
	}
	if used <= 0 || used != total || bindings != 2 {
		t.Fatalf("billing mismatch used=%d ledger=%d bindings=%d", used, total, bindings)
	}
	// Let one more payload exceed the remaining byte, then reconcile authorization.
	if _, e = db.DB.Exec(ctx, `UPDATE entitlement_periods SET quota_bytes=used_bytes+1 WHERE order_id=$1`, order.ID); e != nil {
		t.Fatal(e)
	}
	connection, e := socksConnect(clients["vless"], echoPort)
	if e != nil {
		t.Fatal(e)
	}
	defer connection.Close()
	if e = transfer(connection); e != nil {
		t.Fatal(e)
	}
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("quota reconciliation", e)
	}
	exhausted := snapshot()
	for _, b := range exhausted.Bindings {
		if len(b.Clients) != 0 {
			t.Fatal("exhausted entitlement still authorized")
		}
	}
	if e = transfer(connection); e == nil {
		t.Fatal("quota exhaustion left existing connection alive")
	}
	meterBroken.Store(true)
	if e = runtime.Sample(ctx); e == nil {
		t.Fatal("storage failure did not fail closed")
	}
	for _, port := range ports {
		if c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond); e == nil {
			c.Close()
			t.Fatal("storage failure left proxy listener alive")
		}
	}
	meterBroken.Store(false)
	if e = agent.Sync(ctx); e != nil {
		t.Fatal("meter recovery failed", e)
	}
	// Empty desired state is a deliberate stop, including listener teardown.
	configure([]agentwire.BindingConfig{})
	if e = agent.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	for _, port := range ports {
		if c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond); e == nil {
			c.Close()
			t.Fatal("empty configuration left listener active")
		}
	}
	t.Log("VLESS/Trojan TLS, credential and CA rejection, replacement, lease expiry and restart passed")
}

func TestLeaseExpiryContinuesDuringSlowConfigurationValidation(t *testing.T) {
	bin := os.Getenv("XUI_XRAY_BIN")
	if bin == "" {
		t.Fatal("XUI_XRAY_BIN required")
	}
	root := t.TempDir()
	certificate(t, root, "node")
	marker := filepath.Join(root, "delay-next-validation")
	wrapper := filepath.Join(root, "xray-wrapper")
	script := `#!/bin/sh
if [ "$2" = "-test" ] && [ -f "$XUI_TEST_DELAY_MARKER" ]; then
 rm "$XUI_TEST_DELAY_MARKER"
 sleep 5
fi
exec "$XUI_TEST_REAL_XRAY" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XUI_TEST_DELAY_MARKER", marker)
	t.Setenv("XUI_TEST_REAL_XRAY", bin)
	runtime, err := xraycore.New(wrapper, filepath.Join(root, "runtime"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	oldPort := freePort(t)
	desired := agentwire.DesiredState{SchemaVersion: 1, NodeID: "test", Revision: 1, LeaseExpiresAt: time.Now().Add(2 * time.Second), Bindings: []agentwire.Binding{{ID: "vless", Protocol: "vless", Listen: "127.0.0.1", Port: oldPort, ServerName: "node.test", Certificate: "node", Clients: []agentwire.ProxyUser{}}}}
	oldLease := desired.LeaseExpiresAt
	if _, err = runtime.Apply(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	desired.Bindings[0].Port = freePort(t)
	desired.Revision++
	desired.LeaseExpiresAt = time.Now().Add(time.Minute)
	if err = os.WriteFile(marker, []byte("delay"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runtime.Apply(context.Background(), desired); done <- err }()
	// Observe eventual socket teardown within one second of the lease; this
	// allows OS process cleanup while still detecting a five-second mutex stall.
	time.Sleep(time.Until(oldLease))
	closed := false
	deadline := oldLease.Add(time.Second)
	for time.Now().Before(deadline) {
		c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)), 100*time.Millisecond)
		if e != nil {
			closed = true
			break
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	if !closed {
		lease, pid := runtime.IntegrationLeaseProbe()
		t.Errorf("old lease survived blocked validation: original=%v active=%v now=%v pid=%d", oldLease, lease, time.Now(), pid)
	}
	// Validation has a five second deadline, so either a rejection or a new
	// authorized process is valid here; an old listener surviving is not.
	<-done
}
