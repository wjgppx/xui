package statements_test

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/statements"
)

var columns = []string{"payment_id", "transaction_id", "refund_id", "provider_refund_id", "amount_minor", "currency", "occurred_at"}

func encode(t *testing.T, rows ...[]string) string {
	t.Helper()
	var text strings.Builder
	writer := csv.NewWriter(&text)
	if err := writer.Write(columns); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	return text.String()
}

func paymentRow(id, transaction string) []string {
	return []string{id, transaction, "", "", "1900", "CNY", "2026-10-28T10:11:12+08:00"}
}

func refundRow(id, providerID string) []string {
	return []string{"original-payment", "original-transaction", id, providerID, "1900", "CNY", "2026-10-28T10:11:12+08:00"}
}

func input(t *testing.T) statements.Import {
	t.Helper()
	return statements.Import{Provider: "alipay", Kind: "payment", Date: "2026-10-28", Coverage: "partial", CSV: encode(t, paymentRow("merchant-A", "trade-A"))}
}

func invalid(t *testing.T, input statements.Import) *statements.ValidationError {
	t.Helper()
	doc, err := statements.Parse(input)
	if !errors.Is(err, statements.ErrInvalid) {
		t.Fatalf("expected typed validation failure, got document=%+v error=%v", doc, err)
	}
	var detail *statements.ValidationError
	if !errors.As(err, &detail) {
		t.Fatalf("missing sanitized validation detail: %v", err)
	}
	if !reflect.DeepEqual(doc, statements.Document{}) {
		t.Fatal("failed import returned a partial statement")
	}
	return detail
}

func TestParseStatementsSupportsSuccessfulGrossRecordsAndExternalReferences(t *testing.T) {
	for _, provider := range []string{"alipay", "wechat", "epay_alipay", "epay_wechat"} {
		t.Run(provider, func(t *testing.T) {
			in := input(t)
			in.Provider = provider
			in.CSV = encode(t, paymentRow("external-other-shop", "external-transaction"), paymentRow("merchant-A", "trade-A"))
			doc, err := statements.Parse(in)
			if err != nil || doc.Provider != provider || doc.Kind != "payment" || doc.Date != in.Date || doc.Coverage != "partial" || doc.Total != 3800 || len(doc.Rows) != 2 || len(doc.Digest) != 64 {
				t.Fatal("valid merchant observations rejected", doc, err)
			}
			if doc.Rows[0].PaymentID != "external-other-shop" || doc.Rows[0].Currency != "CNY" || doc.Rows[0].Amount != 1900 || doc.Rows[0].OccurredAt.Location() != time.UTC {
				t.Fatal("observation changed during normalization", doc.Rows)
			}
		})
	}
	for _, provider := range []string{"alipay", "wechat"} {
		in := input(t)
		in.Provider, in.Kind = provider, "refund"
		// Multiple refunds on the same original transaction are legitimate
		// statement observations, even though local product refunds are full-only.
		in.CSV = encode(t, refundRow("refund-B", "platform-B"), refundRow("refund-A", "platform-A"))
		doc, err := statements.Parse(in)
		if err != nil || len(doc.Rows) != 2 || doc.Rows[0].RefundID != "refund-A" || doc.Rows[0].ProviderRefundID != "platform-A" || doc.Total != 3800 {
			t.Fatal("distinct merchant refunds conflated", doc, err)
		}
	}
}

func TestStatementDigestDeduplicatesEquivalentFilesButIncludesScope(t *testing.T) {
	in := input(t)
	a, b := paymentRow("merchant-A", "trade-A"), paymentRow("merchant-B", "trade-B")
	in.CSV = encode(t, a, b)
	want, err := statements.Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	a[6], b[6] = "2026-10-28T02:11:12Z", "2026-10-28T02:11:12.000000000Z"
	in.CSV = "\ufeff" + strings.ReplaceAll(encode(t, b, a), "\n", "\r\n")
	got, err := statements.Parse(in)
	if err != nil || got.Digest != want.Digest || !reflect.DeepEqual(got.Rows, want.Rows) {
		t.Fatal("equivalent bytes or time offsets created another statement", got, err)
	}
	for _, field := range []string{"provider", "coverage", "date", "amount", "transaction", "kind"} {
		changed := in
		switch field {
		case "provider":
			changed.Provider = "wechat"
		case "coverage":
			changed.Coverage = "full"
		case "date":
			changed.Date = "2026-10-29"
			changed.CSV = strings.ReplaceAll(changed.CSV, "2026-10-28", "2026-10-29")
		case "amount":
			changed.CSV = strings.Replace(changed.CSV, "1900", "1901", 1)
		case "transaction":
			changed.CSV = strings.Replace(changed.CSV, "trade-A", "trade-C", 1)
		case "kind":
			changed.Kind, changed.CSV = "refund", encode(t, refundRow("refund-A", "platform-A"))
		}
		doc, e := statements.Parse(changed)
		if e != nil || doc.Digest == want.Digest {
			t.Fatal("different scope or financial data reused digest", field, e)
		}
	}
}

