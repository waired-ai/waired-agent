// Locally derived Ollama model carrying a forced generation ubatch (#642).
//
// The pinned Ollama (0.31.1) exposes no serve-level env for num_batch, and
// its automatic batch sizing falls back to 512 on hosts where the model
// spills — precisely the intentional-spill #624 configuration where a
// 2048 ubatch measured a +38–44 % prefill gain at the 200k coding floor
// (docs/reports/20260705-num-batch-512-vs-2048-24gb.md). The only per-host
// delivery that merges consistently into every request (rather than
// thrash-reloading the runner against Ollama's automatic-batch traffic) is
// a locally derived model created via /api/create with PARAMETER num_batch
// baked in. It is manifest-only: the weight blobs are shared with the base
// tag, so creation is cheap and adds no disk. num_ctx is untouched — it
// stays on the server-global OLLAMA_CONTEXT_LENGTH env, so the derived
// model carries only the batch override. The gateway then routes to the
// derived tag because it becomes the model's OllamaTag (see inference.go).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ollamaDerivedBatchModelTimeout bounds the /api/create call. Creation is
// a metadata operation (blobs shared), so it is fast, but a cold engine
// that is still loading can make the first call wait.
const ollamaDerivedBatchModelTimeout = 60e9 // 60s as time.Duration ns

// ollamaDerivedTag returns the local model name for a base tag with a
// forced ubatch: "<base>-wb<batch>" ("wb" = waired batch). Deterministic,
// so re-creating each boot is idempotent. Returns "" for invalid input.
func ollamaDerivedTag(baseTag string, numBatch int) string {
	if baseTag == "" || numBatch <= 0 {
		return ""
	}
	return fmt.Sprintf("%s-wb%d", baseTag, numBatch)
}

// ensureOllamaDerivedModel (idempotently) creates a local model derived
// FROM baseTag that bakes PARAMETER num_batch, and returns its tag. The
// base model must already be pulled — /api/create FROM an absent model
// fails, in which case the error is returned so the caller falls back to
// the base tag with Ollama's automatic batch sizing. Re-running with the
// same base picks up a freshly re-pulled base's blobs, so calling it every
// boot keeps the derived model current.
func ensureOllamaDerivedModel(ctx context.Context, client *http.Client, baseURL, baseTag string, numBatch int) (string, error) {
	derived := ollamaDerivedTag(baseTag, numBatch)
	if derived == "" {
		return "", fmt.Errorf("invalid derived-model inputs (base=%q batch=%d)", baseTag, numBatch)
	}
	body, err := json.Marshal(map[string]any{
		"model":      derived,
		"from":       baseTag,
		"parameters": map[string]any{"num_batch": numBatch},
		"stream":     false,
	})
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, ollamaDerivedBatchModelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, baseURL+"/api/create", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused; capture a snippet for errors.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("/api/create returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return derived, nil
}
