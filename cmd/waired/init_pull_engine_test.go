package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// engineFailedSnap is the daemon's answer while the engine is down: the
// subsystem state plus the runtime's own reason, which is where the
// daemon folds the engine's stderr tail (so lastErr is routinely
// multi-line — the fixtures below keep that shape).
func engineFailedSnap(lastErr string) management.InferenceStatus {
	return management.InferenceStatus{
		SubsystemState: "engine_failed",
		Runtimes: map[string]management.RuntimeStatus{
			"ollama": {Name: "ollama", Installed: true, State: "failed", LastError: lastErr},
		},
	}
}

const killedDetail = "ollama: process exited during startup: signal: killed\n" +
	"--- ollama serve stderr (tail, full log: /var/lib/waired/runtimes/ollama/engine.log) ---\n" +
	"time=2026-08-02T02:52:04Z level=INFO source=routes.go:1205 msg=\"server config\""

// T1. Before this arm existed, engine_failed matched nothing in the wait's
// switch and fell to `default:`, which renders the generic "Preparing the
// model…" — so a device whose engine could not start waited out its whole
// budget with no error at all (#310). It must instead give up on the
// no_engine grace and say what the daemon knows.
//
// Product contract.
func TestWaitForBundledModel_EngineFailedIsReportedAndBounded(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 30*time.Millisecond, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{engineFailedSnap(killedDetail)}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var res modelWaitResult
	done := make(chan struct{})
	go func() {
		res = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForBundledModel hung on a persistent engine_failed state")
	}

	if res.ready {
		t.Error("a persistent engine_failed must not report the model ready")
	}
	if res.engineFailure == "" {
		t.Error("the wait must report WHY it stopped, so the caller can pick the right summary")
	}
	s := out.String()
	if !strings.Contains(s, "The AI engine failed to start") {
		t.Errorf("expected the engine failure headline, got: %q", s)
	}
	// The whole reason, not a first line: the operator is looking at the
	// terminal at exactly this moment, and the alternative on the host
	// that prompted #310 was no information whatsoever.
	if !strings.Contains(s, killedDetail) {
		t.Errorf("expected the engine's own reason verbatim, got: %q", s)
	}
	if !strings.Contains(s, "waired doctor") {
		t.Errorf("expected an actionable diagnostics hint, got: %q", s)
	}
	if strings.Contains(s, "Preparing the model") {
		t.Errorf("engine_failed must not fall through to the generic prep line, got: %q", s)
	}
}

// T2. The load-bearing one. The daemon reports `starting` for any restart
// in flight — that arm sits ahead of engine_failed in its own derivation —
// so a crash-recovery cycle alternates between the two states. rc7 saw
// exactly that as "Engine starting… / Preparing the model…" forever.
//
// Two things have to hold: the alternation must not re-arm the grace (or
// the wait never ends), and `starting` must not get its own line back
// once a failure has been seen (or the alternation simply returns wearing
// different words).
//
// Product contract.
func TestWaitForBundledModel_EngineFailedFlappingWithStartingStillGivesUp(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 30*time.Millisecond, time.Minute)
	// Long enough that the alternation outlives the grace; pullStub
	// repeats the last entry, and either state is a fine one to end on.
	var seq []management.InferenceStatus
	for range 400 {
		seq = append(seq,
			engineFailedSnap(killedDetail),
			management.InferenceStatus{SubsystemState: "starting"},
		)
	}
	stub := &pullStub{seq: seq}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var res modelWaitResult
	done := make(chan struct{})
	go func() {
		res = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a flapping engine re-armed the grace forever — the wait never ended")
	}

	if res.engineFailure == "" {
		t.Error("a flapping engine is still a failed engine")
	}
	s := out.String()
	if !strings.Contains(s, "The AI engine failed to start") {
		t.Errorf("expected the engine failure headline, got: %q", s)
	}
	// The restart states are folded into this arm once a failure has been
	// seen; getting "Engine starting…" back here is the rc7 alternation.
	if strings.Contains(s, "Engine starting") {
		t.Errorf("a restart mid-failure must not narrate itself as an ordinary start, got: %q", s)
	}
	if n := strings.Count(s, "The AI engine won't start"); n != 1 {
		t.Errorf("the transitional line must print once, printed %d times: %q", n, s)
	}
}

// T3. One observation is not terminal: the daemon restarts the engine on a
// bounded budget and routinely recovers, and a wait that gave up on the
// first failure would abandon a download that was about to start.
//
// Product contract.
func TestWaitForBundledModel_TransientEngineFailureDoesNotEndTheWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	stub := &pullStub{seq: []management.InferenceStatus{
		engineFailedSnap(killedDetail),
		management.InferenceStatus{SubsystemState: "starting"},
		downloadingSnap("qwen", 1*mb, 4*mb),
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	res := waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil)
	if !res.ready {
		t.Fatalf("a recovered engine must still reach ready; out=%q", out.String())
	}
	if res.engineFailure != "" {
		t.Errorf("a recovered engine is not a reported fault, got %q", res.engineFailure)
	}
	if strings.Contains(out.String(), "The AI engine failed to start") {
		t.Errorf("must not announce a failure it recovered from, got: %q", out.String())
	}
}

