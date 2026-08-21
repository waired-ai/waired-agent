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

// MaxResidentModels is how many models an engine may hold in (V)RAM at
// once. One, by owner decision (2026-08-10, waired-agent#644).
//
// What settled it, on a 24 GB host already serving a 21 GB model: the
// install-time speed probe measured 4.4376 s with the cap and 40.9954 s
// without it, minutes apart on the same build. Uncapped, the serving model
// held 97.9% of the GPU and the 3.66 GB probe got 0.86 GB of VRAM; capped,
// the serving model was evicted and the probe ran entirely on the GPU.
// Shrinking the probe instead does not work — measured at three context
// sizes, it spilled 74-81% every time while the serving model owned the
// GPU. Capping residency was the only lever that moved the number.
//
// Delivered per engine, because the engines differ in kind rather than in
// setting:
//   - ollama serves many models from one process, so it is an env var
//     (OllamaAdapter.processEnv).
//   - vLLM already satisfies this: the api_server is launched with a single
//     --model and holds exactly one per process (VLLMAdapter.commandArgs),
//     so there is nothing to set.
const MaxResidentModels = 1

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

// Spawner is the seam where unit tests inject a fake subprocess.
type Spawner interface {
	// Spawn starts binary with args and env. When logW is non-nil the
	// child's stdout and stderr are written to it (merged) so an engine
	// that fails to come up leaves a diagnosable trail; nil discards them
	// (the historical behaviour). The returned RunningProcess MUST close
	// its Done channel exactly once when the process exits.
	Spawn(ctx context.Context, binary string, args, env []string, logW io.Writer) (RunningProcess, error)
}

// ErrSignalUnsupported is returned by RunningProcess.Signal on platforms
// that cannot deliver a signal to an arbitrary process — Windows, which
// has no SIGTERM equivalent. Callers MUST escalate to Kill immediately
// instead of waiting out a grace period the child was never asked to
// honour: the pre-#316 spawner returned nil here, so every Windows stop
// burned the full StopTimeout before escalating and lost the race with
// the caller's own (shorter) budget.
var ErrSignalUnsupported = errors.New("runtime: process signals are not supported on this platform")

// ErrEngineParked is returned by EnsureRunning when the engine has been
// administratively stopped (the hard engine power axis, #186). The gateway
// maps it to a 503 so request traffic does NOT resurrect an engine the
// operator explicitly stopped to free memory.
//
// Lives here rather than beside one adapter, and is returned bare rather
// than wrapped with an engine name, because the state is the operator's and
// is identical on both engines — and because the text reaches a person, as
// the message of that 503 (internal/gateway/openai.go, anthropic.go). User
// prose says "the AI engine", never `ollama` / `vllm`
// (docs-site/TRANSLATION.md, owner ruling 20260819, waired-agent#836/#850).
var ErrEngineParked = errors.New("the AI engine on this computer is stopped")

// ErrEngineUnrecoverable is returned by EnsureRunning once automatic
// recovery has given up. The reason is in Health().LastErr, which the
// mgmt API, `waired status`, and the tray already surface.
//
// Each adapter wraps this with its own name, so the rendering is unchanged
// from when the sentinel lived in ollama.go and carried the prefix itself.
var ErrEngineUnrecoverable = errors.New("engine repeatedly crashed; not retrying (see last_error)")

// RunningProcess abstracts an started OS process.
type RunningProcess interface {
	PID() int
	Done() <-chan struct{}
	Err() error
	// Signal requests a graceful stop. It returns ErrSignalUnsupported
	// when the platform cannot deliver signals at all — that is not a
	// transient failure and the caller must go straight to Kill. Any
	// other error means the signal was not delivered to THIS process
	// (typically because it already exited) and is safe to ignore.
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
