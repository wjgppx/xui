package statements

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	MaxNativeBytes         = 8 << 20
	MaxNativeExpandedBytes = 8 << 20
)

type NativeImport struct {
	Provider      string `json:"provider"`
	Kind          string `json:"kind"`
	Date          string `json:"date"`
	Coverage      string `json:"coverage"`
	Format        string `json:"format"`
	ContentBase64 string `json:"content_base64"`
}

type NativeResult struct {
	Input        Import
	SourceDigest string
	Format       string
	MerchantID   string
	Financials   *FinancialDocument
}

var wechatSuccessHeader = strings.Split("交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,商品名称,商户数据包,手续费,费率,订单金额,费率备注", ",")
var wechatRefundHeader = strings.Split("交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,退款申请时间,退款成功时间,微信退款单号,商户退款单号,退款金额,充值券退款金额,退款类型,退款状态,商品名称,商户数据包,手续费,费率,订单金额,申请退款金额,费率备注", ",")
var wechatSummaryHeader = strings.Split("总交易单数,应结订单总金额,退款总金额,充值券退款总金额,手续费总金额,订单总金额,申请退款总金额", ",")
var alipayTradeHeader = strings.Split("支付宝交易号,商户订单号,业务类型,商品名称,创建时间,完成时间,门店编号,门店名称,操作员,终端号,对方账户,订单金额（元）,商家实收（元）,支付宝红包（元）,集分宝（元）,支付宝优惠（元）,商家优惠（元）,券核销金额（元）,券名称,商家红包消费金额（元）,卡消费金额（元）,退款批次号/请求号,服务费（元）,分润（元）,备注", ",")

var nativeMoneyPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,2})?$`)
var nativeAuxMoneyPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})(\.[0-9]{1,5})?$`)
var nativeCountPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,4})$`)

// ParseNative accepts bytes as strict base64 so binary ZIP/GBK data never passes
// through JSON's UTF-8 replacement behavior. Native files remain observations;
// successful normalization cannot settle or refund a payment.
func ParseNative(input NativeImport) (NativeResult, error) {
	if len(input.ContentBase64) > base64.StdEncoding.EncodedLen(MaxNativeBytes) {
		return NativeResult{}, invalid(0, "content_base64", "native_too_large")
	}
	if strings.IndexAny(input.ContentBase64, "\r\n\t ") >= 0 {
		return NativeResult{}, invalid(0, "content_base64", "invalid_native_record")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(input.ContentBase64)
	if err != nil {
		return NativeResult{}, invalid(0, "content_base64", "invalid_native_record")
	}
	return ConvertNative(input.Provider, input.Kind, input.Date, input.Coverage, input.Format, data, "")
}

// ConvertNative is shared by upload and server-download callers. The optional
// expected merchant comes from server configuration, never from file contents.
// WeChat daily bills may include more than one AppID; all direct-merchant rows
// are retained rather than silently filtering the file to one application.
func ConvertNative(provider, kind, date, coverage, format string, data []byte, expectedMerchantID string) (NativeResult, error) {
	input := Import{Provider: provider, Kind: kind, Date: date, Coverage: coverage, CSV: strings.Join(header, ",") + "\n"}
	if _, err := Parse(input); err != nil {
		return NativeResult{}, err
	}
	if len(data) > MaxNativeBytes {
		return NativeResult{}, invalid(0, "content_base64", "native_too_large")
	}
	if len(data) == 0 {
		return NativeResult{}, invalid(0, "content_base64", "native_empty")
	}
	var rows [][]string
	var merchant string
	var err error
	switch format {
	case "wechat_success_v1", "wechat_refund_v1":
		if provider != "wechat" || (format == "wechat_success_v1" && kind != "payment") || (format == "wechat_refund_v1" && kind != "refund") {
			return NativeResult{}, invalid(0, "format", "unsupported_native_format")
		}
		rows, merchant, err = convertWechat(data, kind, expectedMerchantID)
	case "alipay_trade_v1":
		if provider != "alipay" {
			return NativeResult{}, invalid(0, "format", "unsupported_native_format")
		}
		// The classic trade CSV does not provide a verified completeness profile
		// here. Never claim that a selected subset is a complete daily bill.
		if coverage != "partial" {
			return NativeResult{}, invalid(0, "coverage", "native_partial_only")
		}
		rows, err = convertAlipay(data, kind)
		merchant = expectedMerchantID
	default:
		return NativeResult{}, invalid(0, "format", "unsupported_native_format")
	}
	if err != nil {
		return NativeResult{}, err
	}
	var output strings.Builder
	writer := csv.NewWriter(&output)
	if err = writer.Write(header); err != nil {
		return NativeResult{}, invalid(0, "format", "invalid_native_record")
	}
	if err = writer.WriteAll(rows); err != nil {
		return NativeResult{}, invalid(0, "format", "invalid_native_record")
	}
	input.CSV = output.String()
	document, err := Parse(input)
	if err != nil {
		return NativeResult{}, err
	}
	financials, err := extractNativeFinancials(document, format, data)
	if err != nil {
		return NativeResult{}, err
	}
	digest := sha256.Sum256(data)
	return NativeResult{Input: input, SourceDigest: hex.EncodeToString(digest[:]), Format: format, MerchantID: merchant, Financials: financials}, nil
}

func nativeHeader(values, expected []string) bool {
	if len(values) != len(expected) {
		return false
	}
	for i := range expected {
		if values[i] != expected[i] {
			return false
		}
	}
	return true
}

func nativeText(data []byte, gbk bool) (string, error) {
	if len(data) > MaxNativeExpandedBytes {
		return "", invalid(0, "content_base64", "native_too_large")
	}
	if !utf8.Valid(data) {
		if !gbk {
			return "", invalid(0, "format", "invalid_utf8")
		}
		decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(data)
		if err != nil || bytes.ContainsRune(decoded, utf8.RuneError) {
			return "", invalid(0, "format", "invalid_utf8")
		}
		// The decoder replaces malformed sequences on some inputs. A byte-exact
		// round trip rejects those replacements rather than guessing characters.
		encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes(decoded)
		if err != nil || !bytes.Equal(encoded, data) {
			return "", invalid(0, "format", "invalid_utf8")
		}
		data = decoded
	}
	if len(data) > MaxNativeExpandedBytes {
		return "", invalid(0, "content_base64", "native_too_large")
	}
	text := strings.TrimPrefix(string(data), "\ufeff")
	if err := validatePhysicalLines(text); err != nil {
		return "", err
	}
	return text, nil
}

func nativeGzip(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		return data, nil
	}
	reader := bytes.NewReader(data)
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return nil, invalid(0, "format", "native_archive_invalid")
	}
	gz.Multistream(false)
	decoded, err := io.ReadAll(io.LimitReader(gz, MaxNativeExpandedBytes+1))
	closeErr := gz.Close()
	if len(decoded) > MaxNativeExpandedBytes {
		return nil, invalid(0, "content_base64", "native_too_large")
	}
	if err != nil || closeErr != nil || reader.Len() != 0 {
		return nil, invalid(0, "format", "native_archive_invalid")
	}
	return decoded, nil
}

func nativeFields(values []string, count, line int, backtick bool) ([]string, error) {
	if len(values) != count {
		return nil, invalid(line, "format", "invalid_native_record")
	}
	for i, value := range values {
		if backtick {
			if !strings.HasPrefix(value, "`") {
				return nil, invalid(line, "format", "invalid_native_record")
			}
			value = value[1:]
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, invalid(line, "format", "invalid_control")
		}
		values[i] = value
	}
	return values, nil
}

