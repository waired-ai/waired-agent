package gateway

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// The handler re-spells one id before listing the rows, and the rows come from
// a hook the agent owns. Today's hook builds a fresh slice per call, so an
// in-place edit is invisible; a hook that cached one would find its own table
// rewritten after the first request, and the any-node row would then be
// missing from the Claude picker for the life of the process.
//
// A record of today's behaviour, kept because the failure it guards against
// would look like a bug in a different package.
func TestRouteDirectiveRowsDoesNotEditTheHooksSlice(t *testing.T) {
	cached := []modelrows.Row{
		{DirectiveModel: claudecode.DirectiveModel{ID: claudecode.DirectiveModelAny, DisplayName: "Waired"}},
		{DirectiveModel: claudecode.DirectiveModel{ID: claudecode.DirectiveModelLocal, DisplayName: "Waired local"}},
	}
	h := NewHandlerSet(Deps{
		Runtimes:           runtime.NewRegistry(),
		ListManifests:      asManifestList(nil),
		RouteDirectives:    true,
		RouteDirectiveRows: func() []modelrows.Row { return cached },
	})

	first := h.routeDirectiveRows()
	if first[0].ID != router.DefaultModelAlias {
		t.Fatalf("first call did not re-spell the any-node row: %q", first[0].ID)
	}
	if cached[0].ID != claudecode.DirectiveModelAny {
		t.Fatalf("the hook's own slice was edited: %q", cached[0].ID)
	}
	second := h.routeDirectiveRows()
	if second[0].ID != router.DefaultModelAlias {
		t.Errorf("second call = %q, want the same answer as the first", second[0].ID)
	}
}