// T4. An older daemon, or a failure the adapter could not describe, leaves
// no reason on the wire. The wait must still say what to do — and must
// still tell the caller a fault happened, or the closing summary falls
// back to celebrating.
//
// Product contract.
func TestWaitForBundledModel_EngineFailedWithNoRecordedReasonStillSaysWhatToDo(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 30*time.Millisecond, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{{SubsystemState: "engine_failed"}}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var res modelWaitResult
	done := make(chan struct{})
	go func() {
		res = waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForBundledModel hung on engine_failed with no runtime detail")
	}

	if res.engineFailure == "" {
		t.Error("a failure with no recorded reason is still a failure the caller must hear about")
	}
	s := out.String()
	if !strings.Contains(s, "The AI engine failed to start") {
		t.Errorf("expected the engine failure headline, got: %q", s)
	}
	if !strings.Contains(s, "waired doctor") {
		t.Errorf("expected an actionable diagnostics hint, got: %q", s)
	}
	if strings.Contains(s, "ollama") {
		t.Errorf("must not invent a runtime name it has no reason for, got: %q", s)
	}
}

// T5. NEGATIVE CONTROL, and it passes against the unfixed code — it is here
// to stop the fix from over-reaching, not to prove it landed. `starting`
// before any failure is an ordinary engine start and keeps its own line;
// folding it in unconditionally would take that narration away from every
// first run.
//
// Product contract.
func TestWaitForBundledModel_PlainStartingKeepsItsOwnLine(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{
		{SubsystemState: "starting"},
		{SubsystemState: "starting"},
		{SubsystemState: "ready", Active: activeSel("qwen"), Models: management.ModelsSnapshot{Ready: []string{"qwen"}}},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	if !waitForBundledModel(srv.URL, &out, false, benchPollDeadline, false, nil, nil, nil).ready {
		t.Fatalf("expected ready; out=%q", out.String())
	}
	s := out.String()
	if !strings.Contains(s, "Engine starting") {
		t.Errorf("an ordinary start must still narrate itself, got: %q", s)
	}
	if strings.Contains(s, "won't start") {
		t.Errorf("a start with no failure behind it must not be reported as one, got: %q", s)
	}
}

// T6. The reason is read out of a map, and the line an operator reads must
// not depend on Go's iteration order.
//
// Product contract.
func TestEngineFailureDetailNamesTheFailedRuntime(t *testing.T) {
	st := management.InferenceStatus{Runtimes: map[string]management.RuntimeStatus{
		"vllm":   {State: "failed", LastError: "vllm reason"},
		"ollama": {State: "failed", LastError: "ollama reason"},
		"tgi":    {State: "ready"},
	}}
	// Repeated because one pass over a Go map can agree with a sorted
	// order by luck; the assertion is that every pass does.
	for range 100 {
		if got := engineFailureDetail(st); got != "ollama: ollama reason" {
			t.Fatalf("engineFailureDetail = %q, want the alphabetically first failed runtime", got)
		}
	}

	if got := engineFailureDetail(management.InferenceStatus{Runtimes: map[string]management.RuntimeStatus{
		"ollama": {State: "failed"}, // failed, but said nothing
	}}); got != "" {
		t.Errorf("a runtime with no recorded reason must yield no line, got %q", got)
	}
	if got := engineFailureDetail(management.InferenceStatus{Runtimes: map[string]management.RuntimeStatus{
		"ollama": {State: "ready", LastError: "stale, from a failure it recovered from"},
	}}); got != "" {
		t.Errorf("a healthy runtime's stale error must not be reported, got %q", got)
	}
}

// T7. waitForModelSwitch reads no subsystem state at all, so a dead engine
// used to cost it the full benchPollDeadline — ten minutes of "Preparing to
// download …" for a download with nothing to run on. This is also this
// function's first test.
//
// Product contract.
func TestWaitForModelSwitch_EngineFailedStopsTheWait(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 30*time.Millisecond, time.Minute)
	stub := &pullStub{seq: []management.InferenceStatus{engineFailedSnap(killedDetail)}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	var ready bool
	done := make(chan struct{})
	go func() {
		ready = waitForModelSwitch(srv.URL, "qwen3.6-27b", &out, false, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForModelSwitch hung on a persistent engine_failed state")
	}

	if ready {
		t.Error("a dead engine must not be reported as serving the switch target")
	}
	s := out.String()
	if !strings.Contains(s, "The AI engine failed to start") {
		t.Errorf("expected the engine failure headline, got: %q", s)
	}
	if !strings.Contains(s, killedDetail) {
		t.Errorf("expected the engine's own reason, got: %q", s)
	}
}
