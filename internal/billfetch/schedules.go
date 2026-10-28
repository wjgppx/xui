package billfetch

import (
	"context"
	"log/slog"
	"time"

	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

// Schedule only enqueues existing download jobs. Merchant identity comes from
// configured adapters; this path never calls a merchant or downloads a file.
func Schedule(ctx context.Context, s *store.Store, registry *payments.Registry, now time.Time, limit int) (int, error) {
	fingerprints := make(map[string]string)
	for _, id := range []string{"alipay", "wechat"} {
		provider, ok := registry.Get(id)
		if !ok {
			continue
		}
		billProvider, ok := provider.(payments.BillProvider)
		if ok {
			fingerprints[id] = billProvider.BillMerchantFingerprint()
		}
	}
	return s.DispatchBillSchedules(ctx, now, fingerprints, limit)
}

// RunSchedules is started only when BILL_DOWNLOADS_ENABLED is true. Keeping it
// separate from file I/O lets slow merchant requests leave scheduling responsive.
func RunSchedules(ctx context.Context, s *store.Store, registry *payments.Registry) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		operationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := Schedule(operationCtx, s, registry, time.Now(), 28)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("bill schedule dispatcher temporarily unavailable")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
