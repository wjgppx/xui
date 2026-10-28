package payments

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

type wcaFixture struct {
	config WechatConfig
	key    *rsa.PrivateKey
	mode   atomic.Value
	calls  atomic.Int32
}

func wcaRequest() RefundRequest {
	return RefundRequest{PaymentID: wctRequest().PaymentID, TransactionID: "wx-transaction-123", RefundID: strings.Repeat("a", 32), Reason: "本地退款验收", Currency: "CNY", Amount: 1901, Total: 1901}
}
func wcaResult(r RefundRequest) map[string]any {
	return map[string]any{"refund_id": "provider-refund-123", "out_refund_no": r.RefundID, "transaction_id": r.TransactionID, "out_trade_no": r.PaymentID, "status": "SUCCESS", "amount": map[string]any{"refund": r.Amount, "total": r.Total, "currency": r.Currency}}
}
func wcaSetup(t *testing.T) (ActionProvider, *wcaFixture) {
	t.Helper()
	c, key := wctConfig(t)
	f := &wcaFixture{config: c, key: key}
	f.mode.Store("success")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		input, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !wctVerifyMerchant(c, r, input) {
			t.Error("action request signature invalid")
			w.WriteHeader(400)
			return
		}
		mode := f.mode.Load().(string)
		response := wcaResult(wcaRequest())
		status := 200
		var data []byte
		switch r.URL.Path {
		case "/v3/pay/transactions/out-trade-no/" + wctRequest().PaymentID + "/close":
			var request map[string]string
			if r.Method != "POST" || json.Unmarshal(input, &request) != nil || request["mchid"] != c.MchID || len(request) != 1 {
				t.Error("close request did not bind merchant")
			}
			status = 204
		case "/v3/refund/domestic/refunds":
			var request struct {
				TransactionID string `json:"transaction_id"`
				RefundID      string `json:"out_refund_no"`
				Reason        string `json:"reason"`
				Amount        struct {
					Refund, Total int64
					Currency      string
				} `json:"amount"`
			}
			if r.Method != "POST" || json.Unmarshal(input, &request) != nil || request.TransactionID != wcaRequest().TransactionID || request.RefundID != wcaRequest().RefundID || request.Amount.Refund != 1901 || request.Amount.Total != 1901 || request.Amount.Currency != "CNY" || !utf8.ValidString(request.Reason) || len(request.Reason) > 80 {
				t.Error("refund request lost immutable financial snapshot")
			}
		case "/v3/refund/domestic/refunds/" + wcaRequest().RefundID:
			if r.Method != "GET" || len(input) != 0 {
				t.Error("invalid refund query request")
			}
		default:
			t.Error("unexpected action route")
			w.WriteHeader(400)
			return
		}
		switch mode {
		case "processing":
			response["status"] = "PROCESSING"
		case "closed":
			response["status"] = "CLOSED"
		case "abnormal":
			response["status"] = "ABNORMAL"
		case "unknown_status":
			response["status"] = "NEW_UNKNOWN_STATE"
		case "wrong_payment":
			response["out_trade_no"] = "different"
		case "wrong_transaction":
			response["transaction_id"] = "different"
		case "wrong_refund":
			response["out_refund_no"] = "different"
		case "missing_provider_id":
			response["refund_id"] = ""
		case "wrong_amount":
			response["amount"] = map[string]any{"refund": 1900, "total": 1901, "currency": "CNY"}
		case "wrong_total":
			response["amount"] = map[string]any{"refund": 1901, "total": 3901, "currency": "CNY"}
		case "wrong_currency":
			response["amount"] = map[string]any{"refund": 1901, "total": 1901, "currency": "USD"}
		case "sub_cent":
			response["amount"] = map[string]any{"refund": 1901.5, "total": 1901, "currency": "CNY"}
		case "order_not_found":
			status = 404
			response = map[string]any{"code": "ORDER_NOT_EXIST"}
		case "refund_not_found":
			status = 404
			response = map[string]any{"code": "RESOURCE_NOT_EXISTS"}
		case "unrelated_404":
			status = 404
			response = map[string]any{"code": "UNKNOWN_ERROR"}
		case "business_error":
			status = 400
			response = map[string]any{"code": "ORDERPAID"}
		case "redirect":
			status = 307
			w.Header().Set("Location", "https://must-not-follow.example.test")
		}
		if status != 204 {
			data = wctJSON(t, response)
		}
		if mode != "unsigned" {
			for k, v := range wctHeaders(t, c, key, data) {
				w.Header()[k] = v
			}
		}
		if mode == "wrong_signature" {
			w.Header().Set("Wechatpay-Signature", "not-a-valid-signature")
		}
		if mode == "wrong_serial" {
			w.Header().Set("Wechatpay-Serial", "other-platform")
		}
		if mode == "tampered" {
			data = append(data, ' ')
		}
		w.WriteHeader(status)
		if len(data) > 0 {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)
	c.Gateway = srv.URL
	c.HTTPClient = srv.Client()
	p, err := NewWechat(c)
	if err != nil {
		t.Fatal(err)
	}
	action, ok := p.(ActionProvider)
	if !ok {
		t.Fatal("WeChat action capability missing")
	}
	return action, f
}

