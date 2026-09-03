package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The model switch on a vLLM host (waired-agent#1170).
//
// The defect this file pins was found on real hardware: the browser wizard's
// vLLM opt-in ended with the engine card at ERR and only a daemon restart
// cleared it. SwapPreferredModel publishes BOTH halves of the preference —
// the caller persists preferred-model.json and this publishes the in-process
// override — and its engine guard returned before the in-process half. Every
// reader then kept answering the frozen boot snapshot, so vllmTarget() picked
// the bundled ollama-only model and the bootstrap refused with "no
// vLLM-capable model selected" naming, as its example, the very model the
// wizard had just chosen.
//
// Untagged on purpose: the guard, the publish and the follow-up edge are all
// untagged code, and the windows and darwin legs must run them too
// (CLAUDE.md §Test discipline).

// vllmSwapManifests: one model both engines can serve, one only ollama can.
// The pair is what makes "can the engine this host serves with actually serve
// the target" a question with two answers.
func vllmSwapManifests() []catalog.Manifest {
	return []catalog.Manifest{
		{
			ModelID: "hybrid", ContextLength: 8192, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{
				{
					VariantID: "q4", Format: catalog.FormatOllamaTag,
					RuntimeSupport: []string{catalog.RuntimeOllama},
					Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "hybrid:8b"},
				},
				{
					VariantID:      "safetensors",
					RuntimeSupport: []string{catalog.RuntimeVLLM},
					DType:          "auto",
					Source:         catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "acme/hybrid"},
				},
			},
		},
		{
			ModelID: "ollama-only", ContextLength: 8192, Capabilities: []string{"chat"},
			Variants: []catalog.Variant{{
				VariantID: "q4", Format: catalog.FormatOllamaTag,
				RuntimeSupport: []string{catalog.RuntimeOllama},
				Source:         catalog.VariantSource{Type: catalog.SourceOllama, Tag: "ollama-only:8b"},
			}},
		},
	}
}

// vllmSwapProvider is a vLLM host with no engine up — the state the wizard
// leaves behind while the venv installs and the weights arrive.
//
// Local inference reads "off" so any dispatched bootstrap declines at its
// first gate and records why. That is the observable these tests use to tell
// "the engine was asked to start" from "the reconcile was asked to run", and
// it costs no engine, no venv and no network.
func vllmSwapProvider(t *testing.T) *agentInferenceProvider {
	t.Helper()
	p := &agentInferenceProvider{
		cfg:                 agentconfig.InferenceConfig{AllowPull: true, BundledModelID: "ollama-only"},
		manifests:           vllmSwapManifests(),
		store:               catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		stateDir:            t.TempDir(),
		dlProgress:          newDownloadProgress(),
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		agentCtx:            context.Background(),
		isInferenceDisabled: func() bool { return true },
	}
	p.setServingEngine(catalog.RuntimeVLLM)
	return p
}

// waitForStartDecline waits for a dispatched bootstrap to reach
// startEngineAndBootstrap and record why it declined.
func waitForStartDecline(t *testing.T, p *agentInferenceProvider, what string) {
	t.Helper()
	waitForCond(t, 2*time.Second, what, func() bool {
		d := p.lastStartDecline.Load()
		return d != nil && *d == errInferenceOff.Error()
	})
}

