package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The engine is a host-level resource, not a request's (waired-agent#947).
//
// The gateway calls EnsureRunning with the HTTP request's context
// (internal/gateway/openai.go, anthropic.go) and a request can win the
// single-flight gate, so before the start was detached, an inference request
// that happened to bring the engine up also owned it: DefaultSpawner handed
// that context to exec.CommandContext, whose cancel is a single-pid
// Process.Kill() — the engine's leader died and its model-runner / worker
// children were orphaned still holding VRAM.
//
// PRODUCT CONTRACT (waired-ai/waired-agent#947): a caller's cancellation
// bounds only how long THAT CALLER waits.

func TestOllamaEnsureRunning_CallerCancellationDoesNotOwnTheEngine(t *testing.T) {
	// /api/tags stays silent until the test releases it, so the readiness
	// wait is still in flight when the caller walks away.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	defer close(release)
	host, port := splitHostPort(t, srv.URL)

	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 200,
		StopTimeout: 50 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := a.EnsureRunning(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureRunning = %v, want context.Canceled (the caller's own deadline)", err)
	}

	// The caller is gone. The engine must not be.
	if n := spawner.spawnCount(); n != 1 {
		t.Fatalf("spawner calls = %d, want 1", n)
	}
	// Not "the context is not cancelled YET" — that would pass on a
	// cancellable context the caller simply had not cancelled at this
	// instant. The property is that the child CANNOT be killed by any
	// context: Done() is nil only on one that can never be cancelled.
	got := spawner.lastCtx()
	if got == nil {
		t.Fatal("nothing was spawned")
	}
	if got.Done() != nil {
		t.Fatalf("the child was spawned on a cancellable context (err=%v): "+
			"a request that starts the engine must not own it", got.Err())
	}
	proc := spawner.lastProcess()
	if proc.hasExited() || proc.wasKilled() {
		t.Fatal("the engine was torn down when its caller walked away")
	}
	if st := a.Health(context.Background()).State; st != StateStarting {
		t.Errorf("state = %q, want %q: the start is still in flight", st, StateStarting)
	}
}

// TestOllamaStopCancelsAnInFlightStart is the other half: the adapter's own
// stop IS allowed to cut a start short, and must be — otherwise a stop that
// lands between the decision to spawn and the spawn finds nothing to stop
// and the leader brings an engine up after it returned.
func TestOllamaStopCancelsAnInFlightStart(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	defer close(release)
	host, port := splitHostPort(t, srv.URL)

	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 200,
		StopTimeout: 50 * time.Millisecond,
	})

	started := make(chan struct{})
	go func() {
		defer close(started)
		_ = a.EnsureRunning(context.Background())
	}()
	waitFor(t, time.Second, "the leader to reach the spawn", func() bool { return spawner.lastCtx() != nil })

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the start never finished after Stop; the stop did not reach it")
	}
	if !spawner.lastProcess().hasExited() {
		t.Error("the child survived a deliberate Stop")
	}
}
