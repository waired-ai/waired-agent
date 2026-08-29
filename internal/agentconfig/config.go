// Package agentconfig holds the agent-side runtime configuration for
// the inference subsystem (and, in the future, other subsystems).
//
// Resolution order, from lowest to highest precedence:
//
//	defaults  →  JSON file  →  process environment  →  CLI flags
//
// The caller is expected to:
//
//  1. Start with Defaults().
//  2. Call MergeJSON to overlay values from ~/.config/waired/agent.json
//     (the file is optional; missing means "use the previous layer").
//  3. Call MergeEnv with os.Environ() to overlay values from
//     WAIRED_INFERENCE_* environment variables.
//  4. Call RegisterInferenceFlags before flag.Parse so that any CLI flag
//     the user actually passed becomes the final value.
//
// The package only handles inference-specific config today; other
// subsystem configs can be added as additional fields on Config without
// breaking the on-disk JSON schema.
package agentconfig

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Duration is a time.Duration that JSON-(de)serialises as a Go duration
// string (e.g. "10m"), not as nanoseconds.
type Duration time.Duration

// NewDuration is the obvious constructor.
func NewDuration(d time.Duration) Duration { return Duration(d) }

// Duration returns the underlying time.Duration value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("agentconfig: invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// InferenceConfig captures every inference-subsystem setting that an
// operator may want to tune without recompiling. Field names map 1:1
// to JSON keys (snake_case) and to CLI flags / env vars (kebab-case
// for flags, UPPER_SNAKE for env, both prefixed with `inference-` /
// `WAIRED_INFERENCE_`).
type InferenceConfig struct {
	// BundledModelID is the manifest model_id auto-pulled at agent
	// startup when PullOnStartup is true.
	//
	// It has NO compiled-in default, deliberately. The value is the
	// output of hardware-aware selection (setup.SelectBundledModel,
	// #517) or an operator's explicit pin — both of which know things a
	// constant cannot. The constant that used to sit here named
	// qwen2.5-coder-7b-instruct, a 32k-window model that
	// router.SelectInstallModel excludes on every host, because the
	// #624 coding-agent context floor rejects the window: a default that
	// was not merely stale but unreachable by the picker that is
	// supposed to produce it. Empty means "not chosen yet"; the pre-pull
	// and the vLLM target both skip rather than guess.
	BundledModelID string `json:"bundled_model_id"`

	// PullOnStartup enables a background `ollama pull` of the bundled
	// model when waired-agent boots.
	//
	// The DOWNLOAD only. Weights already on disk are still committed as
	// the active selection at boot, so a host that turns this off still
	// serves what it has (#526) — which matters because the install-time
	// selector turns it off itself when disk is short
	// (setup.SelectBundledModel: keep the model configured, don't fetch
	// it now), i.e. on the hosts most likely to be reusing weights.
	PullOnStartup bool `json:"pull_on_startup"`

	// IdleTimeout is how long the engine holds the serving model in
	// (V)RAM after its last request, exported as OLLAMA_KEEP_ALIVE and
	// sent as the per-request keep_alive. **Zero or negative means the
	// model is never unloaded on idle**, which is the default.
	//
	// Holding is the default because letting the model expire costs a
	// weights reload AND a full prefill on the next request — measured
	// at 16.9 / 43.4 / 55.8 s on three hosts, of which prefill is
	// 57-77% — and only the weights half can be warmed back
	// (waired-agent#861). An operator who wants the memory returned on
	// idle sets a duration here; one who wants it back now runs
	// `waired inference unload`, which leaves the engine up.
	//
	// This field previously had no consumer at all: it was declared,
	// defaulted to 10m, parsed from the environment and registered as
	// -inference-idle-timeout, while the actual residency was a
	// hardcoded 60m constant that disagreed with it.
	IdleTimeout Duration `json:"idle_timeout"`

	// MaxCacheGB caps total on-disk model cache (spec §9.3). Soft
	// limit enforced by the download manager.
	MaxCacheGB int `json:"max_cache_gb"`

	// AllowPull controls whether this host DOWNLOADS model weights:
	// `waired models pull`, the startup pre-pull, the boot-time re-pull of
	// the preferred model, and the update pre-cache.
	//
	// It does NOT stop the engine. `ollama serve` starts either way, so a
	// host whose weights are already on disk serves them (#338) — the
	// earlier wording ("permitted at all") is what made a gate on the
	// engine-start path look defensible. Enabled is the switch that turns
	// local inference off; `waired inference engine stop` is the one that
	// gives the memory back.
	AllowPull bool `json:"allow_pull"`

	// AllowAnthropicAPI exposes /anthropic/v1/messages on the Local
	// Gateway. Disable to lock the gateway down to OpenAI compat only.
	AllowAnthropicAPI bool `json:"allow_anthropic_api"`

	// AllowOpenAIAPI exposes /v1/chat/completions etc. on the Local
	// Gateway.
	AllowOpenAIAPI bool `json:"allow_openai_api"`

	// LocalGatewayPort is the loopback port for the OpenAI/Anthropic
	// compat API server (spec waired_product_spec.md §3.3, §12.1 ⇒ 9473).
	//
	// It is the only local inference listener. There used to be a second
	// one on 9479 serving the same routes with the same handler set, which
	// existed for one reason: the desktop user could not read the 0600
	// bearer token this one required, so the coding-agent plugins needed a
	// door without a lock. The token is gone, so the second door has
	// nothing left to be (waired-ai/waired#1277) and 9479 was free again —
	// it is now the vLLM engine's port (see VLLMPort below).
	//
	// No bearer token. Loopback plus the Host/Origin allow-list is the
	// trust boundary, the same posture as the management API (9476) and
	// the Claude gateway (9472).
	LocalGatewayPort int `json:"local_gateway_port"`

	// ClaudeGatewayPort is the loopback port for the no-token Claude
	// Anthropic data-plane listener (#488). Claude Code's managed-settings
	// ANTHROPIC_BASE_URL points here. It serves the Anthropic /v1/messages*
	// routes locally (fail-open to the real api.anthropic.com when degraded)
	// and reverse-proxies every other path to the real API — the plain-HTTP
	// successor to the retired :443 MITM proxy. No bearer token: credential-
	// less Claude presents its subscription OAuth token, not waired's gateway
	// token, so loopback is the trust boundary. 0, or AllowAnthropicAPI off,
	// disables the listener.
	ClaudeGatewayPort int `json:"claude_gateway_port"`

	// ClaudeTTFBBudgetMainMs / ClaudeTTFBBudgetSubMs bound the pre-first-byte
	// window (milliseconds) for a MAIN / SUBAGENT Claude request routed to a
	// mesh PEER (#757). If the peer returns no response headers within the
	// budget the leg is aborted BEFORE the response commits, so an auto-routed
	// turn reroutes to the Anthropic API instead of hanging on a
	// stalled-but-reachable peer; a pinned (route=waired) leg is never
	// affected. These are generous infinite-hang BACKSTOPS, not snappy reroute
	// thresholds: /healthz readiness does not imply the model is loaded, so a
	// cold model load legitimately sits inside this window. Subagents get the
	// tighter budget (a stalled subagent is cheap to reroute and reads to the
	// user as a hang). 0 disables the deadline for that class.
	ClaudeTTFBBudgetMainMs int `json:"claude_ttfb_budget_main_ms"`
	ClaudeTTFBBudgetSubMs  int `json:"claude_ttfb_budget_sub_ms"`

	// ClaudePeerWaitCeilingMs turns ClaudeTTFBBudgetMainMs from a deadline
	// into a GRACE PERIOD, and bounds the wait that follows it
	// (waired-agent#1040).
	//
	// It exists because the budget above was measuring the wrong thing.
	// What a caller waits out before a peer's first byte is that peer
	// PREFILLING the prompt — a property of its speed and of how much
	// context the client sent, not of its health. A Claude Code first turn
	// is around 30k tokens, and on the 0.0.3-rc4 review fleet three of four
	// peers needed longer than 60 s for one; peers that were working
	// correctly were abandoned and their turns went to the Anthropic API
	// with the mesh idle.
	//
	// So past the grace period the wait continues for as long as the peer's
	// own /waired/v1/inference/healthz says it is serving, and ends when it
	// says it is not, when it stops answering, or when this ceiling is
	// reached. Owner ruling 2026-08-28 on waired-agent#1040: do not cut off
	// a request that is being worked on merely because it is slow — ask the
	// peer instead.
	//
	// The default is THIRTY minutes (owner ruling 2026-08-28, on the first
	// measurement of it: a 30k-token first turn on the fleet's slowest peer
	// took 9 min 10 s to its first byte — 29,553 tokens at ~54 tok/s — which
	// left 50 seconds of margin against the ten minutes this shipped with,
	// and a 60k-token turn on that machine would want ~18).
	//
	// Ten minutes was the local leg's figure (ClaudeLocalTTFBBudgetMs), and
	// the two are not the same question. That one is a PURE timeout: nothing
	// tells this device what its own engine is doing behind a withheld
	// response header, so it has to be conservative. The peer leg has the
	// peer's own answer, and both ways a wait can legitimately end — the peer
	// saying it stopped, and the peer going quiet — are caught by that,
	// before any ceiling. What is left for the ceiling is the case where the
	// peer's claim is WRONG, which is rare enough not to be worth cutting
	// real turns for.
	//
	// The cost of the larger figure is that such a turn is silent for longer:
	// a leg the intercept may reroute cannot be held open with an SSE
	// keepalive (docs/decisions/20260821/2142), so nothing is written until
	// the first byte either way.
	//
	// 0, or any value not longer than the main
	// budget, leaves the flat deadline in place. The SUBAGENT class is
	// deliberately not covered: its tighter budget exists because a stalled
	// subagent is cheap to reroute, and Claude Code's helper requests carry
	// a client-side deadline of their own (waired-agent#1041).
	ClaudePeerWaitCeilingMs int `json:"claude_peer_wait_ceiling_ms"`

	// ClaudeLocalTTFBBudgetMs is the same pre-first-byte window for a Claude
	// request THIS computer's own engine is serving, on the auto route only
	// (waired-agent#837). Until it existed a local leg had no bound at all:
	// the engine withholds response headers until the weights are resident,
	// so a cold load produced zero bytes until the client gave up — and its
	// retry started the same load again.
	//
	// One value for every class, deliberately not split main/sub: the
	// subagent budget above exists because "a stalled subagent is cheap to
	// reroute", which is true of a peer that has an equivalent elsewhere and
	// false of this computer. It is much larger than the peer budgets for
	// the same reason those are generous — a cold load legitimately lands
	// inside it — and the default is the owner's ruling of 2026-08-21: bound
	// it, but at ten minutes, so only a wait no client would still be
	// waiting on ends the turn. 0 disables it and restores the unbounded
	// wait. A pinned (route=waired) leg is never affected; it is held open
	// with a keepalive instead.
	ClaudeLocalTTFBBudgetMs int `json:"claude_local_ttfb_budget_ms"`

	// OllamaPort is the loopback port of the Ollama engine. Leave at
	// OllamaPortAuto (0) to spawn on DefaultOllamaBundledPort (9475,
	// waired-owned). Read it through ResolvedOllamaPort(), never
	// directly: a literal 11434 is treated as the legacy serialized
	// default and flips to 9475 (every pre-cutover agent.json wrote
	// 11434 explicitly, so it cannot be distinguished from "unset").
	OllamaPort int `json:"ollama_port"`

	// VLLMPort is the loopback port the vLLM subprocess binds to. Used
	// by the gateway proxy when the active runtime is vllm. Leave at
	// VLLMPortAuto (0) to spawn on DefaultVLLMBundledPort (9479,
	// waired-owned). Read it through ResolvedVLLMPort(), never directly:
	// a literal 8000 is treated as the legacy serialized default and
	// flips to 9479, the same rule and for the same reason as
	// OllamaPort's 11434 (waired-agent#1026).
	VLLMPort int `json:"vllm_port"`

	// VLLMGPUMemoryUtilization caps the fraction of VRAM vLLM may
	// reserve at startup (vLLM `--gpu-memory-utilization`). Default
	// 0.85 leaves ~15% headroom on a single-GPU host. Operators with
	// no other GPU consumers may raise this to 0.90+ to widen KV cache.
	// Range (0, 1]; values outside are rejected by Validate.
	VLLMGPUMemoryUtilization float64 `json:"vllm_gpu_memory_utilization"`

	// VLLMTensorParallel overrides vLLM's --tensor-parallel-size.
	// 0 (default) means auto: the largest power of two ≤ the number of
	// identical NVIDIA GPUs (router.VLLMTensorParallelSize). N ≥ 1
	// forces that size, clamped to the detected NVIDIA GPU count at
	// bootstrap; 1 is the "force single GPU" escape hatch and is never
	// auto-upgraded.
	VLLMTensorParallel int `json:"vllm_tensor_parallel"`

	// VLLMDisableFP8KV opts out of fp8 (e4m3) KV cache (#676). Default
	// false: on Ada+ (compute_cap ≥ 8.9: L4, RTX 40xx, Hopper) vLLM runs
	// KV at fp8, halving its footprint to roughly double the fittable
	// context window — near-lossless, the vLLM analogue of Ollama's
	// default q8_0. Set true to force fp16 KV (e.g. if a workload is
	// sensitive to the quantization). No effect on sub-Ada GPUs, which
	// never engage fp8. Selection sizes against the default-on; this
	// opt-out affects serving only.
	VLLMDisableFP8KV bool `json:"vllm_disable_fp8_kv"`

	// VLLMSpeculativeNgram enables ngram (prompt-lookup) speculative
	// decoding (#677): vLLM proposes tokens by matching recent context
	// against earlier n-grams (no draft model), accelerating
	// single-stream decode for coding agents. Default false — it can
	// slow multi-stream serving, so it is opt-in until measured per host.
	VLLMSpeculativeNgram bool `json:"vllm_speculative_ngram"`

	// VLLMToolParser overrides vLLM's --tool-call-parser (#410). Empty
	// (default) lets the agent pick from the served model's chat
	// template; a non-empty value is passed through verbatim.
	//
	// The escape hatch exists for a parser vLLM registered after this
	// binary was built, so it is deliberately NOT validated against a
	// known-name list — which means a typo is not a config error but an
	// engine that refuses to start ("invalid tool call parser"). Use
	// `vllm serve --help` on the installed venv to see the accepted
	// names for the pinned version.
	VLLMToolParser string `json:"vllm_tool_parser"`

	// VLLMMaxNumBatchedTokens overrides vLLM's --max-num-batched-tokens
	// (#887). 0 (default) derives it from the host: 4096, or 8192 on a
	// GPU at or above 70 GiB where that is already upstream's own value.
	//
	// Overridable because it trades prefill speed against the KV pool
	// with no fleet measurement to set it from, and because too large is
	// a start-up abort rather than a slowdown — an operator who hits
	// that needs a way down without waiting for a release.
	VLLMMaxNumBatchedTokens int `json:"vllm_max_num_batched_tokens"`

	// VLLMKVOffloadingGiB enables vLLM's native KV offloading with a
	// buffer of this many GiB of HOST RAM (#887). 0 (default) disables
	// it; the agent clamps a request to a quarter of host RAM.
	//
	// Off by default because it commits whole GiB on a machine that is
	// usually also a workstation, and because it does not do what its
	// name suggests: the native backend is CPU RAM in the engine
	// process, never disk, and it does not survive a restart.
	VLLMKVOffloadingGiB float64 `json:"vllm_kv_offloading_gib"`

	// PreferredEngine forces engine selection at install/refresh time.
	// Empty string ("") means auto-pick (NVIDIA GPU + ≥8 GB VRAM ⇒ vllm,
	// else ollama). Accepted values: "", "ollama", "vllm".
	PreferredEngine string `json:"preferred_engine"`

	// PreferredModelID forces a specific manifest model_id when set.
	// Empty string means auto-pick (highest quality_tier that fits
	// the chosen engine and host VRAM/RAM).
	PreferredModelID string `json:"preferred_model_id"`

	// InteractiveFloorTokps is the minimum boot-benchmark throughput
	// (tokens/sec, true decode per #764) below which the agent
	// recommends a lighter model (issue #133). 0 means "use the
	// built-in default" (router.CodingAgentSelectionFloorTokps = 60,
	// #670/#765) — resolved at the consumer so the constant stays the
	// single source of truth.
	// Lower it on a host whose coding agent tolerates slower output to
	// suppress the nag; the recommendation is advisory only and never
	// auto-switches.
	InteractiveFloorTokps float64 `json:"interactive_floor_tokps"`

	// AllowAutoFallback controls bootstrap behaviour when the persisted
	// active runtime is not viable on the current host (e.g. vllm chosen
	// but GPU disappeared). When true (default) the agent walks the
	// fallback chain (vllm → ollama → no-engine) with a warning log;
	// when false the agent exits non-zero so that an operator notices
	// the host degradation immediately. Useful for GPU-required
	// deployments that must not silently fall back to CPU inference.
	AllowAutoFallback bool `json:"allow_auto_fallback"`

	// PreCacheUpdateCandidate enables a background goroutine that, at
	// startup, computes what the auto-picker WOULD pick on the current
	// host and pre-downloads the candidate's weights so that a
	// subsequent `waired runtimes refresh` becomes a near-instant swap.
	// The persisted active model continues to serve requests while the
	// candidate downloads. Disable if disk/bandwidth are constrained.
	PreCacheUpdateCandidate bool `json:"pre_cache_update_candidate"`

	// Enabled is the install-time choice for whether this node runs a
	// local inference engine at all. Default true preserves Phase 4
	// behaviour: a fresh agent.json (or no file at all) keeps the
	// engine on. When false, the agent boots as if no engine were
	// installed: chooseEngine bails, the probe loop short-circuits,
	// and the peer-overlay listener serves only the ping endpoint.
	//
	// This field is read once at boot. The CLI / tray do NOT expose a
	// runtime toggle because installing / uninstalling a local engine
	// is a lifecycle event, not a soft toggle. To change it, edit
	// agent.json and restart the agent (the future installer will own
	// this flow).
	Enabled bool `json:"enabled"`

	// ShareWithMesh is the install-time choice for whether this agent
	// exposes its local engine to the WireGuard overlay mesh. Default
	// true preserves Phase 4 behaviour: signer.InferenceState is
	// pushed to the Control Plane on every probe tick, and the
	// peer-overlay listener accepts signed peer-engine requests.
	//
	// When false: the agent (a) skips the CP push so peers don't see
	// this engine in their mesh snapshot, and (b) returns a 503 with
	// error="waired_inference_not_shared" from the peer-overlay
	// listener so a peer holding a stale snapshot cannot reach the
	// engine. Local-loopback traffic from the host's own gateway is
	// unaffected (the engine is loopback-only).
	//
	// Unlike Enabled, this field is the bootstrap default for a
	// runtime toggle. The CLI (`waired inference share <on|off>`)
	// and tray persist the operator's choice to
	// <state-dir>/runtime/desired-share, which overrides this default
	// on next boot — the same precedence used by the inference-disable
	// and pause/resume desired-state files.
	ShareWithMesh bool `json:"share_with_mesh"`

	// ClaudeModelRouteDirectives toggles the reserved-model-id route
	// directives for Claude Code (#52), default ON so both switching
	// mechanisms — /waired-route AND the /model picker — work out of the box
	// (set false, via agent.json / env / flag, to opt out). When true the
	// Claude intercept (a) advertises two branded ids in /v1/models discovery so
	// they appear in Claude Code's /model picker — "anthropic-waired-local"
	// (pins the conversation to LOCAL inference) and "claude-waired-cloud[1m]"
	// (pins it to the real Anthropic API) — and (b) forces each request's
	// route from the selected id, OVERRIDING the /waired-route per-class
	// policy, so backend selection becomes a one-action /model switch that
	// runs alongside /waired-route. It also makes `waired claude enable`
	// write CLAUDE_CODE_MAX_CONTEXT_TOKENS so the non-"claude-"-prefixed
	// local id carries an honest ~256k window instead of Claude Code's 200k
	// default. Read at boot by the agent (intercept + gateway) and at
	// enable-time by the CLI (managed-settings write).
	ClaudeModelRouteDirectives bool `json:"claude_model_route_directives"`

	// ClaudeModelPeerEntries caps how many per-computer rows the /model
	// picker carries alongside the fixed directives (waired-agent#830).
	// 0 turns them off and leaves the fixed entries alone.
	//
	// Capped rather than unbounded because the picker folds past about ten
	// rows and six of those are Claude Code's own, measured on device
	// (docs/knowledges/20260820/0300-model-picker-measured-on-device.md).
	// The default leaves room for a small household fleet without pushing
	// the fixed entries out of reach; a larger fleet is better served by the
	// tray's pin submenu, which scrolls properly. Distinct from the tray's
	// own MaxWorkerPinEntries for exactly that reason — the constraint here
	// is the fold, not the menu.
	ClaudeModelPeerEntries int `json:"claude_model_peer_entries"`
}

