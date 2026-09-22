package main

import (
	"bytes"
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
// Product contract: waired-ai/waired#1056 decision 5 required the
// 131k-native class to be opt-in "警告つき" — with the warning. The class
// and its warning left with waired-ai/waired-agent#1400 (owner decision
// 2026-09-16, decisions 3 and 4 of docs/decisions/20260916/0340): nothing
// ships below the ~200k window, and window_too_small has no producer. The
// other reasons still need their clause.
func TestNotRecommendedBecause_CoversEveryReasonTheFitRulesProduce(t *testing.T) {
	// Asserted through the rendered warning, not through
	// notRecommendedBecause, since waired-ai/waired-agent#1435: one reason
	// no longer completes "isn't recommended here" and gets a sentence of
	// its own instead. The contract was never about that helper — it is
	// that a person is told WHY — so it is now checked where a person
	// reads it.
	bare := "runs on this computer, but isn't recommended here."
	for _, reason := range []string{
		hostfit.ReasonWeightsSpill,
		hostfit.ReasonTooSlow,
		hostfit.ReasonWindowExceedsMemory,
		// A custom model's own short window (waired-ai/waired#1481).
		hostfit.ReasonModelWindowShort,
	} {
		var b bytes.Buffer
		warnModelNotRecommended(&b, "qwen3.5-9b", reason)
		got := b.String()
		if got == "" {
			t.Errorf("reason %q printed nothing", reason)
			continue
		}
		if strings.Contains(got, bare) {
			t.Errorf("reason %q rendered the reasonless sentence — the warning says "+
				"only that Waired disagrees, never why:\n%s", reason, got)
		}
		if !strings.Contains(got, "qwen3.5-9b") {
			t.Errorf("reason %q did not name the model:\n%s", reason, got)
		}
	}

	// An unknown code still yields nothing rather than a guess: the
	// sentence is already true without a clause, and the vocabulary is
	// allowed to grow ahead of this CLI.
	if got := notRecommendedBecause("something_new"); got != "" {
		t.Errorf("unknown reason = %q, want no clause", got)
	}
}

// PRODUCT CONTRACT (owner ruling 2026-09-20 on waired-ai/waired-agent#1435,
// recorded in docs/decisions/20260920/2345-…, decision 3): the picker says
// that no coding-agent request will come, NOT that the model cannot run.
// The engine does start, so "cannot run" would be false.
func TestWindowExceedsMemory_SaysWhatStopsAndDoesNotSayCannotRun(t *testing.T) {
	var b bytes.Buffer
	warnModelNotRecommended(&b, "qwen3.8-27b", hostfit.ReasonWindowExceedsMemory)
	got := b.String()

	for _, want := range []string{
		"200,704-token",
		"won't send coding-agent requests",
		"Pick a smaller model",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The two readings the ruling rules out.
	for _, forbidden := range []string{
		"cannot run",
		"can't run",
		"isn't recommended",
		// The internal name for a row in a coding tool's model list. It
		// reads as house vocabulary and means nothing to a person who has
		// not been shown the picker (owner, 2026-09-21).
		"Waired row",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("says %q, which the ruling rules out:\n%s", forbidden, got)
		}
	}
}

// A custom model whose own window is short is not blamed on memory: the
// sentence says the window, and never that this computer can't hold it or
// that Waired won't send requests — both false for it (waired-ai/waired#1481,
// found on real hardware as "window exceeds memory" on a 16 GB Mac).
func TestWarnModelNotRecommended_ShortOwnWindow(t *testing.T) {
	var b bytes.Buffer
	warnModelNotRecommended(&b, "Tiny", hostfit.ReasonModelWindowShort)
	got := b.String()
	for _, want := range []string{"Tiny", "200,704", "overflows it on every turn"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"can't hold", "won't send"} {
		if strings.Contains(got, gone) {
			t.Errorf("claims %q:\n%s", gone, got)
		}
	}
}