// nativeMinor parses yuan text without floats or rounding. Gross values need
// exact cents, while auxiliary fee text can retain five decimal places without
// being substituted for the gross transaction/refund amount.
func nativeMinor(value string, positive bool, line int) (int64, error) {
	if !nativeMoneyPattern.MatchString(value) {
		return 0, invalid(line, "amount_minor", "invalid_field")
	}
	parts := strings.SplitN(value, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > math.MaxInt64/100 {
		return 0, invalid(line, "amount_minor", "amount_overflow")
	}
	var cents int64
	if len(parts) == 2 {
		fraction := parts[1]
		if len(fraction) == 1 {
			fraction += "0"
		}
		cents, _ = strconv.ParseInt(fraction, 10, 64)
	}
	amount := whole * 100
	if amount > math.MaxInt64-cents {
		return 0, invalid(line, "amount_minor", "amount_overflow")
	}
	amount += cents
	if positive && amount == 0 {
		return 0, invalid(line, "amount_minor", "invalid_field")
	}
	return amount, nil
}

func nativeAuxMoney(value string, negative bool, line int) error {
	if negative {
		value = strings.TrimPrefix(value, "-")
	}
	if !nativeAuxMoneyPattern.MatchString(value) {
		return invalid(line, "amount_minor", "invalid_field")
	}
	return nil
}

func nativeTime(value string, line int) (string, error) {
	if len(value) != len("2006-01-02 15:04:05") {
		return "", invalid(line, "occurred_at", "invalid_field")
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, shanghai)
	if err != nil || parsed.Format("2006-01-02 15:04:05") != value {
		return "", invalid(line, "occurred_at", "invalid_field")
	}
	return parsed.Format(time.RFC3339), nil
}

func convertWechat(data []byte, kind, expectedMerchant string) ([][]string, string, error) {
	data, err := nativeGzip(data)
	if err != nil {
		return nil, "", err
	}
	text, err := nativeText(data, false)
	if err != nil {
		return nil, "", err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	want := wechatSuccessHeader
	if kind == "refund" {
		want = wechatRefundHeader
	}
	fields, err := reader.Read()
	if err != nil || !nativeHeader(fields, want) {
		return nil, "", invalid(1, "format", "unsupported_native_format")
	}
	rows := [][]string{}
	merchant := expectedMerchant
	var total int64
	for {
		values, readErr := reader.Read()
		if readErr != nil {
			return nil, "", invalid(0, "format", "invalid_native_summary")
		}
		line, _ := reader.FieldPos(0)
		if nativeHeader(values, wechatSummaryHeader) {
			summary, summaryErr := reader.Read()
			if summaryErr != nil {
				return nil, "", invalid(line, "format", "invalid_native_summary")
			}
			summary, summaryErr = nativeFields(summary, len(wechatSummaryHeader), line+1, true)
			if summaryErr != nil || !nativeCountPattern.MatchString(summary[0]) {
				return nil, "", invalid(line+1, "format", "invalid_native_summary")
			}
			count, countErr := strconv.Atoi(summary[0])
			if countErr != nil || count != len(rows) {
				return nil, "", invalid(line+1, "format", "invalid_native_summary")
			}
			for i := 1; i < len(summary); i++ {
				if err = nativeAuxMoney(summary[i], i == 4, line+1); err != nil {
					return nil, "", err
				}
			}
			totalIndex := 5
			if kind == "refund" {
				totalIndex = 6
			}
			grossTotal, grossErr := nativeMinor(summary[totalIndex], false, line+1)
			if grossErr != nil || grossTotal != total {
				return nil, "", invalid(line+1, "format", "invalid_native_summary")
			}
			if _, tailErr := reader.Read(); !errors.Is(tailErr, io.EOF) {
				return nil, "", invalid(line+2, "format", "invalid_native_summary")
			}
			return rows, merchant, nil
		}
		if len(rows) >= MaxRows {
			return nil, "", invalid(line, "format", "too_many_rows")
		}
		values, err = nativeFields(values, len(want), line, true)
		if err != nil {
			return nil, "", err
		}
		if !referencePattern.MatchString(values[2]) || (merchant != "" && merchant != values[2]) {
			return nil, "", invalid(line, "format", "native_merchant_mismatch")
		}
		merchant = values[2]
		if values[3] != "" && values[3] != "0" {
			return nil, "", invalid(line, "format", "unsupported_native_format")
		}
		if values[11] != "CNY" {
			return nil, "", invalid(line, "currency", "invalid_field")
		}
		if _, err = nativeTime(values[0], line); err != nil {
			return nil, "", err
		}
		grossIndex, whenIndex := 18, 0
		refundID, providerRefundID := "", ""
		aux := []int{12, 13, 16}
		feeIndex := 16
		if kind == "refund" {
			if values[9] != "REFUND" || values[21] != "SUCCESS" {
				return nil, "", invalid(line, "format", "unsupported_native_format")
			}
			if _, err = nativeTime(values[14], line); err != nil {
				return nil, "", err
			}
			grossIndex, whenIndex = 27, 15
			refundID, providerRefundID = values[17], values[16]
			aux, feeIndex = []int{12, 13, 18, 19, 24, 26}, 24
		} else if values[9] != "SUCCESS" {
			return nil, "", invalid(line, "format", "unsupported_native_format")
		}
		for _, index := range aux {
			if err = nativeAuxMoney(values[index], index == feeIndex, line); err != nil {
				return nil, "", err
			}
		}
		gross, err := nativeMinor(values[grossIndex], true, line)
		if err != nil {
			return nil, "", err
		}
		if total > math.MaxInt64-gross {
			return nil, "", invalid(line, "amount_minor", "amount_overflow")
		}
		total += gross
		when, err := nativeTime(values[whenIndex], line)
		if err != nil {
			return nil, "", err
		}
		rows = append(rows, []string{values[6], values[5], refundID, providerRefundID, strconv.FormatInt(gross, 10), "CNY", when})
	}
}

func alipayArchive(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("PK")) {
		return data, nil
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) == 0 || len(archive.File) > 8 {
		return nil, invalid(0, "format", "native_archive_invalid")
	}
	var detail []byte
	summary := false
	remaining := MaxNativeExpandedBytes
	for _, file := range archive.File {
		name := file.Name
		if !utf8.ValidString(name) {
			decoded, decodeErr := simplifiedchinese.GBK.NewDecoder().Bytes([]byte(name))
			if decodeErr != nil || bytes.ContainsRune(decoded, utf8.RuneError) {
				return nil, invalid(0, "format", "native_archive_invalid")
			}
			name = string(decoded)
		}
		if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") || !strings.HasSuffix(strings.ToLower(name), ".csv") || file.Flags&1 != 0 || file.Mode()&os.ModeSymlink != 0 || !file.Mode().IsRegular() || (file.Method != zip.Store && file.Method != zip.Deflate) {
			return nil, invalid(0, "format", "native_archive_invalid")
		}
		if file.UncompressedSize64 > uint64(remaining) {
			return nil, invalid(0, "content_base64", "native_too_large")
		}
		reader, openErr := file.Open()
		if openErr != nil {
			return nil, invalid(0, "format", "native_archive_invalid")
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, int64(remaining)+1))
		closeErr := reader.Close()
		if len(content) > remaining {
			return nil, invalid(0, "content_base64", "native_too_large")
		}
		remaining -= len(content)
		if readErr != nil || closeErr != nil || bytes.HasPrefix(content, []byte("PK")) || bytes.HasPrefix(content, []byte{0x1f, 0x8b}) {
			return nil, invalid(0, "format", "native_archive_invalid")
		}
		text, textErr := nativeText(content, true)
		if textErr != nil {
			return nil, textErr
		}
		probe := csv.NewReader(strings.NewReader(text))
		probe.Comment = '#'
		probe.FieldsPerRecord = -1
		columns, headerErr := probe.Read()
		if strings.Contains(name, "业务汇总") && !strings.Contains(name, "业务明细") {
			// Parse the header before classifying it: CSV quoting must not let a
			// second detail file hide behind an innocuous summary filename.
			if summary || headerErr != nil || nativeHeader(columns, alipayTradeHeader) {
				return nil, invalid(0, "format", "native_archive_invalid")
			}
			summary = true
			continue
		}
		if headerErr != nil || !nativeHeader(columns, alipayTradeHeader) || detail != nil {
			return nil, invalid(0, "format", "native_archive_invalid")
		}
		detail = content
	}
	if detail == nil {
		return nil, invalid(0, "format", "native_archive_invalid")
	}
	return detail, nil
}

