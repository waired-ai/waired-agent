package gateway

// The tests in this file drive the mesh serving path against peers that
// cost what a real overlay peer costs: a per-connection dial charge, a
// handler charge, the real peer.Adapter (which sets
// DisableKeepAlives, so every probe pays a fresh handshake), and a
// disco RTT the Selector actually measured.
//
// WHAT WAS MISSING, precisely (waired-agent#709 filed this as "no test
// drives the serving path from a realistic mesh snapshot through the
// real Selector"; that framing is wrong and this comment is the
// correction). sticky_spread_e2e_test.go already drives the real
// Selector over a real inferencemesh.Snapshot with real Capacity, all
// the way to the peer's engine. What no test did was give the probe a
// PRICE:
//
//   - its PeerAdapterFactory returns phase8FakePeerAdapter over
//     http.DefaultTransport, so there is no DisableKeepAlives and no
//     overlay dial,
//   - it wires no LocalRTT, so every candidate carries RTTUnknown and
//     the RTT-to-budget handoff never runs against a measurement,
//   - and every hop is loopback, so a probe completes in microseconds.
//
// Against a free probe, the readiness budget that shipped — one flat
// 50 ms — passed. Against a mesh measuring rtt_ms=52 with keep-alives
// off, a probe needs two round trips, no probe could finish, and every
// mesh request 503'd on every host in both directions (#624) with CI
// green throughout. Cost was the untested axis, not the Selector.
//
// CLAUDE.md §Test discipline: "A fake placed at the defect boundary
// means the subject never runs." The defect boundary here is the price
// of a probe, so that is what these tests stop faking.
//
// PINS: all four cases are product contracts. Ratifying sources are
// waired-agent#624 (the failure, plus the owner's 20260812 ruling that
// probes retry and the budget be generous with RTT), its fix in #707,
// and #709 for the test-shape gap. Two of them — the pair below — are
// the specific mis-attribution #624 was filed over.
//
// WHAT IS STILL FAKED, and why it is not the boundary: the WireGuard
// data plane (peer.OverlayDialer is redirected at httptest, the same
// seam internal/runtime/peer/integration_test.go uses) and the peer's
// signature verification (no inference.Server auth chain stands here).
// Neither decides whether a probe can finish inside its budget.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/internal/runtime/peer"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// distantPeer describes one mesh peer and what talking to it costs.
type distantPeer struct {
	deviceID  string
	overlayIP string

	// capacity is advertised in the snapshot (what the Selector's
	// admission pre-filter compares in-flight against) AND reported as
	// /healthz capacity_total. usedSlots is what /healthz reports in
	// use — the two are separate on purpose, because which one a
	// rejection came from is what #624's diagnosis got wrong.
	capacity  int
	usedSlots int

	// rttMS is what disco measured for this peer, as the Selector's
	// LocalRTT snapshot reports it. 0 means "never pongged", which
	// reaches the gateway as router.RTTUnknown.
	rttMS uint32

	// dialDelay is charged on every TCP connection to this peer, and
	// with DisableKeepAlives that is once per probe and once per
	// inference. healthDelay is charged inside its /healthz handler.
	// Together they are the two round trips a probe really costs.
	dialDelay   time.Duration
	healthDelay time.Duration

	// silentProbe makes /healthz never answer: the request arrives and
	// the handler holds it until the prober gives up. This is a peer
	// whose overlay path is broken in one direction, which is what the
	// real hosts in waired#1127 were doing.
	silentProbe bool
}

// distantMesh is the fixture: N priced peers, a real Selector over
// them, and a real gateway wired to reach them through a real
// peer.Adapter.
type distantMesh struct {
	gw       *Server
	inFlight *router.InFlightTracker
	peers    []distantPeer

	// healthzHits / chatHits count what each layer actually reached.
	// healthzHits is load-bearing: a rejection with zero probes is a
	// rejection the probe layer never made.
	healthzHits atomic.Int32
	chatHits    atomic.Int32
}

