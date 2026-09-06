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

// Reserved /model ids (#52). They name a SIDE rather than a model: selecting
// one makes the intercept serve the turn on Waired, on the node the id names.
// The intercept duplicates these literals to stay stdlib-only — keep both
// sides in sync (internal/proxy/intercept/model_rewrite.go), and
// internal/integration/claudecode/directives.go carries the CLI's copy.
//
// waired-agent#1185 re-spelled them. They used to carry a "claude-" or
// "anthropic-" head, for one reason and with one side effect.
//
// The reason: Claude Code's gateway model discovery keeps only ids containing
// "claude" or "anthropic", and discovery was how the rows reached the picker
// — waired wrote Claude Code's private discovery cache by hand, because
// discovery itself is credential-gated and waired supplies no credential. The
// rows come from the documented `modelPicker` setting now, which filters
// nothing, so the head has no work left to do.
//
// The side effect, which is why the two heads differed: Claude Code honours
// CLAUDE_CODE_MAX_CONTEXT_TOKENS only for ids that do NOT start with
// "claude-" (its predicate is `!id.toLowerCase().startsWith("claude-")`,
// measured on 2.1.261, 2026-09-06). Managed settings set that variable to
// THIS computer's window, which is right for the local row and wrong for a
// peer, so the local id was spelled "anthropic-" to take it and every other
// id was spelled "claude-" to refuse it. The cost was invisible until 2.1.26x
// started enforcing an assumed window for catalog-unknown ids: every
// "claude-"-headed row silently ran in a 200k session and put a notice on
// screen saying the id "isn't described by this version's model catalog".
// With no head at all, every row takes the variable — one number, this
// computer's, approximate for a peer and exact for the local row — and the
// notice is gone. The number is a compaction hint either way; what actually
// refuses an over-long prompt is this gateway's own 400
// (docs/decisions/20260714/0241-drop-static-auto-compact-window-pin.md).
const (
	// ModelWairedAny names any Waired node — this computer or a peer,
	// whichever the mesh offers. Fail-closed: when no node can answer the
	// turn ends saying so, and never crosses to Anthropic
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	ModelWairedAny = "waired"
	// ModelWairedLocal pins the conversation to this computer's engine.
	ModelWairedLocal = "waired/local"
	// ModelWairedPeer restricts the conversation to ANOTHER computer on the
	// mesh, and never falls back to this one — the /model face of the
	// peer-only worker mode, which docs/decisions/20260801/1840 ratified as
	// fail-closed. Owner request on waired-ai/waired#1223.
	ModelWairedPeer = "waired/peer"
	// ModelWairedPeerPrefix heads the per-peer entries generated from the
	// live mesh (waired-agent#830) — "waired/peer-<node>". They are not
	// constants because the set is whatever is serving right now, so every
	// layer recognises them by this prefix. Sharing ModelWairedPeer's
	// spelling is deliberate: one family, one route, one place to look.
	ModelWairedPeerPrefix = ModelWairedPeer + "-"
	// ModelWairedPublic restricts the conversation to a Public Share
	// machine — someone else's computer, lent through Waired
	// (waired-agent#901, owner request). Like the peer entry it names a node
	// class rather than a model, and never falls back to this device.
	//
	// It does NOT override the consumer's standing Public Share posture
	// (owner ruling 2026-08-20): with the posture on `auto`, a public
	// machine still has to beat this host's own best tier, so the entry can
	// legitimately decline.
	//
	// The picker leaves the row out on a host that has not enabled Public
	// Share, but "offered but the posture forbids everything" DOES arise
	// and this comment used to deny it (waired-agent#1201): a session keeps
	// the id it last picked in its own settings, and the legacy spelling
	// below is still routed for exactly that reason. The router words those
	// refusals by naming which switch declined.
	ModelWairedPublic = "waired/public"
)

