package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// A demand carries the context-window floor of the request that made it, and
// the acquirer asks the control plane for a provider that holds it — sending
// the floor only for a 1M demand — and lets go of a held grant below it.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396 (every Waired row is a 200k or 1M session) and the design
// recorded on waired-agent#1399.

func (f *fakeGrantAPI) acquireRequests() []controlclient.AcquirePublicGrantsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]controlclient.AcquirePublicGrantsRequest(nil), f.acquireReqs...)
}

// setWindowGrantPeers puts public provider grants on the map with the windows
// their providers declare.
func (f *fakeMesh) setWindowGrantPeers(windows map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = nil
	for id, w := range windows {
		f.peers = append(f.peers, inferencemesh.PeerView{
			DeviceID:       "dev_" + id,
			Grant:          &signer.PeerGrant{ID: id, Kind: "public", Role: "provider", Pseudonym: "pub-node-x"},
			InferenceState: &signer.InferenceState{ContextWindow: w},
		})
	}
}

// windowLoop starts the acquirer with a demand signal and returns it.
func windowLoop(t *testing.T, api *fakeGrantAPI, mesh *fakeMesh) *publicGrantDemandSignal {
	t.Helper()
	path := writePublicUse(t, t.TempDir(), "auto", 1)
	signal := newPublicGrantDemandSignal()
	deps := grantLoopDeps(api, mesh, path)
	deps.Tick = time.Hour
	deps.Demand = signal.C()
	deps.DemandWindow = signal.Take
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go runPublicGrantLoop(ctx, deps)
	return signal
}

func TestPublicGrantDemandSignal_KeepsTheLargestWindowUntilTaken(t *testing.T) {
	s := newPublicGrantDemandSignal()
	s.Notify(200704)
	s.Notify(1048576)
	s.Notify(0)
	s.Notify(200704)
	if got := s.Take(); got != 1048576 {
		t.Errorf("Take = %d, want 1048576 — a 1M grant serves both rows", got)
	}
	if got := s.Take(); got != 0 {
		t.Errorf("second Take = %d, want 0", got)
	}
	select {
	case <-s.C():
	default:
		t.Error("Notify did not wake the channel")
	}
}

func TestPublicGrantAcquire_SendsTheWindowOnlyForA1MDemand(t *testing.T) {
	for _, tc := range []struct {
		demand, want int
	}{
		{1048576, 1048576},
		// A 200k demand sends nothing: the control plane holds an absent
		// floor to 200704, and one that predates the field refuses it.
		{200704, 0},
		{0, 0},
	} {
		t.Run(fmt.Sprint(tc.demand), func(t *testing.T) {
			api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
				Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
			}}
			signal := windowLoop(t, api, &fakeMesh{})
			signal.Notify(tc.demand)
			waitUntil(t, "an acquire", func() bool { return len(api.acquireRequests()) > 0 })
			if got := api.acquireRequests()[0].MinContextWindow; got != tc.want {
				t.Errorf("min_context_window = %d, want %d", got, tc.want)
			}
		})
	}
}

