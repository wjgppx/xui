package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/statements"
)

// FinancialSnapshot belongs to an immutable raw source, independently of the
// normalized gross-payment statement and its historical reconciliation runs.
type FinancialSnapshot struct {
	statements.FinancialDocument
	Available bool           `json:"available"`
	SourceID  string         `json:"source_id"`
	Runs      []FinancialRun `json:"runs"`
}

type FinancialRun struct {
	ID                             string    `json:"id"`
	SourceID                       string    `json:"source_id"`
	Revision                       int       `json:"revision"`
	ReferenceDigest                string    `json:"reference_digest"`
	MatchedCount                   int       `json:"matched_count"`
	DifferenceCount                int       `json:"difference_count"`
	UnassessedCount                int       `json:"unassessed_count"`
	ObservedRowCount               int       `json:"observed_row_count"`
	ReferenceRowCount              int       `json:"reference_row_count"`
	ObservedFeeTotalYuan           *string   `json:"observed_fee_total_yuan"`
	ObservedChannelAmountTotalYuan *string   `json:"observed_channel_amount_total_yuan"`
	ExpectedFeeTotalYuan           string    `json:"expected_fee_total_yuan"`
	ExpectedChannelAmountTotalYuan string    `json:"expected_channel_amount_total_yuan"`
	SummaryCodes                   []string  `json:"summary_codes"`
	CreatedBy                      string    `json:"created_by"`
	CreatedAt                      time.Time `json:"created_at"`
}

type FinancialComparison struct {
	PaymentID                 string   `json:"payment_id"`
	ExpectedPaymentID         string   `json:"expected_payment_id"`
	RefundID                  string   `json:"refund_id"`
	ObservedFeeYuan           *string  `json:"observed_fee_yuan"`
	ExpectedFeeYuan           *string  `json:"expected_fee_yuan"`
	ObservedChannelAmountYuan *string  `json:"observed_channel_amount_yuan"`
	ExpectedChannelAmountYuan *string  `json:"expected_channel_amount_yuan"`
	Status                    string   `json:"status"`
	Codes                     []string `json:"codes"`
}

type FinancialReport struct {
	Run  FinancialRun          `json:"run"`
	Rows []FinancialComparison `json:"rows"`
}

func financialNormalizedDocument(ctx context.Context, q statementQuery, statementID string) (statements.Document, error) {
	statement, err := scanStatement(q.QueryRow(ctx, `SELECT `+statementColumns+` FROM merchant_statements WHERE id=$1`, statementID))
	if err != nil {
		return statements.Document{}, err
	}
	doc := statements.Document{Provider: statement.Provider, Kind: statement.Kind, Date: statement.Date, Coverage: statement.Coverage, Digest: statement.Digest, Total: statement.Total, Rows: []statements.Row{}}
	rows, err := q.Query(ctx, `SELECT payment_id,transaction_id,refund_id,provider_refund_id,amount_minor,currency,occurred_at FROM merchant_statement_rows WHERE statement_id=$1 ORDER BY row_number`, statementID)
	if err != nil {
		return doc, err
	}
	defer rows.Close()
	for rows.Next() {
		var row statements.Row
		if err = rows.Scan(&row.PaymentID, &row.TransactionID, &row.RefundID, &row.ProviderRefundID, &row.Amount, &row.Currency, &row.OccurredAt); err != nil {
			return doc, err
		}
		doc.Rows = append(doc.Rows, row)
	}
	return doc, rows.Err()
}

