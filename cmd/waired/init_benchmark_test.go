package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// benchStubServer wires the four endpoints promptBenchmarkRecommendation
// touches and records the accept/dismiss calls it receives.
type benchStub struct {
	rec   *management.BenchmarkRecommendation
	ready bool // /benchmark returns 200 (vs 425)
	state string
	// readyAfter simulates a transient startup: /benchmark returns 425 (and
	// /status returns `state`) for the first readyAfter polls, then flips to
	// ready (200). 0 means "honour `ready` verbatim" (never auto-flips).
	readyAfter   int
	acceptedID   string
	dismissFrom  string
	dismissTo    string
	dismissCount int
	acceptCount  int
	disableCount int

	mu         sync.Mutex
	benchCalls int
}

func (b *benchStub) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/benchmark", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.benchCalls++
		flipped := b.readyAfter > 0 && b.benchCalls > b.readyAfter
		b.mu.Unlock()
		if !b.ready && !flipped {
			w.WriteHeader(http.StatusTooEarly)
			return
		}
		_ = json.NewEncoder(w).Encode(management.BenchmarkRunResponse{Ran: true, Recommendation: b.rec})
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: b.state})
	})
	mux.HandleFunc("/waired/v1/inference/preferred-model", func(w http.ResponseWriter, r *http.Request) {
		var req management.PreferredModelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.acceptedID = req.ModelID
		b.acceptCount++
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(management.PreferredModelResponse{ModelID: req.ModelID, WillRestart: true})
	})
	mux.HandleFunc("/waired/v1/inference/recommendation/dismiss", func(w http.ResponseWriter, r *http.Request) {
		var req management.RecommendationDismissRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.dismissFrom, b.dismissTo = req.FromVariantID, req.ToVariantID
		b.dismissCount++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/waired/v1/inference/disable", func(w http.ResponseWriter, r *http.Request) {
		b.disableCount++
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

// tinyRec is a below-floor recommendation: the only lighter step-down is the
// tiny 0.5B, which triggers the disable-or-proceed dialog instead of the
// neutral lighter-model switch.
func tinyRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		FromModelID: "qwen2.5-coder-3b-instruct", FromVariantID: "q4-gguf",
		ToModelID: "qwen2.5-coder-0.5b-instruct", ToVariantID: "q4-gguf",
		MeasuredTokps: 8, FloorTokps: 30,
	}
}

func sampleRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		FromModelID: "heavy", FromVariantID: "q4",
		ToModelID: "light", ToVariantID: "q4-tiny",
		MeasuredTokps: 10, FloorTokps: 30,
	}
}

func TestPromptBenchmark_AcceptSwitches(t *testing.T) {
	stub := &benchStub{ready: true, rec: sampleRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("y\n"))
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 1 || stub.acceptedID != "light" {
		t.Errorf("accept = %d id=%q, want 1 / light", stub.acceptCount, stub.acceptedID)
	}
	if stub.dismissCount != 0 {
		t.Errorf("dismiss = %d, want 0", stub.dismissCount)
	}
}

func TestPromptBenchmark_DeclineDismisses(t *testing.T) {
	stub := &benchStub{ready: true, rec: sampleRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("n\n"))
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 {
		t.Errorf("accept = %d, want 0", stub.acceptCount)
	}
	if stub.dismissCount != 1 || stub.dismissFrom != "q4" || stub.dismissTo != "q4-tiny" {
		t.Errorf("dismiss = %d %q→%q, want 1 q4→q4-tiny", stub.dismissCount, stub.dismissFrom, stub.dismissTo)
	}
}

func TestPromptBenchmark_NonInteractiveNeither(t *testing.T) {
	stub := &benchStub{ready: true, rec: sampleRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	// stdin must NOT be consulted; pass an empty reader.
	if err := promptBenchmarkRecommendation(srv.URL, true, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 || stub.dismissCount != 0 {
		t.Errorf("non-interactive must neither accept (%d) nor dismiss (%d)", stub.acceptCount, stub.dismissCount)
	}
	if !strings.Contains(out.String(), "Non-interactive") {
		t.Errorf("expected a non-interactive notice, got: %q", out.String())
	}
}

func TestPromptBenchmark_NoRecommendationQuiet(t *testing.T) {
	stub := &benchStub{ready: true, rec: nil}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 || stub.dismissCount != 0 {
		t.Errorf("no recommendation must not accept/dismiss")
	}
	if !strings.Contains(out.String(), "Local inference works") {
		t.Errorf("expected an inference-works line, got: %q", out.String())
	}
}

// The benchmark must never return silently: every give-up path prints a
// reason (the "`waired runtimes benchmark` returns instantly with nothing"
// complaint). 404 (old daemon) and an unexpected status are the two paths
// that used to be silent.
func TestPromptBenchmark_NotFoundExplains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Errorf("404 path must print a reason, got empty output")
	}
	if !strings.Contains(out.String(), "doesn't support benchmarking") {
		t.Errorf("expected an unsupported-build notice, got: %q", out.String())
	}
}

