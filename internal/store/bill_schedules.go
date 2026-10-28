package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrBillScheduleChanged means an automatic job no longer has live authorization.
var ErrBillScheduleChanged = errors.New("bill schedule authorization changed")
var billScheduleTimePattern = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}$`)
var billScheduleZone = time.FixedZone("Asia/Shanghai", 8*60*60)

type BillDownloadSchedule struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	Kind             string    `json:"kind"`
	Enabled          bool      `json:"enabled"`
	RunAt            string    `json:"run_at"`
	Timezone         string    `json:"timezone"`
	Revision         int64     `json:"revision"`
	State            string    `json:"state"`
	NextBillDate     *string   `json:"next_bill_date"`
	LastEnqueuedDate *string   `json:"last_enqueued_date"`
	LastJobID        *string   `json:"last_job_id"`
	AuthorizedBy     *string   `json:"authorized_by"`
	UpdatedAt        time.Time `json:"updated_at"`
}
type BillScheduleInput struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Enabled  bool   `json:"enabled"`
	RunAt    string `json:"run_at"`
	Revision int64  `json:"revision"`
}
type billScheduleRecord struct {
	BillDownloadSchedule
	runMinute    int
	actorVersion *int64
	fingerprint  string
	generation   int64
}

const billScheduleColumns = `id,provider,kind,enabled,run_minute,revision,state,next_bill_date::text,last_enqueued_date::text,last_job_id,authorized_by,updated_at,actor_version,merchant_fingerprint,authorization_generation`

func billScheduleTargets(s *billScheduleRecord) []any {
	return []any{&s.ID, &s.Provider, &s.Kind, &s.Enabled, &s.runMinute, &s.Revision, &s.State, &s.NextBillDate, &s.LastEnqueuedDate, &s.LastJobID, &s.AuthorizedBy, &s.UpdatedAt, &s.actorVersion, &s.fingerprint, &s.generation}
}
func finishBillSchedule(s *billScheduleRecord) {
	s.RunAt = fmt.Sprintf("%02d:%02d", s.runMinute/60, s.runMinute%60)
	s.Timezone = "Asia/Shanghai"
}
func scanBillSchedule(row pgx.Row) (billScheduleRecord, error) {
	var s billScheduleRecord
	err := row.Scan(billScheduleTargets(&s)...)
	finishBillSchedule(&s)
	return s, statementError(err)
}
func (s *Store) ListBillSchedules(ctx context.Context) ([]BillDownloadSchedule, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+billScheduleColumns+` FROM bill_download_schedules ORDER BY provider,kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BillDownloadSchedule{}
	for rows.Next() {
		var item billScheduleRecord
		if err = rows.Scan(billScheduleTargets(&item)...); err != nil {
			return nil, err
		}
		finishBillSchedule(&item)
		out = append(out, item.BillDownloadSchedule)
	}
	return out, rows.Err()
}
func billScheduleMinute(value string) (int, bool) {
	if !billScheduleTimePattern.MatchString(value) {
		return 0, false
	}
	hour, _ := strconv.Atoi(value[:2])
	minute, _ := strconv.Atoi(value[3:])
	return hour*60 + minute, hour < 24 && minute < 60
}
func (s *Store) UpdateBillSchedule(ctx context.Context, actor string, input BillScheduleInput, fingerprint string, expectedVersion ...int64) (BillDownloadSchedule, error) {
	if len(expectedVersion) > 1 || (len(expectedVersion) == 1 && expectedVersion[0] < 0) {
		return BillDownloadSchedule{}, ErrStatementConflict
	}
	versionGuard := int64(-1)
	if len(expectedVersion) == 1 {
		versionGuard = expectedVersion[0]
	}
	minute, valid := billScheduleMinute(input.RunAt)
	if !valid || input.Revision < 1 || (input.Provider != "alipay" && input.Provider != "wechat") || (input.Kind != "payment" && input.Kind != "refund") || (input.Enabled && !billDigestPattern.MatchString(fingerprint)) {
		return BillDownloadSchedule{}, ErrStatementConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return BillDownloadSchedule{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, versionGuard, false); err != nil {
		return BillDownloadSchedule{}, err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT security_version FROM users WHERE id=$1`, actor).Scan(&version); err != nil {
		return BillDownloadSchedule{}, err
	}
	old, err := scanBillSchedule(tx.QueryRow(ctx, `SELECT `+billScheduleColumns+` FROM bill_download_schedules WHERE provider=$1 AND kind=$2 FOR NO KEY UPDATE`, input.Provider, input.Kind))
	if err != nil {
		return BillDownloadSchedule{}, err
	}
	if old.Revision != input.Revision {
		return BillDownloadSchedule{}, ErrStatementConflict
	}
	next := old.NextBillDate
	state := "disabled"
	generation := old.generation + 1
	owner := old.AuthorizedBy
	actorVersion := old.actorVersion
	merchant := old.fingerprint
	if input.Enabled {
		state = "active"
		sameAuthority := old.Enabled && old.AuthorizedBy != nil && *old.AuthorizedBy == actor && old.actorVersion != nil && *old.actorVersion == version && old.fingerprint == fingerprint
		if sameAuthority {
			generation = old.generation
		}
		// A different merchant is a different bill scope. It must not inherit a
		// cursor already advanced by the previous merchant's completed dates.
		if !old.Enabled || next == nil || old.fingerprint != fingerprint {
			date := time.Now().In(billScheduleZone).AddDate(0, 0, -1).Format("2006-01-02")
			next = &date
		}
		owner = &actor
		actorVersion = &version
		merchant = fingerprint
	}
	updated, err := scanBillSchedule(tx.QueryRow(ctx, `UPDATE bill_download_schedules SET enabled=$2,run_minute=$3,revision=revision+1,state=$4,next_bill_date=$5::date,authorized_by=$6,actor_version=$7,merchant_fingerprint=$8,authorization_generation=$9,updated_at=clock_timestamp() WHERE id=$1 RETURNING `+billScheduleColumns, old.ID, input.Enabled, minute, state, next, owner, actorVersion, merchant, generation))
	if err != nil {
		return BillDownloadSchedule{}, err
	}
	action := "statement.schedule_configured"
	if !input.Enabled {
		action = "statement.schedule_disabled"
	}
	if err = actionAudit(ctx, tx, actor, action, old.ID); err != nil {
		return BillDownloadSchedule{}, err
	}
	return updated.BillDownloadSchedule, statementError(tx.Commit(ctx))
}

// DispatchBillSchedules queues recent completed business days without calling a
// merchant. A date cursor, stable job identity and audit commit together.
// The returned count is business dates handled, including existing manual jobs.
func (s *Store) DispatchBillSchedules(ctx context.Context, now time.Time, fingerprints map[string]string, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrStatementConflict
	}
	schedules, err := s.ListBillSchedules(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, schedule := range schedules {
		if !schedule.Enabled {
			continue
		}
		for count < limit {
			handled, err := s.dispatchBillSchedule(ctx, schedule.ID, now, fingerprints)
			if err != nil {
				return count, err
			}
			if !handled {
				break
			}
			count++
		}
		if count >= limit {
			break
		}
	}
	return count, nil
}
func (s *Store) dispatchBillSchedule(ctx context.Context, id string, now time.Time, fingerprints map[string]string) (bool, error) {
	// Discover the owner before locking so every mutation uses actor->schedule->job.
	initial, err := scanBillSchedule(s.DB.QueryRow(ctx, `SELECT `+billScheduleColumns+` FROM bill_download_schedules WHERE id=$1`, id))
	if err != nil {
		return false, err
	}
	if !initial.Enabled || initial.AuthorizedBy == nil {
		return false, nil
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var role string
	var version int64
	if err = tx.QueryRow(ctx, `SELECT role,security_version FROM users WHERE id=$1 FOR SHARE`, *initial.AuthorizedBy).Scan(&role, &version); err != nil {
		return false, err
	}
	current, err := scanBillSchedule(tx.QueryRow(ctx, `SELECT `+billScheduleColumns+` FROM bill_download_schedules WHERE id=$1 FOR NO KEY UPDATE`, id))
	if err != nil {
		return false, err
	}
	if !current.Enabled || current.Revision != initial.Revision || current.AuthorizedBy == nil || *current.AuthorizedBy != *initial.AuthorizedBy {
		return false, nil
	}
	pause := ""
	if (role != "admin" && role != "finance") || current.actorVersion == nil || *current.actorVersion != version {
		pause = "actor_changed"
	} else if fp, ok := fingerprints[current.Provider]; !ok || !billDigestPattern.MatchString(fp) {
		pause = "provider_unavailable"
	} else if fp != current.fingerprint {
		pause = "merchant_changed"
	}
	if pause != "" {
		if _, err = tx.Exec(ctx, `UPDATE bill_download_schedules SET enabled=false,state=$2,revision=revision+1,authorization_generation=authorization_generation+1,updated_at=clock_timestamp() WHERE id=$1`, id, pause); err != nil {
			return false, err
		}
		if err = actionAudit(ctx, tx, *current.AuthorizedBy, "statement.schedule_paused", id); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	local := now.In(billScheduleZone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, billScheduleZone)
	if current.NextBillDate == nil {
		return false, ErrStatementConflict
	}
	date, err := time.ParseInLocation("2006-01-02", *current.NextBillDate, billScheduleZone)
	if err != nil {
		return false, ErrStatementConflict
	}
	earliest := today.AddDate(0, 0, -7)
	truncated := date.Before(earliest)
	if truncated {
		date = earliest
	}
	due := date.AddDate(0, 0, 1).Add(time.Duration(current.runMinute) * time.Minute)
	if !date.Before(today) || now.Before(due) {
		return false, nil
	}
	if truncated {
		if err = actionAudit(ctx, tx, *current.AuthorizedBy, "statement.schedule_catchup_truncated", id); err != nil {
			return false, err
		}
	}
	businessDate := date.Format("2006-01-02")
	_, err = tx.Exec(ctx, `INSERT INTO bill_download_jobs(id,provider,kind,business_date,merchant_fingerprint,requested_by,actor_version,schedule_id,schedule_generation) VALUES($1,$2,$3,$4::date,$5,$6,$7,$8,$9) ON CONFLICT(provider,kind,business_date,merchant_fingerprint) DO NOTHING`, ID(), current.Provider, current.Kind, businessDate, current.fingerprint, *current.AuthorizedBy, version, id, current.generation)
	if err != nil {
		return false, statementError(err)
	}
	var jobID string
	if err = tx.QueryRow(ctx, `SELECT id FROM bill_download_jobs WHERE provider=$1 AND kind=$2 AND business_date=$3::date AND merchant_fingerprint=$4`, current.Provider, current.Kind, businessDate, current.fingerprint).Scan(&jobID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE bill_download_schedules SET next_bill_date=$2::date,last_enqueued_date=$3::date,last_job_id=$4,updated_at=clock_timestamp() WHERE id=$1`, id, date.AddDate(0, 0, 1).Format("2006-01-02"), businessDate, jobID); err != nil {
		return false, err
	}
	if err = actionAudit(ctx, tx, *current.AuthorizedBy, "statement.schedule_dispatched", jobID); err != nil {
		return false, err
	}
	return true, statementError(tx.Commit(ctx))
}
