package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// The tests in this file drive the WHOLE chain the concurrent-sub
// spread lives in — the gateway handler, the real router.Selector, the
// parallel /healthz probe, Commit, and the proxy to the peer's engine —
// against HTTP peers. The unit tests either side of it pin the pieces;
// these pin the outcome, which is the only thing waired-ai/waired#828
// asks for: two sub-agents of one conversation running at the same time
// must not both land on one machine.

// spreadPeer is one mesh peer as the gateway sees it: an HTTP server
// answering both the overlay /healthz probe and the inference request,
// recording every inference it served.
type spreadPeer struct {
	deviceID string
	srv      *httptest.Server

	mu    sync.Mutex
	serve []time.Time

	// arrived reports each inference reaching the engine; hold blocks
	// the engine so a test can keep a request in flight while it fires
	// the next one.
	arrived chan string
	hold    chan struct{}
}

func newSpreadPeer(t *testing.T, deviceID string, arrived chan string, hold chan struct{}) *spreadPeer {
	t.Helper()
	p := &spreadPeer{deviceID: deviceID, arrived: arrived, hold: hold}
	mux := http.NewServeMux()
	mux.HandleFunc("/waired/v1/inference/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(readyBody(0, 8)))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.serve = append(p.serve, time.Now())
		p.mu.Unlock()
		if p.arrived != nil {
			p.arrived <- p.deviceID
		}
		if p.hold != nil {
			<-p.hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *spreadPeer) served() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.serve)
}

// spreadMesh is the fixture: N peers, a real Selector over them, and a
// gateway wired to reach them.
type spreadMesh struct {
	peers map[string]*spreadPeer
	gw    *Server
}

func newSpreadMesh(t *testing.T, arrived chan string, hold chan struct{}, deviceIDs ...string) *spreadMesh {
	t.Helper()
	m := &spreadMesh{peers: map[string]*spreadPeer{}}
	var views []inferencemesh.PeerView
	for _, id := range deviceIDs {
		m.peers[id] = newSpreadPeer(t, id, arrived, hold)
		views = append(views, inferencemesh.PeerView{
			DeviceID:   id,
			DeviceName: id,
			InferenceState: &signer.InferenceState{
				Reachable: true,
				Type:      signer.InferenceTypeOllama,
				Models:    []string{"qwen3:8b-q4_K_M"},
				LastCheck: "2026-08-05T18:00:00Z",
				Capacity:  8,
			},
		})
	}
	snap := inferencemesh.Snapshot{Peers: views}

	reg := runtime.NewRegistry()
	sel := router.NewSelector(router.Inputs{
		Manifests: []catalog.Manifest{qwenManifest()},
		// Deliberately empty: the local model is not ready, which is
		// the only path mesh candidates appear on.
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
		Sticky:         router.NewStickyStore(time.Minute, time.Now),
		LocalInFlight:  router.NewInFlightTracker(),
		StickyInFlight: router.NewStickyInFlight(),
	})

	m.gw = NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     &http.Client{},
		AllowOpenAI:    true,
		AllowAnthropic: true,
		PeerAdapterFactory: func(deviceID string) (runtime.Adapter, error) {
			p, ok := m.peers[deviceID]
			if !ok {
				return nil, fmt.Errorf("unknown peer %q", deviceID)
			}
			return phase8FakePeerAdapter{transport: http.DefaultTransport, base: p.srv.URL}, nil
		},
	})
	return m
}

// post fires one chat request carrying a conversation id, which is what
// ComputeStickyID turns into the sticky key.
func (m *spreadMesh) post(conversationID string) *httptest.ResponseRecorder {
	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Waired-Conversation-Id", conversationID)
	w := httptest.NewRecorder()
	m.gw.Handler().ServeHTTP(w, r)
	return w
}

func (m *spreadMesh) servedBy() map[string]int {
	out := map[string]int{}
	for id, p := range m.peers {
		if n := p.served(); n > 0 {
			out[id] = n
		}
	}
	return out
}

