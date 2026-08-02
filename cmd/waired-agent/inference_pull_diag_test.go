package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// diagRunner replays a script of `ollama pull` OUTPUT as well as a script
// of results — the piece every other runner in this package is missing.
// noopRunner, scriptedRunner and failingRunner either emit nothing or
// emit "success", so nothing in the tree exercises what the engine says
// on its way out, which is exactly the text #307 is about.
//
// concurrent mirrors DefaultRunner, which scans stdout and stderr on two
// goroutines and calls onLine from both (internal/download/ollama.go).
// Every other fake calls it from one, so without this flag the capture's
// mutex is unwritable — CLAUDE.md §Test discipline, "put the seam below
// the behaviour under test".
type diagRunner struct {
	mu         sync.Mutex
	lines      [][]string // per attempt; the last entry repeats
	results    []error    // per attempt; the last entry repeats
	concurrent bool
	calls      int
}

func (r *diagRunner) Run(_ context.Context, _ string, _, _ []string, onLine func(string)) error {
	r.mu.Lock()
	i := r.calls
	r.calls++
	var lines []string
	if n := len(r.lines); n > 0 {
		lines = r.lines[min(i, n-1)]
	}
	var err error
	if n := len(r.results); n > 0 {
		err = r.results[min(i, n-1)]
	}
	r.mu.Unlock()

	if !r.concurrent {
		for _, l := range lines {
			onLine(l)
		}
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for half := range 2 {
		go func(half int) {
			defer wg.Done()
			for j := half; j < len(lines); j += 2 {
				onLine(lines[j])
			}
		}(half)
	}
	wg.Wait()
	return err
}

func (r *diagRunner) attempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// exitStatus1 is what a real `ollama pull` failure returns: cmd.Wait()'s
// *exec.ExitError, carrying no diagnosis at all. Every fixture below uses
// it, because assuming anything richer is precisely the mistake that let
// "check its internet connection" ship (#307).
func exitStatus1() error { return errors.New("exit status 1") }

// PRODUCT CONTRACT: a failed pull records what the engine SAID, not only
// that it exited non-zero.
//
// The fixture line is indented, because CommandRunner promises only "every
// line the command writes" — DefaultRunner happens to trim, another
// implementation need not, and an untrimmed line would fail the "error"
// prefix test and be dropped silently.
func TestRunPullJob_RecordsTheEnginesDiagnosticNotJustTheExitStatus(t *testing.T) {
	r := &diagRunner{
		lines:   [][]string{{"   Error: could not connect to ollama app, is it running?"}},
		results: []error{exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "model-a").Error
	if !strings.Contains(got, "could not connect to ollama app") {
		t.Errorf("recorded error = %q, want the engine's own diagnosis", got)
	}
	// The exit status is kept too: it is the only part that is certainly
	// about this attempt.
	if !strings.Contains(got, "exit status 1") {
		t.Errorf("recorded error = %q, want the pull's own error kept as well", got)
	}
}

// PRODUCT CONTRACT: only lines announcing an error are captured.
//
// The masking line here is a progress redraw that still carries its
// cursor-control prefix, so parseProgressLine does NOT recognise it —
// "pulling" is no longer at the front. That is the whole reason the
// capture tests for "error" rather than for "the parser gave up": a
// naive last-unrecognised-line rule hands the operator a fragment of a
// progress bar instead of the reason the download died.
//
// Ordering matters too — the fragment arrives AFTER the error, which is
// what a bar being torn down does.
func TestRunPullJob_KeepsTheDiagnosticNotTheProgressLine(t *testing.T) {
	redraw := "\x1b[2K\x1b[1Gpulling 5b0e2c1f: 47% ▕██▏ 1.2 GB/2.5 GB 40 MB/s"
	r := &diagRunner{
		lines: [][]string{{
			"Error: could not connect to ollama app, is it running?",
			redraw,
		}},
		results: []error{exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "model-a").Error
	if !strings.Contains(got, "could not connect") {
		t.Errorf("recorded error = %q, want the error line", got)
	}
	if strings.Contains(got, "5b0e2c1f") || strings.Contains(got, "40 MB/s") {
		t.Errorf("recorded error = %q, want the progress line left out", got)
	}
}

// RECORD OF TODAY'S BEHAVIOUR, not a product contract: when the engine
// prints more than one error line, the LAST one is kept. That is a guess
// about ollama's output order — the last line is usually the fatal one
// and the earlier ones its causes — and nothing in the tree proves it.
// If a real transcript ever shows otherwise, change this test with it.
func TestRunPullJob_KeepsTheLastDiagnosticLine(t *testing.T) {
	r := &diagRunner{
		lines: [][]string{{
			"Error: pull model manifest: file does not exist",
			"Error: could not connect to ollama app, is it running?",
		}},
		results: []error{exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "model-a").Error
	if !strings.Contains(got, "could not connect") {
		t.Errorf("recorded error = %q, want the last error line", got)
	}
	if strings.Contains(got, "file does not exist") {
		t.Errorf("recorded error = %q, want only one error line", got)
	}
}

// PRODUCT CONTRACT: the capture is bounded, and the bound never splits a
// rune. The scanner behind a real pull admits a 1 MiB token, and since
// this text carries the engine's stderr it routinely holds multibyte
// glyphs and non-ASCII usernames out of Windows paths — a byte-indexed
// cut would surface as mojibake in the wizard, never as an error.
func TestRunPullJob_ClampsTheCapturedDiagnostic(t *testing.T) {
	// Sized so a naive s[:pullDiagnosticMax] lands INSIDE a 3-byte rune.
	long := "Error: " + strings.Repeat("あ", pullDiagnosticMax)
	if utf8.RuneStart(long[pullDiagnosticMax]) {
		t.Fatalf("fixture does not straddle a rune at byte %d; the clamp mutant would survive", pullDiagnosticMax)
	}
	r := &diagRunner{lines: [][]string{{long}}, results: []error{exitStatus1()}}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "model-a").Error
	if !utf8.ValidString(got) {
		t.Errorf("recorded error is not valid UTF-8 — the clamp split a rune")
	}
	if len(got) > len("exit status 1; ")+pullDiagnosticMax {
		t.Errorf("recorded error is %d bytes, want the capture clamped to %d", len(got), pullDiagnosticMax)
	}
}

// PRODUCT CONTRACT: the recorded failure describes the attempt that
// produced it. Hoisting the capture out of the retry loop would report
// attempt 1's transient as attempt 3's cause — the most plausible way to
// write this wrong.
func TestRunPullJob_ResetsTheDiagnosticBetweenAttempts(t *testing.T) {
	r := &diagRunner{
		lines: [][]string{
			{"Error: could not connect to ollama app, is it running?"},
			{}, {},
		},
		results: []error{exitStatus1(), exitStatus1(), exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	if got := r.attempts(); got != modelPullAttempts {
		t.Fatalf("attempts = %d, want %d", got, modelPullAttempts)
	}
	got := modelStateOf(t, p, "model-a").Error
	if strings.Contains(got, "could not connect") {
		t.Errorf("recorded error = %q, want attempt 1's diagnosis dropped — the last attempt said nothing", got)
	}
}

// TestRunPullJob_CapturesDiagnosticsFromBothOutputStreams is the only
// test that makes the capture's concurrency reachable: the real runner
// calls onLine from two goroutines.
//
// The kill is a DATA RACE, so it only fails under `go test -race`, which
// CI does not run. Verified locally; see the PR body.
func TestRunPullJob_CapturesDiagnosticsFromBothOutputStreams(t *testing.T) {
	var emitted []string
	for i := range 200 {
		emitted = append(emitted, "Error: engine stream line "+strings.Repeat("x", i%17)+" done")
	}
	r := &diagRunner{
		concurrent: true,
		lines:      [][]string{emitted},
		results:    []error{exitStatus1()},
	}
	p := retryProvider(t, r)

	if _, err := p.PullModel(context.Background(), "model-a"); err != nil {
		t.Fatalf("PullModel: %v", err)
	}
	p.waitForPulls()

	got := modelStateOf(t, p, "model-a").Error
	kept, ok := strings.CutPrefix(got, "exit status 1; ")
	if !ok {
		t.Fatalf("recorded error = %q, want the exit status then one captured line", got)
	}
	// Which line wins is genuinely nondeterministic — two goroutines race
	// to be last. What must hold is that the winner is a WHOLE line.
	if !slicesContains(emitted, kept) {
		t.Errorf("captured line = %q, want one of the emitted lines verbatim (torn write)", kept)
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
