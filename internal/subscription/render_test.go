package subscription_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/subscription"
)

var endpoints = []subscription.Endpoint{
	{Name: `东京 "A" / #1`, Protocol: "vless", Host: "2001:db8::1", Port: 443, ServerName: "node.example.test", Credential: "12345678-1234-1234-1234-123456789abc"},
	{Name: "Trojan / 测试", Protocol: "trojan", Host: "proxy.example.test", Port: 8443, ServerName: "tls.example.test", Credential: "p@ss:/?#% +"},
}

func TestURIsPreserveIPv6TLSAndEscapedCredentials(t *testing.T) {
	raw, typ, err := subscription.Render("uri", endpoints, 0)
	if err != nil {
		t.Fatal(err)
	}
	if typ != "text/plain; charset=utf-8" || !strings.HasSuffix(string(raw), "\n") {
		t.Fatal("URI media type or final newline missing")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatal("URI count")
	}
	for i, line := range lines {
		u, err := url.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		e := endpoints[i]
		if u.Scheme != e.Protocol || u.Hostname() != e.Host || u.User.Username() != e.Credential || u.Fragment != e.Name || u.Query().Get("security") != "tls" || u.Query().Get("type") != "tcp" || u.Query().Get("sni") != e.ServerName || u.Query().Get("alpn") != "http/1.1" {
			t.Fatal("URI changed endpoint values")
		}
		if _, set := u.User.Password(); set {
			t.Fatal("credential colon became a URL password delimiter")
		}
	}
	if !strings.Contains(lines[0], "[2001:db8::1]:443") || !strings.Contains(lines[0], "encryption=none") || !strings.Contains(lines[1], "%40") || !strings.Contains(lines[1], "%3A") {
		t.Fatal("URI delimiters not escaped")
	}
	b64, _, err := subscription.Render("base64", endpoints, 0)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil || string(decoded) != string(raw) {
		t.Fatal("Base64 changed URI payload")
	}
}
func TestStructuredClientsUseTLSAndNoDirectFallback(t *testing.T) {
	for _, format := range []string{"clash", "xray"} {
		raw, typ, err := subscription.Render(format, endpoints, 10808)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err = json.Unmarshal(raw, &config); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"DIRECT"`) || strings.Contains(string(raw), `"allowInsecure": true`) || strings.Contains(string(raw), `"skip-cert-verify": true`) {
			t.Fatal("insecure or direct fallback emitted")
		}
		if format == "clash" {
			if typ != "application/yaml; charset=utf-8" || config["bind-address"] != "127.0.0.1" || config["allow-lan"] != false {
				t.Fatal("Mihomo listener exposed")
			}
			proxies := config["proxies"].([]any)
			vless := proxies[0].(map[string]any)
			trojan := proxies[1].(map[string]any)
			if vless["tls"] != true || vless["skip-cert-verify"] != false || vless["servername"] != endpoints[0].ServerName || trojan["sni"] != endpoints[1].ServerName || trojan["password"] != endpoints[1].Credential {
				t.Fatal("Mihomo TLS credential mapping")
			}
		} else {
			listener := config["inbounds"].([]any)[0].(map[string]any)
			if listener["listen"] != "127.0.0.1" {
				t.Fatal("Xray listener exposed")
			}
			out := config["outbounds"].([]any)
			if len(out) != 2 {
				t.Fatal("Xray endpoint count")
			}
			tls := out[0].(map[string]any)["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
			if tls["allowInsecure"] != false || tls["serverName"] != endpoints[0].ServerName {
				t.Fatal("Xray TLS verification not required")
			}
			route := config["routing"].(map[string]any)["rules"].([]any)[0].(map[string]any)
			if route["outboundTag"] != "proxy-1" {
				t.Fatal("Xray default did not select first endpoint")
			}
		}
	}
	for _, format := range []string{"uri", "base64", "clash", "xray"} {
		raw, _, err := subscription.Render(format, nil, 10808)
		if err != nil {
			t.Fatal(err)
		}
		if format == "uri" || format == "base64" {
			if len(raw) != 0 {
				t.Fatal("empty URI subscription not empty")
			}
			continue
		}
		if strings.Contains(string(raw), `"DIRECT"`) || (format == "clash" && !strings.Contains(string(raw), `"REJECT"`)) || (format == "xray" && !strings.Contains(string(raw), `"blackhole"`)) {
			t.Fatal("empty structured subscription must reject traffic")
		}
	}
}
func TestMalformedEndpointsCannotProduceConfigurations(t *testing.T) {
	mutations := []func(*subscription.Endpoint){
		func(e *subscription.Endpoint) { e.Host = "https://node.example.test/path" },
		func(e *subscription.Endpoint) { e.Host = "0.0.0.0" },
		func(e *subscription.Endpoint) { e.Host = "bad..example" },
		func(e *subscription.Endpoint) { e.ServerName = "node.example.test?insecure=true" },
		func(e *subscription.Endpoint) { e.Credential = "not-a-uuid" },
		func(e *subscription.Endpoint) { e.Protocol = "vmess" },
		func(e *subscription.Endpoint) { e.Port = 0 },
		func(e *subscription.Endpoint) { e.Name = "REJECT" },
		func(e *subscription.Endpoint) { e.Name = "line\ninjection" },
	}
	for _, mutate := range mutations {
		e := endpoints[0]
		mutate(&e)
		if _, _, err := subscription.Render("uri", []subscription.Endpoint{e}, 0); err == nil {
			t.Fatal("invalid endpoint accepted")
		}
	}
	if _, _, err := subscription.Render("clash", []subscription.Endpoint{endpoints[0], endpoints[0]}, 10808); err == nil {
		t.Fatal("duplicate names accepted")
	}
	if _, _, err := subscription.Render("xray", endpoints, 0); err == nil {
		t.Fatal("invalid local listener port accepted")
	}
	if _, _, err := subscription.Render("automatic", endpoints, 10808); err == nil {
		t.Fatal("unimplemented format accepted")
	}
}
