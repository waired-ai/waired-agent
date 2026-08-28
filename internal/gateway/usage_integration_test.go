package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// Handler-level metering: token counts must reach both the local
// telemetry event and the Deps.OnUsage sink, on every surface and both
// stream modes (public share spec §12).

type captureSink struct {
	mu      sync.Mutex
	samples []UsageSample
}

func (c *captureSink) fn() func(context.Context, UsageSample) {
	return func(_ context.Context, s UsageSample) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.samples = append(c.samples, s)
	}
}

func (c *captureSink) snapshot() []UsageSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]UsageSample(nil), c.samples...)
}

// meteringEngine mimics an engine that reports usage, in either mode.
func meteringEngine(t *testing.T, capture *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if capture != nil {
			*capture = string(body)
		}
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			f, _ := w.(http.Flusher)
			for _, chunk := range []string{
				`data: {"choices":[{"delta":{"content":"hi"}}]}`,
				`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
				`data: [DONE]`,
			} {
				_, _ = w.Write([]byte(chunk + "\n\n"))
				if f != nil {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],` +
			`"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	})
	return httptest.NewServer(mux)
}

func newMeteringGateway(t *testing.T, upstream string, rec Recorder, sink func(context.Context, UsageSample)) *Server {
	t.Helper()
	return meteringGatewayStamped(t, upstream, rec, sink, newFixtureStamp(t))
}

// meteringGatewayStamped is newMeteringGateway with the stamp under the
// caller's control, for the one test whose fixture has to recognise this
// gateway's traffic on the other end.
//
// Every metering gateway gets a stamped client, not just that one: the
// client is where the private connection pool comes from, and sharing
// http.DefaultClient's pool across sixty-odd httptest servers in one
// binary is half of waired-agent#1008.
func meteringGatewayStamped(t *testing.T, upstream string, rec Recorder, sink func(context.Context, UsageSample), stamp string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstream})
	sel := &fakeSelector{sel: router.Selection{
		EndpointID:    "ep_local_ollama_qwen3-8b-instruct",
		ModelID:       "qwen3-8b-instruct",
		VariantID:     "q4-gguf",
		Runtime:       "ollama",
		EngineModel:   "qwen3:8b-q4_K_M",
		ExecutionMode: "local",
	}}
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     stampedClient(stamp),
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
		OnUsage:        sink,
	})
}

func postJSON(t *testing.T, gw *Server, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func TestGateway_RecordsTokens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai non-stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic non-stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := meteringEngine(t, nil)
			defer upstream.Close()
			rec := &captureRecorder{}
			sink := &captureSink{}
			gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
			}

			events := rec.requestsSnapshot()
			if len(events) != 1 {
				t.Fatalf("request events = %d, want 1", len(events))
			}
			if events[0].InputTokens != 11 || events[0].OutputTokens != 7 {
				t.Errorf("local telemetry tokens = %d/%d, want 11/7",
					events[0].InputTokens, events[0].OutputTokens)
			}

			samples := sink.snapshot()
			if len(samples) != 1 {
				t.Fatalf("usage samples = %d, want 1", len(samples))
			}
			s := samples[0]
			if s.InputTokens != 11 || s.OutputTokens != 7 {
				t.Errorf("sample tokens = %d/%d, want 11/7", s.InputTokens, s.OutputTokens)
			}
			// The control plane resolves a quality tier from the ENGINE
			// name, so the sample must carry it, not the catalog id.
			if s.EngineModel != "qwen3:8b-q4_K_M" {
				t.Errorf("EngineModel = %q", s.EngineModel)
			}
			if s.ModelID != "qwen3-8b-instruct" {
				t.Errorf("ModelID = %q", s.ModelID)
			}
			if s.Status != http.StatusOK {
				t.Errorf("Status = %d", s.Status)
			}
		})
	}
}

// Local telemetry is the §12 side benefit and must not depend on any
// Public Share wiring: with no sink at all, the tokens still land in the
// event.
func TestGateway_TokensRecordedWithoutSink(t *testing.T) {
	upstream := meteringEngine(t, nil)
	defer upstream.Close()
	rec := &captureRecorder{}
	gw := newMeteringGateway(t, upstream.URL, rec, nil)

	if w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	events := rec.requestsSnapshot()
	if len(events) != 1 || events[0].InputTokens != 11 {
		t.Fatalf("tokens missing from local telemetry: %+v", events)
	}
}

// A request that never reached an engine must not be metered — counting
// it would inflate a ledger the user sees.
func TestGateway_FailedRequestNotMetered(t *testing.T) {
	sink := &captureSink{}
	reg := runtime.NewRegistry() // no adapter registered
	gw := NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector: &fakeSelector{sel: router.Selection{
			ModelID: "qwen3-8b-instruct", Runtime: "ollama",
			EngineModel: "qwen3:8b-q4_K_M", ExecutionMode: "local",
		}},
		Runtimes:      reg,
		ListManifests: asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:    http.DefaultClient,
		AllowOpenAI:   true,
		OnUsage:       sink.fn(),
	})

	w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code < 400 {
		t.Fatalf("expected a failure status, got %d", w.Code)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("metered a request that never reached an engine: %+v", got)
	}
}

// A transport failure is the same rule as the test above, one branch
// further along: nothing was written to the client by the time it
// happened, so proxyToEngine hands the client a 502 of its own. The
// record used to say 200 with a mid-stream reason, which is the one
// combination emitUsage's status-only gate lets through — and on the
// overlay listener, the only surface that wires OnUsage, the sample
// becomes a request and a duration in the Public Share usage report for
// a turn its guest saw fail.
//
// PRODUCT CONTRACT (waired-agent#538): what the client got is what is
// recorded, and a request that never reached an engine is not metered.
func TestGateway_TransportFailureIsNotMetered(t *testing.T) {
	dead := meteringEngine(t, nil)
	deadURL := dead.URL
	dead.Close() // nothing is listening on that port now

	rec := &captureRecorder{}
	sink := &captureSink{}
	gw := newMeteringGateway(t, deadURL, rec, sink.fn())

	w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("client status = %d, want 502; body: %s", w.Code, w.Body.String())
	}

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("request events = %d, want 1", len(events))
	}
	if events[0].Status != http.StatusBadGateway {
		t.Errorf("recorded status = %d, want the 502 the client was handed", events[0].Status)
	}
	// The reason both Anthropic legs have always used for this exit: one
	// failure described two ways by two transports is how they drift.
	if events[0].ErrorReason != "engine_request_failed" {
		t.Errorf("recorded error_reason = %q, want engine_request_failed", events[0].ErrorReason)
	}
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("metered a request that never reached an engine: %+v", got)
	}
}

// truncatedAfterHeadersEngine answers 200, declares more body than it
// sends, and returns. Go's server cannot keep the connection alive after
// a short write, so the gateway's read of the body fails with the
// response already committed — the post-header half of the same branch.
func truncatedAfterHeadersEngine(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[`))
	}))
	// The short write is the point of the fixture; the server's complaint
	// about it is not test output anyone needs.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(srv.Close)
	return srv
}

