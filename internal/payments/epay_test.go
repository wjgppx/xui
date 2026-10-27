package payments

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

const epayTestKey = "merchant-test-key-1234567890"
const epayTestPayment = "0123456789abcdef0123456789abcdef"

func epayTestProvider(t *testing.T, kind string) Provider {
	t.Helper()
	p, err := NewEpay(EpayConfig{PID: "10001", Key: epayTestKey, Gateway: "https://gateway.example.test/pay/", Type: kind})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func epayTestRequest() Request {
	return Request{PaymentID: epayTestPayment, OrderID: "order-1", Name: "套餐 A", Amount: 1900, Currency: "CNY", NotifyURL: "https://shop.example.test/api/v1/payments/epay_alipay/notify", ReturnURL: "https://shop.example.test/?payment_return=1"}
}
func epayTestValues() url.Values {
	return url.Values{"pid": {"10001"}, "type": {"alipay"}, "out_trade_no": {epayTestPayment}, "trade_no": {"202610270001"}, "money": {"19.00"}, "trade_status": {"TRADE_SUCCESS"}, "name": {"套餐 A"}}
}

// The gateway fixture deliberately signs its input independently of epaySign;
// Create additionally has a fixed digest generated outside the Go adapter.
func epayFixtureSign(v url.Values) url.Values {
	keys := []string{}
	for k := range v {
		if k != "sign" && k != "sign_type" && v.Get(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+v.Get(k))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + epayTestKey))
	v.Set("sign", hex.EncodeToString(sum[:]))
	v.Set("sign_type", "MD5")
	return v
}
func epayGet(v url.Values) *http.Request {
	return httptest.NewRequest("GET", "https://shop.example.test/notify?"+v.Encode(), nil)
}
func epayPost(v url.Values) *http.Request {
	r := httptest.NewRequest("POST", "https://shop.example.test/notify", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return r
}

func TestEpayCheckoutGoldenSignatureAndChannelIdentity(t *testing.T) {
	p := epayTestProvider(t, "alipay")
	checkout, err := p.Create(context.Background(), epayTestRequest())
	if err != nil || checkout.Kind != "redirect" {
		t.Fatal("checkout failed", err)
	}
	u, err := url.Parse(checkout.URL)
	if err != nil || u.Scheme != "https" || u.Host != "gateway.example.test" || u.Path != "/pay/submit.php" {
		t.Fatal("incorrect checkout destination")
	}
	q := u.Query()
	if q.Get("sign") != "d79e6e5fb2b5f12d3c4c8a5100b5be66" || q.Get("sign_type") != "MD5" || q.Get("money") != "19.00" || q.Get("out_trade_no") != epayTestPayment || q.Get("type") != "alipay" {
		t.Fatal("checkout protocol differs from fixed MD5 vector")
	}
	if strings.Contains(checkout.URL, epayTestKey) || q.Has("key") {
		t.Fatal("merchant key leaked to browser")
	}
	if p.ID() != "epay_alipay" || !strings.Contains(p.Name(), "支付宝") {
		t.Fatal("incorrect Alipay identity")
	}
	wx := epayTestProvider(t, "wxpay")
	checkout, err = wx.Create(context.Background(), epayTestRequest())
	if err != nil || wx.ID() != "epay_wechat" || !strings.Contains(wx.Name(), "微信") {
		t.Fatal("incorrect WeChat identity", err)
	}
	u, _ = url.Parse(checkout.URL)
	if u.Query().Get("type") != "wxpay" {
		t.Fatal("WeChat checkout uses wrong channel")
	}
}

func TestEpaySignedCallbacksAndAuthenticatedMerchantBinding(t *testing.T) {
	p := epayTestProvider(t, "alipay")
	for _, request := range []*http.Request{epayGet(epayFixtureSign(epayTestValues())), epayPost(epayFixtureSign(epayTestValues()))} {
		n, err := p.Verify(request)
		if err != nil || !n.Paid || n.PaymentID != epayTestPayment || n.TransactionID != "202610270001" || n.Amount != 1900 || n.Currency != "CNY" {
			t.Fatal("valid callback rejected", err)
		}
	}
	for field, value := range map[string]string{"money": "0.01", "out_trade_no": strings.Repeat("a", 32), "trade_no": "different", "type": "wxpay", "pid": "99999", "trade_status": "TRADE_CLOSED"} {
		t.Run("tamper_"+field, func(t *testing.T) {
			v := epayFixtureSign(epayTestValues())
			v.Set(field, value)
			if _, err := p.Verify(epayGet(v)); !errors.Is(err, ErrVerification) {
				t.Fatal("tampered callback accepted", err)
			}
		})
	}
	for field, value := range map[string]string{"type": "wxpay", "pid": "99999", "out_trade_no": "not-an-intent", "trade_no": "unsafe\r\ntransaction"} {
		v := epayTestValues()
		v.Set(field, value)
		if _, err := p.Verify(epayGet(epayFixtureSign(v))); !errors.Is(err, ErrVerification) {
			t.Fatal("valid signature bypassed merchant/identity validation", field, err)
		}
	}
	// Shared gateway keys still cannot move an Alipay payment onto WeChat.
	if _, err := epayTestProvider(t, "wxpay").Verify(epayGet(epayFixtureSign(epayTestValues()))); !errors.Is(err, ErrVerification) {
		t.Fatal("cross-channel callback accepted", err)
	}
	v := epayTestValues()
	v.Set("trade_status", "TRADE_CLOSED")
	if n, err := p.Verify(epayGet(epayFixtureSign(v))); err != nil || n.Paid {
		t.Fatal("authenticated non-success became paid", err)
	}
}

func TestEpayCallbackRejectsDuplicateAmbiguousOrOversizedInput(t *testing.T) {
	p := epayTestProvider(t, "alipay")
	for _, key := range []string{"pid", "money", "sign", "out_trade_no", "unused"} {
		v := epayFixtureSign(epayTestValues())
		if !v.Has(key) {
			v.Set(key, "")
		}
		v.Add(key, v.Get(key))
		if _, err := p.Verify(epayGet(v)); !errors.Is(err, ErrVerification) {
			t.Fatal("duplicate callback field accepted", key)
		}
	}
	queryBody := epayPost(epayFixtureSign(epayTestValues()))
	queryBody.URL.RawQuery = "pid=10001"
	wrongContent := epayPost(epayFixtureSign(epayTestValues()))
	wrongContent.Header.Set("Content-Type", "application/json")
	oversized := httptest.NewRequest("POST", "/notify", strings.NewReader(strings.Repeat("x", 65537)))
	oversized.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, r := range []*http.Request{queryBody, wrongContent, oversized, httptest.NewRequest("PUT", "/notify", nil), httptest.NewRequest("GET", "/notify?invalid=%ZZ", nil), httptest.NewRequest("GET", "/notify", nil)} {
		if _, err := p.Verify(r); !errors.Is(err, ErrVerification) {
			t.Fatal("ambiguous/invalid callback accepted", err)
		}
	}
	for _, field := range []string{"sign", "sign_type", "money", "pid", "type", "trade_status", "trade_no", "out_trade_no"} {
		v := epayTestValues()
		v.Del(field)
		epayFixtureSign(v)
		if field == "sign" || field == "sign_type" {
			v.Del(field)
		}
		if _, err := p.Verify(epayGet(v)); !errors.Is(err, ErrVerification) {
			t.Fatal("missing callback field accepted", field)
		}
	}
}

func TestEpayAmountsAreExactMinorUnits(t *testing.T) {
	p := epayTestProvider(t, "alipay")
	for input, want := range map[string]int64{"0.01": 1, "0.10": 10, "19": 1900, "19.0": 1900, "19.01": 1901, "92233720368547758.07": 9223372036854775807} {
		v := epayTestValues()
		v.Set("money", input)
		n, err := p.Verify(epayGet(epayFixtureSign(v)))
		if err != nil || n.Amount != want {
			t.Fatal("incorrect exact amount", input, err)
		}
	}
	for _, input := range []string{"", "0", "0.00", "-1.00", "+1.00", " 1.00", "1.00 ", "1e2", "NaN", "1.001", ".01", "1.", "1.0.0", "92233720368547758.08", "999999999999999999999.99"} {
		v := epayTestValues()
		v.Set("money", input)
		if _, err := p.Verify(epayGet(epayFixtureSign(v))); !errors.Is(err, ErrVerification) {
			t.Fatal("invalid decimal accepted", input, err)
		}
	}
}

func TestEpayConfigurationAndCheckoutValidation(t *testing.T) {
	base := EpayConfig{PID: "10001", Key: epayTestKey, Gateway: "https://gateway.example.test", Type: "alipay"}
	for _, gateway := range []string{"http://gateway.example.test", "javascript:alert(1)", "https://user:password@gateway.example.test", "https://gateway.example.test/?key=bad", "https://gateway.example.test/#fragment", "https://gateway.example.test?", "https://gateway.example.test#", "https://:443"} {
		c := base
		c.Gateway = gateway
		if _, err := NewEpay(c); err == nil {
			t.Fatal("unsafe gateway configuration accepted")
		}
	}
	for field, value := range map[string]string{"PID": "merchant\n", "Key": "", "Type": "wechat"} {
		c := base
		switch field {
		case "PID":
			c.PID = value
		case "Key":
			c.Key = value
		case "Type":
			c.Type = value
		}
		if _, err := NewEpay(c); err == nil {
			t.Fatal("invalid merchant configuration accepted", field)
		}
	}
	p := epayTestProvider(t, "alipay")
	for _, mutate := range []func(*Request){func(r *Request) { r.PaymentID = "browser-id" }, func(r *Request) { r.Amount = 0 }, func(r *Request) { r.Amount = -1 }, func(r *Request) { r.Currency = "USD" }, func(r *Request) { r.NotifyURL = "javascript:alert(1)" }, func(r *Request) { r.ReturnURL = "https://shop.example.test/#secret" }, func(r *Request) { r.Name = "unsafe\nname" }} {
		r := epayTestRequest()
		mutate(&r)
		if _, err := p.Create(context.Background(), r); err == nil {
			t.Fatal("invalid checkout request accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Create(ctx, epayTestRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled checkout did not stop")
	}
}

type epayTransport func(*http.Request) (*http.Response, error)

func (f epayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEpayUnsignedLegacyQueryCannotAuthorizePayment(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: epayTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":1,"status":1,"money":"19.00","trade_no":"fake-paid"}`)), Header: make(http.Header)}, nil
	})}
	p, err := NewEpay(EpayConfig{PID: "10001", Key: epayTestKey, Gateway: "https://gateway.example.test", Type: "alipay", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Query(context.Background(), epayTestRequest())
	if !errors.Is(err, ErrVerification) || n.Paid || calls != 0 {
		t.Fatal("unsigned legacy response could affect settlement or expose key in query")
	}
}
