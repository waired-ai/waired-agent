package tray

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_EnableShare_OK(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.EnableShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/inference/share/enable" {
		t.Errorf("server saw %q", got)
	}
}

func TestClient_DisableShare_OK(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.DisableShare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/inference/share/disable" {
		t.Errorf("server saw %q", got)
	}
}

func TestClient_ShareToggle_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.EnableShare(context.Background()); !errors.Is(err, ErrShareUnsupported) {
		t.Errorf("expected ErrShareUnsupported (Enable), got %v", err)
	}
	if err := c.DisableShare(context.Background()); !errors.Is(err, ErrShareUnsupported) {
		t.Errorf("expected ErrShareUnsupported (Disable), got %v", err)
	}
}
