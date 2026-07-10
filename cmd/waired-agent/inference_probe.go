package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// inferenceProbeDeps bundles the collaborators the local probe loop
// needs. Constructed once in main and shared by both the immediate
// initial probe and the ticker-driven loop.
type inferenceProbeDeps struct {
	StateWriter *state.Writer
	Aggregator  *inferencemesh.Aggregator // optional; nil = no diagnose snapshot
	PushClient  *controlclient.Client     // optional; nil = no CP push
	DeviceID    string
	MachineKey  ed25519.PrivateKey

	// EngineKind selects which probe runs each tick. Accepted values:
	// signer.InferenceTypeOllama, signer.InferenceTypeVLLM. Empty
	// string or signer.InferenceTypeNone short-circuits the loop
	// (same effect as Disabled=true) so no spurious "reachable=false
	// ollama" gets pushed when a vLLM-on-GPU host is up.
	EngineKind string
	// EnginePort is the loopback port the EngineKind subprocess
	// listens on. Mapped from cfg.Inference.ResolvedOllamaPort() or
	// cfg.Inference.VLLMPort at wiring time. 0 disables the probe.
	EnginePort int

	// IsShared, when non-nil and returning false, skips the
	// PushInferenceStatus call so mesh peers stop seeing this engine
	// in their inference-mesh snapshot. Local probe / state writer /
	// aggregator updates continue unchanged so the on-host diagnose
	// view and the wrapper's local reachability axis still reflect
	// reality. Nil means "always push" (Phase 4 default).
	IsShared func() bool

	// --- Phase 7 routing inputs --------------------------------------
	//
	// Hardware is the static GPU/RAM summary the agent broadcasts so
	// peers can render "peer X: RTX 4090, 64 GB" in the tray. Read
	// once at boot; never mutates over the agent's lifetime. nil for
	// pre-Phase-7 deployments (the field is omitempty on the wire).
	Hardware *signer.HardwareSummary

	// Capacity is the concurrent-request admission cap the Phase 7
	// Selector enforces against. Derived at boot from the local
	// token/s benchmark. 0 means "unlimited" (= explicit semantics for
	// external openai-compat endpoints + backward compat).
	Capacity int

	// RecommendedMaxParallel, when non-nil, returns the engine's current
	// VRAM-safe parallelism ceiling (from the applied ollama tuning). Reported
	// as advisory telemetry so the admin Device detail page can show it and warn
	// before an operator sets a higher concurrency. Read live each probe tick so
	// it tracks a re-tune; 0 (or nil) omits the field from the push.
	RecommendedMaxParallel func() int

	// ActiveTag is the engine-side tag for the agent's Active variant
	// (Ollama /api/tags name, or vLLM /v1/models id). Empty when no
	// Active selection is set (fresh agent, pre-model-pull). When
	// non-empty, runLocalInferenceProbe enforces the "1 agent =
	// 1 model" invariant by narrowing the published Models list to
	// just this tag; surplus tags pulled locally are stripped from
	// the network-map advertisement (the engine itself still serves
	// them — this only affects what peers see).
	ActiveTag string

	Disabled bool
	Logger   *slog.Logger
}

