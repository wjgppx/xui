// Package mfa implements local TOTP and authenticated encryption for staff
// second-factor secrets. Secrets and recovery codes never enter error strings.
package mfa

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("invalid second-factor credential")
var ErrUnavailable = errors.New("second-factor encryption unavailable")
var codePattern = regexp.MustCompile(`^[0-9]{6}$`)
var recoveryPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var secretPattern = regexp.MustCompile(`^[A-Z2-7]{32}$`)

func GenerateSecret() (string, error) {
	data := make([]byte, 20)
	if _, err := rand.Read(data); err != nil {
		return "", ErrUnavailable
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(data), nil
}
func Code(secret string, at time.Time) (string, error) {
	if at.Unix() < 0 {
		return "", ErrInvalid
	}
	return codeForCounter(secret, at.Unix()/30)
}
func codeForCounter(secret string, counter int64) (string, error) {
	if !secretPattern.MatchString(secret) || counter < 0 {
		return "", ErrInvalid
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return "", ErrInvalid
	}
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(data[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000), nil
}

// Match accepts one adjacent time step in each direction and returns only a
// counter newer than the last successfully consumed counter. The database must
// serialize checking/updating lastCounter across sessions and processes.
func Match(secret, code string, at time.Time, lastCounter int64) (int64, error) {
	if !codePattern.MatchString(code) || at.Unix() < 0 {
		return 0, ErrInvalid
	}
	current := at.Unix() / 30
	for counter := current + 1; counter >= current-1 && counter >= 0; counter-- {
		expected, err := codeForCounter(secret, counter)
		if err != nil {
			return 0, err
		}
		if counter > lastCounter && subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return counter, nil
		}
	}
	return 0, ErrInvalid
}
func TOTPCode(value string) bool     { return codePattern.MatchString(value) }
func RecoveryCode(value string) bool { return recoveryPattern.MatchString(value) }
func RecoveryHash(userID, code string) string {
	sum := sha256.Sum256([]byte(userID + "\x00" + code))
	return hex.EncodeToString(sum[:])
}
func GenerateRecoveryCodes() ([]string, error) {
	codes := make([]string, 10)
	for i := range codes {
		data := make([]byte, 16)
		if _, err := rand.Read(data); err != nil {
			return nil, ErrUnavailable
		}
		codes[i] = hex.EncodeToString(data)
	}
	return codes, nil
}
func EnrollmentURI(secret, email string) string {
	values := url.Values{"secret": {secret}, "issuer": {"XUI"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return (&url.URL{Scheme: "otpauth", Host: "totp", Path: "/XUI:" + email, RawQuery: values.Encode()}).String()
}
func Encrypt(key []byte, userID, secret string) ([]byte, error) {
	if len(key) != 32 || userID == "" || !secretPattern.MatchString(secret) {
		return nil, ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrUnavailable
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, ErrUnavailable
	}
	return gcm.Seal(nonce, nonce, []byte(secret), []byte("xui-staff-mfa-v1\x00"+userID)), nil
}
func Decrypt(key []byte, userID string, data []byte) (string, error) {
	if len(key) != 32 || userID == "" {
		return "", ErrUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", ErrUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", ErrUnavailable
	}
	value, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte("xui-staff-mfa-v1\x00"+userID))
	if err != nil || !secretPattern.MatchString(string(value)) {
		return "", ErrUnavailable
	}
	return string(value), nil
}

// ReadKey accepts a textual key file with exactly 32 decoded bytes. Whitespace
// around a hex key is allowed so deployment secret files may end with a newline.
func ReadKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return nil, ErrUnavailable
	}
	key, err := hex.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, ErrUnavailable
	}
	return key, nil
}
