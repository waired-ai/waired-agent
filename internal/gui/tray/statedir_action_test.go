package tray

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// stageLockedOutSystemDir puts the process in the situation every shipped
// desktop app is in on a system-wide install: a system state dir it cannot
// read, and no $WAIRED_STATE_DIR override.
//
// The override has to be cleared explicitly because this package's TestMain
// sets one for the whole suite (to keep the autostart first-run marker off the
// developer's own home). Left in place it would win outright and the rows
// below would assert nothing.
func stageLockedOutSystemDir(t *testing.T) (sysDir string) {
	t.Helper()
	t.Setenv(paths.EnvOverride, "")
	prev := readStateDirNames
	readStateDirNames = func(string) ([]string, error) {
		return nil, &fs.PathError{Op: "open", Path: "system state dir", Err: fs.ErrPermission}
	}
	t.Cleanup(func() { readStateDirNames = prev })
	return paths.StateDir(paths.System)
}

// answerConfirm makes the confirmation dialog say yes for one test. The
// package default declines, which is what a host with no dialog backend does.
func answerConfirm(t *testing.T, yes bool) {
	t.Helper()
	prev := showConfirm
	showConfirm = func(prompt string) bool {
		seams.add(&seams.confirms, prompt)
		return yes
	}
	t.Cleanup(func() { showConfirm = prev })
}

// waitForElevation blocks until the elevation log records name, or the budget
// lapses. onLogout does its work on a goroutine, so there is nothing else to
// synchronise on.
func waitForElevation(t *testing.T, l *seamLog, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(l.snapshot(&l.elevations), name) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q elevation within the budget; elevations = %v", name, l.snapshot(&l.elevations))
}

// TestOnLogoutPassesTheSystemStateDirWhenTheUserCannotReadIt is the regression
// test for waired-agent#1269.
//
// Product contract (#1269): the elevated sign-out must be pointed at the
// directory the daemon is enrolled in, even when this process cannot read it.
//
// On the pre-fix code this fails twice over. The app resolved its state dir by
// stat'ing <system state dir>/identity.json and reading the resulting EACCES
// as "no system install", so it handed the elevated CLI the per-user
// directory; and the seam stub dropped the argument, so the value was not
// recorded at all. `waired logout` on a directory with no identity removes
// five paths that are all absent, prints "logout: identity + secrets removed."
// and exits 0 — which is exactly why the app showed no error and nothing was
// signed out.
//
// The daemon here 404s /setup/state and /logout, which is the rc5-era daemon
// the defect was reported against: the answer must come from the local facts
// alone.
func TestOnLogoutPassesTheSystemStateDirWhenTheUserCannotReadIt(t *testing.T) {
	l := resetSeams(t)
	sysDir := stageLockedOutSystemDir(t)
	answerConfirm(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := &tray{
		cli: newTestClient(srv.URL),
		opts: Options{
			// What the old code would have resolved to, and what an
			// unfixed app would pass on.
			StateDir: paths.StateDir(paths.Interactive),
		},
	}
	tr.onLogout(context.Background())
	waitForElevation(t, l, "logout")

	want := "logout state-dir=" + sysDir
	if args := l.snapshot(&l.elevationArgs); !slices.Contains(args, want) {
		t.Errorf("elevated sign-out was pointed at the wrong directory:\n got %v\nwant it to contain %q", args, want)
	}
}

// TestOnLogoutFollowsTheDaemonsOwnStateDir pins that the daemon outranks the
// local guess. It is the case a locally-computed default cannot get right: a
// daemon started with its own --state-dir or $WAIRED_STATE_DIR sits nowhere
// either platform candidate names, and the symptom of guessing is silent
// (internal/management/setup_handlers.go, waired#835 §11.1).
func TestOnLogoutFollowsTheDaemonsOwnStateDir(t *testing.T) {
	l := resetSeams(t)
	stageLockedOutSystemDir(t)
	answerConfirm(t, true)

	const daemonDir = "/opt/waired-instance-2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waired/v1/setup/state" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"active":false,"state_dir":"` + daemonDir + `"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL), opts: Options{StateDir: "/should/not/be/used"}}
	tr.onLogout(context.Background())
	waitForElevation(t, l, "logout")

	want := "logout state-dir=" + daemonDir
	if args := l.snapshot(&l.elevationArgs); !slices.Contains(args, want) {
		t.Errorf("elevated sign-out ignored the daemon's own state dir:\n got %v\nwant it to contain %q", args, want)
	}
}

