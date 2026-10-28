// Package billfetch imports approved daily bill downloads as observations. No
// downloaded row can settle a payment, execute a refund or change entitlements.
package billfetch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/statements"
	"github.com/wjgppx/xui/internal/store"
)

func downloadOutcome(ctx context.Context, s *store.Store, registry *payments.Registry, j store.BillDownloadClaim) (string, error) {
	allowed, err := s.BillDownloadAllowed(ctx, j.ID, j.ClaimToken)
	if errors.Is(err, store.ErrRoleForbidden) {
		return "actor_changed", nil
	}
	if err != nil {
		return "storage_error", err
	}
	if !allowed {
		return "manual_required", nil
	}
	provider, ok := registry.Get(j.Provider)
	if !ok {
		return "provider_unavailable", nil
	}
	billProvider, ok := provider.(payments.BillProvider)
	if !ok || (j.Provider != "alipay" && j.Provider != "wechat") {
		return "unsupported", nil
	}
	if billProvider.BillMerchantFingerprint() != j.MerchantFingerprint {
		return "merchant_changed", nil
	}
	bill, err := billProvider.DownloadBill(ctx, payments.BillRequest{Date: j.Date, Kind: j.Kind})
	if errors.Is(err, payments.ErrBillNotReady) {
		return "not_ready", nil
	}
	if errors.Is(err, payments.ErrVerification) {
		return "verification_error", nil
	}
	if errors.Is(err, payments.ErrUnsupportedAction) || errors.Is(err, payments.ErrBillUnsupported) {
		return "unsupported", nil
	}
	if err != nil {
		return "gateway_error", nil
	}
	if bill.Provider != j.Provider || bill.Date != j.Date || bill.Kind != j.Kind || bill.MerchantID == "" {
		return "verification_error", nil
	}
	if billProvider.BillMerchantFingerprint() != j.MerchantFingerprint {
		return "merchant_changed", nil
	}
	coverage := "full"
	if j.Provider == "alipay" {
		coverage = "partial"
	}
	normalized, err := statements.ConvertNative(j.Provider, j.Kind, j.Date, coverage, bill.Format, bill.Data, bill.MerchantID)
	if err != nil {
		return "parse_error", nil
	}
	importCtx, importCancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = s.CompleteBillDownload(importCtx, j.ID, j.ClaimToken, normalized)
	importCancel()
	if errors.Is(err, store.ErrRoleForbidden) {
		return "actor_changed", nil
	}
	if err != nil {
		return "storage_error", err
	}
	return "succeeded", nil
}

// Process holds no row lock across provider I/O. Stable job identities and CAS
// completion keep crash recovery from importing under an obsolete claim.
func Process(ctx context.Context, s *store.Store, registry *payments.Registry, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("invalid bill download limit")
	}
	count := 0
	for i := 0; i < limit; i++ {
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, err := s.ClaimBillDownload(claimCtx)
		cancel()
		if errors.Is(err, store.ErrNotFound) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		operationCtx, operationCancel := context.WithTimeout(ctx, 25*time.Second)
		outcome, operationErr := downloadOutcome(operationCtx, s, registry, job)
		operationCancel()
		if err = ctx.Err(); err != nil {
			return count, err
		}
		if outcome != "succeeded" {
			finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
			err = s.FinishBillDownload(finishCtx, job.ID, job.ClaimToken, outcome)
			finishCancel()
			if err != nil && !errors.Is(err, store.ErrStatementConflict) {
				return count, err
			}
		}
		// A storage failure remains retryable under the same job. Only its finite
		// outcome is stored or logged; provider URLs and file contents stay private.
		if operationErr != nil && outcome != "storage_error" {
			return count, operationErr
		}
		count++
	}
	return count, nil
}

func Run(ctx context.Context, s *store.Store, registry *payments.Registry) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := Process(ctx, s, registry, 10); err != nil && ctx.Err() == nil {
			slog.Warn("bill download worker temporarily unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
