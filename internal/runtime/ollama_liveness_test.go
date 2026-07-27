package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// livenessAdapter builds a spawned, ready adapter over a fake ollama that
// answers /api/tags, plus a recorder for the OnUnhealthy callback.
func livenessAdapter(t *testing.T, dir string) (*OllamaAdapter, *fakeSpawner, *unhealthyRecorder) {
	t.Helper()
	srv := okHealthServer(t)
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	rec := &unhealthyRecorder{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
		LogDir:      dir,
		OnUnhealthy: rec.record,
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if got := a.Health(context.Background()).State; got != StateReady {
		t.Fatalf("precondition: state = %s, want ready", got)
	}
	return a, spawner, rec
}

type unhealthyRecorder struct {
	mu      sync.Mutex
	details []string
}

func (r *unhealthyRecorder) record(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.details = append(r.details, detail)
}

func (r *unhealthyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.details)
}

func (r *unhealthyRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.details) == 0 {
		return ""
	}
	return r.details[len(r.details)-1]
}

// waitFor polls cond for up to d. The callbacks under test run on their own
// goroutine, so the alternative is a sleep long enough to be flaky.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// TestOllamaAdapter_ChildExitAfterReady_DemotesAndNotifies is the core of
// waired-agent#29. PRODUCT CONTRACT: once the engine is serving, waired keeps
// watching the child. Before this, waitReady's exit watcher ended with the
// readiness wait, so a crash left the adapter latched StateReady for the rest
// of the process lifetime while every request 500'd.
func TestOllamaAdapter_ChildExitAfterReady_DemotesAndNotifies(t *testing.T) {
	a, spawner, rec := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	spawner.process.exit(errors.New("signal: segmentation fault (core dumped)"))

	waitFor(t, 2*time.Second, "the adapter to leave StateReady", func() bool {
		return a.Health(context.Background()).State == StateFailed
	})
	// OnUnhealthy runs on its own goroutine (it calls back into the adapter),
	// so wait for it rather than racing it.
	waitFor(t, 2*time.Second, "OnUnhealthy to fire", func() bool { return rec.count() == 1 })
	if h := a.Health(context.Background()); h.LastErr == "" {
		t.Error("LastErr is empty; the reason must reach `waired status` / the tray unchanged")
	}
}

// TestOllamaAdapter_IntentionalStop_DoesNotReportUnhealthy pins the procGen
// guard. PRODUCT CONTRACT: proc.Done() closes on a deliberate stop too, and
// that must never be reported as a crash — otherwise every Park, model swap
// and shutdown would schedule a recovery restart.
func TestOllamaAdapter_IntentionalStop_DoesNotReportUnhealthy(t *testing.T) {
	a, _, rec := livenessAdapter(t, t.TempDir())

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Give any spurious callback a chance to land.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Errorf("OnUnhealthy fired %d times on a deliberate stop, want 0 (detail: %q)", n, rec.last())
	}
	if got := a.Health(context.Background()).State; got != StateStopped {
		t.Errorf("state = %s, want stopped", got)
	}
}

