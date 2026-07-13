//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// TestCollectDoctorFindings_TokenPermissionDenied pins #633: when the
// gateway token lives under a state dir the current user cannot read
// (the shape of a root-owned /var/lib/waired read non-root), the doctor
// must surface a permission finding with a sudo hint — not a
// "missing … run `waired link`" finding, and not a chmod EPERM leaked
// from PathsFor's SecureDir.
func TestCollectDoctorFindings_TokenPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	home := t.TempDir()
	state := t.TempDir()
	secretsDir := filepath.Join(state, "secrets")
	if err := os.Mkdir(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "gateway-token"),
		[]byte("deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Drop all perms on secrets/ so os.Stat of the token yields EACCES.
	if err := os.Chmod(secretsDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secretsDir, 0o700) })

	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535")

	var tok *integration.AuditFinding
	for i := range findings {
		if findings[i].Subject == "gateway token" {
			tok = &findings[i]
			break
		}
	}
	if tok == nil {
		t.Fatal("no gateway token finding emitted")
	}
	if tok.Status != integration.StatusFail {
		t.Errorf("gateway token status = %s, want fail", tok.Status)
	}
	if !strings.Contains(tok.Detail, "permission denied") {
		t.Errorf("detail = %q, want a permission-denied message", tok.Detail)
	}
	if strings.Contains(tok.Detail, "missing:") {
		t.Errorf("detail = %q, must not claim the token is missing", tok.Detail)
	}
	// The elevation hint (sudo on unix) must be present so the operator
	// knows how to recover.
	if !strings.Contains(tok.Detail, "sudo") {
		t.Errorf("detail = %q, want an elevation (sudo) hint", tok.Detail)
	}
}
