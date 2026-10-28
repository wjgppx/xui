package store

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/statements"
)

// StatementSource describes how observations reached the normalized ledger.
// Integrity authenticates the download transport/content, never payment facts.
type StatementSource struct {
	ID                  string    `json:"id"`
	StatementID         string    `json:"statement_id"`
	Format              string    `json:"format"`
	Kind                string    `json:"kind"`
	Digest              string    `json:"digest"`
	Integrity           string    `json:"integrity"`
	MerchantFingerprint string    `json:"merchant_fingerprint"`
	DownloadJobID       *string   `json:"download_job_id"`
	ImportedBy          string    `json:"imported_by"`
	CreatedAt           time.Time `json:"created_at"`
}
type statementSourceInput struct {
	Format, Kind, Digest, Integrity, MerchantFingerprint string
	DownloadJobID                                        *string
}

const statementSourceColumns = `id,statement_id,source_format,source_kind,digest,integrity,merchant_fingerprint,download_job_id,imported_by,created_at`

var billDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var billDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func statementSources(ctx context.Context, q statementQuery, id string) ([]StatementSource, error) {
	rows, err := q.Query(ctx, `SELECT `+statementSourceColumns+` FROM merchant_statement_sources WHERE statement_id=$1 ORDER BY created_at DESC,id DESC LIMIT 20`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []StatementSource{}
	for rows.Next() {
		var s StatementSource
		if err = rows.Scan(&s.ID, &s.StatementID, &s.Format, &s.Kind, &s.Digest, &s.Integrity, &s.MerchantFingerprint, &s.DownloadJobID, &s.ImportedBy, &s.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, s)
	}
	return items, rows.Err()
}
func addStatementSource(ctx context.Context, tx pgx.Tx, actor, statementID string, input statementSourceInput) (bool, error) {
	if !billDigestPattern.MatchString(input.Digest) {
		return false, ErrStatementConflict
	}
	switch input.Format {
	case "normalized_csv_v1", "alipay_trade_v1", "wechat_success_v1", "wechat_refund_v1":
	default:
		return false, ErrStatementConflict
	}
	if input.Kind == "upload" {
		if input.Integrity != "unverified_upload" || input.DownloadJobID != nil || input.MerchantFingerprint != "" {
			return false, ErrStatementConflict
		}
	} else if input.Kind == "download" {
		if input.DownloadJobID == nil || !billDigestPattern.MatchString(input.MerchantFingerprint) || (input.Integrity != "wechat_signed_sha1" && input.Integrity != "alipay_signed_url") {
			return false, ErrStatementConflict
		}
	} else {
		return false, ErrStatementConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO merchant_statement_sources(id,statement_id,source_format,source_kind,digest,integrity,merchant_fingerprint,download_job_id,imported_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(statement_id,source_format,source_kind,digest,merchant_fingerprint) DO NOTHING`, ID(), statementID, input.Format, input.Kind, input.Digest, input.Integrity, input.MerchantFingerprint, input.DownloadJobID, actor)
	return tag.RowsAffected() == 1, err
}
func (s *Store) ImportNativeStatement(ctx context.Context, actor string, input statements.NativeImport) (StatementDetail, error) {
	result, err := statements.ParseNative(input)
	if err != nil {
		return StatementDetail{}, err
	}
	document, err := statements.Parse(result.Input)
	if err != nil {
		return StatementDetail{}, err
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return StatementDetail{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, -1, false); err != nil {
		return StatementDetail{}, statementError(err)
	}
	detail, err := importStatementDocument(ctx, tx, actor, document, statementSourceInput{Format: result.Format, Kind: "upload", Digest: result.SourceDigest, Integrity: "unverified_upload"})
	if err != nil {
		return detail, statementError(err)
	}
	return detail, statementError(tx.Commit(ctx))
}

type BillDownloadJob struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	Kind          string     `json:"kind"`
	Date          string     `json:"date"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastOutcome   string     `json:"last_outcome"`
	StatementID   *string    `json:"statement_id"`
	RequestedBy   string     `json:"requested_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	CompletedAt   *time.Time `json:"completed_at"`
}
type BillDownloadClaim struct {
	BillDownloadJob
	MerchantFingerprint string `json:"-"`
	ClaimToken          string `json:"-"`
	ActorVersion        int64  `json:"-"`
}

const billDownloadColumns = `id,provider,kind,business_date::text,status,attempts,last_outcome,statement_id,requested_by,created_at,updated_at,CASE WHEN status IN ('queued','waiting') THEN next_attempt_at ELSE NULL END,completed_at`

func billDownloadTargets(j *BillDownloadJob) []any {
	return []any{&j.ID, &j.Provider, &j.Kind, &j.Date, &j.Status, &j.Attempts, &j.LastOutcome, &j.StatementID, &j.RequestedBy, &j.CreatedAt, &j.UpdatedAt, &j.NextAttemptAt, &j.CompletedAt}
}
func scanBillDownload(row pgx.Row) (BillDownloadJob, error) {
	var j BillDownloadJob
	err := row.Scan(billDownloadTargets(&j)...)
	return j, statementError(err)
}
func validBillDownloadDate(value string, now time.Time) bool {
	if !billDatePattern.MatchString(value) {
		return false
	}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	date, err := time.ParseInLocation("2006-01-02", value, zone)
	if err != nil || date.Year() < 2000 || date.Year() > 2099 {
		return false
	}
	local := now.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	return date.Before(today) && !date.Before(today.AddDate(0, 0, -90))
}
func (s *Store) RequestBillDownload(ctx context.Context, actor, provider, kind, date, fingerprint string) (BillDownloadJob, error) {
	if (provider != "alipay" && provider != "wechat") || (kind != "payment" && kind != "refund") || !validBillDownloadDate(date, time.Now()) || !billDigestPattern.MatchString(fingerprint) {
		return BillDownloadJob{}, ErrStatementConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return BillDownloadJob{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, -1, false); err != nil {
		return BillDownloadJob{}, err
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT security_version FROM users WHERE id=$1`, actor).Scan(&version); err != nil {
		return BillDownloadJob{}, err
	}
	// The unique identity retains the merchant configuration that authorized the
	// request. A retry can renew an actor, never the date/provider/fingerprint.
	id := ID()
	tag, err := tx.Exec(ctx, `INSERT INTO bill_download_jobs(id,provider,kind,business_date,merchant_fingerprint,requested_by,actor_version) VALUES($1,$2,$3,$4::date,$5,$6,$7) ON CONFLICT(provider,kind,business_date,merchant_fingerprint) DO NOTHING`, id, provider, kind, date, fingerprint, actor, version)
	if err != nil {
		return BillDownloadJob{}, statementError(err)
	}
	job, err := scanBillDownload(tx.QueryRow(ctx, `SELECT `+billDownloadColumns+` FROM bill_download_jobs WHERE provider=$1 AND kind=$2 AND business_date=$3::date AND merchant_fingerprint=$4 FOR NO KEY UPDATE`, provider, kind, date, fingerprint))
	if err != nil {
		return job, err
	}
	action := ""
	if tag.RowsAffected() == 1 {
		action = "statement.download_requested"
	} else if job.Status == "manual_required" {
		job, err = scanBillDownload(tx.QueryRow(ctx, `UPDATE bill_download_jobs SET status='queued',attempts=0,last_outcome='retry_scheduled',requested_by=$2,actor_version=$3,next_attempt_at=clock_timestamp(),horizon_at=clock_timestamp()+interval '7 days',claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1 RETURNING `+billDownloadColumns, job.ID, actor, version))
		if err != nil {
			return job, err
		}
		action = "statement.download_retried"
	}
	if action != "" {
		if err = actionAudit(ctx, tx, actor, action, job.ID); err != nil {
			return job, err
		}
	}
	return job, statementError(tx.Commit(ctx))
}
func (s *Store) ListBillDownloads(ctx context.Context) ([]BillDownloadJob, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+billDownloadColumns+` FROM bill_download_jobs ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []BillDownloadJob{}
	for rows.Next() {
		var j BillDownloadJob
		if err = rows.Scan(billDownloadTargets(&j)...); err != nil {
			return nil, err
		}
		items = append(items, j)
	}
	return items, rows.Err()
}
func (s *Store) ClaimBillDownload(ctx context.Context) (BillDownloadClaim, error) {
	var j BillDownloadClaim
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
 SELECT id FROM bill_download_jobs WHERE status IN ('queued','waiting','processing') AND next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1
 ),claimed AS (
 UPDATE bill_download_jobs SET status='processing',claim_token=$1,lease_until=clock_timestamp()+interval '60 seconds',updated_at=clock_timestamp() WHERE id IN(SELECT id FROM candidate) RETURNING *
 ) SELECT `+billDownloadColumns+`,merchant_fingerprint,claim_token,actor_version FROM claimed`, ID()).Scan(append(billDownloadTargets(&j.BillDownloadJob), &j.MerchantFingerprint, &j.ClaimToken, &j.ActorVersion)...)
	return j, statementError(err)
}
func (s *Store) BillDownloadAllowed(ctx context.Context, id, token string) (bool, error) {
	var within, actorAllowed bool
	err := s.DB.QueryRow(ctx, `SELECT j.horizon_at>clock_timestamp(),u.role IN ('admin','finance') AND u.security_version=j.actor_version FROM bill_download_jobs j JOIN users u ON u.id=j.requested_by WHERE j.id=$1 AND j.claim_token=$2 AND j.status='processing' AND j.lease_until>clock_timestamp()`, id, token).Scan(&within, &actorAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStatementConflict
	}
	if err != nil {
		return false, err
	}
	if !actorAllowed {
		return false, ErrRoleForbidden
	}
	return within, nil
}
func billDownloadOutcome(outcome string) bool {
	switch outcome {
	case "gateway_error", "verification_error", "parse_error", "unsupported", "provider_unavailable", "merchant_changed", "actor_changed", "not_ready", "manual_required", "storage_error":
		return true
	}
	return false
}
func (s *Store) FinishBillDownload(ctx context.Context, id, token, outcome string) error {
	if !billDownloadOutcome(outcome) {
		return ErrStatementConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var attempts int
	var within bool
	var actor string
	err = tx.QueryRow(ctx, `SELECT attempts,horizon_at>clock_timestamp(),requested_by FROM bill_download_jobs WHERE id=$1 AND claim_token=$2 AND status='processing' AND lease_until>clock_timestamp() FOR NO KEY UPDATE`, id, token).Scan(&attempts, &within, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStatementConflict
	}
	if err != nil {
		return err
	}
	attempts++
	status := "waiting"
	if !within || attempts >= 8 || outcome == "parse_error" || outcome == "unsupported" || outcome == "merchant_changed" || outcome == "actor_changed" || outcome == "manual_required" {
		status = "manual_required"
	}
	delay := 30
	for n := 1; n < attempts && delay < 1800; n++ {
		delay *= 2
	}
	if delay > 1800 {
		delay = 1800
	}
	if _, err = tx.Exec(ctx, `UPDATE bill_download_jobs SET status=$2,attempts=$3,last_outcome=$4,next_attempt_at=clock_timestamp()+make_interval(secs=>$5),claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, status, attempts, outcome, delay); err != nil {
		return err
	}
	if status == "manual_required" {
		if err = actionAudit(ctx, tx, actor, "statement.download_manual_required", id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) CompleteBillDownload(ctx context.Context, id, token string, result statements.NativeResult) (StatementDetail, error) {
	document, err := statements.Parse(result.Input)
	if err != nil {
		return StatementDetail{}, err
	}
	if !billDigestPattern.MatchString(result.SourceDigest) {
		return StatementDetail{}, ErrStatementConflict
	}
	// Discover the immutable request actor before acquiring actor then job locks,
	// the same order used when an authorized user renews a manual-required job.
	var actor string
	var version int64
	err = s.DB.QueryRow(ctx, `SELECT requested_by,actor_version FROM bill_download_jobs WHERE id=$1`, id).Scan(&actor, &version)
	if err != nil {
		return StatementDetail{}, statementError(err)
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return StatementDetail{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, false); err != nil {
		return StatementDetail{}, statementError(err)
	}
	var provider, kind, date, fingerprint, currentActor string
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT provider,kind,business_date::text,merchant_fingerprint,requested_by,actor_version FROM bill_download_jobs WHERE id=$1 AND claim_token=$2 AND status='processing' AND lease_until>clock_timestamp() AND horizon_at>clock_timestamp() FOR NO KEY UPDATE`, id, token).Scan(&provider, &kind, &date, &fingerprint, &currentActor, &currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatementDetail{}, ErrStatementConflict
	}
	if err != nil {
		return StatementDetail{}, statementError(err)
	}
	if actor != currentActor || version != currentVersion || document.Provider != provider || document.Kind != kind || document.Date != date {
		return StatementDetail{}, ErrStatementConflict
	}
	integrity := "alipay_signed_url"
	if provider == "alipay" {
		if result.Format != "alipay_trade_v1" || document.Coverage != "partial" {
			return StatementDetail{}, ErrStatementConflict
		}
	} else {
		format := "wechat_success_v1"
		if kind == "refund" {
			format = "wechat_refund_v1"
		}
		if result.Format != format || document.Coverage != "full" {
			return StatementDetail{}, ErrStatementConflict
		}
		integrity = "wechat_signed_sha1"
	}
	detail, err := importStatementDocument(ctx, tx, actor, document, statementSourceInput{Format: result.Format, Kind: "download", Digest: result.SourceDigest, Integrity: integrity, MerchantFingerprint: fingerprint, DownloadJobID: &id})
	if err != nil {
		return detail, statementError(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE bill_download_jobs SET status='succeeded',last_outcome='succeeded',statement_id=$2,completed_at=clock_timestamp(),claim_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, detail.Statement.ID); err != nil {
		return detail, err
	}
	if err = actionAudit(ctx, tx, actor, "statement.download_completed", id); err != nil {
		return detail, err
	}
	return detail, statementError(tx.Commit(ctx))
}
