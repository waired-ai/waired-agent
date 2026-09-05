package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// errDetectFailed stands in for any DetectEffectiveStatusLine failure.
var errDetectFailed = errors.New("detect failed")

// plainStatusline forces deterministic ASCII (no color, no emoji) so the
// rendered segment can be asserted exactly.
func plainStatusline(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	t.Setenv("WAIRED_NO_EMOJI", "1")
}

func routing(opts ...func(*management.ClaudeRoutingState)) management.ClaudeRoutingState {
	var st management.ClaudeRoutingState
	for _, o := range opts {
		o(&st)
	}
	return st
}

func withModel(m string) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.LastLocalModel = m }
}

// TestRenderStatusline pins what the footer says about the side a session is
// on. There is no machine-wide route left to read: the model id Claude Code
// hands in on stdin is the whole input, and a Waired session that nothing can
// answer now says so rather than announcing a fallback
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md,
// owner ruling waired-ai/waired#1313).
func TestRenderStatusline(t *testing.T) {
	plainStatusline(t)
	cases := []struct {
		name    string
		route   management.ClaudeRoutingState
		health  string
		session string
		want    string
	}{
		{"on waired", routing(), "ready", "", "waired: on Waired"},
		{"anthropic model", routing(), "ready", "claude-opus-5", "-> waired: Anthropic"},
		{"nothing can answer", routing(), "degraded", "", "! waired: Waired cannot answer (local degraded)"},
		{"no engine", routing(), "no_engine", "", "! waired: Waired cannot answer (local no_engine)"},
		{"unreadable health counts as ready", routing(), "", "", "waired: on Waired"},
		// #602: the last locally-served model id is appended while serving on
		// Waired, and hidden on every branch that is not.
		{"on waired with the model", routing(withModel("qwen3-8b-instruct")), "ready", "",
			"waired: on Waired (qwen3-8b-instruct)"},
		{"a stopped engine hides the model", routing(withModel("qwen3-8b-instruct")), "no_engine", "",
			"! waired: Waired cannot answer (local no_engine)"},
		{"an Anthropic session hides the model", routing(withModel("qwen3-8b-instruct")), "ready", "claude-opus-5",
			"-> waired: Anthropic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, nil, meshView{}, tc.session); got != tc.want {
				t.Errorf("renderStatusline = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderStatusline_PeersAreATarget is the waired-agent#1042 regression.
//
// PIN: product contract — waired-agent#829 gave the routing decision both
// axes ("not local AND not reachable in mesh"); this line had only the local
// one, so on the engine-less host whose entire role is to borrow a peer's
// engine it announced `fallback -> Anthropic (local disabled)` before any turn
// had run, while peers were in fact serving that host's turns (47 s and 171 s,
// no fallback recorded).
//
// The first row is that exact reported string. Every other row is a way the
// fix could overreach — claiming a peer where none was seen, or where nobody
// looked.
func TestRenderStatusline_PeersAreATarget(t *testing.T) {
	plainStatusline(t)
	withPeer := meshView{known: true, reachable: true, names: map[string]string{"dev-mag": "sv-mag"}}
	noPeer := meshView{known: true, reachable: false, names: map[string]string{}}
	unread := meshView{}
	servedByPeer := func(id string) func(*management.ClaudeRoutingState) {
		return func(st *management.ClaudeRoutingState) { st.LastServedBy = id }
	}

	no := false
	cases := []struct {
		name  string
		route management.ClaudeRoutingState
		// health is this computer's own subsystem_state; "disabled" is the
		// engine-less host the whole issue is about.
		health string
		mesh   meshView
		// resident is nil ("we did not look") except where the row is about
		// waired-agent#837's residency clause.
		resident *bool
		want     string
	}{
		{
			name:   "the reported case: engine off here, a peer is serving",
			route:  routing(servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// A peer whose turn predates the model being recorded (an agent
			// older than #755's header, or a selection that named no catalog
			// id): the machine still gets named.
			name:   "a named peer with no model recorded",
			route:  routing(servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// Nothing has been served yet, so there is no name to give. The
			// line still must not announce a fallback that will not happen.
			name:   "a peer is there but has not answered yet",
			route:  routing(),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer)",
		},
		{
			name:   "engine off here and no peer either is a real fallback",
			route:  routing(),
			health: "disabled", mesh: noPeer,
			want: "! waired: Waired cannot answer (local disabled, no peer)",
		},
		{
			// The mesh read did not come back. Saying "no peer" would be a
			// claim; the line keeps the wording that shipped.
			name:   "an unread mesh claims nothing",
			route:  routing(),
			health: "disabled", mesh: unread,
			want: "! waired: Waired cannot answer (local disabled)",
		},
		{
			// A local engine that is up answers the turn itself, and the
			// peer clause would be about a machine that is not involved.
			name:   "a healthy local engine is unaffected by the mesh",
			route:  routing(withModel("qwen3-8b-instruct")),
			health: "ready", mesh: withPeer,
			want: "waired: on Waired (qwen3-8b-instruct)",
		},
		{
			// waired-agent#1172. This computer's engine is ready AND a peer
			// answered the last turn — a worker pin, or a peer row picked in
			// `/model`. The footer used to decide the form from this
			// computer's engine health, so it printed the peer's model in
			// the local form: on sv-mag, whose only model is gpt-oss-20b,
			// "on Waired (qwen3.6-35b-a3b)" for a turn a MacBook answered,
			// while `waired claude status` named the peer from the same
			// record.
			name:   "a ready local engine does not make a peer's turn local",
			route:  routing(servedByPeer("dev-mag"), withModel("qwen3-8b-instruct")),
			health: "ready", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag: qwen3-8b-instruct)",
		},
		{
			// The same, on the branch where the name cannot be resolved:
			// still not this computer's answer to claim.
			name:   "a ready local engine does not adopt an unnameable peer's model either",
			route:  routing(servedByPeer("dev-gone"), withModel("qwen3-8b-instruct")),
			health: "ready", mesh: withPeer,
			want: "waired: on Waired (peer)",
		},
		{
			// A turn never leaves for Anthropic, so an engine-less host with
			// a serving peer is not "down" — it is doing exactly what it was
			// set up to do.
			name:   "an engine-less host with a peer is not down",
			route:  routing(servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			name:   "nothing anywhere is what the red row is for",
			route:  routing(),
			health: "disabled", mesh: noPeer,
			want: "! waired: Waired cannot answer (local disabled, no peer)",
		},
		{
			// A peer whose name this device does not have (it left the mesh
			// since it answered) is "a peer", never a raw device id.
			name:   "an unnameable peer is not rendered as an identifier",
			route:  routing(servedByPeer("dev-gone")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer)",
		},
		{
			// waired-agent#837's clause is about THIS computer's weights,
			// and on the peer branch this computer is not answering. Before
			// #1042 this branch did not exist, so nothing kept the clause
			// off it — an engine-less host would have grown a permanent
			// "model not loaded".
			name:   "the residency clause does not follow a peer",
			route:  routing(servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer, resident: &no,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// The same clause on the branch it IS about, so the row above
			// is proving an absence and not just a nil reading.
			name:   "the residency clause still follows this computer",
			route:  routing(),
			health: "ready", mesh: withPeer, resident: &no,
			want: "waired: on Waired - model not loaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, tc.resident, tc.mesh, ""); got != tc.want {
				t.Errorf("renderStatusline = %q, want %q", got, tc.want)
			}
		})
	}
}

// meshViewOf takes the names from the same function every other surface uses,
// so a Public Share machine is its grant pseudonym here too and its real
// device id never reaches the footer (spec §8.5).
func TestMeshViewOf_NamesPeersLikeEverySurface(t *testing.T) {
	snap := &inferencemesh.Snapshot{
		Reachable: true,
		Peers: []inferencemesh.PeerView{
			{DeviceID: "dev-a", DeviceName: "sv-mag"},
			{DeviceID: "dev-b"}, // unnamed: falls back to its id
			{DeviceID: "dev-c", DeviceName: "stranger-workstation",
				Grant: &signer.PeerGrant{ID: "g1", Kind: "public", Role: "provider", Pseudonym: "guest-a7f3"}},
		},
	}
	v := meshViewOf(snap)
	if !v.known || !v.reachable {
		t.Fatalf("view = %+v, want a known reachable mesh", v)
	}
	if got := v.peerName("dev-a"); got != "sv-mag" {
		t.Errorf("peerName(dev-a) = %q, want sv-mag", got)
	}
	if got := v.peerName("dev-b"); got != "dev-b" {
		t.Errorf("peerName(dev-b) = %q, want the device id it has no name for", got)
	}
	if got := v.peerName("dev-c"); got != "guest-a7f3" {
		t.Errorf("peerName(dev-c) = %q, want the grant pseudonym — never the real name or id", got)
	}
	if got := v.peerName(""); got != "" {
		t.Errorf("peerName(\"\") = %q, want empty", got)
	}
}

// TestRenderStatusline_ModelNotLoaded covers waired-agent#837's footer
// clause. Claude Code runs this command at transcript updates — the user's
// own submission included — and the string it produces then stays on screen
// for the whole turn, so a footer that already says "model not loaded" when
// the silence starts has answered "is this hung?" before it was asked.
//
// The three negative cases are the ways it could be wrong rather than merely
// absent, and each is why its condition exists.
func TestRenderStatusline_ModelNotLoaded(t *testing.T) {
	plainStatusline(t)
	no, yes := false, true
	peerServed := routing()
	peerServed.LastServedBy = "peer-a"

	cases := []struct {
		name     string
		route    management.ClaudeRoutingState
		health   string
		resident *bool
		want     string
	}{
		{"not loaded, auto", routing(), "ready", &no,
			"waired: on Waired - model not loaded"},
		{"not loaded, a second reading", routing(), "ready", &no,
			"waired: on Waired - model not loaded"},
		{"loaded says nothing extra", routing(), "ready", &yes,
			"waired: on Waired"},
		{"no claim says nothing extra", routing(), "ready", nil,
			"waired: on Waired"},
		// The clause is absent because a peer answered — and since
		// waired-agent#1172 the line says so, instead of describing the turn
		// as this computer's. The row's subject is the missing clause either
		// way.
		{"a peer answered, so local residency is not this turn's fact",
			peerServed, "ready", &no, "waired: on Waired (peer)"},
		{"a loading engine is already saying something else",
			routing(), "loading", &no,
			"! waired: Waired cannot answer (local loading)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, tc.resident, meshView{}, ""); got != tc.want {
				t.Errorf("renderStatusline = %q, want %q", got, tc.want)
			}
		})
	}
}

// A colourized run must not lose the clause: the segment is wrapped once, at
// the end, so anything appended after that wrap would fall outside it.
func TestRenderStatusline_NotLoadedStaysInsideTheColorWrap(t *testing.T) {
	t.Setenv("WAIRED_NO_EMOJI", "1")
	no := false
	got := renderStatusline(routing(), "ready", &no, meshView{}, "")
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Fatalf("segment not wrapped: %q", got)
	}
	if !strings.Contains(strings.TrimSuffix(got, ansiReset), "model not loaded") {
		t.Errorf("clause fell outside the colour wrap: %q", got)
	}
}

func TestStatuslineDownPlain(t *testing.T) {
	plainStatusline(t)
	if got := statuslineDown(); got != "x waired: agent down" {
		t.Errorf("statuslineDown = %q", got)
	}
}

func TestRenderStatuslineColorized(t *testing.T) {
	t.Setenv("WAIRED_NO_EMOJI", "1") // drop glyphs, keep color
	got := renderStatusline(routing(), "ready", nil, meshView{}, "")
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("expected green-wrapped segment, got %q", got)
	}
}