func newDistantMesh(t *testing.T, peers ...distantPeer) *distantMesh {
	t.Helper()
	m := &distantMesh{inFlight: router.NewInFlightTracker(), peers: peers}

	targets := map[netip.Addr]string{}
	delays := map[netip.Addr]time.Duration{}
	overlayOf := map[string]netip.Addr{}
	rtts := map[string]uint32{}
	var views []inferencemesh.PeerView

	for _, p := range peers {
		ip, err := netip.ParseAddr(p.overlayIP)
		if err != nil {
			t.Fatalf("peer %s: overlay ip %q: %v", p.deviceID, p.overlayIP, err)
		}
		srv := m.servePeer(t, p)
		targets[ip] = strings.TrimPrefix(srv.URL, "http://")
		delays[ip] = p.dialDelay
		overlayOf[p.deviceID] = ip
		// Absent from the LocalRTT snapshot is how a peer that has never
		// pongged looks — the Selector turns that into RTTUnknown.
		if p.rttMS != 0 {
			rtts[p.deviceID] = p.rttMS
		}
		views = append(views, inferencemesh.PeerView{
			DeviceID:   p.deviceID,
			DeviceName: p.deviceID,
			OverlayIP:  p.overlayIP,
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    []string{"qwen3:8b-q4_K_M"},
				LastCheck: "2026-08-12T10:00:00Z",
				Capacity:  p.capacity,
			},
		})
	}
	snap := inferencemesh.Snapshot{Peers: views}

	reg := runtime.NewRegistry()
	sel := router.NewSelector(router.Inputs{
		Manifests: []catalog.Manifest{qwenManifest()},
		// Deliberately empty: the local model is not ready, which is the
		// only path mesh candidates appear on.
		LocalState: catalog.State{
			Version:   catalog.StateVersion,
			Models:    map[string]catalog.ModelState{},
			Endpoints: map[string]catalog.EndpointState{},
		},
		Hardware: hardware.Profile{
			OS: "linux", Arch: "x86_64",
			CPU:        hardware.CPUInfo{Cores: 16},
			RAMTotalGB: 64, RAMAvailableGB: 48,
			Engines: hardware.InstalledEngines{Ollama: hardware.EngineInfo{Installed: true, Version: "0.22.1"}},
		},
		Runtimes:       reg,
		MeshSnapshotFn: func() inferencemesh.Snapshot { return snap },
		LocalRTT:       func() map[string]uint32 { return rtts },
		LocalInFlight:  m.inFlight,
	})

	// An ephemeral key: the peer adapter signs every request and this
	// repository is public, so nothing key-shaped is ever a literal here.
	_, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	dialer := &delayingOverlayDialer{targets: targets, delays: delays}

	m.gw = NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     &http.Client{},
		AllowOpenAI:    true,
		AllowAnthropic: true,
		// The real adapter, not phase8FakePeerAdapter: its transport is
		// what sets DisableKeepAlives, and that is the reason a probe
		// costs two round trips instead of one.
		PeerAdapterFactory: func(deviceID string) (runtime.Adapter, error) {
			ip, ok := overlayOf[deviceID]
			if !ok {
				return nil, fmt.Errorf("unknown peer %q", deviceID)
			}
			a, err := peer.NewAdapter(peer.Config{
				SelfDeviceID:  "dev-self",
				SelfPrivKey:   selfPriv,
				PeerDeviceID:  deviceID,
				PeerOverlayIP: ip,
				Dialer:        dialer,
			})
			if err != nil {
				return nil, err
			}
			return a, nil
		},
	})
	return m
}

