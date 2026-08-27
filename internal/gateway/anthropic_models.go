package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// anthropicModel is the Anthropic Models API object, extended with
// max_input_tokens — the field Claude Code's gateway model discovery
// (CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1) reads to size its
// auto-compaction threshold (#623). We advertise the effective LOCAL
// window (min native / host-sustainable, from Deps.ContextWindowFor) so
// Claude Code compacts before it overruns the model and Ollama truncates
// the prompt head. Omitted (0) when the window is unknown.
type anthropicModel struct {
	Type           string `json:"type"`
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	MaxInputTokens int    `json:"max_input_tokens,omitempty"`
}

const anthropicModelsPrefix = "/anthropic/v1/models/"

// Reserved /model route-directive ids (#52), advertised in the Claude
// intercept's /v1/models discovery (gated by Deps.ClaudeModelDirectives) so
// they surface in Claude Code's /model picker — which filters discovered ids
// to ^(claude|anthropic); their display_name is free-form. Selecting one makes
// the intercept force this request's route, overriding the /waired-route
// policy. The intercept duplicates these literals to stay stdlib-only — keep
// both sides in sync (internal/proxy/intercept/model_rewrite.go).
const (
	// ModelWairedLocal pins the conversation to LOCAL inference (the intercept
	// forces route=waired). It deliberately does NOT start with "claude-", so
	// Claude Code applies the CLAUDE_CODE_MAX_CONTEXT_TOKENS managed-settings
	// value to it (that env is honoured only for non-"claude-" ids) — the
	// honest ~256k local window instead of Claude Code's 200k default.
	ModelWairedLocal = "anthropic-waired-local"
	// ModelWairedAuto and ModelWairedAuto1M pin the conversation to AUTO
	// routing (the intercept forces route=auto): Waired inference first,
	// falling back to the real Anthropic API when no node serves the tier.
	// On a fallback leg the intercept rewrites either id to a real model
	// (same as the cloud id).
	//
	// Both start with "claude-", so Claude Code sizes them from the id
	// string alone and never from CLAUDE_CODE_MAX_CONTEXT_TOKENS: the
	// bare id takes the 200k default, the "[1m]" suffix takes 1M. That
	// is the whole reason for the prefix (waired#1031). The env var is a
	// single global shared by EVERY non-"claude-" id, so while auto lived
	// there it could not carry a window different from the local pin's —
	// and after #408 pointed that value at this device's real window,
	// auto's Anthropic fallback leg ran in a session sized to whatever
	// engine happened to be installed here.
	//
	// A tier is a promise about the SERVING node, so the router only
	// selects an endpoint that declares it (RequiredWindowFor); when none
	// does, selection fails and auto's fallback carries the turn to
	// Anthropic, which is the honest answer rather than a local engine
	// pretending to a window it does not hold.
	ModelWairedAuto   = "claude-waired-auto"
	ModelWairedAuto1M = "claude-waired-auto[1m]"

	// ModelWairedPeer restricts the conversation to ANOTHER computer on
	// the mesh, and never falls back to this one — the /model face of the
	// peer-only worker mode, which docs/decisions/20260801/1840 ratified
	// as fail-closed. The intercept forces route=waired, so the turn never
	// leaves for Anthropic either; when no peer can answer, the request
	// fails and says so. Owner request on waired-ai/waired#1223: "peer で
	// の推論に限定するモードとして".
	//
	// It starts with "claude-" and it is NOT a tier promise, which look
	// contradictory until you separate the two questions the prefix
	// decides. The prefix decides how Claude Code sizes the SESSION: a
	// "claude-" id takes its 200k default, a non-"claude-" id takes
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS — and that env is one global holding
	// THIS device's window, which is the wrong number for every peer.
	// RequiredWindowFor decides what the SERVING node must promise, and a
	// request that names a node must not also make demands of it: that is
	// the same reasoning ModelWairedLocal already carries, and it returns
	// 0 for exactly that reason.
	ModelWairedPeer = "claude-waired-peer"
	// ModelWairedPeerPrefix heads the per-peer entries generated from the live
	// mesh (waired-agent#830) — "claude-waired-peer-<node>". They are not
	// constants because the set is whatever is serving right now, so every
	// layer recognises them by this prefix. Sharing ModelWairedPeer's spelling
	// is deliberate: one family, one route, one place to look.
	ModelWairedPeerPrefix = ModelWairedPeer + "-"

	// ModelWairedPublic restricts the conversation to a Public Share
	// machine — someone else's computer, lent through Waired
	// (waired-agent#901, owner request). Like the peer entry it names a
	// node class rather than a model, takes route=waired, and never falls
	// back to this device.
	//
	// It does NOT override the consumer's standing Public Share posture
	// (owner ruling 2026-08-20): with the posture on `auto`, a public
	// machine still has to beat this host's own best tier, so the entry
	// can legitimately decline. It is advertised only on a host that has
	// consented and enabled Public Share, so the case "offered but the
	// posture forbids everything" does not arise.
	ModelWairedPublic = "claude-waired-public"

	// ModelWairedAutoLegacy is the pre-waired#1031 spelling of
	// ModelWairedAuto. It is no longer advertised, and the intercept still
	// routes it: a Claude Code that selected it before an upgrade keeps
	// the id in its own settings until the operator picks again, and a
	// stale picker cache can hand it back for a whole session.
	ModelWairedAutoLegacy = "anthropic-waired-auto"
	// ModelWairedCloud pins the conversation to the real Anthropic API (the
	// intercept forces route=anthropic and rewrites this id to a real model on
	// passthrough). The "[1m]" suffix gives it Claude Code's 1M window.
	ModelWairedCloud = "claude-waired-cloud[1m]"
)

