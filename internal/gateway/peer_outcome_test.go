package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// peerOutcomeSpy records every OnPeerOutcome call with BOTH arguments,
// so a test can assert which peer was charged and not merely that
// something was.
type peerOutcomeSpy struct {
	mu      sync.Mutex
	samples []peerSample
}

type peerSample struct {
	deviceID string
	ok       bool
}

func (s *peerOutcomeSpy) hook(deviceID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, peerSample{deviceID, ok})
}

func (s *peerOutcomeSpy) all() []peerSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]peerSample(nil), s.samples...)
}

// only returns the single sample the spy holds, failing the test when
// the count is anything else — "exactly one" is part of what every case
// below asserts.
func (s *peerOutcomeSpy) only(t *testing.T) peerSample {
	t.Helper()
	got := s.all()
	if len(got) != 1 {
		t.Fatalf("samples = %+v, want exactly 1", got)
	}
	return got[0]
}

// newPeerOutcomeGateway builds a loopback-style listener (local engine +
// peer routing enabled) wired to spy, mirroring newAdmissionGateway.
func newPeerOutcomeGateway(t *testing.T, sel SelectorIface, spy *peerOutcomeSpy, rec Recorder, engineURL string) *Server {
	t.Helper()
	reg := runtime.NewRegistry()
	reg.Register(fakeAdapter{baseURL: engineURL})
	var hook func(string, bool)
	if spy != nil {
		hook = spy.hook
	}
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:       sel,
		Runtimes:       reg,
		ListManifests:  asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:     http.DefaultClient,
		AllowOpenAI:    true,
		AllowAnthropic: true,
		Recorder:       rec,
		PeerAdapterFactory: func(string) (runtime.Adapter, error) {
			return fakeAdapter{baseURL: engineURL}, nil
		},
		OnPeerOutcome: hook,
	})
}

func postChatCtx(t *testing.T, gw *Server, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"waired/default","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)
	return w
}

func remoteSel(runtime string) router.Selection {
	return router.Selection{
		Runtime:     runtime,
		ModelID:     "qwen3-8b-instruct",
		EngineModel: "qwen3:8b-q4_K_M",
	}
}

// TestPeerOutcome_RemoteSuccessIsRecorded: the window's denominator.
// Without successes a peer that fails once would read as 100% failing
// forever, so the served case has to produce a sample too.
func TestPeerOutcome_RemoteSuccessIsRecorded(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("remote:dev-peer-b")}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	if code := postChatCtx(t, gw, nil).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := spy.only(t); got.deviceID != "dev-peer-b" || !got.ok {
		t.Errorf("sample = %+v, want {dev-peer-b true}", got)
	}
}

// TestPeerOutcome_PeerErrorIsChargedToThePeer: the case waired-agent#281
// exists for. A peer whose /healthz answers but whose engine 5xxes wins
// the probe on every request; the only thing that can ever demote it is
// the record of what happened after it was chosen.
func TestPeerOutcome_PeerErrorIsChargedToThePeer(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "engine exploded", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("remote:dev-peer-b")}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	if code := postChatCtx(t, gw, nil).Code; code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the peer's 500 forwarded", code)
	}
	if got := spy.only(t); got.deviceID != "dev-peer-b" || got.ok {
		t.Errorf("sample = %+v, want {dev-peer-b false}", got)
	}
}

// TestPeerOutcome_LocalDispatchIsNotRecorded: the inverse of
// TestLocalAdmission_RemoteDispatchIsNotCounted. A locally-served
// request says nothing about any peer; charging one would move a
// ranking on evidence that never involved it.
func TestPeerOutcome_LocalDispatchIsNotRecorded(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("ollama")}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	if code := postChatCtx(t, gw, nil).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got := spy.all(); len(got) != 0 {
		t.Errorf("samples = %+v, want none for a locally-served request", got)
	}
}

// TestPeerOutcome_KeyIsTheDeviceIDNotTheDisplayIdentifier pins the key
// this signal is filed under.
//
// PRODUCT CONTRACT (waired-agent#281 + public share spec §8.5, as
// implemented by newRemoteCandidate in internal/router): Selection
// carries the real DeviceID in Runtime and the display identifier —
// a grant pseudonym for a Public Share peer — in PeerDisplayID. The
// Selector reads its error window back by DeviceID, so keying this on
// the display identifier would open a second entry that is never read
// and leave the tie-break exactly as unfed as before.
func TestPeerOutcome_KeyIsTheDeviceIDNotTheDisplayIdentifier(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	pub := remoteSel("remote:dev-real-id")
	pub.PeerDisplayID = "pub-pseudonym-xyz"
	sel := &fakeSelector{sel: pub}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	if code := postChatCtx(t, gw, nil).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got := spy.only(t)
	if got.deviceID == "pub-pseudonym-xyz" {
		t.Fatalf("keyed on the display identifier; the Selector reads this back by DeviceID")
	}
	if got.deviceID != "dev-real-id" {
		t.Errorf("deviceID = %q, want %q", got.deviceID, "dev-real-id")
	}
}

