//go:build windows

package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

// PRODUCT CONTRACT (waired-agent#684): a restart the agent asked for is
// reported to the SCM as itself.
//
// Execute used to return a hardcoded (false, 1) whenever run() came back,
// and nothing in the tree ever set ServiceSpecificExitCode — so exit 17
// was structurally invisible on Windows. Worse, the old restart path
// never let run() return at all: it called os.Exit(1) from inside the
// service process, which the SCM read as a hard crash. That skipped the
// daemon's graceful teardown and burned one of the three recovery slots
// inside the 5-minute reset window.
//
// svcSpecificEC=true is what makes x/sys report
// ERROR_SERVICE_SPECIFIC_ERROR with ServiceSpecificExitCode=17. It is
// still a non-zero stop, so the recovery actions applyRecoveryConfig
// installs fire — #315's SetRecoveryActionsOnNonCrashFailures(true) is
// what makes a clean STOPPED count as a failure at all.
func TestSvcHandlerExecute_ReportsARequestedRestartAsExit17(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })
	prevDelay := restartRequestDelay
	restartRequestDelay = 0
	t.Cleanup(func() { restartRequestDelay = prevDelay })

	// run blocks until the handler's context is cancelled, which is what
	// osRequestRestart does under the SCM.
	h := &svcHandler{run: func(ctx context.Context, _ []string) error {
		<-ctx.Done()
		return nil
	}}
	requests := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 16)

	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := h.Execute(nil, requests, status)
		done <- result{specific, code}
	}()

	// Wait for Execute to publish its stop lever, then go through the
	// real request path — that RequestRestart REACHES this handler is
	// half of what is under test.
	deadline := time.Now().Add(10 * time.Second)
	for scmStop.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Execute never published its stop lever")
		}
		time.Sleep(time.Millisecond)
	}
	RequestRestart()

	select {
	case got := <-done:
		if !got.specific || got.code != RestartRequestedExitCode {
			t.Fatalf("Execute returned (%v, %d), want (true, %d)",
				got.specific, got.code, RestartRequestedExitCode)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Execute did not return after a restart request")
	}
	if scmStop.Load() != nil {
		t.Error("the stop lever outlived Execute; a later request would cancel a dead context")
	}
}

// Records today's behaviour: a run() that returns on its own, with no
// restart requested, is still the plain failure exit. Recovery fires for
// that too (#315), but it must not claim to be a requested restart — or
// `sc queryex` would name 17 for a crash.
func TestSvcHandlerExecute_AnUnrequestedExitIsNotReportedAs17(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })

	h := &svcHandler{run: func(context.Context, []string) error { return nil }}
	requests := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 16)

	specific, code := h.Execute(nil, requests, status)
	if specific || code != 1 {
		t.Fatalf("Execute returned (%v, %d), want (false, 1)", specific, code)
	}
}

