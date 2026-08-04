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

// TestDetectOllama_SurfacesTheLegacyBundleMarker closes the gap that let #329
// ship: nothing used to assert that DetectOllama populates the managed fields
// at all, so the marker contract was only ever tested one level down. The
// detection must hand the repair path the exact file to delete.
func TestDetectOllama_SurfacesTheLegacyBundleMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub setup not implemented for windows")
	}
	root := t.TempDir()
	app := filepath.Join(root, "Applications", "Ollama.app")
	stub := filepath.Join(app, "Contents", "Resources", "ollama")
	if err := os.MkdirAll(filepath.Dir(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ollama version is 9.9.9\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(app, WairedManagedMarkerName)
	if err := os.WriteFile(marker, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	t.Setenv("WAIRED_OLLAMA_BINARY", stub)

	det := DetectOllama(context.Background(), filepath.Join(root, "state"))
	if !det.Installed {
		t.Fatalf("Installed = false, want true")
	}
	if det.LegacyBundleMarkerPath != marker {
		t.Errorf("LegacyBundleMarkerPath = %q, want %q", det.LegacyBundleMarkerPath, marker)
	}
	// Only macOS treats the in-bundle marker as proof of ownership; the other
	// two answer with the same-dir marker, which this layout does not have.
	if want := runtime.GOOS == "darwin"; det.WairedManaged != want {
		t.Errorf("WairedManaged = %v on %s, want %v", det.WairedManaged, runtime.GOOS, want)
	}
}
