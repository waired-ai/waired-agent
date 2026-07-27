package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingReporter is a runtime.FailureReporter that records the real
// arguments, so a test can assert WHAT the gateway told the adapter — not
// merely that it told it something.
type recordingReporter struct {
	mu     sync.Mutex
	status int
	body   []byte
	calls  int
}

func (r *recordingReporter) ReportUpstreamFailure(status int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.status = status
	r.body = append([]byte(nil), body...)
}

func (r *recordingReporter) snapshot() (int, string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, string(r.body), r.calls
}

// engineReturning serves one canned status + body on every request.
func engineReturning(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProxyToEngine_Upstream500_ReportsAndRecords is the detection point for
// waired-agent#29.
//
// PRODUCT CONTRACT, three parts:
//   - the adapter is told, with the verbatim body (only it can classify);
//   - the event ring records the REAL status, not 200 (this path used to fall
//     through to the caller's rr.succeed(), so a wire-500 logged as 200);
//   - no usage sample is emitted, so a failed request is not billed.
func TestProxyToEngine_Upstream500_ReportsAndRecords(t *testing.T) {
	const body = `{"error":{"message":"llama-server process has terminated: signal: segmentation fault (core dumped)","type":"api_error"}}`
	srv := engineReturning(t, http.StatusInternalServerError, body)

	rep := &recordingReporter{}
	rr := &requestRec{}
	var usage int
	rr.onUsage = func(context.Context, UsageSample) { usage++ }
	rr.ev.Model = "m"
	w := httptest.NewRecorder()

	err := proxyToEngine(context.Background(), srv.Client(), srv.URL, "/v1/chat/completions",
		http.Header{}, []byte(`{}`), w, rr, rep)
	if err != nil {
		t.Fatalf("proxyToEngine returned %v; a forwarded upstream error is not a transport failure", err)
	}

	status, got, calls := rep.snapshot()
	if calls != 1 {
		t.Errorf("reporter called %d times, want 1", calls)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("reported status = %d, want 500", status)
	}
	if !strings.Contains(got, "llama-server process has terminated") {
		t.Errorf("reported body lost the engine's reason: %q", got)
	}

	if rr.ev.Status != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500", rr.ev.Status)
	}
	if rr.ev.ErrorReason != "engine_error" {
		t.Errorf("recorded error_reason = %q, want engine_error", rr.ev.ErrorReason)
	}
	// succeed() must not be able to overwrite the failure: it only defaults
	// Status when still 0, which is what makes the caller's unchanged
	// rr.succeed() a no-op here.
	rr.succeed()
	if rr.ev.Status != http.StatusInternalServerError {
		t.Errorf("succeed() overwrote the failure status: %d", rr.ev.Status)
	}
	rr.emitUsage()
	if usage != 0 {
		t.Errorf("emitted %d usage samples for a failed request, want 0", usage)
	}

	if w.Code != http.StatusInternalServerError {
		t.Errorf("client status = %d, want 500 forwarded verbatim", w.Code)
	}
	if !strings.Contains(w.Body.String(), "llama-server process has terminated") {
		t.Errorf("client lost the engine's error body: %q", w.Body.String())
	}
}

// TestProxyToEngine_NilReporter pins that an adapter which does not implement
// FailureReporter is simply skipped. PRODUCT CONTRACT: peer and
// openai-compat adapters deliberately do not implement it, so a REMOTE peer's
// 500 can never demote this host's engine.
func TestProxyToEngine_NilReporter(t *testing.T) {
	srv := engineReturning(t, http.StatusInternalServerError, `{"error":"peer exploded"}`)
	rr := &requestRec{}
	w := httptest.NewRecorder()

	if err := proxyToEngine(context.Background(), srv.Client(), srv.URL, "/v1/chat/completions",
		http.Header{}, []byte(`{}`), w, rr, nil); err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if rr.ev.Status != http.StatusInternalServerError {
		t.Errorf("recorded status = %d, want 500 even with no reporter", rr.ev.Status)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("client status = %d, want 500", w.Code)
	}
}

// TestProxyToEngine_LargeErrorBodyReachesClient pins that bounding the sniff
// does not truncate what the client sees: only the classification is capped.
func TestProxyToEngine_LargeErrorBodyReachesClient(t *testing.T) {
	big := strings.Repeat("x", engineErrorSniffMax+4096)
	srv := engineReturning(t, http.StatusBadGateway, big)
	rep := &recordingReporter{}
	w := httptest.NewRecorder()

	if err := proxyToEngine(context.Background(), srv.Client(), srv.URL, "/v1/chat/completions",
		http.Header{}, []byte(`{}`), w, &requestRec{}, rep); err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if got := w.Body.Len(); got != len(big) {
		t.Errorf("client received %d bytes, want the full %d", got, len(big))
	}
	if _, sniffed, _ := rep.snapshot(); len(sniffed) != engineErrorSniffMax {
		t.Errorf("reporter got %d bytes, want the %d-byte cap", len(sniffed), engineErrorSniffMax)
	}
}

// TestProxyToEngine_2xxUnaffected guards the happy path: a success must not
// take the new branch, and must still record 200 via the caller's succeed().
func TestProxyToEngine_2xxUnaffected(t *testing.T) {
	srv := engineReturning(t, http.StatusOK, `{"choices":[]}`)
	rep := &recordingReporter{}
	rr := &requestRec{}
	w := httptest.NewRecorder()

	if err := proxyToEngine(context.Background(), srv.Client(), srv.URL, "/v1/chat/completions",
		http.Header{}, []byte(`{}`), w, rr, rep); err != nil {
		t.Fatalf("proxyToEngine: %v", err)
	}
	if _, _, calls := rep.snapshot(); calls != 0 {
		t.Errorf("reporter called %d times on a 2xx, want 0", calls)
	}
	rr.succeed()
	if rr.ev.Status != http.StatusOK {
		t.Errorf("recorded status = %d, want 200", rr.ev.Status)
	}
	if w.Code != http.StatusOK {
		t.Errorf("client status = %d, want 200", w.Code)
	}
}
