package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A computer with no engine declares the smallest window it can reach
// (waired-agent#1246).
//
// PIN: product contract. Smallest, not largest: this number sizes ONE
// session and the rows it covers are several computers, so declaring more
// than the smallest means a turn is compacted only after the gateway has
// already refused it.
//
// Driven through the real fetch against an httptest listener rather than a
// swapped seam, so the parsing and the route are exercised too
// (CLAUDE.md §Test discipline).
func TestClaudeReachableContextWindow(t *testing.T) {
	serving := func(window int) *signer.InferenceState {
		return &signer.InferenceState{
			Reachable: true, Models: []string{"qwen3-8b-instruct"}, ContextWindow: window,
		}
	}
	for _, tc := range []struct {
		name string
		snap inferencemesh.Snapshot
		want int
	}{
		{
			name: "the smallest of several",
			snap: inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
				{DeviceID: "dev_a", InferenceState: serving(131072)},
				{DeviceID: "dev_b", InferenceState: serving(32768)},
				{DeviceID: "dev_c", InferenceState: serving(65536)},
			}},
			want: 32768,
		},
		{
			// A peer that is not serving is not a row this host offers, so
			// its window is not a number it may declare.
			name: "peers that are not serving do not count",
			snap: inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
				{DeviceID: "dev_a", InferenceState: serving(8192), Stale: true},
				{DeviceID: "dev_b", InferenceState: &signer.InferenceState{Reachable: false, ContextWindow: 4096}},
				{DeviceID: "dev_c", InferenceState: serving(65536)},
			}},
			want: 65536,
		},
		{
			// Declining beats guessing: this decides what an elevated
			// process tells Claude Code about a window.
			name: "a peer that declares no window is skipped",
			snap: inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
				{DeviceID: "dev_a", InferenceState: serving(0)},
				{DeviceID: "dev_b", InferenceState: serving(16384)},
			}},
			want: 16384,
		},
		{
			name: "nothing reachable declares nothing",
			snap: inferencemesh.Snapshot{},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != meshSnapshotPath {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.snap)
			}))
			defer srv.Close()
			if got := claudeReachableContextWindow(srv.URL); got != tc.want {
				t.Errorf("claudeReachableContextWindow = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("an unreachable daemon declares nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if got := claudeReachableContextWindow(srv.URL); got != 0 {
			t.Errorf("claudeReachableContextWindow = %d, want 0", got)
		}
	})
}
