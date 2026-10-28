// Package paymentactions performs approved, durable gateway operations. Only
// signed results bound to the immutable purchase can change financial state.
package paymentactions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func failure(err error) string {
	switch {
	case errors.Is(err, payments.ErrVerification):
		return "verification_error"
	case errors.Is(err, payments.ErrNotPaid):
		return "not_found"
	case errors.Is(err, payments.ErrUnsupportedAction):
		return "unsupported"
	default:
		return "gateway_error"
	}
}
func outcome(ctx context.Context, s *store.Store, registry *payments.Registry, origin string, j store.ActionJob) (string, string) {
	within, err := s.PaymentActionWithinHorizon(ctx, j.ID, j.ClaimToken)
	if err != nil {
		return "storage_error", ""
	}
	if !within {
		return "manual_required", ""
	}
	p, ok := registry.Get(j.Provider)
	if !ok {
		return "provider_unavailable", ""
	}
	action, ok := p.(payments.ActionProvider)
	if !ok || (j.Provider != "alipay" && j.Provider != "wechat") {
		return "unsupported", ""
	}
	if j.Kind == "close" {
		r := payments.Request{PaymentID: j.PaymentID, OrderID: j.OrderID, Amount: j.Amount, Currency: j.Currency, NotifyURL: origin + "/api/v1/payments/notify/" + j.Provider, ReturnURL: origin + "/?payment_return=1"}
		n, err := p.Query(ctx, r)
		if err != nil && !errors.Is(err, payments.ErrNotPaid) {
			return failure(err), ""
		}
		if err == nil {
			if n.PaymentID != j.PaymentID || n.Amount != j.Amount || n.Currency != j.Currency || (n.Paid && n.Closed) {
				return "verification_error", ""
			}
			if n.Paid {
				if err = s.SettlePayment(ctx, j.PaymentID, j.Provider, n.TransactionID, n.Amount, n.Currency); err != nil {
					return "storage_error", ""
				}
				return "paid", ""
			}
			if n.Closed {
				return "closed", ""
			}
		}
		if err = s.MarkPaymentActionSubmitted(ctx, j.ID, j.ClaimToken); err != nil {
			return "storage_error", ""
		}
		closed, err := action.Close(ctx, r)
		if err != nil {
			return failure(err), ""
		}
		if closed.PaymentID != j.PaymentID || closed.Status != "closed" {
			return "verification_error", ""
		}
		return "closed", ""
	}
	r := payments.RefundRequest{PaymentID: j.PaymentID, TransactionID: j.TransactionID, RefundID: j.RefundID, Reason: j.Reason, Amount: j.Amount, Total: j.Amount, Currency: j.Currency}
	result, err := action.QueryRefund(ctx, r)
	if errors.Is(err, payments.ErrNotPaid) {
		// A timeout is never absence. Only a verified explicit refund-not-found
		// response permits submission, with the same durable request ID every time.
		if err = s.MarkPaymentActionSubmitted(ctx, j.ID, j.ClaimToken); err != nil {
			return "storage_error", ""
		}
		result, err = action.Refund(ctx, r)
	}
	if err != nil {
		return failure(err), ""
	}
	if result.PaymentID != j.PaymentID || result.TransactionID != j.TransactionID || result.RefundID != j.RefundID || result.Amount != j.Amount || result.Currency != j.Currency || result.ProviderRefundID == "" {
		return "verification_error", ""
	}
	switch result.Status {
	case "succeeded":
		return "succeeded", result.ProviderRefundID
	case "processing":
		return "pending", ""
	case "failed":
		return "failed", ""
	case "manual_required":
		return "manual_required", ""
	default:
		return "verification_error", ""
	}
}

func Process(ctx context.Context, s *store.Store, registry *payments.Registry, origin string, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("invalid payment action limit")
	}
	count := 0
	for i := 0; i < limit; i++ {
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		j, err := s.ClaimPaymentAction(claimCtx)
		cancel()
		if errors.Is(err, store.ErrNotFound) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		// Three bounded provider calls (query, submit, confirmation) fit inside the
		// 60-second lease with room for the separate database completion transaction.
		operationCtx, operationCancel := context.WithTimeout(ctx, 26*time.Second)
		result, providerID := outcome(operationCtx, s, registry, origin, j)
		operationCancel()
		if err = ctx.Err(); err != nil {
			return count, err
		}
		finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
		err = s.FinishPaymentAction(finishCtx, j.ID, j.ClaimToken, result, providerID)
		finishCancel()
		if err != nil && !errors.Is(err, store.ErrPaymentConflict) {
			return count, err
		}
		count++
	}
	return count, nil
}
func Run(ctx context.Context, s *store.Store, registry *payments.Registry, origin string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := Process(ctx, s, registry, origin, 10); err != nil && ctx.Err() == nil {
			slog.Warn("payment actions temporarily unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
