package payments

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type alipayBillFixture struct {
	provider  BillProvider
	mode      atomic.Value
	downloads atomic.Int32
	archive   []byte
}

func setupAlipayBill(t *testing.T) *alipayBillFixture {
	t.Helper()
	c := alipayTestConfig(t)
	_, platform := alipayKeys(t)
	f := &alipayBillFixture{}
	f.mode.Store("success")
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	entry, err := z.Create("业务明细.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bounded fixture content"))
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	f.archive = out.Bytes()
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := f.mode.Load().(string)
		if r.URL.Path == "/gateway.do" {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			v := r.PostForm
			signature, e := base64.StdEncoding.DecodeString(v.Get("sign"))
			digest := sha256.Sum256([]byte(alipayFixtureCanonical(v, false)))
			var biz map[string]string
			if e != nil || rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, digest[:], signature) != nil || v.Get("app_id") != c.AppID || v.Get("method") != "alipay.data.dataservice.bill.downloadurl.query" || json.Unmarshal([]byte(v.Get("biz_content")), &biz) != nil || biz["bill_type"] != "trade" || biz["bill_date"] != "2026-10-26" || biz["secure"] != "true" {
				t.Error("bill query not bound to signed merchant request")
			}
			url := server.URL + "/downloadBillFile.resource?token=private-fixture-token"
			switch mode {
			case "http":
				url = strings.Replace(url, "https:", "http:", 1)
			case "host":
				url = "https://example.test/downloadBillFile.resource?token=private-fixture-token"
			case "path":
				url = server.URL + "/private?token=private-fixture-token"
			case "userinfo":
				url = strings.Replace(url, "https://", "https://user:pass@", 1)
			case "fragment":
				url += "#secret"
			case "encoded_path":
				url = server.URL + "/%64ownloadBillFile.resource?token=private-fixture-token"
			}
			payload := map[string]string{"code": "10000", "app_id": c.AppID, "seller_id": c.SellerID, "bill_download_url": url}
			if mode == "wrong_merchant" {
				payload["seller_id"] = "2088102700000002"
			}
			if mode == "empty" {
				payload["bill_download_url"] = ""
				payload["bill_file_code"] = "EMPTY_DATA_WITH_BILL_FILE"
			}
			if mode == "metadata_redirect" {
				w.Header().Set("Location", server.URL+"/must-not-follow")
				w.WriteHeader(302)
				return
			}
			data, _ := json.Marshal(payload)
			sig := alipayFixtureSign(t, platform, data)
			if mode == "signature" {
				sig = alipayFixtureSign(t, c.PrivateKey, data)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"alipay_data_dataservice_bill_downloadurl_query_response": json.RawMessage(data), "sign": sig})
			return
		}
		if r.URL.Path != "/downloadBillFile.resource" {
			t.Error("unexpected request path")
			w.WriteHeader(404)
			return
		}
		f.downloads.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("download leaked merchant credentials or implicit encoding")
		}
		switch mode {
		case "download_redirect":
			w.Header().Set("Location", server.URL+"/must-not-follow")
			w.WriteHeader(302)
		case "oversize":
			w.Header().Set("Content-Length", "8388609")
			w.WriteHeader(200)
		case "invalid_archive":
			_, _ = w.Write([]byte("not a zip"))
		default:
			_, _ = w.Write(f.archive)
		}
	}))
	t.Cleanup(server.Close)
	c.Gateway = server.URL + "/gateway.do"
	c.HTTPClient = server.Client()
	c.HTTPClient.Jar, _ = cookiejar.New(nil)
	fixtureOrigin, _ := url.Parse(server.URL)
	c.HTTPClient.Jar.SetCookies(fixtureOrigin, []*http.Cookie{{Name: "merchant_session", Value: "private-cookie"}})
	p, err := NewAlipay(c)
	if err != nil {
		t.Fatal(err)
	}
	f.provider = p.(BillProvider)
	return f
}
func TestAlipayBillUsesSecureSignedMetadataAndCredentialFreeBoundedDownload(t *testing.T) {
	f := setupAlipayBill(t)
	bill, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: "2026-10-26", Kind: "payment"})
	if err != nil || bill.Provider != "alipay" || bill.Date != "2026-10-26" || bill.Kind != "payment" || bill.Format != "alipay_trade_v1" || !bytes.Equal(bill.Data, f.archive) || f.downloads.Load() != 1 {
		t.Fatal(bill.Provider, bill.Format, err)
	}
}
func TestAlipayBillRejectsUntrustedMetadataAndUnsafeDownloadLocations(t *testing.T) {
	f := setupAlipayBill(t)
	for _, mode := range []string{"signature", "wrong_merchant", "http", "host", "path", "userinfo", "fragment", "encoded_path", "empty", "metadata_redirect"} {
		f.mode.Store(mode)
		before := f.downloads.Load()
		_, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: "2026-10-26", Kind: "refund"})
		if err == nil || strings.Contains(err.Error(), "private-fixture-token") || f.downloads.Load() != before {
			t.Fatal("untrusted URL reached download", mode, err)
		}
	}
}
func TestAlipayBillRejectsRedirectOversizeAndInvalidArchives(t *testing.T) {
	f := setupAlipayBill(t)
	for _, mode := range []string{"download_redirect", "oversize", "invalid_archive"} {
		f.mode.Store(mode)
		if _, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: "2026-10-26", Kind: "payment"}); err == nil {
			t.Fatal(mode)
		}
	}
}
func TestBillDateScopeFingerprintAndClassicEpayCapability(t *testing.T) {
	now := time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC)
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	for _, value := range []struct {
		date  string
		valid bool
	}{{"2026-10-27", true}, {"2026-10-28", false}, {day.AddDate(0, 0, -90).Format("2006-01-02"), true}, {day.AddDate(0, 0, -91).Format("2006-01-02"), false}, {"2026-02-30", false}, {"2026-1-1", false}} {
		if ValidBillDate(value.date, now) != value.valid {
			t.Fatal(value.date)
		}
	}
	c := alipayTestConfig(t)
	p, _ := NewAlipay(c)
	a := p.(BillProvider).BillMerchantFingerprint()
	merchant, platform := alipayKeys(t)
	c.PrivateKey = platform
	c.PublicKey = &merchant.PublicKey
	p, _ = NewAlipay(c)
	if p.(BillProvider).BillMerchantFingerprint() != a {
		t.Fatal("key rotation changed merchant identity")
	}
	c.SellerID = "2088102700000002"
	p, _ = NewAlipay(c)
	if p.(BillProvider).BillMerchantFingerprint() == a {
		t.Fatal("merchant change reused identity")
	}
	epay, err := NewEpay(EpayConfig{PID: "1000", Key: "local-fixture-key", Gateway: "https://pay.example.test", Type: "alipay"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := epay.(BillProvider); ok {
		t.Fatal("classic Epay advertised authenticated bill API")
	}
}
