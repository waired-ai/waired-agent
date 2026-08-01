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

// stubbornProc ignores signals and only ends on Kill — the shape of
// ollama on Windows (no deliverable SIGTERM) and of any engine slow to
// wind down. It records the signals it was sent so a test can prove the
// graceful phase was actually attempted.
type stubbornProc struct {
	mu      sync.Mutex
	signals []os.Signal
	killed  bool
	done    chan struct{}
	once    sync.Once
}

func newStubbornProc() *stubbornProc          { return &stubbornProc{done: make(chan struct{})} }
func (p *stubbornProc) PID() int              { return 4243 }
func (p *stubbornProc) Done() <-chan struct{} { return p.done }
func (p *stubbornProc) Err() error            { return nil }
func (p *stubbornProc) Signal(s os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, s)
	p.mu.Unlock()
	return nil
}
func (p *stubbornProc) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}
func (p *stubbornProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}
func (p *stubbornProc) signalCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.signals)
}

type stubbornSpawner struct{ proc *stubbornProc }

func (s *stubbornSpawner) Spawn(context.Context, string, []string, []string, io.Writer) (infruntime.RunningProcess, error) {
	return s.proc, nil
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

// TestEngineController_StopEngine_SurvivesCancelledRequestContext pins
// the PRODUCT CONTRACT introduced by #316: the management handler hands
// StopEngine the HTTP request context, which the tray's own budget
// cancels long before the stop can finish. That cancellation must bound
// only how long the CALLER waits — it must not truncate the graceful
// SIGTERM window, and it must never end with a live engine behind a
// latched "stopped" power state.
func TestEngineController_StopEngine_SurvivesCancelledRequestContext(t *testing.T) {
	const stopTimeout = 80 * time.Millisecond
	proc := newStubbornProc()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := hostPort(t, srv.URL)
	a := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &stubbornSpawner{proc: proc}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: stopTimeout,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// The tray already gave up: its request context is dead on arrival.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ec := newEngineController(context.Background(), a, nil)
	start := time.Now()
	if err := ec.StopEngine(ctx); err != nil {
		t.Fatalf("StopEngine with a cancelled request ctx = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if !proc.wasKilled() {
		t.Fatal("the engine survived the stop; a cancelled caller must not abandon it alive")
	}
	if proc.signalCount() == 0 {
		t.Error("no graceful signal attempted")
	}
	// The graceful window is StopTimeout. If the caller's cancellation had
	// leaked into the adapter, the kill would have been immediate.
	if elapsed < stopTimeout/2 {
		t.Errorf("stop finished in %s (< StopTimeout %s): the caller's cancellation truncated the graceful window", elapsed, stopTimeout)
	}
	if power, _ := ec.EngineState(); power != management.EnginePowerStopped {
		t.Errorf("engine_power = %q, want stopped", power)
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
