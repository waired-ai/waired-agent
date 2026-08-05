package setup

import (
	"context"
	"os"
	"path/filepath"
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

	det := DetectOllama(context.Background(), t.TempDir())
	if !det.Installed {
		t.Fatalf("Installed = false, want true (resolved via WAIRED_OLLAMA_BINARY)")
	}
	if det.Path != stub {
		t.Errorf("Path = %q, want %q", det.Path, stub)
	}
	if det.Version != "9.9.9" {
		t.Errorf("Version = %q, want 9.9.9", det.Version)
	}
}

// TestDetectOllama_NotInstalled checks the zero-value path when no
// ollama is resolvable through any of ResolveBinary's sources.
//
// The well-known-path source is closed by this package's TestMain, so
// this no longer skips on a machine that has Ollama installed — which is
// where the assertion was most worth running (#386).
func TestDetectOllama_NotInstalled(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", "")
	if got, err := download.ResolveBinary(""); err == nil {
		t.Fatalf("ResolveBinary resolved %q with every source sealed", got)
	}
	det := DetectOllama(context.Background(), t.TempDir())
	if det.Installed {
		t.Errorf("Installed = true, want false; det = %#v", det)
	}
}

// TestDetectOllama_SurfacesTheManagedMarker: nothing used to assert that
// DetectOllama populates the managed field at all, so the marker contract was
// only ever tested one level down — which is how #329 shipped.
//
// It is a Windows-only signal since #492: on Linux and macOS the engine lives
// under the state dir, so an install found anywhere else is by definition not
// waired's, marker or no marker.
func TestDetectOllama_SurfacesTheManagedMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub setup not implemented for windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "Ollama")
	stub := filepath.Join(dir, "ollama")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ollama version is 9.9.9\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, WairedManagedMarkerName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", stub)

	det := DetectOllama(context.Background(), filepath.Join(root, "state"))
	if !det.Installed {
		t.Fatalf("Installed = false, want true")
	}
	if want := runtime.GOOS == "windows"; det.WairedManaged != want {
		t.Errorf("WairedManaged = %v on %s, want %v", det.WairedManaged, runtime.GOOS, want)
	}
}
