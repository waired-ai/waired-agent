package tray

import (
	"context"
	"errors"
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

	c := NewClient(srv.URL)
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

	c := NewClient(srv.URL)
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

	c := NewClient(srv.URL)
	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatalf("404 should be translated to not-enrolled, got err=%v", err)
	}
	if id.Enrolled {
		t.Errorf("expected Enrolled=false, got %+v", id)
	}
}

func TestClient_Pause_UnsupportedYields404Sentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.Pause(context.Background()); !errors.Is(err, ErrPauseUnsupported) {
		t.Errorf("expected ErrPauseUnsupported, got %v", err)
	}
	if err := c.Resume(context.Background()); !errors.Is(err, ErrPauseUnsupported) {
		t.Errorf("expected ErrPauseUnsupported, got %v", err)
	}
}

func TestClient_Pause_OK(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/pause" {
		t.Errorf("server saw %q", got)
	}
}

// TestClient_EmptyBodyPost_SetsJSONContentType guards the waired#836 fix: the
// browser-hardened management API 415s a write without Content-Type:
// application/json, so the tray's empty-body POST helpers must set it too.
// c.Pause exercises post(); c.StopEngine exercises postWithUnsupported().
func TestClient_EmptyBodyPost_SetsJSONContentType(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(c *Client) error
	}{
		{"pause", func(c *Client) error { return c.Pause(context.Background()) }},
		{"stop-engine", func(c *Client) error { return c.StopEngine(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ct string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ct = r.Header.Get("Content-Type")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			if err := tc.call(NewClient(srv.URL)); err != nil {
				t.Fatal(err)
			}
			if ct != "application/json" {
				t.Errorf("empty-body POST Content-Type = %q, want application/json", ct)
			}
		})
	}
}

func TestClient_DialError(t *testing.T) {
	c := NewClient("http://127.0.0.1:1") // unlikely to be listening
	if _, err := c.Status(context.Background()); err == nil {
		t.Errorf("expected dial error from down endpoint")
	}
}
