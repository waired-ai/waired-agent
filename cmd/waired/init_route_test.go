package main

import (
	"bufio"
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func routingPrompt(t *testing.T, input string, apply func() (string, error)) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	ok := promptClaudeRoutingWith(&out, bufio.NewScanner(strings.NewReader(input)),
		"http://127.0.0.1:18080", apply)
	return ok, out.String()
}

func TestPromptClaudeRouting_YesApplies(t *testing.T) {
	applied := 0
	ok, out := routingPrompt(t, "y\n", func() (string, error) {
		applied++
		return "/etc/claude-code/managed-settings.json", nil
	})
	if !ok || applied != 1 {
		t.Errorf("yes must apply once and report routed: ok=%v applied=%d", ok, applied)
	}
	if !strings.Contains(out, "ANTHROPIC_BASE_URL=http://127.0.0.1:18080") {
		t.Errorf("expected the applied-route summary, got:\n%s", out)
	}
}

func TestPromptClaudeRouting_NoSkipsAndHints(t *testing.T) {
	ok, out := routingPrompt(t, "n\n", func() (string, error) {
		t.Error("declining must not write managed settings")
		return "", nil
	})
	if ok {
		t.Error("no must report not-routed")
	}
	for _, want := range []string{"Routing left off", elevatedCmdline(runtime.GOOS, "waired claude enable"), "waired claude route"} {
		if !strings.Contains(out, want) {
			t.Errorf("decline output missing %q; got:\n%s", want, out)
		}
	}
}

// EOF takes the default (Yes) — the same convention as every other init
// prompt; real installs reattach stdin to /dev/tty so this only affects
// scripted runs.
func TestPromptClaudeRouting_EOFDefaultsYes(t *testing.T) {
	applied := 0
	ok, _ := routingPrompt(t, "", func() (string, error) {
		applied++
		return "/etc/claude-code/managed-settings.json", nil
	})
	if !ok || applied != 1 {
		t.Errorf("EOF must take the Yes default: ok=%v applied=%d", ok, applied)
	}
}

func TestPromptClaudeRouting_ApplyErrorReportsOff(t *testing.T) {
	ok, _ := routingPrompt(t, "y\n", func() (string, error) {
		return "", errors.New("permission denied")
	})
	if ok {
		t.Error("a failed write must report not-routed")
	}
}
