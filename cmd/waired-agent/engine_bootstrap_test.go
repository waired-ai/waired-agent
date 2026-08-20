package main

import (
	"context"
	"encoding/json"
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
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// bootstrapProvider builds a provider around a real OllamaAdapter whose
// binary is UNRESOLVED at construction time, the way a fresh install
// boots. installed flips what the resolver answers, so a test can make an
// engine appear the way the setup executor does.
//
// The seam is the binary resolver — below the behaviour under test — not
// startEngineAndBootstrap itself, so the subject really runs.
func bootstrapProvider(t *testing.T) (p *agentInferenceProvider, sp *fakeSpawner, installed *bool) {
	t.Helper()
	p, sp, installed, _ = bootstrapProviderServingTags(t)
	return p, sp, installed
}

// bootstrapProviderServingTags is bootstrapProvider with the engine's
// /api/tags answer under the test's control, so a test can say "the
// engine really is serving these weights" — what engineServesTag checks
// before activating a model that is already on disk.
func bootstrapProviderServingTags(t *testing.T) (p *agentInferenceProvider, sp *fakeSpawner, installed *bool, serveTags func(...string)) {
	t.Helper()
	// Taken FIRST so its cleanup is registered first and therefore runs
	// LAST: the engine server and the reconcile join below both have to be
	// finished with this directory before it is removed. Same reasoning
	// hostCutoffProviderAnswering states for its own state dir; here it
	// went unstated and the removal raced a reconcile still writing into
	// it (waired-agent#925).
	stateDir := t.TempDir()
	var servedMu sync.Mutex
	var served []string
	serveTags = func(tags ...string) {
		servedMu.Lock()
		defer servedMu.Unlock()
		served = tags
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		servedMu.Lock()
		entries := make([]map[string]string, 0, len(served))
		for _, tag := range served {
			entries = append(entries, map[string]string{"name": tag})
		}
		servedMu.Unlock()
		body, _ := json.Marshal(map[string]any{"models": entries})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)

	present := false
	sp = &fakeSpawner{}
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		// Empty on purpose: this is a daemon that booted with no engine.
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
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	p = &agentInferenceProvider{
		ollama: a,
		store:  catalog.NewStore(filepath.Join(stateDir, "state.json")),
		cfg:    agentconfig.InferenceConfig{AllowPull: true},
		// Both production constructors always set a profiler, and
		// reconcileEngineServe sizes the tuning from it unconditionally.
		// Leaving it nil here made this fixture the one shape that cannot
		// reconcile — which stayed invisible only while nothing on the
		// bootstrap path asked for one (waired-agent#320).
		profiler:     cpuSwapProfiler(t),
		logger:       slog.New(slog.DiscardHandler),
		agentCtx:     agentCtx,
		ollamaUsable: func() bool { return present },
	}
	// This fixture reaches endPull — bootstrapPulledTags drives a real
	// pull to completion — so it needs the same join hostCutoffProvider
	// takes. Without it the reconcile endPull fires goes on writing
	// state.json while the directory above is being removed, which is
	// waired-agent#925.
	joinEngineReconcile(t, p, cancelAgent)
	return p, sp, &present, serveTags
}

// THE #304 REGRESSION BAR. PRODUCT CONTRACT: an engine installed after
// the daemon booted is adopted in-process. Before this, the boot
// goroutine resolved the binary once, found none, returned, and nothing
// ever started the engine again — the wizard's model row went red and
// only a service restart recovered.
func TestRunEngineBootstrap_AdoptsAnEngineInstalledAfterBoot(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	ctx := context.Background()

	p.runEngineBootstrap(ctx, "boot")
	if got := sp.count(); got != 0 {
		t.Fatalf("spawns with no engine installed = %d, want 0", got)
	}
	if p.engineBootstrapOnce.Load() {
		t.Fatal("bootstrap latched without an engine; a later trigger could never retry")
	}

	// The executor installs the engine and reports done.
	*installed = true
	p.runEngineBootstrap(ctx, "setup: executor reported the engine install done")

	if got := sp.count(); got != 1 {
		t.Fatalf("spawns after the engine appeared = %d, want 1", got)
	}
	if st := p.ollama.Health(ctx).State; st != infruntime.StateReady {
		t.Fatalf("engine state = %s, want %s", st, infruntime.StateReady)
	}
	if !p.engineBootstrapOnce.Load() {
		t.Fatal("bootstrap did not latch after a successful start")
	}
}

// PRODUCT CONTRACT: the bootstrap tail runs at most once per process.
// The executor re-posts `engine_install: done` every 10 s and the
// reconciler re-applies on every network-map frame, so the tail is
// reachable many times over one setup. #305's registry now dedups a
// repeated pull of the same model, but the tail also runs the backend
// probe and the tuning verify, and both stop and restart the engine —
// which fails any download in flight (#305d).
func TestRunEngineBootstrap_TailRunsOnce(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true

	for range 5 {
		p.runEngineBootstrap(context.Background(), "repeat")
	}
	if got := sp.count(); got != 1 {
		t.Fatalf("spawns across 5 bootstrap runs = %d, want 1 (EnsureRunning is idempotent when ready)", got)
	}
}

// Records today's coalescing: a second caller arriving while a start is
// in flight returns immediately rather than stacking a second start. Both
// callers carry the same intent, so joining is complete — unlike
// requestEngineReconcile, whose swapPending exists to hand a NEW model to
// the running reconcile.
func TestRunEngineBootstrap_SecondCallerCoalesces(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true

	p.engineStartInFlight.Store(true)
	p.runEngineBootstrap(context.Background(), "second caller")
	if got := sp.count(); got != 0 {
		t.Fatalf("spawns while a start was already in flight = %d, want 0", got)
	}
}

// PRODUCT CONTRACT: the adopt trigger respects the two latches only an
// operator clears.
//
//   - parked is `waired inference engine stop` — an explicit "give me my
//     VRAM back". A setup executor's heartbeat must not revive it.
//   - the crash give-up latch (waired-agent#29) is the daemon having
//     stopped respawning a binary that keeps dying. Clearing it from a
//     repeating trigger is how the macOS respawn storm in #310 is built.
//
// The documented reset for both stays `waired inference engine start`.
func TestRunEngineBootstrap_RespectsParkAndGiveUp(t *testing.T) {
	t.Run("parked", func(t *testing.T) {
		p, sp, installed := bootstrapProvider(t)
		*installed = true
		ctx := context.Background()
		if err := p.ollama.Park(ctx); err != nil {
			t.Fatalf("Park: %v", err)
		}
		before := sp.count()

		p.runEngineBootstrap(ctx, "setup: engine binary appeared")

		if got := sp.count(); got != before {
			t.Fatalf("spawns against a parked engine = %d, want %d", got, before)
		}
		if !p.ollama.IsParked() {
			t.Fatal("the adopt trigger un-parked the engine; only an explicit start may")
		}
		if p.engineBootstrapOnce.Load() {
			t.Fatal("bootstrap latched on a refusal; an unpark would never bootstrap")
		}
	})

	t.Run("gave up", func(t *testing.T) {
		p, sp, installed := bootstrapProvider(t)
		*installed = true
		ctx := context.Background()
		p.ollama.LatchFailed("ollama: process exited during startup: signal: killed")
		before := sp.count()

		p.runEngineBootstrap(ctx, "setup: executor reported the engine install done")

		if got := sp.count(); got != before {
			t.Fatalf("spawns against a given-up engine = %d, want %d", got, before)
		}
		if err := p.ollama.EnsureRunning(ctx); !errors.Is(err, infruntime.ErrEngineUnrecoverable) {
			t.Fatalf("give-up latch after the trigger: EnsureRunning = %v, want ErrEngineUnrecoverable", err)
		}
	})
}

// THE #338 BAR. PRODUCT CONTRACT (issue #338): allow_pull=false stops
// DOWNLOADS. It does not stop the engine.
//
// This INVERTS TestRunEngineBootstrap_PullsDisabledStillGates, which
// recorded the opposite and said in its own comment that it was arguably
// wrong. The pre-#304 boot goroutine's `binary == "" || !cfg.AllowPull`
// survived the #304 rewrite verbatim, so a host holding its weights on
// disk with pulls turned off never started `ollama serve` at all — and
// nothing said so: hasUsableEngine reads the BINARY, not the process, so
// subsystemState reported a usable engine and fell through to
// awaiting_model rather than to anything an operator could act on. The
// switches that DO stop an engine are inference.enabled=false and
// `waired inference engine stop`.
//
// engineBootstrapOnce is asserted because the old gate returned BEFORE
// the latch: such a host re-ran the refusal on every setup trigger and
// the bootstrap tail never ran once.
func TestRunEngineBootstrap_PullsDisabledStillStartTheEngine(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true
	p.cfg = agentconfig.InferenceConfig{AllowPull: false}
	ctx := context.Background()

	p.runEngineBootstrap(ctx, "boot")

	if got := sp.count(); got != 1 {
		t.Fatalf("spawns with AllowPull=false = %d, want 1 — pulls being off must not "+
			"keep `ollama serve` down on a host whose weights are already there", got)
	}
	if st := p.ollama.Health(ctx).State; st != infruntime.StateReady {
		t.Fatalf("engine state = %s, want %s", st, infruntime.StateReady)
	}
	if !p.engineBootstrapOnce.Load() {
		t.Fatal("bootstrap did not latch; the tail would re-run on every later trigger")
	}
}

// THE #339 REGRESSION BAR. PRODUCT CONTRACT: a vLLM venv installed after
// the daemon booted is adopted in-process, the way #304 made an ollama
// binary be.
//
// Two boot-time snapshots stood between this host and a working engine,
// and #304 only removed the first. The binary was re-resolved live, but
// the ENGINE KIND was still whatever chooseEngine decided at
// construction — and engineViable("vllm") requires the venv to already
// exist, so a host that booted without one was pinned to ollama for the
// life of the process. The setup executor installs the venv (it really
// does: cmd/waired/setup_install.go), fires the same trigger #304 added,
// and every one of them reached only the ollama half. The wizard's engine
// row then read installed=true / ready=false until a service restart.
//
// engineChoice is the seam — the live re-run of the boot rule — because
// the real answer needs a CUDA host with a verified venv on disk. Below
// it, the subject really runs: bootstrapVLLM is called for real and
// declines on the absent venv, which is why no engine comes up here.
func TestRunEngineBootstrap_AdoptsAVLLMVenvInstalledAfterBoot(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	p.stateDir = t.TempDir()
	*installed = true
	ctx := context.Background()

	// The host as it booted: no venv, so the live rule says ollama and the
	// ollama arm runs exactly as before.
	venv := false
	p.engineChoice = func(context.Context) (string, bool) {
		if venv {
			return catalog.RuntimeVLLM, true
		}
		return catalog.RuntimeOllama, true
	}
	p.runEngineBootstrap(ctx, "boot")
	if got := sp.count(); got != 1 {
		t.Fatalf("ollama spawns at boot = %d, want 1", got)
	}
	if got := p.servingEngine(); got != catalog.RuntimeOllama {
		t.Fatalf("serving engine at boot = %q, want %q", got, catalog.RuntimeOllama)
	}

	// A live engine is never switched out from under itself, whatever the
	// venv does. That is the guard that keeps this from disturbing a host
	// that is already answering requests.
	venv = true
	p.runEngineBootstrap(ctx, "setup: executor reported the engine install done")
	if got := p.servingEngine(); got != catalog.RuntimeOllama {
		t.Fatalf("serving engine while ollama is up = %q, want %q (a serving engine is not swapped)",
			got, catalog.RuntimeOllama)
	}

	// The reported host: nothing ever came up, and the venv appears.
	p2, sp2, installed2 := bootstrapProvider(t)
	p2.stateDir = t.TempDir()
	*installed2 = false // no ollama binary either; the boot was inert
	p2.engineChoice = func(context.Context) (string, bool) { return catalog.RuntimeVLLM, true }

	p2.runEngineBootstrap(ctx, "setup: executor reported the engine install done")

	if got := p2.servingEngine(); got != catalog.RuntimeVLLM {
		t.Fatalf("serving engine after the venv appeared = %q, want %q — the venv is still unadopted",
			got, catalog.RuntimeVLLM)
	}
	if got := sp2.count(); got != 0 {
		t.Fatalf("ollama spawns on a vLLM host = %d, want 0", got)
	}
	if p2.engineBootstrapOnce.Load() {
		t.Fatal("the ollama bootstrap tail latched on a vLLM host")
	}
}

// The vLLM half of the #338 bar above: allow_pull=false stops downloads,
// not the engine, on this engine too. vLLM never had the gate —
// bootstrapVLLM refuses only the weights DOWNLOAD — so the two engines
// have to agree, and routing vLLM through a pull gate on the start path
// would take that away from a host whose safetensors are already on disk.
//
// This also pins the ORDER: the engine decision is taken before anything
// ollama-specific, so a vLLM host reaches its own arm rather than being
// measured against the other engine's preconditions.
func TestRunEngineBootstrap_PullsDisabledStillAdoptsVLLM(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	p.stateDir = t.TempDir()
	*installed = true
	p.cfg = agentconfig.InferenceConfig{AllowPull: false}
	p.engineChoice = func(context.Context) (string, bool) { return catalog.RuntimeVLLM, true }

	p.runEngineBootstrap(context.Background(), "boot")

	if got := p.servingEngine(); got != catalog.RuntimeVLLM {
		t.Fatalf("serving engine with allow_pull=false = %q, want %q "+
			"(allow_pull governs downloads, not whether vLLM may start)", got, catalog.RuntimeVLLM)
	}
	if got := sp.count(); got != 0 {
		t.Fatalf("ollama spawns = %d, want 0", got)
	}
}

// Adopting a different engine drops the previous engine's ActiveSelection,
// mirroring the boot-time engine switch in startInferenceSubsystem: the
// old engine's model is not something the new one can serve, and
// activateBundledIfUnset only fills an EMPTY slot — so leaving it recorded
// would leave the new engine with nothing to activate.
func TestRunEngineBootstrap_AdoptClearsThePreviousEnginesActive(t *testing.T) {
	p, _, installed := bootstrapProvider(t)
	p.stateDir = t.TempDir()
	*installed = false
	if err := p.store.Update(func(s *catalog.State) {
		s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeOllama, ModelID: "qwen3.6-35b-a3b"}
	}); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	p.engineChoice = func(context.Context) (string, bool) { return catalog.RuntimeVLLM, true }

	p.runEngineBootstrap(context.Background(), "setup: executor reported the engine install done")

	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Active != nil {
		t.Fatalf("active selection after adopting vLLM = %+v, want nil "+
			"(the ollama model is not servable by the new engine)", st.Active)
	}
}

