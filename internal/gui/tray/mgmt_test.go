package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_name":"a","peer_count":2,"phase":"active"}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceName != "a" || st.PeerCount != 2 || st.Phase != "active" {
		t.Errorf("unexpected: %+v", st)
	}
}

func TestClient_Identity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enrolled":true,"account_email":"x@y"}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !id.Enrolled || id.AccountEmail != "x@y" {
		t.Errorf("unexpected: %+v", id)
	}
}

func TestClient_Identity_404IsNotEnrolled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatalf("404 should be translated to not-enrolled, got err=%v", err)
	}
	if id.Enrolled {
		t.Errorf("expected Enrolled=false, got %+v", id)
	}
}

// The write-path tests (Pause/Resume/StopEngine, the 404 sentinels, and the
// empty-body Content-Type guard) live in mgmt_socket_unix_test.go: since
// waired#838 the tray sends mutating requests over the local IPC socket
// rather than the loopback TCP port, so they need a unix-socket test server.

func TestClient_DialError(t *testing.T) {
	c := NewClient("http://127.0.0.1:1") // unlikely to be listening
	if _, err := c.Status(context.Background()); err == nil {
		t.Errorf("expected dial error from down endpoint")
	}
}
