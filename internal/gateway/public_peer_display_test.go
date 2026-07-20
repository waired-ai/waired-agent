package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// A Public Share peer's real device identifier must never reach a
// client-visible header, an observability event, or a log line — only
// its grant pseudonym may (public share spec §8.5). Every assertion here
// keys off one obviously-synthetic constant so a leak anywhere shows up
// as a substring hit.
const (
	foreignDeviceID = "dev_foreign00000001"
	foreignAlias    = "guest-a7f3"
)

func publicSelection() router.Selection {
	return router.Selection{
		EndpointID:    "ep_remote-" + foreignAlias + "_ollama_qwen3-8b-instruct",
		ModelID:       "qwen3-8b-instruct",
		EngineModel:   "qwen3:8b-q4_K_M",
		Runtime:       remoteRuntimePrefix + foreignDeviceID,
		ExecutionMode: "remote",
		PeerDisplayID: foreignAlias,
	}
}

func TestSetSelectionHeaders_UsesPseudonymOnBothSides(t *testing.T) {
	rec := &captureRecorder{}
	w := httptest.NewRecorder()

	// The fallback-FROM side is the one the design originally missed:
	// with public sorted last, cands[0] is a public peer exactly when the
	// own-candidate set is empty, so a probe failure on it stamps the
	// fallback headers with that peer.
	setSelectionHeaders(w, publicSelection(), foreignAlias, "probe_failed", rec)

	for name, got := range map[string]string{
		HeaderInferencePeer:  w.Header().Get(HeaderInferencePeer),
		HeaderFallbackFrom:   w.Header().Get(HeaderFallbackFrom),
		HeaderFallbackReason: w.Header().Get(HeaderFallbackReason),
	} {
		if strings.Contains(got, foreignDeviceID) {
			t.Errorf("header %s leaks the foreign device id: %q", name, got)
		}
	}
	if got := w.Header().Get(HeaderInferencePeer); got != foreignAlias {
		t.Errorf("%s = %q, want the pseudonym %q", HeaderInferencePeer, got, foreignAlias)
	}

	if len(rec.fallback) != 1 {
		t.Fatalf("fallback events = %d, want 1", len(rec.fallback))
	}
	ev := rec.fallback[0]
	blob, _ := json.Marshal(ev)
	if bytes.Contains(blob, []byte(foreignDeviceID)) {
		t.Errorf("FallbackEvent leaks the foreign device id: %s", blob)
	}
	if ev.To != foreignAlias || ev.From != foreignAlias {
		t.Errorf("FallbackEvent = from %q to %q, want the pseudonym on both", ev.From, ev.To)
	}
}

// The nil-Recorder path keeps the historical direct slog.Warn line; it
// must be scrubbed too, since agent.log is read by support.
func TestSetSelectionHeaders_NilRecorderLogScrubbed(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	setSelectionHeaders(httptest.NewRecorder(), publicSelection(), foreignAlias, "probe_failed", nil)

	if strings.Contains(buf.String(), foreignDeviceID) {
		t.Errorf("slog fallback line leaks the foreign device id: %s", buf.String())
	}
}

func TestRequestEvent_CarriesPseudonymNotDeviceID(t *testing.T) {
	rr := &requestRec{rec: &captureRecorder{}}
	rr.setSelection(publicSelection(), foreignAlias, "probe_failed")

	blob, _ := json.Marshal(rr.ev)
	if bytes.Contains(blob, []byte(foreignDeviceID)) {
		t.Errorf("RequestEvent leaks the foreign device id: %s", blob)
	}
	if rr.ev.PeerID != foreignAlias {
		t.Errorf("RequestEvent.PeerID = %q, want %q", rr.ev.PeerID, foreignAlias)
	}
}

// Own-network peers have no pseudonym; their DeviceID is the display
// identifier and must survive unchanged.
func TestOwnPeerDisplayUnchanged(t *testing.T) {
	sel := router.Selection{
		Runtime:       remoteRuntimePrefix + "dev_own00000001",
		ExecutionMode: "remote",
		PeerDisplayID: "dev_own00000001",
	}
	if got := peerDisplayID(sel); got != "dev_own00000001" {
		t.Errorf("peerDisplayID = %q", got)
	}
	// Selections built before PeerDisplayID existed (and by test fakes)
	// still resolve from the runtime suffix.
	legacy := router.Selection{Runtime: remoteRuntimePrefix + "dev_own00000001", ExecutionMode: "remote"}
	if got := peerDisplayID(legacy); got != "dev_own00000001" {
		t.Errorf("legacy peerDisplayID = %q", got)
	}
	if got := peerDisplayID(router.Selection{Runtime: "ollama", ExecutionMode: "local"}); got != "" {
		t.Errorf("local selection produced a peer id: %q", got)
	}
}

