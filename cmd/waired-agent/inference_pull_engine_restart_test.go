package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// restartRunner answers each `ollama pull` from a per-attempt function and,
// before answering, runs an optional hook. The hook is the seam these tests
// need and the other pull fixtures do not have: it restarts the engine
// UNDERNEATH the download, which is what all four of #359's paths do to it.
//
// Like scriptedRunner it never returns context.Canceled — a pull killed
// through its engine comes back from DefaultRunner as cmd.Wait()'s
// *exec.ExitError ("signal: killed"), carrying nothing that says why.
type restartRunner struct {
	mu     sync.Mutex
	calls  int
	states []string

	onAttempt func(n int)       // runs at the start of attempt n (1-based)
	result    func(n int) error // what attempt n returns
	observe   func() string     // the model row as each attempt starts
}

func (r *restartRunner) Run(_ context.Context, _ string, _, _ []string, onLine func(string)) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()

	if r.observe != nil {
		observed := r.observe()
		r.mu.Lock()
		r.states = append(r.states, observed)
		r.mu.Unlock()
	}
	if r.onAttempt != nil {
		r.onAttempt(n)
	}
	err := r.result(n)
	if err == nil {
		onLine("success")
	}
	return err
}

func (r *restartRunner) attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *restartRunner) observedStates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.states...)
}

// restartProvider builds a provider around a REAL OllamaAdapter, because
// the subject is the adapter's process generation: a fake could only
// restate the implementation. The engine is already installed and serving
// — this is a running host, not a boot.
func restartProvider(t *testing.T, r download.CommandRunner) *agentInferenceProvider {
	t.Helper()
	shrinkPullRetry(t)
	p, _, installed, _ := bootstrapProviderServingTags(t)
	*installed = true
	p.manifests = bounceTestManifests()
	p.puller = newTestPuller(r)
	if err := p.ollama.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("precondition: EnsureRunning: %v", err)
	}
	return p
}

// throttled is the class of failure the attempt budget exists for: a
// transient the download itself is answerable for.
func throttled() error { return errors.New("registry throttled the request") }

// THE #359 REGRESSION BAR. PRODUCT CONTRACT: an attempt the engine was
// restarted out from under is not charged to the download.
//
// `ollama pull` is a client of `ollama serve`. Four paths stop that server
// while a download runs — a control-plane capacity retune, crash recovery,
// an operator's model switch whose weights were already on disk, and the
// boot tail's backend probe / tuning verify — and each one made the pull's
// child exit non-zero. The job spent one of its three attempts on it, so a
// download that met a single genuine transient afterwards reached `failed`
// having only really tried twice, and the wizard's model row went red for
// a reason that had nothing to do with the model.
func TestRunPullJob_AnEngineRestartDoesNotChargeTheAttemptBudget(t *testing.T) {
	ctx := context.Background()
	var p *agentInferenceProvider
	r := &restartRunner{
		onAttempt: func(n int) {
			if n == 1 {
				// Exactly what a reconcile bounce does to it.
				_ = p.ollama.Stop(ctx)
			}
		},
		result: func(int) error { return throttled() },
	}
	p = restartProvider(t, r)

	if _, err := p.PullModel(ctx, "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got, want := r.attempts(), modelPullAttempts+1; got != want {
		t.Fatalf("pull attempts = %d, want %d — the restarted attempt must be free, "+
			"leaving the full %d-attempt budget for the download's own failures",
			got, want, modelPullAttempts)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q — the budget is still bounded", got, catalog.ModelStateFailed)
	}
}

// The other side of the bar above, on the same fixture: with the engine
// left alone, the identical script spends the identical budget. Without
// it, "4 attempts" says nothing — a grace handed out unconditionally would
// pass the test above and quietly turn every download into a 5-attempt
// one.
func TestRunPullJob_ChargesTheBudgetWhenTheEngineDidNotMove(t *testing.T) {
	r := &restartRunner{result: func(int) error { return throttled() }}
	p := restartProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.attempts(); got != modelPullAttempts {
		t.Fatalf("pull attempts with a steady engine = %d, want %d", got, modelPullAttempts)
	}
}

// PRODUCT CONTRACT: the grace is bounded. An engine that restarts on every
// attempt — a crash loop, a reconcile that cannot settle — must still reach
// an honest verdict rather than retry forever, which is the same reason
// modelPullAttempts is finite.
func TestRunPullJob_TheEngineRestartGraceIsBounded(t *testing.T) {
	ctx := context.Background()
	var p *agentInferenceProvider
	r := &restartRunner{
		onAttempt: func(int) { _ = p.ollama.Stop(ctx) },
		result:    func(int) error { return throttled() },
	}
	p = restartProvider(t, r)

	if _, err := p.PullModel(ctx, "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got, want := r.attempts(), modelPullAttempts+enginePullBounceGrace; got != want {
		t.Fatalf("pull attempts against an engine that never settles = %d, want %d", got, want)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateFailed)
	}
}

// #307's contract, re-asserted over the new code path: a download waiting
// out an engine restart is still downloading, and the row must not be
// repainted red on the way through — the wizard renders `failed` straight
// to ERR, so flashing it red mid-retry would show the user exactly the
// error #359 exists to stop showing them.
//
// Not a bar on the grace itself (the two tests above are): removing the
// grace leaves this green, because the ordinary charged retry reaches the
// same end state. It is here to pin that the free retry did not introduce
// a state write the charged one never made.
func TestRunPullJob_ARestartedDownloadStaysDownloadingAndCompletes(t *testing.T) {
	ctx := context.Background()
	var p *agentInferenceProvider
	r := &restartRunner{
		onAttempt: func(n int) {
			if n == 1 {
				_ = p.ollama.Stop(ctx)
			}
		},
		result: func(n int) error {
			if n == 1 {
				return throttled()
			}
			return nil
		},
	}
	p = restartProvider(t, r)
	r.observe = func() string { return modelStateOf(t, p, "model-a").State }

	if _, err := p.PullModel(ctx, "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.attempts(); got != 2 {
		t.Fatalf("pull attempts = %d, want 2 (the restarted one, then the retry)", got)
	}
	states := r.observedStates()
	if len(states) != 2 {
		t.Fatalf("observed states = %v, want one per attempt", states)
	}
	if states[1] != catalog.ModelStateDownloading {
		t.Fatalf("state at the retry = %q, want %q", states[1], catalog.ModelStateDownloading)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateReady {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateReady)
	}
}
