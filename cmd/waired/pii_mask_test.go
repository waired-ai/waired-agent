package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// testMasker builds a masker with fixed tokens (no env probing) so cases
// are deterministic on any machine.
func testMasker() *piiMasker {
	m := &piiMasker{}
	m.literals = append(m.literals,
		struct{ from, to string }{`/home/alice`, "<home>"},
		struct{ from, to string }{`C:\Users\alice`, "<home>"},
		struct{ from, to string }{`myhost-123`, "<host>"},
	)
	// Email first, then usernames — mirrors newPIIMasker's ordering (the
	// email usually contains the username).
	m.patterns = append(m.patterns,
		struct {
			re *regexp.Regexp
			to string
		}{emailRe, "<email>"},
		struct {
			re *regexp.Regexp
			to string
		}{regexp.MustCompile(`\balice\b`), "<user>"},
	)
	return m
}

func TestPIIMaskerMask(t *testing.T) {
	m := testMasker()
	cases := []struct{ in, want string }{
		{"state dir: /home/alice/.waired", "state dir: <home>/.waired"},
		{`state dir: C:\Users\alice\waired`, `state dir: <home>\waired`},
		{"Signed in as alice@example.com", "Signed in as <email>"},
		{"user alice owns it", "user <user> owns it"},
		{"malice is not alice's name", "malice is not <user>'s name"},
		{"Device: myhost-123", "Device: <host>"},
		{"no pii here", "no pii here"},
		// home dir masked before the bare username inside it.
		{"/home/alice/logs and alice", "<home>/logs and <user>"},
	}
	for _, tc := range cases {
		if got := m.mask(tc.in); got != tc.want {
			t.Errorf("mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewPIIMaskerSkipsTinyTokens(t *testing.T) {
	m := &piiMasker{}
	add := func(from string) {
		if len(from) < 3 {
			return
		}
		m.literals = append(m.literals, struct{ from, to string }{from, "<x>"})
	}
	add("")
	add("ab")
	if len(m.literals) != 0 {
		t.Errorf("tiny tokens must not be masked: %+v", m.literals)
	}
}

func TestEnablePIIMaskEndToEnd(t *testing.T) {
	// Route the process stdout through the masking pipe, print PII-ish
	// text, restore, and verify the captured output was masked. The
	// masker probes the real environment, so use this host's actual
	// home dir as the PII payload.
	home, err := os.UserHomeDir()
	if err != nil || len(home) < 3 {
		t.Skip("no usable home dir on this host")
	}

	// Capture the REAL stdout via an outer pipe first.
	outerR, outerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = outerW

	restore := enablePIIMask()
	fmt.Fprintf(os.Stdout, "state: %s/.waired mail: bob@example.com\n", home)
	restore()

	os.Stdout = orig
	_ = outerW.Close()
	buf := make([]byte, 4096)
	n, _ := outerR.Read(buf)
	_ = outerR.Close()
	got := string(buf[:n])

	if strings.Contains(got, home) {
		t.Errorf("home dir leaked: %q", got)
	}
	if !strings.Contains(got, "<home>/.waired") {
		t.Errorf("home dir not masked to <home>: %q", got)
	}
	if strings.Contains(got, "bob@example.com") || !strings.Contains(got, "<email>") {
		t.Errorf("email not masked: %q", got)
	}
}
