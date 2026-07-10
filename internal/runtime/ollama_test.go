package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProcess is a RunningProcess that lets tests control exit timing.
type fakeProcess struct {
	pid      int
	done     chan struct{}
	doneOnce sync.Once
	exitErr  atomic.Value // error
	signals  []os.Signal
	mu       sync.Mutex
	killed   bool
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{pid: 12345, done: make(chan struct{})}
}

func (p *fakeProcess) PID() int              { return p.pid }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Err() error {
	v := p.exitErr.Load()
	if v == nil {
		return nil
	}
	return v.(error)
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
	p.exit(errors.New("killed"))
	return nil
}
func (p *fakeProcess) exit(err error) {
	if err != nil {
		p.exitErr.Store(err)
	}
	p.doneOnce.Do(func() { close(p.done) })
}
func (p *fakeProcess) sentSignals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]os.Signal, len(p.signals))
	copy(out, p.signals)
	return out
}
func (p *fakeProcess) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// fakeSpawner records what was spawned and returns a controllable
// fakeProcess.
type fakeSpawner struct {
	mu       sync.Mutex
	calls    int
	lastBin  string
	lastArgs []string
	lastEnv  []string
	lastLogW io.Writer
	process  *fakeProcess
	startErr error
}

func (s *fakeSpawner) Spawn(_ context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastBin = binary
	s.lastArgs = args
	s.lastEnv = env
	s.lastLogW = logW
	if s.startErr != nil {
		return nil, s.startErr
	}
	if s.process == nil {
		s.process = newFakeProcess()
	}
	return s.process, nil
}

