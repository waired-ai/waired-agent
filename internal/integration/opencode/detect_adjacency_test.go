package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
)

// The detector reads the plugin this package writes, and the two live in
// different packages with no compiler relationship: detect matches a regular
// expression against the file, and the file comes from a template. Every
// existing detector test writes its own hand-made fixture, so a template edit
// that moved the matched literal would leave every one of them green while the
// Waired app, `waired doctor` and the installation audit all reported "not
// configured" on a correctly installed plugin.
//
// waired-agent#1306 moved that literal: the plugin now carries the base URL
// twice, once as a constant it fetches the model listing with and once in the
// provider options where the detector looks.
func TestRenderedPluginIsWhatTheDetectorReads(t *testing.T) {
	const base = "http://127.0.0.1:9473"
	body, err := renderPlugin(base)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode", "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "waired.js"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	r := detect.OpenCode(home, base+"/v1")
	if !r.Configured {
		t.Fatalf("the detector does not recognise the plugin this package writes: %+v", r)
	}
	if r.Stale {
		t.Errorf("a freshly written plugin reads as stale: %+v", r)
	}
	if r.CurrentValue != base+"/v1" {
		t.Errorf("CurrentValue = %q, want %q", r.CurrentValue, base+"/v1")
	}

	// And it still tells a plugin pointing somewhere else apart.
	if r := detect.OpenCode(home, "http://127.0.0.1:19473/v1"); !r.Stale {
		t.Errorf("a plugin on the wrong port did not read as stale: %+v", r)
	}
}
