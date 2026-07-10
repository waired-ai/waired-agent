package main

import (
	"bytes"
	"strings"
	"testing"
)

// Under `go test` the test binary's stdout is not a TTY, so useColor() is false
// and every styling helper must pass content through unchanged (no ANSI). This
// is the contract the rest of the package relies on to keep its substring
// assertions valid.
func TestStyleHelpersPlainOffTTY(t *testing.T) {
	if useColor() {
		t.Skip("stdout is a TTY in this environment; the plain-mode contract can't be checked here")
	}
	for _, got := range []string{
		bold("hi"), dim("hi"), green("hi"), yellow("hi"), cyan("hi"), product("Claude Code"),
	} {
		if strings.Contains(got, "\x1b[") {
			t.Errorf("styled output leaked an ANSI escape off-TTY: %q", got)
		}
	}
	if product("Claude Code") != "Claude Code" {
		t.Errorf("product() must be identity off-TTY, got %q", product("Claude Code"))
	}
}

func TestBoxPlainKeepsContentGreppable(t *testing.T) {
	if useColor() {
		t.Skip("stdout is a TTY; box() draws a Unicode frame here")
	}
	var out bytes.Buffer
	box(&out, "*", "All done", []string{"Account: a@b.com", "Model: qwen"})
	s := out.String()
	for _, want := range []string{"All done", "Account: a@b.com", "Model: qwen"} {
		if !strings.Contains(s, want) {
			t.Errorf("box output missing %q; out:\n%s", want, s)
		}
	}
	// No box-drawing chars should leak into non-UTF-8 / non-TTY output.
	if strings.ContainsAny(s, "╭╮╰╯│") {
		t.Errorf("box drew Unicode frame chars off a UTF-8 TTY; out:\n%s", s)
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"\x1b[1mabc\x1b[0m", 3}, // SGR sequences are zero-width
		{"🎉", 2},                 // emoji is wide
		{"\x1b[32m🎉\x1b[0m done", 2 + 1 + 4},
		{"⬇️", 2}, // arrow + VS16 (selector is zero-width)
	}
	for _, c := range cases {
		if got := displayWidth(c.in); got != c.want {
			t.Errorf("displayWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
