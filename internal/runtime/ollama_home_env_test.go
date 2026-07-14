package runtime

import (
	"strings"
	"testing"
)

// envValues returns every value assigned to key in an env slice (there
// should be at most one after processEnv's de-duplication).
func envValues(env []string, key string) []string {
	var vs []string
	for _, kv := range env {
		if envKey(kv) == key {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				vs = append(vs, kv[i+1:])
			}
		}
	}
	return vs
}

// TestOllamaAdapter_ProcessEnv_HomeFallback covers the #22 fix: a macOS
// system LaunchDaemon starts `ollama serve` with $HOME unset, and ollama
// aborts with "$HOME is not defined". processEnv must supply a writable
// StateHome in that case, but never override a HOME the launcher already
// set (Linux systemd), and never fabricate one when StateHome is unset.
func TestOllamaAdapter_ProcessEnv_HomeFallback(t *testing.T) {
	const stateHome = "/var/state/runtimes/ollama"
	newAdapter := func() *OllamaAdapter {
		return NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
			ModelsDir: "/var/state/runtimes/ollama/models",
			StateHome: stateHome,
		})
	}

	t.Run("injects HOME when the launcher has none", func(t *testing.T) {
		t.Setenv("HOME", "") // macOS system LaunchDaemon: $HOME unset/empty
		got := envValues(newAdapter().processEnv(), "HOME")
		if len(got) != 1 || got[0] != stateHome {
			t.Fatalf("HOME entries = %v, want exactly [%q]", got, stateHome)
		}
	})

	t.Run("keeps an inherited HOME", func(t *testing.T) {
		t.Setenv("HOME", "/Users/dev")
		got := envValues(newAdapter().processEnv(), "HOME")
		if len(got) != 1 || got[0] != "/Users/dev" {
			t.Fatalf("HOME entries = %v, want exactly [/Users/dev] (no override)", got)
		}
	})

	t.Run("no StateHome never fabricates a HOME", func(t *testing.T) {
		t.Setenv("HOME", "")
		a := NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
		})
		for _, v := range envValues(a.processEnv(), "HOME") {
			if v != "" {
				t.Fatalf("unexpected HOME=%q injected without StateHome", v)
			}
		}
	})
}
