package claudemanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeJSON re-serializes obj to path (test helper for simulating operator edits).
func writeJSON(t *testing.T, path string, obj map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stopEntries returns managed-settings.json's hooks.Stop array (nil if absent).
func stopEntries(t *testing.T, path string) []any {
	t.Helper()
	obj := readJSON(t, path)
	hooks, ok := obj["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	stop, _ := hooks["Stop"].([]any)
	return stop
}

// The Stop hook is retired: it announced a turn that had fallen back to the
// real Anthropic API, and nothing falls back
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313). These two rows are INVERTED from
// "Write installs it" / "re-Write does not duplicate it": Write must now take
// a leftover away, so a host upgrading past the retirement stops running a
// command that no longer exists on every turn-end.

func TestWriteRemovesARetiredStopHook(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"waired claude _fallback-hook"}]}]}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if stop := stopEntries(t, p); len(stop) != 0 {
		t.Errorf("hooks.Stop = %v, want the retired entry gone", stop)
	}
	if StopHookInstalled() {
		t.Error("StopHookInstalled() = true after Write; the hook is retired")
	}
}

func TestWriteInstallsNoStopHook(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	if stop := stopEntries(t, p); len(stop) != 0 {
		t.Fatalf("Write installed a Stop hook: %v", stop)
	}
}

func TestWritePreservesForeignStopHook(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Operator already has their own Stop hook and a PreToolUse hook.
	seed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/my-stop"}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard"}]}]}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	obj := readJSON(t, p)
	hooks := obj["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("Write clobbered operator's PreToolUse hook")
	}
	stop := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("Stop should hold the operator's entry alone, got %v", stop)
	}
	if isWairedStopEntry(stop[0]) {
		t.Errorf("the surviving Stop entry is ours, not the operator's: %v", stop[0])
	}
}

func TestRemoveStripsHookLeavesForeign(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/my-stop"}]}]}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("removed=false, want true")
	}
	stop := stopEntries(t, p)
	if len(stop) != 1 {
		t.Fatalf("expected only the operator's Stop entry to remain, got %v", stop)
	}
	if isWairedStopEntry(stop[0]) {
		t.Error("waired Stop entry survived Remove")
	}
	if StopHookInstalled() {
		t.Error("StopHookInstalled() = true after Remove")
	}
}

// Remove must strip the retired Stop hook even when the base URL is
// operator-owned (so the combined artifact is fully cleaned up).
//
// The hook is seeded by hand rather than by Write. Write installs no Stop
// hook — it is retired, and Write takes a leftover away — so a version of
// this test that let Write produce the artifact would be asserting the
// removal of something that was never there. The state it is really about is
// a host that enabled before the retirement and has one on disk.
func TestRemoveStripsHookEvenWithForeignBaseURL(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	// Operator repoints the base URL to their own gateway after enable, and
	// the machine still carries the retired hook.
	obj := readJSON(t, p)
	obj["env"].(map[string]any)["ANTHROPIC_BASE_URL"] = "https://gw.corp.example/v1"
	obj["hooks"] = map[string]any{"Stop": []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "waired claude _fallback-hook"}},
	}}}
	writeJSON(t, p, obj)

	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("removed=false; the Stop hook should still have been stripped")
	}
	if StopHookInstalled() {
		t.Error("Stop hook survived Remove")
	}
	// The operator's non-loopback URL must remain untouched.
	if got := readJSON(t, p)["env"].(map[string]any)["ANTHROPIC_BASE_URL"]; got != "https://gw.corp.example/v1" {
		t.Errorf("operator base URL modified: %v", got)
	}
}

func TestRemoveDeletesFileWhenHookAndURLAreSoleContent(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be gone after removing waired's sole content, stat err=%v", err)
	}
}
