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
	"testing"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
)

const financialHTTPReferenceHeader = "payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan\n"

type financialHTTPRow struct {
	PaymentID                 string   `json:"payment_id"`
	FeeYuan                   *string  `json:"fee_yuan"`
	ChannelAmountYuan         *string  `json:"channel_amount_yuan"`
	ObservedFeeYuan           *string  `json:"observed_fee_yuan"`
	ExpectedFeeYuan           *string  `json:"expected_fee_yuan"`
	ObservedChannelAmountYuan *string  `json:"observed_channel_amount_yuan"`
	ExpectedChannelAmountYuan *string  `json:"expected_channel_amount_yuan"`
	Status                    string   `json:"status"`
	Codes                     []string `json:"codes"`
}
type financialHTTPRun struct {
	ID                string `json:"id"`
	SourceID          string `json:"source_id"`
	Revision          int    `json:"revision"`
	MatchedCount      int    `json:"matched_count"`
	DifferenceCount   int    `json:"difference_count"`
	UnassessedCount   int    `json:"unassessed_count"`
	ReferenceRowCount int    `json:"reference_row_count"`
}
type financialHTTPSnapshot struct {
	Available bool               `json:"available"`
	SourceID  string             `json:"source_id"`
	Basis     string             `json:"basis"`
	Rows      []financialHTTPRow `json:"rows"`
	Runs      []financialHTTPRun `json:"runs"`
}
type financialHTTPReport struct {
	Run  financialHTTPRun   `json:"run"`
	Rows []financialHTTPRow `json:"rows"`
}

func financialHTTPPath(statementID, sourceID string) string {
	return "/api/v1/admin/statements/" + statementID + "/sources/" + sourceID + "/financials"
}
func financialHTTPSource(t *testing.T, f fixture, actor store.User, paymentID string) store.StatementDetail {
	t.Helper()
	detail, err := f.db.ImportNativeStatement(context.Background(), actor.ID, statements.NativeImport{Provider: "wechat", Kind: "payment", Date: "2026-10-27", Coverage: "full", Format: "wechat_success_v1", ContentBase64: base64.StdEncoding.EncodeToString([]byte(nativeHTTPWechatCSV(t, paymentID)))})
	if err != nil || len(detail.Sources) != 1 {
		t.Fatal("native financial fixture", err)
	}
	return detail
}
func financialHTTPRead(t *testing.T, f fixture, cookie *http.Cookie, path string, target any) []byte {
	t.Helper()
	code, body, _ := request(t, f, "GET", path, "", cookie)
	if code != http.StatusOK || json.Unmarshal(body, target) != nil {
		t.Fatal("financial read", path, code, string(body))
	}
	return body
}
func financialHTTPCompare(t *testing.T, f fixture, cookie *http.Cookie, path, csv string) financialHTTPReport {
	t.Helper()
	code, body, _ := request(t, f, "POST", path+"/reconcile", accountBody(map[string]string{"csv": csv}), cookie)
	var report financialHTTPReport
	if code != http.StatusOK || json.Unmarshal(body, &report) != nil || report.Run.ID == "" {
		t.Fatal("financial comparison", code, string(body))
	}
	return report
}

func TestStatementFinancialsHTTPAuthorityMatrixAndIndependentDefaultGates(t *testing.T) {
	for _, role := range []string{"admin", "finance", "support", "operator", "user"} {
		t.Run(role, func(t *testing.T) {
			f := nativeBillHTTPSetup(t, false)
			_, cookie := staffAccount(t, f, role)
			path := financialHTTPPath(store.ID(), store.ID())
			want := http.StatusForbidden
			if role == "admin" || role == "finance" {
				want = http.StatusNotFound
			}
			for _, endpoint := range []struct{ method, path, body string }{
				{"GET", path, ""},
				{"GET", path + "/runs/" + store.ID(), ""},
				{"POST", path + "/reconcile", accountBody(map[string]string{"csv": financialHTTPReferenceHeader})},
			} {
				code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
				if code != want {
					t.Fatal(endpoint.method, endpoint.path, code, want, string(body))
				}
			}
		})
	}
}

