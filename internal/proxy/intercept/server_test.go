package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rtFunc is a fake passthrough transport: it records the outbound request and
// returns a canned response so tests can assert "passthrough was taken" without
// a real upstream.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeUpstream returns a transport that answers every request with 200 and a
// body marking the path it saw, plus a pointer to capture the last request.
func fakeUpstream(last *http.Request) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		if last != nil {
			*last = *r
		}
		body := "UPSTREAM " + r.Method + " " + r.URL.Host + r.URL.Path
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}, "X-Fake-Upstream": []string{"1"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
}

// recordingHandler captures the path it was dispatched with and writes a marker.
func recordingHandler(gotPath *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		w.Header().Set("X-Local-Inference", "1")
		_, _ = io.WriteString(w, "LOCAL "+r.URL.Path)
	})
}

func newServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	s, err := NewServer(Config{Addr: "127.0.0.1:0"}, deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestMessagePathRoutesToLocalInferenceWithRewrite(t *testing.T) {
	var gotPath string
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotPath),
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, p := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		resp, err := http.Post(srv.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("POST %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("X-Local-Inference") != "1" {
			t.Errorf("POST %s did not hit local inference (body=%q)", p, body)
		}
		if want := "/anthropic" + p; gotPath != want {
			t.Errorf("local inference saw path %q, want %q", gotPath, want)
		}
	}
}

func TestNonMessagePathPassesThrough(t *testing.T) {
	var last http.Request
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(new(string)),
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// NB: /v1/models is NOT in this list — it is served locally now (#623).
	for _, p := range []string{"/v1/oauth/token", "/v1/organizations", "/v1/messages/extra/sub"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Fake-Upstream") != "1" {
			t.Errorf("GET %s did not pass through to upstream", p)
		}
		if last.URL.Path != p {
			t.Errorf("upstream saw path %q, want %q", last.URL.Path, p)
		}
		// Regression (#488 LXD): passthrough must target the REAL Anthropic
		// host, NOT the loopback the client connected to — otherwise it loops
		// back onto this listener (tls handshake against itself).
		if last.URL.Host != "api.anthropic.com" || last.Host != "api.anthropic.com" {
			t.Errorf("passthrough host = url:%q host:%q, want api.anthropic.com", last.URL.Host, last.Host)
		}
		if last.URL.Scheme != "https" {
			t.Errorf("passthrough scheme = %q, want https", last.URL.Scheme)
		}
	}
}

func TestFailOpenWhenDegraded(t *testing.T) {
	var gotLocal string
	var last http.Request
	degraded := true
	s := newServer(t, Deps{
		LocalInference:       recordingHandler(&gotLocal),
		Degraded:             func() bool { return degraded },
		PassthroughTransport: fakeUpstream(&last),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Degraded: /v1/messages must FAIL OPEN to the upstream, not 503/local.
	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("degraded message path did not fail open to upstream")
	}
	if gotLocal != "" {
		t.Errorf("degraded path still hit local inference (path=%q)", gotLocal)
	}

	// Recover: now it should serve locally again.
	degraded = false
	resp2, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.Header.Get("X-Local-Inference") != "1" {
		t.Error("recovered message path did not return to local inference")
	}
}

func TestFailOpenWhenNoLocalInference(t *testing.T) {
	s := newServer(t, Deps{
		LocalInference:       nil, // passthrough-only mode
		PassthroughTransport: fakeUpstream(nil),
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Fake-Upstream") != "1" {
		t.Error("nil LocalInference did not pass message path through to upstream")
	}
}

func TestNewServerValidation(t *testing.T) {
	if _, err := NewServer(Config{}, Deps{}); err == nil {
		t.Error("NewServer without PassthroughTransport should error")
	}
	if _, err := NewServer(Config{}, Deps{PassthroughTransport: fakeUpstream(nil)}); err != nil {
		t.Errorf("NewServer with PassthroughTransport should succeed, got %v", err)
	}
}