// TestRenderStatusline_SessionModelDecidesTheSide: the footer describes the
// session it is rendered for. The id arrives on stdin, per render, so two
// sessions on one computer can honestly say different things
// (waired-agent#1037) — and since the routes went it is the only input there
// is (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
func TestRenderStatusline_SessionModelDecidesTheSide(t *testing.T) {
	plainStatusline(t)
	for _, tc := range []struct {
		name  string
		model string
		want  string
		why   string
	}{
		{"picked a real model", "claude-opus-5", "-> waired: Anthropic",
			"naming a model names where it runs"},
		{"picked another real model", "claude-fable-5", "-> waired: Anthropic",
			"whichever model it is"},
		{"picked the any-node Waired row", "claude-waired-auto", "waired: on Waired",
			"the row runs on whichever of your computers can"},
		{"picked the local row", "anthropic-waired-local", "waired: on Waired",
			"this device"},
		{"no payload", "", "waired: on Waired",
			"an unnamed session is served here, which is where it runs"},
		{"an id that names neither side", "waired/subagent", "waired: on Waired",
			"served here, like every id the real API does not serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderStatusline(routing(), "ready", nil, meshView{}, tc.model)
			if got != tc.want {
				t.Errorf("renderStatusline(model=%q) = %q, want %q — %s", tc.model, got, tc.want, tc.why)
			}
		})
	}
}