// PRODUCT CONTRACT (waired-agent#684): a stop the SCM asked for is a
// clean exit, even when a restart was requested first. `net stop` while a
// model switch is in flight must leave the service stopped, not
// restarted by the recovery actions.
func TestSvcHandlerExecute_AnSCMStopStaysAClean0(t *testing.T) {
	restartRequested.Store(true)
	t.Cleanup(func() { restartRequested.Store(false) })

	h := &svcHandler{run: func(ctx context.Context, _ []string) error {
		<-ctx.Done()
		return nil
	}}
	requests := make(chan svc.ChangeRequest, 1)
	status := make(chan svc.Status, 16)

	type result struct {
		specific bool
		code     uint32
	}
	done := make(chan result, 1)
	go func() {
		specific, code := h.Execute(nil, requests, status)
		done <- result{specific, code}
	}()

	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	select {
	case got := <-done:
		if got.specific || got.code != 0 {
			t.Fatalf("Execute returned (%v, %d) for an SCM stop, want (false, 0)", got.specific, got.code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Execute did not return after an SCM stop")
	}
}

// PRODUCT CONTRACT (waired-agent#855): Execute never reports Stopped
// itself. The SCM's own report, written by x/sys on the way out of
// serviceMain, is the only one that carries the exit code.
//
// The issue this pins: a supervised restart on Windows stopped the
// service and the SCM never brought it back — 3m18s down on a real host
// until someone ran Start-Service — while every designed piece was
// present and correct (three RESTART recovery actions, the #315 non-crash
// flag, and the event-log line naming exit 17). x/sys's updateStatus
// derives Win32ExitCode from Execute's RETURN values and ignores the
// Win32ExitCode field of the svc.Status pushed down this channel, so a
// Stopped pushed from inside Execute reaches the SCM as
// SetServiceStatus(SERVICE_STOPPED, dwWin32ExitCode = 0). The SCM
// finalises the service on the first Stopped it sees, and recovery
// actions only fire for a stop that reports a non-zero code.
//
// Measured, not reasoned: a throwaway service with the same recovery
// configuration, differing only in this push, was restarted by the SCM
// 5s after exiting (`sc queryex` WIN32_EXIT_CODE 1066 / SERVICE_EXIT_CODE
// 17) and stayed down for the full 75s watch with it (WIN32_EXIT_CODE 0).
//
// Windows-tagged rather than an untagged (GOOS, facts) -> plan seam
// (CLAUDE.md §Test discipline) because the subject IS the sequence of
// svc.Status values handed to x/sys, and that type only exists here.
// `unit tests (windows)` is a required check.
func TestSvcHandlerExecute_NeverReportsStoppedItself(t *testing.T) {
	cases := []struct {
		name string
		// drive takes the handler's request channel and the running
		// Execute's exit; it produces one of the three ways out.
		drive           func(t *testing.T, requests chan<- svc.ChangeRequest)
		wantStopPending bool
	}{
		{
			name:            "a restart the agent asked for",
			drive:           func(*testing.T, chan<- svc.ChangeRequest) { RequestRestart() },
			wantStopPending: false,
		},
		{
			name: "an SCM stop",
			drive: func(_ *testing.T, requests chan<- svc.ChangeRequest) {
				requests <- svc.ChangeRequest{Cmd: svc.Stop}
			},
			// The SCM needs the progress report or it times the stop out.
			// Asserted so "stop reporting Stopped" cannot be over-applied
			// into a handler that reports nothing on the way down.
			wantStopPending: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restartRequested.Store(false)
			t.Cleanup(func() { restartRequested.Store(false) })
			prevDelay := restartRequestDelay
			restartRequestDelay = 0
			t.Cleanup(func() { restartRequestDelay = prevDelay })

			h := &svcHandler{run: func(ctx context.Context, _ []string) error {
				<-ctx.Done()
				return nil
			}}
			requests := make(chan svc.ChangeRequest, 1)
			status := make(chan svc.Status, 16)

			done := make(chan struct{})
			go func() {
				h.Execute(nil, requests, status)
				close(done)
			}()

			deadline := time.Now().Add(10 * time.Second)
			for scmStop.Load() == nil {
				if time.Now().After(deadline) {
					t.Fatal("Execute never published its stop lever")
				}
				time.Sleep(time.Millisecond)
			}
			tc.drive(t, requests)

			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Execute did not return")
			}

			close(status)
			var reported []svc.State
			for s := range status {
				reported = append(reported, s.State)
			}
			if slices.Contains(reported, svc.Stopped) {
				t.Errorf("Execute reported Stopped itself (states: %v); "+
					"that report carries dwWin32ExitCode = 0 and the SCM "+
					"finalises on it, so the recovery actions never fire (#855)",
					reported)
			}
			if got := slices.Contains(reported, svc.StopPending); got != tc.wantStopPending {
				t.Errorf("StopPending reported = %v, want %v (states: %v)",
					got, tc.wantStopPending, reported)
			}
		})
	}
}

// The same pin for the third way out: run() returning on its own, with no
// restart asked for. Separate because it needs a run hook that returns
// rather than one that waits to be cancelled, and it is the arm #315's
// recovery actions were added for in the first place — a leading clean
// Stopped silenced that one too.
func TestSvcHandlerExecute_AnUnrequestedExitReportsNoStoppedEither(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })

	h := &svcHandler{run: func(context.Context, []string) error { return nil }}
	requests := make(chan svc.ChangeRequest)
	status := make(chan svc.Status, 16)

	h.Execute(nil, requests, status)

	close(status)
	for s := range status {
		if s.State == svc.Stopped {
			t.Fatal("Execute reported Stopped itself on the plain failure exit; " +
				"the SCM reads that as a clean stop and skips recovery (#855)")
		}
	}
}
