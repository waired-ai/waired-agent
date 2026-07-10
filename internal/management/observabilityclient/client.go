// Package observabilityclient is the shared 1-shot HTTP client for
// the agent-local /waired/v1/observability/{state,events} endpoints
// exposed by Phase 9 (PR #67). The tray, `waired doctor`,
// `waired claude` pre-exec warning, and `waired status --observability`
// all read these endpoints; centralizing the GET + decode + 404
// sentinel here keeps four consumers from drifting.
//
// The client is stateless: callers pass a base management URL plus a
// context (which they may have already wrapped in a deadline). No
// retries, no connection pooling tuning — the management API lives on
// loopback and one shot per call is the right shape.
package observabilityclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// ErrUnsupported is returned when the management API responds with
// 404 for an observability route. The caller should treat the
// daemon as predating Phase 9 and degrade gracefully (tray hides the
// menu group, doctor reports StatusSkip, claude pre-exec stays silent).
var ErrUnsupported = errors.New("daemon does not expose observability endpoints")

// EventsResponse mirrors the anonymous struct emitted by
// internal/management.handleObservabilityEvents. It is kept as a
// named export so callers can hold a *EventsResponse without
// re-declaring the shape.
type EventsResponse struct {
	Events    []observability.Event `json:"events"`
	NextSince uint64                `json:"next_since"`
	OldestSeq uint64                `json:"oldest_seq"`
	Gap       bool                  `json:"gap"`
}

// httpClient is the package-shared transport. The 3s timeout is the
// per-call upper bound; callers that want a tighter budget (e.g.
// `waired claude` pre-exec at 200ms) should wrap their context with
// a deadline, which takes effect first.
var httpClient = &http.Client{Timeout: 3 * time.Second}

// GetState fetches /waired/v1/observability/state from mgmtURL. The
// returned pointer is owned by the caller; the underlying response
// body is fully drained and closed before return.
func GetState(ctx context.Context, mgmtURL string) (*management.ObservabilityState, error) {
	endpoint := strings.TrimRight(mgmtURL, "/") + "/waired/v1/observability/state"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("observability/state: HTTP %d", resp.StatusCode)
	}
	var out management.ObservabilityState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("observability/state decode: %w", err)
	}
	return &out, nil
}

// GetEvents fetches /waired/v1/observability/events from mgmtURL,
// passing the supplied cursor / filter parameters as query string.
//
// since == 0  → server returns the full ring (subject to limit).
// kinds  nil  → no kind filter.
// limit  == 0 → no client-side cap (server still caps to ring size).
func GetEvents(
	ctx context.Context,
	mgmtURL string,
	since uint64,
	kinds []observability.Kind,
	limit int,
) (*EventsResponse, error) {
	endpoint := strings.TrimRight(mgmtURL, "/") + "/waired/v1/observability/events"
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if since > 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(kinds) > 0 {
		parts := make([]string, 0, len(kinds))
		for _, k := range kinds {
			parts = append(parts, string(k))
		}
		q.Set("kinds", strings.Join(parts, ","))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("observability/events: HTTP %d", resp.StatusCode)
	}
	var out EventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("observability/events decode: %w", err)
	}
	return &out, nil
}
