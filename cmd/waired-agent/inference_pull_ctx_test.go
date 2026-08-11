package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// blockingRunner holds every pull open until release is closed, and
// records the tag it was asked for and the ctx error it observed on the
// way out. Recording both is what makes the #305 cases writable: a fake
// that dropped either could not tell "the pull finished" from "the pull
// was cancelled", nor one variant from another.
type blockingRunner struct {
	started chan string
	release chan struct{}

	mu      sync.Mutex
	tags    []string
	ctxErrs []error
}

func newBlockingRunner(t *testing.T) *blockingRunner {
	t.Helper()
	r := &blockingRunner{
		started: make(chan string, 8),
		release: make(chan struct{}),
	}
	// Always release, so a failing assertion cannot leave the pull
	// goroutine writing into t.TempDir() during cleanup (#377).
	t.Cleanup(r.releaseAll)
	return r
}

func (r *blockingRunner) releaseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

func (r *blockingRunner) Run(ctx context.Context, _ string, args, _ []string, onLine func(string)) error {
	tag := ""
	if len(args) > 1 {
		tag = args[1]
	}
	// Only `pull` blocks. The same puller also runs `ollama rm` (#671), and
	// a fake that ignored the subcommand held the removal open too — so a
	// test that deleted a model mid-pull deadlocked instead of failing,
	// which is the fake making a real case unwritable (CLAUDE.md §Test
	// discipline).
	if len(args) > 0 && args[0] != "pull" {
		r.mu.Lock()
		r.tags = append(r.tags, tag)
		r.mu.Unlock()
		return nil
	}
	r.mu.Lock()
	r.tags = append(r.tags, tag)
	r.mu.Unlock()
	select {
	case r.started <- tag:
	default:
	}

	var err error
	select {
	case <-r.release:
		onLine("success")
	case <-ctx.Done():
		err = ctx.Err()
	}
	r.mu.Lock()
	r.ctxErrs = append(r.ctxErrs, err)
	r.mu.Unlock()
	return err
}

func (r *blockingRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tags)
}

func (r *blockingRunner) pulledTags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags...)
}

func (r *blockingRunner) firstCtxErr(t *testing.T) error {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ctxErrs) == 0 {
		t.Fatal("no pull ran")
	}
	return r.ctxErrs[0]
}

// awaitStarted blocks until a pull has actually entered the runner, so a
// test never races the dispatch goroutine.
func (r *blockingRunner) awaitStarted(t *testing.T) string {
	t.Helper()
	select {
	case tag := <-r.started:
		return tag
	case <-time.After(5 * time.Second):
		t.Fatal("no pull started")
		return ""
	}
}

func modelStateOf(t *testing.T, p *agentInferenceProvider, modelID string) catalog.ModelState {
	t.Helper()
	st, err := p.store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	return st.Models[modelID]
}

// PRODUCT CONTRACT (#305a): the download outlives whoever dispatched it.
// handleModelsPull hands PullModel the HTTP request context, which
// net/http cancels the microsecond the 202 is written — so every
// `waired models pull` died within milliseconds, was recorded failed with
// "context canceled", and nothing retried.
func TestPullModel_SurvivesACancelledDispatchContext(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	p.agentCtx = context.Background()

	dispatchCtx, cancel := context.WithCancel(context.Background())
	if _, err := p.PullModel(dispatchCtx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)
	cancel() // the "handler returned" moment

	r.releaseAll()
	p.waitForPulls()

	if err := r.firstCtxErr(t); err != nil {
		t.Fatalf("pull observed ctx error %v; it must not be bound to the dispatcher's context", err)
	}
	if got := modelStateOf(t, p, "dense-mtp").State; got != catalog.ModelStateReady {
		t.Fatalf("model state = %q, want %q", got, catalog.ModelStateReady)
	}
}

// Records today's behaviour: the several unit-test providers that build an
// agentInferenceProvider without an agentCtx must keep working.
func TestPullModel_FallsBackToBackgroundWhenAgentCtxIsNil(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	if p.agentCtx != nil {
		t.Fatal("fixture unexpectedly has an agentCtx; this test covers the nil case")
	}

	dispatchCtx, cancel := context.WithCancel(context.Background())
	if _, err := p.PullModel(dispatchCtx, "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)
	cancel()

	r.releaseAll()
	p.waitForPulls()

	if err := r.firstCtxErr(t); err != nil {
		t.Fatalf("pull observed ctx error %v, want nil", err)
	}
}

// PRODUCT CONTRACT: shutdown still cancels. Detaching the pull from the
// request context must not detach it from the daemon — a SIGTERM has to
// stop a multi-GB download rather than orphan it.
func TestPullModel_CancelledByAgentShutdown(t *testing.T) {
	r := newBlockingRunner(t)
	p := pullGateProviderWithRunner(t, pullGateManifest(false), r)
	agentCtx, shutdown := context.WithCancel(context.Background())
	p.agentCtx = agentCtx

	if _, err := p.PullModel(context.Background(), "dense-mtp"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	r.awaitStarted(t)

	shutdown()
	p.waitForPulls()

	if err := r.firstCtxErr(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("pull ctx error on shutdown = %v, want context.Canceled", err)
	}
	if got := modelStateOf(t, p, "dense-mtp").State; got != catalog.ModelStateFailed {
		t.Fatalf("model state after a cancelled pull = %q, want %q", got, catalog.ModelStateFailed)
	}
}