// TestOllamaAdapter_ReportUpstreamFailure pins the classifier.
//
// PRODUCT CONTRACT: only a body naming a dead runner demotes the engine. A
// plain 500 does not — the engine is entitled to reject one request — and
// neither does a 4xx, which is the request's fault. Being wrong in the
// permissive direction bounces healthy engines; being wrong in the strict
// direction just restores the old behaviour, which the canary log covers.
func TestOllamaAdapter_ReportUpstreamFailure(t *testing.T) {
	// The verbatim body from the waired-agent#29 CI failures.
	const segfault = `{"error":{"message":"llama-server process has terminated: signal: segmentation fault (core dumped)","type":"api_error"}}`

	cases := []struct {
		name       string
		status     int
		body       string
		wantDemote bool
	}{
		{"ci-segfault-500", 500, segfault, true},
		{"runner-stopped", 500, `{"error":"model runner has unexpectedly stopped"}`, true},
		{"plain-500-does-not-demote", 500, `{"error":"something went wrong"}`, false},
		{"400-with-marker-does-not-demote", 400, segfault, false},
		{"404-model-missing", 404, `{"error":"model 'x' not found"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _, rec := livenessAdapter(t, t.TempDir())
			defer func() { _ = a.Stop(context.Background()) }()

			a.ReportUpstreamFailure(c.status, []byte(c.body))

			if c.wantDemote {
				waitFor(t, time.Second, "demotion", func() bool {
					return a.Health(context.Background()).State == StateFailed
				})
				waitFor(t, time.Second, "OnUnhealthy to fire", func() bool { return rec.count() == 1 })
				return
			}
			time.Sleep(30 * time.Millisecond)
			if got := a.Health(context.Background()).State; got != StateReady {
				t.Errorf("state = %s, want ready (HTTP %d must not demote)", got, c.status)
			}
			if rec.count() != 0 {
				t.Errorf("OnUnhealthy fired %d times, want 0", rec.count())
			}
		})
	}
}

// TestOllamaAdapter_ReportUpstreamFailure_DebouncedAcrossBurst models the
// observed failure: ~90 requests over 6 minutes all received the same engine
// 500, so all 90 land here. Recovery must be scheduled once, not 90 times.
func TestOllamaAdapter_ReportUpstreamFailure_DebouncedAcrossBurst(t *testing.T) {
	a, _, rec := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.ReportUpstreamFailure(500, []byte("llama-server process has terminated"))
		}()
	}
	wg.Wait()
	waitFor(t, time.Second, "demotion", func() bool {
		return a.Health(context.Background()).State == StateFailed
	})
	waitFor(t, time.Second, "OnUnhealthy to fire", func() bool { return rec.count() >= 1 })
	// Give any extra callbacks time to land before asserting the debounce.
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Errorf("OnUnhealthy fired %d times for one death, want exactly 1", n)
	}
}

// TestOllamaAdapter_EnsureRunning_RespawnsAfterCrash pins that recovery is
// actually possible: after a demotion, EnsureRunning spawns a fresh child.
func TestOllamaAdapter_EnsureRunning_RespawnsAfterCrash(t *testing.T) {
	a, spawner, _ := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	a.ReportUpstreamFailure(500, []byte("llama-server process has terminated"))
	waitFor(t, time.Second, "demotion", func() bool {
		return a.Health(context.Background()).State == StateFailed
	})

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning after crash: %v", err)
	}
	if got := a.Health(context.Background()).State; got != StateReady {
		t.Errorf("state = %s, want ready after recovery", got)
	}
	spawner.mu.Lock()
	calls := spawner.calls
	spawner.mu.Unlock()
	if calls != 2 {
		t.Errorf("spawner.calls = %d, want 2 (the crashed child plus its replacement)", calls)
	}
}

// TestOllamaAdapter_EnsureRunning_ConcurrentCallersJoin is the PRODUCT
// CONTRACT that replaces the old "EnsureRunning called while already
// starting" hard error. That error became a 503 runtime_unhealthy for every
// request that arrived during a start — and crash recovery is exactly what
// makes arrival concurrent, so recovery without this would trade a permanent
// 500 for a wall of 503s.
func TestOllamaAdapter_EnsureRunning_ConcurrentCallersJoin(t *testing.T) {
	srv := okHealthServer(t)
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)
	spawner := &fakeSpawner{}
	a := NewOllamaAdapter(OllamaConfig{
		Binary: "/fake/ollama", Host: host, Port: port,
		Spawner: spawner, HTTPClient: srv.Client(),
		// A slow readiness wait widens the window callers race into.
		HealthInterval: 20 * time.Millisecond, HealthSuccess: 3, HealthMaxFails: 5,
		StopTimeout: 50 * time.Millisecond,
	})
	defer func() { _ = a.Stop(context.Background()) }()

	const n = 12
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.EnsureRunning(context.Background()); err != nil {
				failures.Add(1)
				t.Errorf("concurrent EnsureRunning returned %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	if f := failures.Load(); f != 0 {
		t.Errorf("%d of %d concurrent callers failed", f, n)
	}
	spawner.mu.Lock()
	calls := spawner.calls
	spawner.mu.Unlock()
	if calls != 1 {
		t.Errorf("spawner.calls = %d, want 1 — the joiners must not each spawn an engine", calls)
	}
}

// TestOllamaAdapter_LatchFailed_RefusesRespawnUntilClearFailure pins the
// honest give-up. PRODUCT CONTRACT: a model that crashes on every load must
// not turn every request into a fresh 150-second spawn attempt, and the way
// back is explicit (engineController.StartEngine calls ClearFailure).
func TestOllamaAdapter_LatchFailed_RefusesRespawnUntilClearFailure(t *testing.T) {
	a, spawner, _ := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	spawner.mu.Lock()
	before := spawner.calls
	spawner.mu.Unlock()

	a.LatchFailed("crashed 4 times within 5m")
	if !a.FailureLatched() {
		t.Fatal("FailureLatched() = false after LatchFailed")
	}

	err := a.EnsureRunning(context.Background())
	if !errors.Is(err, ErrEngineUnrecoverable) {
		t.Errorf("EnsureRunning error = %v, want ErrEngineUnrecoverable", err)
	}
	spawner.mu.Lock()
	after := spawner.calls
	spawner.mu.Unlock()
	if after != before {
		t.Errorf("spawner.calls went %d→%d; a latched engine must not respawn", before, after)
	}

	a.ClearFailure()
	if a.FailureLatched() {
		t.Error("FailureLatched() = true after ClearFailure")
	}
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Errorf("EnsureRunning after ClearFailure: %v", err)
	}
}

// TestOllamaAdapter_EngineLog_RotatedOnRespawn pins that the crash trace
// survives the automatic restart that follows it. O_TRUNC on every spawn was
// fine while nothing respawned automatically; with recovery it would destroy
// the only artifact explaining why the engine died.
func TestOllamaAdapter_EngineLog_RotatedOnRespawn(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := livenessAdapter(t, dir)
	defer func() { _ = a.Stop(context.Background()) }()

	logPath := filepath.Join(dir, "engine.log")
	const preCrash = "ggml: pre-crash detail that must survive\n"
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("precondition: engine.log missing: %v", err)
	}
	if err := os.WriteFile(logPath, []byte(preCrash), 0o644); err != nil {
		t.Fatalf("seed engine.log: %v", err)
	}

	a.ReportUpstreamFailure(500, []byte("llama-server process has terminated"))
	waitFor(t, time.Second, "demotion", func() bool {
		return a.Health(context.Background()).State == StateFailed
	})
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning after crash: %v", err)
	}

	rotated, err := os.ReadFile(logPath + ".1")
	if err != nil {
		t.Fatalf("engine.log.1 not created by the respawn: %v", err)
	}
	if string(rotated) != preCrash {
		t.Errorf("engine.log.1 = %q, want the pre-crash content %q", rotated, preCrash)
	}
}
