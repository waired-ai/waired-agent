package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/setup"
)

// cpEnroll stands in for setup.Enroll across several sign-ins to different
// control planes (waired-agent#1343). Each call publishes its control URL
// on started once the login URL is out, then waits for that URL's release
// or for its context — unless ignoreCtx, which models a control plane that
// finishes the sign-in just as the daemon cancels it.
type cpEnroll struct {
	started   chan string
	ignoreCtx bool
	// stopDelay is how long a cancelled call takes to return, like an
	// enrollment finishing an in-flight request. It gives a sign-in that
	// does not wait for the one it replaced time to overlap with it.
	stopDelay time.Duration

	mu       sync.Mutex
	releases map[string]chan struct{}
	ctxErrs  map[string]error

	inFlight int32
	overlap  int32
	calls    int32
}

func newCPEnroll() *cpEnroll {
	return &cpEnroll{
		started:  make(chan string, 8),
		releases: map[string]chan struct{}{},
		ctxErrs:  map[string]error{},
	}
}

func (f *cpEnroll) release(url string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.releases[url]
	if !ok {
		ch = make(chan struct{})
		f.releases[url] = ch
	}
	return ch
}

func (f *cpEnroll) fn(ctx context.Context, opts setup.EnrollOptions) (*setup.EnrollResult, error) {
	atomic.AddInt32(&f.calls, 1)
	if atomic.AddInt32(&f.inFlight, 1) > 1 {
		atomic.StoreInt32(&f.overlap, 1)
	}
	defer atomic.AddInt32(&f.inFlight, -1)
	if opts.OnLoginURL != nil {
		opts.OnLoginURL(opts.ControlURL+"/login/ls_test", "WXYZ-1234")
	}
	f.started <- opts.ControlURL
	rel := f.release(opts.ControlURL)
	if f.ignoreCtx {
		<-rel
		return &setup.EnrollResult{AccountEmail: "u@example.com"}, nil
	}
	select {
	case <-rel:
		return &setup.EnrollResult{AccountEmail: "u@example.com"}, nil
	case <-ctx.Done():
		f.mu.Lock()
		f.ctxErrs[opts.ControlURL] = ctx.Err()
		f.mu.Unlock()
		time.Sleep(f.stopDelay)
		return nil, ctx.Err()
	}
}

func waitStarted(t *testing.T, f *cpEnroll, want string) {
	t.Helper()
	select {
	case got := <-f.started:
		if got != want {
			t.Fatalf("enroll started against %q, want %q", got, want)
		}
	case <-time.After(waitBackstop):
		t.Fatalf("timed out waiting for enroll against %q", want)
	}
}

func newCPController(t *testing.T, f *cpEnroll, activate func(context.Context) error) *loginController {
	t.Helper()
	return newLoginController(&switchboard{}, loginControllerConfig{
		StateDir:          "/tmp/does-not-matter",
		DefaultControlURL: "https://app.dev.waired.net",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          activate,
		Logger:            testLogger(),
		Enroll:            f.fn,
	})
}

// TestLoginResolvesControlURLAtStartNotAtBoot: the daemon asks which
// control plane when a sign-in starts. An agent.env written after the
// service started (the macOS and Linux installers do that) is used without
// a restart.
func TestLoginResolvesControlURLAtStartNotAtBoot(t *testing.T) {
	current := "https://app.waired.ai"
	fe := &fakeEnroll{result: &setup.EnrollResult{AccountEmail: "u@e"}}
	lc := newLoginController(&switchboard{}, loginControllerConfig{
		StateDir:          "/tmp/does-not-matter",
		ResolveControlURL: func() (string, string) { return current, "agent.env" },
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Activate:          func(context.Context) error { return nil },
		Logger:            testLogger(),
		Enroll:            fe.fn,
	})
	current = "https://app.dev.waired.net" // the installer writes agent.env now

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, lc, st.SessionID, management.LoginPhaseActive)
	if got := fe.gotControlURL.Load().(string); got != "https://app.dev.waired.net" {
		t.Errorf("enroll called with %q, want the control plane agent.env names at sign-in", got)
	}
}

// TestLoginStatusReportsTheSessionControlURL: the start answer and every
// status poll name the control plane the sign-in goes to, so the terminal
// can print it (waired-agent#1343).
func TestLoginStatusReportsTheSessionControlURL(t *testing.T) {
	for _, tc := range []struct {
		name, request, want string
	}{
		{"from the request", "app.example.com/", "https://app.example.com"},
		{"from the daemon", "", "https://app.dev.waired.net"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCPEnroll()
			lc := newCPController(t, f, func(context.Context) error { return nil })
			st, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: tc.request})
			if err != nil {
				t.Fatal(err)
			}
			if st.ControlURL != tc.want {
				t.Errorf("start ControlURL = %q, want %q", st.ControlURL, tc.want)
			}
			waitStarted(t, f, tc.want)
			close(f.release(tc.want))
			if got := waitPhase(t, lc, st.SessionID, management.LoginPhaseActive); got.ControlURL != tc.want {
				t.Errorf("status ControlURL = %q, want %q", got.ControlURL, tc.want)
			}
		})
	}
}

func TestLoginRejectsAMalformedRequestControlURL(t *testing.T) {
	f := newCPEnroll()
	lc := newCPController(t, f, func(context.Context) error { return nil })
	if _, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "ftp://cp.example"}); err == nil {
		t.Fatal("Start accepted an ftp control URL")
	}
	if n := atomic.LoadInt32(&f.calls); n != 0 {
		t.Errorf("enroll calls = %d, want 0", n)
	}
}

