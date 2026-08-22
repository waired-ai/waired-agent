package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestNotRecommendedBecause_CoversEveryReasonTheFitRulesProduce.
//
// #464 added two recommendation reasons — a model whose OWN window is
// too small for a coding session, and one whose window will not fit this
// machine's memory — and this switch still knew only the two that
// predated it. So the model class #465 item 5 is entirely about (the
// 131k-native gpt-oss / glm-4.5-air group) printed "Waired would not
// choose it here" and stopped, with no reason and no warning, at the
// exact moment the user is deciding to spend a multi-gigabyte download
// on it.
//
// Product contract: waired-ai/waired#1056 decision 5 requires the
// 131k-native class to be opt-in "警告つき" — with the warning. This is
// where the warning is.
func TestNotRecommendedBecause_CoversEveryReasonTheFitRulesProduce(t *testing.T) {
	// Every reason hostfit can put on Presentation.NotRecommendedReason.
	// A new one added without copy here would print nothing, which is how
	// this gap arrived — so the list is exhaustive on purpose.
	for _, reason := range []string{
		hostfit.ReasonWeightsSpill,
		hostfit.ReasonTooSlow,
		hostfit.ReasonWindowTooSmall,
		hostfit.ReasonWindowExceedsMemory,
	} {
		if got := notRecommendedBecause(reason); got == "" {
			t.Errorf("reason %q renders no clause — the warning would say only "+
				"that Waired disagrees, never why", reason)
		}
	}

	// An unknown code still yields nothing rather than a guess: the
	// sentence is already true without a clause, and the vocabulary is
	// allowed to grow ahead of this CLI.
	if got := notRecommendedBecause("something_new"); got != "" {
		t.Errorf("unknown reason = %q, want no clause", got)
	}
}

// The 131k class is the one whose cost is not about this computer at
// all — no hardware makes it hold a coding session — so its clause has
// to name the session limit rather than the machine, and point at the
// one thing that helps.
func TestNotRecommendedBecause_WindowTooSmallNamesTheSessionLimit(t *testing.T) {
	got := notRecommendedBecause(hostfit.ReasonWindowTooSmall)
	for _, want := range []string{"long coding session", "compact"} {
		if !strings.Contains(got, want) {
			t.Errorf("clause %q does not mention %q", got, want)
		}
	}
	// Not a hardware complaint: a bigger machine changes nothing here,
	// and saying otherwise sends someone shopping.
	for _, unwanted := range []string{"graphics card", "GPU", "VRAM", "memory"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("clause %q blames the hardware; no hardware helps this one", got)
		}
	}
}
