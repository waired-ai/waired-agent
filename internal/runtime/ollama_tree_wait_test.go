package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// treeWaitAdapter is an ollama adapter on a healthy fake engine, sharing
// exits with whatever else the test builds.
func treeWaitAdapter(t *testing.T, exits *PendingExits) (*OllamaAdapter, *fakeSpawner) {
	t.Helper()
	srv := okHealthServer(t)
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    50 * time.Millisecond,
		PendingExits:   exits,
	})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, spawner
}

// startAndRetireHeld brings the engine up, holds the child's tree open,
// and stops it — the moment after a model-switch bounce or a crash reap
// on a host whose runner is still inside a driver call.
func startAndRetireHeld(t *testing.T, a *OllamaAdapter, spawner *fakeSpawner) *fakeProcess {
	t.Helper()
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	old := spawner.lastProcess()
	old.holdTree.Store(true)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return old
}

// PRODUCT CONTRACT (waired-ai/waired-agent#1443): a start does not spawn
// a new engine while the previous engine's processes are still running,
// and spawns once they are gone.
func TestOllamaAdapter_EnsureRunning_WaitsForTheRetiredEnginesTree(t *testing.T) {
	a, spawner := treeWaitAdapter(t, NewPendingExits(time.Minute, time.Millisecond))
	old := startAndRetireHeld(t, a, spawner)

	done := make(chan error, 1)
	go func() { done <- a.EnsureRunning(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if n := spawner.spawnCount(); n != 1 {
		t.Fatalf("spawned %d engines while the previous tree was alive, want 1", n)
	}
	if st := a.Health(context.Background()).State; st != StateStarting {
		t.Errorf("state while waiting = %v, want %v", st, StateStarting)
	}
	if !old.wasKilled() {
		t.Errorf("the retired tree was not killed before the wait")
	}
	old.holdTree.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureRunning after the tree exited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureRunning did not proceed after the tree exited")
	}
	if n := spawner.spawnCount(); n != 2 {
		t.Errorf("spawned %d engines, want 2 (one respawn after the tree exited)", n)
	}
}

// Past the limit the start fails with the reason on last_error and counts
// as a failed start, and nothing is spawned.
func TestOllamaAdapter_EnsureRunning_GivesUpOnATreeThatOutlivesTheLimit(t *testing.T) {
	exits := NewPendingExits(30*time.Millisecond, time.Millisecond)
	a, spawner := treeWaitAdapter(t, exits)
	var mu sync.Mutex
	var failures []string
	a.cfg.OnStartFailed = func(detail string) { mu.Lock(); failures = append(failures, detail); mu.Unlock() }
	startAndRetireHeld(t, a, spawner)

	err := a.EnsureRunning(context.Background())
	if !errors.Is(err, ErrPreviousEngineStillExiting) {
		t.Fatalf("EnsureRunning = %v, want ErrPreviousEngineStillExiting", err)
	}
	if n := spawner.spawnCount(); n != 1 {
		t.Errorf("spawned %d engines past the limit, want 1 (none beside the stuck tree)", n)
	}
	h := a.Health(context.Background())
	if h.State != StateFailed || !strings.Contains(h.LastErr, "previous engine") {
		t.Errorf("health = %v %q, want failed with the reason", h.State, h.LastErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(failures)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("OnStartFailed calls = %d, want 1", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A Stop landing while a start waits cancels the start: no spawn after it.
func TestOllamaAdapter_Stop_CancelsAStartWaitingOnATree(t *testing.T) {
	a, spawner := treeWaitAdapter(t, NewPendingExits(time.Hour, time.Millisecond))
	startAndRetireHeld(t, a, spawner)

	done := make(chan error, 1)
	go func() { done <- a.EnsureRunning(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("EnsureRunning = nil after a Stop cancelled it")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("EnsureRunning kept waiting after Stop")
	}
	time.Sleep(20 * time.Millisecond)
	if n := spawner.spawnCount(); n != 1 {
		t.Errorf("spawned %d engines, want 1 (the cancelled start must not spawn)", n)
	}
}

// The record is host-wide: one engine's stuck processes hold back the
// start of another engine sharing it (ollama and vLLM share the memory).
func TestPendingExits_AreSharedBetweenEngines(t *testing.T) {
	exits := NewPendingExits(time.Minute, time.Millisecond)
	first, firstSpawner := treeWaitAdapter(t, exits)
	old := startAndRetireHeld(t, first, firstSpawner)
	second, secondSpawner := treeWaitAdapter(t, exits)

	done := make(chan error, 1)
	go func() { done <- second.EnsureRunning(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if n := secondSpawner.spawnCount(); n != 0 {
		t.Fatalf("the other engine spawned %d times beside the first engine's stuck tree, want 0", n)
	}
	old.holdTree.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second EnsureRunning: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the other engine did not start after the tree exited")
	}
}
