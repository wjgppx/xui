package agentwire

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

var NamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,47}$`)
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)

func ValidateBinding(b Binding) error {
	if b.ID == "xui-meter" || !NamePattern.MatchString(b.ID) || !NamePattern.MatchString(b.Certificate) {
		return errors.New("invalid binding or certificate reference")
	}
	if b.Protocol != "vless" && b.Protocol != "trojan" {
		return errors.New("unsupported protocol")
	}
	if net.ParseIP(b.Listen) == nil || b.Port < 1024 || b.Port > 65535 {
		return errors.New("literal listen IP and unprivileged port required")
	}
	if !hostPattern.MatchString(b.ServerName) || strings.Contains(b.ServerName, "..") {
		return errors.New("invalid TLS server name")
	}
	return nil
}
