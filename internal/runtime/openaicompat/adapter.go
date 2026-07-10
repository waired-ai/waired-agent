// Package openaicompat is the runtime.Adapter for an external OpenAI-
// compatible HTTP endpoint that the agent does NOT own (no subprocess
// lifecycle). The typical case: an operator points the agent at a
// vLLM / LM Studio / TGI server running elsewhere on the LAN
// (http://192.168.1.10:8000/v1) so the local gateway can fall back
// to it when no ollama / vllm is running on this host.
//
// Phase 5 design constraints (see plan):
//
//   - agent-local only — the adapter never participates in the
//     inferencemesh peer-routing path. signer.InferenceState advertised
//     by this device still reflects the local subprocess engine (or
//     "none"), so other peers never see the external endpoint.
//   - bearer auth secret is read from an env-var at NewAdapter time
//     and lives only on this agent. Outbound requests carry it via the
//     Transporter optional interface so the gateway HandlerSet's HTTP
//     client transparently authenticates.
//   - probe loop runs independently of EnsureRunning's caller — the
//     gateway can query Health() any time, and ListModels() returns
//     the last successful /v1/models snapshot for the Selector to
//     match against manifest model_aliases.
package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/runtime"
)

// Config wires an Adapter. Only URL is required; everything else
// has a defaulting branch in NewAdapter.
type Config struct {
	// ID is the operator-supplied registry suffix. The adapter's
	// Name() returns "openai-compat:<ID>", which lands in
	// router.Selection.Runtime when the Selector picks this
	// endpoint. Empty ID is filled with a sanitized host:port form
	// derived from URL.
	ID string

	// URL is the OpenAI-compat base. Both "http://host:8000" and
	// "http://host:8000/v1" are accepted; NewAdapter normalises by
	// stripping a trailing /v1 so the adapter's BaseURL matches the
	// OllamaAdapter convention (path-less; gateway appends
	// /v1/chat/completions).
	URL string

	// AuthEnvVar names the environment variable holding the Bearer
	// token to inject on outbound requests. Empty disables auth.
	// The value is captured at NewAdapter time, so changing the env
	// later requires an agent restart.
	AuthEnvVar string

	// HTTPClient is injected by tests. Defaults to a 5s-timeout
	// client. ProbeTimeout is enforced at the request level so the
	// client timeout is just the upper bound.
	HTTPClient *http.Client

	// ProbeInterval is the GET /v1/models cadence. Default 10s.
	ProbeInterval time.Duration

	// ProbeTimeout is the per-tick request timeout. Default 3s.
	ProbeTimeout time.Duration

	// ReadySuccess is the consecutive 2xx count required to flip
	// State=Ready. Default 1 (external endpoint is presumed live;
	// there's no subprocess flap-window to wait out).
	ReadySuccess int

	// FailedFails is the consecutive failure count that flips
	// State=Failed (or, while Starting, declares EnsureRunning a
	// failure). Default 3.
	FailedFails int
}

// Adapter implements runtime.Adapter for an external OpenAI-compat
// endpoint. It additionally implements runtime.Transporter so the
// gateway's per-request HTTP client picks up the bearer-injecting
// round-tripper without callsite changes.
type Adapter struct {
	cfg     Config
	baseURL string
	bearer  string

	mu     sync.Mutex
	state  runtime.Health
	models []string
	cancel context.CancelFunc
	done   chan struct{}
}

// NewAdapter constructs an Adapter without starting the probe loop;
// the loop spins up the first time EnsureRunning is called.
func NewAdapter(cfg Config) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("openaicompat: URL required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: parse URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("openaicompat: URL %q missing scheme or host", cfg.URL)
	}
	baseURL := normalizeBaseURL(cfg.URL)

	if cfg.ID == "" {
		cfg.ID = sanitizeID(u.Host)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = 10 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 3 * time.Second
	}
	if cfg.ReadySuccess <= 0 {
		cfg.ReadySuccess = 1
	}
	if cfg.FailedFails <= 0 {
		cfg.FailedFails = 3
	}

	bearer := ""
	if cfg.AuthEnvVar != "" {
		bearer = os.Getenv(cfg.AuthEnvVar)
	}

	return &Adapter{
		cfg:     cfg,
		baseURL: baseURL,
		bearer:  bearer,
		state:   runtime.Health{State: runtime.StateNotStarted},
	}, nil
}

// Name returns "openai-compat:<ID>".
func (a *Adapter) Name() string { return "openai-compat:" + a.cfg.ID }

// BaseURL returns the normalised endpoint without a trailing /v1.
func (a *Adapter) BaseURL() string { return a.baseURL }

