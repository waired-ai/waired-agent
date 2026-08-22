package opencode

import (
	"os"
	"strings"
	"testing"
)

// TestGatewayBaseURL_UsesTheGatewayItWasGiven replaces a table that pinned
// the opposite: every input, including "http://127.0.0.1:9999", came back
// rewritten to 9479. A host that pinned a non-default gateway port therefore
// got a plugin pointing at a port nothing was listening on, and Audit
// compared it against the same wrong constant so nothing reported it
// (waired-ai/waired-agent#999).
//
// The property now is that the caller's host and port survive. The caller
// resolves them from agent.json (cmd/waired/gatewayurl.go).
func TestGatewayBaseURL_UsesTheGatewayItWasGiven(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9473":   "http://127.0.0.1:9473",
		"http://127.0.0.1:19473":  "http://127.0.0.1:19473",
		"http://localhost:9473":   "http://localhost:9473",
		"https://127.0.0.1:19473": "https://127.0.0.1:19473",
		// Unusable input still has to produce something dialable.
		"":        "http://127.0.0.1:9473",
		"garbage": "http://127.0.0.1:9473",
	}
	for in, want := range cases {
		if got := GatewayBaseURL(in); got != want {
			t.Errorf("GatewayBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderPlugin(t *testing.T) {
	body, err := renderPlugin("http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"export const WairedPlugin",
		"config.provider.waired",
		`"@ai-sdk/openai-compatible"`,
		`baseURL: "http://127.0.0.1:9473/v1"`,
		`id: "waired/default"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// waired/coding and waired/small were retired in waired-agent#521;
	// the restored plugin (waired-agent#982) offers the one surviving
	// alias, the same single entry the OpenClaw plugin carries.
	for _, gone := range []string{`waired/coding`, `waired/small`} {
		if strings.Contains(s, gone) {
			t.Errorf("plugin still offers the retired alias %q", gone)
		}
	}
	// The plugin must not carry a credential: the gateway has none to check
	// (waired-ai/waired#1277).
	if strings.Contains(s, "apiKey") {
		t.Errorf("plugin should not embed an apiKey:\n%s", s)
	}
}

func TestInstallRemovePlugin(t *testing.T) {
	home := t.TempDir()
	path, err := installPlugin(home, "http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	if path != PluginFile(home) {
		t.Errorf("installPlugin path = %s, want %s", path, PluginFile(home))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	if err := removePlugin(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("plugin survived removePlugin")
	}
}
