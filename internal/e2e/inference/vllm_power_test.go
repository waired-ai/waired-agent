//go:build e2e && gpu

// The engine power axis against a real vLLM engine and a real GPU
// (waired-agent#881).
//
// Everything else about this axis is unit-testable, and is tested: the
// decision table, the latch's ownership, the refusals. What is not is the
// only claim that matters to an operator — that the VRAM comes back. A fake
// process cannot hold GPU memory, so only this leg can tell "the adapter
// reports stopped" from "the memory was released".
//
// Run with `make e2e-vllm` on a GPU host. NOTE: the GPU CI lane is not
// scheduled today (waired#1229), so this runs by hand until it is.
package inference_e2e

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func TestVLLMEnginePowerReleasesTheGPU(t *testing.T) {
	requireNVIDIAGPU(t)
	requireGPUIdle(t)
	venv := requireVLLMVenv(t)

	// The smallest model that really serves: this test is about the process
	// and its memory, not about generation quality.
	cacheRoot := xdgDataHome() + "/waired/models/" + smokeModelName
	port := freePort(t)
	logDir := t.TempDir()

	var parked bool
	a := infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:               filepath.Join(venv, "bin", "python"),
		Host:                 "127.0.0.1",
		Port:                 port,
		Model:                cacheRoot,
		ServedModelName:      smokeModelName,
		MaxModelLen:          4096,
		GPUMemoryUtilization: 0.30,
		LogDir:               logDir,
		Spawner:              infruntime.DefaultSpawner{},
		HealthInterval:       2 * time.Second,
		HealthSuccess:        2,
		HealthMaxFails:       150,
		StopTimeout:          10 * time.Second,
		Parked:               func() bool { return parked },
	})
	t.Cleanup(func() {
		parked = false
		_ = a.Stop(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v (engine log: %s)", err, filepath.Join(logDir, "engine.log"))
	}
	if n := gpusWithComputeProcs(t); n == 0 {
		t.Fatal("the engine reported ready but nothing is on the GPU; " +
			"this test cannot observe a release that never happened")
	}

	// The operator's hard stop. On vLLM this is the ONLY release valve:
	// --gpu-memory-utilization reserves the pool at start-up and holds it to
	// process exit, so there is no unload to reach.
	parked = true
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForGPUIdle(t, 60*time.Second)
	if st := a.Health(ctx).State; st != infruntime.StateStopped {
		t.Errorf("state after stop = %q, want %q", st, infruntime.StateStopped)
	}

	// And it stays stopped. This is the half a status field cannot show:
	// the gateway calls EnsureRunning per request, so without the latch the
	// next inference request re-spawns the engine and takes the memory back.
	if err := a.EnsureRunning(ctx); !errors.Is(err, infruntime.ErrEngineParked) {
		t.Fatalf("EnsureRunning while stopped = %v, want ErrEngineParked", err)
	}
	if n := gpusWithComputeProcs(t); n != 0 {
		t.Fatalf("%d GPU(s) have compute processes after a refused start", n)
	}

	// Start again: the memory comes back under the engine, not under a
	// second orphan.
	parked = false
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning after start: %v (engine log: %s)", err, filepath.Join(logDir, "engine.log"))
	}
	if n := gpusWithComputeProcs(t); n != 1 {
		t.Fatalf("%d GPU(s) have compute processes after the restart, want exactly 1", n)
	}
}

// waitForGPUIdle waits for the driver to report the compute processes gone.
// A poll rather than an immediate read: Stop returns once the process group
// is reaped, and the driver's own accounting settles a moment later.
func waitForGPUIdle(t *testing.T, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if gpusWithComputeProcs(t) == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("the GPU still has compute processes %s after the engine was stopped; "+
		"the memory was not released", budget)
}
