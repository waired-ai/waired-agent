package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const expectedBaseURL = "http://127.0.0.1:9479/v1"

// writePlugin writes a waired.js plugin file into the OpenCode plugin dir.
// body is the raw JS to write.
func writePlugin(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode", "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "waired.js")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func pluginJS(baseURL string) string {
	return `export const WairedPlugin = async () => ({
  config: async (config) => {
    config.provider = config.provider || {};
    config.provider.waired = {
      npm: "@ai-sdk/openai-compatible",
      options: { baseURL: "` + baseURL + `" },
    };
  },
});
`
}

func TestOpenCode_NotConfigured_NoFile(t *testing.T) {
	home := t.TempDir()
	r := OpenCode(home, expectedBaseURL)
	if r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=false stale=false", r)
	}
	if r.Note != "" {
		t.Errorf("Note should be empty when file is absent, got %q", r.Note)
	}
	if !strings.HasSuffix(r.Path, filepath.Join("plugin", "waired.js")) {
		t.Errorf("Path = %q, want to end with plugin/waired.js", r.Path)
	}
}

func TestOpenCode_Configured_FreshMatch(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, pluginJS(expectedBaseURL))
	r := OpenCode(home, expectedBaseURL)
	if !r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=true stale=false", r)
	}
	if r.CurrentValue != expectedBaseURL {
		t.Errorf("CurrentValue = %q, want %q", r.CurrentValue, expectedBaseURL)
	}
}

func TestOpenCode_StaleBaseURL(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, pluginJS("http://127.0.0.1:9999/v1"))
	r := OpenCode(home, expectedBaseURL)
	if !r.Configured {
		t.Errorf("Configured = false, want true (drifted but present)")
	}
	if !r.Stale {
		t.Errorf("Stale = false, want true (baseURL mismatch)")
	}
	if r.CurrentValue != "http://127.0.0.1:9999/v1" {
		t.Errorf("CurrentValue = %q, want the on-disk drifted value", r.CurrentValue)
	}
}

func TestOpenCode_PluginPresentNoProvider(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, "export const Other = async () => ({});\n")
	r := OpenCode(home, expectedBaseURL)
	if r.Configured || r.Stale {
		t.Errorf("got %+v, want configured=false (no provider.waired)", r)
	}
	if !strings.Contains(r.Note, "provider.waired") {
		t.Errorf("Note = %q, want it to mention provider.waired", r.Note)
	}
}

func TestOpenCode_ProviderPresentBaseURLAbsent(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, `export const WairedPlugin = async () => ({
  config: async (config) => { config.provider.waired = { name: "Waired" }; },
});
`)
	r := OpenCode(home, expectedBaseURL)
	if r.Configured {
		t.Errorf("Configured = true with no baseURL: %+v", r)
	}
	if !strings.Contains(r.Note, "baseURL") {
		t.Errorf("Note = %q, want it to mention baseURL", r.Note)
	}
}

// TestOpenCode_EmptyExpectedTreatsAnyAsFresh — when expectedBaseURL is
// blank we cannot say "stale", so a present provider reports
// Configured + !Stale. This is the management API contract.
func TestOpenCode_EmptyExpectedTreatsAnyAsFresh(t *testing.T) {
	home := t.TempDir()
	writePlugin(t, home, pluginJS("http://anywhere/v1"))
	r := OpenCode(home, "")
	if !r.Configured {
		t.Errorf("Configured = false, want true")
	}
	if r.Stale {
		t.Errorf("Stale = true with empty expected, should not flag")
	}
}
