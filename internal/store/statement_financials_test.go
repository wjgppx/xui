package store_test

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type financialTestRow struct{ payment, refund, fee, amount string }

func financialTestNative(t *testing.T, kind string, rows ...financialTestRow) statements.NativeImport {
	t.Helper()
	head := strings.Split("支付宝交易号,商户订单号,业务类型,商品名称,创建时间,完成时间,门店编号,门店名称,操作员,终端号,对方账户,订单金额（元）,商家实收（元）,支付宝红包（元）,集分宝（元）,支付宝优惠（元）,商家优惠（元）,券核销金额（元）,券名称,商家红包消费金额（元）,卡消费金额（元）,退款批次号/请求号,服务费（元）,分润（元）,备注", ",")
	var content strings.Builder
	w := csv.NewWriter(&content)
	_ = w.Write(head)
	for _, item := range rows {
		values := make([]string, 25)
		values[0], values[1], values[2], values[3] = "transaction-"+item.payment, item.payment, "交易", "private-financial-fixture-product"
		values[4], values[5] = billStoreDate()+" 12:00:00", billStoreDate()+" 12:00:00"
		values[11], values[12], values[22] = "19.00", item.amount, item.fee
		if kind == "refund" {
			values[2], values[11], values[21] = "退款", "-19.00", item.refund
		}
		_ = w.Write(values)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return statements.NativeImport{Provider: "alipay", Kind: kind, Date: billStoreDate(), Coverage: "partial", Format: "alipay_trade_v1", ContentBase64: base64.StdEncoding.EncodeToString([]byte(content.String()))}
}

func financialTestImport(t *testing.T, s *store.Store, actor, kind string, rows ...financialTestRow) (store.StatementDetail, store.FinancialSnapshot) {
	t.Helper()
	detail, err := s.ImportNativeStatement(context.Background(), actor, financialTestNative(t, kind, rows...))
	if err != nil || len(detail.Sources) != 1 {
		t.Fatal("native import", err)
	}
	snapshot, err := s.SourceFinancials(context.Background(), detail.Statement.ID, detail.Sources[0].ID)
	if err != nil || !snapshot.Available {
		t.Fatal("financial observations absent", snapshot, err)
	}
	return detail, snapshot
}

func financialReference(rows ...financialTestRow) string {
	var content strings.Builder
	w := csv.NewWriter(&content)
	_ = w.Write([]string{"payment_id", "refund_id", "expected_fee_yuan", "expected_channel_amount_yuan"})
	for _, row := range rows {
		_ = w.Write([]string{row.payment, row.refund, row.fee, row.amount})
	}
	w.Flush()
	return content.String()
}

func financialHasCode(row store.FinancialComparison, code string) bool {
	for _, item := range row.Codes {
		if item == code {
			return true
		}
	}
	return false
}

func TestSourceFinancialSnapshotPreservesPreciseUnknownAndSignedAmounts(t *testing.T) {
	s := testutil.Database(t)
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment",
		financialTestRow{"observed-a", "", "-0.00001", "92233720368547758.07"},
		financialTestRow{"observed-b", "", "", "0.00"})
	if snapshot.Basis != "alipay_merchant_received" || snapshot.Version != statements.FinancialVersion || len(snapshot.Rows) != 2 || *snapshot.Rows[0].FeeYuan != "-0.00001" || *snapshot.Rows[0].ChannelAmountYuan != "92233720368547758.07000" || snapshot.Rows[1].FeeYuan != nil || snapshot.FeeTotalYuan != nil || *snapshot.ChannelAmountTotalYuan != "92233720368547758.07000" {
		t.Fatal("financial precision or unknown values changed", snapshot)
	}
	if detail.Statement.Total != 3800 || detail.Statement.RowCount != 2 || len(detail.Runs) != 1 {
		t.Fatal("fees changed normalized gross facts", detail.Statement)
	}
	b, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(b), "private-financial-fixture-product") || strings.Contains(string(b), "transaction-") {
		t.Fatal("unrelated raw data leaked", err)
	}
}

