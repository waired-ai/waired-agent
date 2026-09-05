package claudecode

// The reserved /model ids, duplicated from internal/gateway so the `waired`
// CLI can name them without linking the router and the inference stack. The
// gateway is the anchor; internal/gateway/anthropic_models.go carries the
// reasoning and gateway.TestDirectiveTablesMatchTheCLICopy pins the two
// together.
//
// The ids are spelled `waired`, `waired/local`, `waired/peer`,
// `waired/peer-<node>` and `waired/public` since waired-agent#1185. They used
// to carry a `claude-` or `anthropic-` head, which was never a claim about
// Anthropic: Claude Code's gateway discovery keeps only ids containing
// "claude" or "anthropic", so an id that did not carry one never reached the
// picker at all. The rows come from the documented `modelPicker` setting now
// (modelpicker.go), which applies no such filter, so the head has no work left
// to do and is gone.
//
// The one thing the head also decided is documented on DirectiveModelLocal.

// DirectiveModel is one row of the picker: the id a turn carries, and the two
// lines the row shows.
type DirectiveModel struct {
	ID          string
	DisplayName string
	// Description is the picker's second line. A row without one reads
	// "Custom model (<id>)" instead (measured on Claude Code 2.1.261,
	// 2026-09-06); the private cache this replaced had no description field
	// at all, which is why the per-peer rows used to fold the model name
	// into the label.
	Description string
}

// Directive ids.
const (
	// DirectiveModelAny names any Waired node — this computer or a peer,
	// whichever the mesh offers. Waired picks; when no node can answer, the
	// turn ends saying so rather than crossing to Anthropic
	// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
	DirectiveModelAny = "waired"
	// DirectiveModelLocal pins the conversation to this computer's engine.
	//
	// None of these ids starts with "claude-", and that is load-bearing
	// rather than cosmetic: CLAUDE_CODE_MAX_CONTEXT_TOKENS — which managed
	// settings sets to this computer's real window — is honoured only for
	// ids that do NOT start with "claude-" (measured on 2.1.261; the
	// predicate is `!id.startsWith("claude-")`). Under the old spelling
	// only the local row got its real window and every other row silently
	// took Claude Code's assumed 200k, with a notice on screen saying the
	// id "isn't described by this version's model catalog".
	DirectiveModelLocal = "waired/local"
	// DirectiveModelPeer restricts the conversation to another of your
	// computers; this one does not take over for it. Fail-closed, like
	// every Waired id: when no peer can answer, the turn says so.
	DirectiveModelPeer = "waired/peer"
	// DirectiveModelPublic restricts the conversation to a Public Share
	// machine — someone else's computer, lent through Waired
	// (waired-agent#901). Offered only on a host that has enabled Public
	// Share.
	DirectiveModelPublic = "waired/public"
)

// TierMarker1M is the suffix Claude Code sizes a session from. A row spelled
// "<id>[1m]" runs in a 1M-token session and sends `anthropic-beta:
// context-1m-*`; Claude Code strips the marker from the id before sending, so
// the beta header is the only place the tier survives the trip (measured on
// 2.1.261, 2026-09-06 — waired-agent#1036 measured the stripping first).
//
// Every row whose side can declare a 1M window gets a twin carrying it
// (owner ruling 2026-09-06). A twin is offered only when a node actually
// declares 1M: the tier is a promise about the SERVING node, so a twin with
// nothing behind it would be a menu entry whose selection fails.
const TierMarker1M = "[1m]"

// Tier1M spells the 1M twin of a directive id.
func Tier1M(id string) string { return id + TierMarker1M }

// Legacy ids. No surface offers them; every layer still routes them, because
// a session that selected one keeps it in its own settings until the operator
// picks again, and `~/.claude/settings.json` can carry one as a default
// model that a much older waired wrote there.
const (
	// LegacyModelAuto and LegacyModelAuto1M are the pre-#1185 spellings of
	// DirectiveModelAny. "auto" named a route that no longer exists — it
	// used to mean "Waired first, then Anthropic" — and kept the spelling
	// through waired-agent#1184 only because sessions were carrying it.
	LegacyModelAuto   = "claude-waired-auto"
	LegacyModelAuto1M = "claude-waired-auto[1m]"
	// LegacyModelAutoLegacy is the pre-waired#1031 spelling of the same row.
	LegacyModelAutoLegacy = "anthropic-waired-auto"
	// LegacyModelLocal, LegacyModelPeer and LegacyModelPublic are the
	// pre-#1185 spellings of the named rows.
	LegacyModelLocal  = "anthropic-waired-local"
	LegacyModelPeer   = "claude-waired-peer"
	LegacyModelPublic = "claude-waired-public"
	// LegacyPeerDirectivePrefix heads the pre-#1185 per-peer ids.
	LegacyPeerDirectivePrefix = LegacyModelPeer + "-"
	// LegacyModelCloud pinned the conversation to the real Anthropic API.
	// Retired by waired-agent#1037: picking a real Anthropic model in
	// /model reaches the real API on its own, and says which model answered
	// besides. Still recognised so the fail-closed refusal can name the fix.
	LegacyModelCloud = "claude-waired-cloud[1m]"
)

// DirectiveModels returns the fixed picker rows in the order they appear in
// /model. The 1M twins and the per-peer rows are added by the caller, which
// is the only layer that knows which sides declare 1M and which computers are
// serving right now.
func DirectiveModels() []DirectiveModel {
	return []DirectiveModel{
		{ID: DirectiveModelAny, DisplayName: "Waired",
			Description: "Any of your computers"},
		{ID: DirectiveModelLocal, DisplayName: "Waired local",
			Description: "This computer"},
		{ID: DirectiveModelPeer, DisplayName: "Waired peer",
			Description: "Another of your computers"},
		{ID: DirectiveModelPublic, DisplayName: "Waired public share",
			Description: "Someone else's computer"},
	}
}

// Tier1MModel returns the 1M twin of a fixed row.
func Tier1MModel(d DirectiveModel) DirectiveModel {
	return DirectiveModel{
		ID:          Tier1M(d.ID),
		DisplayName: d.DisplayName + " (1M context)",
		Description: d.Description,
	}
}
