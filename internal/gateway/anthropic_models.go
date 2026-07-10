package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
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
	for _, id := range router.DynamicCodingAliases {
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
