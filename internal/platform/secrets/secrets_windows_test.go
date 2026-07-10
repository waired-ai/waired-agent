//go:build windows

package secrets_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// TestWriteSecret_AppliesRestrictiveDACL_Windows verifies via the
// system `icacls` tool that a Secret-written file actually carries a
// protected DACL granting access only to SYSTEM, Administrators, and
// the current user. This is the Windows equivalent of the Unix 0o600
// mode-bit assertion.
func TestWriteSecret_AppliesRestrictiveDACL_Windows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.bin")
	if err := secrets.WriteSecret(path, []byte("x")); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	out, err := exec.Command("icacls", path).Output()
	if err != nil {
		t.Fatalf("icacls: %v", err)
	}
	got := string(out)
	// Two checks: SYSTEM ACE present, AND the "Users" / "Everyone"
	// well-known groups are NOT present (the latter would indicate the
	// permissive parent DACL leaked through).
	if !strings.Contains(got, "NT AUTHORITY\\SYSTEM") && !strings.Contains(got, "SYSTEM:") {
		t.Errorf("expected SYSTEM in icacls output, got:\n%s", got)
	}
	for _, leak := range []string{"BUILTIN\\Users", "\\Everyone", "Authenticated Users"} {
		if strings.Contains(got, leak) {
			t.Errorf("DACL leaked %q (parent inheritance not blocked):\n%s", leak, got)
		}
	}
}

func TestSecureDir_AppliesRestrictiveDACL_Windows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := secrets.SecureDir(dir); err != nil {
		t.Fatalf("SecureDir: %v", err)
	}
	out, err := exec.Command("icacls", dir).Output()
	if err != nil {
		t.Fatalf("icacls: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "SYSTEM") {
		t.Errorf("expected SYSTEM ACE on secure dir, got:\n%s", got)
	}
	for _, leak := range []string{"BUILTIN\\Users", "\\Everyone", "Authenticated Users"} {
		if strings.Contains(got, leak) {
			t.Errorf("DACL leaked %q on secure dir:\n%s", leak, got)
		}
	}
}