// cancellingSelector names a peer and then cancels the request, the
// order an operator's Ctrl-C arrives in: the selection is already made,
// so the record carries a peer, and the dispatch that follows fails on
// the dead context.
type cancellingSelector struct {
	inner  *fakeSelector
	cancel context.CancelFunc
}

func (c *cancellingSelector) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	return c.inner.Select(ctx, req)
}

func (c *cancellingSelector) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	cands, err := c.inner.SelectK(ctx, req, k)
	c.cancel()
	return cands, err
}

// TestPeerOutcome_ClientCancellationIsNotChargedToThePeer.
//
// PRODUCT CONTRACT (waired-agent#281): an operator pressing Ctrl-C
// cancels the request context, the dispatch then fails, and the handler
// records mid_stream_truncate — the same shape a peer failure takes.
// Without the guard, the peer an operator interrupts most would be the
// peer routing demotes first.
//
// The cancel fires after selection on purpose: a context cancelled
// before it would fail inside selectAndProbe with no peer named, and
// the assertion below would hold for a reason that has nothing to do
// with the guard. The recorded event is asserted too, so this is a
// scoping change and not a silent deletion of the telemetry.
func TestPeerOutcome_ClientCancellationIsNotChargedToThePeer(t *testing.T) {
	spy := &peerOutcomeSpy{}
	rec := &captureRecorder{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &cancellingSelector{inner: &fakeSelector{sel: remoteSel("remote:dev-peer-b")}, cancel: cancel}
	gw := newPeerOutcomeGateway(t, sel, spy, rec, upstream.URL)
	postChatCtx(t, gw, ctx)

	if got := spy.all(); len(got) != 0 {
		t.Errorf("samples = %+v, want none: the operator gave up, the peer did not fail", got)
	}
	reqs := rec.requestsSnapshot()
	if len(reqs) != 1 {
		t.Fatalf("RecordRequest emits = %d, want 1", len(reqs))
	}
	if reqs[0].ErrorReason == "" {
		t.Errorf("the request must still be recorded as an error; got %+v", reqs[0])
	}
	if reqs[0].PeerID != "dev-peer-b" {
		t.Fatalf("PeerID = %q — the request never reached the peer, so this case proves nothing", reqs[0].PeerID)
	}
}

// TestPeerOutcome_ContextOverflowIsNotAPeerFault: a peer that refuses a
// prompt too long for the window it serves is working. Charging it
// would rank it below a peer that accepts the same prompt and truncates
// the head, which is the outcome waired-agent#436 set out to stop.
func TestPeerOutcome_ContextOverflowIsNotAPeerFault(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
		http.Error(w, "prompt is too long", http.StatusBadRequest)
	}))
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("remote:dev-peer-b")}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	body := `{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the peer's 400 relayed", w.Code)
	}
	if got := spy.all(); len(got) != 0 {
		t.Errorf("samples = %+v, want none: the prompt was wrong, not the peer", got)
	}
}

// TestPeerOutcome_AnthropicSurfaceRecordsToo: the Claude intercept is
// the busiest surface and reaches the engine through entirely separate
// helpers, so the OpenAI path passing says nothing about this one.
func TestPeerOutcome_AnthropicSurfaceRecordsToo(t *testing.T) {
	spy := &peerOutcomeSpy{}
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("remote:dev-peer-b")}
	gw := newPeerOutcomeGateway(t, sel, spy, nil, upstream.URL)

	body := `{"model":"waired/default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := spy.only(t); got.deviceID != "dev-peer-b" || !got.ok {
		t.Errorf("sample = %+v, want {dev-peer-b true}", got)
	}
}

// TestPeerOutcome_UnwiredListenerIsUnaffected: the overlay listener
// leaves Deps.OnPeerOutcome nil because it cannot dispatch to a peer,
// so the hook has to be entirely optional.
func TestPeerOutcome_UnwiredListenerIsUnaffected(t *testing.T) {
	upstream := fakeOllama(t, nil)
	defer upstream.Close()

	sel := &fakeSelector{sel: remoteSel("remote:dev-peer-b")}
	gw := newPeerOutcomeGateway(t, sel, nil, nil, upstream.URL)
	if code := postChatCtx(t, gw, nil).Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
}
