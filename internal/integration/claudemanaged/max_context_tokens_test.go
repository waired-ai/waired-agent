package claudemanaged

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteWithOptionsSetsMaxContextTokens: the #52 opt-in writes
// CLAUDE_CODE_MAX_CONTEXT_TOKENS (the honest ~256k local window for the
// non-"claude-" directive id); the default Write leaves it unset.
func TestWriteWithOptionsSetsMaxContextTokens(t *testing.T) {
	t.Run("on sets the value", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := WriteWithOptions("http://127.0.0.1:9472", WriteOptions{ModelRouteDirectives: true}); err != nil {
			t.Fatalf("WriteWithOptions: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if env[maxContextTokensKey] != directivesMaxContextTokensValue {
			t.Errorf("%s = %v, want %q", maxContextTokensKey, env[maxContextTokensKey], directivesMaxContextTokensValue)
		}
	})

	t.Run("off leaves it unset", func(t *testing.T) {
		p := withTempPath(t)
		if _, err := Write("http://127.0.0.1:9472"); err != nil {
			t.Fatalf("Write: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if v, bad := env[maxContextTokensKey]; bad {
			t.Errorf("%s = %v, want absent when the feature is off", maxContextTokensKey, v)
		}
	})
}

// TestWriteScrubsOurMaxContextTokensWhenOff: toggling the feature off and
// re-running enable removes the value waired wrote, but leaves an operator's
// own override alone.
func TestWriteScrubsOurMaxContextTokensWhenOff(t *testing.T) {
	t.Run("ours is scrubbed", func(t *testing.T) {
		p := withTempPath(t)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"` + directivesMaxContextTokensValue + `"}}`
		if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Write("http://127.0.0.1:9472"); err != nil { // feature off
			t.Fatalf("Write: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if v, bad := env[maxContextTokensKey]; bad {
			t.Errorf("Write left our %s = %v behind", maxContextTokensKey, v)
		}
	})

	t.Run("operator override preserved", func(t *testing.T) {
		p := withTempPath(t)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		seed := `{"env":{"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"500000"}}`
		if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Write("http://127.0.0.1:9472"); err != nil {
			t.Fatalf("Write: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if env[maxContextTokensKey] != "500000" {
			t.Errorf("%s = %v, want operator's \"500000\" preserved", maxContextTokensKey, env[maxContextTokensKey])
		}
	})
}

// TestRemoveStripsOurMaxContextTokens: disable removes the value waired wrote
// alongside the loopback base URL, but preserves an operator's own override.
func TestRemoveStripsOurMaxContextTokens(t *testing.T) {
	t.Run("ours stripped", func(t *testing.T) {
		p := withTempPath(t)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		// A filler operator key keeps the file (and its env block) from being
		// deleted entirely, so we can assert the specific key was stripped.
		seed := `{"env":{"FOO":"bar","ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"` + directivesMaxContextTokensValue + `"}}`
		if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Remove(); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if v, bad := env[maxContextTokensKey]; bad {
			t.Errorf("Remove left our %s = %v behind", maxContextTokensKey, v)
		}
		if env["FOO"] != "bar" {
			t.Error("Remove clobbered operator's env.FOO")
		}
	})

	t.Run("operator override preserved on disable", func(t *testing.T) {
		p := withTempPath(t)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472",` +
			`"CLAUDE_CODE_MAX_CONTEXT_TOKENS":"500000"}}`
		if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Remove(); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		env := readJSON(t, p)["env"].(map[string]any)
		if env[maxContextTokensKey] != "500000" {
			t.Errorf("%s = %v, want operator's \"500000\" preserved", maxContextTokensKey, env[maxContextTokensKey])
		}
	})
}
