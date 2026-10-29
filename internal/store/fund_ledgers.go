package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wjgppx/xui/internal/fundledger"
)

var ErrFundLedgerConflict = errors.New("fund ledger scope or concurrent state conflict")
var ErrFundAccountConflict = errors.New("fund account alias has a different provider")

// FundLedgerSummary is one immutable observation batch. It is never an
// accumulated/current account balance, even when another batch overlaps it.
type FundLedgerSummary struct {
	fundledger.Metadata
	ID         string    `json:"id"`
	ImportedBy string    `json:"imported_by"`
	CreatedAt  time.Time `json:"created_at"`
}
type FundLedger struct {
	FundLedgerSummary
	Rows []fundledger.Entry `json:"rows"`
}

type FundReconciliationSummary struct {
	ID                      string    `json:"id"`
	MerchantBatchID         string    `json:"merchant_batch_id"`
	BankBatchID             string    `json:"bank_batch_id"`
	CreatedBy               string    `json:"created_by"`
	CreatedAt               time.Time `json:"created_at"`
	Version                 string    `json:"version"`
	MerchantAccountKey      string    `json:"merchant_account_key"`
	BankAccountKey          string    `json:"bank_account_key"`
	MatchedCount            int       `json:"matched_count"`
	DifferenceCount         int       `json:"difference_count"`
	UnmatchedCount          int       `json:"unmatched_count"`
	AmbiguousCount          int       `json:"ambiguous_count"`
	MerchantCandidateCount  int       `json:"merchant_candidate_count"`
	BankCandidateCount      int       `json:"bank_candidate_count"`
	MerchantOutOfScopeCount int       `json:"merchant_out_of_scope_count"`
	BankOutOfScopeCount     int       `json:"bank_out_of_scope_count"`
	MerchantOtherCount      int       `json:"merchant_other_count"`
	BankOtherCount          int       `json:"bank_other_count"`
	SourceWarnings          []string  `json:"source_warnings"`
}
type FundReconciliation struct {
	FundReconciliationSummary
	Rows []fundledger.TransferComparison `json:"rows"`
}