// Health returns the most recent probe verdict.
func (a *Adapter) Health(_ context.Context) runtime.Health {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// ListModels returns a copy of the last successful /v1/models id list.
// Empty when the adapter has never probed successfully.
func (a *Adapter) ListModels() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.models))
	copy(out, a.models)
	return out
}

// EnsureRunning starts the background probe loop (if not running)
// and blocks until either the loop reports Ready or the configured
// FailedFails threshold is crossed.
func (a *Adapter) EnsureRunning(parent context.Context) error {
	a.mu.Lock()
	if a.state.State == runtime.StateReady {
		a.mu.Unlock()
		return nil
	}
	if a.cancel != nil {
		a.mu.Unlock()
		return errors.New("openaicompat: EnsureRunning called while already starting")
	}
	a.state = runtime.Health{State: runtime.StateStarting}
	probeCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.done = make(chan struct{})
	a.mu.Unlock()

	go a.probeLoop(probeCtx)

	return a.waitReady(parent)
}

// Stop cancels the probe loop and pins State=Stopped.
func (a *Adapter) Stop(_ context.Context) error {
	a.mu.Lock()
	cancel := a.cancel
	done := a.done
	a.cancel = nil
	a.state = runtime.Health{State: runtime.StateStopped}
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

// Transport satisfies the optional runtime.Transporter interface.
// The returned RoundTripper injects "Authorization: Bearer <token>"
// when the configured env-var was non-empty at NewAdapter time;
// otherwise it forwards to http.DefaultTransport untouched.
func (a *Adapter) Transport() http.RoundTripper {
	return &bearerRT{token: a.bearer}
}

// probeLoop runs until ctx is cancelled. State transitions:
//
//	NotStarted/Starting + ReadySuccess consecutive OKs → Ready
//	Ready             + FailedFails  consecutive fails → Failed
//	Starting          + FailedFails  consecutive fails → Failed
//
// On every successful probe, the cached Models list is replaced
// with the new /v1/models snapshot (so adds and removes propagate
// without an agent restart). On the first failure after Ready the
// models cache is NOT cleared — transient blips would otherwise
// flip the Selector's eligibility decision every 10s.
func (a *Adapter) probeLoop(ctx context.Context) {
	defer func() {
		a.mu.Lock()
		done := a.done
		a.done = nil
		a.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	consecOK, consecFail := 0, 0
	tick := func() {
		models, err := probeModels(ctx, a.cfg.HTTPClient, a.baseURL, a.cfg.ProbeTimeout, a.bearer)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.state.State == runtime.StateStopped {
			return
		}
		now := time.Now()
		if err == nil {
			consecOK++
			consecFail = 0
			a.models = models
			if a.state.State != runtime.StateReady && consecOK >= a.cfg.ReadySuccess {
				a.state = runtime.Health{State: runtime.StateReady, LastOK: now}
			} else if a.state.State == runtime.StateReady {
				a.state.LastOK = now
				a.state.LastErr = ""
			}
		} else {
			consecFail++
			consecOK = 0
			if consecFail >= a.cfg.FailedFails {
				switch a.state.State {
				case runtime.StateReady, runtime.StateStarting:
					a.state = runtime.Health{State: runtime.StateFailed, LastErr: err.Error()}
					a.models = nil
				}
			} else if a.state.State == runtime.StateReady {
				a.state.LastErr = err.Error()
			}
		}
	}

	tick()
	t := time.NewTicker(a.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// waitReady polls Health() until State leaves Starting or ctx is done.
// Polling interval is short enough that the test suite doesn't sleep
// noticeably while still being cheap in production.
func (a *Adapter) waitReady(parent context.Context) error {
	check := func() (done bool, err error) {
		a.mu.Lock()
		st := a.state
		a.mu.Unlock()
		switch st.State {
		case runtime.StateReady:
			return true, nil
		case runtime.StateFailed:
			return true, fmt.Errorf("openaicompat: probe failed: %s", st.LastErr)
		default:
			return false, nil
		}
	}
	if done, err := check(); done {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case <-ticker.C:
			if done, err := check(); done {
				return err
			}
		}
	}
}

// normalizeBaseURL strips a trailing slash and a trailing "/v1" so
// the adapter's BaseURL matches the OllamaAdapter convention.
func normalizeBaseURL(s string) string {
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	return strings.TrimRight(s, "/")
}

// sanitizeID converts a host[:port] into a registry-safe ID
// (lowercase alphanumerics + underscore). Used when Config.ID is
// blank.
func sanitizeID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "endpoint"
	}
	return out
}