func TestStatementStoredTimesUseMicrosecondsAfterDayValidation(t *testing.T) {
	in := input(t)
	row := paymentRow("merchant-A", "trade-A")
	row[6] = "2026-10-28T10:11:12.123456001+08:00"
	in.CSV = encode(t, row)
	first, err := statements.Parse(in)
	if err != nil || first.Rows[0].OccurredAt.Nanosecond() != 123456000 {
		t.Fatal("persisted timestamp precision differs from digest input", first, err)
	}
	row[6] = "2026-10-28T02:11:12.123456999Z"
	in.CSV = encode(t, row)
	second, err := statements.Parse(in)
	if err != nil || first.Digest != second.Digest || !first.Rows[0].OccurredAt.Equal(second.Rows[0].OccurredAt) {
		t.Fatal("submicrosecond variations created different persisted identities", second, err)
	}
	row[6] = "2026-10-28T02:11:12.123457000Z"
	in.CSV = encode(t, row)
	third, err := statements.Parse(in)
	if err != nil || first.Digest == third.Digest || third.Rows[0].OccurredAt.Nanosecond() != 123457000 {
		t.Fatal("distinct persisted microseconds were conflated", third, err)
	}
	row[6] = "2026-10-27T15:59:59.999999999Z"
	in.CSV = encode(t, row)
	if err := invalid(t, in); err.Code != "outside_date" {
		t.Fatal("precision normalization admitted an out-of-day observation", err)
	}
	row[6] = "2026-10-28T16:00:00.000000001Z"
	in.CSV = encode(t, row)
	if err := invalid(t, in); err.Code != "outside_date" {
		t.Fatal("precision normalization admitted the next day", err)
	}
}

func TestStatementMetadataRejectsImplicitOrUnsupportedScope(t *testing.T) {
	for _, test := range []struct{ field, value string }{
		{"provider", ""}, {"provider", "ALIPAY"}, {"provider", "unknown-secret-merchant"},
		{"kind", ""}, {"kind", "paid"}, {"kind", "payment,refund"},
		{"coverage", ""}, {"coverage", "complete"}, {"coverage", "FULL"},
		{"date", "2026-2-28"}, {"date", "2026-02-29"}, {"date", "2026-10-28Z"}, {"date", "1999-12-31"}, {"date", "2100-01-01"},
	} {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			in := input(t)
			switch test.field {
			case "provider":
				in.Provider = test.value
			case "kind":
				in.Kind = test.value
			case "coverage":
				in.Coverage = test.value
			case "date":
				in.Date = test.value
			}
			err := invalid(t, in)
			if err.Line != 0 || err.Field != test.field || strings.Contains(err.Error(), "unknown-secret-merchant") {
				t.Fatal("invalid metadata not safely attributed", err)
			}
		})
	}
	for _, provider := range []string{"epay_alipay", "epay_wechat"} {
		in := input(t)
		in.Provider, in.Kind, in.CSV = provider, "refund", encode(t, refundRow("refund", "platform"))
		if err := invalid(t, in); err.Code != "unsupported_refund_provider" {
			t.Fatal("unsupported funds protocol not distinguished", err)
		}
	}
}

