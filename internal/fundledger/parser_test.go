package fundledger_test

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/fundledger"
)

const ledgerHeader = "sequence,entry_id,occurred_at,kind,direction,amount_yuan,balance_after_yuan,transfer_id,counterparty_account_key\n"

func input(provider, account, date, opening, closing string, rows ...[]string) fundledger.Import {
	var out strings.Builder
	out.WriteString(ledgerHeader)
	w := csv.NewWriter(&out)
	for i, row := range rows {
		values := append([]string{fmt.Sprint(i + 1)}, row...)
		_ = w.Write(values)
	}
	w.Flush()
	return fundledger.Import{Provider: provider, AccountKey: account, Date: date, Coverage: "partial", OpeningBalanceYuan: opening, ClosingBalanceYuan: closing, CSV: out.String()}
}

func row(id, date, kind, direction, amount, balance, transfer, account string) []string {
	return []string{id, date + "T12:00:00+08:00", kind, direction, amount, balance, transfer, account}
}
func parse(t *testing.T, value fundledger.Import) fundledger.Document {
	t.Helper()
	doc, err := fundledger.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func TestBalancesPreserveFiveDecimalsAndNegativeObservations(t *testing.T) {
	doc := parse(t, input("alipay", "merchant_main", "2026-10-28", "100", "99.99999",
		row("in-1", "2026-10-28", "payment", "credit", "0.00001", "100.00001", "", ""),
		row("out-1", "2026-10-28", "withdrawal", "debit", "0.00002", "99.99999", "transfer-1", "bank_main")))
	if doc.TotalCreditYuan != "0.00001" || doc.TotalDebitYuan != "0.00002" || doc.ExpectedClosingBalanceYuan != "99.99999" || doc.BalanceDifferenceCount != 0 || len(doc.SummaryCodes) != 0 || doc.Rows[1].StepExpectedBalanceYuan != "99.99999" {
		t.Fatal("sub-cent funds movements lost precision", doc.Metadata)
	}
	negative := parse(t, input("bank", "bank_main", "2026-10-29", "-5.5", "-5.62345", row("fee-1", "2026-10-29", "fee", "debit", "0.12345", "-5.62345", "", "")))
	if negative.OpeningBalanceYuan != "-5.50000" || negative.ExpectedClosingBalanceYuan != "-5.62345" || negative.BalanceDifferenceCount != 0 {
		t.Fatal("negative balance observation changed", negative.Metadata)
	}
}

func TestBalanceDiscrepanciesRemainEvidenceAndLocateTheFirstBrokenStep(t *testing.T) {
	doc := parse(t, input("bank", "bank_main", "2026-10-29", "100", "99",
		row("wrong-first", "2026-10-29", "other", "debit", "10", "95", "", ""),
		row("continued", "2026-10-29", "other", "credit", "5", "100", "", "")))
	if doc.Rows[0].BalanceAfterYuan != "95.00000" || doc.Rows[0].ExpectedBalanceYuan != "90.00000" || len(doc.Rows[0].Codes) != 2 {
		t.Fatal("bad source balance was repaired or discarded")
	}
	if doc.Rows[1].ExpectedBalanceYuan != "95.00000" || doc.Rows[1].StepExpectedBalanceYuan != "100.00000" || len(doc.Rows[1].Codes) != 1 || !contains(doc.Rows[1].Codes, "running_balance_mismatch") || doc.BalanceDifferenceCount != 2 {
		t.Fatal("local and cumulative checks were conflated")
	}
	if !contains(doc.SummaryCodes, "closing_balance_mismatch") || !contains(doc.SummaryCodes, "closing_entry_mismatch") || doc.ClosingBalanceYuan != "99.00000" {
		t.Fatal("closing evidence missing", doc.Metadata)
	}
	zero := parse(t, input("bank", "bank_zero", "2026-10-29", "-0.00000", "0"))
	if zero.Rows == nil || zero.SummaryCodes == nil || zero.ExpectedClosingBalanceYuan != "0.00000" || len(zero.SummaryCodes) != 0 {
		t.Fatal("empty explicit scope was not preserved")
	}
	unequal := parse(t, input("bank", "bank_empty", "2026-10-29", "1", "2"))
	if !contains(unequal.SummaryCodes, "closing_balance_mismatch") {
		t.Fatal("empty scope falsely reconciled unequal balances")
	}
}

func TestLedgerDigestNormalizesRepresentationButPreservesSameTimeSequence(t *testing.T) {
	first := input("bank", "bank_main", "2026-10-29", "0", "3",
		row("credit-a", "2026-10-29", "other", "credit", "1", "1", "", ""),
		row("credit-b", "2026-10-29", "other", "credit", "2", "3", "", ""))
	a := parse(t, first)
	equivalent := first
	equivalent.OpeningBalanceYuan, equivalent.ClosingBalanceYuan = "-0.0", "3.00000"
	equivalent.CSV = "\ufeff" + strings.ReplaceAll(strings.ReplaceAll(first.CSV, "2026-10-29T12:00:00+08:00", "2026-10-29T04:00:00.000000100Z"), ",1,1,,", ",1.00000,1.00000,,")
	b := parse(t, equivalent)
	if a.Digest != b.Digest || !b.Rows[0].OccurredAt.Equal(a.Rows[0].OccurredAt) {
		t.Fatal("equivalent input created different evidence")
	}
	reversed := parse(t, input("bank", "bank_main", "2026-10-29", "0", "3",
		row("credit-b", "2026-10-29", "other", "credit", "2", "2", "", ""),
		row("credit-a", "2026-10-29", "other", "credit", "1", "3", "", "")))
	if reversed.Digest == a.Digest || reversed.Rows[0].EntryID != "credit-b" {
		t.Fatal("source sequence was sorted away")
	}
}

func TestLedgerRejectsMalformedIdentityMoneySequenceAndScopeWithoutEchoingInput(t *testing.T) {
	base := input("alipay", "merchant_main", "2026-10-28", "100", "90", row("out-1", "2026-10-28", "withdrawal", "debit", "10", "90", "transfer-1", "bank_main"))
	changes := []struct {
		name   string
		change func(*fundledger.Import)
	}{
		{"provider", func(v *fundledger.Import) { v.Provider = "private-provider" }},
		{"alias", func(v *fundledger.Import) { v.AccountKey = "Private-bank-number" }},
		{"date", func(v *fundledger.Import) { v.Date = "2026-02-30" }},
		{"coverage", func(v *fundledger.Import) { v.Coverage = "trusted" }},
		{"opening", func(v *fundledger.Import) { v.OpeningBalanceYuan = "1e2" }},
		{"closing", func(v *fundledger.Import) { v.ClosingBalanceYuan = "+90" }},
		{"zero", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, ",10,90,", ",0,90,", 1) }},
		{"negative_amount", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, ",10,90,", ",-10,90,", 1) }},
		{"precision", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, ",10,90,", ",10.000001,90,", 1) }},
		{"amount_size", func(v *fundledger.Import) {
			v.CSV = strings.Replace(v.CSV, ",10,90,", ",10000000000000000000000000,90,", 1)
		}},
		{"sequence", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, "\n1,", "\n2,", 1) }},
		{"cross_day", func(v *fundledger.Import) {
			v.CSV = strings.ReplaceAll(v.CSV, "T12:00:00+08:00", "T00:00:00Z")
			v.Date = "2026-10-27"
		}},
		{"timezone", func(v *fundledger.Import) { v.CSV = strings.ReplaceAll(v.CSV, "+08:00", "") }},
		{"wrong_direction", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, ",debit,", ",credit,", 1) }},
		{"missing_reference", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, "transfer-1", "", 1) }},
		{"self_account", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, "bank_main", "merchant_main", 1) }},
		{"noncandidate_reference", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, ",withdrawal,", ",other,", 1) }},
		{"blank", func(v *fundledger.Import) { v.CSV += "\n" }},
		{"control", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, "out-1", "private\x00entry", 1) }},
		{"utf8", func(v *fundledger.Import) { v.CSV += string([]byte{255}) }},
		{"header", func(v *fundledger.Import) { v.CSV = strings.Replace(v.CSV, "sequence", "Sequence", 1) }},
		{"columns", func(v *fundledger.Import) {
			v.CSV = strings.Replace(v.CSV, ",bank_main", ",bank_main,private-extra", 1)
		}},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			value := base
			tc.change(&value)
			_, err := fundledger.Parse(value)
			var validation *fundledger.ValidationError
			if !errors.Is(err, fundledger.ErrInvalid) || !errors.As(err, &validation) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "transfer-1") {
				t.Fatal("malformed observation was accepted or echoed", err)
			}
		})
	}
}

