//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
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

	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{}, claudeDoctor{})

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

// TestCollectDoctorFindings_UnreadableStateIsReportedNotDropped pins #651:
// an unprivileged run of a root-owned state dir used to drop the sign-in
// and phase rows entirely and exit 0 — a clean bill of health from a
// doctor that never examined either. They must now appear as skipped
// checks naming the elevated re-run.
//
// This is a product contract from #651, not a record of today's behaviour.
func TestCollectDoctorFindings_UnreadableStateIsReportedNotDropped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	home := t.TempDir()
	state := t.TempDir()
	// Both checks read files directly under the state dir, so removing
	// all perms from it is the shape of /var/lib/waired read non-root.
	if err := os.Chmod(state, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{}, claudeDoctor{})

	bySubject := map[string]integration.AuditFinding{}
	for _, f := range findings {
		bySubject[f.Subject] = f
	}
	for _, subject := range []string{"device sign-in", "waired phase"} {
		f, ok := bySubject[subject]
		if !ok {
			t.Errorf("%q row is absent; an unreadable check must say it was skipped", subject)
			continue
		}
		if f.Status != integration.StatusSkip {
			t.Errorf("%q status = %s, want skip", subject, f.Status)
		}
		if !strings.Contains(f.Detail, "sudo") {
			t.Errorf("%q detail = %q, want an elevation (sudo) hint", subject, f.Detail)
		}
	}
	// A skipped check is not a failing one: the exit code must not move.
	for _, f := range findings {
		if f.Subject == "device sign-in" || f.Subject == "waired phase" {
			if f.Status == integration.StatusFail {
				t.Errorf("%q must not count as a failure", f.Subject)
			}
		}
	}
}