// RoutingConfig is the install-time default for the inference routing
// policy (Tailscale-exit-node-style manual peer selection). Mode is
// the boot-time fallback when the operator has not touched the runtime
// toggle; the persisted state.RoutingPreference from
// <state-dir>/runtime/desired-worker overrides this at boot (same
// precedence pattern as ShareWithMesh / desired-share).
//
// Lives at top-level (not inside InferenceConfig) because routing is
// outbound — it tells this agent's gateway where to send requests —
// while the inference block describes the local engine surface. The
// two axes were tangled in earlier prototypes and the cleanup was a
// repeated source of test churn.
type RoutingConfig struct {
	// Mode picks between auto / local-only / peer-preferred / pinned.
	// Empty == "auto" (= current pre-feature behaviour).
	Mode state.RoutingMode `json:"mode,omitempty"`

	// PinnedPeerDeviceID is meaningful only when Mode ==
	// state.RoutingModePinned. Ignored otherwise.
	PinnedPeerDeviceID string `json:"pinned_peer_device_id,omitempty"`

	// Prefer is what the mesh ordering optimises for when several
	// computers could answer: "speed" (the default) or "size"
	// (waired-agent#1128). Empty == speed.
	Prefer state.RoutingPrefer `json:"prefer,omitempty"`

	// MinModelSize is the smallest model class this device will route to
	// — "" (no floor, the default), "small", "medium" or "large".
	MinModelSize string `json:"min_model_size,omitempty"`
}

