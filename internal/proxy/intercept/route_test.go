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

// streamingThenStopHandler commits a 200 + flushes a partial SSE body, then
// returns without finishing — the unrecoverable, post-first-byte class.
func streamingThenStopHandler(hit *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hit != nil {
			*hit = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, "event: message_start\n")
			f.Flush()
		}
		// Engine dies mid-stream: nothing more written. Status already 200.
	})
}

func classRouteFunc(route string) func(string) string { return func(string) string { return route } }

func TestRouteAnthropicAlwaysPassesThrough(t *testing.T) {
	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false }, // healthy
		ClassRoute:           classRouteFunc(routeAnthropic),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("route=anthropic did not pass through even though local was healthy")
	}
	if localHit {
		t.Error("route=anthropic must never hit local inference")
	}
}

func TestRouteWairedNeverPassesThrough(t *testing.T) {
	var localHit bool
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return true }, // degraded — must NOT fail open in local mode
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !localHit {
		t.Error("route=waired did not serve locally")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=waired must never pass through to upstream")
	}
}

func TestRouteWairedUnavailableWhenNoLocalInference(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       nil,
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("route=waired with no engine: status=%d want 503", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=waired must not leak upstream when unavailable")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "waired_local_unavailable") {
		t.Errorf("expected waired_local_unavailable error, got %q", body)
	}
}

