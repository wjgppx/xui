//go:build acceptance

package acceptance_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
	"golang.org/x/crypto/bcrypt"
)

// This runs the actual control executable with production policy against an
// isolated schema. SMTP is deliberately unreachable and no mail is enqueued;
// passing cannot establish external delivery, public TLS or merchant readiness.
func TestProductionControlStartupAndPolicy(t *testing.T) {
	s := testutil.Database(t)
	schema := s.DB.Config().ConnConfig.RuntimeParams["search_path"]
	if !strings.HasPrefix(schema, "test_") || len(schema) != 37 {
		t.Fatal("isolated test schema required")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(schema, "test_")); err != nil {
		t.Fatal("invalid isolated schema")
	}
	dsn, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || (dsn.Scheme != "postgres" && dsn.Scheme != "postgresql") || dsn.Host == "" {
		t.Fatal("production process acceptance requires a PostgreSQL test URL")
	}
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("repository unavailable")
	}
	directory := t.TempDir()
	binary, key := filepath.Join(directory, "xui-control"), filepath.Join(directory, "mfa.key")
	if err = os.WriteFile(key, []byte(strings.Repeat("2a", 32)+"\n"), 0600); err != nil {
		t.Fatal("private fixture key unavailable")
	}
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "-ldflags=-X main.releaseID=production-policy-test", "./cmd/control")
	build.Dir, build.Stdout, build.Stderr = root, io.Discard, io.Discard
	if err = build.Run(); err != nil {
		t.Fatal("control acceptance build failed")
	}
	base := map[string]string{
		"APP_MODE": "production", "PUBLIC_ORIGIN": "https://launch.example.test", "DATABASE_URL": dsn.String(),
		"HTTP_ADDR": "127.0.0.1:0", "MFA_KEY_FILE": key, "REQUIRE_STAFF_MFA": "", "TRUSTED_PROXY_CIDRS": "",
		"SMTP_ADDR": "127.0.0.1:1", "SMTP_FROM": "noreply@example.invalid", "SMTP_TLS_SERVER_NAME": "", "SMTP_USER": "", "SMTP_PASSWORD": "",
		"PAYMENT_METHODS": "", "PAYMENT_ACTIONS_ENABLED": "false", "BILL_DOWNLOADS_ENABLED": "false",
	}
	environment := func(overrides map[string]string) []string {
		result := []string{}
		for _, pair := range os.Environ() {
			name, _, _ := strings.Cut(pair, "=")
			if _, overridden := base[name]; !overridden {
				result = append(result, pair)
			}
		}
		for name, value := range base {
			if override, exists := overrides[name]; exists {
				value = override
			}
			result = append(result, name+"="+value)
		}
		return result
	}
	for name, override := range map[string]map[string]string{
		"missing_mfa_key":       {"MFA_KEY_FILE": ""},
		"empty_query_origin":    {"PUBLIC_ORIGIN": "https://launch.example.test?"},
		"empty_fragment_origin": {"PUBLIC_ORIGIN": "https://launch.example.test#"},
		"universal_proxy":       {"TRUSTED_PROXY_CIDRS": "0.0.0.0/0"},
		"missing_smtp":          {"SMTP_ADDR": ""},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary)
			command.Env, command.Stdout, command.Stderr = environment(override), io.Discard, io.Discard
			if err := command.Run(); err == nil || ctx.Err() != nil {
				t.Fatal("invalid production configuration did not promptly fail startup")
			}
		})
	}
	password := "production-test-password"
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal("fixture password unavailable")
	}
	adminID, userID := store.ID(), store.ID()
	if _, err = s.DB.Exec(context.Background(), `INSERT INTO users(id,email,password_hash,role,email_verified_at) VALUES($1,'staff@example.test',$3,'admin',now()),($2,'customer@example.test',$3,'user',NULL)`, adminID, userID, string(passwordHash)); err != nil {
		t.Fatal("production policy fixture unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("local listener unavailable")
	}
	address := listener.Addr().String()
	listener.Close()
	command := exec.Command(binary)
	command.Env, command.Stdout, command.Stderr = environment(map[string]string{"HTTP_ADDR": address}), io.Discard, io.Discard
	if err = command.Start(); err != nil {
		t.Fatal("production process could not start")
	}
	transport := &http.Transport{}
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-finished:
			if err != nil {
				t.Error("production process did not exit gracefully")
			}
		// main allows ten seconds for HTTP drain. The test must let that
		// deadline complete before classifying a graceful exit as a failure.
		case <-time.After(15 * time.Second):
			_ = command.Process.Kill()
			<-finished
			t.Error("production process shutdown timed out")
		}
	})
	request := func(method, path string, body any, cookie *http.Cookie) (int, []byte, []*http.Cookie) {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			if err != nil {
				t.Fatal("fixture encoding failed")
			}
		}
		req, _ := http.NewRequest(method, "http://"+address+path, bytes.NewReader(encoded))
		if body != nil {
			req.Header.Set("Origin", base["PUBLIC_ORIGIN"])
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("production fixture request failed")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil {
			t.Fatal("production fixture response failed")
		}
		return response.StatusCode, data, response.Cookies()
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		response, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("production fixture did not become ready")
		}
		time.Sleep(25 * time.Millisecond)
	}
	code, data, _ := request("GET", "/readyz", nil, nil)
	var ready struct {
		Release string `json:"release"`
		Schema  int    `json:"schema_version"`
	}
	if code != 200 || json.Unmarshal(data, &ready) != nil || ready.Release != "production-policy-test" || ready.Schema != 16 {
		t.Fatal("production release identity mismatch")
	}
	code, data, _ = request("GET", "/api/v1/config", nil, nil)
	var config map[string]bool
	if code != 200 || json.Unmarshal(data, &config) != nil || config["development_payments"] || config["development_mail"] || !config["email_verification_required"] || config["payment_actions_enabled"] || config["bill_downloads_enabled"] {
		t.Fatal("unsafe production feature policy")
	}
	login := func(email string) *http.Cookie {
		t.Helper()
		code, _, cookies := request("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": password}, nil)
		if code != 200 || len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
			t.Fatal("unsafe production login cookie")
		}
		return cookies[0]
	}
	staffCookie, customerCookie := login("staff@example.test"), login("customer@example.test")
	code, data, _ = request("GET", "/api/v1/admin/users", nil, staffCookie)
	if code != 403 || !bytes.Contains(data, []byte("mfa_enrollment_required")) {
		t.Fatal("production staff bypassed mandatory MFA enrollment")
	}
	code, data, _ = request("GET", "/api/v1/me/mfa", nil, staffCookie)
	var factor struct{ Available, Required, Enabled bool }
	if code != 200 || json.Unmarshal(data, &factor) != nil || !factor.Available || !factor.Required || factor.Enabled {
		t.Fatal("production staff enrollment unavailable")
	}
	for _, path := range []string{"/api/v1/me/development-mail", "/api/v1/dev/orders/unused/settle"} {
		method, body := "GET", any(nil)
		if strings.Contains(path, "/dev/orders/") {
			method, body = "POST", map[string]any{}
		}
		if code, _, _ := request(method, path, body, staffCookie); code != 404 {
			t.Fatal("development endpoint exposed in production")
		}
	}
	if code, _, _ := request("POST", "/api/v1/orders", map[string]string{"plan_id": "unused"}, customerCookie); code != 403 {
		t.Fatal("unverified customer bypassed production order policy")
	}
	if code, data, _ := request("GET", "/api/v1/payment-methods", nil, nil); code != 200 || strings.TrimSpace(string(data)) != "[]" {
		t.Fatal("unconfigured real payment methods became available")
	}
	var count int
	if err = s.DB.QueryRow(context.Background(), `SELECT count(*) FROM mail_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatal("production smoke test unexpectedly enqueued mail")
	}
}
