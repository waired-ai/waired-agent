package main

import (
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
// Product contract: waired-ai/waired#1056 decision 5 required the
// 131k-native class to be opt-in "警告つき" — with the warning. The class
// and its warning left with waired-ai/waired-agent#1400 (owner decision
// 2026-09-16, decisions 3 and 4 of docs/decisions/20260916/0340): nothing
// ships below the ~200k window, and window_too_small has no producer. The
// other reasons still need their clause.
func TestNotRecommendedBecause_CoversEveryReasonTheFitRulesProduce(t *testing.T) {
	// Every reason hostfit can put on Presentation.NotRecommendedReason.
	// A new one added without copy here would print nothing, which is how
	// this gap arrived — so the list is exhaustive on purpose.
	for _, reason := range []string{
		hostfit.ReasonWeightsSpill,
		hostfit.ReasonTooSlow,
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
