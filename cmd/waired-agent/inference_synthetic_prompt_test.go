package main

import "testing"

// anchorTokensPerLine is the #625 harness's measured cost of one filler
// line on its anchor model (42431 tok / 1228 lines). A test value, not a
// product constant: syntheticPrompt takes the calibration from its
// caller precisely because it differs per model family.
const anchorTokensPerLine = 35

func TestSyntheticPrompt(t *testing.T) {
	p1 := syntheticPrompt(65536, anchorTokensPerLine, "nonce-a")
	p2 := syntheticPrompt(65536, anchorTokensPerLine, "nonce-b")
	// The #625 calibration for this line shape is ~2.5 chars/token
	// (dense digits tokenize short); the estimate only needs the right
	// ballpark — the real depth is read back from prompt_eval_count.
	if len(p1) < 65536*2 || len(p1) > 65536*4 {
		t.Errorf("prompt length %d chars implausible for 65536 tokens", len(p1))
	}
	// No shared prefix between runs — different nonces must diverge
	// immediately, or the engine's prompt cache poisons the prefill
	// measurement.
	limit := 64
	if p1[:limit] == p2[:limit] {
		t.Error("prompts share a prefix; prompt caching would skew prefill")
	}
}
