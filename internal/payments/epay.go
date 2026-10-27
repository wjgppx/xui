package payments

import (
	"context"
	"crypto/md5" // Classic Epay protocol requires MD5; not used for account passwords.
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// EpayConfig targets the common submit.php / MD5 protocol, not every gateway
// using the Epay name. HTTPClient is reserved for a future authenticated query
// protocol; this adapter intentionally performs no unsigned api.php query.
type EpayConfig struct {
	PID, Key, Gateway, Type string
	HTTPClient              *http.Client
}

type epayProvider struct {
	pid, key, kind, submit string
}

func NewEpay(c EpayConfig) (Provider, error) {
	u, err := url.Parse(c.Gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.Gateway, "#") || u.Opaque != "" {
		return nil, errors.New("invalid Epay HTTPS gateway")
	}
	if !epayDigits(c.PID) || len(c.PID) > 32 || c.Key == "" || len(c.Key) > 1024 || strings.ContainsAny(c.Key, "\r\n") || (c.Type != "alipay" && c.Type != "wxpay") {
		return nil, errors.New("invalid Epay merchant configuration")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/submit.php"
	u.RawPath = ""
	return &epayProvider{pid: c.PID, key: c.Key, kind: c.Type, submit: u.String()}, nil
}

func (p *epayProvider) ID() string {
	if p.kind == "wxpay" {
		return "epay_wechat"
	}
	return "epay_alipay"
}
func (p *epayProvider) Name() string {
	if p.kind == "wxpay" {
		return "微信支付（易支付）"
	}
	return "支付宝（易支付）"
}
func epayDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func epayID(s string, length int) bool {
	if len(s) < 1 || len(s) > length {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func epayPaymentID(s string) bool {
	if len(s) != 32 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func epayReturnURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Fragment == "" && !strings.Contains(s, "#") && u.Opaque == ""
}

func epaySign(values url.Values, key string) [16]byte {
	keys := make([]string, 0, len(values))
	for name := range values {
		if name != "sign" && name != "sign_type" && values.Get(name) != "" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, name := range keys {
		parts = append(parts, name+"="+values.Get(name))
	}
	return md5.Sum([]byte(strings.Join(parts, "&") + key))
}

func (p *epayProvider) Create(ctx context.Context, r Request) (Checkout, error) {
	if err := ctx.Err(); err != nil {
		return Checkout{}, err
	}
	if !epayPaymentID(r.PaymentID) || r.Amount <= 0 || r.Currency != "CNY" || r.Name == "" || len(r.Name) > 256 || strings.ContainsAny(r.Name, "\r\n") || !epayReturnURL(r.NotifyURL) || !epayReturnURL(r.ReturnURL) {
		return Checkout{}, ErrGateway
	}
	v := url.Values{
		"pid": {p.pid}, "type": {p.kind}, "out_trade_no": {r.PaymentID},
		"notify_url": {r.NotifyURL}, "return_url": {r.ReturnURL}, "name": {r.Name},
		"money": {strconv.FormatInt(r.Amount/100, 10) + "." + epayCents(r.Amount%100)},
	}
	sum := epaySign(v, p.key)
	v.Set("sign", hex.EncodeToString(sum[:]))
	v.Set("sign_type", "MD5")
	return Checkout{Kind: "redirect", URL: p.submit + "?" + v.Encode()}, nil
}
func epayCents(cents int64) string {
	if cents < 10 {
		return "0" + strconv.FormatInt(cents, 10)
	}
	return strconv.FormatInt(cents, 10)
}

// Decimal input is parsed as integer minor units. Exponents, signs, whitespace,
// sub-cent precision and overflow are rejected rather than rounded.
func epayAmount(value string) (int64, bool) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || !epayDigits(parts[0]) || len(parts[0]) > 17 {
		return 0, false
	}
	fraction := "00"
	if len(parts) == 2 {
		if len(parts[1]) < 1 || len(parts[1]) > 2 || !epayDigits(parts[1]) {
			return 0, false
		}
		fraction = parts[1]
		if len(fraction) == 1 {
			fraction += "0"
		}
	}
	amount, err := strconv.ParseInt(parts[0]+fraction, 10, 64)
	return amount, err == nil && amount > 0
}

func epayCallbackValues(r *http.Request) (url.Values, error) {
	var raw string
	switch r.Method {
	case http.MethodGet:
		raw = r.URL.RawQuery
	case http.MethodPost:
		// Combining query and body values can make the signed interpretation differ
		// from the caller's interpretation. Accept one canonical source only.
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/x-www-form-urlencoded" || r.URL.RawQuery != "" || r.Body == nil {
			return nil, ErrVerification
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(b) > 65536 {
			return nil, ErrVerification
		}
		raw = string(b)
	default:
		return nil, ErrVerification
	}
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, ErrVerification
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(values) > 100 {
		return nil, ErrVerification
	}
	for name, list := range values {
		if !epayID(name, 64) || len(list) != 1 || strings.ContainsAny(list[0], "\r\n") {
			return nil, ErrVerification
		}
	}
	return values, nil
}
func (p *epayProvider) Verify(r *http.Request) (Notification, error) {
	v, err := epayCallbackValues(r)
	if err != nil {
		return Notification{}, ErrVerification
	}
	got, err := hex.DecodeString(v.Get("sign"))
	want := epaySign(v, p.key)
	if err != nil || len(got) != len(want) || subtle.ConstantTimeCompare(got, want[:]) != 1 || v.Get("sign_type") != "MD5" {
		return Notification{}, ErrVerification
	}
	if v.Get("pid") != p.pid || v.Get("type") != p.kind || !epayPaymentID(v.Get("out_trade_no")) || !epayID(v.Get("trade_no"), 128) {
		return Notification{}, ErrVerification
	}
	amount, ok := epayAmount(v.Get("money"))
	if !ok || v.Get("trade_status") == "" {
		return Notification{}, ErrVerification
	}
	return Notification{PaymentID: v.Get("out_trade_no"), TransactionID: v.Get("trade_no"), Amount: amount, Currency: "CNY", Paid: v.Get("trade_status") == "TRADE_SUCCESS"}, nil
}

// Classic api.php returns unsigned JSON and commonly places the merchant key in
// the query string. It cannot establish a financial fact. Do not make that call
// or return a paid result; only a signed notify callback can settle this adapter.
func (p *epayProvider) Query(context.Context, Request) (Notification, error) {
	return Notification{}, ErrVerification
}
