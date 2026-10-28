package store_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

const statementHeader = "payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at\n"

func statementDate() string {
	return time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02")
}
func statementInput(provider, kind, coverage string, rows ...[]string) statements.Import {
	var content strings.Builder
	w := csv.NewWriter(&content)
	_ = w.Write(strings.Split(strings.TrimSpace(statementHeader), ","))
	for _, row := range rows {
		_ = w.Write(row)
	}
	w.Flush()
	return statements.Import{Provider: provider, Kind: kind, Date: statementDate(), Coverage: coverage, CSV: content.String()}
}
func statementPaymentRow(p store.Payment, amount int64) []string {
	return []string{p.ID, "transaction-" + p.ID, "", "", fmt.Sprint(amount), "CNY", statementDate() + "T12:00:00+08:00"}
}
func statementPaid(t *testing.T, s *store.Store, provider string) (string, store.Order, store.Payment) {
	t.Helper()
	u, o, p := paymentFixture(t, s, provider)
	if err := s.SettlePayment(context.Background(), p.ID, p.Provider, "transaction-"+p.ID, o.Price, o.Currency); err != nil {
		t.Fatal(err)
	}
	return u, o, p
}
func importStatement(t *testing.T, s *store.Store, actor string, in statements.Import) store.StatementDetail {
	t.Helper()
	d, err := s.ImportStatement(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func statementFindings(t *testing.T, s *store.Store, d store.StatementDetail) store.StatementFindingsPage {
	t.Helper()
	page, err := s.StatementFindings(context.Background(), d.Statement.ID, d.LatestRun.ID, "all", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
func hasStatementCode(f store.StatementFinding, code string) bool {
	for _, item := range f.Codes {
		if item == code {
			return true
		}
	}
	return false
}

func TestStatementImportDeduplicatesCanonicalFactsAndNeverWritesFinancialRows(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, o, p := statementPaid(t, s, "alipay")
	// Reject every financial-table update so a future reconciliation shortcut
	// cannot silently turn an imported observation into trusted payment proof.
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_statement_money_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'financial write forbidden'; END $$; CREATE TRIGGER reject_statement_payment BEFORE UPDATE ON payment_intents FOR EACH ROW EXECUTE FUNCTION reject_statement_money_write(); CREATE TRIGGER reject_statement_order BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION reject_statement_money_write(); CREATE TRIGGER reject_statement_refund BEFORE UPDATE ON payment_refunds FOR EACH ROW EXECUTE FUNCTION reject_statement_money_write(); CREATE TRIGGER reject_statement_entitlement BEFORE UPDATE ON entitlement_periods FOR EACH ROW EXECUTE FUNCTION reject_statement_money_write(); CREATE TRIGGER reject_statement_job BEFORE UPDATE ON payment_action_jobs FOR EACH ROW EXECUTE FUNCTION reject_statement_money_write()`)
	if err != nil {
		t.Fatal(err)
	}
	input := statementInput("alipay", "payment", "partial", statementPaymentRow(p, o.Price))
	d := importStatement(t, s, actor, input)
	if d.Statement.RowCount != 1 || d.Statement.Total != o.Price || d.LatestRun.MatchedCount != 1 || d.LatestRun.DifferenceCount != 0 || len(d.Runs) != 1 {
		t.Fatal("matching statement incorrect", d)
	}
	input.CSV = "\ufeff" + strings.ReplaceAll(input.CSV, "\n", "\r\n")
	same := importStatement(t, s, actor, input)
	if same.Statement.ID != d.Statement.ID || same.LatestRun.ID != d.LatestRun.ID {
		t.Fatal("semantic duplicate created evidence/report")
	}
	var statementsCount, runs, audits int
	if err = s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statements),(SELECT count(*) FROM merchant_statement_runs),(SELECT count(*) FROM audit_events WHERE action='statement.imported')`).Scan(&statementsCount, &runs, &audits); err != nil || statementsCount != 1 || runs != 1 || audits != 1 {
		t.Fatal(statementsCount, runs, audits, err)
	}
	got, err := s.Statement(ctx, d.Statement.ID)
	if err != nil || got.LatestRun.ID != d.LatestRun.ID {
		t.Fatal(err)
	}
	listed, err := s.ListStatements(ctx)
	if err != nil || len(listed) != 1 || listed[0].LatestRun.ID != d.LatestRun.ID {
		t.Fatal(listed, err)
	}
}

func TestStatementForwardDifferencesRetainExactAmountsAndMultipleReasons(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, o, paid := statementPaid(t, s, "alipay")
	_, _, pending := paymentFixture(t, s, "alipay")
	_, otherOrder, other := statementPaid(t, s, "wechat")
	wrong := statementPaymentRow(paid, o.Price+1)
	wrong[1] = "wrong-transaction"
	unknown := []string{"unknown-payment", "unknown-transaction", "", "", "9223372036854770000", "CNY", statementDate() + "T12:00:00+08:00"}
	d := importStatement(t, s, actor, statementInput("alipay", "payment", "partial", wrong, statementPaymentRow(pending, 1900), statementPaymentRow(other, otherOrder.Price), unknown))
	page := statementFindings(t, s, d)
	if d.LatestRun.DifferenceCount != 4 || page.Total != 4 || d.LatestRun.MatchedCount != 0 {
		t.Fatal(d.LatestRun, page)
	}
	byID := map[string]store.StatementFinding{}
	for _, f := range page.Items {
		byID[f.PaymentID] = f
	}
	if !hasStatementCode(byID[paid.ID], "identity_mismatch") || !hasStatementCode(byID[paid.ID], "amount_mismatch") || !hasStatementCode(byID[pending.ID], "not_confirmed") || !hasStatementCode(byID[other.ID], "provider_mismatch") || !hasStatementCode(byID["unknown-payment"], "missing_local") {
		t.Fatal("differences misclassified", byID)
	}
	encoded, err := json.Marshal(byID["unknown-payment"])
	if err != nil || !strings.Contains(string(encoded), `"statement_amount_minor":"9223372036854770000"`) || !strings.Contains(string(encoded), `"local_amount_minor":null`) {
		t.Fatal("large amount rounded or null lost", string(encoded), err)
	}
	var active int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM entitlement_periods WHERE revoked_at IS NULL`).Scan(&active); err != nil || active != 2 {
		t.Fatal("import changed rights", active, err)
	}
}

func TestStatementFullCoverageUsesShanghaiConfirmationCandidatesWithoutRejectingLateMatches(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, matchedOrder, matched := statementPaid(t, s, "alipay")
	_, _, begin := statementPaid(t, s, "alipay")
	_, _, end := statementPaid(t, s, "alipay")
	_, _, before := statementPaid(t, s, "alipay")
	start, err := time.Parse(time.RFC3339, statementDate()+"T00:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id string
		at time.Time
	}{{matched.ID, start.Add(25 * time.Hour)}, {begin.ID, start}, {end.ID, start.Add(24 * time.Hour)}, {before.ID, start.Add(-time.Nanosecond)}} {
		if _, err = s.DB.Exec(ctx, `UPDATE payment_intents SET paid_at=$2 WHERE id=$1`, item.id, item.at); err != nil {
			t.Fatal(err)
		}
	}
	input := statementInput("alipay", "payment", "partial", statementPaymentRow(matched, matchedOrder.Price))
	partial := importStatement(t, s, actor, input)
	if partial.LatestRun.MatchedCount != 1 || partial.LatestRun.DifferenceCount != 0 {
		t.Fatal("late local confirmation rejected valid merchant identity", partial)
	}
	input.Coverage = "full"
	full := importStatement(t, s, actor, input)
	page := statementFindings(t, s, full)
	if full.LatestRun.MatchedCount != 1 || full.LatestRun.LocalCandidateCount != 1 || page.Total != 1 || page.Items[0].PaymentID != begin.ID || !hasStatementCode(page.Items[0], "local_only") || page.Items[0].StatementAmount != nil {
		t.Fatal("Shanghai half-open window or candidate semantics wrong", full, page)
	}
}

func TestStatementRefundRerunPreservesOldSnapshotAndReviewedHistory(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, o, p, r := refundFixture(t, s)
	input := statementInput("alipay", "refund", "partial", []string{p.ID, "transaction-" + p.ID, r.ID, "provider-refund", fmt.Sprint(o.Price), "CNY", statementDate() + "T12:00:00+08:00"})
	d := importStatement(t, s, actor, input)
	old := statementFindings(t, s, d)
	if old.Total != 1 || !hasStatementCode(old.Items[0], "not_confirmed") {
		t.Fatal(old)
	}
	first, err := s.ReviewStatementFinding(ctx, actor, old.Items[0].ID, "待核对渠道确认")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ReviewStatementFinding(ctx, actor, old.Items[0].ID, "待核对渠道确认")
	if err != nil || again.ID != first.ID {
		t.Fatal("duplicate review not idempotent", err)
	}
	if err = s.ApproveRefund(ctx, actor, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	job := claimAction(t, s)
	if err = s.FinishPaymentAction(ctx, job.ID, job.ClaimToken, "succeeded", "provider-refund"); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ReconcileStatement(ctx, actor, d.Statement.ID)
	if err != nil || fresh.Revision != 2 || fresh.MatchedCount != 1 || fresh.DifferenceCount != 0 {
		t.Fatal("refund did not converge on explicit rerun", fresh, err)
	}
	old = statementFindings(t, s, d)
	if old.Total != 1 || old.Items[0].ReviewCount != 1 || old.Items[0].LatestReview.ID != first.ID || old.Items[0].LocalStatus != "requested" {
		t.Fatal("old report mutated", old)
	}
	// Original paid evidence survives a refund and still belongs in payment bills.
	paymentBill := importStatement(t, s, actor, statementInput("alipay", "payment", "full", statementPaymentRow(p, o.Price)))
	if paymentBill.LatestRun.MatchedCount != 1 || paymentBill.LatestRun.LocalCandidateCount != 0 {
		t.Fatal("refunded order vanished from gross payment comparison", paymentBill)
	}
	// A successful local refund without a complete gateway identity is not a
	// match, even though pending refunds legitimately have no provider ID yet.
	if _, err = s.DB.Exec(ctx, `UPDATE payment_refunds SET provider_refund_id=NULL WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	incomplete, err := s.ReconcileStatement(ctx, actor, d.Statement.ID)
	if err != nil || incomplete.MatchedCount != 0 || incomplete.DifferenceCount != 1 {
		t.Fatal("missing successful refund identity matched", incomplete, err)
	}
	check, err := s.StatementFindings(ctx, d.Statement.ID, incomplete.ID, "all", 0, 100)
	if err != nil || len(check.Items) != 1 || !hasStatementCode(check.Items[0], "identity_mismatch") {
		t.Fatal(check, err)
	}
}

func TestStatementEvidenceReportsAndReviewsAreAppendOnly(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	d := importStatement(t, s, actor, statementInput("alipay", "payment", "partial", []string{"unknown-payment", "unknown-transaction", "", "", "1900", "CNY", statementDate() + "T12:00:00+08:00"}))
	page := statementFindings(t, s, d)
	if _, err := s.ReviewStatementFinding(ctx, actor, page.Items[0].ID, "已复核，仍需渠道确认"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE merchant_statements SET coverage='full'`, `DELETE FROM merchant_statements`, `UPDATE merchant_statement_rows SET amount_minor=1`, `DELETE FROM merchant_statement_rows`, `UPDATE merchant_statement_runs SET matched_count=1`, `DELETE FROM merchant_statement_runs`, `UPDATE merchant_statement_findings SET codes=ARRAY['local_only']`, `DELETE FROM merchant_statement_findings`, `UPDATE merchant_statement_reviews SET note='changed'`, `DELETE FROM merchant_statement_reviews`} {
		if _, err := s.DB.Exec(ctx, query); err == nil {
			t.Fatal("immutable evidence changed", query)
		}
	}
}

func TestStatementPermissionsValidationAndInvalidInputLeaveNoEvidence(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	ordinary := testutil.User(t, s)
	input := statementInput("alipay", "payment", "partial")
	if _, err := s.ImportStatement(ctx, ordinary, input); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("ordinary user imported", err)
	}
	bad := input
	bad.CSV = "invalid"
	if _, err := s.ImportStatement(ctx, actor, bad); !errors.Is(err, statements.ErrInvalid) {
		t.Fatal("invalid parse persisted", err)
	}
	d := importStatement(t, s, actor, statementInput("alipay", "payment", "partial", []string{"unknown-payment", "unknown-transaction", "", "", "1900", "CNY", statementDate() + "T12:00:00+08:00"}))
	page := statementFindings(t, s, d)
	if _, err := s.ReconcileStatement(ctx, ordinary, d.Statement.ID); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("ordinary user reran", err)
	}
	if _, err := s.ReviewStatementFinding(ctx, ordinary, page.Items[0].ID, "forbidden"); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("ordinary user reviewed", err)
	}
	for _, note := range []string{"", strings.Repeat("界", 334), "control\nvalue"} {
		if _, err := s.ReviewStatementFinding(ctx, actor, page.Items[0].ID, note); !errors.Is(err, store.ErrStatementConflict) {
			t.Fatal("invalid note accepted", err)
		}
	}
	if _, err := s.ReviewStatementFinding(ctx, actor, "missing-finding", "review"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.StatementFindingReviews(ctx, "missing-finding"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestStatementAuditFailureRollsBackImportRerunAndReview(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	fail := func() {
		t.Helper()
		if _, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_statement_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'statement.%' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_statement_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_statement_audit()`); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	input := statementInput("alipay", "payment", "partial", []string{"unknown-payment", "unknown-transaction", "", "", "1900", "CNY", statementDate() + "T12:00:00+08:00"})
	if _, err := s.ImportStatement(ctx, actor, input); err == nil {
		t.Fatal("audit failure committed import")
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statements)+(SELECT count(*) FROM merchant_statement_rows)+(SELECT count(*) FROM merchant_statement_runs)+(SELECT count(*) FROM merchant_statement_findings)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial evidence committed", count, err)
	}
	if _, err := s.DB.Exec(ctx, `DROP TRIGGER reject_statement_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	d := importStatement(t, s, actor, input)
	page := statementFindings(t, s, d)
	if _, err := s.DB.Exec(ctx, `CREATE TRIGGER reject_statement_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_statement_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileStatement(ctx, actor, d.Statement.ID); err == nil {
		t.Fatal("rerun audit failure committed")
	}
	if _, err := s.ReviewStatementFinding(ctx, actor, page.Items[0].ID, "not committed"); err == nil {
		t.Fatal("review audit failure committed")
	}
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statement_runs)+(SELECT count(*) FROM merchant_statement_reviews)`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial rerun/review committed", count, err)
	}
}

func TestStatementFindingPaginationNestedOwnershipAndReviewFilters(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	rows := [][]string{}
	for i := 0; i < 105; i++ {
		rows = append(rows, []string{fmt.Sprintf("unknown-%03d", i), fmt.Sprintf("transaction-%03d", i), "", "", "1900", "CNY", statementDate() + "T12:00:00+08:00"})
	}
	d := importStatement(t, s, actor, statementInput("alipay", "payment", "partial", rows...))
	first := statementFindings(t, s, d)
	if first.Total != 105 || len(first.Items) != 100 || first.Offset != 0 || first.Limit != 100 {
		t.Fatal(first.Total, len(first.Items))
	}
	second, err := s.StatementFindings(ctx, d.Statement.ID, d.LatestRun.ID, "all", 100, 100)
	if err != nil || len(second.Items) != 5 || second.Items[0].ID == first.Items[0].ID {
		t.Fatal(err)
	}
	other := importStatement(t, s, actor, statementInput("wechat", "payment", "partial"))
	if _, err = s.StatementFindings(ctx, other.Statement.ID, d.LatestRun.ID, "all", 0, 100); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("cross-report access accepted", err)
	}
	for _, args := range []struct {
		review        string
		offset, limit int
	}{{"unknown", 0, 100}, {"all", -1, 100}, {"all", 0, 101}} {
		if _, err = s.StatementFindings(ctx, d.Statement.ID, d.LatestRun.ID, args.review, args.offset, args.limit); !errors.Is(err, store.ErrStatementConflict) {
			t.Fatal("bad pagination accepted", err)
		}
	}
	finding := first.Items[0]
	for _, note := range []string{"第一次复核", "补充渠道查询结果"} {
		if _, err = s.ReviewStatementFinding(ctx, actor, finding.ID, note); err != nil {
			t.Fatal(err)
		}
	}
	reviewed, err := s.StatementFindings(ctx, d.Statement.ID, d.LatestRun.ID, "reviewed", 0, 100)
	if err != nil || reviewed.Total != 1 || reviewed.Items[0].ReviewCount != 2 || reviewed.Items[0].LatestReview.Note != "补充渠道查询结果" {
		t.Fatal(reviewed, err)
	}
	unreviewed, err := s.StatementFindings(ctx, d.Statement.ID, d.LatestRun.ID, "unreviewed", 0, 100)
	if err != nil || unreviewed.Total != 104 {
		t.Fatal(unreviewed.Total, err)
	}
	history, err := s.StatementFindingReviews(ctx, finding.ID)
	if err != nil || len(history) != 2 || history[0].Note != "补充渠道查询结果" {
		t.Fatal(history, err)
	}
	newRun, err := s.ReconcileStatement(ctx, actor, d.Statement.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.StatementFindings(ctx, d.Statement.ID, newRun.ID, "reviewed", 0, 100)
	if err != nil || fresh.Total != 0 {
		t.Fatal("old review automatically hid fresh differences", fresh, err)
	}
}

func TestStatementConcurrentImportsAndRerunsNeverOverwriteSnapshots(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := statementInput("alipay", "payment", "partial")
	var wg sync.WaitGroup
	results := make(chan store.StatementDetail, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.ImportStatement(ctx, actor, input)
			if err != nil {
				if !errors.Is(err, store.ErrStatementConflict) {
					t.Error(err)
				}
				return
			}
			results <- d
		}()
	}
	wg.Wait()
	close(results)
	id := ""
	for d := range results {
		if id == "" {
			id = d.Statement.ID
		}
		if id != d.Statement.ID {
			t.Fatal("duplicate source identity")
		}
	}
	if id == "" {
		t.Fatal("no concurrent import succeeded")
	}
	successes := make(chan store.StatementRun, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.ReconcileStatement(ctx, actor, id)
			if err != nil {
				if !errors.Is(err, store.ErrStatementConflict) {
					t.Error(err)
				}
				return
			}
			successes <- r
		}()
	}
	wg.Wait()
	close(successes)
	count := 0
	revisions := map[int]bool{}
	for r := range successes {
		count++
		if revisions[r.Revision] {
			t.Fatal("duplicate revision")
		}
		revisions[r.Revision] = true
	}
	if count == 0 {
		t.Fatal("no concurrent rerun succeeded")
	}
	var sources, runs int
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statements),(SELECT count(*) FROM merchant_statement_runs)`).Scan(&sources, &runs); err != nil || sources != 1 || runs != count+1 {
		t.Fatal(sources, runs, count, err)
	}
}

func TestStatementFullLocalScanLimitRejectsAtomicallyInsteadOfTruncating(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	user := testutil.User(t, s)
	_, err := s.DB.Exec(ctx, `INSERT INTO orders(id,user_id,plan_id,plan_name,price_minor,currency,duration_days,quota_bytes,status,paid_at) SELECT 'statement-order-'||g,$1,'explore','fixture',100,'CNY',30,1024,'paid',now() FROM generate_series(1,2001) g`, user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `INSERT INTO payment_intents(id,order_id,provider,status,transaction_id,paid_at) SELECT 'statement-payment-'||g,'statement-order-'||g,'alipay','paid','statement-transaction-'||g,now() FROM generate_series(1,2001) g`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportStatement(ctx, actor, statementInput("alipay", "payment", "full")); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("local over-limit was silently truncated", err)
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statements`).Scan(&count); err != nil || count != 0 {
		t.Fatal("over-limit source persisted", count, err)
	}
	d := importStatement(t, s, actor, statementInput("alipay", "payment", "partial"))
	if d.LatestRun.DifferenceCount != 0 {
		t.Fatal("partial statement invented reverse missing entries", d)
	}
}
