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
	BundledModelID string `json:"bundled_model_id"`

	// PullOnStartup enables a background `ollama pull` of the bundled
	// model when waired-agent boots.
	PullOnStartup bool `json:"pull_on_startup"`

	// IdleTimeout is how long an Ollama subprocess may stay idle before
	// the agent stops it (engine lifecycle, spec §8.4).
	IdleTimeout Duration `json:"idle_timeout"`

	// MaxCacheGB caps total on-disk model cache (spec §9.3). Soft
	// limit enforced by the download manager.
	MaxCacheGB int `json:"max_cache_gb"`

	// AllowPull controls whether `waired models pull` and the startup
	// pre-pull are permitted at all.
	AllowPull bool `json:"allow_pull"`

	// AllowAnthropicAPI exposes /anthropic/v1/messages on the Local
	// Gateway. Disable to lock the gateway down to OpenAI compat only.
	AllowAnthropicAPI bool `json:"allow_anthropic_api"`

	// AllowOpenAIAPI exposes /v1/chat/completions etc. on the Local
	// Gateway.
	AllowOpenAIAPI bool `json:"allow_openai_api"`

	// LocalGatewayPort is the loopback port for the OpenAI/Anthropic
	// compat API server (spec waired_product_spec.md §3.3, §12.1 ⇒ 9473).
	LocalGatewayPort int `json:"local_gateway_port"`

	// OpenCodeGatewayPort is the loopback port for the no-token OpenCode
	// data-plane gateway. The waired-authored OpenCode plugin points its
	// provider baseURL here. Unlike LocalGatewayPort it does NOT require a
	// bearer token: the system-service deployment runs the agent as a
	// dedicated user whose 0600 gateway token the desktop user's OpenCode
	// cannot read, so loopback is the trust boundary (same posture as the
	// Claude proxy's no-token overlay handler). 0 disables the listener.
	OpenCodeGatewayPort int `json:"opencode_gateway_port"`

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

	// CodeUIEnabled gates the bundled OpenCode coding-agent web UI (#429):
	// a waired-vendored `opencode serve` instance wired to the no-token
	// data-plane gateway, opened from the tray. Default true. It starts
	// lazily on first "Open Coding Agent" and is loopback-only.
	CodeUIEnabled bool `json:"codeui_enabled"`

	// CodeUIPort is the loopback port the bundled coding-agent UI binds.
	// 0 (the default) resolves to DefaultCodeUIPort via ResolvedCodeUIPort().
	CodeUIPort int `json:"codeui_port"`

	// OllamaPort is the loopback port of the Ollama engine. Leave at
	// OllamaPortAuto (0) to resolve by OllamaSource: bundled spawns on
	// DefaultOllamaBundledPort (9475, waired-owned), reuse probes the
	// user's engine on DefaultOllamaReusePort (11434, upstream default).
	// Read it through ResolvedOllamaPort(), never directly: a literal
	// 11434 under bundled is treated as the legacy serialized default
	// and flips to 9475 (every pre-cutover agent.json wrote 11434
	// explicitly, so it cannot be distinguished from "unset").
	OllamaPort int `json:"ollama_port"`

	// OllamaSource selects how the Ollama engine is provided (#188):
	//   "bundled" (default): waired downloads + supervises its own pinned
	//                        Ollama as a foreground child (no service).
	//   "reuse":             borrow an Ollama the user already runs at
	//                        Host:OllamaPort — the agent probes it and
	//                        never spawns/stops it.
	// Chosen at `waired init`; default is always "bundled".
	OllamaSource string `json:"ollama_source"`

	// VLLMPort is the loopback port the vLLM subprocess binds to. Used
	// by the gateway proxy when the active runtime is vllm.
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

	// ExternalEndpoints lists OpenAI-compatible HTTP endpoints the
	// agent's loopback gateway may fall back to when no local engine
	// has the requested model. Each entry becomes a registered
	// runtime.Adapter under the name "openai-compat:<id>".
	//
	// Phase 5 scope: agent-local only. These endpoints are NEVER
	// surfaced in signer.InferenceState, so mesh peers never see them
	// and cannot proxy through this agent to reach them.
	ExternalEndpoints []ExternalEndpoint `json:"external_endpoints,omitempty"`

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
}

