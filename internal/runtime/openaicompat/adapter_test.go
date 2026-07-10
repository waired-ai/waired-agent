package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime"
)

const okModelsBody = `{"object":"list","data":[{"id":"meta-llama/Llama-3.1-8B-Instruct","object":"model"},{"id":"gpt-4o","object":"model"}]}`

func newOKServer(t *testing.T, recordedAuth *atomic.Value) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recordedAuth != nil {
			recordedAuth.Store(r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, okModelsBody)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestNewAdapter_NormalizesURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://192.168.1.10:8000/v1", "http://192.168.1.10:8000"},
		{"http://192.168.1.10:8000/v1/", "http://192.168.1.10:8000"},
		{"http://192.168.1.10:8000/", "http://192.168.1.10:8000"},
		{"http://192.168.1.10:8000", "http://192.168.1.10:8000"},
	}
	for _, tc := range cases {
		a, err := NewAdapter(Config{URL: tc.in})
		if err != nil {
			t.Fatalf("NewAdapter(%q): %v", tc.in, err)
		}
		if a.BaseURL() != tc.want {
			t.Errorf("NewAdapter(%q).BaseURL = %q, want %q", tc.in, a.BaseURL(), tc.want)
		}
	}
}

func TestNewAdapter_DefaultIDFromHost(t *testing.T) {
	a, err := NewAdapter(Config{URL: "http://192.168.1.10:8000/v1"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	want := "openai-compat:192_168_1_10_8000"
	if a.Name() != want {
		t.Errorf("Name() = %q, want %q", a.Name(), want)
	}
}

func TestNewAdapter_ExplicitID(t *testing.T) {
	a, err := NewAdapter(Config{URL: "http://x:8000", ID: "lan-vllm"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if a.Name() != "openai-compat:lan-vllm" {
		t.Errorf("Name() = %q, want openai-compat:lan-vllm", a.Name())
	}
}

func TestNewAdapter_RejectsEmptyURL(t *testing.T) {
	if _, err := NewAdapter(Config{}); err == nil {
		t.Error("expected error for empty URL")
	}
}

func TestAdapter_EnsureRunningReachesReady(t *testing.T) {
	srv := newOKServer(t, nil)
	defer srv.Close()
	a, err := NewAdapter(Config{
		URL:           srv.URL,
		ID:            "test",
		ProbeInterval: 50 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	got := a.Health(context.Background())
	if got.State != runtime.StateReady {
		t.Errorf("State = %q, want %q", got.State, runtime.StateReady)
	}
	if got.LastOK.IsZero() {
		t.Error("LastOK must be stamped on Ready")
	}
	models := a.ListModels()
	if len(models) != 2 || models[0] != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("ListModels = %v, want [Llama, gpt-4o]", models)
	}
}

func TestAdapter_EnsureRunningFailsAfterFailedFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := NewAdapter(Config{
		URL:           srv.URL,
		ID:            "fail",
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
		FailedFails:   3,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err == nil {
		t.Error("EnsureRunning should return error after FailedFails 500s")
	}
	got := a.Health(context.Background())
	if got.State != runtime.StateFailed {
		t.Errorf("State = %q, want %q (LastErr=%q)", got.State, runtime.StateFailed, got.LastErr)
	}
}

func TestAdapter_BearerInjected(t *testing.T) {
	var lastAuth atomic.Value
	lastAuth.Store("")
	srv := newOKServer(t, &lastAuth)
	defer srv.Close()

	t.Setenv("EXTERNAL_KEY", "secret-token-xyz")
	a, err := NewAdapter(Config{
		URL:           srv.URL,
		ID:            "auth",
		AuthEnvVar:    "EXTERNAL_KEY",
		ProbeInterval: 50 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	gotAuth, _ := lastAuth.Load().(string)
	if want := "Bearer secret-token-xyz"; gotAuth != want {
		t.Errorf("probe Authorization = %q, want %q", gotAuth, want)
	}

	// Transport() must also inject the same header.
	rt := a.Transport()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	gotAuth, _ = lastAuth.Load().(string)
	if gotAuth != "Bearer secret-token-xyz" {
		t.Errorf("Transport-issued Authorization = %q, want Bearer secret-token-xyz", gotAuth)
	}
}

func TestAdapter_NoBearerWhenEnvVarEmpty(t *testing.T) {
	var lastAuth atomic.Value
	lastAuth.Store("")
	srv := newOKServer(t, &lastAuth)
	defer srv.Close()

	// EXTERNAL_KEY intentionally unset.
	a, err := NewAdapter(Config{
		URL:           srv.URL,
		ID:            "noauth",
		AuthEnvVar:    "",
		ProbeInterval: 50 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	gotAuth, _ := lastAuth.Load().(string)
	if gotAuth != "" {
		t.Errorf("expected no Authorization header, got %q", gotAuth)
	}
}

func TestAdapter_TransportNilTokenForwards(t *testing.T) {
	a, err := NewAdapter(Config{URL: "http://x:8000"})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	rt := a.Transport()
	bearer, ok := rt.(*bearerRT)
	if !ok {
		t.Fatalf("Transport() did not return *bearerRT, got %T", rt)
	}
	if bearer.token != "" {
		t.Errorf("token = %q, want empty", bearer.token)
	}
}

func TestAdapter_StopCancelsProbe(t *testing.T) {
	srv := newOKServer(t, nil)
	defer srv.Close()
	a, err := NewAdapter(Config{
		URL:           srv.URL,
		ID:            "stop",
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := a.Health(context.Background()).State; got != runtime.StateStopped {
		t.Errorf("State after Stop = %q, want %q", got, runtime.StateStopped)
	}
}

func TestAdapter_ImplementsInterfaces(t *testing.T) {
	var a interface{} = &Adapter{}
	if _, ok := a.(runtime.Transporter); !ok {
		t.Error("*Adapter must satisfy runtime.Transporter")
	}
	if _, ok := a.(runtime.Adapter); !ok {
		t.Error("*Adapter must satisfy runtime.Adapter")
	}
	if _, ok := a.(runtime.ModelLister); !ok {
		t.Error("*Adapter must satisfy runtime.ModelLister")
	}
}

func TestProbeModels_RejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"missing api key"}`)
	}))
	defer srv.Close()
	_, err := probeModels(context.Background(), srv.Client(), srv.URL, time.Second, "")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("probeModels err = %v, want HTTP 401", err)
	}
}

func TestProbeModels_RejectsBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `not json`)
	}))
	defer srv.Close()
	_, err := probeModels(context.Background(), srv.Client(), srv.URL, time.Second, "")
	if err == nil {
		t.Error("probeModels should error on non-JSON body")
	}
}
