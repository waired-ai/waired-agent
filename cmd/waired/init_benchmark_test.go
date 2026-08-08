package main

import (
	"bufio"
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
	readyAfter int
	// failed makes /benchmark answer 503 benchmark_did_not_complete — the
	// benchmark RAN and did not finish, as opposed to 425 "not ready yet".
	failed       bool
	failedMsg    string
	active       *management.ActiveSelection         // /status Active (names the benchmarked model)
	measured     float64                             // /benchmark measured_tokps
	upgrade      *management.BenchmarkRecommendation // /benchmark upgrade suggestion
	downloading  bool                                // preferred-model response Downloading
	statusSeq    []statusStep                        // scripted /status sequence (last repeats)
	statusCalls  int
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
		if b.failed {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error_code": "benchmark_did_not_complete",
				"message":    b.failedMsg,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(management.BenchmarkRunResponse{
			Ran: true, MeasuredTokps: b.measured, Recommendation: b.rec, Upgrade: b.upgrade,
		})
	})
	mux.HandleFunc("/waired/v1/inference/status", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		i := b.statusCalls
		b.statusCalls++
		seq := b.statusSeq
		b.mu.Unlock()
		if len(seq) > 0 {
			if i >= len(seq) {
				i = len(seq) - 1
			}
			if seq[i].code != 0 && seq[i].code != http.StatusOK {
				w.WriteHeader(seq[i].code)
				return
			}
			_ = json.NewEncoder(w).Encode(seq[i].st)
			return
		}
		_ = json.NewEncoder(w).Encode(management.InferenceStatus{SubsystemState: b.state, Active: b.active})
	})
	mux.HandleFunc("/waired/v1/inference/preferred-model", func(w http.ResponseWriter, r *http.Request) {
		var req management.PreferredModelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.acceptedID = req.ModelID
		b.acceptCount++
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(management.PreferredModelResponse{
			ModelID: req.ModelID, WillRestart: true, Downloading: b.downloading,
		})
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

// tinyRec is a recommendation that steps down onto the lightest model we
// offer, which triggers the disable-or-proceed dialog instead of the
// neutral lighter-model switch: there is nothing lighter to fall back to
// after it, so the real question is whether to keep local inference at
// all.
//
// The target is qwen3.5-0.8b, the lightest offered entry since #200
// retired qwen2.5-coder-0.5b-instruct. Not cosmetic:
// isLightestOfferedModel is what selects the branch under test, and a
// target the catalog cannot resolve takes the OTHER one. Record of
// today's catalog.
//
// It used to say "below the install quality floor", which #522 removed —
// the branch is chosen by an ordering now, not a threshold.
func tinyRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		FromModelID: "qwen2.5-coder-3b-instruct", FromVariantID: "q4-gguf",
		ToModelID: "qwen3.5-0.8b", ToVariantID: "q8-gguf",
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
	err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false)
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
	err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("n\n")), false)
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
	if err := promptBenchmarkRecommendation(srv.URL, true, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
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
	stub := &benchStub{ready: true, rec: nil, measured: 120}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 0 || stub.dismissCount != 0 {
		t.Errorf("no recommendation must not accept/dismiss")
	}
	if !strings.Contains(out.String(), "Local inference works") {
		t.Errorf("expected an inference-works line, got: %q", out.String())
	}
}

// TestPromptBenchmark_OldDaemonWithoutRateIsNeutral covers the other half of
// the split: a 200 carrying NO measured_tokps. That is now only reachable
// from an OLDER daemon (a current one 503s a failed run), so the wording
// states what is actually known — a generation ran — without claiming local
// inference works.
//
// This test and the one above replace a single case that asserted the green
// "Local inference works" line for measured_tokps == 0. That assertion
// pinned the defect: a benchmark whose warm-up got an engine 500 also
// reports 0, so a dead engine printed a success line (waired-agent#29).
func TestPromptBenchmark_OldDaemonWithoutRateIsNeutral(t *testing.T) {
	stub := &benchStub{ready: true, rec: nil} // measured stays 0
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "Local inference works") {
		t.Errorf("a daemon that reports no rate must not claim inference works: %q", got)
	}
	if !strings.Contains(got, "does not report a throughput figure") {
		t.Errorf("expected the neutral old-daemon wording, got: %q", got)
	}
}

