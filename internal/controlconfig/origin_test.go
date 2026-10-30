package controlconfig_test

import (
	"testing"

	"github.com/wjgppx/xui/internal/controlconfig"
)

func TestPublicOriginRejectsNonOriginAndAmbiguousBrowserInputs(t *testing.T) {
	for _, value := range []string{
		"", "https://", "https:example.com", "https://example.com/", "https://example.com/path",
		"https://example.com?", "https://example.com?x=1", "https://example.com#", "https://example.com#fragment",
		"https://user:password@example.com", " https://example.com", "https://example.com\n", "https://example.com\\path",
		"https://example.com:", "https://example.com:0", "https://example.com:65536", "https://example.com:abc",
		"https://[::1", "https://[localhost]", "https://[127.0.0.1]", "https://[fe80::1%25eth0]", "https://example..com", "https://-example.com",
		"https://example-.com", "https://中文.example", "https://127.1", "https://127.000.0.1", "https://2130706433",
		"https://0x7f000001", "https://0xfffffffffffffffffffff", "https://example.123", "https://example.0x1", "https://example.0x", "ftp://example.com",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := controlconfig.PublicOrigin(false, value); err == nil {
				t.Fatal("invalid or browser-ambiguous origin accepted")
			}
		})
	}
	if _, err := controlconfig.PublicOrigin(true, "http://panel.example.com"); err == nil {
		t.Fatal("production accepted insecure HTTP")
	}
}

func TestPublicOriginNormalizesBrowserHostPortAndIPv6WithoutChangingScope(t *testing.T) {
	for input, expected := range map[string]string{
		"HTTPS://PANEL.Example.COM:443":   "https://panel.example.com",
		"https://panel.example.com:00443": "https://panel.example.com",
		"https://panel.example.com:08443": "https://panel.example.com:8443",
		"http://localhost:80":             "http://localhost",
		"http://127.0.0.1:5173":           "http://127.0.0.1:5173",
		"https://[0:0:0:0:0:0:0:1]:443":   "https://[::1]",
		"https://[2001:0DB8::1]:8443":     "https://[2001:db8::1]:8443",
		"https://[::ffff:192.0.2.1]":      "https://[::ffff:c000:201]",
		"https://xn--fiq228c.example":     "https://xn--fiq228c.example",
		"https://panel.example.com.":      "https://panel.example.com.",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := controlconfig.PublicOrigin(false, input)
			if err != nil || got != expected {
				t.Fatal("incorrect browser origin", got, expected, err)
			}
			if got2, err := controlconfig.PublicOrigin(false, got); err != nil || got2 != got {
				t.Fatal("normalized origin was not stable", got2, err)
			}
		})
	}
}
