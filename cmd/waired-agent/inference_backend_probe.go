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

// GPU-backend engagement check (#290, #70).
//
// ResolveOllamaBackend (internal/runtime) returns a plan whose Backend is
// a LABEL for what the engine is expected to run on. This check reads
// /api/ps once and corrects the label to "cpu" when the model it finds is
// CPU-resident, so a GPU host that silently runs on the CPU says so in the
// doctor and the inference status — a broken CUDA runtime, VRAM already
// exhausted.
//
// It used to do more: for a plan with a fallback (ROCm, then Vulkan) it
// restarted the engine on the next backend (#290). Ollama falls back by
// itself since 0.30 — a device ROCm cannot serve is served through Vulkan,
// which is on by default — so the restart duplicated the engine and is
// gone, with the multi-step plans that fed it (waired-agent#1492). What
// remains is read-only: it never loads a model and never restarts
// anything, so it cannot make a host worse than the engine left it.

const (
	probeHTTPTimeout = 10 * time.Second // /api/tags, /api/ps
	probeLoadTimeout = 3 * time.Minute  // cold /api/generate model load
)

// gpuEngagement is the verdict of one /api/ps inspection.
type gpuEngagement struct {
	OnGPU   bool   // a loaded model reports size_vram > 0
	Checked bool   // a model was actually loaded, so the verdict is meaningful
	Detail  string // human-readable summary for logs
}

// verifyBackendEngaged returns the label the engine has earned: the
// plan's own, or "cpu" on positive evidence that the resident model is
// CPU-only. Every inconclusive read — nothing loaded yet, /api/ps
// unreachable — keeps the plan's label. "" (a provider built without a
// boot plan) is returned as "", which the caller reads as "not decided".
func verifyBackendEngaged(ctx context.Context, plan infruntime.BackendPlan, baseURL string, client *http.Client, logger *slog.Logger) infruntime.OllamaBackend {
	if plan.Backend == "" || plan.Backend == infruntime.BackendCPU {
		return plan.Backend
	}
	eng, ok := psEngagement(ctx, client, baseURL)
	switch {
	case !ok || !eng.Checked:
		detail := eng.Detail
		if detail == "" {
			detail = "no model resident to read GPU engagement from"
		}
		logger.Warn("ollama GPU engagement unverified; keeping backend",
			"backend", plan.Backend, "detail", detail)
		return plan.Backend
	case eng.OnGPU:
		logger.Info("ollama GPU engaged", "backend", plan.Backend, "detail", eng.Detail)
		return plan.Backend
	default:
		logger.Warn("ollama did not engage the GPU; reporting CPU",
			"backend", plan.Backend, "detail", eng.Detail)
		return infruntime.BackendCPU
	}
}

type psResponse struct {
	Models []psModel `json:"models"`
}

// psModel is one loaded-model row of /api/ps. ContextLength (the context
// window the runner actually allocated) is present since well before the
// 0.31 line — verified live against 0.31.1, and the pin has only moved
// forward since (0.32.13 as of #823) — and is the primary
// signal for the #621 tuning verification; 0 means an engine that
// doesn't report it.
type psModel struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	SizeVRAM      int64  `json:"size_vram"`
	ContextLength int    `json:"context_length"`
	// ExpiresAt is when the engine intends to unload this model (#879).
	// RFC3339 with a numeric offset. An indefinite keep-alive is not a
	// sentinel here — Ollama renders it as a date centuries out — so a
	// far-future value is a normal answer, not a parse failure.
	ExpiresAt string `json:"expires_at"`
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
//
// keepAlive, when non-empty, is sent as the request's keep_alive so the
// load does not depend on the serve-level OLLAMA_KEEP_ALIVE. The probe
// callers pass "" — they want the engine's own policy, whatever it is,
// because they are measuring the engine rather than configuring it.
func loadOllamaModel(ctx context.Context, client *http.Client, baseURL, tag, keepAlive string) error {
	payload := map[string]any{"model": tag, "stream": false}
	if keepAlive != "" {
		payload["keep_alive"] = keepAlive
	}
	body, _ := json.Marshal(payload)
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
