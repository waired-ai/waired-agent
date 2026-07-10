package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// GPU-backend engagement probe (#290).
//
// ResolveOllamaBackend (internal/runtime) returns an ordered backend
// plan. For most hosts it has a single, unambiguous step. For Strix Halo
// on Linux it has two — ROCm (preferred) then Vulkan (fallback) — because
// Ollama's bundled ROCm runtime may silently fail to engage the gfx1151
// iGPU and fall back to CPU. This probe verifies the running backend
// actually placed a model on the GPU and, when it didn't, restarts the
// engine on the next backend so the host never *silently* runs on CPU
// when a working GPU path exists.
//
// Design constraint: it must never make things worse than the
// deterministic default. Every uncertain outcome (no model loaded,
// /api/ps unreachable, a restart that errors) keeps the current backend
// rather than risk a needless engine restart. Only positive evidence of
// CPU-only residency (a loaded model with size_vram == 0) triggers a
// fallback.

const (
	probeHTTPTimeout = 10 * time.Second // /api/tags, /api/ps
	probeLoadTimeout = 3 * time.Minute  // cold /api/generate model load
)

// backendSwitcher is the slice of *infruntime.OllamaAdapter the probe
// needs to relaunch the engine on a different GPU backend.
type backendSwitcher interface {
	SetBackendEnv([]string)
	Stop(context.Context) error
	EnsureRunning(context.Context) error
}

// gpuEngagement is the verdict of one /api/ps inspection.
type gpuEngagement struct {
	OnGPU   bool   // a loaded model reports size_vram > 0
	Checked bool   // a model was actually loaded, so the verdict is meaningful
	Detail  string // human-readable summary for logs
}

// resolveBackendWithProbe verifies that plan's preferred backend engaged
// the GPU and, for plans with a fallback step, switches the engine to the
// next backend when the current one is CPU-bound. It returns the backend
// the engine ended up on (informational; surfaced by the caller).
//
// Conservative by design (see file header): a single-step plan returns
// immediately without touching the engine, and any inconclusive check
// keeps the current backend.
func resolveBackendWithProbe(ctx context.Context, sw backendSwitcher, plan infruntime.BackendPlan, baseURL string, client *http.Client, logger *slog.Logger) infruntime.OllamaBackend {
	if !plan.Probes() {
		return plan.Preferred().Backend
	}
	for i, step := range plan.Steps {
		eng := ollamaEngagement(ctx, client, baseURL)
		switch {
		case !eng.Checked:
			logger.Warn("ollama GPU engagement unverified; keeping backend",
				"backend", step.Backend, "detail", eng.Detail)
			return step.Backend
		case eng.OnGPU:
			logger.Info("ollama GPU engaged", "backend", step.Backend, "detail", eng.Detail)
			return step.Backend
		}
		// Positive evidence the model is CPU-resident.
		if i == len(plan.Steps)-1 {
			logger.Warn("ollama still CPU-bound after exhausting GPU backends; running on CPU",
				"backend", step.Backend, "detail", eng.Detail)
			return infruntime.BackendCPU
		}
		next := plan.Steps[i+1]
		logger.Warn("ollama backend did not engage GPU; falling back",
			"from", step.Backend, "to", next.Backend, "detail", eng.Detail)
		sw.SetBackendEnv(next.Env)
		if err := sw.Stop(ctx); err != nil {
			logger.Warn("stop before backend fallback failed; keeping current backend",
				"backend", step.Backend, "err", err)
			return step.Backend
		}
		if err := sw.EnsureRunning(ctx); err != nil {
			logger.Warn("restart on fallback backend failed; engine down",
				"backend", next.Backend, "err", err)
			return step.Backend
		}
	}
	return plan.Preferred().Backend
}

// ollamaEngagement reports whether a model is currently resident on the
// GPU. It inspects /api/ps first; if nothing is loaded it loads the first
// available tag (POST /api/generate with model only) and re-inspects.
// Checked is false when no model could be loaded — the caller must treat
// that as "unknown" and NOT trigger a fallback.
func ollamaEngagement(ctx context.Context, client *http.Client, baseURL string) gpuEngagement {
	if eng, ok := psEngagement(ctx, client, baseURL); ok {
		return eng
	}
	tag, err := firstOllamaTag(ctx, client, baseURL)
	if err != nil || tag == "" {
		return gpuEngagement{Detail: "no model available to probe GPU engagement"}
	}
	if err := loadOllamaModel(ctx, client, baseURL, tag); err != nil {
		return gpuEngagement{Detail: fmt.Sprintf("probe model load failed: %v", err)}
	}
	if eng, ok := psEngagement(ctx, client, baseURL); ok {
		return eng
	}
	return gpuEngagement{Detail: "model not visible in /api/ps after load"}
}

type psResponse struct {
	Models []psModel `json:"models"`
}

// psModel is one loaded-model row of /api/ps. ContextLength (the context
// window the runner actually allocated) is present since well before the
// pinned 0.31 line — verified live against 0.31.1 — and is the primary
// signal for the #621 tuning verification; 0 means an engine that
// doesn't report it.
type psModel struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	SizeVRAM      int64  `json:"size_vram"`
	ContextLength int    `json:"context_length"`
}

// psEngagement inspects /api/ps. ok=false means no model is loaded (the
// caller should load one, or treat the result as unknown). When a model
// is loaded, size_vram > 0 on any model means the GPU is engaged
// (Ollama reports the bytes resident in VRAM; 0 is a pure-CPU load).
func psEngagement(ctx context.Context, client *http.Client, baseURL string) (gpuEngagement, bool) {
	var ps psResponse
	if err := getJSON(ctx, client, baseURL+"/api/ps", probeHTTPTimeout, &ps); err != nil {
		return gpuEngagement{Detail: fmt.Sprintf("/api/ps error: %v", err)}, false
	}
	if len(ps.Models) == 0 {
		return gpuEngagement{}, false
	}
	for _, m := range ps.Models {
		if m.SizeVRAM > 0 {
			return gpuEngagement{OnGPU: true, Checked: true,
				Detail: fmt.Sprintf("%s resident on GPU (size_vram=%d of %d)", m.Name, m.SizeVRAM, m.Size)}, true
		}
	}
	first := ps.Models[0]
	return gpuEngagement{OnGPU: false, Checked: true,
		Detail: fmt.Sprintf("%s CPU-resident (size_vram=0 of %d)", first.Name, first.Size)}, true
}

func firstOllamaTag(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	var tags ollamaTagsResponse
	if err := getJSON(ctx, client, baseURL+"/api/tags", probeHTTPTimeout, &tags); err != nil {
		return "", err
	}
	for _, m := range tags.Models {
		if m.Name != "" {
			return m.Name, nil
		}
	}
	return "", nil
}

// loadOllamaModel asks Ollama to load a model into memory without
// generating output (POST /api/generate with just "model"). Ollama
// resolves the placement (GPU vs CPU) during this load, which is what
// makes the subsequent /api/ps inspection meaningful.
func loadOllamaModel(ctx context.Context, client *http.Client, baseURL, tag string) error {
	body, _ := json.Marshal(map[string]any{"model": tag, "stream": false})
	cctx, cancel := context.WithTimeout(ctx, probeLoadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func getJSON(ctx context.Context, client *http.Client, url string, timeout time.Duration, v any) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
