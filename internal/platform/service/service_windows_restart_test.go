//go:build windows

package service

import (
	"context"
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
