package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// oomBody is the verbatim reply measured on sv-mag (RTX PRO 4000
// Blackwell, ollama 0.32.13) serving qwen3.8:27b-mtp-q4_K_M-wb2048 for a
// ~2,000-token prompt, 2026-08-27 (waired-agent#1038).
const oomBody = `{"error":"an error was encountered while running the model: CUDA error\nCUDA error: out of memory"}`

func TestEngineOOMBody(t *testing.T) {
	if !engineOOMBody([]byte(oomBody)) {
		t.Error("the measured CUDA OOM body was not recognised")
	}
	for _, b := range []string{
		`{"error":"something went wrong"}`,
		`{"error":"model runner has unexpectedly stopped"}`,
		`{"error":{"message":"system message must be at the beginning","type":"api_error"}}`,
	} {
		if engineOOMBody([]byte(b)) {
			t.Errorf("false positive on %q", b)
		}
	}
	// A dead-runner body must reach engineDeadBody first, and the OOM
	// list must not claim it.
	if engineOOMBody([]byte(`{"error":"llama-server process has terminated"}`)) {
		t.Error("a dead-runner body must not read as an accelerator OOM")
	}
}

// TestEngineOutOfMemory_NamesTheVendor pins that the classification does
// not stop at NVIDIA (waired-agent#1058). ggml prints this line as
// GGML_CUDA_NAME " error: %s", and GGML_CUDA_NAME is "ROCm" in a HIP
// build — so a CUDA-only marker list would have made the depth
// benchmark's new out-of-memory verdict inert on every AMD host.
//
// A record of upstream's wording, not of a run: there is no discrete AMD
// host with ROCm in this fleet or in CI, so the ROCm case is unverified
// on hardware and says so here as well as at the marker list.
func TestEngineOutOfMemory_NamesTheVendor(t *testing.T) {
	rocm := `{"error":"an error was encountered while running the model: ROCm error\nROCm error: out of memory"}`
	if !EngineOutOfMemory(rocm) {
		t.Error("a ROCm out-of-memory was not recognised")
	}
	if !EngineOutOfMemory(oomBody) {
		t.Error("the measured CUDA out-of-memory was not recognised")
	}
	// No Moore Threads host exists here, so MUSA is deliberately absent.
	if EngineOutOfMemory(`MUSA error: out of memory`) {
		t.Error("MUSA was added without a host or an issue to justify it")
	}
}

// TestOllamaAdapter_OOMFiresFitFailureNotUnhealthy.
//
// PRODUCT CONTRACT — docs/decisions is silent, so this cites the issue:
// waired-agent#1038. An accelerator out-of-memory is a fact about the
// CONFIGURATION, not about engine health. Demoting the engine would
// restart it into the same configuration and fail the same way; the
// dead-runner contract pinned by TestOllamaAdapter_ReportUpstreamFailure
// stays exactly as it was.
func TestOllamaAdapter_OOMFiresFitFailureNotUnhealthy(t *testing.T) {
	a, _, rec := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	var mu sync.Mutex
	var fits []string
	a.SetOnFitFailure(func(detail string) {
		mu.Lock()
		fits = append(fits, detail)
		mu.Unlock()
	})

	a.ReportUpstreamFailure(500, []byte(oomBody))

	waitFor(t, time.Second, "OnFitFailure to fire", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(fits) == 1
	})
	if got := a.Health(context.Background()).State; got != StateReady {
		t.Errorf("state = %s, want ready — an OOM must not demote the engine", got)
	}
	if rec.count() != 0 {
		t.Errorf("OnUnhealthy fired %d times, want 0", rec.count())
	}
	mu.Lock()
	detail := fits[0]
	mu.Unlock()
	if !strings.Contains(detail, "out of memory") {
		t.Errorf("detail = %q, want the engine's own sentence", detail)
	}
}

// TestOllamaAdapter_OOMBurstIsOneReport: the OOM kills the runner and
// evicts the model, so the next request pays a cold reload and fails the
// same way. A burst is one fact about one configuration.
func TestOllamaAdapter_OOMBurstIsOneReport(t *testing.T) {
	a, _, _ := livenessAdapter(t, t.TempDir())
	defer func() { _ = a.Stop(context.Background()) }()

	var mu sync.Mutex
	n := 0
	a.SetOnFitFailure(func(string) {
		mu.Lock()
		n++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.ReportUpstreamFailure(500, []byte(oomBody))
		}()
	}
	wg.Wait()
	waitFor(t, time.Second, "OnFitFailure to fire", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return n >= 1
	})
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := n
	mu.Unlock()
	if got != 1 {
		t.Errorf("OnFitFailure fired %d times for one configuration, want exactly 1", got)
	}
}
