package httpapi_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/payments"
)

// This crosses the actual adapter, callback middleware, database transaction and
// entitlement boundary. It uses ephemeral merchant/platform keys, never a mock
// Provider.Verify implementation or an external payment account.
func TestActualPaymentAdaptersFulfillOnlyAuthenticatedCallbacks(t *testing.T) {
	merchant, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const app = "2026102700000001"
	const seller = "2088102700000001"
	const epayKey = "local-fixture-merchant-secret"
	const wxKey = "12345678901234567890123456789012"
	for _, id := range []string{"alipay", "wechat", "epay_alipay", "epay_wechat"} {
		t.Run(id, func(t *testing.T) {
			f := setup(t, true)
			u, _ := register(t, f)
			ctx := context.Background()
			o, err := f.db.CreateOrder(ctx, u.ID, "explore")
			if err != nil {
				t.Fatal(err)
			}
			p, err := f.db.CreatePayment(ctx, u.ID, o.ID, id)
			if err != nil {
				t.Fatal(err)
			}
			var provider payments.Provider
			switch id {
			case "alipay":
				provider, err = payments.NewAlipay(payments.AlipayConfig{AppID: app, SellerID: seller, PrivateKey: merchant, PublicKey: &platform.PublicKey})
			case "wechat":
				provider, err = payments.NewWechat(payments.WechatConfig{AppID: app, MchID: seller, Serial: "MERCHANT_SERIAL", PlatformSerial: "PUB_KEY_ID_FIXTURE", APIv3Key: wxKey, PrivateKey: merchant, PlatformKey: &platform.PublicKey})
			default:
				kind := "alipay"
				if id == "epay_wechat" {
					kind = "wxpay"
				}
				provider, err = payments.NewEpay(payments.EpayConfig{PID: "1001", Key: epayKey, Gateway: "https://pay.example.test/", Type: kind})
			}
			if err != nil {
				t.Fatal(err)
			}
			registry, err := payments.New(provider)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, Development: true, Payments: registry, PasswordCost: 4}).Handler())
			defer server.Close()
			path := server.URL + "/api/v1/payments/notify/" + id
			var payload []byte
			headers := make(http.Header)
			if id == "wechat" {
				plain, _ := json.Marshal(map[string]any{"appid": app, "mchid": seller, "out_trade_no": p.ID, "transaction_id": "wx-actual-transaction", "trade_state": "SUCCESS", "amount": map[string]any{"total": o.Price, "currency": "CNY"}})
				block, _ := aes.NewCipher([]byte(wxKey))
				gcm, _ := cipher.NewGCM(block)
				nonce := "nonce-123456"
				associated := "transaction"
				encrypted := gcm.Seal(nil, []byte(nonce), plain, []byte(associated))
				payload, _ = json.Marshal(map[string]any{"id": "fixture-event", "event_type": "TRANSACTION.SUCCESS", "resource_type": "encrypt-resource", "resource": map[string]any{"original_type": "transaction", "algorithm": "AEAD_AES_256_GCM", "nonce": nonce, "associated_data": associated, "ciphertext": base64.StdEncoding.EncodeToString(encrypted)}})
				headers.Set("Content-Type", "application/json")
				timestamp := strconv.FormatInt(time.Now().Unix(), 10)
				headers.Set("Wechatpay-Timestamp", timestamp)
				headers.Set("Wechatpay-Nonce", "response-nonce")
				headers.Set("Wechatpay-Serial", "PUB_KEY_ID_FIXTURE")
				digest := sha256.Sum256(append([]byte(timestamp+"\nresponse-nonce\n"), append(append([]byte{}, payload...), '\n')...))
				signature, err := rsa.SignPKCS1v15(rand.Reader, platform, crypto.SHA256, digest[:])
				if err != nil {
					t.Fatal(err)
				}
				headers.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(signature))
			} else {
				v := url.Values{"out_trade_no": {p.ID}, "trade_no": {"fixture-transaction"}, "trade_status": {"TRADE_SUCCESS"}}
				money := strconv.FormatInt(o.Price/100, 10) + "." + strconv.FormatInt(o.Price%100/10, 10) + strconv.FormatInt(o.Price%10, 10)
				if id == "alipay" {
					v.Set("app_id", app)
					v.Set("seller_id", seller)
					v.Set("total_amount", money)
				} else {
					v.Set("pid", "1001")
					v.Set("money", money)
					kind := "alipay"
					if id == "epay_wechat" {
						kind = "wxpay"
					}
					v.Set("type", kind)
				}
				keys := []string{}
				for k := range v {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := []string{}
				for _, k := range keys {
					parts = append(parts, k+"="+v.Get(k))
				}
				canonical := strings.Join(parts, "&")
				if id == "alipay" {
					digest := sha256.Sum256([]byte(canonical))
					signature, err := rsa.SignPKCS1v15(rand.Reader, platform, crypto.SHA256, digest[:])
					if err != nil {
						t.Fatal(err)
					}
					v.Set("sign", base64.StdEncoding.EncodeToString(signature))
					v.Set("sign_type", "RSA2")
				} else {
					digest := md5.Sum([]byte(canonical + epayKey))
					v.Set("sign", hex.EncodeToString(digest[:]))
					v.Set("sign_type", "MD5")
				}
				payload = []byte(v.Encode())
				headers.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			post := func(body []byte) int {
				t.Helper()
				r, err := http.NewRequest("POST", path, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header = headers.Clone()
				// No cookie or Origin: provider proof alone authenticates this path.
				response, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				io.Copy(io.Discard, response.Body)
				return response.StatusCode
			}
			bad := append([]byte{}, payload...)
			if id == "wechat" {
				bad[0] = '['
			} else {
				bad = append(bad, []byte("&money=0.01&total_amount=0.01")...)
			}
			if code := post(bad); code != 400 {
				t.Fatal("tampered callback reached settlement", code)
			}
			for i := 0; i < 2; i++ {
				if code := post(payload); code != 200 {
					t.Fatal("authenticated callback rejected", code)
				}
			}
			var periods, events int
			if err = f.db.DB.QueryRow(ctx, `SELECT count(*) FROM entitlement_periods WHERE order_id=$1`, o.ID).Scan(&periods); err != nil {
				t.Fatal(err)
			}
			if err = f.db.DB.QueryRow(ctx, `SELECT count(*) FROM payment_events WHERE order_id=$1`, o.ID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			paid, err := f.db.Payment(ctx, u.ID, o.ID)
			if err != nil || paid.Status != "paid" || periods != 1 || events != 1 {
				t.Fatal("payment was not atomically fulfilled exactly once", err, periods, events)
			}
		})
	}
}
