package payments

import (
	"context"
	"errors"
)

var ErrUnsupportedAction = errors.New("payment action unsupported")

// ActionProvider is optional. Classic Epay does not implement it: unsigned
// query/refund responses are not proof that money moved or an order closed.
type ActionProvider interface {
	Close(context.Context, Request) (CloseResult, error)
	Refund(context.Context, RefundRequest) (RefundResult, error)
	QueryRefund(context.Context, RefundRequest) (RefundResult, error)
}
type CloseResult struct{ PaymentID, Status string }
type RefundRequest struct {
	PaymentID, TransactionID, RefundID, Reason, Currency string
	Amount, Total                                        int64
}
type RefundResult struct {
	PaymentID, TransactionID, RefundID, ProviderRefundID, Currency, Status string
	Amount                                                                 int64
}
