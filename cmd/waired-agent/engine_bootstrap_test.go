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
	p = &agentInferenceProvider{
		ollama:       a,
		store:        catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:          agentconfig.InferenceConfig{AllowPull: true},
		logger:       slog.New(slog.DiscardHandler),
		agentCtx:     context.Background(),
		ollamaUsable: func() bool { return present },
	}
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

// Records today's behaviour: pulls disabled by config still gate the
// whole engine-start sequence, exactly as the pre-#304 boot goroutine's
// `binary == "" || !cfg.AllowPull` did. (That it gates engine START and
// not just pulls is arguably wrong, but changing it is not this fix.)
func TestRunEngineBootstrap_PullsDisabledStillGates(t *testing.T) {
	p, sp, installed := bootstrapProvider(t)
	*installed = true
	p.cfg = agentconfig.InferenceConfig{AllowPull: false}

	p.runEngineBootstrap(context.Background(), "boot")
	if got := sp.count(); got != 0 {
		t.Fatalf("spawns with AllowPull=false = %d, want 0", got)
	}
}
