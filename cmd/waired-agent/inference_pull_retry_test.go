package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
)

// scriptedRunner answers each successive `ollama pull` from a script of
// results, the last entry repeating. It records the tag it was asked for
// and — through the observe seam — the model's stored state as each
// attempt STARTS, which is the only way to tell a retry that left the row
// `downloading` from one that flashed it red in between.
//
// It deliberately does not return ctx.Err(): a pull killed through its
// context comes back from DefaultRunner as `cmd.Wait()`'s *exec.ExitError
// ("signal: killed"), indistinguishable from an OOM kill, so a fake that
// hands back context.Canceled would let a wrong implementation pass.
type scriptedRunner struct {
	mu      sync.Mutex
	results []error
	tags    []string
	states  []string
	observe func() string
}

func (r *scriptedRunner) Run(_ context.Context, _ string, args, _ []string, onLine func(string)) error {
	tag := ""
	if len(args) > 1 {
		tag = args[1]
	}
	observed := ""
	if r.observe != nil {
		observed = r.observe()
	}

	r.mu.Lock()
	r.tags = append(r.tags, tag)
	r.states = append(r.states, observed)
	err := errors.New("scriptedRunner: empty script")
	if n := len(r.results); n > 0 {
		i := min(len(r.tags)-1, n-1)
		err = r.results[i]
	}
	r.mu.Unlock()

	if err == nil {
		onLine("success")
	}
	return err
}

func (r *scriptedRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tags)
}

func (r *scriptedRunner) observedStates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.states...)
}

// shrinkPullRetry keeps the retry pacing out of the test's wall clock,
// the way setBenchTiming does for the CLI waits.
func shrinkPullRetry(t *testing.T) {
	t.Helper()
	prev := modelPullBackoff
	modelPullBackoff = time.Millisecond
	t.Cleanup(func() { modelPullBackoff = prev })
}

func retryProvider(t *testing.T, r download.CommandRunner) *agentInferenceProvider {
	t.Helper()
	// pullGateProviderWithRunner already shrinks modelPullBackoff.
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.manifests = bounceTestManifests()
	p.agentCtx = context.Background()
	return p
}

// PRODUCT CONTRACT: a model download that fails for a passing reason is
// retried inside the job that owns it.
//
// Nothing in the tree retried a failed pull — every dispatcher is
// one-shot, with no ticker, backoff or re-entry timer, and the setup
// reconciler is stricter still (Apply marks the desired model applied
// BEFORE calling setupApplyModel, cleared only by an engine false→true
// transition, which never fires on a host whose engine was already
// installed at boot). That was survivable only because the bundled
// pre-pull happened to run alongside; #306 makes the operator's model the
// only one downloading, so a single transient failure would otherwise
// leave the host with nothing until a restart.
func TestRunPullJob_RetriesATransientFailure(t *testing.T) {
	r := &scriptedRunner{results: []error{errors.New("registry throttled the request"), nil}}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != 2 {
		t.Fatalf("pull attempts = %d, want 2 (one failure, then the retry that succeeds)", got)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateReady {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateReady)
	}
}

// PRODUCT CONTRACT: the retry does not repaint the row red between
// attempts. A model that is being retried is still downloading, and the
// wizard renders `failed` straight to ERR (#307).
func TestRunPullJob_IntermediateFailureLeavesTheModelDownloading(t *testing.T) {
	r := &scriptedRunner{results: []error{errors.New("connection reset by peer"), nil}}
	p := retryProvider(t, r)
	r.observe = func() string { return modelStateOf(t, p, "model-a").State }

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	states := r.observedStates()
	if len(states) != 2 {
		t.Fatalf("observed states = %v, want one per attempt", states)
	}
	if states[1] != catalog.ModelStateDownloading {
		t.Fatalf("state at the retry = %q, want %q — a retry in progress must not "+
			"be reported as a failure", states[1], catalog.ModelStateDownloading)
	}
}

// PRODUCT CONTRACT: a full disk is not retried. It cannot get better on
// its own, and three multi-GB attempts at it only delay the honest error
// the wizard needs to show.
func TestRunPullJob_DoesNotRetryADiskFullFailure(t *testing.T) {
	r := &scriptedRunner{results: []error{errors.New("write /var/lib/waired: no space left on device")}}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pull attempts on a full disk = %d, want 1", got)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateFailed)
	}
}

// TestRunPullJob_DoesNotRetryADiskFullFailureReportedOnStderr is the
// version of the test above that a REAL pull can produce, and it is the
// reason the one above stayed green while the field behaviour was three
// multi-GB retries onto a full disk.
//
// scriptedRunner puts the ENOSPC marker in the RETURNED error. `ollama
// pull` never does that: it prints the diagnosis to stderr and exits with
// a bare status, so the short-circuit's isDiskFullText("exit status 1")
// was dead code for every real download (#307).
//
// PRODUCT CONTRACT.
func TestRunPullJob_DoesNotRetryADiskFullFailureReportedOnStderr(t *testing.T) {
	r := &diagRunner{
		lines:   [][]string{{"Error: write /var/lib/waired/blobs/sha256-abc: no space left on device"}},
		results: []error{exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.attempts(); got != 1 {
		t.Fatalf("pull attempts on a full disk = %d, want 1", got)
	}
	ms := modelStateOf(t, p, "model-a")
	if ms.State != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q", ms.State, catalog.ModelStateFailed)
	}
}

// PRODUCT CONTRACT: shutdown ends the job, it does not restart it.
// agentCtx is cancelled on SIGTERM, and retrying there would hold the
// daemon open re-attempting a download nobody is waiting for. Keeps
// TestPullModel_CancelledByAgentShutdown's outcome unchanged.
func TestRunPullJob_ShutdownDoesNotRetry(t *testing.T) {
	r := &scriptedRunner{results: []error{errors.New("signal: killed")}}
	p := retryProvider(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	p.agentCtx = ctx
	cancel()

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != 1 {
		t.Fatalf("pull attempts after shutdown = %d, want 1", got)
	}
	if got := modelStateOf(t, p, "model-a").State; got != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateFailed)
	}
}

// PRODUCT CONTRACT: the retry is bounded. A permanently broken download
// must reach `failed` so the wizard and `waired models pull --wait` get
// their answer instead of spinning.
func TestRunPullJob_GivesUpAfterTheAttemptBudget(t *testing.T) {
	r := &scriptedRunner{results: []error{errors.New("registry throttled the request")}}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.calls(); got != modelPullAttempts {
		t.Fatalf("pull attempts = %d, want %d", got, modelPullAttempts)
	}
	ms := modelStateOf(t, p, "model-a")
	if ms.State != catalog.ModelStateFailed {
		t.Fatalf("model state = %q, want %q", ms.State, catalog.ModelStateFailed)
	}
	if ms.Error == "" {
		t.Fatal("no error recorded on the exhausted model row")
	}
}
