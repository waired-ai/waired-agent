package claudemanaged

import (
	"os"
	"path/filepath"
	"testing"
)

const testBaseURL = "http://127.0.0.1:9472"

// seedManagedSettings writes a managed-settings file with the given env body
// (raw JSON object contents) so a test can start from an operator's or an older
// waired's state.
func seedManagedSettings(t *testing.T, path, envBody string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"env":{`+envBody+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func envOf(t *testing.T, path string) map[string]any {
	t.Helper()
	env, ok := readJSON(t, path)["env"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return env
}

// TestWriteWithOptionsWritesTheRealLocalWindow: since #408 the #52 opt-in sizes
// CLAUDE_CODE_MAX_CONTEXT_TOKENS from the window this host actually serves
// (WriteOptions.LocalContextWindow), not the old static 250000. Product
// contract: the value Claude Code is handed must be one local inference can
// stand behind.
//
// This inverts the pre-#408 assertion that the key always equalled
// directivesMaxContextTokensValue.
func TestWriteWithOptionsWritesTheRealLocalWindow(t *testing.T) {
	t.Run("on with a resolved window writes it", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{
			ModelRouteDirectives: true, LocalContextWindow: 32768,
		}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := envOf(t, p)[maxContextTokensKey]; got != "32768" {
			t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "32768")
		}
	})

	t.Run("upgrade replaces the pre-#408 static value", func(t *testing.T) {
		p := withTempPath(t)
		seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+legacyDirectivesMaxContextTokensValue+`"`)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{
			ModelRouteDirectives: true, LocalContextWindow: 8192,
		}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := envOf(t, p)[maxContextTokensKey]; got != "8192" {
			t.Errorf("%s = %v, want the derived %q (the 250000 claim must not survive an upgrade)",
				maxContextTokensKey, got, "8192")
		}
	})

	t.Run("default Write leaves it unset", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := Write(testBaseURL); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if v, bad := envOf(t, p)[maxContextTokensKey]; bad {
			t.Errorf("%s = %v, want absent when the feature is off", maxContextTokensKey, v)
		}
	})
}

// TestWriteLeavesTheWindowAloneWhenUnresolved: a 0 window means the agent could
// not be asked (down, no active model, unknown sizing). Product contract:
// declining to write beats restating a number nothing verified — the whole
// point of #408 is that the file must not carry a claim.
func TestWriteLeavesTheWindowAloneWhenUnresolved(t *testing.T) {
	t.Run("an existing value survives untouched", func(t *testing.T) {
		p := withTempPath(t)
		seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"32768"`)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{
			ModelRouteDirectives: true, LocalContextWindow: 0,
		}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := envOf(t, p)[maxContextTokensKey]; got != "32768" {
			t.Errorf("%s = %v, want the previous %q kept when the window is unknown",
				maxContextTokensKey, got, "32768")
		}
	})

	t.Run("nothing is invented on a fresh file", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{
			ModelRouteDirectives: true, LocalContextWindow: 0,
		}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if v, bad := envOf(t, p)[maxContextTokensKey]; bad {
			t.Errorf("%s = %v, want absent — an unknown window must not become a written claim",
				maxContextTokensKey, v)
		}
	})
}

// TestWriteScrubsOurMaxContextTokensWhenOff: toggling the feature off and
// re-running enable removes the value waired wrote — whether that is the
// pre-#408 constant or a host-derived number — but leaves an operator's own
// override alone.
func TestWriteScrubsOurMaxContextTokensWhenOff(t *testing.T) {
	cases := []struct {
		name    string
		seeded  string
		window  int
		wantKey any // nil = must be gone
	}{
		{"legacy static value is ours", legacyDirectivesMaxContextTokensValue, 32768, nil},
		{"host-derived value is ours", "32768", 32768, nil},
		{"legacy is ours even with no window resolved", legacyDirectivesMaxContextTokensValue, 0, nil},
		{"operator override preserved", "500000", 32768, "500000"},
		// Record of today's behaviour, not a contract: with no window resolved
		// we cannot tell a host-derived value from an operator's, so it stays.
		// It is inert — the loopback base URL that gave the non-"claude-"
		// directive ids their meaning is gone too. See
		// wairedOwnedMaxContextTokens.
		{"host-derived survives when the window is unknown", "32768", 0, "32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
				`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+tc.seeded+`"`)
			if _, err := WriteWithOptions(testBaseURL, WriteOptions{
				ModelRouteDirectives: false, LocalContextWindow: tc.window,
			}); err != nil {
				t.Fatalf("WriteWithOptions: %v", err)
			}
			got, present := envOf(t, p)[maxContextTokensKey]
			switch {
			case tc.wantKey == nil && present:
				t.Errorf("Write left %s = %v behind", maxContextTokensKey, got)
			case tc.wantKey != nil && got != tc.wantKey:
				t.Errorf("%s = %v (present=%v), want %v", maxContextTokensKey, got, present, tc.wantKey)
			}
		})
	}
}

