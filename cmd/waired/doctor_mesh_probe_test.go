package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
)

// TestMeshFinding_MeasurementOutranksTheClaim is the case waired#1137
// found and the reason this measurement exists: macmini's overlay data
// path was dead in both directions, and all three hosts' doctors said
// `✓ mesh peers — 2/3 reachable, 2 ready`.
//
// Product contract from waired#1137 and the owner ruling of 2026-08-12
// ("measure it"), not a record of today's behaviour.
func TestMeshFinding_MeasurementOutranksTheClaim(t *testing.T) {
	m := management.MeshState{PeersEnrolled: 3, PeersReachable: 2, PeersReady: 2}
	probes := []meshPeerProbe{
		{Name: "macmini", Answered: false},
		{Name: "xps15", Answered: false},
	}

	got := meshFinding(m, probes)

	// Warn, not fail: probeObservability's findings never fail the run
	// (TestProbeObservability_NoFailNeverEmitsStatusFail). What #1137
	// reported broken was the WORDING — a ✓ on a host where nothing
	// answered — not the severity.
	if got.Status != integration.StatusWarn {
		t.Errorf("status = %s, want warn — nothing answered on the data path", got.Status)
	}
	for _, want := range []string{"macmini", "xps15", "2/3 reported reachable"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
		}
	}
	// The published number must not be the headline when it is the one
	// under suspicion.
	if strings.HasPrefix(got.Detail, "2/3 reachable") {
		t.Errorf("detail = %q leads with the unmeasured claim", got.Detail)
	}
}

func TestMeshFinding_PartialAnswerNamesOnlyTheSilentOnes(t *testing.T) {
	m := management.MeshState{PeersEnrolled: 3, PeersReachable: 2, PeersReady: 2}
	got := meshFinding(m, []meshPeerProbe{
		{Name: "xps15", Answered: true},
		{Name: "macmini", Answered: false},
	})
	if got.Status != integration.StatusWarn {
		t.Errorf("status = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "macmini") {
		t.Errorf("detail = %q does not name the peer that went quiet", got.Detail)
	}
	if strings.Contains(got.Detail, "xps15") {
		t.Errorf("detail = %q blames a peer that answered", got.Detail)
	}
	if !strings.Contains(got.Detail, "only 1 answered") {
		t.Errorf("detail = %q does not say how many answered", got.Detail)
	}
}

// A fully answered mesh says so, so the line never reads identically
// whether it was checked or merely reported.
func TestMeshFinding_AllAnsweredIsMarkedMeasured(t *testing.T) {
	m := management.MeshState{PeersEnrolled: 3, PeersReachable: 3, PeersReady: 3}
	got := meshFinding(m, []meshPeerProbe{
		{Name: "a", Answered: true}, {Name: "b", Answered: true}, {Name: "c", Answered: true},
	})
	if got.Status != integration.StatusOK {
		t.Errorf("status = %s, want ok", got.Status)
	}
	if !strings.Contains(got.Detail, "(measured)") {
		t.Errorf("detail = %q does not distinguish a measured count", got.Detail)
	}
}

// No measurement is not a failed measurement. A host that could not ask —
// no daemon, no mesh route, no reported-reachable peers — falls back to
// the published counts, unqualified, exactly as before.
func TestMeshFinding_NoProbesKeepsTheOldBehaviour(t *testing.T) {
	m := management.MeshState{PeersEnrolled: 3, PeersReachable: 3, PeersReady: 3}
	got := meshFinding(m, nil)
	if got.Status != integration.StatusOK {
		t.Errorf("status = %s, want ok", got.Status)
	}
	if got.Detail != "3/3 reachable, 3 ready" {
		t.Errorf("detail = %q, want the pre-measurement wording verbatim", got.Detail)
	}
	if strings.Contains(got.Detail, "measured") {
		t.Errorf("detail = %q claims a measurement that did not happen", got.Detail)
	}
}

func TestMeshFinding_SoloDeploymentUnchanged(t *testing.T) {
	got := meshFinding(management.MeshState{}, nil)
	if got.Status != integration.StatusOK || !strings.Contains(got.Detail, "solo deployment") {
		t.Errorf("got %+v, want the unchanged solo line", got)
	}
}

func TestSilentPeers_SortedAndOnlyTheQuietOnes(t *testing.T) {
	got := silentPeers([]meshPeerProbe{
		{Name: "zeta", Answered: false},
		{Name: "alpha", Answered: false},
		{Name: "mid", Answered: true},
	})
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("silentPeers = %v, want [alpha zeta]", got)
	}
}

// probeMeshPeers must skip the peers the snapshot already calls stale or
// silent: they are not a surprise, and the budget is better spent on the
// claims worth testing.
func TestProbeMeshPeers_OnlyProbesReportedReachable(t *testing.T) {
	restoreSnap := swapMeshSnapshot(t, &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		{DeviceName: "live", OverlayIP: "100.64.0.2"},
		{DeviceName: "stale-one", OverlayIP: "100.64.0.3", Stale: true},
		{DeviceName: "silent-one", OverlayIP: "100.64.0.4", Silent: true},
	}})
	defer restoreSnap()

	var asked []string
	var mu chanGuard
	restorePing := swapPing(t, func(_ context.Context, _, peer string) bool {
		mu.add(&asked, peer)
		return true
	})
	defer restorePing()

	got := probeMeshPeers(context.Background(), "http://127.0.0.1:1")

	if len(got) != 1 || got[0].Name != "live" {
		t.Fatalf("probes = %+v, want just the reported-reachable peer", got)
	}
	if len(asked) != 1 || asked[0] != "live" {
		t.Errorf("pinged %v, want only [live]", asked)
	}
}