func TestOllamaAdapter_EnsureRunning_Success(t *testing.T) {
	healthCalls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			healthCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/version": // post-ready version cache probe
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"0.30.7"}`))
		default:
			t.Errorf("health hit unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  2,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
	})

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if a.Health(context.Background()).State != StateReady {
		t.Errorf("state = %s, want %s", a.Health(context.Background()).State, StateReady)
	}
	if spawner.calls != 1 {
		t.Errorf("spawner called %d times, want 1", spawner.calls)
	}
	if !contains(spawner.lastEnv, "OLLAMA_HOST=") || !contains(spawner.lastEnv, "OLLAMA_NO_CLOUD=1") {
		t.Errorf("expected OLLAMA_HOST and OLLAMA_NO_CLOUD in env, got %v", spawner.lastEnv)
	}
	if !contains(spawner.lastEnv, "OLLAMA_KEEP_ALIVE=60m") {
		t.Errorf("expected OLLAMA_KEEP_ALIVE=60m in env, got %v", spawner.lastEnv)
	}
	if healthCalls.Load() < 2 {
		t.Errorf("expected at least 2 health probes, got %d", healthCalls.Load())
	}
}

// survivorServer is an httptest server playing an engine that already
// owns the adapter's host:port: it answers /api/tags (health) and
// /api/version with the given version string.
func survivorServer(t *testing.T, version string) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	host, port := splitHostPort(t, srv.URL)
	return srv, host, port
}

// exitingSpawner returns a spawner whose child exits immediately, as
// `ollama serve` does on an already-bound port ("address already in use").
func exitingSpawner() *fakeSpawner {
	proc := newFakeProcess()
	proc.exit(errors.New("exit status 1"))
	return &fakeSpawner{process: proc}
}

func conflictAdapterConfig(host string, port int, spawner *fakeSpawner, client *http.Client, expected string) OllamaConfig {
	return OllamaConfig{
		Binary:          "/fake/ollama",
		Host:            host,
		Port:            port,
		Spawner:         spawner,
		HTTPClient:      client,
		ExpectedVersion: expected,
		HealthInterval:  5 * time.Millisecond,
		HealthSuccess:   3, // >1 so the process-exit is seen before "ready"
		HealthMaxFails:  50,
		StopTimeout:     100 * time.Millisecond,
	}
}

// TestOllamaAdapter_EnsureRunning_AdoptsExactPinOrphan: on the
// waired-owned port an EADDRINUSE survivor that reports exactly the
// pinned version is our own orphan from a previous agent run (the
// child outlived a crashed parent) — adopt it instead of failing.
func TestOllamaAdapter_EnsureRunning_AdoptsExactPinOrphan(t *testing.T) {
	srv, host, port := survivorServer(t, "0.30.7")
	defer srv.Close()
	spawner := exitingSpawner()
	a := NewOllamaAdapter(conflictAdapterConfig(host, port, spawner, srv.Client(), "0.30.7"))

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning should adopt the exact-pin orphan, got: %v", err)
	}
	if st := a.Health(context.Background()).State; st != StateReady {
		t.Errorf("state = %s, want %s", st, StateReady)
	}
	if spawner.calls != 1 {
		t.Errorf("spawner called %d times, want 1 (it attempts a spawn first)", spawner.calls)
	}
	if got := a.Mode(); got != EngineModeAdopted {
		t.Errorf("Mode() = %s, want %s", got, EngineModeAdopted)
	}
	if got := a.EngineVersion(); got != "0.30.7" {
		t.Errorf("EngineVersion() = %q, want 0.30.7", got)
	}
	// Adopted, not managed: a.proc must be nil so Stop()/Park() never signal a
	// process we don't own.
	a.mu.Lock()
	gotProc := a.proc
	a.mu.Unlock()
	if gotProc != nil {
		t.Errorf("a.proc = %v, want nil after adopting an orphan engine", gotProc)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop after adoption should be a no-op, got: %v", err)
	}
	if err := a.Park(context.Background()); !errors.Is(err, ErrEngineNotOwned) {
		t.Errorf("Park after adoption = %v, want ErrEngineNotOwned", err)
	}
}

// TestOllamaAdapter_EnsureRunning_RefusesVersionMismatch: a survivor
// reporting any other version is a foreign engine (e.g. a system
// ollama.service that wandered onto our port). Silent adoption is the
// incident class this design removes — the adapter must fail with an
// error naming the port, both versions, and the reuse remediation.
func TestOllamaAdapter_EnsureRunning_RefusesVersionMismatch(t *testing.T) {
	srv, host, port := survivorServer(t, "0.24.0")
	defer srv.Close()
	a := NewOllamaAdapter(conflictAdapterConfig(host, port, exitingSpawner(), srv.Client(), "0.30.7"))

	err := a.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("EnsureRunning should refuse a version-mismatched survivor")
	}
	for _, want := range []string{fmt.Sprintf("port %d", port), "0.24.0", "0.30.7", "reuse"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err.Error(), want)
		}
	}
	if st := a.Health(context.Background()); st.State != StateFailed || st.LastErr == "" {
		t.Errorf("state = %+v, want StateFailed with LastErr", st)
	}
}

// TestOllamaAdapter_EnsureRunning_RefusesWhenExpectedVersionEmpty:
// without an ExpectedVersion there is no way to identify an orphan, so
// adoption is disabled outright.
func TestOllamaAdapter_EnsureRunning_RefusesWhenExpectedVersionEmpty(t *testing.T) {
	srv, host, port := survivorServer(t, "0.30.7")
	defer srv.Close()
	a := NewOllamaAdapter(conflictAdapterConfig(host, port, exitingSpawner(), srv.Client(), ""))

	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("EnsureRunning should refuse adoption when ExpectedVersion is unset")
	}
	if got := a.Mode(); got == EngineModeAdopted {
		t.Errorf("Mode() = %s, must not be adopted", got)
	}
}

// TestOllamaAdapter_EnsureRunning_NoSurvivorKeepsOriginalError: when the
// spawn dies and nothing answers on the port, the original startup
// error must surface (not a confusing version-probe error).
func TestOllamaAdapter_EnsureRunning_NoSurvivorKeepsOriginalError(t *testing.T) {
	// Reserve a port with no listener: bind, read the port, close.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	host, port := splitHostPort(t, srv.URL)
	srv.Close()

	a := NewOllamaAdapter(conflictAdapterConfig(host, port, exitingSpawner(), &http.Client{Timeout: 50 * time.Millisecond}, "0.30.7"))
	err := a.EnsureRunning(context.Background())
	if err == nil {
		t.Fatal("EnsureRunning should fail with no engine answering")
	}
	if !strings.Contains(err.Error(), "process exited during startup") {
		t.Errorf("error %q should be the original startup error", err.Error())
	}
}

// TestOllamaAdapter_EngineVersion_CachedAfterReady covers the spawned
// and borrowed happy paths: once ready, the adapter caches the live
// /api/version answer and reports the matching mode.
func TestOllamaAdapter_EngineVersion_CachedAfterReady(t *testing.T) {
	srv, host, port := survivorServer(t, "0.30.7")
	defer srv.Close()

	t.Run("spawned", func(t *testing.T) {
		a := NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: host, Port: port,
			Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
			HealthInterval: 5 * time.Millisecond, HealthSuccess: 2,
			HealthMaxFails: 5, StopTimeout: 100 * time.Millisecond,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if got := a.EngineVersion(); got != "0.30.7" {
			t.Errorf("EngineVersion() = %q, want 0.30.7", got)
		}
		if got := a.Mode(); got != EngineModeSpawned {
			t.Errorf("Mode() = %s, want %s", got, EngineModeSpawned)
		}
	})

	t.Run("borrowed", func(t *testing.T) {
		a := NewOllamaAdapter(OllamaConfig{
			Borrowed: true, Host: host, Port: port,
			HTTPClient:     srv.Client(),
			HealthInterval: 5 * time.Millisecond, HealthSuccess: 2,
			HealthMaxFails: 5, StopTimeout: 100 * time.Millisecond,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if got := a.EngineVersion(); got != "0.30.7" {
			t.Errorf("EngineVersion() = %q, want 0.30.7", got)
		}
		if got := a.Mode(); got != EngineModeBorrowed {
			t.Errorf("Mode() = %s, want %s", got, EngineModeBorrowed)
		}
	})
}

// TestOllamaAdapter_ProcessEnv_ModelsDir: a configured ModelsDir is
// exported as OLLAMA_MODELS to the child (overriding any inherited
// value); when empty the variable is left untouched.
func TestOllamaAdapter_ProcessEnv_ModelsDir(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", "/inherited/should/lose")

	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
		ModelsDir: "/var/lib/waired/runtimes/ollama/models",
	})
	env := a.processEnv()
	if !contains(env, "OLLAMA_MODELS=/var/lib/waired/runtimes/ollama/models") {
		t.Errorf("env should contain the configured OLLAMA_MODELS, got %v", env)
	}
	for _, kv := range env {
		if kv == "OLLAMA_MODELS=/inherited/should/lose" {
			t.Errorf("inherited OLLAMA_MODELS should be dropped, got %v", env)
		}
	}

	b := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	if !contains(b.processEnv(), "OLLAMA_MODELS=/inherited/should/lose") {
		t.Errorf("without ModelsDir the inherited value should pass through")
	}
}

// TestOllamaAdapter_EnsureRunning_LazyResolve verifies the #188 lazy
// binary re-resolution: when Binary is empty at construction, the
// adapter consults BinaryResolver on EnsureRunning and spawns with the
// resolved path. This lets an agent that booted before ollama was
// installed adopt the freshly installed binary without a restart.
func TestOllamaAdapter_EnsureRunning_LazyResolve(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	resolveCalls := 0
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		BinaryResolver: func() (string, error) {
			resolveCalls++
			return "/freshly/installed/ollama", nil
		},
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if resolveCalls != 1 {
		t.Errorf("BinaryResolver called %d times, want 1", resolveCalls)
	}
	if spawner.lastBin != "/freshly/installed/ollama" {
		t.Errorf("spawned binary = %q, want resolved path", spawner.lastBin)
	}
}

// TestOllamaAdapter_EnsureRunning_ResolverError verifies that a failed
// resolution (ollama still not installed) lands the adapter in
// StateFailed rather than spawning with an empty path.
func TestOllamaAdapter_EnsureRunning_ResolverError(t *testing.T) {
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "", Host: "127.0.0.1", Port: 11434,
		Spawner: spawner,
		BinaryResolver: func() (string, error) {
			return "", errors.New("not installed")
		},
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatalf("expected error when resolver fails")
	}
	if spawner.calls != 0 {
		t.Errorf("spawner called %d times, want 0 (must not spawn with empty binary)", spawner.calls)
	}
	if h := a.Health(context.Background()); h.State != StateFailed {
		t.Errorf("state = %s, want %s", h.State, StateFailed)
	}
}

// TestOllamaAdapter_Borrowed_NoSpawn verifies reuse mode (#188): the
// adapter probes an already-running ollama and reports Ready WITHOUT
// spawning, and Stop is a no-op so we never kill the user's engine.
func TestOllamaAdapter_Borrowed_NoSpawn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Borrowed: true, Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 3,
		StopTimeout: 100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning (borrowed): %v", err)
	}
	if spawner.calls != 0 {
		t.Errorf("borrowed mode spawned %d times, want 0", spawner.calls)
	}
	if a.Health(context.Background()).State != StateReady {
		t.Errorf("state = %s, want ready", a.Health(context.Background()).State)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop (borrowed) returned %v, want nil no-op", err)
	}
}

// TestOllamaAdapter_Borrowed_Unreachable: reuse mode with nothing
// listening fails (rather than silently spawning our own).
func TestOllamaAdapter_Borrowed_Unreachable(t *testing.T) {
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Borrowed: true, Host: "127.0.0.1", Port: 1, // nothing listens on :1
		Spawner: spawner, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 2,
		StopTimeout: 100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("expected error when borrowed engine is unreachable")
	}
	if spawner.calls != 0 {
		t.Errorf("borrowed mode must never spawn; got %d calls", spawner.calls)
	}
}

func TestOllamaAdapter_EnsureRunning_Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 100 * time.Millisecond,
	})

	for i := 0; i < 3; i++ {
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning #%d: %v", i, err)
		}
	}
	if spawner.calls != 1 {
		t.Errorf("spawner called %d times across 3 EnsureRunning calls, want 1", spawner.calls)
	}
}

func TestOllamaAdapter_EnsureRunning_HealthTimeout(t *testing.T) {
	// Health endpoint always 503 and the (fake) child never exits, so the
	// supervised spawn path gives up only when StartupReadyTimeout fires —
	// NOT after HealthMaxFails. HealthMaxFails is deliberately tiny here to
	// prove it is ignored for a spawned, still-alive engine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 3, HealthMaxFails: 2,
		StartupReadyTimeout: 80 * time.Millisecond,
		StopTimeout:         50 * time.Millisecond,
	})
	start := time.Now()
	err := a.EnsureRunning(context.Background())
	if err == nil {
		t.Fatalf("expected startup-deadline error")
	}
	// Must not have bailed at ~HealthMaxFails*HealthInterval (~10ms); the
	// deadline (80ms) is the only thing that ends a supervised wait.
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Errorf("gave up after %v; supervised mode must wait for StartupReadyTimeout, not HealthMaxFails", elapsed)
	}
	if !strings.Contains(err.Error(), "not ready within") {
		t.Errorf("error %q should name the startup deadline", err.Error())
	}
	if h := a.Health(context.Background()); h.State != StateFailed {
		t.Errorf("state after health timeout = %s, want %s", h.State, StateFailed)
	}
}

// TestOllamaAdapter_EnsureRunning_SupervisedColdStart is the load-bearing
// regression test for the Windows cold-start bug: a spawned engine that is
// alive but slow to answer (its first probes fail) must NOT be killed after
// HealthMaxFails — it should keep being probed until it comes ready within
// StartupReadyTimeout. HealthMaxFails is tiny here so a regression to the
// old "give up after N fails" behaviour fails this test.
func TestOllamaAdapter_EnsureRunning_SupervisedColdStart(t *testing.T) {
	var tagCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			// First 3 probes fail (cold start), then the engine answers.
			if tagCalls.Add(1) <= 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/version":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":"0.30.7"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{} // child stays alive (never exits)
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 2,
		HealthMaxFails:      2, // would trip after 2 fails under the OLD policy
		StartupReadyTimeout: 3 * time.Second,
		StopTimeout:         100 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning should ride out a slow cold start, got: %v", err)
	}
	if st := a.Health(context.Background()).State; st != StateReady {
		t.Errorf("state = %s, want %s", st, StateReady)
	}
	if spawner.calls != 1 {
		t.Errorf("spawner called %d times, want 1 (no re-spawn)", spawner.calls)
	}
	if tagCalls.Load() < 5 {
		t.Errorf("health probed %d times; expected to keep probing past HealthMaxFails", tagCalls.Load())
	}
}

// TestOllamaAdapter_EngineLog_Captured verifies the spawned engine's
// stdout/stderr capture wiring: with LogDir set the adapter opens
// <LogDir>/engine.log and passes a non-nil writer to the spawner; without
// LogDir the writer is nil (discard) and no file is created.
func TestOllamaAdapter_EngineLog_Captured(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)

	t.Run("with LogDir", func(t *testing.T) {
		dir := t.TempDir()
		spawner := &fakeSpawner{}
		a := NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: host, Port: port,
			Spawner: spawner, HTTPClient: srv.Client(),
			HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
			StopTimeout: 50 * time.Millisecond,
			LogDir:      dir,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if spawner.lastLogW == nil {
			t.Error("spawner received nil log writer; want a non-nil capture writer when LogDir is set")
		}
		if _, err := os.Stat(filepath.Join(dir, "engine.log")); err != nil {
			t.Errorf("engine.log not created: %v", err)
		}
		// Close the open handle so TempDir cleanup can remove it (Windows).
		_ = a.Stop(context.Background())
	})

	t.Run("without LogDir", func(t *testing.T) {
		spawner := &fakeSpawner{}
		a := NewOllamaAdapter(OllamaConfig{
			Binary: "/fake/ollama", Host: host, Port: port,
			Spawner: spawner, HTTPClient: srv.Client(),
			HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
			StopTimeout: 50 * time.Millisecond,
		})
		if err := a.EnsureRunning(context.Background()); err != nil {
			t.Fatalf("EnsureRunning: %v", err)
		}
		if spawner.lastLogW != nil {
			t.Error("spawner received a non-nil log writer; want nil (discard) when LogDir is unset")
		}
		_ = a.Stop(context.Background())
	})
}

func TestOllamaAdapter_EnsureRunning_SpawnError(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 11434,
		Spawner:        &fakeSpawner{startErr: errors.New("boom")},
		HTTPClient:     http.DefaultClient,
		HealthInterval: time.Millisecond, HealthSuccess: 1, HealthMaxFails: 1,
		StopTimeout: 50 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Errorf("expected spawn error to propagate")
	}
	if h := a.Health(context.Background()); h.State != StateFailed {
		t.Errorf("state after spawn failure = %s, want %s", h.State, StateFailed)
	}
}

func TestOllamaAdapter_Stop_Sigterm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	proc := newFakeProcess()
	spawner := &fakeSpawner{process: proc}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 200 * time.Millisecond,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// Make the process exit shortly after SIGTERM (a well-behaved engine).
	go func() {
		time.Sleep(20 * time.Millisecond)
		proc.exit(nil)
	}()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	sigs := proc.sentSignals()
	if len(sigs) == 0 {
		t.Fatalf("Stop did not send any signal")
	}
	if !sentSIGTERM(sigs) {
		t.Errorf("expected SIGTERM, signals = %v", sigs)
	}
	if proc.wasKilled() {
		t.Errorf("expected graceful exit, but Kill was called")
	}
	if h := a.Health(context.Background()); h.State != StateStopped {
		t.Errorf("state after stop = %s, want %s", h.State, StateStopped)
	}
}

func TestOllamaAdapter_Stop_SigkillAfterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	proc := newFakeProcess()
	spawner := &fakeSpawner{process: proc}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 30 * time.Millisecond, // very short
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// Process never exits on its own → adapter must Kill.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !proc.wasKilled() {
		t.Errorf("expected Kill after stop timeout")
	}
}

func TestOllamaAdapter_Stop_NoOpWhenNeverStarted(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{Binary: "/fake/ollama", Host: "127.0.0.1", Port: 11434, Spawner: &fakeSpawner{}, HTTPClient: http.DefaultClient})
	if err := a.Stop(context.Background()); err != nil {
		t.Errorf("Stop on never-started adapter: %v", err)
	}
}

func TestOllamaAdapter_BaseURL(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{Host: "127.0.0.1", Port: 11434})
	if got, want := a.BaseURL(), "http://127.0.0.1:11434"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}
}

func TestOllamaAdapter_Name(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{})
	if a.Name() != "ollama" {
		t.Errorf("Name = %q", a.Name())
	}
}

func TestOllamaAdapter_HealthBeforeStart(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{})
	if h := a.Health(context.Background()); h.State != StateNotStarted {
		t.Errorf("state before EnsureRunning = %s, want %s", h.State, StateNotStarted)
	}
}

// --- helpers ---

func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	// httptest servers come back as "http://127.0.0.1:NNNNN"
	rest := strings.TrimPrefix(raw, "http://")
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		t.Fatalf("bad URL %q", raw)
	}
	host := rest[:colon]
	port := 0
	for _, c := range rest[colon+1:] {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return host, port
}

func contains(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

func okHealthServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
}

// TestOllamaAdapter_BackendEnv_AppliedAndOverridesInherited verifies the
// #290 GPU-backend wiring: BackendEnv reaches `ollama serve`, and any
// inherited env var with the same key is dropped so our value wins
// regardless of getenv duplicate-resolution order.
func TestOllamaAdapter_BackendEnv_AppliedAndOverridesInherited(t *testing.T) {
	t.Setenv("OLLAMA_VULKAN", "0") // inherited value that must lose
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
		BackendEnv:     []string{"OLLAMA_VULKAN=1", "HSA_OVERRIDE_GFX_VERSION=11.5.1"},
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !contains(spawner.lastEnv, "OLLAMA_VULKAN=1") {
		t.Errorf("OLLAMA_VULKAN=1 not in spawn env: %v", spawner.lastEnv)
	}
	if !contains(spawner.lastEnv, "HSA_OVERRIDE_GFX_VERSION=11.5.1") {
		t.Errorf("HSA_OVERRIDE_GFX_VERSION=11.5.1 not in spawn env: %v", spawner.lastEnv)
	}
	if n := countPrefix(spawner.lastEnv, "OLLAMA_VULKAN="); n != 1 {
		t.Errorf("OLLAMA_VULKAN= appears %d times, want exactly 1 (inherited must be dropped)", n)
	}
	if contains(spawner.lastEnv, "OLLAMA_VULKAN=0") {
		t.Errorf("inherited OLLAMA_VULKAN=0 leaked into spawn env: %v", spawner.lastEnv)
	}
}

// TestOllamaAdapter_SetBackendEnv_NextSpawn verifies the ROCm->Vulkan
// probe mechanism (#290): switching the backend env then re-spawning
// (after Stop) launches `ollama serve` with the new env.
func TestOllamaAdapter_SetBackendEnv_NextSpawn(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
		BackendEnv:     []string{"HSA_OVERRIDE_GFX_VERSION=11.5.1"}, // ROCm attempt
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning #1: %v", err)
	}
	if !contains(spawner.lastEnv, "HSA_OVERRIDE_GFX_VERSION=11.5.1") {
		t.Fatalf("first spawn missing ROCm env: %v", spawner.lastEnv)
	}

	// Simulate "GPU didn't engage": stop and switch to the Vulkan step.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	a.SetBackendEnv([]string{"OLLAMA_VULKAN=1"})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning #2: %v", err)
	}
	if spawner.calls != 2 {
		t.Errorf("spawner called %d times, want 2 (re-spawn after backend switch)", spawner.calls)
	}
	if !contains(spawner.lastEnv, "OLLAMA_VULKAN=1") {
		t.Errorf("second spawn missing Vulkan env: %v", spawner.lastEnv)
	}
	if contains(spawner.lastEnv, "HSA_OVERRIDE_GFX_VERSION=") {
		t.Errorf("ROCm env leaked into Vulkan re-spawn: %v", spawner.lastEnv)
	}
}

func sentSIGTERM(sigs []os.Signal) bool {
	for _, s := range sigs {
		if s != nil && s.String() == "terminated" {
			return true
		}
	}
	return false
}

// --- #186 engine power axis (Park / Unpark) ---

// readyHealthServer is a /api/tags stub that always returns 200, used by
// the park tests to bring the engine to StateReady deterministically.
func readyHealthServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
}

// TestOllamaAdapter_Park_StopsAndGuards is the load-bearing test for #186:
// after Park the engine is stopped (SIGTERM sent) and a subsequent
// EnsureRunning — the call the gateway makes per request — returns
// ErrEngineParked WITHOUT re-spawning. Unpark + EnsureRunning then brings
// it back.
func TestOllamaAdapter_Park_StopsAndGuards(t *testing.T) {
	srv := readyHealthServer(t)
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 100 * time.Millisecond,
	})

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}

	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park: %v", err)
	}
	if !a.IsParked() {
		t.Error("IsParked = false after Park, want true")
	}
	if got := a.Health(context.Background()).State; got != StateStopped {
		t.Errorf("state after Park = %s, want %s", got, StateStopped)
	}
	if !sentSIGTERM(spawner.process.sentSignals()) {
		t.Error("expected SIGTERM to the engine process on Park")
	}

	// The gateway's per-request EnsureRunning must NOT revive a parked
	// engine.
	if err := a.EnsureRunning(context.Background()); !errors.Is(err, ErrEngineParked) {
		t.Errorf("EnsureRunning while parked = %v, want ErrEngineParked", err)
	}
	if spawner.calls != 1 {
		t.Errorf("spawner calls = %d after parked EnsureRunning, want still 1", spawner.calls)
	}

	// Unpark + restart spawns a fresh process.
	a.Unpark()
	if a.IsParked() {
		t.Error("IsParked = true after Unpark, want false")
	}
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning after Unpark: %v", err)
	}
	if spawner.calls != 2 {
		t.Errorf("spawner calls = %d after restart, want 2", spawner.calls)
	}
	if got := a.Health(context.Background()).State; got != StateReady {
		t.Errorf("state after restart = %s, want %s", got, StateReady)
	}
}

// TestOllamaAdapter_Park_BeforeStart verifies parking an engine that never
// started is a safe no-op-stop that still latches the guard.
func TestOllamaAdapter_Park_BeforeStart(t *testing.T) {
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 1,
		Spawner: spawner, StopTimeout: 100 * time.Millisecond,
	})
	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park before start: %v", err)
	}
	if !a.IsParked() {
		t.Error("IsParked = false, want true")
	}
	if err := a.EnsureRunning(context.Background()); !errors.Is(err, ErrEngineParked) {
		t.Errorf("EnsureRunning = %v, want ErrEngineParked", err)
	}
	if spawner.calls != 0 {
		t.Errorf("spawner calls = %d, want 0 (never spawned)", spawner.calls)
	}
}

// TestOllamaAdapter_Park_Borrowed verifies reuse mode refuses the power
// axis: Park returns ErrEngineBorrowed, does not latch parked, and never
// signals the user's process.
func TestOllamaAdapter_Park_Borrowed(t *testing.T) {
	srv := readyHealthServer(t)
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	a := NewOllamaAdapter(OllamaConfig{
		Borrowed: true, Host: host, Port: port,
		Spawner: &fakeSpawner{}, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning (borrowed probe): %v", err)
	}
	if err := a.Park(context.Background()); !errors.Is(err, ErrEngineBorrowed) {
		t.Errorf("Park (borrowed) = %v, want ErrEngineBorrowed", err)
	}
	if a.IsParked() {
		t.Error("IsParked = true for borrowed engine, want false (power axis n/a)")
	}
	if !a.Borrowed() {
		t.Error("Borrowed() = false, want true")
	}
}

// TestOllamaAdapter_Park_DuringStartup verifies that parking while the
// readiness probe is still in flight tears the spawned process down rather
// than leaving a live engine with parked==true.
func TestOllamaAdapter_Park_DuringStartup(t *testing.T) {
	// Health endpoint never returns 2xx, so waitReady stays in flight
	// until the process is killed by Park's Stop escalation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 2, HealthMaxFails: 1000,
		StopTimeout: 20 * time.Millisecond,
	})

	errc := make(chan error, 1)
	go func() { errc <- a.EnsureRunning(context.Background()) }()

	// Wait until the spawn happened and the adapter is in Starting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.Health(context.Background()).State == StateStarting && spawner.calls == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := a.Park(context.Background()); err != nil {
		t.Fatalf("Park during startup: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Error("EnsureRunning returned nil; expected failure/parked after kill")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureRunning did not return after Park")
	}
	if !a.IsParked() {
		t.Error("IsParked = false after Park during startup, want true")
	}
	if !spawner.process.wasKilled() {
		t.Error("expected the in-flight process to be killed by Park")
	}
	if spawner.calls != 1 {
		t.Errorf("spawner calls = %d, want 1", spawner.calls)
	}
}

// --- #621 per-model tuning env (SetModelEnv / AppliedTuning) ---

// TestOllamaAdapter_ProcessEnv_ModelEnvOverridesInherited: modelEnv keys
// are dropped from the inherited environment so our computed values win,
// and ExtraEnv (test hook) keeps the last word over modelEnv.
func TestOllamaAdapter_ProcessEnv_ModelEnvOverridesInherited(t *testing.T) {
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096") // inherited value that must lose

	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	a.SetModelEnv([]string{
		"OLLAMA_CONTEXT_LENGTH=131072",
		"OLLAMA_KV_CACHE_TYPE=q8_0",
		"OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_FLASH_ATTENTION=1",
	})
	env := a.processEnv()
	for _, want := range []string{
		"OLLAMA_CONTEXT_LENGTH=131072",
		"OLLAMA_KV_CACHE_TYPE=q8_0",
		"OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_FLASH_ATTENTION=1",
	} {
		if !contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	if n := countPrefix(env, "OLLAMA_CONTEXT_LENGTH="); n != 1 {
		t.Errorf("OLLAMA_CONTEXT_LENGTH= appears %d times, want exactly 1", n)
	}
	if contains(env, "OLLAMA_CONTEXT_LENGTH=4096") {
		t.Errorf("inherited OLLAMA_CONTEXT_LENGTH leaked into env: %v", env)
	}

	// ExtraEnv is appended after modelEnv so a test can still override.
	b := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
		ExtraEnv: []string{"OLLAMA_KV_CACHE_TYPE=f16"},
	})
	b.SetModelEnv([]string{"OLLAMA_KV_CACHE_TYPE=q8_0"})
	benv := b.processEnv()
	iModel, iExtra := -1, -1
	for i, kv := range benv {
		switch kv {
		case "OLLAMA_KV_CACHE_TYPE=q8_0":
			iModel = i
		case "OLLAMA_KV_CACHE_TYPE=f16":
			iExtra = i
		}
	}
	if iModel == -1 || iExtra == -1 || iExtra < iModel {
		t.Errorf("ExtraEnv should come after modelEnv (model=%d extra=%d): %v", iModel, iExtra, benv)
	}
}

// TestOllamaAdapter_SetModelEnv_NextSpawn verifies the #621 degrade path
// mechanism: swapping the model env then re-spawning (after Stop)
// launches `ollama serve` with the recomputed values.
func TestOllamaAdapter_SetModelEnv_NextSpawn(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
	})
	a.SetModelEnv([]string{"OLLAMA_CONTEXT_LENGTH=200704", "OLLAMA_KV_CACHE_TYPE=q8_0"})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning #1: %v", err)
	}
	if !contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=200704") {
		t.Fatalf("first spawn missing tuning env: %v", spawner.lastEnv)
	}

	// Simulate the f16-fallback degrade: stop, shrink, re-spawn.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	a.SetModelEnv([]string{"OLLAMA_CONTEXT_LENGTH=100352", "OLLAMA_KV_CACHE_TYPE=q8_0"})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning #2: %v", err)
	}
	if spawner.calls != 2 {
		t.Errorf("spawner called %d times, want 2", spawner.calls)
	}
	if !contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=100352") {
		t.Errorf("second spawn missing recomputed env: %v", spawner.lastEnv)
	}
	if contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=200704") {
		t.Errorf("stale tuning env leaked into re-spawn: %v", spawner.lastEnv)
	}
}

// TestOllamaAdapter_AppliedTuning_RoundTrip: the applied-tuning record is
// stored and returned as-is; the zero value signals "not computed yet".
func TestOllamaAdapter_AppliedTuning_RoundTrip(t *testing.T) {
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: "127.0.0.1", Port: 9475,
	})
	if got := a.AppliedTuning(); got != (ModelTuning{}) {
		t.Errorf("AppliedTuning before set = %+v, want zero value", got)
	}
	want := ModelTuning{
		ModelID: "qwen3.6-35b-a3b", VariantID: "mtp-q4-gguf",
		ContextLength: 131072, NumParallel: 1, KVCacheType: "q8_0",
		Verified: true, Warning: "",
	}
	a.SetAppliedTuning(want)
	if got := a.AppliedTuning(); got != want {
		t.Errorf("AppliedTuning = %+v, want %+v", got, want)
	}
}

// #624: a model-env provider is consulted at spawn time when no
// explicit tuning env was exported — the fix for engines that become
// viable only after the boot-time engine decision (fresh installs
// where the binary lands mid-bootstrap previously served untuned).
func TestOllamaAdapter_ModelEnvProvider_AppliesAtSpawn(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
	})
	a.SetModelEnvProvider(func() ([]string, ModelTuning, bool) {
		return []string{"OLLAMA_CONTEXT_LENGTH=200704", "OLLAMA_KV_CACHE_TYPE=q8_0"},
			ModelTuning{ModelID: "m", VariantID: "v", ContextLength: 200704, KVCacheType: "q8_0", NumParallel: 1},
			true
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=200704") {
		t.Fatalf("provider env missing from spawn: %v", spawner.lastEnv)
	}
	if got := a.AppliedTuning(); got.ContextLength != 200704 || got.ModelID != "m" {
		t.Errorf("AppliedTuning not recorded from provider: %+v", got)
	}
}

// An explicit SetModelEnv (boot compute, degrade path) always wins over
// the provider — the provider only fills the "nothing exported yet" gap.
func TestOllamaAdapter_ModelEnvProvider_ExplicitEnvWins(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
	})
	a.SetModelEnvProvider(func() ([]string, ModelTuning, bool) {
		return []string{"OLLAMA_CONTEXT_LENGTH=999999"}, ModelTuning{ContextLength: 999999}, true
	})
	a.SetModelEnv([]string{"OLLAMA_CONTEXT_LENGTH=100352"})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if !contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=100352") {
		t.Fatalf("explicit env missing from spawn: %v", spawner.lastEnv)
	}
	if contains(spawner.lastEnv, "OLLAMA_CONTEXT_LENGTH=999999") {
		t.Errorf("provider env leaked over the explicit one: %v", spawner.lastEnv)
	}
}

// A provider that cannot resolve a target (ok=false) leaves the spawn
// untuned — never guess a window.
func TestOllamaAdapter_ModelEnvProvider_NotResolvable(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary:         "/fake/ollama",
		Host:           host,
		Port:           port,
		Spawner:        spawner,
		HTTPClient:     srv.Client(),
		HealthInterval: 10 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 5,
		StopTimeout:    100 * time.Millisecond,
	})
	a.SetModelEnvProvider(func() ([]string, ModelTuning, bool) {
		return nil, ModelTuning{}, false
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	for _, kv := range spawner.lastEnv {
		if strings.HasPrefix(kv, "OLLAMA_CONTEXT_LENGTH=") {
			t.Errorf("unresolvable provider must not export tuning: %v", kv)
		}
	}
	if got := a.AppliedTuning(); got.ContextLength != 0 {
		t.Errorf("AppliedTuning should stay zero: %+v", got)
	}
}
