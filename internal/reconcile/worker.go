// Package reconcile recovers lost payment notifications using authenticated
// provider queries. It never treats legacy unsigned gateway queries as proof.
package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func queryOutcome(ctx context.Context, s *store.Store, registry *payments.Registry, origin string, job store.PaymentJob) string {
	if job.Provider != "alipay" && job.Provider != "wechat" {
		return "provider_unavailable"
	}
	provider, ok := registry.Get(job.Provider)
	if !ok {
		return "provider_unavailable"
	}
	queryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	n, err := provider.Query(queryCtx, payments.Request{PaymentID: job.PaymentID, OrderID: job.OrderID, Name: job.PlanName, Amount: job.Amount, Currency: job.Currency, NotifyURL: origin + "/api/v1/payments/notify/" + job.Provider, ReturnURL: origin + "/?payment_return=1"})
	cancel()
	if errors.Is(err, payments.ErrNotPaid) {
		return "not_paid"
	}
	if errors.Is(err, payments.ErrVerification) {
		return "verification_error"
	}
	if err != nil {
		return "gateway_error"
	}
	if n.PaymentID != job.PaymentID || n.Amount != job.Amount || n.Currency != job.Currency {
		return "verification_error"
	}
	if !n.Paid {
		return "pending"
	}
	// A lease grants permission to perform work, not authority over a payment
	// fact. Even a late query's valid proof must converge with the webhook.
	err = s.SettlePayment(ctx, job.PaymentID, job.Provider, n.TransactionID, n.Amount, n.Currency)
	if errors.Is(err, store.ErrPaymentConflict) || errors.Is(err, store.ErrNotFound) {
		return "conflict"
	}
	if err != nil {
		return "storage_error"
	}
	return "paid"
}

// Process leases one job at a time before network I/O. A crashed process leaves
// an expiring lease; no database row lock is held over a gateway operation.
func Process(ctx context.Context, s *store.Store, registry *payments.Registry, origin string, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("invalid payment reconciliation limit")
	}
	processed := 0
	for i := 0; i < limit; i++ {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, err := s.ClaimPaymentReconciliation(claimCtx)
		cancel()
		if errors.Is(err, store.ErrNotFound) {
			return processed, nil
		}
		if err != nil {
			return processed, err
		}
		outcome := queryOutcome(ctx, s, registry, origin, job)
		if err = ctx.Err(); err != nil {
			return processed, err
		}
		finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
		err = s.FinishPaymentReconciliation(finishCtx, job.PaymentID, job.ClaimToken, outcome)
		finishCancel()
		if err != nil && !errors.Is(err, store.ErrPaymentConflict) {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func Run(ctx context.Context, s *store.Store, registry *payments.Registry, origin string) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.SchedulePaymentReconciliation(scanCtx)
		cancel()
		if err == nil {
			_, err = Process(ctx, s, registry, origin, 10)
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("payment reconciliation temporarily unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
