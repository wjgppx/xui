package store_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wjgppx/xui/internal/fundledger"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

type fundTestRow struct{ id, kind, direction, amount, balance, transfer, counterparty string }

func fundTestInput(provider, account, date, opening, closing string, entries ...fundTestRow) fundledger.Import {
	var content strings.Builder
	w := csv.NewWriter(&content)
	_ = w.Write([]string{"sequence", "entry_id", "occurred_at", "kind", "direction", "amount_yuan", "balance_after_yuan", "transfer_id", "counterparty_account_key"})
	for i, row := range entries {
		_ = w.Write([]string{strconv.Itoa(i + 1), row.id, date + "T10:00:00+08:00", row.kind, row.direction, row.amount, row.balance, row.transfer, row.counterparty})
	}
	w.Flush()
	return fundledger.Import{Provider: provider, AccountKey: account, Date: date, Coverage: "partial", OpeningBalanceYuan: opening, ClosingBalanceYuan: closing, CSV: content.String()}
}
func fundTestImport(t *testing.T, s *store.Store, actor string, input fundledger.Import) store.FundLedger {
	t.Helper()
	result, err := s.ImportFundLedger(context.Background(), actor, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func fundTestPair(t *testing.T, s *store.Store, actor string) (store.FundLedger, store.FundLedger) {
	t.Helper()
	merchant := fundTestImport(t, s, actor, fundTestInput("alipay", "merchant_main", "2026-10-27", "100", "81",
		fundTestRow{"withdraw-1", "withdrawal", "debit", "19", "81", "transfer-1", "bank_main"}))
	bank := fundTestImport(t, s, actor, fundTestInput("bank", "bank_main", "2026-10-29", "0", "19",
		fundTestRow{"bank-credit-1", "settlement", "credit", "19", "19", "transfer-1", "merchant_main"}))
	return merchant, bank
}
func fundTestCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

func TestFundLedgerImportPreservesPreciseDiscrepanciesWithoutChangingFinancialFacts(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, order, payment := paymentFixture(t, s, "alipay")
	input := fundTestInput("alipay", "merchant_main", "2026-10-29", "100", "120",
		fundTestRow{"entry-1", "payment", "credit", "19.12345", "120", "", ""})
	batch := fundTestImport(t, s, actor, input)
	if batch.ExpectedClosingBalanceYuan != "119.12345" || batch.ClosingBalanceYuan != "120.00000" || batch.BalanceDifferenceCount != 1 || !fundTestCode(batch.SummaryCodes, "closing_balance_mismatch") || len(batch.Rows) != 1 || batch.Rows[0].AmountYuan != "19.12345" || batch.Rows[0].ExpectedBalanceYuan != "119.12345" || batch.Rows[0].BalanceAfterYuan != "120.00000" {
		t.Fatal("source discrepancy was rounded, corrected or lost", batch)
	}
	readback, err := s.FundLedger(ctx, batch.ID)
	if err != nil || readback.ID != batch.ID || readback.Digest != batch.Digest || len(readback.Rows) != 1 || readback.Rows[0].StepExpectedBalanceYuan != "119.12345" {
		t.Fatal("full ledger read lost evidence", readback, err)
	}
	var paymentStatus, orderStatus string
	var grants, paymentEvents, actions int
	err = s.DB.QueryRow(ctx, `SELECT p.status,o.status,(SELECT count(*) FROM entitlement_periods),(SELECT count(*) FROM payment_events),(SELECT count(*) FROM payment_refunds) FROM payment_intents p JOIN orders o ON o.id=p.order_id WHERE p.id=$1 AND o.id=$2`, payment.ID, order.ID).Scan(&paymentStatus, &orderStatus, &grants, &paymentEvents, &actions)
	if err != nil || paymentStatus != "pending" || orderStatus != "pending" || grants+paymentEvents+actions != 0 {
		t.Fatal("ledger import became money authority", paymentStatus, orderStatus, grants, paymentEvents, actions, err)
	}
	large := fundTestImport(t, s, actor, fundTestInput("bank", "bank_large", "2026-10-29", "92233720368547758.07", "92233720368547758.06999",
		fundTestRow{"fee-1", "fee", "debit", "0.00001", "92233720368547758.06999", "", ""}))
	if large.ExpectedClosingBalanceYuan != "92233720368547758.06999" || large.BalanceDifferenceCount != 0 || len(large.SummaryCodes) != 0 {
		t.Fatal("large exact balance scaled through int64", large)
	}
}

func TestFundLedgerAliasPinsProviderAndOverlappingBatchesRemainSeparate(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := fundTestInput("alipay", "merchant_main", "2026-10-29", "0", "19",
		fundTestRow{"shared-entry", "payment", "credit", "19", "19", "", ""})
	first := fundTestImport(t, s, actor, input)
	input.ClosingBalanceYuan = "20"
	second := fundTestImport(t, s, actor, input)
	if first.ID == second.ID || first.ClosingBalanceYuan != "19.00000" || second.ClosingBalanceYuan != "20.00000" || second.RowCount != 1 {
		t.Fatal("corrected/overlapping batch replaced prior source", first, second)
	}
	input.Provider = "wechat"
	if _, err := s.ImportFundLedger(ctx, actor, input); !errors.Is(err, store.ErrFundAccountConflict) {
		t.Fatal("provider silently changed for account alias", err)
	}
	items, err := s.ListFundLedgers(ctx)
	if err != nil || len(items) != 2 || items[0].RowCount != 1 || items[1].RowCount != 1 {
		t.Fatal("batches were aggregated or scoped incorrectly", items, err)
	}
	data, err := json.Marshal(items)
	if err != nil || strings.Contains(string(data), `"rows"`) || strings.Contains(string(data), `"csv"`) {
		t.Fatal("summary included raw rows or CSV", err)
	}
}

func TestFundLedgerConcurrentImportReplayKeepsOneBatchAndAudit(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := fundTestInput("bank", "bank_main", "2026-10-29", "0", "0")
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch, err := s.ImportFundLedger(ctx, actor, input)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- batch.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for current := range ids {
		if id != "" && current != id {
			t.Fatal("duplicate import got a new identity")
		}
		id = current
	}
	var accounts, batches, audits int
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM fund_ledger_accounts),(SELECT count(*) FROM fund_ledger_batches),(SELECT count(*) FROM audit_events WHERE action='fund_ledger.imported')`).Scan(&accounts, &batches, &audits); err != nil || id == "" || accounts != 1 || batches != 1 || audits != 1 {
		t.Fatal(accounts, batches, audits, err)
	}
	input.OpeningBalanceYuan, input.ClosingBalanceYuan = "-0.00000", "0.0"
	again := fundTestImport(t, s, actor, input)
	if again.ID != id {
		t.Fatal("semantic decimal replay duplicated the observation")
	}
}

func TestFundLedgerComparisonIsAccountBoundAcrossDatesAndRetainsAmbiguity(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	merchant := fundTestImport(t, s, actor, fundTestInput("wechat", "merchant_main", "2026-10-27", "100", "29",
		fundTestRow{"withdraw-match", "withdrawal", "debit", "19", "81", "transfer-match", "bank_main"},
		fundTestRow{"withdraw-other-bank", "withdrawal", "debit", "19", "62", "transfer-other", "other_bank"},
		fundTestRow{"withdraw-ambiguous-1", "withdrawal", "debit", "10", "52", "transfer-duplicate", "bank_main"},
		fundTestRow{"withdraw-ambiguous-2", "withdrawal", "debit", "10", "42", "transfer-duplicate", "bank_main"},
		fundTestRow{"withdraw-unlisted", "withdrawal", "debit", "12", "30", "transfer-unlisted", "bank_main"},
		fundTestRow{"fee", "fee", "debit", "1", "29", "", ""}))
	bank := fundTestImport(t, s, actor, fundTestInput("bank", "bank_main", "2026-10-29", "0", "58",
		fundTestRow{"bank-match", "settlement", "credit", "19", "19", "transfer-match", "merchant_main"},
		fundTestRow{"bank-other-merchant", "settlement", "credit", "19", "38", "transfer-other", "other_merchant"},
		fundTestRow{"bank-ambiguous", "settlement", "credit", "20", "58", "transfer-duplicate", "merchant_main"}))
	report, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID)
	if err != nil || report.MatchedCount != 1 || report.AmbiguousCount != 1 || report.UnmatchedCount != 1 || report.DifferenceCount != 0 || report.MerchantOutOfScopeCount != 1 || report.BankOutOfScopeCount != 1 || report.MerchantOtherCount != 1 || len(report.Rows) != 3 || !fundTestCode(report.SourceWarnings, "merchant_partial") || !fundTestCode(report.SourceWarnings, "bank_partial") {
		t.Fatal("comparison guessed across accounts/dates or merged duplicates", report, err)
	}
	readback, err := s.FundReconciliation(ctx, report.ID)
	if err != nil || readback.ID != report.ID || len(readback.Rows) != 3 || readback.MerchantBatchID != merchant.ID || readback.BankBatchID != bank.ID {
		t.Fatal("readback lost comparison scope", readback, err)
	}
	for _, row := range readback.Rows {
		if row.TransferID == "transfer-duplicate" && (len(row.MerchantEntries) != 2 || len(row.BankEntries) != 1 || row.Status != "ambiguous") {
			t.Fatal("ambiguous evidence was truncated", row)
		}
		if row.TransferID == "transfer-match" && row.MerchantEntries[0].OccurredAt.Equal(row.BankEntries[0].OccurredAt) {
			t.Fatal("cross-day times were overwritten")
		}
	}
}

func TestFundLedgerConcurrentComparisonDeduplicatesPairAndRejectsWrongScope(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	merchant, bank := fundTestPair(t, s, actor)
	var wg sync.WaitGroup
	ids := make(chan string, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- report.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for current := range ids {
		if id != "" && id != current {
			t.Fatal("same immutable pair created multiple reports")
		}
		id = current
	}
	var reports, audits int
	if err := s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM fund_ledger_reconciliations),(SELECT count(*) FROM audit_events WHERE action='fund_ledger.reconciled')`).Scan(&reports, &audits); err != nil || id == "" || reports != 1 || audits != 1 {
		t.Fatal(reports, audits, err)
	}
	for _, pair := range [][2]string{{bank.ID, merchant.ID}, {merchant.ID, merchant.ID}} {
		if _, err := s.ReconcileFundLedgers(ctx, actor, pair[0], pair[1]); !errors.Is(err, store.ErrFundLedgerConflict) {
			t.Fatal("wrong provider roles accepted", err)
		}
	}
	if _, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, store.ID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unknown batch not rejected", err)
	}
}

