package clientip_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/clientip"
)

func TestTrustedProxyScopeSelectsClosestUntrustedHopWithoutTrustingOtherHeaders(t *testing.T) {
	p, err := clientip.Parse("127.0.0.1/32,10.0.0.0/24,::1/128")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ peer, chain, want string }{
		{"192.0.2.1:8080", "203.0.113.9", "192.0.2.1"},
		{"127.0.0.1:8080", "203.0.113.9, 10.0.0.5", "203.0.113.9"},
		{"127.0.0.1:8080", "203.0.113.9, 198.51.100.7", "198.51.100.7"},
		{"[::1]:8080", "::ffff:192.0.2.5, 10.0.0.5", "192.0.2.5"},
		{"[::ffff:192.0.2.5]:8080", "203.0.113.9", "192.0.2.5"},
		{"127.0.0.1:8080", "10.0.0.1, 10.0.0.5", "10.0.0.1"},
	} {
		headers := http.Header{"X-Forwarded-For": []string{tc.chain}, "Forwarded": []string{"for=1.2.3.4"}, "X-Real-Ip": []string{"1.2.3.4"}}
		actual, err := p.Resolve(tc.peer, headers)
		if err != nil || actual != tc.want {
			t.Fatal(tc.peer, actual, tc.want, err)
		}
	}
	actual, err := p.Resolve("127.0.0.1:8080", http.Header{"X-Real-Ip": []string{"1.2.3.4"}})
	if err != nil || actual != "127.0.0.1" {
		t.Fatal("non-XFF header was trusted", actual, err)
	}
	var none *clientip.Policy
	actual, err = none.Resolve("127.0.0.1:1", http.Header{"X-Forwarded-For": []string{"bad-header"}})
	if err != nil || actual != "127.0.0.1" {
		t.Fatal("default trusted forwarded input", actual, err)
	}
}

func TestMalformedTrustedChainsFailClosedAndUntrustedHeadersAreIgnored(t *testing.T) {
	p, _ := clientip.Parse("127.0.0.1/32")
	for _, value := range []string{"", "unknown", "192.0.2.1:1234", "192.0.2.1,,192.0.2.2", "fe80::1%eth0", "192.0.2.1\n", strings.Repeat("1", 4097), strings.Repeat("192.0.2.1,", 16) + "192.0.2.1"} {
		headers := http.Header{"X-Forwarded-For": []string{value}}
		if _, err := p.Resolve("127.0.0.1:1", headers); err == nil {
			t.Fatal("bad trusted chain accepted")
		}
		if address, err := p.Resolve("192.0.2.1:1", headers); err != nil || address != "192.0.2.1" {
			t.Fatal("untrusted header changed identity", address, err)
		}
	}
	if _, err := p.Resolve("127.0.0.1:1", http.Header{"X-Forwarded-For": []string{"192.0.2.1", "192.0.2.2"}}); err == nil {
		t.Fatal("multiple XFF lines accepted")
	}
}

func TestProxyConfigurationRejectsUniversalOrMalformedTrustAndNormalizesMappedNetworks(t *testing.T) {
	for _, value := range []string{"0.0.0.0/0", "::/0", "127.0.0.1", ",", "127.0.0.1/32,", "::ffff:10.0.0.0/96", strings.Repeat("127.0.0.1/32,", 32) + "::1/128"} {
		if _, err := clientip.Parse(value); err == nil {
			t.Fatal("bad proxy policy accepted", value)
		}
	}
	p, err := clientip.Parse("::ffff:127.0.0.1/128")
	if err != nil {
		t.Fatal(err)
	}
	if address, err := p.Resolve("127.0.0.1:1", http.Header{"X-Forwarded-For": []string{"192.0.2.4"}}); err != nil || address != "192.0.2.4" {
		t.Fatal("mapped trust range not normalized", address, err)
	}
}
