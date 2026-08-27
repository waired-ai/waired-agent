package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestClaudeSubDisplay locks the subagent display line, in particular the
// readable framing when Sub == "same" (it spells out the effective route
// without the old nested-paren "auto(prefer…" wart).
func TestClaudeSubDisplay(t *testing.T) {
	cases := []struct {
		name string
		pol  state.ClaudeRoutingPolicy
		want string
	}{
		{
			name: "same follows auto main",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteSame},
			want: "same as main  (auto — prefer Waired; visible fallback to Anthropic on failure)",
		},
		{
			name: "empty sub is treated as same",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteWaired, Sub: ""},
			want: "same as main  (waired — Waired only, unless a turn names a model or auto mode checks one)",
		},
		{
			name: "explicit anthropic sub",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAuto, Sub: state.ClaudeRouteAnthropic},
			want: "anthropic  (the real Anthropic API)",
		},
		{
			name: "explicit waired sub",
			pol:  state.ClaudeRoutingPolicy{Main: state.ClaudeRouteAnthropic, Sub: state.ClaudeRouteWaired},
			want: "waired  (Waired only, unless a turn names a model or auto mode checks one)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeSubDisplay(tc.pol)
			if got != tc.want {
				t.Fatalf("claudeSubDisplay(%+v) = %q, want %q", tc.pol, got, tc.want)
			}
			if strings.Contains(got, "((") || strings.Contains(got, ")(") {
				t.Fatalf("nested/garbled parens in %q", got)
			}
		})
	}
}

// TestClaudeRouteHint keeps the parenthesized main-line hint and the bare-text
// form in sync (the former just wraps the latter).
func TestClaudeRouteHint(t *testing.T) {
	for _, r := range []state.ClaudeRouteClass{state.ClaudeRouteAuto, state.ClaudeRouteWaired, state.ClaudeRouteAnthropic} {
		hint := claudeRouteHint(r)
		text := claudeRouteHintText(r)
		if want := "  (" + text + ")"; hint != want {
			t.Fatalf("claudeRouteHint(%q) = %q, want %q", r, hint, want)
		}
		if strings.HasPrefix(text, "(") || strings.HasSuffix(text, ")") {
			t.Fatalf("claudeRouteHintText(%q) should be bare, got %q", r, text)
		}
	}
}

// TestClaudeServedDisplay locks the `last served:` value for each combination
// of the fields behind it. The peer branch carries the weight: until #755 the
// gateway only reported the serving model for a remapped id, so a mesh-served
// turn left both fields empty and this rendering had never run against a real
// state. Record of today's behaviour.
func TestClaudeServedDisplay(t *testing.T) {
	served := time.Date(2026, 8, 13, 10, 4, 11, 0, time.UTC)
	stamp := served.Local().Format(time.RFC3339)
	cases := []struct {
		name string
		st   management.ClaudeRoutingState
		// peerName is what the mesh lookup resolved LastServedBy to, "" when
		// this device could not name it (waired-agent#1040).
		peerName string
		want     string
	}{
		{
			name: "this device",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.5-2b", LastServedAt: served},
			want: stamp + " — qwen3.5-2b (this device)",
		},
		{
			// waired-agent#1040: the owner could not tell which computer had
			// answered without a wire capture, and a DeviceID is not an
			// answer to that question.
			name:     "a named peer",
			st:       management.ClaudeRoutingState{LastLocalModel: "qwen3.6-27b", LastServedBy: "dev-mag", LastServedAt: served},
			peerName: "sv-mag",
			want:     stamp + " — qwen3.6-27b (peer sv-mag)",
		},
		{
			// A machine that has left the mesh has no name here. The id is
			// still better than nothing, and it is what shipped.
			name: "a peer this device cannot name",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.6-27b", LastServedBy: "shared-7", LastServedAt: served},
			want: stamp + " — qwen3.6-27b (peer shared-7)",
		},
		{
			name:     "peer without a model",
			st:       management.ClaudeRoutingState{LastServedBy: "dev-mag", LastServedAt: served},
			peerName: "sv-mag",
			want:     stamp + " — peer sv-mag",
		},
		{
			// An agent predating LastServedAt reports no time; the line
			// falls back to the form it had before #755 rather than
			// printing a zero date.
			name: "no time reported",
			st:   management.ClaudeRoutingState{LastLocalModel: "qwen3.5-2b"},
			want: "qwen3.5-2b (this device)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeServedDisplay(tc.st, tc.peerName); got != tc.want {
				t.Fatalf("claudeServedDisplay(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}

// claudePeerNameLookup goes through the same display function every other
// surface uses, so a Public Share machine is its grant pseudonym and its real
// device id never reaches the terminal (spec §8.5). It is best-effort: this
// is one line of a status report, and a mesh that cannot be read prints the
// identifier rather than failing the command.
func TestClaudePeerNameLookup(t *testing.T) {
	snap := &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		{DeviceID: "dev-mag", DeviceName: "sv-mag"},
		{DeviceID: "dev-pub", DeviceName: "stranger-workstation",
			Grant: &signer.PeerGrant{ID: "g1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}},
	}}
	restore := fetchMeshSnapshotCtx
	t.Cleanup(func() { fetchMeshSnapshotCtx = restore })
	fetchMeshSnapshotCtx = func(context.Context, string) (*inferencemesh.Snapshot, error) { return snap, nil }

	for _, tc := range []struct {
		name     string
		deviceID string
		want     string
	}{
		{"a peer on the mesh", "dev-mag", "sv-mag"},
		{"a public machine is its pseudonym", "dev-pub", "guest-a7f3"},
		{"a peer that has left has no name here", "dev-gone", ""},
		{"nothing to look up", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudePeerNameLookup("", tc.deviceID); got != tc.want {
				t.Errorf("claudePeerNameLookup(%q) = %q, want %q", tc.deviceID, got, tc.want)
			}
		})
	}

	t.Run("a mesh that cannot be read names nothing", func(t *testing.T) {
		fetchMeshSnapshotCtx = func(context.Context, string) (*inferencemesh.Snapshot, error) {
			return nil, errors.New("no mesh route")
		}
		if got := claudePeerNameLookup("", "dev-mag"); got != "" {
			t.Errorf("claudePeerNameLookup = %q, want empty", got)
		}
	})
}
