package controlconfig

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var ErrPublicOrigin = errors.New("PUBLIC_ORIGIN must be one HTTP(S) origin with an ASCII/Punycode host and a valid optional port; production requires HTTPS")

// PublicOrigin rejects URL components which an HTTP Origin cannot contain and
// normalizes the host and default port to the browser's serialized form. This
// same value is used for CSRF checks, mail links and payment return URLs.
func PublicOrigin(production bool, value string) (string, error) {
	if len(value) > 4096 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "#\\") {
		return "", ErrPublicOrigin
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrPublicOrigin
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || (production && scheme != "https") {
		return "", ErrPublicOrigin
	}
	host := strings.ToLower(u.Hostname())
	if strings.ContainsAny(host, "%") || host == "" || strings.HasSuffix(u.Host, ":") {
		return "", ErrPublicOrigin
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Is4() && strings.ContainsAny(u.Host, "[]") {
			return "", ErrPublicOrigin
		}
		host = address.String()
		if address.Is4In6() {
			// Go prints mapped IPv6 with a dotted tail; browser URL serialization
			// uses two hexadecimal groups instead.
			bytes := address.As16()
			host = fmt.Sprintf("::ffff:%x:%x", binary.BigEndian.Uint16(bytes[12:14]), binary.BigEndian.Uint16(bytes[14:16]))
		}
		if address.Is6() {
			host = "[" + host + "]"
		}
	} else {
		if strings.ContainsAny(u.Host, "[]") || !originDNSHost(host) {
			return "", ErrPublicOrigin
		}
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", ErrPublicOrigin
		}
		if !((scheme == "https" && number == 443) || (scheme == "http" && number == 80)) {
			host += ":" + strconv.Itoa(number)
		}
	}
	return scheme + "://" + host, nil
}

func originDNSHost(host string) bool {
	// A trailing DNS root dot is meaningful to browser origins and is retained.
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	// WHATWG treats hosts ending in an IPv4 number as IP input, including
	// shorthand/octal/hex forms. Require canonical dotted IPs instead of allowing
	// the browser and Go to silently derive different Origin values.
	last := labels[len(labels)-1]
	digits := true
	for _, character := range last {
		digits = digits && character >= '0' && character <= '9'
	}
	if digits {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		hex := true
		for _, character := range strings.TrimPrefix(last, "0x") {
			hex = hex && ((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f'))
		}
		if hex {
			return false
		}
	}
	return true
}
