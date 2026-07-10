package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// BenchResult captures the outcome of one boot-time token/s probe.
// Capacity is the Phase 7 admission cap the agent will advertise to
// the mesh.
type BenchResult struct {
	TokensPerSec float64
	Capacity     int
	VariantID    string
	Failed       bool
	Err          string
}

// avgCodingAgentTokRate is the rough steady-state token throughput
// one coding-agent session consumes (claude / codex /
// continue.dev-style). Used as the divisor in N = floor(tokps / 30):
// a host that benches at ~120 tok/s ends up advertising Capacity=4.
//
// 30 is conservative — real coding-agent traffic spikes higher
// during code generation but stalls during tool use, so the
// effective sustained rate sits below the wall-clock token/s the
// benchmark measures. Easier to bump this up in a follow-up than
// to silently over-admit and flood a single peer.
//
// This is deliberately NOT the interactive/selection floor (#670,
// router.CodingAgentSelectionFloorTokps = 100): the divisor models
// how much throughput one admitted session CONSUMES on average, the
// floor models the wall-clock decode rate below which a session FEELS
// too slow. They used to share this constant when the floor was also
// 30; raising the floor to 100 must not cut every host's advertised
// mesh Capacity by 3.3×.
const avgCodingAgentTokRate = 30.0

// resolveInteractiveFloor returns the throughput (tokens/sec) below
// which the agent recommends a lighter model (issue #133). A
// configured value > 0 wins; 0 (the default) falls back to the
// coding-agent selection floor (#670): decode below ~100 tok/s at
// shallow context degrades to well under ~80 tok/s at the ~200k
// coding window, which real coding-agent use showed is too slow.
func resolveInteractiveFloor(cfg float64) float64 {
	if cfg > 0 {
		return cfg
	}
	return router.CodingAgentSelectionFloorTokps
}

// benchPromptCompletionTokens is the target completion length the
// benchmark requests. 200 tokens is long enough to cover the first
// few decoder iterations (where most overhead lives), short enough
// to keep the boot path under ~10 s on a midrange GPU.
const benchPromptCompletionTokens = 200

// benchTimeout caps the timed measurement request only — the warm-up
// that precedes it absorbs model-load latency under its own deadline.
// CUDA OOM, network errors, or a misbehaving engine should not block
// agent startup — RunBootBenchmark logs and returns Capacity=1
// (= serialise) on timeout so the agent comes up degraded rather than
// not at all.
const benchTimeout = 30 * time.Second

// benchWarmupCompletionTokens is the tiny completion the warm-up
// requests — just enough to force the engine to fully load the model
// before the timed window opens.
const benchWarmupCompletionTokens = 8

// benchWarmupTimeout bounds the untimed warm-up request. Generous: a
// 17–62 GB model cold-loading from disk takes tens of seconds, and
// that load used to land INSIDE the measured window — a host that
// decodes at ~100 tok/s warm read as ~5 tok/s cold and got a bogus
// lighter-model recommendation (observed live on sv-mag, 2026-06-09).
const benchWarmupTimeout = 180 * time.Second

// benchPrompt is the boilerplate user message the benchmark sends.
// Kept generic so the chosen model can complete it regardless of
// fine-tuning bias; keep under 100 tokens so the prompt processing
// stage doesn't dominate the wall-clock measurement.
const benchPrompt = "Briefly describe what a Linux process is, in one short paragraph."

// BenchDeps lists everything RunBootBenchmark touches. Passed in
// (rather than read from globals) so unit tests can inject a
// fake engine / clock / engine kind.
type BenchDeps struct {
	// EngineKind is the runtime's wire kind (signer.InferenceTypeOllama
	// / signer.InferenceTypeVLLM / signer.InferenceTypeNone). The
	// benchmark skips entirely for "none" or anything else — external
	// openai-compat is also skipped so we don't burn the operator's
	// token budget.
	EngineKind string

	// EnginePort is the loopback port the engine listens on. 0
	// short-circuits the benchmark (same effect as the probe loop's
	// skip).
	EnginePort int

	// VariantID is the catalog variant the engine is configured to
	// serve. Recorded on the result for traceability; the benchmark
	// does NOT use it to pick what to send — the engine answers
	// whatever it has loaded.
	VariantID string

	// EngineModel is the engine-native model name (Ollama tag or
	// vLLM /v1/models id). The benchmark inserts this verbatim into
	// the chat-completions request body.
	EngineModel string

	// Phase 7 follow-up (C2): cache key inputs. When all four are
	// populated AND Cache is non-nil, RunBootBenchmark consults the
	// on-disk cache before measuring and persists successful
	// measurements after. Empty GPUModel or VariantSHA disables
	// caching (CPU-only host or unknown variant — both would produce
	// un-discriminating keys across machines).
	GPUModel      string
	VRAMTotalMB   int
	DriverVersion string
	VariantSHA    string

	// Cache, when non-nil, is consulted before measuring and updated
	// after a successful measurement. Failed measurements
	// (Failed=true) are NEVER persisted so transient OOM / engine
	// warmup blips don't stick. nil = caching disabled.
	Cache *benchCache

	// Now defaults to time.Now if nil. Test injection.
	Now func() time.Time

	// HTTPClient defaults to http.DefaultClient if nil. Test injection.
	HTTPClient *http.Client

	Logger *slog.Logger
}

