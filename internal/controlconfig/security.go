package controlconfig

import (
	"errors"
	"io"
	"os"

	"github.com/wjgppx/xui/internal/mfa"
)

// StaffMFA reads a stable private encryption key. It never invents a startup
// key: losing or changing that key requires an explicit host-console recovery.
func StaffMFA(production bool, required, path string) (bool, []byte, error) {
	policy := production
	switch required {
	case "":
	case "true":
		policy = true
	case "false":
		policy = false
	default:
		return false, nil, errors.New("REQUIRE_STAFF_MFA must be true or false")
	}
	if path == "" {
		if policy {
			return false, nil, errors.New("MFA_KEY_FILE is required by staff MFA policy")
		}
		return policy, nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return false, nil, errors.New("MFA_KEY_FILE must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return false, nil, errors.New("MFA_KEY_FILE could not be opened")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return false, nil, errors.New("MFA_KEY_FILE changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return false, nil, errors.New("MFA_KEY_FILE is invalid")
	}
	key, err := mfa.ReadKey(string(data))
	if err != nil {
		return false, nil, errors.New("MFA_KEY_FILE must contain a 32-byte hex key")
	}
	return policy, key, nil
}
