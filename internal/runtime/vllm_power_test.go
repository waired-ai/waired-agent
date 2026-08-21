//go:build linux

package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// newParkableVLLM builds an adapter against a healthy fake server, with a
// park hook and a crash hook the test drives.
func newParkableVLLM(t *testing.T, parked *atomic.Bool, onUnhealthy func(string)) (*VLLMAdapter, *fakeSpawner, *vllmFakeServer) {
	t.Helper()
	server := newVLLMFakeServer("qwen3-32b-instruct")
	t.Cleanup(server.srv.Close)
	server.healthy.Store(true)
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		Model: "/models/qwen3-32b/awq", ServedModelName: "qwen3-32b-instruct",
		Spawner: spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 50,
		StopTimeout: 50 * time.Millisecond,
		Parked:      func() bool { return parked.Load() },
		OnUnhealthy: onUnhealthy,
	})
	return a, spawner, server
}

// TestVLLMAdapter_EnsureRunning_RefusesWhenParked is waired-agent#881 at the
// adapter: the gateway calls EnsureRunning per request
// (internal/gateway/openai.go), so without this the next inference request
// re-spawns the engine an operator stopped to get their VRAM back — and on
// vLLM stopping the process is the ONLY way to release it, because the pool
// is reserved at start-up and held to process exit.
//
// The second half is the negative control. A refusal test that never watches
// the same subject SPAWN cannot tell "the latch worked" from "this fixture
// never starts anything".
func TestVLLMAdapter_EnsureRunning_RefusesWhenParked(t *testing.T) {
	var parked atomic.Bool
	parked.Store(true)
	a, spawner, _ := newParkableVLLM(t, &parked, nil)

	err := a.EnsureRunning(context.Background())
	if !errors.Is(err, ErrEngineParked) {
		t.Fatalf("EnsureRunning while parked = %v, want ErrEngineParked", err)
	}
	if n := spawner.spawnCount(); n != 0 {
		t.Fatalf("spawned %d times while parked; the refusal must come before the spawn", n)
	}

	// Negative control: the only thing that changed is the latch.
	parked.Store(false)
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning after unpark = %v, want nil", err)
	}
	if n := spawner.spawnCount(); n != 1 {
		t.Fatalf("spawned %d times after unpark, want 1: the assertion above was vacuous", n)
	}
}

// TestVLLMAdapter_ParkedRefusesEvenWhenReady pins the ordering: the park
// check runs BEFORE the StateReady fast path. The latch is what the status
// surfaces report, so an adapter that still believes it is ready must not
// hand that engine back to request traffic.
func TestVLLMAdapter_ParkedRefusesEvenWhenReady(t *testing.T) {
	var parked atomic.Bool
	a, _, _ := newParkableVLLM(t, &parked, nil)

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if st := a.Health(context.Background()).State; st != StateReady {
		t.Fatalf("state = %q, want ready", st)
	}
	parked.Store(true)
	if err := a.EnsureRunning(context.Background()); !errors.Is(err, ErrEngineParked) {
		t.Fatalf("EnsureRunning while parked-and-ready = %v, want ErrEngineParked", err)
	}
}

// TestVLLMAdapter_ChildExitAfterReady_DemotesAndNotifies is waired-agent#946
// — the ollama fix from waired-agent#29, mirrored. Before this nothing
// watched the child once waitReady returned: a vLLM that died stayed latched
// StateReady for the life of the daemon, EnsureRunning short-circuited on it,
// and the gateway proxied every request into a dead port.
func TestVLLMAdapter_ChildExitAfterReady_DemotesAndNotifies(t *testing.T) {
	var parked atomic.Bool
	notified := make(chan string, 1)
	a, spawner, _ := newParkableVLLM(t, &parked, func(d string) {
		select {
		case notified <- d:
		default:
		}
	})

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	spawner.lastProcess().exit(errors.New("exit status 1"))

	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("the engine died and nothing noticed")
	}
	waitFor(t, time.Second, "the adapter to leave StateReady", func() bool {
		return a.Health(context.Background()).State == StateFailed
	})
}