func TestLedgerRejectsDuplicateEntriesAndEarlierTimesWhileAllowingDuplicateTransferReferences(t *testing.T) {
	value := input("alipay", "merchant_main", "2026-10-28", "20", "0",
		row("out-1", "2026-10-28", "withdrawal", "debit", "10", "10", "same-transfer", "bank_main"),
		row("out-2", "2026-10-28", "withdrawal", "debit", "10", "0", "same-transfer", "bank_main"))
	parse(t, value)
	duplicate := value
	duplicate.CSV = strings.ReplaceAll(duplicate.CSV, "out-2", "out-1")
	if _, err := fundledger.Parse(duplicate); !errors.Is(err, fundledger.ErrInvalid) {
		t.Fatal("duplicate entry accepted", err)
	}
	earlier := value
	earlier.CSV = strings.Replace(earlier.CSV, "2,out-2,2026-10-28T12:00", "2,out-2,2026-10-28T11:00", 1)
	if _, err := fundledger.Parse(earlier); !errors.Is(err, fundledger.ErrInvalid) {
		t.Fatal("earlier timestamp accepted", err)
	}
}

func TestLedgerMaximumRowsAndLargeComputedBalancesDoNotOverflowInputPrecision(t *testing.T) {
	rows := make([][]string, 2000)
	for i := range rows {
		rows[i] = row(fmt.Sprintf("entry-%d", i), "2026-10-29", "other", "credit", "1000000000000000000000000", "0", "", "")
	}
	value := input("bank", "bank_main", "2026-10-29", "0", "0", rows...)
	doc := parse(t, value)
	if doc.RowCount != 2000 || doc.ExpectedClosingBalanceYuan != "2000000000000000000000000000.00000" || doc.TotalCreditYuan != doc.ExpectedClosingBalanceYuan || doc.BalanceDifferenceCount != 2000 {
		t.Fatal("computed balances overflowed or truncated", doc.Metadata)
	}
	value.CSV += "2001,extra,2026-10-29T12:00:00+08:00,other,credit,1,0,,\n"
	if _, err := fundledger.Parse(value); !errors.Is(err, fundledger.ErrInvalid) {
		t.Fatal("row overflow accepted", err)
	}
	value.CSV = strings.Repeat("x", fundledger.MaxBytes+1)
	if _, err := fundledger.Parse(value); !errors.Is(err, fundledger.ErrInvalid) {
		t.Fatal("byte overflow accepted", err)
	}
}