func TestStatementFinancialsHTTPSourcePrecisionReferenceDedupAndNoMoneyMutation(t *testing.T) {
	f := nativeBillHTTPSetup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	order := paymentOrder(t, f, actor)
	payment, err := f.db.CreatePayment(context.Background(), actor.ID, order.ID, "wechat")
	if err != nil {
		t.Fatal(err)
	}
	detail := financialHTTPSource(t, f, actor, payment.ID)
	path := financialHTTPPath(detail.Statement.ID, detail.Sources[0].ID)
	var snapshot financialHTTPSnapshot
	body := financialHTTPRead(t, f, cookie, path, &snapshot)
	if !snapshot.Available || snapshot.Basis != "wechat_settlement_order" || len(snapshot.Rows) != 1 || snapshot.Rows[0].FeeYuan == nil || *snapshot.Rows[0].FeeYuan != "0.01000" || snapshot.Rows[0].ChannelAmountYuan == nil || *snapshot.Rows[0].ChannelAmountYuan != "19.00000" {
		t.Fatal("financial fields rounded or mislabelled", string(body))
	}
	for _, secret := range []string{"sensitive-merchant-private-token", "private-fixture-user", "merchant_fingerprint", "content_base64"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("unrelated raw financial source content leaked")
		}
	}
	reference := financialHTTPReferenceHeader + payment.ID + ",,0.01001,19.00000\n"
	report := financialHTTPCompare(t, f, cookie, path, reference)
	if report.Run.DifferenceCount != 1 || len(report.Rows) != 1 || report.Rows[0].ExpectedFeeYuan == nil || *report.Rows[0].ExpectedFeeYuan != "0.01001" || len(report.Rows[0].Codes) != 1 || report.Rows[0].Codes[0] != "fee_mismatch" {
		t.Fatal("five-decimal difference lost", report)
	}
	repeated := financialHTTPCompare(t, f, cookie, path, "\ufeff"+strings.ReplaceAll(reference, "\n", "\r\n"))
	if repeated.Run.ID != report.Run.ID || repeated.Run.Revision != report.Run.Revision {
		t.Fatal("semantic duplicate created another financial report")
	}
	var stored financialHTTPReport
	financialHTTPRead(t, f, cookie, path+"/runs/"+report.Run.ID, &stored)
	if stored.Run.ID != report.Run.ID || len(stored.Rows) != 1 || stored.Rows[0].Status != "difference" {
		t.Fatal("immutable financial report unavailable")
	}
	var status string
	var paymentEvents, grants, actions int
	err = f.db.DB.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM payment_events),(SELECT count(*) FROM entitlement_periods),(SELECT count(*) FROM payment_action_jobs) FROM payment_intents WHERE id=$1`, payment.ID).Scan(&status, &paymentEvents, &grants, &actions)
	if err != nil || status != "pending" || paymentEvents != 0 || grants != 0 || actions != 0 {
		t.Fatal("reference comparison changed financial facts", status, paymentEvents, grants, actions, err)
	}
}

func TestStatementFinancialsHTTPLegacyUnknownAndSourceParentIsolation(t *testing.T) {
	f := nativeBillHTTPSetup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	legacy, err := f.db.ImportStatement(context.Background(), actor.ID, statements.Import{Provider: "wechat", Kind: "payment", Date: "2026-10-27", Coverage: "partial", CSV: statementCSVHeader})
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := financialHTTPPath(legacy.Statement.ID, legacy.Sources[0].ID)
	var snapshot financialHTTPSnapshot
	body := financialHTTPRead(t, f, cookie, legacyPath, &snapshot)
	if snapshot.Available || len(snapshot.Rows) != 0 || len(snapshot.Runs) != 0 {
		t.Fatal("legacy source represented as zero-valued financial evidence", string(body))
	}
	if code, body, _ := request(t, f, "POST", legacyPath+"/reconcile", accountBody(map[string]string{"csv": financialHTTPReferenceHeader}), cookie); code != http.StatusConflict {
		t.Fatal("legacy source accepted a financial comparison", code, string(body))
	}
	native := financialHTTPSource(t, f, actor, store.ID())
	nativePath := financialHTTPPath(native.Statement.ID, native.Sources[0].ID)
	report := financialHTTPCompare(t, f, cookie, nativePath, financialHTTPReferenceHeader)
	if report.Run.UnassessedCount != 1 || report.Run.MatchedCount != 0 || len(report.Rows) != 1 || report.Rows[0].Status != "unassessed" {
		t.Fatal("missing reference falsely matched", report)
	}
	wrongParent := financialHTTPPath(legacy.Statement.ID, native.Sources[0].ID)
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", wrongParent, ""},
		{"POST", wrongParent + "/reconcile", accountBody(map[string]string{"csv": financialHTTPReferenceHeader})},
		{"GET", wrongParent + "/runs/" + report.Run.ID, ""},
		{"GET", nativePath + "/runs/" + store.ID(), ""},
	} {
		code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
		if code != http.StatusNotFound {
			t.Fatal("nested source/report parent escaped", endpoint.path, code, string(body))
		}
	}

	// Empty fields in a recognized Alipay archive remain unknown observations,
	// even when the reference explicitly declares zero.
	values := make([]string, 25)
	values[0], values[1], values[2], values[4], values[5], values[11] = "alipay-transaction", "alipay-payment", "交易", "2026-10-27 11:00:00", "2026-10-27 11:00:00", "19.00"
	var csvText strings.Builder
	w := csv.NewWriter(&csvText)
	_ = w.Write(strings.Split("支付宝交易号,商户订单号,业务类型,商品名称,创建时间,完成时间,门店编号,门店名称,操作员,终端号,对方账户,订单金额（元）,商家实收（元）,支付宝红包（元）,集分宝（元）,支付宝优惠（元）,商家优惠（元）,券核销金额（元）,券名称,商家红包消费金额（元）,卡消费金额（元）,退款批次号/请求号,服务费（元）,分润（元）,备注", ","))
	_ = w.Write(values)
	w.Flush()
	alipay, err := f.db.ImportNativeStatement(context.Background(), actor.ID, statements.NativeImport{Provider: "alipay", Kind: "payment", Date: "2026-10-27", Coverage: "partial", Format: "alipay_trade_v1", ContentBase64: base64.StdEncoding.EncodeToString([]byte(csvText.String()))})
	if err != nil {
		t.Fatal(err)
	}
	unknown := financialHTTPCompare(t, f, cookie, financialHTTPPath(alipay.Statement.ID, alipay.Sources[0].ID), financialHTTPReferenceHeader+"alipay-payment,,0,0\n")
	if unknown.Run.DifferenceCount != 1 || unknown.Run.MatchedCount != 0 || len(unknown.Rows) != 1 || unknown.Rows[0].ObservedFeeYuan != nil || unknown.Rows[0].ObservedChannelAmountYuan != nil || len(unknown.Rows[0].Codes) != 2 {
		t.Fatal("blank source values treated as zero", unknown)
	}
}

func TestStatementFinancialsHTTPStrictInputBoundsQueryAndFiniteErrors(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"csv":null}`, `{"csv":"","source_id":"browser-authority"}`, `{"csv":""} {"csv":""}`,
		accountBody(map[string]string{"csv": strings.Repeat("x", (1<<20)+1)}),
		accountBody(map[string]string{"csv": financialHTTPReferenceHeader + "private-sensitive-ref,,1.234567,1\n"}),
		accountBody(map[string]string{"csv": financialHTTPReferenceHeader + "private-sensitive-ref,,,1\n"}),
		accountBody(map[string]string{"csv": strings.Repeat("x", (2<<20)+1)}),
	} {
		t.Run("invalid", func(t *testing.T) {
			f := nativeBillHTTPSetup(t, false)
			actor, cookie := staffAccount(t, f, "finance")
			detail := financialHTTPSource(t, f, actor, store.ID())
			path := financialHTTPPath(detail.Statement.ID, detail.Sources[0].ID)
			code, reply, _ := request(t, f, "POST", path+"/reconcile", body, cookie)
			if code != http.StatusBadRequest || bytes.Contains(reply, []byte("private-sensitive-ref")) || bytes.Contains(reply, []byte("browser-authority")) {
				t.Fatal("unsafe reference error", code, string(reply))
			}
		})
	}
	f := nativeBillHTTPSetup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	detail := financialHTTPSource(t, f, actor, store.ID())
	path := financialHTTPPath(detail.Statement.ID, detail.Sources[0].ID)
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", path + "?cursor=untrusted", ""},
		{"POST", path + "/reconcile?source=other", accountBody(map[string]string{"csv": financialHTTPReferenceHeader})},
		{"GET", path + "/runs/" + store.ID() + "?limit=1", ""},
	} {
		code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
		if code != http.StatusBadRequest {
			t.Fatal("query parameters accepted", code, string(body))
		}
	}
	code, body, _ := request(t, f, "GET", financialHTTPPath("not-an-id", detail.Sources[0].ID), "", cookie)
	if code != http.StatusNotFound {
		t.Fatal("malformed parent accepted", code, string(body))
	}
}

