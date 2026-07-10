package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOpenClawPlugin writes an index.mjs into the OpenClaw plugin dir.
func writeOpenClawPlugin(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".openclaw", "plugins", "waired")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index.mjs")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openClawJS(baseURL string) string {
	return `const BASE_URL = "` + baseURL + `";
export default { id: "waired", register(api) { api.registerProvider({ id: "waired" }); } };
`
}

func TestOpenClaw_NotConfigured_NoFile(t *testing.T) {
	home := t.TempDir()
	r := OpenClaw(home, expectedBaseURL)
	if r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=false stale=false", r)
	}
	if r.Note != "" {
		t.Errorf("Note should be empty when file is absent, got %q", r.Note)
	}
	if !strings.HasSuffix(r.Path, filepath.Join("plugins", "waired", "index.mjs")) {
		t.Errorf("Path = %q, want to end with plugins/waired/index.mjs", r.Path)
	}
}

func TestOpenClaw_Configured_FreshMatch(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, openClawJS(expectedBaseURL))
	r := OpenClaw(home, expectedBaseURL)
	if !r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=true stale=false", r)
	}
	if r.CurrentValue != expectedBaseURL {
		t.Errorf("CurrentValue = %q, want %q", r.CurrentValue, expectedBaseURL)
	}
}

func TestOpenClaw_StaleBaseURL(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, openClawJS("http://127.0.0.1:9999/v1"))
	r := OpenClaw(home, expectedBaseURL)
	if !r.Configured || !r.Stale {
		t.Errorf("got %+v, want configured=true stale=true", r)
	}
	if r.CurrentValue != "http://127.0.0.1:9999/v1" {
		t.Errorf("CurrentValue = %q, want the on-disk drifted value", r.CurrentValue)
	}
}

func TestOpenClaw_PluginPresentNoProvider(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, "export default { id: \"other\" };\n")
	r := OpenClaw(home, expectedBaseURL)
	if r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=false (no registerProvider)", r)
	}
	if !strings.Contains(r.Note, "register") {
		t.Errorf("Note = %q, want it to mention register", r.Note)
	}
}

func TestOpenClaw_EmptyExpectedTreatsAnyAsFresh(t *testing.T) {
	home := t.TempDir()
	writeOpenClawPlugin(t, home, openClawJS("http://anywhere/v1"))
	r := OpenClaw(home, "")
	if !r.Configured {
		t.Errorf("Configured = false, want true")
	}
	if r.Stale {
		t.Errorf("Stale = true with empty expected, should not flag")
	}
}
