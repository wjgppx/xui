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
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/agentwire"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/nodeagent"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"github.com/wjgppx/xui/internal/xraycore"
)

func runGeneratedClient(t *testing.T, kind, binary, root string, source []byte, index int, cert string) int {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal(source, &config); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	if kind == "xray" {
		config["inbounds"].([]any)[0].(map[string]any)["port"] = port
		for _, outbound := range config["outbounds"].([]any) {
			tls := outbound.(map[string]any)["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
			tls["certificates"] = []any{map[string]any{"certificateFile": cert, "usage": "verify"}}
		}
		config["routing"].(map[string]any)["rules"].([]any)[0].(map[string]any)["outboundTag"] = "proxy-" + strconv.Itoa(index+1)
	} else {
		config["mixed-port"] = port
		// Exercise each generated proxy while preserving the server/TLS/credential fields.
		name := config["proxies"].([]any)[index].(map[string]any)["name"]
		config["proxy-groups"].([]any)[0].(map[string]any)["proxies"] = []any{name}
	}
	directory := filepath.Join(root, "client-"+store.ID())
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	encoded, _ := json.Marshal(config)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "-config", path}
	checkArgs := []string{"run", "-test", "-config", path}
	if kind == "clash" {
		args = []string{"-d", directory, "-f", path}
		checkArgs = []string{"-t", "-d", directory, "-f", path}
	}
	env := append(os.Environ(), "SSL_CERT_FILE="+cert, "SSL_CERT_DIR="+directory)
	check := exec.Command(binary, checkArgs...)
	check.Env = env
	if err := check.Run(); err != nil {
		t.Fatal(kind, "rejected generated configuration", err)
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	for i := 0; i < 60; i++ {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(kind, "did not listen")
	return 0
}
func TestPublicSubscriptionsWithXrayAndMihomoAndCredentialRotation(t *testing.T) {
	bin := os.Getenv("XUI_XRAY_BIN")
	mihomo := os.Getenv("XUI_MIHOMO_BIN")
	if bin == "" || mihomo == "" {
		t.Fatal("XUI_XRAY_BIN and XUI_MIHOMO_BIN required for subscription compatibility")
	}
	ctx := context.Background()
	db := testutil.Database(t)
	admin, user := testutil.User(t, db), testutil.User(t, db)
	if _, err := db.DB.Exec(ctx, `UPDATE users SET role='admin' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	cookies := map[string]string{}
	for _, id := range []string{admin, user} {
		token := store.ID() + store.ID()
		sum := sha256.Sum256([]byte(token))
		if _, err := db.DB.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '1 hour')`, hex.EncodeToString(sum[:]), id); err != nil {
			t.Fatal(err)
		}
		cookies[id] = token
	}
	server := httptest.NewServer(httpapi.New(db, httpapi.Config{Origin: "http://test.local", Development: true, PasswordCost: 4}).Handler())
	defer server.Close()
	request := func(method, path string, body any, who string) (int, []byte) {
		t.Helper()
		data, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(data))
		r.Header.Set("Origin", "http://test.local")
		r.Header.Set("Content-Type", "application/json")
		if who != "" {
			r.AddCookie(&http.Cookie{Name: "xui_session", Value: cookies[who]})
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, out
	}
	post := func(path string, body any, who string, out any) {
		t.Helper()
		status, data := request("POST", path, body, who)
		if status >= 300 {
			t.Fatalf("POST %s status %d", path, status)
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var enrollment struct {
		NodeID string `json:"node_id"`
		Token  string `json:"enrollment_token"`
	}
	post("/api/v1/admin/nodes", map[string]string{"name": "subscription TLS"}, admin, &enrollment)
	ports := []int{freePort(t), freePort(t)}
	for ports[0] == ports[1] {
		ports[1] = freePort(t)
	}
	definitions := []agentwire.BindingConfig{}
	for i, protocol := range []string{"vless", "trojan"} {
		definitions = append(definitions, agentwire.BindingConfig{ID: protocol, Protocol: protocol, Listen: "127.0.0.1", Port: ports[i], ServerName: "node.test", Certificate: "node", PublicHost: "127.0.0.1", PublicPort: ports[i], Users: []string{user + "@example.test"}})
	}
	post("/api/v1/admin/nodes/"+enrollment.NodeID+"/configuration", map[string]any{"revision": 1, "bindings": definitions}, admin, nil)
	order, err := db.CreateOrder(ctx, user, "explore")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Settle(ctx, "test", "subscription-event", order.ID, user, order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	certificate(t, root, "node")
	certificate(t, root, "untrusted")
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
	var subscription struct {
		Token string `json:"token"`
	}
	post("/api/v1/me/subscription/rotate", map[string]any{}, user, &subscription)
	fetch := func(format string) []byte {
		t.Helper()
		status, data := request("GET", "/api/v1/subscriptions/"+subscription.Token+"?format="+format, nil, "")
		if status != 200 {
			t.Fatal("subscription rejected", status)
		}
		return data
	}
	oldConfigs := map[string][]byte{"xray": fetch("xray"), "clash": fetch("clash")}
	active := []net.Conn{}
	oldClients := []int{}
	for _, kind := range []string{"xray", "clash"} {
		binary := bin
		if kind == "clash" {
			binary = mihomo
		}
		for index := 0; index < 2; index++ {
			port := runGeneratedClient(t, kind, binary, root, oldConfigs[kind], index, filepath.Join(root, "node.crt"))
			oldClients = append(oldClients, port)
			c, err := socksConnect(port, echoPort)
			if err != nil {
				t.Fatal(kind, err)
			}
			defer c.Close()
			if err = transfer(c); err != nil {
				t.Fatal(kind, "TLS generated client transfer", err)
			}
			active = append(active, c)
		}
	}
	badCA := runGeneratedClient(t, "clash", mihomo, root, oldConfigs["clash"], 0, filepath.Join(root, "untrusted.crt"))
	if c, err := socksConnect(badCA, echoPort); err == nil {
		err = transfer(c)
		c.Close()
		if err == nil {
			t.Fatal("Mihomo accepted untrusted TLS certificate")
		}
	}
	post("/api/v1/me/proxy-credentials/rotate", map[string]any{}, user, nil)
	if len(fetch("uri")) != 0 {
		t.Fatal("credentials published before node applied")
	}
	if err = agent.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range active {
		if err = transfer(c); err == nil {
			t.Fatal("old connection survived credential rotation")
		}
	}
	for _, port := range oldClients {
		if c, err := socksConnect(port, echoPort); err == nil {
			err = transfer(c)
			c.Close()
			if err == nil {
				t.Fatal("old imported credentials still connected")
			}
		}
	}
	for _, kind := range []string{"xray", "clash"} {
		binary := bin
		if kind == "clash" {
			binary = mihomo
		}
		fresh := fetch(kind)
		for i := 0; i < 2; i++ {
			port := runGeneratedClient(t, kind, binary, root, fresh, i, filepath.Join(root, "node.crt"))
			c, err := socksConnect(port, echoPort)
			if err != nil {
				t.Fatal(kind, err)
			}
			defer c.Close()
			if err = transfer(c); err != nil {
				t.Fatal(kind, "new credential transfer", err)
			}
		}
	}
	oldToken := subscription.Token
	post("/api/v1/me/subscription/rotate", map[string]any{}, user, &subscription)
	if status, _ := request("GET", "/api/v1/subscriptions/"+oldToken+"?format=uri", nil, ""); status != 401 {
		t.Fatal("old subscription token accepted")
	}
	t.Log("Public Xray and Mihomo configs connected over verified VLESS/Trojan TLS; rotated credentials rejected by both clients")
}
