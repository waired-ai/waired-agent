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

// PRODUCT CONTRACT (waired-agent#586; owner ruling 2026-08-09, recorded
// on that issue): an abandoned model question is persisted and reads
// back as a record, not as absence — reporting it ok=false is how a
// restart would turn the abandonment into consent and start the
// download nobody agreed to. Any actual answer replaces it.
func TestPreference_UnansweredRoundTripsAsARecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	if err := SavePreference(path, Preference{Unanswered: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadPreference(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok || !got.Unanswered || got.ModelID != "" || got.None {
		t.Fatalf("got %+v ok=%v, want an Unanswered record read back ok=true", got, ok)
	}

	if err := SavePreference(path, Preference{None: true}); err != nil {
		t.Fatalf("save none: %v", err)
	}
	got, ok, err = LoadPreference(path)
	if err != nil || !ok {
		t.Fatalf("load after answer: %v ok=%v", err, ok)
	}
	if got.Unanswered || !got.None {
		t.Errorf("an answer must replace the abandonment record: %+v", got)
	}
}

// PRODUCT CONTRACT (waired-agent#647 wire-contract table on the issue,
// and waired-agent#627): the file has to say whether a PERSON HERE
// answered, because a bare model id cannot tell an answer apart from a
// control-plane instruction the setup reconciler applied — and both
// consumers of that distinction do the wrong thing when they guess.
func TestPreference_ChosenHereNeedsAnOperatorAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Preference
		want bool
	}{
		{"a model a person chose here", Preference{ModelID: "qwen3.5-2b", Source: PreferenceSourceOperator}, true},
		{"'run without a local model' is an answer too", Preference{None: true, Source: PreferenceSourceOperator}, true},
		{"an instruction the reconciler applied", Preference{ModelID: "qwen3.5-4b", Source: PreferenceSourceDesired}, false},
		// Empty source is a file written before provenance existed. It is
		// UNKNOWN, and both consumers must keep their pre-#647 behaviour
		// rather than assume the friendlier answer.
		{"a record from before this field existed", Preference{ModelID: "qwen3.5-4b"}, false},
		// The question was put and nobody replied — that is what its own
		// field says, and it is not an answer.
		{"an abandoned question", Preference{Unanswered: true, Source: PreferenceSourceOperator}, false},
		{"nothing at all", Preference{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.ChosenHere(); got != tc.want {
				t.Errorf("ChosenHere() = %v, want %v for %+v", got, tc.want, tc.p)
			}
		})
	}
}

// The provenance has to survive the file, not just the struct: the
// control plane reads it a heartbeat later, in another process.
func TestPreference_SourceRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preferred-model.json")
	if err := SavePreference(path, Preference{ModelID: "qwen3.5-2b", Source: PreferenceSourceOperator}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := LoadPreference(path)
	if err != nil || !ok {
		t.Fatalf("load: %v ok=%v", err, ok)
	}
	if !got.ChosenHere() || got.Source != PreferenceSourceOperator {
		t.Errorf("got %+v, want an operator answer", got)
	}

	// An instruction arriving later replaces the whole file, provenance
	// included — otherwise a stale "operator" would outlive the answer it
	// described and keep licensing a desired-state correction.
	if err := SavePreference(path, Preference{ModelID: "qwen3.5-4b", Source: PreferenceSourceDesired}); err != nil {
		t.Fatalf("save desired: %v", err)
	}
	got, _, err = LoadPreference(path)
	if err != nil {
		t.Fatalf("load after instruction: %v", err)
	}
	if got.ChosenHere() {
		t.Errorf("an applied instruction must not read as a local choice: %+v", got)
	}
}

// TestApplyPreferenceOverride_MissingProvenanceStillNamesTheModel pins
// that a record written before Source existed still says WHICH model this
// host is set to serve.
//
// Product contract (waired-ai/waired-agent#779). The two answers a
// preference gives are separable and only one of them needs provenance:
// "did a person here choose this" requires it and correctly answers no
// without it (ChosenHere, #647), while "what is this host set to serve"
// does not. The desired-model reconcile compares against the second, so
// wiring it to the first would leave every host upgraded from a build
// predating Source unable to converge — the agent update alone would not
// fix them, which is exactly how #647's own follow-through was missed.
func TestApplyPreferenceOverride_MissingProvenanceStillNamesTheModel(t *testing.T) {
	pre := Preference{ModelID: "qwen3.5-2b"} // no Source: written before the field
	if pre.ChosenHere() {
		t.Fatalf("a record with no provenance must not read as a local choice: %+v", pre)
	}
	c := &InferenceConfig{PreferredModelID: "qwen3.5-4b"}
	ApplyPreferenceOverride(c, pre)
	if c.PreferredModelID != "qwen3.5-2b" {
		t.Errorf("PreferredModelID = %q, want the file's model even with no provenance", c.PreferredModelID)
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
