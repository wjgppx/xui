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

	"github.com/wjgppx/xui/internal/fundledger"
	"github.com/wjgppx/xui/internal/httpapi"
	"github.com/wjgppx/xui/internal/store"
)

const fundsHTTPHeader = "sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\n"

func fundsHTTPJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func fundsHTTPInput(provider, account, date, opening, closing, rows string) fundledger.Import {
	return fundledger.Import{Provider: provider, AccountKey: account, Date: date, Coverage: "partial", OpeningBalanceYuan: opening, ClosingBalanceYuan: closing, CSV: fundsHTTPHeader + rows}
}
func fundsHTTPImport(t *testing.T, f fixture, cookie *http.Cookie, input fundledger.Import) store.FundLedger {
	t.Helper()
	code, body, _ := request(t, f, "POST", "/api/v1/admin/fund-ledgers", fundsHTTPJSON(t, input), cookie)
	var result store.FundLedger
	if code != http.StatusOK || json.Unmarshal(body, &result) != nil || result.ID == "" {
		t.Fatal("fund ledger import", code, string(body))
	}
	return result
}
func fundsHTTPCompare(t *testing.T, f fixture, cookie *http.Cookie, merchantID, bankID string) store.FundReconciliation {
	t.Helper()
	code, body, _ := request(t, f, "POST", "/api/v1/admin/fund-reconciliations", fundsHTTPJSON(t, map[string]string{"merchant_batch_id": merchantID, "bank_batch_id": bankID}), cookie)
	var result store.FundReconciliation
	if code != http.StatusOK || json.Unmarshal(body, &result) != nil || result.ID == "" {
		t.Fatal("fund ledger comparison", code, string(body))
	}
	return result
}

func TestFundLedgersHTTPAuthorityMatrixAndIndependentFinancialGates(t *testing.T) {
	for _, role := range []string{"admin", "finance", "support", "operator", "user"} {
		t.Run(role, func(t *testing.T) {
			f := setup(t, false)
			_, cookie := staffAccount(t, f, role)
			allowed := role == "admin" || role == "finance"
			input := fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "0", "0", "")
			for _, endpoint := range []struct {
				method, path, body string
				status             int
			}{
				{"GET", "/api/v1/admin/fund-ledgers", "", http.StatusOK},
				{"POST", "/api/v1/admin/fund-ledgers", fundsHTTPJSON(t, input), http.StatusOK},
				{"GET", "/api/v1/admin/fund-ledgers/" + store.ID(), "", http.StatusNotFound},
				{"GET", "/api/v1/admin/fund-reconciliations", "", http.StatusOK},
				{"POST", "/api/v1/admin/fund-reconciliations", fundsHTTPJSON(t, map[string]string{"merchant_batch_id": store.ID(), "bank_batch_id": store.ID()}), http.StatusNotFound},
				{"GET", "/api/v1/admin/fund-reconciliations/" + store.ID(), "", http.StatusNotFound},
			} {
				want := http.StatusForbidden
				if allowed {
					want = endpoint.status
				}
				code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
				if code != want {
					t.Fatal(endpoint.method, endpoint.path, code, want, string(body))
				}
			}
		})
	}
}

