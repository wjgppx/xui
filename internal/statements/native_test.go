package statements_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/statements"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const wxPaymentColumns = "交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,商品名称,商户数据包,手续费,费率,订单金额,费率备注"
const wxRefundColumns = "交易时间,公众账号ID,商户号,特约商户号,设备号,微信订单号,商户订单号,用户标识,交易类型,交易状态,付款银行,货币种类,应结订单金额,代金券金额,退款申请时间,退款成功时间,微信退款单号,商户退款单号,退款金额,充值券退款金额,退款类型,退款状态,商品名称,商户数据包,手续费,费率,订单金额,申请退款金额,费率备注"
const wxTotalColumns = "总交易单数,应结订单总金额,退款总金额,充值券退款总金额,手续费总金额,订单总金额,申请退款总金额"
const aliColumns = "支付宝交易号,商户订单号,业务类型,商品名称,创建时间,完成时间,门店编号,门店名称,操作员,终端号,对方账户,订单金额（元）,商家实收（元）,支付宝红包（元）,集分宝（元）,支付宝优惠（元）,商家优惠（元）,券核销金额（元）,券名称,商家红包消费金额（元）,卡消费金额（元）,退款批次号/请求号,服务费（元）,分润（元）,备注"

func wxNativeRow(refund bool) []string {
	if refund {
		return []string{"2026-10-26 23:59:00", "wx-app-A", "merchant-A", "0", "", "wx-transaction-A", "payment-A", "private-user", "NATIVE", "REFUND", "BANK", "CNY", "18.01", "1.00", "2026-10-27 00:00:01", "2026-10-27 00:00:05", "wx-refund-A", "refund-A", "18.01", "0.00", "ORIGINAL", "SUCCESS", "商品", "", "-0.01000", "0.60%", "19.01", "19.01", ""}
	}
	return []string{"2026-10-27 00:00:05", "wx-app-A", "merchant-A", "0", "", "wx-transaction-A", "payment-A", "private-user", "NATIVE", "SUCCESS", "BANK", "CNY", "18.01", "1.00", "商品", "", "0.01000", "0.60%", "19.01", ""}
}
func nativeCSV(t *testing.T, records ...[]string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := csv.NewWriter(&out)
	for _, r := range records {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func tick(values []string) []string {
	r := make([]string, len(values))
	for i, v := range values {
		r[i] = "`" + v
	}
	return r
}
func wxNativeCSV(t *testing.T, refund bool, rows [][]string, footer []string) []byte {
	t.Helper()
	head := wxPaymentColumns
	if refund {
		head = wxRefundColumns
	}
	records := [][]string{strings.Split(head, ",")}
	for _, r := range rows {
		records = append(records, tick(r))
	}
	if footer != nil {
		records = append(records, strings.Split(wxTotalColumns, ","), tick(footer))
	}
	return nativeCSV(t, records...)
}
func wxNativeTotals(refund bool) []string {
	if refund {
		return []string{"1", "18.01", "18.01", "0.00", "-0.01000", "19.01", "19.01"}
	}
	return []string{"1", "18.01", "0.00", "0.00", "0.01000", "19.01", "0.00"}
}
func convertWx(t *testing.T, refund bool, data []byte) (statements.NativeResult, error) {
	t.Helper()
	kind, format := "payment", "wechat_success_v1"
	if refund {
		kind, format = "refund", "wechat_refund_v1"
	}
	return statements.ConvertNative("wechat", kind, "2026-10-27", "full", format, data, "merchant-A")
}
func requireNativeError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, statements.ErrInvalid) {
		t.Fatal("invalid native bill accepted", err)
	}
}
func nativeGZ(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestNativeWechatUsesGrossAmountAndExplicitRefundSuccessTime(t *testing.T) {
	for _, refund := range []bool{false, true} {
		raw := wxNativeCSV(t, refund, [][]string{wxNativeRow(refund)}, wxNativeTotals(refund))
		result, err := convertWx(t, refund, raw)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := statements.Parse(result.Input)
		if err != nil || len(doc.Rows) != 1 || doc.Rows[0].Amount != 1901 || doc.Rows[0].OccurredAt.Format("2006-01-02T15:04:05Z") != "2026-10-26T16:00:05Z" || result.MerchantID != "merchant-A" {
			t.Fatal("gross or refund date lost", doc, err)
		}
		if refund && (doc.Rows[0].RefundID != "refund-A" || doc.Rows[0].ProviderRefundID != "wx-refund-A") {
			t.Fatal("refund identity lost")
		}
	}
}
func TestNativeWechatRequiresCompleteCorrectSummary(t *testing.T) {
	for _, mode := range []string{"missing", "count", "gross", "extra", "truncated", "sub_cent"} {
		row := wxNativeRow(false)
		footer := wxNativeTotals(false)
		switch mode {
		case "missing":
			footer = nil
		case "count":
			footer[0] = "2"
		case "gross":
			footer[5] = "18.01"
		case "sub_cent":
			row[18] = "19.001"
		}
		raw := wxNativeCSV(t, false, [][]string{row}, footer)
		if mode == "extra" {
			raw = append(raw, []byte("unexpected\n")...)
		}
		if mode == "truncated" {
			raw = raw[:len(raw)-12]
		}
		_, err := convertWx(t, false, raw)
		requireNativeError(t, err)
	}
}
func TestNativeWechatRejectsMissingRefundTimeAndWrongDay(t *testing.T) {
	for _, value := range []string{"", "2026-10-26 23:59:59", "2026-10-28 00:00:00", "not-a-time"} {
		row := wxNativeRow(true)
		row[15] = value
		_, err := convertWx(t, true, wxNativeCSV(t, true, [][]string{row}, wxNativeTotals(true)))
		requireNativeError(t, err)
	}
}
func TestNativeWechatRetainsMultipleAppsButRejectsMerchantAndSubmerchantMix(t *testing.T) {
	for _, mode := range []string{"other_app", "other_merchant", "submerchant", "wrong_state", "currency", "duplicate"} {
		a, b := wxNativeRow(false), wxNativeRow(false)
		b[5] = "wx-transaction-B"
		b[6] = "payment-B"
		b[1] = "wx-app-B"
		switch mode {
		case "other_merchant":
			b[2] = "merchant-B"
		case "submerchant":
			b[3] = "submerchant"
		case "wrong_state":
			b[9] = "NOTPAY"
		case "currency":
			b[11] = "USD"
		case "duplicate":
			b[6] = a[6]
		}
		footer := []string{"2", "36.02", "0.00", "0.00", "0.02000", "38.02", "0.00"}
		result, err := convertWx(t, false, wxNativeCSV(t, false, [][]string{a, b}, footer))
		if mode == "other_app" {
			doc, e := statements.Parse(result.Input)
			if err != nil || e != nil || len(doc.Rows) != 2 {
				t.Fatal("merchant-wide app records filtered", err, e)
			}
		} else {
			requireNativeError(t, err)
		}
	}
}
func TestNativeWechatZeroBillRequiresExplicitValidatedZeroSummary(t *testing.T) {
	raw := wxNativeCSV(t, false, nil, []string{"0", "0.00", "0.00", "0.00", "0.00000", "0.00", "0.00"})
	result, err := convertWx(t, false, raw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := statements.Parse(result.Input)
	if err != nil || len(doc.Rows) != 0 || doc.Total != 0 {
		t.Fatal(doc, err)
	}
	_, err = convertWx(t, false, wxNativeCSV(t, false, nil, nil))
	requireNativeError(t, err)
}
func TestNativeGzipRequiresCRCAndSingleBoundedMember(t *testing.T) {
	raw := wxNativeCSV(t, false, [][]string{wxNativeRow(false)}, wxNativeTotals(false))
	compressed := nativeGZ(t, raw)
	if _, err := convertWx(t, false, compressed); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte{}, compressed...)
	bad[len(bad)-1] ^= 1
	for _, data := range [][]byte{bad, compressed[:len(compressed)-3], append(append([]byte{}, compressed...), compressed...), nativeGZ(t, bytes.Repeat([]byte{'x'}, statements.MaxNativeExpandedBytes+1))} {
		_, err := convertWx(t, false, data)
		requireNativeError(t, err)
	}
}
func alipayNativeCSV(t *testing.T, refund bool) []byte {
	t.Helper()
	row := make([]string, 25)
	row[0], row[1], row[2], row[3] = "ali-trade-A", "ali-payment-A", "交易", "商品"
	row[4], row[5] = "2026-10-26 23:59:00", "2026-10-27 00:00:05"
	row[11], row[12] = "19.01", "18.01"
	if refund {
		row[2], row[11], row[21] = "退款", "-19.01", "ali-refund-A"
	}
	return nativeCSV(t, strings.Split(aliColumns, ","), row)
}

type zipEntry struct {
	name string
	data []byte
}

func nativeZIP(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, entry := range entries {
		f, err := w.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func convertAli(data []byte, refund bool, coverage string) (statements.NativeResult, error) {
	kind := "payment"
	if refund {
		kind = "refund"
	}
	return statements.ConvertNative("alipay", kind, "2026-10-27", coverage, "alipay_trade_v1", data, "")
}
func TestNativeAlipaySupportsConservativeUTF8GBKAndZIPProfiles(t *testing.T) {
	for _, refund := range []bool{false, true} {
		raw := alipayNativeCSV(t, refund)
		gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, data := range [][]byte{raw, gbk, nativeZIP(t, zipEntry{"业务明细.csv", gbk})} {
			result, err := convertAli(data, refund, "partial")
			if err != nil {
				t.Fatal(err)
			}
			doc, err := statements.Parse(result.Input)
			if err != nil || doc.Total != 1901 || len(doc.Rows) != 1 {
				t.Fatal(doc, err)
			}
			if refund && (doc.Rows[0].RefundID != "ali-refund-A" || doc.Rows[0].ProviderRefundID != "ali-refund-A") {
				t.Fatal("Alipay stable refund identity lost")
			}
		}
	}
}
func TestNativeAlipayRefusesFullEmptyOrAmbiguousRecords(t *testing.T) {
	_, err := convertAli(alipayNativeCSV(t, false), false, "full")
	requireNativeError(t, err)
	for _, data := range [][]byte{nativeCSV(t, strings.Split(aliColumns, ",")), []byte(strings.Replace(string(alipayNativeCSV(t, true)), "-19.01", "19.01", 1)), []byte(strings.Replace(string(alipayNativeCSV(t, false)), "交易,", "撤销,", 1)), []byte{0x81, 0x00}} {
		_, err = convertAli(data, false, "partial")
		requireNativeError(t, err)
	}
	_, err = convertAli(alipayNativeCSV(t, false), true, "partial")
	requireNativeError(t, err)
}
func TestNativeZIPRefusesUnsafePathsAndDisguisedSecondDetail(t *testing.T) {
	raw := alipayNativeCSV(t, false)
	quoted := []byte(strings.Replace(string(raw), aliColumns, `"`+strings.ReplaceAll(aliColumns, ",", `","`)+`"`, 1))
	for _, archive := range [][]byte{nativeZIP(t, zipEntry{"../业务明细.csv", raw}), nativeZIP(t, zipEntry{"业务明细.csv", raw}, zipEntry{"another.csv", raw}), nativeZIP(t, zipEntry{"业务明细.csv", raw}, zipEntry{"业务汇总.csv", quoted}), nativeZIP(t, zipEntry{"nested.csv", nativeZIP(t, zipEntry{"x.csv", raw})})} {
		_, err := convertAli(archive, false, "partial")
		requireNativeError(t, err)
	}
	valid := nativeZIP(t, zipEntry{"业务明细.csv", raw})
	valid[len(valid)/3] ^= 0xff
	_, err := convertAli(valid, false, "partial")
	requireNativeError(t, err)
}
func TestNativeBase64LimitsAndIntegerOverflowRejectWholeImport(t *testing.T) {
	input := statements.NativeImport{Provider: "wechat", Kind: "payment", Date: "2026-10-27", Coverage: "full", Format: "wechat_success_v1", ContentBase64: "bad!"}
	_, err := statements.ParseNative(input)
	requireNativeError(t, err)
	input.ContentBase64 = strings.Repeat("A", base64.StdEncoding.EncodedLen(statements.MaxNativeBytes)+4)
	_, err = statements.ParseNative(input)
	requireNativeError(t, err)
	a, b := wxNativeRow(false), wxNativeRow(false)
	a[18], b[18] = "92233720368547758.07", "92233720368547758.07"
	b[5], b[6] = "txn-B", "payment-B"
	_, err = convertWx(t, false, wxNativeCSV(t, false, [][]string{a, b}, []string{"2", "0.00", "0.00", "0.00", "0.00", "0.00", "0.00"}))
	requireNativeError(t, err)
}
