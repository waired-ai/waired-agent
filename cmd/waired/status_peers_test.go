package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// PRODUCT CONTRACT (public share spec §8.5, waired-agent#809, owner ruling
// 2026-08-21): `waired status` may not print a Public Share peer's real
// device identifier, and must leave every row that is one of your own
// machines exactly as it was.
//
// Asserted as a PROHIBITION rather than as an expected document. The thing
// that must not happen is a real id reaching the terminal, and an equality
// test against a rendered blob would go green the moment someone reworded
// a neighbouring field — while a leak reintroduced through some other
// field would not fail it at all.
const (
	publicPeerRealID = "dev_stranger0000001"
	ownPeerRealID    = "dev_mine00000000001"
)

func statusBodyWithPeers(t *testing.T) []byte {
	t.Helper()
	// The shape /waired/v1/status actually emits, with the fields that
	// carry an identifier: the peer row itself, and the "which peers have
	// answered" bookkeeping that quotes the same ids.
	return []byte(`{
	  "disco_enabled": true,
	  "peers": [
	    {"device_id": "` + ownPeerRealID + `", "display_id": "` + ownPeerRealID + `",
	     "device_name": "studio-desktop", "current_path": "direct"},
	    {"device_id": "` + publicPeerRealID + `", "display_id": "guest-a7f3",
	     "public": true, "current_path": "relay"}
	  ]
	}`)
}

// renderStatusPeers runs the body through the function `waired status`
// actually calls, and captures what a terminal would show.
//
// Deliberately prettyPrintStatus and not scrubStatusPeersForDisplay: a
// test that called the scrub directly would still pass with the call site
// removed, which is precisely the failure being guarded against.
func renderStatusPeers(t *testing.T, body []byte) string {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = prettyPrintStatus(body) })
	if err != nil {
		t.Fatalf("prettyPrintStatus: %v", err)
	}
	return out
}

func TestStatusPeers_APublicPeersRealIDNeverReachesTheTerminal(t *testing.T) {
	body := statusBodyWithPeers(t)
	// Anti-vacuity: the leak has to be present in the input, or the
	// assertion below proves nothing.
	if !strings.Contains(string(body), publicPeerRealID) {
		t.Fatal("the fixture no longer carries a public peer's real id, so this test " +
			"cannot observe whether it is withheld")
	}

	got := renderStatusPeers(t, body)

	if strings.Contains(got, publicPeerRealID) {
		t.Errorf("`waired status` printed a Public Share peer's real device id:\n%s", got)
	}
	if !strings.Contains(got, "guest-a7f3") {
		t.Errorf("the grant pseudonym is missing, so the row names nothing at all:\n%s", got)
	}
}

func TestStatusPeers_YourOwnMachinesAreUntouched(t *testing.T) {
	got := renderStatusPeers(t, statusBodyWithPeers(t))
	if !strings.Contains(got, ownPeerRealID) {
		t.Errorf("your own machine's device id was scrubbed too — the ruling is that the "+
			"substitution applies to public peers only:\n%s", got)
	}
	if !strings.Contains(got, "studio-desktop") {
		t.Errorf("an own-machine field went missing in the re-encode:\n%s", got)
	}
}

// A public peer whose grant carries no pseudonym still must not fall back
// to the id. The daemon fills display_id with the public-machine label in
// that case; an agent predating that sends none, and the CLI substitutes
// the bare label rather than the id it is here to withhold.
func TestStatusPeers_NoPseudonymFallsBackToTheLabelNotTheID(t *testing.T) {
	body := []byte(`{"peers": [{"device_id": "` + publicPeerRealID + `", "public": true}]}`)
	got := renderStatusPeers(t, body)
	if strings.Contains(got, publicPeerRealID) {
		t.Errorf("a public peer with no pseudonym fell back to its real device id:\n%s", got)
	}
	if !strings.Contains(got, inferencemesh.PublicPeerLabel) {
		t.Errorf("the row names nothing at all:\n%s", got)
	}
}

// The command's contract is that it shows what the daemon said. A field
// this build has never heard of must still reach the terminal — decoding
// into a typed struct would drop it silently, which is why the scrub walks
// the decoded document instead.
func TestStatusPeers_UnknownFieldsSurvive(t *testing.T) {
	body := []byte(`{"peers": [{"device_id": "` + publicPeerRealID + `", "public": true,
	  "display_id": "guest-a7f3", "a_field_from_a_newer_daemon": "keep me"}],
	  "a_top_level_field_from_a_newer_daemon": 42}`)
	got := renderStatusPeers(t, body)
	if !strings.Contains(got, "a_field_from_a_newer_daemon") || !strings.Contains(got, "keep me") {
		t.Errorf("a peer field this build does not know was dropped:\n%s", got)
	}
	if !strings.Contains(got, "a_top_level_field_from_a_newer_daemon") {
		t.Errorf("a top-level field this build does not know was dropped:\n%s", got)
	}
}

// The command itself, end to end against a fake daemon.
//
// The tests above call prettyPrintStatus, which pins the rendering but not
// the WIRING: `waired status` used to hand the body to prettyPrint, and
// swapping that one call back would leave every assertion above green.
// This is the test that would not be.
func TestRunStatusBody_DoesNotPrintAPublicPeersRealID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(paths.EnvOverride, dir)
	if err := identity.Save(dir, &identity.Identity{
		DeviceID:     "dev_self0000000001",
		DeviceName:   "this-machine",
		AccountEmail: "someone@example.test",
		NetworkID:    "net_1",
		NetworkName:  "home",
		ControlURL:   "https://cp.example.test",
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	body := statusBodyWithPeers(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	var err error
	out := captureStdout(t, func() { err = runStatusBody(srv.URL, dir, false, "") })
	if err != nil {
		t.Fatalf("runStatusBody: %v", err)
	}
	// Anti-vacuity: without the daemon's rows in the output the
	// prohibition below holds for the wrong reason.
	if !strings.Contains(out, "studio-desktop") {
		t.Fatalf("the daemon's peer rows never reached the output, so this test cannot "+
			"observe whether the public row was scrubbed:\n%s", out)
	}
	if strings.Contains(out, publicPeerRealID) {
		t.Errorf("`waired status` printed a Public Share peer's real device id:\n%s", out)
	}
	if !strings.Contains(out, ownPeerRealID) {
		t.Errorf("your own machine's device id went missing:\n%s", out)
	}
}

// A document with no peers, or one shaped nothing like the status body, is
// left alone rather than panicking. `waired status` prints whatever the
// daemon answered, including from a daemon that is not this one.
func TestStatusPeers_ToleratesDocumentsItDoesNotRecognise(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"peers": null}`,
		`{"peers": "not an array"}`,
		`{"peers": [null, 7, "row"]}`,
		`[1, 2, 3]`,
	} {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("fixture %q does not decode: %v", body, err)
		}
		scrubStatusPeersForDisplay(v) // must not panic
	}
}
