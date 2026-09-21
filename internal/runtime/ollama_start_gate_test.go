package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The start gate (waired-agent#1511): nothing is spawned while the gate is
// closed, the start proceeds once it opens, and a Stop that ends the wait
// spawns nothing and costs the engine no strike.
func TestOllamaAdapter_StartGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.34.2"}`))
		}
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)

	newAdapter := func(gate func(context.Context) error, spawner *fakeSpawner, strikes *atomic.Int32) *OllamaAdapter {
		return NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: host, Port: port,
			Spawner: spawner, HTTPClient: srv.Client(),
			HealthInterval: 10 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
			StopTimeout:   100 * time.Millisecond,
			StartGate:     gate,
			OnStartFailed: func(string) { strikes.Add(1) },
		})
	}
	chanGate := func(open <-chan struct{}) func(context.Context) error {
		return func(ctx context.Context) error {
			select {
			case <-open:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	t.Run("nothing spawns while the update runs", func(t *testing.T) {
		open := make(chan struct{})
		spawner := &fakeSpawner{}
		var strikes atomic.Int32
		a := newAdapter(chanGate(open), spawner, &strikes)
		errc := make(chan error, 1)
		go func() { errc <- a.EnsureRunning(context.Background()) }()
		time.Sleep(100 * time.Millisecond)
		if n := spawner.spawnCount(); n != 0 {
			t.Fatalf("spawned %d times while the engine's files were being replaced", n)
		}
		if st := a.Health(context.Background()).State; st != StateStarting {
			t.Errorf("state while held = %s, want %s", st, StateStarting)
		}
		close(open)
		if err := <-errc; err != nil {
			t.Fatalf("EnsureRunning after the update = %v", err)
		}
		if n := spawner.spawnCount(); n != 1 {
			t.Errorf("spawned %d times after the gate opened, want 1", n)
		}
		spawner.lastProcess().exit(nil)
	})

	t.Run("a stop during the wait is not a failed start", func(t *testing.T) {
		spawner := &fakeSpawner{}
		var strikes atomic.Int32
		a := newAdapter(chanGate(make(chan struct{})), spawner, &strikes)
		errc := make(chan error, 1)
		go func() { errc <- a.EnsureRunning(context.Background()) }()
		time.Sleep(50 * time.Millisecond)
		_ = a.Stop(context.Background())
		err := <-errc
		if err == nil {
			t.Fatal("EnsureRunning succeeded although the gate never opened")
		}
		time.Sleep(50 * time.Millisecond) // OnStartFailed runs on its own goroutine
		if spawner.spawnCount() != 0 || strikes.Load() != 0 {
			t.Errorf("spawns=%d strikes=%d after a stop during the wait; want neither", spawner.spawnCount(), strikes.Load())
		}
	})
}

func TestStartFailureIsEvidence_AHeldStartIsNot(t *testing.T) {
	held := fmt.Errorf("%w: %v", ErrEngineStartHeld, context.Canceled)
	if startFailureIsEvidence(held, false) {
		t.Error("a start that ended while held back from spawning counted as a failed start")
	}
	if !startFailureIsEvidence(errors.New("ollama: process exited during startup"), false) {
		t.Error("a real start failure stopped counting")
	}
}