func TestStatementCSVRejectsMalformedHeadersBlankRecordsAndControls(t *testing.T) {
	valid := input(t).CSV
	for _, value := range []string{
		"", "\ufeff", "\ufeff\ufeff" + valid, " " + valid,
		strings.Replace(valid, "payment_id,transaction_id", "transaction_id,payment_id", 1),
		strings.Replace(valid, "amount_minor", "amount", 1),
		strings.Replace(valid, "occurred_at\n", "occurred_at,extra\n", 1),
		strings.Replace(valid, "trade-A,", "\"trade-A,", 1),
		"\n" + valid, valid + "\n", strings.Replace(valid, "\n", "\n\n", 1),
		strings.Replace(valid, "\n", "\r", 1),
		strings.Replace(valid, "merchant-A", "secret-private-key\x00", 1),
		strings.Replace(valid, "merchant-A", "secret-private-key\t", 1),
		strings.Replace(valid, "merchant-A", "secret-private-key\u0085", 1),
		valid + string([]byte{0xff}),
	} {
		in := input(t)
		in.CSV = value
		err := invalid(t, in)
		if strings.Contains(err.Error(), "secret-private-key") {
			t.Fatal("CSV validation exposed an uploaded value", err)
		}
	}
	for _, value := range []string{strings.TrimSuffix(valid, "\n"), strings.ReplaceAll(valid, "\n", "\r\n"), "\ufeff" + valid} {
		in := input(t)
		in.CSV = value
		if _, err := statements.Parse(in); err != nil {
			t.Fatal("valid CSV framing rejected", err)
		}
	}
}

func TestStatementMoneyIsPositiveExactCentsAndTotalCannotOverflow(t *testing.T) {
	for _, amount := range []string{"", "0", "00", "01900", "-1", "+1", "1.0", "1e3", " 1", "1 ", "１", "9223372036854775808", "99999999999999999999"} {
		in := input(t)
		row := paymentRow("merchant-A", "trade-A")
		row[4] = amount
		in.CSV = encode(t, row)
		if err := invalid(t, in); err.Field != "amount_minor" {
			t.Fatal("inexact amount not rejected", amount, err)
		}
	}
	in := input(t)
	row := paymentRow("merchant-A", "trade-A")
	row[4] = strconv.FormatInt(math.MaxInt64, 10)
	in.CSV = encode(t, row)
	doc, err := statements.Parse(in)
	if err != nil || doc.Total != math.MaxInt64 || doc.Rows[0].Amount != math.MaxInt64 {
		t.Fatal("exact int64 boundary lost", doc, err)
	}
	in.CSV = encode(t, row, paymentRow("merchant-B", "trade-B"))
	if err := invalid(t, in); err.Code != "amount_overflow" || err.Line != 3 {
		t.Fatal("overflowed total accepted", err)
	}
	row[4], row[5] = "1", "USD"
	in.CSV = encode(t, row)
	if err := invalid(t, in); err.Field != "currency" {
		t.Fatal("foreign currency silently converted", err)
	}
}

func TestStatementReferencesAreBoundedAndDuplicateFinancialIdentitiesReject(t *testing.T) {
	for _, ref := range []string{"", " leading", "trailing ", "=HYPERLINK", "+formula", "@formula", "-formula", "bad/slash", "nonascii中文", strings.Repeat("a", 129)} {
		in := input(t)
		in.CSV = encode(t, paymentRow(ref, "trade-A"))
		if err := invalid(t, in); err.Field != "payment_id" {
			t.Fatal("unsafe opaque reference accepted", err)
		}
	}
	in := input(t)
	in.CSV = encode(t, paymentRow(strings.Repeat("a", 128), "platform:2026-10_28.01"))
	if _, err := statements.Parse(in); err != nil {
		t.Fatal("bounded opaque reference rejected", err)
	}
	for _, same := range []string{"payment_id", "transaction_id"} {
		a, b := paymentRow("merchant-A", "trade-A"), paymentRow("merchant-B", "trade-B")
		if same == "payment_id" {
			b[0] = a[0]
		} else {
			b[1] = a[1]
		}
		in.CSV = encode(t, a, b)
		if err := invalid(t, in); err.Code != "duplicate_identity" || err.Field != same || err.Line != 3 {
			t.Fatal("duplicate financial identity accepted", same, err)
		}
	}
	in.Kind = "refund"
	for _, same := range []string{"refund_id", "provider_refund_id"} {
		a, b := refundRow("refund-A", "platform-A"), refundRow("refund-B", "platform-B")
		if same == "refund_id" {
			b[2] = a[2]
		} else {
			b[3] = a[3]
		}
		in.CSV = encode(t, a, b)
		if err := invalid(t, in); err.Code != "duplicate_identity" || err.Field != same {
			t.Fatal("duplicate refund identity accepted", same, err)
		}
	}
}