func TestCompareKeepsAccountScopesCrossDaySignsAndAmbiguousEvidence(t *testing.T) {
	m := parse(t, input("wechat", "merchant_main", "2026-10-28", "0", "0",
		row("m-match", "2026-10-28", "withdrawal", "debit", "3.12345", "0", "match", "bank_main"),
		row("m-diff", "2026-10-28", "withdrawal", "debit", "20", "0", "diff", "bank_main"),
		row("m-only", "2026-10-28", "withdrawal", "debit", "5", "0", "merchant-only", "bank_main"),
		row("m-dup-1", "2026-10-28", "withdrawal", "debit", "1", "0", "dup", "bank_main"),
		row("m-dup-2", "2026-10-28", "withdrawal", "debit", "2", "0", "dup", "bank_main"),
		row("m-out", "2026-10-28", "withdrawal", "debit", "3.12345", "0", "match", "another_bank"),
		row("m-other", "2026-10-28", "fee", "credit", "1", "0", "", "")))
	b := parse(t, input("bank", "bank_main", "2026-10-29", "0", "0",
		row("b-match", "2026-10-29", "settlement", "credit", "3.12345", "0", "match", "merchant_main"),
		row("b-diff", "2026-10-29", "settlement", "credit", "19.9", "0", "diff", "merchant_main"),
		row("b-only", "2026-10-29", "settlement", "credit", "7", "0", "bank-only", "merchant_main"),
		row("b-dup", "2026-10-29", "settlement", "credit", "3", "0", "dup", "merchant_main"),
		row("b-out", "2026-10-29", "settlement", "credit", "3.12345", "0", "match", "another_merchant"),
		row("b-other", "2026-10-29", "withdrawal", "debit", "1", "0", "", "")))
	result, err := fundledger.Compare(m, b)
	if err != nil || result.MatchedCount != 1 || result.DifferenceCount != 1 || result.UnmatchedCount != 2 || result.AmbiguousCount != 1 || len(result.Rows) != 5 || result.MerchantCandidateCount != 5 || result.BankCandidateCount != 4 || result.MerchantOutOfScopeCount != 1 || result.BankOutOfScopeCount != 1 || result.MerchantOtherCount != 1 || result.BankOtherCount != 1 || len(result.SourceWarnings) != 4 {
		t.Fatal("comparison scope or evidence lost", result, err)
	}
	for _, group := range result.Rows {
		if group.TransferID == "dup" && (len(group.MerchantEntries) != 2 || len(group.BankEntries) != 1 || !contains(group.Codes, "duplicate_transfer_id")) {
			t.Fatal("duplicate reference was guessed or summed")
		}
		if group.TransferID == "merchant-only" && !contains(group.Codes, "bank_record_missing") {
			t.Fatal("missing bank scope was not explicit")
		}
	}
}