// AsPreference projects the install-time default into the on-disk
// shape used by state.RoutingPreference, normalising the empty Mode
// to RoutingModeAuto so downstream readers see a single canonical
// representation.
func (r RoutingConfig) AsPreference() state.RoutingPreference {
	m := r.Mode
	if m == "" {
		m = state.RoutingModeAuto
	}
	pin := r.PinnedPeerDeviceID
	if m != state.RoutingModePinned {
		// Validate() rejects this, but be defensive on the read path
		// so callers that bypass validation never see a misshapen
		// preference.
		pin = ""
	}
	return state.RoutingPreference{
		Mode:               m,
		PinnedPeerDeviceID: pin,
		Prefer:             r.Prefer,
		MinModelSize:       r.MinModelSize,
	}
}

// Config is the root document persisted to ~/.config/waired/agent.json.
// New top-level subsystems should be added as additional fields, not
// promoted into the top-level struct (so the JSON schema stays stable).
//
// The retired transparent-proxy (MITM) subsystem config was removed in #488;
// Claude Code routing is now configured via the Claude Code managed-settings
// ANTHROPIC_BASE_URL (see internal/integration/claudemanaged) pointing at the
// loopback gateway on Inference.ClaudeGatewayPort.
type Config struct {
	Inference InferenceConfig `json:"inference"`
	Routing   RoutingConfig   `json:"routing,omitempty"`
	Logging   LoggingConfig   `json:"logging,omitempty"`
}

