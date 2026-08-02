package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// pullEngineProvider is pullGateProvider with a real, initially-stopped
// OllamaAdapter attached, so a test can observe whether dispatching a
// pull brings the serving engine up.
func pullEngineProvider(t *testing.T) (*agentInferenceProvider, *fakeSpawner) {
	t.Helper()
	return pullEngineProviderWithRunner(t, noopRunner{})
}

// pullEngineProviderWithRunner is pullEngineProvider with the pull's
// outcome scriptable, for the tests that need the DOWNLOAD to fail while
// the engine is in a known state.
//
// It shrinks the retry backoff, which pullEngineProvider never needed:
// every test using it pulls successfully on the first attempt, so nobody
// noticed that a failing runner here costs 45 s of wall clock.
func pullEngineProviderWithRunner(t *testing.T, runner download.CommandRunner) (*agentInferenceProvider, *fakeSpawner) {
	t.Helper()
	shrinkPullRetry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	sp := &fakeSpawner{}
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: sp, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	p := &agentInferenceProvider{
		ollama:     a,
		store:      catalog.NewStore(filepath.Join(t.TempDir(), "state.json")),
		cfg:        agentconfig.InferenceConfig{AllowPull: true},
		manifests:  []catalog.Manifest{pullGateManifest(false)},
		puller:     download.NewPuller("ollama-fake", runner),
		dlProgress: newDownloadProgress(),
		logger:     slog.New(slog.DiscardHandler),
		agentCtx:   context.Background(),
	}
	return p, sp
}

// PRODUCT CONTRACT (#304): `ollama pull` is a CLIENT of `ollama serve`, so
// a pull job brings the engine up before shelling out. Setup admission
// keys off a stat of the engine binary, which flips true seconds before
// the server is listening; the pull used to die on connection-refused,
// the model was recorded failed, and admission is once-per-desired-value
// so nothing retried.
func TestRunPullJob_JoinsEngineStartBeforePulling(t *testing.T) {
	p, sp := pullEngineProvider(t)
	ctx := context.Background()

	if got := sp.count(); got != 0 {
		t.Fatalf("spawns before the pull = %d, want 0", got)
	}
	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := sp.count(); got != 1 {
		t.Fatalf("spawns during the pull = %d, want 1 (the pull must bring the engine up)", got)
	}
	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if got := st.Models["dense-mtp"].State; got != catalog.ModelStateReady {
		t.Fatalf("model state after the pull = %q, want %q", got, catalog.ModelStateReady)
	}
}

// PRODUCT CONTRACT: joining the engine start must not become a back door
// around `waired inference engine stop`. A parked engine returns its
// sentinel without spawning; the pull proceeds and reports whatever the
// real outcome is, exactly as before this change.
func TestRunPullJob_ParkedEngineIsNotRevivedByAPull(t *testing.T) {
	p, sp := pullEngineProvider(t)
	ctx := context.Background()
	if err := p.ollama.Park(ctx); err != nil {
		t.Fatalf("Park: %v", err)
	}
	before := sp.count()

	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := sp.count(); got != before {
		t.Fatalf("spawns with a parked engine = %d, want %d", got, before)
	}
	if !p.ollama.IsParked() {
		t.Fatal("a pull un-parked the engine; only an explicit start may")
	}
}

// THE #305/R0 REGRESSION BAR. PRODUCT CONTRACT: a pull that starts the
// engine must not take it down again when the download finishes.
//
// #304 added an EnsureRunning join to runPullJob so a pull dispatched the
// moment the engine binary appears waits for the server. But runPullJob
// wrapped its work in a self-cancelling context and passed THAT to
// EnsureRunning. When the pull wins the single-flight leader race — which
// is exactly the fresh-install case #304 targets — the ctx reaches
// ensureRunningLeader -> Spawner.Spawn -> exec.CommandContext, so the
// `defer cancel()` killed `ollama serve` on completion. The engine then
// self-healed via crash recovery, burning one of three strikes toward the
// give-up latch.
//
// It escaped review because fakeSpawner discarded the ctx; it now honours
// it, the way DefaultSpawner does.
func TestRunPullJob_DoesNotKillTheEngineItStarted(t *testing.T) {
	p, sp := pullEngineProvider(t)
	ctx := context.Background()

	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := sp.count(); got != 1 {
		t.Fatalf("spawns during the pull = %d, want 1 (the pull must bring the engine up)", got)
	}
	// Assert on the context the child was spawned with rather than waiting
	// for the child to notice a cancellation: the former is deterministic,
	// the latter is a race against the kill goroutine.
	spawnCtx := sp.lastCtx()
	if spawnCtx == nil {
		t.Fatal("no child was spawned")
	}
	if err := spawnCtx.Err(); err != nil {
		t.Fatalf("the engine was spawned on a context that is already cancelled (%v); "+
			"a finished pull must not take the engine down with it", err)
	}
	if proc := sp.lastProc(); proc != nil {
		select {
		case <-proc.Done():
			t.Fatal("the engine child died when the pull finished")
		default:
		}
	}
	if st := p.ollama.Health(ctx).State; st != infruntime.StateReady {
		t.Fatalf("engine state after the pull = %s, want %s", st, infruntime.StateReady)
	}
}

