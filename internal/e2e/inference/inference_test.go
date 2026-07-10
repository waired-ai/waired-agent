//go:build e2e

// Inference e2e: spins up a real ollama subprocess, drives the
// production gateway / router / runtime adapter stack, and asserts
// that OpenAI- and Anthropic-shaped chat requests land back with
// content. Skips when ollama isn't installed.
//
// Run with:
//
//	go test -tags e2e -run TestInferenceGatewayE2E ./internal/e2e/inference/...
//
// The first run pulls the smallest practical Ollama model
// (qwen3:0.6b, ~500MB). Subsequent runs reuse the cached weights.
package inference_e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// E2eModelTag is the (small) Ollama tag the e2e pulls. Keep this in
// sync with the test's manifest below.
const e2eModelTag = "qwen2.5:0.5b"
const e2eModelID = "qwen2.5-0.5b"

func TestInferenceGatewayE2E(t *testing.T) {
	bin, err := exec.LookPath("ollama")
	if err != nil {
		t.Skipf("ollama not installed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	port := freeTCPPort(t)
	adapter := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary:         bin,
		Host:           "127.0.0.1",
		Port:           port,
		Spawner:        infruntime.DefaultSpawner{},
		HealthInterval: 500 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 60,
		StopTimeout:    5 * time.Second,
	})
	if err := adapter.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = adapter.Stop(stopCtx)
	})

	t.Logf("ollama listening on %s", adapter.BaseURL())

	// `ollama pull` is a separate CLI invocation; without OLLAMA_HOST
	// it talks to 11434 (which we deliberately aren't using). Point
	// it at the test's subprocess instead.
	t.Setenv("OLLAMA_HOST", fmt.Sprintf("127.0.0.1:%d", port))

	// Pull the tiny model. Cached after first run.
	puller := download.NewPuller(bin, download.DefaultRunner{})
	t.Logf("ensuring %s is pulled (may take a few minutes on first run)", e2eModelTag)
	if err := puller.Pull(ctx, e2eModelTag, func(p download.Progress) {
		if p.State == download.StatePulling && p.Percent%25 == 0 && p.Percent > 0 {
			t.Logf("  pull: %s %d%%", p.State, p.Percent)
		} else if p.State == download.StateVerifying || p.State == download.StateSuccess {
			t.Logf("  pull: %s", p.State)
		}
	}); err != nil {
		t.Fatalf("pull: %v", err)
	}

	manifest := catalog.Manifest{
		ModelID:       e2eModelID,
		ModelAliases:  []string{"waired/test"},
		ContextLength: 4096,
		Capabilities:  []string{"chat", "json_mode"},
		Variants: []catalog.Variant{{
			VariantID: "q4-gguf", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: "ollama", Tag: e2eModelTag},
		}},
	}
	state := catalog.State{
		Version: catalog.StateVersion,
		Models: map[string]catalog.ModelState{
			e2eModelID: {
				VariantID: "q4-gguf",
				OllamaTag: e2eModelTag,
				State:     catalog.ModelStateReady,
			},
		},
		Endpoints: map[string]catalog.EndpointState{},
	}

	registry := infruntime.NewRegistry()
	registry.Register(adapter)

	gwPort := freeTCPPort(t)
	selector := &fixedSelector{manifests: []catalog.Manifest{manifest}, state: state, registry: registry}
	gw := gateway.NewServer(gateway.ServerConfig{}, gateway.Deps{
		Selector:       selector,
		Runtimes:       registry,
		ListManifests:  func() []catalog.Manifest { return []catalog.Manifest{manifest} },
		HTTPClient:     &http.Client{Timeout: 5 * time.Minute},
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", gwPort))
	if err != nil {
		t.Fatalf("listen gateway: %v", err)
	}
	gwCtx, gwCancel := context.WithCancel(ctx)
	defer gwCancel()
	go func() {
		_ = gw.Serve(gwCtx, ln)
	}()
	gwBase := fmt.Sprintf("http://127.0.0.1:%d", gwPort)
	waitForGateway(t, gwBase)

	t.Run("OpenAI_ChatCompletions_NonStream", func(t *testing.T) {
		body := `{"model":"waired/test","stream":false,"messages":[{"role":"user","content":"Reply with a single word: hello"}]}`
		resp := postJSON(t, ctx, gwBase+"/v1/chat/completions", body)
		var got struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(resp, &got); err != nil {
			t.Fatalf("decode: %v body=%s", err, resp)
		}
		if len(got.Choices) == 0 || got.Choices[0].Message.Content == "" {
			t.Errorf("empty response: %s", resp)
		}
		t.Logf("openai response: %s", got.Choices[0].Message.Content)
	})

	t.Run("OpenAI_Models_ListsBundled", func(t *testing.T) {
		resp := getURL(t, ctx, gwBase+"/v1/models")
		if !strings.Contains(string(resp), e2eModelID) {
			t.Errorf("/v1/models missing %s: %s", e2eModelID, resp)
		}
	})

	t.Run("Anthropic_Messages_NonStream", func(t *testing.T) {
		body := `{"model":"waired/test","max_tokens":32,"messages":[{"role":"user","content":"Reply with a single word: hello"}]}`
		resp := postJSON(t, ctx, gwBase+"/anthropic/v1/messages", body)
		var got struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(resp, &got); err != nil {
			t.Fatalf("decode: %v body=%s", err, resp)
		}
		if got.Type != "message" {
			t.Errorf("type = %q, want message; body=%s", got.Type, resp)
		}
		if len(got.Content) == 0 || got.Content[0].Text == "" {
			t.Errorf("empty content: %s", resp)
		}
		t.Logf("anthropic response: %s (stop=%s)", got.Content[0].Text, got.StopReason)
	})

	t.Run("Anthropic_Messages_Stream", func(t *testing.T) {
		body := `{"model":"waired/test","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Reply with one word: hi"}]}`
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwBase+"/anthropic/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status=%d body=%s", resp.StatusCode, b)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		text := string(bodyBytes)
		for _, want := range []string{
			"event: message_start",
			"event: content_block_delta",
			"event: message_stop",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("stream missing %q\nbody=%s", want, text)
			}
		}
	})
}

// fixedSelector is a SelectorIface that always returns the e2e
// manifest's single endpoint via the production router (so the
// router is exercised, not bypassed).
type fixedSelector struct {
	manifests []catalog.Manifest
	state     catalog.State
	registry  *infruntime.Registry
}

func (f *fixedSelector) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 64}
	s := router.NewSelector(router.Inputs{
		Manifests: f.manifests, LocalState: f.state, Hardware: hw, Runtimes: f.registry,
	})
	return s.Select(ctx, req)
}

// SelectK completes the SelectorIface contract (gateway probe / fan-out
// paths call it). Like Select it delegates to the production router so the
// e2e exercises real selection; #556 added it after gateway.Deps.Selector
// grew SelectK, which had been breaking the `gpu`-tagged TestVLLMGatewayE2E
// build.
func (f *fixedSelector) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	hw := hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 64}
	s := router.NewSelector(router.Inputs{
		Manifests: f.manifests, LocalState: f.state, Hardware: hw, Runtimes: f.registry,
	})
	return s.SelectK(ctx, req, k)
}

// --- helpers ---

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForGateway(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/models")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway never came up at %s", base)
}

func postJSON(t *testing.T, ctx context.Context, url, body string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	return out
}

func getURL(t *testing.T, ctx context.Context, url string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	return out
}

// silence unused-import warnings if a helper is removed locally
var _ = os.Getenv
