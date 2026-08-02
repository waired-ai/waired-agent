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

// Directive ids. Duplicated from gateway.ModelWaired{Auto,Local,Cloud} — see
// the file comment.
const (
	// DirectiveModelAuto routes Waired-first with an Anthropic fallback. Does
	// not start with "claude-", so CLAUDE_CODE_MAX_CONTEXT_TOKENS sizes it.
	DirectiveModelAuto = "anthropic-waired-auto"
	// DirectiveModelLocal pins the conversation to this device's inference.
	// Also non-"claude-", and the id whose real window that env var carries
	// (#408).
	DirectiveModelLocal = "anthropic-waired-local"
	// DirectiveModelCloud pins to the real Anthropic API. The "[1m]" suffix is
	// what gives it Claude Code's 1M window, and outranks the env var above.
	DirectiveModelCloud = "claude-waired-cloud[1m]"
)

// DirectiveModels returns the picker entries in the order the gateway
// advertises them, which is the order they appear in /model.
func DirectiveModels() []DirectiveModel {
	return []DirectiveModel{
		{ID: DirectiveModelAuto, DisplayName: "Waired auto (local, fallback to Anthropic)"},
		{ID: DirectiveModelLocal, DisplayName: "Waired local (this device)"},
		{ID: DirectiveModelCloud, DisplayName: "Waired cloud (Anthropic API)"},
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
