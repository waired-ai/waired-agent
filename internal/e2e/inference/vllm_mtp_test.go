//go:build e2e && gpu

package inference_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestVLLMSpeculativeMTP (waired-ai/waired#1432): a catalog build with
// MTP layers, served the way the daemon serves it — the speculative
// config from router.VLLMSpeculative and the window from
// router.VLLMMaxModelLenFor with the draft priced in — must start, hold
// a KV pool that clears the window, stay on Model Runner V2, actually
// draft, and still return a structured tool call.
//
// The smallest Qwen3.5 build keeps the lane inside its budget. When the
// catalog chose no draft for it (the decode gain did not clear the bar
// on that size), the lane drafts one token anyway: what is under test is
// that the product's config and sizing start an engine, not the choice.
//
// Run with: make e2e-vllm-mtp
func TestVLLMSpeculativeMTP(t *testing.T) {
	requireNVIDIAGPU(t)
	venvPath := requireVLLMVenv(t)
	hw := nvidiaProfile(t)

	const modelID, variantID, util = "qwen3.5-0.8b", "bf16", router.DefaultVLLMGPUMemoryUtilization
	m, v := bundledVariant(t, modelID, variantID)
	if v.MTPLayers <= 0 || v.MTPKVBytesPerTokenFP16 <= 0 {
		t.Fatalf("%s/%s carries no MTP facts (mtp_layers=%d, mtp_kv_bytes_per_token_fp16=%d); the lane has nothing to serve",
			modelID, variantID, v.MTPLayers, v.MTPKVBytesPerTokenFP16)
	}
	if v.MTPDraftTokens <= 0 {
		v.MTPDraftTokens = 1
	}
	spec := router.VLLMSpeculative(v, false, false, true)
	if spec.Method != "mtp" {
		t.Fatalf("router.VLLMSpeculative chose %+v for an MTP build", spec)
	}
	kvDType, kvFactor := "", scoring.KVFactorF16
	if router.VLLMUsesFP8KV(hw) {
		kvDType, kvFactor = "fp8", scoring.KVFactorFP8
	}
	window := router.VLLMMaxModelLenFor(v, spec.DraftTokens(), 1, util, kvFactor, hw)
	if window <= 0 {
		t.Fatalf("router.VLLMMaxModelLenFor returned %d on %s", window, gpuCaps(hw))
	}
	if m.ContextLength > 0 && window > m.ContextLength {
		window = m.ContextLength
	}
	t.Logf("serving %s with %s at max_model_len=%d (kv %q)", v.Source.RepoID, spec.Config, window, kvDType)

	requireGPUIdle(t)
	res := runVLLMSmokeOpts(t, venvPath, v.Source.RepoID, modelID, vllmSmokeOpts{
		maxModelLen:               window,
		gpuMemUtil:                util,
		kvCacheDType:              kvDType,
		speculativeConfig:         spec.Config,
		enablePromptTokensDetails: true,
		maxNumBatchedTokens:       router.VLLMMaxNumBatchedTokens(window, hw, 0),
		toolCallParser:            "qwen3_xml",
		benchTokens:               256,
		startBudget:               15 * time.Minute,
		whileServing: func(t *testing.T, port int) {
			drafts := specMetric(t, port, "vllm:spec_decode_num_drafts_total")
			accepted := specMetric(t, port, "vllm:spec_decode_num_accepted_tokens_total")
			if drafts <= 0 {
				t.Errorf("vllm:spec_decode_num_drafts_total = %v after a 256-token generation; the draft never ran", drafts)
			} else {
				t.Logf("mean acceptance length %.2f (%v drafts, %v accepted tokens)", 1+accepted/drafts, drafts, accepted)
			}
			assertStructuredToolCall(t, port, modelID)
		},
	})

	if pool := parseKVCapacity(t, res.logDir); pool < window {
		t.Errorf("KV pool %d tokens does not clear max_model_len %d: VLLMMaxModelLenFor underprices the draft", pool, window)
	} else {
		t.Logf("calibration: max_model_len=%d, KV pool=%d tokens (headroom %.1f%%)",
			window, pool, 100*float64(pool-window)/float64(window))
	}
	raw, err := os.ReadFile(filepath.Join(res.logDir, "engine.log"))
	if err != nil {
		t.Fatalf("engine.log: %v", err)
	}
	// ngram costs Model Runner V2 on 0.29.0 and MTP must not. The worker
	// logs "Using V2 Model Runner" when it runs it, and the config names
	// the fallback when it does not (vllm/v1/worker/gpu_worker.py,
	// vllm/config/vllm.py at 0.29.0).
	last := infruntime.LastEngineLogSpawn(string(raw))
	if fb := regexp.MustCompile(`Model Runner V2 does not yet support[^\n]*`).FindString(last); fb != "" {
		t.Errorf("the engine fell back from Model Runner V2: %s", fb)
	} else if !strings.Contains(last, "Using V2 Model Runner") {
		t.Errorf("engine.log does not say the V2 model runner ran; the log wording may have changed")
	}
}

// bundledVariant finds one build in the catalog this binary ships.
func bundledVariant(t *testing.T, modelID, variantID string) (catalog.Manifest, catalog.Variant) {
	t.Helper()
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatalf("bundled manifests: %v", err)
	}
	for _, m := range ms {
		if m.ModelID != modelID {
			continue
		}
		for _, v := range m.Variants {
			if v.VariantID == variantID {
				return m, v
			}
		}
	}
	t.Fatalf("%s/%s is not in the bundled catalog", modelID, variantID)
	return catalog.Manifest{}, catalog.Variant{}
}

// specMetric sums one Prometheus counter across its label sets.
func specMetric(t *testing.T, port int, name string) float64 {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var sum float64
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `(?:\{[^}]*\})?\s+([0-9.eE+-]+)$`)
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		f, _ := strconv.ParseFloat(m[1], 64)
		sum += f
	}
	return sum
}

// assertStructuredToolCall asks for one tool call and requires it back in
// tool_calls, not as text: a draft the target rejects must not change what
// the parser sees.
func assertStructuredToolCall(t *testing.T, port int, model string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"max_tokens":  400,
		"messages":    []map[string]string{{"role": "user", "content": "Read the file src/main.py using the tool."}},
		"tools": []map[string]any{{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "Read a file",
			"parameters": map[string]any{"type": "object",
				"properties": map[string]any{"path": map[string]string{"type": "string"}},
				"required":   []string{"path"}},
		}}},
		"tool_choice":          "auto",
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tool call request: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("tool call response: status %d, decode err %v", resp.StatusCode, err)
	}
	if len(out.Choices) == 0 || len(out.Choices[0].Message.ToolCalls) == 0 ||
		out.Choices[0].Message.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("no structured read_file call; got %+v", out.Choices)
	}
}
