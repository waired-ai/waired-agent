package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlurl"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// fakeEnroll stands in for setup.Enroll. It invokes OnLoginURL (so the
// controller captures the URL mid-flight), then optionally blocks on
// release before returning, letting a test observe the intermediate
// phase deterministically.
type fakeEnroll struct {
	urled         chan struct{} // signalled after OnLoginURL fires
	release       chan struct{} // enroll returns once this is closed/received
	result        *setup.EnrollResult
	err           error
	calls         int32
	gotEndpoint   atomic.Value // last opts.Endpoint seen (string)
	gotControlURL atomic.Value // last opts.ControlURL seen (string)
	gotHTTPClient atomic.Value // last opts.HTTPClient seen (*http.Client)
}

func (f *fakeEnroll) fn(ctx context.Context, opts setup.EnrollOptions) (*setup.EnrollResult, error) {
	atomic.AddInt32(&f.calls, 1)
	f.gotEndpoint.Store(opts.Endpoint)
	f.gotControlURL.Store(opts.ControlURL)
	if opts.HTTPClient != nil {
		f.gotHTTPClient.Store(opts.HTTPClient)
	}
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

// newTestReauthController is the same, plus the reactivate hook a re-auth
// needs. Separate so the tests above keep proving that a controller
// WITHOUT one still serves every non-reauth login (#175).
func newTestReauthController(sb *switchboard, enroll enrollFunc, activate, reactivate func(context.Context) error) *loginController {
	return newLoginController(sb, loginControllerConfig{
		StateDir:          "/tmp/does-not-matter",
		DefaultControlURL: "https://cp.example",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          activate,
		Reactivate:        reactivate,
		Logger:            testLogger(),
		Enroll:            enroll,
	})
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitPhase(t *testing.T, lc *loginController, sessID string, want management.LoginPhase) management.LoginStatus {
	t.Helper()
	deadline := time.Now().Add(waitBackstop)
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

// TestLoginUsesBakedDefaultOnStockInstall is the #174 regression. A stock
// install has no --control and no $WAIRED_CONTROL_URL in the daemon's
// environment — launchd and the Windows SCM cannot supply one, and Linux's
// EnvironmentFile only carries a URL when install.sh was given
// --control/--dev — so the app's "Sign in…" used to fail outright with
// "login: no control URL". With main.go resolving through
// resolveDaemonControlURL, that same daemon reaches enroll against the
// production Control Plane. Product contract.
func TestLoginUsesBakedDefaultOnStockInstall(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{AccountEmail: "u@e"}}
	lc := newLoginController(sb, loginControllerConfig{
		StateDir: "/tmp/does-not-matter",
		// What main.go now computes when nothing is configured.
		DefaultControlURL: resolveDaemonControlURL("", "", testLogger()),
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          func(context.Context) error { return nil },
		Logger:            testLogger(),
		Enroll:            fe.fn,
	})

	// No control_url on the request either: the tray sends an empty one
	// unless it was itself started with --control.
	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatalf("Start on a stock install must not fail: %v", err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)

	if got, want := fe.gotControlURL.Load().(string), controlurl.Default; got != want {
		t.Errorf("enroll called with control URL %q, want the production default %q", got, want)
	}
}

// TestLoginRequestControlURLWins keeps the daemon's default from
// overriding an explicit per-request choice now that it is never empty.
func TestLoginRequestControlURLWins(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{AccountEmail: "u@e"}}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{
		ControlURL: "https://from-request.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)

	if got, want := fe.gotControlURL.Load().(string), "https://from-request.example"; got != want {
		t.Errorf("enroll called with control URL %q, want %q", got, want)
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

// PRODUCT CONTRACT (#175): Reauth is the one request that means "yes, I
// know this device is enrolled — enrol it again anyway". It is what
// replaced the standalone re-auth path, so if this stops working there is
// no other way for an enrolled device to renew credentials its refresh
// loop can no longer renew.
//
// This does not invert TestLoginIdempotentWhenAlreadyActive above: that
// pins the no-op for a request that did NOT ask, and still does.
func TestLoginReauthReenrollsALiveDevice(t *testing.T) {
	sb := &switchboard{}
	sb.publish(&session{provider: &agentProvider{id: &identity.Identity{DeviceID: "d1"}}})
	fe := &fakeEnroll{result: &setup.EnrollResult{DeviceID: "d1", AccountEmail: "ops@example.com"}}

	var activates, reactivates int32
	lc := newTestReauthController(sb, fe.fn,
		func(context.Context) error { atomic.AddInt32(&activates, 1); return nil },
		func(context.Context) error { atomic.AddInt32(&reactivates, 1); return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{Reauth: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionID == "" {
		t.Fatal("a re-auth must open a real session, not report the no-op status")
	}
	got := waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)
	if got.AccountEmail != "ops@example.com" {
		t.Errorf("account email = %q, want the one the re-enrolment returned", got.AccountEmail)
	}
	if n := atomic.LoadInt32(&fe.calls); n != 1 {
		t.Errorf("enroll ran %d times, want exactly 1", n)
	}
	// The live session is running on the credentials that were just
	// replaced, so it has to be rebuilt — activate alone refuses to
	// publish over a current session, and leaving it up would keep the
	// daemon on tokens the control plane has rotated away from.
	if atomic.LoadInt32(&reactivates) != 1 {
		t.Errorf("reactivate ran %d times, want 1", reactivates)
	}
	if atomic.LoadInt32(&activates) != 0 {
		t.Errorf("plain activate ran %d times on a re-auth, want 0", activates)
	}
}

// A fresh daemon has no session to tear down, so a Reauth request there is
// just a login. Worth pinning because the CLI sets the flag from "does
// identity.json exist", which the daemon has no reason to agree with — a
// state dir restored from backup, or an identity the daemon failed to
// activate, both land here.
func TestLoginReauthOnUnenrolledDaemonIsAPlainLogin(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{DeviceID: "d1"}}

	var activates, reactivates int32
	lc := newTestReauthController(sb, fe.fn,
		func(context.Context) error { atomic.AddInt32(&activates, 1); return nil },
		func(context.Context) error { atomic.AddInt32(&reactivates, 1); return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{Reauth: true})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)
	if atomic.LoadInt32(&activates) != 1 || atomic.LoadInt32(&reactivates) != 0 {
		t.Errorf("activate=%d reactivate=%d; an unenrolled daemon has nothing to rebuild",
			activates, reactivates)
	}
}

// Refusing beats half-succeeding: with no way to rebuild the session, a
// re-auth would rewrite the tokens on disk and leave the running session
// using the old ones — an inconsistency nothing would report.
func TestLoginReauthRefusedWithoutARebuildHook(t *testing.T) {
	sb := &switchboard{}
	sb.publish(&session{provider: &agentProvider{id: &identity.Identity{DeviceID: "d1"}}})
	fe := &fakeEnroll{}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	if _, err := lc.Start(context.Background(), management.LoginStartRequest{Reauth: true}); err == nil {
		t.Fatal("want an error when the controller cannot rebuild the session")
	}
	if atomic.LoadInt32(&fe.calls) != 0 {
		t.Error("enroll must not run when the rebuild it depends on is impossible")
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

// PRODUCT CONTRACT: whatever client the daemon was configured to enrol
// with must actually reach setup.Enroll, and it must be built for the
// control URL this login resolved.
//
// Under --bypass-cp-iam that client carries a GCE identity token whose
// audience IS that URL; without it the IAM-gated control plane answers the
// enrolment POST with a 403 HTML page from ingress. That is what the
// testnet's agents hit after #175 moved enrolment into the daemon and the
// CLI-side client was deleted along with --bypass-mode.
func TestLoginEnrollUsesTheConfiguredHTTPClient(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{DeviceID: "d1"}}

	want := &http.Client{}
	var sawURL atomic.Value
	lc := newLoginController(sb, loginControllerConfig{
		StateDir:          "/tmp/does-not-matter",
		DefaultControlURL: "https://cp.example",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          func(context.Context) error { return nil },
		Logger:            testLogger(),
		Enroll:            fe.fn,
		EnrollHTTPFor: func(_ context.Context, cpURL string) *http.Client {
			sawURL.Store(cpURL)
			return want
		},
	})

	st, err := lc.Start(context.Background(), management.LoginStartRequest{
		ControlURL: "https://bypass.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)

	// The factory takes the RESOLVED url, not the daemon default: the token
	// audience has to match the host the request actually goes to.
	if got, _ := sawURL.Load().(string); got != "https://bypass.example" {
		t.Errorf("factory got control URL %q, want the one the request resolved to", got)
	}
	if got, _ := fe.gotHTTPClient.Load().(*http.Client); got != want {
		t.Errorf("enroll got HTTPClient %p, want the configured one %p", got, want)
	}
}

// A daemon with no factory configured enrols with the default client --
// the ordinary desktop case, where the control plane needs no per-request
// credential.
func TestLoginEnrollDefaultsToNoHTTPClient(t *testing.T) {
	sb := &switchboard{}
	fe := &fakeEnroll{result: &setup.EnrollResult{DeviceID: "d1"}}
	lc := newTestLoginController(sb, fe.fn, func(context.Context) error { return nil })

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)

	if got := fe.gotHTTPClient.Load(); got != nil {
		t.Errorf("enroll got a client (%v) with no factory configured; want nil", got)
	}
}
