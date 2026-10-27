// Package subscription encodes independently designed client subscriptions from
// already authorized endpoints. It does not decide account or node permissions.
package subscription

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Endpoint struct {
	Name, Protocol, Host   string
	Port                   int
	ServerName, Credential string
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Render produces URI lines, their standard Base64 encoding, a Mihomo config
// (JSON is valid YAML 1.2), or an Xray client config. Xray uses the first endpoint
// by default while retaining the rest as separately tagged outbounds. Empty
// structured configurations reject traffic and never fall back to direct access.
func Render(format string, endpoints []Endpoint, socksPort int) ([]byte, string, error) {
	switch format {
	case "uri", "base64", "clash", "xray":
	default:
		return nil, "", errors.New("unsupported subscription format")
	}
	if (format == "clash" || format == "xray") && (socksPort < 1024 || socksPort > 65535) {
		return nil, "", errors.New("client port must be between 1024 and 65535")
	}
	if len(endpoints) > 800 {
		return nil, "", errors.New("too many subscription endpoints")
	}
	names := map[string]bool{}
	for _, e := range endpoints {
		if err := validate(e); err != nil {
			return nil, "", err
		}
		if names[e.Name] {
			return nil, "", errors.New("subscription endpoint names must be unique")
		}
		names[e.Name] = true
	}
	if format == "uri" || format == "base64" {
		lines := make([]string, 0, len(endpoints))
		for _, e := range endpoints {
			lines = append(lines, encodeURI(e))
		}
		text := strings.Join(lines, "\n")
		if len(lines) > 0 {
			text += "\n"
		}
		if format == "base64" {
			text = base64.StdEncoding.EncodeToString([]byte(text))
		}
		return []byte(text), "text/plain; charset=utf-8", nil
	}
	var config map[string]any
	contentType := "application/json; charset=utf-8"
	if format == "clash" {
		config = mihomo(endpoints, socksPort)
		contentType = "application/yaml; charset=utf-8"
	} else {
		config = xray(endpoints, socksPort)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, "", errors.New("subscription configuration encoding failed")
	}
	return append(data, '\n'), contentType, nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 || host != strings.TrimSpace(host) {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsUnspecified() && !ip.IsMulticast()
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}
func validate(e Endpoint) error {
	if e.Name == "" || len(e.Name) > 240 || !utf8.ValidString(e.Name) {
		return errors.New("invalid endpoint name")
	}
	for _, r := range e.Name {
		if unicode.IsControl(r) {
			return errors.New("invalid endpoint name")
		}
	}
	if !validHost(e.Host) || !validHost(e.ServerName) || e.Port < 1 || e.Port > 65535 {
		return errors.New("invalid endpoint address or TLS name")
	}
	if e.Protocol != "vless" && e.Protocol != "trojan" {
		return errors.New("unsupported endpoint protocol")
	}
	if e.Protocol == "vless" && !uuidPattern.MatchString(e.Credential) {
		return errors.New("invalid VLESS credential")
	}
	if e.Protocol == "trojan" {
		if e.Credential == "" || len(e.Credential) > 256 || !utf8.ValidString(e.Credential) {
			return errors.New("invalid Trojan credential")
		}
		for _, r := range e.Credential {
			if unicode.IsControl(r) {
				return errors.New("invalid Trojan credential")
			}
		}
	}
	// Built-in proxy/group names cannot be used as user-controlled selectors.
	if e.Name == "XUI" || e.Name == "DIRECT" || e.Name == "REJECT" || e.Name == "GLOBAL" {
		return errors.New("reserved endpoint name")
	}
	return nil
}
func encodeURI(e Endpoint) string {
	query := url.Values{"security": {"tls"}, "type": {"tcp"}, "sni": {e.ServerName}, "alpn": {"http/1.1"}}
	if e.Protocol == "vless" {
		query.Set("encryption", "none")
	}
	u := url.URL{Scheme: e.Protocol, User: url.User(e.Credential), Host: net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), RawQuery: query.Encode(), Fragment: e.Name}
	return u.String()
}

func mihomo(endpoints []Endpoint, port int) map[string]any {
	proxies := []any{}
	names := []string{}
	for _, e := range endpoints {
		proxy := map[string]any{"name": e.Name, "type": e.Protocol, "server": e.Host, "port": e.Port, "network": "tcp", "udp": false, "skip-cert-verify": false, "alpn": []string{"http/1.1"}}
		if e.Protocol == "vless" {
			proxy["uuid"] = e.Credential
			proxy["tls"] = true
			proxy["servername"] = e.ServerName
		} else {
			proxy["password"] = e.Credential
			proxy["sni"] = e.ServerName
		}
		proxies = append(proxies, proxy)
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		names = []string{"REJECT"}
	}
	return map[string]any{"mixed-port": port, "allow-lan": false, "bind-address": "127.0.0.1", "mode": "rule", "log-level": "warning", "proxies": proxies, "proxy-groups": []any{map[string]any{"name": "XUI", "type": "select", "proxies": names}}, "rules": []string{"MATCH,XUI"}}
}
func xray(endpoints []Endpoint, port int) map[string]any {
	outbounds := []any{}
	for i, e := range endpoints {
		settings := map[string]any{}
		if e.Protocol == "vless" {
			settings["vnext"] = []any{map[string]any{"address": e.Host, "port": e.Port, "users": []any{map[string]any{"id": e.Credential, "encryption": "none"}}}}
		} else {
			settings["servers"] = []any{map[string]any{"address": e.Host, "port": e.Port, "password": e.Credential}}
		}
		outbounds = append(outbounds, map[string]any{"tag": "proxy-" + strconv.Itoa(i+1), "protocol": e.Protocol, "settings": settings, "streamSettings": map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"serverName": e.ServerName, "allowInsecure": false, "alpn": []string{"http/1.1"}}}})
	}
	first := "proxy-1"
	if len(outbounds) == 0 {
		first = "reject"
		outbounds = append(outbounds, map[string]any{"tag": first, "protocol": "blackhole", "settings": map[string]any{}})
	}
	return map[string]any{"log": map[string]any{"loglevel": "warning"}, "inbounds": []any{map[string]any{"tag": "local-socks", "listen": "127.0.0.1", "port": port, "protocol": "socks", "settings": map[string]any{"auth": "noauth", "udp": false}}}, "outbounds": outbounds, "routing": map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": []string{"local-socks"}, "outboundTag": first}}}}
}
