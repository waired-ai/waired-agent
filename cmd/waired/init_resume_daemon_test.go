package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDaemonIdentityMapsEveryFailureToNoAnswer exercises the real
// daemonIdentity. It is a `var fn = func(...)` that nothing in the tree
// assigns, so without this nothing ever calls it (CLAUDE.md §Test
// discipline: "a `var xFn = realFn` seam needs a table test on realFn, or
// the real one is never called by any test").
//
// The contract under test is stated on the function itself: a nil answer
// means "no answer", and every caller must read that as "unknown", never
// as "not enrolled". A record of today's behaviour. Why it matters is
// #313 — the CLI's own view of the state dir is the thing under
// suspicion, so nil must not be allowed to masquerade as a verdict.
func TestDaemonIdentityMapsEveryFailureToNoAnswer(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantAnswer bool
		wantEnroll bool
	}{
		{name: "enrolled", status: 200, body: `{"enrolled":true,"device_id":"dev_a"}`, wantAnswer: true, wantEnroll: true},
		{name: "answered-not-enrolled", status: 200, body: `{"enrolled":false}`, wantAnswer: true},
		{name: "route-absent", status: 404, body: `404 page not found`},
		// The #785 case: the loopback read guard refuses a socket-only
		// route, and the refusal is indistinguishable here from having no
		// daemon at all. That is correct for this function and wrong for
		// the transport, which is why the fix is in the transport.
		{name: "read-guard-refused", status: 403, body: `{"error_code":"forbidden"}`},
		{name: "malformed", status: 200, body: `{"enrolled":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != identityPath {
					t.Errorf("path = %q, want %s", r.URL.Path, identityPath)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got := daemonIdentity(srv.URL)
			if !tt.wantAnswer {
				if got != nil {
					t.Fatalf("daemonIdentity = %+v, want nil (no answer)", got)
				}
				return
			}
			if got == nil {
				t.Fatal("daemonIdentity = nil, want the daemon's answer")
			}
			if got.Enrolled != tt.wantEnroll {
				t.Fatalf("Enrolled = %v, want %v", got.Enrolled, tt.wantEnroll)
			}
		})
	}
}

// TestDaemonIdentityWithNoDaemon pins the other half of the same
// contract: an absent daemon is "no answer" too.
func TestDaemonIdentityWithNoDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if got := daemonIdentity(url); got != nil {
		t.Fatalf("daemonIdentity against a closed listener = %+v, want nil", got)
	}
}

// TestConnectionFindingOverHTTP covers connectionFinding itself. Only the
// pure connectionFindingFrom had a test, so the half that talks to the
// daemon — the half #785 broke — was never exercised.
func TestConnectionFindingOverHTTP(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantSubject string
	}{
		{name: "connected", status: 200, body: `{"enrolled":true,"active":true}`, wantSubject: "network connection"},
		// Not signed in: nothing to say about a connection, so the row is
		// deliberately absent rather than reported as a fault.
		{name: "not-enrolled", status: 200, body: `{"enrolled":false}`},
		{name: "read-guard-refused", status: 403, body: `{"error_code":"forbidden"}`},
		{name: "malformed", status: 200, body: `{"enrolled":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != identityPath {
					t.Errorf("path = %q, want %s", r.URL.Path, identityPath)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			got := connectionFinding(t.Context(), srv.URL)
			if got.Subject != tt.wantSubject {
				t.Fatalf("Subject = %q, want %q", got.Subject, tt.wantSubject)
			}
		})
	}
}
