package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// fakeOllama is an httptest server mimicking the subset of the Ollama API
// the backend probe uses. sizeVRAM controls /api/ps placement; a restart
// (triggered by the switcher) can flip it via onRestart.
type fakeOllama struct {
	mu        sync.Mutex
	srv       *httptest.Server
	tags      []string
	sizeVRAM  int64 // size_vram reported for the loaded model
	loaded    bool  // whether a model has been "loaded" (POST /api/generate)
	loadCalls int
}

func newFakeOllama(tags []string, sizeVRAM int64) *fakeOllama {
	f := &fakeOllama{tags: tags, sizeVRAM: sizeVRAM}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeOllama) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		names := ""
		for i, t := range f.tags {
			if i > 0 {
				names += ","
			}
			names += fmt.Sprintf(`{"name":%q}`, t)
		}
		_, _ = io.WriteString(w, `{"models":[`+names+`]}`)
	case "/api/generate":
		f.loaded = true
		f.loadCalls++
		_, _ = io.WriteString(w, `{"done":true}`)
	case "/api/ps":
		if !f.loaded || len(f.tags) == 0 {
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"models":[{"name":%q,"size":1000,"size_vram":%d}]}`,
			f.tags[0], f.sizeVRAM))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeOllama) setVRAM(v int64) {
	f.mu.Lock()
	f.sizeVRAM = v
	f.loaded = false // a restart unloads the model
	f.mu.Unlock()
}

// markResident makes /api/ps answer without a POST /api/generate first,
// which is the state a real host is in by the time the boot path runs
// this probe: warmServingModelNow has already loaded the serving model.
// It is what lets a single-step plan be verified read-only (#70).
func (f *fakeOllama) markResident() {
	f.mu.Lock()
	f.loaded = true
	f.mu.Unlock()
}

func (f *fakeOllama) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadCalls
}

func (f *fakeOllama) close() { f.srv.Close() }

// fakeSwitcher records backend switches and, on restart, runs onRestart
// to simulate the new backend's effect (e.g. Vulkan engaging the GPU).
type fakeSwitcher struct {
	setEnvs   [][]string
	stops     int
	starts    int
	onRestart func()
	startErr  error
}

func (s *fakeSwitcher) SetBackendEnv(env []string) { s.setEnvs = append(s.setEnvs, env) }
func (s *fakeSwitcher) Stop(context.Context) error { s.stops++; return nil }
func (s *fakeSwitcher) EnsureRunning(context.Context) error {
	s.starts++
	if s.onRestart != nil {
		s.onRestart()
	}
	return s.startErr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func strixHaloPlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux", StrixHaloAPU: true})
}

// amdDiscretePlan is the 2-step [rocm, vulkan+igpu] plan a ROCm-capable
// discrete AMD card resolves to (#40/#68) — same probe shape as Strix
// Halo Linux, so discrete AMD also self-heals to Vulkan if ROCm is
// CPU-bound.
func amdDiscretePlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{
		GOOS: "linux", PrimaryGPUVendor: "amd", PrimaryGPUModel: "AMD Radeon RX 7900 XTX",
	})
}

// nvidiaPlan is the single-step [cuda] plan a detected NVIDIA card
// resolves to. It has no fallback, so its probe may only relabel.
func nvidiaPlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"})
}

// applePlan is darwin's single-step [metal] plan. gpu_apple_darwin
// reports Metal for every arm64 Mac with no usability check, so this is
// the plan waired-agent#35 is about — and it reaches the same probe as
// every other single-step plan rather than needing one of its own.
func applePlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "apple"})
}

func TestResolveBackendWithProbe_SingleStepUnreachableKeepsBackend(t *testing.T) {
	// Renamed from TestResolveBackendWithProbe_SingleStepNoProbe, which
	// asserted the same outcome for a reason that no longer holds: the
	// probe used to return before looking at anything. It now looks, and
	// an unreachable engine is inconclusive — which keeps the backend by
	// the file's "never make it worse" rule instead of by not asking.
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, nvidiaPlan(), "http://127.0.0.1:1", &http.Client{}, discardLogger())
	if got != infruntime.BackendCUDA {
		t.Errorf("backend = %q, want cuda", got)
	}
	if sw.stops != 0 || sw.starts != 0 {
		t.Errorf("single-step plan must not restart the engine: stops=%d starts=%d", sw.stops, sw.starts)
	}
}

// TestResolveBackendWithProbe_SingleStepCPUBoundIsRelabelled is the
// waired-agent#70 fix.
//
// PRODUCT CONTRACT — #70. A DETECTED GPU that fails to ENGAGE (broken
// driver runtime, VRAM already exhausted) used to keep its GPU label
// while inference ran on the CPU, which is precisely the silent fallback
// entry.Backend exists to make visible. There is no better backend to
// try on a single-step host, so the correction is the label alone.
func TestResolveBackendWithProbe_SingleStepCPUBoundIsRelabelled(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan infruntime.BackendPlan
		from infruntime.OllamaBackend
	}{
		{"cuda", nvidiaPlan(), infruntime.BackendCUDA},
		{"metal", applePlan(), infruntime.BackendMetal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOllama([]string{"a:q4"}, 0) // loaded, nothing in VRAM
			defer f.close()
			f.markResident()
			sw := &fakeSwitcher{}

			got := resolveBackendWithProbe(context.Background(), sw, tc.plan, f.srv.URL, &http.Client{}, discardLogger())
			if got != infruntime.BackendCPU {
				t.Errorf("backend = %q, want cpu: the model is CPU-resident and %q would be a lie",
					got, tc.from)
			}
			if sw.stops != 0 || sw.starts != 0 {
				t.Errorf("a single-step plan has nowhere to fall back to and must not restart: stops=%d starts=%d",
					sw.stops, sw.starts)
			}
		})
	}
}

func TestResolveBackendWithProbe_SingleStepEngagedKeepsItsBackend(t *testing.T) {
	f := newFakeOllama([]string{"a:q4"}, 800)
	defer f.close()
	f.markResident()
	sw := &fakeSwitcher{}

	got := resolveBackendWithProbe(context.Background(), sw, nvidiaPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendCUDA {
		t.Errorf("backend = %q, want cuda: the model is on the GPU", got)
	}
	if sw.starts != 0 {
		t.Errorf("no restart may follow a working backend: starts=%d", sw.starts)
	}
}

// TestResolveBackendWithProbe_SingleStepNeverForcesALoad pins the read-only
// half of the #70 change. A plan whose verdict can only relabel must not
// pay probeLoadTimeout for a cold load, and must not push a load into a
// window where the engine may be restarting under it — the engine is
// known to restart mid-measurement on its own.
func TestResolveBackendWithProbe_SingleStepNeverForcesALoad(t *testing.T) {
	f := newFakeOllama([]string{"a:q4"}, 0) // tags exist, nothing resident
	defer f.close()
	sw := &fakeSwitcher{}

	got := resolveBackendWithProbe(context.Background(), sw, nvidiaPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendCUDA {
		t.Errorf("backend = %q, want cuda: nothing was resident, so the reading is inconclusive", got)
	}
	if n := f.loads(); n != 0 {
		t.Errorf("a single-step probe loaded a model %d time(s); it may only read what is already there", n)
	}
	if sw.starts != 0 {
		t.Errorf("inconclusive must not restart: starts=%d", sw.starts)
	}
}

// TestResolveBackendWithProbe_MultiStepStillLoadsToDecide is the other
// side of that: where a restart CAN follow, the probe is still allowed to
// spend a load to get a verdict, because an unverified plan would leave a
// working GPU path untried. This pins that #70 did not quietly narrow the
// #290 behaviour.
func TestResolveBackendWithProbe_MultiStepStillLoadsToDecide(t *testing.T) {
	f := newFakeOllama([]string{"a:q4"}, 800) // nothing resident yet
	defer f.close()
	sw := &fakeSwitcher{}

	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendROCm {
		t.Errorf("backend = %q, want rocm", got)
	}
	if n := f.loads(); n == 0 {
		t.Error("a plan with a fallback must still load a model to reach a verdict")
	}
}

func TestResolveBackendWithProbe_CPUPlanIsNotProbed(t *testing.T) {
	// A host with no detected GPU already reports cpu. There is no claim
	// to verify, so the probe must not spend a request — the URL is
	// unreachable to prove none is made.
	plan := infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux"})
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, plan, "http://127.0.0.1:1", &http.Client{}, discardLogger())
	if got != infruntime.BackendCPU {
		t.Errorf("backend = %q, want cpu", got)
	}
}

// TestResolveBackendWithProbe_EmptyPlanDeclines covers the zero-value
// plan a provider built without a boot plan carries. Preferred() indexes
// Steps[0] unguarded, and the Probes() gate that used to stand at the
// call site was incidentally shielding it, so removing that gate made
// this reachable. "" is ResolvedBackend's own "not decided".
func TestResolveBackendWithProbe_EmptyPlanDeclines(t *testing.T) {
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, infruntime.BackendPlan{}, "http://127.0.0.1:1", &http.Client{}, discardLogger())
	if got != "" {
		t.Errorf("backend = %q, want \"\" for a plan with no steps", got)
	}
}

func TestResolveBackendWithProbe_ROCmEngages(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 800) // size_vram>0 => GPU
	defer f.close()
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendROCm {
		t.Errorf("backend = %q, want rocm (GPU engaged on first step)", got)
	}
	if sw.starts != 0 {
		t.Errorf("no fallback expected; starts=%d", sw.starts)
	}
}

func TestResolveBackendWithProbe_ROCmCPUBound_FallsBackToVulkan(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 0) // ROCm load is CPU-bound
	defer f.close()
	sw := &fakeSwitcher{}
	// Simulate Vulkan engaging the GPU after the restart.
	sw.onRestart = func() { f.setVRAM(900) }
	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendVulkan {
		t.Errorf("backend = %q, want vulkan (fell back after CPU-bound ROCm)", got)
	}
	if sw.stops != 1 || sw.starts != 1 {
		t.Errorf("expected exactly one fallback restart: stops=%d starts=%d", sw.stops, sw.starts)
	}
	if len(sw.setEnvs) != 1 || len(sw.setEnvs[0]) != 2 ||
		sw.setEnvs[0][0] != "OLLAMA_VULKAN=1" || sw.setEnvs[0][1] != "OLLAMA_IGPU_ENABLE=1" {
		t.Errorf("expected SetBackendEnv([OLLAMA_VULKAN=1 OLLAMA_IGPU_ENABLE=1]); got %v", sw.setEnvs)
	}
}

func TestResolveBackendWithProbe_BothCPUBound_ReturnsCPU(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 0) // stays CPU-bound across restarts
	defer f.close()
	sw := &fakeSwitcher{}
	sw.onRestart = func() { f.setVRAM(0) } // Vulkan also CPU-bound
	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendCPU {
		t.Errorf("backend = %q, want cpu (no backend engaged the GPU)", got)
	}
	if sw.starts != 1 {
		t.Errorf("expected one fallback attempt; starts=%d", sw.starts)
	}
}

func TestResolveBackendWithProbe_NoModel_KeepsPreferred(t *testing.T) {
	f := newFakeOllama(nil, 0) // no tags -> inconclusive
	defer f.close()
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendROCm {
		t.Errorf("backend = %q, want rocm (inconclusive must keep preferred, not fall back)", got)
	}
	if sw.stops != 0 || sw.starts != 0 {
		t.Errorf("inconclusive probe must not restart the engine: stops=%d starts=%d", sw.stops, sw.starts)
	}
}

func TestResolveBackendWithProbe_RestartError_KeepsPreferred(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 0) // CPU-bound -> wants fallback
	defer f.close()
	sw := &fakeSwitcher{startErr: fmt.Errorf("spawn failed")}
	got := resolveBackendWithProbe(context.Background(), sw, strixHaloPlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendROCm {
		t.Errorf("backend = %q, want rocm (restart failed -> keep current, don't claim vulkan)", got)
	}
}

func TestResolveBackendWithProbe_AMDDiscreteROCmEngages(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 800) // size_vram>0 => GPU
	defer f.close()
	sw := &fakeSwitcher{}
	got := resolveBackendWithProbe(context.Background(), sw, amdDiscretePlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendROCm {
		t.Errorf("backend = %q, want rocm (discrete AMD engaged ROCm on first step)", got)
	}
	if sw.starts != 0 {
		t.Errorf("no fallback expected; starts=%d", sw.starts)
	}
}

func TestResolveBackendWithProbe_AMDDiscreteROCmCPUBound_FallsBackToVulkan(t *testing.T) {
	f := newFakeOllama([]string{"qwen3:8b"}, 0) // ROCm load is CPU-bound
	defer f.close()
	sw := &fakeSwitcher{}
	sw.onRestart = func() { f.setVRAM(900) } // Vulkan engages the GPU after restart
	got := resolveBackendWithProbe(context.Background(), sw, amdDiscretePlan(), f.srv.URL, &http.Client{}, discardLogger())
	if got != infruntime.BackendVulkan {
		t.Errorf("backend = %q, want vulkan (fell back after CPU-bound ROCm)", got)
	}
	if sw.stops != 1 || sw.starts != 1 {
		t.Errorf("expected exactly one fallback restart: stops=%d starts=%d", sw.stops, sw.starts)
	}
	if len(sw.setEnvs) != 1 || len(sw.setEnvs[0]) != 2 ||
		sw.setEnvs[0][0] != "OLLAMA_VULKAN=1" || sw.setEnvs[0][1] != "OLLAMA_IGPU_ENABLE=1" {
		t.Errorf("expected SetBackendEnv([OLLAMA_VULKAN=1 OLLAMA_IGPU_ENABLE=1]); got %v", sw.setEnvs)
	}
}