func TestStatementKindRequiresCorrectRefundBindings(t *testing.T) {
	in := input(t)
	row := paymentRow("merchant-A", "trade-A")
	row[2] = "unexpected-refund"
	in.CSV = encode(t, row)
	if err := invalid(t, in); err.Code != "unexpected_refund_reference" {
		t.Fatal("payment row smuggled refund metadata", err)
	}
	in.Kind = "refund"
	for _, index := range []int{0, 1, 2, 3} {
		row = refundRow("refund-A", "platform-A")
		row[index] = ""
		in.CSV = encode(t, row)
		if err := invalid(t, in); err.Field != columns[index] {
			t.Fatal("incomplete refund binding accepted", err)
		}
	}
}

func TestStatementTimesUseShanghaiHalfOpenDayAndExplicitOffsets(t *testing.T) {
	for _, instant := range []string{
		"2026-10-28T00:00:00+08:00",
		"2026-10-27T16:00:00Z",
		"2026-10-27T11:00:00-05:00",
		"2026-10-28T23:59:59.999999999+08:00",
	} {
		in := input(t)
		row := paymentRow("merchant-A", "trade-A")
		row[6] = instant
		in.CSV = encode(t, row)
		if _, err := statements.Parse(in); err != nil {
			t.Fatal("time within Shanghai day rejected", instant, err)
		}
	}
	for _, instant := range []string{
		"2026-10-27T15:59:59.999999999Z", "2026-10-28T16:00:00Z", "2026-10-29T00:00:00+08:00",
		"2026-10-28 10:11:12", "2026-10-28T10:11:12", "2026-10-28T10:11:12+0800",
		"2026-10-28T10:11:12+24:00", "2026-10-28T10:11:12+08:60", "2026-10-28T24:00:00+08:00",
		"2026-10-28T10:11:12.1234567890+08:00", "2026-10-28T10:11:12,1+08:00",
	} {
		in := input(t)
		row := paymentRow("merchant-A", "trade-A")
		row[6] = instant
		in.CSV = encode(t, row)
		if err := invalid(t, in); err.Field != "occurred_at" {
			t.Fatal("ambiguous or out-of-period time accepted", instant, err)
		}
	}
	for _, day := range []string{"2000-02-29", "2099-12-31"} {
		in := input(t)
		in.Date = day
		row := paymentRow("merchant-A", "trade-A")
		row[6] = day + "T12:00:00+08:00"
		in.CSV = encode(t, row)
		if _, err := statements.Parse(in); err != nil {
			t.Fatal("supported date boundary rejected", day, err)
		}
	}
}

func TestStatementLimitsAreEnforcedBeforeReturningAnyRows(t *testing.T) {
	in := input(t)
	rows := make([][]string, statements.MaxRows)
	for index := range rows {
		rows[index] = paymentRow(fmt.Sprintf("merchant-%d", index), fmt.Sprintf("trade-%d", index))
	}
	in.CSV = encode(t, rows...)
	doc, err := statements.Parse(in)
	if err != nil || len(doc.Rows) != statements.MaxRows || doc.Total != int64(statements.MaxRows)*1900 {
		t.Fatal("documented row boundary rejected", err)
	}
	rows = append(rows, paymentRow("one-more", "one-more-trade"))
	in.CSV = encode(t, rows...)
	if err := invalid(t, in); err.Code != "too_many_rows" {
		t.Fatal("unbounded row import accepted", err)
	}
	in.CSV = strings.Repeat("a", statements.MaxBytes+1)
	if err := invalid(t, in); err.Code != "too_large" {
		t.Fatal("oversized statement not rejected before parsing", err)
	}
}

func TestEmptyStatementsHaveExplicitCoverageAndInvalidLaterRowsAreAtomic(t *testing.T) {
	for _, coverage := range []string{"partial", "full"} {
		in := input(t)
		in.Coverage, in.CSV = coverage, encode(t)
		doc, err := statements.Parse(in)
		if err != nil || doc.Rows == nil || len(doc.Rows) != 0 || doc.Total != 0 || doc.Coverage != coverage || doc.Digest == "" {
			t.Fatal("explicit empty day observation lost", doc, err)
		}
	}
	in := input(t)
	bad := paymentRow("merchant-secret-value", "trade-secret-value")
	bad[6] = "untrusted-sensitive-content"
	in.CSV = encode(t, paymentRow("merchant-A", "trade-A"), bad)
	err := invalid(t, in)
	if err.Line != 3 || err.Field != "occurred_at" || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), bad[6]) {
		t.Fatal("later invalid row leaked or lost safe source location", err)
	}
}