func convertAlipay(data []byte, kind string) ([][]string, error) {
	data, err := alipayArchive(data)
	if err != nil {
		return nil, err
	}
	text, err := nativeText(data, true)
	if err != nil {
		return nil, err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.Comment = '#'
	reader.FieldsPerRecord = -1
	columns, err := reader.Read()
	if err != nil || !nativeHeader(columns, alipayTradeHeader) {
		return nil, invalid(1, "format", "unsupported_native_format")
	}
	rows := [][]string{}
	count := 0
	for {
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, invalid(0, "format", "invalid_native_record")
		}
		line, _ := reader.FieldPos(0)
		count++
		if count > MaxRows {
			return nil, invalid(line, "format", "too_many_rows")
		}
		values, err = nativeFields(values, len(alipayTradeHeader), line, false)
		if err != nil {
			return nil, err
		}
		if !referencePattern.MatchString(values[0]) || !referencePattern.MatchString(values[1]) {
			return nil, invalid(line, "payment_id", "invalid_field")
		}
		rowKind := "payment"
		amountText := values[11]
		refundID := ""
		switch values[2] {
		case "交易":
			if strings.HasPrefix(amountText, "-") {
				return nil, invalid(line, "amount_minor", "invalid_field")
			}
		case "退款":
			rowKind = "refund"
			if !strings.HasPrefix(amountText, "-") || !referencePattern.MatchString(values[21]) {
				return nil, invalid(line, "format", "invalid_native_record")
			}
			amountText = amountText[1:]
			refundID = values[21]
		default:
			return nil, invalid(line, "format", "unsupported_native_format")
		}
		gross, err := nativeMinor(amountText, true, line)
		if err != nil {
			return nil, err
		}
		when, err := nativeTime(values[5], line)
		if err != nil {
			return nil, err
		}
		if _, err = nativeTime(values[4], line); err != nil {
			return nil, err
		}
		if rowKind == kind {
			rows = append(rows, []string{values[1], values[0], refundID, refundID, strconv.FormatInt(gross, 10), "CNY", when})
		}
	}
	if len(rows) == 0 {
		return nil, invalid(0, "format", "native_empty")
	}
	return rows, nil
}
