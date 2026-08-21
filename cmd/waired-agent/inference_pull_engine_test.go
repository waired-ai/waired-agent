package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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
	return pullEngineProviderReporting(t, runner, "")
}

// pullEngineProviderReporting is pullEngineProviderWithRunner whose fake
// engine answers GET /api/version with engineVersion, so a test can put
// the version out of reach at dispatch and within reach after the join.
// "" keeps the historical fixture — a server that reports no version at
// all — which is what leaves the unknown-version path reachable for the
// tests that pin it.
func pullEngineProviderReporting(
	t *testing.T, runner download.CommandRunner, engineVersion string,
) (*agentInferenceProvider, *fakeSpawner) {
	t.Helper()
	shrinkPullRetry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/api/version" && engineVersion != "" {
			_, _ = fmt.Fprintf(w, `{"version":%q}`, engineVersion)
			return
		}
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
	// The engine's own reason survives — a stopped engine is a different
	// fix from a crashed one, and only this text distinguishes them.
	//
	// The wording moved with waired-agent#881: the sentinel used to open
	// with "ollama:", which put an internal engine name on a surface the
	// setup wizard renders (docs-site/TRANSLATION.md forbids that), and it
	// could not be reused for the vLLM engine, whose power axis this
	// release adds. What is asserted is unchanged: that the CAUSE reaches
	// the recorded failure instead of being flattened into the pull's own
	// error.
	if !strings.Contains(got, infruntime.ErrEngineParked.Error()) {
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
	// The subject is the ATTRIBUTION, not the code: a healthy engine must
	// not be blamed. waired-agent#328 changed what an unrecognised text
	// classifies as (network_error was the catch-all, internal is the
	// honest answer), so assert the thing this test is about — anything
	// but engine_not_ready.
	if got := classifyModelPullFailure(ms.Error); got == signer.SetupErrorEngineNotReady {
		t.Errorf("classifyModelPullFailure(%q) = %q, want the engine NOT blamed", ms.Error, got)
	}
}

// --- the variant is chosen against a version the engine can report (#361) ---

// tagRecordingRunner succeeds like noopRunner but remembers which tag it
// was asked to fetch. The state row alone cannot tell "downloaded the
// right weights" from "downloaded the wrong ones and relabelled them".
type tagRecordingRunner struct {
	mu   sync.Mutex
	tags []string
}

func (r *tagRecordingRunner) Run(_ context.Context, _ string, args, _ []string, onLine func(string)) error {
	tag := ""
	if len(args) > 1 {
		tag = args[1]
	}
	r.mu.Lock()
	r.tags = append(r.tags, tag)
	r.mu.Unlock()
	onLine("success")
	return nil
}

func (r *tagRecordingRunner) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags...)
}

// PRODUCT CONTRACT (#361): a pull downloads the best variant the engine
// can actually load — including when nothing could say what the engine
// was at the moment the pull was dispatched.
//
// The version gate fails closed, so an unknown version excludes every
// variant carrying a MinEngineVersion floor. On a fresh install the
// version IS unknown for a window (the adapter has never been ready and
// the profiler's snapshot predates the engine install), so the pull
// dropped to the unfloored variant — and #305's dedup then made that
// first, lower choice win any later dispatch. #304's EnsureRunning join
// is what makes this recoverable: by the time bytes move, the engine is
// serving and its version is knowable.
func TestRunPullJob_UpgradesTheVariantOnceTheEngineReportsItsVersion(t *testing.T) {
	r := &tagRecordingRunner{}
	p, _ := pullEngineProviderReporting(t, r, "0.31.1")
	ctx := context.Background()

	// The premise: at dispatch time nothing on this provider can name the
	// engine version. Without this the test would pass on the pre-#361
	// code for the wrong reason.
	if v := p.ollamaEngineVersion(ctx); v != "" {
		t.Fatalf("engine version before the engine ever started = %q, want unknown", v)
	}

	if _, err := p.PullModel(ctx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.recorded(); len(got) != 1 || got[0] != "dense:mtp-q4" {
		t.Fatalf("tags fetched = %v, want exactly [dense:mtp-q4] — the engine reports 0.31.1, "+
			"which clears the mtp variant's 0.30.0 floor", got)
	}
	ms := modelStateOf(t, p, "dense-mtp")
	if ms.VariantID != "mtp-q4" || ms.OllamaTag != "dense:mtp-q4" {
		t.Errorf("recorded variant/tag = %q/%q, want mtp-q4/dense:mtp-q4 — this row is what "+
			"the gateway puts on the wire and the mesh advertises", ms.VariantID, ms.OllamaTag)
	}
}

// The other half of the same contract: the upgrade is driven by what the
// engine reports, not by optimism. An engine that answers with a version
// BELOW the floor keeps the unfloored variant — the qwen3.6 mtp incident
// is what the floor exists for.
func TestRunPullJob_KeepsTheUnflooredVariantWhenTheEngineIsBelowTheFloor(t *testing.T) {
	r := &tagRecordingRunner{}
	p, _ := pullEngineProviderReporting(t, r, "0.24.0")

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.recorded(); len(got) != 1 || got[0] != "dense:q4" {
		t.Fatalf("tags fetched = %v, want exactly [dense:q4] — 0.24.0 is below the mtp floor", got)
	}
}

// And the case that must stay untouched: an engine that reports nothing
// leaves the blind choice in place. "Unknown" is not evidence that the
// floored variant would load.
//
// Records today's behaviour — the fixture's server answers no version at
// all, which is what the historical helper already provided.
func TestRunPullJob_AnEngineThatReportsNoVersionKeepsTheBlindChoice(t *testing.T) {
	r := &tagRecordingRunner{}
	p, _ := pullEngineProviderReporting(t, r, "")

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.recorded(); len(got) != 1 || got[0] != "dense:q4" {
		t.Fatalf("tags fetched = %v, want exactly [dense:q4]", got)
	}
}
