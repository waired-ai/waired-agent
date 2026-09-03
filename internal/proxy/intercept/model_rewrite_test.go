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

// A waired-owned id that names the real Anthropic API — the retired cloud row,
// which sessions still carry — must be rewritten to a real model before it
// leaves, or the API rejects it. The replacement follows whatever model the
// main loop was last seen using.
//
// The subagent label used to reach this leg too. It does not any more: it
// names neither side, so it is served on Waired
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md;
// waired-agent#1186 retires the label itself).
func TestCloudIDIsRewrittenAndFollowsTheMainModel(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Before any main observation: the built-in default.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+wairedCloudBareModel+`","max_tokens":16}`)
	// A main-loop request passes through untouched and is observed.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"claude-fable-5","max_tokens":16}`)
	// Subsequent cloud turns follow the observed main model.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+wairedCloudBareModel+`","max_tokens":16}`)
	// count_tokens rides the same message path.
	postJSON(t, srv.URL+"/v1/messages/count_tokens", `{"model":"`+wairedCloudBareModel+`"}`)

	if len(bodies) != 4 {
		t.Fatalf("upstream saw %d bodies, want 4", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != defaultPassthroughModel {
		t.Errorf("first cloud turn model = %q, want default %q", got, defaultPassthroughModel)
	}
	if got := upstreamModel(t, bodies[1]); got != "claude-fable-5" {
		t.Errorf("main turn model = %q, want claude-fable-5 (verbatim)", got)
	}
	if got := upstreamModel(t, bodies[2]); got != "claude-fable-5" {
		t.Errorf("cloud turn after observation = %q, want claude-fable-5", got)
	}
	if got := upstreamModel(t, bodies[3]); got != "claude-fable-5" {
		t.Errorf("count_tokens model = %q, want claude-fable-5", got)
	}
}

// The subagent label names neither side, so it stays here rather than being
// rewritten and relayed.
func TestSubagentLabelIsServedHere(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":"waired/subagent","max_tokens":16}`)
	if !localHit {
		t.Error("the subagent label was not served here")
	}
	if len(bodies) != 0 {
		t.Errorf("the subagent label reached the real Anthropic API: %v", bodies)
	}
}

func TestAnthropicModePassesNonWairedBodyByteIdentical(t *testing.T) {
	var bodies []string
	s := newServer(t, Deps{
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

}

// A body whose model cannot be read names no side, so it is served here — the
// same answer an unknown id gets, and the one that keeps the guarantee simple:
// only a turn carrying a real Anthropic model id leaves this machine
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestUnreadableBodyIsServedHere(t *testing.T) {
	var localHit bool
	var bodies []string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: bodyCapturingUpstream(&bodies),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	postJSON(t, srv.URL+"/v1/messages", `{"model":`)
	if !localHit {
		t.Error("a body nobody could parse was not served here")
	}
	if len(bodies) != 0 {
		t.Errorf("an unparseable body was relayed to the real Anthropic API: %v", bodies)
	}
}

// TestWairedIdsNeverBecomeThePassthroughReplacement: none of waired's own
// spellings may be remembered as "the model the main loop is using". This is
// how waired-agent#1036 stuck a whole host — `claude-waired-cloud` reached the
// wire (Claude Code strips "[1m]"), missed the exact-match table, was stored
// here, and every later fallback replay was rewritten to it and 404'd.
func TestWairedIdsNeverBecomeThePassthroughReplacement(t *testing.T) {
	for _, id := range []string{
		"claude-waired-cloud", wairedCloudModel, wairedAutoModel, "claude-waired-auto[1m]",
		wairedLocalModel, wairedPeerModel, wairedPublicModel, wairedAutoLegacyModel,
		"claude-waired-peer-linux-gpu", "waired/subagent", "waired/default",
		"CLAUDE-WAIRED-CLOUD", "claude-waired-something-this-build-never-heard-of",
	} {
		s := newServer(t, Deps{PassthroughTransport: fakeUpstream(nil)})
		s.observeMainModel(id)
		if got := s.passthroughReplacement(); got != defaultPassthroughModel {
			t.Errorf("observeMainModel(%q) made it the replacement (%q); no waired id is a model Anthropic serves", id, got)
		}
	}
}

// TestUpstreamRejectionRetiresTheObservedReplacement: a 404 means the id waired
// substituted is not a model. Recover on the next replay instead of repeating
// it for the rest of the process lifetime (waired-agent#1036).
func TestUpstreamRejectionRetiresTheObservedReplacement(t *testing.T) {
	var bodies []string
	notFound := rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"type":"error","error":{"type":"not_found_error","message":"model: claude-retired-9"}}`)),
			Request: r,
		}, nil
	})
	localFails := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	s := newServer(t, Deps{
		LocalInference:       localFails,
		PassthroughTransport: notFound,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// A main turn on a model that upstream later stops serving.
	s.observeMainModel("claude-retired-9")
	// A cloud turn is now rewritten to that id and 404s.
	postJSON(t, srv.URL+"/v1/messages", `{"model":"`+wairedCloudBareModel+`","max_tokens":16}`)
	if len(bodies) != 1 {
		t.Fatalf("upstream saw %d bodies, want 1", len(bodies))
	}
	if got := upstreamModel(t, bodies[0]); got != "claude-retired-9" {
		t.Fatalf("replay model = %q, want the observed id", got)
	}
	if got := s.passthroughReplacement(); got != defaultPassthroughModel {
		t.Errorf("replacement after a 404 = %q, want %q — a rejected id must not be replayed forever",
			got, defaultPassthroughModel)
	}
}
