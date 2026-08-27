package claudemanaged

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeBOM writes a UTF-8 file with the byte-order mark PowerShell 5.1's
// `Set-Content -Encoding utf8` and Notepad's "UTF-8 with BOM" produce.
func writeBOM(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, body...), 0o644); err != nil {
		t.Fatal(err)
	}
}

const bomSettings = `{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:9472",
    "CLAUDE_CODE_SUBAGENT_MODEL": "waired/subagent",
    "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "200704"
  },
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "waired claude _fallback-hook", "timeout": 5}]}]
  }
}`

// TestReadersTolerateAUTF8BOM is the regression guard for waired-agent#1067.
//
// The failure it removes is worse than "waired cannot read the file": Claude
// Code reads it fine, so ROUTING KEEPS WORKING while every waired surface
// reports it as absent — and the SessionStart hook, whose job is to write the
// /model picker entries, stops. The one surface that would have contradicted
// "waired is not routing Claude Code" is the surface that broke.
func TestReadersTolerateAUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed-settings.json")
	writeBOM(t, path, bomSettings)

	present, baseURL := ViewAt(path)
	if !present || baseURL != "http://127.0.0.1:9472" {
		t.Errorf("ViewAt = (%v, %q), want the base URL a BOM-free file yields", present, baseURL)
	}
	if got := SubagentModelAt(path); got != "waired/subagent" {
		t.Errorf("SubagentModelAt = %q", got)
	}
	if got := MaxContextTokensAt(path); got != "200704" {
		t.Errorf("MaxContextTokensAt = %q", got)
	}
	if got := StopHookCommandAt(path); got != "waired claude _fallback-hook" {
		t.Errorf("StopHookCommandAt = %q", got)
	}
	// The write path too: without this, an operator whose editor added the mark
	// could not be repaired by `waired claude enable` either.
	obj, err := readObject(path)
	if err != nil {
		t.Fatalf("readObject on a BOM'd file: %v", err)
	}
	if _, ok := obj["env"].(map[string]any); !ok {
		t.Errorf("readObject lost the env block: %v", obj)
	}
}

// TestUnparseableIsNotTheSameAsUnset is the other half of waired-agent#1067.
// A file waired cannot parse and a file that simply sets no base URL both
// yield "", and reporting them the same way is what turned a broken read into
// the confident, wrong statement "ANTHROPIC_BASE_URL: (not set)".
func TestUnparseableIsNotTheSameAsUnset(t *testing.T) {
	dir := t.TempDir()

	unset := filepath.Join(dir, "unset.json")
	if err := os.WriteFile(unset, []byte(`{"env":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"env": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.json")

	for _, tc := range []struct {
		name        string
		path        string
		wantPresent bool
		wantErr     bool
	}{
		{"a file that sets nothing", unset, true, false},
		{"a file that cannot be parsed", broken, true, true},
		{"no file at all", absent, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			present, baseURL, err := ViewDetailAt(tc.path)
			if present != tc.wantPresent {
				t.Errorf("present = %v, want %v", present, tc.wantPresent)
			}
			if baseURL != "" {
				t.Errorf("baseURL = %q, want empty", baseURL)
			}
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, errSettingsUnparseable) {
				t.Errorf("err = %v, want it to name the unparseable state", err)
			}
		})
	}
}

// TestRemoveLeavesAnUnparseableFileAlone: tolerating a BOM must not turn into
// rewriting documents waired does not understand. The posture on a file it
// cannot parse is unchanged — hands off.
func TestRemoveLeavesAnUnparseableFileAlone(t *testing.T) {
	path := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const broken = `{"env": {`
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := RemoveWithOptions(RemoveOptions{})
	if err != nil || changed {
		t.Fatalf("RemoveWithOptions = (%v, %v), want (false, nil)", changed, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != broken {
		t.Errorf("file was rewritten: %q", got)
	}
}