// handleAnthropicModels serves the Anthropic Models API locally so Claude
// Code — routed here by the intercept's /v1/models override (#623) —
// discovers the LOCAL catalog and, crucially, each model's effective
// context window rather than the real Anthropic 1M/200k metadata. It
// mirrors handleOpenAIModels' listing (the dynamic coding aliases plus
// every manifest id/alias, deduped) but in Anthropic's
// {data, has_more, first_id, last_id} envelope, and additionally stamps
// max_input_tokens from Deps.ContextWindowFor.
//
//   - GET /anthropic/v1/models        → the list
//   - GET /anthropic/v1/models/{id}   → a single model object
func (h *HandlerSet) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "GET only")
		return
	}

	models := h.anthropicModelList()
	slog.Debug("anthropic models request", "method", r.Method, "path", r.URL.Path, "count", len(models))

	// Single-object form: a non-empty id after the collection prefix.
	if id, ok := strings.CutPrefix(r.URL.Path, anthropicModelsPrefix); ok && id != "" {
		for _, m := range models {
			if m.ID == id {
				writeJSON(w, http.StatusOK, m)
				return
			}
		}
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("model %q not found", id))
		return
	}

	out := map[string]any{"data": models, "has_more": false}
	if len(models) > 0 {
		out["first_id"] = models[0].ID
		out["last_id"] = models[len(models)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// DirectiveModel is one reserved route-directive id and the label the
// /model picker renders for it.
type DirectiveModel struct {
	ID          string
	DisplayName string
}

// DirectiveModels is the set of reserved route-directive ids this gateway
// advertises, in the order they appear in /model.
//
// Exported because it is the parity anchor for the two hand-duplicated
// copies of the same table — internal/proxy/intercept (which stays
// stdlib-only, so it cannot import this) and internal/integration/claudecode
// (which the `waired` CLI links, and which must not pull in the router and
// the whole inference stack for four strings). Both are asserted equal to
// this one, display names included.
//
// It replaced four inline add() calls: hand-written id lists in three tests
// covered three of the four entries, so ModelWairedAuto1M could be — and
// was — missing from a surface while every parity test stayed green
// (waired-agent#830).
func DirectiveModels() []DirectiveModel {
	return []DirectiveModel{
		{ModelWairedAuto, "Waired auto — 200k (local, fallback to Anthropic)"},
		{ModelWairedAuto1M, "Waired auto — 1M (local, fallback to Anthropic)"},
		{ModelWairedLocal, "Waired local (this device)"},
		// Directly after the local pin: both name a node rather than a
		// tier, and only about four Waired rows are visible in the picker
		// before Claude Code folds the rest behind "… +N models" (measured
		// on device, waired-ai/waired#1223). Owner ruling 2026-08-20.
		{ModelWairedPeer, "Waired peer (another device, no local fallback)"},
		// Next to the peer entry: both send the turn to another computer,
		// and this one only differs in whose. Advertised conditionally —
		// the picker-cache writer drops it on a host that has not enabled
		// Public Share — but present here, because the intercept has to be
		// able to route an id a client still holds from before.
		{ModelWairedPublic, "Waired public share (someone else's computer)"},
		// ModelWairedCloud is NOT here. Picking a real Anthropic model in
		// /model now routes to the real Anthropic API on its own
		// (waired-agent#1037), which says the same thing and also says WHICH
		// model answers — so the row bought nothing and cost one of the ~4
		// Waired rows visible before the picker folds. It is still routed:
		// see RoutedDirectiveModels.
	}
}

// RoutedDirectiveModels are ids the intercept still honours but no surface
// offers. The picker cache has no TTL and a Claude Code that selected one
// keeps it in its own settings, so a whole session can arrive under a name
// this build no longer advertises.
func RoutedDirectiveModels() []DirectiveModel {
	return []DirectiveModel{
		{ModelWairedCloud, "Waired cloud (Anthropic API)"},
		{ModelWairedAutoLegacy, "Waired auto — 200k (local, fallback to Anthropic)"},
	}
}

// anthropicModelList builds the deduped model list. The advertised window
// comes from Deps.ContextWindowFor, which resolves dynamic aliases and
// unknown claude-* ids to the device-active model (so waired/default and
// the claude-* ids Claude Code selects both carry the real local window).
func (h *HandlerSet) anthropicModelList() []anthropicModel {
	created := time.Now().UTC().Format(time.RFC3339)
	out := []anthropicModel{}
	seen := map[string]struct{}{}
	add := func(id, display string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		m := anthropicModel{Type: "model", ID: id, DisplayName: display, CreatedAt: created}
		if h.deps.ContextWindowFor != nil {
			m.MaxInputTokens = h.deps.ContextWindowFor(id)
		}
		out = append(out, m)
	}
	// #52: reserved route-directive ids first, so they are prominent in the
	// /model picker. Opt-in via agentconfig; only advertised on the Claude
	// intercept surface. add() stamps each with ContextWindowFor (harmless —
	// Claude Code sizes the window from the id string, not this field); the
	// honest local window comes from CLAUDE_CODE_MAX_CONTEXT_TOKENS instead.
	if h.deps.ClaudeModelDirectives {
		for _, d := range DirectiveModels() {
			add(d.ID, d.DisplayName)
		}
	}
	for _, id := range router.DynamicCodingAliases {
		add(id, "")
	}
	for _, mf := range h.deps.ListManifests() {
		add(mf.ModelID, mf.DisplayName)
		for _, alias := range mf.ModelAliases {
			add(alias, mf.DisplayName)
		}
	}
	return out
}

// RequiredWindowFor is the input-token window a request for modelID
// obliges the serving endpoint to hold, or 0 when the id makes no such
// promise (waired#1031).
//
// Only the two auto tiers do. The local id routes to this device
// whatever its window is — that is what pinning means, and it is the
// only way to reach a device that declares no window at all. The cloud
// id never touches a Waired endpoint.
//
// The legacy auto spelling deliberately returns 0 rather than the 200k
// tier: it is a non-"claude-" id, so a client that still holds it is in
// a session sized by CLAUDE_CODE_MAX_CONTEXT_TOKENS, and holding its
// endpoint to a window its own session was never sized for would refuse
// turns that used to work.
// NodeDirectiveFor reports the directive id when modelID names a NODE to
// serve on, or "" when it does not.
//
// Separate from RequiredWindowFor because the two answer different
// questions about the same id — one is a promise the serving node must
// keep, the other is which node serves at all — and a directive can be
// one without being the other. The peer id is: naming a node and then
// demanding a window of it would refuse turns on the very machine the
// operator chose, which is why RequiredWindowFor returns 0 for it.
//
// The local pin is deliberately NOT one of these. It resolves to this
// device without a routing preference at all (the intercept forces
// route=waired and the overlay-side Selector has no mesh), so giving it
// a node directive would add a second, redundant way to say the same
// thing — and two mechanisms for one behaviour is how they drift.
func NodeDirectiveFor(modelID string) string {
	modelID = NormalizeModelID(modelID)
	if modelID == ModelWairedPeer || modelID == ModelWairedPublic {
		return modelID
	}
	// Per-peer ids are generated from the live mesh, so they are recognised by
	// prefix rather than enumerated. The whole id travels: the layer that
	// resolves it re-derives the same slug from the same snapshot, and
	// carrying the id rather than a parsed slug keeps that one comparison in
	// one place (waired-agent#830).
	if strings.HasPrefix(modelID, ModelWairedPeerPrefix) && len(modelID) > len(ModelWairedPeerPrefix) {
		return modelID
	}
	return ""
}

// tierMarker1M is the suffix Claude Code sizes a session from. It reads the
// marker case-insensitively and anywhere in the id — and strips it before the
// request leaves, which is why lookups here are on the bare form and the tier
// itself has to come from the beta header (RequiredWindowForRequest).
const tierMarker1M = "[1m]"

// NormalizeModelID reduces a client-sent id to the form these tables are keyed
// by: lower-cased, with every tier marker removed. The ADVERTISED spelling is
// unchanged — Claude Code needs "[1m]" in the id to size the session — so only
// the lookup is on the bare form (waired-agent#1036).
func NormalizeModelID(modelID string) string {
	bare := strings.ToLower(strings.TrimSpace(modelID))
	for {
		i := strings.Index(bare, tierMarker1M)
		if i < 0 {
			return bare
		}
		bare = bare[:i] + bare[i+len(tierMarker1M):]
	}
}

// RequiredWindowFor is the tier a model id promises on its own. Since Claude
// Code strips "[1m]" on the wire, the 1M spelling reaches us as the bare auto
// id and this function cannot see the tier — RequiredWindowForRequest reads it
// off the beta header instead. Both remain: an id that still carries the marker
// (another client, a replayed capture) is answered here.
func RequiredWindowFor(modelID string) int {
	switch NormalizeModelID(modelID) {
	case ModelWairedAuto:
		if strings.Contains(strings.ToLower(modelID), tierMarker1M) {
			return hostfit.ServingWindow1M
		}
		return hostfit.ServingWindow200k
	default:
		return 0
	}
}

// context1MBetaPrefix heads the beta flag Claude Code sends for a 1M session
// ("context-1m-2025-08-07" at the time of measurement). The date moves, so the
// prefix is what is matched.
const context1MBetaPrefix = "context-1m"

// RequiredWindowForRequest is RequiredWindowFor plus the one place the 1M tier
// actually survives the trip. Claude Code strips "[1m]" from the model id
// before sending and keeps the tier only in `anthropic-beta` (measured on
// 2.1.229 / 2.1.241 / 2.1.245, waired-agent#1036), so a session sized to 1M
// arrives indistinguishable from a 200k one unless the header is read.
//
// The header widens the demand and never narrows it: a client that asks for 1M
// gets a serving node that declares 1M, or the auto route's fallback carries
// the turn to Anthropic — which is the tier's contract. Ids that make no tier
// promise (local, peer, public, cloud) stay at 0 whatever the header says:
// naming a node must not also make demands of it.
func RequiredWindowForRequest(modelID string, betaHeader []string) int {
	want := RequiredWindowFor(modelID)
	if want == 0 || want >= hostfit.ServingWindow1M {
		return want
	}
	for _, h := range betaHeader {
		for _, flag := range strings.Split(h, ",") {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(flag)), context1MBetaPrefix) {
				return hostfit.ServingWindow1M
			}
		}
	}
	return want
}
