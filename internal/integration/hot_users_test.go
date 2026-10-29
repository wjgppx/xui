//go:build xrayintegration

package integration_test

import (
	"context"
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

func TestHotAddPreservesRealTLSStreamsCountersAndRevocation(t *testing.T) {
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
			defer connections.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
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
			if err = runtime.EnableHotAdd(); err != nil {
				t.Fatal(err)
			}
			echoPort := echo.Addr().(*net.TCPAddr).Port
			runtime.AllowLoopbackEcho(echoPort)
			user, newUser := store.ID(), store.ID()
			credential, newCredential, rotatedCredential := "12345678-1234-1234-1234-123456789abc", "12345678-1234-1234-1234-123456789abd", "12345678-1234-1234-1234-123456789abe"
			if protocol == "trojan" {
				credential, newCredential, rotatedCredential = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
			}
			type observation struct {
				up, down int64
				instance string
				started  time.Time
			}
			latest := map[string]observation{}
			runtime.SetRecorder(func(instance string, started, at time.Time, counters []xraycore.TrafficCounter) error {
				for _, counter := range counters {
					old := latest[counter.UserID]
					next := observation{instance: instance, started: started}
					if counter.Uplink != nil {
						next.up = *counter.Uplink
					}
					if counter.Downlink != nil {
						next.down = *counter.Downlink
					}
					if old.instance == instance && (next.up < old.up || next.down < old.down) {
						t.Error("hot addition reset cumulative counters")
					}
					latest[counter.UserID] = next
				}
				return nil
			})
			port := freePort(t)
			expires := time.Now().Add(time.Hour)
			desired := agentwire.DesiredState{SchemaVersion: 1, NodeID: store.ID(), Revision: 1, LeaseExpiresAt: time.Now().Add(time.Minute), Bindings: []agentwire.Binding{{ID: protocol, Protocol: protocol, Listen: "127.0.0.1", Port: port, ServerName: "node.test", Certificate: "node", Clients: []agentwire.ProxyUser{{UserID: user, EntitlementID: store.ID(), Credential: credential, ExpiresAt: expires}}}}}
			instance, err := runtime.Apply(context.Background(), desired)
			if err != nil {
				t.Fatal(err)
			}
			_, pid := runtime.IntegrationLeaseProbe()
			socks := startClient(t, bin, root, protocol, port, credential, filepath.Join(root, "node.crt"))
			oldStream, err := socksConnect(socks, echoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer oldStream.Close()
			if err = transfer(oldStream); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 20; i++ {
				if err = runtime.Sample(context.Background()); err != nil {
					t.Fatal(err)
				}
				if latest[user].up > 0 && latest[user].down > 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			before := latest[user]
			if before.up <= 0 || before.down <= 0 {
				t.Fatal("no original traffic counters")
			}
			desired.Revision++
			desired.Bindings[0].Clients = append(desired.Bindings[0].Clients, agentwire.ProxyUser{UserID: newUser, EntitlementID: store.ID(), Credential: newCredential, ExpiresAt: expires})
			hotInstance, err := runtime.Apply(context.Background(), desired)
			if err != nil {
				t.Fatal(err)
			}
			_, afterPID := runtime.IntegrationLeaseProbe()
			if instance != hotInstance || pid != afterPID {
				t.Fatal("add-only update restarted real Xray")
			}
			if err = transfer(oldStream); err != nil {
				t.Fatal("existing TLS stream interrupted by addition", err)
			}
			newSocks := startClient(t, bin, root, protocol, port, newCredential, filepath.Join(root, "node.crt"))
			newStream, err := socksConnect(newSocks, echoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer newStream.Close()
			if err = transfer(newStream); err != nil {
				t.Fatal("added user cannot transfer", err)
			}
			if err = runtime.Sample(context.Background()); err != nil {
				t.Fatal(err)
			}
			if latest[user].instance != before.instance || !latest[user].started.Equal(before.started) || latest[user].up < before.up || latest[user].down < before.down {
				t.Fatal("add changed meter identity or reset totals")
			}
			// Replacing a credential must restart the core and close old streams.
			desired.Revision++
			desired.Bindings[0].Clients[0].Credential = rotatedCredential
			rotatedInstance, err := runtime.Apply(context.Background(), desired)
			if err != nil {
				t.Fatal(err)
			}
			if rotatedInstance == hotInstance {
				t.Fatal("credential change was applied without revoking existing connections")
			}
			if transfer(oldStream) == nil {
				t.Fatal("old credential retained an open proxy stream")
			}
			rotatedSocks := startClient(t, bin, root, protocol, port, rotatedCredential, filepath.Join(root, "node.crt"))
			rotatedStream, err := socksConnect(rotatedSocks, echoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer rotatedStream.Close()
			if err = transfer(rotatedStream); err != nil {
				t.Fatal("rotated credential unavailable", err)
			}
			// Removing this user also requires termination, not RemoveUser alone.
			desired.Revision++
			desired.Bindings[0].Clients = desired.Bindings[0].Clients[1:]
			removedInstance, err := runtime.Apply(context.Background(), desired)
			if err != nil {
				t.Fatal(err)
			}
			if removedInstance == rotatedInstance || transfer(rotatedStream) == nil {
				t.Fatal("removed user kept its established stream")
			}
			denied, err := socksConnect(rotatedSocks, echoPort)
			if err == nil {
				defer denied.Close()
				if transfer(denied) == nil {
					t.Fatal("removed credential opened a new stream")
				}
			}
			allowed, err := socksConnect(newSocks, echoPort)
			if err != nil {
				t.Fatal(err)
			}
			defer allowed.Close()
			if err = transfer(allowed); err != nil {
				t.Fatal("remaining user unavailable after revocation", err)
			}
		})
	}
}
