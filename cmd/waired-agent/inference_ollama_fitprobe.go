// Post-load allocation probe for the Ollama serve tuning
// (waired-agent#1038).
//
// The /api/ps spill check is not enough to say a configuration works.
// size and size_vram cover the WEIGHTS only: the KV buffer and the
// generation compute buffer are additional VRAM that does not appear
// there, and the forced #642 ubatch is under-accounted by the engine's
// own allocator. On the reproduction host the load reported a tolerable
// spill and left 491 MB free, and the first prompt longer than about a
// thousand tokens came back "CUDA error: out of memory" and evicted the
// model — so the only honest question is whether the runner can actually
// allocate the working set a real prompt needs, and the only way to ask
// it is to make it do so.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ollamaFitProbeTimeout bounds the allocation probe. Generous: on a
// spilled discrete host the prefill it forces is genuinely slow, and a
// probe that timed out would be read as "no evidence" and let the broken
// configuration through.
const ollamaFitProbeTimeout = 90 * time.Second

// fitProbeWord is one token-ish unit of filler. The probe's prompt says
// nothing and asks for nothing: it exists to make the runner allocate,
// and a prompt with meaning would only tempt someone to read the reply.
const fitProbeWord = "alpha bravo charlie delta echo foxtrot golf hotel india juliet "

// fitProbePrompt builds a prompt of roughly promptTokens tokens.
//
// Roughly is enough. The measured cliff is a step function — a
// 914-token prompt served and a ~2,000-token one did not — so what
// matters is being comfortably past one forced ubatch, not hitting a
// count exactly.
func fitProbePrompt(promptTokens int) string {
	if promptTokens <= 0 {
		return ""
	}
	// ~1.4 tokens per word on this filler; 4 chars per token is the
	// long-standing rule of thumb and errs long, which is the safe side.
	want := promptTokens * 4
	var b strings.Builder
	b.Grow(want + len(fitProbeWord))
	for b.Len() < want {
		b.WriteString(fitProbeWord)
	}
	return b.String()
}

// probeOllamaAllocation sends one generation spanning several ubatches
// and returns the engine's error verbatim.
//
// A non-2xx body is returned as the error text rather than a status code
// so the caller can classify it with runtime.EngineOutOfMemory — the
// marker list has one home, and this probe is not the place to reimplement
// it. num_predict is 1: the question is whether the PREFILL allocates,
// and decoding further would only cost time.
func probeOllamaAllocation(ctx context.Context, client *http.Client, baseURL, tag string, promptTokens int) error {
	payload := map[string]any{
		"model":  tag,
		"prompt": fitProbePrompt(promptTokens),
		"stream": false,
		"options": map[string]any{
			"num_predict": 1,
			"temperature": 0,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, ollamaFitProbeTimeout)
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("engine returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return nil
}
