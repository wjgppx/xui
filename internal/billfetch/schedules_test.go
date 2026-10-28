package billfetch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/billfetch"
	"github.com/wjgppx/xui/internal/payments"
	"github.com/wjgppx/xui/internal/store"
)

func scheduleRow(t *testing.T, s *store.Store) store.BillDownloadSchedule {
	t.Helper()
	items, err := s.ListBillSchedules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Provider == "wechat" && item.Kind == "payment" {
			return item
		}
	}
	t.Fatal("seeded schedule absent")
	return store.BillDownloadSchedule{}
}
func setSchedule(t *testing.T, s *store.Store, actor, fp string, enabled bool, runAt string) store.BillDownloadSchedule {
	t.Helper()
	old := scheduleRow(t, s)
	item, err := s.UpdateBillSchedule(context.Background(), actor, store.BillScheduleInput{Provider: old.Provider, Kind: old.Kind, Enabled: enabled, RunAt: runAt, Revision: old.Revision}, fp)
	if err != nil {
		t.Fatal(err)
	}
	return item
}
func scheduleToday() time.Time {
	local := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	return time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, local.Location())
}
func dispatchSchedule(t *testing.T, s *store.Store, r *payments.Registry) {
	t.Helper()
	if n, err := billfetch.Schedule(context.Background(), s, r, scheduleToday(), 28); err != nil || n != 1 {
		t.Fatal("expected one daily job", n, err)
	}
}

func TestScheduledDownloadUsesExistingImportAndReconciliationWithoutFulfillingPayment(t *testing.T) {
	s, actor, payment, date := fixture(t)
	fp := strings.Repeat("a", 64)
	configured := setSchedule(t, s, actor, fp, true, "10:00")
	calls := 0
	r := registry(t, &billProvider{fingerprint: fp, download: func(req payments.BillRequest) (payments.Bill, error) {
		calls++
		if req.Date != date || req.Kind != "payment" {
			t.Fatal("scheduler selected the wrong bill")
		}
		return payments.Bill{Provider: "wechat", Date: req.Date, Kind: req.Kind, Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, req.Date, payment.ID)}, nil
	}})
	dispatchSchedule(t, s, r)
	if calls != 0 {
		t.Fatal("dispatch called merchant")
	}
	if n, err := billfetch.Schedule(context.Background(), s, r, scheduleToday(), 28); err != nil || n != 0 {
		t.Fatal("duplicate dispatch", n, err)
	}
	if process(t, s, r) != 1 || calls != 1 {
		t.Fatal("download not processed exactly once")
	}
	jobs, err := s.ListBillDownloads(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Status != "succeeded" || jobs[0].ScheduleID == nil || *jobs[0].ScheduleID != configured.ID {
		t.Fatal("schedule provenance lost", jobs, err)
	}
	detail, err := s.Statement(context.Background(), *jobs[0].StatementID)
	if err != nil || detail.LatestRun.DifferenceCount != 1 || len(detail.Sources) != 1 {
		t.Fatal("automatic reconciliation not produced", err)
	}
	var status string
	var grants int
	if err = s.DB.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM entitlement_periods) FROM payment_intents WHERE id=$1`, payment.ID).Scan(&status, &grants); err != nil || status != "pending" || grants != 0 {
		t.Fatal("schedule changed financial state", status, grants, err)
	}
}

func TestRevokedScheduledJobDoesNotCallMerchantAndExplicitManualRetryDetachesSchedule(t *testing.T) {
	s, actor, payment, _ := fixture(t)
	fp := strings.Repeat("a", 64)
	setSchedule(t, s, actor, fp, true, "10:00")
	calls := 0
	r := registry(t, &billProvider{fingerprint: fp, download: func(req payments.BillRequest) (payments.Bill, error) {
		calls++
		return payments.Bill{Provider: "wechat", Date: req.Date, Kind: req.Kind, Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, req.Date, payment.ID)}, nil
	}})
	dispatchSchedule(t, s, r)
	setSchedule(t, s, actor, "", false, "10:00")
	if process(t, s, r) != 1 || calls != 0 {
		t.Fatal("revoked schedule called merchant")
	}
	jobs, err := s.ListBillDownloads(context.Background())
	if err != nil || len(jobs) != 1 || jobs[0].Status != "manual_required" || jobs[0].LastOutcome != "schedule_changed" {
		t.Fatal("revocation did not stop job", jobs, err)
	}
	manual, err := s.RequestBillDownload(context.Background(), actor, "wechat", "payment", jobs[0].Date, fp)
	if err != nil || manual.ID != jobs[0].ID || manual.ScheduleID != nil {
		t.Fatal("explicit retry retained revoked automatic authority", manual, err)
	}
	if process(t, s, r) != 1 || calls != 1 {
		t.Fatal("explicit manual retry could not complete")
	}
}

func TestScheduleRevocationDuringDownloadRejectsCompletionAndReenableCannotReviveOldJob(t *testing.T) {
	for _, mutation := range []string{"disable", "disable_reenable", "transfer", "time_edit"} {
		t.Run(mutation, func(t *testing.T) {
			s, actor, payment, _ := fixture(t)
			fp := strings.Repeat("a", 64)
			setSchedule(t, s, actor, fp, true, "10:00")
			other := actor
			if mutation == "transfer" {
				if err := s.DB.QueryRow(context.Background(), `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,'fixture','finance') RETURNING id`, store.ID(), store.ID()+"@example.test").Scan(&other); err != nil {
					t.Fatal(err)
				}
			}
			r := registry(t, &billProvider{fingerprint: fp, download: func(req payments.BillRequest) (payments.Bill, error) {
				switch mutation {
				case "disable", "disable_reenable":
					setSchedule(t, s, actor, "", false, "10:00")
					if mutation == "disable_reenable" {
						setSchedule(t, s, actor, fp, true, "10:00")
					}
				case "transfer":
					setSchedule(t, s, other, fp, true, "10:00")
				case "time_edit":
					setSchedule(t, s, actor, fp, true, "11:30")
				}
				return payments.Bill{Provider: "wechat", Date: req.Date, Kind: req.Kind, Format: "wechat_success_v1", MerchantID: "1900000109", Data: nativeBill(t, req.Date, payment.ID)}, nil
			}})
			dispatchSchedule(t, s, r)
			if process(t, s, r) != 1 {
				t.Fatal("job not processed")
			}
			jobs, err := s.ListBillDownloads(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Fatal(err)
			}
			statements, err := s.ListStatements(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "time_edit" {
				if jobs[0].Status != "succeeded" || len(statements) != 1 {
					t.Fatal("time-only edit revoked unchanged authority")
				}
			} else if jobs[0].Status != "manual_required" || jobs[0].LastOutcome != "schedule_changed" || len(statements) != 0 {
				t.Fatal("revoked automatic authority imported evidence", jobs[0], len(statements))
			}
		})
	}
}

func TestSchedulerRegistryRemovalPausesPlanWithoutMerchantIO(t *testing.T) {
	s, actor, _, _ := fixture(t)
	setSchedule(t, s, actor, strings.Repeat("a", 64), true, "10:00")
	if n, err := billfetch.Schedule(context.Background(), s, nil, scheduleToday(), 28); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if plan := scheduleRow(t, s); plan.Enabled || plan.State != "provider_unavailable" {
		t.Fatal("missing provider did not pause schedule", plan)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := billfetch.Schedule(ctx, s, nil, scheduleToday(), 28); !errors.Is(err, context.Canceled) {
		t.Fatal("scheduler ignored cancellation", err)
	}
}
