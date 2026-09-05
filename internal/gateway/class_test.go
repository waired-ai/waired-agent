package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// Traffic-class plumbing (#645): the class is derived from the request's
// headers (waired-agent#1186 — it used to read a model id waired had pinned
// as every subagent's model), reaches the resolver, and namespaces the sticky
// id.
//
// The header is the one Claude Code stamps on a request from an agent it
// spawned inside the session, and only on such a request. Reading it here
// rather than the model id also means a subagent whose definition pins its
// own model is classified correctly — the label never was.

func classifyingGateway(t *testing.T, sel SelectorIface, adapterURL string, resolverClasses *[]string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	return NewServer(ServerConfig{}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList(nil),
		HTTPClient:     http.DefaultClient,
		AllowAnthropic: true,
		ClassifyRequest: func(h http.Header) string {
			if h.Get("X-Claude-Code-Agent-Id") != "" {
				return "sub"
			}
			return "main"
		},
		ResolveUnknownModel: func(_, class string) (string, bool) {
			if resolverClasses != nil {
				*resolverClasses = append(*resolverClasses, class)
			}
			return "qwen3-8b-instruct", true
		},
	})
}

func postMessages(t *testing.T, gw *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	return postMessagesAs(t, gw, model, "")
}

// postMessagesAs sends a turn as a subagent when agentID is non-empty, the
// way Claude Code marks one.
func postMessagesAs(t *testing.T, gw *Server, model, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	if agentID != "" {
		r.Header.Set("X-Claude-Code-Agent-Id", agentID)
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func localSelection(modelID, engineModel string) router.Selection {
	return router.Selection{
		ModelID:       modelID,
		VariantID:     "q4-gguf",
		Runtime:       "ollama",
		EngineModel:   engineModel,
		ExecutionMode: "local",
	}
}

func TestAnthropicMessages_ClassReachesResolverAndSelector(t *testing.T) {
	engine := fakeOllama(t, nil)
	defer engine.Close()

	sel := &modelAwareSelector{known: map[string]router.Selection{
		"qwen3-8b-instruct": localSelection("qwen3-8b-instruct", "qwen3:8b-q4_K_M"),
	}}
	var classes []string
	gw := classifyingGateway(t, sel, engine.URL, &classes)

	// An id the selector does not know, so ResolveUnknownModel runs and the
	// class it was called with is observable. The header is what makes this
	// one a subagent's turn.
	if w := postMessagesAs(t, gw, "claude-opus-4-8", "a1"); w.Code != http.StatusOK {
		t.Fatalf("sub request status = %d body=%s", w.Code, w.Body.String())
	}
	if w := postMessages(t, gw, "claude-fable-5"); w.Code != http.StatusOK {
		t.Fatalf("main request status = %d body=%s", w.Code, w.Body.String())
	}

	if want := []string{"sub", "main"}; len(classes) != 2 || classes[0] != want[0] || classes[1] != want[1] {
		t.Fatalf("resolver saw classes %v, want %v", classes, want)
	}

	// Each POST produces two selector calls (miss on the original id,
	// hit on the mapped id). The class must be present on all four and
	// survive the remap retry; the sticky id must be class-suffixed so
	// main/sub of one conversation don't share peer affinity.
	if len(sel.got) != 4 {
		t.Fatalf("selector calls = %d, want 4", len(sel.got))
	}
	for i, wantClass := range []string{"sub", "sub", "main", "main"} {
		if sel.got[i].Class != wantClass {
			t.Fatalf("selector call %d Class = %q, want %q", i, sel.got[i].Class, wantClass)
		}
		if !strings.HasSuffix(sel.got[i].StickyID, ":"+wantClass) {
			t.Fatalf("selector call %d StickyID = %q, want suffix %q", i, sel.got[i].StickyID, ":"+wantClass)
		}
	}
}

func TestAnthropicMessages_NoClassifierLeavesRequestUnclassified(t *testing.T) {
	engine := fakeOllama(t, nil)
	defer engine.Close()

	sel := &fakeSelector{sel: localSelection("qwen3-8b-instruct", "qwen3:8b-q4_K_M")}
	gw := newGatewayUnderTest(t, sel, engine.URL)

	if w := postMessages(t, gw, "qwen3-8b-instruct"); w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if sel.got.Class != "" {
		t.Fatalf("Class = %q, want empty when ClassifyModel is nil", sel.got.Class)
	}
	if strings.Contains(sel.got.StickyID, ":") {
		t.Fatalf("StickyID = %q must not carry a class suffix", sel.got.StickyID)
	}
}
