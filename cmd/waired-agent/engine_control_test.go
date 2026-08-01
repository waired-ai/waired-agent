package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// --- minimal fake subprocess for the engineController tests ---

type fakeProc struct {
	done chan struct{}
	once sync.Once
}

func newFakeProc() *fakeProc { return &fakeProc{done: make(chan struct{})} }

// watchCtx mirrors what exec.CommandContext does to a real child: the
// process dies when the context that spawned it is cancelled. Without
// this the fake could not express #305/R0 — a pull that spawned the
// engine on its own self-cancelling context killed it on completion, and
// a spawner that discards the ctx makes that case unwritable.
func (p *fakeProc) watchCtx(ctx context.Context) *fakeProc {
	if ctx == nil || ctx.Done() == nil {
		return p
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p
}

func (p *fakeProc) PID() int              { return 4242 }
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { return nil }
func (p *fakeProc) Signal(os.Signal) error {
	p.once.Do(func() { close(p.done) })
	return nil
}
func (p *fakeProc) Kill() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

type fakeSpawner struct {
	mu    sync.Mutex
	calls int
	procs []*fakeProc
	ctxs  []context.Context
}

// Spawn honours the context it is handed, because DefaultSpawner does:
// it builds the child with exec.CommandContext (internal/runtime/
// spawner_unix.go, spawner_windows.go), so cancelling the spawning
// context kills the engine.
func (s *fakeSpawner) Spawn(ctx context.Context, _ string, _, _ []string, _ io.Writer) (infruntime.RunningProcess, error) {
	p := newFakeProc().watchCtx(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.procs = append(s.procs, p)
	s.ctxs = append(s.ctxs, ctx)
	return p, nil
}

// lastProc returns the most recently spawned child, or nil.
func (s *fakeSpawner) lastProc() *fakeProc {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.procs) == 0 {
		return nil
	}
	return s.procs[len(s.procs)-1]
}

// lastCtx returns the context the most recent child was spawned with, or
// nil. Asserting on this is deterministic where waiting for the child to
// notice a cancellation is not.
func (s *fakeSpawner) lastCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ctxs) == 0 {
		return nil
	}
	return s.ctxs[len(s.ctxs)-1]
}

func hostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	rest := strings.TrimPrefix(raw, "http://")
	host, portStr, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatalf("bad url %q", raw)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port in %q: %v", raw, err)
	}
	return host, p
}

func newTestAdapter(t *testing.T, borrowed bool) *infruntime.OllamaAdapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	return infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Borrowed: borrowed, Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
}

func TestEngineController_StopThenStart(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	ec := newEngineController(context.Background(), a, nil)

	if power, managed := ec.EngineState(); power != management.EnginePowerRunning || !managed {
		t.Fatalf("initial EngineState = %s managed=%v, want running/true", power, managed)
	}

	if err := ec.StopEngine(context.Background()); err != nil {
		t.Fatalf("StopEngine: %v", err)
	}
	if power, _ := ec.EngineState(); power != management.EnginePowerStopped {
		t.Errorf("after stop power = %s, want stopped", power)
	}
	if !a.IsParked() {
		t.Error("adapter not parked after StopEngine")
	}

	// StartEngine must return promptly (async restart) and clear the park.
	done := make(chan error, 1)
	go func() { done <- ec.StartEngine(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartEngine: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartEngine did not return promptly (should be async)")
	}
	if a.IsParked() {
		t.Error("adapter still parked after StartEngine")
	}
	// Background EnsureRunning should bring it back to running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if power, _ := ec.EngineState(); power == management.EnginePowerRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("engine did not return to running after StartEngine")
}

// TestEngineReady_ParkedIsNotReady verifies the /healthz path reports
// not-ready when the engine is hard-stopped, so the remote coordinator
// doesn't advertise capacity that would 503. The parked check short-
// circuits before the store load, so no store fixture is needed.
func TestEngineReady_ParkedIsNotReady(t *testing.T) {
	a := newTestAdapter(t, false)
	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	p := &agentInferenceProvider{ollama: a}
	if ready, _ := p.EngineReady(); ready {
		t.Error("EngineReady = true while parked, want false")
	}
}

func TestEngineController_BorrowedNotManaged(t *testing.T) {
	a := newTestAdapter(t, true)
	ec := newEngineController(context.Background(), a, nil)
	if _, managed := ec.EngineState(); managed {
		t.Error("EngineState managed = true for borrowed engine, want false")
	}
	if err := ec.StopEngine(context.Background()); err != infruntime.ErrEngineBorrowed {
		t.Errorf("StopEngine (borrowed) = %v, want ErrEngineBorrowed", err)
	}
	if a.IsParked() {
		t.Error("borrowed engine parked; power axis must be a no-op")
	}
}
