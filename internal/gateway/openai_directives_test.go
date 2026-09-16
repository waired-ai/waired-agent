package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// openAIModel mirrors the fields handleOpenAIModels writes. Declared here
// rather than shared with the handler's own anonymous struct on purpose: a
// test that decoded the producer's type could not catch a renamed JSON key,
// and these keys are what two coding-tool plugins read.
type openAIModel struct {
	ID             string `json:"id"`
	OwnedBy        string `json:"owned_by"`
	MaxInputTokens int    `json:"max_input_tokens"`
	WairedRoute    bool   `json:"waired_route"`
	DisplayName    string `json:"display_name"`
	Description    string `json:"description"`
}

// openAIModels drives GET /v1/models and returns the listing in order, plus a
// lookup by id.
func openAIModels(t *testing.T, deps Deps) ([]openAIModel, map[string]openAIModel) {
	t.Helper()
	if deps.Runtimes == nil {
		deps.Runtimes = runtime.NewRegistry()
	}
	if deps.ListManifests == nil {
		deps.ListManifests = asManifestList([]catalog.Manifest{{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B"}})
	}
	deps.AllowOpenAI = true
	if deps.HTTPClient == nil {
		deps.HTTPClient = http.DefaultClient
	}
	h := NewHandlerSet(deps)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := make(map[string]openAIModel, len(env.Data))
	for _, m := range env.Data {
		byID[m.ID] = m
	}
	return env.Data, byID
}

// PIN: product contract — the owner asked for the same choices in OpenCode's
// and OpenClaw's pickers that Claude Code's /model has (rc6 review,
// waired-ai/waired#1349, tracked as waired-agent#1306). The shape of the
// listing (which keys carry the label and the marker) is a record of today's
// behaviour; the plugins those keys feed are in this repo.
func TestOpenAIModels_ListsTheRouteRows(t *testing.T) {
	rows := modelrows.Rows(modelrows.Facts{
		LocalServes: true,
		LocalWindow: 131072,
		PeerLimit:   5,
		Peers: []claudecode.PeerFact{
			{DisplayID: "linux-gpu", Key: "dev-1", Model: "qwen3.5-35b-a3b", ContextWindow: 200704},
			{DisplayID: "big-box", Key: "dev-2", Model: "deepseek-v4-flash", Window1M: true, ContextWindow: hostfit.ServingWindow1M},
			{DisplayID: "quiet-box", Key: "dev-3", Model: "gpt-oss-20b"},
		},
		PeerWindow1M: true,
	})
	data, byID := openAIModels(t, Deps{
		RouteDirectives:    true,
		RouteDirectiveRows: func() []modelrows.Row { return rows },
		ContextWindowFor:   func(string) int { return 65536 },
	})

	// The any-node row is spelled waired/default here: OpenCode and OpenClaw
	// both address a model as <provider>/<model>, so a bare "waired" has no
	// second segment to be.
	if _, bare := byID[claudecode.DirectiveModelAny]; bare {
		t.Errorf("the bare any-node id is offered; it cannot be addressed by these clients")
	}
	any, ok := byID[router.DefaultModelAlias]
	if !ok {
		t.Fatalf("no %q row: %+v", router.DefaultModelAlias, data)
	}
	if !any.WairedRoute || any.DisplayName != "Waired" || any.Description != "Any of your computers" {
		t.Errorf("the any-node row lost its marker or its label: %+v", any)
	}

	local := byID[claudecode.DirectiveModelLocal]
	if !local.WairedRoute || local.DisplayName != "Waired local" {
		t.Errorf("local row = %+v", local)
	}
	// Every row states the session it is — 200704, or 1048576 for a twin —
	// never a computer's own window, this one's or the one it names
	// (waired-agent#1396). The rows naming one computer used to state that
	// computer's (waired-agent#1001, #1395).
	if local.MaxInputTokens != hostfit.ServingWindow200k {
		t.Errorf("local row window = %d, want 200704", local.MaxInputTokens)
	}
	if peer := byID["waired/peer-linux-gpu"]; peer.MaxInputTokens != hostfit.ServingWindow200k || !peer.WairedRoute {
		t.Errorf("peer row = %+v, want 200704 and the marker", peer)
	}
	if big := byID["waired/peer-big-box"]; big.MaxInputTokens != hostfit.ServingWindow200k {
		t.Errorf("a 1M computer's own row = %+v, want 200704: its twin is the 1M session", big)
	}
	if any.MaxInputTokens != hostfit.ServingWindow200k {
		t.Errorf("any-node window = %d, want the 200k floor it routes by", any.MaxInputTokens)
	}
	if p := byID[claudecode.DirectiveModelPeer]; p.MaxInputTokens != hostfit.ServingWindow200k {
		t.Errorf("peer row window = %d, want the 200k floor", p.MaxInputTokens)
	}
	if q, ok := byID["waired/peer-quiet-box"]; !ok || q.MaxInputTokens != hostfit.ServingWindow200k {
		t.Errorf("row = %+v (listed %v), want 200704", q, ok)
	}

	// The same twins Claude Code's /model shows, each right after its row, and
	// the any-node one spelled like its row.
	for _, tc := range []struct{ row, twin string }{
		{router.DefaultModelAlias, claudecode.Tier1M(router.DefaultModelAlias)},
		{claudecode.DirectiveModelPeer, claudecode.Tier1M(claudecode.DirectiveModelPeer)},
		{"waired/peer-big-box", claudecode.Tier1M("waired/peer-big-box")},
	} {
		twin, ok := byID[tc.twin]
		if !ok {
			t.Errorf("no twin %q", tc.twin)
			continue
		}
		if !twin.WairedRoute || twin.MaxInputTokens != hostfit.ServingWindow1M {
			t.Errorf("twin %q = %+v, want a route row stating 1M", tc.twin, twin)
		}
		for i := range data {
			if data[i].ID == tc.row && (i+1 >= len(data) || data[i+1].ID != tc.twin) {
				t.Errorf("twin %q does not follow %q", tc.twin, tc.row)
			}
		}
	}
	if _, bare := byID[claudecode.Tier1M(claudecode.DirectiveModelAny)]; bare {
		t.Errorf("the bare any-node twin is offered; it cannot be addressed by these clients")
	}
	for _, none := range []string{claudecode.Tier1M(claudecode.DirectiveModelLocal), claudecode.Tier1M("waired/peer-linux-gpu")} {
		if _, ok := byID[none]; ok {
			t.Errorf("twin %q offered for a side that declares no 1M window", none)
		}
	}

	// The route rows come first, so a picker built from this listing shows
	// the choices about where a turn runs above the catalog it could run on.
	routeRows := 0
	for routeRows < len(data) && data[routeRows].WairedRoute {
		routeRows++
	}
	if routeRows != len(rows) {
		t.Errorf("%d route rows at the head of the listing, want %d: %+v", routeRows, len(rows), data)
	}
	for _, m := range data[routeRows:] {
		if m.WairedRoute {
			t.Errorf("a route row after the catalog began: %+v", m)
		}
	}
}

// The overlay is the SERVING side of a mesh leg. Honouring "send this to a
// peer" there would forward a peer's turn to a third computer, which is the
// loop PeerAdapterFactory is left nil to prevent — so it must neither offer
// the rows nor act on one.
func TestOpenAIModels_OverlayOffersNoRouteRows(t *testing.T) {
	_, byID := openAIModels(t, Deps{
		RouteDirectives:  false,
		ContextWindowFor: func(string) int { return 65536 },
	})
	for _, id := range []string{claudecode.DirectiveModelAny, claudecode.DirectiveModelLocal, claudecode.DirectiveModelPeer, claudecode.DirectiveModelPublic} {
		if m, ok := byID[id]; ok {
			t.Errorf("overlay listing offers %q: %+v", id, m)
		}
	}
	// waired/default is still listed — it is a catalog alias and predates the
	// route rows — but not as one of them.
	if m := byID[router.DefaultModelAlias]; m.WairedRoute {
		t.Errorf("waired/default is marked as a route row on the overlay: %+v", m)
	}
}

// With no hook wired the fixed table is still offered: the four ids route on
// any host, and a host with no peers gets a peer row that fails with a reason
// rather than a menu that silently lacks the choice.
func TestOpenAIModels_NoHookStillOffersTheFixedTable(t *testing.T) {
	_, byID := openAIModels(t, Deps{RouteDirectives: true})
	for _, id := range []string{router.DefaultModelAlias, claudecode.DirectiveModelLocal, claudecode.DirectiveModelPeer} {
		if !byID[id].WairedRoute {
			t.Errorf("%q is not offered as a route row without the hook: %+v", id, byID[id])
		}
	}
	// The public row needs a fact this listener does not have.
	if _, ok := byID[claudecode.DirectiveModelPublic]; ok {
		t.Errorf("the public row was offered without knowing the Public Share posture")
	}
}

// PIN: product contract — a turn addressed to a Waired row runs on the
// computer that row names, or fails saying so
// (docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md).
// The mapping to DefaultModelAlias is a record of today's behaviour: the id
// names a node, and the node still needs a model to run.
func TestApplyRouteDirective(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model         string
		wantHandled   bool
		wantDirective string
		wantModel     string
		wantWindow    int
	}{
		// The same floors the Claude listener routes by: 200k on every row,
		// 1M on every twin (owner decision 2026-09-16, waired-agent#1396).
		{"the any-node row demands the 200k floor", claudecode.DirectiveModelAny, true, "", router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"the local row names this computer and demands the floor", claudecode.DirectiveModelLocal, true, claudecode.DirectiveModelLocal, router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"the peer row names another and demands the floor", claudecode.DirectiveModelPeer, true, claudecode.DirectiveModelPeer, router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"a per-peer row names one and demands the floor", "waired/peer-linux-gpu", true, "waired/peer-linux-gpu", router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"the public row names someone else's and demands the floor", claudecode.DirectiveModelPublic, true, claudecode.DirectiveModelPublic, router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"a pre-#1185 spelling still routes", "claude-waired-peer", true, claudecode.DirectiveModelPeer, router.DefaultModelAlias, hostfit.ServingWindow200k},
		{"a tier spelled into the id is a window demand", claudecode.Tier1M(claudecode.DirectiveModelAny), true, "", router.DefaultModelAlias, hostfit.ServingWindow1M},
		{"a per-peer twin demands 1M", claudecode.Tier1M("waired/peer-linux-gpu"), true, "waired/peer-linux-gpu", router.DefaultModelAlias, hostfit.ServingWindow1M},
		// The listing's spelling of the any-node twin, which a plugin from
		// before the wire-id mapping sends as it is.
		{"the listed any-node twin routes as the twin", claudecode.Tier1M(router.DefaultModelAlias), true, "", router.DefaultModelAlias, hostfit.ServingWindow1M},
		{"in any case", "WAIRED/DEFAULT[1M]", true, "", router.DefaultModelAlias, hostfit.ServingWindow1M},
		// The catalog alias is not a directive: chat apps and `waired infer`
		// send it, and they never picked a row that promises a window.
		{"the default alias is left alone", router.DefaultModelAlias, false, "", router.DefaultModelAlias, 0},
		// A typo'd model name is a typo. Mapping every catalog miss to the
		// host's default — which is what the Anthropic leg does, because
		// every id it sees is an Anthropic one — would serve a model nobody
		// asked for and swallow the 404 that says so.
		{"an unknown model keeps its own name", "qwen3.5-9b-typo", false, "", "qwen3.5-9b-typo", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandlerSet(Deps{RouteDirectives: true, Runtimes: runtime.NewRegistry(), ListManifests: asManifestList(nil)})
			req := router.Request{Model: tc.model}
			if got := h.applyRouteDirective(&req); got != tc.wantHandled {
				t.Fatalf("applyRouteDirective = %v, want %v", got, tc.wantHandled)
			}
			if req.NodeDirective != tc.wantDirective {
				t.Errorf("NodeDirective = %q, want %q", req.NodeDirective, tc.wantDirective)
			}
			if req.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", req.Model, tc.wantModel)
			}
			if req.MinContextWindow != tc.wantWindow {
				t.Errorf("MinContextWindow = %d, want %d", req.MinContextWindow, tc.wantWindow)
			}
		})
	}
}

// The overlay must not act on a directive even when a peer sends one.
func TestApplyRouteDirective_OffOnTheOverlay(t *testing.T) {
	h := NewHandlerSet(Deps{RouteDirectives: false, Runtimes: runtime.NewRegistry(), ListManifests: asManifestList(nil)})
	req := router.Request{Model: claudecode.DirectiveModelPeer}
	if h.applyRouteDirective(&req) {
		t.Fatal("the overlay acted on a route directive")
	}
	if req.NodeDirective != "" || req.Model != claudecode.DirectiveModelPeer {
		t.Errorf("request was rewritten: %+v", req)
	}
}
