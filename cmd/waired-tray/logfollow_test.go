package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchDaemonLogLevel(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   slog.Level
		wantOK bool
	}{
		{"ok debug", http.StatusOK, `{"level":"debug"}`, slog.LevelDebug, true},
		{"ok warn", http.StatusOK, `{"level":"warn"}`, slog.LevelWarn, true},
		{"404 older daemon", http.StatusNotFound, "not found", 0, false},
		{"unknown level", http.StatusOK, `{"level":"loud"}`, 0, false},
		{"garbage body", http.StatusOK, `{`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/waired/v1/log/level" {
					http.Error(w, "wrong path", http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got, ok := fetchDaemonLogLevel(context.Background(), srv.Client(), srv.URL)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("level = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFetchDaemonLogLevel_Unreachable(t *testing.T) {
	// A server that is closed immediately → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, ok := fetchDaemonLogLevel(context.Background(), &http.Client{Timeout: time.Second}, url); ok {
		t.Fatal("expected ok=false against a closed server")
	}
}

func TestFollowDaemonLogLevel_AppliesAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		followDaemonLogLevel(ctx, srv.URL, lv, 10*time.Millisecond)
		close(done)
	}()

	// The initial apply() runs before the first tick; poll until it lands.
	deadline := time.After(2 * time.Second)
	for lv.Level() != slog.LevelDebug {
		select {
		case <-deadline:
			t.Fatal("level never followed the daemon to debug")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("followDaemonLogLevel did not return after ctx cancel")
	}
}
