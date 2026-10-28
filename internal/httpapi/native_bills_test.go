package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type nativeHTTPBillProvider struct {
	paymentFake
	calls atomic.Int32
}

func (p *nativeHTTPBillProvider) BillMerchantFingerprint() string { return strings.Repeat("a", 64) }
func (p *nativeHTTPBillProvider) DownloadBill(context.Context, payments.BillRequest) (payments.Bill, error) {
	p.calls.Add(1)
	return payments.Bill{}, payments.ErrBillUnsupported
}

func nativeBillHTTPSetup(t *testing.T, enabled bool, providers ...payments.Provider) fixture {
	t.Helper()
	s := testutil.Database(t)
	registry, err := payments.New(providers...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.New(s, httpapi.Config{Origin: origin, PasswordCost: 4, Payments: registry, BillDownloadsEnabled: enabled}).Handler())
	t.Cleanup(server.Close)
	return fixture{s, server}
}

func nativeHTTPWechatCSV(t *testing.T, paymentID string) string {
	t.Helper()
	var text strings.Builder
	writer := csv.NewWriter(&text)
	header := "交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,商品名称,商户数据包,手续费,费率,订单金额,费率备注"
	row := []string{"2026-10-27 12:01:02", "fixture-app", "fixture-merchant", "0", "", "external-provider-transaction", paymentID, "private-fixture-user", "NATIVE", "SUCCESS", "BANK", "CNY", "19.00", "0.01", "sensitive-merchant-private-token", "", "0.01000", "0.60%", "19.01", ""}
	summary := []string{"1", "19.00", "0.00", "0.00", "0.01000", "19.01", "0.00"}
	for i := range row {
		row[i] = "`" + row[i]
	}
	for i := range summary {
		summary[i] = "`" + summary[i]
	}
	for _, values := range [][]string{strings.Split(header, ","), row, strings.Split("总交易单数,应结订单总金额,退款总金额,充值券退款总金额,手续费总金额,订单总金额,申请退款总金额", ","), summary} {
		if err := writer.Write(values); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func nativeHTTPInput(csv string) map[string]string {
	return map[string]string{"provider": "wechat", "kind": "payment", "date": "2026-10-27", "coverage": "full", "format": "wechat_success_v1", "content_base64": base64.StdEncoding.EncodeToString([]byte(csv))}
}

func nativeHTTPDownloadBody(provider string) string {
	date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).AddDate(0, 0, -1).Format("2006-01-02")
	return accountBody(map[string]string{"provider": provider, "kind": "payment", "date": date})
}

func TestNativeBillsHTTPAuthorityAndSeparateDownloadGate(t *testing.T) {
	for _, role := range []string{"admin", "finance", "support", "operator", "user"} {
		t.Run(role, func(t *testing.T) {
			provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
			f := nativeBillHTTPSetup(t, true, provider)
			_, cookie := staffAccount(t, f, role)
			want := http.StatusForbidden
			if role == "admin" || role == "finance" {
				want = http.StatusOK
			}
			for _, endpoint := range []struct{ method, path, body string }{
				{"GET", "/api/v1/admin/bill-downloads", ""},
				{"POST", "/api/v1/admin/bill-downloads", nativeHTTPDownloadBody("wechat")},
				{"POST", "/api/v1/admin/statements/native", accountBody(nativeHTTPInput(nativeHTTPWechatCSV(t, store.ID())))},
			} {
				code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
				if code != want {
					t.Fatal(endpoint.method, endpoint.path, code, want, string(body))
				}
			}
			if provider.calls.Load() != 0 {
				t.Fatal("browser enqueue handler performed a merchant call")
			}
		})
	}
}

func TestNativeBillsHTTPUploadRetainsObservationProvenanceWithoutChangingMoney(t *testing.T) {
	f := nativeBillHTTPSetup(t, false)
	user, finance := staffAccount(t, f, "finance")
	order := paymentOrder(t, f, user)
	payment, err := f.db.CreatePayment(context.Background(), user.ID, order.ID, "wechat")
	if err != nil {
		t.Fatal(err)
	}
	body := accountBody(nativeHTTPInput(nativeHTTPWechatCSV(t, payment.ID)))
	var first store.StatementDetail
	for i := 0; i < 2; i++ {
		code, reply, _ := request(t, f, "POST", "/api/v1/admin/statements/native", body, finance)
		if code != http.StatusOK || bytes.Contains(reply, []byte("sensitive-merchant-private-token")) || bytes.Contains(reply, []byte("private-fixture-user")) {
			t.Fatal(code, string(reply))
		}
		var detail store.StatementDetail
		if err := json.Unmarshal(reply, &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Statement.Total != 1901 || detail.LatestRun.DifferenceCount != 1 || len(detail.Sources) != 1 || detail.Sources[0].Kind != "upload" || detail.Sources[0].Integrity != "unverified_upload" || detail.Sources[0].Format != "wechat_success_v1" || detail.Sources[0].DownloadJobID != nil {
			t.Fatal("native gross amount or source provenance lost", detail)
		}
		if i == 0 {
			first = detail
		} else if first.Statement.ID != detail.Statement.ID || first.LatestRun.ID != detail.LatestRun.ID {
			t.Fatal("duplicate native observations created a second ledger/report")
		}
	}
	var paymentStatus, orderStatus string
	var facts, periods, actions int
	err = f.db.DB.QueryRow(context.Background(), `SELECT p.status,o.status,(SELECT count(*) FROM payment_events),(SELECT count(*) FROM entitlement_periods),(SELECT count(*) FROM payment_action_jobs) FROM payment_intents p JOIN orders o ON o.id=p.order_id WHERE p.id=$1`, payment.ID).Scan(&paymentStatus, &orderStatus, &facts, &periods, &actions)
	if err != nil || paymentStatus != "pending" || orderStatus != "pending" || facts != 0 || periods != 0 || actions != 0 {
		t.Fatal("untrusted file changed financial facts", paymentStatus, orderStatus, facts, periods, actions, err)
	}
	code, reply, _ := request(t, f, "POST", "/api/v1/admin/bill-downloads", nativeHTTPDownloadBody("wechat"), finance)
	if code != http.StatusServiceUnavailable {
		t.Fatal("download gate bypassed", code, string(reply))
	}
	code, reply, _ = request(t, f, "GET", "/api/v1/admin/bill-downloads", "", finance)
	if code != http.StatusOK || !bytes.Contains(reply, []byte(`"items":[]`)) {
		t.Fatal("disabled download history unavailable", code, string(reply))
	}
}

func TestNativeBillsHTTPQueueIsIdempotentAndDoesNotExposeIdentityOrCallNetwork(t *testing.T) {
	provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
	f := nativeBillHTTPSetup(t, true, provider)
	_, finance := staffAccount(t, f, "finance")
	var id string
	for i := 0; i < 2; i++ {
		code, body, _ := request(t, f, "POST", "/api/v1/admin/bill-downloads", nativeHTTPDownloadBody("wechat"), finance)
		if code != http.StatusOK || bytes.Contains(body, []byte("merchant_fingerprint")) || bytes.Contains(body, []byte("actor_version")) || bytes.Contains(body, []byte("claim_token")) {
			t.Fatal(code, string(body))
		}
		var job store.BillDownloadJob
		if err := json.Unmarshal(body, &job); err != nil || job.Status != "queued" || job.StatementID != nil || job.CompletedAt != nil || job.NextAttemptAt == nil {
			t.Fatal("invalid queued state", job, err)
		}
		if i == 0 {
			id = job.ID
		} else if job.ID != id {
			t.Fatal("duplicate request created a second remote job")
		}
	}
	if provider.calls.Load() != 0 {
		t.Fatal("HTTP handler called remote merchant")
	}
	code, body, _ := request(t, f, "GET", "/api/v1/admin/bill-downloads", "", finance)
	var list struct {
		Items []store.BillDownloadJob `json:"items"`
	}
	if err := json.Unmarshal(body, &list); code != http.StatusOK || err != nil || len(list.Items) != 1 || list.Items[0].ID != id {
		t.Fatal(code, string(body), err)
	}
}

func TestNativeBillsHTTPRejectsBadFormatsArchivesAndStrictBodyBoundaries(t *testing.T) {
	valid := nativeHTTPInput(nativeHTTPWechatCSV(t, store.ID()))
	bad := func(key, value string) string {
		copy := make(map[string]string, len(valid))
		for k, v := range valid {
			copy[k] = v
		}
		copy[key] = value
		return accountBody(copy)
	}
	for name, body := range map[string]string{
		"unknown_field":       strings.TrimSuffix(accountBody(valid), "}") + `,"download_url":"https://private.example.test/sensitive-private-fixture"}`,
		"multiple_objects":    accountBody(valid) + `{}`,
		"invalid_base64":      bad("content_base64", "sensitive-private-fixture"),
		"unsupported_version": bad("format", "sensitive-private-fixture"),
		"broken_summary":      bad("content_base64", base64.StdEncoding.EncodeToString([]byte(strings.ReplaceAll(nativeHTTPWechatCSV(t, store.ID()), "`1,`19.00", "`2,`19.00")))),
		"broken_archive":      bad("content_base64", base64.StdEncoding.EncodeToString([]byte{0x1f, 0x8b, 0x08})),
		"oversized_file":      bad("content_base64", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), statements.MaxNativeBytes+1))),
		"oversized_request":   bad("content_base64", strings.Repeat("x", (12<<20)+1)),
		"null":                "null",
	} {
		t.Run(name, func(t *testing.T) {
			f := nativeBillHTTPSetup(t, false)
			_, finance := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "POST", "/api/v1/admin/statements/native", body, finance)
			if code != http.StatusBadRequest || bytes.Contains(reply, []byte("sensitive-private-fixture")) {
				t.Fatal(code, string(reply))
			}
			if name == "broken_summary" && !bytes.Contains(reply, []byte("汇总")) {
				t.Fatal("summary failure lacks safe actionable message", string(reply))
			}
			var count int
			if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM merchant_statements`).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected native import left evidence", count, err)
			}
		})
	}
}

func TestNativeBillsHTTPRejectsUnsupportedOrUnconfiguredDownloadsAndEditableInputs(t *testing.T) {
	valid := nativeHTTPDownloadBody("wechat")
	today := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
	for name, config := range map[string]struct {
		body      string
		providers []payments.Provider
		want      int
	}{
		"not_configured":     {valid, nil, http.StatusServiceUnavailable},
		"no_bill_capability": {valid, []payments.Provider{&paymentFake{id: "wechat"}}, http.StatusConflict},
		"epay":               {nativeHTTPDownloadBody("epay_alipay"), nil, http.StatusConflict},
		"current_day":        {accountBody(map[string]string{"provider": "wechat", "kind": "payment", "date": today}), nil, http.StatusBadRequest},
		"invalid_date":       {accountBody(map[string]string{"provider": "wechat", "kind": "payment", "date": "2026-02-30"}), nil, http.StatusBadRequest},
		"invalid_kind":       {accountBody(map[string]string{"provider": "wechat", "kind": "transfer", "date": "2026-10-27"}), nil, http.StatusBadRequest},
		"caller_url":         {strings.TrimSuffix(valid, "}") + `,"download_url":"https://127.0.0.1/private"}`, nil, http.StatusBadRequest},
		"caller_fingerprint": {strings.TrimSuffix(valid, "}") + `,"merchant_fingerprint":"` + strings.Repeat("f", 64) + `"}`, nil, http.StatusBadRequest},
		"multiple_objects":   {valid + `{}`, nil, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			f := nativeBillHTTPSetup(t, true, config.providers...)
			_, finance := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "POST", "/api/v1/admin/bill-downloads", config.body, finance)
			if code != config.want {
				t.Fatal(code, config.want, string(reply))
			}
			var count int
			if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM bill_download_jobs`).Scan(&count); err != nil || count != 0 {
				t.Fatal("invalid request queued remote work", count, err)
			}
		})
	}
}