// ExternalEndpoint configures one entry in InferenceConfig.ExternalEndpoints.
// See internal/runtime/openaicompat for the adapter and the bearer-auth
// round-tripper that consumes AuthEnvVar.
type ExternalEndpoint struct {
	// ID is the registry suffix (Adapter.Name() returns
	// "openai-compat:<ID>"). Must be unique within ExternalEndpoints.
	// Empty falls back to a host:port-derived identifier inside the
	// adapter at construction time.
	ID string `json:"id,omitempty"`

	// URL is the OpenAI-compat base. Both "http://host:8000" and
	// "http://host:8000/v1" forms are accepted; the adapter normalises
	// by trimming a trailing /v1.
	URL string `json:"url"`

	// AuthEnvVar names the environment variable holding the Bearer
	// token. Empty string disables auth-header injection. The value
	// is captured once at adapter construction; mid-run env changes
	// require an agent restart.
	AuthEnvVar string `json:"auth_env_var,omitempty"`

	// Disable, when true, leaves the entry in the file (so the
	// history of what was once configured stays visible) but skips
	// registry registration on this boot.
	Disable bool `json:"disable,omitempty"`
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
	return state.RoutingPreference{Mode: m, PinnedPeerDeviceID: pin}
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
}

// Defaults returns the Config that ships when no file / env / flag is
// supplied. Update spec §19-6 (and bump Phase A docs) whenever these
// change.
// Ollama engine source values for InferenceConfig.OllamaSource (#188).
const (
	OllamaSourceBundled = "bundled" // waired downloads + supervises its own ollama
	OllamaSourceReuse   = "reuse"   // borrow a user-run ollama at Host:OllamaPort
)

// Ollama port resolution. The bundled engine used to bind the upstream
// default 11434 and silently adopt whatever system ollama already owned
// it — which broke the version pin invisibly (a 0.30.7-pinned node was
// actually served by a system 0.24.0). The bundled engine now owns
// 9475, the free slot in waired's loopback family (9473 gateway, 9474
// overlay, 9476 management, 9477 control plane, 9478 relay), so it
// never contends with a user's ollama; reuse mode keeps 11434.
const (
	OllamaPortAuto           = 0     // resolve by OllamaSource
	DefaultOllamaReusePort   = 11434 // probe the user's engine (upstream default)
	DefaultOllamaBundledPort = 9475  // waired-owned spawn target
)

// DefaultCodeUIPort is the loopback port for the bundled OpenCode
// coding-agent web UI (#429): 9480, the next free slot in waired's loopback
// family (9473 gateway, 9474 overlay, 9475 ollama-bundled, 9476 management,
// 9477 control plane, 9478 relay, 9479 opencode data-plane gateway). The
// vendored `opencode serve` binds here; its provider points at 9479. Chosen
// off the common chat/IDE defaults so it never collides with a user's own
// `opencode serve` (the same anti-collision rule as bundled Ollama on 9475).
const DefaultCodeUIPort = 9480

// ResolvedCodeUIPort returns the port the bundled coding-agent UI binds:
// CodeUIPort when set, else DefaultCodeUIPort.
func (c InferenceConfig) ResolvedCodeUIPort() int {
	if c.CodeUIPort > 0 {
		return c.CodeUIPort
	}
	return DefaultCodeUIPort
}

// ResolvedOllamaPort returns the port the Ollama engine actually uses.
// See the OllamaPort field comment for the legacy-11434 flip rule.
func (c InferenceConfig) ResolvedOllamaPort() int {
	reuse := c.OllamaSource == OllamaSourceReuse
	switch c.OllamaPort {
	case OllamaPortAuto, DefaultOllamaReusePort:
		if reuse {
			return DefaultOllamaReusePort
		}
		return DefaultOllamaBundledPort
	default:
		return c.OllamaPort
	}
}

