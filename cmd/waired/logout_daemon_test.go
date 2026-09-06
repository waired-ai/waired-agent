package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// signOutDaemon stands in for a running agent that offers the sign-out
// route. It records what was asked of it and answers `status`.
type signOutDaemon struct {
	srv    *httptest.Server
	calls  atomic.Int32
	gotReq management.LogoutRequest
}

func newSignOutDaemon(t *testing.T, status int, resp management.LogoutResponse) *signOutDaemon {
	t.Helper()
	d := &signOutDaemon{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/logout" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		d.calls.Add(1)
		if body, err := io.ReadAll(r.Body); err == nil {
			_ = json.Unmarshal(body, &d.gotReq)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

// TestRunLogoutHandsTheJobToARunningDaemon is the CLI half of
// waired-agent#1269's second defect.
//
// Product contract: when a daemon is running it performs the sign-out, because
// it is the only process that can stop serving the identity it deletes. Doing
// it from out here left the daemon answering as enrolled until its access
// token lapsed — the two minutes the issue recorded — and let a subsequent
// sign-in write identity.json back from the live session (waired-agent#800).
//
// The local files are checked too: delegation must not turn into "the daemon
// was asked and nobody looked at whether anything happened".
func TestRunLogoutHandsTheJobToARunningDaemon(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusOK, management.LogoutResponse{Deauthed: true})

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.example")

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("the daemon's sign-out route was called %d times, want 1", got)
	}
	// The daemon owns the removal in this path, so the CLI must NOT also
	// delete: a second remover is a second copy of the path list.
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Errorf("the CLI deleted the files itself after delegating: %v", err)
	}
}

// --local means "do not call the control plane". It must still reach the
// daemon: an operator signing out offline wants the running service to stop
// serving just as much as an online one does.
func TestRunLogoutLocalStillTellsTheDaemon(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusOK, management.LogoutResponse{})

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.example")

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes", "--local"}); err != nil {
		t.Fatalf("runLogout --local: %v", err)
	}
	if got := d.calls.Load(); got != 1 {
		t.Fatalf("the daemon was called %d times, want 1", got)
	}
	if !d.gotReq.SkipDeauth {
		t.Error("--local did not reach the daemon as skip_deauth; it would call the control plane anyway")
	}
}

func TestRunLogoutRevokeReachesTheDaemonAsRevoke(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusOK, management.LogoutResponse{Deauthed: true})

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.example")

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes", "--revoke"}); err != nil {
		t.Fatalf("runLogout --revoke: %v", err)
	}
	if !d.gotReq.Revoke {
		t.Error("--revoke did not reach the daemon; the device row would survive an uninstall")
	}
}

// --server-only keeps the local files on purpose — the deb's prerm uses it so
// dpkg stays the single owner of deletion. The daemon route always removes
// them, so that mode must not be delegated.
func TestRunLogoutServerOnlyIsNotDelegated(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusOK, management.LogoutResponse{})

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer cp.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, cp.URL)

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes", "--server-only"}); err != nil {
		t.Fatalf("runLogout --server-only: %v", err)
	}
	if got := d.calls.Load(); got != 0 {
		t.Errorf("the daemon route was called %d times for --server-only; it would have deleted the files dpkg owns", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		t.Errorf("--server-only removed local state: %v", err)
	}
}

// An agent too old to offer the route answers 404. The CLI must fall through
// to doing the job itself rather than reporting a failure.
func TestRunLogoutFallsBackWhenTheDaemonPredatesTheRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.invalid")

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", srv.URL, "--yes", "--local"}); err != nil {
		t.Fatalf("runLogout against an old daemon: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Errorf("the fallback did not remove identity.json (stat err = %v)", err)
	}
}

// A daemon that refuses because a sign-in is in flight must stop the command,
// not be raced by deleting the files out from under the sign-in.
func TestRunLogoutStopsWhenTheDaemonRefuses(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusConflict, management.LogoutResponse{})

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.example")

	err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes"})
	if err == nil {
		t.Fatal("runLogout against a refusing daemon = nil, want the refusal reported")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "identity.json")); statErr != nil {
		t.Errorf("a refused sign-out still removed identity.json: %v", statErr)
	}
}

// The control-plane warning has to survive the hop. The app had no way to say
// "signed out here, but the device may still be active server-side" before
// this, and the CLI has printed it since #115.
func TestRunLogoutReportsTheDaemonsControlPlaneWarning(t *testing.T) {
	d := newSignOutDaemon(t, http.StatusOK,
		management.LogoutResponse{DeauthError: "control plane unreachable"})

	dir := t.TempDir()
	seedEnrolled(t, dir, "https://cp.example")

	var buf strings.Builder
	prev := stderr
	stderr = &buf
	t.Cleanup(func() { stderr = prev })

	if err := runLogout([]string{"--state-dir", dir, "--mgmt", d.srv.URL, "--yes"}); err != nil {
		t.Fatalf("runLogout: %v", err)
	}
	if !strings.Contains(buf.String(), "may still be active server-side") {
		t.Errorf("stderr = %q, want the control-plane warning", buf.String())
	}
}
