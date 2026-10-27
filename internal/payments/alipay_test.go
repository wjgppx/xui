package payments

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var alipayTestKeys struct {
	once               sync.Once
	merchant, platform *rsa.PrivateKey
	err                error
}

func alipayKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	alipayTestKeys.once.Do(func() {
		alipayTestKeys.merchant, alipayTestKeys.err = rsa.GenerateKey(rand.Reader, 2048)
		if alipayTestKeys.err == nil {
			alipayTestKeys.platform, alipayTestKeys.err = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if alipayTestKeys.err != nil {
		t.Fatal(alipayTestKeys.err)
	}
	return alipayTestKeys.merchant, alipayTestKeys.platform
}
func alipayTestConfig(t *testing.T) AlipayConfig {
	merchant, platform := alipayKeys(t)
	return AlipayConfig{AppID: "2026102700000001", SellerID: "2088102700000001", PrivateKey: merchant, PublicKey: &platform.PublicKey, Now: func() time.Time { return time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC) }}
}
func alipayTestRequest() Request {
	return Request{PaymentID: strings.Repeat("a", 32), OrderID: strings.Repeat("b", 32), Name: "独立套餐 & 续费", Amount: 1901, Currency: "CNY", NotifyURL: "https://control.example.test/payments/alipay/notify", ReturnURL: "https://control.example.test/"}
}
func alipayFixtureCanonical(v url.Values, notify bool) string {
	keys := []string{}
	for k := range v {
		if k != "sign" && (!notify || k != "sign_type") && v.Get(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+v.Get(k))
	}
	return strings.Join(parts, "&")
}
func alipayFixtureSign(t *testing.T, key *rsa.PrivateKey, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}
func alipayFixtureNotification(t *testing.T, config AlipayConfig, edit func(url.Values)) *http.Request {
	_, platform := alipayKeys(t)
	v := url.Values{"app_id": {config.AppID}, "seller_id": {config.SellerID}, "out_trade_no": {strings.Repeat("a", 32)}, "trade_no": {"202610272200000001"}, "trade_status": {"TRADE_SUCCESS"}, "total_amount": {"19.01"}, "sign_type": {"RSA2"}, "charset": {"utf-8"}, "subject": {"独立套餐 & 续费"}}
	if edit != nil {
		edit(v)
	}
	v.Set("sign", alipayFixtureSign(t, platform, []byte(alipayFixtureCanonical(v, true))))
	r := httptest.NewRequest("POST", "https://control.example.test/notify", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return r
}

func TestAlipayPageCheckoutUsesMerchantRSA2AndExactSnapshot(t *testing.T) {
	c := alipayTestConfig(t)
	p, err := NewAlipay(c)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := p.Create(context.Background(), alipayTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(checkout.URL)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query()
	if checkout.Kind != "redirect" || u.Host != "openapi.alipay.com" || v.Get("method") != "alipay.trade.page.pay" || v.Get("timestamp") != "2026-10-27 09:02:03" || v.Get("app_id") != c.AppID || v.Get("sign_type") != "RSA2" {
		t.Fatal("incorrect signed checkout protocol")
	}
	var biz map[string]string
	if err = json.Unmarshal([]byte(v.Get("biz_content")), &biz); err != nil {
		t.Fatal(err)
	}
	if biz["out_trade_no"] != alipayTestRequest().PaymentID || biz["total_amount"] != "19.01" || biz["subject"] != alipayTestRequest().Name || biz["seller_id"] != c.SellerID || biz["product_code"] != "FAST_INSTANT_TRADE_PAY" {
		t.Fatal("checkout changed immutable order values")
	}
	sig, err := base64.StdEncoding.DecodeString(v.Get("sign"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(alipayFixtureCanonical(v, false)))
	if rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, digest[:], sig) != nil {
		t.Fatal("merchant request RSA2 verification failed")
	}
	if v.Get("notify_url") != alipayTestRequest().NotifyURL || v.Get("return_url") != alipayTestRequest().ReturnURL {
		t.Fatal("callback origin changed")
	}
	for _, currency := range []string{"USD", ""} {
		r := alipayTestRequest()
		r.Currency = currency
		if _, e := p.Create(context.Background(), r); e == nil {
			t.Fatal("unsupported currency")
		}
	}
}

func TestAlipayNotificationAuthenticatesMerchantAndRejectsAmbiguity(t *testing.T) {
	c := alipayTestConfig(t)
	p, err := NewAlipay(c)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Verify(alipayFixtureNotification(t, c, nil))
	if err != nil || !n.Paid || n.Amount != 1901 || n.Currency != "CNY" || n.TransactionID != "202610272200000001" {
		t.Fatal("valid notification rejected", err)
	}
	for _, status := range []string{"WAIT_BUYER_PAY", "TRADE_CLOSED", "TRADE_FINISHED"} {
		n, e := p.Verify(alipayFixtureNotification(t, c, func(v url.Values) { v.Set("trade_status", status) }))
		if e != nil || n.Paid != (status == "TRADE_FINISHED") {
			t.Fatalf("trade status %s misclassified", status)
		}
	}
	cases := map[string]func(*http.Request){
		"amount tampered": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(b), "total_amount=19.01", "total_amount=0.01", 1)))
		},
		"duplicate amount": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b) + "&total_amount=19.01"))
		},
		"duplicate signature": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b) + "&sign=ignored"))
		},
		"query pollution": func(r *http.Request) { r.URL.RawQuery = "out_trade_no=other" },
		"GET return":      func(r *http.Request) { r.Method = "GET" },
		"JSON content":    func(r *http.Request) { r.Header.Set("Content-Type", "application/json") },
		"oversized":       func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", 65537))) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			r := alipayFixtureNotification(t, c, nil)
			edit(r)
			if _, e := p.Verify(r); !errors.Is(e, ErrVerification) {
				t.Fatal("untrusted notification accepted", e)
			}
		})
	}
	for field, value := range map[string]string{"app_id": "2026102700000002", "seller_id": "2088102700000002", "trade_status": "unknown", "trans_currency": "USD", "total_amount": "19.001", "out_trade_no": "external-order", "sign_type": "RSA"} {
		t.Run(field, func(t *testing.T) {
			r := alipayFixtureNotification(t, c, func(v url.Values) { v.Set(field, value) })
			if _, e := p.Verify(r); !errors.Is(e, ErrVerification) {
				t.Fatal("signed mismatched identity or amount accepted", e)
			}
		})
	}
}

