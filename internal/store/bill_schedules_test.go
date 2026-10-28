package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/store"
	"github.com/wjgppx/xui/internal/testutil"
)

var scheduleTestZone = time.FixedZone("Asia/Shanghai", 8*3600)

func scheduleTestFind(t *testing.T, s *store.Store, provider, kind string) store.BillDownloadSchedule {
	t.Helper()
	items, err := s.ListBillSchedules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Provider == provider && item.Kind == kind {
			return item
		}
	}
	t.Fatal("schedule missing")
	return store.BillDownloadSchedule{}
}
func scheduleTestEnable(t *testing.T, s *store.Store, actor, provider, kind string) store.BillDownloadSchedule {
	t.Helper()
	old := scheduleTestFind(t, s, provider, kind)
	result, err := s.UpdateBillSchedule(context.Background(), actor, store.BillScheduleInput{Provider: provider, Kind: kind, Enabled: true, RunAt: "10:00", Revision: old.Revision}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func scheduleTestSetDate(t *testing.T, s *store.Store, id, date string) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), `UPDATE bill_download_schedules SET next_bill_date=$2::date WHERE id=$1`, id, date); err != nil {
		t.Fatal(err)
	}
}
func scheduleTestNow() time.Time {
	local := time.Now().In(scheduleTestZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, scheduleTestZone)
}
func TestBillSchedulesDefaultDisabledAndRejectInvalidAuthorityOrRevision(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	items, err := s.ListBillSchedules(ctx)
	if err != nil || len(items) != 4 {
		t.Fatal(items, err)
	}
	for _, item := range items {
		if item.Enabled || item.State != "disabled" || item.Revision != 1 || item.RunAt != "10:00" || item.Timezone != "Asia/Shanghai" || item.AuthorizedBy != nil {
			t.Fatal(item)
		}
	}
	actor := actionAdmin(t, s)
	input := store.BillScheduleInput{Provider: "wechat", Kind: "payment", Enabled: true, RunAt: "10:00", Revision: 1}
	if _, err = s.UpdateBillSchedule(ctx, testutil.User(t, s), input, strings.Repeat("a", 64)); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal(err)
	}
	for _, runAt := range []string{"1:00", "24:00", "10:60", "10:00:00", " 10:00", "10:00\n"} {
		bad := input
		bad.RunAt = runAt
		if _, err = s.UpdateBillSchedule(ctx, actor, bad, strings.Repeat("a", 64)); !errors.Is(err, store.ErrStatementConflict) {
			t.Fatal(runAt, err)
		}
	}
	if _, err = s.UpdateBillSchedule(ctx, actor, input, "caller-controlled"); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal(err)
	}
	configured := scheduleTestEnable(t, s, actor, "wechat", "payment")
	if configured.Revision != 2 || configured.NextBillDate == nil || *configured.NextBillDate != billStoreDate() || configured.AuthorizedBy == nil || *configured.AuthorizedBy != actor {
		t.Fatal(configured)
	}
	if _, err = s.UpdateBillSchedule(ctx, actor, input, strings.Repeat("a", 64)); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("stale write accepted", err)
	}
	b, _ := json.Marshal(configured)
	for _, secret := range []string{"merchant_fingerprint", "actor_version", "authorization_generation", strings.Repeat("a", 64)} {
		if strings.Contains(string(b), secret) {
			t.Fatal("private authority leaked")
		}
	}
}
func TestBillSchedulesUseShanghaiBusinessDateAndExactDailyBoundary(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	item := scheduleTestEnable(t, s, actor, "wechat", "payment")
	scheduleTestSetDate(t, s, item.ID, "2026-12-31")
	before := time.Date(2027, 1, 1, 1, 59, 59, 0, time.UTC)
	registry := map[string]string{"wechat": strings.Repeat("a", 64)}
	if count, err := s.DispatchBillSchedules(ctx, before, registry, 10); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if count, err := s.DispatchBillSchedules(ctx, before.Add(time.Second), registry, 10); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Date != "2026-12-31" {
		t.Fatal(jobs, err)
	}
	current := scheduleTestFind(t, s, "wechat", "payment")
	if current.NextBillDate == nil || *current.NextBillDate != "2027-01-01" || current.LastJobID == nil || *current.LastJobID != jobs[0].ID {
		t.Fatal(current)
	}
	if count, err := s.DispatchBillSchedules(ctx, before.Add(12*time.Hour), registry, 10); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestBillSchedulesConcurrentDispatchersAdvanceOneCursorAndCreateOneJob(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	scheduleTestEnable(t, s, actor, "wechat", "refund")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": strings.Repeat("a", 64)}, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "refund" {
		t.Fatal(jobs, err)
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='statement.schedule_dispatched'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestBillSchedulesCatchupIsBoundedAndBatchLimitPreservesCursor(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	item := scheduleTestEnable(t, s, actor, "alipay", "payment")
	scheduleTestSetDate(t, s, item.ID, "2026-09-01")
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, scheduleTestZone)
	registry := map[string]string{"alipay": strings.Repeat("a", 64)}
	if count, err := s.DispatchBillSchedules(ctx, now, registry, 3); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	current := scheduleTestFind(t, s, "alipay", "payment")
	if current.NextBillDate == nil || *current.NextBillDate != "2026-10-24" {
		t.Fatal(current)
	}
	if count, err := s.DispatchBillSchedules(ctx, now, registry, 10); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 7 {
		t.Fatal(jobs, err)
	}
	for _, job := range jobs {
		if job.Date < "2026-10-21" || job.Date > "2026-10-27" {
			t.Fatal(job)
		}
	}
	var count int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='statement.schedule_catchup_truncated'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if count, err := s.DispatchBillSchedules(ctx, now, registry, 0); !errors.Is(err, store.ErrStatementConflict) || count != 0 {
		t.Fatal(count, err)
	}
}
func TestBillSchedulesNeverRestartOrTakeOwnershipOfManualJob(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	job := billStoreQueue(t, s, actor, "wechat", strings.Repeat("a", 64))
	claim := billStoreClaim(t, s)
	if err := s.FinishBillDownload(ctx, claim.ID, claim.ClaimToken, "parse_error"); err != nil {
		t.Fatal(err)
	}
	scheduleTestEnable(t, s, actor, "wechat", "payment")
	if count, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": strings.Repeat("a", 64)}, 10); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Status != "manual_required" || jobs[0].Attempts != 1 {
		t.Fatal(jobs, err)
	}
	var scheduleID *string
	if err = s.DB.QueryRow(ctx, `SELECT schedule_id FROM bill_download_jobs WHERE id=$1`, job.ID).Scan(&scheduleID); err != nil || scheduleID != nil {
		t.Fatal("manual authorization stolen", err)
	}
}

