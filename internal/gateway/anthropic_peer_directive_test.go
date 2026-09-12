package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	runtime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The "Waired peer" /model entry names a NODE, and the node has to survive
// the model remap that happens two lines later.
//
// An Anthropic id is not in the catalog, so the first selection returns
// ErrModelNotFound and ResolveUnknownModel overwrites routeReq.Model with the
// default alias before the retry. Any per-request node choice derived from
// Model at the selector is therefore correct on the first attempt and gone on
// the second — which is every real request. Carrying it in its own field is
// the whole reason Request.NodeDirective exists (waired-agent#830), and this
// is the test that would fail if someone "simplified" it back.
//
// PIN: product contract — waired-agent#830. The remap behaviour it rides on
// is docs/decisions/20260819/1900-routing-selects-a-node-not-a-model.md §4.
func TestAnthropicMessages_PeerDirectiveSurvivesTheModelRemap(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	// The resolver returns router.DefaultModelAlias, exactly as the agent does
	// on this surface since waired-agent#828, so the retry here is the one
	// production performs.
	sel := &modelAwareSelector{known: map[string]router.Selection{
		router.DefaultModelAlias: {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: upstream.URL})
	gw := NewServer(ServerConfig{}, Deps{
		Selector:              sel,
		Runtimes:              reg,
		ListManifests:         asManifestList(nil),
		HTTPClient:            http.DefaultClient,
		AllowAnthropic:        true,
		ClaudeModelDirectives: true,
		ResolveUnknownModel: func(string, string) (string, bool) {
			return router.DefaultModelAlias, true
		},
	})

	body := `{"model":"` + ModelWairedPeer + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sel.got) != 2 {
		t.Fatalf("selector saw %d requests, want 2 (the miss and the retry): %+v", len(sel.got), sel.got)
	}
	if sel.got[0].Model != ModelWairedPeer {
		t.Errorf("first attempt Model = %q, want the client's own id", sel.got[0].Model)
	}
	// The retry is where it used to be lost.
	if sel.got[1].Model == ModelWairedPeer {
		t.Fatalf("the retry did not remap the model; this test no longer exercises the remap")
	}
	for i, r := range sel.got {
		if r.NodeDirective != ModelWairedPeer {
			t.Errorf("attempt %d lost the node directive: NodeDirective = %q, Model = %q",
				i, r.NodeDirective, r.Model)
		}
	}
}

// A tier promise and a node choice are different questions about the same
// id, so an id that names a node makes no window demand: naming a node and
// then refusing it for its window would refuse the very machine the operator
// chose.
//
// PIN: product contract. The rows name where the turn runs
// (docs/decisions/20260828/0252-the-model-you-pick-is-where-the-turn-runs.md),
// and the window rule is the owner ruling of 2026-08-20
// (docs/decisions/20260820/0200-model-picker-can-name-a-node.md §4).
//
// This test used to assert that ModelWairedLocal names NO node, which was the
// defect waired-agent#1320: the local id carried no directive, an empty
// directive falls through to the operator's `waired worker` setting, and the
// row that says "This computer" was answered by a peer on a machine set to
// peer-only. The ids that still name no node are the any-node row (Waired
// chooses, which is what the row means) and the retired cloud id.
func TestPeerDirectiveMakesNoWindowDemand(t *testing.T) {
	for _, id := range []string{ModelWairedPeer, ModelWairedLocal, ModelWairedPublic} {
		if got := RequiredWindowFor(id); got != 0 {
			t.Errorf("RequiredWindowFor(%q) = %d, want 0", id, got)
		}
		if got := NodeDirectiveFor(id); got != id {
			t.Errorf("NodeDirectiveFor(%q) = %q, want the id itself", id, got)
		}
	}
	for _, id := range []string{ModelWairedAny, Tier1M(ModelWairedAny), ModelWairedCloud, "claude-sonnet-5"} {
		if got := NodeDirectiveFor(id); got != "" {
			t.Errorf("NodeDirectiveFor(%q) = %q, want \"\" — these name no node", id, got)
		}
	}
	// A session that selected the pre-waired-agent#1185 spelling keeps
	// sending it, and it has to reach the same node.
	if got := NodeDirectiveFor(ModelWairedLocalLegacy); got != ModelWairedLocal {
		t.Errorf("NodeDirectiveFor(%q) = %q, want %q", ModelWairedLocalLegacy, got, ModelWairedLocal)
	}
}

// With directives off, a deployment must not grow a routing behaviour it
// never opted into — the same gate that keeps the ids out of discovery.
func TestPeerDirectiveIgnoredWhenDirectivesOff(t *testing.T) {
	var captured string
	upstream := fakeOllamaForAnthropic(t, &captured)
	defer upstream.Close()

	// The resolver returns router.DefaultModelAlias, exactly as the agent does
	// on this surface since waired-agent#828, so the retry here is the one
	// production performs.
	sel := &modelAwareSelector{known: map[string]router.Selection{
		router.DefaultModelAlias: {Runtime: "ollama", EngineModel: "qwen3:8b-q4_K_M", ModelID: "qwen3-8b-instruct"},
	}}
	gw := anthropicGatewayWithResolver(t, sel, upstream.URL, func(string) (string, bool) {
		return router.DefaultModelAlias, true
	})

	body := `{"model":"` + ModelWairedPeer + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sel.got) == 0 {
		t.Fatal("the selector saw nothing — this test would pass vacuously")
	}

	for i, r := range sel.got {
		if r.NodeDirective != "" {
			t.Errorf("attempt %d carries a node directive with the feature off: %q", i, r.NodeDirective)
		}
	}
}
