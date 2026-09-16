package claudemanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#796, #1396): a routed file that does not say
// 200704 — written before anything served by an older wizard, or carrying the
// window an older build derived — is set to it, without disturbing anything
// else in a machine-wide file that an operator or an MDM may also own.
func TestSetMaxContextTokensAt(t *testing.T) {
	t.Run("fills in a key an older wizard left out", func(t *testing.T) {
		p := withTempPath(t)
		// The file such a build wrote: routed, hooks in place, no window.
		if _, err := Write(testBaseURL); err != nil {
			t.Fatalf("Write: %v", err)
		}
		before := readJSON(t, p)
		if _, ok := envOf(t, p)[maxContextTokensKey]; ok {
			t.Fatal("the fixture already carries the key; there would be nothing to top up")
		}

		wrote, err := SetMaxContextTokensAt(p)
		if err != nil {
			t.Fatalf("SetMaxContextTokensAt: %v", err)
		}
		if !wrote {
			t.Error("reported no write")
		}
		if got := envOf(t, p)[maxContextTokensKey]; got != "200704" {
			t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "200704")
		}
		// Everything else byte-identical. The whole risk of a second writer on a
		// machine-wide file is that it quietly drops what the first one put there.
		after := readJSON(t, p)
		delete(after["env"].(map[string]any), maxContextTokensKey)
		if !jsonEqual(t, before, after) {
			t.Errorf("the top-up disturbed the rest of the file\nbefore: %v\nafter:  %v", before, after)
		}
	})

	t.Run("an already-correct value is not rewritten", func(t *testing.T) {
		p := withTempPath(t)
		seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"200704"`)
		wrote, err := SetMaxContextTokensAt(p)
		if err != nil {
			t.Fatalf("SetMaxContextTokensAt: %v", err)
		}
		if wrote {
			t.Error("rewrote a file that already said the right thing")
		}
	})

	for _, old := range []string{"250000", "262144"} {
		t.Run("replaces "+old, func(t *testing.T) {
			p := withTempPath(t)
			seedManagedSettings(t, p, `"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+old+`"`)
			if _, err := SetMaxContextTokensAt(p); err != nil {
				t.Fatalf("SetMaxContextTokensAt: %v", err)
			}
			if got := envOf(t, p)[maxContextTokensKey]; got != "200704" {
				t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "200704")
			}
		})
	}

	t.Run("never creates the file", func(t *testing.T) {
		// A host that was never routed has no managed settings, and inventing
		// one carrying only a window would leave Claude Code told a context size
		// by a file that does not route it anywhere.
		p := filepath.Join(t.TempDir(), "claude-code", "managed-settings.json")
		wrote, err := SetMaxContextTokensAt(p)
		if err != nil {
			t.Fatalf("SetMaxContextTokensAt: %v", err)
		}
		if wrote {
			t.Error("reported a write for an absent file")
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Error("created a managed-settings file out of nothing")
		}
	})

	t.Run("an unparseable operator file is left alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "managed-settings.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		wrote, err := SetMaxContextTokensAt(p)
		if err != nil {
			t.Fatalf("SetMaxContextTokensAt: %v", err)
		}
		if wrote {
			t.Error("rewrote a file it could not read")
		}
		b, _ := os.ReadFile(p)
		if string(b) != "{not json" {
			t.Errorf("file changed to %q", b)
		}
	})

	t.Run("no path (unsupported OS)", func(t *testing.T) {
		if wrote, err := SetMaxContextTokensAt(""); wrote || err != nil {
			t.Errorf("wrote=%v err=%v, want no write and no error", wrote, err)
		}
	})
}

func jsonEqual(t *testing.T, a, b map[string]any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ab) == string(bb)
}
