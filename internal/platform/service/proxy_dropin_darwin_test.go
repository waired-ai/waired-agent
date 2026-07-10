package service

import (
	"os"
	"strings"
	"testing"
)

// TestRemoveProxyDropInBootsOutAndDeletes verifies the legacy-cleanup path
// boots out both retired proxy LaunchDaemons and deletes their plists.
func TestRemoveProxyDropInBootsOutAndDeletes(t *testing.T) {
	dir := t.TempDir()
	orig := systemDaemonDir
	systemDaemonDir = dir
	t.Cleanup(func() { systemDaemonDir = orig })
	f := withFakeLaunchctl(t)

	// Seed both legacy plists as a prior install would have.
	for _, p := range []string{proxySocketPlistPath(), proxyConvergePlistPath()} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := RemoveProxyDropIn(); err != nil {
		t.Fatalf("RemoveProxyDropIn: %v", err)
	}

	for _, p := range []string{proxySocketPlistPath(), proxyConvergePlistPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("plist %s not removed: %v", p, err)
		}
	}
	var sawBootout int
	for _, c := range f.calls {
		if len(c) >= 2 && c[0] == "bootout" && strings.HasPrefix(c[1], "system/com.waired.proxy-") {
			sawBootout++
		}
	}
	if sawBootout != 2 {
		t.Errorf("want 2 bootout calls, got %d (%v)", sawBootout, f.calls)
	}
}

// TestRemoveProxyDropInMissingIsNoError verifies removal tolerates absent plists.
func TestRemoveProxyDropInMissingIsNoError(t *testing.T) {
	dir := t.TempDir()
	orig := systemDaemonDir
	systemDaemonDir = dir
	t.Cleanup(func() { systemDaemonDir = orig })
	withFakeLaunchctl(t)

	if err := RemoveProxyDropIn(); err != nil {
		t.Fatalf("RemoveProxyDropIn on missing plists should succeed, got %v", err)
	}
}