func TestFundLedgerWritesRequireCurrentRoleAndSessionVersion(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	merchant, bank := fundTestPair(t, s, actor)
	if _, err := s.DB.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	input := fundTestInput("bank", "bank_new", "2026-10-29", "0", "0")
	if _, err := s.ImportFundLedger(ctx, actor, input, 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("invalidated session imported a ledger", err)
	}
	if _, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID, 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("invalidated session created comparison", err)
	}
	if _, err := s.ImportFundLedger(ctx, actor, input, 1); err != nil {
		t.Fatal("current session could not import", err)
	}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET role='support' WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID, 1); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("support role created comparison", err)
	}
}

func TestFundLedgerAuditFailuresRollbackAndEvidenceIsImmutable(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_fund_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='fund_ledger.imported' THEN RAISE EXCEPTION 'fixture unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_fund_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_fund_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ImportFundLedger(ctx, actor, fundTestInput("bank", "bank_main", "2026-10-29", "0", "0")); err == nil {
		t.Fatal("failed audit committed account/import")
	}
	var accounts, batches int
	if err = s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM fund_ledger_accounts),(SELECT count(*) FROM fund_ledger_batches)`).Scan(&accounts, &batches); err != nil || accounts+batches != 0 {
		t.Fatal("partial import survived rollback", accounts, batches, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_fund_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	merchant, bank := fundTestPair(t, s, actor)
	_, err = s.DB.Exec(ctx, `CREATE OR REPLACE FUNCTION reject_fund_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='fund_ledger.reconciled' THEN RAISE EXCEPTION 'fixture unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_fund_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_fund_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID); err == nil {
		t.Fatal("comparison audit failure ignored")
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM fund_ledger_reconciliations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("report survived audit rollback", count, err)
	}
	if _, err = s.DB.Exec(ctx, `DROP TRIGGER reject_fund_audit ON audit_events`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID); err != nil {
		t.Fatal("comparison could not recover", err)
	}
	for _, query := range []string{`UPDATE fund_ledger_accounts SET provider=provider`, `DELETE FROM fund_ledger_accounts`, `UPDATE fund_ledger_batches SET digest=digest`, `DELETE FROM fund_ledger_batches`, `UPDATE fund_ledger_reconciliations SET version=version`, `DELETE FROM fund_ledger_reconciliations`} {
		if _, err = s.DB.Exec(ctx, query); err == nil {
			t.Fatal("immutable evidence changed", query)
		}
	}
}

