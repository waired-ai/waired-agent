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

// Rewritten for waired-agent#1492. The engagement check used to restart
// the engine on a fallback backend (ROCm, then Vulkan) when a model was
// CPU-resident; the engine does that itself since 0.30, and the plans no
// longer have steps. Removed with the restart: the fallback, restart-error
// and "multi-step may load to decide" tests. What the check still owes is
// #70's honest label, pinned below for every backend a plan can name.

// fakeOllama is an httptest server mimicking the subset of the Ollama API
// the check reads. sizeVRAM controls /api/ps placement.
type fakeOllama struct {
	mu        sync.Mutex
	srv       *httptest.Server
	tags      []string
	sizeVRAM  int64 // size_vram reported for the loaded model
	loaded    bool  // whether a model is resident
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

// markResident makes /api/ps answer without a POST /api/generate first,
// which is the state a real host is in by the time the boot path runs
// this check: warmServingModelNow has already loaded the serving model.
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// strixHaloPlan is the one plan with an override: Windows Strix Halo on
// Vulkan.
func strixHaloPlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{
		GOOS: "windows", PrimaryGPUVendor: "amd", StrixHaloAPU: true, AMDGPU: true,
	})
}

// amdPlan leaves ROCm-or-Vulkan to the engine.
func amdPlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "amd", AMDGPU: true})
}

func nvidiaPlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux", PrimaryGPUVendor: "nvidia"})
}

// applePlan is darwin's plan. gpu_apple_darwin reports Metal for every
// arm64 Mac with no usability check, so this is the plan waired-agent#35
// is about.
func applePlan() infruntime.BackendPlan {
	return infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "darwin", PrimaryGPUVendor: "apple"})
}

func TestVerifyBackendEngaged_UnreachableKeepsBackend(t *testing.T) {
	got := verifyBackendEngaged(context.Background(), nvidiaPlan(), "http://127.0.0.1:1", &http.Client{}, discardLogger())
	if got != infruntime.BackendCUDA {
		t.Errorf("backend = %q, want cuda: an unreachable engine is inconclusive", got)
	}
}

// PRODUCT CONTRACT — #70. A DETECTED GPU that fails to ENGAGE (broken
// driver runtime, VRAM already exhausted) used to keep its GPU label
// while inference ran on the CPU, which is precisely the silent fallback
// the label exists to make visible.
func TestVerifyBackendEngaged_CPUBoundIsRelabelled(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan infruntime.BackendPlan
	}{
		{"cuda", nvidiaPlan()},
		{"metal", applePlan()},
		{"vulkan (windows strix halo)", strixHaloPlan()},
		{"auto (amd)", amdPlan()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOllama([]string{"a:q4"}, 0) // loaded, nothing in VRAM
			defer f.close()
			f.markResident()
			got := verifyBackendEngaged(context.Background(), tc.plan, f.srv.URL, &http.Client{}, discardLogger())
			if got != infruntime.BackendCPU {
				t.Errorf("backend = %q, want cpu: the model is CPU-resident and %q would be a lie",
					got, tc.plan.Backend)
			}
		})
	}
}

func TestVerifyBackendEngaged_EngagedKeepsItsBackend(t *testing.T) {
	f := newFakeOllama([]string{"a:q4"}, 800)
	defer f.close()
	f.markResident()
	for _, plan := range []infruntime.BackendPlan{nvidiaPlan(), strixHaloPlan(), amdPlan()} {
		if got := verifyBackendEngaged(context.Background(), plan, f.srv.URL, &http.Client{}, discardLogger()); got != plan.Backend {
			t.Errorf("backend = %q, want %q: the model is on the GPU", got, plan.Backend)
		}
	}
}

// The check is read-only for every plan now: it must not pay a cold load
// to reach a verdict, nor push one into a window where the engine may be
// restarting under it.
func TestVerifyBackendEngaged_NeverForcesALoad(t *testing.T) {
	f := newFakeOllama([]string{"a:q4"}, 0) // tags exist, nothing resident
	defer f.close()
	for _, plan := range []infruntime.BackendPlan{nvidiaPlan(), strixHaloPlan(), amdPlan(), applePlan()} {
		if got := verifyBackendEngaged(context.Background(), plan, f.srv.URL, &http.Client{}, discardLogger()); got != plan.Backend {
			t.Errorf("backend = %q, want %q: nothing was resident, so the reading is inconclusive", got, plan.Backend)
		}
	}
	if n := f.loads(); n != 0 {
		t.Errorf("the check loaded a model %d time(s); it may only read what is already there", n)
	}
}

// A host with no GPU in use already reports cpu. There is no claim to
// verify, so no request is made — the URL is unreachable to prove it.
func TestVerifyBackendEngaged_CPUPlanIsNotChecked(t *testing.T) {
	plan := infruntime.ResolveOllamaBackend(infruntime.BackendInputs{GOOS: "linux"})
	if got := verifyBackendEngaged(context.Background(), plan, "http://127.0.0.1:1", &http.Client{}, discardLogger()); got != infruntime.BackendCPU {
		t.Errorf("backend = %q, want cpu", got)
	}
}

// The zero-value plan a provider built without a boot plan carries. ""
// is ResolvedBackend's own "not decided".
func TestVerifyBackendEngaged_EmptyPlanDeclines(t *testing.T) {
	if got := verifyBackendEngaged(context.Background(), infruntime.BackendPlan{}, "http://127.0.0.1:1", &http.Client{}, discardLogger()); got != "" {
		t.Errorf("backend = %q, want \"\" for an empty plan", got)
	}
}
