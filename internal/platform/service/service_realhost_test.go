//go:build darwin

package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealLaunchdRoundTrip exercises the live /bin/launchctl against
// a synthetic test job in the system domain. Gated by
// WAIRED_LAUNCHD_REALHOST=1 because it mutates the host's launchd and
// writes to /Library/LaunchDaemons; it additionally requires root
// (the system domain + /Library/LaunchDaemons are root-only), so it
// skips when euid != 0.
//
// Steps:
//
//  1. renderLaunchDaemonPlist with /usr/bin/yes as the program (benign
//     daemon-shaped process).
//  2. plutil -lint to confirm the XML is well-formed.
//  3. darwinManager.Install — writes plist, bootstrap system, enable.
//  4. launchctl print system/com.waired.agent — confirms registration.
//  5. Stop (kill SIGTERM) — graceful.
//  6. Start (kickstart -k) — restarts the job.
//  7. Uninstall — bootout + remove plist.
//  8. stat the plist path — must be gone.
//
// To run manually:
//
//	sudo WAIRED_LAUNCHD_REALHOST=1 go test ./internal/platform/service/ -run RealLaunchd -v
//
// Cleanup is best-effort even on test failure (t.Cleanup runs Uninstall).
func TestRealLaunchdRoundTrip(t *testing.T) {
	if os.Getenv("WAIRED_LAUNCHD_REALHOST") == "" {
		t.Skip("set WAIRED_LAUNCHD_REALHOST=1 to exercise real launchctl")
	}
	if os.Geteuid() != 0 {
		t.Skip("system LaunchDaemon round-trip needs root; re-run under sudo")
	}

	cfg := Config{
		Binary:    "/usr/bin/yes",
		StateDir:  t.TempDir(),
		ExtraArgs: []string{"waired-launchd-test-token"},
	}
	body, err := renderLaunchDaemonPlist(cfg)
	if err != nil {
		t.Fatalf("renderLaunchDaemonPlist: %v", err)
	}

	// 1. plutil -lint to confirm the plist is well-formed.
	lintTmp := filepath.Join(t.TempDir(), "lint.plist")
	if err := os.WriteFile(lintTmp, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", lintTmp).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint failed: %v\n%s", err, out)
	} else {
		t.Logf("plutil -lint OK: %s", strings.TrimSpace(string(out)))
	}

	// 2. Install (writes to /Library/LaunchDaemons/com.waired.agent.plist).
	t.Cleanup(func() { _ = (darwinManager{}).Uninstall() })
	if err := (darwinManager{}).Install(cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Logf("Install OK")

	// 3. launchctl print to confirm the job is registered.
	target := "system/" + darwinLabel
	out, _ := exec.Command("/bin/launchctl", "print", target).CombinedOutput()
	if !strings.Contains(string(out), darwinLabel) {
		t.Fatalf("launchctl print did not show our job:\n%s", out)
	}
	t.Logf("launchctl print OK")

	// 4. Stop (SIGTERM to the running instance).
	if err := (darwinManager{}).Stop(); err != nil {
		t.Logf("Stop: %v (acceptable if /usr/bin/yes already exited via SIGPIPE)", err)
	}

	// 5. Start (kickstart -k restarts the job).
	if err := (darwinManager{}).Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("Start (kickstart) OK")

	// 6. Uninstall — bootout + plist removal.
	if err := (darwinManager{}).Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	t.Logf("Uninstall OK")

	// 7. Confirm the plist file is gone.
	if _, err := os.Stat(systemLaunchDaemonPath(darwinLabel)); err == nil {
		t.Errorf("plist should be removed after Uninstall")
	}
}