func TestNativeBillsHTTPRejectsUnknownQueriesAndRequiresBrowserOrigin(t *testing.T) {
	provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
	f := nativeBillHTTPSetup(t, true, provider)
	_, finance := staffAccount(t, f, "finance")
	for _, ep := range []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/bill-downloads?provider=wechat", ""},
		{"POST", "/api/v1/admin/bill-downloads?limit=1", nativeHTTPDownloadBody("wechat")},
		{"POST", "/api/v1/admin/statements/native?format=anything", accountBody(nativeHTTPInput(nativeHTTPWechatCSV(t, store.ID())))},
	} {
		code, body, _ := request(t, f, ep.method, ep.path, ep.body, finance)
		if code != http.StatusBadRequest {
			t.Fatal(code, string(body))
		}
	}
	for _, endpoint := range []string{"/api/v1/admin/statements/native", "/api/v1/admin/bill-downloads"} {
		for _, input := range []struct {
			origin, contentType string
			cookie              bool
			want                int
		}{
			{"", "application/json", true, http.StatusForbidden},
			{"https://untrusted.example.test", "application/json", true, http.StatusForbidden},
			{origin, "text/csv", true, http.StatusUnsupportedMediaType},
			{origin, "application/json", false, http.StatusUnauthorized},
		} {
			req, err := http.NewRequest("POST", f.server.URL+endpoint, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", input.origin)
			req.Header.Set("Content-Type", input.contentType)
			if input.cookie {
				req.AddCookie(finance)
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			reply, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != input.want {
				t.Fatal(endpoint, response.StatusCode, string(reply), err)
			}
		}
	}
}

func TestNativeBillsHTTPWriteRatesAreIndependentAndLimiterFailureIsUnavailable(t *testing.T) {
	t.Run("purpose_limits", func(t *testing.T) {
		provider := &nativeHTTPBillProvider{paymentFake: paymentFake{id: "wechat"}}
		f := nativeBillHTTPSetup(t, true, provider)
		_, finance := staffAccount(t, f, "finance")
		input := accountBody(nativeHTTPInput(nativeHTTPWechatCSV(t, store.ID())))
		for _, ep := range []struct{ path, body string }{{"/api/v1/admin/statements/native", input}, {"/api/v1/admin/bill-downloads", nativeHTTPDownloadBody("wechat")}} {
			for i := 0; i < 5; i++ {
				code, body, _ := request(t, f, "POST", ep.path, ep.body, finance)
				if code != http.StatusOK {
					t.Fatal(i, ep.path, code, string(body))
				}
			}
			code, body, _ := request(t, f, "POST", ep.path, ep.body, finance)
			if code != http.StatusTooManyRequests {
				t.Fatal(ep.path, code, string(body))
			}
		}
		if provider.calls.Load() != 0 {
			t.Fatal("rate test reached merchant network")
		}
	})
	t.Run("limiter_failure", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, true)
		_, finance := staffAccount(t, f, "finance")
		_, err := f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_native_rate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'private fixture failure'; END $$; CREATE TRIGGER reject_native_rate BEFORE INSERT ON rate_limit_buckets FOR EACH ROW EXECUTE FUNCTION reject_native_rate()`)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/admin/statements/native", "/api/v1/admin/bill-downloads"} {
			code, body, _ := request(t, f, "POST", path, `{}`, finance)
			if code != http.StatusServiceUnavailable || bytes.Contains(body, []byte("private fixture failure")) {
				t.Fatal(path, code, string(body))
			}
		}
	})
}
