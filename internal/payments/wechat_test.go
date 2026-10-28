package payments

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func wctConfig(t *testing.T) (WechatConfig, *rsa.PrivateKey) {
	t.Helper()
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0)
	return WechatConfig{AppID: "wxFixtureApp", MchID: "1234567890", Serial: "A123", PlatformSerial: "PUB_KEY_ID_FIXTURE", APIv3Key: strings.Repeat("a", 32), PrivateKey: merchant, PlatformKey: &platform.PublicKey, Now: func() time.Time { return now }}, platform
}
func wctRequest() Request {
	return Request{PaymentID: "payment_01234567890123456789", OrderID: "internal_order", Name: "节点套餐", Amount: 1901, Currency: "CNY", NotifyURL: "https://panel.example.test/api/v1/payments/wechat/notify"}
}
func wctJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func wctSign(t *testing.T, key *rsa.PrivateKey, message string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}
func wctHeaders(t *testing.T, c WechatConfig, key *rsa.PrivateKey, body []byte) http.Header {
	t.Helper()
	timestamp := strconv.FormatInt(c.Now().Unix(), 10)
	nonce := "fixture-nonce"
	h := make(http.Header)
	h.Set("Wechatpay-Timestamp", timestamp)
	h.Set("Wechatpay-Nonce", nonce)
	h.Set("Wechatpay-Serial", c.PlatformSerial)
	h.Set("Wechatpay-Signature", wctSign(t, key, timestamp+"\n"+nonce+"\n"+string(body)+"\n"))
	return h
}
func wctTransaction(c WechatConfig) map[string]any {
	return map[string]any{"appid": c.AppID, "mchid": c.MchID, "out_trade_no": wctRequest().PaymentID, "transaction_id": "wx-transaction-123", "trade_state": "SUCCESS", "amount": map[string]any{"total": 1901, "currency": "CNY", "payer_total": 1800, "payer_currency": "CNY"}}
}
func wctEvent(t *testing.T, c WechatConfig, transaction map[string]any) []byte {
	t.Helper()
	block, err := aes.NewCipher([]byte(c.APIv3Key))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce, associated := "nonce-12byte", "transaction"
	sealed := gcm.Seal(nil, []byte(nonce), wctJSON(t, transaction), []byte(associated))
	return wctJSON(t, map[string]any{"id": "notification-123", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource", "resource": map[string]any{"original_type": "transaction", "algorithm": "AEAD_AES_256_GCM", "nonce": nonce, "associated_data": associated, "ciphertext": base64.StdEncoding.EncodeToString(sealed)}})
}
func wctNotify(body []byte, h http.Header) *http.Request {
	r := httptest.NewRequest("POST", "https://panel.example.test/notify", strings.NewReader(string(body)))
	r.Header = h.Clone()
	r.Header.Set("Content-Type", "application/json")
	return r
}
func wctVerifyMerchant(c WechatConfig, r *http.Request, body []byte) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "WECHATPAY2-SHA256-RSA2048 ") {
		return false
	}
	fields := map[string]string{}
	for _, match := range regexp.MustCompile(`([a-z_]+)="([^"]+)"`).FindAllStringSubmatch(auth, -1) {
		fields[match[1]] = match[2]
	}
	if fields["mchid"] != c.MchID || fields["serial_no"] != c.Serial || fields["timestamp"] != strconv.FormatInt(c.Now().Unix(), 10) || fields["nonce_str"] == "" || r.Header.Get("Wechatpay-Serial") != c.PlatformSerial {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + fields["timestamp"] + "\n" + fields["nonce_str"] + "\n" + string(body) + "\n"))
	return rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, digest[:], decoded) == nil
}

