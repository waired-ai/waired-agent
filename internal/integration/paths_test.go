package integration

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathsFor_CreatesTreeWithModes(t *testing.T) {
	dir := t.TempDir()
	p, err := PathsFor(dir)
	if err != nil {
		t.Fatalf("PathsFor: %v", err)
	}

	if p.GatewayToken != filepath.Join(dir, "secrets", "gateway-token") {
		t.Fatalf("GatewayToken path = %s", p.GatewayToken)
	}
	if p.Ledger != filepath.Join(dir, "integrations", "applied.json") {
		t.Fatalf("Ledger path = %s", p.Ledger)
	}

	if runtime.GOOS == "windows" {
		return
	}
	si, err := os.Stat(filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode().Perm() != 0o700 {
		t.Fatalf("secrets/ mode = %o, want 0700", si.Mode().Perm())
	}
}

func TestPathsFor_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := PathsFor(dir); err != nil {
		t.Fatal(err)
	}
	// Second call must not error even though dirs already exist.
	if _, err := PathsFor(dir); err != nil {
		t.Fatalf("second PathsFor: %v", err)
	}
}

// TestPathsUnder_NoFilesystemSideEffects guards the read-path contract:
// PathsUnder must compute the layout without creating or chmod-ing any
// directory, so a status query against a root-owned dir surfaces the
// real permission error rather than a chmod EPERM (#633).
func TestPathsUnder_NoFilesystemSideEffects(t *testing.T) {
	// A non-existent subdir of TempDir — PathsUnder must not create it.
	dir := filepath.Join(t.TempDir(), "state")

	p, err := PathsUnder(dir)
	if err != nil {
		t.Fatalf("PathsUnder: %v", err)
	}
	if p.GatewayToken != filepath.Join(dir, "secrets", "gateway-token") {
		t.Fatalf("GatewayToken path = %s", p.GatewayToken)
	}
	if p.Ledger != filepath.Join(dir, "integrations", "applied.json") {
		t.Fatalf("Ledger path = %s", p.Ledger)
	}

	for _, sub := range []string{"", "secrets", "integrations"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); !os.IsNotExist(err) {
			t.Fatalf("PathsUnder created %q (stat err = %v), want no side effects", sub, err)
		}
	}
}

func TestPathsUnder_EmptyErrors(t *testing.T) {
	if _, err := PathsUnder(""); err == nil {
		t.Fatal("PathsUnder(\"\") = nil error, want error")
	}
}
