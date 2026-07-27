package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestYnPrompt_UnparseableThenDefault(t *testing.T) {
	out := &bytes.Buffer{}
	sc := bufio.NewScanner(strings.NewReader("maybe\nperhaps\nwhat\n"))
	got := ynPrompt(out, sc, "Enable?", true)
	if !got {
		t.Errorf("got false, want default true after 3 bad answers")
	}
}

func TestYnPrompt_DefaultHintSpelledOut(t *testing.T) {
	// The hint must spell out the default ("default: Yes/No") so non-native
	// speakers aren't left to infer it from the [Y/n] capitalization alone.
	yes := &bytes.Buffer{}
	_ = ynPrompt(yes, bufio.NewScanner(strings.NewReader("\n")), "Enable?", true)
	if !strings.Contains(yes.String(), "default: Yes") {
		t.Errorf("default-true prompt missing 'default: Yes' hint, got %q", yes.String())
	}
	no := &bytes.Buffer{}
	_ = ynPrompt(no, bufio.NewScanner(strings.NewReader("\n")), "Enable?", false)
	if !strings.Contains(no.String(), "default: No") {
		t.Errorf("default-false prompt missing 'default: No' hint, got %q", no.String())
	}
}
