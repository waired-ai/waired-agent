package legacycleanup

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPresent covers the exported presence check `waired claude status` uses to
// warn about a lingering retired MITM proxy (waired#750).
func TestPresent(t *testing.T) {
	// A fresh state dir with no on-disk MITM CA does not, by itself, report a
	// legacy proxy. (The hosts-redirect leg is environmental; a clean build /
	// CI host has no waired api.anthropic.com redirect.)
	empty := t.TempDir()
	if Present(empty) {
		t.Fatalf("Present(%q) = true for a state dir with no proxy/ca.crt (and no hosts redirect)", empty)
	}

	// Once the waired-written MITM CA is on disk, Present reports true so status
	// can point the user at `waired claude enable` to sweep it.
	withCA := t.TempDir()
	caPath := filepath.Join(withCA, "proxy", "ca.crt")
	if err := os.MkdirAll(filepath.Dir(caPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Present(withCA) {
		t.Fatalf("Present(%q) = false with proxy/ca.crt present", withCA)
	}
}
