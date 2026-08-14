package main

import (
	"os"
	"path/filepath"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#796): "is Claude Code routed?" is answered from
// the managed-settings file, so init's closing card and `waired claude status`
// cannot disagree — which is the contradiction the issue reports.
func TestClaudeRoutedNow(t *testing.T) {
	const ours = "http://127.0.0.1:9472"
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "managed-settings.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("routed at this host's gateway", func(t *testing.T) {
		p := write(t, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472"}}`)
		if !claudeRoutedNow(p, ours) {
			t.Error("a file pointing at our gateway is not reported as routed")
		}
	})
	t.Run("routed somewhere that is not us", func(t *testing.T) {
		// An operator's own gateway, or a stale port from a previous config.
		// Claiming it as ours would be the same false positive in reverse.
		p := write(t, `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9999"}}`)
		if claudeRoutedNow(p, ours) {
			t.Error("a file pointing elsewhere is reported as routed through us")
		}
	})
	t.Run("present but no base URL", func(t *testing.T) {
		p := write(t, `{"env":{"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"200704"}}`)
		if claudeRoutedNow(p, ours) {
			t.Error("a file with no base URL is reported as routed")
		}
	})
	t.Run("no file", func(t *testing.T) {
		if claudeRoutedNow(filepath.Join(t.TempDir(), "absent.json"), ours) {
			t.Error("an absent file is reported as routed")
		}
	})
	t.Run("unsupported OS has no path at all", func(t *testing.T) {
		if claudeRoutedNow("", ours) {
			t.Error("an empty path is reported as routed")
		}
	})
}

// PRODUCT CONTRACT (waired-agent#796): the browser wizard routes before the
// model exists, so the context window has to be filled in afterwards — and only
// then. Every other combination must leave the file alone.
//
// RECORD OF TODAY'S BEHAVIOUR: an already-correct value is not rewritten, so the
// terminal path (which writes the right number before this runs) costs no second
// write.
func TestClaudeWindowTopUpNeeded(t *testing.T) {
	base := claudeWindowFacts{routed: true, directives: true, elevated: true, managed: "", live: 200704}

	cases := map[string]struct {
		mutate func(*claudeWindowFacts)
		want   bool
	}{
		"the wizard case: routed, directives on, window now known": {func(*claudeWindowFacts) {}, true},
		"replacing a stale number":                                 {func(f *claudeWindowFacts) { f.managed = "250000" }, true},

		"already correct — no second write":  {func(f *claudeWindowFacts) { f.managed = "200704" }, false},
		"not routed — not our file to touch": {func(f *claudeWindowFacts) { f.routed = false }, false},
		"directives off — the key means nothing": {
			func(f *claudeWindowFacts) { f.directives = false }, false},
		"not elevated — cannot write the machine-wide file": {
			func(f *claudeWindowFacts) { f.elevated = false }, false},
		"window still unknown — a stale honest number beats a guess": {
			func(f *claudeWindowFacts) { f.live = 0 }, false},
		"window unknown and nothing recorded yet": {
			func(f *claudeWindowFacts) { f.live, f.managed = 0, "" }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := base
			tc.mutate(&f)
			if got := claudeWindowTopUpNeeded(f); got != tc.want {
				t.Errorf("claudeWindowTopUpNeeded(%+v) = %v, want %v", f, got, tc.want)
			}
		})
	}
}
