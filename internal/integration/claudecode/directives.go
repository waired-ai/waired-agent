package claudecode

// Reserved /model route-directive ids (#52) as the picker sees them: the id
// plus the label Claude Code renders. The gateway advertises exactly these on
// /v1/models (internal/gateway/anthropic_models.go), and since #407 the agent
// writes them straight into the picker cache, because discovery never runs
// under subscription OAuth to fetch them.
//
// The literals are duplicated from the gateway rather than imported so the
// `waired` CLI does not link the gateway package — and through it the router
// and the whole inference stack — for three strings. internal/proxy/intercept
// makes the same trade for the same reason (model_rewrite.go), and pins it the
// same way: DirectiveModels is asserted equal to the gateway's list in
// directive_sync_test.go. Nothing else stops the two drifting, and drift here
// is invisible — the picker would simply offer an id the gateway no longer
// advertises, or omit one it does.
//
// Claude Code filters discovered ids to ^(claude|anthropic)/i, which every id
// below satisfies deliberately. Raw catalog ids (qwen…) never survive that
// filter, which is why the branded directive ids exist at all.
//
// Display names are user-visible copy, rendered in the /model picker.

// DirectiveModel is one reserved id and its picker label. It stays a distinct
// type from GatewayCacheModel — this is the gateway's advertisement mirrored,
// that one is the on-disk format of somebody else's client — and the conversion
// in DirectiveCacheModels is what keeps them honest: the day the cache format
// needs another field, that line stops compiling instead of quietly writing a
// document the reader rejects.
type DirectiveModel struct {
	ID          string
	DisplayName string
}

// Directive ids. Duplicated from gateway.ModelWaired{Auto,Auto1M,Local,Cloud}
// — see the file comment.
const (
	// DirectiveModelAuto and DirectiveModelAuto1M route Waired-first with an
	// Anthropic fallback, at the 200k and 1M tiers. Both start with
	// "claude-", so Claude Code sizes them from the id alone — the bare id
	// takes its 200k default, the "[1m]" suffix takes 1M — and neither
	// consults CLAUDE_CODE_MAX_CONTEXT_TOKENS. Waired serves the turn only
	// when a node declares that window; otherwise it goes to Anthropic
	// (waired#1031).
	DirectiveModelAuto   = "claude-waired-auto"
	DirectiveModelAuto1M = "claude-waired-auto[1m]"
	// DirectiveModelLocal pins the conversation to this device's inference.
	// The one deliberately non-"claude-" id, and so the only one whose
	// window comes from CLAUDE_CODE_MAX_CONTEXT_TOKENS (#408) — which is
	// what lets it report a window that is neither 200k nor 1M. Pinning is
	// how you reach a device that declares no tier at all.
	DirectiveModelLocal = "anthropic-waired-local"
	// DirectiveModelPeer restricts the conversation to another computer on
	// the mesh and never falls back to this one. "claude-" prefixed, so it
	// takes Claude Code's 200k default rather than this device's window
	// out of CLAUDE_CODE_MAX_CONTEXT_TOKENS — which is a single global and
	// the wrong number for any peer. See gateway.ModelWairedPeer.
	DirectiveModelPeer = "claude-waired-peer"
	// DirectiveModelPublic restricts the conversation to a Public Share
	// machine — someone else's computer (waired-agent#901). Advertised only
	// on a host that has enabled Public Share; see the picker-cache writer.
	DirectiveModelPublic = "claude-waired-public"
	// DirectiveModelCloud pins to the real Anthropic API. The "[1m]" suffix is
	// what gives it Claude Code's 1M window, and outranks the env var above.
	DirectiveModelCloud = "claude-waired-cloud[1m]"
)

// DirectiveModels returns the picker entries in the order the gateway
// advertises them, which is the order they appear in /model.
func DirectiveModels() []DirectiveModel {
	return []DirectiveModel{
		{ID: DirectiveModelAuto, DisplayName: "Waired auto — 200k (local, fallback to Anthropic)"},
		{ID: DirectiveModelAuto1M, DisplayName: "Waired auto — 1M (local, fallback to Anthropic)"},
		{ID: DirectiveModelLocal, DisplayName: "Waired local (this device)"},
		{ID: DirectiveModelPeer, DisplayName: "Waired peer (another device, no local fallback)"},
		{ID: DirectiveModelPublic, DisplayName: "Waired public share (someone else's computer)"},
		// DirectiveModelCloud is NOT offered any more: picking a real Anthropic
		// model in /model routes to the real Anthropic API on its own
		// (waired-agent#1037), and says which model answers besides. The id is
		// still routed by the intercept, for the sessions that already hold it.
	}
}

// DirectiveCacheModels is DirectiveModels projected onto the picker cache's
// wire shape.
func DirectiveCacheModels() []GatewayCacheModel {
	src := DirectiveModels()
	out := make([]GatewayCacheModel, 0, len(src))
	for _, m := range src {
		out = append(out, GatewayCacheModel(m))
	}
	return out
}