func TestBillSchedulesNewMerchantStartsYesterdayWithoutInheritingPreviousCursor(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	scheduleTestEnable(t, s, actor, "wechat", "payment")
	firstFP, secondFP := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if n, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": firstFP}, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	old := scheduleTestFind(t, s, "wechat", "payment")
	updated, err := s.UpdateBillSchedule(ctx, actor, store.BillScheduleInput{Provider: "wechat", Kind: "payment", Enabled: true, RunAt: "10:00", Revision: old.Revision}, secondFP)
	if err != nil || updated.NextBillDate == nil || *updated.NextBillDate != billStoreDate() {
		t.Fatal("new merchant inherited old cursor", updated, err)
	}
	if n, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": secondFP}, 10); err != nil || n != 1 {
		t.Fatal("new merchant's yesterday bill not dispatched", n, err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 2 || jobs[0].Date != billStoreDate() || jobs[1].Date != billStoreDate() || jobs[0].ID == jobs[1].ID {
		t.Fatal("merchant bill scopes collided", jobs, err)
	}
}
func TestBillSchedulesPauseWhenActorOrMerchantAuthorizationChanges(t *testing.T) {
	for _, change := range []string{"role", "version", "merchant", "missing"} {
		t.Run(change, func(t *testing.T) {
			s := testutil.Database(t)
			ctx := context.Background()
			actor := actionAdmin(t, s)
			scheduleTestEnable(t, s, actor, "wechat", "payment")
			registry := map[string]string{"wechat": strings.Repeat("a", 64)}
			expected := "actor_changed"
			switch change {
			case "role":
				if _, err := s.DB.Exec(ctx, `UPDATE users SET role='user' WHERE id=$1`, actor); err != nil {
					t.Fatal(err)
				}
			case "version":
				if _, err := s.DB.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, actor); err != nil {
					t.Fatal(err)
				}
			case "merchant":
				registry["wechat"] = strings.Repeat("b", 64)
				expected = "merchant_changed"
			case "missing":
				delete(registry, "wechat")
				expected = "provider_unavailable"
			}
			if count, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), registry, 10); err != nil || count != 0 {
				t.Fatal(count, err)
			}
			current := scheduleTestFind(t, s, "wechat", "payment")
			if current.Enabled || current.State != expected || current.Revision != 3 {
				t.Fatal(current)
			}
			jobs, err := s.ListBillDownloads(ctx)
			if err != nil || len(jobs) != 0 {
				t.Fatal(jobs, err)
			}
		})
	}
}
func TestBillSchedulesTimeEditPreservesPendingAuthorizationWhileDisableRevokes(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	item := scheduleTestEnable(t, s, actor, "wechat", "payment")
	if _, err := s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": strings.Repeat("a", 64)}, 10); err != nil {
		t.Fatal(err)
	}
	before := scheduleTestFind(t, s, "wechat", "payment")
	var generation int64
	if err := s.DB.QueryRow(ctx, `SELECT authorization_generation FROM bill_download_schedules WHERE id=$1`, item.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	edited, err := s.UpdateBillSchedule(ctx, actor, store.BillScheduleInput{Provider: "wechat", Kind: "payment", Enabled: true, RunAt: "11:30", Revision: before.Revision}, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var after int64
	if err = s.DB.QueryRow(ctx, `SELECT authorization_generation FROM bill_download_schedules WHERE id=$1`, item.ID).Scan(&after); err != nil || after != generation || edited.NextBillDate == nil || *edited.NextBillDate != *before.NextBillDate {
		t.Fatal(edited, after, err)
	}
	disabled, err := s.UpdateBillSchedule(ctx, actor, store.BillScheduleInput{Provider: "wechat", Kind: "payment", Enabled: false, RunAt: "11:30", Revision: edited.Revision}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT authorization_generation FROM bill_download_schedules WHERE id=$1`, item.ID).Scan(&after); err != nil || after <= generation || disabled.Enabled {
		t.Fatal(disabled, after, err)
	}
	reenabled := scheduleTestEnable(t, s, actor, "wechat", "payment")
	if reenabled.NextBillDate == nil || *reenabled.NextBillDate != billStoreDate() {
		t.Fatal(reenabled)
	}
}
func TestBillSchedulesAuditFailureRollsBackJobAndCursor(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	item := scheduleTestEnable(t, s, actor, "wechat", "payment")
	_, err := s.DB.Exec(ctx, `CREATE FUNCTION reject_schedule_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='statement.schedule_dispatched' THEN RAISE EXCEPTION 'fixture audit unavailable'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_schedule_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_schedule_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchBillSchedules(ctx, scheduleTestNow(), map[string]string{"wechat": strings.Repeat("a", 64)}, 10); err == nil {
		t.Fatal("audit failure ignored")
	}
	current := scheduleTestFind(t, s, "wechat", "payment")
	if current.NextBillDate == nil || *current.NextBillDate != *item.NextBillDate || current.LastJobID != nil {
		t.Fatal("cursor escaped rollback", current)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatal("job escaped rollback", jobs, err)
	}
}
func TestBillSchedulesMigrationPreservesExistingVersionTwelveJobs(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	job := billStoreQueue(t, s, actor, "wechat", strings.Repeat("a", 64))
	_, err := s.DB.Exec(ctx, `ALTER TABLE bill_download_jobs DROP COLUMN schedule_id,DROP COLUMN schedule_generation;DROP TABLE bill_download_schedules;DELETE FROM schema_migrations WHERE version=13`)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration not idempotent", err)
	}
	jobs, err := s.ListBillDownloads(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Status != "queued" {
		t.Fatal(jobs, err)
	}
	items, err := s.ListBillSchedules(ctx)
	if err != nil || len(items) != 4 {
		t.Fatal(items, err)
	}
}

func TestBillSchedulesSessionVersionCannotAuthorizeAfterConcurrentRevocation(t *testing.T) {
	s := testutil.Database(t)
	ctx := context.Background()
	actor := actionAdmin(t, s)
	input := store.BillScheduleInput{Provider: "wechat", Kind: "payment", Enabled: true, RunAt: "10:00", Revision: 1}
	if _, err := s.DB.Exec(ctx, `UPDATE users SET security_version=security_version+1 WHERE id=$1`, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBillSchedule(ctx, actor, input, strings.Repeat("a", 64), 0); !errors.Is(err, store.ErrRoleForbidden) {
		t.Fatal("obsolete request renewed persistent authority", err)
	}
	current := scheduleTestFind(t, s, "wechat", "payment")
	if current.Enabled || current.Revision != 1 {
		t.Fatal("revoked request changed schedule", current)
	}
	if _, err := s.UpdateBillSchedule(ctx, actor, input, strings.Repeat("a", 64), 1, 1); !errors.Is(err, store.ErrStatementConflict) {
		t.Fatal("ambiguous guard accepted", err)
	}
	configured, err := s.UpdateBillSchedule(ctx, actor, input, strings.Repeat("a", 64), 1)
	if err != nil || !configured.Enabled {
		t.Fatal(configured, err)
	}
	// Trusted in-process callers may omit the session guard; the actor is still
	// locked and its current role/version is captured atomically.
	input.Provider = "alipay"
	if _, err = s.UpdateBillSchedule(ctx, actor, input, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}
