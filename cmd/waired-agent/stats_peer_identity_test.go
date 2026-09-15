package main

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
)

// Rows as Status builds them: one of this account's computers, a Public
// Share peer (DisplayID = grant pseudonym) and a Team Share peer
// (DisplayID = "<device> (<owner>)"). The device ids are fakes; what the
// tests check is that the grant rows' ids never reach a record.
const (
	ownPeerID    = "dev_own_peer_a"
	guestPeerID  = "dev_guest_real_id"
	guestDisplay = "guest-0001"
	teamPeerID   = "dev_team_real_id"
	teamDisplay  = "laptop (Alex Example)"
)

func statsTestPeers() []management.PeerStatus {
	return []management.PeerStatus{
		{DeviceID: ownPeerID, DisplayID: ownPeerID, CurrentPath: "direct"},
		{DeviceID: guestPeerID, DisplayID: guestDisplay, Public: true, CurrentPath: "relay"},
		{DeviceID: teamPeerID, DisplayID: teamDisplay, Team: true, CurrentPath: "direct"},
	}
}

// PRODUCT CONTRACT: public share spec §8.5 — logs and events name a
// public peer by its pseudonym and never keep its real device id — and
// team share spec §10.2 for a teammate's computer. waired-agent#1368
// found both sides of a grant logging each other's real id through
// waired_agent_peers_changed.
func TestLogPeerSetChange_GrantPeersJoinAndLeaveByDisplayID(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	own := management.Status{Peers: statsTestPeers()[:1]}
	all := management.Status{Peers: statsTestPeers()}

	set := logPeerSetChange(nil, own)
	set = logPeerSetChange(set, all)
	logPeerSetChange(set, own)

	recs := h.snapshot()
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want a join and a leave", recs)
	}
	want := []string{guestDisplay, teamDisplay} // sorted
	if joined, _ := recs[0].Attrs["joined"].([]string); !reflect.DeepEqual(joined, want) {
		t.Errorf("joined = %v, want %v", recs[0].Attrs["joined"], want)
	}
	if left, _ := recs[1].Attrs["left"].([]string); !reflect.DeepEqual(left, want) {
		t.Errorf("left = %v, want %v", recs[1].Attrs["left"], want)
	}
	assertNoGrantPeerIDs(t, recs)
}

// A row with no identifier names nobody. Path state for a peer that had
// already left the map used to come back as exactly such a row, and it
// was logged as `joined:[""]` (waired-agent#1372).
func TestLogPeerSetChange_RowWithoutAnIdentifierIsNotAPeer(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	before := management.Status{Peers: []management.PeerStatus{{DeviceID: ownPeerID}}}
	after := management.Status{Peers: []management.PeerStatus{{DeviceID: ownPeerID}, {DeviceID: "", CurrentPath: "relay"}}}

	set := logPeerSetChange(nil, before)
	logPeerSetChange(set, after)
	if recs := h.snapshot(); len(recs) != 0 {
		t.Errorf("records = %+v, want none: an empty id is not a peer joining", recs)
	}
}

// PRODUCT CONTRACT: public share spec §8.5 (see above). The DEBUG record
// carries the whole PeerStatus rows, so the grant rows' device_id is
// replaced — and the Status the caller holds is left alone, since the
// management API serves it and the router pin needs the real id (#768).
func TestEmitStatsRecord_DebugPeersNameGrantPeersByDisplayID(t *testing.T) {
	h, restore := withCaptureLogger(t)
	defer restore()

	st := management.Status{DeviceID: "dev_self", PeerCount: 3, Peers: statsTestPeers()}
	emitStatsRecord(st, nil)

	var debug *captured
	recs := h.snapshot()
	for i := range recs {
		if recs[i].Level == slog.LevelDebug && recs[i].Msg == "waired_agent_stats_peers" {
			debug = &recs[i]
		}
	}
	if debug == nil {
		t.Fatal("no DEBUG waired_agent_stats_peers record")
	}
	peers, ok := debug.Attrs["peers"].([]management.PeerStatus)
	if !ok {
		t.Fatalf("DEBUG peers attr type = %T", debug.Attrs["peers"])
	}
	assertPeerIDs(t, peers, []string{ownPeerID, guestDisplay, teamDisplay})
	assertNoGrantPeerIDs(t, recs)
	if st.Peers[1].DeviceID != guestPeerID || st.Peers[2].DeviceID != teamPeerID {
		t.Errorf("emitStatsRecord rewrote the caller's Status: %+v", st.Peers)
	}
}

// The Cloud Logging payload follows the same rule for grant rows, and
// keeps the device id of this account's own computers: the testnet
// fallback runner finds the other VM of the same network by it (waired
// scripts/dev/lib/testnet_path_verdict.py `_find_peer`).
func TestBuildPayload_GrantPeersByDisplayIDOwnPeersByDeviceID(t *testing.T) {
	got := buildPayload("waired_agent_stats", management.Status{Peers: statsTestPeers()})
	peers, ok := got["peers"].([]management.PeerStatus)
	if !ok {
		t.Fatalf("payload peers type = %T", got["peers"])
	}
	assertPeerIDs(t, peers, []string{ownPeerID, guestDisplay, teamDisplay})
}

// Status fills DisplayID for every grant row; a row built without one
// still does not fall back to the device id.
func TestPeerLogID_GrantRowWithoutDisplayID(t *testing.T) {
	cases := []struct {
		name string
		in   management.PeerStatus
		want string
	}{
		{"own", management.PeerStatus{DeviceID: ownPeerID}, ownPeerID},
		{"public", management.PeerStatus{DeviceID: guestPeerID, Public: true}, inferencemesh.PublicPeerLabel},
		{"team", management.PeerStatus{DeviceID: teamPeerID, Team: true}, inferencemesh.TeamPeerFallbackLabel},
	}
	for _, c := range cases {
		if got := peerLogID(c.in); got != c.want {
			t.Errorf("%s: peerLogID = %q, want %q", c.name, got, c.want)
		}
	}
}

func assertPeerIDs(t *testing.T, peers []management.PeerStatus, want []string) {
	t.Helper()
	got := make([]string, 0, len(peers))
	for _, p := range peers {
		got = append(got, p.DeviceID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("peer device_id values = %v, want %v", got, want)
	}
}

func assertNoGrantPeerIDs(t *testing.T, recs []captured) {
	t.Helper()
	for _, r := range recs {
		for k, v := range r.Attrs {
			s := fmt.Sprint(v)
			if strings.Contains(s, guestPeerID) || strings.Contains(s, teamPeerID) {
				t.Errorf("record %q attr %q carries a grant peer's device id: %s", r.Msg, k, s)
			}
		}
	}
}
