package codeui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeProcess implements infruntime.RunningProcess with test-controllable
// exit timing.
type fakeProcess struct {
	done     chan struct{}
	doneOnce sync.Once
	exitErr  atomic.Value
	mu       sync.Mutex
	signals  []os.Signal
	killed   bool
}

func newFakeProcess() *fakeProcess { return &fakeProcess{done: make(chan struct{})} }

func (p *fakeProcess) PID() int              { return 4242 }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Err() error {
	if v := p.exitErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}
func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	return nil
}
func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
	return nil
}
func (p *fakeProcess) sentSignals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]os.Signal, len(p.signals))
	copy(out, p.signals)
	return out
}

// fakeSpawner records the spawn call and returns a controllable process.
type fakeSpawner struct {
	mu       sync.Mutex
	calls    int
	lastBin  string
	lastArgs []string
	lastEnv  []string
	lastCtx  context.Context
	proc     *fakeProcess
}

func (s *fakeSpawner) Spawn(ctx context.Context, binary string, args, env []string, _ io.Writer) (infruntime.RunningProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastBin = binary
	s.lastArgs = args
	s.lastEnv = env
	s.lastCtx = ctx
	if s.proc == nil {
		s.proc = newFakeProcess()
	}
	return s.proc, nil
}

// spawnContext returns the context the child was spawned under (the real
// DefaultSpawner passes this to exec.CommandContext, so its cancellation
// kills the process).
func (s *fakeSpawner) spawnContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCtx
}

func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func TestService_EnsureRunning_Success(t *testing.T) {
	// httptest server stands in for the listening `opencode serve`.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)

	sp := &fakeSpawner{}
	svc := New(Config{
		Host:           host,
		Port:           port,
		BinaryPath:     "/opt/waired/opencode",
		XDGConfigHome:  "/state/config",
		DataDir:        "/state/data",
		Workspace:      "/home/u/project",
		ServerPassword: "s3cr3t",
		Spawner:        sp,
		HTTPClient:     srv.Client(),
		StopTimeout:    50 * time.Millisecond, // fake proc ignores SIGTERM; keep the grace short
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := svc.Health().State; got != infruntime.StateReady {
		t.Fatalf("state = %q, want ready", got)
	}

	// serve subcommand with loopback bind flags.
	wantArgs := []string{"serve", "--port", strconv.Itoa(port), "--hostname", host}
	if strings.Join(sp.lastArgs, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("args = %v, want %v", sp.lastArgs, wantArgs)
	}
	if sp.lastBin != "/opt/waired/opencode" {
		t.Errorf("binary = %q", sp.lastBin)
	}

	// Isolation env: XDG_CONFIG_HOME (the real isolation knob in 1.17.8, not
	// OPENCODE_CONFIG_DIR) + data dir set, autoupdate off, and the backend
	// password present so the proxy is the only client that can reach opencode.
	if v, _ := envValue(sp.lastEnv, "XDG_CONFIG_HOME"); v != "/state/config" {
		t.Errorf("XDG_CONFIG_HOME = %q, want /state/config", v)
	}
	if _, ok := envValue(sp.lastEnv, "OPENCODE_CONFIG_DIR"); ok {
		t.Error("OPENCODE_CONFIG_DIR must NOT be set (ignored by opencode 1.17.8; XDG_CONFIG_HOME is the knob)")
	}
	if v, _ := envValue(sp.lastEnv, "XDG_DATA_HOME"); v != "/state/data" {
		t.Errorf("XDG_DATA_HOME = %q, want /state/data", v)
	}
	if v, _ := envValue(sp.lastEnv, "OPENCODE_DISABLE_AUTOUPDATE"); v != "1" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q, want 1", v)
	}
	if v, _ := envValue(sp.lastEnv, "OPENCODE_SERVER_PASSWORD"); v != "s3cr3t" {
		t.Errorf("OPENCODE_SERVER_PASSWORD = %q, want the configured backend password", v)
	}

	// Stop sends SIGTERM.
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	gotTerm := false
	for _, s := range sp.proc.sentSignals() {
		if s == syscall.SIGTERM {
			gotTerm = true
		}
	}
	if !gotTerm {
		t.Errorf("Stop did not send SIGTERM; signals=%v", sp.proc.sentSignals())
	}
}

// TestService_EnsureRunning_ProcessOutlivesCallerContext is the regression
// guard for the bundled coding agent dying the instant the request that
// started it returns. EnsureRunning is reached with the HTTP request context
// of POST /waired/v1/codeui/open; the real DefaultSpawner feeds the spawn
// context to exec.CommandContext, so if the child is bound to that request
// context it is SIGKILLed when the handler responds — leaving the Service
// cached as Ready with nothing listening on :9480. The child's lifetime must
// be tied to the Service, not the caller's context.
func TestService_EnsureRunning_ProcessOutlivesCallerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)

	sp := &fakeSpawner{}
	svc := New(Config{
		Host: host, Port: port, BinaryPath: "/x/opencode",
		Spawner: sp, HTTPClient: srv.Client(),
		StopTimeout: 50 * time.Millisecond, // fake proc ignores SIGTERM
	})

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	if err := svc.EnsureRunning(callerCtx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	spawnCtx := sp.spawnContext()
	if spawnCtx == nil {
		t.Fatal("spawner captured no context")
	}
	if spawnCtx == callerCtx {
		t.Fatal("child was spawned under the caller's context; it dies when the request ends")
	}

	// The request completing (caller ctx cancelled) must NOT cancel the child.
	cancelCaller()
	select {
	case <-spawnCtx.Done():
		t.Fatal("child context cancelled with the caller context; opencode would be SIGKILLed on request completion")
	case <-time.After(50 * time.Millisecond):
	}
	if got := svc.Health().State; got != infruntime.StateReady {
		t.Fatalf("state = %q, want ready after caller ctx cancel", got)
	}

	// Stop() owns teardown and must release the Service spawn context (no leak).
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-spawnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the child context")
	}
}

func TestService_EnsureRunning_NoBinary(t *testing.T) {
	svc := New(Config{Host: "127.0.0.1", Port: 9480, Spawner: &fakeSpawner{}})
	err := svc.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("want error for empty BinaryPath")
	}
	if got := svc.Health().State; got != infruntime.StateFailed {
		t.Errorf("state = %q, want failed", got)
	}
}

func TestService_EnsureRunning_Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	sp := &fakeSpawner{}
	svc := New(Config{Host: host, Port: port, BinaryPath: "/x/opencode", Spawner: sp, HTTPClient: srv.Client()})
	if err := svc.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("first EnsureRunning: %v", err)
	}
	if err := svc.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("second EnsureRunning: %v", err)
	}
	if sp.calls != 1 {
		t.Errorf("spawn calls = %d, want 1 (idempotent)", sp.calls)
	}
}

// splitHostPort extracts "127.0.0.1" + port from an httptest URL.
func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	hp := strings.TrimPrefix(raw, "http://")
	host, portStr, ok := strings.Cut(hp, ":")
	if !ok {
		t.Fatalf("bad test URL %q", raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port %q: %v", portStr, err)
	}
	return host, port
}