func TestStatementFinancialsHTTPOriginJSONSessionAndVerifiedEmail(t *testing.T) {
	f := nativeBillHTTPSetup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	detail := financialHTTPSource(t, f, actor, store.ID())
	path := financialHTTPPath(detail.Statement.ID, detail.Sources[0].ID)
	for _, test := range []struct {
		origin, contentType string
		cookie              bool
		want                int
	}{
		{"", "application/json", true, http.StatusForbidden},
		{"https://untrusted.example.test", "application/json", true, http.StatusForbidden},
		{origin, "text/csv", true, http.StatusUnsupportedMediaType},
		{origin, "application/json", false, http.StatusUnauthorized},
	} {
		req, err := http.NewRequest("POST", f.server.URL+path+"/reconcile", strings.NewReader(accountBody(map[string]string{"csv": financialHTTPReferenceHeader})))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Content-Type", test.contentType)
		if test.cookie {
			req.AddCookie(cookie)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		reply, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != test.want {
			t.Fatal(response.StatusCode, string(reply), readErr)
		}
	}
	server := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4, RequireVerified: true}).Handler())
	t.Cleanup(server.Close)
	verifiedOnly := fixture{f.db, server}
	for _, endpoint := range []struct{ method, path, body string }{{"GET", path, ""}, {"GET", path + "/runs/" + store.ID(), ""}, {"POST", path + "/reconcile", accountBody(map[string]string{"csv": financialHTTPReferenceHeader})}} {
		code, reply, _ := request(t, verifiedOnly, endpoint.method, endpoint.path, endpoint.body, cookie)
		if code != http.StatusForbidden || !bytes.Contains(reply, []byte("邮箱验证")) {
			t.Fatal("unverified financial access accepted", endpoint.method, code, string(reply))
		}
	}
}

