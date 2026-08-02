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
	// The fourth source is the host filesystem, so it needs the seam too.
	// This used to t.Skip when the host had a real install, which made
	// the assertion unreachable on every developer machine that has
	// Ollama — exactly where it is most likely to be needed (#386).
	t.Cleanup(SwapCandidatesForTest(nil))

	got, err := ResolveBinary("")
	if err == nil {
		t.Fatalf("ResolveBinary resolved %q with every source sealed, want ErrNotInstalled", got)
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
}