// Log level names accepted in agent.json (logging.level), the
// WAIRED_LOG_LEVEL env var, and the `--log-level` flags. These map 1:1
// onto slog levels via ParseLogLevel.
const (
	LogLevelDebug = "debug"
	LogLevelInfo  = "info"
	LogLevelWarn  = "warn"
	LogLevelError = "error"
)

// LoggingConfig captures logging behavior an operator may tune without
// recompiling. It is read once at boot by both the daemon and the tray;
// the daemon additionally exposes a live toggle over the management API
// (surfaced as `waired config log-level`) that updates the running level
// immediately and persists the choice back here so it survives a restart.
type LoggingConfig struct {
	// Level is the minimum slog level emitted: "debug", "info", "warn",
	// or "error". Empty is treated as "info". Raising it to "debug" is
	// the pre-release debugging switch for both the service and the app.
	Level string `json:"level,omitempty"`
}

// SlogLevel maps the configured Level to a slog.Level. An empty or
// unrecognized value falls back to slog.LevelInfo — Validate rejects
// unrecognized values on the config path, so this is only the defensive
// read-path default.
func (l LoggingConfig) SlogLevel() slog.Level {
	lvl, _ := ParseLogLevel(l.Level)
	return lvl
}

// ParseLogLevel converts a level name to a slog.Level. It accepts
// "debug", "info", "warn", and "error" (case-insensitive, surrounding
// whitespace ignored); "" maps to info. An unrecognized value returns
// slog.LevelInfo together with an error.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", LogLevelInfo:
		return slog.LevelInfo, nil
	case LogLevelDebug:
		return slog.LevelDebug, nil
	case LogLevelWarn:
		return slog.LevelWarn, nil
	case LogLevelError:
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("agentconfig: unknown log level %q (want %s|%s|%s|%s)",
			s, LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError)
	}
}

