//go:build e2e && gpu

// The engine power axis, and the engine's lifetime, against a real vLLM
// engine and a real GPU (waired-agent#881, #946, #947).
//
// Everything else about these is unit-testable, and is tested: the decision
// table, the latch's ownership, the refusals, the supervisor's bookkeeping.
// What is not is the only claim that matters to an operator — that the VRAM
// comes back, and that it does not stay held by something nobody is watching.
// A fake process cannot hold GPU memory, so only this leg can tell "the
// adapter reports stopped" from "the memory was released", and only this leg
// can see a worker orphaned to init still holding 7 GB.
//
// One target, not three (`make e2e-vllm-power`). The three are one surface —
// who owns the engine process and for how long — so they fail for related
// reasons and are read in the same place; and every extra target is another
// chance for one to end up with no caller, which is what waired#1229 was.
package inference_e2e

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// livePowerEngine builds an adapter against the smoke model at a modest
// utilisation — these tests are about the process and its memory, not about
// generation, and a small pool leaves room for the driver's own accounting.
func livePowerEngine(t *testing.T, venv string, parked func() bool, onUnhealthy func(string)) *infruntime.VLLMAdapter {
	t.Helper()
	return infruntime.NewVLLMAdapter(infruntime.VLLMConfig{
		Python:               filepath.Join(venv, "bin", "python"),
		Host:                 "127.0.0.1",
		Port:                 freePort(t),
		Model:                xdgDataHome() + "/waired/models/" + smokeModelName,
		ServedModelName:      smokeModelName,
		MaxModelLen:          4096,
		GPUMemoryUtilization: 0.30,
		LogDir:               t.TempDir(),
		Spawner:              infruntime.DefaultSpawner{},
		HealthInterval:       2 * time.Second,
		HealthSuccess:        2,
		HealthMaxFails:       150,
		StopTimeout:          10 * time.Second,
		Parked:               parked,
		OnUnhealthy:          onUnhealthy,
	})
}

// TestVLLMEnginePower_ReleasesTheGPU is waired-agent#881.
//
// Measured before the fix on this hardware: with vLLM serving, the engine
// power axis reported success and latched engine_power=stopped while
// nvidia-smi still showed the compute process. And a vLLM that HAD been
// stopped did not stay stopped — the next EnsureRunning re-spawned it,
// because that adapter had no latch to refuse with.
func TestVLLMEnginePower_ReleasesTheGPU(t *testing.T) {
	requireNVIDIAGPU(t)
	requireGPUIdle(t)
	venv := requireVLLMVenv(t)

	var parked bool
	a := livePowerEngine(t, venv, func() bool { return parked }, nil)
	t.Cleanup(func() {
		parked = false
		_ = a.Stop(context.Background())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
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

	// And it STAYS stopped. This is the half a status field cannot show: the
	// gateway calls EnsureRunning per request, so without the latch the next
	// inference request takes the memory straight back.
	if err := a.EnsureRunning(ctx); !errors.Is(err, infruntime.ErrEngineParked) {
		t.Fatalf("EnsureRunning while stopped = %v, want ErrEngineParked", err)
	}
	if n := gpusWithComputeProcs(t); n != 0 {
		t.Fatalf("%d GPU(s) have compute processes after a refused start", n)
	}

	parked = false
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning after start: %v", err)
	}
	if n := gpusWithComputeProcs(t); n != 1 {
		t.Fatalf("%d GPU(s) have compute processes after the restart, want exactly 1", n)
	}
}

// TestVLLMEnginePower_NoticesACrash is waired-agent#946.
//
// Measured before the fix: SIGKILL to the compute process left
// Health().State == "ready" indefinitely, and EnsureRunning short-circuited
// on that and returned nil without trying to restart — so the gateway would
// have gone on proxying into a dead port.
func TestVLLMEnginePower_NoticesACrash(t *testing.T) {
	requireNVIDIAGPU(t)
	requireGPUIdle(t)
	venv := requireVLLMVenv(t)

	notified := make(chan string, 1)
	a := livePowerEngine(t, venv, nil, func(d string) {
		select {
		case notified <- d:
		default:
		}
	})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	pid := firstComputeAppPID(t)
	t.Logf("killing the engine's compute process (pid %d)", pid)
	if out, err := exec.Command("kill", "-9", strconv.Itoa(pid)).CombinedOutput(); err != nil {
		t.Fatalf("kill: %v (%s)", err, out)
	}

	select {
	case d := <-notified:
		t.Logf("the death was reported: %s", firstLineOf(d))
	case <-time.After(60 * time.Second):
		t.Fatal("the engine died and nothing noticed")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && a.Health(ctx).State == infruntime.StateReady {
		time.Sleep(time.Second)
	}
	if st := a.Health(ctx).State; st == infruntime.StateReady {
		t.Fatal("the adapter is still latched ready over a dead engine")
	}
}

// TestVLLMEnginePower_SurvivesItsCaller is waired-agent#947.
//
// Measured before the fix on this hardware: cancelling the context that
// started the engine reaped the api_server leader and left VLLM::EngineCore
// re-parented to init (PPID 1) holding 7616 MiB. exec.CommandContext's cancel
// is a single-pid Process.Kill(), which does not reach the group even with
// Setpgid set — so the caller's cancellation freed nothing and lost the
// handle needed to free it later.
func TestVLLMEnginePower_SurvivesItsCaller(t *testing.T) {
	requireNVIDIAGPU(t)
	requireGPUIdle(t)
	venv := requireVLLMVenv(t)

	// The shape the gateway uses: EnsureRunning on a per-request context.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	a := livePowerEngine(t, venv, nil, nil)
	t.Cleanup(func() {
		cancelReq()
		_ = a.Stop(context.Background())
	})
	if err := a.EnsureRunning(reqCtx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if n := gpusWithComputeProcs(t); n != 1 {
		t.Fatalf("%d GPU(s) have compute processes, want 1", n)
	}

	cancelReq()
	time.Sleep(10 * time.Second)

	if n := gpusWithComputeProcs(t); n != 1 {
		t.Fatalf("the engine did not survive the request that started it (%d compute GPUs)", n)
	}
	if st := a.Health(context.Background()).State; st != infruntime.StateReady {
		t.Errorf("state = %q, want %q: the engine is still serving", st, infruntime.StateReady)
	}
	// Nothing was orphaned to init either — the failure this test exists for
	// looked like a live engine from the GPU's side while waired had lost the
	// process it needed to stop it.
	if orphans := orphanedEngineProcs(t); orphans != "" {
		t.Errorf("engine processes were re-parented to init:\n%s", orphans)
	}

	// And a deliberate stop still takes the WHOLE group.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForGPUIdle(t, 60*time.Second)
	if orphans := orphanedEngineProcs(t); orphans != "" {
		t.Errorf("a deliberate stop left processes behind:\n%s", orphans)
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

// orphanedEngineProcs lists vLLM processes whose parent is init, which is
// what a leader-only kill leaves behind.
func orphanedEngineProcs(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pid,ppid,rss,comm").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "1" {
			continue
		}
		if strings.Contains(strings.ToLower(f[3]), "vllm") ||
			strings.Contains(strings.ToLower(f[3]), "enginecor") {
			found = append(found, line)
		}
	}
	return strings.Join(found, "\n")
}

func firstComputeAppPID(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader").Output()
	if err != nil {
		t.Fatalf("nvidia-smi compute-apps: %v", err)
	}
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	pid, err := strconv.Atoi(first)
	if err != nil {
		t.Fatalf("parse compute-app pid %q: %v", first, err)
	}
	return pid
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
