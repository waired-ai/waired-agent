package claudemanaged

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteSetsContextWindowFlags: two keys waired used to co-write with the
// base URL and no longer does. CLAUDE_CODE_AUTO_COMPACT_WINDOW went with #771
// — Claude Code's own per-model resolution must govern the window, and a
// static env override capped 1M sessions at 200k. The discovery flag went
// with waired-agent#1185: it never did anything on a subscription-OAuth host
// (discovery is credential-gated and waired supplies none), and the Waired
// /model rows come from the `modelPicker` setting now.
func TestWriteSetsContextWindowFlags(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	for _, k := range []string{discoveryKey, autoCompactWindowKey} {
		if v, bad := env[k]; bad {
			t.Errorf("%s = %v, want absent", k, v)
		}
	}
}

// TestWriteScrubsTheDiscoveryFlagItWrote: an upgrade re-running Write over a
// pre-#1185 file removes the flag waired itself set, and only that value — an
// operator who turned discovery on for their own reasons keeps it, the same
// way the auto-compact window works.
func TestWriteScrubsTheDiscoveryFlagItWrote(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed string
		want any
	}{
		{"ours", `"1"`, nil},
		{"an operator's own value", `"0"`, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
				`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":` + tc.seed + `}}`
			if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Write("http://127.0.0.1:9472"); err != nil {
				t.Fatalf("Write: %v", err)
			}
			env := readJSON(t, p)["env"].(map[string]any)
			if got := env[discoveryKey]; got != tc.want {
				t.Errorf("%s = %v, want %v", discoveryKey, got, tc.want)
			}
		})
	}
}

// TestWriteScrubsLegacyAutoCompactWindow: an upgrade re-running Write over a
// pre-#771 file removes the static window waired itself wrote.
func TestWriteScrubsLegacyAutoCompactWindow(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
		`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if v, bad := env[autoCompactWindowKey]; bad {
		t.Errorf("Write left legacy %s = %v behind", autoCompactWindowKey, v)
	}
}

// TestWriteKeepsOperatorAutoCompactWindow: a value that is not the one waired
// wrote is an operator's deliberate override — Write must not touch it.
func TestWriteKeepsOperatorAutoCompactWindow(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"CLAUDE_CODE_AUTO_COMPACT_WINDOW":"300000"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env[autoCompactWindowKey] != "300000" {
		t.Errorf("%s = %v, want operator's \"300000\" preserved", autoCompactWindowKey, env[autoCompactWindowKey])
	}
}

// TestRemoveStripsContextWindowFlags: Remove strips the discovery flag and the
// legacy auto-compact window together with our loopback base URL, preserving
// operator keys.
func TestRemoveStripsContextWindowFlags(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"FOO":"bar","ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
		`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove reported removed=false, want true")
	}
	env := readJSON(t, p)["env"].(map[string]any)
	for _, k := range []string{"ANTHROPIC_BASE_URL", discoveryKey, autoCompactWindowKey} {
		if _, bad := env[k]; bad {
			t.Errorf("Remove left %q behind", k)
		}
	}
	if env["FOO"] != "bar" {
		t.Error("Remove clobbered operator's env.FOO")
	}
}

// TestRemoveKeepsOperatorAutoCompactWindow: on disable, a non-legacy window
// value is operator-owned and survives even though the loopback base URL (and
// the flags waired wrote) are stripped.
func TestRemoveKeepsOperatorAutoCompactWindow(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
		`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"300000"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("Remove reported removed=false, want true")
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if _, bad := env["ANTHROPIC_BASE_URL"]; bad {
		t.Error("Remove left our loopback base URL behind")
	}
	if env[autoCompactWindowKey] != "300000" {
		t.Errorf("%s = %v, want operator's \"300000\" preserved", autoCompactWindowKey, env[autoCompactWindowKey])
	}
}

// TestRemoveKeepsFlagsWhenBaseURLOperatorOwned: when the base URL is an
// operator's own non-loopback gateway, the whole env block is theirs — Remove
// must not strip the flags either.
func TestRemoveKeepsFlagsWhenBaseURLOperatorOwned(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"env":{"ANTHROPIC_BASE_URL":"https://gw.corp.example/v1","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"200000"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://gw.corp.example/v1" {
		t.Error("Remove stripped operator's non-loopback base URL")
	}
	if env[autoCompactWindowKey] != "200000" {
		t.Error("Remove stripped a flag despite operator-owned base URL")
	}
}
