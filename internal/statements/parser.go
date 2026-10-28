// Package statements validates a deliberately small, versioned interchange
// format for merchant statements. Imported records are observations, never
// authenticated evidence that a payment or refund succeeded.
package statements

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxBytes = 1 << 20
	MaxRows  = 2000
)

var ErrInvalid = errors.New("invalid merchant statement")

// ValidationError contains only known field names and finite error codes. It
// never copies an account reference or other uploaded content into an error.
type ValidationError struct {
	Line  int
	Field string
	Code  string
}

func (e *ValidationError) Error() string {
	message := "账单字段 " + e.Field + " 格式不正确"
	switch e.Code {
	case "too_large":
		message = "账单文件不能超过 1 MiB"
	case "too_many_rows":
		message = "账单不能超过 2000 条记录"
	case "invalid_utf8":
		message = "账单必须使用有效的 UTF-8 编码"
	case "invalid_control":
		message = "账单不能包含控制字符或单独的回车符"
	case "blank_record":
		message = "账单不能包含空白记录"
	case "invalid_header":
		message = "账单表头必须与标准模板完全一致"
	case "invalid_record":
		message = "账单记录必须包含格式正确的 7 列"
	case "unsupported_refund_provider":
		message = "退款账单暂仅支持支付宝和微信支付"
	case "unexpected_refund_reference":
		message = "收款账单的退款编号字段必须为空"
	case "duplicate_identity":
		message = "账单字段 " + e.Field + " 出现重复标识"
	case "amount_overflow":
		message = "账单金额或金额合计超出支持范围"
	case "outside_date":
		message = "账单发生时间不在所选北京时间日期内"
	}
	if e.Line > 0 {
		return fmt.Sprintf("账单第 %d 行：%s", e.Line, message)
	}
	return message
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func invalid(line int, field, code string) error {
	return &ValidationError{Line: line, Field: field, Code: code}
}

type Import struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Date     string `json:"date"`
	Coverage string `json:"coverage"`
	CSV      string `json:"csv"`
}

type Document struct {
	Provider string
	Kind     string
	Date     string
	Coverage string
	Digest   string
	Rows     []Row
	Total    int64
}

type Row struct {
	PaymentID        string    `json:"payment_id"`
	TransactionID    string    `json:"transaction_id"`
	RefundID         string    `json:"refund_id"`
	ProviderRefundID string    `json:"provider_refund_id"`
	Currency         string    `json:"currency"`
	Amount           int64     `json:"amount_minor"`
	OccurredAt       time.Time `json:"occurred_at"`
}

