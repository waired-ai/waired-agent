package opencode

import (
	"os"
	"strings"
	"testing"
)

func TestDataPlaneBaseURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9473":  "http://127.0.0.1:9479",
		"http://127.0.0.1:9999":  "http://127.0.0.1:9479",
		"http://localhost:9473":  "http://localhost:9479",
		"https://127.0.0.1:9473": "https://127.0.0.1:9479",
		"":                       "http://127.0.0.1:9479",
		"garbage":                "http://127.0.0.1:9479",
	}
	for in, want := range cases {
		if got := DataPlaneBaseURL(in); got != want {
			t.Errorf("DataPlaneBaseURL(%q) = %q, want %q", in, got, want)
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
		`baseURL: "http://127.0.0.1:9479/v1"`,
		`id: "waired/default"`,
		`id: "waired/coding"`,
		`id: "waired/small"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// The plugin must not carry a bearer token: it points at the no-token
	// data-plane listener.
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
