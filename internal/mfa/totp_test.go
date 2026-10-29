package mfa_test

import (
	"bytes"
	"encoding/base32"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wjgppx/xui/internal/mfa"
)

func TestTOTPRFC6238VectorsWindowAndReplay(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, v := range []struct {
		at   int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		code, err := mfa.Code(secret, time.Unix(v.at, 0))
		if err != nil || code != v.code {
			t.Fatal(v.at, code, err)
		}
		counter, err := mfa.Match(secret, code, time.Unix(v.at, 0), -1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = mfa.Match(secret, code, time.Unix(v.at, 0), counter); !errors.Is(err, mfa.ErrInvalid) {
			t.Fatal("counter replay accepted", err)
		}
	}
	now := time.Unix(1234567890, 0)
	code, _ := mfa.Code(secret, now)
	if _, err := mfa.Match(secret, code, now.Add(30*time.Second), -1); err != nil {
		t.Fatal("adjacent window lost", err)
	}
	if _, err := mfa.Match(secret, code, now.Add(60*time.Second), -1); !errors.Is(err, mfa.ErrInvalid) {
		t.Fatal("wide window accepted", err)
	}
	for _, bad := range []string{"12345", "1234567", " 123456", "1e0000", "１２３４５６"} {
		if _, err := mfa.Match(secret, bad, now, -1); !errors.Is(err, mfa.ErrInvalid) {
			t.Fatal(err)
		}
	}
}
func TestMFAEncryptionBindsUserAndRejectsTamperingAndWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	secret, err := mfa.GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := mfa.Encrypt(key, "user-a", secret)
	if err != nil || bytes.Contains(encrypted, []byte(secret)) {
		t.Fatal("secret not protected", err)
	}
	decoded, err := mfa.Decrypt(key, "user-a", encrypted)
	if err != nil || decoded != secret {
		t.Fatal(err)
	}
	for _, v := range []struct {
		key  []byte
		user string
		data []byte
	}{{key, "user-b", encrypted}, {bytes.Repeat([]byte{5}, 32), "user-a", encrypted}, {key, "user-a", encrypted[:4]}} {
		if _, err := mfa.Decrypt(v.key, v.user, v.data); !errors.Is(err, mfa.ErrUnavailable) {
			t.Fatal("invalid ciphertext accepted", err)
		}
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := mfa.Decrypt(key, "user-a", encrypted); !errors.Is(err, mfa.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := mfa.ReadKey(strings.Repeat("a", 63)); err == nil {
		t.Fatal("short key accepted")
	}
	if parsed, err := mfa.ReadKey(strings.Repeat("ab", 32) + "\n"); err != nil || len(parsed) != 32 {
		t.Fatal(err)
	}
}
func TestMFARecoveryCredentialsAreRandomScopedAndURIEscaped(t *testing.T) {
	codes, err := mfa.GenerateRecoveryCodes()
	if err != nil || len(codes) != 10 {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if !mfa.RecoveryCode(code) || seen[code] || mfa.RecoveryHash("a", code) == mfa.RecoveryHash("b", code) {
			t.Fatal("recovery credential invalid")
		}
		seen[code] = true
	}
	secret, _ := mfa.GenerateSecret()
	uri := mfa.EnrollmentURI(secret, "operator+test@example.test")
	if !strings.HasPrefix(uri, "otpauth://totp/") || strings.Contains(uri, " ") || !strings.Contains(uri, "issuer=XUI") {
		t.Fatal("bad local enrollment URI")
	}
}
