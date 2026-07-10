package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// fakeEnroll stands in for setup.Enroll. It invokes OnLoginURL (so the
// controller captures the URL mid-flight), then optionally blocks on
// release before returning, letting a test observe the intermediate
// phase deterministically.
type fakeEnroll struct {
	urled       chan struct{} // signalled after OnLoginURL fires
	release     chan struct{} // enroll returns once this is closed/received
	result      *setup.EnrollResult
	err         error
	calls       int32
	gotEndpoint atomic.Value // last opts.Endpoint seen (string)
}

func (f *fakeEnroll) fn(ctx context.Context, opts setup.EnrollOptions) (*setup.EnrollResult, error) {
	atomic.AddInt32(&f.calls, 1)
	f.gotEndpoint.Store(opts.Endpoint)
	if opts.OnLoginURL != nil {
		opts.OnLoginURL("https://login.example/abc", "WXYZ-1234")
	}
	if f.urled != nil {
		f.urled <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func newTestLoginController(sb *switchboard, enroll enrollFunc, activate func(context.Context) error) *loginController {
	return newLoginController(sb, loginControllerConfig{
		StateDir:          "/tmp/does-not-matter",
		DefaultControlURL: "https://cp.example",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          activate,
		Logger:            testLogger(),
		Enroll:            enroll,
	})
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitPhase(t *testing.T, lc *loginController, sessID string, want management.LoginPhase) management.LoginStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := lc.Status(context.Background(), sessID)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Phase == want {
			return st
		}
		if st.Phase == management.LoginPhaseError && want != management.LoginPhaseError {
			t.Fatalf("login errored while waiting for %s: %s", want, st.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for phase %s", want)
	return management.LoginStatus{}
}

func TestLoginProgressesToActive(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{
		urled:   make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  &setup.EnrollResult{AccountEmail: "user@example.com"},
	}
	activated := false
	activate := func(ctx context.Context) error {
		activated = true
		sb.publish(&session{provider: &agentProvider{id: &identity.Identity{DeviceID: "d1"}}})
		return nil
	}
	lc := newTestLoginController(sb, fe.fn, activate)

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != management.LoginPhaseLoggingIn || st.SessionID == "" {
		t.Fatalf("initial status: %+v", st)
	}

	// Once OnLoginURL has fired, the login URL/code are observable while
	// enroll is still blocked.
	<-fe.urled
	mid := waitPhase(t, lc, st.SessionID, management.LoginPhaseLoggingIn)
	if mid.LoginURL != "https://login.example/abc" || mid.UserCode != "WXYZ-1234" {
		t.Fatalf("login url/code not captured: %+v", mid)
	}

	close(fe.release) // let enroll return → activating → active
	final := waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)
	if final.AccountEmail != "user@example.com" {
		t.Errorf("account email not propagated: %+v", final)
	}
	if !activated {
		t.Error("activate was not called")
	}
	if got := atomic.LoadInt32(&fe.calls); got != 1 {
		t.Errorf("enroll calls = %d, want 1", got)
	}
}

// TestLoginResolvesEndpointPortBeforeEnroll pins issue #576: the daemon-driven
// login path must hand enroll an endpoint with a concrete port, never the raw
// "udp4:127.0.0.1:0" placeholder that made activate() fail with
// `parse endpoint "udp4:127.0.0.1:0": port out of range: 0`.
func TestLoginResolvesEndpointPortBeforeEnroll(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{AccountEmail: "u@e"}}
	// newTestLoginController seeds Endpoint "udp4:127.0.0.1:0" (port 0).
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)

	ep, _ := fe.gotEndpoint.Load().(string)
	if ep == "" {
		t.Fatal("enroll was not called with an endpoint")
	}
	// Parse it exactly as activate() does; port 0 would error here.
	port, err := udpListenPortFromEndpoint(ep)
	if err != nil {
		t.Fatalf("enroll endpoint %q not concrete: %v", ep, err)
	}
	if port == 0 {
		t.Fatalf("enroll endpoint %q still has port 0", ep)
	}
}

func TestLoginSingleFlight(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{
		urled:   make(chan struct{}, 1),
		release: make(chan struct{}),
		result:  &setup.EnrollResult{AccountEmail: "user@example.com"},
	}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	st1, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-fe.urled // enroll in flight, blocked on release

	// A second Start while logging in returns the SAME session and does
	// not spawn a second enrollment.
	st2, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st2.SessionID != st1.SessionID {
		t.Errorf("second Start got a new session: %s != %s", st2.SessionID, st1.SessionID)
	}
	close(fe.release)
	waitPhase(t, lc, st1.SessionID, management.LoginPhaseActive)
	if got := atomic.LoadInt32(&fe.calls); got != 1 {
		t.Errorf("enroll calls = %d, want 1 (single-flight)", got)
	}
}

func TestLoginEnrollErrorSetsErrorPhase(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{err: errors.New("control plane denied")}
	activateCalls := int32(0)
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error {
		atomic.AddInt32(&activateCalls, 1)
		return nil
	})

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitPhase(t, lc, st.SessionID, management.LoginPhaseError)
	if got.Error == "" {
		t.Error("expected error message in status")
	}
	if atomic.LoadInt32(&activateCalls) != 0 {
		t.Error("activate must not run when enroll fails")
	}
}

func TestLoginActivateErrorSetsErrorPhase(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{AccountEmail: "u@e"}}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error {
		return errors.New("engine bind failed")
	})

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitPhase(t, lc, st.SessionID, management.LoginPhaseError)
	if got.Error == "" {
		t.Error("expected activation error in status")
	}
}

func TestLoginIdempotentWhenAlreadyActive(t *testing.T) {
	sb := &switchboard{}
	sb.publish(&session{provider: &agentProvider{id: &identity.Identity{DeviceID: "d1"}}})
	fe := &fakeEnroll{}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != management.LoginPhaseActive {
		t.Errorf("already-enrolled Start should report active, got %+v", st)
	}
	if atomic.LoadInt32(&fe.calls) != 0 {
		t.Error("enroll must not run when already enrolled")
	}
}

func TestLoginStatusUnknownSessionResting(t *testing.T) {
	sb := &switchboard{}
	lc := newTestLoginController(sb, (&fakeEnroll{}).fn, func(context.Context) error { return nil })

	st, err := lc.Status(context.Background(), "no-such-session")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != management.LoginPhaseUnenrolled {
		t.Errorf("unknown session on fresh daemon should be unenrolled, got %+v", st)
	}
}