// TestOnLogoutUsesTheDaemonRouteAndDoesNotElevate pins the shape of a
// sign-out against a current daemon.
//
// Product contract, waired-agent#1269: sign-out is the daemon's job, as
// sign-in has been since #175. Two things follow and both are asserted here —
// the app raises no authorization prompt at all, and the menu says "not signed
// in" on the next poll rather than continuing to show the account until the
// access token lapses (the ~2 minutes the issue recorded) or the daemon is
// restarted by hand.
//
// On the pre-fix code the first half fails outright: the app always elevated.
func TestOnLogoutUsesTheDaemonRouteAndDoesNotElevate(t *testing.T) {
	l := resetSeams(t)
	stageLockedOutSystemDir(t)
	answerConfirm(t, true)

	var signedOut atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/waired/v1/logout":
			signedOut.Store(true)
			_, _ = w.Write([]byte(`{"enrolled":false,"deauthed":true}`))
		case "/waired/v1/status":
			_, _ = w.Write([]byte(`{"phase":"active"}`))
		case "/waired/v1/identity":
			if signedOut.Load() {
				_, _ = w.Write([]byte(`{"enrolled":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"enrolled":true,"account_email":"someone@example.test"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL)}
	tr.onLogout(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !signedOut.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	if !signedOut.Load() {
		t.Fatal("the daemon's sign-out route was never called")
	}
	if got := l.snapshot(&l.elevations); len(got) != 0 {
		t.Errorf("the app elevated anyway: %v — sign-out must not ask for an administrator password "+
			"against a daemon that offers the route", got)
	}
	// pollOnce runs after the route returns; wait for the menu to catch up.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		title := tr.last.HeaderTitle
		tr.mu.Unlock()
		if title == "○ Not signed in" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	tr.mu.Lock()
	got := tr.last.HeaderTitle
	tr.mu.Unlock()
	t.Errorf("menu header = %q after a sign-out, want %q", got, "○ Not signed in")
}

// A sign-in in flight is answered with a message, not by tearing it down and
// not by falling back to the elevated path.
func TestOnLogoutReportsASignInInFlight(t *testing.T) {
	l := resetSeams(t)
	stageLockedOutSystemDir(t)
	answerConfirm(t, true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waired/v1/logout" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := &tray{cli: newTestClient(srv.URL)}
	tr.onLogout(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(l.snapshot(&l.errors)) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	errs := l.snapshot(&l.errors)
	if len(errs) == 0 || !strings.Contains(errs[0], "sign-in is in progress") {
		t.Errorf("errors = %v, want one naming the sign-in in progress", errs)
	}
	if got := l.snapshot(&l.elevations); len(got) != 0 {
		t.Errorf("a refused sign-out fell back to elevation: %v", got)
	}
}

// TestElevationStateDirHonoursAnExplicitFlag pins the one case that must NOT
// consult the daemon: an operator who passed --state-dir named a directory and
// gets it. Same rule --log-level already follows.
func TestElevationStateDirHonoursAnExplicitFlag(t *testing.T) {
	resetSeams(t)
	stageLockedOutSystemDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state_dir":"/from/the/daemon"}`))
	}))
	defer srv.Close()

	tr := &tray{
		cli:  newTestClient(srv.URL),
		opts: Options{StateDir: "/operator/said/so", StateDirPinned: true},
	}
	if got := tr.elevationStateDir(context.Background()); got != "/operator/said/so" {
		t.Errorf("elevationStateDir = %q, want the pinned flag value", got)
	}
}
