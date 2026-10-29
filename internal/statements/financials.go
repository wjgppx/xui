package statements

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const FinancialVersion = "native-financials-v1"

// Amounts retain five decimal places in yuan. The field meaning is defined by
// Basis; none of these trade-bill observations proves bank settlement.
type FinancialRow struct {
	PaymentID         string  `json:"payment_id"`
	RefundID          string  `json:"refund_id"`
	FeeYuan           *string `json:"fee_yuan"`
	ChannelAmountYuan *string `json:"channel_amount_yuan"`
}
type FinancialDocument struct {
	Version                        string         `json:"version"`
	Basis                          string         `json:"basis"`
	Rows                           []FinancialRow `json:"rows"`
	FeeTotalYuan                   *string        `json:"fee_total_yuan"`
	ChannelAmountTotalYuan         *string        `json:"channel_amount_total_yuan"`
	DeclaredFeeTotalYuan           *string        `json:"declared_fee_total_yuan"`
	DeclaredChannelAmountTotalYuan *string        `json:"declared_channel_amount_total_yuan"`
	SummaryCodes                   []string       `json:"summary_codes"`
}
type FinancialReference struct {
	Rows                   []FinancialRow `json:"rows"`
	Digest                 string         `json:"digest"`
	FeeTotalYuan           string         `json:"fee_total_yuan"`
	ChannelAmountTotalYuan string         `json:"channel_amount_total_yuan"`
}

var financialYuanPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]{0,24})(\.[0-9]{1,5})?$`)

// CanonicalYuan never rounds, converts through floating point or scales a
// machine integer. Values allow 25 integer digits and five fractional digits.
func CanonicalYuan(value string) (string, error) {
	units, err := financialUnits(value, false)
	if err != nil {
		return "", err
	}
	return financialFormat(units), nil
}
func financialUnits(value string, total bool) (*big.Int, error) {
	if !financialYuanPattern.MatchString(value) {
		if !total {
			return nil, invalid(0, "fee_yuan", "financial_amount_invalid")
		}
		// Totals can have up to 29 integer digits after summing 2000 rows.
		unsigned := strings.TrimPrefix(value, "-")
		parts := strings.Split(unsigned, ".")
		if len(parts) != 2 || len(parts[1]) != 5 || len(parts[0]) > 29 || len(parts[0]) == 0 || (len(parts[0]) > 1 && parts[0][0] == '0') || strings.Trim(parts[0]+parts[1], "0123456789") != "" {
			return nil, invalid(0, "fee_yuan", "financial_amount_invalid")
		}
	}
	negative := strings.HasPrefix(value, "-")
	parts := strings.Split(strings.TrimPrefix(value, "-"), ".")
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	units, ok := new(big.Int).SetString(parts[0]+fraction+strings.Repeat("0", 5-len(fraction)), 10)
	if !ok {
		return nil, invalid(0, "fee_yuan", "financial_amount_invalid")
	}
	if negative {
		units.Neg(units)
	}
	return units, nil
}
func financialFormat(units *big.Int) string {
	negative := units.Sign() < 0
	abs := new(big.Int).Abs(units).String()
	if len(abs) <= 5 {
		abs = strings.Repeat("0", 6-len(abs)) + abs
	}
	value := abs[:len(abs)-5] + "." + abs[len(abs)-5:]
	if negative {
		value = "-" + value
	}
	return value
}

// SumYuan permits up to 2000 signed row values and bounds the final sum to 34
// scaled digits. Cancellation is exact and independent of row ordering.
func SumYuan(values []string) (string, error) {
	if len(values) > MaxRows {
		return "", invalid(0, "csv", "too_many_rows")
	}
	total := new(big.Int)
	for _, value := range values {
		units, err := financialUnits(value, false)
		if err != nil {
			return "", err
		}
		total.Add(total, units)
	}
	if len(new(big.Int).Abs(total).String()) > 34 {
		return "", invalid(0, "fee_yuan", "financial_amount_overflow")
	}
	return financialFormat(total), nil
}
func financialIdentity(kind string, row FinancialRow) string {
	if kind == "refund" {
		return row.RefundID
	}
	return row.PaymentID
}
func sortFinancialRows(kind string, rows []FinancialRow) {
	sort.Slice(rows, func(i, j int) bool { return financialIdentity(kind, rows[i]) < financialIdentity(kind, rows[j]) })
}
func financialTotals(rows []FinancialRow) (*string, *string, error) {
	fees, amounts := make([]string, 0, len(rows)), make([]string, 0, len(rows))
	allFees, allAmounts := true, true
	for _, row := range rows {
		if row.FeeYuan == nil {
			allFees = false
		} else {
			fees = append(fees, *row.FeeYuan)
		}
		if row.ChannelAmountYuan == nil {
			allAmounts = false
		} else {
			amounts = append(amounts, *row.ChannelAmountYuan)
		}
	}
	var feeTotal, amountTotal *string
	if allFees {
		value, err := SumYuan(fees)
		if err != nil {
			return nil, nil, err
		}
		feeTotal = &value
	}
	if allAmounts {
		value, err := SumYuan(amounts)
		if err != nil {
			return nil, nil, err
		}
		amountTotal = &value
	}
	return feeTotal, amountTotal, nil
}
func financialSummaryCodes(d FinancialDocument) []string {
	codes := []string{}
	if d.FeeTotalYuan != nil && d.DeclaredFeeTotalYuan != nil && *d.FeeTotalYuan != *d.DeclaredFeeTotalYuan {
		codes = append(codes, "fee_total_mismatch")
	}
	if d.ChannelAmountTotalYuan != nil && d.DeclaredChannelAmountTotalYuan != nil && *d.ChannelAmountTotalYuan != *d.DeclaredChannelAmountTotalYuan {
		codes = append(codes, "channel_amount_total_mismatch")
	}
	return codes
}

// ValidateFinancialDocument verifies the source snapshot against its normalized
// identities and recomputes every derived field; nil amounts remain unknown.
func ValidateFinancialDocument(document Document, financial FinancialDocument) error {
	wantBasis := "alipay_merchant_received"
	if document.Provider == "wechat" {
		wantBasis = "wechat_settlement_order"
		if document.Kind == "refund" {
			wantBasis = "wechat_refund"
		}
	} else if document.Provider != "alipay" {
		return invalid(0, "format", "financial_document_invalid")
	}
	if (document.Kind != "payment" && document.Kind != "refund") || financial.Version != FinancialVersion || financial.Basis != wantBasis || len(financial.Rows) != len(document.Rows) || len(financial.Rows) > MaxRows || financial.Rows == nil {
		return invalid(0, "format", "financial_document_invalid")
	}
	expected := make(map[string]string, len(document.Rows))
	for _, row := range document.Rows {
		id := row.PaymentID
		if document.Kind == "refund" {
			id = row.RefundID
		}
		expected[id] = row.PaymentID
	}
	previous := ""
	for _, row := range financial.Rows {
		id := financialIdentity(document.Kind, row)
		payment, exists := expected[id]
		if !exists || payment != row.PaymentID || (document.Kind == "payment" && row.RefundID != "") || (previous != "" && id <= previous) {
			return invalid(0, "format", "financial_document_invalid")
		}
		previous = id
		for _, value := range []*string{row.FeeYuan, row.ChannelAmountYuan} {
			if value != nil {
				canonical, err := CanonicalYuan(*value)
				if err != nil || canonical != *value {
					return invalid(0, "format", "financial_document_invalid")
				}
			}
		}
		if document.Provider == "wechat" && (row.FeeYuan == nil || row.ChannelAmountYuan == nil || strings.HasPrefix(*row.ChannelAmountYuan, "-")) {
			return invalid(0, "format", "financial_document_invalid")
		}
	}
	fee, amount, err := financialTotals(financial.Rows)
	if err != nil || !reflect.DeepEqual(fee, financial.FeeTotalYuan) || !reflect.DeepEqual(amount, financial.ChannelAmountTotalYuan) {
		return invalid(0, "format", "financial_document_invalid")
	}
	if document.Provider == "wechat" {
		if financial.DeclaredFeeTotalYuan == nil || financial.DeclaredChannelAmountTotalYuan == nil || strings.HasPrefix(*financial.DeclaredChannelAmountTotalYuan, "-") {
			return invalid(0, "format", "financial_document_invalid")
		}
	} else if financial.DeclaredFeeTotalYuan != nil || financial.DeclaredChannelAmountTotalYuan != nil {
		return invalid(0, "format", "financial_document_invalid")
	}
	for _, value := range []*string{financial.DeclaredFeeTotalYuan, financial.DeclaredChannelAmountTotalYuan} {
		if value != nil {
			units, e := financialUnits(*value, true)
			if e != nil || financialFormat(units) != *value {
				return invalid(0, "format", "financial_document_invalid")
			}
		}
	}
	if !reflect.DeepEqual(financial.SummaryCodes, financialSummaryCodes(financial)) {
		return invalid(0, "format", "financial_document_invalid")
	}
	return nil
}

func extractNativeFinancials(document Document, format string, data []byte) (*FinancialDocument, error) {
	financial := &FinancialDocument{Version: FinancialVersion, Rows: []FinancialRow{}, SummaryCodes: []string{}}
	var text string
	var err error
	if document.Provider == "wechat" {
		financial.Basis = "wechat_settlement_order"
		if document.Kind == "refund" {
			financial.Basis = "wechat_refund"
		}
		data, err = nativeGzip(data)
		if err == nil {
			text, err = nativeText(data, false)
		}
	} else {
		financial.Basis = "alipay_merchant_received"
		data, err = alipayArchive(data)
		if err == nil {
			text, err = nativeText(data, true)
		}
	}
	if err != nil {
		return nil, err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	if document.Provider == "alipay" {
		reader.Comment = '#'
	}
	if _, err = reader.Read(); err != nil {
		return nil, invalid(1, "format", "financial_document_invalid")
	}
	for {
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, invalid(0, "format", "financial_document_invalid")
		}
		line, _ := reader.FieldPos(0)
		if document.Provider == "wechat" && nativeHeader(values, wechatSummaryHeader) {
			summary, e := reader.Read()
			if e != nil {
				return nil, invalid(line, "format", "financial_document_invalid")
			}
			summary, e = nativeFields(summary, len(wechatSummaryHeader), line+1, true)
			if e != nil {
				return nil, e
			}
			fee, e := CanonicalYuan(summary[4])
			if e != nil {
				return nil, invalid(line+1, "fee_yuan", "financial_amount_invalid")
			}
			amountIndex := 1
			if document.Kind == "refund" {
				amountIndex = 2
			}
			amount, e := CanonicalYuan(summary[amountIndex])
			if e != nil {
				return nil, invalid(line+1, "channel_amount_yuan", "financial_amount_invalid")
			}
			financial.DeclaredFeeTotalYuan, financial.DeclaredChannelAmountTotalYuan = &fee, &amount
			break
		}
		var row FinancialRow
		feeIndex, amountIndex := 22, 12
		if document.Provider == "wechat" {
			count := len(wechatSuccessHeader)
			if document.Kind == "refund" {
				count = len(wechatRefundHeader)
			}
			values, err = nativeFields(values, count, line, true)
			if err != nil {
				return nil, err
			}
			row.PaymentID, feeIndex, amountIndex = values[6], 16, 12
			if document.Kind == "refund" {
				row.RefundID, feeIndex, amountIndex = values[17], 24, 18
			}
		} else {
			values, err = nativeFields(values, len(alipayTradeHeader), line, false)
			if err != nil {
				return nil, err
			}
			kind := "payment"
			if values[2] == "退款" {
				kind = "refund"
			}
			if kind != document.Kind {
				continue
			}
			row.PaymentID = values[1]
			if kind == "refund" {
				row.RefundID = values[21]
			}
		}
		if values[feeIndex] != "" {
			fee, e := CanonicalYuan(values[feeIndex])
			if e != nil {
				return nil, invalid(line, "fee_yuan", "financial_amount_invalid")
			}
			row.FeeYuan = &fee
		}
		if values[amountIndex] != "" {
			amount, e := CanonicalYuan(values[amountIndex])
			if e != nil {
				return nil, invalid(line, "channel_amount_yuan", "financial_amount_invalid")
			}
			row.ChannelAmountYuan = &amount
		}
		financial.Rows = append(financial.Rows, row)
	}
	sortFinancialRows(document.Kind, financial.Rows)
	financial.FeeTotalYuan, financial.ChannelAmountTotalYuan, err = financialTotals(financial.Rows)
	if err != nil {
		return nil, err
	}
	financial.SummaryCodes = financialSummaryCodes(*financial)
	if err = ValidateFinancialDocument(document, *financial); err != nil {
		return nil, err
	}
	return financial, nil
}

var financialReferenceHeader = []string{"payment_id", "refund_id", "expected_fee_yuan", "expected_channel_amount_yuan"}

func ParseFinancialReference(content, kind string) (FinancialReference, error) {
	result := FinancialReference{Rows: []FinancialRow{}}
	if kind != "payment" && kind != "refund" {
		return result, invalid(0, "kind", "invalid_field")
	}
	if len(content) > MaxBytes {
		return result, invalid(0, "csv", "too_large")
	}
	if !utf8.ValidString(content) {
		return result, invalid(0, "csv", "invalid_utf8")
	}
	content = strings.TrimPrefix(content, "\ufeff")
	if err := validatePhysicalLines(content); err != nil {
		return result, err
	}
	reader := csv.NewReader(strings.NewReader(content))
	reader.FieldsPerRecord = len(financialReferenceHeader)
	columns, err := reader.Read()
	if err != nil || !nativeHeader(columns, financialReferenceHeader) {
		return result, invalid(1, "header", "financial_reference_header")
	}
	seen := map[string]bool{}
	for {
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return FinancialReference{}, invalid(0, "csv", "financial_reference_record")
		}
		line, _ := reader.FieldPos(0)
		if len(result.Rows) >= MaxRows {
			return FinancialReference{}, invalid(line, "csv", "too_many_rows")
		}
		if !referencePattern.MatchString(values[0]) || (kind == "payment" && values[1] != "") || (kind == "refund" && !referencePattern.MatchString(values[1])) {
			return FinancialReference{}, invalid(line, "payment_id", "financial_reference_record")
		}
		row := FinancialRow{PaymentID: values[0], RefundID: values[1]}
		id := financialIdentity(kind, row)
		if seen[id] {
			return FinancialReference{}, invalid(line, "payment_id", "duplicate_identity")
		}
		seen[id] = true
		fee, e := CanonicalYuan(values[2])
		if e != nil {
			return FinancialReference{}, invalid(line, "fee_yuan", "financial_amount_invalid")
		}
		amount, e := CanonicalYuan(values[3])
		if e != nil {
			return FinancialReference{}, invalid(line, "channel_amount_yuan", "financial_amount_invalid")
		}
		row.FeeYuan, row.ChannelAmountYuan = &fee, &amount
		result.Rows = append(result.Rows, row)
	}
	sortFinancialRows(kind, result.Rows)
	fee, amount, err := financialTotals(result.Rows)
	if err != nil {
		return FinancialReference{}, err
	}
	result.FeeTotalYuan, result.ChannelAmountTotalYuan = *fee, *amount
	canonical, err := json.Marshal(struct {
		Version, Kind string
		Rows          []FinancialRow
	}{"financial-reference-v1", kind, result.Rows})
	if err != nil {
		return FinancialReference{}, invalid(0, "csv", "financial_reference_record")
	}
	digest := sha256.Sum256(canonical)
	result.Digest = hex.EncodeToString(digest[:])
	return result, nil
}
