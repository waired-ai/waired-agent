package intercept

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeModelsUpstream answers with a JSON /v1/models envelope (application/json),
// marking the passthrough with X-Fake-Upstream and capturing the last request.
func fakeModelsUpstream(last *http.Request, body string) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		if last != nil {
			*last = *r
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":    []string{"application/json"},
				"X-Fake-Upstream": []string{"1"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: r,
		}, nil
	})
}

func directiveServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0", ModelRouteDirectives: true}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// upstreamModelsBody is a minimal real-shaped Anthropic /v1/models envelope
// carrying one real model plus a novel field to prove unknown-field preservation.
const upstreamModelsBody = `{"data":[{"type":"model","id":"claude-sonnet-5","display_name":"Claude Sonnet 5","created_at":"2025-01-01T00:00:00Z"}],"has_more":false,"first_id":"claude-sonnet-5","last_id":"claude-sonnet-5","novel_field":"keep"}`

type modelsEnvelope struct {
	Data []struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
	HasMore    bool   `json:"has_more"`
	FirstID    string `json:"first_id"`
	LastID     string `json:"last_id"`
	NovelField string `json:"novel_field"`
}

// TestModelsAnthropicMergesDirectivesWhenFlagOn: on the anthropic route with the
// #52 feature on, /v1/models still passes through to the real Anthropic list
// (invariant preserved) but the two directive ids are spliced in so they appear
// in Claude Code's /model picker. Real models and unknown fields survive.
func TestModelsAnthropicMergesDirectivesWhenFlagOn(t *testing.T) {
	var last http.Request
	s := directiveServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		Degraded:             func() bool { return false }, // healthy
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeModelsUpstream(&last, upstreamModelsBody),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("anthropic route must still pass /v1/models through to upstream")
	}
	if last.URL.Path != "/v1/models" {
		t.Errorf("upstream saw path %q, want /v1/models", last.URL.Path)
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length header %q != body length %d", cl, len(body))
	}

	var env modelsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("merged body is not valid JSON: %v\n%s", err, body)
	}
	if len(env.Data) != 4 {
		t.Fatalf("data length = %d, want 4 (3 directives + 1 upstream)", len(env.Data))
	}
	if env.Data[0].ID != wairedAutoModel || env.Data[1].ID != wairedLocalModel || env.Data[2].ID != wairedCloudModel {
		t.Errorf("directive order = %q,%q,%q; want %q,%q,%q",
			env.Data[0].ID, env.Data[1].ID, env.Data[2].ID, wairedAutoModel, wairedLocalModel, wairedCloudModel)
	}
	if env.Data[0].DisplayName != wairedAutoDisplay {
		t.Errorf("auto display_name = %q, want %q", env.Data[0].DisplayName, wairedAutoDisplay)
	}
	if env.Data[3].ID != "claude-sonnet-5" {
		t.Errorf("upstream model dropped: data[3].id = %q, want claude-sonnet-5", env.Data[3].ID)
	}
	if env.FirstID != wairedAutoModel {
		t.Errorf("first_id = %q, want %q (first prepended directive)", env.FirstID, wairedAutoModel)
	}
	if env.LastID != "claude-sonnet-5" {
		t.Errorf("last_id = %q, want claude-sonnet-5 (unchanged)", env.LastID)
	}
	if env.NovelField != "keep" {
		t.Errorf("unknown field dropped: novel_field = %q, want keep", env.NovelField)
	}
}

// Both advertised ids must pass Claude Code's ^(claude|anthropic) picker filter.
func TestMergedDirectiveIdsPassPickerFilter(t *testing.T) {
	for _, id := range []string{wairedAutoModel, wairedLocalModel, wairedCloudModel} {
		if !strings.HasPrefix(id, "claude") && !strings.HasPrefix(id, "anthropic") {
			t.Errorf("directive id %q must start with claude/anthropic to survive Claude Code's picker filter", id)
		}
	}
}

// TestModelsFlagOffNoMerge: with the feature off, the anthropic passthrough is
// byte-verbatim — no directive ids injected (regression guard on the opt-in).
func TestModelsFlagOffNoMerge(t *testing.T) {
	var last http.Request
	s := newServer(t, Deps{ // newServer => ModelRouteDirectives off
		LocalInference:       recordingHandler(new(string)),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeModelsUpstream(&last, upstreamModelsBody),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != upstreamModelsBody {
		t.Errorf("flag off must pass body verbatim.\n got: %s\nwant: %s", body, upstreamModelsBody)
	}
}

// TestModelsSingleObjectDirectiveSynthesized: GET /v1/models/{directive-id} is
// answered locally (the real API has no such model → would 404), without hitting
// upstream.
func TestModelsSingleObjectDirectiveSynthesized(t *testing.T) {
	var last http.Request
	s := directiveServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeModelsUpstream(&last, upstreamModelsBody),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models/" + wairedLocalModel)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("single-object directive id must be synthesized locally, not passed through")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var obj struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("synth body not JSON: %v (%s)", err, body)
	}
	if obj.ID != wairedLocalModel || obj.DisplayName != wairedLocalDisplay {
		t.Errorf("synth object = %+v, want id=%q display=%q", obj, wairedLocalModel, wairedLocalDisplay)
	}
}

// TestModelsSingleObjectNonDirectivePassesThrough: a real model id in the
// single-object form still passes straight through even with the flag on.
func TestModelsSingleObjectNonDirectivePassesThrough(t *testing.T) {
	var last http.Request
	s := directiveServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeModelsUpstream(&last, `{"type":"model","id":"claude-sonnet-5"}`),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models/claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("non-directive single-object id must pass through to upstream")
	}
	if last.URL.Path != "/v1/models/claude-sonnet-5" {
		t.Errorf("upstream saw path %q, want /v1/models/claude-sonnet-5", last.URL.Path)
	}
}

// TestModelsDirectivesIdempotent: if the upstream list already carries a
// directive id, it is not duplicated (guards against double-injection if the
// list is ever served through more than one merge).
func TestModelsDirectivesIdempotent(t *testing.T) {
	// Upstream already includes the local directive id + one real model.
	upstream := `{"data":[` +
		`{"type":"model","id":"` + wairedLocalModel + `","display_name":"x"},` +
		`{"type":"model","id":"claude-sonnet-5"}` +
		`],"has_more":false,"first_id":"` + wairedLocalModel + `","last_id":"claude-sonnet-5"}`
	s := directiveServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeModelsUpstream(nil, upstream),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var env modelsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, body)
	}
	autos, locals, clouds := 0, 0, 0
	for _, m := range env.Data {
		switch m.ID {
		case wairedAutoModel:
			autos++
		case wairedLocalModel:
			locals++
		case wairedCloudModel:
			clouds++
		}
	}
	if locals != 1 {
		t.Errorf("local directive appears %d times, want exactly 1 (idempotent — already present, not re-added)", locals)
	}
	if autos != 1 {
		t.Errorf("auto directive appears %d times, want exactly 1 (missing one prepended)", autos)
	}
	if clouds != 1 {
		t.Errorf("cloud directive appears %d times, want exactly 1 (missing one prepended)", clouds)
	}
}
