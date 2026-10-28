package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
)

const statementCSVHeader = "payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at\n"

type statementHTTPResult struct {
	Statement struct {
		ID string `json:"id"`
	} `json:"statement"`
	LatestRun struct {
		ID                  string `json:"id"`
		Revision            int    `json:"revision"`
		MatchedCount        int    `json:"matched_count"`
		DifferenceCount     int    `json:"difference_count"`
		LocalCandidateCount int    `json:"local_candidate_count"`
	} `json:"latest_run"`
}

type statementHTTPFindings struct {
	Items []struct {
		ID           string   `json:"id"`
		Codes        []string `json:"codes"`
		ReviewCount  int      `json:"review_count"`
		LatestReview *struct {
			Note string `json:"note"`
		} `json:"latest_review"`
	} `json:"items"`
	Total int `json:"total"`
}

func statementHTTPBody(provider, csv string) string {
	return accountBody(map[string]string{"provider": provider, "kind": "payment", "date": "2026-10-27", "coverage": "partial", "csv": csv})
}

func importHTTPStatement(t *testing.T, f fixture, cookie *http.Cookie, provider, csv string) statementHTTPResult {
	t.Helper()
	status, body, _ := request(t, f, "POST", "/api/v1/admin/statements", statementHTTPBody(provider, csv), cookie)
	if status != http.StatusOK {
		t.Fatalf("statement import %d %s", status, body)
	}
	var detail statementHTTPResult
	if err := json.Unmarshal(body, &detail); err != nil || detail.Statement.ID == "" || detail.LatestRun.ID == "" {
		t.Fatalf("invalid statement response: %s (%v)", body, err)
	}
	return detail
}

func findingsHTTPPath(detail statementHTTPResult) string {
	return "/api/v1/admin/statements/" + detail.Statement.ID + "/runs/" + detail.LatestRun.ID + "/findings"
}