// An identical pre-v14 source may receive its first financial snapshot on
// explicit re-import. Once present, observations cannot be replaced or amended.
func addFinancialSnapshot(ctx context.Context, tx pgx.Tx, actor, sourceID string, financial statements.FinancialDocument) error {
	var statementID, format string
	err := tx.QueryRow(ctx, `SELECT statement_id,source_format FROM merchant_statement_sources WHERE id=$1`, sourceID).Scan(&statementID, &format)
	if err != nil {
		return statementError(err)
	}
	wantBasis := map[string]string{"wechat_success_v1": "wechat_settlement_order", "wechat_refund_v1": "wechat_refund", "alipay_trade_v1": "alipay_merchant_received"}[format]
	if wantBasis == "" || wantBasis != financial.Basis {
		return ErrStatementConflict
	}
	document, err := financialNormalizedDocument(ctx, tx, statementID)
	if err != nil {
		return err
	}
	if err = statements.ValidateFinancialDocument(document, financial); err != nil {
		return err
	}
	data, err := json.Marshal(financial)
	if err != nil {
		return ErrStatementConflict
	}
	digestBytes := sha256.Sum256(data)
	digest := hex.EncodeToString(digestBytes[:])
	tag, err := tx.Exec(ctx, `INSERT INTO merchant_statement_financials(source_id,digest,document) VALUES($1,$2,$3::jsonb) ON CONFLICT(source_id) DO NOTHING`, sourceID, digest, data)
	if err != nil {
		return statementError(err)
	}
	if tag.RowsAffected() == 0 {
		var existing string
		if err = tx.QueryRow(ctx, `SELECT digest FROM merchant_statement_financials WHERE source_id=$1`, sourceID).Scan(&existing); err != nil {
			return statementError(err)
		}
		if existing != digest {
			return ErrStatementConflict
		}
		return nil
	}
	return actionAudit(ctx, tx, actor, "statement.financials_added", sourceID)
}

func financialSourceParent(ctx context.Context, q statementQuery, statementID, sourceID string) error {
	var ignored string
	return statementError(q.QueryRow(ctx, `SELECT id FROM merchant_statement_sources WHERE id=$1 AND statement_id=$2`, sourceID, statementID).Scan(&ignored))
}

