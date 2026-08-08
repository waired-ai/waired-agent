package setup

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// These run against the REAL bundled catalog, not a synthetic manifest
// set. The property under test is not "SelectBundledModel honours the
// list it is handed" — it does, trivially — but "the list production
// hands it excludes withheld models". A hand-built fixture would pass
// while the wiring was broken, which is the whole failure mode.
func realCatalog(t *testing.T) []catalog.Manifest {
	t.Helper()
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("BundledManifests: %v", err)
	}
	return ms
}

func internalModelIDs(t *testing.T) []string {
	t.Helper()
	all, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	var out []string
	for _, m := range all {
		if m.InternalOnly != "" {
			out = append(out, m.ModelID)
		}
	}
	return out
}

// A withheld model is invisible to install-time selection on every host
// size — including, and especially, the small ones.
//
// The small ones are the point. A withheld model is small (that is why
// it is cheap enough to be a test fixture), so it fits exactly the
// hosts that have nothing else, and the under-spec path exists to offer
// a below-the-floor model to precisely those hosts. Without this,
// "we don't offer it" would hold everywhere except where it would
// actually be reached for.
func TestSelectBundledModel_neverOffersAnInternalModel(t *testing.T) {
	internal := internalModelIDs(t)
	if len(internal) == 0 {
		t.Skip("no manifest is marked internal_only; nothing to withhold")
	}
	withheld := make(map[string]bool, len(internal))
	for _, id := range internal {
		withheld[id] = true
	}
	manifests := realCatalog(t)

	// 2 GB is where a withheld fixture would fit and a real coding model
	// would not; 1 GB is below everything; the rest bracket the range.
	for _, ramGB := range []int{1, 2, 4, 8, 16, 32, 64} {
		in := BundledModelInputs{
			Hardware:  cpuProfile(ramGB),
			Manifests: manifests,
			Inference: agentconfig.InferenceConfig{
				BundledModelID: "qwen3.5-4b",
			},
			StateDir:      "/var/lib/waired",
			FreeDiskBytes: fixedDisk(500),
		}
		sel, err := SelectBundledModel(in)
		if err != nil {
			t.Fatalf("%d GB: %v", ramGB, err)
		}
		// #522 removed the below-recommended-spec fallback that used to
		// need its own check here: it reached past the quality floor for a
		// host that had nothing else, and a withheld model is exactly the
		// shape of thing it would find there. There is one selection path
		// now, so one assertion covers every host in the sweep.
		if withheld[sel.ModelID] {
			t.Errorf("%d GB host: selected the withheld model %q", ramGB, sel.ModelID)
		}
	}
}

// The pinned path bypasses auto-selection entirely — an operator asking
// for a specific model gets it. That is correct and is how CI pins the
// fixture; this records it so nobody "fixes" it into a filter later and
// silently breaks the routing sentinel.
func TestSelectBundledModel_pinStillHonoursAnInternalModel(t *testing.T) {
	internal := internalModelIDs(t)
	if len(internal) == 0 {
		t.Skip("no manifest is marked internal_only")
	}
	in := BundledModelInputs{
		Hardware:  cpuProfile(4),
		Manifests: realCatalog(t),
		Inference: agentconfig.InferenceConfig{
			BundledModelID: internal[0],
		},
		StateDir: "/var/lib/waired",
		Pinned:   true,
	}
	sel, err := SelectBundledModel(in)
	if err != nil {
		t.Fatalf("pinned select: %v", err)
	}
	if sel.ModelID != internal[0] {
		t.Errorf("a pinned internal model must be honoured verbatim; got %q want %q",
			sel.ModelID, internal[0])
	}
	if !sel.EnableInference {
		t.Error("a pinned model should not disable inference")
	}
}