// A control plane that predates the field answers 400; the acquirer asks once
// more without it.
func TestPublicGrantAcquire_RetriesWithoutTheWindowOnBadRequest(t *testing.T) {
	api := &fakeGrantAPI{}
	api.acquireFn = func(req controlclient.AcquirePublicGrantsRequest) (controlclient.AcquirePublicGrantsResponse, error) {
		if req.MinContextWindow > 0 {
			return controlclient.AcquirePublicGrantsResponse{}, fmt.Errorf("%w: unknown field", controlclient.ErrPublicShareBadRequest)
		}
		return controlclient.AcquirePublicGrantsResponse{
			Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p0000001"}},
		}, nil
	}
	signal := windowLoop(t, api, &fakeMesh{})
	signal.Notify(1048576)
	waitUntil(t, "two acquire attempts", func() bool { return len(api.acquireRequests()) >= 2 })
	reqs := api.acquireRequests()
	if reqs[0].MinContextWindow != 1048576 || reqs[1].MinContextWindow != 0 {
		t.Fatalf("requests = %+v, want 1048576 then 0", reqs)
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(api.acquireRequests()); n != 2 {
		t.Errorf("acquire attempts = %d, want exactly one retry", n)
	}
}

// Before waired-agent#1399 a held grant stopped every acquire, so a provider
// the 1M row refused was kept, and renewed, for as long as the row was used.
func TestPublicGrantDemand_ReleasesAHeldGrantBelowTheWindow(t *testing.T) {
	api := &fakeGrantAPI{}
	api.acquireFn = func(req controlclient.AcquirePublicGrantsRequest) (controlclient.AcquirePublicGrantsResponse, error) {
		if req.MinContextWindow >= 1048576 {
			return controlclient.AcquirePublicGrantsResponse{
				Grants: []controlclient.PublicGrant{{GrantID: "grant_1m", ProviderDeviceID: "dev_p1m"}},
			}, nil
		}
		return controlclient.AcquirePublicGrantsResponse{
			Grants: []controlclient.PublicGrant{{GrantID: "grant_200k", ProviderDeviceID: "dev_p200k"}},
		}, nil
	}
	mesh := &fakeMesh{}
	path := writePublicUse(t, t.TempDir(), "auto", 1)
	signal := newPublicGrantDemandSignal()
	deps := grantLoopDeps(api, mesh, path)
	deps.Tick = time.Hour
	deps.Demand = signal.C()
	deps.DemandWindow = signal.Take
	base := time.Now()
	var offset atomic.Int64
	deps.Now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	// A 200k demand takes the 200k provider.
	signal.Notify(200704)
	waitUntil(t, "the 200k acquire", func() bool { return len(api.acquireRequests()) == 1 })
	mesh.setWindowGrantPeers(map[string]int{"grant_200k": 200704})

	// A 1M demand, past the demand throttle: the 200k grant goes, and the
	// acquire asks for 1M.
	offset.Store(int64(publicGrantDemandMinInterval + time.Second))
	signal.Notify(1048576)
	waitUntil(t, "the 1M acquire", func() bool { return len(api.acquireRequests()) == 2 })
	if got := api.acquireRequests()[1].MinContextWindow; got != 1048576 {
		t.Errorf("second acquire min_context_window = %d, want 1048576", got)
	}
	_, _, releases := api.snapshot()
	if len(releases) != 1 || len(releases[0]) != 1 || releases[0][0] != "grant_200k" {
		t.Errorf("releases = %v, want the 200k grant let go", releases)
	}
}

// A control plane that cannot filter on the window may grant the provider it
// was just asked to replace. The acquirer lets go of it again and waits,
// rather than hold a grant the row refuses or churn on every demand.
func TestPublicGrantDemand_BacksOffWhenTheSameProviderIsGrantedAgain(t *testing.T) {
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Grants: []controlclient.PublicGrant{{GrantID: "grant_a", ProviderDeviceID: "dev_small"}},
	}}
	mesh := &fakeMesh{}
	path := writePublicUse(t, t.TempDir(), "auto", 1)
	signal := newPublicGrantDemandSignal()
	deps := grantLoopDeps(api, mesh, path)
	deps.Tick = time.Hour
	deps.Demand = signal.C()
	deps.DemandWindow = signal.Take
	base := time.Now()
	var offset atomic.Int64
	deps.Now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go runPublicGrantLoop(ctx, deps)

	signal.Notify(200704)
	waitUntil(t, "the first acquire", func() bool { return len(api.acquireRequests()) == 1 })
	mesh.setWindowGrantPeers(map[string]int{"grant_a": 131072})

	offset.Store(int64(publicGrantDemandMinInterval + time.Second))
	signal.Notify(200704)
	waitUntil(t, "the second acquire", func() bool { return len(api.acquireRequests()) == 2 })
	waitUntil(t, "two releases", func() bool { _, _, r := api.snapshot(); return len(r) == 2 })

	// Another demand past the throttle but inside the backoff: no acquire.
	offset.Store(int64(2*publicGrantDemandMinInterval + 2*time.Second))
	signal.Notify(200704)
	time.Sleep(50 * time.Millisecond)
	if n := len(api.acquireRequests()); n != 2 {
		t.Errorf("acquire attempts = %d, want 2 — the regranted provider should start a backoff", n)
	}
}
