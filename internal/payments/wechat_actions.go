package payments

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (w *wechatProvider) Close(ctx context.Context, r Request) (CloseResult, error) {
	if !wechatValidRequest(r) {
		return CloseResult{}, ErrGateway
	}
	body, err := json.Marshal(map[string]string{"mchid": w.config.MchID})
	if err != nil {
		return CloseResult{}, ErrGateway
	}
	data, err := w.callStatus(ctx, http.MethodPost, "/v3/pay/transactions/out-trade-no/"+url.PathEscape(r.PaymentID)+"/close", body, http.StatusNoContent)
	if err != nil {
		return CloseResult{}, err
	}
	if len(data) != 0 {
		return CloseResult{}, ErrVerification
	}
	return CloseResult{PaymentID: r.PaymentID, Status: "closed"}, nil
}

func wechatValidRefund(r RefundRequest) bool {
	return wechatTradeID.MatchString(r.PaymentID) && wechatTradeID.MatchString(r.RefundID) &&
		wechatToken(r.TransactionID, 64) && r.Amount > 0 && r.Amount == r.Total && r.Total <= math.MaxInt32 && r.Currency == "CNY" &&
		utf8.ValidString(r.Reason) && len(r.Reason) <= 1024 && strings.IndexFunc(r.Reason, unicode.IsControl) < 0
}

func wechatReason(value string) string {
	// WeChat limits the reason to 80 UTF-8 bytes. Keep a complete code point at
	// the boundary; the platform retains the complete operator reason separately.
	for len(value) > 80 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func (w *wechatProvider) refundResult(data []byte, r RefundRequest) (RefundResult, error) {
	var body struct {
		RefundID         string `json:"refund_id"`
		MerchantRefundID string `json:"out_refund_no"`
		TransactionID    string `json:"transaction_id"`
		PaymentID        string `json:"out_trade_no"`
		Status           string `json:"status"`
		Amount           struct {
			Refund, Total int64
			Currency      string
		} `json:"amount"`
	}
	if json.Unmarshal(data, &body) != nil || body.MerchantRefundID != r.RefundID || body.TransactionID != r.TransactionID || body.PaymentID != r.PaymentID || !wechatToken(body.RefundID, 64) || body.Amount.Refund != r.Amount || body.Amount.Total != r.Total || body.Amount.Currency != r.Currency {
		return RefundResult{}, ErrVerification
	}
	status := ""
	switch body.Status {
	case "SUCCESS":
		status = "succeeded"
	case "PROCESSING":
		status = "processing"
	case "CLOSED":
		status = "failed"
	case "ABNORMAL":
		status = "manual_required"
	default:
		return RefundResult{}, ErrVerification
	}
	return RefundResult{PaymentID: r.PaymentID, TransactionID: r.TransactionID, RefundID: r.RefundID, ProviderRefundID: body.RefundID, Currency: r.Currency, Amount: r.Amount, Status: status}, nil
}

func (w *wechatProvider) Refund(ctx context.Context, r RefundRequest) (RefundResult, error) {
	if !wechatValidRefund(r) {
		return RefundResult{}, ErrGateway
	}
	body, err := json.Marshal(map[string]any{"transaction_id": r.TransactionID, "out_refund_no": r.RefundID, "reason": wechatReason(r.Reason), "amount": map[string]any{"refund": r.Amount, "total": r.Total, "currency": r.Currency}})
	if err != nil {
		return RefundResult{}, ErrGateway
	}
	data, err := w.call(ctx, http.MethodPost, "/v3/refund/domestic/refunds", body)
	if err != nil {
		return RefundResult{}, err
	}
	return w.refundResult(data, r)
}

func (w *wechatProvider) QueryRefund(ctx context.Context, r RefundRequest) (RefundResult, error) {
	if !wechatValidRefund(r) {
		return RefundResult{}, ErrGateway
	}
	data, err := w.call(ctx, http.MethodGet, "/v3/refund/domestic/refunds/"+url.PathEscape(r.RefundID), nil)
	if err != nil {
		return RefundResult{}, err
	}
	return w.refundResult(data, r)
}

var _ ActionProvider = (*wechatProvider)(nil)
