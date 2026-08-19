// Locally derived Ollama model carrying the runner parameters the engine
// exposes no serve-level environment variable for (#642, waired-ai/waired#762).
//
// Two parameters need this vehicle today:
//
//   - num_batch (#642). Ollama exposes no serve-level env for it, and its
//     automatic batch sizing falls back to 512 on hosts where the model spills
//     — precisely the intentional-spill #624 configuration where a 2048 ubatch
//     measured a +38-44 % prefill gain at the 200k coding floor
//     (docs/reports/20260705-num-batch-512-vs-2048-24gb.md).
//   - use_mmap (waired-ai/waired#762). On unified-memory hosts the GGUF mapping
//     is charged to the small OS-visible RAM half even though the weights are
//     GPU-resident in the firmware carve-out, so a model the GPU pool holds
//     comfortably can still pin free RAM at zero and page-thrash the machine.
//
// The only per-host delivery that merges consistently into every request
// (rather than thrash-reloading the runner against traffic that omits the
// option) is a locally derived model created via /api/create with the
// parameters baked in. It is manifest-only: the weight blobs are shared with
// the base tag, so creation is cheap and adds no disk. num_ctx is untouched —
// it stays on the server-global OLLAMA_CONTEXT_LENGTH env, so the derived model
// carries only the parameters that have nowhere else to go. The gateway then
// routes to the derived tag because it becomes the model's OllamaTag (see
// inference.go).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ollamaDerivedBatchModelTimeout bounds the /api/create call. Creation is
// a metadata operation (blobs shared), so it is fast, but a cold engine
// that is still loading can make the first call wait.
const ollamaDerivedBatchModelTimeout = 60e9 // 60s as time.Duration ns

// ollamaDerivedParams is the parameter set baked into a derived model.
// The zero value means "no derived model is needed"; serve the base tag.
type ollamaDerivedParams struct {
	// NumBatch > 0 forces the generation ubatch (#642).
	NumBatch int
	// NoMmap true reads the weights instead of mapping them (waired#762).
	NoMmap bool
}

// needed reports whether any parameter asks for a derived model at all.
func (p ollamaDerivedParams) needed() bool { return p.NumBatch > 0 || p.NoMmap }

// suffix is the tag fragment encoding the parameter set. The order is fixed
// so the tag is deterministic across boots, and each parameter contributes a
// fragment only when it is set — which is what keeps a batch-only host on the
// exact "-wb<batch>" tag it already has.
func (p ollamaDerivedParams) suffix() string {
	var b strings.Builder
	if p.NumBatch > 0 {
		fmt.Fprintf(&b, "-wb%d", p.NumBatch)
	}
	if p.NoMmap {
		b.WriteString("-nommap")
	}
	return b.String()
}

// apiParameters renders the set as the /api/create "parameters" object.
func (p ollamaDerivedParams) apiParameters() map[string]any {
	m := make(map[string]any, 2)
	if p.NumBatch > 0 {
		m["num_batch"] = p.NumBatch
	}
	if p.NoMmap {
		m["use_mmap"] = false
	}
	return m
}

// ollamaDerivedTag returns the local model name for a base tag carrying
// params: "<base>-wb<batch>" ("wb" = waired batch), "-nommap" appended when
// the mapping is off. Deterministic, so re-creating each boot is idempotent.
// Returns "" for invalid input (no base tag, or nothing to bake).
func ollamaDerivedTag(baseTag string, params ollamaDerivedParams) string {
	if baseTag == "" || !params.needed() {
		return ""
	}
	return baseTag + params.suffix()
}

// ensureOllamaDerivedModel (idempotently) creates a local model derived
// FROM baseTag that bakes params, and returns its tag. The base model must
// already be pulled — /api/create FROM an absent model fails, in which case
// the error is returned so the caller falls back to the base tag with
// Ollama's own defaults. Re-running with the same base picks up a freshly
// re-pulled base's blobs, so calling it every boot keeps the derived model
// current.
func ensureOllamaDerivedModel(ctx context.Context, client *http.Client, baseURL, baseTag string, params ollamaDerivedParams) (string, error) {
	derived := ollamaDerivedTag(baseTag, params)
	if derived == "" {
		return "", fmt.Errorf("invalid derived-model inputs (base=%q params=%+v)", baseTag, params)
	}
	body, err := json.Marshal(map[string]any{
		"model":      derived,
		"from":       baseTag,
		"parameters": params.apiParameters(),
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
