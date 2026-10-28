package payments

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func alaRequest() RefundRequest {
	return RefundRequest{PaymentID: strings.Repeat("a", 32), TransactionID: "202610272200000001", RefundID: strings.Repeat("c", 32), Reason: "全额退款", Currency: "CNY", Amount: 1901, Total: 1901}
}
func alaFixture(t *testing.T) (ActionProvider, *atomic.Value, *atomic.Int32) {
	t.Helper()
	c := alipayTestConfig(t)
	_, platform := alipayKeys(t)
	mode := &atomic.Value{}
	mode.Store("success")
	calls := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		v := r.PostForm
		sig, e := base64.StdEncoding.DecodeString(v.Get("sign"))
		sum := sha256.Sum256([]byte(alipayFixtureCanonical(v, false)))
		if e != nil || rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, sum[:], sig) != nil || v.Get("app_id") != c.AppID {
			t.Error("merchant action signature invalid")
		}
		var biz map[string]string
		if json.Unmarshal([]byte(v.Get("biz_content")), &biz) != nil || biz["out_trade_no"] != alaRequest().PaymentID {
			t.Error("payment identity lost")
		}
		method := v.Get("method")
		name := ""
		payload := map[string]string{"code": "10000", "out_trade_no": alaRequest().PaymentID, "trade_no": alaRequest().TransactionID}
		switch method {
		case "alipay.trade.close":
			name = "alipay_trade_close_response"
		case "alipay.trade.refund":
			name = "alipay_trade_refund_response"
			payload["refund_fee"] = "19.01"
			payload["fund_change"] = "Y"
			if biz["out_request_no"] != alaRequest().RefundID || biz["trade_no"] != alaRequest().TransactionID || biz["refund_amount"] != "19.01" {
				t.Error("refund snapshot changed")
			}
		case "alipay.trade.fastpay.refund.query":
			name = "alipay_trade_fastpay_refund_query_response"
			payload["out_request_no"] = alaRequest().RefundID
			payload["refund_amount"] = "19.01"
			payload["total_amount"] = "19.01"
			payload["refund_status"] = "REFUND_SUCCESS"
			if biz["out_request_no"] != alaRequest().RefundID || biz["trade_no"] != alaRequest().TransactionID {
				t.Error("refund query identity lost")
			}
		default:
			t.Error("unexpected method")
			w.WriteHeader(400)
			return
		}
		m := mode.Load().(string)
		switch m {
		case "wrong_payment":
			payload["out_trade_no"] = "wrong"
		case "wrong_transaction":
			payload["trade_no"] = "wrong"
		case "wrong_refund":
			payload["out_request_no"] = "wrong"
		case "wrong_amount":
			payload["refund_fee"] = "19.02"
			payload["refund_amount"] = "19.02"
		case "wrong_total":
			payload["total_amount"] = "19.02"
		case "wrong_currency":
			payload["trans_currency"] = "USD"
		case "fraction":
			payload["refund_amount"] = "19.001"
		case "processing":
			payload["refund_status"] = "REFUND_PROCESSING"
		case "missing_state":
			delete(payload, "refund_status")
		case "failed":
			payload["refund_status"] = "REFUND_FAIL"
		case "unknown_state":
			payload["refund_status"] = "unknown"
		case "refund_not_found":
			payload["code"] = "40004"
			payload["sub_code"] = "ACQ.REFUND_NOT_EXIST"
		case "trade_not_found":
			payload["code"] = "40004"
			payload["sub_code"] = "ACQ.TRADE_NOT_EXIST"
		case "business_error":
			payload["code"] = "40004"
			payload["sub_code"] = "ACQ.INVALID_PARAMETER"
		}
		raw, _ := json.Marshal(payload)
		signature := alipayFixtureSign(t, platform, raw)
		if m == "unsigned" {
			signature = ""
		}
		if m == "wrong_signature" {
			signature = alipayFixtureSign(t, c.PrivateKey, raw)
		}
		if m == "tampered" {
			raw = append([]byte(" "), raw...)
		}
		// Envelope verifies the exact bytes between the response object's braces.
		if m == "tampered" {
			raw = []byte(strings.ReplaceAll(string(raw), "19.01", "19.02"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{name: json.RawMessage(raw), "sign": signature})
	}))
	t.Cleanup(server.Close)
	c.Gateway = server.URL
	c.HTTPClient = server.Client()
	p, err := NewAlipay(c)
	if err != nil {
		t.Fatal(err)
	}
	return p.(ActionProvider), mode, calls
}
func TestAlipayActionsRequireSignedClosureAndExactRefundConfirmation(t *testing.T) {
	p, _, calls := alaFixture(t)
	c, err := p.Close(context.Background(), alipayTestRequest())
	if err != nil || c.Status != "closed" {
		t.Fatal(c, err)
	}
	r, err := p.Refund(context.Background(), alaRequest())
	if err != nil || r.Status != "succeeded" || r.RefundID != alaRequest().RefundID || r.Amount != 1901 || calls.Load() != 3 {
		t.Fatal("submit was not followed by exact signed query", r, err, calls.Load())
	}
}
func TestAlipayRefundRejectsMismatchedAndUntrustedResults(t *testing.T) {
	p, mode, _ := alaFixture(t)
	for _, m := range []string{"wrong_payment", "wrong_transaction", "wrong_refund", "wrong_amount", "wrong_total", "wrong_currency", "fraction", "unknown_state", "unsigned", "wrong_signature", "tampered", "business_error", "trade_not_found"} {
		mode.Store(m)
		if _, err := p.QueryRefund(context.Background(), alaRequest()); err == nil || errors.Is(err, ErrNotPaid) {
			t.Fatal("unsafe refund proof", m, err)
		}
	}
	mode.Store("refund_not_found")
	if _, err := p.QueryRefund(context.Background(), alaRequest()); !errors.Is(err, ErrNotPaid) {
		t.Fatal("signed refund absence not recognized", err)
	}
	for _, m := range []string{"unsigned", "wrong_signature", "wrong_payment", "trade_not_found", "business_error"} {
		mode.Store(m)
		r, err := p.Close(context.Background(), alipayTestRequest())
		if err == nil || r.Status == "closed" {
			t.Fatal("unproven closure accepted", m)
		}
	}
}
func TestAlipayRefundPendingIsConservativeAndInputsAreFullOnly(t *testing.T) {
	p, mode, calls := alaFixture(t)
	for m, want := range map[string]string{"processing": "processing", "missing_state": "processing", "failed": "failed"} {
		mode.Store(m)
		r, err := p.QueryRefund(context.Background(), alaRequest())
		if err != nil || r.Status != want {
			t.Fatal(m, r, err)
		}
	}
	before := calls.Load()
	for _, edit := range []func(*RefundRequest){func(r *RefundRequest) { r.Amount-- }, func(r *RefundRequest) { r.Currency = "USD" }, func(r *RefundRequest) { r.RefundID = "invalid" }, func(r *RefundRequest) { r.Reason = "bad\nreason" }} {
		r := alaRequest()
		edit(&r)
		if _, err := p.Refund(context.Background(), r); err == nil {
			t.Fatal("invalid refund accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input reached network")
	}
}
func TestPaymentActionGateIsExplicitAndClassicEpayHasNoMoneyActions(t *testing.T) {
	for _, value := range []string{"", "false", "true", "yes", "1"} {
		enabled, err := actionsEnabled(value)
		if value == "true" {
			if err != nil || !enabled {
				t.Fatal(value)
			}
		} else if value == "" || value == "false" {
			if err != nil || enabled {
				t.Fatal(value)
			}
		} else if err == nil || enabled {
			t.Fatal("ambiguous gate enabled", value)
		}
	}
	p, err := NewEpay(EpayConfig{PID: "1000", Key: "local-fixture-key", Gateway: "https://pay.example.test", Type: "alipay"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(ActionProvider); ok {
		t.Fatal("unsigned classic epay exposed financial actions")
	}
}