var (
	referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	datePattern      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	amountPattern    = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	timePattern      = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])$`)
	shanghai         = time.FixedZone("Asia/Shanghai", 8*60*60)
)

var header = []string{"payment_id", "transaction_id", "refund_id", "provider_refund_id", "amount_minor", "currency", "occurred_at"}

// Parse rejects a whole import when any row is invalid. The digest covers the
// canonical sorted observations plus metadata, so BOMs, record ordering, CSV
// quoting and equivalent time-zone offsets cannot create duplicate statements.
// Times retain PostgreSQL's microsecond precision; submicrosecond differences
// do not create a digest that cannot be reconstructed from persisted rows.
func Parse(input Import) (Document, error) {
	var result Document
	switch input.Provider {
	case "alipay", "wechat", "epay_alipay", "epay_wechat":
	default:
		return result, invalid(0, "provider", "invalid_field")
	}
	if input.Kind != "payment" && input.Kind != "refund" {
		return result, invalid(0, "kind", "invalid_field")
	}
	if input.Kind == "refund" && (input.Provider == "epay_alipay" || input.Provider == "epay_wechat") {
		return result, invalid(0, "provider", "unsupported_refund_provider")
	}
	if input.Coverage != "partial" && input.Coverage != "full" {
		return result, invalid(0, "coverage", "invalid_field")
	}
	if !datePattern.MatchString(input.Date) {
		return result, invalid(0, "date", "invalid_field")
	}
	start, err := time.ParseInLocation("2006-01-02", input.Date, shanghai)
	if err != nil || start.Year() < 2000 || start.Year() > 2099 {
		return result, invalid(0, "date", "invalid_field")
	}
	end := start.AddDate(0, 0, 1)
	if len(input.CSV) > MaxBytes {
		return result, invalid(0, "csv", "too_large")
	}
	if !utf8.ValidString(input.CSV) {
		return result, invalid(0, "csv", "invalid_utf8")
	}
	content := strings.TrimPrefix(input.CSV, "\ufeff")
	if err = validatePhysicalLines(content); err != nil {
		return result, err
	}
	reader := csv.NewReader(strings.NewReader(content))
	reader.FieldsPerRecord = len(header)
	columns, err := reader.Read()
	if err != nil || !sameHeader(columns) {
		return result, invalid(1, "header", "invalid_header")
	}
	result = Document{Provider: input.Provider, Kind: input.Kind, Date: input.Date, Coverage: input.Coverage, Rows: []Row{}}
	identities, providerIdentities := map[string]bool{}, map[string]bool{}
	for {
		values, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			line := len(result.Rows) + 2
			var parseErr *csv.ParseError
			if errors.As(readErr, &parseErr) {
				line = parseErr.StartLine
			}
			return Document{}, invalid(line, "csv", "invalid_record")
		}
		line, _ := reader.FieldPos(0)
		if len(result.Rows) >= MaxRows {
			return Document{}, invalid(line, "csv", "too_many_rows")
		}
		row, rowErr := parseRow(values, input.Kind, start, end, line)
		if rowErr != nil {
			return Document{}, rowErr
		}
		identity, providerIdentity := row.PaymentID, row.TransactionID
		identityField, providerIdentityField := "payment_id", "transaction_id"
		if input.Kind == "refund" {
			identity, providerIdentity = row.RefundID, row.ProviderRefundID
			identityField, providerIdentityField = "refund_id", "provider_refund_id"
		}
		if identities[identity] {
			return Document{}, invalid(line, identityField, "duplicate_identity")
		}
		if providerIdentities[providerIdentity] {
			return Document{}, invalid(line, providerIdentityField, "duplicate_identity")
		}
		identities[identity], providerIdentities[providerIdentity] = true, true
		if result.Total > math.MaxInt64-row.Amount {
			return Document{}, invalid(line, "amount_minor", "amount_overflow")
		}
		result.Total += row.Amount
		result.Rows = append(result.Rows, row)
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		if input.Kind == "refund" {
			return result.Rows[i].RefundID < result.Rows[j].RefundID
		}
		return result.Rows[i].PaymentID < result.Rows[j].PaymentID
	})
	canonical, err := json.Marshal(struct {
		Version  string `json:"version"`
		Provider string `json:"provider"`
		Kind     string `json:"kind"`
		Date     string `json:"date"`
		Coverage string `json:"coverage"`
		Rows     []Row  `json:"rows"`
	}{"normalized-csv-v1", result.Provider, result.Kind, result.Date, result.Coverage, result.Rows})
	if err != nil {
		// All fields have already been validated; this is a defensive finite
		// error rather than a channel for returning uploaded content.
		return Document{}, invalid(0, "csv", "invalid_record")
	}
	digest := sha256.Sum256(canonical)
	result.Digest = hex.EncodeToString(digest[:])
	return result, nil
}

func sameHeader(columns []string) bool {
	if len(columns) != len(header) {
		return false
	}
	for i := range header {
		if columns[i] != header[i] {
			return false
		}
	}
	return true
}

func validatePhysicalLines(content string) error {
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
		// A single final record terminator does not introduce an empty record.
		if index == len(lines)-1 && value == "" {
			continue
		}
		if strings.TrimSuffix(value, "\r") == "" {
			return invalid(index+1, "csv", "blank_record")
		}
	}
	return nil
}

func parseRow(values []string, kind string, start, end time.Time, line int) (Row, error) {
	row := Row{PaymentID: values[0], TransactionID: values[1], RefundID: values[2], ProviderRefundID: values[3], Currency: values[5]}
	for index := 0; index < 2; index++ {
		if !referencePattern.MatchString(values[index]) {
			return Row{}, invalid(line, header[index], "invalid_field")
		}
	}
	if kind == "payment" {
		if row.RefundID != "" || row.ProviderRefundID != "" {
			return Row{}, invalid(line, "refund_id", "unexpected_refund_reference")
		}
	} else {
		for index := 2; index < 4; index++ {
			if !referencePattern.MatchString(values[index]) {
				return Row{}, invalid(line, header[index], "invalid_field")
			}
		}
	}
	if !amountPattern.MatchString(values[4]) {
		return Row{}, invalid(line, "amount_minor", "invalid_field")
	}
	amount, err := strconv.ParseInt(values[4], 10, 64)
	if err != nil {
		return Row{}, invalid(line, "amount_minor", "amount_overflow")
	}
	row.Amount = amount
	if row.Currency != "CNY" {
		return Row{}, invalid(line, "currency", "invalid_field")
	}
	if !timePattern.MatchString(values[6]) {
		return Row{}, invalid(line, "occurred_at", "invalid_field")
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, values[6])
	if err != nil {
		return Row{}, invalid(line, "occurred_at", "invalid_field")
	}
	if occurredAt.Before(start) || !occurredAt.Before(end) {
		return Row{}, invalid(line, "occurred_at", "outside_date")
	}
	// Check the precise input against the selected day before normalizing it to
	// the timestamp precision used by the statement store.
	row.OccurredAt = occurredAt.UTC().Truncate(time.Microsecond)
	return row, nil
}
