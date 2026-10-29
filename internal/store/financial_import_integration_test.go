package store_test

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func nativeFinancialImport(t *testing.T, paymentID, fee, channelAmount string) statements.NativeImport {
	t.Helper()
	input := billStoreNative(t, paymentID)
	data, err := base64.StdEncoding.DecodeString(input.ContentBase64)
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	records[1][12], records[1][22] = channelAmount, fee
	var out strings.Builder
	w := csv.NewWriter(&out)
	if err = w.WriteAll(records); err != nil {
		t.Fatal(err)
	}
	input.ContentBase64 = base64.StdEncoding.EncodeToString([]byte(out.String()))
	return input
}

func financialSourceID(t *testing.T, detail store.StatementDetail, digest string) string {
	t.Helper()
	for _, source := range detail.Sources {
		if source.Digest == digest && source.Format == "alipay_trade_v1" {
			return source.ID
		}
	}
	t.Fatal("raw financial source absent")
	return ""
}

func TestNativeFinancialImportsPreserveDifferentFeesForTheSameGrossStatement(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, _, payment := paymentFixture(t, s, "alipay")
	firstInput := nativeFinancialImport(t, payment.ID, "0.00001", "18.00")
	firstResult, err := statements.ParseNative(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ImportNativeStatement(ctx, actor, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := nativeFinancialImport(t, payment.ID, "-0.00002", "17.99")
	secondResult, err := statements.ParseNative(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ImportNativeStatement(ctx, actor, secondInput)
	if err != nil || second.Statement.ID != first.Statement.ID || len(second.Sources) != 2 || len(second.Runs) != 1 || second.LatestRun.ID != first.LatestRun.ID {
		t.Fatal("financial observations replaced gross facts or reports", second, err)
	}
	firstSource := financialSourceID(t, second, firstResult.SourceDigest)
	secondSource := financialSourceID(t, second, secondResult.SourceDigest)
	for _, fixture := range []struct{ source, fee, amount string }{{firstSource, "0.00001", "18.00000"}, {secondSource, "-0.00002", "17.99000"}} {
		observed, err := s.SourceFinancials(ctx, second.Statement.ID, fixture.source)
		if err != nil || !observed.Available || observed.FeeTotalYuan == nil || *observed.FeeTotalYuan != fixture.fee || observed.ChannelAmountTotalYuan == nil || *observed.ChannelAmountTotalYuan != fixture.amount {
			t.Fatal("source-level exact values changed", observed, err)
		}
	}
	again, err := s.ImportNativeStatement(ctx, actor, firstInput)
	if err != nil || len(again.Sources) != 2 {
		t.Fatal("same financial source duplicated", err)
	}
	var snapshots, audits, grants int
	var status string
	if err = s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statement_financials),(SELECT count(*) FROM audit_events WHERE action='statement.financials_added'),(SELECT count(*) FROM entitlement_periods),status FROM payment_intents WHERE id=$1`, payment.ID).Scan(&snapshots, &audits, &grants, &status); err != nil || snapshots != 2 || audits != 2 || grants != 0 || status != "pending" {
		t.Fatal("financial imports duplicated or changed business state", snapshots, audits, grants, status, err)
	}
}

func TestExplicitReimportEnrichesAnExistingLegacyRawSourceOnlyOnce(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := nativeFinancialImport(t, "legacy-financial-payment", "0.12345", "18.00")
	result, err := statements.ParseNative(input)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := s.ImportStatement(ctx, actor, result.Input)
	if err != nil {
		t.Fatal(err)
	}
	sourceID := store.ID()
	if _, err = s.DB.Exec(ctx, `INSERT INTO merchant_statement_sources(id,statement_id,source_format,source_kind,digest,integrity,imported_by) VALUES($1,$2,'alipay_trade_v1','upload',$3,'unverified_upload',$4)`, sourceID, detail.Statement.ID, result.SourceDigest, actor); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.SourceFinancials(ctx, detail.Statement.ID, sourceID)
	if err != nil || legacy.Available || legacy.FeeTotalYuan != nil {
		t.Fatal("legacy source was guessed to have financial observations", legacy, err)
	}
	for i := 0; i < 2; i++ {
		again, err := s.ImportNativeStatement(ctx, actor, input)
		if err != nil || len(again.Sources) != 2 || again.LatestRun.ID != detail.LatestRun.ID {
			t.Fatal("enrichment duplicated source or historical gross run", err)
		}
	}
	enriched, err := s.SourceFinancials(ctx, detail.Statement.ID, sourceID)
	if err != nil || !enriched.Available || enriched.FeeTotalYuan == nil || *enriched.FeeTotalYuan != "0.12345" {
		t.Fatal(enriched, err)
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='statement.financials_added' AND resource_id=$1`, sourceID).Scan(&count); err != nil || count != 1 {
		t.Fatal("enrichment audit not idempotent", count, err)
	}
}

func TestNativeFinancialSnapshotAuditFailureRollsBackEntireImport(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_financial_import_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='statement.financials_added' THEN RAISE EXCEPTION 'fixture unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_financial_import_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_financial_import_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	input := nativeFinancialImport(t, "atomic-financial-payment", "0.12345", "18.00")
	if _, err = s.ImportNativeStatement(ctx, actor, input); err == nil {
		t.Fatal("failed financial snapshot audit committed import")
	}
	var facts, sources, snapshots, runs int
	if err = s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM merchant_statements),(SELECT count(*) FROM merchant_statement_sources),(SELECT count(*) FROM merchant_statement_financials),(SELECT count(*) FROM merchant_statement_runs)`).Scan(&facts, &sources, &snapshots, &runs); err != nil || facts+sources+snapshots+runs != 0 {
		t.Fatal("partial import survived rollback", facts, sources, snapshots, runs, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_financial_import_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportNativeStatement(ctx, actor, input); err != nil {
		t.Fatal("valid retry did not recover", err)
	}
}