// TestRemoveStripsOurMaxContextTokens: disable removes the value waired wrote
// alongside the loopback base URL, but preserves an operator's own override.
// RemoveWithOptions carries the resolved window for the same ownership test
// Write uses.
func TestRemoveStripsOurMaxContextTokens(t *testing.T) {
	cases := []struct {
		name    string
		seeded  string
		window  int
		wantKey any // nil = must be gone
	}{
		{"legacy static value stripped", legacyDirectivesMaxContextTokensValue, 0, nil},
		{"host-derived value stripped when the window is known", "32768", 32768, nil},
		{"operator override preserved", "500000", 32768, "500000"},
		// A RECORD OF TODAY'S BEHAVIOUR, not a contract
		// (waired-agent#1174). The window is 0 whenever the daemon is not
		// answering — which the doc comment on runClaudeDisable says is
		// frequent, since disable often runs with the agent already
		// stopped — and the value we wrote then survives as the file's
		// only content. On a machine that no longer runs Waired it goes on
		// steering every Claude Code session from it. Whether an unknown
		// window should remove the key anyway is a trade between two
		// harms and is with the owner on the issue.
		{"host-derived value survives an unknown window", "200704", 0, "200704"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			// A filler operator key keeps the file (and its env block) from
			// being deleted entirely, so we can assert on the specific key.
			seedManagedSettings(t, p, `"FOO":"bar","ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
				`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+tc.seeded+`"`)
			if _, err := RemoveWithOptions(RemoveOptions{LocalContextWindow: tc.window}); err != nil {
				t.Fatalf("RemoveWithOptions: %v", err)
			}
			env := envOf(t, p)
			got, present := env[maxContextTokensKey]
			switch {
			case tc.wantKey == nil && present:
				t.Errorf("Remove left %s = %v behind", maxContextTokensKey, got)
			case tc.wantKey != nil && got != tc.wantKey:
				t.Errorf("%s = %v (present=%v), want %v", maxContextTokensKey, got, present, tc.wantKey)
			}
			if env["FOO"] != "bar" {
				t.Error("Remove clobbered operator's env.FOO")
			}
		})
	}
}

// TestMaxContextTokensAt reads back what Write put in the file — the accessor
// `waired claude status` uses to show the window Claude Code will start with
// next to the one local inference serves.
func TestMaxContextTokensAt(t *testing.T) {
	t.Run("reads the written value", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{
			ModelRouteDirectives: true, LocalContextWindow: 32768,
		}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := MaxContextTokensAt(p); got != "32768" {
			t.Errorf("MaxContextTokensAt = %q, want %q", got, "32768")
		}
	})

	// Every "not there" shape must collapse to "" — status must render a
	// missing or malformed operator file, not fail on it.
	t.Run("absent, unset, malformed and empty-path all read empty", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "missing.json")
		unset := filepath.Join(dir, "unset.json")
		malformed := filepath.Join(dir, "malformed.json")
		if err := os.WriteFile(unset, []byte(`{"env":{"FOO":"bar"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(malformed, []byte(`not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"", missing, unset, malformed} {
			if got := MaxContextTokensAt(p); got != "" {
				t.Errorf("MaxContextTokensAt(%q) = %q, want \"\"", p, got)
			}
		}
	})
}