// The other half of waired-agent#538, and the reason the two cheap fixes
// it rejects are rejected: a stream that broke after the client started
// receiving it IS metered.
//
// PRODUCT CONTRACT (waired-agent#538): the engine did the work and the
// client received part of it, so widening emitUsage's gate to "any
// error_reason" — the obvious-looking fix — would stop metering a turn
// that really ran. This is what tells the two apart.
func TestGateway_MidStreamTruncationIsStillMetered(t *testing.T) {
	upstream := truncatedAfterHeadersEngine(t)
	rec := &captureRecorder{}
	sink := &captureSink{}
	gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

	w := postJSON(t, gw, "/v1/chat/completions", map[string]any{
		"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	// The engine's 200 went out before anything went wrong; HTTP has no
	// way to take it back.
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want the 200 already committed", w.Code)
	}

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("request events = %d, want 1", len(events))
	}
	if events[0].ErrorReason != "mid_stream_truncate" {
		t.Errorf("recorded error_reason = %q, want mid_stream_truncate", events[0].ErrorReason)
	}
	if got := sink.snapshot(); len(got) != 1 {
		t.Fatalf("usage samples = %d, want 1: the engine ran and the client got part of it", len(got))
	}
}

// unusableTurnEngine ends every attempt the way ollama 0.31.1 does when
// its own tool parser rejects what the model emitted (#442): reasoning
// in full, no content, no finish_reason, no [DONE] — the body simply
// closes. Each attempt reports its own usage, and a DIFFERENT number
// each time, so a test can tell three attempts folded together from one
// attempt counted three times.
//
// The attempt counter is returned rather than inferred from the tokens:
// what needs proving is that the engine really was asked three times.
//
// It counts stamped requests to /v1/chat/completions and nothing else.
// Every other counted fake in this package is mux-bound to that path;
// this one used to take a bare HandlerFunc and increment on the first
// line, so anything at all that reached its ephemeral port was an engine
// attempt as far as the assertion could tell (waired-agent#1008). What it
// declines to count it records instead, so a recurrence names what
// arrived.
func unusableTurnEngine(t *testing.T, stamp string) (*httptest.Server, *atomic.Int32, *foreignTraffic) {
	t.Helper()
	var attempts atomic.Int32
	foreign := &foreignTraffic{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !foreign.mine(r, stamp) {
			w.WriteHeader(http.StatusOK)
			return
		}
		n := int(attempts.Add(1))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		send := func(payload string) {
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		send(`{"choices":[{"index":0,"delta":{"reasoning":"deciding which tool to call"}}]}`)
		send(fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, 10*n, n))
		// The measured tail: a chunk carrying nothing, a null
		// finish_reason, and then silence.
		send(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &attempts, foreign
}

// The fixture's own filter, pinned end to end rather than left to the
// test above: that test would still pass if the filter counted
// everything, which is the state waired-agent#1008 was reported from.
//
// A stranger here is a stand-in for whatever really arrived on that run.
// This package opens more than sixty httptest listeners in one binary and
// the kernel re-issues their ports, so "something else reached the port"
// needs no culprit to be worth defending against — #932/#933 could not
// name theirs either.
func TestUnusableTurnEngine_DoesNotCountAStrangersRequest(t *testing.T) {
	stamp := newFixtureStamp(t)
	upstream, attempts, foreign := unusableTurnEngine(t, stamp)

	resp, err := http.Post(upstream.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"whatever","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got := attempts.Load(); got != 0 {
		t.Fatalf("engine attempts = %d after a request this fixture did not cause, want 0 — "+
			"the counter is measuring the process, not the subject", got)
	}
	if got := foreign.report(); !strings.Contains(got, "1 unstamped request(s)") {
		t.Fatalf("report = %q, want the stranger named; a fixture that drops it silently "+
			"leaves the next occurrence looking like a product defect", got)
	}
}

// waired-agent#554. The anthropic streaming leg gives up on a turn no
// retry could make usable and records the failure — but the client was
// handed a 200 back when the SSE headers went out, and the record said
// 502, the one thing emitUsage's status gate drops.
//
// The tokens are the point. proxyAnthropicStream folds every abandoned
// attempt into setUsage deliberately (waired-agent#458: "the engine
// really did that work, and leaving it out would make a model that needs
// three tries look as cheap as one that needs none") — and then the 502
// threw the sample away, so the one surface that reports usage saw none
// of it. This follows those tokens all the way to the sink.
//
// PRODUCT CONTRACT (waired-agent#554, #458, #112): a turn the engine
// really ran is metered even when nothing usable came back, and the
// record states the 200 the client received.
func TestGateway_AnthropicUnusableTurnIsMeteredWithRetriesFolded(t *testing.T) {
	stamp := newFixtureStamp(t)
	upstream, attempts, foreign := unusableTurnEngine(t, stamp)
	noteForeignTraffic(t, foreign)
	rec := &captureRecorder{}
	sink := &captureSink{}
	gw := meteringGatewayStamped(t, upstream.URL, rec, sink.fn(), stamp)

	w := postJSON(t, gw, "/anthropic/v1/messages", map[string]any{
		"model": "waired/default", "max_tokens": 16, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want the 200 the SSE headers already committed", w.Code)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("engine attempts = %d, want 3 (one draw plus maxStreamRetries) — %s",
			got, foreign.report())
	}

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("request events = %d, want 1", len(events))
	}
	if events[0].Status != http.StatusOK {
		t.Errorf("recorded status = %d, want the 200 the client was handed", events[0].Status)
	}
	if events[0].ErrorReason != "engine_truncated_stream" {
		t.Errorf("recorded error_reason = %q, want engine_truncated_stream", events[0].ErrorReason)
	}

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("usage samples = %d, want 1: the engine drew three times", len(got))
	}
	// 10+20+30 and 1+2+3. Distinct per attempt, so this cannot pass with
	// one attempt counted three times, or with only the last one kept.
	if got[0].InputTokens != 60 || got[0].OutputTokens != 6 {
		t.Errorf("metered (in=%d, out=%d), want (60, 6) — every attempt folded in",
			got[0].InputTokens, got[0].OutputTokens)
	}
}

