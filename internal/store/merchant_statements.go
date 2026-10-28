package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wjgppx/xui/internal/statements"
)

var ErrStatementConflict = errors.New("statement state, bounds or concurrent report conflict")

type Statement struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	Kind       string    `json:"kind"`
	Date       string    `json:"date"`
	Coverage   string    `json:"coverage"`
	Digest     string    `json:"digest"`
	ImportedBy string    `json:"imported_by"`
	RowCount   int       `json:"row_count"`
	Total      int64     `json:"total_minor,string"`
	CreatedAt  time.Time `json:"created_at"`
}
type StatementRun struct {
	ID                  string    `json:"id"`
	StatementID         string    `json:"statement_id"`
	Revision            int       `json:"revision"`
	MatchedCount        int       `json:"matched_count"`
	DifferenceCount     int       `json:"difference_count"`
	LocalCandidateCount int       `json:"local_candidate_count"`
	AsOf                time.Time `json:"as_of"`
	CreatedBy           string    `json:"created_by"`
}
type StatementDetail struct {
	Statement Statement      `json:"statement"`
	LatestRun StatementRun   `json:"latest_run"`
	Runs      []StatementRun `json:"runs"`
}
type StatementSummary struct {
	Statement
	LatestRun StatementRun `json:"latest_run"`
}
type StatementReview struct {
	ID        string    `json:"id"`
	FindingID string    `json:"finding_id"`
	ActorID   string    `json:"actor_id"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}
type StatementFinding struct {
	ID                        string           `json:"id"`
	RunID                     string           `json:"run_id"`
	RowNumber                 *int             `json:"row_number"`
	PaymentID                 string           `json:"payment_id"`
	RefundID                  string           `json:"refund_id"`
	Codes                     []string         `json:"codes"`
	StatementTransactionID    string           `json:"statement_transaction_id"`
	LocalTransactionID        string           `json:"local_transaction_id"`
	StatementProviderRefundID string           `json:"statement_provider_refund_id"`
	LocalProviderRefundID     string           `json:"local_provider_refund_id"`
	StatementAmount           *int64           `json:"statement_amount_minor,string"`
	LocalAmount               *int64           `json:"local_amount_minor,string"`
	LocalProvider             string           `json:"local_provider"`
	LocalStatus               string           `json:"local_status"`
	LocalConfirmedAt          *time.Time       `json:"local_confirmed_at"`
	StatementOccurredAt       *time.Time       `json:"statement_occurred_at"`
	ReviewCount               int              `json:"review_count"`
	LatestReview              *StatementReview `json:"latest_review"`
}
type StatementFindingsPage struct {
	Items  []StatementFinding `json:"items"`
	Total  int                `json:"total"`
	Offset int                `json:"offset"`
	Limit  int                `json:"limit"`
}

const statementColumns = `id,provider,kind,business_date::text,coverage,digest,imported_by,row_count,total_minor,created_at`
const statementRunColumns = `id,statement_id,revision,matched_count,difference_count,local_candidate_count,as_of,created_by`
const statementReviewColumns = `id,finding_id,actor_id,note,created_at`
const statementFindingColumns = `id,run_id,row_number,payment_id,refund_id,codes,statement_transaction_id,local_transaction_id,statement_provider_refund_id,local_provider_refund_id,statement_amount_minor,local_amount_minor,local_provider,local_status,local_confirmed_at,statement_occurred_at`

type statementQuery interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func statementTargets(s *Statement) []any {
	return []any{&s.ID, &s.Provider, &s.Kind, &s.Date, &s.Coverage, &s.Digest, &s.ImportedBy, &s.RowCount, &s.Total, &s.CreatedAt}
}
func statementRunTargets(r *StatementRun) []any {
	return []any{&r.ID, &r.StatementID, &r.Revision, &r.MatchedCount, &r.DifferenceCount, &r.LocalCandidateCount, &r.AsOf, &r.CreatedBy}
}
func statementFindingTargets(f *StatementFinding) []any {
	return []any{&f.ID, &f.RunID, &f.RowNumber, &f.PaymentID, &f.RefundID, &f.Codes, &f.StatementTransactionID, &f.LocalTransactionID, &f.StatementProviderRefundID, &f.LocalProviderRefundID, &f.StatementAmount, &f.LocalAmount, &f.LocalProvider, &f.LocalStatus, &f.LocalConfirmedAt, &f.StatementOccurredAt}
}
func scanStatement(row pgx.Row) (Statement, error) {
	var s Statement
	err := row.Scan(statementTargets(&s)...)
	return s, statementError(err)
}
func scanStatementReview(row pgx.Row) (StatementReview, error) {
	var r StatementReview
	err := row.Scan(&r.ID, &r.FindingID, &r.ActorID, &r.Note, &r.CreatedAt)
	return r, statementError(err)
}
func statementError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "40001" || pg.Code == "40P01" || pg.Code == "23505") {
		return ErrStatementConflict
	}
	return err
}
func statementDetail(ctx context.Context, q statementQuery, id string) (StatementDetail, error) {
	var d StatementDetail
	s, err := scanStatement(q.QueryRow(ctx, `SELECT `+statementColumns+` FROM merchant_statements WHERE id=$1`, id))
	if err != nil {
		return d, err
	}
	d.Statement = s
	rows, err := q.Query(ctx, `SELECT `+statementRunColumns+` FROM merchant_statement_runs WHERE statement_id=$1 ORDER BY revision DESC LIMIT 20`, id)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	d.Runs = []StatementRun{}
	for rows.Next() {
		var r StatementRun
		if err = rows.Scan(statementRunTargets(&r)...); err != nil {
			return d, err
		}
		d.Runs = append(d.Runs, r)
	}
	if err = rows.Err(); err != nil {
		return d, err
	}
	if len(d.Runs) == 0 {
		return d, ErrNotFound
	}
	d.LatestRun = d.Runs[0]
	return d, nil
}
func (s *Store) Statement(ctx context.Context, id string) (StatementDetail, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return StatementDetail{}, err
	}
	defer tx.Rollback(ctx)
	d, err := statementDetail(ctx, tx, id)
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}
func (s *Store) ListStatements(ctx context.Context) ([]StatementSummary, error) {
	// The list and its latest-run summary use one MVCC snapshot in one SELECT.
	prefixed := func(cols, alias string) string { return alias + "." + strings.ReplaceAll(cols, ",", ","+alias+".") }
	rows, err := s.DB.Query(ctx, `SELECT `+prefixed(statementColumns, "s")+`,`+prefixed(statementRunColumns, "r")+` FROM merchant_statements s JOIN LATERAL (SELECT * FROM merchant_statement_runs WHERE statement_id=s.id ORDER BY revision DESC LIMIT 1) r ON true ORDER BY s.created_at DESC,s.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatementSummary{}
	for rows.Next() {
		var item StatementSummary
		if err = rows.Scan(append(statementTargets(&item.Statement), statementRunTargets(&item.LatestRun)...)...); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) ImportStatement(ctx context.Context, actor string, input statements.Import) (StatementDetail, error) {
	document, err := statements.Parse(input)
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
	existing, err := scanStatement(tx.QueryRow(ctx, `SELECT `+statementColumns+` FROM merchant_statements WHERE provider=$1 AND kind=$2 AND business_date=$3::date AND coverage=$4 AND digest=$5`, document.Provider, document.Kind, document.Date, document.Coverage, document.Digest))
	if err == nil {
		d, e := statementDetail(ctx, tx, existing.ID)
		if e != nil {
			return d, e
		}
		return d, statementError(tx.Commit(ctx))
	}
	if !errors.Is(err, ErrNotFound) {
		return StatementDetail{}, err
	}
	id := ID()
	_, err = tx.Exec(ctx, `INSERT INTO merchant_statements(id,provider,kind,business_date,coverage,digest,imported_by,row_count,total_minor) VALUES($1,$2,$3,$4::date,$5,$6,$7,$8,$9)`, id, document.Provider, document.Kind, document.Date, document.Coverage, document.Digest, actor, len(document.Rows), document.Total)
	if err != nil {
		return StatementDetail{}, statementError(err)
	}
	sourceRows := make([][]any, 0, len(document.Rows))
	for i, row := range document.Rows {
		sourceRows = append(sourceRows, []any{id, i + 1, row.PaymentID, row.TransactionID, row.RefundID, row.ProviderRefundID, row.Amount, row.Currency, row.OccurredAt})
	}
	if len(sourceRows) > 0 {
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"merchant_statement_rows"}, []string{"statement_id", "row_number", "payment_id", "transaction_id", "refund_id", "provider_refund_id", "amount_minor", "currency", "occurred_at"}, pgx.CopyFromRows(sourceRows))
		if err != nil {
			return StatementDetail{}, statementError(err)
		}
	}
	if _, err = statementReconcile(ctx, tx, actor, id, document); err != nil {
		return StatementDetail{}, statementError(err)
	}
	if err = actionAudit(ctx, tx, actor, "statement.imported", id); err != nil {
		return StatementDetail{}, err
	}
	d, err := statementDetail(ctx, tx, id)
	if err != nil {
		return d, err
	}
	return d, statementError(tx.Commit(ctx))
}
func (s *Store) ReconcileStatement(ctx context.Context, actor, id string) (StatementRun, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return StatementRun{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, -1, false); err != nil {
		return StatementRun{}, statementError(err)
	}
	// Only the immutable source row is locked. No payment, order or refund row
	// is locked or updated by reconciliation, so webhooks can keep committing.
	source, err := scanStatement(tx.QueryRow(ctx, `SELECT `+statementColumns+` FROM merchant_statements WHERE id=$1 FOR NO KEY UPDATE`, id))
	if err != nil {
		return StatementRun{}, err
	}
	document := statements.Document{Provider: source.Provider, Kind: source.Kind, Date: source.Date, Coverage: source.Coverage, Digest: source.Digest, Total: source.Total, Rows: []statements.Row{}}
	rows, err := tx.Query(ctx, `SELECT payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at FROM merchant_statement_rows WHERE statement_id=$1 ORDER BY row_number`, id)
	if err != nil {
		return StatementRun{}, err
	}
	for rows.Next() {
		var row statements.Row
		if err = rows.Scan(&row.PaymentID, &row.TransactionID, &row.RefundID, &row.ProviderRefundID, &row.Amount, &row.Currency, &row.OccurredAt); err != nil {
			rows.Close()
			return StatementRun{}, err
		}
		document.Rows = append(document.Rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return StatementRun{}, err
	}
	run, err := statementReconcile(ctx, tx, actor, id, document)
	if err != nil {
		return run, statementError(err)
	}
	if err = actionAudit(ctx, tx, actor, "statement.reconciled", run.ID); err != nil {
		return run, err
	}
	return run, statementError(tx.Commit(ctx))
}

type statementLocal struct {
	ID, PaymentID, Provider, TransactionID, ProviderRefundID, Currency, Status string
	Amount                                                                     int64
	ConfirmedAt                                                                *time.Time
}

func statementLocals(ctx context.Context, tx pgx.Tx, d statements.Document) (map[string]statementLocal, []statementLocal, error) {
	ids := make([]string, 0, len(d.Rows))
	for _, row := range d.Rows {
		if d.Kind == "payment" {
			ids = append(ids, row.PaymentID)
		} else {
			ids = append(ids, row.RefundID)
		}
	}
	columns := `p.id,p.id,p.provider,COALESCE(p.transaction_id,''),'',o.currency,p.status,o.price_minor,p.paid_at`
	base := ` FROM payment_intents p JOIN orders o ON o.id=p.order_id`
	idExpr, providerExpr, statusExpr, timeExpr := "p.id", "p.provider", "p.status='paid'", "p.paid_at"
	if d.Kind == "refund" {
		columns = `r.id,r.payment_id,r.provider,r.transaction_id,COALESCE(r.provider_refund_id,''),r.currency,r.status,r.amount_minor,r.completed_at`
		base = ` FROM payment_refunds r`
		idExpr, providerExpr, statusExpr, timeExpr = "r.id", "r.provider", "r.status='succeeded'", "r.completed_at"
	}
	result := map[string]statementLocal{}
	read := func(query string, args ...any) ([]statementLocal, error) {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []statementLocal{}
		for rows.Next() {
			var local statementLocal
			if err = rows.Scan(&local.ID, &local.PaymentID, &local.Provider, &local.TransactionID, &local.ProviderRefundID, &local.Currency, &local.Status, &local.Amount, &local.ConfirmedAt); err != nil {
				return nil, err
			}
			items = append(items, local)
		}
		return items, rows.Err()
	}
	if len(ids) > 0 {
		items, err := read(`SELECT `+columns+base+` WHERE `+idExpr+`=ANY($1::text[])`, ids)
		if err != nil {
			return nil, nil, err
		}
		for _, local := range items {
			result[local.ID] = local
		}
	}
	reverse := []statementLocal{}
	if d.Coverage == "full" {
		start, err := time.ParseInLocation("2006-01-02", d.Date, time.FixedZone("Asia/Shanghai", 8*60*60))
		if err != nil {
			return nil, nil, ErrStatementConflict
		}
		reverse, err = read(`SELECT `+columns+base+` WHERE `+providerExpr+`=$1 AND `+statusExpr+` AND `+timeExpr+`>=$2 AND `+timeExpr+`<$3 ORDER BY `+idExpr+` LIMIT 2001`, d.Provider, start, start.Add(24*time.Hour))
		if err != nil {
			return nil, nil, err
		}
		if len(reverse) > statements.MaxRows {
			return nil, nil, ErrStatementConflict
		}
	}
	return result, reverse, nil
}
func statementReconcile(ctx context.Context, tx pgx.Tx, actor, id string, d statements.Document) (StatementRun, error) {
	run := StatementRun{ID: ID(), StatementID: id, CreatedBy: actor}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1,transaction_timestamp() FROM merchant_statement_runs WHERE statement_id=$1`, id).Scan(&run.Revision, &run.AsOf); err != nil {
		return run, err
	}
	locals, reverse, err := statementLocals(ctx, tx, d)
	if err != nil {
		return run, err
	}
	findings := []StatementFinding{}
	present := map[string]bool{}
	for i, row := range d.Rows {
		key := row.PaymentID
		if d.Kind == "refund" {
			key = row.RefundID
		}
		present[key] = true
		number := i + 1
		amount, occurred := row.Amount, row.OccurredAt
		finding := StatementFinding{ID: ID(), RunID: run.ID, RowNumber: &number, PaymentID: row.PaymentID, RefundID: row.RefundID, Codes: []string{}, StatementTransactionID: row.TransactionID, StatementProviderRefundID: row.ProviderRefundID, StatementAmount: &amount, StatementOccurredAt: &occurred}
		local, ok := locals[key]
		if !ok {
			finding.Codes = append(finding.Codes, "missing_local")
		} else {
			localAmount := local.Amount
			finding.LocalAmount = &localAmount
			finding.LocalProvider = local.Provider
			finding.LocalStatus = local.Status
			finding.LocalTransactionID = local.TransactionID
			finding.LocalProviderRefundID = local.ProviderRefundID
			finding.LocalConfirmedAt = local.ConfirmedAt
			if local.Provider != d.Provider {
				finding.Codes = append(finding.Codes, "provider_mismatch")
			}
			succeeded := local.Status == "paid"
			if d.Kind == "refund" {
				succeeded = local.Status == "succeeded"
			}
			if local.PaymentID != row.PaymentID || ((local.TransactionID != "" || succeeded) && local.TransactionID != row.TransactionID) || (d.Kind == "refund" && (local.ProviderRefundID != "" || succeeded) && local.ProviderRefundID != row.ProviderRefundID) {
				finding.Codes = append(finding.Codes, "identity_mismatch")
			}
			if local.Amount != row.Amount || local.Currency != row.Currency {
				finding.Codes = append(finding.Codes, "amount_mismatch")
			}
			if !succeeded {
				finding.Codes = append(finding.Codes, "not_confirmed")
			}
		}
		if len(finding.Codes) == 0 {
			run.MatchedCount++
		} else {
			findings = append(findings, finding)
		}
	}
	for _, local := range reverse {
		if present[local.ID] {
			continue
		}
		amount := local.Amount
		refundID := ""
		if d.Kind == "refund" {
			refundID = local.ID
		}
		findings = append(findings, StatementFinding{ID: ID(), RunID: run.ID, PaymentID: local.PaymentID, RefundID: refundID, Codes: []string{"local_only"}, LocalTransactionID: local.TransactionID, LocalProviderRefundID: local.ProviderRefundID, LocalAmount: &amount, LocalProvider: local.Provider, LocalStatus: local.Status, LocalConfirmedAt: local.ConfirmedAt})
		run.LocalCandidateCount++
	}
	run.DifferenceCount = len(findings)
	if run.DifferenceCount > 2*statements.MaxRows {
		return run, ErrStatementConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO merchant_statement_runs(id,statement_id,revision,matched_count,difference_count,local_candidate_count,as_of,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, run.ID, id, run.Revision, run.MatchedCount, run.DifferenceCount, run.LocalCandidateCount, run.AsOf, actor); err != nil {
		return run, err
	}
	if len(findings) > 0 {
		data := make([][]any, 0, len(findings))
		for _, f := range findings {
			data = append(data, []any{f.ID, f.RunID, f.RowNumber, f.PaymentID, f.RefundID, f.Codes, f.StatementTransactionID, f.LocalTransactionID, f.StatementProviderRefundID, f.LocalProviderRefundID, f.StatementAmount, f.LocalAmount, f.LocalProvider, f.LocalStatus, f.LocalConfirmedAt, f.StatementOccurredAt})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"merchant_statement_findings"}, strings.Split(statementFindingColumns, ","), pgx.CopyFromRows(data))
		if err != nil {
			return run, err
		}
	}
	return run, nil
}

func (s *Store) StatementFindings(ctx context.Context, statementID, runID, review string, offset, limit int) (StatementFindingsPage, error) {
	page := StatementFindingsPage{Items: []StatementFinding{}, Offset: offset, Limit: limit}
	if limit == 0 {
		limit = 100
		page.Limit = limit
	}
	if offset < 0 || limit < 1 || limit > 100 || (review != "all" && review != "unreviewed" && review != "reviewed") {
		return page, ErrStatementConflict
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merchant_statement_runs WHERE id=$1 AND statement_id=$2)`, runID, statementID).Scan(&exists); err != nil {
		return page, err
	}
	if !exists {
		return page, ErrNotFound
	}
	where := `f.run_id=$1 AND ($2='all' OR ($2='reviewed')=EXISTS(SELECT 1 FROM merchant_statement_reviews v WHERE v.finding_id=f.id))`
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM merchant_statement_findings f WHERE `+where, runID, review).Scan(&page.Total); err != nil {
		return page, err
	}
	prefixed := "f." + strings.ReplaceAll(statementFindingColumns, ",", ",f.")
	rows, err := tx.Query(ctx, `SELECT `+prefixed+`,(SELECT count(*) FROM merchant_statement_reviews v WHERE v.finding_id=f.id),v.id,v.actor_id,v.note,v.created_at FROM merchant_statement_findings f LEFT JOIN LATERAL (SELECT id,actor_id,note,created_at FROM merchant_statement_reviews WHERE finding_id=f.id ORDER BY created_at DESC,id DESC LIMIT 1) v ON true WHERE `+where+` ORDER BY f.row_number NULLS LAST,f.payment_id,f.refund_id,f.id OFFSET $3 LIMIT $4`, runID, review, offset, limit)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var f StatementFinding
		var reviewID, actorID, note *string
		var createdAt *time.Time
		if err = rows.Scan(append(statementFindingTargets(&f), &f.ReviewCount, &reviewID, &actorID, &note, &createdAt)...); err != nil {
			rows.Close()
			return page, err
		}
		if reviewID != nil {
			f.LatestReview = &StatementReview{ID: *reviewID, FindingID: f.ID, ActorID: *actorID, Note: *note, CreatedAt: *createdAt}
		}
		page.Items = append(page.Items, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	return page, tx.Commit(ctx)
}
func (s *Store) ReviewStatementFinding(ctx context.Context, actor, findingID, note string) (StatementReview, error) {
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 1000 || !utf8.ValidString(note) || strings.IndexFunc(note, unicode.IsControl) >= 0 {
		return StatementReview{}, ErrStatementConflict
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return StatementReview{}, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, -1, false); err != nil {
		return StatementReview{}, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merchant_statement_findings WHERE id=$1)`, findingID).Scan(&exists); err != nil {
		return StatementReview{}, err
	}
	if !exists {
		return StatementReview{}, ErrNotFound
	}
	review, err := scanStatementReview(tx.QueryRow(ctx, `INSERT INTO merchant_statement_reviews(id,finding_id,actor_id,note) VALUES($1,$2,$3,$4) ON CONFLICT(finding_id,actor_id,note) DO NOTHING RETURNING `+statementReviewColumns, ID(), findingID, actor, note))
	if errors.Is(err, ErrNotFound) {
		review, err = scanStatementReview(tx.QueryRow(ctx, `SELECT `+statementReviewColumns+` FROM merchant_statement_reviews WHERE finding_id=$1 AND actor_id=$2 AND note=$3`, findingID, actor, note))
	} else if err == nil {
		err = actionAudit(ctx, tx, actor, "statement.finding_reviewed", findingID)
	}
	if err != nil {
		return StatementReview{}, err
	}
	return review, tx.Commit(ctx)
}
func (s *Store) StatementFindingReviews(ctx context.Context, findingID string) ([]StatementReview, error) {
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM merchant_statement_findings WHERE id=$1)`, findingID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT `+statementReviewColumns+` FROM merchant_statement_reviews WHERE finding_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatementReview{}
	for rows.Next() {
		r, err := scanStatementReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
