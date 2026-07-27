package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

func TestEffectiveOllamaSource(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", agentconfig.OllamaSourceBundled},
		{agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceBundled},
		{agentconfig.OllamaSourceReuse, agentconfig.OllamaSourceReuse},
	}
	for _, tc := range cases {
		if got := effectiveOllamaSource(tc.in); got != tc.want {
			t.Errorf("effectiveOllamaSource(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