// TestSwapPreferredModel_VLLMHostWithNoEngineAppliesInProcess is the defect.
//
// PRODUCT CONTRACT (waired-agent#1170): a model this host's engine can serve
// is published in process, whichever engine that is. The old guard asked "is
// the serving engine ollama", so a vLLM→vLLM switch — same engine, both
// variants safetensors — was called cross-engine and skipped the publish.
func TestSwapPreferredModel_VLLMHostWithNoEngineAppliesInProcess(t *testing.T) {
	p := vllmSwapProvider(t)
	if err := p.store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"hybrid": {State: catalog.ModelStateReady, VariantID: "safetensors", LocalPath: t.TempDir()},
		}
	}); err != nil {
		t.Fatal(err)
	}

	downloading, err := p.SwapPreferredModel(context.Background(), "hybrid")
	if err != nil {
		t.Fatalf("SwapPreferredModel on a vLLM host with no engine up: %v", err)
	}
	if downloading {
		t.Error("the weights are on disk; downloading should be false")
	}
	if got := p.effectivePreferredModelID(); got != "hybrid" {
		t.Fatalf("effectivePreferredModelID = %q, want hybrid\n"+
			"the in-process preference is what vllmTarget() reads; leaving it on the boot\n"+
			"snapshot is what made the bootstrap refuse with \"no vLLM-capable model selected\"", got)
	}
	// The apply route: a start, not a reconcile. reconcileEngineServe returns
	// immediately on a non-ollama host, so asking for one here would be the
	// same dead end the weights-landed edge used to have.
	waitForStartDecline(t, p, "the switch to ask the engine to start")
	if p.swapPending.Load() {
		t.Error("the switch asked for an ollama reconcile on a vLLM host")
	}
}

// TestSwapPreferredModel_VLLMHostPullsWhenTheWeightsAreAbsent: the wizard's
// own shape — the model was chosen before its weights existed. The preference
// is published now and the pull is dispatched; the engine is asked once the
// weights land (noteWeightsLanded → endPull).
func TestSwapPreferredModel_VLLMHostPullsWhenTheWeightsAreAbsent(t *testing.T) {
	p := vllmSwapProvider(t)
	// dispatchHFPull needs a venv to build its puller and there is none, so
	// the dispatch fails — which is fine and is not what this test asserts.
	// What must hold either way is the publish: it happens before the engine
	// route is chosen, so a host whose download cannot start still stops
	// disagreeing with its own preferred-model.json.
	_, _ = p.SwapPreferredModel(context.Background(), "hybrid")
	if got := p.effectivePreferredModelID(); got != "hybrid" {
		t.Errorf("effectivePreferredModelID = %q, want hybrid", got)
	}
}

