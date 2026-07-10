package intercept

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewritePassthroughModel(t *testing.T) {
	t.Run("waired id rewritten", func(t *testing.T) {
		body := []byte(`{"model":"waired/subagent","max_tokens":16}`)
		out, ok := rewritePassthroughModel(body, "claude-sonnet-5")
		if !ok {
			t.Fatal("expected a rewrite")
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("rewritten body unparseable: %v", err)
		}
		if obj["model"] != "claude-sonnet-5" {
			t.Fatalf("model = %v", obj["model"])
		}
	})

	t.Run("non-waired ids untouched", func(t *testing.T) {
		for name, body := range map[string]string{
			"anthropic id":           `{"model":"claude-fable-5","max_tokens":16}`,
			"no model key":           `{"max_tokens":16}`,
			"model not str":          `{"model":42}`,
			"malformed json":         `{"model":`,
			"prefix only in content": `{"model":"claude-x","messages":[{"role":"user","content":"say \"waired/subagent\""}]}`,
		} {
			t.Run(name, func(t *testing.T) {
				if _, ok := rewritePassthroughModel([]byte(body), "claude-sonnet-5"); ok {
					t.Fatal("must not rewrite")
				}
			})
		}
	})

	t.Run("lossless for other fields", func(t *testing.T) {
		// Large integers, floats, unicode, and unknown fields must
		// survive byte-exact (json.RawMessage guarantee).
		body := []byte(`{"model":"waired/subagent","big":9007199254740993,"pi":3.141592653589793238,"uni":"日本語 ","nested":{"keep":[1,2,3]}}`)
		out, ok := rewritePassthroughModel(body, "claude-sonnet-5")
		if !ok {
			t.Fatal("expected a rewrite")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]string{
			"big":    `9007199254740993`,
			"pi":     `3.141592653589793238`,
			"nested": `{"keep":[1,2,3]}`,
		} {
			if string(obj[field]) != want {
				t.Errorf("%s = %s, want %s (must be byte-exact)", field, obj[field], want)
			}
		}
	})
}

func TestPassthroughReplacementResolution(t *testing.T) {
	s := newServer(t, Deps{PassthroughTransport: fakeUpstream(nil)})
	if got := s.passthroughReplacement(); got != defaultPassthroughModel {
		t.Fatalf("before observation = %q, want default %q", got, defaultPassthroughModel)
	}
	s.observeMainModel("waired/subagent") // labels are never a rewrite target
	if got := s.passthroughReplacement(); got != defaultPassthroughModel {
		t.Fatalf("waired id must not be observed; got %q", got)
	}
	s.observeMainModel("claude-fable-5")
	if got := s.passthroughReplacement(); got != "claude-fable-5" {
		t.Fatalf("after observation = %q, want claude-fable-5", got)
	}

	over, err := NewServer(Config{Addr: "127.0.0.1:0", PassthroughModelOverride: "claude-opus-4-8"},
		Deps{PassthroughTransport: fakeUpstream(nil)})
	if err != nil {
		t.Fatal(err)
	}
	over.observeMainModel("claude-fable-5")
	if got := over.passthroughReplacement(); got != "claude-opus-4-8" {
		t.Fatalf("override must win; got %q", got)
	}
}

// bodyCapturingUpstream is fakeUpstream plus request-body capture, for
// asserting what actually reaches the real Anthropic API.
func bodyCapturingUpstream(bodies *[]string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(b))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
			Request:    r,
		}, nil
	})
}

func postJSON(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func upstreamModel(t *testing.T, body string) string {
	t.Helper()
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("upstream body unparseable: %v (%q)", err, body)
	}
	return obj.Model
}

func TestAnthropicModeRewritesSubagentModelAndFollowsMain(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Before any main observation: the built-in default.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"waired/subagent","max_tokens":16}`)
	// A main-loop request passes through untouched and is observed.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"claude-fable-5","max_tokens":16}`)
	// Subsequent subagent turns follow the observed main model.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"waired/subagent","max_tokens":16}`)
	// count_tokens rides the same message path.
	postJSON(t, srv.URL+"/v1/messages/count_tokens", `{"model":"waired/subagent"}`)

	if len(bodies) != 4 {
		t.Fatalf("upstream saw %d bodies, want 4", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("first subagent turn model = %q, want default %q", got, defaultPassthroughModel)
	}
	if got := upstreamModel(t, bodies[1]); got != "claude-fable-5" {
		t.Errorf("main turn model = %q, want claude-fable-5 (verbatim)", got)
	}
	if got := upstreamModel(t, bodies[2]); got != "claude-fable-5" {
		t.Errorf("labelled turn after observation = %q, want claude-fable-5", got)
	}
	if got := upstreamModel(t, bodies[3]); got != "claude-fable-5" {
		t.Errorf("count_tokens model = %q, want claude-fable-5", got)
	}
}

func TestAnthropicModePassesNonWairedBodyByteIdentical(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Unusual formatting must survive untouched — no re-marshal for
	// bodies that don't need the rewrite.
	body := "{\n  \"model\": \"claude-fable-5\",\n  \"big\": 9007199254740993\n}"
	postJSON(t, srv.URL+"/v1/messages", body)
	if len(bodies) != 1 || bodies[0] != body {
		t.Fatalf("upstream body = %q, want byte-identical original", bodies)
	}

	// Malformed JSON also passes through verbatim (fail-open).
	postJSON(t, srv.URL+"/v1/messages", `{"model":`)
	if bodies[1] != `{"model":` {
		t.Fatalf("malformed body = %q, want verbatim", bodies[1])
	}
}

func TestDegradedFailOpenRewritesSubagentModel(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		ClassRoute:           classRouteFunc(routeAuto),
		Degraded:             func() bool { return true },
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"waired/subagent","max_tokens":16}`)
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d bodies, want 1", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("degraded fail-open model = %q, want %q", got, defaultPassthroughModel)
	}
}

func TestFallbackReplayRewritesSubagentModel(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       errorHandler(http.StatusServiceUnavailable, nil),
		ClassRoute:           classRouteFunc(routeAuto),
		Degraded:             func() bool { return false },
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"waired/subagent","max_tokens":16}`)
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d bodies, want 1 (the fallback replay)", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("fallback replay model = %q, want %q", got, defaultPassthroughModel)
	}
}

func TestDispatchAutoObservesMainModelForLaterRewrites(t *testing.T) {
	var bodies []string
	localOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"message"}`)
	})
	s := newServer(t, Deps{
		LocalInference:       localOK,
		ClassRoute:           classRouteFunc(routeAuto),
		Degraded:             func() bool { return false },
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// A locally-served main turn still records the main model...
	postJSON(t, srv.URL+"/v1/messages", `{"model":"claude-fable-5","max_tokens":16}`)
	if got := s.passthroughReplacement(); got != "claude-fable-5" {
		t.Fatalf("observed replacement = %q, want claude-fable-5", got)
	}
}
