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

// PRODUCT CONTRACT (waired-agent#586; owner-ruled 2026-08-08,
// waired-ai/waired#1067): a None record is a stated choice, not an empty
// file — LoadPreference must report it ok=true, or every boot would read
// "no preference" and re-arm the fallback download the choice stands down.
func TestPreference_NoneRoundTripsAsAStatedChoice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	if err := SavePreference(path, Preference{None: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadPreference(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatalf("a None record must be ok=true, not 'no preference'")
	}
	if !got.None || got.ModelID != "" {
		t.Errorf("got %+v, want None=true with no model", got)
	}

	// A later model choice overwrites the whole file: none is gone.
	if err := SavePreference(path, Preference{ModelID: "qwen3-4b-instruct"}); err != nil {
		t.Fatalf("save model: %v", err)
	}
	got, ok, err = LoadPreference(path)
	if err != nil || !ok {
		t.Fatalf("load after model choice: %v ok=%v", err, ok)
	}
	if got.None || got.ModelID != "qwen3-4b-instruct" {
		t.Errorf("model choice must replace none: %+v", got)
	}
}

// ApplyPreferenceOverride deliberately ignores a None record: it names no
// model, and the fallback stand-down is the provider's job (#586).
func TestApplyPreferenceOverride_NoneChangesNothing(t *testing.T) {
	c := &InferenceConfig{PreferredModelID: "qwen2.5-coder-7b-instruct", BundledModelID: "qwen3-4b-instruct"}
	ApplyPreferenceOverride(c, Preference{None: true})
	if c.PreferredModelID != "qwen2.5-coder-7b-instruct" || c.BundledModelID != "qwen3-4b-instruct" {
		t.Errorf("none must leave the config untouched, got %+v", c)
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
