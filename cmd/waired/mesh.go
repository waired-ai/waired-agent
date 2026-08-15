package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
)

// defaultMgmtAddr is the local management API's default listener, re-exposed
// as the default for the CLI commands that read the mesh snapshot
// (`waired peers`, `waired worker`). Aliased rather than literal-coded so a
// future change to management.DefaultListen stays consistent.
var defaultMgmtAddr = management.DefaultListen

// meshSnapshotPath is the management route that serves the mesh snapshot.
// There is no /waired/v1/peers route and never has been: `waired peers list`
// renders this snapshot (#785).
const meshSnapshotPath = "/waired/v1/inference/mesh"

// fetchMeshSnapshotCtx calls the local management API's inference-mesh
// endpoint. Empty mgmt falls back to the default listener; the address may
// be given bare ("127.0.0.1:9476") or as a URL, since mgmtURL normalises
// both — and mgmtReadRoute cannot take the bare form, because url.Parse
// rejects a leading "host:port" outright ("first path segment in URL cannot
// contain colon").
//
// The read goes through mgmtReadRoute, which sends it over the local IPC
// socket with a loopback-TCP fallback. That is not a preference: this route
// is not in the daemon's tcpReadRoutes allow-list, so while the socket is
// bound the loopback port answers 403 and every caller of this helper failed
// — `waired peers list`, `waired worker set --pin=<name>`, and the doctor's
// mesh probe (#785, waired#836).
//
// A package var so tests can answer without a daemon; the doctor swaps it.
var fetchMeshSnapshotCtx = func(ctx context.Context, mgmt string) (*inferencemesh.Snapshot, error) {
	if mgmt == "" {
		mgmt = defaultMgmtAddr
	}
	target, client, err := mgmtReadRoute(mgmtURL(mgmt, meshSnapshotPath), 0)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Provider not attached — agent is up but doesn't expose mesh
		// (e.g. older binary). Surface as "no data" rather than a hard
		// error so callers still complete.
		return nil, errors.New("mgmt API returned 404; agent does not expose " + meshSnapshotPath + " (Phase 3+ feature)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mgmt API status %d", resp.StatusCode)
	}
	var out inferencemesh.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// fetchMeshSnapshot is fetchMeshSnapshotCtx with a caller-supplied budget
// instead of a context. Used by `waired peers` and `waired worker`, which
// have no deadline of their own to pass down.
func fetchMeshSnapshot(mgmtAddr string, timeout time.Duration) (*inferencemesh.Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return fetchMeshSnapshotCtx(ctx, mgmtAddr)
}
