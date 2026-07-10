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

// fetchMeshSnapshot calls the local management API's inference-mesh endpoint.
// Empty mgmtAddr falls back to the default listener. Used by `waired peers`
// and `waired worker`.
func fetchMeshSnapshot(mgmtAddr string, timeout time.Duration) (*inferencemesh.Snapshot, error) {
	if mgmtAddr == "" {
		mgmtAddr = defaultMgmtAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+mgmtAddr+"/waired/v1/inference/mesh", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Provider not attached — agent is up but doesn't expose mesh
		// (e.g. older binary). Surface as "no data" rather than a hard
		// error so callers still complete.
		return nil, errors.New("mgmt API returned 404; agent does not expose /waired/v1/inference/mesh (Phase 3+ feature)")
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
