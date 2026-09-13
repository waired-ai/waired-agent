package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The OpenAI-compatible listener's traffic class and capacity wait
// (waired-agent#1366). OpenCode sends its subagents here, marked with
// x-parent-session-id, and they used to be unclassified and refused with
// 503 the moment every slot on their row was taken.

func openAIClassGateway(t *testing.T, sel SelectorIface, adapterURL string, deps func(*Deps)) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: adapterURL})
	d := Deps{
		Selector:      sel,
		Runtimes:      reg,
		ListManifests: asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
		// The agent's classifier is cmd/waired-agent's
		// classifyOpenAIListenerClass; this is its shape, so the test is
		// about what the handler does with a class.
		ClassifyRequest: func(h http.Header) string {
			switch {
			case h.Get("X-Parent-Session-Id") != "":
				return "sub"
			case h.Get("X-Session-Id") != "":
				return "main"
			}
			return ""
		},
	}
	if deps != nil {
		deps(&d)
	}
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, d)
}

func postChatCompletionWith(t *testing.T, gw *Server, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

// TestOpenAIChat_ClassReachesTheSelector: the class the classifier reads is
// on the router request, where the Serve main conversation / Serve subagents
// exclusions read it, and it namespaces the sticky id the way the Anthropic
// surface's does. Product contract, ratifying source: owner decision
// 2026-09-14 that the switches apply to OpenCode
// (docs/decisions/20260914/0300-opencode-subagents-get-the-subagent-class.md).
func TestOpenAIChat_ClassReachesTheSelector(t *testing.T) {
	engine := fakeOllama(t, nil)
	defer engine.Close()

	for _, tc := range []struct {
		name      string
		headers   map[string]string
		wantClass string
	}{
		{"OpenCode subagent", map[string]string{"X-Session-Id": "ses_child", "X-Parent-Session-Id": "ses_parent"}, "sub"},
		{"OpenCode main turn", map[string]string{"X-Session-Id": "ses_parent"}, "main"},
		{"a client that says neither", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := &fakeSelector{sel: localSelection("qwen3-8b-instruct", "qwen3:8b-q4_K_M")}
			gw := openAIClassGateway(t, sel, engine.URL, nil)
			if w := postChatCompletionWith(t, gw, tc.headers); w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if sel.got.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", sel.got.Class, tc.wantClass)
			}
			hasSuffix := strings.Contains(sel.got.StickyID, ":")
			if tc.wantClass == "" && hasSuffix {
				t.Errorf("StickyID = %q carries a class suffix for an unclassified request", sel.got.StickyID)
			}
			if tc.wantClass != "" && !strings.HasSuffix(sel.got.StickyID, ":"+tc.wantClass) {
				t.Errorf("StickyID = %q, want suffix %q", sel.got.StickyID, ":"+tc.wantClass)
			}
		})
	}
}

// TestOpenAIChat_WaitsForASlotBeforeRefusing: a request that finds every
// slot taken is held for its class's budget, not refused after the three
// quick rounds. Measured before the fix: six OpenCode subagents on a
// peers-only row drew thirteen 503s in 85 s, and one subagent failed.
func TestOpenAIChat_WaitsForASlotBeforeRefusing(t *testing.T) {
	engine := fakeOllama(t, nil)
	defer engine.Close()

	// Full for longer than the three quick rounds (about half a second),
	// and well inside the budget.
	const fullRounds = 8
	sel := &capacitySelector{fullFor: fullRounds, sel: localSelection("qwen3-8b-instruct", "qwen3:8b-q4_K_M")}
	gw := openAIClassGateway(t, sel, engine.URL, func(d *Deps) {
		d.CapacityQueueBudget = func(class string) time.Duration {
			if class != "sub" {
				t.Errorf("budget asked for class %q, want sub", class)
			}
			return 30 * time.Second
		}
	})
	w := postChatCompletionWith(t, gw, map[string]string{"X-Session-Id": "ses_child", "X-Parent-Session-Id": "ses_parent"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s — a subagent that would have got a slot after %d rounds was refused",
			w.Code, w.Body.String(), fullRounds)
	}
	if len(sel.calls) != fullRounds+1 {
		t.Errorf("SelectK calls = %d, want %d", len(sel.calls), fullRounds+1)
	}
}

// TestOpenAIChat_NoBudgetStillRefusesQuickly: the overlay listener's
// HandlerSet wires no budget, and a peer relaying to this machine must keep
// its answer at once rather than hold the caller's connection open.
func TestOpenAIChat_NoBudgetStillRefusesQuickly(t *testing.T) {
	sel := &capacitySelector{fullFor: 99, sel: capacityTestSelection()}
	gw := openAIClassGateway(t, sel, "", func(d *Deps) { d.ClassifyRequest = nil })
	w := postChatCompletionWith(t, gw, map[string]string{"X-Parent-Session-Id": "ses_parent"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if len(sel.calls) != probeAttempts {
		t.Errorf("SelectK calls = %d, want the %d quick rounds and no wait", len(sel.calls), probeAttempts)
	}
}

// TestComputeStickyID_SessionAffinity: a client's own session id separates
// sessions whose bodies begin alike. OpenCode opens every session of one
// agent with the same system prompt, so the first-message hash put all the
// subagents of a turn in one conversation, bound to one peer.
func TestComputeStickyID_SessionAffinity(t *testing.T) {
	body := []byte(`{"model":"waired/peer","messages":[{"role":"system","content":"You are a subagent."}]}`)
	ident := StickyIdentity{FirstMessage: []byte(`{"role":"system","content":"You are a subagent."}`)}
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	a := ComputeStickyID(hdr("X-Session-Affinity", "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"), body, ident)
	b := ComputeStickyID(hdr("X-Session-Affinity", "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2"), body, ident)
	if a == "" || a == b {
		t.Errorf("two sessions sharing a 35-character prefix got ids %q and %q, want distinct", a, b)
	}
	if again := ComputeStickyID(hdr("X-Session-Affinity", "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"), body, ident); again != a {
		t.Errorf("one session got %q then %q, want the same id", a, again)
	}
	if none := ComputeStickyID(http.Header{}, body, ident); none == a {
		t.Errorf("the affinity header changed nothing: %q", none)
	}
	// The explicit conversation header still outranks it.
	if got := ComputeStickyID(hdr("X-Waired-Conversation-Id", "conv-1", "X-Session-Affinity", "ses_x"), body, ident); got != "conv-1" {
		t.Errorf("with both headers id = %q, want the explicit conv-1", got)
	}
}
