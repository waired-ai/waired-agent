package main

import (
	"strings"
	"testing"
)

// TestPickerWriteGuard pins every refusal on the way to writing Claude Code's
// /model picker cache. Product contract: the cache may only name a gateway this
// machine is actually routed at.
//
// Every rejected case fails SILENTLY if it slips through — Claude Code neither
// validates nor reports on the file. A cache whose baseUrl does not byte-match
// the live ANTHROPIC_BASE_URL is simply ignored (empty picker, no error), and
// one written on an unrouted host offers entries that route nowhere with no
// surface telling the user the file exists.
func TestPickerWriteGuard(t *testing.T) {
	const managedPath = "/etc/claude-code/managed-settings.json"
	const url = "http://127.0.0.1:9472"

	cases := []struct {
		name      string
		present   bool
		current   string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{name: "routed at the same URL", present: true, current: url, want: url},
		{
			name: "no managed settings at all", present: false, current: "", want: url,
			wantErr: true, errSubstr: "isn't present",
		},
		{
			name: "routed somewhere else", present: true, current: "http://127.0.0.1:9999", want: url,
			wantErr: true, errSubstr: "Refusing to offer rows",
		},
		{
			// The operator repointed ANTHROPIC_BASE_URL at their own gateway.
			name: "operator's non-loopback gateway", present: true, current: "https://gw.example.com", want: url,
			wantErr: true, errSubstr: "Refusing to offer rows",
		},
		{
			// A trailing slash is a real difference to the reader, so it must be
			// one here too — otherwise we write a file Claude Code ignores.
			name: "trailing slash is not a match", present: true, current: url + "/", want: url,
			wantErr: true, errSubstr: "Refusing to offer rows",
		},
		{
			name: "present but no base URL set", present: true, current: "", want: url,
			wantErr: true, errSubstr: "Refusing to offer rows",
		},
		{
			name: "caller passed no base URL", present: true, current: url, want: "",
			wantErr: true, errSubstr: "--base-url is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pickerWriteGuard(tc.present, tc.current, tc.want, managedPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("pickerWriteGuard(%v, %q, %q) = nil, want an error", tc.present, tc.current, tc.want)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not mention %q", err, tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Errorf("pickerWriteGuard(%v, %q, %q) = %v, want nil", tc.present, tc.current, tc.want, err)
			}
		})
	}
}

// TestPickerWriteGuardNamesThePath: the "not present" refusal is the one a user
// can act on, so it has to say which file is missing — the path differs per OS
// and is not somewhere anyone would guess.
func TestPickerWriteGuardNamesThePath(t *testing.T) {
	const managedPath = `C:\Program Files\ClaudeCode\managed-settings.json`
	err := pickerWriteGuard(false, "", "http://127.0.0.1:9472", managedPath)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), managedPath) {
		t.Errorf("error %q does not name the managed-settings path", err)
	}
	if !strings.Contains(err.Error(), "waired claude enable") {
		t.Errorf("error %q does not name the command that fixes it", err)
	}
}
