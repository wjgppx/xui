package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (a *alipayProvider) actionCall(ctx context.Context, method, responseName string, biz any) (json.RawMessage, error) {
	values, err := a.parameters(method, biz)
	if err != nil {
		return nil, err
	}
	if err = a.sign(values); err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.Gateway, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, ErrGateway
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(r)
	if err != nil {
		return nil, ErrGateway
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || resp.StatusCode != http.StatusOK {
		return nil, ErrGateway
	}
	payload, signature, err := alipayNamedEnvelope(body, responseName)
	if err != nil {
		return nil, err
	}
	if !a.verify(payload, signature) {
		return nil, ErrVerification
	}
	var result struct {
		Code     string `json:"code"`
		SubCode  string `json:"sub_code"`
		AppID    string `json:"app_id"`
		SellerID string `json:"seller_id"`
	}
	if json.Unmarshal(payload, &result) != nil {
		return nil, ErrGateway
	}
	if (result.AppID != "" && result.AppID != a.config.AppID) || (result.SellerID != "" && result.SellerID != a.config.SellerID) {
		return nil, ErrVerification
	}
	if result.Code != "10000" {
		// A signed, explicit not-found result is distinct from network or signature
		// failure. It never proves that a deferred PagePay trade has been closed.
		if (method == "alipay.trade.close" && result.SubCode == "ACQ.TRADE_NOT_EXIST") || (method == "alipay.trade.fastpay.refund.query" && result.SubCode == "ACQ.REFUND_NOT_EXIST") {
			return nil, ErrNotPaid
		}
		return nil, ErrGateway
	}
	return payload, nil
}

func (a *alipayProvider) Close(ctx context.Context, r Request) (CloseResult, error) {
	if !alipayPaymentID.MatchString(r.PaymentID) || r.Amount <= 0 || r.Currency != "CNY" {
		return CloseResult{}, ErrGateway
	}
	payload, err := a.actionCall(ctx, "alipay.trade.close", "alipay_trade_close_response", map[string]string{"out_trade_no": r.PaymentID})
	if err != nil {
		return CloseResult{}, err
	}
	var result struct {
		PaymentID string `json:"out_trade_no"`
		TradeID   string `json:"trade_no"`
	}
	if json.Unmarshal(payload, &result) != nil || result.PaymentID != r.PaymentID || !alipayReference.MatchString(result.TradeID) {
		return CloseResult{}, ErrVerification
	}
	return CloseResult{PaymentID: r.PaymentID, Status: "closed"}, nil
}

func alipayRefundRequest(r RefundRequest) bool {
	return alipayPaymentID.MatchString(r.PaymentID) && alipayPaymentID.MatchString(r.RefundID) && alipayReference.MatchString(r.TransactionID) && r.Currency == "CNY" && r.Amount > 0 && r.Amount == r.Total && utf8.ValidString(r.Reason) && len(r.Reason) <= 256 && strings.IndexFunc(r.Reason, unicode.IsControl) < 0
}

func (a *alipayProvider) Refund(ctx context.Context, r RefundRequest) (RefundResult, error) {
	if !alipayRefundRequest(r) {
		return RefundResult{}, ErrGateway
	}
	payload, err := a.actionCall(ctx, "alipay.trade.refund", "alipay_trade_refund_response", map[string]string{"out_trade_no": r.PaymentID, "trade_no": r.TransactionID, "out_request_no": r.RefundID, "refund_amount": alipayMoney(r.Amount), "refund_reason": r.Reason})
	if err != nil {
		return RefundResult{}, err
	}
	var result struct {
		PaymentID     string `json:"out_trade_no"`
		TransactionID string `json:"trade_no"`
		Fee           string `json:"refund_fee"`
		FundChange    string `json:"fund_change"`
	}
	if json.Unmarshal(payload, &result) != nil || result.PaymentID != r.PaymentID || result.TransactionID != r.TransactionID || (result.FundChange != "Y" && result.FundChange != "N") {
		return RefundResult{}, ErrVerification
	}
	amount, err := alipayMinor(result.Fee)
	if err != nil || amount != r.Amount {
		return RefundResult{}, ErrVerification
	}
	// The submit response does not reliably echo out_request_no. Do not promote
	// an ambiguous response to a completed refund. Confirm the exact stable
	// request identity through the signed refund query, including on retries.
	confirmed, err := a.QueryRefund(ctx, r)
	if err == nil {
		return confirmed, nil
	}
	if !errors.Is(err, ErrNotPaid) {
		return RefundResult{}, err
	}
	return RefundResult{PaymentID: r.PaymentID, TransactionID: r.TransactionID, RefundID: r.RefundID, ProviderRefundID: r.RefundID, Amount: r.Amount, Currency: r.Currency, Status: "processing"}, nil
}

func (a *alipayProvider) QueryRefund(ctx context.Context, r RefundRequest) (RefundResult, error) {
	if !alipayRefundRequest(r) {
		return RefundResult{}, ErrGateway
	}
	payload, err := a.actionCall(ctx, "alipay.trade.fastpay.refund.query", "alipay_trade_fastpay_refund_query_response", map[string]string{"out_trade_no": r.PaymentID, "trade_no": r.TransactionID, "out_request_no": r.RefundID})
	if err != nil {
		return RefundResult{}, err
	}
	var result struct {
		PaymentID     string `json:"out_trade_no"`
		TransactionID string `json:"trade_no"`
		RefundID      string `json:"out_request_no"`
		Amount        string `json:"refund_amount"`
		Total         string `json:"total_amount"`
		Currency      string `json:"trans_currency"`
		State         string `json:"refund_status"`
	}
	if json.Unmarshal(payload, &result) != nil || result.PaymentID != r.PaymentID || result.TransactionID != r.TransactionID || result.RefundID != r.RefundID || (result.Currency != "" && result.Currency != "CNY") {
		return RefundResult{}, ErrVerification
	}
	amount, err := alipayMinor(result.Amount)
	if err != nil || amount != r.Amount {
		return RefundResult{}, ErrVerification
	}
	if result.Total != "" {
		total, err := alipayMinor(result.Total)
		if err != nil || total != r.Total {
			return RefundResult{}, ErrVerification
		}
	}
	state := "processing"
	switch result.State {
	case "REFUND_SUCCESS":
		state = "succeeded"
	case "":
	case "REFUND_PROCESSING":
	case "REFUND_FAIL":
		state = "failed"
	default:
		return RefundResult{}, ErrVerification
	}
	return RefundResult{PaymentID: r.PaymentID, TransactionID: r.TransactionID, RefundID: r.RefundID, ProviderRefundID: r.RefundID, Amount: amount, Currency: r.Currency, Status: state}, nil
}