// NormalizeLogLevel lower-cases and trims a level name, returning the
// canonical form used for storage. It errors on an unrecognized value.
func NormalizeLogLevel(s string) (string, error) {
	if _, err := ParseLogLevel(s); err != nil {
		return "", err
	}
	n := strings.ToLower(strings.TrimSpace(s))
	if n == "" {
		n = LogLevelInfo
	}
	return n, nil
}

// LogLevelName maps a slog.Level back to its config/API name. Range-based
// so an out-of-band level still resolves to the nearest bucket.
func LogLevelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return LogLevelDebug
	case l < slog.LevelWarn:
		return LogLevelInfo
	case l < slog.LevelError:
		return LogLevelWarn
	default:
		return LogLevelError
	}
}

// ResolveLogLevel picks the effective boot log level for a binary from,
// in precedence order (highest first):
//
//  1. flagVal — an explicit --log-level flag, when non-empty
//  2. $WAIRED_LOG_LEVEL
//  3. $WAIRED_DEBUG — legacy switch: any non-empty value → debug
//  4. cfgLevel — a persisted logging.level (agent.json); pass "" for a
//     binary that has no config layer (e.g. the tray)
//  5. info
//
// getenv is injected (os.Getenv in production) so precedence is unit
// testable. Unrecognized flag/env values are ignored (fall through to the
// next source) rather than failing the process at boot.
func ResolveLogLevel(cfgLevel, flagVal string, getenv func(string) string) slog.Level {
	if flagVal != "" {
		if lvl, err := ParseLogLevel(flagVal); err == nil {
			return lvl
		}
	}
	if v := getenv("WAIRED_LOG_LEVEL"); v != "" {
		if lvl, err := ParseLogLevel(v); err == nil {
			return lvl
		}
	}
	if getenv("WAIRED_DEBUG") != "" {
		return slog.LevelDebug
	}
	lvl, _ := ParseLogLevel(cfgLevel)
	return lvl
}

// Defaults returns the Config that ships when no file / env / flag is
// supplied. Update spec §19-6 (and bump Phase A docs) whenever these
// change.
// Ollama port resolution. The engine used to bind the upstream default
// 11434 and silently adopt whatever system ollama already owned it —
// which broke the version pin invisibly (a 0.30.7-pinned node was
// actually served by a system 0.24.0). The waired-managed engine now
// owns 9475, the free slot in waired's loopback family (9473 gateway,
// 9474 overlay, 9476 management, 9477 control plane, 9478 relay), so it
// never contends with a user's ollama.
const (
	OllamaPortAuto           = 0     // resolve to the waired-owned port
	DefaultOllamaBundledPort = 9475  // waired-owned spawn target
	legacyOllamaDefaultPort  = 11434 // upstream default; pre-cutover files wrote it explicitly
)

// ResolvedOllamaPort returns the port the Ollama engine actually uses.
// See the OllamaPort field comment for the legacy-11434 flip rule.
func (c InferenceConfig) ResolvedOllamaPort() int {
	switch c.OllamaPort {
	case OllamaPortAuto, legacyOllamaDefaultPort:
		return DefaultOllamaBundledPort
	default:
		return c.OllamaPort
	}
}

// vLLM port resolution, the same arrangement as ollama's above and for a
// sharper version of the same reason.
//
// vLLM's own default is 8000, and the engine bound it unconditionally. 8000
// is one of the most contended ports on a developer machine — a Docker
// publish range, a Django or FastAPI dev server, another inference tool —
// and a busy one is not survivable for this engine the way it is for ollama:
// there is no adopt path, the API server just fails to bind and exits, and
// the bootstrap's retries all fail the same way. On real hardware a Docker
// container publishing 8000-8019 kept a healthy host with no local AI, and
// nothing on any surface said why (waired-agent#1026).
//
// 9479 is the free slot in waired's loopback family (9472 Claude gateway,
// 9473 local gateway, 9474 overlay, 9475 ollama, 9476 management, 9477
// control plane, 9478 relay). It was the second, token-less gateway listener
// until waired-ai/waired#1277 retired it, so it is both free and already
// ours — see the LocalGatewayPort field comment, which records that.
//
// A fixed waired-owned port rather than a kernel-assigned one, exactly as
// ollama does it: the port is read by out-of-process consumers that hold
// only the config (the benchmark, the depth bench, the mesh probe target,
// the foreign-engine listener check), so a per-spawn ephemeral port would
// have to be published to all of them first — the shape of waired-agent#1024
// — and it would move across restarts for no gain.
const (
	VLLMPortAuto           = 0    // resolve to the waired-owned port
	DefaultVLLMBundledPort = 9479 // waired-owned spawn target
	legacyVLLMDefaultPort  = 8000 // vLLM's upstream default; pre-cutover files wrote it explicitly
)

// ResolvedVLLMPort returns the port the vLLM engine actually uses.
// See the VLLMPort field comment for the legacy-8000 flip rule.
//
// The flip is what makes the move reach existing hosts. Defaults() is
// serialized into agent.json when the file is written, so every host set up
// before this change carries a literal 8000 that cannot be told apart from
// "unset" — the identical situation OllamaPort's 11434 flip was written for.
// An operator who deliberately moved the engine somewhere else keeps their
// port; the only value that moves is the one nobody chose.
func (c InferenceConfig) ResolvedVLLMPort() int {
	switch c.VLLMPort {
	case VLLMPortAuto, legacyVLLMDefaultPort:
		return DefaultVLLMBundledPort
	default:
		return c.VLLMPort
	}
}