// The pre-waired-agent#1185 spellings. No surface offers them; every layer
// still routes them, because a session that selected one keeps the id in its
// own settings until the operator picks again.
const (
	// ModelWairedAnyLegacy and ModelWairedAuto1MLegacy named the any-node
	// row. "auto" was the route it used to force — Waired first, then the
	// real Anthropic API — which waired-agent#1184 removed; the spelling
	// outlived the route only because sessions were carrying it.
	ModelWairedAnyLegacy    = "claude-waired-auto"
	ModelWairedAuto1MLegacy = "claude-waired-auto[1m]"
	// ModelWairedAnyOldest is the pre-waired#1031 spelling of the same row.
	ModelWairedAnyOldest = "anthropic-waired-auto"
	// The named rows before #1185.
	ModelWairedLocalLegacy      = "anthropic-waired-local"
	ModelWairedPeerLegacy       = "claude-waired-peer"
	ModelWairedPeerPrefixLegacy = ModelWairedPeerLegacy + "-"
	ModelWairedPublicLegacy     = "claude-waired-public"
	// ModelWairedCloud pinned the conversation to the real Anthropic API.
	// Retired by waired-agent#1037: naming a real Anthropic model in /model
	// reaches the real API on its own, and says which model answered.
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
	// Description is the picker row's second line.
	Description string
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
		{ID: ModelWairedAny, DisplayName: "Waired",
			Description: "Any of your computers"},
		{ID: ModelWairedLocal, DisplayName: "Waired local",
			Description: "This computer"},
		// Directly after the local pin: both name a node rather than a
		// tier, and only about four Waired rows are visible in the picker
		// before Claude Code folds the rest behind "… +N models" (measured
		// on device, waired-ai/waired#1223). Owner ruling 2026-08-20.
		{ID: ModelWairedPeer, DisplayName: "Waired peer",
			Description: "Another of your computers"},
		// Next to the peer entry: both send the turn to another computer,
		// and this one only differs in whose. Offered conditionally — the
		// picker writer drops it on a host that has not enabled Public
		// Share — but present here, because the intercept has to be able to
		// route an id a client still holds from before.
		{ID: ModelWairedPublic, DisplayName: "Waired public share",
			Description: "Someone else's computer"},
		// The 1M twins are NOT here. A tier is a promise about the serving
		// node, so a twin is offered only where a node declares 1M, and
		// this table cannot know that — the picker writer adds them
		// (owner ruling 2026-09-06).
	}
}

// RoutedDirectiveModels are ids the intercept still honours but no surface
// offers: the 1M twins, and every pre-waired-agent#1185 spelling. A Claude
// Code that selected one keeps it in its own settings until the operator
// picks again, so a whole session can arrive under a name this build no
// longer advertises.
func RoutedDirectiveModels() []DirectiveModel {
	out := []DirectiveModel{
		{ID: ModelWairedCloud, DisplayName: "Waired cloud (Anthropic API)"},
		{ID: ModelWairedAnyLegacy, DisplayName: "Waired"},
		{ID: ModelWairedAuto1MLegacy, DisplayName: "Waired (1M context)"},
		{ID: ModelWairedAnyOldest, DisplayName: "Waired"},
		{ID: ModelWairedLocalLegacy, DisplayName: "Waired local"},
		{ID: ModelWairedPeerLegacy, DisplayName: "Waired peer"},
		{ID: ModelWairedPublicLegacy, DisplayName: "Waired public share"},
	}
	// Every current row also answers to its 1M twin, whether or not this
	// host offers one: the twin's id is what a session carries after the
	// operator picked it, and the mesh it was offered from can change under
	// that session. Whether a node can actually serve the tier is
	// RequiredWindowForRequest's question, asked per request.
	for _, d := range DirectiveModels() {
		out = append(out, DirectiveModel{
			ID:          Tier1M(d.ID),
			DisplayName: d.DisplayName + " (1M context)",
			Description: d.Description,
		})
	}
	return out
}