func TestWechatCloseRequiresPositiveSignedClosureProof(t *testing.T) {
	a, f := wcaSetup(t)
	r, err := a.Close(context.Background(), wctRequest())
	if err != nil || r.Status != "closed" || r.PaymentID != wctRequest().PaymentID {
		t.Fatal("valid close proof rejected", r, err)
	}
	for _, mode := range []string{"order_not_found", "refund_not_found", "unrelated_404", "business_error", "unsigned", "wrong_signature", "wrong_serial", "redirect"} {
		f.mode.Store(mode)
		r, err = a.Close(context.Background(), wctRequest())
		if err == nil || r.Status == "closed" {
			t.Fatal("unproven closure accepted", mode)
		}
		if mode == "order_not_found" && !errors.Is(err, ErrNotPaid) {
			t.Fatal("signed absence not classified", err)
		}
		if mode != "order_not_found" && errors.Is(err, ErrNotPaid) {
			t.Fatal("generic failure misclassified as absence", mode)
		}
	}
}

func TestWechatRefundAndQueryUseStableIdentityAndVerifiedStatuses(t *testing.T) {
	a, f := wcaSetup(t)
	req := wcaRequest()
	req.Reason = strings.Repeat("退款说明", 20)
	r, err := a.Refund(context.Background(), req)
	if err != nil || r.Status != "succeeded" || r.RefundID != req.RefundID || r.PaymentID != req.PaymentID || r.TransactionID != req.TransactionID || r.Amount != req.Amount || r.Currency != "CNY" || r.ProviderRefundID != "provider-refund-123" {
		t.Fatal("verified refund rejected", r, err)
	}
	for wire, want := range map[string]string{"success": "succeeded", "processing": "processing", "closed": "failed", "abnormal": "manual_required"} {
		f.mode.Store(wire)
		r, err = a.QueryRefund(context.Background(), req)
		if err != nil || r.Status != want || r.RefundID != req.RefundID {
			t.Fatal("refund status lost", wire, r, err)
		}
	}
}

func TestWechatRefundRejectsTamperedOrMismatchedFinancialData(t *testing.T) {
	a, f := wcaSetup(t)
	for _, mode := range []string{"wrong_payment", "wrong_transaction", "wrong_refund", "missing_provider_id", "wrong_amount", "wrong_total", "wrong_currency", "sub_cent", "unknown_status", "unsigned", "wrong_signature", "wrong_serial", "tampered", "redirect"} {
		f.mode.Store(mode)
		if _, err := a.QueryRefund(context.Background(), wcaRequest()); err == nil || errors.Is(err, ErrNotPaid) {
			t.Fatal("untrusted refund response accepted", mode, err)
		}
		if _, err := a.Refund(context.Background(), wcaRequest()); err == nil {
			t.Fatal("untrusted refund creation accepted", mode)
		}
	}
	for _, mode := range []string{"refund_not_found", "order_not_found", "unrelated_404", "unsigned"} {
		f.mode.Store(mode)
		_, err := a.QueryRefund(context.Background(), wcaRequest())
		if mode == "refund_not_found" {
			if !errors.Is(err, ErrNotPaid) {
				t.Fatal("signed refund absence rejected", err)
			}
		} else if mode == "order_not_found" {
			if err == nil || errors.Is(err, ErrNotPaid) {
				t.Fatal("missing original transaction is not refund absence", err)
			}
		} else if err == nil || errors.Is(err, ErrNotPaid) {
			t.Fatal("unsafe absence classification", mode, err)
		}
	}
}

func TestWechatActionsRejectPartialRefundsInvalidInputAndCancelledRequests(t *testing.T) {
	a, f := wcaSetup(t)
	for _, edit := range []func(*RefundRequest){func(r *RefundRequest) { r.Amount-- }, func(r *RefundRequest) { r.Amount = 0; r.Total = 0 }, func(r *RefundRequest) { r.Total = 1 << 32; r.Amount = r.Total }, func(r *RefundRequest) { r.Currency = "USD" }, func(r *RefundRequest) { r.RefundID = "bad/id" }, func(r *RefundRequest) { r.PaymentID = "bad/id" }, func(r *RefundRequest) { r.TransactionID = "bad\ntransaction" }, func(r *RefundRequest) { r.Reason = "bad\nreason" }} {
		r := wcaRequest()
		edit(&r)
		if _, err := a.Refund(context.Background(), r); err == nil {
			t.Fatal("invalid refund request sent")
		}
		if _, err := a.QueryRefund(context.Background(), r); err == nil {
			t.Fatal("invalid refund query sent")
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid actions reached network")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Refund(ctx, wcaRequest()); err == nil {
		t.Fatal("cancelled refund accepted")
	}
	if _, err := a.Close(ctx, wctRequest()); err == nil {
		t.Fatal("cancelled close accepted")
	}
}