// RunBootBenchmark issues one token/s benchmark against the local
// engine and returns the derived Capacity. Failures (engine
// unreachable, malformed response, timeout) are warn-logged and
// returned as Capacity=1 (single-stream) so the agent still comes
// up — the alternative (refuse to start) would hide the typical
// "engine still warming up" race in installer flows.
//
// Skipped paths return Capacity=0 ("unlimited") with Failed=false:
//
//   - EngineKind == "none" / ""        — no engine to bench
//   - EngineKind == "openai-compat"    — external endpoint, the
//     upstream does its own rate limit
//   - EnginePort == 0                  — engine intentionally off
//
// The Capacity=0 backward-compat value is the right encoding for
// "no admission cap" — the receiver-side capacityGate skips itself
// at Capacity=0 and the sender-side InFlightTracker permits any
// in-flight count.
func RunBootBenchmark(ctx context.Context, deps BenchDeps) BenchResult {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	// Skip paths: no engine, external endpoint, or engine off.
	if deps.EnginePort == 0 ||
		deps.EngineKind == "" ||
		deps.EngineKind == signer.InferenceTypeNone {
		return BenchResult{Capacity: 0}
	}
	// Ollama and vLLM both expose an OpenAI-compatible
	// /v1/chat/completions surface; the benchmark talks to it
	// directly rather than going through the agent's own gateway
	// (avoids a self-loop at boot, before the loopback listener
	// is up).
	switch deps.EngineKind {
	case signer.InferenceTypeOllama, signer.InferenceTypeVLLM:
		// supported
	default:
		// openai-compat or any unknown kind: skip.
		return BenchResult{Capacity: 0, VariantID: deps.VariantID}
	}

	// Phase 7 follow-up (C2): consult the on-disk cache before
	// burning ~5-30 s on a measurement. The key embeds the host's
	// GPU + driver + the variant's content digest, so a cache hit
	// implies "we already measured this exact (machine, variant,
	// engine) combination once".
	cacheKey := benchCacheKey(deps)
	if cacheKey != "" && deps.Cache != nil {
		if cached, measuredAt, hit, err := deps.Cache.Load(cacheKey); err != nil {
			deps.Logger.Warn("inference boot benchmark: cache load failed; will measure",
				"err", err)
		} else if hit {
			deps.Logger.Info("inference boot benchmark: cache hit",
				"key", cacheKey,
				"capacity", cached.Capacity,
				"tokens_per_sec", cached.TokensPerSec,
				"measured_at", measuredAt.UTC().Format(time.RFC3339),
				"age", deps.Now().Sub(measuredAt).Truncate(time.Second).String())
			return cached
		} else {
			deps.Logger.Info("inference boot benchmark: cache miss; measuring",
				"key", cacheKey)
		}
	}

	// Warm-up: one tiny untimed completion so the engine loads the
	// model OUTSIDE the measured window. Without it a cold multi-GB
	// load dominated the elapsed time and the host read as an order of
	// magnitude slower than its real decode rate.
	if err := warmUpEngine(ctx, deps); err != nil {
		return failBench(deps, "warmup", err)
	}

	bctx, cancel := context.WithTimeout(ctx, benchTimeout)
	defer cancel()

	req, err := benchChatRequest(bctx, deps, benchPromptCompletionTokens)
	if err != nil {
		return failBench(deps, "build_request", err)
	}

	start := deps.Now()
	resp, err := deps.HTTPClient.Do(req)
	elapsed := deps.Now().Sub(start)
	if err != nil {
		// Distinguish timeout (context deadline) from other errors
		// in the log line so operators can tell "model loading too
		// slow" from "engine not listening".
		if errors.Is(bctx.Err(), context.DeadlineExceeded) {
			return failBench(deps, "timeout", err)
		}
		return failBench(deps, "dial_or_send", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return failBench(deps, "http_status", fmt.Errorf("HTTP %d", resp.StatusCode))
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return failBench(deps, "read_response", err)
	}
	tokens, err := extractCompletionTokens(respBody)
	if err != nil {
		return failBench(deps, "parse_completion_tokens", err)
	}
	if tokens <= 0 {
		return failBench(deps, "zero_tokens", fmt.Errorf("response reported %d completion tokens", tokens))
	}
	if elapsed <= 0 {
		// Clock that doesn't move — only happens with broken Now
		// injection. Treat as a benchmark failure so the test
		// surface doesn't paper over a real wiring bug.
		return failBench(deps, "zero_elapsed", fmt.Errorf("benchmark elapsed time was %v", elapsed))
	}
	tokps := float64(tokens) / elapsed.Seconds()
	cap := int(tokps / avgCodingAgentTokRate)
	if cap < 1 {
		cap = 1
	}
	deps.Logger.Info("inference boot benchmark completed",
		"engine_kind", deps.EngineKind,
		"variant", deps.VariantID,
		"engine_model", deps.EngineModel,
		"tokens", tokens,
		"elapsed_ms", elapsed.Milliseconds(),
		"tokens_per_sec", tokps,
		"capacity", cap)
	result := BenchResult{
		TokensPerSec: tokps,
		Capacity:     cap,
		VariantID:    deps.VariantID,
	}
	// Phase 7 follow-up (C2): persist only successful measurements.
	// failBench paths return above without reaching this point so
	// transient OOM / engine warmup blips never become sticky.
	if cacheKey != "" && deps.Cache != nil {
		meta := benchCacheHumanMeta{
			VariantID:     deps.VariantID,
			GPUModel:      deps.GPUModel,
			VRAMTotalMB:   deps.VRAMTotalMB,
			DriverVersion: deps.DriverVersion,
			EngineKind:    deps.EngineKind,
			EngineModel:   deps.EngineModel,
		}
		if err := deps.Cache.Store(cacheKey, result, meta, deps.Now()); err != nil {
			deps.Logger.Warn("inference boot benchmark: cache store failed",
				"key", cacheKey, "err", err)
		} else {
			deps.Logger.Info("inference boot benchmark: cache stored",
				"key", cacheKey, "capacity", cap)
		}
	}
	return result
}

// benchChatRequest builds the OpenAI-compatible chat-completions
// request both the warm-up and the timed measurement send; only the
// completion budget differs.
func benchChatRequest(ctx context.Context, deps BenchDeps, maxTokens int) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{
		"model":      deps.EngineModel,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": benchPrompt},
		},
		"stream": false,
	})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", deps.EnginePort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// warmUpEngine issues one tiny untimed completion so the engine loads
