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

// modelsWithWindow serves /v1/models from a gateway whose ContextWindowFor is
// the given function (nil for "the daemon cannot resolve one").
func modelsWithWindow(t *testing.T, windowFor func(string) int) map[string]int {
	t.Helper()
	gw := NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:         &fakeSelector{},
		Runtimes:         runtime.NewRegistry(),
		ListManifests:    asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:       http.DefaultClient,
		AllowOpenAI:      true,
		AllowAnthropic:   true,
		ContextWindowFor: windowFor,
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var got struct {
		Data []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string]int{}
	for _, m := range got.Data {
		out[m.ID] = m.MaxInputTokens
	}
	return out
}

// TestOpenAIModels_StampsTheServedWindow: the coding-agent plugins bake this
// number in at link time, so the OpenAI listing has to publish the window
// this host can actually serve — the same field and source the Anthropic
// listing already stamps (#1001).
func TestOpenAIModels_StampsTheServedWindow(t *testing.T) {
	got := modelsWithWindow(t, func(id string) int {
		if id == "waired/default" {
			return 200704
		}
		return 131072
	})
	if got["waired/default"] != 200704 {
		t.Errorf("waired/default max_input_tokens = %d, want 200704", got["waired/default"])
	}
	if got["qwen3-8b-instruct"] != 131072 {
		t.Errorf("qwen3-8b-instruct max_input_tokens = %d, want 131072", got["qwen3-8b-instruct"])
	}
}

// TestOpenAIModels_OmitsAnUnknownWindow: a reader must be able to tell "we do
// not know" from a real figure, so an unresolvable window is absent, not 0.
func TestOpenAIModels_OmitsAnUnknownWindow(t *testing.T) {
	for name, windowFor := range map[string]func(string) int{
		"no resolver": nil,
		"resolves 0":  func(string) int { return 0 },
	} {
		t.Run(name, func(t *testing.T) {
			got := modelsWithWindow(t, windowFor)
			if _, ok := got["waired/default"]; !ok {
				t.Fatal("waired/default missing from the listing entirely")
			}
			if got["waired/default"] != 0 {
				t.Errorf("max_input_tokens = %d, want the field absent", got["waired/default"])
			}
		})
	}
}

// TestOpenAIModels_UnknownWindowIsAbsentFromTheJSON is the same assertion one
// level down: `omitempty` has to actually keep the key out of the body, since
// a client reading `"max_input_tokens": 0` would take 0 for an answer.
func TestOpenAIModels_UnknownWindowIsAbsentFromTheJSON(t *testing.T) {
	gw := NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       &fakeSelector{},
		Runtimes:       runtime.NewRegistry(),
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	if body := w.Body.String(); strings.Contains(body, "max_input_tokens") {
		t.Errorf("body carries max_input_tokens with no resolver:\n%s", body)
	}
}
