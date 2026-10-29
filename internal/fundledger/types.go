// Package fundledger validates manually supplied fund-flow observations and
// compares explicitly selected merchant and bank scopes. It never creates a
// withdrawal, changes a payment or proves an actual bank balance or arrival.
package fundledger

import (
	"errors"
	"fmt"
	"time"
)

const (
	Version           = "fund-ledger-v1"
	ComparisonVersion = "fund-transfer-comparison-v1"
	MaxBytes          = 1 << 20
	MaxRows           = 2000
)

var ErrInvalid = errors.New("invalid fund ledger observation")

// ValidationError contains only finite field names/codes and a physical CSV
// line. Uploaded identifiers, account values and contents never enter errors.
type ValidationError struct {
	Line  int
	Field string
	Code  string
}

func (e *ValidationError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("fund ledger line %d: %s (%s)", e.Line, e.Field, e.Code)
	}
	return "fund ledger: " + e.Field + " (" + e.Code + ")"
}
func (e *ValidationError) Unwrap() error { return ErrInvalid }
func invalid(line int, field, code string) error {
	return &ValidationError{Line: line, Field: field, Code: code}
}

type Import struct {
	Provider           string `json:"provider"`
	AccountKey         string `json:"account_key"`
	Date               string `json:"date"`
	Coverage           string `json:"coverage"`
	OpeningBalanceYuan string `json:"opening_balance_yuan"`
	ClosingBalanceYuan string `json:"closing_balance_yuan"`
	CSV                string `json:"csv"`
}

type Metadata struct {
	Version                    string   `json:"version"`
	Provider                   string   `json:"provider"`
	AccountKey                 string   `json:"account_key"`
	Date                       string   `json:"date"`
	Coverage                   string   `json:"coverage"`
	Currency                   string   `json:"currency"`
	Digest                     string   `json:"digest"`
	OpeningBalanceYuan         string   `json:"opening_balance_yuan"`
	ClosingBalanceYuan         string   `json:"closing_balance_yuan"`
	ExpectedClosingBalanceYuan string   `json:"expected_closing_balance_yuan"`
	TotalCreditYuan            string   `json:"total_credit_yuan"`
	TotalDebitYuan             string   `json:"total_debit_yuan"`
	RowCount                   int      `json:"row_count"`
	BalanceDifferenceCount     int      `json:"balance_difference_count"`
	SummaryCodes               []string `json:"summary_codes"`
}

type Entry struct {
	Sequence                int       `json:"sequence"`
	EntryID                 string    `json:"entry_id"`
	OccurredAt              time.Time `json:"occurred_at"`
	Kind                    string    `json:"kind"`
	Direction               string    `json:"direction"`
	AmountYuan              string    `json:"amount_yuan"`
	BalanceAfterYuan        string    `json:"balance_after_yuan"`
	ExpectedBalanceYuan     string    `json:"expected_balance_yuan"`
	StepExpectedBalanceYuan string    `json:"step_expected_balance_yuan"`
	TransferID              string    `json:"transfer_id"`
	CounterpartyAccountKey  string    `json:"counterparty_account_key"`
	Codes                   []string  `json:"codes"`
}

type Document struct {
	Metadata
	Rows []Entry `json:"rows"`
}

type TransferComparison struct {
	TransferID      string   `json:"transfer_id"`
	MerchantEntries []Entry  `json:"merchant_entries"`
	BankEntries     []Entry  `json:"bank_entries"`
	Status          string   `json:"status"`
	Codes           []string `json:"codes"`
}

type Comparison struct {
	Version                 string               `json:"version"`
	MerchantAccountKey      string               `json:"merchant_account_key"`
	BankAccountKey          string               `json:"bank_account_key"`
	MatchedCount            int                  `json:"matched_count"`
	DifferenceCount         int                  `json:"difference_count"`
	UnmatchedCount          int                  `json:"unmatched_count"`
	AmbiguousCount          int                  `json:"ambiguous_count"`
	MerchantCandidateCount  int                  `json:"merchant_candidate_count"`
	BankCandidateCount      int                  `json:"bank_candidate_count"`
	MerchantOutOfScopeCount int                  `json:"merchant_out_of_scope_count"`
	BankOutOfScopeCount     int                  `json:"bank_out_of_scope_count"`
	MerchantOtherCount      int                  `json:"merchant_other_count"`
	BankOtherCount          int                  `json:"bank_other_count"`
	SourceWarnings          []string             `json:"source_warnings"`
	Rows                    []TransferComparison `json:"rows"`
}