// truncatedSSEStreamEngine delivers part of an answer and then stops
// short of the body it declared, so the gateway's scanner fails with the
// SSE response already committed. The anthropic twin of
// truncatedAfterHeadersEngine: the same physical event, a peer's engine
// dying with the client already reading.
//
// Content, not just reasoning, so the turn is past the point of a redraw
// — this is a broken stream, not a bad draw.
func truncatedSSEStreamEngine(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"the answer beg"}}]}` + "\n\n"))
	}))
	// The short write is the point of the fixture; the server's complaint
	// about it is not test output anyone needs.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(srv.Close)
	return srv
}

// waired-agent#554. The same event as
// TestGateway_MidStreamTruncationIsStillMetered, arriving on the other
// transport. It was metered on /v1/chat/completions and dropped on
// /anthropic/v1/messages, so what a provider reported for one dead
// engine depended on which API its guest happened to call.
//
// PRODUCT CONTRACT (waired-agent#554, #532): two transports do not
// describe one failure differently.
func TestGateway_AnthropicTruncatedStreamIsMetered(t *testing.T) {
	upstream := truncatedSSEStreamEngine(t)
	rec := &captureRecorder{}
	sink := &captureSink{}
	gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

	w := postJSON(t, gw, "/anthropic/v1/messages", map[string]any{
		"model": "waired/default", "max_tokens": 16, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d, want the 200 the SSE headers already committed", w.Code)
	}

	events := rec.requestsSnapshot()
	if len(events) != 1 {
		t.Fatalf("request events = %d, want 1", len(events))
	}
	if events[0].Status != http.StatusOK {
		t.Errorf("recorded status = %d, want the 200 the client was handed", events[0].Status)
	}
	if events[0].ErrorReason != "engine_truncated_stream" {
		t.Errorf("recorded error_reason = %q, want engine_truncated_stream", events[0].ErrorReason)
	}
	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("usage samples = %d, want 1: the engine ran and the client got part of it", len(got))
	}
	if got[0].InputTokens != 11 || got[0].OutputTokens != 7 {
		t.Errorf("metered (in=%d, out=%d), want (11, 7)", got[0].InputTokens, got[0].OutputTokens)
	}
}

// §15-10: prompt content must not appear in telemetry, anywhere. The
// canary rides in the message body and in the response; if any capture
// path ever widens to include content, this fails.
func TestNoPromptContentInTelemetry(t *testing.T) {
	const canary = "waired-canary-8e1d4c"

	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": canary}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": canary}}}},
		{"anthropic", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": canary}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := meteringEngine(t, nil)
			defer upstream.Close()
			rec := &captureRecorder{}
			sink := &captureSink{}
			gw := newMeteringGateway(t, upstream.URL, rec, sink.fn())

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
			}

			for _, ev := range rec.requestsSnapshot() {
				blob, _ := json.Marshal(ev)
				if bytes.Contains(blob, []byte(canary)) {
					t.Errorf("RequestEvent carries prompt content: %s", blob)
				}
			}
			for _, s := range sink.snapshot() {
				blob, _ := json.Marshal(s)
				if bytes.Contains(blob, []byte(canary)) {
					t.Errorf("UsageSample carries prompt content: %s", blob)
				}
				if strings.Contains(s.ModelID+s.EngineModel+s.Class, canary) {
					t.Errorf("UsageSample string field carries prompt content: %+v", s)
				}
			}
		})
	}
}
