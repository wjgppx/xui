//go:build xrayintegration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/xraycore"
)

// Compare increments on one established connection so the initial proxy request
// and TLS handshake cannot be confused with application payload accounting.
func TestRealTLSMeterPayloadByteAccuracy(t *testing.T) {
	bin := os.Getenv("XUI_XRAY_BIN")
	if bin == "" {
		t.Fatal("XUI_XRAY_BIN required")
	}
	for _, protocol := range []string{"vless", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			root := t.TempDir()
			certificate(t, root, "node")
			echo, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer echo.Close()
			var connections sync.Map
			defer connections.Range(func(k, _ any) bool { _ = k.(net.Conn).Close(); return true })
			go func() {
				for {
					c, err := echo.Accept()
					if err != nil {
						return
					}
					connections.Store(c, true)
					go func() { defer c.Close(); defer connections.Delete(c); _, _ = io.Copy(c, c) }()
				}
			}()
			runtime, err := xraycore.New(bin, filepath.Join(root, "runtime"), root)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			echoPort := echo.Addr().(*net.TCPAddr).Port
			runtime.AllowLoopbackEcho(echoPort)
			user, period := store.ID(), store.ID()
			credential := "12345678-1234-1234-1234-123456789abc"
			if protocol == "trojan" {
				credential = strings.Repeat("a", 64)
			}
			type count struct{ up, down int64 }
			var latest count
			var instance string
			var startedAt time.Time
			runtime.SetRecorder(func(id string, started, at time.Time, counters []xraycore.TrafficCounter) error {
				if instance != "" && (id != instance || !startedAt.Equal(started)) {
					return fmt.Errorf("runtime identity changed during byte measurement")
				}
				instance, startedAt = id, started
				for _, counter := range counters {
					if counter.BindingID != protocol || counter.UserID != user || counter.EntitlementID != period {
						return fmt.Errorf("unexpected accounting subject")
					}
					if counter.Uplink != nil {
						latest.up = *counter.Uplink
					}
					if counter.Downlink != nil {
						latest.down = *counter.Downlink
					}
				}
				return nil
			})
			port := freePort(t)
			desired := agentwire.DesiredState{SchemaVersion: 1, NodeID: store.ID(), Revision: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Bindings: []agentwire.Binding{{ID: protocol, Protocol: protocol, Listen: "127.0.0.1", Port: port, ServerName: "node.test", Certificate: "node", Clients: []agentwire.ProxyUser{{UserID: user, EntitlementID: period, Credential: credential, ExpiresAt: time.Now().Add(time.Hour)}}}}}
			if _, err = runtime.Apply(context.Background(), desired); err != nil {
				t.Fatal(err)
			}
			socks := startClient(t, bin, root, protocol, port, credential, filepath.Join(root, "node.crt"))
			stream, err := socksConnect(socks, echoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			roundTrip := func(size, seed int) {
				t.Helper()
				if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				payload := make([]byte, size)
				for i := range payload {
					payload[i] = byte(i*31 + seed)
				}
				if _, err := io.Copy(stream, bytes.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
				response := make([]byte, size)
				if _, err := io.ReadFull(stream, response); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(payload, response) {
					t.Fatal("echo payload differs")
				}
			}
			// The complete initial protocol request and response have passed before
			// establishing a quiet, nonzero baseline on this same TLS connection.
			const warmup = 8192
			roundTrip(warmup, 7)
			stable := func(minimum count) count {
				t.Helper()
				previous, equal := count{}, 0
				deadline := time.Now().Add(4 * time.Second)
				for time.Now().Before(deadline) {
					if err := runtime.Sample(context.Background()); err != nil {
						t.Fatal(err)
					}
					if latest.up >= minimum.up && latest.down >= minimum.down && latest == previous {
						equal++
					} else {
						equal = 0
					}
					if equal >= 2 {
						return latest
					}
					previous = latest
					time.Sleep(50 * time.Millisecond)
				}
				t.Fatalf("counters did not stabilize: up=%d down=%d minimum_up=%d minimum_down=%d", latest.up, latest.down, minimum.up, minimum.down)
				return count{}
			}
			baseline := stable(count{up: warmup, down: warmup})
			var payloadBytes int64
			for round := 0; round < 16; round++ {
				for _, size := range []int{1, 31, 1024, 4096, 16384, 65536} {
					roundTrip(size, round)
					payloadBytes += int64(size)
				}
			}
			after := stable(count{up: baseline.up + payloadBytes, down: baseline.down + payloadBytes})
			up, down := after.up-baseline.up, after.down-baseline.down
			t.Logf("protocol=%s rounds=96 payload_per_direction=%d warmup_up=%d warmup_down=%d delta_up=%d delta_down=%d error_up=%d error_down=%d", protocol, payloadBytes, baseline.up, baseline.down, up, down, up-payloadBytes, down-payloadBytes)
			if up != payloadBytes || down != payloadBytes {
				t.Fatalf("application byte accounting mismatch: expected=%d delta_up=%d delta_down=%d", payloadBytes, up, down)
			}
		})
	}
}
