package openclaw

import (
	"os"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/detect"
)

// The detector reads the plugin this package writes, and the two live in
// different packages with no compiler relationship: detect matches a regular
// expression against index.mjs, and index.mjs comes from a template. Every
// existing detector test writes its own hand-made fixture, so a template edit
// that moved the matched literal would leave every one of them green while the
// Waired app, `waired doctor` and the installation audit all reported "not
// configured" on a correctly installed plugin.
//
// The same applies to the window line topup.go reads back
// (declaredWindowRe) and, since waired-agent#1306, to the rows line
// (declaredModelsRe).
func TestRenderedPluginIsWhatTheReadersRead(t *testing.T) {
	const base = "http://127.0.0.1:9473"
	rows := pluginRows(nil)
	body, err := renderEntry(base, 200704, rows)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(PluginDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PluginEntryFile(home), body, 0o644); err != nil {
		t.Fatal(err)
	}

	r := detect.OpenClaw(home, base+"/v1")
	if !r.Configured || r.Stale {
		t.Fatalf("the detector does not recognise the plugin this package writes: %+v", r)
	}
	if r.CurrentValue != base+"/v1" {
		t.Errorf("CurrentValue = %q, want %q", r.CurrentValue, base+"/v1")
	}

	if got, ok := DeclaredContextWindow(home); !ok || got != 200704 {
		t.Errorf("DeclaredContextWindow = %d, %v — the window line and its reader moved apart", got, ok)
	}
	if got := declaredRefs(home); len(got) != 1 || got[0] != "waired/default" {
		t.Errorf("declaredRefs = %v — the rows line and its reader moved apart", got)
	}
}