// TestSpreadE2E_ConcurrentSubsLandOnDifferentPeers is the outcome
// waired-ai/waired#828 asks for, proved through the real chain.
//
// Three sub-agent requests of one conversation are in flight at the
// same time — each engine blocks until all three have arrived, so the
// overlap is real and not a timing accident. Before the spread all
// three went to whichever peer answered /healthz first.
func TestSpreadE2E_ConcurrentSubsLandOnDifferentPeers(t *testing.T) {
	arrived := make(chan string, 8)
	hold := make(chan struct{})
	m := newSpreadMesh(t, arrived, hold, "peer-A", "peer-B", "peer-C")

	const concurrent = 3
	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		// Each request is fired only after the previous one has reached
		// its engine, so every SelectK after the first runs with the
		// earlier requests genuinely in flight.
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = m.post("conv-spread").Code
		}(i)
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(hold)
			t.Fatalf("request %d never reached an engine", i)
		}
	}
	close(hold)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}
	served := m.servedBy()
	if len(served) < 2 {
		t.Fatalf("all %d concurrent sub-agents landed on one peer: %v", concurrent, served)
	}
	total := 0
	for _, n := range served {
		total += n
	}
	if total != concurrent {
		t.Errorf("engines served %d requests in total, want %d: %v", total, concurrent, served)
	}
	t.Logf("concurrent sub-agents spread over %d peers: %v", len(served), served)
}

// TestSpreadE2E_SequentialTurnsStayOnOnePeer is the control, and the
// reason the spread keys on overlap rather than on the conversation.
// The same conversation taking turns one at a time must keep its KV
// prefix on one machine — spreading these would rebuild the prefix
// every turn.
func TestSpreadE2E_SequentialTurnsStayOnOnePeer(t *testing.T) {
	m := newSpreadMesh(t, nil, nil, "peer-A", "peer-B", "peer-C")

	const turns = 6
	for i := 0; i < turns; i++ {
		if code := m.post("conv-sequential").Code; code != http.StatusOK {
			t.Fatalf("turn %d: status = %d, want 200", i, code)
		}
	}

	served := m.servedBy()
	if len(served) != 1 {
		t.Fatalf("sequential turns spread over %d peers, want 1 (sticky affinity regressed): %v", len(served), served)
	}
	for id, n := range served {
		if n != turns {
			t.Errorf("peer %s served %d of %d turns", id, n, turns)
		}
	}
	t.Logf("sequential turns all stayed on: %v", served)
}

// TestSpreadE2E_ConcurrentDistinctConversationsAlsoSpread covers the
// other half of the fan-out, and it is the half that decides whether
// #828 is worth anything in practice.
//
// Claude Code sets no X-Waired-Conversation-Id (nothing in this repo
// does), so the sticky key for its traffic comes from the identity rung
// of ComputeStickyID: the client's own user id plus its first message.
// Two concurrent sub-agents of one session share the user id — the
// measured value carries a session id, not a turn id — and differ in
// their task prompt, so whether they collide on one key depends on the
// client. When they do collide, demoteBusySticky spreads them; when
// they do not, nothing sticky-shaped is involved at all and the spread
// has to come from the Selector's weighted-least-loaded axis
// (loadFraction in sortMeshCandidates).
//
// That axis existed before this change and could not take effect: it
// only orders candidates, and the probe used to ignore the order. Both
// halves therefore need the same ranking fix, and both are pinned here.
func TestSpreadE2E_ConcurrentDistinctConversationsAlsoSpread(t *testing.T) {
	arrived := make(chan string, 8)
	hold := make(chan struct{})
	m := newSpreadMesh(t, arrived, hold, "peer-A", "peer-B", "peer-C")

	const concurrent = 3
	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = m.post(fmt.Sprintf("conv-distinct-%d", i)).Code
		}(i)
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(hold)
			t.Fatalf("request %d never reached an engine", i)
		}
	}
	close(hold)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}
	served := m.servedBy()
	if len(served) != concurrent {
		t.Fatalf("%d unrelated concurrent requests used %d peers, want %d: %v",
			concurrent, len(served), concurrent, served)
	}
	t.Logf("unrelated concurrent requests spread over %d peers: %v", len(served), served)
}

// TestSpreadE2E_SoloPeerStillServesConcurrentSubs guards the failure
// mode a hard exclusion would have introduced: with only one peer there
// is nowhere to spread to, and refusing the second concurrent sub would
// be strictly worse than serving it.
func TestSpreadE2E_SoloPeerStillServesConcurrentSubs(t *testing.T) {
	arrived := make(chan string, 4)
	hold := make(chan struct{})
	m := newSpreadMesh(t, arrived, hold, "peer-A")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = m.post("conv-solo").Code
		}(i)
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(hold)
			t.Fatalf("request %d never reached the engine", i)
		}
	}
	close(hold)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, code)
		}
	}
	if got := m.peers["peer-A"].served(); got != 2 {
		t.Errorf("peer-A served %d requests, want 2", got)
	}
}
