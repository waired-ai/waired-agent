package openclaw

import (
	"bytes"
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

// The template is embedded at BUILD time, and Git for Windows checks a text
// file out with CRLF endings by default (this repo has no .gitattributes to
// say otherwise). So the bytes go:embed carries — and therefore the plugin
// file written on a Windows host — end their lines with "\r\n", and every
// reader that anchors a line to its end has to survive that.
//
// Found by the Windows unit-test leg on waired-agent#1306 and not by any
// Linux one, which is the shape §Cross-OS parity warns about: the run on the
// developer's machine cannot see it. Written here as a plain byte
// substitution so the Linux run does.
func TestReadersSurviveCRLFLineEndings(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", 200704, pluginRows(nil))
	if err != nil {
		t.Fatal(err)
	}
	crlf := bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
	home := t.TempDir()
	if err := os.MkdirAll(PluginDir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PluginEntryFile(home), crlf, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := DeclaredContextWindow(home); !ok || got != 200704 {
		t.Errorf("DeclaredContextWindow = %d, %v — the window line is unreadable with CRLF endings", got, ok)
	}
	if got := declaredRefs(home); len(got) != 1 || got[0] != "waired/default" {
		t.Errorf("declaredRefs = %v — the rows line is unreadable with CRLF endings", got)
	}
	if r := detect.OpenClaw(home, "http://127.0.0.1:9473/v1"); !r.Configured || r.Stale {
		t.Errorf("the detector cannot read a CRLF plugin: %+v", r)
	}
}
