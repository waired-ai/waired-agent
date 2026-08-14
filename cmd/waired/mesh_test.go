package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchMeshSnapshotCtx table-tests the real fetcher rather than a fake
// standing in for it: it is a `var fn = func(...)` seam that the doctor's
// tests swap, and CLAUDE.md §Test discipline requires the real one be
// exercised somewhere or no test ever calls it.
//
// A record of today's behaviour, not a product contract — except the 404
// wording, which is load-bearing: it is what tells "this daemon predates
// the mesh API" from "this daemon did not answer".
func TestFetchMeshSnapshotCtx(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantPeers  int
		wantNilErr bool
	}{
		{name: "ok", status: 200, body: `{"peers":[{"device_id":"dev_1"},{"device_id":"dev_2"}]}`, wantPeers: 2, wantNilErr: true},
		{name: "empty-mesh", status: 200, body: `{"peers":[]}`, wantPeers: 0, wantNilErr: true},
		{name: "daemon-predates-the-route", status: 404, body: `404 page not found`, wantErr: "Phase 3+ feature"},
		{name: "guard-refused-the-read", status: 403, body: `{"error_code":"forbidden"}`, wantErr: "mgmt API status 403"},
		{name: "server-error", status: 500, body: ``, wantErr: "mgmt API status 500"},
		{name: "unparseable", status: 200, body: `{"peers":`, wantErr: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != meshSnapshotPath {
					t.Errorf("path = %q, want %s", r.URL.Path, meshSnapshotPath)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			snap, err := fetchMeshSnapshotCtx(context.Background(), srv.URL)
			if tt.wantNilErr {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if len(snap.Peers) != tt.wantPeers {
					t.Fatalf("peers = %d, want %d", len(snap.Peers), tt.wantPeers)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestFetchMeshSnapshotAcceptsBothAddressForms pins that the two forms in
// use both reach the daemon. `waired peers list` defaults --mgmt to the
// bare "127.0.0.1:9476" (management.DefaultListen) while `waired worker`
// passes a full URL, and mgmtReadRoute cannot take the bare form at all —
// url.Parse rejects a leading "host:port" with "first path segment in URL
// cannot contain colon", so the normalisation is what keeps both working.
func TestFetchMeshSnapshotAcceptsBothAddressForms(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != meshSnapshotPath {
			t.Errorf("path = %q, want %s", r.URL.Path, meshSnapshotPath)
		}
		_, _ = w.Write([]byte(`{"peers":[{"device_id":"dev_1"}]}`))
	}))
	defer srv.Close()

	for _, addr := range []string{srv.URL, meshAddrFromURL(srv.URL), srv.URL + "/"} {
		snap, err := fetchMeshSnapshot(addr, 2*time.Second)
		if err != nil {
			t.Fatalf("fetchMeshSnapshot(%q): %v", addr, err)
		}
		if len(snap.Peers) != 1 {
			t.Fatalf("fetchMeshSnapshot(%q): peers = %d, want 1", addr, len(snap.Peers))
		}
	}
	if hits != 3 {
		t.Fatalf("server saw %d requests, want 3", hits)
	}
}

// TestFetchMeshSnapshotHonoursContext pins that the caller's deadline
// governs, which is why the doctor's probe uses the ctx form: its own 8s
// budget has to be able to cut this short.
func TestFetchMeshSnapshotHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"peers":[]}`))
	}))
	defer func() { close(release); srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := fetchMeshSnapshotCtx(ctx, srv.URL); err == nil {
		t.Fatal("err = nil, want the context deadline")
	}
}