func TestSourceFinancialReportsCompareUnionAndKeepUnknownSeparateFromZero(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment",
		financialTestRow{"observed-a", "", "0.12345", "18"},
		financialTestRow{"observed-b", "", "", "17"},
		financialTestRow{"observed-c", "", "0", "0"})
	reference := financialReference(financialTestRow{"observed-a", "", "0.12345", "18"}, financialTestRow{"observed-b", "", "0", "17"}, financialTestRow{"reference-only", "", "0.1", "100"})
	report, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference, 0)
	if err != nil || report.Run.MatchedCount != 1 || report.Run.DifferenceCount != 2 || report.Run.UnassessedCount != 1 || len(report.Rows) != 4 || report.Run.Revision != 1 {
		t.Fatal("comparison scope/counts", report, err)
	}
	if !financialHasCode(report.Rows[1], "fee_unavailable") || !financialHasCode(report.Rows[2], "missing_reference") || report.Rows[2].Status != "unassessed" || !financialHasCode(report.Rows[3], "missing_observation") {
		t.Fatal("missing and unknown became zero or match", report.Rows)
	}
	same, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, "\ufeff"+financialReference(financialTestRow{"reference-only", "", "0.10000", "100.00000"}, financialTestRow{"observed-b", "", "0.00000", "17.0"}, financialTestRow{"observed-a", "", "0.12345", "18.00000"}))
	if err != nil || same.Run.ID != report.Run.ID {
		t.Fatal("semantic reference replay created a report", same.Run, err)
	}
	changed, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, strings.Replace(reference, "0.12345", "0.12344", 1))
	if err != nil || changed.Run.Revision != 2 || changed.Run.DifferenceCount != 3 || !financialHasCode(changed.Rows[0], "fee_mismatch") {
		t.Fatal("changed expectations overwrote history", changed, err)
	}
	old, err := s.SourceFinancialReport(ctx, detail.Statement.ID, snapshot.SourceID, report.Run.ID)
	if err != nil || old.Run.ID != report.Run.ID || old.Run.Revision != 1 || old.Run.MatchedCount != 1 {
		t.Fatal("old report unavailable or changed", old, err)
	}
	current, err := s.SourceFinancials(ctx, detail.Statement.ID, snapshot.SourceID)
	if err != nil || len(current.Runs) != 2 || current.Runs[0].ID != changed.Run.ID {
		t.Fatal("history listing", current.Runs, err)
	}
}

func TestSourceFinancialRefundComparisonPreservesSignAndChecksPaymentIdentity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "refund", financialTestRow{"original-payment", "refund-a", "-0.00234", "-18.00"})
	wrongPayment, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, financialReference(financialTestRow{"wrong-payment", "refund-a", "-0.00234", "-18"}))
	if err != nil || wrongPayment.Run.DifferenceCount != 1 || !financialHasCode(wrongPayment.Rows[0], "identity_mismatch") || financialHasCode(wrongPayment.Rows[0], "fee_mismatch") || wrongPayment.Rows[0].ExpectedPaymentID != "wrong-payment" {
		t.Fatal("refund matched different original payment", wrongPayment, err)
	}
	readBack, err := s.SourceFinancialReport(ctx, detail.Statement.ID, snapshot.SourceID, wrongPayment.Run.ID)
	if err != nil || readBack.Rows[0].ExpectedPaymentID != "wrong-payment" {
		t.Fatal("wrong reference payment identity lost from immutable report", readBack, err)
	}
	wrongSign, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, financialReference(financialTestRow{"original-payment", "refund-a", "0.00234", "-18"}))
	if err != nil || !financialHasCode(wrongSign.Rows[0], "fee_mismatch") || *wrongSign.Rows[0].ObservedFeeYuan != "-0.00234" {
		t.Fatal("refund fee sign normalized away", wrongSign, err)
	}
}

func TestSourceFinancialParentsAndUnavailableLegacySourcesAreExplicit(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment", financialTestRow{"observed-payment", "", "0.01", "18"})
	normal, err := s.ImportStatement(ctx, actor, statementInput("wechat", "payment", "partial"))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.SourceFinancials(ctx, normal.Statement.ID, normal.Sources[0].ID)
	if err != nil || legacy.Available || legacy.FeeTotalYuan != nil || legacy.ChannelAmountTotalYuan != nil || len(legacy.Rows) != 0 || legacy.Rows == nil || legacy.SummaryCodes == nil {
		t.Fatal("legacy source became zero observations", legacy, err)
	}
	reference := financialReference(financialTestRow{"observed-payment", "", "0.01", "18"})
	if _, err = s.ReconcileSourceFinancials(ctx, actor, normal.Statement.ID, normal.Sources[0].ID, reference); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("reconciled unavailable observations", err)
	}
	if _, err = s.SourceFinancials(ctx, normal.Statement.ID, snapshot.SourceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("source parent not checked", err)
	}
	if _, err = s.ReconcileSourceFinancials(ctx, actor, normal.Statement.ID, snapshot.SourceID, reference); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("reconcile parent not checked", err)
	}
	report, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][3]string{{normal.Statement.ID, snapshot.SourceID, report.Run.ID}, {detail.Statement.ID, normal.Sources[0].ID, report.Run.ID}, {detail.Statement.ID, snapshot.SourceID, store.ID()}} {
		if _, err = s.SourceFinancialReport(ctx, scope[0], scope[1], scope[2]); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("report escaped nested scope", err)
		}
	}
}

func TestSourceFinancialReportsRequireLiveRoleAndSessionVersion(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment", financialTestRow{"observed-payment", "", "0", "18"})
	reference := financialReference(financialTestRow{"observed-payment", "", "0", "18"})
	user := testutil.User(t, s)
	if _, err := s.ReconcileSourceFinancials(ctx, user, detail.Statement.ID, snapshot.SourceID, reference); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("ordinary user reconciled", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET security_version=1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference, 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("invalidated session authorized report", err)
	}
	if _, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference, 1); err != nil {
		t.Fatal("fresh session refused", err)
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_financial_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("denied requests left report", count, err)
	}
}