func TestPromptBenchmark_UnexpectedStatusExplains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "Benchmark unavailable (HTTP 500)") {
		t.Errorf("expected an HTTP-500 notice, got: %q", out.String())
	}
}

func TestPromptBenchmark_TransportErrorExplains(t *testing.T) {
	// Point at a closed port so the POST fails at the transport layer.
	var out strings.Builder
	if err := promptBenchmarkRecommendation("http://127.0.0.1:1", false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "Could not reach the waired-agent service") {
		t.Errorf("expected an unreachable-service notice, got: %q", out.String())
	}
}

func TestPromptBenchmark_DismissedQuiet(t *testing.T) {
	rec := sampleRec()
	rec.Dismissed = true
	stub := &benchStub{ready: true, rec: rec}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 || stub.dismissCount != 0 {
		t.Errorf("dismissed recommendation must stay quiet")
	}
}

func TestPromptBenchmark_TerminalStateSkips(t *testing.T) {
	// /benchmark always 425; status says pull_failed → skip without hanging.
	stub := &benchStub{ready: false, state: "pull_failed"}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "download failed") {
		t.Errorf("expected pull_failed skip notice, got: %q", out.String())
	}
}

// When the only lighter step-down is the tiny 0.5B, declining (default No)
// disables local inference rather than switching / dismissing.
func TestPromptBenchmark_TinyDeclineDisables(t *testing.T) {
	stub := &benchStub{ready: true, rec: tinyRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("n\n")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.disableCount != 1 {
		t.Errorf("decline should disable local inference once, got %d", stub.disableCount)
	}
	if stub.acceptCount != 0 {
		t.Errorf("decline must not switch model, got accept=%d", stub.acceptCount)
	}
	if !strings.Contains(out.String(), "Local inference disabled") {
		t.Errorf("expected a disabled notice, got: %q", out.String())
	}
}

// Accepting the tiny-model dialog switches to the 0.5B (keeps local inference).
func TestPromptBenchmark_TinyAcceptSwitches(t *testing.T) {
	stub := &benchStub{ready: true, rec: tinyRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 1 || stub.acceptedID != "qwen2.5-coder-0.5b-instruct" {
		t.Errorf("accept = %d id=%q, want 1 / qwen2.5-coder-0.5b-instruct", stub.acceptCount, stub.acceptedID)
	}
	if stub.disableCount != 0 {
		t.Errorf("accepting must not disable inference, got %d", stub.disableCount)
	}
}

// Non-interactive must neither switch nor disable on a tiny-model recommendation.
func TestPromptBenchmark_TinyNonInteractiveNeither(t *testing.T) {
	stub := &benchStub{ready: true, rec: tinyRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, true, &out, strings.NewReader("")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 || stub.disableCount != 0 {
		t.Errorf("non-interactive must neither switch (%d) nor disable (%d)", stub.acceptCount, stub.disableCount)
	}
	if !strings.Contains(out.String(), "Non-interactive") {
		t.Errorf("expected a non-interactive notice, got: %q", out.String())
	}
}

// setBenchTiming shrinks the package-level benchmark / pull poll timings for
// a test and restores them afterwards, so the no_engine grace / poll loops
// run in milliseconds instead of minutes.
func setBenchTiming(t *testing.T, interval, grace, deadline time.Duration) {
	t.Helper()
	oi, og, od, op := benchPollInterval, benchNoEngineGrace, benchPollDeadline, pullPollInterval
	benchPollInterval, benchNoEngineGrace, benchPollDeadline, pullPollInterval = interval, grace, deadline, interval
	t.Cleanup(func() {
		benchPollInterval, benchNoEngineGrace, benchPollDeadline, pullPollInterval = oi, og, od, op
	})
}

// A transient `no_engine` (engine still coming up on a fresh bundled install,
// issue #489) must be waited out, not skipped: once the engine/model become
// ready within the grace window the benchmark — and the #133 lighter switch —
// must run.
func TestPromptBenchmark_TransientNoEngineThenRuns(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &benchStub{state: "no_engine", readyAfter: 2, rec: sampleRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.Contains(out.String(), "No inference engine available") {
		t.Errorf("transient no_engine must not skip immediately; got: %q", out.String())
	}
	if stub.acceptCount != 1 || stub.acceptedID != "light" {
		t.Errorf("expected the #133 lighter switch to run after the wait: accept=%d id=%q\nout=%q",
			stub.acceptCount, stub.acceptedID, out.String())
	}
}

// A `no_engine` that never resolves (the engine genuinely won't come up) must
// still give up — after the bounded grace, not the full deadline — and must
// not hang.
func TestPromptBenchmark_PersistentNoEngineSkipsAfterGrace(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 20*time.Millisecond, time.Minute)
	stub := &benchStub{state: "no_engine"} // /benchmark stays 425 forever
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	done := make(chan struct{})
	go func() {
		_ = promptBenchmarkRecommendation(srv.URL, false, &out, strings.NewReader(""))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForBenchmark hung on a persistent no_engine state")
	}
	if !strings.Contains(out.String(), "No inference engine available") {
		t.Errorf("expected the no_engine skip after the grace window, got: %q", out.String())
	}
}
