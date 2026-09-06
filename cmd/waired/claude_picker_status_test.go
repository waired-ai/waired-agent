package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

const pickerStatusPath = "/home/you/.claude/settings.json"

// TestClaudePickerStatusRow: each state has to say something the operator can
// act on, and they have to be distinguishable — "not written" and "left alone"
// call for opposite next steps, and before this row existed an empty picker
// looked exactly like a healthy one.
func TestClaudePickerStatusRow(t *testing.T) {
	live := "http://127.0.0.1:9472"

	t.Run("silent when this machine is not routed at waired", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupOurs, rows: 4, path: pickerStatusPath,
		})
		if got != "" {
			t.Errorf("row = %q, want silence — the picker is not ours to explain", got)
		}
	})

	t.Run("ours: the count and where it is", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupOurs, rows: 4, path: pickerStatusPath, liveBaseURL: live,
		})
		if !strings.Contains(got, "4 rows") || !strings.Contains(got, pickerStatusPath) {
			t.Errorf("row = %q, want the count and the path", got)
		}
	})

	t.Run("one row is singular", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupOurs, rows: 1, path: pickerStatusPath, liveBaseURL: live,
		})
		if !strings.Contains(got, "1 row\n") && !strings.Contains(got, "1 row ") {
			t.Errorf("row = %q, want \"1 row\"", got)
		}
	})

	// The likeliest cause is not "enable never ran" but "it ran as root", so
	// the next step has to name the user rather than the command alone.
	t.Run("absent: says which user and what to run", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupNone, path: pickerStatusPath, liveBaseURL: live,
			viaSudo: true, sudoUser: "you",
		})
		if !strings.Contains(got, "not written") || !strings.Contains(got, "user you") ||
			!strings.Contains(got, "waired claude enable") {
			t.Errorf("row = %q, want the state, the user and the command", got)
		}
	})

	// Distinct from absent on purpose: there is nothing for the operator to
	// re-run, and telling them to run enable again would be wrong advice.
	t.Run("foreign: says waired left it alone", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupForeign, rows: 2, path: pickerStatusPath, liveBaseURL: live,
		})
		if !strings.Contains(got, "left alone") || strings.Contains(got, "waired claude enable") {
			t.Errorf("row = %q, want the hands-off state and no re-run advice", got)
		}
	})

	t.Run("unreadable is not absent", func(t *testing.T) {
		got := claudePickerStatusRow(claudePickerFacts{
			kind: claudecode.PickerLineupUnreadable, path: pickerStatusPath, liveBaseURL: live,
		})
		if !strings.Contains(got, "UNREADABLE") {
			t.Errorf("row = %q, want the unreadable state", got)
		}
	})
}

// TestDetectPickerLineupClassifies is the ownership rule the status row and
// the writer both hang off. Claude Code takes the whole lineup from the
// highest source that sets the key and never merges two, so a lineup that is
// not ours is one waired must not replace: replacing it would delete it.
func TestDetectPickerLineupClassifies(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, tc := range []struct {
		name string
		body string
		want claudecode.PickerLineupKind
	}{
		{"absent file", "", claudecode.PickerLineupNone},
		{"no key", `{"statusLine":{"type":"command","command":"x"}}`, claudecode.PickerLineupNone},
		{"empty options", `{"modelPicker":{"options":[]}}`, claudecode.PickerLineupNone},
		{"all ours", `{"modelPicker":{"options":[{"model":"waired"},{"model":"waired/local"}]}}`,
			claudecode.PickerLineupOurs},
		{"ours in the old spelling", `{"modelPicker":{"options":[{"model":"claude-waired-auto"}]}}`,
			claudecode.PickerLineupOurs},
		{"someone else's", `{"modelPicker":{"options":[{"model":"us.anthropic.claude-opus-4-8"}]}}`,
			claudecode.PickerLineupForeign},
		// One foreign row is enough: waired writes the lineup whole, so it
		// cannot preserve a row inside one it replaces.
		{"one foreign row among ours", `{"modelPicker":{"options":[{"model":"waired"},{"model":"opus"}]}}`,
			claudecode.PickerLineupForeign},
		{"not JSON", `{`, claudecode.PickerLineupUnreadable},
		{"key is not a lineup", `{"modelPicker":"yes"}`, claudecode.PickerLineupUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "settings.json")
			if tc.body != "" {
				p = write(t, tc.body)
			}
			if got, _ := claudecode.DetectPickerLineup(p); got != tc.want {
				t.Errorf("DetectPickerLineup(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestWritePickerLineupOwnership: the write refuses what the classification
// says is not ours, is a no-op when nothing changed (the SessionStart hook
// runs on every launch), and removes the key rather than writing an empty
// lineup.
func TestWritePickerLineupOwnership(t *testing.T) {
	rows := []claudecode.PickerRow{{Model: "waired", Label: "Waired"}}

	t.Run("writes into a file it shares with other keys", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, []byte(`{"statusLine":{"type":"command","command":"keep me"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		changed, err := claudecode.WritePickerLineup(p, rows)
		if err != nil || !changed {
			t.Fatalf("WritePickerLineup = (%v, %v), want (true, nil)", changed, err)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "keep me") {
			t.Errorf("the operator's own key was dropped: %s", b)
		}
		if kind, got := claudecode.DetectPickerLineup(p); kind != claudecode.PickerLineupOurs || len(got) != 1 {
			t.Errorf("read back kind=%v rows=%v", kind, got)
		}
	})

	t.Run("second write with the same rows changes nothing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "settings.json")
		if _, err := claudecode.WritePickerLineup(p, rows); err != nil {
			t.Fatal(err)
		}
		changed, err := claudecode.WritePickerLineup(p, rows)
		if err != nil || changed {
			t.Errorf("WritePickerLineup = (%v, %v), want (false, nil)", changed, err)
		}
	})

	t.Run("refuses a lineup that is not ours", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "settings.json")
		body := `{"modelPicker":{"options":[{"model":"us.anthropic.claude-opus-4-8","label":"theirs"}]}}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := claudecode.WritePickerLineup(p, rows); err == nil {
			t.Fatal("WritePickerLineup replaced someone else's lineup")
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "theirs") {
			t.Errorf("their lineup was rewritten anyway: %s", b)
		}
	})

	t.Run("no rows removes the key", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "settings.json")
		if _, err := claudecode.WritePickerLineup(p, rows); err != nil {
			t.Fatal(err)
		}
		removed, err := claudecode.RemovePickerLineup(p)
		if err != nil || !removed {
			t.Fatalf("RemovePickerLineup = (%v, %v), want (true, nil)", removed, err)
		}
		if kind, _ := claudecode.DetectPickerLineup(p); kind != claudecode.PickerLineupNone {
			t.Errorf("kind after removal = %v, want none", kind)
		}
	})

	t.Run("remove leaves a foreign lineup alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "settings.json")
		body := `{"modelPicker":{"options":[{"model":"opus","label":"theirs"}]}}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := claudecode.RemovePickerLineup(p)
		if err != nil || removed {
			t.Errorf("RemovePickerLineup = (%v, %v), want (false, nil)", removed, err)
		}
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), "theirs") {
			t.Errorf("disable deleted a lineup waired never wrote: %s", b)
		}
	})
}
