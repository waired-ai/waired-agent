package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// desired_model_id arrives from the control plane and is folded into the
// setup state; the convergence test in Apply is a raw string compare
// against setupPreferredModelID(), which reports the id the model switch
// actually PUBLISHED. Any name the two ends spell differently makes
// setupModelState miss in state.Models (which the pull path keys by
// canonical model_id), so the model step never leaves pending and a
// restart re-applies the choice on every boot.
//
// These tests pin the single canonicalisation point that closes it
// (waired-agent#200). The retirement case is the new one; the ALIAS case
// was already broken before #200 and is fixed by the same line.

// canonicalTestManifests is the shape the daemon actually hands the
// resolver: a live model with an alias, plus the successor the shipped
// retirement table points at.
func canonicalTestManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{ModelID: "qwen2.5-coder-14b-instruct", ModelAliases: []string{"waired/medium", "qwen2.5-coder-14b"}},
		{ModelID: "qwen3.5-0.8b"},
		{ModelID: "granite4-350m", ModelAliases: []string{"waired/tiny"}, InternalOnly: "CI fixture"},
	}
}

// PRODUCT CONTRACT (#200, and the hazard documented at
// cmd/waired/init_modelselect.go's canonicalBundledModelID): a name from
// outside this process becomes the id this device keys its own state by.
//
// The table is on the free function rather than the method, per CLAUDE.md
// §Test discipline: the fake provider delegates to this, so without a
// direct table the real implementation would never be under test.
func TestCanonicalSetupModelID(t *testing.T) {
	ms := canonicalTestManifests()
	cases := []struct {
		name, in, want string
	}{
		{"canonical id is itself", "qwen2.5-coder-14b-instruct", "qwen2.5-coder-14b-instruct"},
		{"alias becomes the id", "waired/medium", "qwen2.5-coder-14b-instruct"},
		{"short alias becomes the id", "qwen2.5-coder-14b", "qwen2.5-coder-14b-instruct"},
		// Resolution takes the COMPLETE set: the control plane can
		// legitimately desire a withheld model, and the routing sentinel
		// pins one on every PR across three operating systems.
		{"withheld alias still resolves", "waired/tiny", "granite4-350m"},
		{"retired id becomes the successor", "qwen2.5-coder-0.5b-instruct", "qwen3.5-0.8b"},
		{"retired alias becomes the successor", "qwen2.5-coder-0.5b", "qwen3.5-0.8b"},
		// Unchanged, not empty: it degrades to exactly the compare the
		// caller would have made anyway, so an unknown desired value fails
		// the way it always did rather than in a new way.
		{"unknown is unchanged", "no-such-model", "no-such-model"},
		{"empty stays empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canonicalSetupModelID(c.in, ms); got != c.want {
				t.Errorf("canonicalSetupModelID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The behaviour, not the helper. Without the canonicalisation the
// reconciler keys everything on the control plane's spelling while the
// device reports the canonical one, so `converged` is permanently false:
// it re-applies on every frame the in-memory guard does not cover, and
// the wizard's model row never reaches done.
//
// Two cases, one mechanism. The alias case fails on origin/main too — it
// is a pre-existing defect this fix happens to close.
func TestSetupConvergesOnANameTheControlPlaneSpellsDifferently(t *testing.T) {
	for _, c := range []struct {
		name    string
		desired string
		serving string // what the device reports it is set to serve
	}{
		{"alias", "waired/medium", "qwen2.5-coder-14b-instruct"},
		{"retired id", "qwen2.5-coder-0.5b-instruct", "qwen3.5-0.8b"},
		{"retired alias", "qwen2.5-coder-0.5b", "qwen3.5-0.8b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeSetupProvider{
				manifests:  canonicalTestManifests(),
				modelState: catalog.ModelStateReady,
				preferred:  c.serving,
			}
			r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
			ctx := context.Background()

			// Three identical frames, as streaming delivers them.
			for i := 0; i < 3; i++ {
				r.Apply(ctx, desiredFrame("", c.desired, 0))
			}

			// Already serving it and the weights are Ready: converged, so
			// nothing to apply. An uncanonicalised compare would apply once
			// here and again after every restart.
			if len(f.applies) != 0 {
				t.Errorf("applies = %v, want none — the device already serves %q",
					f.applies, c.serving)
			}
			if len(f.pulls) != 0 {
				t.Errorf("pulls = %v, want none", f.pulls)
			}

			// And the state echoed back to `waired init`'s watcher names
			// what the device actually serves, so the CLI's own
			// canonicalBundledModelID compare lands on the same string.
			if got := r.SetupState(ctx).DesiredModelID; got != c.serving {
				t.Errorf("SetupState().DesiredModelID = %q, want %q — the CLI watcher "+
					"compares this against the model the daemon reports ready", got, c.serving)
			}
		})
	}
}

// The other half: when the device is NOT yet serving the successor, the
// reconciler applies the SUCCESSOR — never the retired name, which
// setupApplyModel would answer with "unknown model".
func TestSetupAppliesTheSuccessorForARetiredDesiredModel(t *testing.T) {
	f := &fakeSetupProvider{
		manifests:  canonicalTestManifests(),
		modelState: catalog.ModelStateNotPresent,
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		r.Apply(ctx, desiredFrame("", "qwen2.5-coder-0.5b-instruct", 0))
	}

	if len(f.applies) != 1 {
		t.Fatalf("applies = %v, want exactly one", f.applies)
	}
	if f.applies[0] != "qwen3.5-0.8b" {
		t.Errorf("applied %q, want the successor qwen3.5-0.8b — applying the retired "+
			"name is a refusal the wizard reports as a failed step", f.applies[0])
	}
	// Canonicalisation has to happen at the fold, before anything is keyed
	// on the value; the reconciler's own maps are keyed by d.modelID.
	if len(f.canonicalCalls) == 0 || f.canonicalCalls[0] != "qwen2.5-coder-0.5b-instruct" {
		t.Errorf("canonicalCalls = %v, want the control plane's raw value first",
			f.canonicalCalls)
	}
}

// An unknown desired model must keep failing exactly the way it did: the
// retirement table migrates names we withdrew, it does not absorb typos
// into some nearby model.
func TestSetupLeavesAnUnknownDesiredModelAlone(t *testing.T) {
	f := &fakeSetupProvider{
		manifests:  canonicalTestManifests(),
		modelState: catalog.ModelStateNotPresent,
	}
	r := newSetupReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(context.Background(), desiredFrame("", "qwen2.5-coder-0.4b", 0))

	if len(f.applies) != 1 || f.applies[0] != "qwen2.5-coder-0.4b" {
		t.Errorf("applies = %v, want the unknown name passed through unchanged", f.applies)
	}
}
