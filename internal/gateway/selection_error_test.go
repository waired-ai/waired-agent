package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
)

// Before this mapping existed, ErrPinnedPeerUnreachable fell into the
// default: branches — 500 api_error / "selection_failed" — which reads
// as a gateway bug rather than "the operator-pinned peer is down".

func TestAnthropicMessages_PinnedPeerUnreachableMapsTo503(t *testing.T) {
	sel := &fakeSelector{err: router.ErrPinnedPeerUnreachable}
	gw := newGatewayUnderTest(t, sel, "")

	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"qwen3-8b-instruct","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Type != "waired_pinned_peer_unreachable" {
		t.Fatalf("error type = %q, want waired_pinned_peer_unreachable", got.Error.Type)
	}
	if h := w.Header().Get(HeaderLocalError); h != "pinned_peer_unreachable" {
		t.Fatalf("%s = %q, want pinned_peer_unreachable (intercept fallback reason)", HeaderLocalError, h)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After must hint the client to back off")
	}
}

func TestChatCompletions_PinnedPeerUnreachableMapsTo503(t *testing.T) {
	sel := &fakeSelector{err: router.ErrPinnedPeerUnreachable}
	gw := newGatewayUnderTest(t, sel, "")

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Code != "waired_pinned_peer_unreachable" {
		t.Fatalf("error code = %q, want waired_pinned_peer_unreachable", got.Error.Code)
	}
}

// pinnedUnreachableErr is what the Selector actually returns for a strict
// pin failure: the sentinel wrapped in the typed error that names the peer.
func pinnedUnreachableErr(peer string) error {
	return &router.PinnedPeerUnreachableError{PeerDisplayID: peer, ModelID: "qwen3-8b-instruct"}
}

// TestPinnedPeerUnreachable_NamesThePeer pins the PRODUCT CONTRACT from
// waired-agent#325: a selection failure produces no Selection, so
// setSelection never runs and the WARN emit used to log
// `error_reason=pinned_peer_unreachable ... peer_id=` — an operator with
// several workers could not tell which pin was down. Both surfaces must now
// carry the peer in the event and in the response header.
func TestPinnedPeerUnreachable_NamesThePeer(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
	}{
		{
			"anthropic", "/anthropic/v1/messages",
			`{"model":"qwen3-8b-instruct","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"openai", "/v1/chat/completions",
			`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &captureRecorder{}
			gw := newGatewayWithRecorder(t, &fakeSelector{err: pinnedUnreachableErr("linux-gpu")}, "", rec)

			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.RemoteAddr = "127.0.0.1:1"
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, r)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
			}
			if h := w.Header().Get(HeaderInferencePeer); h != "linux-gpu" {
				t.Errorf("%s = %q, want linux-gpu", HeaderInferencePeer, h)
			}
			evs := rec.requestsSnapshot()
			if len(evs) != 1 {
				t.Fatalf("recorded %d request events, want 1", len(evs))
			}
			if evs[0].ErrorReason != "pinned_peer_unreachable" {
				t.Errorf("ErrorReason = %q", evs[0].ErrorReason)
			}
			if evs[0].PeerID != "linux-gpu" {
				t.Errorf("PeerID = %q, want linux-gpu", evs[0].PeerID)
			}
		})
	}
}

