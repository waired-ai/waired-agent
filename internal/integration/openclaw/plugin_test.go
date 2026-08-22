package openclaw

import (
	"strings"
	"testing"
)

func TestRenderEntry_BaseURLAndHooks(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", 200704)
	if err != nil {
		t.Fatalf("renderEntry: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`const BASE_URL = "http://127.0.0.1:9473/v1";`,
		`SYNTHETIC_KEY = "waired-local"`,
		`["default"]`,
		"resolveDynamicModel",
		"resolveSyntheticAuth",
		"registerProvider",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered entry missing %q:\n%s", want, s)
		}
	}
}

// TestProviderBaseURL_UsesTheGatewayItWasGiven replaces a table that pinned
// the opposite: every input, including a pinned non-default port, came back
// rewritten to 9479. That was the defect in
// waired-ai/waired-agent#999 — a host that moved its gateway got a plugin
// pointing somewhere nothing was listening, and Audit compared the file
// against the same wrong constant, so every surface agreed it was healthy.
//
// The property now is that the caller's port survives. The caller resolves
// it from agent.json (cmd/waired/gatewayurl.go), so a pinned port reaches
// the plugin.
func TestProviderBaseURL_UsesTheGatewayItWasGiven(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9473":  "http://127.0.0.1:9473/v1",
		"http://127.0.0.1:19473": "http://127.0.0.1:19473/v1",
		"http://localhost:1234":  "http://localhost:1234/v1",
		// Unusable input still has to produce something dialable.
		"":              "http://127.0.0.1:9473/v1",
		"::not-a-url::": "http://127.0.0.1:9473/v1",
	}
	for in, want := range cases {
		if got := providerBaseURL(in); got != want {
			t.Errorf("providerBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGatewayBaseURL_NoV1Suffix(t *testing.T) {
	if got := GatewayBaseURL("http://127.0.0.1:19473"); got != "http://127.0.0.1:19473" {
		t.Errorf("GatewayBaseURL = %q, want http://127.0.0.1:19473", got)
	}
}
