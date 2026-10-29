package statements_test

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/statements"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const referenceColumns = "payment_id,refund_id,expected_fee_yuan,expected_channel_amount_yuan\n"

func TestFinancialYuanPreservesFiveDecimalsWithoutMachineIntegerOverflow(t *testing.T) {
	cases := map[string]string{
		"0": "0.00000", "-0.00000": "0.00000", "1.2": "1.20000", "-0.00001": "-0.00001",
		"92233720368547758.07": "92233720368547758.07000", "9999999999999999999999999.99999": "9999999999999999999999999.99999",
	}
	for value, want := range cases {
		got, err := statements.CanonicalYuan(value)
		if err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"", " 0", "1 ", "+1", "01", "1.", ".1", "1.000001", "1e3", "NaN", "10000000000000000000000000", "--1", "-", "１"} {
		if _, err := statements.CanonicalYuan(value); !errors.Is(err, statements.ErrInvalid) {
			t.Fatal("nonexact value accepted", value, err)
		}
	}
	values := make([]string, statements.MaxRows)
	for i := range values {
		values[i] = "9999999999999999999999999.99999"
	}
	total, err := statements.SumYuan(values)
	if err != nil || total != "19999999999999999999999999999.98000" {
		t.Fatal(total, err)
	}
	if got, e := statements.SumYuan([]string{"9999999999999999999999999.99999", "-9999999999999999999999999.99999", "0.00001"}); e != nil || got != "0.00001" {
		t.Fatal(got, e)
	}
	if _, err = statements.SumYuan(append(values, "0")); !errors.Is(err, statements.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestFinancialWechatPreservesCouponAdjustedBaseAndOriginalRefundFeeSign(t *testing.T) {
	for _, refund := range []bool{false, true} {
		row, totals := wxNativeRow(refund), wxNativeTotals(refund)
		wantFee, wantBase, wantBasis := "0.01000", "18.01000", "wechat_settlement_order"
		if refund {
			row[18], totals[2], row[24], totals[4] = "17.01", "17.01", "-0.00001", "-0.00001"
			wantFee, wantBase, wantBasis = "-0.00001", "17.01000", "wechat_refund"
		}
		result, err := convertWx(t, refund, wxNativeCSV(t, refund, [][]string{row}, totals))
		if err != nil {
			t.Fatal(err)
		}
		f := result.Financials
		if f == nil || f.Version != statements.FinancialVersion || f.Basis != wantBasis || len(f.Rows) != 1 || *f.Rows[0].FeeYuan != wantFee || *f.Rows[0].ChannelAmountYuan != wantBase || *f.DeclaredFeeTotalYuan != wantFee || *f.DeclaredChannelAmountTotalYuan != wantBase || len(f.SummaryCodes) != 0 {
			t.Fatal(f)
		}
		doc, err := statements.Parse(result.Input)
		if err != nil || doc.Rows[0].Amount != 1901 {
			t.Fatal("economic base replaced gross", err)
		}
		if err = statements.ValidateFinancialDocument(doc, *f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFinancialSummaryDifferencesDoNotInventGrossOrBankSettlementFailures(t *testing.T) {
	totals := wxNativeTotals(false)
	totals[1], totals[4] = "18.00", "0.02000"
	result, err := convertWx(t, false, wxNativeCSV(t, false, [][]string{wxNativeRow(false)}, totals))
	if err != nil {
		t.Fatal("gross-consistent bill rejected instead of recording economic differences", err)
	}
	f := result.Financials
	if !reflect.DeepEqual(f.SummaryCodes, []string{"fee_total_mismatch", "channel_amount_total_mismatch"}) || *f.FeeTotalYuan != "0.01000" || *f.DeclaredFeeTotalYuan != "0.02000" || *f.ChannelAmountTotalYuan != "18.01000" {
		t.Fatal(f)
	}
}

func TestFinancialWechatZeroBillRetainsKnownZeroTotals(t *testing.T) {
	result, err := convertWx(t, false, wxNativeCSV(t, false, nil, []string{"0", "0", "0", "0", "0", "0", "0"}))
	if err != nil || result.Financials == nil || result.Financials.Rows == nil || len(result.Financials.Rows) != 0 || *result.Financials.FeeTotalYuan != "0.00000" || *result.Financials.ChannelAmountTotalYuan != "0.00000" {
		t.Fatal(result.Financials, err)
	}
}

func alipayFinancialFixture(t *testing.T, fee, amount string, refund bool) []byte {
	t.Helper()
	reader := csv.NewReader(strings.NewReader(string(alipayNativeCSV(t, refund))))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	rows[1][22], rows[1][12] = fee, amount
	return nativeCSV(t, rows...)
}

func TestFinancialAlipayKeepsUnknownEmptyValuesAndLiteralSignedAmounts(t *testing.T) {
	for _, mode := range []string{"unknown", "zero", "signed"} {
		fee, amount := "", ""
		if mode == "zero" {
			fee, amount = "0", "0.00000"
		}
		if mode == "signed" {
			fee, amount = "-0.00001", "-18.01"
		}
		raw := alipayFinancialFixture(t, fee, amount, true)
		result, err := statements.ConvertNative("alipay", "refund", "2026-10-27", "partial", "alipay_trade_v1", raw, "")
		if err != nil {
			t.Fatal(err)
		}
		f := result.Financials
		if f == nil || f.Basis != "alipay_merchant_received" || f.DeclaredFeeTotalYuan != nil || f.DeclaredChannelAmountTotalYuan != nil || f.Rows[0].RefundID != "ali-refund-A" {
			t.Fatal(f)
		}
		if mode == "unknown" {
			if f.Rows[0].FeeYuan != nil || f.Rows[0].ChannelAmountYuan != nil || f.FeeTotalYuan != nil || f.ChannelAmountTotalYuan != nil {
				t.Fatal("unknown was converted to zero", f)
			}
		} else {
			wantFee, _ := statements.CanonicalYuan(fee)
			wantAmount, _ := statements.CanonicalYuan(amount)
			if *f.Rows[0].FeeYuan != wantFee || *f.Rows[0].ChannelAmountYuan != wantAmount {
				t.Fatal(f)
			}
		}
	}
}

func TestFinancialNativeChangesFeeWithoutChangingGrossStatementDigest(t *testing.T) {
	a, err := statements.ConvertNative("alipay", "payment", "2026-10-27", "partial", "alipay_trade_v1", alipayFinancialFixture(t, "0.1", "18.01", false), "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := statements.ConvertNative("alipay", "payment", "2026-10-27", "partial", "alipay_trade_v1", alipayFinancialFixture(t, "0.2", "18.01", false), "")
	if err != nil {
		t.Fatal(err)
	}
	ad, _ := statements.Parse(a.Input)
	bd, _ := statements.Parse(b.Input)
	if ad.Digest != bd.Digest || a.SourceDigest == b.SourceDigest || *a.Financials.FeeTotalYuan == *b.Financials.FeeTotalYuan {
		t.Fatal("source economics changed canonical gross identity")
	}
}

func TestFinancialDocumentRejectsForgedDerivedFieldsOrIdentityMapping(t *testing.T) {
	result, err := convertWx(t, false, wxNativeCSV(t, false, [][]string{wxNativeRow(false)}, wxNativeTotals(false)))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := statements.Parse(result.Input)
	for _, mutation := range []string{"version", "basis", "identity", "refund", "row_missing", "fee_nil", "row_noncanonical", "total", "summary", "declared_nil", "declared_noncanonical", "negative_channel", "negative_declared_channel"} {
		var f statements.FinancialDocument
		data, _ := json.Marshal(result.Financials)
		_ = json.Unmarshal(data, &f)
		switch mutation {
		case "version":
			f.Version = "other"
		case "basis":
			f.Basis = "alipay_merchant_received"
		case "identity":
			f.Rows[0].PaymentID = "other-payment"
		case "refund":
			f.Rows[0].RefundID = "not-a-refund"
		case "row_missing":
			f.Rows = nil
		case "fee_nil":
			f.Rows[0].FeeYuan = nil
		case "row_noncanonical":
			value := "0.01"
			f.Rows[0].FeeYuan = &value
		case "total":
			value := "0.02000"
			f.FeeTotalYuan = &value
		case "summary":
			f.SummaryCodes = []string{"fee_total_mismatch"}
		case "declared_nil":
			f.DeclaredFeeTotalYuan = nil
		case "declared_noncanonical":
			value := "0.01"
			f.DeclaredFeeTotalYuan = &value
		case "negative_channel":
			value := "-18.01000"
			f.Rows[0].ChannelAmountYuan, f.ChannelAmountTotalYuan, f.DeclaredChannelAmountTotalYuan = &value, &value, &value
		case "negative_declared_channel":
			value := "-18.01000"
			f.DeclaredChannelAmountTotalYuan = &value
			f.SummaryCodes = []string{"channel_amount_total_mismatch"}
		}
		if err := statements.ValidateFinancialDocument(doc, f); !errors.Is(err, statements.ErrInvalid) {
			t.Fatal("forged snapshot accepted", mutation, err)
		}
	}
}

func TestFinancialExtractionRetainsArchiveEncodingAndSourceBudgetGuards(t *testing.T) {
	raw := alipayFinancialFixture(t, "0.00001", "18.01", false)
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{gbk, nativeZIP(t, zipEntry{name: "业务明细.csv", data: gbk})} {
		result, e := statements.ConvertNative("alipay", "payment", "2026-10-27", "partial", "alipay_trade_v1", data, "")
		if e != nil || *result.Financials.FeeTotalYuan != "0.00001" {
			t.Fatal(result.Financials, e)
		}
	}
	wx := wxNativeCSV(t, false, [][]string{wxNativeRow(false)}, wxNativeTotals(false))
	result, err := convertWx(t, false, nativeGZ(t, wx))
	if err != nil || *result.Financials.FeeTotalYuan != "0.01000" {
		t.Fatal(result.Financials, err)
	}
}

func TestFinancialReferenceCanonicalDigestSortsRowsAndPreservesExactSigns(t *testing.T) {
	a, err := statements.ParseFinancialReference(referenceColumns+"payment-B,,0,92233720368547758.07\npayment-A,,-0.00001,18.01\n", "payment")
	if err != nil {
		t.Fatal(err)
	}
	b, err := statements.ParseFinancialReference("\ufeff"+strings.ReplaceAll(referenceColumns, "\n", "\r\n")+"\"payment-A\",,-0.00001,18.01000\r\npayment-B,,-0.00000,92233720368547758.07000\r\n", "payment")
	if err != nil || a.Digest != b.Digest || len(a.Rows) != 2 || a.Rows[0].PaymentID != "payment-A" || a.FeeTotalYuan != "-0.00001" || a.ChannelAmountTotalYuan != "92233720368547776.08000" {
		t.Fatal(a, b, err)
	}
	empty, err := statements.ParseFinancialReference(referenceColumns, "refund")
	if err != nil || empty.Rows == nil || len(empty.Rows) != 0 || empty.FeeTotalYuan != "0.00000" {
		t.Fatal(empty, err)
	}
	r, err := statements.ParseFinancialReference(referenceColumns+"payment-A,refund-A,-0.01,18.01\n", "refund")
	if err != nil || r.Rows[0].RefundID != "refund-A" {
		t.Fatal(r, err)
	}
}

func TestFinancialReferenceRejectsAmbiguousMissingDuplicateOrOversizedRecords(t *testing.T) {
	for _, content := range []string{
		"payment_id,refund_id,fee_yuan,channel_amount_yuan\npayment-A,,0,1\n",
		referenceColumns + "payment-A,,,1\n", referenceColumns + "payment-A,,0,\n", referenceColumns + "payment-A,,0.000001,1\n",
		referenceColumns + "payment-A,,0,1\npayment-A,,1,1\n", referenceColumns + "payment-A,refund-A,0,1\n",
		referenceColumns + "private-uploaded-reference/invalid,,0,1\n", referenceColumns + "payment-A,,0,1\n\n",
		referenceColumns + "payment-A,, 0,1\n", referenceColumns + "payment-A,,0,1\nextra", string([]byte{0xff}), strings.Repeat("a", statements.MaxBytes+1),
	} {
		_, err := statements.ParseFinancialReference(content, "payment")
		if !errors.Is(err, statements.ErrInvalid) || strings.Contains(err.Error(), "private-uploaded-reference") {
			t.Fatal("invalid reference accepted or echoed", err)
		}
	}
	if _, err := statements.ParseFinancialReference(referenceColumns+"payment-A,,0,1\n", "refund"); !errors.Is(err, statements.ErrInvalid) {
		t.Fatal("refund missing refundID", err)
	}
	if _, err := statements.ParseFinancialReference(referenceColumns, "unsupported"); !errors.Is(err, statements.ErrInvalid) {
		t.Fatal(err)
	}
	var rows strings.Builder
	rows.WriteString(referenceColumns)
	for i := 0; i <= statements.MaxRows; i++ {
		fmt.Fprintf(&rows, "payment-%d,,0,1\n", i)
	}
	if _, err := statements.ParseFinancialReference(rows.String(), "payment"); !errors.Is(err, statements.ErrInvalid) {
		t.Fatal("row limit ignored", err)
	}
}