func readFinancialDocument(ctx context.Context, q statementQuery, sourceID string) (statements.FinancialDocument, bool, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT document FROM merchant_statement_financials WHERE source_id=$1`, sourceID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return statements.FinancialDocument{Rows: []statements.FinancialRow{}, SummaryCodes: []string{}}, false, nil
	}
	if err != nil {
		return statements.FinancialDocument{}, false, err
	}
	var doc statements.FinancialDocument
	if err = json.Unmarshal(data, &doc); err != nil {
		return doc, false, err
	}
	return doc, true, nil
}

func (s *Store) SourceFinancials(ctx context.Context, statementID, sourceID string) (FinancialSnapshot, error) {
	result := FinancialSnapshot{SourceID: sourceID, Runs: []FinancialRun{}}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = financialSourceParent(ctx, tx, statementID, sourceID); err != nil {
		return result, err
	}
	result.FinancialDocument, result.Available, err = readFinancialDocument(ctx, tx, sourceID)
	if err != nil {
		return result, err
	}
	rows, err := tx.Query(ctx, `SELECT report->'run' FROM merchant_statement_financial_runs WHERE source_id=$1 ORDER BY revision DESC LIMIT 20`, sourceID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return result, err
		}
		var run FinancialRun
		if err = json.Unmarshal(raw, &run); err != nil {
			rows.Close()
			return result, err
		}
		result.Runs = append(result.Runs, run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Store) SourceFinancialReport(ctx context.Context, statementID, sourceID, runID string) (FinancialReport, error) {
	var result FinancialReport
	var data []byte
	err := s.DB.QueryRow(ctx, `SELECT r.report FROM merchant_statement_financial_runs r JOIN merchant_statement_sources src ON src.id=r.source_id WHERE r.id=$1 AND r.source_id=$2 AND src.statement_id=$3`, runID, sourceID, statementID).Scan(&data)
	if err != nil {
		return result, statementError(err)
	}
	err = json.Unmarshal(data, &result)
	return result, err
}

func financialRowKey(kind string, row statements.FinancialRow) string {
	if kind == "refund" {
		return row.RefundID
	}
	return row.PaymentID
}

func compareFinancialReference(sourceID, actor, kind string, document statements.FinancialDocument, reference statements.FinancialReference) FinancialReport {
	report := FinancialReport{Run: FinancialRun{ID: ID(), SourceID: sourceID, ReferenceDigest: reference.Digest, CreatedBy: actor, ObservedRowCount: len(document.Rows), ReferenceRowCount: len(reference.Rows), ObservedFeeTotalYuan: document.FeeTotalYuan, ObservedChannelAmountTotalYuan: document.ChannelAmountTotalYuan, ExpectedFeeTotalYuan: reference.FeeTotalYuan, ExpectedChannelAmountTotalYuan: reference.ChannelAmountTotalYuan, SummaryCodes: append([]string{}, document.SummaryCodes...)}, Rows: []FinancialComparison{}}
	observations := map[string]statements.FinancialRow{}
	refs := map[string]statements.FinancialRow{}
	keys := map[string]bool{}
	for _, row := range document.Rows {
		key := financialRowKey(kind, row)
		observations[key], keys[key] = row, true
	}
	for _, row := range reference.Rows {
		key := financialRowKey(kind, row)
		refs[key], keys[key] = row, true
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		observed, hasObserved := observations[key]
		expected, hasExpected := refs[key]
		row := FinancialComparison{PaymentID: observed.PaymentID, ExpectedPaymentID: expected.PaymentID, RefundID: observed.RefundID, ObservedFeeYuan: observed.FeeYuan, ObservedChannelAmountYuan: observed.ChannelAmountYuan, ExpectedFeeYuan: expected.FeeYuan, ExpectedChannelAmountYuan: expected.ChannelAmountYuan, Codes: []string{}}
		if !hasObserved {
			row.PaymentID, row.RefundID = expected.PaymentID, expected.RefundID
			row.Codes = append(row.Codes, "missing_observation")
		} else if !hasExpected {
			row.Status = "unassessed"
			row.Codes = append(row.Codes, "missing_reference")
		} else {
			if observed.PaymentID != expected.PaymentID {
				row.Codes = append(row.Codes, "identity_mismatch")
			}
			if observed.FeeYuan == nil {
				row.Codes = append(row.Codes, "fee_unavailable")
			} else if *observed.FeeYuan != *expected.FeeYuan {
				row.Codes = append(row.Codes, "fee_mismatch")
			}
			if observed.ChannelAmountYuan == nil {
				row.Codes = append(row.Codes, "channel_amount_unavailable")
			} else if *observed.ChannelAmountYuan != *expected.ChannelAmountYuan {
				row.Codes = append(row.Codes, "channel_amount_mismatch")
			}
		}
		if row.Status == "unassessed" {
			report.Run.UnassessedCount++
		} else if len(row.Codes) > 0 {
			row.Status = "difference"
			report.Run.DifferenceCount++
		} else {
			row.Status = "matched"
			report.Run.MatchedCount++
		}
		report.Rows = append(report.Rows, row)
	}
	return report
}

func (s *Store) ReconcileSourceFinancials(ctx context.Context, actor, statementID, sourceID, csv string, expectedVersion ...int64) (FinancialReport, error) {
	var result FinancialReport
	if len(expectedVersion) > 1 {
		return result, ErrStatementConflict
	}
	version := int64(-1)
	if len(expectedVersion) == 1 {
		version = expectedVersion[0]
		if version < 0 {
			return result, ErrRoleForbidden
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, false); err != nil {
		return result, err
	}
	// Serialize only this immutable source's report sequence, never money facts.
	var kind string
	err = tx.QueryRow(ctx, `SELECT s.kind FROM merchant_statement_sources src JOIN merchant_statements s ON s.id=src.statement_id WHERE src.id=$1 AND src.statement_id=$2 FOR NO KEY UPDATE OF src`, sourceID, statementID).Scan(&kind)
	if err != nil {
		return result, statementError(err)
	}
	ref, err := statements.ParseFinancialReference(csv, kind)
	if err != nil {
		return result, err
	}
	doc, available, err := readFinancialDocument(ctx, tx, sourceID)
	if err != nil {
		return result, err
	}
	if !available {
		return result, ErrStatementConflict
	}
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT report FROM merchant_statement_financial_runs WHERE source_id=$1 AND reference_digest=$2`, sourceID, ref.Digest).Scan(&existing)
	if err == nil {
		if err = json.Unmarshal(existing, &result); err != nil {
			return result, err
		}
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	result = compareFinancialReference(sourceID, actor, kind, doc, ref)
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1 FROM merchant_statement_financial_runs WHERE source_id=$1`, sourceID).Scan(&result.Run.Revision); err != nil {
		return result, err
	}
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&result.Run.CreatedAt); err != nil {
		return result, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO merchant_statement_financial_runs(id,source_id,revision,reference_digest,report,created_by,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, result.Run.ID, sourceID, result.Run.Revision, ref.Digest, raw, actor, result.Run.CreatedAt); err != nil {
		return result, statementError(err)
	}
	if err = actionAudit(ctx, tx, actor, "statement.financials_reconciled", result.Run.ID); err != nil {
		return result, err
	}
	return result, statementError(tx.Commit(ctx))
}