// TestStatuslineSessionModel: the payload is read best-effort. Anything the
// caller cannot make sense of yields "", and the footer falls back to the
// machine-wide policy rather than rendering something wrong.
func TestStatuslineSessionModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"the payload Claude Code sends",
			`{"model":{"id":"claude-opus-5","display_name":"Opus 5"},"session_id":"x"}`, "claude-opus-5"},
		{"no model key", `{"session_id":"x"}`, ""},
		{"truncated", `{"model":{"id":"claude-`, ""},
		{"not JSON at all", "hello", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statuslineSessionModel(strings.NewReader(tc.in)); got != tc.want {
				t.Errorf("statuslineSessionModel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := statuslineSessionModel(nil); got != "" {
		t.Errorf("statuslineSessionModel(nil) = %q, want empty", got)
	}
}

// routeStub serves the given routing state on the Claude route endpoint and an
// inference status carrying subsystemState.
func routeStub(t *testing.T, st management.ClaudeRoutingState, subsystemState string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/integration/claude/route":
			_ = json.NewEncoder(w).Encode(st)
		case inferenceStatusPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"subsystem_state": subsystemState})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchRouteAndHealth(t *testing.T) {
	srv := routeStub(t, routing(withModel("qwen3-8b-instruct")), "degraded")
	route, health, _, _, ok := fetchRouteAndHealth(srv.URL)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if route.LastLocalModel != "qwen3-8b-instruct" {
		t.Errorf("last local model = %q", route.LastLocalModel)
	}
	if health != "degraded" {
		t.Errorf("health = %q", health)
	}
}

