package openclaw

import (
	"strings"
	"testing"
)

func TestRenderEntry_BaseURLAndHooks(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473")
	if err != nil {
		t.Fatalf("renderEntry: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		`const BASE_URL = "http://127.0.0.1:9479/v1";`,
		`SYNTHETIC_KEY = "waired-local"`,
		`["default", "coding", "small"]`,
		"resolveDynamicModel",
		"resolveSyntheticAuth",
		"registerProvider",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered entry missing %q:\n%s", want, s)
		}
	}
}

func TestRenderEntry_PortSwapAndFallback(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9473": "http://127.0.0.1:9479/v1",
		"http://localhost:1234": "http://localhost:9479/v1",
		"":                      "http://127.0.0.1:9479/v1",
		"::not-a-url::":         "http://127.0.0.1:9479/v1",
	}
	for in, want := range cases {
		if got := providerBaseURL(in); got != want {
			t.Errorf("providerBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDataPlaneBaseURL_NoV1Suffix(t *testing.T) {
	if got := DataPlaneBaseURL("http://127.0.0.1:9473"); got != "http://127.0.0.1:9479" {
		t.Errorf("DataPlaneBaseURL = %q, want http://127.0.0.1:9479", got)
	}
}
