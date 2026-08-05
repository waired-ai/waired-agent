package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// anthropicModel is the Anthropic Models API object, extended with
// max_input_tokens — the field Claude Code's gateway model discovery
// (CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1) reads to size its
// auto-compaction threshold (#623). We advertise the effective LOCAL
// window (min native / host-sustainable, from Deps.ContextWindowFor) so
// Claude Code compacts before it overruns the model and Ollama truncates
// the prompt head. Omitted (0) when the window is unknown.
type anthropicModel struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	MaxInputTokens int    `json:"max_input_tokens,omitempty"`
}

const anthropicModelsPrefix = "/anthropic/v1/models/"

// Reserved /model route-directive ids (#52), advertised in the Claude
// intercept's /v1/models discovery (gated by Deps.ClaudeModelDirectives) so
// they surface in Claude Code's /model picker — which filters discovered ids
// to ^(claude|anthropic); their display_name is free-form. Selecting one makes
// the intercept force this request's route, overriding the /waired-route
// policy. The intercept duplicates these literals to stay stdlib-only — keep
// both sides in sync (internal/proxy/intercept/model_rewrite.go).
const (
	// ModelWairedLocal pins the conversation to LOCAL inference (the intercept
	// forces route=waired). It deliberately does NOT start with "claude-", so
	// Claude Code applies the CLAUDE_CODE_MAX_CONTEXT_TOKENS managed-settings
	// value to it (that env is honoured only for non-"claude-" ids) — the
	// honest ~256k local window instead of Claude Code's 200k default.
	ModelWairedLocal = "anthropic-waired-local"
	// ModelWairedAuto and ModelWairedAuto1M pin the conversation to AUTO
	// routing (the intercept forces route=auto): Waired inference first,
	// falling back to the real Anthropic API when no node serves the tier.
	// On a fallback leg the intercept rewrites either id to a real model
	// (same as the cloud id).
	//
	// Both start with "claude-", so Claude Code sizes them from the id
	// string alone and never from CLAUDE_CODE_MAX_CONTEXT_TOKENS: the
	// bare id takes the 200k default, the "[1m]" suffix takes 1M. That
	// is the whole reason for the prefix (waired#1031). The env var is a
	// single global shared by EVERY non-"claude-" id, so while auto lived
	// there it could not carry a window different from the local pin's —
	// and after #408 pointed that value at this device's real window,
	// auto's Anthropic fallback leg ran in a session sized to whatever
	// engine happened to be installed here.
	//
	// A tier is a promise about the SERVING node, so the router only
	// selects an endpoint that declares it (RequiredWindowFor); when none
	// does, selection fails and auto's fallback carries the turn to
	// Anthropic, which is the honest answer rather than a local engine
	// pretending to a window it does not hold.
	ModelWairedAuto   = "claude-waired-auto"
	ModelWairedAuto1M = "claude-waired-auto[1m]"

	// ModelWairedAutoLegacy is the pre-waired#1031 spelling of
	// ModelWairedAuto. It is no longer advertised, and the intercept still
	// routes it: a Claude Code that selected it before an upgrade keeps
	// the id in its own settings until the operator picks again, and a
	// stale picker cache can hand it back for a whole session.
	ModelWairedAutoLegacy = "anthropic-waired-auto"
	// ModelWairedCloud pins the conversation to the real Anthropic API (the
	// intercept forces route=anthropic and rewrites this id to a real model on
	// passthrough). The "[1m]" suffix gives it Claude Code's 1M window.
	ModelWairedCloud = "claude-waired-cloud[1m]"
)

// handleAnthropicModels serves the Anthropic Models API locally so Claude
// Code — routed here by the intercept's /v1/models override (#623) —
// discovers the LOCAL catalog and, crucially, each model's effective
// context window rather than the real Anthropic 1M/200k metadata. It
// mirrors handleOpenAIModels' listing (the dynamic coding aliases plus
// every manifest id/alias, deduped) but in Anthropic's
// {data, has_more, first_id, last_id} envelope, and additionally stamps
// max_input_tokens from Deps.ContextWindowFor.
//
//   - GET /anthropic/v1/models        → the list
//   - GET /anthropic/v1/models/{id}   → a single model object
func (h *HandlerSet) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "GET only")
		return
	}

	models := h.anthropicModelList()
	slog.Debug("anthropic models request", "method", r.Method, "path", r.URL.Path, "count", len(models))

	// Single-object form: a non-empty id after the collection prefix.
	if id, ok := strings.CutPrefix(r.URL.Path, anthropicModelsPrefix); ok && id != "" {
		for _, m := range models {
			if m.ID == id {
				writeJSON(w, http.StatusOK, m)
				return
			}
		}
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q not found", id))
		return
	}

	out := map[string]any{"data": models, "has_more": false}
	if len(models) > 0 {
		out["first_id"] = models[0].ID
		out["last_id"] = models[len(models)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// anthropicModelList builds the deduped model list. The advertised window
// comes from Deps.ContextWindowFor, which resolves dynamic aliases and
// unknown claude-* ids to the device-active model (so waired/default and
// the claude-* ids Claude Code selects both carry the real local window).
func (h *HandlerSet) anthropicModelList() []anthropicModel {
	created := time.Now().UTC().Format(time.RFC3339)
	out := []anthropicModel{}
	seen := map[string]struct{}{}
	add := func(id, display string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		m := anthropicModel{Type: "model", ID: id, DisplayName: display, CreatedAt: created}
		if h.deps.ContextWindowFor != nil {
			m.MaxInputTokens = h.deps.ContextWindowFor(id)
		}
		out = append(out, m)
	}
	// #52: reserved route-directive ids first, so they are prominent in the
	// /model picker. Opt-in via agentconfig; only advertised on the Claude
	// intercept surface. add() stamps each with ContextWindowFor (harmless —
	// Claude Code sizes the window from the id string, not this field); the
	// honest local window comes from CLAUDE_CODE_MAX_CONTEXT_TOKENS instead.
	if h.deps.ClaudeModelDirectives {
		add(ModelWairedAuto, "Waired auto — 200k (local, fallback to Anthropic)")
		add(ModelWairedAuto1M, "Waired auto — 1M (local, fallback to Anthropic)")
		add(ModelWairedLocal, "Waired local (this device)")
		add(ModelWairedCloud, "Waired cloud (Anthropic API)")
	}
	for _, id := range router.DynamicAliases() {
		add(id, "")
	}
	for _, mf := range h.deps.ListManifests() {
		add(mf.ModelID, mf.DisplayName)
		for _, alias := range mf.ModelAliases {
			add(alias, mf.DisplayName)
		}
	}
	return out
}

// RequiredWindowFor is the input-token window a request for modelID
// obliges the serving endpoint to hold, or 0 when the id makes no such
// promise (waired#1031).
//
// Only the two auto tiers do. The local id routes to this device
// whatever its window is — that is what pinning means, and it is the
// only way to reach a device that declares no window at all. The cloud
// id never touches a Waired endpoint.
//
// The legacy auto spelling deliberately returns 0 rather than the 200k
// tier: it is a non-"claude-" id, so a client that still holds it is in
// a session sized by CLAUDE_CODE_MAX_CONTEXT_TOKENS, and holding its
// endpoint to a window its own session was never sized for would refuse
// turns that used to work.
func RequiredWindowFor(modelID string) int {
	switch modelID {
	case ModelWairedAuto:
		return hostfit.ServingWindow200k
	case ModelWairedAuto1M:
		return hostfit.ServingWindow1M
	default:
		return 0
	}
}
