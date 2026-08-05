package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
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
	// The control plane answers, and nobody is driving this host — which is
	// what every fixture in this file describes. Without it the bundled
	// fallback's dispatch sits out prePullFrameGrace waiting for a frame
	// that a unit test never sends (#379). Ordering is not a race: the
	// note is sticky state, not a wakeup, so the waiter reads it whether it
	// has parked yet or not.
	p.setupNoteDesired("", false)
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

// probeOrderProvider builds a boot fixture whose engine reports a
// CPU-RESIDENT model, so the two-step Strix Halo backend plan finds no GPU
// and restarts the engine on its fallback backend — the boot tail's own
// engine restart, reproduced.
//
// It returns the downloads that had been admitted at each /api/ps the
// probe made. That is the ordering fact, and it is read at a point where
// there is no race to lose: the probe's HTTP call and PullModel's
// state write both happen synchronously on the boot goroutine, so the
// count is decided by the order of the calls and not by when a pull
// goroutine happens to be scheduled.
func probeOrderProvider(t *testing.T, r download.CommandRunner) (*agentInferenceProvider, *fakeSpawner, *bool, func() []int) {
	t.Helper()
	var p *agentInferenceProvider
	var mu sync.Mutex
	var admitted []int

	countAdmitted := func() int {
		st, err := p.store.Load()
		if err != nil {
			return -1
		}
		n := 0
		for _, m := range st.Models {
			switch m.State {
			case catalog.ModelStateQueued, catalog.ModelStateDownloading, catalog.ModelStateVerifying:
				n++
			}
		}
		return n
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/ps" {
			mu.Lock()
			admitted = append(admitted, countAdmitted())
			mu.Unlock()
			// One model loaded, size_vram absent: the CPU-resident shape
			// the probe treats as positive evidence to fall back on.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[{"name":"a:q4","size":1,"size_vram":0}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"a:q4"}]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)

	present := false
	sp := &fakeSpawner{}
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "",
		BinaryResolver: func() (string, error) {
			if !present {
				return "", errors.New("ollama binary not found")
			}
			return "/fake/ollama", nil
		},
		Host: host, Port: port,
		Spawner: sp, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	p = &agentInferenceProvider{
		ollama:       a,
		store:        catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:          agentconfig.InferenceConfig{AllowPull: true},
		manifests:    bounceTestManifests(),
		puller:       download.NewPuller("ollama-fake", r),
		profiler:     cpuSwapProfiler(t),
		logger:       slog.New(slog.DiscardHandler),
		agentCtx:     context.Background(),
		ollamaUsable: func() bool { return present },
	}
	// Strix Halo Linux: ROCm then Vulkan, the only shape that probes.
	p.bootPlan.backend = strixHaloPlan()
	return p, sp, &present, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), admitted...)
	}
}

