package main

import (
	"strings"
	"testing"
)

// rc9POSIXHookCommand is verbatim what every waired up to v0.0.2-rc9 wrote into
// managed-settings.json, on Windows as well as the Unixes.
const rc9POSIXHookCommand = "command -v waired >/dev/null 2>&1 && waired claude _fallback-hook || true"

// PRODUCT CONTRACT (waired-agent#787): `waired claude status` must not call a
// command "installed" full stop when this computer's shell cannot run it. The
// row was rendered from StopHookInstalled() — a presence check — which is
// precisely how a Windows host reported a working integration over an inert one.
//
// The three OSes are covered here rather than by running on three OSes: CI's
// test jobs are Linux-only (CONTRIBUTING.md §Building and testing), so a branch
// keyed on runtime.GOOS would never execute for Windows.
func TestClaudeHookStatusRows(t *testing.T) {
	cases := map[string]struct {
		goos, cmd  string
		wantFirst  string
		wantNote   bool
		wantInFix  string
		notWantFix string
	}{
		"linux, nothing installed": {
			goos: "linux", cmd: "", wantFirst: "fallback hook:      not installed"},
		"windows, nothing installed": {
			goos: "windows", cmd: "", wantFirst: "fallback hook:      not installed"},

		"linux, rc9 form is correct there": {
			goos: "linux", cmd: rc9POSIXHookCommand, wantFirst: "fallback hook:      installed"},
		"darwin, rc9 form is correct there": {
			goos: "darwin", cmd: rc9POSIXHookCommand, wantFirst: "fallback hook:      installed"},

		"windows, rc9 form cannot be run here": {
			goos: "windows", cmd: rc9POSIXHookCommand,
			wantFirst: "fallback hook:      installed, but not in the form this computer runs",
			wantNote:  true,
			// Windows has no sudo, and the managed-settings rewrite needs an
			// elevated prompt — the wrong half of that pair was waired#752.
			wantInFix:  "elevated (Administrator) prompt",
			notWantFix: "sudo",
		},
		"windows, the bare form": {
			goos: "windows", cmd: "waired claude _fallback-hook",
			wantFirst: "fallback hook:      installed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := claudeHookStatusRows(tc.goos, tc.cmd)
			lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
			if lines[0] != tc.wantFirst {
				t.Errorf("first row = %q, want %q", lines[0], tc.wantFirst)
			}
			// Anti-vacuity: a healthy host must gain no continuation at all, or
			// the note stops meaning anything.
			if hasNote := len(lines) > 1; hasNote != tc.wantNote {
				t.Errorf("continuation present = %v, want %v\n%s", hasNote, tc.wantNote, got)
			}
			if !tc.wantNote {
				return
			}
			for _, l := range lines[1:] {
				if !strings.HasPrefix(l, claudeStatusIndent) {
					t.Errorf("continuation %q is not aligned under the label column", l)
				}
			}
			if tc.wantInFix != "" && !strings.Contains(got, tc.wantInFix) {
				t.Errorf("note does not say %q:\n%s", tc.wantInFix, got)
			}
			if tc.notWantFix != "" && strings.Contains(got, tc.notWantFix) {
				t.Errorf("note says %q, which is wrong for %s:\n%s", tc.notWantFix, tc.goos, got)
			}
		})
	}
}
