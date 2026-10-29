package fundledger

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wjgppx/xui/internal/statements"
)

var header = []string{"sequence", "entry_id", "occurred_at", "kind", "direction", "amount_yuan", "balance_after_yuan", "transfer_id", "counterparty_account_key"}
var accountPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
var timePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

func baseMetadata(input Import) (Metadata, time.Time, error) {
	m := Metadata{Version: Version, Provider: input.Provider, AccountKey: input.AccountKey, Date: input.Date, Coverage: input.Coverage, Currency: "CNY", SummaryCodes: []string{}}
	if input.Provider != "alipay" && input.Provider != "wechat" && input.Provider != "bank" {
		return m, time.Time{}, invalid(0, "provider", "invalid_field")
	}
	if !accountPattern.MatchString(input.AccountKey) {
		return m, time.Time{}, invalid(0, "account_key", "invalid_field")
	}
	if input.Coverage != "partial" && input.Coverage != "full" {
		return m, time.Time{}, invalid(0, "coverage", "invalid_field")
	}
	if !datePattern.MatchString(input.Date) {
		return m, time.Time{}, invalid(0, "date", "invalid_field")
	}
	start, err := time.ParseInLocation("2006-01-02", input.Date, shanghai)
	if err != nil || start.Year() < 2000 || start.Year() > 2099 {
		return m, time.Time{}, invalid(0, "date", "invalid_field")
	}
	m.OpeningBalanceYuan, err = statements.CanonicalYuan(input.OpeningBalanceYuan)
	if err != nil {
		return m, time.Time{}, invalid(0, "opening_balance_yuan", "invalid_amount")
	}
	m.ClosingBalanceYuan, err = statements.CanonicalYuan(input.ClosingBalanceYuan)
	if err != nil {
		return m, time.Time{}, invalid(0, "closing_balance_yuan", "invalid_amount")
	}
	return m, start, nil
}

func physicalLines(content string) error {
	line := 1
	for index, r := range content {
		if r == '\r' {
			if index+1 >= len(content) || content[index+1] != '\n' {
				return invalid(line, "csv", "invalid_control")
			}
		} else if r != '\n' && unicode.IsControl(r) {
			return invalid(line, "csv", "invalid_control")
		}
		if r == '\n' {
			line++
		}
	}
	lines := strings.Split(content, "\n")
	for index, value := range lines {
		if index == len(lines)-1 && value == "" {
			continue
		}
		if strings.TrimSuffix(value, "\r") == "" {
			return invalid(index+1, "csv", "blank_record")
		}
	}
	return nil
}

func transferCandidate(provider string, row Entry) bool {
	return provider == "bank" && row.Kind == "settlement" || provider != "bank" && row.Kind == "withdrawal"
}

func validKindDirection(provider, kind, direction string) bool {
	if direction != "credit" && direction != "debit" {
		return false
	}
	switch kind {
	case "fee", "adjustment", "other":
		return true
	case "withdrawal":
		return direction == "debit"
	case "deposit":
		return direction == "credit"
	case "settlement":
		return provider == "bank" && direction == "credit"
	case "payment":
		return provider != "bank" && direction == "credit"
	case "refund":
		return provider != "bank" && direction == "debit"
	}
	return false
}

func parseEntry(values []string, index, line int, metadata Metadata, start, previous time.Time) (Entry, time.Time, error) {
	var row Entry
	if len(values) != len(header) {
		return row, time.Time{}, invalid(line, "csv", "invalid_record")
	}
	if values[0] != strconv.Itoa(index+1) {
		return row, time.Time{}, invalid(line, "sequence", "invalid_sequence")
	}
	row.Sequence, row.EntryID = index+1, values[1]
	if !referencePattern.MatchString(row.EntryID) {
		return row, time.Time{}, invalid(line, "entry_id", "invalid_field")
	}
	if !timePattern.MatchString(values[2]) {
		return row, time.Time{}, invalid(line, "occurred_at", "invalid_field")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, values[2])
	if err != nil {
		return row, time.Time{}, invalid(line, "occurred_at", "invalid_field")
	}
	if occurredAt.Before(start) || !occurredAt.Before(start.AddDate(0, 0, 1)) {
		return row, time.Time{}, invalid(line, "occurred_at", "outside_date")
	}
	if !previous.IsZero() && occurredAt.Before(previous) {
		return row, time.Time{}, invalid(line, "occurred_at", "invalid_order")
	}
	row.OccurredAt = occurredAt.UTC().Truncate(time.Microsecond)
	row.Kind, row.Direction = values[3], values[4]
	if row.Direction != "credit" && row.Direction != "debit" {
		return row, time.Time{}, invalid(line, "direction", "invalid_field")
	}
	if !validKindDirection(metadata.Provider, row.Kind, row.Direction) {
		return row, time.Time{}, invalid(line, "kind", "invalid_field")
	}
	row.AmountYuan, err = statements.CanonicalYuan(values[5])
	if err != nil {
		return row, time.Time{}, invalid(line, "amount_yuan", "invalid_amount")
	}
	if units(row.AmountYuan).Sign() <= 0 {
		return row, time.Time{}, invalid(line, "amount_yuan", "invalid_amount")
	}
	row.BalanceAfterYuan, err = statements.CanonicalYuan(values[6])
	if err != nil {
		return row, time.Time{}, invalid(line, "balance_after_yuan", "invalid_amount")
	}
	row.TransferID, row.CounterpartyAccountKey = values[7], values[8]
	if transferCandidate(metadata.Provider, row) {
		if !referencePattern.MatchString(row.TransferID) {
			return row, time.Time{}, invalid(line, "transfer_id", "invalid_field")
		}
		if !accountPattern.MatchString(row.CounterpartyAccountKey) || row.CounterpartyAccountKey == metadata.AccountKey {
			return row, time.Time{}, invalid(line, "counterparty_account_key", "invalid_field")
		}
	} else if row.TransferID != "" || row.CounterpartyAccountKey != "" {
		return row, time.Time{}, invalid(line, "transfer_id", "invalid_field")
	}
	return row, occurredAt, nil
}