// runLocalInferenceProbe is the agent-side feeder for the
// InferenceReachableLocal flag the `waired claude` wrapper consults,
// AND for Phase 3's mesh-aggregation push:
//
//   - probes the local engine (ollama on /api/tags or vLLM on /health
//   - /v1/models, selected by EngineKind) at HeartbeatInterval
//   - writes the boolean result into runtime/state (for the wrapper
//     hot path)
//   - feeds the full InferenceState into the in-memory aggregator
//     (for diagnose / tray / mgmt API consumers)
//   - pushes the same InferenceState to the Control Plane so peers
//     see it via their network map (Phase 3)
//
// Disabled=true OR EngineKind in {"", "none"} OR EnginePort==0 pin
// the runtime/state flag to false and skip both aggregator updates
// and CP pushes — the device is intentionally engine-less, so peers
// see no entry rather than a misleading reachable=false ping.
func runLocalInferenceProbe(ctx context.Context, deps inferenceProbeDeps) {
	if deps.StateWriter == nil {
		return
	}
	if deps.Disabled || deps.EnginePort == 0 || !engineKindProbable(deps.EngineKind) {
		_ = deps.StateWriter.SetInferenceReachableLocal(false)
		_ = deps.StateWriter.SetInferenceReachableInMesh(false)
		if deps.Aggregator != nil {
			deps.Aggregator.UpdateLocal(nil)
		}
		return
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", deps.EnginePort)
	probe := func() signer.InferenceState {
		switch deps.EngineKind {
		case signer.InferenceTypeVLLM:
			return probeLocalVLLM(ctx, baseURL, time.Second)
		default:
			return probeLocalOllama(ctx, baseURL, time.Second)
		}
	}

	// lastSurplusSig dedups the "surplus locally-pulled models" warning
	// across probe ticks (every state.HeartbeatInterval). Without dedup,
	// an operator with two ollama tags pulled would see the warning
	// every 15 s. The signature is the sorted union of {surplus tags,
	// mismatch indicator}; it changes only when the underlying state
	// changes.
	var lastSurplusSig string

	tick := func() {
		s := probe()
		narrowPublishedModels(&s, deps.ActiveTag, &lastSurplusSig, deps.Logger)
		// Phase 7: decorate the probe result with Hardware and Capacity.
		// Both are baked in once at boot and omitempty on the wire so a
		// zero-value agent (no hardware probe, no benchmark) still
		// produces a compact push.
		if deps.Hardware != nil {
			s.Hardware = deps.Hardware
		}
		if deps.Capacity != 0 {
			s.Capacity = deps.Capacity
		}
		if deps.RecommendedMaxParallel != nil {
			if n := deps.RecommendedMaxParallel(); n > 0 {
				s.RecommendedMaxParallel = n
			}
		}
		if err := deps.StateWriter.SetInferenceReachableLocal(s.Reachable); err != nil && deps.Logger != nil {
			deps.Logger.Warn("inference reachability write failed", "err", err)
		}
		if deps.Aggregator != nil {
			deps.Aggregator.UpdateLocal(&s)
			// Phase 4: also publish the peers-only mesh aggregate to
			// runtime/state so the wrapper's Stage 1 gate can OR in
			// the mesh axis without crossing a process boundary.
			snap := deps.Aggregator.Snapshot()
			if err := deps.StateWriter.SetInferenceReachableInMesh(snap.Reachable); err != nil && deps.Logger != nil {
				deps.Logger.Warn("inference mesh reachability write failed", "err", err)
			}
		}
		if deps.PushClient != nil && deps.DeviceID != "" && len(deps.MachineKey) == ed25519.PrivateKeySize {
			if deps.IsShared != nil && !deps.IsShared() {
				if deps.Logger != nil {
					deps.Logger.Debug("inference status push skipped: share disabled")
				}
			} else {
				pushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := deps.PushClient.PushInferenceStatus(pushCtx, deps.DeviceID, s, deps.MachineKey)
				cancel()
				if err != nil && deps.Logger != nil && !errors.Is(err, context.Canceled) {
					deps.Logger.Warn("inference status push failed", "err", err)
				}
			}
		}
	}

	tick()

	t := time.NewTicker(state.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// engineKindProbable reports whether the EngineKind is one this
// package knows how to probe. signer.InferenceTypeNone is excluded
// deliberately so a "no engine" host short-circuits to the disabled
// branch rather than running an unused ollama probe against port 0.
func engineKindProbable(kind string) bool {
	switch kind {
	case signer.InferenceTypeOllama, signer.InferenceTypeVLLM:
		return true
	}
	return false
}

// ollamaTagsResponse is the relevant subset of /api/tags. We only
// pull names; sizes / modified timestamps would bloat the network
// map without serving any current consumer.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
		// Size is the on-disk blob size; the #621 tuning verification
		// uses it as the weight baseline for its KV-size heuristic.
		Size int64 `json:"size"`
	} `json:"models"`
}

// probeLocalOllama issues a single GET /api/tags against the local
// engine. On a 2xx response it parses the model list (best-effort —
// a non-JSON body still counts as reachable but yields nil Models).
// Any 2xx-4xx HTTP status counts as "reachable" — the precise model
// list does not matter for the wrapper gate, only the fact that
// something is listening and answering HTTP. Network errors, dials,
// and timeouts all map to reachable=false with the error captured
// in LastError.
func probeLocalOllama(parent context.Context, baseURL string, timeout time.Duration) signer.InferenceState {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := signer.InferenceState{
		Type:      signer.InferenceTypeOllama,
		Endpoint:  baseURL,
		LastCheck: now,
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		out.LastError = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return out
	}
	out.Reachable = true
	if resp.StatusCode == http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if err == nil {
			var tags ollamaTagsResponse
			if json.Unmarshal(body, &tags) == nil {
				for _, m := range tags.Models {
					if m.Name != "" {
						out.Models = append(out.Models, m.Name)
					}
				}
			}
		}
	}
	return out
}

