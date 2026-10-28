package payments

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// BillProvider is optional. Classic Epay has no common authenticated bill API.
// A downloaded bill remains an observation for reconciliation, not a payment
// notification and never a reason to change financial or entitlement state.
type BillProvider interface {
	BillMerchantFingerprint() string
	DownloadBill(context.Context, BillRequest) (Bill, error)
}

type BillRequest struct{ Date, Kind string }
type Bill struct {
	Provider, Date, Kind, Format, MerchantID string
	Data                                     []byte
}

const MaxBillBytes = 8 << 20

var (
	ErrBillNotReady    = errors.New("merchant bill not ready")
	ErrBillUnsupported = errors.New("merchant bill unsupported")
	billDatePattern    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// ValidBillDate limits remote downloads to the previous ninety Shanghai dates.
// Native/manual import can have a different date policy, independent of this
// deliberately conservative bound on outbound merchant requests.
func ValidBillDate(date string, now time.Time) bool {
	if !billDatePattern.MatchString(date) {
		return false
	}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	day, err := time.ParseInLocation("2006-01-02", date, zone)
	if err != nil || day.Year() < 2000 || day.Year() > 2099 {
		return false
	}
	today := now.In(zone)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, zone)
	return day.Before(today) && !day.Before(today.AddDate(0, 0, -90))
}

func billFingerprint(provider, appID, merchantID string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + appID + "\x00" + merchantID))
	return hex.EncodeToString(digest[:])
}

func validBillKind(kind string) bool { return kind == "payment" || kind == "refund" }

func parseBillURL(raw, path string) (*url.URL, error) {
	if raw == "" || len(raw) > 4096 || strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.ContainsAny(raw, "\\#") {
		return nil, ErrVerification
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.ForceQuery || u.RawPath != "" || u.Path != path || u.EscapedPath() != path {
		return nil, ErrVerification
	}
	return u, nil
}

func sameBillOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// readBillBody reads the exact HTTP entity bytes. Callers request identity
// transfer content encoding so decompression and checksums have one explicit
// protocol-defined order. No error contains a token URL or response content.
func readBillBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxBillBytes {
		return nil, ErrGateway
	}
	encoding := response.Header.Get("Content-Encoding")
	if encoding != "" && encoding != "identity" {
		return nil, ErrGateway
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBillBytes+1))
	if err != nil || len(data) == 0 || len(data) > MaxBillBytes {
		return nil, ErrGateway
	}
	return data, nil
}

// WeChat's official SDK verifies the bill hash after gzip decompression. The
// gzip member must finish with a valid CRC; extra members/trailing data reject.
func expandWechatBill(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return nil, ErrGateway
	}
	source := bytes.NewReader(data)
	reader, err := gzip.NewReader(source)
	if err != nil {
		return nil, ErrGateway
	}
	reader.Multistream(false)
	plain, readErr := io.ReadAll(io.LimitReader(reader, MaxBillBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(plain) == 0 || len(plain) > MaxBillBytes || source.Len() != 0 {
		return nil, ErrGateway
	}
	return plain, nil
}
