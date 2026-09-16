package claudemanaged

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
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

// TestWriteWithOptionsWritesTheFixedWindow: the #52 opt-in sets
// CLAUDE_CODE_MAX_CONTEXT_TOKENS to 200704 on every host, whatever window any
// computer serves and whether or not anything serves yet.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396 — every Waired row without "[1m]" is a 200k session. This
// inverts #408's contract (the window this host serves) and
// waired-agent#1246's (a reachable window on a host with no engine), and the
// "leave it alone while the window is unknown" rule that went with them.
func TestWriteWithOptionsWritesTheFixedWindow(t *testing.T) {
	if DirectivesMaxContextTokensValue != strconv.Itoa(hostfit.ServingWindow200k) {
		t.Fatalf("DirectivesMaxContextTokensValue = %q, want the 200k tier %d", DirectivesMaxContextTokensValue, hostfit.ServingWindow200k)
	}

	t.Run("a fresh file gets it, with nothing serving", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{ModelRouteDirectives: true}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := envOf(t, p)[maxContextTokensKey]; got != "200704" {
			t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "200704")
		}
	})

	for _, old := range []string{legacyDirectivesMaxContextTokensValue, "262144", "32768", "500000"} {
		t.Run("replaces "+old, func(t *testing.T) {
			p := withTempPath(t)
			seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
				`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+old+`"`)
			if _, err := WriteWithOptions(testBaseURL, WriteOptions{ModelRouteDirectives: true}); err != nil {
				t.Fatalf("WriteWithOptions: %v", err)
			}
			if got := envOf(t, p)[maxContextTokensKey]; got != "200704" {
				t.Errorf("%s = %v, want %q", maxContextTokensKey, got, "200704")
			}
		})
	}

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

// TestWriteScrubsOurMaxContextTokensWhenOff: toggling the feature off and
// re-running enable removes the value waired wrote — the fixed value, the
// pre-#408 constant, or the window an older build derived here — but leaves an
// operator's own override alone.
func TestWriteScrubsOurMaxContextTokensWhenOff(t *testing.T) {
	cases := []struct {
		name    string
		seeded  string
		prior   int
		wantKey any // nil = must be gone
	}{
		{"the fixed value is ours", DirectivesMaxContextTokensValue, 0, nil},
		{"legacy static value is ours", legacyDirectivesMaxContextTokensValue, 0, nil},
		{"a value an older build derived here is ours", "32768", 32768, nil},
		{"operator override preserved", "500000", 32768, "500000"},
		// Record of today's behaviour, not a contract: with no prior window
		// resolved we cannot tell an older build's derived value from an
		// operator's, so it stays. See wairedOwnedMaxContextTokens.
		{"a derived value survives when the prior window is unknown", "32768", 0, "32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			seedManagedSettings(t, p, `"ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
				`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+tc.seeded+`"`)
			if _, err := WriteWithOptions(testBaseURL, WriteOptions{
				ModelRouteDirectives: false, PriorContextWindow: tc.prior,
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
func TestRemoveStripsOurMaxContextTokens(t *testing.T) {
	cases := []struct {
		name    string
		seeded  string
		prior   int
		wantKey any // nil = must be gone
	}{
		// PRODUCT CONTRACT (waired-agent#1174, #1396): the value this build
		// writes goes with the base URL even when the agent cannot be asked,
		// which is the common case for disable. It used to survive exactly
		// then, and steer every Claude Code session on a machine that no
		// longer runs Waired.
		{"the fixed value stripped with no agent to ask", DirectivesMaxContextTokensValue, 0, nil},
		{"legacy static value stripped", legacyDirectivesMaxContextTokensValue, 0, nil},
		{"a derived value stripped when the prior window is known", "32768", 32768, nil},
		{"operator override preserved", "500000", 32768, "500000"},
		// A RECORD OF TODAY'S BEHAVIOUR, not a contract: a value an older
		// build derived cannot be told from an operator's without the agent.
		{"a derived value survives an unknown prior window", "32768", 0, "32768"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			// A filler operator key keeps the file (and its env block) from
			// being deleted entirely, so we can assert on the specific key.
			seedManagedSettings(t, p, `"FOO":"bar","ANTHROPIC_BASE_URL":"`+testBaseURL+`",`+
				`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"`+tc.seeded+`"`)
			if _, err := RemoveWithOptions(RemoveOptions{PriorContextWindow: tc.prior}); err != nil {
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
// `waired claude status` uses to show the window Claude Code will start with.
func TestMaxContextTokensAt(t *testing.T) {
	t.Run("reads the written value", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions(testBaseURL, WriteOptions{ModelRouteDirectives: true}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		if got := MaxContextTokensAt(p); got != "200704" {
			t.Errorf("MaxContextTokensAt = %q, want %q", got, "200704")
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