// TestLoginSingleFlight_DifferentControlURLReplacesPendingSession is the
// #1343 shape: a sign-in to one control plane is waiting for the browser,
// and `waired init --control <another>` runs. The new request gets a new
// sign-in for the control plane it named; the old one is cancelled, its
// terminal is told why, and the two never enroll at the same time.
func TestLoginSingleFlight_DifferentControlURLReplacesPendingSession(t *testing.T) {
	f := newCPEnroll()
	f.stopDelay = 300 * time.Millisecond
	var activations int32
	lc := newCPController(t, f, func(context.Context) error { atomic.AddInt32(&activations, 1); return nil })

	old, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.waired.ai"})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, f, "https://app.waired.ai")

	next, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.dev.waired.net"})
	if err != nil {
		t.Fatal(err)
	}
	if next.SessionID == old.SessionID || next.ControlURL != "https://app.dev.waired.net" {
		t.Fatalf("second Start = %+v, want a new sign-in for the dev control plane", next)
	}
	waitStarted(t, f, "https://app.dev.waired.net")
	f.mu.Lock()
	oldErr := f.ctxErrs["https://app.waired.ai"]
	f.mu.Unlock()
	if oldErr == nil {
		t.Error("the replaced sign-in's enroll was not cancelled")
	}
	if atomic.LoadInt32(&f.overlap) != 0 {
		t.Error("the new sign-in enrolled while the replaced one was still running")
	}

	st, err := lc.Status(context.Background(), old.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionID != old.SessionID || st.Phase != management.LoginPhaseError || !strings.Contains(st.Error, "https://app.dev.waired.net") {
		t.Errorf("status of the replaced sign-in = %+v, want an error naming the new control plane", st)
	}

	close(f.release("https://app.dev.waired.net"))
	waitPhase(t, lc, next.SessionID, management.LoginPhaseActive)
	if n := atomic.LoadInt32(&activations); n != 1 {
		t.Errorf("activations = %d, want 1", n)
	}
}

// TestLoginSingleFlight_EmptyControlURLJoinsPendingSession: a request that
// names no control plane (the app's "Sign in…", or a terminal that left
// the choice to the daemon) joins the sign-in in flight.
func TestLoginSingleFlight_EmptyControlURLJoinsPendingSession(t *testing.T) {
	f := newCPEnroll()
	lc := newCPController(t, f, func(context.Context) error { return nil })
	first, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, f, "https://app.example.com")
	again, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != first.SessionID {
		t.Errorf("an empty request started a new sign-in: %s != %s", again.SessionID, first.SessionID)
	}
	close(f.release("https://app.example.com"))
	waitPhase(t, lc, first.SessionID, management.LoginPhaseActive)
	if n := atomic.LoadInt32(&f.calls); n != 1 {
		t.Errorf("enroll calls = %d, want 1", n)
	}
}

// TestLoginSingleFlight_SameURLAfterNormalizationJoins: the same control
// plane spelled differently is not a different one.
func TestLoginSingleFlight_SameURLAfterNormalizationJoins(t *testing.T) {
	f := newCPEnroll()
	lc := newCPController(t, f, func(context.Context) error { return nil })
	first, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.dev.waired.net"})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, f, "https://app.dev.waired.net")
	again, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "app.dev.waired.net/"})
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != first.SessionID {
		t.Errorf("the same control plane spelled differently replaced the sign-in")
	}
	close(f.release("https://app.dev.waired.net"))
	waitPhase(t, lc, first.SessionID, management.LoginPhaseActive)
}

// TestLoginSingleFlight_ActivatingSessionIsNotReplaced: once the control
// plane has authorized the device, a request for another one joins.
func TestLoginSingleFlight_ActivatingSessionIsNotReplaced(t *testing.T) {
	f := newCPEnroll()
	activating := make(chan struct{})
	releaseActivate := make(chan struct{})
	lc := newCPController(t, f, func(context.Context) error {
		close(activating)
		<-releaseActivate
		return nil
	})
	first, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.dev.waired.net"})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, f, "https://app.dev.waired.net")
	close(f.release("https://app.dev.waired.net"))
	<-activating

	again, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.waired.ai"})
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != first.SessionID || again.Phase != management.LoginPhaseActivating {
		t.Errorf("Start during activation = %+v, want the activating sign-in", again)
	}
	close(releaseActivate)
	waitPhase(t, lc, first.SessionID, management.LoginPhaseActive)
	if n := atomic.LoadInt32(&f.calls); n != 1 {
		t.Errorf("enroll calls = %d, want 1", n)
	}
}

// TestLoginReplacedEnrollmentIsNotActivated: a replaced sign-in that the
// control plane finishes anyway is not brought up; only the sign-in that
// replaced it is.
func TestLoginReplacedEnrollmentIsNotActivated(t *testing.T) {
	f := newCPEnroll()
	f.ignoreCtx = true
	var activations int32
	lc := newCPController(t, f, func(context.Context) error { atomic.AddInt32(&activations, 1); return nil })

	if _, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.waired.ai"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, f, "https://app.waired.ai")
	next, err := lc.Start(context.Background(), management.LoginStartRequest{ControlURL: "https://app.dev.waired.net"})
	if err != nil {
		t.Fatal(err)
	}
	close(f.release("https://app.waired.ai")) // the old sign-in completes regardless
	waitStarted(t, f, "https://app.dev.waired.net")
	if n := atomic.LoadInt32(&activations); n != 0 {
		t.Fatalf("activations after the replaced sign-in finished = %d, want 0", n)
	}
	close(f.release("https://app.dev.waired.net"))
	waitPhase(t, lc, next.SessionID, management.LoginPhaseActive)
	if n := atomic.LoadInt32(&activations); n != 1 {
		t.Errorf("activations = %d, want 1", n)
	}
}