func TestAutoFallbackOnPreFirstByteError(t *testing.T) {
	var localHit bool
	var last http.Request
	var fbReason string
	s := newServer(t, Deps{
		LocalInference:       errorHandler(http.StatusBadRequest, &localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(reason string) { fbReason = reason },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !localHit {
		t.Error("local inference should have been tried first")
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Errorf("fallback did not reach upstream: status=%d upstream=%q", resp.StatusCode, resp.Header.Get("X-Fake-Upstream"))
	}
	if got := resp.Header.Get(fallbackHeader); !strings.Contains(got, "anthropic") {
		t.Errorf("missing %s header, got %q", fallbackHeader, got)
	}
	// The local error's Content-Type (application/json) must NOT leak onto
	// the passthrough response (upstream sent text/plain).
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("local error headers leaked: Content-Type=%q want text/plain", ct)
	}
	if fbReason != "local_status_400" {
		t.Errorf("OnFallback reason=%q want local_status_400", fbReason)
	}
	// The upstream must have received the original body path/shape.
	if last.URL.Path != "/v1/messages" {
		t.Errorf("upstream saw path %q, want /v1/messages", last.URL.Path)
	}
}

func TestWairedRouteSurfacesLocalError(t *testing.T) {
	// route=waired is the privacy-strong option: serve local, surface a local
	// error, never redirect to Anthropic.
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       errorHandler(http.StatusBadRequest, &localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeWaired),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !localHit {
		t.Error("local inference should have been tried")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("route=waired should surface local 400, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("route=waired must NOT redirect to upstream")
	}
	if resp.Header.Get(fallbackHeader) != "" {
		t.Error("no fallback header expected on route=waired")
	}
}

func TestAutoStreamedThenErrorDoesNotFallback(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       streamingThenStopHandler(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !localHit {
		t.Error("local inference should have been tried")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("committed stream status=%d want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("must NOT fall back once the stream has committed 200 (fail-fast)")
	}
	if resp.Header.Get(fallbackHeader) != "" {
		t.Error("no fallback header expected after commit")
	}
	if !strings.Contains(string(body), "message_start") {
		t.Errorf("partial stream body lost: %q", body)
	}
}

func TestAutoNormalSuccessIsTransparent(t *testing.T) {
	var localHit bool
	s := newServer(t, Deps{
		LocalInference:       recordingHandler2(&localHit),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !localHit || resp.Header.Get("X-Local-Inference") != "1" {
		t.Error("normal 2xx should be served locally")
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("normal 2xx should not touch upstream")
	}
	if !strings.Contains(string(body), "LOCAL") {
		t.Errorf("local body lost: %q", body)
	}
}

func TestAutoOverCapErrorDoesNotBufferOrFallback(t *testing.T) {
	// Shrink the cap so a tiny body counts as "too big to buffer".
	orig := maxFallbackBodyBytes
	maxFallbackBodyBytes = 4
	defer func() { maxFallbackBodyBytes = orig }()

	var gotLen int
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotLen = len(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "err")
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	payload := "0123456789" // 10 bytes > cap 4
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Over-cap: transparent local dispatch. The local handler must have
	// received the FULL body (restore must not truncate), and no fallback.
	if gotLen != len(payload) {
		t.Errorf("over-cap local body truncated: got %d want %d", gotLen, len(payload))
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("over-cap should surface local error, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("over-cap must not fall back (memory bound)")
	}
}

func TestAutoHandlerWritesNothingEmits200(t *testing.T) {
	// A local handler that returns without writing anything must not be
	// treated as fallback-eligible; the client gets an implicit 200.
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	s := newServer(t, Deps{
		LocalInference:       local,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("empty local handler: status=%d want 200", resp.StatusCode)
	}
	if resp.Header.Get("X-Fake-Upstream") == "1" {
		t.Error("empty (non-error) local response must not fall back")
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

func TestAutoSuccessReportsLocalModel(t *testing.T) {
	var served string
	s := newServer(t, Deps{
		LocalInference:       localModelHandler("qwen3-8b-instruct"),
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnServed:             func(model, _ string) { served = model },
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if served != "qwen3-8b-instruct" {
		t.Errorf("OnServed model = %q, want qwen3-8b-instruct", served)
	}
	// The header stays on the response too (curl / proxy observability).
	if got := resp.Header.Get(localModelHeader); got != "qwen3-8b-instruct" {
		t.Errorf("%s = %q, want qwen3-8b-instruct", localModelHeader, got)
	}
}

func TestAutoSuccessAttributesPeerServing(t *testing.T) {
	// #601: the Claude surface is mesh-capable, so a committed response
	// may carry the gateway's peer header — the hook must attribute it
	// instead of misreporting a peer-served response as local.
	var servedModel, servedPeer string
	peerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(localModelHeader, "big-peer")
		w.Header().Set(inferencePeerHeader, "peer-X")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	})
	s := newServer(t, Deps{
		LocalInference:       peerHandler,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnServed:             func(model, peer string) { servedModel, servedPeer = model, peer },
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if servedModel != "big-peer" || servedPeer != "peer-X" {
		t.Errorf("OnServed = (%q, %q), want (big-peer, peer-X)", servedModel, servedPeer)
	}
}

func TestLocalModeReportsLocalModel(t *testing.T) {
	var served string
	s := newServer(t, Deps{
		LocalInference:       localModelHandler("qwen3-8b-instruct"),
		ClassRoute:           classRouteFunc(routeWaired),
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

// TestOnLocalServedNotFiredOnFallbackOrPlainSuccess: a fallback-retried
// error must not report a local model, and a local success without the
// gateway's model header reports nothing either.
func TestOnLocalServedNotFiredOnFallbackOrPlainSuccess(t *testing.T) {
	fired := false
	mk := func(local http.Handler) *httptest.Server {
		s := newServer(t, Deps{
			LocalInference:       local,
			Degraded:             func() bool { return false },
			ClassRoute:           classRouteFunc(routeAuto),
			OnServed:             func(string, string) { fired = true },
			PassthroughTransport: fakeUpstream(nil),
		})
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)
		return srv
	}

	for name, local := range map[string]http.Handler{
		"fallback":      errorHandler(http.StatusBadRequest, nil),
		"plain-success": recordingHandler2(nil),
	} {
		srv := mk(local)
		resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if fired {
			t.Errorf("%s: OnServed fired without a local model header", name)
		}
	}
}

// TestAutoFallbackReasonLocalNoModel: when the local gateway marks its
// (uncommitted) error with the machine-readable X-Waired-Local-Error
// header, the fallback reason becomes local_<value> instead of the
// generic local_status_<code>, and the marker never leaks to the client.
func TestAutoFallbackReasonLocalNoModel(t *testing.T) {
	var localHit bool
	var last http.Request
	var fbReason string
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localHit = true
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(localErrorHeader, "no_model")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"waired_no_local_model"}}`)
	})
	s := newServer(t, Deps{
		LocalInference:       local,
		Degraded:             func() bool { return false },
		ClassRoute:           classRouteFunc(routeAuto),
		OnFallback:           func(reason string) { fbReason = reason },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !localHit {
		t.Error("local inference should have been tried first")
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Errorf("fallback did not reach upstream: status=%d upstream=%q", resp.StatusCode, resp.Header.Get("X-Fake-Upstream"))
	}
	if fbReason != "local_no_model" {
		t.Errorf("OnFallback reason=%q want local_no_model", fbReason)
	}
	if got := resp.Header.Get(fallbackHeader); !strings.Contains(got, "reason=local_no_model") {
		t.Errorf("%s = %q, want reason=local_no_model", fallbackHeader, got)
	}
	if resp.Header.Get(localErrorHeader) != "" {
		t.Errorf("staged %s header leaked to the client", localErrorHeader)
	}
}
