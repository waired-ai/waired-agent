package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errorHandler writes an uncommitted Anthropic-shaped error (status >= 400
// before any 2xx) — the recoverable, pre-first-byte class (#578).
func errorHandler(status int, hit *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit != nil {
			*hit = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"unsupported_feature"}}`)
	})
}

// The three rows of the dispatch contract, one test each: an id the real
// Anthropic API serves leaves, a Waired id does not, and an id naming neither
// side is served here so that nothing but the first kind ever leaves
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313).

func TestAnthropicIDPassesThrough(t *testing.T) {
	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("a model the real Anthropic API serves did not pass through")
	}
	if localHit {
		t.Error("a model the user named must not be answered by a local model")
	}
}

func TestWairedIDNeverPassesThrough(t *testing.T) {
	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedAutoModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("a Waired id was not served here")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a Waired id reached the real Anthropic API")
	}
}

func TestIDNamingNeitherSideIsServedHere(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// No model at all, and a body the real API would reject anyway.
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("an unnamed turn was not served here")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("an unnamed turn left the machine")
	}
}

// PRODUCT CONTRACT (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4): a Waired turn on a machine with nothing wired ends with a
// reason, in the one status Claude Code shows at once and verbatim. This row
// is INVERTED from "fails open to the real Anthropic API", and from the 503
// that produced the anonymous ten-retry loop in waired-agent#1180.
func TestWairedIDWithNothingWiredFailsClosed(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       nil,
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedAutoModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 — Claude Code retries a 5xx ten times behind an anonymous label", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a Waired id leaked upstream when nothing here could serve it")
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"waired_cannot_serve", "/model", "waired doctor"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the answer does not carry %q — it has to name a way out: %s", want, body)
		}
	}
}

// An Anthropic id still passes through on a machine with nothing wired: that
// half was never waired's to decide.
func TestAnthropicIDPassesThroughWithNothingWired(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       nil,
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Errorf("status=%d: an Anthropic id must still reach the real API", resp.StatusCode)
	}
}

func TestWairedIDSurfacesTheLocalError(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       errorHandler(http.StatusBadRequest, &localHit),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"`+wairedAutoModel+`","max_tokens":16}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !localHit {
		t.Error("local inference should have been tried")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the local 400 was not surfaced, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("a failed Waired turn was retried against the real Anthropic API")
	}
}

// recordingHandler2 marks a hit and writes a normal 2xx local body.
func recordingHandler2(hit *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit != nil {
			*hit = true
		}
		w.Header().Set("X-Local-Inference", "1")
		_, _ = io.WriteString(w, "LOCAL "+r.URL.Path)
	})
}

// localModelHandler mimics the gateway's mapped-response success: it stamps
// X-Waired-Local-Model before committing a 2xx body (#602).
func localModelHandler(model string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(localModelHeader, model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	})
}

func TestLocalModeReportsLocalModel(t *testing.T) {
	var served string
	s := newServer(t, Deps{
		LocalInference:       localModelHandler("qwen3-8b-instruct"),
		OnServed:             func(model, _ string) { served = model },
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if served != "qwen3-8b-instruct" {
		t.Errorf("OnServed model = %q, want qwen3-8b-instruct", served)
	}
}