func TestFundLedgerLargeSnapshotsAndReportsRemainCompleteWhileListsUseMetadata(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	merchantRows, bankRows := make([]fundTestRow, 2000), make([]fundTestRow, 2000)
	for i := range merchantRows {
		merchantRows[i] = fundTestRow{fmt.Sprintf("merchant-%04d", i), "withdrawal", "debit", "1", strconv.Itoa(1999 - i), fmt.Sprintf("merchant-transfer-%04d", i), "bank_main"}
		bankRows[i] = fundTestRow{fmt.Sprintf("bank-%04d", i), "settlement", "credit", "1", strconv.Itoa(i + 1), fmt.Sprintf("bank-transfer-%04d", i), "merchant_main"}
	}
	merchant := fundTestImport(t, s, actor, fundTestInput("alipay", "merchant_main", "2026-10-28", "2000", "0", merchantRows...))
	bank := fundTestImport(t, s, actor, fundTestInput("bank", "bank_main", "2026-10-29", "0", "2000", bankRows...))
	report, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID)
	if err != nil || len(report.Rows) != 4000 || report.UnmatchedCount != 4000 || report.MatchedCount != 0 {
		t.Fatal("union report truncated or falsely matched", len(report.Rows), report.UnmatchedCount, err)
	}
	readback, err := s.FundReconciliation(ctx, report.ID)
	if err != nil || len(readback.Rows) != 4000 {
		t.Fatal("stored report lost bounded rows", err)
	}
	ledgerList, err := s.ListFundLedgers(ctx)
	if err != nil || len(ledgerList) != 2 {
		t.Fatal(err)
	}
	reportList, err := s.ListFundReconciliations(ctx)
	if err != nil || len(reportList) != 1 || reportList[0].UnmatchedCount != 4000 {
		t.Fatal("metadata list lost counts", reportList, err)
	}
	data, err := json.Marshal(struct {
		Ledgers []store.FundLedgerSummary
		Reports []store.FundReconciliationSummary
	}{ledgerList, reportList})
	if err != nil || strings.Contains(string(data), `"rows"`) || strings.Contains(string(data), "merchant-transfer-") {
		t.Fatal("metadata lists exposed full row payload", err)
	}
}

func TestFundLedgerMigrationPreservesVersionFourteenFactsAndIsRepeatable(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	_, order, payment := paymentFixture(t, s, "alipay")
	_, err := s.DB.Exec(ctx, `DROP TABLE fund_ledger_reconciliations;DROP TABLE fund_ledger_batches;DROP TABLE fund_ledger_accounts;DELETE FROM schema_migrations WHERE version=15`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration not repeatable", err)
	}
	var originalOrder, originalPayment string
	if err = s.DB.QueryRow(ctx, `SELECT o.id,p.id FROM orders o JOIN payment_intents p ON p.order_id=o.id WHERE o.id=$1`, order.ID).Scan(&originalOrder, &originalPayment); err != nil || originalOrder != order.ID || originalPayment != payment.ID {
		t.Fatal("migration changed old financial facts", err)
	}
	if _, err = s.ImportFundLedger(ctx, actor, fundTestInput("bank", "bank_after_migration", "2026-10-29", "0", "0")); err != nil {
		t.Fatal("new ledger unavailable after upgrade", err)
	}
}
