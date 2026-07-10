package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureLinkStdout swaps os.Stdout for a pipe, runs fn, and returns the
// bytes that fn wrote. Used to assert on user-facing CLI output.
func captureLinkStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	fn()
	_ = w.Close()
	return string(<-done)
}

func TestPrintLinkPlan_ApplyAll(t *testing.T) {
	out := captureLinkStdout(t, func() {
		_ = printLinkPlan("all", false, false, "/h", "/s", "http://127.0.0.1:9473")
	})
	for _, want := range []string{
		"apply coding-agent",
		"$HOME              = /h",
		"state directory    = /s",
		"gateway base URL   = http://127.0.0.1:9473",
		"agents             = claude-code (skills only), opencode (plugin), openclaw (plugin + openclaw.json)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestPrintLinkPlan_UninstallSingle(t *testing.T) {
	out := captureLinkStdout(t, func() {
		_ = printLinkPlan("claude-code", true, false, "/h", "/s", "http://x")
	})
	if !strings.Contains(out, "remove coding-agent integration (claude-code)") {
		t.Errorf("missing remove summary:\n%s", out)
	}
	if strings.Contains(out, "shell-rc           =") {
		t.Errorf("uninstall plan should not list shell-rc as a write target:\n%s", out)
	}
}

// TestRunLink_UnlinkFlagSurface pins the CLI surface: removal takes no
// consent flags, so `waired unlink --force` must be a parse error
// instead of being silently accepted, while --dry-run still works.
func TestRunLink_UnlinkFlagSurface(t *testing.T) {
	if err := runLink([]string{"--force", "all"}, true); err == nil {
		t.Error("unlink --force should be a flag parse error")
	}
	out := captureLinkStdout(t, func() {
		if err := runLink([]string{"--dry-run"}, true); err != nil {
			t.Errorf("unlink --dry-run: %v", err)
		}
	})
	if !strings.Contains(out, "remove coding-agent integration (all)") {
		t.Errorf("unlink --dry-run should print the removal plan; out:\n%s", out)
	}
}

func TestResolveLinkForce(t *testing.T) {
	undetected := []agentDetection{{ID: "claude-code", Found: false}, {ID: "opencode", Found: true}}
	allFound := []agentDetection{{ID: "claude-code", Found: true}, {ID: "opencode", Found: true}}

	cases := []struct {
		name        string
		in          string
		force       bool
		noPrompt    bool
		interactive bool
		dets        []agentDetection
		want        bool
	}{
		{"force flag wins without prompting", "", true, true, false, undetected, true},
		{"interactive default Yes", "\n", false, false, true, undetected, true},
		{"interactive explicit no", "n\n", false, false, true, undetected, false},
		{"everything detected: no prompt needed", "", false, false, true, allFound, false},
		{"non-interactive stays detect-gated", "", false, false, false, undetected, false},
		{"no-prompt stays detect-gated", "", false, true, true, undetected, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := resolveLinkForce(strings.NewReader(tc.in), &out,
				tc.force, tc.noPrompt, tc.interactive, tc.dets)
			if got != tc.want {
				t.Errorf("resolveLinkForce = %v, want %v; out:\n%s", got, tc.want, out.String())
			}
		})
	}
}
