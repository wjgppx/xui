//go:build xrayintegration

package integration_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/usageledger"
	"github.com/wjgppx/xui/internal/xraycore"
)

func TestShutdownFinalSampleSurvivesLedgerReopen(t *testing.T) {
	bin := os.Getenv("XUI_XRAY_BIN")
	if bin == "" {
		t.Fatal("XUI_XRAY_BIN required")
	}
	root := t.TempDir()
	certificate(t, root, "node")
	path := filepath.Join(root, "usage", "ledger.sqlite")
	ledger, err := usageledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if ledger != nil {
			ledger.Close()
		}
	}()
	if binder, ok := any(ledger).(interface{ BindIdentity(string) error }); ok {
		if err = binder.BindIdentity("fixture-controller\x00fixture-node"); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := xraycore.New(bin, filepath.Join(root, "runtime"), root)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
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
	runtime.AllowLoopbackEcho(echoPort)
	runtime.SetRecorder(func(instance string, start, at time.Time, counters []xraycore.TrafficCounter) error {
		observations := make([]usageledger.Counter, 0, len(counters))
		for _, v := range counters {
			observations = append(observations, usageledger.Counter{BindingID: v.BindingID, UserID: v.UserID, EntitlementID: v.EntitlementID, Uplink: v.Uplink, Downlink: v.Downlink})
		}
		return ledger.Sample(instance, start, at, observations)
	})
	port := freePort(t)
	credential := "12345678-1234-1234-1234-123456789abc"
	user, period := store.ID(), store.ID()
	d := agentwire.DesiredState{SchemaVersion: 1, NodeID: store.ID(), Revision: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Bindings: []agentwire.Binding{{ID: "vless", Protocol: "vless", Listen: "127.0.0.1", Port: port, ServerName: "node.test", Certificate: "node", Clients: []agentwire.ProxyUser{{UserID: user, EntitlementID: period, Credential: credential, ExpiresAt: time.Now().Add(time.Hour)}}}}}
	if _, err = runtime.Apply(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	socks := startClient(t, bin, root, "vless", port, credential, filepath.Join(root, "node.crt"))
	c, err := socksConnect(socks, echoPort)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = transfer(c); err != nil {
		t.Fatal(err)
	}
	pending, err := ledger.Pending(10)
	if err != nil || len(pending) != 0 {
		t.Fatal("unexpected periodic sample in final-only fixture")
	}
	if err = runtime.Close(); err != nil {
		t.Fatal("final sampling failed", err)
	}
	pending, err = ledger.Pending(10)
	if err != nil || len(pending) != 1 {
		t.Fatal("final sample not durably queued", err)
	}
	original := pending[0]
	if len(original.Items) != 1 || original.Items[0].UplinkBytes <= 0 || original.Items[0].DownlinkBytes <= 0 {
		t.Fatal("final traffic attribution missing")
	}
	if err = ledger.Close(); err != nil {
		t.Fatal(err)
	}
	ledger = nil
	ledger, err = usageledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = ledger.Pending(10)
	if err != nil || len(pending) != 1 || pending[0].BatchID != original.BatchID {
		t.Fatal("restart changed unsent batch")
	}
	digest, _ := agentwire.UsageDigest(original)
	if err = ledger.Ack(original.BatchID, digest); err != nil {
		t.Fatal(err)
	}
}