func Defaults() Config {
	return Config{
		Inference: InferenceConfig{
			// BundledModelID is deliberately absent — see its field doc.
			PullOnStartup: true,
			// 0 = hold indefinitely; see the field doc (waired-agent#861).
			IdleTimeout:              0,
			MaxCacheGB:               100,
			AllowPull:                true,
			AllowAnthropicAPI:        true,
			AllowOpenAIAPI:           true,
			LocalGatewayPort:         9473,
			ClaudeGatewayPort:        9472,
			ClaudeTTFBBudgetMainMs:   60000,
			ClaudeTTFBBudgetSubMs:    20000,
			ClaudePeerWaitCeilingMs:  1800000,
			ClaudeLocalTTFBBudgetMs:  600000,
			OllamaPort:               OllamaPortAuto,
			VLLMPort:                 VLLMPortAuto,
			VLLMGPUMemoryUtilization: 0.85,
			VLLMTensorParallel:       0,
			PreferredEngine:          "",
			PreferredModelID:         "",
			InteractiveFloorTokps:    0,
			AllowAutoFallback:        true,
			PreCacheUpdateCandidate:  true,
			Enabled:                  true,
			ShareWithMesh:            true,

			ClaudeModelRouteDirectives: true,
			ClaudeModelPeerEntries:     5,
		},
		Routing: RoutingConfig{Mode: state.RoutingModeAuto},
		Logging: LoggingConfig{Level: LogLevelInfo},
	}
}

// Validate enforces invariants that cannot be expressed by zero-value
// defaults: numeric ranges, enum membership, etc. Call after the merge
// chain (JSON → env → flags) and before using the config.
func (c *Config) Validate() error {
	if v := c.Inference.VLLMGPUMemoryUtilization; v <= 0 || v > 1 {
		return fmt.Errorf("agentconfig: vllm_gpu_memory_utilization must be in (0, 1], got %v", v)
	}
	if v := c.Inference.VLLMTensorParallel; v < 0 {
		return fmt.Errorf("agentconfig: vllm_tensor_parallel must be >= 0 (0 = auto), got %d", v)
	}
	// 0 means auto; anything positive must clear vLLM's own max_num_seqs
	// default, which config/scheduler.py requires it to reach or exceed.
	if v := c.Inference.VLLMMaxNumBatchedTokens; v < 0 || (v > 0 && v < 256) {
		return fmt.Errorf("agentconfig: vllm_max_num_batched_tokens must be 0 (auto) or >= 256, got %d", v)
	}
	if v := c.Inference.VLLMKVOffloadingGiB; v < 0 {
		return fmt.Errorf("agentconfig: vllm_kv_offloading_gib must be >= 0 (0 = disabled), got %v", v)
	}
	if v := c.Inference.InteractiveFloorTokps; v < 0 {
		return fmt.Errorf("agentconfig: interactive_floor_tokps must be >= 0 (0 = default), got %v", v)
	}
	switch c.Inference.PreferredEngine {
	case "", "ollama", "vllm":
	default:
		return fmt.Errorf("agentconfig: preferred_engine must be \"\", \"ollama\", or \"vllm\", got %q", c.Inference.PreferredEngine)
	}
	if p := c.Inference.OllamaPort; p < 0 || p > 65535 {
		return fmt.Errorf("agentconfig: ollama_port must be in [0, 65535] (0 = auto), got %d", p)
	}
	if p := c.Inference.VLLMPort; p < 0 || p > 65535 {
		return fmt.Errorf("agentconfig: vllm_port must be in [0, 65535] (0 = auto), got %d", p)
	}
	if err := validateRouting(c.Routing); err != nil {
		return err
	}
	if _, err := ParseLogLevel(c.Logging.Level); err != nil {
		return err
	}
	return nil
}

func validateRouting(r RoutingConfig) error {
	switch r.Prefer {
	case "", state.RoutingPreferSpeed, state.RoutingPreferSize:
	default:
		return fmt.Errorf("agentconfig: unknown routing.prefer %q", r.Prefer)
	}
	if err := state.ValidateMinModelSize(r.MinModelSize); err != nil {
		return fmt.Errorf("agentconfig: routing.min_model_size: %w", err)
	}
	switch r.Mode {
	case "", state.RoutingModeAuto, state.RoutingModeLocalOnly,
		state.RoutingModePeerPreferred, state.RoutingModePeerOnly:
		if r.PinnedPeerDeviceID != "" {
			return fmt.Errorf("agentconfig: routing.mode %q must not carry pinned_peer_device_id", r.Mode)
		}
		return nil
	case state.RoutingModePinned:
		if r.PinnedPeerDeviceID == "" {
			return fmt.Errorf("agentconfig: routing.mode %q requires pinned_peer_device_id", r.Mode)
		}
		return nil
	default:
		return fmt.Errorf("agentconfig: unknown routing.mode %q", r.Mode)
	}
}

// MergeJSON overlays values from a JSON config file. A missing file is
// not an error (returns nil, leaves the receiver unchanged). Fields
// absent from the file keep their previous values.
func (c *Config) MergeJSON(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("agentconfig: read %s: %w", path, err)
	}
	// Decode into a copy so partially-specified JSON keeps existing values.
	tmp := *c
	if err := json.Unmarshal(data, &tmp); err != nil {
		return fmt.Errorf("agentconfig: parse %s: %w", path, err)
	}
	*c = tmp
	return nil
}

// MergeEnv overlays values from a list of "KEY=VALUE" strings (i.e. the
// shape returned by os.Environ()). Keys with the WAIRED_INFERENCE_
// prefix set inference fields; WAIRED_LOG_LEVEL sets logging.level.
// Unknown keys are ignored. Malformed values for known keys produce an
// error. (WAIRED_DEBUG is a separate legacy debug switch resolved by the
// binaries at boot, not a config field, so it is not consulted here.)
func (c *Config) MergeEnv(env []string) error {
	const inferencePrefix = "WAIRED_INFERENCE_"
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		switch {
		case key == "WAIRED_LOG_LEVEL":
			n, err := NormalizeLogLevel(val)
			if err != nil {
				return fmt.Errorf("agentconfig: env %s: %w", key, err)
			}
			c.Logging.Level = n
		case strings.HasPrefix(key, inferencePrefix):
			name := key[len(inferencePrefix):]
			if err := setInferenceField(&c.Inference, name, val); err != nil {
				return fmt.Errorf("agentconfig: env %s: %w", key, err)
			}
		}
	}
	return nil
}