// The pings run concurrently, which is what keeps the cost of measuring
// at one slow peer rather than the sum of them — the whole reason the
// budget is a wall-clock ceiling.
func TestProbeMeshPeers_PingsConcurrently(t *testing.T) {
	restoreSnap := swapMeshSnapshot(t, &inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
		{DeviceName: "a"}, {DeviceName: "b"}, {DeviceName: "c"},
	}})
	defer restoreSnap()

	const each = 150 * time.Millisecond
	var inflight, peak int32
	restorePing := swapPing(t, func(ctx context.Context, _, _ string) bool {
		n := atomic.AddInt32(&inflight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		select {
		case <-time.After(each):
		case <-ctx.Done():
		}
		atomic.AddInt32(&inflight, -1)
		return true
	})
	defer restorePing()

	start := time.Now()
	got := probeMeshPeers(context.Background(), "http://127.0.0.1:1")
	elapsed := time.Since(start)

	if len(got) != 3 {
		t.Fatalf("probes = %+v, want 3", got)
	}
	if atomic.LoadInt32(&peak) < 2 {
		t.Errorf("peak concurrency = %d, want the pings to overlap", peak)
	}
	// Serial would be 3×150ms; allow generous slack for a loaded runner
	// while still failing a genuinely serial implementation.
	if elapsed > 2*each+200*time.Millisecond {
		t.Errorf("took %v, want roughly one ping's worth — the pings are serial", elapsed)
	}
}

// No peers, no daemon, no mesh route: nil, so the caller keeps the
// published counts rather than reporting nothing measured.
func TestProbeMeshPeers_NothingToMeasureReturnsNil(t *testing.T) {
	restoreSnap := swapMeshSnapshot(t, &inferencemesh.Snapshot{})
	defer restoreSnap()
	if got := probeMeshPeers(context.Background(), "http://127.0.0.1:1"); got != nil {
		t.Errorf("probes = %+v, want nil for an empty mesh", got)
	}
}

// End to end through probeObservability: the daemon reports 2 of 3
// reachable, and neither answers the overlay. This is the shape of the
// three rc8 hosts in waired#1137.
func TestProbeObservability_MeasuredMeshContradictsTheReport(t *testing.T) {
	srv := newObservabilityServer(t, &observabilityMux{
		state: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{
				Agent: management.AgentState{EngineReady: true, ModelID: "qwen3:8b"},
				Mesh:  management.MeshState{PeersEnrolled: 3, PeersReachable: 2, PeersReady: 2},
			})
		},
		events: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{})
		},
		mesh: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{
				{DeviceName: "macmini"}, {DeviceName: "xps15"},
			}})
		},
		ping: func(w http.ResponseWriter, _ *http.Request) {
			// The daemon answers, but the peer did not: 502 is what
			// handlePing returns when PingPeer fails.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"i/o timeout"}`))
		},
	})

	got, _ := probeObservability(context.Background(), srv.URL)

	var mesh *integration.AuditFinding
	for i := range got {
		if got[i].Subject == "mesh peers" {
			mesh = &got[i]
		}
	}
	if mesh == nil {
		t.Fatal("no mesh peers finding")
	}
	if mesh.Status != integration.StatusWarn {
		t.Errorf("status = %s, want warn: the report says reachable, the overlay says otherwise (detail %q)",
			mesh.Status, mesh.Detail)
	}
	// The old line would have been a tick. That is the regression.
	if mesh.Status == integration.StatusOK {
		t.Error("a mesh that answered nothing must not render as a tick")
	}
	for _, want := range []string{"macmini", "xps15"} {
		if !strings.Contains(mesh.Detail, want) {
			t.Errorf("detail = %q does not name %q", mesh.Detail, want)
		}
	}
}

// --- helpers ---

// swapMeshSnapshot replaces the snapshot fetch so the probe runs without
// a daemon. The fake takes and honours the real arguments' shape.
func swapMeshSnapshot(t *testing.T, snap *inferencemesh.Snapshot) func() {
	t.Helper()
	prev := fetchMeshSnapshotCtx
	fetchMeshSnapshotCtx = func(context.Context, string) (*inferencemesh.Snapshot, error) {
		return snap, nil
	}
	return func() { fetchMeshSnapshotCtx = prev }
}

func swapPing(t *testing.T, fn func(context.Context, string, string) bool) func() {
	t.Helper()
	prev := pingPeerOverOverlay
	pingPeerOverOverlay = fn
	return func() { pingPeerOverOverlay = prev }
}

// chanGuard serialises appends from the concurrent probe goroutines.
type chanGuard struct{ mu chan struct{} }

func (g *chanGuard) add(dst *[]string, v string) {
	if g.mu == nil {
		g.mu = make(chan struct{}, 1)
	}
	g.mu <- struct{}{}
	*dst = append(*dst, v)
	<-g.mu
}
