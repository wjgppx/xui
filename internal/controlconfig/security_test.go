package controlconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wjgppx/xui/internal/controlconfig"
)

func TestStaffMFADefaultsRequirePrivateStableKeyInProduction(t *testing.T) {
	if required, key, err := controlconfig.StaffMFA(false, "", ""); err != nil || required || key != nil {
		t.Fatal(required, err)
	}
	if _, _, err := controlconfig.StaffMFA(true, "", ""); err == nil {
		t.Fatal("production silently disabled second-factor policy")
	}
	if required, _, err := controlconfig.StaffMFA(true, "false", ""); err != nil || required {
		t.Fatal("explicit migration opt-out was ignored", err)
	}
	path := filepath.Join(t.TempDir(), "mfa.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if required, key, err := controlconfig.StaffMFA(true, "", path); err != nil || !required || len(key) != 32 {
		t.Fatal("valid persisted key rejected", err)
	}
	if _, _, err := controlconfig.StaffMFA(false, "TRUE", path); err == nil {
		t.Fatal("ambiguous policy accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlconfig.StaffMFA(false, "", path); err == nil {
		t.Fatal("public-readable key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := controlconfig.StaffMFA(false, "", link); err == nil {
		t.Fatal("symlink key accepted")
	}
}