// --- the engine as a named cause of a failed download (#307) ---

// PRODUCT CONTRACT: when the engine could not be brought up for the
// attempt that failed, the recorded failure NAMES the engine.
//
// Before this, EnsureRunning's error was logged and dropped, the pull
// went ahead against a server that was not there, and all that survived
// was "exit status 1" — which classifySetupFailure could only read as
// network_error. That is how "Check its internet connection" ended up on
// the wizard while the real problem was an engine that never started.
func TestRunPullJob_AParkedEngineIsNamedAsTheCauseOfAFailedPull(t *testing.T) {
	p, _ := pullEngineProviderWithRunner(t, failingRunner{})
	ctx := context.Background()
	if err := p.ollama.Park(ctx); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "dense-mtp").Error
	if !strings.Contains(got, engineNotRunningMarker) {
		t.Errorf("recorded error = %q, want it to name the engine", got)
	}
	// The engine's own reason survives — "parked" is a different fix from
	// "crashed", and only this text distinguishes them.
	if !strings.Contains(got, "engine parked") {
		t.Errorf("recorded error = %q, want EnsureRunning's own reason kept", got)
	}
	// And so does the pull's error: if the attribution is ever wrong, the
	// operator has still lost nothing.
	if !strings.Contains(got, "simulated registry throttle") {
		t.Errorf("recorded error = %q, want the pull's own error kept", got)
	}
}

// TestRunPullJob_EngineFailureClassifiesAsEngineNotReady is the join:
// real provider -> real recorded text -> real classifier. It is the only
// test that walks the whole #307 chain, and the only thing that catches
// the producer's marker drifting from the consumer's.
//
// PRODUCT CONTRACT.
func TestRunPullJob_EngineFailureClassifiesAsEngineNotReady(t *testing.T) {
	p, _ := pullEngineProviderWithRunner(t, failingRunner{})
	ctx := context.Background()
	if err := p.ollama.Park(ctx); err != nil {
		t.Fatalf("Park: %v", err)
	}

	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	ms := modelStateOf(t, p, "dense-mtp")
	if got := classifyModelPullFailure(ms.Error); got != signer.SetupErrorEngineNotReady {
		t.Fatalf("classifyModelPullFailure(%q) = %q, want %q", ms.Error, got, signer.SetupErrorEngineNotReady)
	}
}

// TestRunPullJob_AHealthyEngineIsNotBlamedForAFailedPull is the negative
// control, and it is the only thing standing between this change and
// blaming the engine for every failed download. A running engine plus a
// failing pull is a network problem, and must still say so.
//
// PRODUCT CONTRACT.
func TestRunPullJob_AHealthyEngineIsNotBlamedForAFailedPull(t *testing.T) {
	p, sp := pullEngineProviderWithRunner(t, failingRunner{})

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := sp.count(); got == 0 {
		t.Fatal("the engine never started; this fixture cannot tell a healthy engine from a missing one")
	}
	ms := modelStateOf(t, p, "dense-mtp")
	if strings.Contains(ms.Error, engineNotRunningMarker) {
		t.Errorf("recorded error = %q, want the engine NOT blamed — it was running", ms.Error)
	}
	if got := classifyModelPullFailure(ms.Error); got != signer.SetupErrorNetworkError {
		t.Errorf("classifyModelPullFailure(%q) = %q, want %q", ms.Error, got, signer.SetupErrorNetworkError)
	}
}
