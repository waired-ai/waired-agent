package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// flipServer is a /v1/models endpoint whose health the test can flip
// atomically, with a request counter for loop-leak assertions.
type flipServer struct {
	srv   *httptest.Server
	ok    atomic.Bool
	calls atomic.Int32
}

func newFlipServer(t *testing.T) *flipServer {
	t.Helper()
	f := &flipServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.calls.Add(1)
		if !f.ok.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, okModelsBody)
	}))
	return f
}

// waitForState polls Health() until it reports want, mirroring the
// waitFor helper in internal/runtime's tests (no clock injection in
// this package either).
func waitForState(t *testing.T, a *Adapter, d time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if a.Health(context.Background()).State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for state %q (now %q)",
		d, want, a.Health(context.Background()).State)
}

// TestAdapter_EnsureRunning_ConcurrentCallersAllSucceed is the PRODUCT
// CONTRACT that replaces the old "EnsureRunning called while already
// starting" hard error (#280): callers that arrive while the probe loop
// is still deciding wait for its verdict instead of erroring. The old
// error became a 503 runtime_unhealthy for every concurrent request.
// Ratified shape: TestOllamaAdapter_EnsureRunning_ConcurrentCallersJoin
// (#279) — here there is no join latch because waitReady already lets
// every caller wait independently under its own ctx.
func TestAdapter_EnsureRunning_ConcurrentCallersAllSucceed(t *testing.T) {
	f := newFlipServer(t)
	defer f.srv.Close()
	f.ok.Store(true)

	a, err := NewAdapter(Config{
		URL: f.srv.URL,
		ID:  "concurrent",
		// Two consecutive OKs one interval apart widen the Starting
		// window the callers race into.
		ProbeInterval: 40 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
		ReadySuccess:  2,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	const n = 12
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := a.EnsureRunning(ctx); err != nil {
				failures.Add(1)
				t.Errorf("concurrent EnsureRunning returned %v, want nil", err)
			}
		}()
	}
	wg.Wait()
	if fails := failures.Load(); fails != 0 {
		t.Errorf("%d of %d concurrent callers failed", fails, n)
	}

	// The concurrent entries must not have started a second probe loop.
	// Stop cancels the one recorded loop and drains it; a leaked twin
	// would keep hitting the endpoint on its own ticker.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	quiesced := f.calls.Load()
	time.Sleep(200 * time.Millisecond) // 5 probe intervals
	if got := f.calls.Load(); got != quiesced {
		t.Errorf("probe requests kept arriving after Stop (%d → %d) — a duplicate probe loop leaked", quiesced, got)
	}
}

// TestAdapter_EnsureRunning_AfterFailedReportsProbeErrorAndRecovers pins
// the second defect found under #280: the old guard keyed on a.cancel,
// which stays non-nil for the probe loop's whole life, so after a
// Ready→Failed demotion every later EnsureRunning hard-errored forever
// (only Stop could reset it). It must instead report the probe verdict,
// and once the loop self-recovers, succeed again — with no Stop in
// between.
func TestAdapter_EnsureRunning_AfterFailedReportsProbeErrorAndRecovers(t *testing.T) {
	f := newFlipServer(t)
	defer f.srv.Close()
	f.ok.Store(true)

	a, err := NewAdapter(Config{
		URL:           f.srv.URL,
		ID:            "refail",
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
		ReadySuccess:  1,
		FailedFails:   3,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatalf("initial EnsureRunning: %v", err)
	}

	f.ok.Store(false)
	waitForState(t, a, 2*time.Second, runtime.StateFailed)

	err = a.EnsureRunning(ctx)
	if err == nil {
		t.Fatal("EnsureRunning during StateFailed = nil, want the probe verdict")
	}
	if !strings.Contains(err.Error(), "probe failed") {
		t.Errorf("EnsureRunning during StateFailed = %v, want the probe verdict", err)
	}
	if strings.Contains(err.Error(), "already starting") {
		t.Errorf("EnsureRunning returned the removed concurrent-entry error: %v", err)
	}

	f.ok.Store(true)
	waitForState(t, a, 2*time.Second, runtime.StateReady)
	if err := a.EnsureRunning(ctx); err != nil {
		t.Errorf("EnsureRunning after self-recovery = %v, want nil (recovery must not require Stop)", err)
	}
}

// TestAdapter_Stop_ReleasesWaitingCallers: a caller waiting out the
// Starting window must get a prompt verdict when Stop lands, not spin
// until its own ctx expires — gateway request ctxs can be deadline-less
// (#280).
func TestAdapter_Stop_ReleasesWaitingCallers(t *testing.T) {
	f := newFlipServer(t)
	defer f.srv.Close()
	f.ok.Store(true)

	a, err := NewAdapter(Config{
		URL:           f.srv.URL,
		ID:            "stopwait",
		ProbeInterval: 20 * time.Millisecond,
		ProbeTimeout:  500 * time.Millisecond,
		// Unreachably high: the caller is pinned in StateStarting.
		ReadySuccess: 1000,
	})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	errCh := make(chan error, 1)
	go func() {
		// Deliberately deadline-less: a prompt return can only come
		// from Stop's verdict, which is the thing under test.
		errCh <- a.EnsureRunning(context.Background())
	}()

	waitForState(t, a, 2*time.Second, runtime.StateStarting)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("EnsureRunning released by Stop = %v, want a stopped error", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("EnsureRunning still waiting 1s after Stop — waiters were not released")
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