func TestCompareDoesNotGuessReferencesOrPairUnrelatedAccounts(t *testing.T) {
	m := parse(t, input("alipay", "merchant_main", "2026-10-28", "1", "0", row("m", "2026-10-28", "withdrawal", "debit", "1", "0", "same-money", "other_bank")))
	b := parse(t, input("bank", "bank_main", "2026-10-29", "0", "1", row("b", "2026-10-29", "settlement", "credit", "1", "1", "same-money", "other_merchant")))
	result, err := fundledger.Compare(m, b)
	if err != nil || len(result.Rows) != 0 || result.Rows == nil || result.MatchedCount != 0 || result.MerchantOutOfScopeCount != 1 || result.BankOutOfScopeCount != 1 {
		t.Fatal("unrelated accounts matched", result, err)
	}
	m = parse(t, input("alipay", "merchant_main", "2026-10-28", "1", "0", row("m", "2026-10-28", "withdrawal", "debit", "1", "0", "merchant-ref", "bank_main")))
	b = parse(t, input("bank", "bank_main", "2026-10-29", "0", "1", row("b", "2026-10-29", "settlement", "credit", "1", "1", "bank-ref", "merchant_main")))
	result, err = fundledger.Compare(m, b)
	if err != nil || result.MatchedCount != 0 || result.UnmatchedCount != 2 {
		t.Fatal("equal money without shared reference matched", result, err)
	}
}

func TestCompareRejectsForgedSnapshotsAndAcceptsJSONRoundTrips(t *testing.T) {
	m := parse(t, input("alipay", "merchant_main", "2026-10-28", "1", "0", row("m", "2026-10-28", "withdrawal", "debit", "1", "0", "transfer", "bank_main")))
	b := parse(t, input("bank", "bank_main", "2026-10-29", "0", "1", row("b", "2026-10-29", "settlement", "credit", "1", "1", "transfer", "merchant_main")))
	clone := func() fundledger.Document {
		data, _ := json.Marshal(m)
		var result fundledger.Document
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result, err := fundledger.Compare(clone(), b); err != nil || result.MatchedCount != 1 {
		t.Fatal("stored JSON observation could not be compared", err)
	}
	for _, mutate := range []func(*fundledger.Document){
		func(d *fundledger.Document) { d.Rows[0].AmountYuan = "0" },
		func(d *fundledger.Document) { d.Rows[0].ExpectedBalanceYuan = "999.00000" },
		func(d *fundledger.Document) { d.Rows[0].Codes = []string{"fake"} },
		func(d *fundledger.Document) { d.Digest = strings.Repeat("0", 64) },
		func(d *fundledger.Document) { d.TotalDebitYuan = "0.00000" },
		func(d *fundledger.Document) { d.RowCount++ },
		func(d *fundledger.Document) { d.Currency = "USD" },
		func(d *fundledger.Document) { d.Rows = nil },
	} {
		value := clone()
		mutate(&value)
		if _, err := fundledger.Compare(value, b); !errors.Is(err, fundledger.ErrInvalid) {
			t.Fatal("forged snapshot accepted", err)
		}
	}
	if _, err := fundledger.Compare(b, m); !errors.Is(err, fundledger.ErrInvalid) {
		t.Fatal("reversed provider pair accepted", err)
	}
}
