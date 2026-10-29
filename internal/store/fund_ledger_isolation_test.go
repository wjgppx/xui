package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/fundledger"
	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

func fundBusinessSnapshot(t *testing.T, s *store.Store) map[string]string {
	t.Helper()
	result := map[string]string{}
	// Compare existing business evidence, rather than empty-table counts. Ledger
	// rows deliberately reuse real fixture payment/refund IDs below.
	for _, table := range []string{"orders", "payment_intents", "payment_events", "payment_refunds", "payment_action_jobs", "entitlement_periods", "outbox_events", "merchant_statements", "merchant_statement_rows", "merchant_statement_runs", "merchant_statement_sources", "merchant_statement_financials"} {
		var data []byte
		err := s.DB.QueryRow(context.Background(), `SELECT COALESCE(jsonb_agg(to_jsonb(snapshot) ORDER BY to_jsonb(snapshot)::text),'[]'::jsonb) FROM `+table+` snapshot`).Scan(&data)
		if err != nil {
			t.Fatal("business evidence snapshot", table, err)
		}
		result[table] = string(data)
	}
	return result
}

func TestFundLedgerObservationsCannotSettleOrRefundExistingBusinessFacts(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor, _, _, paid, refund := refundFixture(t, s)
	_, _, pending := paymentFixture(t, s, "alipay")
	if _, err := s.ImportNativeStatement(ctx, actor, nativeFinancialImport(t, paid.ID, "0.12345", "18.00")); err != nil {
		t.Fatal(err)
	}
	before := fundBusinessSnapshot(t, s)
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	merchantDate := time.Now().In(zone).AddDate(0, 0, -1).Format("2006-01-02")
	bankDate := time.Now().In(zone).Format("2006-01-02")
	header := "sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\n"
	merchantCSV := header + strings.Join([]string{"1", pending.ID, merchantDate + "T12:00:00+08:00", "payment", "credit", "3.00001", "103.00001", "", ""}, ",") + "\n" + strings.Join([]string{"2", paid.ID, merchantDate + "T12:01:00+08:00", "withdrawal", "debit", "5.12345", "97.87656", refund.ID, "bank_ops"}, ",") + "\n"
	merchantInput := fundledger.Import{Provider: "alipay", AccountKey: "alipay_main", Date: merchantDate, Coverage: "partial", OpeningBalanceYuan: "100", ClosingBalanceYuan: "97.87656", CSV: merchantCSV}
	merchant, err := s.ImportFundLedger(ctx, actor, merchantInput, 0)
	if err != nil || merchant.BalanceDifferenceCount != 0 || len(merchant.SummaryCodes) != 0 {
		t.Fatal("merchant observation import", err)
	}
	bankCSV := header + strings.Join([]string{"1", paid.ID, bankDate + "T12:00:00+08:00", "settlement", "credit", "5.12345", "5.12345", refund.ID, "alipay_main"}, ",") + "\n"
	bank, err := s.ImportFundLedger(ctx, actor, fundledger.Import{Provider: "bank", AccountKey: "bank_ops", Date: bankDate, Coverage: "partial", OpeningBalanceYuan: "0", ClosingBalanceYuan: "5.12345", CSV: bankCSV}, 0)
	if err != nil {
		t.Fatal("bank observation import", err)
	}
	comparison, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID, 0)
	if err != nil || comparison.MatchedCount != 1 || comparison.UnmatchedCount != 0 || len(comparison.SourceWarnings) != 2 {
		t.Fatal("cross-day observation comparison", err)
	}
	// A repeated file and repeated comparison reuse evidence; neither consumes a
	// transfer nor posts money to any platform balance or existing payment.
	again, err := s.ImportFundLedger(ctx, actor, merchantInput, 0)
	if err != nil || again.ID != merchant.ID {
		t.Fatal("source replay duplicated ledger", err)
	}
	replayed, err := s.ReconcileFundLedgers(ctx, actor, merchant.ID, bank.ID, 0)
	if err != nil || replayed.ID != comparison.ID {
		t.Fatal("comparison replay duplicated evidence", err)
	}
	after := fundBusinessSnapshot(t, s)
	for table, expected := range before {
		if after[table] != expected {
			t.Fatal("fund ledger changed existing business facts", table)
		}
	}
}
