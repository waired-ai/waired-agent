package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

type fakeGrantAPI struct {
	mu           sync.Mutex
	acquireCalls int
	renewCalls   [][]string
	releaseCalls [][]string
	acquireRes   controlclient.AcquirePublicGrantsResponse
	acquireErr   error
	renewRes     controlclient.RenewPublicGrantsResponse
}

func (f *fakeGrantAPI) AcquirePublicGrants(_ context.Context, req controlclient.AcquirePublicGrantsRequest) (controlclient.AcquirePublicGrantsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCalls++
	return f.acquireRes, f.acquireErr
}

func (f *fakeGrantAPI) RenewPublicGrants(_ context.Context, ids []string) (controlclient.RenewPublicGrantsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls = append(f.renewCalls, append([]string(nil), ids...))
	return f.renewRes, nil
}

func (f *fakeGrantAPI) ReleasePublicGrants(_ context.Context, ids []string) (controlclient.ReleasePublicGrantsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, append([]string(nil), ids...))
	return controlclient.ReleasePublicGrantsResponse{Released: ids}, nil
}

func (f *fakeGrantAPI) snapshot() (int, [][]string, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquireCalls, append([][]string(nil), f.renewCalls...), append([][]string(nil), f.releaseCalls...)
}

// fakeMesh serves a fixed netmap snapshot.
type fakeMesh struct {
	mu    sync.Mutex
	peers []inferencemesh.PeerView
}

func (f *fakeMesh) Snapshot() inferencemesh.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return inferencemesh.Snapshot{Peers: append([]inferencemesh.PeerView(nil), f.peers...)}
}

func (f *fakeMesh) setGrantPeers(grantIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = nil
	for _, id := range grantIDs {
		f.peers = append(f.peers, inferencemesh.PeerView{
			DeviceID: "dev_" + id,
			Grant:    &signer.PeerGrant{ID: id, Kind: "public", Role: "provider", Pseudonym: "pub-node-x"},
		})
	}
}

func writePublicUse(t *testing.T, dir, mode string, consentVersion int) string {
	t.Helper()
	path := filepath.Join(dir, "public_use.json")
	pu := map[string]any{"mode": mode, "min_quality_tier": 0, "main": true, "sub": true}
	if consentVersion > 0 {
		pu["consent"] = map[string]any{
			"warning_version": consentVersion,
			"accepted_at":     time.Now().UTC().Format(time.RFC3339),
		}
	}
	raw, _ := json.Marshal(pu)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func grantWaitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not reached within %v", timeout)
}

func grantLoopDeps(api *fakeGrantAPI, mesh *fakeMesh, path string) publicGrantDeps {
	return publicGrantDeps{
		API:            api,
		Mesh:           mesh,
		PublicUsePath:  path,
		WarningVersion: 1,
		Logger:         quietLogger(),
		Tick:           5 * time.Millisecond,
	}
}

// TestPublicGrantLoopOffModeMakesNoCalls: unconsented (or off) mode
// must produce zero CP traffic.
func TestPublicGrantLoopOffModeMakesNoCalls(t *testing.T) {
	api := &fakeGrantAPI{}
	path := writePublicUse(t, t.TempDir(), "off", 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, grantLoopDeps(api, &fakeMesh{}, path)); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if calls, _, releases := api.snapshot(); calls != 0 || len(releases) != 0 {
		t.Fatalf("off mode made CP calls: acquire=%d releases=%v", calls, releases)
	}
}

// TestPublicGrantLoopAcquiresAndReleasesOnShutdown: consented auto mode
// acquires the active set; ctx cancellation releases it best-effort.
func TestPublicGrantLoopAcquiresAndReleasesOnShutdown(t *testing.T) {
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Status: "ok",
		Grants: []controlclient.PublicGrant{
			{GrantID: "grant_1", ProviderDeviceID: "dev_p1", ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339), Created: true},
			{GrantID: "grant_2", ProviderDeviceID: "dev_p2", ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339), Created: true},
		},
	}}
	mesh := &fakeMesh{}
	mesh.setGrantPeers("grant_1", "grant_2")
	path := writePublicUse(t, t.TempDir(), "auto", 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, grantLoopDeps(api, mesh, path)); close(done) }()
	grantWaitFor(t, 5*time.Second, func() bool {
		calls, _, _ := api.snapshot()
		return calls >= 1
	})
	cancel()
	<-done
	_, _, releases := api.snapshot()
	if len(releases) != 1 || len(releases[0]) != 2 {
		t.Fatalf("shutdown release = %v, want both grants", releases)
	}
}