func TestStatementFinancialsHTTPSharedLimitsAndStorageErrorsRemainPrivate(t *testing.T) {
	t.Run("shared_limits", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, false)
		actor, first := staffAccount(t, f, "finance")
		_, second := staffAccount(t, f, "finance")
		detail := financialHTTPSource(t, f, actor, store.ID())
		path := financialHTTPPath(detail.Statement.ID, detail.Sources[0].ID) + "/reconcile"
		body := accountBody(map[string]string{"csv": financialHTTPReferenceHeader})
		for i := 0; i < 5; i++ {
			if code, reply, _ := request(t, f, "POST", path, body, first); code != http.StatusOK {
				t.Fatal(i, code, string(reply))
			}
		}
		if code, reply, _ := request(t, f, "POST", path, body, second); code != http.StatusTooManyRequests {
			t.Fatal("second user bypassed peer limit", code, string(reply))
		}
		handler := httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4}).Handler()
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.80:23456"
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(first)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != http.StatusTooManyRequests {
			t.Fatal("fresh peer bypassed user bucket", result.Code, result.Body.String())
		}
	})
	t.Run("limiter_failure", func(t *testing.T) {
		f := nativeBillHTTPSetup(t, false)
		_, cookie := staffAccount(t, f, "finance")
		if _, err := f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_financial_rate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'sensitive fixture rate failure'; END $$; CREATE TRIGGER reject_financial_rate BEFORE INSERT ON rate_limit_buckets FOR EACH ROW EXECUTE FUNCTION reject_financial_rate()`); err != nil {
			t.Fatal(err)
		}
		path := financialHTTPPath(store.ID(), store.ID())
		code, body, _ := request(t, f, "POST", path+"/reconcile", accountBody(map[string]string{"csv": financialHTTPReferenceHeader}), cookie)
		if code != http.StatusServiceUnavailable || bytes.Contains(body, []byte("sensitive fixture")) {
			t.Fatal("limiter failure incorrectly disclosed", code, string(body))
		}
	})
}