func TestFundLedgersHTTPPrecisionDedupDiscrepanciesAndNoBusinessMutation(t *testing.T) {
	f := setup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	order := paymentOrder(t, f, actor)
	payment, err := f.db.CreatePayment(context.Background(), actor.ID, order.ID, "wechat")
	if err != nil {
		t.Fatal(err)
	}
	input := fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "92233720368547758.07", "92233720368547758.06999", "1,precise-fee,2026-10-28T09:00:00+08:00,fee,debit,0.00001,92233720368547758.06999,,\n")
	first := fundsHTTPImport(t, f, cookie, input)
	if first.Currency != "CNY" || first.ClosingBalanceYuan != "92233720368547758.06999" || first.ExpectedClosingBalanceYuan != first.ClosingBalanceYuan || first.TotalDebitYuan != "0.00001" || len(first.Rows) != 1 || first.Rows[0].BalanceAfterYuan != first.ClosingBalanceYuan || first.BalanceDifferenceCount != 0 {
		t.Fatal("precise observed balances changed", first)
	}
	input.CSV = "\ufeff" + strings.ReplaceAll(input.CSV, "\n", "\r\n")
	again := fundsHTTPImport(t, f, cookie, input)
	if again.ID != first.ID || again.Digest != first.Digest {
		t.Fatal("equivalent file created a second balance batch")
	}
	badBalances := fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "100", "90", "1,balance-discrepancy,2026-10-28T09:00:00+08:00,withdrawal,debit,10,90.00001,transfer-A,bank_ops\n")
	observed := fundsHTTPImport(t, f, cookie, badBalances)
	if observed.ID == first.ID || observed.BalanceDifferenceCount != 1 || len(observed.Rows[0].Codes) != 2 || !slices.Contains(observed.SummaryCodes, "closing_entry_mismatch") || observed.Rows[0].BalanceAfterYuan != "90.00001" || observed.Rows[0].ExpectedBalanceYuan != "90.00000" {
		t.Fatal("arithmetic discrepancy rejected or silently corrected", observed)
	}
	var read store.FundLedger
	code, body, _ := request(t, f, "GET", "/api/v1/admin/fund-ledgers/"+first.ID, "", cookie)
	if code != http.StatusOK || json.Unmarshal(body, &read) != nil || read.ID != first.ID || read.Rows[0].AmountYuan != "0.00001" {
		t.Fatal("immutable batch read", code, string(body))
	}
	var list struct {
		Items []store.FundLedgerSummary `json:"items"`
	}
	code, body, _ = request(t, f, "GET", "/api/v1/admin/fund-ledgers", "", cookie)
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Items) != 2 || bytes.Contains(body, []byte(`"rows"`)) {
		t.Fatal("batch list mixed or aggregated observations", code, string(body))
	}
	var status string
	var grants, events, actions, audits int
	err = f.db.DB.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM entitlement_periods),(SELECT count(*) FROM payment_events),(SELECT count(*) FROM payment_action_jobs),(SELECT count(*) FROM audit_events WHERE action='fund_ledger.imported') FROM payment_intents WHERE id=$1`, payment.ID).Scan(&status, &grants, &events, &actions, &audits)
	if err != nil || status != "pending" || grants != 0 || events != 0 || actions != 0 || audits != 2 {
		t.Fatal("manual fund observations changed business facts or duplicated audit", status, grants, events, actions, audits, err)
	}
}

func TestFundLedgersHTTPExplicitAccountPairCrossDayAmbiguityAndImmutableReports(t *testing.T) {
	f := setup(t, false)
	_, cookie := staffAccount(t, f, "finance")
	merchant := fundsHTTPImport(t, f, cookie, fundsHTTPInput("wechat", "merchant_main", "2026-10-28", "100", "69", "1,m-1,2026-10-28T09:00:00+08:00,withdrawal,debit,10,90,transfer-A,bank_ops\n2,m-2,2026-10-28T09:01:00+08:00,withdrawal,debit,5,85,transfer-duplicate,bank_ops\n3,m-3,2026-10-28T09:02:00+08:00,withdrawal,debit,5,80,transfer-duplicate,bank_ops\n4,m-4,2026-10-28T09:03:00+08:00,withdrawal,debit,10,70,transfer-other-account,bank_other\n5,m-5,2026-10-28T09:04:00+08:00,withdrawal,debit,1,69,transfer-difference,bank_ops\n"))
	bank := fundsHTTPImport(t, f, cookie, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "27.00001", "1,b-1,2026-10-29T09:00:00+08:00,settlement,credit,10,10,transfer-A,merchant_main\n2,b-2,2026-10-29T09:01:00+08:00,settlement,credit,5,15,transfer-duplicate,merchant_main\n3,b-3,2026-10-29T09:02:00+08:00,settlement,credit,10,25,transfer-other-account,other_merchant\n4,b-4,2026-10-29T09:03:00+08:00,settlement,credit,1,26,transfer-bank-only,merchant_main\n5,b-5,2026-10-29T09:04:00+08:00,settlement,credit,1.00001,27.00001,transfer-difference,merchant_main\n"))
	report := fundsHTTPCompare(t, f, cookie, merchant.ID, bank.ID)
	if report.MatchedCount != 1 || report.DifferenceCount != 1 || report.UnmatchedCount != 1 || report.AmbiguousCount != 1 || len(report.Rows) != 4 || report.MerchantOutOfScopeCount != 1 || report.BankOutOfScopeCount != 1 || report.MerchantAccountKey != "merchant_main" || report.BankAccountKey != "bank_ops" || !slices.Contains(report.SourceWarnings, "merchant_partial") || !slices.Contains(report.SourceWarnings, "bank_partial") {
		t.Fatal("pair scope or ambiguity lost", report)
	}
	for _, row := range report.Rows {
		if row.TransferID == "transfer-other-account" {
			t.Fatal("cross-account reference was matched")
		}
		if row.TransferID == "transfer-duplicate" && (row.Status != "ambiguous" || len(row.MerchantEntries) != 2 || len(row.BankEntries) != 1) {
			t.Fatal("duplicate reference picked a first entry", row)
		}
		if row.TransferID == "transfer-A" && (row.Status != "matched" || row.MerchantEntries[0].OccurredAt.Equal(row.BankEntries[0].OccurredAt)) {
			t.Fatal("cross-day explicit match rejected or timestamp overwritten", row)
		}
	}
	repeated := fundsHTTPCompare(t, f, cookie, merchant.ID, bank.ID)
	if repeated.ID != report.ID {
		t.Fatal("same immutable scopes created another report")
	}
	var stored store.FundReconciliation
	code, body, _ := request(t, f, "GET", "/api/v1/admin/fund-reconciliations/"+report.ID, "", cookie)
	if code != http.StatusOK || json.Unmarshal(body, &stored) != nil || stored.ID != report.ID || stored.AmbiguousCount != 1 || len(stored.Rows) != 4 {
		t.Fatal("stored report evidence lost", code, string(body))
	}
	code, body, _ = request(t, f, "GET", "/api/v1/admin/fund-reconciliations", "", cookie)
	if code != http.StatusOK || bytes.Contains(body, []byte(`"rows"`)) || !bytes.Contains(body, []byte(report.ID)) {
		t.Fatal("report list returned full facts or lost summary", code, string(body))
	}
	for _, pair := range [][2]string{{bank.ID, merchant.ID}, {merchant.ID, merchant.ID}} {
		code, body, _ = request(t, f, "POST", "/api/v1/admin/fund-reconciliations", fundsHTTPJSON(t, map[string]string{"merchant_batch_id": pair[0], "bank_batch_id": pair[1]}), cookie)
		if code != http.StatusConflict {
			t.Fatal("invalid merchant/bank scope accepted", code, string(body))
		}
	}
}

func TestFundLedgersHTTPStrictInputBoundsIdentityAndAliasPinning(t *testing.T) {
	base := fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "0", "0", "")
	for _, body := range []string{
		`{}`, `null`, fundsHTTPJSON(t, base) + ` {}`,
		strings.TrimSuffix(fundsHTTPJSON(t, base), "}") + `,"currency":"USD","credential":"private-sensitive-token"}`,
		fundsHTTPJSON(t, fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "0", "0", strings.Repeat("x", (1<<20)+1))),
		fundsHTTPJSON(t, fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "0", "0", "1,private-sensitive-reference,2026-10-28T09:00:00+08:00,fee,debit,0.123456,0,,\n")),
		fundsHTTPJSON(t, fundsHTTPInput("alipay", "merchant_main", "2026-10-28", "0", "0", strings.Repeat("x", (2<<20)+1))),
	} {
		t.Run("invalid", func(t *testing.T) {
			f := setup(t, false)
			_, cookie := staffAccount(t, f, "finance")
			code, reply, _ := request(t, f, "POST", "/api/v1/admin/fund-ledgers", body, cookie)
			if code != http.StatusBadRequest || bytes.Contains(reply, []byte("private-sensitive")) {
				t.Fatal("unsafe ledger rejection", code, string(reply))
			}
		})
	}
	f := setup(t, false)
	_, cookie := staffAccount(t, f, "finance")
	fundsHTTPImport(t, f, cookie, base)
	base.Provider = "bank"
	code, body, _ := request(t, f, "POST", "/api/v1/admin/fund-ledgers", fundsHTTPJSON(t, base), cookie)
	if code != http.StatusConflict {
		t.Fatal("existing account alias changed provider", code, string(body))
	}
	for _, path := range []string{"/api/v1/admin/fund-ledgers?cursor=1", "/api/v1/admin/fund-ledgers/" + store.ID() + "?limit=100", "/api/v1/admin/fund-reconciliations?bank=other", "/api/v1/admin/fund-reconciliations/" + store.ID() + "?source=other", "/api/v1/admin/fund-ledgers/malformed-id"} {
		if code, body, _ := request(t, f, "GET", path, "", cookie); code != http.StatusBadRequest {
			t.Fatal("query/malformed identity accepted", path, code, string(body))
		}
	}
	if code, body, _ := request(t, f, "POST", "/api/v1/admin/fund-reconciliations", `{"merchant_batch_id":"malformed","bank_batch_id":"malformed","status":"matched"}`, cookie); code != http.StatusBadRequest {
		t.Fatal("browser comparison authority accepted", code, string(body))
	}
}

func TestFundLedgersHTTPOriginJSONSessionAndProductionVerification(t *testing.T) {
	f := setup(t, false)
	actor, cookie := staffAccount(t, f, "finance")
	input := fundsHTTPJSON(t, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "0", ""))
	for _, endpoint := range []string{"/api/v1/admin/fund-ledgers", "/api/v1/admin/fund-reconciliations"} {
		for _, sample := range []struct {
			origin, contentType string
			cookie              bool
			want                int
		}{{"", "application/json", true, http.StatusForbidden}, {"https://untrusted.example.test", "application/json", true, http.StatusForbidden}, {origin, "text/csv", true, http.StatusUnsupportedMediaType}, {origin, "application/json", false, http.StatusUnauthorized}} {
			req, err := http.NewRequest("POST", f.server.URL+endpoint, strings.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", sample.origin)
			req.Header.Set("Content-Type", sample.contentType)
			if sample.cookie {
				req.AddCookie(cookie)
			}
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != sample.want {
				t.Fatal(endpoint, response.StatusCode, string(body), readErr)
			}
		}
	}
	server := httptest.NewServer(httpapi.New(f.db, httpapi.Config{Origin: origin, RequireVerified: true, PasswordCost: 4}).Handler())
	t.Cleanup(server.Close)
	verifiedOnly := fixture{f.db, server}
	for _, endpoint := range []struct{ method, path, body string }{{"GET", "/api/v1/admin/fund-ledgers", ""}, {"POST", "/api/v1/admin/fund-ledgers", input}, {"GET", "/api/v1/admin/fund-reconciliations", ""}, {"POST", "/api/v1/admin/fund-reconciliations", `{}`}} {
		code, body, _ := request(t, verifiedOnly, endpoint.method, endpoint.path, endpoint.body, cookie)
		if code != http.StatusForbidden || !bytes.Contains(body, []byte("邮箱验证")) {
			t.Fatal("unverified funds access", endpoint.method, endpoint.path, code, string(body))
		}
	}
	if _, err := f.db.DB.Exec(context.Background(), `UPDATE users SET email_verified_at=clock_timestamp() WHERE id=$1`, actor.ID); err != nil {
		t.Fatal(err)
	}
	fundsHTTPImport(t, verifiedOnly, cookie, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "0", ""))
}

func TestFundLedgersHTTPIndependentSharedPeerAndUserWriteLimits(t *testing.T) {
	f := setup(t, false)
	_, first := staffAccount(t, f, "finance")
	_, second := staffAccount(t, f, "finance")
	ledgerBody := fundsHTTPJSON(t, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "0", ""))
	comparisonBody := fundsHTTPJSON(t, map[string]string{"merchant_batch_id": store.ID(), "bank_batch_id": store.ID()})
	for _, endpoint := range []struct {
		path, body string
		want       int
	}{{"/api/v1/admin/fund-ledgers", ledgerBody, http.StatusOK}, {"/api/v1/admin/fund-reconciliations", comparisonBody, http.StatusNotFound}} {
		for i := 0; i < 5; i++ {
			if code, body, _ := request(t, f, "POST", endpoint.path, endpoint.body, first); code != endpoint.want {
				t.Fatal("independent write bucket", endpoint.path, i, code, string(body))
			}
		}
		if code, body, _ := request(t, f, "POST", endpoint.path, endpoint.body, second); code != http.StatusTooManyRequests {
			t.Fatal("second user bypassed peer limit", code, string(body))
		}
		handler := httpapi.New(f.db, httpapi.Config{Origin: origin, PasswordCost: 4}).Handler()
		req := httptest.NewRequest("POST", endpoint.path, strings.NewReader(endpoint.body))
		req.RemoteAddr = "192.0.2.90:12345"
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(first)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusTooManyRequests {
			t.Fatal("fresh peer bypassed per-user write limit", response.Code, response.Body.String())
		}
	}
}

func TestFundLedgersHTTPStorageAndLimiterFailuresStayPrivate(t *testing.T) {
	t.Run("limiter", func(t *testing.T) {
		f := setup(t, false)
		_, cookie := staffAccount(t, f, "finance")
		if _, err := f.db.DB.Exec(context.Background(), `CREATE FUNCTION reject_funds_rate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'sensitive fixture rate failure'; END $$; CREATE TRIGGER reject_funds_rate BEFORE INSERT ON rate_limit_buckets FOR EACH ROW EXECUTE FUNCTION reject_funds_rate()`); err != nil {
			t.Fatal(err)
		}
		code, body, _ := request(t, f, "POST", "/api/v1/admin/fund-ledgers", fundsHTTPJSON(t, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "0", "")), cookie)
		if code != http.StatusServiceUnavailable || bytes.Contains(body, []byte("sensitive fixture")) {
			t.Fatal("limiter error leaked", code, string(body))
		}
	})
	t.Run("storage", func(t *testing.T) {
		f := setup(t, false)
		_, cookie := staffAccount(t, f, "finance")
		if _, err := f.db.DB.Exec(context.Background(), `ALTER TABLE fund_ledger_batches RENAME TO sensitive_ledger_failure; ALTER TABLE fund_ledger_reconciliations RENAME TO sensitive_reconciliation_failure`); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []struct{ method, path, body string }{
			{"GET", "/api/v1/admin/fund-ledgers", ""}, {"GET", "/api/v1/admin/fund-ledgers/" + store.ID(), ""},
			{"POST", "/api/v1/admin/fund-ledgers", fundsHTTPJSON(t, fundsHTTPInput("bank", "bank_ops", "2026-10-29", "0", "0", ""))},
			{"GET", "/api/v1/admin/fund-reconciliations", ""}, {"GET", "/api/v1/admin/fund-reconciliations/" + store.ID(), ""},
			{"POST", "/api/v1/admin/fund-reconciliations", fundsHTTPJSON(t, map[string]string{"merchant_batch_id": store.ID(), "bank_batch_id": store.ID()})},
		} {
			code, body, _ := request(t, f, endpoint.method, endpoint.path, endpoint.body, cookie)
			if code != http.StatusServiceUnavailable || bytes.Contains(body, []byte("sensitive_")) || bytes.Contains(body, []byte("fund_ledger_")) {
				t.Fatal("storage failure exposed internal values", endpoint.path, code, string(body))
			}
		}
	})
}