func TestWechatNativeAndQueryUseSignedRequestsAndVerifiedResponses(t *testing.T) {
	c, key := wctConfig(t)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !wctVerifyMerchant(c, r, body) {
			t.Error("invalid merchant request signature")
			w.WriteHeader(400)
			return
		}
		var response []byte
		switch r.URL.Path {
		case "/v3/pay/transactions/native":
			if r.Method != "POST" {
				t.Error("wrong native method")
			}
			var request struct {
				AppID     string `json:"appid"`
				MchID     string `json:"mchid"`
				PaymentID string `json:"out_trade_no"`
				NotifyURL string `json:"notify_url"`
				Amount    struct {
					Total    int64  `json:"total"`
					Currency string `json:"currency"`
				} `json:"amount"`
			}
			if json.Unmarshal(body, &request) != nil || request.AppID != c.AppID || request.MchID != c.MchID || request.PaymentID != wctRequest().PaymentID || request.Amount.Total != 1901 || request.Amount.Currency != "CNY" || request.NotifyURL != wctRequest().NotifyURL {
				t.Error("native request did not preserve order snapshot")
			}
			response = []byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=fixture"}`)
		case "/v3/pay/transactions/out-trade-no/" + wctRequest().PaymentID:
			if r.Method != "GET" || r.URL.Query().Get("mchid") != c.MchID || len(body) != 0 {
				t.Error("wrong query request")
			}
			response = wctJSON(t, wctTransaction(c))
		default:
			t.Error("unexpected API path")
			w.WriteHeader(404)
			return
		}
		for k, v := range wctHeaders(t, c, key, response) {
			w.Header()[k] = v
		}
		_, _ = w.Write(response)
	}))
	defer server.Close()
	c.Gateway = server.URL
	c.HTTPClient = server.Client()
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := p.Create(context.Background(), wctRequest())
	if err != nil || checkout.Kind != "qr" || checkout.URL != "weixin://wxpay/bizpayurl?pr=fixture" {
		t.Fatal("native checkout", checkout, err)
	}
	n, err := p.Query(context.Background(), wctRequest())
	if err != nil || !n.Paid || n.Amount != 1901 || n.PaymentID != wctRequest().PaymentID || n.TransactionID != "wx-transaction-123" {
		t.Fatal("query verification", n, err)
	}
	if requests != 2 {
		t.Fatal("unexpected request count", requests)
	}
}

func TestWechatNotificationAuthenticatesAndDecryptsBeforeAccepting(t *testing.T) {
	c, key := wctConfig(t)
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	data := wctEvent(t, c, wctTransaction(c))
	headers := wctHeaders(t, c, key, data)
	for i := 0; i < 2; i++ {
		n, err := p.Verify(wctNotify(data, headers))
		if err != nil || !n.Paid || n.PaymentID != wctRequest().PaymentID || n.Amount != 1901 || n.TransactionID != "wx-transaction-123" {
			t.Fatal("valid callback", n, err)
		}
	}
	for _, mutation := range []string{"appid", "mchid", "currency", "amount", "state", "transaction_id", "out_trade_no"} {
		t.Run(mutation, func(t *testing.T) {
			transaction := wctTransaction(c)
			switch mutation {
			case "appid", "mchid":
				transaction[mutation] = "other-merchant"
			case "currency":
				transaction["amount"] = map[string]any{"total": 1901, "currency": "USD"}
			case "amount":
				transaction["amount"] = map[string]any{"total": 1901.5, "currency": "CNY"}
			case "state":
				transaction["trade_state"] = "NOTPAY"
			case "transaction_id", "out_trade_no":
				transaction[mutation] = ""
			}
			body := wctEvent(t, c, transaction)
			if _, err := p.Verify(wctNotify(body, wctHeaders(t, c, key, body))); !errors.Is(err, ErrVerification) {
				t.Fatal("invalid transaction accepted", err)
			}
		})
	}
	wrong := c
	wrong.APIv3Key = strings.Repeat("b", 32)
	other, err := NewWechat(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Verify(wctNotify(data, headers)); !errors.Is(err, ErrVerification) {
		t.Fatal("wrong decryption key accepted", err)
	}
}

func TestWechatNotificationRejectsTamperingStaleAndDuplicateHeaders(t *testing.T) {
	c, key := wctConfig(t)
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	body := wctEvent(t, c, wctTransaction(c))
	header := wctHeaders(t, c, key, body)
	for _, mutation := range []string{"signature", "serial", "stale", "future", "duplicate", "nonce", "body", "method", "content-type"} {
		t.Run(mutation, func(t *testing.T) {
			h := header.Clone()
			data := append([]byte{}, body...)
			switch mutation {
			case "signature":
				h.Set("Wechatpay-Signature", "bad")
			case "serial":
				h.Set("Wechatpay-Serial", "OTHER_KEY")
			case "stale", "future":
				delta := int64(-301)
				if mutation == "future" {
					delta = 301
				}
				timestamp := strconv.FormatInt(c.Now().Unix()+delta, 10)
				h.Set("Wechatpay-Timestamp", timestamp)
				h.Set("Wechatpay-Signature", wctSign(t, key, timestamp+"\n"+h.Get("Wechatpay-Nonce")+"\n"+string(body)+"\n"))
			case "duplicate":
				h.Add("Wechatpay-Timestamp", h.Get("Wechatpay-Timestamp"))
			case "nonce":
				h.Set("Wechatpay-Nonce", "x\ny")
			case "body":
				data = append(data, ' ')
			}
			r := wctNotify(data, h)
			if mutation == "method" {
				r.Method = "GET"
			}
			if mutation == "content-type" {
				r.Header.Set("Content-Type", "text/plain")
			}
			if _, err := p.Verify(r); !errors.Is(err, ErrVerification) {
				t.Fatal("tampered callback accepted", err)
			}
		})
	}
	var event map[string]any
	if json.Unmarshal(body, &event) != nil {
		t.Fatal("fixture event invalid")
	}
	for _, mutation := range []string{"event_type", "resource_type", "algorithm", "nonce", "ciphertext", "original_type"} {
		t.Run("resource-"+mutation, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatal(err)
			}
			if mutation == "event_type" || mutation == "resource_type" {
				v[mutation] = "wrong"
			} else {
				v["resource"].(map[string]any)[mutation] = "wrong"
			}
			data := wctJSON(t, v)
			if _, err := p.Verify(wctNotify(data, wctHeaders(t, c, key, data))); !errors.Is(err, ErrVerification) {
				t.Fatal("invalid resource accepted", err)
			}
		})
	}
}

