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
		for _, s := range []string{n.Subject, n.Title, n.Text, n.Target} {
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
	// The subject is what `waired doctor` puts in its first column,
	// beside "state directory" and "inference engine" — a short noun,
	// not a sentence.
	if want := "model suggestion"; l.Subject != want {
		t.Errorf("lighter subject = %q, want %q", l.Subject, want)
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

// TestEngineTuningSeverityFollowsDegradedNotTheString
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: a notice is a warning only
// for something Waired can assert is unwanted). A tuning note is set on
// a host that works exactly as intended — a context window deliberately
// traded against decode speed — and RuntimeStatus.TuningDegraded is the
// field that separates that from a configuration this computer could not
// hold. Keying the severity on "the string is not empty" is what makes
// `waired doctor` warn about a healthy computer.
func TestEngineTuningSeverityFollowsDegradedNotTheString(t *testing.T) {
	deliberate := EngineTuning("ollama",
		"context window set to 200000 tokens for coding-agent workloads; about 12% of the model is expected to sit in system RAM (larger window traded for some decode speed)",
		false)
	if deliberate.Severity != SeverityInfo {
		t.Errorf("a deliberate trade is not a fault: severity = %v", deliberate.Severity)
	}
	if deliberate.Text == "" {
		t.Error("info severity must still carry the detail — the tray and `waired status` show these")
	}

	broken := EngineTuning("ollama",
		"this computer's GPU has no room left to serve a request at this model and window",
		true)
	if broken.Severity != SeverityWarn {
		t.Errorf("a configuration this host could not hold is a fault: severity = %v", broken.Severity)
	}
	if broken.Title == deliberate.Title {
		t.Errorf("both titles are %q; the two cases say different things", broken.Title)
	}
}

// TestEngineConstructorsSanitiseTheDetail
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: the notice module refuses
// what a renderer would misread). These three constructors are the only
// ones that take prose from elsewhere, so this is the exact point where
// the module's guarantee would be lost if it were not applied.
func TestEngineConstructorsSanitiseTheDetail(t *testing.T) {
	hostile := "spill\nof \x1b[2J ✓ everything is fine"
	for _, n := range []Notice{
		EngineVersion("ollama", hostile),
		EngineTuning("ollama", hostile, true),
		EngineTuning("ollama", hostile, false),
		UpdateAvailable(hostile, hostile),
	} {
		for _, s := range []string{n.Subject, n.Title, n.Text, n.Target} {
			if strings.ContainsAny(s, "\n\r\x1b") {
				t.Errorf("%s: %q kept a control character", n.Kind, s)
			}
			for _, r := range s {
				if statusMark(r) {
					t.Errorf("%s: %q carries the status mark %q", n.Kind, s, string(r))
				}
			}
		}
	}
}

// TestEngineNotAskedGetsANameAnyway records today's behaviour: a host
// with no inference subsystem reports no engine name, and a title
// starting with a space reads as a rendering fault rather than as
// missing data.
func TestEngineNotAskedGetsANameAnyway(t *testing.T) {
	n := EngineVersion("", "0.24.0 is not 0.33.2")
	if strings.HasPrefix(n.Title, " ") {
		t.Errorf("title = %q, want no leading space", n.Title)
	}
	if !strings.HasPrefix(n.Title, "the inference engine") {
		t.Errorf("title = %q, want it to name the engine generically", n.Title)
	}
}

// TestTextIsBoundedButNotAtMenuWidth
//
// PRODUCT CONTRACT (owner ruling above, applied to a field): Text is
// never a menu row, and the tuning details this carries run past a menu
// width on their own — truncating one at maxRunes cuts it in the middle
// of the clause that says what is wrong.
func TestTextIsBoundedButNotAtMenuWidth(t *testing.T) {
	n := EngineTuning("ollama", strings.Repeat("x", 10_000), true)
	got := utf8.RuneCountInString(n.Text)
	if got <= maxRunes {
		t.Errorf("Text is %d runes, want more than the %d-rune menu bound", got, maxRunes)
	}
	if got > maxTextRunes+1 {
		t.Errorf("Text is %d runes, want at most %d plus the ellipsis", got, maxTextRunes)
	}
	if n := utf8.RuneCountInString(EngineTuning("ollama", strings.Repeat("x", 10_000), true).Title); n > maxRunes+1 {
		t.Errorf("Title is %d runes, want the menu bound of %d", n, maxRunes)
	}
}

// TestUpdateAvailableIsInfoAndOffersTheInstall
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: 更新通知については info で
// いい). Info keeps it out of `waired doctor`, which reports faults, while
// the tray and `waired status` still show it.
func TestUpdateAvailableIsInfoAndOffersTheInstall(t *testing.T) {
	n := UpdateAvailable("v0.9.1", "v0.9.3")
	if n.Severity != SeverityInfo {
		t.Errorf("severity = %v, want info", n.Severity)
	}
	if n.Action != ActionInstallUpdate {
		t.Errorf("action = %v, want ActionInstallUpdate", n.Action)
	}
	if want := "Update available — install v0.9.3"; n.Title != want {
		t.Errorf("title = %q, want %q", n.Title, want)
	}
	if want := "This computer runs v0.9.1."; n.Text != want {
		t.Errorf("text = %q, want %q", n.Text, want)
	}
	if n.Target != "v0.9.3" {
		t.Errorf("target = %q, want the version so a re-publish carries FirstSeen forward", n.Target)
	}
}

// TestUnmarshalKeepsAnActionItKnows
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: an unknown value is
// clamped, not rejected). The clamp is a switch over the actions this
// build knows; a new one added to the enum without being added there
// would arrive as ActionNone and its row would silently do nothing.
func TestUnmarshalKeepsAnActionItKnows(t *testing.T) {
	for _, want := range []Action{ActionNone, ActionModelSuggestion, ActionInstallUpdate} {
		b, err := json.Marshal(Notice{Kind: "k", Title: "t", Action: want})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got Notice
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Action != want {
			t.Errorf("action %v survived the wire as %v", want, got.Action)
		}
	}
	var got Notice
	if err := json.Unmarshal([]byte(`{"kind":"k","title":"t","action":99}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Action != ActionNone {
		t.Errorf("an action from a newer daemon = %v, want ActionNone", got.Action)
	}
}
