package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readSettingsMap(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(SettingsPath(home))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return m
}

func statusLineCmd(t *testing.T, home string) string {
	t.Helper()
	sl, ok := readSettingsMap(t, home)[statuslineKey].(map[string]any)
	if !ok {
		return ""
	}
	c, _ := sl["command"].(string)
	return c
}

func TestInstallStatusLineInjectsWhenAbsent(t *testing.T) {
	home := t.TempDir()
	res, err := InstallStatusLine(home, false)
	if err != nil {
		t.Fatalf("InstallStatusLine: %v", err)
	}
	if res.Action != "injected" {
		t.Errorf("Action = %q, want injected", res.Action)
	}
	if got := statusLineCmd(t, home); got != statuslineRenderCommand {
		t.Errorf("statusLine.command = %q, want %q", got, statuslineRenderCommand)
	}
	kind, _, err := DetectStatusLine(home)
	if err != nil || kind != StatusLineOurs {
		t.Errorf("DetectStatusLine = (%v,%v), want ours", kind, err)
	}
}

func TestInstallStatusLinePreservesUnknownKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"permissions":{"allow":["Bash"]},"model":"opus"}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStatusLine(home, false); err != nil {
		t.Fatal(err)
	}
	m := readSettingsMap(t, home)
	if _, ok := m["permissions"]; !ok {
		t.Error("clobbered permissions")
	}
	if m["model"] != "opus" {
		t.Error("clobbered model")
	}
	if _, ok := m[statuslineKey]; !ok {
		t.Error("statusLine not added")
	}
}

func TestInstallStatusLineIdempotent(t *testing.T) {
	home := t.TempDir()
	if _, err := InstallStatusLine(home, false); err != nil {
		t.Fatal(err)
	}
	res, err := InstallStatusLine(home, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "refreshed" {
		t.Errorf("second install Action = %q, want refreshed", res.Action)
	}
	if got := statusLineCmd(t, home); got != statuslineRenderCommand {
		t.Errorf("command drifted: %q", got)
	}
}

func TestInstallStatusLineForeignSkippedWithoutWrap(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"statusLine":{"type":"command","command":"~/my-statusline.sh"}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := InstallStatusLine(home, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "skipped-foreign" {
		t.Errorf("Action = %q, want skipped-foreign", res.Action)
	}
	if res.Existing != "~/my-statusline.sh" {
		t.Errorf("Existing = %q", res.Existing)
	}
	if got := statusLineCmd(t, home); got != "~/my-statusline.sh" {
		t.Errorf("foreign statusLine was modified: %q", got)
	}
}

func TestInstallStatusLineWrapAndRestore(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"statusLine":{"type":"command","command":"~/my-statusline.sh --flag","padding":2}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := InstallStatusLine(home, true)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if res.Action != "wrapped" {
		t.Fatalf("Action = %q, want wrapped", res.Action)
	}
	if got := statusLineCmd(t, home); got != statuslineWrapperPath(home) {
		t.Errorf("statusLine.command = %q, want wrapper path", got)
	}
	// Wrapper artifacts exist and are executable.
	if fi, err := os.Stat(statuslineWrapperPath(home)); err != nil {
		t.Fatalf("wrapper missing: %v", err)
	} else if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("wrapper not executable: %v", fi.Mode())
	}
	if b, err := os.ReadFile(statuslineOrigPath(home)); err != nil || string(b) != "~/my-statusline.sh --flag\n" {
		t.Errorf(".orig = %q err=%v", b, err)
	}
	// Original object stashed losslessly (padding preserved).
	if _, ok := readSettingsMap(t, home)[statuslineStashKey]; !ok {
		t.Error("original not stashed")
	}
	kind, _, _ := DetectStatusLine(home)
	if kind != StatusLineWrapped {
		t.Errorf("kind = %v, want wrapped", kind)
	}

	// Restore.
	if err := RemoveStatusLine(home); err != nil {
		t.Fatalf("remove: %v", err)
	}
	sl := readSettingsMap(t, home)[statuslineKey].(map[string]any)
	if sl["command"] != "~/my-statusline.sh --flag" {
		t.Errorf("restored command = %v", sl["command"])
	}
	if sl["padding"] != float64(2) {
		t.Errorf("restore lost padding: %v", sl["padding"])
	}
	if _, ok := readSettingsMap(t, home)[statuslineStashKey]; ok {
		t.Error("stash key survived restore")
	}
	if _, err := os.Stat(statuslineWrapperPath(home)); !os.IsNotExist(err) {
		t.Error("wrapper script survived restore")
	}
}

func TestRemoveStatusLineOursDeletesEmptyFile(t *testing.T) {
	home := t.TempDir()
	if _, err := InstallStatusLine(home, false); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStatusLine(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SettingsPath(home)); !os.IsNotExist(err) {
		t.Errorf("settings.json should be gone (sole content), err=%v", err)
	}
}

func TestRemoveStatusLineOursPreservesOtherKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsPath(home), []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallStatusLine(home, false); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStatusLine(home); err != nil {
		t.Fatal(err)
	}
	m := readSettingsMap(t, home)
	if m["model"] != "opus" {
		t.Error("Remove clobbered model")
	}
	if _, ok := m[statuslineKey]; ok {
		t.Error("statusLine survived Remove")
	}
}

func TestRemoveStatusLineLeavesForeign(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"statusLine":{"type":"command","command":"~/mine.sh"}}`
	if err := os.WriteFile(SettingsPath(home), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStatusLine(home); err != nil {
		t.Fatal(err)
	}
	if got := statusLineCmd(t, home); got != "~/mine.sh" {
		t.Errorf("Remove touched a foreign statusLine: %q", got)
	}
}
