//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
)

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

// TestStateDiskAnswerFor_SystemWide pins the shape measured on sv-mag and
// pc-mbp14-m5 (waired-agent#1005): the caller's own state dir is readable
// and empty, and the system-wide one holds the identity behind 0700
// root/service ownership. Nothing about that is a missing identity.
//
// This is a product contract from #1005, not a record of today's
// behaviour. Unix-only because os.Chmod(dir, 0) is a no-op on Windows;
// the same locked-dir case is exercised there by the NTFS DACL that
// installtest asserts for #751.
func TestStateDiskAnswerFor_SystemWide(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	sys := t.TempDir()
	if err := os.WriteFile(filepath.Join(sys, "identity.json"),
		[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sys, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sys, 0o700) })

	got, gotDir := stateDiskAnswerFor(t.TempDir(), sys, "linux")
	if got != diskSystemWide {
		t.Fatalf("answer = %v, want diskSystemWide — an empty per-user dir next to a locked enrolled system dir", got)
	}
	if gotDir != sys {
		t.Errorf("sysDir = %q, want %q so the row can name it", gotDir, sys)
	}
}

// TestStateDiskAnswerFor_ReadableCases covers the answers that need no
// permission trick, so the classifier is pinned on every platform's
// runner too. Record of today's behaviour, except the last case, which is
// the #800 contract.
func TestStateDiskAnswerFor_ReadableCases(t *testing.T) {
	withIdentity := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "identity.json"),
			[]byte(`{"device_id":"did_x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("identity in the dir doctor was given", func(t *testing.T) {
		if got, _ := stateDiskAnswerFor(withIdentity(t), t.TempDir(), "linux"); got != diskHasIdentity {
			t.Errorf("answer = %v, want diskHasIdentity", got)
		}
	})
	// An elevated Windows run resolves to an empty %AppData% first and
	// reaches the enrolled system dir through the fallback.
	t.Run("identity in a readable system dir", func(t *testing.T) {
		if got, _ := stateDiskAnswerFor(t.TempDir(), withIdentity(t), "windows"); got != diskHasIdentity {
			t.Errorf("answer = %v, want diskHasIdentity", got)
		}
	})
	// Unix root: the dir doctor was given IS the system dir, so there is
	// no second place to look and #800 keeps its row.
	t.Run("empty, and it is the system dir", func(t *testing.T) {
		dir := t.TempDir()
		if got, _ := stateDiskAnswerFor(dir, dir, "linux"); got != diskAbsent {
			t.Errorf("answer = %v, want diskAbsent", got)
		}
	})
	t.Run("empty, and the system dir is empty too", func(t *testing.T) {
		if got, _ := stateDiskAnswerFor(t.TempDir(), t.TempDir(), "linux"); got != diskAbsent {
			t.Errorf("answer = %v, want diskAbsent", got)
		}
	})
}

// TestCollectDoctorFindings_UnreadableStateDirIsNotAMissingIdentity is the
// wiring assert for #1005: the classifier above only matters if
// collectDoctorFindings actually routes through it. With the daemon
// answering "enrolled" and the state dir refusing the read, the run used
// to print
//
//	✗ state directory — … identity is missing from disk … run `waired init` …
//
// and exit 1. It must now report a skipped check and leave the exit code
// alone.
//
// This is a product contract from #1005, not a record of today's behaviour.
func TestCollectDoctorFindings_UnreadableStateDirIsNotAMissingIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}
	restore := daemonIdentity
	daemonIdentity = func(string) *management.IdentityView {
		return &management.IdentityView{Enrolled: true}
	}
	t.Cleanup(func() { daemonIdentity = restore })

	state := t.TempDir()
	// The runner's own /var/lib/waired (or %ProgramData%) must not decide
	// this: point the system-dir lookup at the same locked dir, which is
	// the service-install shape anyway.
	t.Setenv(paths.EnvOverride, state)
	if err := os.Chmod(state, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })

	findings := collectDoctorFindings(t.Context(), t.TempDir(), state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{}, claudeDoctor{})

	var row *integration.AuditFinding
	for i := range findings {
		if findings[i].Subject == "state directory" {
			row = &findings[i]
		}
	}
	if row == nil {
		t.Fatal("no `state directory` row; a check that could not run must say so")
	}
	if row.Status != integration.StatusFail {
		if strings.Contains(row.Detail, "waired init") {
			t.Errorf("detail = %q, must not tell the user to re-enroll a device whose identity was never read", row.Detail)
		}
	}
	if row.Status == integration.StatusFail {
		t.Errorf("status = fail (%q) — a permission error is not an emptied state dir", row.Detail)
	}
	// Other rows legitimately fail on a locked state dir, so the assert is
	// that THIS row adds nothing to the count — a skip never does.
	if n := countFails([]integration.AuditFinding{*row}); n != 0 {
		t.Errorf("the `state directory` row counts as %d failure(s); a check that could not run must not move the exit code", n)
	}
}

// TestCollectDoctorFindings_EmptyStateDirStillReportsTheGap keeps #800
// reachable through the same path: a readable, empty state dir with an
// enrolled daemon is still the split-brain that check exists for.
//
// This is a product contract from #800, not a record of today's behaviour.
func TestCollectDoctorFindings_EmptyStateDirStillReportsTheGap(t *testing.T) {
	restore := daemonIdentity
	daemonIdentity = func(string) *management.IdentityView {
		return &management.IdentityView{Enrolled: true}
	}
	t.Cleanup(func() { daemonIdentity = restore })

	state := t.TempDir()
	t.Setenv(paths.EnvOverride, state)
	findings := collectDoctorFindings(t.Context(), t.TempDir(), state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{}, claudeDoctor{})

	for _, f := range findings {
		if f.Subject != "state directory" {
			continue
		}
		if f.Status != integration.StatusFail {
			t.Errorf("status = %v, want fail: the daemon is signed in and the disk is genuinely empty (#800)", f.Status)
		}
		if !strings.Contains(f.Detail, "waired init") {
			t.Errorf("detail = %q, want the `waired init` recovery (#800)", f.Detail)
		}
		return
	}
	t.Fatal("no `state directory` row; #800 needs it")
}