// A selection error that is NOT a pin failure must leave PeerID empty —
// there is no peer to name, and inventing one would mislead.
func TestSelectionError_WithoutPin_LeavesPeerIDEmpty(t *testing.T) {
	rec := &captureRecorder{}
	gw := newGatewayWithRecorder(t, &fakeSelector{err: router.ErrModelNotReady}, "", rec)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`))
	r.RemoteAddr = "127.0.0.1:1"
	gw.Handler().ServeHTTP(httptest.NewRecorder(), r)

	evs := rec.requestsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("recorded %d request events, want 1", len(evs))
	}
	if evs[0].PeerID != "" {
		t.Errorf("PeerID = %q, want empty", evs[0].PeerID)
	}
}

func TestSelectionStatusAndReason_PinnedPeerUnreachable(t *testing.T) {
	if s := selectionStatus(router.ErrPinnedPeerUnreachable); s != http.StatusServiceUnavailable {
		t.Fatalf("selectionStatus = %d, want 503", s)
	}
	if r := selectionErrorReason(router.ErrPinnedPeerUnreachable); r != "pinned_peer_unreachable" {
		t.Fatalf("selectionErrorReason = %q, want pinned_peer_unreachable", r)
	}
}

// TestSelectionRecord_MatchesWhatTheClientReceives pins the PRODUCT
// CONTRACT from waired-agent#740: the status the gateway RECORDS for a
// selection failure is the status it SENDS.
//
// ev.Status is not decorative. It goes into the event ring the management
// API serves, decides the result label on InferenceRequestsTotal, and is the
// `status` field of the WARN and DEBUG lines
// (internal/observability.Recorder.RecordRequest). The only use a reader has
// for it is comparing it against what the client reported, so a silent
// disagreement gets spent on doubting the client instead of reading the
// number. ErrHardwareInsufficient disagreed — recorded 400, sent 422 —
// because selectionStatus and the two responders are separate switches over
// the same sentinels and only the responders write the response.
//
// Every sentinel is a row rather than only the one that was reported: the
// three switches can drift for any of them, and the one that drifted was the
// one nobody was looking at. Both wire surfaces are driven for every row
// because each has its own responder.
//
// What this test cannot see is a sentinel added to router with no row here —
// the same gap that let ErrAllPeersOverloaded answer 500 from the explain
// endpoint (waired-agent#710). So the last two rows assert the shape of that
// failure instead: an unhandled error lands in every default: and answers
// 500, which is what a reader confronted with one should recognise.
func TestSelectionRecord_MatchesWhatTheClientReceives(t *testing.T) {
	surfaces := []struct{ name, path, body string }{
		{
			"openai", "/v1/chat/completions",
			`{"model":"qwen3-8b-instruct","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			"anthropic", "/anthropic/v1/messages",
			`{"model":"qwen3-8b-instruct","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
		// defensive marks a sentinel this path cannot currently be handed:
		// the gateway's own probe round produces it, not the Selector. The
		// mapping is still asserted; the reachability is not claimed. Same
		// convention as management's TestMapRouterStatus_AgreesWithServingSurfaces.
		defensive bool
	}{
		{name: "model not found", err: router.ErrModelNotFound, want: http.StatusNotFound},
		{name: "capability not met", err: router.ErrCapabilityNotMet, want: http.StatusBadRequest},
		{name: "hardware insufficient", err: router.ErrHardwareInsufficient, want: http.StatusUnprocessableEntity},
		{name: "model not ready", err: router.ErrModelNotReady, want: http.StatusServiceUnavailable},
		{name: "all peers overloaded", err: router.ErrAllPeersOverloaded, want: http.StatusServiceUnavailable},
		{
			name: "peers did not answer", err: router.ErrPeersDidNotAnswer,
			want: http.StatusServiceUnavailable, defensive: true,
		},
		{name: "pinned peer unreachable", err: router.ErrPinnedPeerUnreachable, want: http.StatusServiceUnavailable},
		{
			name: "pinned peer unreachable, wrapped with the peer identity",
			err:  pinnedUnreachableErr("linux-gpu"), want: http.StatusServiceUnavailable,
		},
		{
			name: "peer routing disabled", err: ErrPeerRoutingDisabled,
			want: http.StatusServiceUnavailable, defensive: true,
		},
		{name: "runtime not installed", err: router.ErrRuntimeNotInstalled, want: http.StatusServiceUnavailable},
		{
			// Record of today's behaviour on both sides, not a considered
			// choice — the same note management's table carries for it.
			name: "no endpoint for window", err: router.ErrNoEndpointForWindow,
			want: http.StatusInternalServerError,
		},
		{
			name: "an error no switch handles", err: errors.New("router: a sentinel from the future"),
			want: http.StatusInternalServerError,
		},
	} {
		for _, surface := range surfaces {
			t.Run(tc.name+"/"+surface.name, func(t *testing.T) {
				if tc.defensive {
					t.Log("defensive row: the gateway's probe round produces this, " +
						"not the Selector, so a real request cannot arrive here with it; " +
						"the mapping is asserted, the reachability is not claimed")
				}
				rec := &captureRecorder{}
				gw := newGatewayWithRecorder(t, &fakeSelector{err: tc.err}, "", rec)

				r := httptest.NewRequest(http.MethodPost, surface.path, strings.NewReader(surface.body))
				r.RemoteAddr = "127.0.0.1:1"
				w := httptest.NewRecorder()
				gw.Handler().ServeHTTP(w, r)

				if w.Code != tc.want {
					t.Errorf("client received %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
				}
				evs := rec.requestsSnapshot()
				if len(evs) != 1 {
					t.Fatalf("recorded %d request events, want 1", len(evs))
				}
				if evs[0].Status != w.Code {
					t.Errorf("recorded status = %d but the client received %d: "+
						"a reader comparing the event ring against what the client "+
						"reported would see a disagreement the gateway invented "+
						"(waired-agent#740)", evs[0].Status, w.Code)
				}
				if evs[0].Status != tc.want {
					t.Errorf("recorded status = %d, want %d", evs[0].Status, tc.want)
				}
			})
		}
	}
}
