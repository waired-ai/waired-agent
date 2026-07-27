package main

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// The standalone (non-daemon) `waired init` had four independent readers
// over one stdin, so a keystroke aimed at one step was spent on the next
// (#223 — the standalone twin of #184/#185). These pin the two halves of
// the fix: one owner for the run, and a discard before its first
// question, since this path has no poll loop that could acknowledge a
// stray keystroke the way the daemon path does.

// hostThatWantsInference is a profile above hardwareEnabledDefault's
// 8 GB threshold, so "Run AI models on this computer?" defaults to Yes —
// which is what makes a stray Enter and a typed "n" distinguishable.
func hostThatWantsInference() hardware.Profile {
	return hardware.Profile{UnifiedMemory: true, UsableVRAMMB: 16384}
}

func TestStandaloneSignInGateDoesNotAnswerTheNextQuestion(t *testing.T) {
	stubOpener(t, nil)
	// A pipe, not a canned string: the two keystrokes belong to different
	// moments — one pressed at the sign-in gate, one typed at a question
	// the operator can see — and a canned string would make both
	// available before either step ran.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	owner := newStdinReader(pr)
	in := promptReader(owner)

	var out strings.Builder
	presentLoginURL(in, &out, "https://cp.example/login/abc", "", gatePrintOnly)

	// The muscle-memory Enter, pressed at a gate that reads nothing.
	go func() { _, _ = pw.Write([]byte("\n")) }()

	// runInit discards before the first question. In production the
	// sign-in round trip has long since delivered the keystroke; here,
	// wait for it rather than racing the reader goroutine.
	waitForCond(t, func() bool { return owner.Discard() > 0 },
		"the stray Enter to reach the stdin owner")

	// Only now does the operator see a question, and answer it.
	go func() { _, _ = pw.Write([]byte("n\n")) }()
	choice := promptInference(in, &out,
		agentconfig.InferenceConfig{}, false, hostThatWantsInference(),
		nil, nil, false /*interactive*/)

	// If the stray Enter had survived it would have taken this question's
	// default (Yes) and "n" would have slid down to the next one — the
	// #223 symptom exactly.
	if choice.Enabled {
		t.Errorf("the first question was answered by the sign-in keystroke, not by the operator:\n%s", out.String())
	}
}

// The same two keystrokes with no discard between them: the stray Enter
// takes the first question's default and the typed answer slides down to
// the next one. Pinned so the guard above cannot quietly become a no-op
// if the hardware default or the prompt order changes.
func TestStandaloneStrayEnterWouldAnswerWithoutTheDiscard(t *testing.T) {
	stubOpener(t, nil)
	owner := newStdinReader(strings.NewReader("\nn\n"))
	in := promptReader(owner)

	var out strings.Builder
	presentLoginURL(in, &out, "https://cp.example/login/abc", "", gatePrintOnly)

	choice := promptInference(in, &out,
		agentconfig.InferenceConfig{}, false, hostThatWantsInference(),
		nil, nil, false)
	if !choice.Enabled {
		t.Fatalf("expected the stray Enter to take the Yes default, got %+v", choice)
	}
	if choice.ShareWithMesh {
		t.Errorf("expected the typed \"n\" to slide down to the sharing question, got %+v", choice)
	}
}

// `waired claude enable` has no owner — it is a one-question command —
// so the statusline prompt must still work off its own scanner.
func TestPromptYesNoReadsItsOwnSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"\n", false}, // default No
		{"", false},   // EOF
	} {
		if got := promptYesNo("q?", bufio.NewScanner(strings.NewReader(tc.in))); got != tc.want {
			t.Errorf("promptYesNo(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Off a terminal there is no owner and the prompts fall back to the
// on-demand scanner they used before #223 — the shape every scripted
// install (`--non-interactive </dev/null`) runs in.
func TestPromptReaderWithoutOwner(t *testing.T) {
	if got := promptReader(nil); got == nil {
		t.Fatal("promptReader(nil) returned no line source")
	}
	owner := newStdinReader(strings.NewReader("x\n"))
	if got := promptReader(owner); got != lineReader(owner) {
		t.Error("promptReader must hand back the owner it was given")
	}
}