func fundLedgerError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if errors.Is(statementError(err), ErrStatementConflict) {
		return ErrFundLedgerConflict
	}
	return err
}
func fundActorVersion(expected []int64) (int64, error) {
	if len(expected) > 1 {
		return 0, ErrFundLedgerConflict
	}
	if len(expected) == 1 {
		if expected[0] < 0 {
			return 0, ErrRoleForbidden
		}
		return expected[0], nil
	}
	return -1, nil
}
func readFundLedger(ctx context.Context, q statementQuery, id string) (FundLedger, error) {
	var result FundLedger
	var raw []byte
	err := q.QueryRow(ctx, `SELECT id,document,imported_by,created_at FROM fund_ledger_batches WHERE id=$1`, id).Scan(&result.ID, &raw, &result.ImportedBy, &result.CreatedAt)
	if err != nil {
		return result, fundLedgerError(err)
	}
	var document fundledger.Document
	if err = json.Unmarshal(raw, &document); err != nil {
		return result, err
	}
	result.Metadata, result.Rows = document.Metadata, document.Rows
	return result, nil
}
func (s *Store) FundLedger(ctx context.Context, id string) (FundLedger, error) {
	return readFundLedger(ctx, s.DB, id)
}
func (s *Store) ListFundLedgers(ctx context.Context) ([]FundLedgerSummary, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,document-'rows',imported_by,created_at FROM fund_ledger_batches ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FundLedgerSummary{}
	for rows.Next() {
		var item FundLedgerSummary
		var raw []byte
		if err = rows.Scan(&item.ID, &raw, &item.ImportedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &item.Metadata); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func (s *Store) ImportFundLedger(ctx context.Context, actor string, input fundledger.Import, expectedVersion ...int64) (FundLedger, error) {
	var result FundLedger
	version, err := fundActorVersion(expectedVersion)
	if err != nil {
		return result, err
	}
	document, err := fundledger.Parse(input)
	if err != nil {
		return result, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, false); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO fund_ledger_accounts(account_key,provider,currency,created_by) VALUES($1,$2,'CNY',$3) ON CONFLICT(account_key) DO NOTHING`, document.AccountKey, document.Provider, actor); err != nil {
		return result, fundLedgerError(err)
	}
	var provider, currency string
	if err = tx.QueryRow(ctx, `SELECT provider,currency FROM fund_ledger_accounts WHERE account_key=$1 FOR SHARE`, document.AccountKey).Scan(&provider, &currency); err != nil {
		return result, fundLedgerError(err)
	}
	if provider != document.Provider || currency != document.Currency {
		return result, ErrFundAccountConflict
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return result, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO fund_ledger_batches(id,account_key,digest,document,imported_by) VALUES($1,$2,$3,$4::jsonb,$5) ON CONFLICT(account_key,digest) DO NOTHING`, ID(), document.AccountKey, document.Digest, raw, actor)
	if err != nil {
		return result, fundLedgerError(err)
	}
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM fund_ledger_batches WHERE account_key=$1 AND digest=$2`, document.AccountKey, document.Digest).Scan(&id); err != nil {
		return result, fundLedgerError(err)
	}
	if tag.RowsAffected() == 1 {
		if err = actionAudit(ctx, tx, actor, "fund_ledger.imported", id); err != nil {
			return result, err
		}
	}
	result, err = readFundLedger(ctx, tx, id)
	if err != nil {
		return result, err
	}
	return result, fundLedgerError(tx.Commit(ctx))
}

func (s *Store) FundReconciliation(ctx context.Context, id string) (FundReconciliation, error) {
	var result FundReconciliation
	var raw []byte
	err := s.DB.QueryRow(ctx, `SELECT report FROM fund_ledger_reconciliations WHERE id=$1`, id).Scan(&raw)
	if err != nil {
		return result, fundLedgerError(err)
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}
func (s *Store) ListFundReconciliations(ctx context.Context) ([]FundReconciliationSummary, error) {
	rows, err := s.DB.Query(ctx, `SELECT report-'rows' FROM fund_ledger_reconciliations ORDER BY created_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []FundReconciliationSummary{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item FundReconciliationSummary
		if err = json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
func newFundReconciliation(actor, merchantID, bankID string, c fundledger.Comparison) FundReconciliation {
	return FundReconciliation{FundReconciliationSummary: FundReconciliationSummary{
		ID: ID(), MerchantBatchID: merchantID, BankBatchID: bankID, CreatedBy: actor,
		Version: c.Version, MerchantAccountKey: c.MerchantAccountKey, BankAccountKey: c.BankAccountKey,
		MatchedCount: c.MatchedCount, DifferenceCount: c.DifferenceCount, UnmatchedCount: c.UnmatchedCount, AmbiguousCount: c.AmbiguousCount,
		MerchantCandidateCount: c.MerchantCandidateCount, BankCandidateCount: c.BankCandidateCount,
		MerchantOutOfScopeCount: c.MerchantOutOfScopeCount, BankOutOfScopeCount: c.BankOutOfScopeCount,
		MerchantOtherCount: c.MerchantOtherCount, BankOtherCount: c.BankOtherCount, SourceWarnings: c.SourceWarnings,
	}, Rows: c.Rows}
}
func (s *Store) ReconcileFundLedgers(ctx context.Context, actor, merchantBatchID, bankBatchID string, expectedVersion ...int64) (FundReconciliation, error) {
	var result FundReconciliation
	version, err := fundActorVersion(expectedVersion)
	if err != nil {
		return result, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if err = actionActor(ctx, tx, actor, version, false); err != nil {
		return result, err
	}
	merchant, err := readFundLedger(ctx, tx, merchantBatchID)
	if err != nil {
		return result, err
	}
	bank, err := readFundLedger(ctx, tx, bankBatchID)
	if err != nil {
		return result, err
	}
	if merchantBatchID == bankBatchID || (merchant.Provider != "alipay" && merchant.Provider != "wechat") || bank.Provider != "bank" || merchant.AccountKey == bank.AccountKey {
		return result, ErrFundLedgerConflict
	}
	comparison, err := fundledger.Compare(fundledger.Document{Metadata: merchant.Metadata, Rows: merchant.Rows}, fundledger.Document{Metadata: bank.Metadata, Rows: bank.Rows})
	if err != nil {
		if errors.Is(err, fundledger.ErrInvalid) {
			return result, ErrFundLedgerConflict
		}
		return result, err
	}
	result = newFundReconciliation(actor, merchantBatchID, bankBatchID, comparison)
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&result.CreatedAt); err != nil {
		return result, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO fund_ledger_reconciliations(id,merchant_batch_id,bank_batch_id,version,report,created_by,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7) ON CONFLICT(merchant_batch_id,bank_batch_id,version) DO NOTHING`, result.ID, merchantBatchID, bankBatchID, result.Version, raw, actor, result.CreatedAt)
	if err != nil {
		return result, fundLedgerError(err)
	}
	if tag.RowsAffected() == 1 {
		if err = actionAudit(ctx, tx, actor, "fund_ledger.reconciled", result.ID); err != nil {
			return result, err
		}
	} else {
		if err = tx.QueryRow(ctx, `SELECT report FROM fund_ledger_reconciliations WHERE merchant_batch_id=$1 AND bank_batch_id=$2 AND version=$3`, merchantBatchID, bankBatchID, result.Version).Scan(&raw); err != nil {
			return result, fundLedgerError(err)
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return result, err
		}
	}
	return result, fundLedgerError(tx.Commit(ctx))
}
