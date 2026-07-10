package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/setup"
)

func TestPromptOllamaSource(t *testing.T) {
	installedSupported := setup.OllamaDetection{Installed: true, Path: "/usr/bin/ollama", Version: "0.24.0", Supported: true}
	installedOld := setup.OllamaDetection{Installed: true, Path: "/usr/bin/ollama", Version: "0.5.0", Supported: false}
	absent := setup.OllamaDetection{}

	cases := []struct {
		name           string
		det            setup.OllamaDetection
		override       string
		nonInteractive bool
		input          string
		want           string
		wantWarn       bool
	}{
		{"override reuse wins", installedSupported, "reuse", false, "", agentconfig.OllamaSourceReuse, false},
		{"override bundled wins", installedOld, "bundled", false, "", agentconfig.OllamaSourceBundled, false},
		{"not installed -> bundled", absent, "", false, "", agentconfig.OllamaSourceBundled, false},
		{"non-interactive -> bundled", installedSupported, "", true, "", agentconfig.OllamaSourceBundled, false},
		{"interactive default (empty) -> bundled", installedSupported, "", false, "\n", agentconfig.OllamaSourceBundled, false},
		{"interactive N -> reuse", installedSupported, "", false, "n\n", agentconfig.OllamaSourceReuse, false},
		{"unsupported still defaults bundled + warns", installedOld, "", false, "\n", agentconfig.OllamaSourceBundled, true},
		{"unsupported opt-in reuse", installedOld, "", false, "n\n", agentconfig.OllamaSourceReuse, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			got := promptOllamaSource(strings.NewReader(tc.input), &out, tc.det, tc.override, tc.nonInteractive)
			if got != tc.want {
				t.Errorf("source = %q, want %q", got, tc.want)
			}
			warned := strings.Contains(out.String(), "supported minimum")
			if warned != tc.wantWarn {
				t.Errorf("warning printed = %v, want %v (out=%q)", warned, tc.wantWarn, out.String())
			}
		})
	}
}

func TestValidateOllamaSourceFlag(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{agentconfig.OllamaSourceBundled, false},
		{agentconfig.OllamaSourceReuse, false},
		{"system", true},
		{"xyz", true},
		{"Bundled", true}, // case-sensitive: the on-wire value is lower-case
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := validateOllamaSourceFlag(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateOllamaSourceFlag(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

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

func TestRenewOllamaSourceChange(t *testing.T) {
	cases := []struct {
		name        string
		current     string
		override    string
		wantNext    string
		wantChanged bool
	}{
		{"no override keeps bundled", agentconfig.OllamaSourceBundled, "", agentconfig.OllamaSourceBundled, false},
		{"no override keeps reuse", agentconfig.OllamaSourceReuse, "", agentconfig.OllamaSourceReuse, false},
		{"no override keeps empty", "", "", "", false},
		{"same value is a no-op", agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceBundled, false},
		{"empty current vs bundled override is a no-op", "", agentconfig.OllamaSourceBundled, "", false},
		{"bundled -> reuse", agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceReuse, agentconfig.OllamaSourceReuse, true},
		{"reuse -> bundled", agentconfig.OllamaSourceReuse, agentconfig.OllamaSourceBundled, agentconfig.OllamaSourceBundled, true},
		{"empty current -> reuse", "", agentconfig.OllamaSourceReuse, agentconfig.OllamaSourceReuse, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, changed := renewOllamaSourceChange(tc.current, tc.override)
			if next != tc.wantNext || changed != tc.wantChanged {
				t.Errorf("renewOllamaSourceChange(%q, %q) = (%q, %v), want (%q, %v)",
					tc.current, tc.override, next, changed, tc.wantNext, tc.wantChanged)
			}
		})
	}
}
