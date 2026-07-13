package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

func consentInput(dets []agentDetection) integrationConsentInput {
	return integrationConsentInput{
		StepLabel:  "* [3b/4]",
		Detections: dets,
	}
}

func TestPromptIntegrationConsent_DefaultYes(t *testing.T) {
	var out bytes.Buffer
	ok := promptIntegrationConsent(strings.NewReader("\n"), &out, consentInput(nil))
	if !ok {
		t.Fatalf("empty answer should resolve to the default Yes; out:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[Y/n] (default: Yes)") {
		t.Errorf("prompt should advertise default Yes; out:\n%s", out.String())
	}
}

func TestPromptIntegrationConsent_ExplicitNo(t *testing.T) {
	var out bytes.Buffer
	ok := promptIntegrationConsent(strings.NewReader("n\n"), &out, consentInput(nil))
	if ok {
		t.Fatal("explicit 'n' should decline")
	}
	if !strings.Contains(out.String(), "waired link") {
		t.Errorf("decline should print the re-run hint; out:\n%s", out.String())
	}
}

// TestPromptIntegrationConsent_NonInteractive: no stdin read (the reader
// would panic), resolves Yes, and announces the --skip-integration
// opt-out.
func TestPromptIntegrationConsent_NonInteractive(t *testing.T) {
	var out bytes.Buffer
	inp := consentInput(nil)
	inp.NonInteractive = true
	ok := promptIntegrationConsent(panicReader{}, &out, inp)
	if !ok {
		t.Fatal("non-interactive should resolve to Yes")
	}
	if !strings.Contains(out.String(), "--skip-integration") {
		t.Errorf("non-interactive should mention the opt-out; out:\n%s", out.String())
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) {
	panic("non-interactive consent must not read stdin")
}

func TestPromptIntegrationConsent_RendersDetectionsAndSudo(t *testing.T) {
	var out bytes.Buffer
	inp := consentInput([]agentDetection{
		{ID: integration.AgentClaudeCode, Found: true, Detail: "claude at /home/alice/.local/bin/claude (not on PATH)"},
		{ID: integration.AgentOpenCode, Found: false},
	})
	inp.SudoTarget = "alice"
	_ = promptIntegrationConsent(strings.NewReader("\n"), &out, inp)
	s := out.String()
	for _, want := range []string{
		"detected — claude at /home/alice/.local/bin/claude",
		"not detected — can be set up now; activates once installed",
		`set up for user "alice", not root`,
		"Claude Code skills",
		"OpenCode plugin",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q; out:\n%s", want, s)
		}
	}
}

// TestPromptIntegrationConsent_ClaudeManagedDisclosure: when ClaudeManaged is
// set (elevated init), the consent must disclose the system-wide managed
// settings (ANTHROPIC_BASE_URL, no credential) — and never the retired MITM /
// hosts / CA copy. When it is not set, none of the managed-settings copy appears.
func TestPromptIntegrationConsent_ClaudeManagedDisclosure(t *testing.T) {
	managedMarkers := []string{
		"managed settings",
		"ANTHROPIC_BASE_URL",
		"subscription",
	}
	retiredMarkers := []string{"MITM", "/etc/hosts", "NODE_EXTRA_CA_CERTS", "transparent proxy"}

	t.Run("on", func(t *testing.T) {
		var out bytes.Buffer
		inp := consentInput(nil)
		inp.ClaudeManaged = true
		_ = promptIntegrationConsent(strings.NewReader("\n"), &out, inp)
		s := out.String()
		for _, want := range managedMarkers {
			if !strings.Contains(s, want) {
				t.Errorf("ClaudeManaged consent missing %q; out:\n%s", want, s)
			}
		}
		// Interactive installs defer the routing flip to a second question
		// at the end of install (waired#772) — the disclosure must say so.
		if !strings.Contains(s, "at the end of install") {
			t.Errorf("interactive consent must announce the deferred routing question; out:\n%s", s)
		}
		for _, gone := range retiredMarkers {
			if strings.Contains(s, gone) {
				t.Errorf("managed-settings consent must not mention retired %q; out:\n%s", gone, s)
			}
		}
	})

	t.Run("on-non-interactive", func(t *testing.T) {
		var out bytes.Buffer
		inp := consentInput(nil)
		inp.ClaudeManaged = true
		inp.NonInteractive = true
		_ = promptIntegrationConsent(panicReader{}, &out, inp)
		s := out.String()
		for _, want := range managedMarkers {
			if !strings.Contains(s, want) {
				t.Errorf("non-interactive ClaudeManaged consent missing %q; out:\n%s", want, s)
			}
		}
		// Non-interactive keeps the single-consent immediate flip, so the
		// disclosure keeps the present-tense "it also writes" phrasing.
		if !strings.Contains(s, "it also writes") {
			t.Errorf("non-interactive consent must disclose the immediate write; out:\n%s", s)
		}
	})

	t.Run("off", func(t *testing.T) {
		var out bytes.Buffer
		inp := consentInput(nil)
		inp.ClaudeManaged = false
		_ = promptIntegrationConsent(strings.NewReader("\n"), &out, inp)
		s := out.String()
		if strings.Contains(s, "managed settings") || strings.Contains(s, "ANTHROPIC_BASE_URL") {
			t.Errorf("non-managed consent should not mention managed settings; out:\n%s", s)
		}
	})
}

// TestPromptIntegrationConsent_SkipHint proves the decline hint points users at
// `waired claude enable` for Claude routing.
func TestPromptIntegrationConsent_SkipHint(t *testing.T) {
	var out bytes.Buffer
	ok := promptIntegrationConsent(strings.NewReader("n\n"), &out, consentInput(nil))
	if ok {
		t.Fatal("explicit 'n' should decline")
	}
	s := out.String()
	if !strings.Contains(s, "sudo waired claude enable") {
		t.Errorf("decline hint should mention `sudo waired claude enable`; out:\n%s", s)
	}
}

// TestLinkAllChildArgs also guards the flag ordering: stdlib flag
// parsing stops at the first non-flag argument, so the "all" target
// must come last or every flag is silently dropped. The child only
// applies the per-user integration (skills + opencode.json); Claude
// routing is the proxy, set up separately as root, so there are no
// dotfile-consent flags here anymore.
func TestLinkAllChildArgs(t *testing.T) {
	got := linkAllChildArgs("http://127.0.0.1:9473")
	want := []string{"link", "--force", "--no-prompt",
		"--gateway-base-url", "http://127.0.0.1:9473", "all"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestScrubbedChildEnv(t *testing.T) {
	in := []string{
		"HOME=/root",
		"WAIRED_STATE_DIR=/var/lib/waired",
		"XDG_CONFIG_HOME=/root/.config",
		"TERM=xterm-256color",
		"LANG=ja_JP.UTF-8",
		"PATH=/usr/bin",
	}
	got := scrubbedChildEnv(in)
	want := []string{"TERM=xterm-256color", "LANG=ja_JP.UTF-8", "PATH=/usr/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestInvokingSudoUserAt(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		euid     int
		sudoUser string
		want     string
		wantOK   bool
	}{
		{"sudo from alice", "linux", 0, "alice", "alice", true},
		{"sudo from alice on macOS", "darwin", 0, "alice", "alice", true},
		{"real root login", "linux", 0, "", "", false},
		{"sudo from root", "linux", 0, "root", "", false},
		{"not elevated", "linux", 1000, "alice", "", false},
		{"windows has no sudo", "windows", -1, "alice", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := invokingSudoUserAt(tc.goos, tc.euid, tc.sudoUser)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("invokingSudoUserAt(%q, %d, %q) = (%q, %v), want (%q, %v)", tc.goos, tc.euid, tc.sudoUser, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// writeExecutableAt creates an executable file at path, with parents.
func writeExecutableAt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDetectIntegrationAgents_TargetHome proves detection looks at the
// supplied home dir (the SUDO_USER home under the hop), not the
// process's own — the bug that silently skipped the integration on
// `sudo waired init`.
func TestDetectIntegrationAgents_TargetHome(t *testing.T) {
	t.Setenv("PATH", "") // hermetic: PATH lookups must miss
	home := t.TempDir()
	writeExecutableAt(t, home+"/.local/bin/claude")

	dets := detectIntegrationAgents(context.Background(), home)
	var claude *agentDetection
	for i := range dets {
		if dets[i].ID == integration.AgentClaudeCode {
			claude = &dets[i]
		}
	}
	if claude == nil {
		t.Fatal("claude-code detection missing")
	}
	if !claude.Found {
		t.Fatalf("claude at <home>/.local/bin must be detected; got %+v", *claude)
	}
	if !strings.Contains(claude.Detail, home) {
		t.Errorf("Detail should reference the probed home; got %q", claude.Detail)
	}
}
