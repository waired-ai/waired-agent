package deauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// seedEnrolled writes a minimal enrolled state dir (identity with a CP URL
// + an access token) so Deregister has something to call.
func seedEnrolled(t *testing.T, dir, controlURL string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"device_id":"dev_1","network_id":"net_1","control_url":%q}`, controlURL)
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(id), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "access_token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeregister_ModeSelectsEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		mode     Mode
		wantPath string
	}{
		{"logout", ModeLogout, "/v1/devices/self/logout"},
		{"revoke", ModeRevoke, "/v1/devices/self/revoke"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth string
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			dir := t.TempDir()
			seedEnrolled(t, dir, srv.URL)

			outcome, err := Deregister(context.Background(), dir, tc.mode)
			if err != nil {
				t.Fatalf("Deregister: %v", err)
			}
			if outcome != OutcomeDone {
				t.Fatalf("outcome=%v want OutcomeDone", outcome)
			}
			if atomic.LoadInt32(&hits) != 1 {
				t.Errorf("expected 1 CP call, got %d", hits)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path=%q want %q", gotPath, tc.wantPath)
			}
			if gotAuth != "Bearer tok" {
				t.Errorf("auth=%q want Bearer tok", gotAuth)
			}
		})
	}
}

// A CP error surfaces as OutcomeDone (an attempt was made) with a non-nil
// error, so callers can warn. It must never panic or block.
func TestDeregister_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seedEnrolled(t, dir, srv.URL)

	outcome, err := Deregister(context.Background(), dir, ModeRevoke)
	if outcome != OutcomeDone {
		t.Errorf("outcome=%v want OutcomeDone (an attempt was made)", outcome)
	}
	if err == nil {
		t.Errorf("expected a surfaced error on 5xx")
	}
}

func TestDeregister_NotEnrolledSkips(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// State dir exists but has no identity.json / token.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}

	outcome, err := Deregister(context.Background(), dir, ModeRevoke)
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if outcome != OutcomeSkipped {
		t.Errorf("outcome=%v want OutcomeSkipped", outcome)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("not-enrolled must not contact the CP, got %d calls", hits)
	}
}

// A state dir with an identity but no pinned control URL has nothing to
// call and must skip silently.
func TestDeregister_NoControlURLSkips(t *testing.T) {
	dir := t.TempDir()
	seedEnrolled(t, dir, "") // empty control_url
	outcome, err := Deregister(context.Background(), dir, ModeRevoke)
	if err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if outcome != OutcomeSkipped {
		t.Errorf("outcome=%v want OutcomeSkipped", outcome)
	}
}
