package download

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeStubBinary creates an executable file named like ollama (cmd
// name varies per OS) in dir and returns its path.
func writeStubBinary(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, ollamaCmdName)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}

func TestResolveBinary_OverrideWins(t *testing.T) {
	// Override is returned verbatim without touching $PATH or env.
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "/some/env/ollama")
	got, err := ResolveBinary("/explicit/override")
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if got != "/explicit/override" {
		t.Errorf("got %q, want /explicit/override", got)
	}
}

func TestResolveBinary_EnvWins(t *testing.T) {
	// With no override, $WAIRED_OLLAMA_BINARY beats $PATH discovery.
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "/env/ollama")
	got, err := ResolveBinary("")
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if got != "/env/ollama" {
		t.Errorf("got %q, want /env/ollama", got)
	}
}

func TestResolveBinary_PathLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit PATH stub not portable to windows")
	}
	dir := t.TempDir()
	stub := writeStubBinary(t, dir)
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	t.Setenv("PATH", dir)
	got, err := ResolveBinary("")
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	// LookPath may return an absolute path; compare base + dir.
	if filepath.Dir(got) != dir || filepath.Base(got) != filepath.Base(stub) {
		t.Errorf("got %q, want a path under %q", got, dir)
	}
}

func TestResolveBinary_NotFound(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	// If the host has a real install at an OS well-known candidate path
	// this legitimately resolves; only assert the error shape when it
	// truly cannot be found.
	got, err := ResolveBinary("")
	if err == nil {
		t.Skipf("host has a resolvable ollama at %q; cannot exercise not-found", got)
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}
