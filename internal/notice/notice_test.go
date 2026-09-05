package notice

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSanitiseStripsWhatARendererMisreads
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: the notice module itself
// refuses what a renderer would misread). Each case is a thing one of
// the three surfaces gets wrong, not a style preference: a newline
// breaks the one-line contract of a doctor finding and a menu label, an
// escape sequence is EXECUTED by the terminal doctor prints to, a bidi
// control reorders the line a person reads, and a status mark forges the
// verdict the line is supposed to carry.
func TestSanitiseStripsWhatARendererMisreads(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"newline", "one\ntwo", "one two"},
		{"carriage return", "one\r\ntwo", "one two"},
		{"tab", "one\ttwo", "one two"},
		{"ansi escape", "clean\x1b[2Jwiped", "clean [2Jwiped"},
		{"c0 control", "a\x00b", "a b"},
		{"c1 control", "a\u0085b", "a b"},
		{"bidi override", "admin\u202egnp.txt", "admingnp.txt"},
		{"bidi isolate", "\u2066a\u2069b", "ab"},
		{"zero width", "a\u200bb", "ab"},
		{"byte order mark", "\ufeffabc", "abc"},
		{"tick mark", "✓ everything is fine", "everything is fine"},
		{"cross mark", "✗ broken", "broken"},
		{"warning mark", "⚠ careful", "careful"},
		{"state dot", "● ready", "ready"},
		{"leading and trailing space", "  spaced  ", "spaced"},
		{"collapsed run", "a     b", "a b"},
		{"kept as is", "qwen3-8b-instruct answers at 42.1 tok/s.", "qwen3-8b-instruct answers at 42.1 tok/s."},
		{"non-ascii prose is kept", "モデルを切り替えて", "モデルを切り替えて"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sanitise(tc.in); got != tc.want {
				t.Errorf("Sanitise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitiseBoundsLength
//
// PRODUCT CONTRACT (owner ruling above). A menu label is truncated by
// the OS and the Windows status dialog does not scroll, so an unbounded
// notice is a rendering defect rather than a long line.
func TestSanitiseBoundsLength(t *testing.T) {
	got := Sanitise(strings.Repeat("x", 10_000))
	if n := utf8.RuneCountInString(got); n > maxRunes+1 {
		t.Fatalf("length = %d runes, want at most %d plus the ellipsis", n, maxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated string should say so, got %q", got)
	}
}

// TestUnmarshalJSONSanitisesHostileWire
//
// PRODUCT CONTRACT (owner ruling above). The constructors protect the
// producer; this protects the reader. All three surfaces decode this
// type off a socket and render the result, so a daemon that is newer,
// older or simply wrong must not be able to hand a terminal an escape
// sequence — the invariant has to hold on both sides of the wire, not
// only where the prose is composed.
func TestUnmarshalJSONSanitisesHostileWire(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"kind":     "evil\nkind",
		"severity": 9,
		"title":    "\x1b[2J\u2713 all good",
		"text":     "line\nbreak",
		"action":   7,
		"target":   "a\u202eb",
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	var n Notice
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for name, got := range map[string]string{
		"kind": string(n.Kind), "title": n.Title, "text": n.Text, "target": n.Target,
	} {
		if strings.ContainsAny(got, "\n\r\x1b") {
			t.Errorf("%s still carries a control character: %q", name, got)
		}
	}
	if strings.Contains(n.Title, "✓") {
		t.Errorf("title still forges a status mark: %q", n.Title)
	}
	if n.Severity != SeverityInfo {
		t.Errorf("unknown severity should clamp to info, got %v", n.Severity)
	}
	if n.Action != ActionNone {
		t.Errorf("unknown action should clamp to none, got %v", n.Action)
	}
}

// TestConstructorsCarryNoStatusMark
//
// PRODUCT CONTRACT (owner ruling above): the notice module carries no
// glyphs, because each surface adds its own marker — the tray writes one
// into the menu label, the CLI takes one from emo() so it can degrade on
// a console that cannot draw it.
func TestConstructorsCarryNoStatusMark(t *testing.T) {
	for _, n := range []Notice{
		LighterModel("qwen3-30b-a3b", "qwen3-8b-instruct", 42.05, 60),
		BetterModel("qwen3-8b-instruct", "qwen3-30b-a3b", 118, 64.2),
	} {
		for _, s := range []string{n.Title, n.Text, n.Target} {
			for _, r := range s {
				if statusMark(r) {
					t.Errorf("%s: %q carries the status mark %q", n.Kind, s, string(r))
				}
			}
		}
	}
}

// TestConstructorsComposeTheShippedWording records today's behaviour:
// these are the strings the tray already showed before the suggestion
// moved into the notice field (internal/gui/tray/state.go), minus the
// marker each surface now adds for itself.
func TestConstructorsComposeTheShippedWording(t *testing.T) {
	l := LighterModel("qwen3-30b-a3b", "qwen3-8b-instruct", 42.05, 60)
	if want := "Lighter model recommended — switch to qwen3-8b-instruct"; l.Title != want {
		t.Errorf("lighter title = %q, want %q", l.Title, want)
	}
	if want := "This computer answers at 42 tok/s with qwen3-30b-a3b, below the 60 tok/s floor."; l.Text != want {
		t.Errorf("lighter text = %q, want %q", l.Text, want)
	}
	if l.Severity != SeverityWarn || l.Action != ActionModelSuggestion || l.Target != "qwen3-8b-instruct" {
		t.Errorf("lighter = %+v", l)
	}

	u := BetterModel("qwen3-8b-instruct", "qwen3-30b-a3b", 118, 64.2)
	if want := "Better model available — switch to qwen3-30b-a3b"; u.Title != want {
		t.Errorf("upgrade title = %q, want %q", u.Title, want)
	}
	if u.Severity != SeverityInfo {
		t.Errorf("a step-up suggestion is not a problem: severity = %v", u.Severity)
	}
}

// TestConstructorsSanitiseTheirInputs
//
// PRODUCT CONTRACT (owner ruling above). A model id reaches the
// constructor from a catalog manifest, so it is not the producer's own
// invention and cannot be assumed clean.
func TestConstructorsSanitiseTheirInputs(t *testing.T) {
	n := LighterModel("from", "evil\nid\x1b[2J", 1, 2)
	if strings.ContainsAny(n.Title, "\n\x1b") || strings.ContainsAny(n.Target, "\n\x1b") {
		t.Fatalf("constructor let a control character through: %+v", n)
	}
}