// servePeer stands up one peer's overlay listener: the /healthz the
// probe coordinator reads and the chat route the inference leg proxies
// to, on one server, because that is how a real agent presents them.
func (m *distantMesh) servePeer(t *testing.T, p distantPeer) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/healthz", func(w http.ResponseWriter, r *http.Request) {
		m.healthzHits.Add(1)
		if p.silentProbe {
			// Hold the request until the prober's budget expires and it
			// cancels. Returning on ctx.Done rather than sleeping keeps
			// httptest.Server.Close from waiting on this handler.
			<-r.Context().Done()
			return
		}
		if !waitOrCancel(r.Context(), p.healthDelay) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The producer's own type (internal/inference), not a hand-written
		// literal: this peer stands in for a real agent's listener, and
		// probe_client_test.go already pins that HealthSnapshot and
		// router.HealthStatus agree on the wire.
		_ = json.NewEncoder(w).Encode(inference.HealthSnapshot{
			EngineReady:   true,
			ModelID:       "qwen3:8b-q4_K_M",
			CapacityTotal: p.capacity,
			CapacityUsed:  p.usedSlots,
			ShareEnabled:  true,
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		m.chatHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// post fires one chat request at the gateway's loopback surface.
func (m *distantMesh) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.gw.Handler().ServeHTTP(w, r)
	return w
}

// errorBody decodes the gateway's error envelope. The message string is
// what tells the probe layer's rejection apart from the router's, so it
// is read here rather than substring-matched on raw bytes.
func (m *distantMesh) errorBody(t *testing.T, w *httptest.ResponseRecorder) OpenAIError {
	t.Helper()
	var env openAIErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return env.Error
}

// saturate fills every peer's admission slots so the Selector's
// pre-filter drops them before any probe is sent.
func (m *distantMesh) saturate(t *testing.T) {
	t.Helper()
	for _, p := range m.peers {
		for i := 0; i < p.capacity; i++ {
			release, ok := m.inFlight.Acquire(p.deviceID, p.capacity)
			if !ok {
				t.Fatalf("peer %s: slot %d/%d refused while saturating", p.deviceID, i+1, p.capacity)
			}
			t.Cleanup(release)
		}
	}
}

// delayingOverlayDialer is the httptest-backed overlay dialer
// (internal/runtime/peer/integration_test.go's httptestOverlayDialer)
// plus a per-connection charge, which is the cost DisableKeepAlives
// makes the probe pay every time.
type delayingOverlayDialer struct {
	targets map[netip.Addr]string
	delays  map[netip.Addr]time.Duration
}

func (d *delayingOverlayDialer) DialOverlayTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error) {
	target, ok := d.targets[ip]
	if !ok {
		return nil, fmt.Errorf("no peer listening at overlay %s", netip.AddrPortFrom(ip, port))
	}
	if !waitOrCancel(ctx, d.delays[ip]) {
		return nil, ctx.Err()
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", target)
}

// waitOrCancel charges d, or reports false when the caller gave up
// first. Every delay in this file respects the context so a budget
// overrun costs the budget and not the delay.
func waitOrCancel(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// TestMeshDistanceE2E_PeersARoundTripAwayAreServed is #624's symptom,
// asserted through the whole stack: a mesh whose only peers are a real
// round trip away has to serve the request.
//
// The numbers are the ones that mattered. Each peer advertises
// rtt_ms=150, so probeBudgetFor gives the round 6×150 = 900 ms. Talking
// to it costs 30 ms to connect plus 30 ms in its handler — two round
// trips, because the peer adapter keeps no connections. 60 ms fits in
// 900 ms with room to spare, and did NOT fit in the flat 50 ms budget
// that shipped: that is the whole of #624.
//
// Deliberately no assertion about elapsed time. The test would then
// fail on a loaded runner for being slow, which is not the property
// being pinned — the property is that a peer at LAN distance is
// routable at all.
func TestMeshDistanceE2E_PeersARoundTripAwayAreServed(t *testing.T) {
	m := newDistantMesh(t,
		distantPeer{deviceID: "peer-a", overlayIP: "100.96.0.11", capacity: 4, rttMS: 150,
			dialDelay: 30 * time.Millisecond, healthDelay: 30 * time.Millisecond},
		distantPeer{deviceID: "peer-b", overlayIP: "100.96.0.12", capacity: 4, rttMS: 150,
			dialDelay: 30 * time.Millisecond, healthDelay: 30 * time.Millisecond},
		distantPeer{deviceID: "peer-c", overlayIP: "100.96.0.13", capacity: 4, rttMS: 150,
			dialDelay: 30 * time.Millisecond, healthDelay: 30 * time.Millisecond},
	)

	w := m.post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	// peer-a is the Selector's first choice (equal scores, deviceID-asc)
	// and it probed ready, so it must also be the peer that served —
	// ParallelProbe decides on rank, not on arrival.
	if got := w.Header().Get(HeaderInferencePeer); got != "peer-a" {
		t.Errorf("%s = %q, want %q", HeaderInferencePeer, got, "peer-a")
	}
	if got := w.Header().Get(HeaderFallbackFrom); got != "" {
		t.Errorf("%s = %q, want empty (first choice served it)", HeaderFallbackFrom, got)
	}
	if got := m.chatHits.Load(); got != 1 {
		t.Errorf("inference calls = %d, want 1", got)
	}
}

// TestMeshDistanceE2E_ProbesThatNeverAnswerAreNotReportedAsCapacity is
// the naming half of #707: when no probe comes back, the gateway knows
// nothing about the mesh's load, and saying "at capacity" sends the
// reader to the capacity filter — which is exactly the wrong turn #624
// took.
//
// The message is compared for equality, not containment: the probe
// layer returns the sentinel bare, and that is what distinguishes it
// from the router's wrapped one. See the pair below.
func TestMeshDistanceE2E_ProbesThatNeverAnswerAreNotReportedAsCapacity(t *testing.T) {
	m := newDistantMesh(t,
		distantPeer{deviceID: "peer-a", overlayIP: "100.96.0.11", capacity: 4, rttMS: 1, silentProbe: true},
		distantPeer{deviceID: "peer-b", overlayIP: "100.96.0.12", capacity: 4, rttMS: 1, silentProbe: true},
	)

	w := m.post(t)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	errBody := m.errorBody(t, w)
	if errBody.Code != "waired_peers_did_not_answer" {
		t.Errorf("code = %q, want %q", errBody.Code, "waired_peers_did_not_answer")
	}
	if errBody.Message != router.ErrPeersDidNotAnswer.Error() {
		t.Errorf("message = %q, want the bare probe-layer sentinel %q",
			errBody.Message, router.ErrPeersDidNotAnswer.Error())
	}
	if got := m.healthzHits.Load(); got == 0 {
		t.Error("no probe was sent; this case is about probes that go unanswered, not unsent")
	}
	if got := m.chatHits.Load(); got != 0 {
		t.Errorf("inference calls = %d, want 0 (nothing was verified)", got)
	}
}

// TestMeshDistanceE2E_FullPeersRejectAtTheProbeLayer and its sibling
// below are the pair #709 exists for. Both end in 503
// waired_all_peers_overloaded, and they come from different layers:
//
//   - here the peers answer /healthz with capacity_used ==
//     capacity_total, so the PROBE layer rejects and returns
//     router.ErrAllPeersOverloaded bare (internal/gateway/probe.go),
//   - below the Selector's admission pre-filter rejects first, and
//     SelectK wraps the same sentinel with the model id.
//
// #624 read the bare string as evidence for the second and went
// hunting in endpoint_router.go's capacity filter, which had not run.
// The strings are the only thing that tells them apart on the wire, so
// they are asserted exactly, and the probe count is asserted with them:
// a rejection with zero probes cannot have come from the probe layer.
func TestMeshDistanceE2E_FullPeersRejectAtTheProbeLayer(t *testing.T) {
	m := newDistantMesh(t,
		distantPeer{deviceID: "peer-a", overlayIP: "100.96.0.11", capacity: 4, usedSlots: 4, rttMS: 1},
		distantPeer{deviceID: "peer-b", overlayIP: "100.96.0.12", capacity: 4, usedSlots: 4, rttMS: 1},
	)

	w := m.post(t)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	errBody := m.errorBody(t, w)
	if errBody.Code != "waired_all_peers_overloaded" {
		t.Errorf("code = %q, want %q", errBody.Code, "waired_all_peers_overloaded")
	}
	if errBody.Message != router.ErrAllPeersOverloaded.Error() {
		t.Errorf("message = %q, want the bare probe-layer sentinel %q",
			errBody.Message, router.ErrAllPeersOverloaded.Error())
	}
	if got := m.healthzHits.Load(); got == 0 {
		t.Error("peers were rejected as full without being asked; that verdict belongs to the probe layer")
	}
	if got := m.chatHits.Load(); got != 0 {
		t.Errorf("inference calls = %d, want 0", got)
	}
}

// TestMeshDistanceE2E_SaturatedPeersRejectAtTheRouterLayer is the
// sibling: the peers would answer /healthz ready, but this agent already
// holds every admission slot it advertised, so the Selector drops them
// before a probe is sent.
//
// Same 503, same wire code, and a message that must be distinguishable:
// SelectK wraps the sentinel with the model id, and no probe is sent at
// all.
func TestMeshDistanceE2E_SaturatedPeersRejectAtTheRouterLayer(t *testing.T) {
	m := newDistantMesh(t,
		distantPeer{deviceID: "peer-a", overlayIP: "100.96.0.11", capacity: 2, rttMS: 1},
		distantPeer{deviceID: "peer-b", overlayIP: "100.96.0.12", capacity: 2, rttMS: 1},
	)
	m.saturate(t)

	w := m.post(t)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	errBody := m.errorBody(t, w)
	if errBody.Code != "waired_all_peers_overloaded" {
		t.Errorf("code = %q, want %q", errBody.Code, "waired_all_peers_overloaded")
	}
	bare := router.ErrAllPeersOverloaded.Error()
	if errBody.Message == bare {
		t.Errorf("message = %q: the router layer's rejection must not be indistinguishable "+
			"from the probe layer's bare sentinel (waired-agent#624 read one as the other)", errBody.Message)
	}
	if !strings.HasPrefix(errBody.Message, bare+": ") {
		t.Errorf("message = %q, want it to start with %q", errBody.Message, bare+": ")
	}
	if !strings.Contains(errBody.Message, `"qwen3-8b-instruct"`) {
		t.Errorf("message = %q, want the model id SelectK wraps in", errBody.Message)
	}
	if got := m.healthzHits.Load(); got != 0 {
		t.Errorf("probes sent = %d, want 0: the admission pre-filter runs before any probe", got)
	}
	if got := m.chatHits.Load(); got != 0 {
		t.Errorf("inference calls = %d, want 0", got)
	}
}
