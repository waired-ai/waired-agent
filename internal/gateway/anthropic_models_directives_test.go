package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// discoveryModels drives GET /anthropic/v1/models against a HandlerSet with the
// #52 directives flag on/off and returns the advertised models keyed by id.
func discoveryModels(t *testing.T, directives bool) map[string]anthropicModel {
	t.Helper()
	manifests := []catalog.Manifest{{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B"}}
	h := NewHandlerSet(Deps{
		Runtimes:              runtime.NewRegistry(),
		ListManifests:         asManifestList(manifests),
		AllowAnthropic:        true,
		ContextWindowFor:      func(string) int { return 262144 },
		ClaudeModelDirectives: directives,
		HTTPClient:            http.DefaultClient,
	})
	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /anthropic/v1/models = %d, body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Data []anthropicModel `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make(map[string]anthropicModel, len(env.Data))
	for _, m := range env.Data {
		out[m.ID] = m
	}
	return out
}

// TestAnthropicModels_DirectivesGatedByFlag: the reserved route-directive ids
// appear in discovery only when the opt-in is on.
func TestAnthropicModels_DirectivesGatedByFlag(t *testing.T) {
	off := discoveryModels(t, false)
	for _, id := range []string{ModelWairedLocal, ModelWairedCloud} {
		if _, ok := off[id]; ok {
			t.Errorf("reserved id %q must be absent from discovery when the feature is off", id)
		}
	}

	on := discoveryModels(t, true)
	for _, id := range []string{ModelWairedLocal, ModelWairedCloud} {
		m, ok := on[id]
		if !ok {
			t.Errorf("reserved id %q must be advertised when the feature is on", id)
			continue
		}
		if m.DisplayName == "" {
			t.Errorf("reserved id %q must carry a display name for the /model picker", id)
		}
		// Load-bearing: Claude Code filters discovered ids to ^(claude|anthropic).
		// A reserved id that stops matching would silently vanish from the picker.
		if !strings.HasPrefix(id, "claude") && !strings.HasPrefix(id, "anthropic") {
			t.Errorf("reserved id %q must start with claude/anthropic to survive Claude Code's picker filter", id)
		}
	}

	// The manifest model is unaffected in both cases.
	if _, ok := on["qwen3-8b-instruct"]; !ok {
		t.Error("manifest models must still be advertised alongside the directives")
	}
}
