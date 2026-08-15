package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// The doctor's "mesh peers" line used to be entirely second-hand. Its
// reachable count came from the published mesh snapshot, where a peer
// counts as reachable when `!Stale && !Silent`
// (cmd/waired-agent/main.go:2312) — control-plane freshness plus disco's
// verdict — and its ready count is the peer's own claim about itself. No
// byte ever crossed the overlay to check.
//
// Worse, disco's own contract says a peer it has never heard a pong from
// is absent from the snapshot and consumers "treat absence as unknown,
// default trust" (internal/network/disco/service.go:471), so a peer that
// has never once answered is counted reachable forever. And even a pong
// is not proof: disco probes raw UDP and the relay's WSS control session,
// "neither of which is the WireGuard data plane an inference request
// rides" (ibid.).
//
// On the rc8 hosts that produced exactly the failure it should have
// caught: macmini's overlay data path was dead in both directions, and
// all three machines' doctors reported `✓ mesh peers — 2/3 reachable, 2
// ready` (waired-ai/waired#1137). Owner ruling 2026-08-12: measure it.
//
// The measurement is `POST /waired/v1/ping`, which makes the daemon issue
// a real HTTP GET to the peer's overlay address (internal/inference/
// client.go:62) — the data plane, not the control plane.

// meshProbeBudget bounds the whole probe, however many peers there are.
// The pings run concurrently, so this is a wall-clock ceiling rather than
// a per-peer one: the cost of measuring is one slow peer, not the sum of
// them. It has to fit inside runDoctorBody's 30 s context alongside the
// HTTP probes that follow.
const meshProbeBudget = 8 * time.Second

// meshPeerProbe is one peer's measured result.
type meshPeerProbe struct {
	// Name is what the operator is shown. DeviceName is safe to print
	// for a Public Share peer: the control plane substitutes the grant
	// pseudonym there at injection time (see scrubPeersForDisplay), so
	// unlike DeviceID it never carries a stranger's real identifier.
	Name string
	// Answered is true when the peer returned a ping over the overlay.
	Answered bool
}

// probeMeshPeers pings every peer the published snapshot calls reachable
// and reports what actually answered.
//
// Only the reported-reachable peers are probed. A peer the snapshot
// already calls stale or silent is not a surprise, and spending the
// budget re-confirming it would crowd out the peers whose claim is worth
// testing.
//
// Returns nil when there is nothing to say — no daemon, no mesh route, or
// no peers — so the caller falls back to the published counts rather than
// reporting zero measured peers on a host that simply could not ask.
func probeMeshPeers(ctx context.Context, mgmtURL string) []meshPeerProbe {
	snap, err := fetchMeshSnapshotCtx(ctx, mgmtURL)
	if err != nil || snap == nil {
		return nil
	}
	var targets []inferencemesh.PeerView
	for _, p := range snap.Peers {
		if !p.Stale && !p.Silent {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, meshProbeBudget)
	defer cancel()

	out := make([]meshPeerProbe, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Add(1)
		go func(i int, p inferencemesh.PeerView) {
			defer wg.Done()
			out[i] = meshPeerProbe{
				Name:     p.DeviceName,
				Answered: pingPeerOverOverlay(ctx, mgmtURL, p.DeviceName),
			}
		}(i, p)
	}
	wg.Wait()
	return out
}

// pingPeerOverOverlay asks the daemon to ping one peer and reports
// whether it answered. Every failure — transport, 502 from the handler,
// an unparseable body — is "did not answer": this is a reachability
// measurement, and the reasons a peer is unreachable are not what the
// line is counting.
//
// The route is mgmtWriteRoute rather than a client of its own: POST
// /waired/v1/ping is the one mutating verb that stays on the loopback TCP
// port, and mgmtWriteRoute exempts it by exactly the rule the daemon's own
// writeGuard uses (internal/management/socket.go). Deciding the transport
// here independently is how the reads in this file came to bypass the
// guard and 403 (#785).
var pingPeerOverOverlay = func(ctx context.Context, mgmt, peer string) bool {
	body, err := json.Marshal(map[string]string{"peer": peer})
	if err != nil {
		return false
	}
	// meshProbeBudget, not an invented number: it is the cap the caller's
	// ctx already carries. http.DefaultClient, which this used before, has
	// no Timeout at all.
	target, client, _, err := mgmtWriteRoute(mgmtURL(mgmt, mgmtPingPath), meshProbeBudget)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var res struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return false
	}
	return res.OK
}

// silentPeers names the probed peers that did not answer, sorted so the
// line is stable between runs.
func silentPeers(probes []meshPeerProbe) []string {
	var names []string
	for _, p := range probes {
		if !p.Answered {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return names
}