// Tier1M spells the 1M twin of a directive id. Claude Code sizes a session
// from this suffix and strips it before sending, so the tier survives the
// trip only in `anthropic-beta: context-1m-*` — see RequiredWindowForRequest.
func Tier1M(id string) string { return id + tierMarker1M }

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
	switch modelID {
	case ModelWairedPeer, ModelWairedPublic:
		return modelID
	case ModelWairedPeerLegacy:
		return ModelWairedPeer
	case ModelWairedPublicLegacy:
		return ModelWairedPublic
	}
	// Per-peer ids are generated from the live mesh, so they are recognised by
	// prefix rather than enumerated. The whole id travels: the layer that
	// resolves it re-derives the same slug from the same snapshot, and
	// carrying the id rather than a parsed slug keeps that one comparison in
	// one place (waired-agent#830). A session still on the pre-#1185 spelling
	// carries the same slug, so it maps onto the current id rather than
	// needing a second resolver.
	if slug, ok := strings.CutPrefix(modelID, ModelWairedPeerPrefix); ok && slug != "" {
		return modelID
	}
	if slug, ok := strings.CutPrefix(modelID, ModelWairedPeerPrefixLegacy); ok && slug != "" {
		return ModelWairedPeerPrefix + slug
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
// Code strips "[1m]" on the wire, a 1M spelling reaches us bare and this
// function cannot see the tier — RequiredWindowForRequest reads it off the
// beta header instead. Both remain: an id that still carries the marker
// (another client, a replayed capture) is answered here.
func RequiredWindowFor(modelID string) int {
	if !isWairedDirective(modelID) {
		return 0
	}
	if strings.Contains(strings.ToLower(modelID), tierMarker1M) {
		return hostfit.ServingWindow1M
	}
	// A BARE id that names a node makes no promise: naming a node and then
	// demanding a window of it would refuse turns on the very machine the
	// operator chose. Only the any-node row promises a floor on its own,
	// because there the operator named no machine and Waired is choosing.
	if isAnyNodeDirective(modelID) {
		return hostfit.ServingWindow200k
	}
	return 0
}

// isAnyNodeDirective reports whether the id is the any-node row, in any
// spelling.
func isAnyNodeDirective(modelID string) bool {
	switch NormalizeModelID(modelID) {
	case ModelWairedAny, ModelWairedAnyLegacy, ModelWairedAnyOldest:
		return true
	}
	return false
}

// isWairedDirective reports whether the id is one of the reserved /model ids,
// in any spelling this build recognises.
func isWairedDirective(modelID string) bool {
	bare := NormalizeModelID(modelID)
	switch bare {
	case ModelWairedAny, ModelWairedLocal, ModelWairedPeer, ModelWairedPublic,
		ModelWairedAnyLegacy, ModelWairedAnyOldest, ModelWairedLocalLegacy,
		ModelWairedPeerLegacy, ModelWairedPublicLegacy:
		return true
	}
	for _, prefix := range []string{ModelWairedPeerPrefix, ModelWairedPeerPrefixLegacy} {
		if slug, ok := strings.CutPrefix(bare, prefix); ok && slug != "" {
			return true
		}
	}
	return false
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
// The header widens the demand and never narrows it: a client that asks for
// 1M gets a serving node that declares 1M, or the turn ends saying no node
// does. There is no longer a crossing to Anthropic to absorb the difference
// (waired-agent#1184), so the tier is fail-closed like every other Waired
// promise.
//
// Since waired-agent#1185 this applies to the ids that NAME a node too, not
// only the any-node one. It used to be reserved for the any-node id, on the
// reasoning that naming a node must not also make demands of it. A 1M twin is
// a different row from the bare one, and the operator only ever sees it on a
// host where that side declares 1M (owner ruling 2026-09-06) — so choosing
// "Waired local (1M context)" IS the demand, and serving it at 200k instead
// would be the surprise.
func RequiredWindowForRequest(modelID string, betaHeader []string) int {
	want := RequiredWindowFor(modelID)
	if want >= hostfit.ServingWindow1M {
		return want
	}
	// Only a Waired id can carry the tier. A real Anthropic model sent with
	// the same beta header is on its way to Anthropic, which sizes it.
	if !isWairedDirective(modelID) {
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