// THE #359 BAR for the boot path. PRODUCT CONTRACT: every engine restart
// the boot tail performs happens before it asks for a single byte.
//
// bootstrapAfterEngineStart dispatched the model pulls and THEN ran the
// backend probe and the tuning verify, both of which stop and restart the
// engine. `ollama pull` is a client of `ollama serve`, so the tail killed
// the very downloads it had just started — its own, uniquely: no other
// path in #359 attacks a pull it dispatched itself. Two restarts are
// reachable in one boot (a backend fallback and a tuning degrade), so the
// tail could spend two of the job's three attempts before the network was
// ever at fault, and the harm latched: engineBootstrapOnce runs this tail
// exactly once per process.
//
// Driven through the PREFERRED model (the #347 resume), not the bundled
// fallback: #379's hold already parks the fallback's dispatch behind the
// control plane, so it is no longer the exposed one. bootstrapPreferredModel
// still calls PullModel inline, on the boot goroutine, exactly as before.
func TestBootstrap_TheEngineSettlesBeforeAnyDownloadStarts(t *testing.T) {
	r := newBlockingRunner(t)
	p, sp, installed, admittedAtProbe := probeOrderProvider(t, r)
	p.cfg.PreferredModelID = "model-b"
	*installed = true

	p.runEngineBootstrap(context.Background(), "boot")
	r.releaseAll()
	p.waitForPulls()

	if got := sp.count(); got < 2 {
		t.Fatalf("engine spawns = %d, want at least 2 — the probe was expected to "+
			"fall back and restart, which is what this test is about", got)
	}
	admitted := admittedAtProbe()
	if len(admitted) == 0 {
		t.Fatal("the probe never inspected the engine; the ordering has nothing to read")
	}
	if admitted[0] != 0 {
		t.Fatalf("%d download(s) already admitted when the boot tail probed the engine, want 0 — "+
			"the restart it is about to perform kills them", admitted[0])
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

// PRODUCT CONTRACT (issue #542): the EQUAL case of the bar above — when the
// operator's choice IS the hardware auto-select, that model is downloaded
// once.
//
// What provides that today is the ordering (#306): bootstrapPreferredModel
// takes the model on, so the bundled arm never runs and there is no second
// dispatch to dedup. #305's in-flight registry is the backstop underneath —
// it is what caught this when the two ran back to back — and it keeps its
// own pin at its own seam, TestPullModel_SecondDispatchJoinsTheInFlightJob.
//
// Driven through runEngineBootstrap rather than by calling the two
// bootstraps directly. The version this replaces called them in the
// pre-#306 order, through bootstrapBundledModel — a function the product
// stopped calling at #379 — so it exercised a sequence no boot produces
// while keeping that function compilable.
func TestBootstrap_PreferredEqualsBundledDownloadsItOnce(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"   // what the daemon picked from hardware
	p.cfg.PreferredModelID = "model-a" // and what the operator chose too

	got := bootstrapPulledTags(t, p, r, installed)

	if len(got) != 1 || got[0] != "a:q4" {
		t.Fatalf("tags pulled = %v, want exactly [a:q4] — one model, one "+
			"download; two dispatches for the same id fight over one state "+
			"row and whichever lands first wipes the other's byte progress", got)
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
// bundledPrePullTarget is not only a download decision — its already-ready
// arm is the only caller of activateBundledIfUnset on the boot path.
// Gating the whole call on "someone else took the model on" would leave
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

// PRODUCT CONTRACT (issue #526): PullOnStartup=false suppresses the
// DOWNLOAD, and this is the test that exercises the refusal.
//
// PullOnStartup=false is the disk-short verdict from the install-time
// selector (setup.SelectBundledModel: keep the model configured, don't
// pull it now). It must stay off even when nothing else took the model on.
//
// The gate it guards lives in bundledPrePullTarget since #526, one call
// below the caller that used to hold it — with no weights seeded,
// activateBundledIfReady declines and the refusal there is what answers
// false. The sibling below is the other half of that move.
func TestBootstrap_PullOnStartupFalseSuppressesTheFallback(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed := orderProvider(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PullOnStartup = false

	if got := bootstrapPulledTags(t, p, r, installed); len(got) != 0 {
		t.Fatalf("tags pulled = %v, want none", got)
	}
}

// PRODUCT CONTRACT (issue #526): suppressing the startup PULL must not
// suppress the ACTIVATION either.
//
// Same separation as TestBootstrap_SuppressedBundledStillActivatesWeightsOnDisk
// above, for the OTHER suppressor on the same arm. The activation lives
// inside bundledPrePullTarget, and until #526 the whole call sat under
// `else if cfg.PullOnStartup` — so a host that had been told not to
// download never committed the weights it already held either: Active
// nil, EngineReady() false, /inference/benchmark 425ing, Status()
// reporting awaiting_model.
//
// Not a hypothetical config: applyBundledSelection (bundled_model_select.go)
// sets PullOnStartup=false itself on the selector's disk-short verdict —
// precisely the host that ought to be reusing weights rather than
// fetching more.
//
// serveTags is what makes the weights real to the engine as well as to
// state.json: activateBundledIfReady asks engineServesTag first.
func TestBootstrap_PullOnStartupFalseStillActivatesWeightsOnDisk(t *testing.T) {
	r := newBlockingRunner(t)
	p, installed, serveTags := orderProviderServingTags(t, bounceTestManifests(), r)
	p.cfg.BundledModelID = "model-a"
	p.cfg.PullOnStartup = false // the fixture defaults it true
	seedReady(t, p, "model-a", "q4", "a:q4")
	serveTags("a:q4") // the engine really is holding those weights

	// No hold can be parked here in either direction — the ready weights
	// make bundledPrePullTarget answer false, and a regression skips the
	// arm entirely — so waitForPulls returns and the assertion below is
	// what reports the failure.
	*installed = true
	p.runEngineBootstrap(context.Background(), "boot")
	p.waitForPulls()

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	switch {
	case st.Active == nil:
		t.Fatal("Active is nil with the weights right there on disk — being told " +
			"not to DOWNLOAD also stopped the host committing what it already had")
	case st.Active.ModelID != "model-a":
		t.Fatalf("Active.ModelID = %q, want model-a", st.Active.ModelID)
	}
	if got := r.calls(); got != 0 {
		t.Fatalf("pulls executed = %d, want 0 — the download really is still "+
			"suppressed; only the activation was freed", got)
	}
}