// Parse preserves the input sequence and arithmetic discrepancies. A valid
// observation may be internally inconsistent; malformed structure is rejected.
func Parse(input Import) (Document, error) {
	metadata, start, err := baseMetadata(input)
	if err != nil {
		return Document{}, err
	}
	if len(input.CSV) > MaxBytes {
		return Document{}, invalid(0, "csv", "too_large")
	}
	if !utf8.ValidString(input.CSV) {
		return Document{}, invalid(0, "csv", "invalid_utf8")
	}
	content := strings.TrimPrefix(input.CSV, "\ufeff")
	if err = physicalLines(content); err != nil {
		return Document{}, err
	}
	reader := csv.NewReader(strings.NewReader(content))
	reader.FieldsPerRecord = len(header)
	columns, err := reader.Read()
	if err != nil || !reflect.DeepEqual(columns, header) {
		return Document{}, invalid(1, "csv", "invalid_header")
	}
	rows := []Entry{}
	seen := map[string]bool{}
	var previous time.Time
	for {
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			line := len(rows) + 2
			var parseErr *csv.ParseError
			if errors.As(readErr, &parseErr) {
				line = parseErr.StartLine
			}
			return Document{}, invalid(line, "csv", "invalid_record")
		}
		line, _ := reader.FieldPos(0)
		if len(rows) >= MaxRows {
			return Document{}, invalid(line, "csv", "too_many_rows")
		}
		row, rawTime, rowErr := parseEntry(values, len(rows), line, metadata, start, previous)
		if rowErr != nil {
			return Document{}, rowErr
		}
		if seen[row.EntryID] {
			return Document{}, invalid(line, "entry_id", "duplicate_entry_id")
		}
		seen[row.EntryID] = true
		previous = rawTime
		rows = append(rows, row)
	}
	return buildDocument(metadata, rows)
}

// units is used only after canonical monetary validation.
func units(canonical string) *big.Int {
	value, ok := new(big.Int).SetString(strings.ReplaceAll(canonical, ".", ""), 10)
	if !ok {
		panic("noncanonical internal fund amount")
	}
	return value
}

func formatUnits(value *big.Int) string {
	negative := value.Sign() < 0
	digits := new(big.Int).Abs(value).String()
	if len(digits) < 6 {
		digits = strings.Repeat("0", 6-len(digits)) + digits
	}
	result := digits[:len(digits)-5] + "." + digits[len(digits)-5:]
	if negative {
		return "-" + result
	}
	return result
}

func bounded(value *big.Int) bool { return len(new(big.Int).Abs(value).String()) <= 34 }

type digestEntry struct {
	Sequence               int       `json:"sequence"`
	EntryID                string    `json:"entry_id"`
	OccurredAt             time.Time `json:"occurred_at"`
	Kind                   string    `json:"kind"`
	Direction              string    `json:"direction"`
	AmountYuan             string    `json:"amount_yuan"`
	BalanceAfterYuan       string    `json:"balance_after_yuan"`
	TransferID             string    `json:"transfer_id"`
	CounterpartyAccountKey string    `json:"counterparty_account_key"`
}