// TestPromptBenchmark_FailedBenchmarkPrintsNoSuccessLine is the direct
// regression test for waired-agent#29: the daemon reports that the benchmark
// ran and failed, and init must say so instead of printing a green line.
//
// PRODUCT CONTRACT.
func TestPromptBenchmark_FailedBenchmarkPrintsNoSuccessLine(t *testing.T) {
	stub := &benchStub{
		ready:     true,
		failed:    true,
		failedMsg: "warm-up failed: HTTP 500: llama-server process has terminated",
	}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := out.String()
	for _, forbidden := range []string{"Local inference works", "tok/s", "looks good"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("a failed benchmark must not print %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "could not complete a test generation") {
		t.Errorf("expected the failure line, got: %q", got)
	}
	// The engine's own reason must survive to the operator.
	if !strings.Contains(got, "llama-server process has terminated") {
		t.Errorf("expected the daemon's reason to be surfaced, got: %q", got)
	}
	if !strings.Contains(got, "waired doctor") {
		t.Errorf("expected a pointer at the diagnosis tools, got: %q", got)
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "Benchmark unavailable (HTTP 500)") {
		t.Errorf("expected an HTTP-500 notice, got: %q", out.String())
	}
}

func TestPromptBenchmark_TransportErrorExplains(t *testing.T) {
	// Point at a closed port so the POST fails at the transport layer.
	var out strings.Builder
	if err := promptBenchmarkRecommendation("http://127.0.0.1:1", false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "download failed") {
		t.Errorf("expected pull_failed skip notice, got: %q", out.String())
	}
}

// PRODUCT CONTRACT: a subsystem that is switched off or parked never
// produces a ready model, so the wait must end at the first poll — the
// same call waitForBundledModel already makes (init_pull.go).
//
// These two states used to fall through to "engine is up, a download must
// be in flight", so `waired init --inference-enabled=false` on the daemon
// path printed "Waiting for the model to finish downloading…" and held the
// terminal for the full ten-minute deadline before reporting that it had
// given up — on a host with no model and no intention of getting one.
// Measured: three installtest legs, ten minutes each, every PR.
func TestPromptBenchmark_OffSubsystemSkipsImmediately(t *testing.T) {
	for _, state := range []string{"disabled", "stopped"} {
		t.Run(state, func(t *testing.T) {
			stub := &benchStub{ready: false, state: state}
			srv := stub.server()
			defer srv.Close()

			var out strings.Builder
			done := make(chan error, 1)
			go func() {
				done <- promptBenchmarkRecommendation(srv.URL, false, &out,
					bufio.NewScanner(strings.NewReader("")), false)
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("prompt: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("still waiting: an off subsystem must not be waited out")
			}

			if got := out.String(); !strings.Contains(got, "Local inference is off") {
				t.Errorf("expected the off-subsystem skip notice, got: %q", got)
			}
			if got := out.String(); strings.Contains(got, "finish downloading") {
				t.Errorf("announced a model download for state %q: %q", state, got)
			}
			// One probe of each endpoint is enough to decide; anything more
			// means the loop kept going after a terminal answer.
			stub.mu.Lock()
			calls := stub.benchCalls
			stub.mu.Unlock()
			if calls != 1 {
				t.Errorf("/benchmark polled %d times, want 1", calls)
			}
		})
	}
}

// When the only lighter step-down is the tiny 0.5B, declining (default No)
// disables local inference rather than switching / dismissing.
func TestPromptBenchmark_TinyDeclineDisables(t *testing.T) {
	stub := &benchStub{ready: true, rec: tinyRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("n\n")), false); err != nil {
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stub.acceptCount != 1 || stub.acceptedID != "qwen3.5-0.8b" {
		t.Errorf("accept = %d id=%q, want 1 / qwen3.5-0.8b", stub.acceptCount, stub.acceptedID)
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
	if err := promptBenchmarkRecommendation(srv.URL, true, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
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
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
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
		_ = promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false)
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

// realRec is a lighter recommendation between two real bundled-catalog models,
// so the display resolves labels and quality tiers (waired#773).
func realRec() *management.BenchmarkRecommendation {
	return &management.BenchmarkRecommendation{
		FromModelID: "qwen3-coder-30b-a3b-instruct", FromVariantID: "q4-gguf",
		ToModelID: "qwen3.6-27b", ToVariantID: "q4-gguf",
		MeasuredTokps: 43, FloorTokps: 100,
	}
}

// Every benchmark line must name the model it talks about: the slow
// headline names the benchmarked (from) model and the suggestion names
// the from → to pair (waired#773).
//
// The names used to carry "(quality N)" beside them. #537 removed the
// figure — after #518 it is arithmetic over two catalog fields, not a
// measurement — and this flow says which direction it is offering in its
// own prose instead of leaving a reader to compare two numbers.
func TestPromptBenchmark_NamesFromTo(t *testing.T) {
	stub := &benchStub{ready: true, rec: realRec()}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("n\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Qwen3 Coder 30B-A3B Instruct measured 43 tok/s",
		"Recommend switching Qwen3 Coder 30B-A3B Instruct → Qwen3.6 27B",
		// The direction, which the numbers used to carry.
		"the lighter model should run more smoothly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "quality 65") || strings.Contains(got, "quality 70") {
		t.Errorf("output still prints a quality figure (#537); got:\n%s", got)
	}
}

// The no-recommendation "works" line names the benchmarked model, resolved
// from /inference/status Active (the benchmark response carries no model id).
func TestPromptBenchmark_WorksLineNamesActiveModel(t *testing.T) {
	stub := &benchStub{ready: true, measured: 120,
		active: &management.ActiveSelection{ModelID: "qwen3-coder-30b-a3b-instruct", VariantID: "q4-gguf"}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	want := "Local inference works — Qwen3 Coder 30B-A3B Instruct measured 120 tok/s"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q; got:\n%s", want, out.String())
	}
}

// When /status carries no Active selection (old daemon), the works line keeps
// the model-less wording rather than printing an empty name.
func TestPromptBenchmark_WorksLineFallsBackWhenActiveUnknown(t *testing.T) {
	stub := &benchStub{ready: true, measured: 120}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	want := "Local inference works — measured 120 tok/s"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q; got:\n%s", want, out.String())
	}
}

// The upgrade recommendation names the from → to pair and contrasts
// predicted vs measured throughput.
//
// It also has to SAY that the target is the stronger model. The line is
// otherwise entirely about speed, and this flow is the one that offers a
// multi-GB download — with the quality figures gone (#537) nothing else
// in it tells the reader what they would be getting.
func TestPromptBenchmark_UpgradeNamesFromTo(t *testing.T) {
	upgrade := &management.BenchmarkRecommendation{
		Direction:   "upgrade",
		FromModelID: "qwen3.6-27b", FromVariantID: "q4-gguf",
		ToModelID: "qwen3.6-35b-a3b", ToVariantID: "q4-gguf",
		MeasuredTokps: 140, FloorTokps: 100, PredictedTokps: 110,
	}
	stub := &benchStub{ready: true, measured: 140, upgrade: upgrade}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("n\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	want := "Qwen3.6 35B-A3B is a stronger model and is predicted to run at ~110 tok/s here (vs 140 tok/s measured on Qwen3.6 27B)"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q; got:\n%s", want, out.String())
	}
	if strings.Contains(out.String(), "quality 89") || strings.Contains(out.String(), "quality 70") {
		t.Errorf("upgrade line still prints a quality figure (#537); got:\n%s", out.String())
	}
}

// statusStep is one scripted /inference/status response: code 0/200 encodes
// st; any other code is returned bare (e.g. 500 during the restart window).
type statusStep struct {
	code int
	st   management.InferenceStatus
}

func switchDownloading(modelID string, completed, total int64) statusStep {
	return statusStep{st: management.InferenceStatus{
		SubsystemState: "downloading",
		Models: management.ModelsSnapshot{
			Downloading: []string{modelID},
			Downloads:   []management.ModelDownload{{Model: modelID, CompletedBytes: completed, TotalBytes: total}},
		},
	}}
}

func switchReady(modelID string) statusStep {
	return statusStep{st: management.InferenceStatus{
		SubsystemState: "ready",
		Models:         management.ModelsSnapshot{Ready: []string{modelID}},
	}}
}

func switchFailed(modelID string) statusStep {
	return statusStep{st: management.InferenceStatus{
		SubsystemState: "downloading",
		Models:         management.ModelsSnapshot{Failed: []string{modelID}},
	}}
}

// Accepting a switch whose target still needs a download foreground-waits
// for it, tolerating the restart window (status 500s) the accept schedules,
// and reports the model ready (waired#774).
func TestAcceptSwitch_WaitsThroughRestartThenReady(t *testing.T) {
	setBenchTiming(t, time.Millisecond, time.Second, 30*time.Second)
	const target = "qwen3.6-27b"
	stub := &benchStub{ready: true, rec: realRec(), downloading: true,
		statusSeq: []statusStep{
			{code: http.StatusInternalServerError},
			{code: http.StatusInternalServerError},
			switchDownloading(target, 1<<30, 4<<30),
			switchReady(target),
		}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	// The Enter escape is offered only when this run owns stdin, which on
	// a real terminal it does (#223).
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, newStdinReader(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := out.String()
	if stub.acceptCount != 1 || stub.acceptedID != target {
		t.Errorf("accept = %d id=%q, want 1 / %s", stub.acceptCount, stub.acceptedID, target)
	}
	for _, want := range []string{
		"Press Enter anytime",
		"Waiting for the agent to restart",
		"Qwen3.6 27B ready — the agent is now serving it",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// Pressing Enter during the wait backgrounds the download and explains how
// to keep watching it.
func TestAcceptSwitch_EnterBackgrounds(t *testing.T) {
	setBenchTiming(t, time.Millisecond, time.Second, 30*time.Second)
	const target = "qwen3.6-27b"
	stub := &benchStub{ready: true, rec: realRec(), downloading: true,
		statusSeq: []statusStep{switchDownloading(target, 1<<30, 4<<30)}} // never ready
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	done := make(chan struct{})
	go func() {
		// "y" accepts the switch; the second line is the backgrounding Enter.
		_ = promptBenchmarkRecommendation(srv.URL, false, &out, newStdinReader(strings.NewReader("y\n\n")), false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enter did not background the wait")
	}
	if !strings.Contains(out.String(), "Continuing in the background") {
		t.Errorf("expected the background note, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "waired models ls") {
		t.Errorf("expected the models-ls hint, got:\n%s", out.String())
	}
}

// Off a terminal there is no owner, so no Enter escape is offered — and
// none is possible: a scripted stdin that ran out of lines never
// backgrounded anything even before #223. The wait still runs to
// completion, which is the behaviour `waired runtimes benchmark | tee`
// and the installtest legs have always had.
func TestAcceptSwitch_NoOwnerOffersNoEscape(t *testing.T) {
	setBenchTiming(t, time.Millisecond, time.Second, 30*time.Second)
	const target = "qwen3.6-27b"
	stub := &benchStub{ready: true, rec: realRec(), downloading: true,
		statusSeq: []statusStep{
			switchDownloading(target, 1<<30, 4<<30),
			switchReady(target),
		}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out,
		bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "Press Enter anytime") {
		t.Errorf("offered an Enter escape with no terminal to press it on:\n%s", got)
	}
	if !strings.Contains(got, "downloading it now") {
		t.Errorf("missing the switch narration:\n%s", got)
	}
}

// A target that is already on disk skips the wait entirely.
func TestAcceptSwitch_AlreadyDownloadedSkipsWait(t *testing.T) {
	stub := &benchStub{ready: true, rec: realRec(), downloading: false}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "already downloaded") {
		t.Errorf("expected the already-downloaded fast path, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Continuing in the background") {
		t.Errorf("must not enter the wait when nothing is downloading:\n%s", out.String())
	}
}

// A persistently failed download is terminal — with a retry hint — but only
// after several consecutive observations.
func TestAcceptSwitch_PersistentFailureExplains(t *testing.T) {
	setBenchTiming(t, time.Millisecond, time.Second, 30*time.Second)
	const target = "qwen3.6-27b"
	stub := &benchStub{ready: true, rec: realRec(), downloading: true,
		statusSeq: []statusStep{switchFailed(target)}} // fails forever
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.Contains(out.String(), "Download failed") ||
		!strings.Contains(out.String(), "waired models pull qwen3.6-27b") {
		t.Errorf("expected the terminal-failure hint, got:\n%s", out.String())
	}
}

// A single transient failed record (the cancelled pre-restart pull) must not
// abort the wait: the post-restart bootstrap retries and the model readies.
func TestAcceptSwitch_TransientFailureRecovers(t *testing.T) {
	setBenchTiming(t, time.Millisecond, time.Second, 30*time.Second)
	const target = "qwen3.6-27b"
	stub := &benchStub{ready: true, rec: realRec(), downloading: true,
		statusSeq: []statusStep{
			switchFailed(target),
			switchDownloading(target, 2<<30, 4<<30),
			switchReady(target),
		}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if err := promptBenchmarkRecommendation(srv.URL, false, &out, bufio.NewScanner(strings.NewReader("y\n")), false); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if strings.Contains(out.String(), "Download failed") {
		t.Errorf("one transient failed tick must not be terminal:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ready — the agent is now serving it") {
		t.Errorf("expected the wait to reach ready, got:\n%s", out.String())
	}
}
