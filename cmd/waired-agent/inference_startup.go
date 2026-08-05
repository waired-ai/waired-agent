package main

import (
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// inferenceStartupPlan is what the boot decision produces: the state the
// inference controller starts in, and whether that state is new enough
// to be worth writing down.
type inferenceStartupPlan struct {
	State   state.InferenceState
	Persist bool
}

// planInitialInference decides whether local inference is on at boot.
//
// The three inputs, weakest first:
//
//	cfgEnabled  agentconfig.Inference.Enabled — the INSTALL-TIME default.
//	            A host below the recommended spec gets false here, which
//	            is a default and nothing more.
//	persisted   <state-dir>/runtime/desired-inference — the choice made
//	            since, from the CLI, the tray, the browser wizard or the
//	            management API. Empty means nobody has moved the toggle.
//	explicit    --inference-enabled / WAIRED_INFERENCE_ENABLED on this
//	            boot, or nil. The only signal newer than the file.
//
// This replaces `if !cfgRoot.Inference.Enabled { *disableInference = true }`,
// which made the install-time default a kill switch: the inference
// subsystem was never constructed, so the management routes that could
// turn it back on were never registered, the onboarding capability was
// withdrawn (the browser wizard's dead end) and the tray hid its
// inference group on the resulting 404. Nothing in the product wrote
// agent.json, so the state had no exit. The ratified position is that a
// host below the recommended spec gets local inference off as a DEFAULT
// with a working opt-in — waired-ai/waired#1056 (2026-08-03 owner
// decision), recorded in
// waired/docs/decisions/20260803/1332-hard-vs-soft-model-limits.md §4.
//
// --disable-inference is untouched by all of this and remains the
// operator's kill switch. It is a flag, not persisted state, so it
// cannot latch.
//
// Persisting an explicit flag is the other half of the same defect: the
// documented recovery path (`--inference-enabled=true`) won for exactly
// one boot and wrote nothing, so a machine came back latched and the
// docs were wrong about their own advice. Saying it on the command line
// is now the same act as saying it from the CLI or the tray.
func planInitialInference(cfgEnabled bool, persisted state.InferenceState, explicit *bool) inferenceStartupPlan {
	switch {
	case explicit != nil:
		s := state.InferenceDisabled
		if *explicit {
			s = state.InferenceEnabled
		}
		// Only when it actually changes something: a unit file carrying
		// the flag would otherwise rewrite the file on every restart.
		return inferenceStartupPlan{State: s, Persist: s != persisted}
	case persisted != "":
		return inferenceStartupPlan{State: persisted}
	case cfgEnabled:
		return inferenceStartupPlan{State: state.InferenceEnabled}
	default:
		return inferenceStartupPlan{State: state.InferenceDisabled}
	}
}