func TestSourceFinancialConcurrentReferenceReplayCreatesOneImmutableRevision(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment", financialTestRow{"observed-payment", "", "0.00001", "18"})
	reference := financialReference(financialTestRow{"observed-payment", "", "0.00001", "18"})
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- report.Run.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for current := range ids {
		if id != "" && id != current {
			t.Fatal("concurrent replay duplicated report")
		}
		id = current
	}
	var count int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_financial_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestSourceFinancialAuditFailureRollsBackAndEvidenceCannotBeChanged(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	detail, snapshot := financialTestImport(t, s, actor, "payment", financialTestRow{"observed-payment", "", "0.01", "18"})
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_financial_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='statement.financials_reconciled' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_financial_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_financial_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	reference := financialReference(financialTestRow{"observed-payment", "", "0.01", "18"})
	if _, err = s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference); err == nil {
		t.Fatal("audit failure ignored")
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_financial_runs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("report escaped rollback", count, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_financial_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	report, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, reference)
	if err != nil || report.Run.Revision != 1 {
		t.Fatal(report.Run, err)
	}
	for _, query := range []string{`UPDATE merchant_statement_financials SET digest=digest`, `DELETE FROM merchant_statement_financials`, `UPDATE merchant_statement_financial_runs SET revision=revision`, `DELETE FROM merchant_statement_financial_runs`} {
		if _, err = s.DB.Exec(ctx, query); err == nil {
			t.Fatal("immutable financial evidence changed", query)
		}
	}
}

func TestSourceFinancialReportsBoundUnionAndLatestHistoryWithoutTruncatingFacts(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	observations, references := make([]financialTestRow, 2000), make([]financialTestRow, 2000)
	for i := range observations {
		observations[i] = financialTestRow{fmt.Sprintf("observed-%04d", i), "", "0.00001", "18"}
		references[i] = financialTestRow{fmt.Sprintf("reference-%04d", i), "", "0.00001", "18"}
	}
	detail, snapshot := financialTestImport(t, s, actor, "payment", observations...)
	report, err := s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, financialReference(references...))
	if err != nil || len(report.Rows) != 4000 || report.Run.DifferenceCount != 2000 || report.Run.UnassessedCount != 2000 || report.Run.MatchedCount != 0 {
		t.Fatal("bounded union lost facts", len(report.Rows), report.Run, err)
	}
	// Latest-history limits apply only to summaries, not the stored reports.
	for i := 0; i < 20; i++ {
		if _, err = s.ReconcileSourceFinancials(ctx, actor, detail.Statement.ID, snapshot.SourceID, financialReference(financialTestRow{"observed-0000", "", fmt.Sprintf("0.%05d", i), "18"})); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := s.SourceFinancials(ctx, detail.Statement.ID, snapshot.SourceID)
	if err != nil || len(latest.Runs) != 20 || latest.Runs[0].Revision != 21 || latest.Runs[19].Revision != 2 || len(latest.Rows) != 2000 {
		t.Fatal("latest limit changed source scope", len(latest.Rows), latest.Runs, err)
	}
	old, err := s.SourceFinancialReport(ctx, detail.Statement.ID, snapshot.SourceID, report.Run.ID)
	if err != nil || len(old.Rows) != 4000 {
		t.Fatal("old report truncated", err)
	}
}

func TestSourceFinancialMigrationKeepsLegacyFactsAndExplicitReimportEnrichesOnce(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := financialTestNative(t, "payment", financialTestRow{"observed-payment", "", "0.01", "18"})
	detail, err := s.ImportNativeStatement(ctx, actor, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TABLE merchant_statement_financial_runs;DROP TABLE merchant_statement_financials;DELETE FROM schema_migrations WHERE version=14`); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration not repeatable", err)
	}
	legacy, err := s.SourceFinancials(ctx, detail.Statement.ID, detail.Sources[0].ID)
	if err != nil || legacy.Available {
		t.Fatal("migration invented old fee data", legacy, err)
	}
	for i := 0; i < 2; i++ {
		same, err := s.ImportNativeStatement(ctx, actor, input)
		if err != nil || same.Statement.ID != detail.Statement.ID || same.LatestRun.ID != detail.LatestRun.ID || len(same.Sources) != 1 || same.Sources[0].ID != detail.Sources[0].ID {
			t.Fatal("explicit enrichment changed old gross evidence", same, err)
		}
	}
	enriched, err := s.SourceFinancials(ctx, detail.Statement.ID, detail.Sources[0].ID)
	if err != nil || !enriched.Available || *enriched.FeeTotalYuan != "0.01000" {
		t.Fatal("explicit old-file enrichment failed", enriched, err)
	}
}
