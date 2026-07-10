// Package runtime owns engine adapters: code that knows how to start,
// stop, health-check, and HTTP-proxy a backend LLM runtime
// (Ollama in Phase A; vLLM in Phase B). Higher-level code (router,
// gateway, management) treats every engine through the Adapter
// interface so it doesn't grow an `if runtime == "ollama"` cascade.
package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Engine state machine values per spec waired_inference_spec.md §8.1.
// `starting` and `ready` are the only states that may transition to
// `serving`; the agent decides "serving" externally based on whether
// requests are in flight, so adapters do not own that label.
const (
	StateNotStarted = "not_started"
	StateStarting   = "starting"
	StateReady      = "ready"
	StateFailed     = "failed"
	StateStopped    = "stopped"
)

// Health is one snapshot of an adapter's state.
type Health struct {
	State   string    `json:"state"`
	LastOK  time.Time `json:"last_ok,omitempty"`
	LastErr string    `json:"last_err,omitempty"`
}

// Adapter is the uniform interface every backend runtime exposes.
// EnsureRunning is idempotent (a no-op when the engine is already
// healthy); BaseURL is stable across the engine's lifetime so that
// callers can build reverse proxies once.
type Adapter interface {
	// Name returns "ollama" / "vllm" / etc.
	Name() string

	// EnsureRunning starts the backend if needed and blocks until
	// either Health == StateReady or the engine is StateFailed.
	EnsureRunning(ctx context.Context) error

	// Health is non-blocking; it returns the most recent state.
	Health(ctx context.Context) Health

	// Stop shuts down the backend gracefully (SIGTERM, then SIGKILL).
	// Calling Stop on a never-started adapter is a no-op.
	Stop(ctx context.Context) error

	// BaseURL is "http://host:port" of the engine's HTTP API. Empty
	// before the engine has been started.
	BaseURL() string
}

// Transporter is an OPTIONAL interface adapters may implement when
// they need a custom http.RoundTripper to reach BaseURL — peer
// adapters dialing over the WireGuard overlay being the prime
// example. Adapters that don't implement Transporter (Ollama, vLLM)
// are reached over the gateway's default HTTPClient. Returning nil
// from Transport is equivalent to not implementing the interface.
//
// Splitting this out as an optional interface — rather than promoting
// it to the base Adapter contract — keeps the existing local
// adapters unchanged and lets the gateway handler do a typed runtime
// assertion when it constructs the per-request HTTP client.
type Transporter interface {
	Transport() http.RoundTripper
}

// ModelLister is an OPTIONAL interface adapters implement when their
// model inventory is discovered at runtime (via the engine's API)
// rather than driven by the agent's catalog/state. External
// OpenAI-compatible endpoints are the prime example — the agent
// never pulled their weights, so catalog.State has no record of
// them; the adapter's own probe loop is the source of truth.
//
// Adapters that do not implement this (the Ollama and vLLM
// subprocess adapters) signal "ask catalog.State for my model
// list" by failing a type assertion. The router consults this
// interface only when scanning registered adapters for external
// fallback eligibility.
type ModelLister interface {
	ListModels() []string
}

// Spawner is the seam where unit tests inject a fake subprocess.
type Spawner interface {
	// Spawn starts binary with args and env. When logW is non-nil the
	// child's stdout and stderr are written to it (merged) so an engine
	// that fails to come up leaves a diagnosable trail; nil discards them
	// (the historical behaviour). The returned RunningProcess MUST close
	// its Done channel exactly once when the process exits.
	Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error)
}

// RunningProcess abstracts an started OS process.
type RunningProcess interface {
	PID() int
	Done() <-chan struct{}
	Err() error
	Signal(sig os.Signal) error
	Kill() error
}

// ParseBaseURL is a small helper for adapters that need to validate
// or rewrite their own BaseURL during proxying.
func ParseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("runtime: empty base URL")
	}
	return url.Parse(raw)
}
