package payments

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type wechatBillFixture struct {
	provider  BillProvider
	mode      atomic.Value
	downloads atomic.Int32
	date      string
	plain     []byte
}

func wechatBillGZ(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func setupWechatBill(t *testing.T) *wechatBillFixture {
	t.Helper()
	c, key := wctConfig(t)
	f := &wechatBillFixture{plain: []byte("synthetic UTF-8 bill 明文"), date: c.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).AddDate(0, 0, -1).Format("2006-01-02")}
	f.mode.Store("success")
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := f.mode.Load().(string)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if !wctVerifyMerchant(c, r, body) {
			t.Error("merchant bill request was not signed")
		}
		compressed := wechatBillGZ(t, f.plain)
		if r.URL.Path == "/v3/bill/tradebill" {
			if r.Method != "GET" || r.URL.Query().Get("bill_date") != f.date || r.URL.Query().Get("tar_type") != "GZIP" || (r.URL.Query().Get("bill_type") != "SUCCESS" && r.URL.Query().Get("bill_type") != "REFUND") {
				t.Error("bill scope not requested exactly")
			}
			digest := sha1.Sum(f.plain)
			hash := hex.EncodeToString(digest[:])
			url := server.URL + "/v3/billdownload/file?token=private-fixture-token"
			hashType := "SHA1"
			switch mode {
			case "hash":
				hash = strings.Repeat("0", 40)
			case "compressed_hash":
				sum := sha1.Sum(compressed)
				hash = hex.EncodeToString(sum[:])
			case "hash_type":
				hashType = "MD5"
			case "host":
				url = "https://example.test/v3/billdownload/file?token=private-fixture-token"
			case "path":
				url = server.URL + "/private?token=private-fixture-token"
			case "duplicate_token":
				url += "&token=second"
			case "fragment":
				url += "#secret"
			case "http":
				url = strings.Replace(url, "https:", "http:", 1)
			}
			data, _ := json.Marshal(map[string]string{"download_url": url, "hash_type": hashType, "hash_value": hash})
			headers := wctHeaders(t, c, key, data)
			if mode == "signature" {
				headers.Set("Wechatpay-Signature", "bad")
			}
			for name, values := range headers {
				w.Header()[name] = values
			}
			_, _ = w.Write(data)
			return
		}
		if r.URL.Path != "/v3/billdownload/file" {
			t.Error("unexpected download path")
			w.WriteHeader(404)
			return
		}
		f.downloads.Add(1)
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("HTTP layer may silently decompress checksum input")
		}
		switch mode {
		case "redirect":
			w.Header().Set("Location", server.URL+"/must-not-follow")
			w.WriteHeader(302)
			return
		case "crc":
			compressed[len(compressed)-1] ^= 1
		case "members":
			compressed = append(compressed, compressed...)
		case "bomb":
			compressed = wechatBillGZ(t, bytes.Repeat([]byte{'a'}, MaxBillBytes+1))
		case "plain":
			compressed = f.plain
		}
		// Per official protocol this response deliberately has no JSON signature.
		_, _ = w.Write(compressed)
	}))
	t.Cleanup(server.Close)
	c.Gateway = server.URL
	c.HTTPClient = server.Client()
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	f.provider = p.(BillProvider)
	return f
}
func TestWechatBillVerifiesSignedMetadataAndUncompressedChecksum(t *testing.T) {
	f := setupWechatBill(t)
	for _, kind := range []string{"payment", "refund"} {
		bill, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: f.date, Kind: kind})
		if err != nil || bill.Provider != "wechat" || bill.Kind != kind || bill.MerchantID != "1234567890" || !bytes.Equal(bill.Data, f.plain) {
			t.Fatal(kind, err)
		}
	}
	if f.downloads.Load() != 2 {
		t.Fatal("expected signed file downloads")
	}
}
func TestWechatBillRejectsUnverifiedMetadataBeforeDownloading(t *testing.T) {
	f := setupWechatBill(t)
	for _, mode := range []string{"signature", "hash_type", "host", "path", "duplicate_token", "fragment", "http"} {
		f.mode.Store(mode)
		before := f.downloads.Load()
		_, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: f.date, Kind: "payment"})
		if err == nil || strings.Contains(err.Error(), "private-fixture-token") || f.downloads.Load() != before {
			t.Fatal(mode, err)
		}
	}
}
func TestWechatBillRejectsWrongHashCorruptOrOversizedFiles(t *testing.T) {
	f := setupWechatBill(t)
	for _, mode := range []string{"hash", "compressed_hash", "redirect", "crc", "members", "bomb", "plain"} {
		f.mode.Store(mode)
		if _, err := f.provider.DownloadBill(context.Background(), BillRequest{Date: f.date, Kind: "payment"}); err == nil {
			t.Fatal("invalid bill accepted", mode)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.provider.DownloadBill(ctx, BillRequest{Date: f.date, Kind: "payment"}); err == nil {
		t.Fatal("cancelled download succeeded")
	}
}