// TestSwapPreferredModel_VLLMEngineUpKeepsTheRestartPath pins the boundary
// this fix deliberately did NOT cross.
//
// Record of today's behaviour, with a reason: swapping the model of a RUNNING
// vLLM engine means killing the process and spawning another on the new
// weights — the KV pool is reserved at start-up and held to exit — so it is
// minutes with nothing serving. That stays on the restart path (#347); the
// in-process arm is for a host where nothing is serving in the first place.
func TestSwapPreferredModel_VLLMEngineUpKeepsTheRestartPath(t *testing.T) {
	p := vllmSwapProvider(t)
	p.setVLLM(&recordingAdapter{name: "vllm", health: infruntime.StateReady})
	if err := p.store.Update(func(s *catalog.State) {
		s.Models = map[string]catalog.ModelState{
			"hybrid": {State: catalog.ModelStateReady, VariantID: "safetensors"},
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SwapPreferredModel(context.Background(), "hybrid"); !errors.Is(err, errSwapNeedsRestart) {
		t.Errorf("SwapPreferredModel against a serving vLLM engine = %v, want errSwapNeedsRestart", err)
	}
}

// TestSwapPreferredModel_TargetTheServingEngineCannotLoadNeedsRestart: the
// guard that remains. A vLLM host asked for an ollama-only model has to
// restart — chooseEngine re-decides the engine kind at boot from the
// preference the caller persisted.
func TestSwapPreferredModel_TargetTheServingEngineCannotLoadNeedsRestart(t *testing.T) {
	p := vllmSwapProvider(t)
	if _, err := p.SwapPreferredModel(context.Background(), "ollama-only"); !errors.Is(err, errSwapNeedsRestart) {
		t.Errorf("SwapPreferredModel to an ollama-only model on a vLLM host = %v, want errSwapNeedsRestart", err)
	}
	if got := p.effectivePreferredModelID(); got != "" {
		t.Errorf("effectivePreferredModelID = %q, want empty\n"+
			"publishing a model the running engine cannot serve would have\n"+
			"activatePreferredIfNeeded commit an Active nothing can answer from", got)
	}
}

// TestNoteWeightsLanded is the edge that did not exist: on vLLM a finished
// download is the ONLY thing that can ask a refused bootstrap to try again.
//
// PRODUCT CONTRACT (waired-agent#1170) for the refusal arm. The pending-swap
// arm is runPullJob's #812 behaviour, mirrored here so the two engines agree.
func TestNoteWeightsLanded(t *testing.T) {
	tests := []struct {
		name        string
		engine      string
		pendingSwap string
		refusal     string
		modelID     string
		want        bool
	}{
		{
			name:   "an unrelated boot-time pull asks for nothing",
			engine: catalog.RuntimeOllama, modelID: "hybrid", want: false,
		},
		{
			name:   "the switch this pull was dispatched for bounces the engine",
			engine: catalog.RuntimeOllama, pendingSwap: "hybrid", modelID: "hybrid", want: true,
		},
		{
			name:   "another model's pull does not consume the pending switch",
			engine: catalog.RuntimeOllama, pendingSwap: "hybrid", modelID: "other", want: false,
		},
		{
			// The wizard's host: the bootstrap refused before the weights
			// existed, and nothing else will ever ask it again.
			name:   "weights landing on a vLLM host whose bootstrap refused asks for a start",
			engine: catalog.RuntimeVLLM, refusal: "no vLLM-capable model selected", modelID: "hybrid", want: true,
		},
		{
			// A refusal is not a reason to bounce an ollama engine: it is
			// already up, model-agnostic, and its refusal record is not read.
			name:   "a refusal on an ollama host is not a reason on its own",
			engine: catalog.RuntimeOllama, refusal: "vllm venv not active", modelID: "hybrid", want: false,
		},
		{
			name:   "a vLLM host with no refusal recorded is left alone",
			engine: catalog.RuntimeVLLM, modelID: "hybrid", want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &agentInferenceProvider{}
			p.setServingEngine(tc.engine)
			if tc.pendingSwap != "" {
				id := tc.pendingSwap
				p.pendingSwapModel.Store(&id)
			}
			p.refuseEngineBootstrap(tc.refusal)

			if got := p.noteWeightsLanded(tc.modelID); got != tc.want {
				t.Errorf("noteWeightsLanded(%q) = %v, want %v", tc.modelID, got, tc.want)
			}
			if got := p.swapBounceDeferred.Load(); got != tc.want {
				t.Errorf("swapBounceDeferred = %v, want %v — endPull reads this, not the return", got, tc.want)
			}
		})
	}
}

// TestEndPull_VLLMSwapAsksForAStart: the deferred intent has to reach the
// engine, and reconcileEngineServe is not a route on this host.
func TestEndPull_VLLMSwapAsksForAStart(t *testing.T) {
	p := vllmSwapProvider(t)
	p.swapBounceDeferred.Store(true)

	p.endPull("hybrid")

	waitForStartDecline(t, p, "endPull to ask the vLLM engine to start")
	if p.swapPending.Load() {
		t.Error("endPull asked for an ollama reconcile on a vLLM host")
	}
}

// TestPullInFlight reports what the bootstrap asks before it fetches weights
// itself: its download does not pass through beginPull, so without this two
// `hf download` processes would write the same directory.
func TestPullInFlight(t *testing.T) {
	p := &agentInferenceProvider{}
	if p.pullInFlight("hybrid") {
		t.Error("an empty registry reported a pull in flight")
	}
	job := &pullJob{modelID: "hybrid"}
	if _, joined := p.beginPull(job); joined {
		t.Fatal("precondition: the first claim must not join")
	}
	if !p.pullInFlight("hybrid") {
		t.Error("a claimed slot did not report a pull in flight")
	}
	if p.pullInFlight("other") {
		t.Error("the registry is keyed by model; another model reported in flight")
	}
	p.endPull("hybrid")
	if p.pullInFlight("hybrid") {
		t.Error("the slot was not released")
	}
}
