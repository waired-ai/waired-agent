package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// Client wraps the loopback Local Management API for the tray.
// All requests are short-lived and bound to the caller's context;
// no connection pooling tuning, no retries — the polling loop
// retries by re-calling.
type Client struct {
	base string
	hc   *http.Client
	// wc carries mutating requests over the local IPC socket / named pipe
	// instead of the loopback TCP port (waired#838). Reads stay on hc:
	// they are already covered by the #836 browser guard, and the shared
	// observabilityclient is TCP-bound.
	wc *http.Client
	// writeBase is the authority writes are addressed to. In production it
	// is ipcclient.BaseURL, a dummy host the socket transport ignores;
	// tests point it (with wc) at an httptest server to exercise endpoint
	// semantics without a real socket.
	writeBase string
}

// NewClient builds a Client targeting baseURL (default
// http://127.0.0.1:9476) for reads. Trailing slashes are tolerated.
// Writes go to the local management socket, whose endpoint ipcclient
// resolves on its own (honouring $WAIRED_MGMT_SOCKET), so no state dir
// needs threading here.
func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://" + management.DefaultListen
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		hc: &http.Client{
			Timeout: 3 * time.Second,
		},
		wc:        ipcclient.NewHTTPClient(3 * time.Second),
		writeBase: ipcclient.BaseURL,
	}
}

// ErrPauseUnsupported is returned by Pause/Resume when the daemon
// predates the pause/resume API (HTTP 404). The tray surfaces this
// as a "Connect/Disconnect requires waired-agent ≥ X.Y" message
// rather than a generic error.
var ErrPauseUnsupported = errors.New("daemon does not expose pause/resume; upgrade waired-agent")

// ErrInferenceUnsupported is returned by InferenceStatus / EnableInference /
// DisableInference when the daemon predates the inference toggle API
// (HTTP 404). The tray hides the inference menu group rather than
// surfacing a generic error.
var ErrInferenceUnsupported = errors.New("daemon does not expose inference control; upgrade waired-agent")

// ErrClaudeIntegrationUnsupported is returned by ClaudeIntegration when
// the daemon predates the integration-status API (HTTP 404). The tray
// hides the Claude menu group rather than surfacing a generic error.
var ErrClaudeIntegrationUnsupported = errors.New("daemon does not expose claude integration status; upgrade waired-agent")

// ErrClaudeRoutingUnsupported is returned by ClaudeRouting / SetClaudeRouting
// when the daemon predates the per-class routing API (HTTP 404, #649). The
// tray hides the "Claude Code" routing submenu rather than surfacing an error.
var ErrClaudeRoutingUnsupported = errors.New("daemon does not expose claude routing control; upgrade waired-agent")

// ErrCatalogUnsupported is returned by ModelCatalog / SetPreferredModel
// when the daemon predates the catalog API (HTTP 404). The tray hides
// the model-catalog submenu rather than surfacing a generic error.
var ErrCatalogUnsupported = errors.New("daemon does not expose model catalog; upgrade waired-agent")

// ErrOpenCodeIntegrationUnsupported is returned by OpenCodeIntegration /
// ReconfigureOpenCode when the daemon predates those endpoints (HTTP
// 404). The tray hides the OpenCode menu group rather than surfacing
// a generic error.
var ErrOpenCodeIntegrationUnsupported = errors.New("daemon does not expose opencode integration status; upgrade waired-agent")

// ErrOpenClawIntegrationUnsupported is the OpenClaw counterpart of
// ErrOpenCodeIntegrationUnsupported (HTTP 404 on an older daemon).
var ErrOpenClawIntegrationUnsupported = errors.New("daemon does not expose openclaw integration status; upgrade waired-agent")

// ErrObservabilityUnsupported is returned by ObservabilityState /
// ObservabilityEvents when the daemon predates Phase 9 (HTTP 404).
// The tray hides the recent-activity submenu and skips the
// degraded-icon override rather than surfacing a generic error.
var ErrObservabilityUnsupported = errors.New("daemon does not expose observability; upgrade waired-agent")

