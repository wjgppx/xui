// Package clientip resolves rate-limit identity through explicitly trusted
// proxies. Forwarding headers never establish TLS, origin or authentication.
package clientip

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

var ErrForwarded = errors.New("invalid trusted forwarding chain")

type Policy struct{ trusted []netip.Prefix }

func Parse(value string) (*Policy, error) {
	p := &Policy{}
	if value == "" {
		return p, nil
	}
	items := strings.Split(value, ",")
	if len(items) > 32 {
		return nil, errors.New("too many trusted proxy ranges")
	}
	for _, item := range items {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil || prefix.Bits() == 0 {
			return nil, errors.New("trusted proxies require explicit non-universal CIDRs")
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() <= 96 {
				return nil, errors.New("invalid mapped trusted proxy range")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		p.trusted = append(p.trusted, prefix.Masked())
	}
	return p, nil
}

func (p *Policy) contains(address netip.Addr) bool {
	if p != nil {
		for _, prefix := range p.trusted {
			if prefix.Contains(address) {
				return true
			}
		}
	}
	return false
}

// Resolve ignores all forwarding headers from an untrusted transport peer.
// Trusted X-Forwarded-For chains are walked from the closest hop backwards.
// With no XFF header even a trusted peer remains its own rate-limit identity.
func (p *Policy) Resolve(remote string, headers http.Header) (string, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		// Non-TCP in-process callers share a bounded fallback; arbitrary strings
		// must not become an unlimited family of rate-limit keys.
		return "unknown", nil
	}
	peer = peer.WithZone("").Unmap()
	if !p.contains(peer) {
		return peer.String(), nil
	}
	values := headers.Values("X-Forwarded-For")
	if len(values) == 0 {
		return peer.String(), nil
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 4096 || strings.ContainsAny(values[0], "\r\n\t") {
		return "", ErrForwarded
	}
	parts := strings.Split(values[0], ",")
	if len(parts) > 16 {
		return "", ErrForwarded
	}
	hops := make([]netip.Addr, len(parts))
	for index, part := range parts {
		address, err := netip.ParseAddr(strings.Trim(part, " "))
		if err != nil || address.Zone() != "" {
			return "", ErrForwarded
		}
		hops[index] = address.Unmap()
	}
	for index := len(hops) - 1; index >= 0; index-- {
		if !p.contains(hops[index]) || index == 0 {
			return hops[index].String(), nil
		}
	}
	return peer.String(), nil
}