// TestPublicGrantLoopModeOffReleases: flipping mode to off mid-run
// releases the held set on the next tick.
func TestPublicGrantLoopModeOffReleases(t *testing.T) {
	api := &fakeGrantAPI{acquireRes: controlclient.AcquirePublicGrantsResponse{
		Status: "ok",
		Grants: []controlclient.PublicGrant{{GrantID: "grant_1", ProviderDeviceID: "dev_p1",
			ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)}},
	}}
	mesh := &fakeMesh{}
	mesh.setGrantPeers("grant_1")
	dir := t.TempDir()
	path := writePublicUse(t, dir, "auto", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, grantLoopDeps(api, mesh, path)); close(done) }()
	grantWaitFor(t, 5*time.Second, func() bool { calls, _, _ := api.snapshot(); return calls >= 1 })

	writePublicUse(t, dir, "off", 1)
	grantWaitFor(t, 5*time.Second, func() bool { _, _, rel := api.snapshot(); return len(rel) >= 1 })
	cancel()
	<-done
	_, _, releases := api.snapshot()
	if len(releases[0]) != 1 || releases[0][0] != "grant_1" {
		t.Fatalf("mode-off release = %v", releases)
	}
}

// TestPublicGrantLoopBackoffOnNotEligible: a 403 suppresses further
// acquire attempts (no hammering inside the backoff window).
func TestPublicGrantLoopBackoffOnNotEligible(t *testing.T) {
	api := &fakeGrantAPI{acquireErr: controlclient.ErrPublicShareNotEligible}
	path := writePublicUse(t, t.TempDir(), "auto", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, grantLoopDeps(api, &fakeMesh{}, path)); close(done) }()
	grantWaitFor(t, 5*time.Second, func() bool { calls, _, _ := api.snapshot(); return calls >= 1 })
	time.Sleep(80 * time.Millisecond) // many ticks worth
	cancel()
	<-done
	if calls, _, _ := api.snapshot(); calls != 1 {
		t.Fatalf("acquire called %d times despite backoff, want 1", calls)
	}
}

// TestPublicGrantLoopDropsMapAbsentAndRenews: a held grant absent from
// the netmap past the grace is dropped without renew; a present one is
// renewed once due, and ids missing from renewed[] are dropped.
func TestPublicGrantLoopDropsMapAbsentAndRenews(t *testing.T) {
	now := time.Now()
	api := &fakeGrantAPI{
		acquireRes: controlclient.AcquirePublicGrantsResponse{
			Status: "ok",
			Grants: []controlclient.PublicGrant{
				// Expiring soon → renew due immediately on the next tick.
				{GrantID: "grant_keep", ProviderDeviceID: "dev_p1", ExpiresAt: now.Add(30 * time.Millisecond).UTC().Format(time.RFC3339Nano)},
				{GrantID: "grant_gone", ProviderDeviceID: "dev_p2", ExpiresAt: now.Add(30 * time.Millisecond).UTC().Format(time.RFC3339Nano)},
			},
		},
		renewRes: controlclient.RenewPublicGrantsResponse{
			Status: "ok", Renewed: []string{"grant_keep"},
			ExpiresAt: now.Add(10 * time.Minute).UTC().Format(time.RFC3339),
		},
	}
	mesh := &fakeMesh{}
	mesh.setGrantPeers("grant_keep", "grant_gone")
	path := writePublicUse(t, t.TempDir(), "auto", 1)

	deps := grantLoopDeps(api, mesh, path)
	// Advancing test clock: acquire happens at base; we then jump past
	// the map-propagation grace before removing the peer, so absence
	// acts on the very next tick.
	var clockMu sync.Mutex
	offset := time.Duration(0)
	deps.Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now.Add(offset)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { runPublicGrantLoop(ctx, deps); close(done) }()

	grantWaitFor(t, 5*time.Second, func() bool { calls, _, _ := api.snapshot(); return calls >= 1 })
	// Jump past the grace, then remove grant_gone from the map: the
	// next tick must drop it before renewing, so only grant_keep ever
	// reaches the renew batch.
	clockMu.Lock()
	offset = publicGrantMapGrace + time.Minute
	clockMu.Unlock()
	mesh.setGrantPeers("grant_keep")
	grantWaitFor(t, 5*time.Second, func() bool { _, renews, _ := api.snapshot(); return len(renews) >= 1 })
	cancel()
	<-done

	_, renews, _ := api.snapshot()
	for _, batch := range renews {
		for _, id := range batch {
			if id == "grant_gone" {
				t.Fatalf("map-absent grant was renewed: %v", renews)
			}
		}
	}
}