// End to end through a handler: a peer-adapter failure must not put the
// peer's real DeviceID in the response, whichever way the request ends.
//
// The production PeerAdapterFactory formats that identifier into its
// errors ("peer %q not in current mesh snapshot"), and it is consulted
// more than once per request — the probe coordinator resolves a
// transport, the dispatch path resolves an adapter. This drives the
// factory failing after the probe already used it, which with the
// current probe fixtures terminates as waired_all_peers_overloaded.
//
// Scope note: the scrub applied at the dispatch branch itself is pinned
// directly by TestAdapterErrorForClient_KeepsDetailForOwnPeers, which is
// mutation-sensitive; this test guards the surrounding handler path,
// where a regression would surface as the identifier appearing in an
// error body or a header.
func TestRuntimeUnavailableError_DoesNotLeakDeviceID(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true}`))
	}))
	defer healthy.Close()

	var calls int
	sel := publicSelection()
	gw := newGatewayWithPeerFactory(t, &fakeSelector{sel: sel},
		func(deviceID string) (runtime.Adapter, error) {
			calls++
			if calls == 1 {
				return probeableAdapter{baseURL: healthy.URL}, nil
			}
			return nil, fmt.Errorf("peer %q not in current mesh snapshot", deviceID)
		})
	body, _ := json.Marshal(map[string]any{
		"model":    "waired/default",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if calls < 2 {
		t.Fatalf("the failing factory was consulted %d time(s); the fixture no longer drives "+
			"a post-probe adapter failure", calls)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), foreignDeviceID) {
		t.Errorf("error body leaks the foreign device id: %s", w.Body.String())
	}
	for k, vs := range w.Header() {
		for _, v := range vs {
			if strings.Contains(v, foreignDeviceID) {
				t.Errorf("header %s leaks the foreign device id: %q", k, v)
			}
		}
	}
}

// newGatewayWithPeerFactory is newGatewayUnderTest with a peer-adapter
// factory wired, so the remote dispatch path can be driven.
func newGatewayWithPeerFactory(t *testing.T, sel SelectorIface, f func(string) (runtime.Adapter, error)) *Server {
	t.Helper()
	return NewServer(ServerConfig{Addr: "127.0.0.1:0"}, Deps{
		Selector:           sel,
		Runtimes:           runtime.NewRegistry(),
		ListManifests:      asManifestList([]catalog.Manifest{qwenManifest()}),
		HTTPClient:         http.DefaultClient,
		AllowOpenAI:        true,
		AllowAnthropic:     true,
		PeerAdapterFactory: f,
	})
}

// The scrub must not blunt the error for an OWN peer: those identifiers
// are the operator's own and support depends on them.
func TestAdapterErrorForClient_KeepsDetailForOwnPeers(t *testing.T) {
	own := router.Selection{
		Runtime:       remoteRuntimePrefix + "dev_own00000001",
		PeerDisplayID: "dev_own00000001",
		ExecutionMode: "remote",
	}
	got := adapterErrorForClient(own, fmt.Errorf("peer %q not in current mesh snapshot", "dev_own00000001"))
	if !strings.Contains(got, "not in current mesh snapshot") {
		t.Errorf("own-peer error lost its detail: %q", got)
	}

	pub := publicSelection()
	got = adapterErrorForClient(pub, fmt.Errorf("peer %q not in current mesh snapshot", foreignDeviceID))
	if strings.Contains(got, foreignDeviceID) {
		t.Errorf("public-peer error leaks the device id: %q", got)
	}
}

// candidateDisplayID is what tryProbeAndCommit stamps into
// probedSelection.FallbackFrom, the value that reaches
// X-Waired-Fallback-From and the FallbackEvent.
func TestCandidateDisplayID(t *testing.T) {
	pub := router.Candidate{PeerID: foreignDeviceID, PeerDisplayID: foreignAlias}
	if got := candidateDisplayID(pub); got != foreignAlias {
		t.Errorf("candidateDisplayID = %q, want the pseudonym", got)
	}
	own := router.Candidate{PeerID: "dev_own00000001"}
	if got := candidateDisplayID(own); got != "dev_own00000001" {
		t.Errorf("candidateDisplayID = %q", got)
	}
}

// probeableAdapter is a peer adapter whose Transport()/BaseURL() point
// at a test server, so the Phase 8 probe coordinator can mark it ready.
type probeableAdapter struct{ baseURL string }

func (p probeableAdapter) Name() string                          { return "peer" }
func (p probeableAdapter) EnsureRunning(_ context.Context) error { return nil }
func (p probeableAdapter) Health(_ context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (p probeableAdapter) Stop(_ context.Context) error { return nil }
func (p probeableAdapter) BaseURL() string              { return p.baseURL }
func (p probeableAdapter) Transport() http.RoundTripper { return http.DefaultTransport }
