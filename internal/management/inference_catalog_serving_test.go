package management

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

// waired-agent#1038: the catalog used to read status.Runtimes only for
// version fields and throw the tuning outcome away, so `models ls
// --detail` printed "✓ fits" for a model the running engine had already
// recorded as unservable on this computer.

func servingWarningInference(warning string, degraded bool) *fakeInference {
	return &fakeInference{
		hwProfile: hardware.Profile{RAMTotalGB: 32},
		canned: InferenceStatus{
			Active: &ActiveSelection{ModelID: "qwen3-4b-instruct", VariantID: "q4-gguf"},
			Runtimes: map[string]RuntimeStatus{
				catalog.RuntimeOllama: {
					Installed:      true,
					LiveVersion:    "0.32.13",
					TuningWarning:  warning,
					TuningDegraded: degraded,
				},
			},
		},
		models: []ModelEntry{{ModelID: "qwen3-4b-instruct", State: catalog.ModelStateReady}},
	}
}

func TestModelCatalog_ActiveRowCarriesTheServingWarning(t *testing.T) {
	const warning = "loaded with only 491 MB of GPU memory left free"
	s := newCatalogTestServer(t, servingWarningInference(warning, true), t.TempDir())

	_, got := doGet(t, s, "/waired/v1/inference/catalog")
	for _, f := range got.Families {
		if f.Active {
			if f.ServingWarning != warning {
				t.Errorf("active row serving_warning = %q, want the engine's own sentence", f.ServingWarning)
			}
			if !f.ServingDegraded {
				t.Error("active row serving_degraded = false, want true")
			}
			continue
		}
		// Every other row is a prediction about a model that may never
		// have run here; there is no evidence to attach to it.
		if f.ServingWarning != "" || f.ServingDegraded {
			t.Errorf("row %s carries serving evidence it cannot have: %+v", f.ModelID, f)
		}
	}
}

func TestModelCatalog_PlannedSpillIsNotDegraded(t *testing.T) {
	// The trap: every host serving the planned #624 spill has a warning,
	// and those hosts work. A surface must key on the flag, not on the
	// warning being non-empty.
	const planned = "context window set to 200704 tokens for coding-agent workloads"
	s := newCatalogTestServer(t, servingWarningInference(planned, false), t.TempDir())

	_, got := doGet(t, s, "/waired/v1/inference/catalog")
	for _, f := range got.Families {
		if !f.Active {
			continue
		}
		if f.ServingWarning != planned {
			t.Errorf("serving_warning = %q, want the planned-spill note carried through", f.ServingWarning)
		}
		if f.ServingDegraded {
			t.Error("serving_degraded = true on a working host — the flag must not follow the warning")
		}
	}
}