func setInferenceField(c *InferenceConfig, envName, val string) error {
	switch envName {
	case "BUNDLED_MODEL_ID":
		c.BundledModelID = val
	case "PULL_ON_STARTUP":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.PullOnStartup = b
	case "IDLE_TIMEOUT":
		d, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		c.IdleTimeout = Duration(d)
	case "MAX_CACHE_GB":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.MaxCacheGB = n
	case "ALLOW_PULL":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.AllowPull = b
	case "ALLOW_ANTHROPIC_API":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.AllowAnthropicAPI = b
	case "ALLOW_OPENAI_API":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.AllowOpenAIAPI = b
	case "LOCAL_GATEWAY_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.LocalGatewayPort = n
	case "CLAUDE_GATEWAY_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudeGatewayPort = n
	case "CLAUDE_TTFB_BUDGET_MAIN_MS":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudeTTFBBudgetMainMs = n
	case "CLAUDE_TTFB_BUDGET_SUB_MS":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudeTTFBBudgetSubMs = n
	case "CLAUDE_PEER_WAIT_CEILING_MS":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudePeerWaitCeilingMs = n
	case "CLAUDE_LOCAL_TTFB_BUDGET_MS":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudeLocalTTFBBudgetMs = n
	case "OLLAMA_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.OllamaPort = n
	case "VLLM_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.VLLMPort = n
	case "VLLM_GPU_MEMORY_UTILIZATION":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		c.VLLMGPUMemoryUtilization = f
	case "VLLM_TENSOR_PARALLEL":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.VLLMTensorParallel = n
	case "VLLM_DISABLE_FP8_KV":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.VLLMDisableFP8KV = b
	case "VLLM_SPECULATIVE_NGRAM":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.VLLMSpeculativeNgram = b
	case "VLLM_MAX_NUM_BATCHED_TOKENS":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.VLLMMaxNumBatchedTokens = n
	case "VLLM_KV_OFFLOADING_GIB":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		c.VLLMKVOffloadingGiB = f
	case "VLLM_TOOL_PARSER":
		c.VLLMToolParser = val
	case "PREFERRED_ENGINE":
		c.PreferredEngine = val
	case "PREFERRED_MODEL_ID":
		c.PreferredModelID = val
	case "INTERACTIVE_FLOOR_TOKPS":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		c.InteractiveFloorTokps = f
	case "ALLOW_AUTO_FALLBACK":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.AllowAutoFallback = b
	case "PRE_CACHE_UPDATE_CANDIDATE":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.PreCacheUpdateCandidate = b
	case "ENABLED":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.Enabled = b
	case "SHARE_WITH_MESH":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.ShareWithMesh = b
	case "CLAUDE_MODEL_ROUTE_DIRECTIVES":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.ClaudeModelRouteDirectives = b
	case "CLAUDE_MODEL_PEER_ENTRIES":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.ClaudeModelPeerEntries = n
	default:
		// Unknown WAIRED_INFERENCE_* variable: ignore silently so we
		// can add new env-overridable fields in later phases without
		// breaking older agents.
	}
	return nil
}

// flagDuration adapts agentconfig.Duration to flag.Value so that
// time-duration strings parse the same way through every layer.
type flagDuration struct{ d *Duration }

func (f flagDuration) String() string {
	if f.d == nil {
		return ""
	}
	return time.Duration(*f.d).String()
}

func (f flagDuration) Set(s string) error {
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*f.d = Duration(parsed)
	return nil
}