func Defaults() Config {
	return Config{
		Inference: InferenceConfig{
			BundledModelID:           "qwen2.5-coder-7b-instruct",
			PullOnStartup:            true,
			IdleTimeout:              Duration(10 * time.Minute),
			MaxCacheGB:               100,
			AllowPull:                true,
			AllowAnthropicAPI:        true,
			AllowOpenAIAPI:           true,
			LocalGatewayPort:         9473,
			OpenCodeGatewayPort:      9479,
			ClaudeGatewayPort:        9472,
			ClaudeTTFBBudgetMainMs:   60000,
			ClaudeTTFBBudgetSubMs:    20000,
			CodeUIEnabled:            true,
			OllamaPort:               OllamaPortAuto,
			OllamaSource:             OllamaSourceBundled,
			VLLMPort:                 8000,
			VLLMGPUMemoryUtilization: 0.85,
			VLLMTensorParallel:       0,
			PreferredEngine:          "",
			PreferredModelID:         "",
			InteractiveFloorTokps:    0,
			AllowAutoFallback:        true,
			PreCacheUpdateCandidate:  true,
			Enabled:                  true,
			ShareWithMesh:            true,
		},
		Routing: RoutingConfig{Mode: state.RoutingModeAuto},
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
	if v := c.Inference.InteractiveFloorTokps; v < 0 {
		return fmt.Errorf("agentconfig: interactive_floor_tokps must be >= 0 (0 = default), got %v", v)
	}
	switch c.Inference.PreferredEngine {
	case "", "ollama", "vllm":
	default:
		return fmt.Errorf("agentconfig: preferred_engine must be \"\", \"ollama\", or \"vllm\", got %q", c.Inference.PreferredEngine)
	}
	switch c.Inference.OllamaSource {
	case "", OllamaSourceBundled, OllamaSourceReuse:
		// "" is accepted for backward compatibility (pre-#188 agent.json)
		// and treated as bundled by the agent.
	default:
		return fmt.Errorf("agentconfig: ollama_source must be %q or %q, got %q", OllamaSourceBundled, OllamaSourceReuse, c.Inference.OllamaSource)
	}
	if p := c.Inference.OllamaPort; p < 0 || p > 65535 {
		return fmt.Errorf("agentconfig: ollama_port must be in [0, 65535] (0 = auto), got %d", p)
	}
	if err := validateExternalEndpoints(c.Inference.ExternalEndpoints); err != nil {
		return err
	}
	if err := validateRouting(c.Routing); err != nil {
		return err
	}
	return nil
}

func validateRouting(r RoutingConfig) error {
	switch r.Mode {
	case "", state.RoutingModeAuto, state.RoutingModeLocalOnly, state.RoutingModePeerPreferred:
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

// validateExternalEndpoints enforces uniqueness of IDs (so the runtime
// registry does not collide) and basic URL well-formedness. Disabled
// entries are still checked because flipping Disable=false should not
// require an additional validation round-trip.
func validateExternalEndpoints(eps []ExternalEndpoint) error {
	seen := make(map[string]int, len(eps))
	for i, ep := range eps {
		if ep.URL == "" {
			return fmt.Errorf("agentconfig: external_endpoints[%d]: url required", i)
		}
		// Light URL well-formedness check; the adapter parses and
		// normalises authoritatively at NewAdapter time.
		switch {
		case strings.HasPrefix(ep.URL, "http://"), strings.HasPrefix(ep.URL, "https://"):
		default:
			return fmt.Errorf("agentconfig: external_endpoints[%d].url %q must start with http:// or https://", i, ep.URL)
		}
		key := ep.ID
		if key == "" {
			// Two empty IDs would collide post-normalisation, so
			// require operators to disambiguate explicitly when there
			// is more than one endpoint without an ID.
			key = "<empty>"
		}
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("agentconfig: external_endpoints[%d].id %q duplicates external_endpoints[%d]", i, ep.ID, prev)
		}
		seen[key] = i
	}
	return nil
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
// shape returned by os.Environ()). Only keys with the WAIRED_INFERENCE_
// prefix are consulted. Unknown keys are ignored. Malformed values for
// known keys produce an error.
func (c *Config) MergeEnv(env []string) error {
	const inferencePrefix = "WAIRED_INFERENCE_"
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		switch {
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
	case "OPENCODE_GATEWAY_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.OpenCodeGatewayPort = n
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
	case "OLLAMA_PORT":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		c.OllamaPort = n
	case "OLLAMA_SOURCE":
		c.OllamaSource = val
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
		"how long an idle Ollama subprocess may run before being stopped")
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
	fs.IntVar(&c.Inference.OllamaPort, "inference-ollama-port",
		c.Inference.OllamaPort,
		"loopback port for the Ollama engine (0 = auto: bundled 9475, reuse 11434)")
	fs.StringVar(&c.Inference.OllamaSource, "inference-ollama-source",
		c.Inference.OllamaSource,
		"how Ollama is provided: \"bundled\" (waired-managed) or \"reuse\" (borrow a user-run ollama)")
	fs.IntVar(&c.Inference.VLLMPort, "inference-vllm-port",
		c.Inference.VLLMPort,
		"loopback port for the vLLM subprocess to listen on")
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
