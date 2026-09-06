package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// waired#1297. `waired share` is the one sharing switch on the computer;
// who it is shared with is set in the console. PRODUCT CONTRACT from the
// owner ruling on that issue.

func TestRunShareTransition_HitsTheDaemonWhenReachable(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"off","desired_state":"off"}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := runShareTransition(srv.URL, t.TempDir(), state.SharingOff, "share off"); err != nil {
			t.Fatalf("runShareTransition: %v", err)
		}
	})
	if len(paths) != 1 || paths[0] != "/waired/v1/sharing/disable" {
		t.Fatalf("routes hit = %v, want [/waired/v1/sharing/disable]", paths)
	}
	if !strings.Contains(out, "share off ok") {
		t.Errorf("output did not confirm the change:\n%s", out)
	}
}

// A daemon that is not running is also a computer that is not serving
// anybody, so persisting the choice loses nothing but the
// acknowledgement — the same dual path pause/resume has.
func TestRunShareTransition_PersistsWhenTheDaemonIsUnreachable(t *testing.T) {
	dir := t.TempDir()
	addr, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		if err := runShareTransition("http://"+addr, dir, state.SharingOff, "share off"); err != nil {
			t.Fatalf("runShareTransition: %v", err)
		}
	})
	if got, _ := state.ReadDesiredSharing(dir); got != state.SharingOff {
		t.Errorf("desired-sharing = %q, want %q", got, state.SharingOff)
	}
}

func TestRunShareStatus_ReportsTheWholePicture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"on","desired_state":"on","mesh_share":"on","public_share":"off","public_max_clients":2}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := runShareStatus(srv.URL, false, &buf); err != nil {
		t.Fatalf("runShareStatus: %v", err)
	}
	for _, want := range []string{
		"Sharing this computer: on",
		"Your other computers: on",
		"People outside your account: off",
		"Guest limit: 2 at once",
		"Waired console",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output missing %q\n---\n%s", want, buf.String())
		}
	}
}

// Empty is not "off": before the first signed map of this run the
// console's settings are unknown, and reporting them as off would send a
// reader looking for a switch nobody moved.
func TestRunShareStatus_UnknownIsNotOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"on","desired_state":"on"}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := runShareStatus(srv.URL, false, &buf); err != nil {
		t.Fatalf("runShareStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "Your other computers: not known yet") {
		t.Errorf("an unheard setting was reported as a choice\n---\n%s", buf.String())
	}
}

// The persisted choice and the live one differ while the app is closed.
// Saying only "off" there would read as a setting the operator made and
// send them looking for the command that undoes it.
func TestRunShareStatus_SaysWhenTheAppIsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"off","desired_state":"on","suspended":true,"mesh_share":"on"}`))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := runShareStatus(srv.URL, false, &buf); err != nil {
		t.Fatalf("runShareStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "Waired app is not running") {
		t.Errorf("output did not explain the pause\n---\n%s", buf.String())
	}
}

// An older daemon has no sharing route. Say so rather than failing: the
// command is a read, and a 404 here is a version fact, not an error.
func TestRunShareStatus_UnsupportedDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if err := runShareStatus(srv.URL, false, &buf); err != nil {
		t.Fatalf("runShareStatus should not fail on a 404: %v", err)
	}
	if !strings.Contains(buf.String(), "unsupported by this background service") {
		t.Errorf("output did not name the cause\n---\n%s", buf.String())
	}
}

// TestRunShareTransition_NamesTheAddressItCouldNotReach.
//
// The other dual-path fallbacks print "waired-agent not running", which
// is a guess: connection-refused is equally what a wrong --mgmt or
// WAIRED_MGMT produces. For a log level that is a small wrong. For a kill
// switch it is not — the operator is told their computer stopped sharing
// while a daemon they never reached goes on serving (waired#1305).
func TestRunShareTransition_NamesTheAddressItCouldNotReach(t *testing.T) {
	dir := t.TempDir()
	addr, err := newClosedTCPAddr()
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := runShareTransition("http://"+addr, dir, state.SharingOff, "share off"); err != nil {
			t.Fatalf("runShareTransition: %v", err)
		}
	})
	if !strings.Contains(out, addr) {
		t.Errorf("the fallback did not name the address it could not reach:\n%s", out)
	}
	if strings.Contains(out, "not running") {
		t.Errorf("the fallback claimed the daemon is not running, which it did not check:\n%s", out)
	}
}