func TestWechatQueryRejectsUntrustedResponsesAndCrossOrderData(t *testing.T) {
	c, key := wctConfig(t)
	for _, mutation := range []string{"signature", "serial", "stale", "unsigned", "amount", "payment", "merchant", "currency", "oversized", "redirect", "pending", "closed", "not-found"} {
		t.Run(mutation, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				transaction := wctTransaction(c)
				code := 200
				switch mutation {
				case "amount":
					transaction["amount"] = map[string]any{"total": 1900, "currency": "CNY"}
				case "payment":
					transaction["out_trade_no"] = "another_payment"
				case "merchant":
					transaction["mchid"] = "other-merchant"
				case "currency":
					transaction["amount"] = map[string]any{"total": 1901, "currency": "USD"}
				case "closed":
					transaction["trade_state"] = "CLOSED"
					delete(transaction, "transaction_id")
				case "pending":
					transaction["trade_state"] = "NOTPAY"
					delete(transaction, "transaction_id")
				}
				data := wctJSON(t, transaction)
				if mutation == "oversized" {
					data = []byte(strings.Repeat("x", 65537))
				}
				if mutation == "not-found" {
					data = []byte(`{"code":"ORDER_NOT_EXIST"}`)
					code = 404
				}
				h := wctHeaders(t, c, key, data)
				switch mutation {
				case "signature":
					h.Set("Wechatpay-Signature", "invalid")
				case "serial":
					h.Set("Wechatpay-Serial", "other-key")
				case "stale":
					h.Set("Wechatpay-Timestamp", "1")
				case "unsigned":
					h.Del("Wechatpay-Signature")
				case "redirect":
					code = 302
					h.Set("Location", "/redirect-must-not-be-followed")
				}
				for k, v := range h {
					w.Header()[k] = v
				}
				w.WriteHeader(code)
				_, _ = w.Write(data)
			}))
			defer server.Close()
			config := c
			config.Gateway = server.URL
			config.HTTPClient = server.Client()
			p, err := NewWechat(config)
			if err != nil {
				t.Fatal(err)
			}
			n, err := p.Query(context.Background(), wctRequest())
			if mutation == "pending" || mutation == "closed" {
				if err != nil || n.Paid || n.Closed != (mutation == "closed") {
					t.Fatal("pending query", n, err)
				}
				return
			}
			if mutation == "not-found" {
				if !errors.Is(err, ErrNotPaid) {
					t.Fatal("not-found query", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestWechatConfigurationAndInputValidation(t *testing.T) {
	c, _ := wctConfig(t)
	for _, change := range []func(*WechatConfig){func(c *WechatConfig) { c.APIv3Key = "short" }, func(c *WechatConfig) { c.AppID = "" }, func(c *WechatConfig) { c.MchID = "bad\nmerchant" }, func(c *WechatConfig) { c.PlatformSerial = "" }, func(c *WechatConfig) { c.PrivateKey = nil }, func(c *WechatConfig) { c.PlatformKey = nil }, func(c *WechatConfig) { c.Gateway = "http://gateway.example.test" }, func(c *WechatConfig) { c.Gateway = "https://gateway.example.test/path" }, func(c *WechatConfig) { c.Gateway = "https://gateway.example.test?" }} {
		bad := c
		change(&bad)
		if _, err := NewWechat(bad); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){func(r *Request) { r.Currency = "USD" }, func(r *Request) { r.Amount = 0 }, func(r *Request) { r.PaymentID = "bad/id" }, func(r *Request) { r.NotifyURL = "http://panel.example.test/notify" }, func(r *Request) { r.Name = "" }} {
		bad := wctRequest()
		change(&bad)
		if _, err := p.Create(context.Background(), bad); err == nil {
			t.Fatal("invalid payment request accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = p.Query(ctx, wctRequest()); !errors.Is(err, ErrGateway) {
		t.Fatal("canceled context was not honored", err)
	}
}