// A provider with no engineChoice wired — every unit fixture that predates
// #339 — keeps its boot engine and reaches the ollama arm unchanged. The
// live rule being unavailable must not read as "no engine viable".
func TestRunEngineBootstrap_NoLiveChoiceKeepsTheBootEngine(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true
	p.engineChoice = nil

	p.runEngineBootstrap(context.Background(), "boot")

	if got := sp.count(); got != 1 {
		t.Fatalf("ollama spawns with no live engine rule = %d, want 1", got)
	}
	if got := p.servingEngine(); got != catalog.RuntimeOllama {
		t.Fatalf("serving engine = %q, want %q", got, catalog.RuntimeOllama)
	}
}

// TestRunEngineBootstrap_StandsDownWhileLocalInferenceIsOff is what
// makes the #465 latch removal safe to ship. The inference subsystem is
// now built on a host below the recommended spec — that is the point,
// since the management routes, the onboarding capability and the tray's
// menu group all hang off it — so "off" has to mean something at the
// engine instead. Without this a machine that was told not to serve
// would still start ollama and download weights.
//
// The second half is the half that is easy to get wrong: the bootstrap
// must not LATCH while off, or turning local inference on would find
// engineBootstrapOnce already set and never run the tail.
func TestRunEngineBootstrap_StandsDownWhileLocalInferenceIsOff(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true
	off := true
	p.isInferenceDisabled = func() bool { return off }

	p.runEngineBootstrap(context.Background(), "boot")
	if got := sp.count(); got != 0 {
		t.Fatalf("spawns while local inference is off = %d, want 0", got)
	}
	if p.engineBootstrapOnce.Load() {
		t.Fatal("bootstrap latched while off; the opt-in would never run the tail")
	}

	off = false
	p.runEngineBootstrap(context.Background(), "inference enabled")
	if got := sp.count(); got == 0 {
		t.Fatal("turning local inference on started no engine; the opt-in does nothing")
	}
}