// RegisterInferenceFlags registers --inference-* flags on fs whose
// defaults are the receiver's current values, and whose handlers
// mutate the receiver in-place. Call this AFTER MergeJSON+MergeEnv but
// BEFORE fs.Parse so flags become the final precedence layer.
func (c *Config) RegisterInferenceFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.Inference.BundledModelID, "inference-bundled-model-id",
		c.Inference.BundledModelID,
		"manifest model_id to auto-pull on agent startup")
	fs.BoolVar(&c.Inference.PullOnStartup, "inference-pull-on-startup",
		c.Inference.PullOnStartup,
		"background-pull the bundled model when waired-agent starts")
	fs.Var(flagDuration{&c.Inference.IdleTimeout}, "inference-idle-timeout",
		"how long the engine holds the model in memory after the last request (0 = never unload)")
	fs.IntVar(&c.Inference.MaxCacheGB, "inference-max-cache-gb",
		c.Inference.MaxCacheGB,
		"soft cap on total on-disk model cache size, in GB")
	fs.BoolVar(&c.Inference.AllowPull, "inference-allow-pull",
		c.Inference.AllowPull,
		"permit `waired models pull` and the startup pre-pull")
	fs.BoolVar(&c.Inference.AllowAnthropicAPI, "inference-allow-anthropic-api",
		c.Inference.AllowAnthropicAPI,
		"expose /anthropic/v1/messages on the Local Gateway")
	fs.BoolVar(&c.Inference.AllowOpenAIAPI, "inference-allow-openai-api",
		c.Inference.AllowOpenAIAPI,
		"expose /v1/chat/completions etc. on the Local Gateway")
	fs.IntVar(&c.Inference.LocalGatewayPort, "inference-local-gateway-port",
		c.Inference.LocalGatewayPort,
		"loopback port for the Local Gateway HTTP server")
	fs.IntVar(&c.Inference.ClaudeTTFBBudgetMainMs, "inference-claude-ttfb-budget-main-ms",
		c.Inference.ClaudeTTFBBudgetMainMs,
		"pre-first-byte deadline (ms) for a MAIN Claude request on a mesh peer before auto-rerouting to Anthropic (0=off)")
	fs.IntVar(&c.Inference.ClaudeTTFBBudgetSubMs, "inference-claude-ttfb-budget-sub-ms",
		c.Inference.ClaudeTTFBBudgetSubMs,
		"pre-first-byte deadline (ms) for a SUBAGENT Claude request on a mesh peer before auto-rerouting to Anthropic (0=off)")
	fs.IntVar(&c.Inference.ClaudePeerWaitCeilingMs, "inference-claude-peer-wait-ceiling-ms",
		c.Inference.ClaudePeerWaitCeilingMs,
		"total wait (ms) for a MAIN Claude request on a mesh peer that keeps reporting it is serving (0=off, flat deadline only)")
	fs.IntVar(&c.Inference.ClaudeLocalTTFBBudgetMs, "inference-claude-local-ttfb-budget-ms",
		c.Inference.ClaudeLocalTTFBBudgetMs,
		"pre-first-byte deadline (ms) for a Claude request on THIS computer's engine before auto-rerouting to Anthropic (0=off)")
	fs.IntVar(&c.Inference.OllamaPort, "inference-ollama-port",
		c.Inference.OllamaPort,
		"loopback port for the Ollama engine (0 = auto: 9475)")
	fs.IntVar(&c.Inference.VLLMPort, "inference-vllm-port",
		c.Inference.VLLMPort,
		"loopback port for the vLLM engine (0 = auto: 9479)")
	fs.Float64Var(&c.Inference.VLLMGPUMemoryUtilization, "inference-vllm-gpu-memory-utilization",
		c.Inference.VLLMGPUMemoryUtilization,
		"fraction of VRAM vLLM may reserve at startup (range (0, 1])")
	fs.IntVar(&c.Inference.VLLMTensorParallel, "inference-vllm-tensor-parallel",
		c.Inference.VLLMTensorParallel,
		"vLLM --tensor-parallel-size (0 = auto from identical NVIDIA GPU count)")
	fs.BoolVar(&c.Inference.VLLMDisableFP8KV, "inference-vllm-disable-fp8-kv",
		c.Inference.VLLMDisableFP8KV,
		"force fp16 KV cache instead of the Ada+ default fp8 (--kv-cache-dtype)")
	fs.BoolVar(&c.Inference.VLLMSpeculativeNgram, "inference-vllm-speculative-ngram",
		c.Inference.VLLMSpeculativeNgram,
		"enable vLLM ngram speculative decoding (single-stream decode boost)")
	fs.StringVar(&c.Inference.VLLMToolParser, "inference-vllm-tool-parser",
		c.Inference.VLLMToolParser,
		"override vLLM --tool-call-parser (\"\" picks from the served model)")
	fs.IntVar(&c.Inference.VLLMMaxNumBatchedTokens, "inference-vllm-max-num-batched-tokens",
		c.Inference.VLLMMaxNumBatchedTokens,
		"override vLLM --max-num-batched-tokens (0 = auto from the host's GPU)")
	fs.Float64Var(&c.Inference.VLLMKVOffloadingGiB, "inference-vllm-kv-offloading-gib",
		c.Inference.VLLMKVOffloadingGiB,
		"GiB of host RAM for vLLM KV offloading (0 = disabled; never persists across a restart)")
	fs.StringVar(&c.Inference.PreferredEngine, "inference-preferred-engine",
		c.Inference.PreferredEngine,
		"force engine pick (\"\" auto, \"ollama\", or \"vllm\")")
	fs.StringVar(&c.Inference.PreferredModelID, "inference-preferred-model-id",
		c.Inference.PreferredModelID,
		"force a specific manifest model_id (\"\" lets the auto-picker decide)")
	fs.Float64Var(&c.Inference.InteractiveFloorTokps, "inference-interactive-floor-tokps",
		c.Inference.InteractiveFloorTokps,
		"min boot-benchmark tokens/sec below which a lighter model is recommended (0 = default 60)")
	fs.BoolVar(&c.Inference.AllowAutoFallback, "inference-allow-auto-fallback",
		c.Inference.AllowAutoFallback,
		"allow bootstrap to fall back when the chosen runtime is unavailable; false means exit non-zero")
	fs.BoolVar(&c.Inference.PreCacheUpdateCandidate, "inference-pre-cache-update-candidate",
		c.Inference.PreCacheUpdateCandidate,
		"pre-download a better candidate (if any) at startup so refresh becomes a near-instant swap")
	fs.BoolVar(&c.Inference.Enabled, "inference-enabled",
		c.Inference.Enabled,
		"install-time choice: run a local inference engine on this node (read once at boot)")
	fs.BoolVar(&c.Inference.ShareWithMesh, "inference-share-with-mesh",
		c.Inference.ShareWithMesh,
		"install-time default: expose local engine to mesh peers (runtime toggle: `waired inference share`)")
	fs.BoolVar(&c.Inference.ClaudeModelRouteDirectives, "inference-claude-model-route-directives",
		c.Inference.ClaudeModelRouteDirectives,
		"opt-in: expose Waired as /model entries that switch Claude Code's backend + set an honest local window (#52)")
	fs.IntVar(&c.Inference.ClaudeModelPeerEntries, "inference-claude-model-peer-entries",
		c.Inference.ClaudeModelPeerEntries,
		"how many per-computer rows the /model picker carries alongside the fixed entries (0 = none)")
}

// DefaultJSONPath returns the canonical agent.json location under the
// platform-specific state directory (paths.StateDir with AutoDetect).
// The returned path is not guaranteed to exist.
func DefaultJSONPath() string {
	return filepath.Join(paths.StateDir(paths.AutoDetect), "agent.json")
}

// JSONPathFor returns <stateDir>/agent.json. Used by `waired init` so
// the installer writes to the same state-dir it persisted identity into
// instead of falling back to paths.AutoDetect (which may resolve to a
// different directory on Windows SCM vs interactive contexts).
func JSONPathFor(stateDir string) string {
	return filepath.Join(stateDir, "agent.json")
}

// Save atomically writes c to path with NonSecret protection
// (world-readable on Unix; default DACL on Windows). Mirrors the
// identity.Save pattern: json.MarshalIndent + secrets.WriteFile.
// The caller is expected to ensure the parent directory exists.
func (c *Config) Save(path string) error {
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("agentconfig: marshal: %w", err)
	}
	if err := secrets.WriteFile(path, body, secrets.NonSecret); err != nil {
		return fmt.Errorf("agentconfig: write %s: %w", path, err)
	}
	return nil
}