// the model outside the measured window. Failure is treated as a
// benchmark failure by the caller: a host that cannot serve 8 tokens
// within benchWarmupTimeout will not produce a usable measurement
// either.
func warmUpEngine(ctx context.Context, deps BenchDeps) error {
	wctx, cancel := context.WithTimeout(ctx, benchWarmupTimeout)
	defer cancel()
	req, err := benchChatRequest(wctx, deps, benchWarmupCompletionTokens)
	if err != nil {
		return err
	}
	resp, err := deps.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the keep-alive connection is immediately reusable for
	// the timed request.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// failBench logs a warning and returns Capacity=1 so the agent
// continues with a single-stream admission rather than refusing to
// start. Reason is a short slug for log filtering.
func failBench(deps BenchDeps, reason string, err error) BenchResult {
	deps.Logger.Warn("inference boot benchmark failed; falling back to Capacity=1",
		"reason", reason,
		"err", err)
	return BenchResult{
		Capacity:  1,
		VariantID: deps.VariantID,
		Failed:    true,
		Err:       err.Error(),
	}
}

// extractCompletionTokens reads the OpenAI-compatible response
// envelope and pulls out usage.completion_tokens. Ollama mirrors
// this shape since v0.5 and vLLM does so by spec. Falls back to
// counting tokens from the message content (whitespace-split) when
// the engine omits usage — a degraded but non-fatal accuracy hit.
func extractCompletionTokens(body []byte) (int, error) {
	var env struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, err
	}
	if env.Usage.CompletionTokens > 0 {
		return env.Usage.CompletionTokens, nil
	}
	if len(env.Choices) == 0 {
		return 0, errors.New("response has no choices and no usage")
	}
	// Whitespace-based fallback. Off by ~10% vs the real tokeniser
	// but adequate for tok/s on the order-of-magnitude scale the
	// admission cap consumes.
	content := env.Choices[0].Message.Content
	if content == "" {
		return 0, errors.New("choices[0].message.content is empty")
	}
	tokens := 1 // start at 1 to capture the leading non-space chunk
	for _, c := range content {
		if c == ' ' || c == '\n' || c == '\t' {
			tokens++
		}
	}
	return tokens, nil
}