func TestAlipayQueryAuthenticatesRawResponseAndRequest(t *testing.T) {
	c := alipayTestConfig(t)
	_, platform := alipayKeys(t)
	mode := "paid"
	var target *httptest.Server
	target = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		v := r.PostForm
		signature, e := base64.StdEncoding.DecodeString(v.Get("sign"))
		digest := sha256.Sum256([]byte(alipayFixtureCanonical(v, false)))
		if e != nil || rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, digest[:], signature) != nil || v.Get("method") != "alipay.trade.query" || v.Get("app_id") != c.AppID || v.Get("biz_content") != `{"out_trade_no":"`+strings.Repeat("a", 32)+`"}` {
			t.Error("invalid merchant query authentication")
			w.WriteHeader(400)
			return
		}
		payload := `{ "code" : "10000", "out_trade_no":"` + strings.Repeat("a", 32) + `", "trade_no":"202610272200000001", "trade_status":"TRADE_SUCCESS", "total_amount":"19.01" }`
		switch mode {
		case "pending":
			payload = strings.Replace(payload, "TRADE_SUCCESS", "WAIT_BUYER_PAY", 1)
		case "wrong_order":
			payload = strings.Replace(payload, strings.Repeat("a", 32), strings.Repeat("b", 32), 1)
		case "wrong_amount":
			payload = strings.Replace(payload, "19.01", "19.02", 1)
		case "wrong_merchant":
			payload = strings.Replace(payload, `"code"`, `"seller_id":"2088102700000002","code"`, 1)
		case "redirect":
			w.Header().Set("Location", target.URL+"/must-not-follow")
			w.WriteHeader(302)
			return
		case "oversized":
			w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		sig := alipayFixtureSign(t, platform, []byte(payload))
		if mode == "wrong_signature" {
			sig = alipayFixtureSign(t, c.PrivateKey, []byte(payload))
		}
		if mode == "tampered_after_signing" {
			payload = strings.Replace(payload, "19.01", "19.02", 1)
		}
		if mode == "duplicate_envelope" {
			w.Write([]byte(`{"alipay_trade_query_response":` + payload + `,"alipay_trade_query_response":` + payload + `,"sign":"` + sig + `"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"alipay_trade_query_response":` + payload + `,"sign":"` + sig + `"}`))
	}))
	defer target.Close()
	c.Gateway = target.URL
	c.HTTPClient = target.Client()
	p, err := NewAlipay(c)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Query(context.Background(), alipayTestRequest())
	if err != nil || !n.Paid || n.Amount != 1901 {
		t.Fatal("valid signed query rejected", err)
	}
	mode = "pending"
	n, err = p.Query(context.Background(), alipayTestRequest())
	if err != nil || n.Paid {
		t.Fatal("pending trade settled", err)
	}
	for _, m := range []string{"wrong_order", "wrong_amount", "wrong_merchant", "wrong_signature", "tampered_after_signing", "duplicate_envelope", "redirect", "oversized"} {
		t.Run(m, func(t *testing.T) {
			mode = m
			if _, e := p.Query(context.Background(), alipayTestRequest()); e == nil {
				t.Fatal("invalid query accepted")
			}
		})
	}
}

func TestAlipayConfigurationAndMoneyValidation(t *testing.T) {
	c := alipayTestConfig(t)
	for _, gateway := range []string{"http://openapi.alipay.com/gateway.do", "https://merchant.example.test/gateway.do", "https://openapi.alipay.com/gateway.do?", "https://openapi.alipay.com/gateway.do#"} {
		bad := c
		bad.Gateway = gateway
		if _, e := NewAlipay(bad); e == nil {
			t.Fatal("unsafe gateway accepted")
		}
	}
	for _, value := range []string{"0", "-1.00", "+1.00", "1.001", "1e2", "1.", ".10", "1.00 ", "92233720368547758.08", "999999999999999999999999"} {
		if _, e := alipayMinor(value); e == nil {
			t.Fatalf("invalid exact money accepted %q", value)
		}
	}
	for value, want := range map[string]int64{"0.01": 1, "19": 1900, "19.1": 1910, "19.01": 1901, "92233720368547758.07": 9223372036854775807} {
		got, e := alipayMinor(value)
		if e != nil || got != want {
			t.Fatalf("money %q = %d (%v)", value, got, e)
		}
	}
	bad := c
	bad.PublicKey = nil
	if _, e := NewAlipay(bad); e == nil {
		t.Fatal("missing verification key accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, e := NewAlipay(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Create(ctx, alipayTestRequest()); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
}