// openAIModelsResponse is the relevant subset of vLLM's (and any
// OpenAI-compatible server's) /v1/models payload — only the ids
// are kept; vLLM populates `owned_by` and `object` but they bloat
// the network map without any consumer.
type openAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// probeLocalVLLM issues GET /health for the alive verdict and (on
// success) GET /v1/models to discover served names. Reachable
// follows the /health result alone — vLLM flips /health green once
// the model is loaded and ready, so a 2xx there is enough to call
// the engine usable. /v1/models is best-effort: a failure leaves
// Models nil but does not flip Reachable back to false. Failure of
// /health (dial error / 5xx) maps to Reachable=false with LastError
// populated, matching the ollama probe shape so the rest of the
// pipeline does not branch on engine kind.
func probeLocalVLLM(parent context.Context, baseURL string, timeout time.Duration) signer.InferenceState {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := signer.InferenceState{
		Type:      signer.InferenceTypeVLLM,
		Endpoint:  baseURL,
		LastCheck: now,
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Step 1: /health — vLLM's authoritative readiness signal.
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	hresp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode < 200 || hresp.StatusCode >= 300 {
		out.LastError = fmt.Sprintf("HTTP %d on /health", hresp.StatusCode)
		return out
	}
	out.Reachable = true

	// Step 2: /v1/models — best-effort enumeration. vLLM always returns
	// at least one entry (the --served-model-name) when /health is
	// green, but defensively tolerate missing / malformed bodies.
	mreq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return out
	}
	mresp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		return out
	}
	defer mresp.Body.Close()
	if mresp.StatusCode < 200 || mresp.StatusCode >= 300 {
		return out
	}
	body, err := io.ReadAll(io.LimitReader(mresp.Body, 64*1024))
	if err != nil {
		return out
	}
	var models openAIModelsResponse
	if err := json.Unmarshal(body, &models); err != nil {
		return out
	}
	for _, m := range models.Data {
		if m.ID != "" {
			out.Models = append(out.Models, m.ID)
		}
	}
	return out
}

// narrowPublishedModels enforces the "1 agent = 1 model" invariant on
// the InferenceState the agent broadcasts to peers. When activeTag is
// non-empty the published Models list is forced to a single-element
// {activeTag} regardless of what the engine reported; this prevents
// operator misconfiguration (extra `ollama pull` runs leaving surplus
// tags around) from leaking into Selector's candidate set as variants
// the agent isn't actually serving.
//
// When activeTag is empty (fresh agent before any model pull) the
// probe result passes through unmodified — the Selector falls back
// to its pre-Phase-7 behaviour.
//
// Side-effect: emits a warn on the supplied logger when the engine's
// reported tags don't match the Active selection. The dedup key
// (*lastSurplusSig) suppresses the same warning across consecutive
// probe ticks (every state.HeartbeatInterval, otherwise noisy).
// Pass a fresh `var sig string` for tests; pass a closure-scoped one
// from runLocalInferenceProbe for production.
func narrowPublishedModels(s *signer.InferenceState, activeTag string, lastSurplusSig *string, logger *slog.Logger) {
	if s == nil {
		return
	}
	if activeTag == "" {
		// Nothing to enforce — let the probe result through unmodified.
		// Reset the dedup so a subsequent transition back to "Active set"
		// re-emits the first warn.
		if lastSurplusSig != nil {
			*lastSurplusSig = ""
		}
		return
	}

	reported := s.Models
	matched := false
	surplus := make([]string, 0, len(reported))
	for _, m := range reported {
		switch m {
		case "":
			continue
		case activeTag:
			matched = true
		default:
			surplus = append(surplus, m)
		}
	}
	sort.Strings(surplus)

	// Compose the dedup signature: surplus tags + whether activeTag was
	// served + whether the engine reported anything at all. Three
	// distinct misconfiguration shapes (surplus only, active-missing,
	// engine-empty) should each emit their own warn/info, and the
	// signature must differ from the zero value so the first tick
	// actually fires.
	sig := strings.Join(surplus, ",")
	switch {
	case !matched && len(reported) == 0:
		sig = "empty"
	case !matched && len(reported) > 0:
		sig = "missing|" + sig
	case matched && len(surplus) > 0:
		sig = "surplus|" + sig
	default:
		// matched + no surplus is the steady state; no warn to dedup.
		sig = ""
	}

	if logger != nil && lastSurplusSig != nil && sig != *lastSurplusSig {
		switch {
		case len(surplus) > 0 && matched:
			logger.Warn("agent has surplus locally-pulled models; design is 1 agent = 1 model",
				"active_tag", activeTag,
				"surplus", surplus,
				"hint", "remove the extras (e.g. 'ollama rm <tag>') so peers see a consistent advertisement")
		case len(surplus) > 0 && !matched:
			logger.Warn("active tag not served by engine; engine reports different tags",
				"active_tag", activeTag,
				"engine_reports", surplus,
				"hint", "engine state has diverged from the agent's Active selection — restart or re-pull")
		case len(reported) == 0:
			logger.Info("engine has not yet reported active tag; publishing optimistically",
				"active_tag", activeTag)
		}
	}
	if lastSurplusSig != nil {
		*lastSurplusSig = sig
	}

	// Final invariant: publish only the active tag, regardless of what
	// the engine returned. Optimistic for the loading case (engine
	// reports nothing yet); defensive for the misconfigured case.
	s.Models = []string{activeTag}
}