// Status returns the live network state. A connection-refused or
// timeout is wrapped so callers can detect daemon-down.
func (c *Client) Status(ctx context.Context) (*management.Status, error) {
	var s management.Status
	if err := c.getJSON(ctx, "/waired/v1/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Identity returns the enrolled identity. When the daemon does not
// expose the /identity endpoint (older build), returns a synthetic
// not-enrolled view rather than an error so the tray can render.
func (c *Client) Identity(ctx context.Context) (*management.IdentityView, error) {
	var v management.IdentityView
	if err := c.getJSON(ctx, "/waired/v1/identity", &v); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return &management.IdentityView{Enrolled: false}, nil
		}
		return nil, err
	}
	return &v, nil
}

// Pause sends POST /waired/v1/pause. Translates 404 into
// ErrPauseUnsupported so the UI can render a clearer hint.
func (c *Client) Pause(ctx context.Context) error {
	return c.post(ctx, "/waired/v1/pause")
}

// Resume sends POST /waired/v1/resume. 404 → ErrPauseUnsupported.
func (c *Client) Resume(ctx context.Context) error {
	return c.post(ctx, "/waired/v1/resume")
}

// InferenceStatus returns the inference subsystem snapshot. 404 →
// ErrInferenceUnsupported so the tray can hide the inference group on
// older daemons rather than rendering an error.
func (c *Client) InferenceStatus(ctx context.Context) (*management.InferenceStatus, error) {
	var s management.InferenceStatus
	if err := c.getJSON(ctx, "/waired/v1/inference/status", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrInferenceUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// EnableInference sends POST /waired/v1/inference/enable. 404 →
// ErrInferenceUnsupported.
func (c *Client) EnableInference(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/enable", ErrInferenceUnsupported)
}

// ClaudeIntegration returns the wrapper-side reachability + per-IDE/
// per-shell wrapper-path detection. 404 → ErrClaudeIntegrationUnsupported
// so the tray can hide the Claude group on older daemons.
func (c *Client) ClaudeIntegration(ctx context.Context) (*management.ClaudeIntegrationStatus, error) {
	var s management.ClaudeIntegrationStatus
	if err := c.getJSON(ctx, "/waired/v1/integration/claude", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrClaudeIntegrationUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// ClaudeRouting returns the unified per-class routing state (#649): the
// main/sub policy plus the last fallback event and last locally-served model.
// 404 → ErrClaudeRoutingUnsupported so the tray hides the "Claude Code"
// routing submenu on daemons that predate the endpoint.
func (c *Client) ClaudeRouting(ctx context.Context) (*management.ClaudeRoutingState, error) {
	var s management.ClaudeRoutingState
	if err := c.getJSON(ctx, "/waired/v1/integration/claude/route", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrClaudeRoutingUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// SetClaudeRouting POSTs a per-class routing change. A nil Main/Sub leaves
// that class unchanged; the 200 body is the resulting state. 404 →
// ErrClaudeRoutingUnsupported.
func (c *Client) SetClaudeRouting(ctx context.Context, req management.ClaudeRoutingRequest) (*management.ClaudeRoutingState, error) {
	var s management.ClaudeRoutingState
	if err := c.postJSON(ctx, "/waired/v1/integration/claude/route", req, &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrClaudeRoutingUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// DisableInference sends POST /waired/v1/inference/disable. 404 →
// ErrInferenceUnsupported.
func (c *Client) DisableInference(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/disable", ErrInferenceUnsupported)
}

// ErrEngineControlUnsupported is returned by StopEngine / StartEngine when
// the daemon predates the hard engine power axis (#186, HTTP 404). The
// tray hides the Stop/Start engine item rather than surfacing a generic
// error.
var ErrEngineControlUnsupported = errors.New("daemon does not expose engine power control; upgrade waired-agent")

// StopEngine sends POST /waired/v1/inference/engine/stop — hard-stops the
// local engine to free memory (#186). 404 → ErrEngineControlUnsupported.
// 409 (reuse mode) surfaces as an httpError the caller can recognise.
func (c *Client) StopEngine(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/engine/stop", ErrEngineControlUnsupported)
}

// StartEngine sends POST /waired/v1/inference/engine/start. 404 →
// ErrEngineControlUnsupported.
func (c *Client) StartEngine(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/engine/start", ErrEngineControlUnsupported)
}

// ErrShareUnsupported is returned by EnableShare / DisableShare when
// the daemon predates the Phase 6 share-toggle endpoints (HTTP 404).
// The tray hides the "Share engine to mesh" item rather than surfacing
// a generic error.
var ErrShareUnsupported = errors.New("daemon does not expose inference-share control; upgrade waired-agent")

// EnableShare sends POST /waired/v1/inference/share/enable. 404 →
// ErrShareUnsupported.
func (c *Client) EnableShare(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/share/enable", ErrShareUnsupported)
}

// DisableShare sends POST /waired/v1/inference/share/disable. 404 →
// ErrShareUnsupported.
func (c *Client) DisableShare(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/inference/share/disable", ErrShareUnsupported)
}

// OpenCodeIntegration returns the on-disk drift report for the waired
// OpenCode plugin's provider baseURL plus the path of the plugin file.
// 404 → ErrOpenCodeIntegrationUnsupported so the tray can hide the
// OpenCode group on older daemons rather than rendering an error.
func (c *Client) OpenCodeIntegration(ctx context.Context) (*management.OpenCodeIntegrationStatus, error) {
	var s management.OpenCodeIntegrationStatus
	if err := c.getJSON(ctx, "/waired/v1/integration/opencode", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrOpenCodeIntegrationUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// ReconfigureOpenCode triggers POST /waired/v1/integration/opencode/reconfigure.
// 404 → ErrOpenCodeIntegrationUnsupported. Other non-2xx are surfaced
// verbatim so the tray's user notification can include the daemon's
// reason (e.g. "opencode adapter exploded").
func (c *Client) ReconfigureOpenCode(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/integration/opencode/reconfigure", ErrOpenCodeIntegrationUnsupported)
}

// OpenClawIntegration returns the on-disk drift report for the waired
// OpenClaw plugin's provider baseURL plus the path of the plugin entry.
// 404 → ErrOpenClawIntegrationUnsupported so the tray can hide the OpenClaw
// group on older daemons rather than rendering an error.
func (c *Client) OpenClawIntegration(ctx context.Context) (*management.OpenClawIntegrationStatus, error) {
	var s management.OpenClawIntegrationStatus
	if err := c.getJSON(ctx, "/waired/v1/integration/openclaw", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrOpenClawIntegrationUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// ReconfigureOpenClaw triggers POST /waired/v1/integration/openclaw/reconfigure.
// 404 → ErrOpenClawIntegrationUnsupported; other non-2xx surfaced verbatim.
func (c *Client) ReconfigureOpenClaw(ctx context.Context) error {
	return c.postWithUnsupported(ctx, "/waired/v1/integration/openclaw/reconfigure", ErrOpenClawIntegrationUnsupported)
}

// The bundled coding agent (#486) now runs user-side via the `waired codeui`
// CLI, not the daemon — the tray shells out to it (see onCodeUI), so there is
// no codeui management-API client here anymore.

// ModelCatalog returns the bundled-manifest catalog with per-family
// fit / active / preferred / downloaded annotations. 404 →
// ErrCatalogUnsupported so the tray can hide the catalog submenu on
// older daemons rather than rendering an error.
func (c *Client) ModelCatalog(ctx context.Context) (*management.ModelCatalogResponse, error) {
	var s management.ModelCatalogResponse
	if err := c.getJSON(ctx, "/waired/v1/inference/catalog", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrCatalogUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// SetPreferredModel persists the user's choice and asks the daemon to
// restart so the new model becomes active. 404 → ErrCatalogUnsupported.
func (c *Client) SetPreferredModel(ctx context.Context, modelID string) (*management.PreferredModelResponse, error) {
	var resp management.PreferredModelResponse
	err := c.postJSON(ctx, "/waired/v1/inference/preferred-model",
		management.PreferredModelRequest{ModelID: modelID}, &resp)
	if err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrCatalogUnsupported
		}
		return nil, err
	}
	return &resp, nil
}

// DismissRecommendation records that the operator declined the #133
// lighter-model suggestion (from→to variant IDs) so it is not
// re-surfaced after a re-benchmark of the same pairing. 404 →
// ErrCatalogUnsupported so the tray degrades silently on older daemons.
func (c *Client) DismissRecommendation(ctx context.Context, fromVariantID, toVariantID string) error {
	err := c.postJSON(ctx, "/waired/v1/inference/recommendation/dismiss",
		management.RecommendationDismissRequest{FromVariantID: fromVariantID, ToVariantID: toVariantID}, nil)
	if err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return ErrCatalogUnsupported
		}
		return err
	}
	return nil
}

// ErrWorkerUnsupported is returned by Worker / SetWorker when the
// daemon does not expose /waired/v1/worker. Tray hides the
// "Inference worker" submenu in that case rather than surfacing the
// 404 to the operator — same pattern as Catalog / Observability.
var ErrWorkerUnsupported = errors.New("daemon does not expose /waired/v1/worker (pre-worker-pin agent)")

// Worker fetches the operator's current manual-routing choice. 404 →
// ErrWorkerUnsupported. Used by the standalone `waired worker get`
// CLI path and as a fallback when the tray cannot get the worker
// state from /waired/v1/inference/status (= older daemons).
func (c *Client) Worker(ctx context.Context) (*management.WorkerResponse, error) {
	var resp management.WorkerResponse
	if err := c.getJSON(ctx, "/waired/v1/worker", &resp); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrWorkerUnsupported
		}
		return nil, err
	}
	return &resp, nil
}

// SetWorker POSTs the new routing intent. Validation lives on the
// daemon side; this is a thin transport. 404 → ErrWorkerUnsupported.
func (c *Client) SetWorker(ctx context.Context, req management.WorkerRequest) (*management.WorkerResponse, error) {
	var resp management.WorkerResponse
	err := c.postJSON(ctx, "/waired/v1/worker", req, &resp)
	if err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrWorkerUnsupported
		}
		return nil, err
	}
	return &resp, nil
}

// MeshSnapshot fetches /waired/v1/inference/mesh — used by the tray to
// enumerate inference-capable peers for the worker-pin submenu. 404 →
// nil snapshot + nil error so the tray simply renders an empty pin
// section (matches older daemons gracefully).
func (c *Client) MeshSnapshot(ctx context.Context) (*inferencemesh.Snapshot, error) {
	var s inferencemesh.Snapshot
	if err := c.getJSON(ctx, "/waired/v1/inference/mesh", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// ObservabilityState fetches /waired/v1/observability/state. 404 →
// ErrObservabilityUnsupported so the tray can hide the recent-activity
// submenu and skip the degraded-icon override on older daemons.
// Implementation delegates to the shared observabilityclient package
// so the tray, doctor, claude pre-exec, and `waired status` consumers
// stay byte-identical against the wire.
func (c *Client) ObservabilityState(ctx context.Context) (*management.ObservabilityState, error) {
	st, err := observabilityclient.GetState(ctx, c.base)
	if err != nil {
		if errors.Is(err, observabilityclient.ErrUnsupported) {
			return nil, ErrObservabilityUnsupported
		}
		return nil, err
	}
	return st, nil
}

// ObservabilityEvents fetches /waired/v1/observability/events with the
// supplied cursor / filter parameters. 404 → ErrObservabilityUnsupported.
//
// since == 0  → full ring (subject to limit). Use this on the first
//
//	tray poll after startup; subsequent polls pass the
//	previous response's NextSince so the server returns only
//	deltas (with Gap=true if the ring rolled over since then).
//
// kinds  nil  → all kinds.
// limit  == 0 → no client-side cap.
func (c *Client) ObservabilityEvents(
	ctx context.Context,
	since uint64,
	kinds []observability.Kind,
	limit int,
) (*observabilityclient.EventsResponse, error) {
	resp, err := observabilityclient.GetEvents(ctx, c.base, since, kinds, limit)
	if err != nil {
		if errors.Is(err, observabilityclient.ErrUnsupported) {
			return nil, ErrObservabilityUnsupported
		}
		return nil, err
	}
	return resp, nil
}

// ErrLoginUnsupported is returned by LoginStart / LoginStatus when the
// daemon predates the daemon-driven login API (HTTP 404). The tray
// falls back to the legacy pkexec elevation path in that case rather
// than surfacing a generic error.
var ErrLoginUnsupported = errors.New("daemon does not expose daemon-driven login; upgrade waired-agent")

// LoginStart POSTs /waired/v1/login/start to begin (or rejoin) a
// daemon-driven login session. 404 → ErrLoginUnsupported so the tray
// can fall back to LoginViaElevation on older daemons.
func (c *Client) LoginStart(ctx context.Context, req management.LoginStartRequest) (*management.LoginStatus, error) {
	var resp management.LoginStatus
	if err := c.postJSON(ctx, "/waired/v1/login/start", req, &resp); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrLoginUnsupported
		}
		return nil, err
	}
	return &resp, nil
}

// LoginStatus GETs /waired/v1/login/status?session=<id>. 404 →
// ErrLoginUnsupported.
func (c *Client) LoginStatus(ctx context.Context, sessionID string) (*management.LoginStatus, error) {
	var resp management.LoginStatus
	path := "/waired/v1/login/status?session=" + url.QueryEscape(sessionID)
	if err := c.getJSON(ctx, path, &resp); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrLoginUnsupported
		}
		return nil, err
	}
	return &resp, nil
}

// ErrUpdateUnsupported is returned by UpdateStatus / UpdateCheck when the
// daemon predates the manual-update API (HTTP 404). The tray hides the
// "Update available" banner rather than surfacing a generic error. #293.
var ErrUpdateUnsupported = errors.New("daemon does not expose update check; upgrade waired-agent")

// UpdateStatus GETs /waired/v1/update/status — the daemon's last cached
// check result (cheap; safe to poll). 404 → ErrUpdateUnsupported.
func (c *Client) UpdateStatus(ctx context.Context) (*management.UpdateStatus, error) {
	var s management.UpdateStatus
	if err := c.getJSON(ctx, "/waired/v1/update/status", &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrUpdateUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// UpdateCheck POSTs /waired/v1/update/check to refresh the cached result
// (force bypasses the daemon's cache TTL). 404 → ErrUpdateUnsupported.
func (c *Client) UpdateCheck(ctx context.Context, force bool) (*management.UpdateStatus, error) {
	var s management.UpdateStatus
	if err := c.postJSON(ctx, "/waired/v1/update/check", management.UpdateCheckRequest{Force: force}, &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrUpdateUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// UpdateSettings POSTs /waired/v1/update/settings to toggle the proactive
// update prompt (#294) and returns the refreshed status. 404 →
// ErrUpdateUnsupported so the tray hides the toggle on older daemons.
func (c *Client) UpdateSettings(ctx context.Context, notify bool) (*management.UpdateStatus, error) {
	var s management.UpdateStatus
	if err := c.postJSON(ctx, "/waired/v1/update/settings", management.UpdateSettingsRequest{Notify: notify}, &s); err != nil {
		var hr *httpError
		if errors.As(err, &hr) && hr.StatusCode == http.StatusNotFound {
			return nil, ErrUpdateUnsupported
		}
		return nil, err
	}
	return &s, nil
}

// postJSON sends body as JSON to path and decodes the response into
// out (when non-nil). 2xx is success; 4xx/5xx return an *httpError that
// callers can match for sentinel translation.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	// Writes go over the local IPC socket (waired#838), addressed to a
	// dummy authority the transport ignores.
	u, err := url.Parse(c.writeBase + path)
	if err != nil {
		return err
	}
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.wc.Do(req)
	if err != nil {
		return ipcclient.WrapDialError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &httpError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(errBody)),
		}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// postWithUnsupported is the same as post but returns the supplied
// sentinel on 404, so different endpoints can carry different "this
// daemon doesn't support that" sentinels.
func (c *Client) postWithUnsupported(ctx context.Context, path string, unsupported error) error {
	// Writes go over the local IPC socket (waired#838).
	u, err := url.Parse(c.writeBase + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(""))
	if err != nil {
		return err
	}
	// The browser-hardened management API (waired#836) requires a JSON
	// Content-Type on writes even when the body is empty.
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.wc.Do(req)
	if err != nil {
		return ipcclient.WrapDialError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return unsupported
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &httpError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	u, err := url.Parse(c.base + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &httpError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, path string) error {
	// Writes go over the local IPC socket (waired#838).
	u, err := url.Parse(c.writeBase + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(""))
	if err != nil {
		return err
	}
	// The browser-hardened management API (waired#836) requires a JSON
	// Content-Type on writes even when the body is empty.
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.wc.Do(req)
	if err != nil {
		return ipcclient.WrapDialError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrPauseUnsupported
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &httpError{
			StatusCode: resp.StatusCode,
			Path:       path,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return nil
}

type httpError struct {
	StatusCode int
	Path       string
	Body       string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Path, e.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.StatusCode, e.Body)
}
