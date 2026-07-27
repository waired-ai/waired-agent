package setup

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
)

// TestDetectOllama_ResolvesViaEnvOverride proves DetectOllama no longer
// depends on $PATH: with $PATH stripped but $WAIRED_OLLAMA_BINARY set to
// an off-PATH executable, detection still succeeds and parses the
// version. This is the seam that lets ResolveBinary find macOS
// Ollama.app / Homebrew installs and Windows-service installs that a
// plain LookPath would miss. (#268)
func TestDetectOllama_ResolvesViaEnvOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub setup not implemented for windows")
	}
	dir := t.TempDir()
	stub := dir + "/ollama"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ollama version is 9.9.9\"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", "") // ensure detection cannot come from $PATH.
	t.Setenv("WAIRED_OLLAMA_BINARY", stub)

	det := DetectOllama(context.Background())
	if !det.Installed {
		t.Fatalf("Installed = false, want true (resolved via WAIRED_OLLAMA_BINARY)")
	}
	if det.Path != stub {
		t.Errorf("Path = %q, want %q", det.Path, stub)
	}
	if det.Version != "9.9.9" {
		t.Errorf("Version = %q, want 9.9.9", det.Version)
	}
	if !det.Supported {
		t.Errorf("Supported = false, want true for version 9.9.9")
	}
}

// TestDetectOllama_NotInstalled checks the zero-value path when no
// ollama is resolvable through any of ResolveBinary's sources.
func TestDetectOllama_NotInstalled(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	if _, err := download.ResolveBinary(""); err == nil {
		t.Skip("environment still has a resolvable ollama; cannot test not-found path")
	}
	det := DetectOllama(context.Background())
	if det.Installed {
		t.Errorf("Installed = true, want false; det = %#v", det)
	}
}
