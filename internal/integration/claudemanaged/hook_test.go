package claudemanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestWriteInstallsStopHook(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stop := stopEntries(t, p)
	if len(stop) != 1 {
		t.Fatalf("hooks.Stop = %v, want exactly one waired entry", stop)
	}
	if !isWairedStopEntry(stop[0]) {
		t.Errorf("Stop[0] is not waired's entry: %v", stop[0])
	}
	if !StopHookInstalled() {
		t.Error("StopHookInstalled() = false after Write")
	}
	// The command must self-guard on `command -v waired` and reference the hook.
	inner := stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	cmd, _ := inner["command"].(string)
	if !strings.Contains(cmd, "command -v waired") || !strings.Contains(cmd, fallbackHookMarker) {
		t.Errorf("hook command %q missing self-guard or marker", cmd)
	}
}

func TestWriteIsIdempotentForHook(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	if stop := stopEntries(t, p); len(stop) != 1 {
		t.Fatalf("re-Write duplicated the Stop hook: %v", stop)
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
	if len(stop) != 2 {
		t.Fatalf("Stop should have operator + waired entries, got %v", stop)
	}
	var sawForeign, sawWaired bool
	for _, e := range stop {
		if isWairedStopEntry(e) {
			sawWaired = true
		} else {
			sawForeign = true
		}
	}
	if !sawForeign || !sawWaired {
		t.Errorf("expected both foreign and waired Stop entries, got foreign=%v waired=%v", sawForeign, sawWaired)
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

// Remove must strip the hook even when the base URL is operator-owned (so the
// combined artifact is fully cleaned up).
func TestRemoveStripsHookEvenWithForeignBaseURL(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	// Operator repoints the base URL to their own gateway after enable.
	obj := readJSON(t, p)
	obj["env"].(map[string]any)["ANTHROPIC_BASE_URL"] = "https://gw.corp.example/v1"
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
