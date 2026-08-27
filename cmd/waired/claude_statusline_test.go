package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
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

func fallbackAt(when time.Time, count int64, reason, direction string) *management.ClaudeRoutingFallbackEvent {
	return &management.ClaudeRoutingFallbackEvent{When: when, Reason: reason, Count: count, Direction: direction}
}

func routing(main state.ClaudeRouteClass, opts ...func(*management.ClaudeRoutingState)) management.ClaudeRoutingState {
	st := management.ClaudeRoutingState{Policy: state.ClaudeRoutingPolicy{Main: main, Sub: state.ClaudeRouteSame}}
	for _, o := range opts {
		o(&st)
	}
	return st
}

func withModel(m string) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.LastLocalModel = m }
}

func withFallback(e *management.ClaudeRoutingFallbackEvent) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.LastFallback = e }
}

func withSub(c state.ClaudeRouteClass) func(*management.ClaudeRoutingState) {
	return func(st *management.ClaudeRoutingState) { st.Policy.Sub = c }
}

func TestRenderStatusline(t *testing.T) {
	plainStatusline(t)
	now := time.Now()
	cases := []struct {
		name   string
		route  management.ClaudeRoutingState
		health string
		want   string
	}{
		{"auto-waired", routing(state.ClaudeRouteAuto), "ready", "waired: on Waired"},
		{"auto-degraded", routing(state.ClaudeRouteAuto), "degraded", "waired: fallback -> Anthropic (local degraded)"},
		{"auto-recent-fallback", routing(state.ClaudeRouteAuto, withFallback(fallbackAt(now.Add(-2*time.Second), 1, "local_status_503", "anthropic"))), "ready", "waired: fell back -> Anthropic"},
		{"waired-ready", routing(state.ClaudeRouteWaired), "ready", "waired: Waired-only"},
		{"waired-down", routing(state.ClaudeRouteWaired), "no_engine", "! waired: Waired-only (down)"},
		{"anthropic", routing(state.ClaudeRouteAnthropic), "ready", "-> waired: Anthropic"},
		{"empty-mode-defaults-auto", management.ClaudeRoutingState{}, "ready", "waired: on Waired"},
		// #602: the last locally-served model id is appended while serving on
		// Waired, and hidden on every non-Waired-serving branch.
		{"auto-waired-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct")), "ready", "waired: on Waired (qwen3-8b-instruct)"},
		{"auto-degraded-hides-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct")), "degraded", "waired: fallback -> Anthropic (local degraded)"},
		{"auto-recent-fallback-hides-model", routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct"), withFallback(fallbackAt(now.Add(-2*time.Second), 1, "local_status_503", "anthropic"))), "ready", "waired: fell back -> Anthropic"},
		{"waired-ready-model", routing(state.ClaudeRouteWaired, withModel("qwen3-8b-instruct")), "ready", "waired: Waired-only (qwen3-8b-instruct)"},
		{"waired-down-hides-model", routing(state.ClaudeRouteWaired, withModel("qwen3-8b-instruct")), "no_engine", "! waired: Waired-only (down)"},
		// A local-degrade fallback (anthropic route → local) must NOT read as a
		// "fell back to Anthropic" segment.
		{"local-degrade-ignored-in-auto", routing(state.ClaudeRouteAuto, withFallback(fallbackAt(now.Add(-2*time.Second), 1, "anthropic_unreachable", "local"))), "ready", "waired: on Waired"},
		// waired-agent#817: a subagent split is named, and only when there
		// is one. The reported shape is the first row — `waired claude
		// route anthropic --sub waired` printed "-> waired: Anthropic" and
		// said nothing about the split, on the one surface a user watches
		// every turn.
		{"split-anthropic-main-waired-sub", routing(state.ClaudeRouteAnthropic, withSub(state.ClaudeRouteWaired)), "ready", "-> waired: Anthropic - subagents: Waired"},
		{"split-auto-main-anthropic-sub", routing(state.ClaudeRouteAuto, withSub(state.ClaudeRouteAnthropic), withModel("qwen3-8b-instruct")), "ready", "waired: on Waired (qwen3-8b-instruct) - subagents: Anthropic"},
		{"split-survives-a-down-engine", routing(state.ClaudeRouteWaired, withSub(state.ClaudeRouteAnthropic)), "no_engine", "! waired: Waired-only (down) - subagents: Anthropic"},
		// Not a split: subagents following main is the default, and an
		// explicit pin to the class main already uses changes nothing an
		// operator could act on. Both must render exactly as before.
		{"following-main-is-not-a-split", routing(state.ClaudeRouteAnthropic, withSub(state.ClaudeRouteSame)), "ready", "-> waired: Anthropic"},
		{"pinned-to-the-same-class-is-not-a-split", routing(state.ClaudeRouteAnthropic, withSub(state.ClaudeRouteAnthropic)), "ready", "-> waired: Anthropic"},
		// An unset Sub means the same thing "same" does, and a host that
		// has never touched the setting must not sprout a tail.
		{"unset-sub-is-not-a-split", routing(state.ClaudeRouteAuto, withSub("")), "ready", "waired: on Waired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, nil, meshView{}); got != tc.want {
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
			route:  routing(state.ClaudeRouteAuto, servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// A peer whose turn predates the model being recorded (an agent
			// older than #755's header, or a selection that named no catalog
			// id): the machine still gets named.
			name:   "a named peer with no model recorded",
			route:  routing(state.ClaudeRouteAuto, servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// Nothing has been served yet, so there is no name to give. The
			// line still must not announce a fallback that will not happen.
			name:   "a peer is there but has not answered yet",
			route:  routing(state.ClaudeRouteAuto),
			health: "disabled", mesh: withPeer,
			want: "waired: on Waired (peer)",
		},
		{
			name:   "engine off here and no peer either is a real fallback",
			route:  routing(state.ClaudeRouteAuto),
			health: "disabled", mesh: noPeer,
			want: "waired: fallback -> Anthropic (local disabled, no peer)",
		},
		{
			// The mesh read did not come back. Saying "no peer" would be a
			// claim; the line keeps the wording that shipped.
			name:   "an unread mesh claims nothing",
			route:  routing(state.ClaudeRouteAuto),
			health: "disabled", mesh: unread,
			want: "waired: fallback -> Anthropic (local disabled)",
		},
		{
			// A local engine that is up answers the turn itself, and the
			// peer clause would be about a machine that is not involved.
			name:   "a healthy local engine is unaffected by the mesh",
			route:  routing(state.ClaudeRouteAuto, withModel("qwen3-8b-instruct")),
			health: "ready", mesh: withPeer,
			want: "waired: on Waired (qwen3-8b-instruct)",
		},
		{
			// A fallback that just happened outranks either serving branch:
			// the user is looking at a reply Waired did not produce.
			name: "a fallback a moment ago still wins",
			route: routing(state.ClaudeRouteAuto,
				withFallback(fallbackAt(time.Now().Add(-2*time.Second), 1, "local_status_503", "anthropic"))),
			health: "disabled", mesh: withPeer,
			want: "waired: fell back -> Anthropic",
		},
		{
			// route=waired never leaves for Anthropic, so an engine-less
			// host with a serving peer is not "down" — it is doing exactly
			// what it was set up to do.
			name:   "Waired-only on an engine-less host with a peer is not down",
			route:  routing(state.ClaudeRouteWaired, servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer,
			want: "waired: Waired-only (peer sv-mag)",
		},
		{
			name:   "Waired-only with nothing anywhere is still down",
			route:  routing(state.ClaudeRouteWaired),
			health: "disabled", mesh: noPeer,
			want: "! waired: Waired-only (down)",
		},
		{
			// A peer whose name this device does not have (it left the mesh
			// since it answered) is "a peer", never a raw device id.
			name:   "an unnameable peer is not rendered as an identifier",
			route:  routing(state.ClaudeRouteAuto, servedByPeer("dev-gone")),
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
			route:  routing(state.ClaudeRouteAuto, servedByPeer("dev-mag")),
			health: "disabled", mesh: withPeer, resident: &no,
			want: "waired: on Waired (peer sv-mag)",
		},
		{
			// The same clause on the branch it IS about, so the row above
			// is proving an absence and not just a nil reading.
			name:   "the residency clause still follows this computer",
			route:  routing(state.ClaudeRouteAuto),
			health: "ready", mesh: withPeer, resident: &no,
			want: "waired: on Waired - model not loaded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, tc.resident, tc.mesh); got != tc.want {
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
	peerServed := routing(state.ClaudeRouteAuto)
	peerServed.LastServedBy = "peer-a"

	cases := []struct {
		name     string
		route    management.ClaudeRoutingState
		health   string
		resident *bool
		want     string
	}{
		{"not loaded, auto", routing(state.ClaudeRouteAuto), "ready", &no,
			"waired: on Waired - model not loaded"},
		{"not loaded, waired-only", routing(state.ClaudeRouteWaired), "ready", &no,
			"waired: Waired-only - model not loaded"},
		{"loaded says nothing extra", routing(state.ClaudeRouteAuto), "ready", &yes,
			"waired: on Waired"},
		{"no claim says nothing extra", routing(state.ClaudeRouteAuto), "ready", nil,
			"waired: on Waired"},
		{"a peer answered, so local residency is not this turn's fact",
			peerServed, "ready", &no, "waired: on Waired"},
		{"not the branch this computer answers on",
			routing(state.ClaudeRouteAnthropic), "ready", &no, "-> waired: Anthropic"},
		{"degraded is already saying something else",
			routing(state.ClaudeRouteAuto), "loading", &no,
			"waired: fallback -> Anthropic (local loading)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderStatusline(tc.route, tc.health, tc.resident, meshView{}); got != tc.want {
				t.Errorf("renderStatusline = %q, want %q", got, tc.want)
			}
		})
	}
}

// The clause goes before the subagent tail: residency is about this turn,
// the split is about configuration, and reading them the other way round
// suggests the split is what is not loaded.
func TestRenderStatusline_NotLoadedPrecedesTheSubagentTail(t *testing.T) {
	plainStatusline(t)
	no := false
	got := renderStatusline(routing(state.ClaudeRouteAuto, withSub(state.ClaudeRouteAnthropic)), "ready", &no, meshView{})
	want := "waired: on Waired - model not loaded - subagents: Anthropic"
	if got != want {
		t.Errorf("renderStatusline = %q, want %q", got, want)
	}
}

// A colourized run must not lose the clause: the segment is wrapped once, at
// the end, so anything appended after that wrap would fall outside it.
func TestRenderStatusline_NotLoadedStaysInsideTheColorWrap(t *testing.T) {
	t.Setenv("WAIRED_NO_EMOJI", "1")
	no := false
	got := renderStatusline(routing(state.ClaudeRouteAuto), "ready", &no, meshView{})
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
	got := renderStatusline(routing(state.ClaudeRouteAuto), "ready", nil, meshView{})
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) {
		t.Errorf("expected green-wrapped segment, got %q", got)
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
	srv := routeStub(t, routing(state.ClaudeRouteWaired), "degraded")
	route, health, _, _, ok := fetchRouteAndHealth(srv.URL)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if route.Policy.Main != state.ClaudeRouteWaired {
		t.Errorf("main = %q", route.Policy.Main)
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
			_ = json.NewEncoder(w).Encode(routing(state.ClaudeRouteAuto))
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
	srv := routeStub(t, routing(state.ClaudeRouteAuto), "disabled")
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

// sealUserCache points the per-session fallback marker at a directory
// unique to this test, so the hook's own writes cannot decide the next
// run's verdict.
//
// These tests used to set only XDG_CACHE_HOME, which os.UserCacheDir
// ignores on darwin (it reads HOME) and on Windows (LocalAppData). The
// marker therefore landed in the developer's real
// ~/Library/Caches/waired/claude-fallback, where
// TestRunFallbackHookEmitsOnNewFallback's own `writeFallbackCount` made
// `fb.Count <= prev` true forever after: the test passed exactly once per
// machine and then failed until pruneFallbackCache aged the file out a
// week later (#386). All three variables are set so the same hole cannot
// reopen on a different OS.
//
// seams_test.go's TestMain seals the same three package-wide; this narrows
// it to one directory per test, which is what makes `go test -count=2`
// meaningful here.
func sealUserCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
}

// TestFallbackCacheDirIsSealed is the direct pin on the above: if the seal
// stops working, this fails loudly instead of the five hook tests failing
// obscurely, one machine at a time.
func TestFallbackCacheDirIsSealed(t *testing.T) {
	sealUserCache(t)
	dir, err := fallbackCacheDir()
	if err != nil {
		t.Fatalf("fallbackCacheDir: %v", err)
	}
	home := os.Getenv("HOME")
	if home == "" || !strings.HasPrefix(dir, home) {
		t.Errorf("fallbackCacheDir() = %q, want it under the sealed home %q — "+
			"the hook would be writing to the developer's real cache", dir, home)
	}
}

func TestRunFallbackHookEmitsOnNewFallback(t *testing.T) {
	sealUserCache(t)
	st := routing(state.ClaudeRouteAuto, withFallback(fallbackAt(time.Now(), 1, "local_status_503", "anthropic")))
	srv := routeStub(t, st, "ready")
	stdin := strings.NewReader(`{"session_id":"sess-A","hook_event_name":"Stop"}`)

	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, stdin, &out); err != nil {
		t.Fatalf("hook: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("expected a systemMessage JSON, got %q (%v)", out.String(), err)
	}
	if !strings.Contains(got["systemMessage"], "real Anthropic API") || !strings.Contains(got["systemMessage"], "local_status_503") {
		t.Errorf("systemMessage = %q", got["systemMessage"])
	}

	// Same count again for the same session ⇒ no repeat.
	out.Reset()
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-A"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook repeated on unchanged count: %q", out.String())
	}
}

func TestRunFallbackHookSuppressesStale(t *testing.T) {
	sealUserCache(t)
	st := routing(state.ClaudeRouteAuto, withFallback(fallbackAt(time.Now().Add(-10*time.Minute), 5, "local_status_400", "anthropic")))
	srv := routeStub(t, st, "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-B"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted for a stale fallback: %q", out.String())
	}
}

func TestRunFallbackHookSuppressesLocalDirection(t *testing.T) {
	sealUserCache(t)
	// A local-degrade (anthropic route → local) is not a "reply came from
	// Anthropic" notice.
	st := routing(state.ClaudeRouteAnthropic, withFallback(fallbackAt(time.Now(), 1, "anthropic_unreachable", "local")))
	srv := routeStub(t, st, "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-L"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted for a local-direction fallback: %q", out.String())
	}
}

func TestRunFallbackHookSilentWhenNoFallback(t *testing.T) {
	sealUserCache(t)
	srv := routeStub(t, routing(state.ClaudeRouteAuto), "ready")
	var out bytes.Buffer
	if err := runFallbackHook(srv.URL, strings.NewReader(`{"session_id":"sess-C"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted with no LastFallback: %q", out.String())
	}
}

func TestRunFallbackHookSilentWhenUnreachable(t *testing.T) {
	sealUserCache(t)
	srv := routeStub(t, management.ClaudeRoutingState{}, "ready")
	url := srv.URL
	srv.Close()
	var out bytes.Buffer
	if err := runFallbackHook(url, strings.NewReader(`{"session_id":"sess-D"}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("hook emitted against a closed agent: %q", out.String())
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