// The mesh is read in the same budget as the other two, and a daemon that
// does not serve the route (an older agent) leaves it unknown rather than
// empty — which is what keeps the footer failing open onto the wording that
// shipped (waired-agent#1042).
func TestFetchRouteAndHealth_ReadsTheMesh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/integration/claude/route":
			_ = json.NewEncoder(w).Encode(routing())
		case inferenceStatusPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"subsystem_state": "disabled"})
		case meshSnapshotPath:
			_ = json.NewEncoder(w).Encode(inferencemesh.Snapshot{
				Reachable: true,
				Peers:     []inferencemesh.PeerView{{DeviceID: "dev-mag", DeviceName: "sv-mag"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, health, _, mesh, ok := fetchRouteAndHealth(srv.URL)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if health != "disabled" {
		t.Errorf("health = %q, want disabled", health)
	}
	if !mesh.known || !mesh.reachable {
		t.Fatalf("mesh = %+v, want a known reachable mesh", mesh)
	}
	if got := mesh.peerName("dev-mag"); got != "sv-mag" {
		t.Errorf("peerName = %q, want sv-mag", got)
	}
}

// An agent with no mesh route at all: the read fails and the view stays
// unknown, never "there are no peers".
func TestFetchRouteAndHealth_MeshRouteMissingStaysUnknown(t *testing.T) {
	srv := routeStub(t, routing(), "disabled")
	_, _, _, mesh, ok := fetchRouteAndHealth(srv.URL)
	if !ok {
		t.Fatal("ok = false, want true — the route endpoint answered")
	}
	if mesh.known {
		t.Errorf("mesh = %+v, want unknown when the route 404s", mesh)
	}
}

func TestFetchRouteAndHealthUnreachable(t *testing.T) {
	srv := routeStub(t, management.ClaudeRoutingState{}, "ready")
	url := srv.URL
	srv.Close() // now unreachable
	if _, _, _, _, ok := fetchRouteAndHealth(url); ok {
		t.Error("ok = true against a closed server")
	}
}

func TestMgmtURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9476":        "http://127.0.0.1:9476" + inferenceStatusPath,
		"http://127.0.0.1:9476": "http://127.0.0.1:9476" + inferenceStatusPath,
		"127.0.0.1:9476/":       "http://127.0.0.1:9476" + inferenceStatusPath,
	}
	for in, want := range cases {
		if got := mgmtURL(in, inferenceStatusPath); got != want {
			t.Errorf("mgmtURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatuslineShadowNotice(t *testing.T) {
	eff := claudecode.EffectiveStatusLine{
		Scope:   claudecode.ScopeProjectLocal,
		Path:    "/repo/.claude/settings.local.json",
		Kind:    claudecode.StatusLineForeign,
		Command: "bash ~/.claude/statusline.sh",
	}
	got := statuslineShadowNotice(eff, nil)
	for _, want := range []string{"/repo/.claude/settings.local.json", "project-local", statuslineSnippet} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q:\n%s", want, got)
		}
	}

	if got := statuslineShadowNotice(claudecode.EffectiveStatusLine{Scope: claudecode.ScopeUser}, nil); got != "" {
		t.Errorf("user scope must not be reported as shadowed: %q", got)
	}
	if got := statuslineShadowNotice(claudecode.EffectiveStatusLine{}, nil); got != "" {
		t.Errorf("no statusline must not be reported as shadowed: %q", got)
	}
	if got := statuslineShadowNotice(eff, errDetectFailed); got != "" {
		t.Errorf("detection errors must be silent (best-effort): %q", got)
	}
}