func readHTTPFindings(t *testing.T, f fixture, cookie *http.Cookie, detail statementHTTPResult, query string) statementHTTPFindings {
	t.Helper()
	status, body, _ := request(t, f, "GET", findingsHTTPPath(detail)+query, "", cookie)
	if status != http.StatusOK {
		t.Fatalf("statement findings %d %s", status, body)
	}
	var page statementHTTPFindings
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestStatementsHTTPAuthorityMatrixAndNoMoneyGateDependency(t *testing.T) {
	for _, role := range []string{"admin", "finance", "support", "operator", "user"} {
		t.Run(role, func(t *testing.T) {
			f := setup(t, false)
			_, cookie := staffAccount(t, f, role)
			allowed := role == "admin" || role == "finance"
			want := http.StatusForbidden
			if allowed {
				want = http.StatusOK
			}
			for _, ep := range []struct{ method, path, body string }{
				{"GET", "/api/v1/admin/statements", ""},
				{"POST", "/api/v1/admin/statements", statementHTTPBody("alipay", statementCSVHeader)},
			} {
				status, body, _ := request(t, f, ep.method, ep.path, ep.body, cookie)
				if status != want {
					t.Fatalf("%s %s: %d want%d: %s", ep.method, ep.path, status, want, body)
				}
			}
			// Each write route must be authorized independently, even before any
			// referenced resource can be looked up.
			if !allowed {
				for _, ep := range []struct{ method, path, body string }{
					{"GET", "/api/v1/admin/statements/" + store.ID(), ""},
					{"POST", "/api/v1/admin/statements/" + store.ID() + "/reconcile", "{}"},
					{"GET", "/api/v1/admin/statements/" + store.ID() + "/runs/" + store.ID() + "/findings", ""},
					{"GET", "/api/v1/admin/statement-findings/" + store.ID() + "/reviews", ""},
					{"POST", "/api/v1/admin/statement-findings/" + store.ID() + "/reviews", `{"note":"核对渠道日期"}`},
				} {
					status, body, _ := request(t, f, ep.method, ep.path, ep.body, cookie)
					if status != http.StatusForbidden {
						t.Fatalf("role %s reached %s %s: %d %s", role, ep.method, ep.path, status, body)
					}
				}
			}
		})
	}
}

func TestStatementsHTTPUntrustedImportNeverSettlesOrChangesAccess(t *testing.T) {
	f := setup(t, true)
	_, finance := staffAccount(t, f, "finance")
	buyer, _ := register(t, f)
	order := paymentOrder(t, f, buyer)
	payment, err := f.db.CreatePayment(context.Background(), buyer.ID, order.ID, "alipay")
	if err != nil {
		t.Fatal(err)
	}
	csv := statementCSVHeader + payment.ID + ",imported-transaction,,,1900,CNY,2026-10-27T12:00:00+08:00\n"
	detail := importHTTPStatement(t, f, finance, "alipay", csv)
	if detail.LatestRun.MatchedCount != 0 || detail.LatestRun.DifferenceCount != 1 {
		t.Fatal("pending payment accepted as proof", detail)
	}
	page := readHTTPFindings(t, f, finance, detail, "")
	if len(page.Items) != 1 || !slices.Contains(page.Items[0].Codes, "not_confirmed") {
		t.Fatal("pending payment discrepancy missing", page)
	}
	var status, paymentStatus string
	var periods, evidence, jobs int
	err = f.db.DB.QueryRow(context.Background(), `SELECT o.status,p.status,
 (SELECT count(*) FROM entitlement_periods WHERE order_id=o.id),
 (SELECT count(*) FROM payment_events WHERE order_id=o.id),
 (SELECT count(*) FROM payment_action_jobs WHERE payment_id=p.id)
 FROM orders o JOIN payment_intents p ON p.order_id=o.id WHERE o.id=$1`, order.ID).Scan(&status, &paymentStatus, &periods, &evidence, &jobs)
	if err != nil || status != "pending" || paymentStatus != "pending" || periods != 0 || evidence != 0 || jobs != 0 {
		t.Fatal("import changed financial facts or rights", status, paymentStatus, periods, evidence, jobs, err)
	}
	// Only the separate trusted settlement path can change payment state.
	if err = f.db.SettlePayment(context.Background(), payment.ID, "alipay", "imported-transaction", order.Price, order.Currency); err != nil {
		t.Fatal(err)
	}
	code, body, _ := request(t, f, "POST", "/api/v1/admin/statements/"+detail.Statement.ID+"/reconcile", "{}", finance)
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var run struct {
		ID              string `json:"id"`
		Revision        int    `json:"revision"`
		MatchedCount    int    `json:"matched_count"`
		DifferenceCount int    `json:"difference_count"`
	}
	if err = json.Unmarshal(body, &run); err != nil || run.Revision != 2 || run.MatchedCount != 1 || run.DifferenceCount != 0 || run.ID == detail.LatestRun.ID {
		t.Fatal("new reconciliation did not snapshot trusted payment", string(body), err)
	}
	old := readHTTPFindings(t, f, finance, detail, "")
	if len(old.Items) != 1 || !slices.Contains(old.Items[0].Codes, "not_confirmed") {
		t.Fatal("rerun overwrote old findings", old)
	}
}

func TestStatementsHTTPFindingsOwnershipPaginationAndReviewHistory(t *testing.T) {
	f := setup(t, false)
	_, admin := staffAccount(t, f, "admin")
	id := store.ID()
	csv := statementCSVHeader + id + ",outside-transaction,,,1900,CNY,2026-10-27T12:00:00+08:00\n"
	first := importHTTPStatement(t, f, admin, "alipay", csv)
	second := importHTTPStatement(t, f, admin, "wechat", csv)
	code, body, _ := request(t, f, "GET", "/api/v1/admin/statements/"+first.Statement.ID+"/runs/"+second.LatestRun.ID+"/findings", "", admin)
	if code != http.StatusNotFound {
		t.Fatal("foreign run accepted", code, string(body))
	}
	page := readHTTPFindings(t, f, admin, first, "?review=unreviewed&offset=0&limit=1")
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatal(page)
	}
	path := "/api/v1/admin/statement-findings/" + page.Items[0].ID + "/reviews"
	for _, note := range []string{"  已核对渠道交易号，待核实原始记录  ", "补充核对：需检查跨日入账"} {
		code, body, _ = request(t, f, "POST", path, accountBody(map[string]string{"note": note}), admin)
		if code != http.StatusOK {
			t.Fatal(code, string(body))
		}
	}
	code, body, _ = request(t, f, "GET", path, "", admin)
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var reviews struct {
		Items []struct {
			Note string `json:"note"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &reviews); err != nil || len(reviews.Items) != 2 || !slices.ContainsFunc(reviews.Items, func(item struct {
		Note string `json:"note"`
	}) bool {
		return item.Note == "已核对渠道交易号，待核实原始记录"
	}) {
		t.Fatal("review history lost", string(body), err)
	}
	reviewed := readHTTPFindings(t, f, admin, first, "?review=reviewed")
	if reviewed.Total != 1 || reviewed.Items[0].ReviewCount != 2 || reviewed.Items[0].LatestReview == nil {
		t.Fatal("review state missing", reviewed)
	}
	if page := readHTTPFindings(t, f, admin, first, "?review=unreviewed"); page.Total != 0 || len(page.Items) != 0 {
		t.Fatal("review filter ignored", page)
	}
	code, body, _ = request(t, f, "POST", "/api/v1/admin/statements/"+first.Statement.ID+"/reconcile", "{}", admin)
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var next struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &next)
	first.LatestRun.ID = next.ID
	fresh := readHTTPFindings(t, f, admin, first, "?review=unreviewed")
	if fresh.Total != 1 || fresh.Items[0].ReviewCount != 0 || fresh.Items[0].LatestReview != nil {
		t.Fatal("old review hid a new snapshot difference", fresh)
	}
}

func TestStatementsHTTPStrictImportBoundariesAndSanitizedErrors(t *testing.T) {
	valid := statementHTTPBody("alipay", statementCSVHeader)
	for name, body := range map[string]string{
		"unknown_field":    strings.TrimSuffix(valid, "}") + `,"amount_minor":1}`,
		"multiple_objects": valid + `{}`,
		"invalid_metadata": statementHTTPBody("unknown", statementCSVHeader),
		"bad_csv":          statementHTTPBody("alipay", "sensitive-private-fixture-value\n"),
		"invalid_amount":   statementHTTPBody("alipay", statementCSVHeader+store.ID()+",external-transaction,,,sensitive-private-fixture-value,CNY,2026-10-27T12:00:00+08:00\n"),
		"oversized_csv":    statementHTTPBody("alipay", strings.Repeat("x", (1<<20)+1)),
		"oversized_body":   statementHTTPBody("alipay", strings.Repeat("x", (2<<20)+1)),
		"null":             "null",
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, false)
			_, finance := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "POST", "/api/v1/admin/statements", body, finance)
			if code != http.StatusBadRequest || bytes.Contains(reply, []byte("sensitive-private-fixture-value")) {
				t.Fatal(code, string(reply))
			}
			if name == "invalid_amount" && (!bytes.Contains(reply, []byte("第 2 行")) || !bytes.Contains(reply, []byte("金额"))) {
				t.Fatal("invalid amount was not located safely", string(reply))
			}
			var count int
			if err := f.db.DB.QueryRow(context.Background(), `SELECT count(*) FROM merchant_statements`).Scan(&count); err != nil || count != 0 {
				t.Fatal("invalid import left a statement", count, err)
			}
		})
	}
}

func TestStatementsHTTPMoneyFieldsPreserveInt64Digits(t *testing.T) {
	f := setup(t, false)
	_, finance := staffAccount(t, f, "finance")
	csv := statementCSVHeader + store.ID() + ",large-external-transaction,,,9223372036854775807,CNY,2026-10-27T12:00:00+08:00\n"
	code, body, _ := request(t, f, "POST", "/api/v1/admin/statements", statementHTTPBody("alipay", csv), finance)
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var result struct {
		Statement struct {
			ID    string `json:"id"`
			Total string `json:"total_minor"`
		} `json:"statement"`
		LatestRun struct {
			ID string `json:"id"`
		} `json:"latest_run"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Statement.Total != "9223372036854775807" {
		t.Fatal("statement total lost decimal precision", string(body), err)
	}
	code, body, _ = request(t, f, "GET", "/api/v1/admin/statements/"+result.Statement.ID+"/runs/"+result.LatestRun.ID+"/findings", "", finance)
	if code != http.StatusOK {
		t.Fatal(code, string(body))
	}
	var page struct {
		Items []struct {
			Amount string  `json:"statement_amount_minor"`
			Local  *string `json:"local_amount_minor"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Items) != 1 || page.Items[0].Amount != "9223372036854775807" || page.Items[0].Local != nil {
		t.Fatal("finding money lost decimal precision", string(body), err)
	}
}

func TestStatementsHTTPRejectsUnknownQueriesPathsAndEditableReviewState(t *testing.T) {
	f := setup(t, false)
	_, admin := staffAccount(t, f, "admin")
	csv := statementCSVHeader + store.ID() + ",external-transaction,,,1900,CNY,2026-10-27T12:00:00+08:00\n"
	detail := importHTTPStatement(t, f, admin, "alipay", csv)
	page := readHTTPFindings(t, f, admin, detail, "")
	reviewPath := "/api/v1/admin/statement-findings/" + page.Items[0].ID + "/reviews"
	for _, suffix := range []string{"?unknown=1", "?review=all&review=reviewed", "?review=", "?review=resolved", "?offset=-1", "?offset=1.0", "?offset=4001", "?limit=0", "?limit=101", "?limit=abc", "?limit=", "?offset=%zz"} {
		code, body, _ := request(t, f, "GET", findingsHTTPPath(detail)+suffix, "", admin)
		if code != http.StatusBadRequest {
			t.Fatal(suffix, code, string(body))
		}
	}
	for _, path := range []string{"/api/v1/admin/statements?provider=alipay", "/api/v1/admin/statements/" + detail.Statement.ID + "?unused=1", reviewPath + "?limit=1"} {
		code, body, _ := request(t, f, "GET", path, "", admin)
		if code != http.StatusBadRequest {
			t.Fatal(path, code, string(body))
		}
	}
	for _, body := range []string{`{"note":"","resolved":true}`, `{"note":""}`, `{"note":"bad\ncontrol"}`, accountBody(map[string]string{"note": strings.Repeat("x", 1001)})} {
		code, reply, _ := request(t, f, "POST", reviewPath, body, admin)
		if code != http.StatusBadRequest {
			t.Fatal(code, string(reply))
		}
	}
	for _, path := range []string{"/api/v1/admin/statements/not-an-id", "/api/v1/admin/statements/" + store.ID(), "/api/v1/admin/statement-findings/" + store.ID() + "/reviews"} {
		code, body, _ := request(t, f, "GET", path, "", admin)
		if code != http.StatusNotFound {
			t.Fatal(path, code, string(body))
		}
	}
}

func TestStatementsHTTPBrowserOriginAndJSONRemainRequired(t *testing.T) {
	f := setup(t, false)
	_, admin := staffAccount(t, f, "admin")
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
		req, err := http.NewRequest("POST", f.server.URL+"/api/v1/admin/statements", strings.NewReader(statementHTTPBody("alipay", statementCSVHeader)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", input.origin)
		req.Header.Set("Content-Type", input.contentType)
		if input.cookie {
			req.AddCookie(admin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != input.want {
			t.Fatal(resp.StatusCode, string(body), err)
		}
	}
}

func TestStatementsHTTPSharedWriteLimitsAndLimiterFailure(t *testing.T) {
	t.Run("peer_and_user", func(t *testing.T) {
		f := setup(t, false)
		_, first := staffAccount(t, f, "finance")
		_, second := staffAccount(t, f, "finance")
		body := statementHTTPBody("alipay", statementCSVHeader)
		for i := 0; i < 5; i++ {
			status, reply, _ := request(t, f, "POST", "/api/v1/admin/statements", body, first)
			if status != http.StatusOK {
				t.Fatal(i, status, string(reply))
			}
		}
		status, reply, _ := request(t, f, "POST", "/api/v1/admin/statements", body, second)
		if status != http.StatusTooManyRequests {
			t.Fatal("another user bypassed peer limit", status, string(reply))
		}
		// A fresh direct peer still cannot bypass the same user's own bucket.
		handler := httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4}).Handler()
		req := httptest.NewRequest("POST", "/api/v1/admin/statements", strings.NewReader(body))
		req.RemoteAddr = "192.0.2.44:12345"
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(first)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != http.StatusTooManyRequests {
			t.Fatal("new peer bypassed user limit", result.Code, result.Body.String())
		}
	})
	t.Run("failure_is_unavailable", func(t *testing.T) {
		f := setup(t, false)
		_, finance := staffAccount(t, f, "finance")
		if _, err := f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_statement_rate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture limiter failure'; END $$; CREATE TRIGGER reject_statement_rate BEFORE INSERT ON rate_limit_buckets FOR EACH ROW EXECUTE FUNCTION reject_statement_rate()`); err != nil {
			t.Fatal(err)
		}
		status, body, _ := request(t, f, "POST", "/api/v1/admin/statements", statementHTTPBody("alipay", statementCSVHeader), finance)
		if status != http.StatusServiceUnavailable || bytes.Contains(body, []byte("fixture limiter failure")) {
			t.Fatal(status, string(body))
		}
	})
}
