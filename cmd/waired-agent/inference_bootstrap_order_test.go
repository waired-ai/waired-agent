package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// orderProvider drives the REAL bootstrapAfterEngineStart: bootstrapProvider
// gives a live OllamaAdapter whose binary appears on demand, and this adds
// the catalog and the recording puller the bootstrap needs. The seam is the
// command runner — below the behaviour under test — so which model gets
// downloaded is observed from the arguments `ollama pull` was called with,
// not from a count.
func orderProvider(t *testing.T, manifests []catalog.Manifest, r download.CommandRunner) (*agentInferenceProvider, *bool) {
	t.Helper()
	p, installed, _ := orderProviderServingTags(t, manifests, r)
	return p, installed
}

func orderProviderServingTags(t *testing.T, manifests []catalog.Manifest, r download.CommandRunner) (*agentInferenceProvider, *bool, func(...string)) {
	t.Helper()
	p, _, installed, serveTags := bootstrapProviderServingTags(t)
	p.manifests = manifests
	p.puller = download.NewPuller("ollama-fake", r)
	p.cfg.PullOnStartup = true
	return p, installed, serveTags
}

// seedReady records modelID as already downloaded, the way a completed
// pull (or `waired init`'s own pre-pull) leaves state.json.
func seedReady(t *testing.T, p *agentInferenceProvider, modelID, variantID, tag string) {
	t.Helper()
	if err := p.store.Update(func(s *catalog.State) {
		s.Models[modelID] = catalog.ModelState{
			State: catalog.ModelStateReady, VariantID: variantID, OllamaTag: tag,
		}
	}); err != nil {
		t.Fatalf("seed %s ready: %v", modelID, err)
	}
}

// bootstrapPulledTags runs one full engine bootstrap with the engine
// present and returns the tags `ollama pull` was asked for.
func bootstrapPulledTags(t *testing.T, p *agentInferenceProvider, r *blockingRunner, installed *bool) []string {
	t.Helper()
	*installed = true
	p.runEngineBootstrap(context.Background(), "boot")
	r.releaseAll()
	p.waitForPulls()
	return r.pulledTags()
}

// noOllamaVariantManifest mirrors the three bundled manifests that ship
// with no ollama-servable variant at all (glm-5.2, glm-4.5-air-106b-a12b,
// deepseek-v4-flash). LookupByAlias finds them, so "the preference
// resolves in the catalog" is NOT evidence that anything can be pulled.
func noOllamaVariantManifest(id string) catalog.Manifest {
	return catalog.Manifest{
		ModelID: id,
		Variants: []catalog.Variant{{
			VariantID: "fp8", Format: catalog.FormatSafetensors,
			RuntimeSupport: []string{catalog.RuntimeVLLM},
			Source:         catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: id},
		}},
	}
}

// THE #306 BAR. PRODUCT CONTRACT: one model is downloaded on a boot, and
// it is the operator's.
//
// bootstrapAfterEngineStart dispatched the hardware auto-select AND the
// operator's choice back to back on the same goroutine. #305's registry is
// keyed by model_id alone (deliberately — keying on the tag is what let
// 16.3 GB and 18.0 GB of the same model download at once), so two
// DIFFERENT ids never deduped: rc7 pulled a 9 GB model the daemon picked
// for itself alongside the 44 GB one the operator chose in the wizard, and
// on a 16 GB CI runner the pair drove the box into the OOM killer.
func TestBootstrap_PreferredDiffersFromBundledPullsOnlyThePreferred(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"   // what the daemon picked from hardware
	p.cfg.PreferredModelID = "model-b" // what the operator chose

	got := bootstrapPulledTags(t, p, r, installed)

	if len(got) != 1 || got[0] != "b:q4" {
		t.Fatalf("tags pulled = %v, want exactly [b:q4] — the bundled auto-select "+
			"must not add a second multi-GB download alongside the operator's model", got)
	}
}

// PRODUCT CONTRACT on the SHAPE of the gate: the bundled pre-pull is the
// fallback for a host with nothing else to serve, so it is skipped only
// when the operator's model was actually taken on — never merely because a
// preference exists.
//
// Records today's behaviour (the bundled pull happens either way at the
// time of writing), but it is what rules out the gate this fix was nearly
// written as: `preferredManifest()` returns ok for a manifest with no
// ollama-servable variant, PullModel then refuses it with errEngineTooOld,
// and the host would end up downloading NOTHING — for the life of the
// process, since engineBootstrapOnce latches the tail exactly once.
func TestBootstrap_PreferenceWithNoServableVariantStillPullsTheBundled(t *testing.T) {
	r := newBlockingRunner(t)
	manifests := append(bounceTestManifests(), noOllamaVariantManifest("vllm-only"))
	p, installed := orderProvider(t, manifests, r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PreferredModelID = "vllm-only"

	got := bootstrapPulledTags(t, p, r, installed)

	if len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4] — a preference the engine "+
			"cannot serve must leave the bundled fallback in place", got)
	}
}

// Same contract, the other way a preference can be unusable: it names
// something this agent build has never heard of (the control plane owns
// the model list; the agent ships a frozen catalog).
func TestBootstrap_PreferenceOutsideTheCatalogStillPullsTheBundled(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PreferredModelID = "model-from-the-future"

	got := bootstrapPulledTags(t, p, r, installed)

	if len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4]", got)
	}
}

// PRODUCT CONTRACT: suppressing the bundled PRE-PULL must not suppress
// the bundled ACTIVATION.
//
// bootstrapBundledModel is not only a download — its already-ready arm is
// the only caller of activateBundledIfUnset on the boot path. Gating the
// whole function on "someone else took the model on" would leave
// state.Active nil for the hours the chosen model downloads, on a host
// whose bundled weights are sitting on disk: EngineReady() false,
// engineModelForActive() empty, the boot benchmark 400ing,
// /inference/benchmark 425ing, Capacity 1 and Status() reporting
// awaiting_model.
func TestBootstrap_SuppressedBundledStillActivatesWeightsOnDisk(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed, serveTags := orderProviderServingTags(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PreferredModelID = "model-b"
	seedReady(t, p, "model-a", "q4", "a:q4")
	serveTags("a:q4") // the engine really is holding those weights

	// Observed with the chosen model still downloading — that window is
	// the whole point. runEngineBootstrap returns once the tail has
	// dispatched, and the tail activates synchronously, so this needs no
	// polling. (Once model-b lands, activatePreferredIfNeeded correctly
	// takes the Active slot over; that is not what this pins.)
	*installed = true
	p.runEngineBootstrap(context.Background(), "boot")

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	switch {
	case st.Active == nil:
		t.Fatal("Active is nil while the chosen model downloads — the bundled weights " +
			"already on disk were never committed, so the device serves nothing")
	case st.Active.ModelID != "model-a":
		t.Fatalf("Active.ModelID = %q, want model-a", st.Active.ModelID)
	}

	r.releaseAll()
	p.waitForPulls()
	if got := r.pulledTags(); len(got) != 1 || got[0] != "b:q4" {
		t.Fatalf("tags pulled = %v, want exactly [b:q4]", got)
	}
}

// PullOnStartup=false is the disk-short verdict from the install-time
// selector (setup.SelectBundledModel: keep the model configured, don't
// pull it now). It must stay off even when nothing else took the model on.
func TestBootstrap_PullOnStartupFalseSuppressesTheFallback(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PullOnStartup = false

	if got := bootstrapPulledTags(t, p, r, installed); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none", got)
	}
}
