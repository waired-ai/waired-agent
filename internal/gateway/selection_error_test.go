package gateway

import (
	"encoding/json"
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
