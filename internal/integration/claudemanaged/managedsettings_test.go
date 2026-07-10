package claudemanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withTempPath redirects the managed-settings path at a temp file for the test.
func withTempPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude-code", "managed-settings.json")
	prev := pathResolver
	pathResolver = func() string { return p }
	t.Cleanup(func() { pathResolver = prev })
	return p
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return obj
}

func TestWriteCreatesBaseURLNoCredential(t *testing.T) {
	p := withTempPath(t)
	got, err := Write("http://127.0.0.1:9472")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got != p {
		t.Errorf("Write returned %q, want %q", got, p)
	}
	obj := readJSON(t, p)
	env, ok := obj["env"].(map[string]any)
	if !ok {
		t.Fatalf("env block missing: %v", obj)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9472" {
		t.Errorf("ANTHROPIC_BASE_URL = %v, want loopback URL", env["ANTHROPIC_BASE_URL"])
	}
	// Crucial #488 invariant: NO credential variable is ever written, so the
	// claude.ai subscription (and auto-mode) is preserved.
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "apiKeyHelper"} {
		if _, bad := env[k]; bad {
			t.Errorf("env unexpectedly contains credential key %q", k)
		}
	}
	if _, bad := obj["apiKeyHelper"]; bad {
		t.Error("object unexpectedly contains apiKeyHelper")
	}
}

func TestWritePreservesExistingKeys(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Operator already has a managed-settings.json with unrelated keys.
	seed := `{"permissions":{"allow":["Bash"]},"env":{"FOO":"bar"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	obj := readJSON(t, p)
	if _, ok := obj["permissions"]; !ok {
		t.Error("Write clobbered operator's permissions key")
	}
	env := obj["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Error("Write clobbered operator's env.FOO")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:9472" {
		t.Error("Write did not set ANTHROPIC_BASE_URL")
	}
}

func TestRemoveDropsOnlyOurKey(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"permissions":{"allow":["Bash"]},"env":{"FOO":"bar","ANTHROPIC_BASE_URL":"http://127.0.0.1:9472"}}`
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
	obj := readJSON(t, p)
	if _, ok := obj["permissions"]; !ok {
		t.Error("Remove clobbered operator's permissions key")
	}
	env := obj["env"].(map[string]any)
	if _, bad := env["ANTHROPIC_BASE_URL"]; bad {
		t.Error("Remove left ANTHROPIC_BASE_URL behind")
	}
	if env["FOO"] != "bar" {
		t.Error("Remove clobbered operator's env.FOO")
	}
}

func TestRemoveDeletesWholeFileWhenSoleContent(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Error("removed=false, want true")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be gone, stat err=%v", err)
	}
}

func TestRemoveLeavesOperatorNonLoopbackURL(t *testing.T) {
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// Operator pointed Claude at their OWN gateway — waired must not strip it.
	seed := `{"env":{"ANTHROPIC_BASE_URL":"https://gw.corp.example/v1"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Error("Remove stripped an operator-owned non-loopback URL")
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://gw.corp.example/v1" {
		t.Error("operator URL was modified")
	}
}

func TestRemoveNoFileIsNoOp(t *testing.T) {
	withTempPath(t)
	removed, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Error("removed=true for absent file")
	}
}

func TestViewReportsState(t *testing.T) {
	withTempPath(t)
	if _, present, _ := View(); present {
		t.Error("View present=true before write")
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	path, present, baseURL := View()
	if path == "" || !present || baseURL != "http://127.0.0.1:9472" {
		t.Errorf("View = (%q, %v, %q), want populated", path, present, baseURL)
	}
}

func TestViewAtExplicitPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "managed-settings.json")
	if present, _ := ViewAt(p); present {
		t.Error("ViewAt present=true for absent file")
	}
	seed := `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472"}}`
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	present, baseURL := ViewAt(p)
	if !present || baseURL != "http://127.0.0.1:9472" {
		t.Errorf("ViewAt = (%v, %q), want populated", present, baseURL)
	}
}

func TestViewAtEmptyPath(t *testing.T) {
	// Empty path = unsupported OS; reports absent rather than erroring.
	if present, baseURL := ViewAt(""); present || baseURL != "" {
		t.Errorf("ViewAt(\"\") = (%v, %q), want (false, \"\")", present, baseURL)
	}
}

func TestWriteInjectsSubagentModel(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != SubagentModelID {
		t.Errorf("CLAUDE_CODE_SUBAGENT_MODEL = %v, want %q", env["CLAUDE_CODE_SUBAGENT_MODEL"], SubagentModelID)
	}

	// Re-enable overwrites a stale value (same policy as the base URL).
	obj := readJSON(t, p)
	obj["env"].(map[string]any)["CLAUDE_CODE_SUBAGENT_MODEL"] = "waired/old-label"
	b, _ := json.Marshal(obj)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	env = readJSON(t, p)["env"].(map[string]any)
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != SubagentModelID {
		t.Errorf("stale label not overwritten: %v", env["CLAUDE_CODE_SUBAGENT_MODEL"])
	}
}

func TestRemoveStripsSubagentModelOnlyWhenOurs(t *testing.T) {
	p := withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove()
	if err != nil || !removed {
		t.Fatalf("Remove = (%v, %v), want (true, nil)", removed, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("file should be gone when waired keys were its only content (stat err=%v)", err)
	}

	// Operator-owned value (not our exact id) must survive Remove.
	operator := map[string]any{"env": map[string]any{
		"ANTHROPIC_BASE_URL":         "http://127.0.0.1:9472",
		"CLAUDE_CODE_SUBAGENT_MODEL": "claude-haiku-4-5",
	}}
	b, _ := json.Marshal(operator)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(); err != nil {
		t.Fatal(err)
	}
	env := readJSON(t, p)["env"].(map[string]any)
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != "claude-haiku-4-5" {
		t.Errorf("operator-owned subagent model clobbered: %v", env["CLAUDE_CODE_SUBAGENT_MODEL"])
	}
	if _, still := env["ANTHROPIC_BASE_URL"]; still {
		t.Error("our loopback base URL should still be removed")
	}
}

func TestSubagentModelAt(t *testing.T) {
	p := withTempPath(t)
	if SubagentModelAt(p) != "" {
		t.Error("missing file must report empty")
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatal(err)
	}
	if got := SubagentModelAt(p); got != SubagentModelID {
		t.Errorf("SubagentModelAt = %q, want %q", got, SubagentModelID)
	}
	if SubagentModelAt("") != "" {
		t.Error("empty path must report empty")
	}
}
