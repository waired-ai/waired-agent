package inference

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestServingTerms_ServingReady walks every combination of the terms.
// Exhaustive rather than sampled because this predicate is the product's
// single answer to "is this node ready", and the table is the only place
// the whole answer is visible at once.
func TestServingTerms_ServingReady(t *testing.T) {
	cases := []struct {
		name   string
		terms  ServingTerms
		ready  bool
		reason string
	}{
		{
			name:  "everything true and resident",
			terms: ServingTerms{EngineReady: true, ModelResident: boolPtr(true)},
			ready: true,
		},
		{
			// The case waired-agent#1307 measured: the engine is up, the
			// weights are on disk, and the agent's own warm-up is
			// reading them into memory. Every predicate before this one
			// said ready here.
			name:   "engine up, load in flight, nothing resident",
			terms:  ServingTerms{EngineReady: true, ModelResident: boolPtr(false), ModelLoading: true},
			reason: NotReadyModelLoad,
		},
		{
			// A cold peer is not unready, it is slower — which the
			// ranking already expresses.
			// docs/decisions/20260822/0218 rules that residency breaks a
			// tie and never a ranking, and excluding cold hosts here
			// would starve a mesh whose peers are all cold: nothing
			// admitted, so nothing ever warmed.
			name:  "observed cold with no load running",
			terms: ServingTerms{EngineReady: true, ModelResident: boolPtr(false)},
			ready: true,
		},
		{
			// The first seconds after an engine start: no probe has run,
			// so residency is unobserved, and the load latch is the only
			// thing that can speak for that window.
			name:   "residency unobserved, load in flight",
			terms:  ServingTerms{EngineReady: true, ModelLoading: true},
			reason: NotReadyModelLoad,
		},
		{
			// docs/decisions/20260820/0130: nil is "we have not looked",
			// and docs/decisions/20260822/0218: an unobserved peer is
			// neither promoted nor demoted. A host that answers nothing
			// about residency keeps the readiness it had before the term
			// existed.
			name:  "residency unobserved, nothing loading",
			terms: ServingTerms{EngineReady: true},
			ready: true,
		},
		{
			// A switch loads the new model while the old one keeps
			// answering (docs/decisions/20260813/2123), so a load in
			// flight over a resident model is not an interruption.
			name:  "resident and loading a replacement",
			terms: ServingTerms{EngineReady: true, ModelResident: boolPtr(true), ModelLoading: true},
			ready: true,
		},
		{
			name:   "engine not ready outranks residency",
			terms:  ServingTerms{ModelResident: boolPtr(true)},
			reason: NotReadyEngine,
		},
		{
			name:   "paused",
			terms:  ServingTerms{EngineReady: true, Paused: true, ModelResident: boolPtr(true)},
			reason: NotReadyPaused,
		},
		{
			name:   "measuring outranks residency",
			terms:  ServingTerms{EngineReady: true, Measuring: true, ModelResident: boolPtr(true)},
			reason: NotReadyMeasuring,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.terms.ServingReady(); got != tc.ready {
				t.Errorf("ServingReady() = %v, want %v", got, tc.ready)
			}
			if got := tc.terms.NotReadyReason(); got != tc.reason {
				t.Errorf("NotReadyReason() = %q, want %q", got, tc.reason)
			}
			// The two must never disagree: an empty reason IS ready.
			if (tc.terms.NotReadyReason() == "") != tc.terms.ServingReady() {
				t.Errorf("ServingReady and NotReadyReason disagree: %+v", tc.terms)
			}
		})
	}
}

// TestHealthSnapshot_ServingTerms pins that the snapshot hands the
// predicate every term it has. A field added to the snapshot and not to
// this projection is a term that silently stops counting.
func TestHealthSnapshot_ServingTerms(t *testing.T) {
	snap := HealthSnapshot{
		EngineReady:   true,
		Paused:        true,
		Measuring:     true,
		ModelResident: boolPtr(false),
		ModelLoading:  true,
	}
	got := snap.ServingTerms()
	want := ServingTerms{
		EngineReady:   true,
		Paused:        true,
		Measuring:     true,
		ModelResident: snap.ModelResident,
		ModelLoading:  true,
	}
	if got != want {
		t.Errorf("ServingTerms() = %+v, want %+v", got, want)
	}
}
