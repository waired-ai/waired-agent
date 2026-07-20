package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/router"
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

// End to end through a handler: the error body an unreachable runtime
// produces must not echo the raw runtime string.
func TestRuntimeUnavailableError_DoesNotLeakDeviceID(t *testing.T) {
	sel := publicSelection()
	gw := newGatewayUnderTest(t, &fakeSelector{sel: sel}, "")
	// PeerAdapterFactory is nil on this fixture, so lookupAdapter fails
	// with ErrPeerRoutingDisabled and takes the error-body path.
	body, _ := json.Marshal(map[string]any{
		"model":    "waired/default",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

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

var _ = observability.FallbackEvent{}
