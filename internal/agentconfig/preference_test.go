package agentconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPreferenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")

	if _, ok, err := LoadPreference(path); err != nil || ok {
		t.Fatalf("missing file: want (false, nil), got (%v, %v)", ok, err)
	}

	want := Preference{ModelID: "qwen3-4b-instruct", SetAt: time.Date(2026, 5, 9, 8, 55, 0, 0, time.UTC)}
	if err := SavePreference(path, want); err != nil {
		t.Fatalf("SavePreference: %v", err)
	}

	got, ok, err := LoadPreference(path)
	if err != nil {
		t.Fatalf("LoadPreference: %v", err)
	}
	if !ok {
		t.Fatalf("LoadPreference: want ok=true after save")
	}
	if got.ModelID != want.ModelID {
		t.Errorf("ModelID: got %q, want %q", got.ModelID, want.ModelID)
	}
	if !got.SetAt.Equal(want.SetAt) {
		t.Errorf("SetAt: got %v, want %v", got.SetAt, want.SetAt)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows ignores the Go file-mode bits and reports 0o666
		// for any file Go writes; permission enforcement comes from
		// the NTFS ACL applied to the parent directory.
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("permissions: got %o, want 0600", mode)
		}
	}
}

func TestPreference_EmptyModelIDIsNoPreference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	if err := os.WriteFile(path, []byte(`{"model_id": ""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := LoadPreference(path)
	if err != nil {
		t.Fatalf("LoadPreference: %v", err)
	}
	if ok {
		t.Errorf("present-but-empty file should be reported as 'no preference'")
	}
}

func TestPreference_MalformedReportsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ok, err := LoadPreference(path)
	if err == nil {
		t.Fatalf("expected parse error, got ok=%v", ok)
	}
}

func TestPreference_SaveAutoFillsSetAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	before := time.Now().UTC().Add(-time.Second)
	if err := SavePreference(path, Preference{ModelID: "x"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadPreference(path)
	if err != nil || !ok {
		t.Fatalf("load: %v ok=%v", err, ok)
	}
	if got.SetAt.Before(before) {
		t.Errorf("SetAt %v should be >= %v", got.SetAt, before)
	}
}

func TestApplyPreferenceOverride(t *testing.T) {
	c := &InferenceConfig{PreferredModelID: "qwen2.5-coder-7b-instruct"}
	ApplyPreferenceOverride(c, Preference{ModelID: "qwen3-4b-instruct"})
	if c.PreferredModelID != "qwen3-4b-instruct" {
		t.Errorf("expected override to win, got %q", c.PreferredModelID)
	}

	c = &InferenceConfig{PreferredModelID: "qwen2.5-coder-7b-instruct"}
	ApplyPreferenceOverride(c, Preference{}) // empty: leave as-is
	if c.PreferredModelID != "qwen2.5-coder-7b-instruct" {
		t.Errorf("empty preference must not clobber existing config, got %q", c.PreferredModelID)
	}
}
