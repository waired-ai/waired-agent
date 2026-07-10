package claudemanaged

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteSetsContextWindowFlags: Write co-writes the two #623
// context-window env flags alongside the base URL (neither a credential).
func TestWriteSetsContextWindowFlags(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env[discoveryKey] != "1" {
		t.Errorf("%s = %v, want \"1\"", discoveryKey, env[discoveryKey])
	}
	if env[autoCompactWindowKey] != autoCompactWindowValue {
		t.Errorf("%s = %v, want %q", autoCompactWindowKey, env[autoCompactWindowKey], autoCompactWindowValue)
	}
}

// TestRemoveStripsContextWindowFlags: Remove strips both flags together with
// our loopback base URL, preserving operator keys.
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