func buildDocument(metadata Metadata, rows []Entry) (Document, error) {
	document := Document{Metadata: metadata, Rows: make([]Entry, 0, len(rows))}
	document.RowCount, document.BalanceDifferenceCount = len(rows), 0
	document.SummaryCodes = []string{}
	credit, debit := new(big.Int), new(big.Int)
	running, reported := units(metadata.OpeningBalanceYuan), units(metadata.OpeningBalanceYuan)
	inputs := make([]digestEntry, 0, len(rows))
	for _, row := range rows {
		amount := units(row.AmountYuan)
		signed := new(big.Int).Set(amount)
		if row.Direction == "credit" {
			credit.Add(credit, amount)
		} else {
			debit.Add(debit, amount)
			signed.Neg(signed)
		}
		running.Add(running, signed)
		step := new(big.Int).Add(reported, signed)
		if !bounded(running) || !bounded(step) || !bounded(credit) || !bounded(debit) {
			return Document{}, invalid(row.Sequence+1, "amount_yuan", "amount_overflow")
		}
		row.ExpectedBalanceYuan, row.StepExpectedBalanceYuan = formatUnits(running), formatUnits(step)
		row.Codes = []string{}
		if row.BalanceAfterYuan != row.ExpectedBalanceYuan {
			row.Codes = append(row.Codes, "running_balance_mismatch")
		}
		if row.BalanceAfterYuan != row.StepExpectedBalanceYuan {
			row.Codes = append(row.Codes, "step_balance_mismatch")
		}
		if len(row.Codes) > 0 {
			document.BalanceDifferenceCount++
		}
		reported = units(row.BalanceAfterYuan)
		document.Rows = append(document.Rows, row)
		inputs = append(inputs, digestEntry{row.Sequence, row.EntryID, row.OccurredAt, row.Kind, row.Direction, row.AmountYuan, row.BalanceAfterYuan, row.TransferID, row.CounterpartyAccountKey})
	}
	document.TotalCreditYuan, document.TotalDebitYuan = formatUnits(credit), formatUnits(debit)
	document.ExpectedClosingBalanceYuan = formatUnits(running)
	if document.ClosingBalanceYuan != document.ExpectedClosingBalanceYuan {
		document.SummaryCodes = append(document.SummaryCodes, "closing_balance_mismatch")
	}
	if len(rows) > 0 && rows[len(rows)-1].BalanceAfterYuan != document.ClosingBalanceYuan {
		document.SummaryCodes = append(document.SummaryCodes, "closing_entry_mismatch")
	}
	canonical, err := json.Marshal(struct {
		Version            string        `json:"version"`
		Provider           string        `json:"provider"`
		AccountKey         string        `json:"account_key"`
		Date               string        `json:"date"`
		Coverage           string        `json:"coverage"`
		Currency           string        `json:"currency"`
		OpeningBalanceYuan string        `json:"opening_balance_yuan"`
		ClosingBalanceYuan string        `json:"closing_balance_yuan"`
		Rows               []digestEntry `json:"rows"`
	}{Version, metadata.Provider, metadata.AccountKey, metadata.Date, metadata.Coverage, "CNY", metadata.OpeningBalanceYuan, metadata.ClosingBalanceYuan, inputs})
	if err != nil {
		return Document{}, invalid(0, "csv", "invalid_document")
	}
	digest := sha256.Sum256(canonical)
	document.Digest = hex.EncodeToString(digest[:])
	return document, nil
}

func validateDocument(document Document) error {
	if document.Version != Version || document.Currency != "CNY" || !digestPattern.MatchString(document.Digest) || document.Rows == nil || len(document.Rows) > MaxRows || document.RowCount != len(document.Rows) {
		return invalid(0, "csv", "invalid_document")
	}
	metadata, start, err := baseMetadata(Import{Provider: document.Provider, AccountKey: document.AccountKey, Date: document.Date, Coverage: document.Coverage, OpeningBalanceYuan: document.OpeningBalanceYuan, ClosingBalanceYuan: document.ClosingBalanceYuan})
	if err != nil {
		return invalid(0, "csv", "invalid_document")
	}
	rows := make([]Entry, 0, len(document.Rows))
	seen := map[string]bool{}
	var previous time.Time
	for i, row := range document.Rows {
		values := []string{strconv.Itoa(row.Sequence), row.EntryID, row.OccurredAt.Format(time.RFC3339Nano), row.Kind, row.Direction, row.AmountYuan, row.BalanceAfterYuan, row.TransferID, row.CounterpartyAccountKey}
		normalized, rawTime, rowErr := parseEntry(values, i, i+2, metadata, start, previous)
		if rowErr != nil || seen[row.EntryID] {
			return invalid(i+2, "csv", "invalid_document")
		}
		seen[row.EntryID], previous = true, rawTime
		rows = append(rows, normalized)
	}
	rebuilt, err := buildDocument(metadata, rows)
	if err != nil || !reflect.DeepEqual(document, rebuilt) {
		return invalid(0, "csv", "invalid_document")
	}
	return nil
}