// TestVLLMAdapter_DeliberateStopIsNotACrash: Stop closes proc.Done() too, and
// charging that as a crash would let every model switch and every operator
// stop spend the recovery budget.
func TestVLLMAdapter_DeliberateStopIsNotACrash(t *testing.T) {
	var parked atomic.Bool
	var crashes atomic.Int32
	a, _, _ := newParkableVLLM(t, &parked, func(string) { crashes.Add(1) })

	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := crashes.Load(); n != 0 {
		t.Fatalf("a deliberate stop was reported as %d crash(es)", n)
	}
	if st := a.Health(context.Background()).State; st != StateStopped {
		t.Errorf("state after Stop = %q, want %q", st, StateStopped)
	}
}

// TestVLLMAdapter_GiveUpLatchRefusesUntilCleared: once automatic recovery has
// given up, a request must not turn into a fresh multi-minute spawn attempt.
// The documented reset is an explicit engine start, which calls ClearFailure.
func TestVLLMAdapter_GiveUpLatchRefusesUntilCleared(t *testing.T) {
	var parked atomic.Bool
	a, spawner, _ := newParkableVLLM(t, &parked, nil)

	a.LatchFailed("CUDA out of memory")
	err := a.EnsureRunning(context.Background())
	if !errors.Is(err, ErrEngineUnrecoverable) {
		t.Fatalf("EnsureRunning while latched = %v, want ErrEngineUnrecoverable", err)
	}
	if n := spawner.spawnCount(); n != 0 {
		t.Fatalf("spawned %d times while latched", n)
	}
	if latched, reason := a.FailureLatchedReason(); !latched || reason != "CUDA out of memory" {
		t.Fatalf("FailureLatchedReason = %v, %q", latched, reason)
	}

	a.ClearFailure()
	if err := a.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning after ClearFailure = %v, want nil", err)
	}
	if n := spawner.spawnCount(); n != 1 {
		t.Fatalf("spawned %d times after ClearFailure, want 1", n)
	}
}

// TestVLLMAdapter_ParkDuringStartupTearsDownTheChild: a vLLM start is minutes
// on a multi-GB model, so a park landing mid-start is the ordinary case, not
// a race to ignore. Finishing that start would hand the operator an engine
// holding the whole pool they just asked to get back.
func TestVLLMAdapter_ParkDuringStartupTearsDownTheChild(t *testing.T) {
	var parked atomic.Bool
	server := newVLLMFakeServer("qwen3-32b-instruct")
	defer server.srv.Close()
	host, port := server.hostPort(t)

	spawner := &fakeSpawner{}
	a := NewVLLMAdapter(VLLMConfig{
		Python: "/venv/bin/python", Host: host, Port: port,
		ServedModelName: "qwen3-32b-instruct",
		Spawner:         spawner, HTTPClient: vllmHTTPClient(),
		HealthInterval: 5 * time.Millisecond, HealthSuccess: 1, HealthMaxFails: 400,
		StopTimeout: 50 * time.Millisecond,
		Parked:      func() bool { return parked.Load() },
	})

	res := make(chan error, 1)
	go func() { res <- a.EnsureRunning(context.Background()) }()
	waitFor(t, time.Second, "the leader to spawn", func() bool { return spawner.spawnCount() == 1 })

	// The operator parks, and only then does the engine answer /health.
	parked.Store(true)
	server.healthy.Store(true)

	select {
	case err := <-res:
		if !errors.Is(err, ErrEngineParked) {
			t.Fatalf("EnsureRunning = %v, want ErrEngineParked", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the start never finished after the park")
	}
	if !spawner.lastProcess().hasExited() {
		t.Error("the child survived a park that landed during start-up, still holding the pool")
	}
	if st := a.Health(context.Background()).State; st != StateStopped {
		t.Errorf("state = %q, want %q", st, StateStopped)
	}
}
